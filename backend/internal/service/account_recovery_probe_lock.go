package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// ErrRecoveryLockUnavailable 表示恢复探测的协调后端不可用。
//
// 批1 明确要求：Redis 不可用时**失败关闭**，本轮不探测——既不沿用
// tryAcquireSingletonLeaderLock 的 PG 兜底，也不沿用“无协调即放行”。
var ErrRecoveryLockUnavailable = errors.New("recovery probe lock backend unavailable")

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
