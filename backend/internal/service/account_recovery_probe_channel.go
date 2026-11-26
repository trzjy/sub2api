package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const (
	accountRecoveryLeaderLockKey = "recovery:probe:leader"
	// accountRecoveryLeaderTTL 覆盖单轮预算（300s）+ 余量。
	accountRecoveryLeaderTTL = 360 * time.Second
	// accountRecoveryRoundBudget 是单轮墙钟预算；用完停止派发新探测。
	accountRecoveryRoundBudget = 300 * time.Second
	// accountRecoveryLockContentionDelay 是争锁失败时 probe_due_at 的确定重排间隔。
	accountRecoveryLockContentionDelay = 60 * time.Second
	// accountRecoveryChannelConcurrency 是新通道的并发上限。
	accountRecoveryChannelConcurrency = 8
)

// accountRecoveryProbeLockTTL 覆盖单账号临界区上界并留余量：
//   - cursor 额度查询 225s；
//   - 余额快照持久化；
//   - CodeBuddy / Web Console 探测 TimeoutSeconds + 10s（HTTP grace）。
//
// 默认 330s，可由装配期推导通过 SetRecoveryAccountLockTTL 覆盖。
var accountRecoveryProbeLockTTL = 330 * time.Second

// SetRecoveryAccountLockTTL 在 d>0 时覆盖账号探测锁 TTL，供装配期推导注入。
func SetRecoveryAccountLockTTL(d time.Duration) {
	if d > 0 {
		accountRecoveryProbeLockTTL = d
	}
}

// RunRecoveryChannel 是批1 的通用恢复通道：按 probe_due_at 取候选，逐账号加锁，
// 按 extra 分量执行额度/实际调用探测，成功后按分量代际清除。
//
// 与既有 marker sweep 并行运行但**同账号互斥**（同一把 Redis 账号锁）。
func (p *AccountHealthRecoveryProbeService) RunRecoveryChannel(ctx context.Context) {
	if p == nil || p.accountRepo == nil {
		return
	}
	// 协调后端不可用 → 失败关闭，本轮不探测。
	owner := newRecoveryLockOwner()
	leader, err := tryAcquireRedisLeaderLock(ctx, p.recoveryLock, accountRecoveryLeaderLockKey, owner, accountRecoveryLeaderTTL)
	if err != nil || !leader {
		if err != nil {
			logger.L().Warn("account_recovery_channel_lock_unavailable", zap.Error(err))
		}
		return
	}
	defer releaseRecoveryLock(p.recoveryLock, accountRecoveryLeaderLockKey, owner)

	roundCtx, cancel := context.WithTimeout(ctx, accountRecoveryRoundBudget)
	defer cancel()

	candidates, err := p.accountRepo.ListProbeDueAccounts(roundCtx, time.Now(), accountHealthProbeCandidateLimit)
	if err != nil {
		logger.L().Warn("account_recovery_channel_list_failed", zap.Error(err))
		return
	}
	if len(candidates) == 0 {
		return
	}

	sem := make(chan struct{}, accountRecoveryChannelConcurrency)
	var wg sync.WaitGroup
	for _, acc := range candidates {
		if roundCtx.Err() != nil {
			break
		}
		acc := acc
		// cursor 额度查询最长 180s：单独限并发 4，避免长查询叠加打满上游。
		limit := accountRecoveryChannelConcurrency
		if acc.Platform == PlatformCursor {
			limit = 4
		}
		if len(sem) >= limit {
			select {
			case <-roundCtx.Done():
				continue
			case sem <- struct{}{}:
			}
		} else {
			sem <- struct{}{}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			p.probeRecoveryAccount(roundCtx, acc, owner)
		}()
	}
	wg.Wait()
}

// probeRecoveryAccount 对单个账号执行到期的探测分量。
func (p *AccountHealthRecoveryProbeService) probeRecoveryAccount(ctx context.Context, acc *Account, owner string) {
	if acc == nil {
		return
	}
	now := time.Now()
	quotaGen, quotaDue, _, quotaKind, hasQuota := acc.RecoveryProbeComponent(recoveryProbeComponentQuota)
	transportGen, transportDue, transportMode, _, hasTransport := acc.RecoveryProbeComponent(recoveryProbeComponentTransport)
	quotaDueNow := hasQuota && !quotaDue.After(now)
	transportDueNow := hasTransport && !transportDue.After(now)
	if !quotaDueNow && !transportDueNow {
		return
	}

	lockKey := fmt.Sprintf("recovery:probe:%d", acc.ID)
	locked, err := tryAcquireRedisLeaderLock(ctx, p.recoveryLock, lockKey, owner, accountRecoveryProbeLockTTL)
	if err != nil || !locked {
		// 争锁或锁后端不可用：确定重排，不探测。
		newDue := now.Add(accountRecoveryLockContentionDelay)
		if quotaDueNow {
			p.rescheduleComponent(ctx, acc.ID, recoveryProbeComponentQuota, quotaGen, newDue)
		}
		if transportDueNow {
			p.rescheduleComponent(ctx, acc.ID, recoveryProbeComponentTransport, transportGen, newDue)
		}
		return
	}
	defer releaseRecoveryLock(p.recoveryLock, lockKey, owner)

	if quotaDueNow {
		p.runQuotaComponent(ctx, acc, quotaGen, quotaKind)
	}
	if transportDueNow {
		p.runTransportComponent(ctx, acc, transportGen, transportMode)
	}
}

func (p *AccountHealthRecoveryProbeService) runQuotaComponent(ctx context.Context, acc *Account, gen int64, kind string) {
	ok, err := p.probeQuotaComponent(ctx, acc, kind)
	switch {
	case err != nil:
		logger.L().Info("account_recovery_quota_probe_uncertain",
			zap.Int64("account_id", acc.ID), zap.Error(err))
		p.rescheduleComponent(ctx, acc.ID, recoveryProbeComponentQuota, gen, time.Now().Add(recoveryQuotaProbeCadence))
	case ok:
		if cleared, cerr := p.accountRepo.ClearRecoveryProbeComponent(ctx, acc.ID, recoveryProbeComponentQuota, gen); cerr != nil {
			logger.L().Warn("account_recovery_clear_failed", zap.Int64("account_id", acc.ID), zap.Error(cerr))
		} else if !cleared {
			logger.L().Info("account_recovery_clear_generation_takenover", zap.Int64("account_id", acc.ID))
		} else {
			p.afterRecovered(acc.ID)
		}
	default:
		p.rescheduleComponent(ctx, acc.ID, recoveryProbeComponentQuota, gen, time.Now().Add(recoveryQuotaProbeCadence))
	}
}

func (p *AccountHealthRecoveryProbeService) runTransportComponent(ctx context.Context, acc *Account, gen int64, mode string) {
	ok, err := p.probeUpstream(ctx, acc)
	cadence := recoveryUnavailableCadence
	if mode == recoveryModeTransient {
		cadence = recoveryTransientCadence
	}
	switch {
	case err != nil:
		logger.L().Info("account_recovery_transport_probe_uncertain",
			zap.Int64("account_id", acc.ID), zap.Error(err))
		p.rescheduleComponent(ctx, acc.ID, recoveryProbeComponentTransport, gen, time.Now().Add(cadence))
	case ok:
		if cleared, cerr := p.accountRepo.ClearRecoveryProbeComponent(ctx, acc.ID, recoveryProbeComponentTransport, gen); cerr != nil {
			logger.L().Warn("account_recovery_clear_failed", zap.Int64("account_id", acc.ID), zap.Error(cerr))
		} else if !cleared {
			logger.L().Info("account_recovery_clear_generation_takenover", zap.Int64("account_id", acc.ID))
		} else {
			p.afterRecovered(acc.ID)
		}
	default:
		p.rescheduleComponent(ctx, acc.ID, recoveryProbeComponentTransport, gen, time.Now().Add(cadence))
	}
}

// probeQuotaComponent 按 kind 执行额度探测。
//   - quota：底层真实额度查询（balance_probe / antigravity / cursor key）；
//   - actual：实际数据面调用；
//   - both：两项都做、都成功才算成功（R2）。
func (p *AccountHealthRecoveryProbeService) probeQuotaComponent(ctx context.Context, acc *Account, kind string) (bool, error) {
	if kind == recoveryKindActual || kind == "" {
		return p.probeUpstream(ctx, acc)
	}
	quotaOK, err := p.probeQuotaSignal(ctx, acc)
	if err != nil {
		return false, err
	}
	if kind != recoveryKindBoth {
		return quotaOK, nil
	}
	if !quotaOK {
		return false, nil
	}
	return p.probeUpstream(ctx, acc)
}

func (p *AccountHealthRecoveryProbeService) probeQuotaSignal(ctx context.Context, acc *Account) (bool, error) {
	if acc.BalanceProbeConfig().Enabled {
		if p.balanceProbe == nil {
			return false, fmt.Errorf("balance probe service is not wired")
		}
		res, err := p.balanceProbe.Query(ctx, acc.ID)
		if err != nil {
			return false, err
		}
		if res == nil {
			return false, fmt.Errorf("balance probe returned nil result")
		}
		return res.Success && res.Valid && res.Remaining != nil && *res.Remaining > 0, nil
	}
	switch {
	case acc.Platform == PlatformAntigravity:
		if p.antigravityQuota == nil || p.antigravityTokens == nil {
			return false, fmt.Errorf("antigravity quota probe is not wired")
		}
		proxyURL := ""
		if acc.Proxy != nil {
			proxyURL = acc.Proxy.URL()
		}
		return p.antigravityQuota.ProbeUserQuota(ctx, acc, proxyURL, p.probeModelForAccount(acc), p.antigravityTokens)
	case acc.Platform == PlatformCursor && acc.QuotaKeyID != nil && *acc.QuotaKeyID > 0:
		if p.cursorEntry == nil {
			return false, fmt.Errorf("cursor quota probe is not wired")
		}
		info, err := p.cursorEntry.ProbeKeyQuota(ctx, *acc.QuotaKeyID)
		if err != nil {
			return false, err
		}
		if info == nil || info.TodayRemainingCent == nil {
			return false, fmt.Errorf("cursor quota observation missing today_remaining")
		}
		return *info.TodayRemainingCent > 0, nil
	}
	// 无额度入口：退化为实际调用（用户裁定 5）。
	return p.probeUpstream(ctx, acc)
}

func (p *AccountHealthRecoveryProbeService) rescheduleComponent(ctx context.Context, accountID int64, component string, gen int64, due time.Time) {
	if p == nil || p.accountRepo == nil {
		return
	}
	if _, err := p.accountRepo.RescheduleRecoveryProbeComponent(ctx, accountID, component, gen, due); err != nil {
		logger.L().Warn("account_recovery_reschedule_failed",
			zap.Int64("account_id", accountID), zap.String("component", component), zap.Error(err))
	}
}

// afterRecovered 复用既有恢复后副作用（冻结上界、渠道陈旧收敛）。
func (p *AccountHealthRecoveryProbeService) afterRecovered(accountID int64) {
	p.clearCandidateBound(accountID, "")
	if p.channelFreshness != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.channelFreshness.RefreshChannelFreshnessForAccount(ctx, accountID); err != nil {
			logger.L().Warn("account_recovery_channel_freshness_refresh_failed",
				zap.Int64("account_id", accountID), zap.Error(err))
		}
	}
	logger.L().Info("account_recovery_probe_recovered", zap.Int64("account_id", accountID))
}

// accountRecoveryProbeRepoAdapter 把 AccountRepository 适配为探测服务需要的窄接口。
//
// 为什么用适配器而不是把三个方法加进 AccountRepository 大接口：大接口有十余个测试桩，
// 加方法会让所有桩被迫实现探测原语；探测原语只在恢复通道使用，运行时用类型断言取用，
// 未实现时失败关闭（不探测）。
type accountRecoveryProbeRepoAdapter struct {
	AccountRepository
}

func (a accountRecoveryProbeRepoAdapter) ListProbeDueAccounts(ctx context.Context, now time.Time, limit int) ([]*Account, error) {
	if repo, ok := a.AccountRepository.(interface {
		ListProbeDueAccounts(ctx context.Context, now time.Time, limit int) ([]*Account, error)
	}); ok {
		return repo.ListProbeDueAccounts(ctx, now, limit)
	}
	return nil, ErrRecoveryLockUnavailable
}

func (a accountRecoveryProbeRepoAdapter) ClearRecoveryProbeComponent(ctx context.Context, accountID int64, component string, expectedGen int64) (bool, error) {
	if repo, ok := a.AccountRepository.(interface {
		ClearRecoveryProbeComponent(ctx context.Context, accountID int64, component string, expectedGen int64) (bool, error)
	}); ok {
		return repo.ClearRecoveryProbeComponent(ctx, accountID, component, expectedGen)
	}
	return false, ErrRecoveryLockUnavailable
}

func (a accountRecoveryProbeRepoAdapter) RescheduleRecoveryProbeComponent(ctx context.Context, accountID int64, component string, expectedGen int64, newDue time.Time) (bool, error) {
	if repo, ok := a.AccountRepository.(interface {
		RescheduleRecoveryProbeComponent(ctx context.Context, accountID int64, component string, expectedGen int64, newDue time.Time) (bool, error)
	}); ok {
		return repo.RescheduleRecoveryProbeComponent(ctx, accountID, component, expectedGen, newDue)
	}
	return false, ErrRecoveryLockUnavailable
}

func (a accountRecoveryProbeRepoAdapter) ListModelRateLimitProbeDue(ctx context.Context, now time.Time, limit int) ([]ModelProbeTask, error) {
	if repo, ok := a.AccountRepository.(interface {
		ListModelRateLimitProbeDue(ctx context.Context, now time.Time, limit int) ([]ModelProbeTask, error)
	}); ok {
		return repo.ListModelRateLimitProbeDue(ctx, now, limit)
	}
	return nil, ErrRecoveryLockUnavailable
}

func (a accountRecoveryProbeRepoAdapter) PatchModelRateLimitEntry(ctx context.Context, accountID int64, scope string, expectedGen int64, patch ModelProbePatch) (bool, error) {
	if repo, ok := a.AccountRepository.(interface {
		PatchModelRateLimitEntry(ctx context.Context, accountID int64, scope string, expectedGen int64, patch ModelProbePatch) (bool, error)
	}); ok {
		return repo.PatchModelRateLimitEntry(ctx, accountID, scope, expectedGen, patch)
	}
	return false, ErrRecoveryLockUnavailable
}

// acquireAccountProbeLock 获取账号级探测锁（三条探测路径共用）。
//
// 未注入恢复锁时（单实例部署或未接线）退回“无锁执行”，保持既有 marker 路径
// 的可用性；一旦注入了 Redis 锁，则争锁失败/后端不可用都返回 ok=false，
// 调用方本轮跳过该账号（批1-1）。
func (p *AccountHealthRecoveryProbeService) acquireAccountProbeLock(ctx context.Context, accountID int64, owner string) (func(), bool, error) {
	if p == nil || p.recoveryLock == nil {
		return func() {}, true, nil
	}
	key := fmt.Sprintf("recovery:probe:%d", accountID)
	ok, err := tryAcquireRedisLeaderLock(ctx, p.recoveryLock, key, owner, accountRecoveryProbeLockTTL)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return func() { releaseRecoveryLock(p.recoveryLock, key, owner) }, true, nil
}
