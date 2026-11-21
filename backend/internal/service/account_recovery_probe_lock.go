package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// ErrRecoveryLockUnavailable 表示恢复探测的协调后端不可用。
//
// 批1 明确要求：Redis 不可用时**失败关闭**，本轮不探测——既不沿用
// tryAcquireSingletonLeaderLock 的 PG 兜底，也不沿用“无协调即放行”。
var ErrRecoveryLockUnavailable = errors.New("recovery probe lock backend unavailable")

// ErrRecoveryProbeAccountBusy 表示该账号正被另一条探测路径持锁，手动入口本轮拒绝。
var ErrRecoveryProbeAccountBusy = errors.New("account recovery probe already in progress")

var recoveryLockOwnerSeq atomic.Uint64

// newRecoveryLockOwner 生成跨实例唯一的锁持有者标识。
func newRecoveryLockOwner() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d:%d", host, os.Getpid(), recoveryLockOwnerSeq.Add(1))
}

// tryAcquireRedisLeaderLock 是 Redis-only 的锁获取（批1）。
//
// 与 tryAcquireSingletonLeaderLock 的区别：
//   - cache 为 nil 或报错 → 返回 ErrRecoveryLockUnavailable，调用方本轮不探测；
//   - 不做 PG advisory lock 兜底，不做 fail-open。
func tryAcquireRedisLeaderLock(ctx context.Context, cache LeaderLockCache, key, owner string, ttl time.Duration) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cache == nil {
		return false, ErrRecoveryLockUnavailable
	}
	ok, err := cache.TryAcquireLeaderLock(ctx, key, owner, ttl)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrRecoveryLockUnavailable, err)
	}
	return ok, nil
}

// releaseRecoveryLock 用独立短超时释放，避免调用方 ctx 已取消时锁泄漏到 TTL。
func releaseRecoveryLock(cache LeaderLockCache, key, owner string) {
	if cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = cache.ReleaseLeaderLock(ctx, key, owner)
}

// 批4：六源单账号探测点共用的账号级锁入口。
//
// 六源服务自身不持有 recoveryLock（其中 CN 生命周期/CN provider/CodeBuddy 连 leader 锁都没有），
// 故由健康服务装配时把同一把 Redis 锁缓存注入到包级变量，六源统一经
// acquireRecoveryAccountProbeLock 取同一把账号锁 recovery:probe:<id>。
var (
	recoveryAccountLockMu    sync.RWMutex
	recoveryAccountLockCache LeaderLockCache
)

// SetRecoveryAccountLockCache 由健康服务装配处注入账号锁后端（与批 1 同一把锁）。
func SetRecoveryAccountLockCache(cache LeaderLockCache) {
	recoveryAccountLockMu.Lock()
	recoveryAccountLockCache = cache
	recoveryAccountLockMu.Unlock()
}

// acquireRecoveryAccountProbeLock 获取账号级探测锁。
//
// 返回 (release, ok, err)：
//   - ok=true  → 已持锁，调用方探测完必须 release()；
//   - ok=false → 锁被占用或后端不可用，调用方本轮跳过该账号；
//   - 未注入锁后端（单实例/未接线）→ 与批 1 一致退回“无锁执行”，ok=true。
func acquireRecoveryAccountProbeLock(ctx context.Context, accountID int64) (func(), bool, error) {
	recoveryAccountLockMu.RLock()
	cache := recoveryAccountLockCache
	recoveryAccountLockMu.RUnlock()
	if cache == nil {
		return func() {}, true, nil
	}
	key := fmt.Sprintf("recovery:probe:%d", accountID)
	owner := newRecoveryLockOwner()
	ok, err := tryAcquireRedisLeaderLock(ctx, cache, key, owner, accountRecoveryProbeLockTTL)
	if err != nil {
		// 锁后端故障与正常争锁区分：后者按既定规则静默跳过，前者记录日志/指标，
		// 避免「后端挂了」被误当成「无到期账号」（外审 suggestion）。
		logger.L().Warn("recovery_account_probe_lock_unavailable",
			zap.Int64("account_id", accountID), zap.Error(err))
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return func() { releaseRecoveryLock(cache, key, owner) }, true, nil
}

// withRecoveryAccountProbeLock 在账号锁内执行 fn；未获锁（争锁/后端不可用）则跳过，
// 返回 false。fn 内不得再调用 acquireRecoveryAccountProbeLock（锁不可重入）。
func withRecoveryAccountProbeLock(ctx context.Context, accountID int64, fn func()) bool {
	release, ok, err := acquireRecoveryAccountProbeLock(ctx, accountID)
	if err != nil || !ok {
		return false
	}
	defer release()
	fn()
	return true
}
