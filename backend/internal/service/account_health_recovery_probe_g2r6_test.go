//go:build unit

// AC-G2R6 第7轮 must_fix 第2条（attempt 回写代际守卫）定向回归：补探失败回写 attempt 计数时，
// 若代际令牌 until 已被他人接管改写（必写更晚 until），仓储层 BumpTempUnschedulableReasonIfGeneration
// 代际失配返回 false → bumpProbeAttempts 返回 false，调用方（probeAccountWithReason）据此终止本轮
// 补探（不再补探该账号），接管者写入的 reason 保留。命名以 TestHealthProbe 前缀命中验证白名单
// `-run "TestHealthProbe|TestCodeBuddy|TestProbeInterval"`。
package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestHealthProbeAttemptBumpSkippedWhenUntilTakenOver 锁定 attempt 回写代际守卫（AC-G2R6 must_fix）：
// 补探失败回写 attempt 计数时，until 已被他人接管改写（必写更晚 until）→ 仓储层代际失配返回
// false → bumpProbeAttempts 返回 false，调用方据此终止本轮补探、保留接管者 reason。
func TestHealthProbeAttemptBumpSkippedWhenUntilTakenOver(t *testing.T) {
	const acct int64 = 9
	acc := parkedBreakerAccount(acct, 0)
	originalUntil := acc.TempUnschedulableUntil.Unix()
	// 模拟 DB 中该账号的 until 已被他人改写为更晚值（新停调接管）。
	repo := &probeRepoMock{
		accounts:     map[int64]*Account{acct: acc},
		storedUntils: map[int64]int64{acct: originalUntil + 100},
	}
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	p := newProbeService(t, true, 10, rl, repo)

	// 传入探测入队时读到的 until（原始令牌）：与 DB 当前更晚 until 失配 → 回写失败。
	bumped := p.bumpProbeAttempts(context.Background(), acct, 2, *acc.TempUnschedulableUntil)

	require.False(t, bumped, "代际失配（已被接管）时回写必须返回 false，终止本轮补探")
	require.Equal(t, 1, repo.bumpTempIfReasonCalls, "必须已尝试一次回写（BumpTempUnschedulableReasonIfGeneration）")
	// 接管者的 reason 未被本代回写覆盖：DB 行 until 仍为更晚值（storedUntils 未变）。
	require.Equal(t, originalUntil+100, repo.storedUntils[acct], "接管者写入的 until 必须保留（未被本代覆盖）")
}
