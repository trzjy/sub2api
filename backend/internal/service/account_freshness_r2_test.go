//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR2ProbeObservationStats_Wired 验证 R2 触发观测埋点与只读查询面：实际探测路径
// （probeOneTokenHarbor，经 override 命中）累计「发送数 / 成功数」，并经
// GetProbeObservationStats 按账号查得。
func TestR2ProbeObservationStats_Wired(t *testing.T) {
	p := NewAccountHealthRecoveryProbeService(nil, nil, nil, nil, nil, nil)
	p.SetTokenHarborProbeOverride(func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
		return ProbeOutcomeSuccess, nil
	})

	// 模型级候选探测一次：应累计 1 发送 + 1 成功。
	p.probeOneTokenHarbor(context.Background(), &Account{ID: 42}, tokenHarborCandidate{modelKey: "gpt-4"})
	stats, ok := p.GetProbeObservationStats(42)
	require.True(t, ok)
	require.Equal(t, int64(1), stats.Sends)
	require.Equal(t, int64(1), stats.Success)
	require.Equal(t, int64(0), stats.BudgetRejects)
	require.Equal(t, int64(0), stats.FreeTier429)
	require.Equal(t, int64(0), stats.Unclassified)

	// 免费档 429 分类计数。
	p.SetTokenHarborProbeOverride(func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
		return ProbeOutcomeFreeTier429, nil
	})
	p.probeOneTokenHarbor(context.Background(), &Account{ID: 42}, tokenHarborCandidate{modelKey: "gpt-4"})
	stats, _ = p.GetProbeObservationStats(42)
	require.Equal(t, int64(2), stats.Sends)
	require.Equal(t, int64(1), stats.Success)
	require.Equal(t, int64(1), stats.FreeTier429)
}

// TestR2ProbeObservationStats_UnclassifiedAndUnknown 未分类结果计入 Unclassified；
// 未观测账号返回 false（查询面边界）。
func TestR2ProbeObservationStats_UnclassifiedAndUnknown(t *testing.T) {
	p := NewAccountHealthRecoveryProbeService(nil, nil, nil, nil, nil, nil)
	p.SetTokenHarborProbeOverride(func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
		return ProbeOutcomeUnclassified, nil
	})
	p.probeOneTokenHarbor(context.Background(), &Account{ID: 7}, tokenHarborCandidate{modelKey: "gpt-3"})
	stats, ok := p.GetProbeObservationStats(7)
	require.True(t, ok)
	require.Equal(t, int64(1), stats.Sends)
	require.Equal(t, int64(1), stats.Unclassified)

	// 账号 99 从未观测 -> 查询返回 false。
	_, ok99 := p.GetProbeObservationStats(99)
	require.False(t, ok99, "未观测账号查询应返回 false")
}

// TestR2ProbeObservationStats_AccountLevel 账号级（无模型键）候选的计数同样按账号累计。
func TestR2ProbeObservationStats_AccountLevel(t *testing.T) {
	p := NewAccountHealthRecoveryProbeService(nil, nil, nil, nil, nil, nil)
	p.SetTokenHarborProbeOverride(func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
		return ProbeOutcomeSuccess, nil
	})
	p.probeOneTokenHarbor(context.Background(), &Account{ID: 11}, tokenHarborCandidate{modelKey: "", accountLevel: true})
	stats, ok := p.GetProbeObservationStats(11)
	require.True(t, ok)
	require.Equal(t, int64(1), stats.Sends)
	require.Equal(t, int64(1), stats.Success)
}
