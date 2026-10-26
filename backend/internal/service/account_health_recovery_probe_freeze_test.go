//go:build unit

// D-QL-007 F1 定向测试：账号级冻结上界写入者恢复。
//
// D-QL-003C 删除 D2 相位时把 freezeCandidateBound 的唯一生产调用点一并删除，但
// freshness 链（EvaluateAccountLevelFreshness → AccountFreshnessUpperBound）仍读
// frozenBounds——缺写入者则账号级上界恒 0、阈值退化为基线。本文件锁定：
//   - RunOnce 账号级相位（isHealthBreakerTrip 通过的账号）在 probeAccount 前
//     冻结账号级（modelKey=""）上界：frozenCandidateBoundOK == (1, true)、
//     AccountFreshnessUpperBound > 0；
//   - 恢复路径（probeAccount 成功 → clearCandidateBound）后回到未冻结态。
package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// RunOnce 账号级相位冻结账号级上界（探测失败路径，账号保持 parked）。
func TestHealthRecoveryRunOnceFreezesAccountLevelFreshnessUpperBound(t *testing.T) {
	acc := parkedBreakerAccount(7, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: acc},
		candidates: []*Account{acc},
	}
	// probeEnabled=true、探测 override 返回 false（失败路径：账号保持 parked，
	// 冻结值不被 clearCandidateBound 清除）。rateLimit 传 nil（观测记录窄面自带
	// nil 守卫，与冻结上界无关联）。
	p := newProbeService(t, true, nil, repo)
	// 永久失败 + 补探无轮数上限（C1）：以 ctx 取消划定补探边界，单测注入毫秒级节奏。
	p.retryInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int32
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		if atomic.AddInt32(&calls, 1) >= 2 {
			cancel()
		}
		return false, nil
	})

	p.RunOnce(ctx)
	p.wg.Wait()

	total, frozen := p.frozenCandidateBoundOK(7, "")
	require.True(t, frozen, "RunOnce 账号级相位必须冻结账号级（modelKey=\"\"）候选总数")
	require.Equal(t, 1, total, "账号级相位每账号每轮至多一个候选，候选总数=1")
	require.Greater(t, p.AccountFreshnessUpperBound(7, ""), time.Duration(0),
		"冻结后 freshness 链消费的账号级上界必须 > 0（否则阈值退化为基线）")
}

// RunOnce 未进入账号级相位（probe 开关关闭）时不得冻结（门禁语义）。
func TestHealthRecoveryRunOnceProbeDisabledDoesNotFreezeFreshnessUpperBound(t *testing.T) {
	acc := parkedBreakerAccount(7, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: acc},
		candidates: []*Account{acc},
	}
	p := newProbeService(t, false, nil, repo)

	p.RunOnce(context.Background())

	_, frozen := p.frozenCandidateBoundOK(7, "")
	require.False(t, frozen, "probe 开关关闭时不得进入账号级相位，不得冻结")
	require.Equal(t, time.Duration(0), p.AccountFreshnessUpperBound(7, ""))
}

// 恢复路径（探测连续成功达标 → ClearTempUnschedulable 成功 → clearCandidateBound）后
// 回到未冻结态：AccountFreshnessUpperBound == 0，下一轮进入调度重新冻结。
func TestHealthRecoveryProbeRecoveryClearsFrozenFreshnessUpperBound(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	acc := parkedBreakerAccount(7, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: acc},
		candidates: []*Account{acc},
	}
	p := newProbeService(t, true, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })
	setFastProbeRetry(p)

	// C1：首次成功仅计数并保持停调，异步补探在同一 sweep 内补齐第二次成功 → 清除停调
	// 并 clearCandidateBound。
	p.RunOnce(context.Background())
	p.wg.Wait()

	require.Equal(t, 1, repo.clearTempIfReasonCalls, "连续 2 次成功后应清除 temp-unschedulable")
	_, frozen := p.frozenCandidateBoundOK(7, "")
	require.False(t, frozen, "恢复成功后账号级冻结上界必须被清除（回到未冻结态）")
	require.Equal(t, time.Duration(0), p.AccountFreshnessUpperBound(7, ""),
		"未冻结态下 freshness 链读到的账号级上界为 0（走基线，等待下一轮重新冻结）")
}
