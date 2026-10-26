//go:build unit

package service

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDailyRefreshLimit_ClassifiedAsDailyLimit pins the 2026-10-27 fix:
// shareai 上游对每个 key 每日只允许刷新 5 次（8:00 重置），quota_key_reconcile 周期 5 分钟
// 必然撞上该上限。此前该文案落 UpstreamUnknown（真失败哨兵），导致 ops 面板 jobScore
// 长期被扣、并掩盖真正的任务故障。
//
// 生产实测文案（quota_key id=7 的 last_refresh_error 原文）：
//
//	"shareai: upstream code=1 unknown result: 当日刷新次数达到上限(5次)，将在每日8:00重置，还需6小时48分钟"
func TestDailyRefreshLimit_ClassifiedAsDailyLimit(t *testing.T) {
	err := classifyUpstreamError("当日刷新次数达到上限(5次)，将在每日8:00重置，还需6小时48分钟")

	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe), "must be *UpstreamPullError")
	require.Equal(t, UpstreamDailyRefreshLimit, upe.Kind,
		"每日刷新次数用尽必须归类为可预期节流，不得落真失败")
	require.True(t, errors.Is(err, ErrUpstreamDailyRefreshLimit),
		"必须能被 errors.Is 识别为每日刷新上限哨兵")
	require.False(t, errors.Is(err, ErrUpstreamKeyBlocked),
		"不得同时被当作未知失败（真失败）")
	require.False(t, errors.Is(err, ErrUpstreamKeyExpired),
		"每日节流不是终态，不得落 expired")
	require.False(t, errors.Is(err, ErrUpstreamCooldown),
		"每日上限与短窗冷却的恢复窗口不同（天 vs 分钟），须分开归类")

	// msg 原文必须逐字保留（handler 逐字回显给用户）。
	require.Contains(t, upe.Message(), "当日刷新次数达到上限")
}

// TestDailyRefreshLimit_ExhaustedDimensionUnchanged pins the hard constraint:
// 每日节流不是已确认终态，不得把 key 标 exhausted（该维度只认已确认终态）。
func TestDailyRefreshLimit_ExhaustedDimensionUnchanged(t *testing.T) {
	err := classifyUpstreamError("当日刷新次数达到上限(5次)，将在每日8:00重置")
	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe))
	require.NotEqual(t, UpstreamExpired, upe.Kind,
		"每日节流不得走 expired 分支（会标 exhausted 终态维度）")
	require.NotEqual(t, UpstreamUnknown, upe.Kind,
		"每日节流不得走 unknown 分支（会记为真失败）")
}

// TestDailyRefreshLimit_NegativeAndUnrelatedMessages pins the anti-false-positive
// guard: 否定句与无关文案不得被误判为每日节流。
func TestDailyRefreshLimit_NegativeAndUnrelatedMessages(t *testing.T) {
	cases := []string{
		"未达到刷新次数上限",
		"系统当前繁忙，请稍后再试",
		"刷新失败",
		"每5分钟内仅可刷新1次", // 短窗冷却，仍归 cooldown
		"密钥已过期",       // 终态，仍归 expired
	}
	for _, msg := range cases {
		err := classifyUpstreamError(msg)
		var upe *UpstreamPullError
		require.True(t, errors.As(err, &upe), msg)
		require.NotEqual(t, UpstreamDailyRefreshLimit, upe.Kind,
			"msg %q 不得被判为每日刷新上限", msg)
	}
}

// TestDailyRefreshLimit_RealFailureStillUnknown pins the hard constraint:
// 真失败（网络错误、鉴权失败、上游 5xx 之外的未知结果）仍须归真失败，不得被放宽。
func TestDailyRefreshLimit_RealFailureStillUnknown(t *testing.T) {
	err := classifyUpstreamError("系统当前繁忙，请稍后再试")
	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe))
	require.Equal(t, UpstreamUnknown, upe.Kind, "真失败仍须归 unknown")
	require.False(t, errors.Is(err, ErrUpstreamDailyRefreshLimit))
}
