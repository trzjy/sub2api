//go:build unit

package service

// 本文件覆盖 CURSOR-PROBE-001-A3 的拒钥处理：
//  1. 普通路径 defer 命中 *CursorKeyRejectedError → 触发永久停用回调 + 产出可换号错误
//     （NextAccountRetry / account scope / 独立 reason cursor_key_rejected），断言 reason
//     **不是** CursorUpstreamAccountFailureReason；
//  2. 非拒钥错误（上游内部错误）不触发拒钥回调（不误伤），且走原有账号故障 reason；
//  3. 已发布正文字节 → 不换号（既有边界保留），但永久停用回调仍触发（与发布无关）；
//  4. 续轮路径命中拒钥 → 同样触发永久停用回调，且重建哨兵 reason 为 cursor_key_rejected；
//  5. ProbeAgentRun 变参预算：缺省 16、传 ≤0 回落 16、传 256 = 256。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cursorKeyRejectedStreamBody 构造一条以"拒付模板"文本帧收尾、随后 EndStream 的上游流。
// withBody=true 时先发一帧正常正文，用于验证已发布字节后不得换号。
func cursorKeyRejectedStreamBody(t *testing.T, withBody bool) []byte {
	t.Helper()
	var out []byte
	if withBody {
		out = append(out, cursorTextDeltaFrame(t, "Hi")...)
	}
	out = append(out, cursorTextDeltaFrame(t, cursorKeyRejectedTemplate+" (1792310906551-b924f5ef)")...)
	out = append(out, cursorTestEndStream(t, "{}")...)
	return out
}

// TestCursorKeyRejectedNoBodySwitchesAccountAndDisables 覆盖 1：
// 普通路径命中拒钥 + 未发布字节 → 替换为可换号 failover（account scope / NextAccountRetry /
// cursor_key_rejected），且永久停用回调恰好调用一次，reason 不得复用账号故障 reason。
func TestCursorKeyRejectedNoBodySwitchesAccountAndDisables(t *testing.T) {
	for _, stream := range []bool{true, false} {
		svc, c := newCursorRLStopService(t, cursorKeyRejectedStreamBody(t, false))

		var disabled cursorRLStopHandlerCall
		svc.cursorExecution.SetAccountKeyRejectedHandler(disabled.fn)

		acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
		_, err := executeCursorForTest(t, svc, context.Background(), c, acc,
			cursorRLStopRequestBody(stream), "cursor-sonnet", stream, time.Now())
		require.Error(t, err, "stream=%v", stream)

		var fe *UpstreamFailoverError
		require.ErrorAs(t, err, &fe, "stream=%v：拒钥未发布字节必须换号", stream)
		require.Equal(t, GatewayFailureScopeAccount, fe.Scope, "stream=%v", stream)
		require.Equal(t, NextAccountRetry, fe.NextAccountAction, "stream=%v", stream)
		require.Equal(t, cursorKeyRejectedReason, fe.Reason, "stream=%v：必须使用独立 reason", stream)
		require.NotEqual(t, CursorUpstreamAccountFailureReason, fe.Reason,
			"stream=%v：不得复用账号故障 reason（否则会写探针 marker）", stream)
		require.True(t, fe.ShouldRetryNextAccount(), "stream=%v", stream)
		require.False(t, fe.RetryableOnSameAccount, "stream=%v：拒钥不得同账号重试", stream)

		// A8 卡契约：换号不得丢弃原始拒钥错误链 —— sentinel 与 TurnID 必须仍在链上，
		// 供对外映射 CUR-EXEC-12 与既有 e2e 契约 TestCursorProtocolRejectedKeyFailsClosed 使用。
		require.ErrorIs(t, err, ErrCursorKeyRejectedUpstream,
			"stream=%v：换号后必须保留拒钥 sentinel", stream)
		var keyErr *CursorKeyRejectedError
		require.ErrorAs(t, err, &keyErr, "stream=%v：换号后必须保留 *CursorKeyRejectedError", stream)
		require.Equal(t, "1792310906551-b924f5ef", keyErr.TurnID,
			"stream=%v：换号后 TurnID 不得丢失", stream)

		n, aid, _ := disabled.snapshot()
		require.Equal(t, 1, n, "stream=%v：拒钥永久停用回调必须恰好一次", stream)
		require.Equal(t, int64(1), aid, "stream=%v", stream)
	}
}

// TestCursorKeyRejectedHandlerNotTriggeredForOtherErrors 覆盖 2：
// 上游内部错误（非拒钥）走普通路径 → 拒钥回调不得触发（不误伤），且 reason 仍是账号故障 reason。
func TestCursorKeyRejectedHandlerNotTriggeredForOtherErrors(t *testing.T) {
	svc, c := newCursorRLStopService(t, cursorInternalErrorStreamBody(t, false))

	var disabled cursorRLStopHandlerCall
	svc.cursorExecution.SetAccountKeyRejectedHandler(disabled.fn)

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	_, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(false), "cursor-sonnet", false, time.Now())
	require.Error(t, err)

	var fe *UpstreamFailoverError
	require.ErrorAs(t, err, &fe)
	require.Equal(t, CursorUpstreamAccountFailureReason, fe.Reason, "内部错误必须走账号故障 reason，而非拒钥 reason")

	n, _, _ := disabled.snapshot()
	require.Equal(t, 0, n, "非拒钥错误不得触发拒钥永久停用回调")
}

// TestCursorKeyRejectedWithBodyKeepsOriginalErrorAndDisables 覆盖 3：
// 本 HTTP 已先写出正常正文、随后命中拒付模板 → 不换号（原文仍是 *CursorKeyRejectedError，
// 保留 sentinel 供对外映射 CUR-EXEC-12），但永久停用回调仍须触发（与发布字节无关）。
func TestCursorKeyRejectedWithBodyKeepsOriginalErrorAndDisables(t *testing.T) {
	svc, c := newCursorRLStopService(t, cursorKeyRejectedStreamBody(t, true))

	var disabled cursorRLStopHandlerCall
	svc.cursorExecution.SetAccountKeyRejectedHandler(disabled.fn)

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	_, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(true), "cursor-sonnet", true, time.Now())
	require.Error(t, err)

	// 已发布正文：不换号，错误原样（仍是 *CursorKeyRejectedError）。
	var fe *UpstreamFailoverError
	require.False(t, errors.As(err, &fe), "已发布正文不得 mid-stream 换号")
	var keyErr *CursorKeyRejectedError
	require.ErrorAs(t, err, &keyErr, "已发布正文错误必须是 *CursorKeyRejectedError")
	require.ErrorIs(t, err, ErrCursorKeyRejectedUpstream, "仍须保留拒钥 sentinel 供对外映射 CUR-EXEC-12")

	// 永久停用回调仍须触发（与发布字节无关）。
	n, _, _ := disabled.snapshot()
	require.Equal(t, 1, n, "已发布正文的拒钥仍须永久停用")
}

// TestCursorResumeKeyRejectedReturnsRebuildAndDisables 覆盖 4：
// 续轮 serve 命中拒付模板 + 本 HTTP 未发布字节 → 可重建哨兵（reason=cursor_key_rejected），
// 且永久停用回调恰好调用一次。
func TestCursorResumeKeyRejectedReturnsRebuildAndDisables(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 101, IDField: []byte("exec-101")}}
	svc, body := freshResumeQuotaFixture(t, cursorFullChainScript{
		calls:     calls,
		afterText: cursorKeyRejectedTemplate + " (1792310906551-b924f5ef)",
		usage:     [4]uint64{12601, 15, 128, 0},
	})

	var disabled cursorRLStopHandlerCall
	svc.SetAccountKeyRejectedHandler(disabled.fn)

	w, result, err := resumeSession(t, svc, body)
	require.Error(t, err)
	require.Nil(t, result, "失败不得返回可计费结果")

	var rebuild *CursorResumeRebuildError
	require.ErrorAs(t, err, &rebuild, "未发布字节的拒钥必须包装为可换号重建哨兵")
	require.Equal(t, int64(1), rebuild.DeadAccountID)
	require.Equal(t, cursorKeyRejectedReason, rebuild.Reason, "续轮拒钥必须用独立 reason")
	require.NotEqual(t, CursorUpstreamAccountFailureReason, rebuild.Reason, "不得复用账号故障 reason")
	require.Empty(t, w.Body.String(), "本 HTTP 未发布任何语义字节")

	n, accountID, _ := disabled.snapshot()
	require.Equal(t, 1, n, "续轮拒钥永久停用回调必须恰好一次")
	require.Equal(t, int64(1), accountID)
}

// TestResolveProbeMaxTokens 覆盖 5：变参预算解析口径。
func TestResolveProbeMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		in   []int
		want int
	}{
		{"缺省", nil, 16},
		{"空切片", []int{}, 16},
		{"显式 16", []int{16}, 16},
		{"传 0 回落 16", []int{0}, 16},
		{"传负数回落 16", []int{-5}, 16},
		{"显式 256", []int{256}, 256},
		{"显式 48", []int{48}, 48},
		{"显式 1", []int{1}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, resolveProbeMaxTokens(tc.in...), "in=%v", tc.in)
		})
	}
}

// TestProbeAgentRunVariadicDefaultsAndExplicit 覆盖 5（端到端）：
// 既有两个调用方均不传预算 → 缺省 16 不应 panic；显式传 256 也应正常出站并解析出非空文本。
func TestProbeAgentRunVariadicDefaultsAndExplicit(t *testing.T) {
	svc, _ := newCursorRLStopService(t, cursorOKStreamBody(t))
	exec := svc.cursorExecution

	account := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")

	// 缺省预算（与 account_test_service.go / request_codes_test.go 同形：不传第三个参数）。
	status, _, text, err := exec.ProbeAgentRun(context.Background(), account, "upstream-sonnet")
	require.NoError(t, err, "缺省预算应正常出站并解析")
	require.Equal(t, 200, status)
	require.NotEmpty(t, text, "探针应解析出非空文本")

	// 显式传 256（探针侧口径）。
	status2, _, text2, err2 := exec.ProbeAgentRun(context.Background(), account, "upstream-sonnet", 256)
	require.NoError(t, err2)
	require.Equal(t, 200, status2)
	require.NotEmpty(t, text2)
}
