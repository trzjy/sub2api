//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// D4c 定向测试：precise_reset 标记（精确恢复信号）的持久化与保留。
//   - SET 路径：有上游精准恢复信号 → true；D5 无信号哨兵分支 → false；
//   - 探测链写入口更新（429 / 不可分类 / 账号级成功）保留既有标记不被清除；
//   - 模型级成功走恢复迁移（整条清除），该路径下不存在"标记被清除"问题。

func TestPreciseResetPersistedOnTokenHarborSet(t *testing.T) {
	// 有上游精准恢复信号：正文给出下一滚动周期起点 → precise_reset=true。
	repoSignal := &tokenHarborAccountRepoStub{}
	svcSignal := &RateLimitService{accountRepo: repoSignal}
	svcSignal.HandleUpstreamError(
		context.Background(),
		tokenHarborDeepSeekAccount(),
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(tokenHarborFreeExhaustedBodyAt(time.Now().Add(48*time.Hour))),
		"deepseek-v4.1-flash:free",
	)
	require.Len(t, repoSignal.modelRateLimitCalls, 1)
	callSignal := repoSignal.modelRateLimitCalls[0]
	require.Equal(t, "deepseek-v4.1-flash:free", callSignal.scope)
	require.NotNil(t, callSignal.precise, "有精准恢复信号的 SET 必须持久化 precise_reset")
	require.True(t, *callSignal.precise, "上游给出可解析恢复时刻 → precise_reset=true")

	// D5 无信号哨兵分支：仅类型码、无时间信号 → precise_reset=false（恢复时刻未知，
	// 由主动复探确认；前端不得按哨兵 reset_at 显示倒计时）。
	repoNoSignal := &tokenHarborAccountRepoStub{}
	svcNoSignal := &RateLimitService{accountRepo: repoNoSignal}
	svcNoSignal.HandleUpstreamError(
		context.Background(),
		tokenHarborDeepSeekAccount(),
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"type":"free_tier_limit_reached","code":"free_tier_limit_reached"}}`),
		"deepseek-v4.1-flash:free",
	)
	require.Len(t, repoNoSignal.modelRateLimitCalls, 1)
	callNoSignal := repoNoSignal.modelRateLimitCalls[0]
	require.Contains(t, callNoSignal.reason, "precise reset unknown")
	require.NotNil(t, callNoSignal.precise, "无信号分支同样必须持久化 precise_reset=false")
	require.False(t, *callNoSignal.precise, "无上游时间信号 → precise_reset=false")
	// 哨兵 reset_at 仍在（持续受限），只是不得被当作恢复时刻展示。
	require.Greater(t, callNoSignal.resetAt.Sub(time.Now()), 300*24*time.Hour)
}

func TestPreciseResetPreservedByProbeChainUpdates(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}
	const accountID int64 = 146
	const scope = "deepseek-v4.1-flash:free"
	ctx := context.Background()

	// 存量条目：无信号哨兵（precise_reset=false）+ 哨兵 reset_at。
	resetAt := time.Now().Add(300 * 24 * time.Hour)
	repo.presetEntry(accountID, scope, map[string]any{
		"rate_limited_at":     time.Now().UTC().Format(time.RFC3339),
		"rate_limit_reset_at": resetAt.UTC().Format(time.RFC3339),
		"reason":              tokenHarborFreeTierReasonPrefix + ": no signal",
		"precise_reset":       false,
	})

	// 1) 免费档 429 确认仍受限：只置 observed_at，precise_reset 保留。
	t1 := time.Now()
	require.NoError(t, svc.ApplyModelRateLimitObservation(ctx, accountID, scope, ModelRateLimitObservation{
		EventTime: t1,
		Outcome:   ProbeOutcomeFreeTier429,
	}))
	entry, ok := repo.entryFor(accountID, scope)
	require.True(t, ok)
	require.Equal(t, false, entry["precise_reset"], "429 更新不得清除 precise_reset")
	require.Equal(t, t1.UTC().Format(time.RFC3339Nano), entry[entryObservedAtKey])

	// 2) 不可分类结果（5xx/超时/传输错误等）：只置 attempted_at，precise_reset 保留。
	t2 := t1.Add(time.Minute)
	require.NoError(t, svc.ApplyModelRateLimitObservation(ctx, accountID, scope, ModelRateLimitObservation{
		EventTime: t2,
		Outcome:   ProbeOutcomeUnclassified,
	}))
	entry, ok = repo.entryFor(accountID, scope)
	require.True(t, ok)
	require.Equal(t, false, entry["precise_reset"], "不可分类更新不得清除 precise_reset")
	require.Equal(t, t2.UTC().Format(time.RFC3339Nano), entry[entryAttemptedAtKey])

	// 3) 账号级成功（无模型键，同一状态入口）：只置 observed_at，条目既有键保留。
	const accountScope = tokenHarborAccountLevelProbeScope
	repo.presetEntry(accountID, accountScope, map[string]any{
		"rate_limited_at": time.Now().UTC().Format(time.RFC3339),
		"attempted_at":    t2.UTC().Format(time.RFC3339),
	})
	t3 := t2.Add(time.Minute)
	require.NoError(t, svc.ApplyModelRateLimitObservation(ctx, accountID, accountScope, ModelRateLimitObservation{
		EventTime:    t3,
		Outcome:      ProbeOutcomeSuccess,
		AccountLevel: true,
	}))
	accountEntry, ok := repo.entryFor(accountID, accountScope)
	require.True(t, ok)
	require.Equal(t, t3.UTC().Format(time.RFC3339Nano), accountEntry[entryObservedAtKey])
	require.Equal(t, t2.UTC().Format(time.RFC3339), accountEntry[entryAttemptedAtKey],
		"账号级成功走读-改-写，不得丢弃条目既有键")

	// 4) 模型级成功 = 恢复迁移整条清除（既有语义）。该路径下条目整体不存在，
	//    不存在"标记被清除而限流仍在"的问题。
	t4 := t3.Add(time.Minute)
	require.NoError(t, svc.ApplyModelRateLimitObservation(ctx, accountID, scope, ModelRateLimitObservation{
		EventTime: t4,
		Outcome:   ProbeOutcomeSuccess,
	}))
	_, ok = repo.entryFor(accountID, scope)
	require.False(t, ok, "模型级成功必须幂等清除该 scope（恢复迁移既有语义）")
}
