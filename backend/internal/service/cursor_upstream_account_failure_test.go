//go:build unit

package service

// 本文件覆盖「cursor 上游对本账号返回非业务码终态错误」的处理：
//  1. 未发布正文 + 未交付工具批 → 换成可换号错误（502 / account scope /
//     CursorUpstreamAccountFailureReason / NextAccountRetry），请求由既有 failover 循环
//     换到本组下一个 cursor 账号，而不是把 502 直接甩给用户；
//  2. 已发布正文 → 不换号，错误原样，保持「禁止 mid-stream 换号」锁定项；
//  3. 业务码（ERROR_ 前缀）不受影响，语义仍由各自专门分支处理；
//  4. isCursorBusinessErrorCode 的边界。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// cursorInternalErrorPayload 复刻 2026-11-03 04:50–04:54 生产实测的上游报文：
// 无 details[].debug.error（业务码空），顶层 code=internal，message 是上游 Node 侧
// 的 TypeError 原文。
const cursorInternalErrorPayload = `{"error":{"code":"internal",` +
	`"message":"Cannot read properties of undefined (reading 'toString')"}}`

// cursorInternalErrorStreamBody 构造以 internal 终态错误收尾的上游流。
// withBody=true 时先发一帧正文，用于验证已发布字节后不得换号。
func cursorInternalErrorStreamBody(t *testing.T, withBody bool) []byte {
	t.Helper()
	var out []byte
	if withBody {
		out = append(out, cursorTextDeltaFrame(t, "Hi")...)
	}
	out = append(out, cursorTestEndStream(t, cursorInternalErrorPayload)...)
	return out
}

func TestCursorInternalErrorNoBodySwitchesAccount(t *testing.T) {
	for _, stream := range []bool{true, false} {
		svc, c := newCursorRLStopService(t, cursorInternalErrorStreamBody(t, false))

		acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
		res, err := executeCursorForTest(t, svc, context.Background(), c, acc,
			cursorRLStopRequestBody(stream), "cursor-sonnet", stream, time.Now())
		require.Error(t, err, "stream=%v", stream)
		require.Nil(t, res)

		var fe *UpstreamFailoverError
		require.ErrorAs(t, err, &fe, "stream=%v：账号级上游故障必须换号", stream)
		require.Equal(t, 502, fe.StatusCode)
		require.Equal(t, CursorUpstreamAccountFailureReason, fe.Reason)
		require.Equal(t, GatewayFailureScopeAccount, fe.Scope)
		require.Equal(t, NextAccountRetry, fe.NextAccountAction)
		require.Equal(t, 502, fe.ClientStatusCode)
		require.True(t, fe.ShouldRetryNextAccount())
		// 不停号回调：停号由 handler 侧的 TempUnscheduleRetryableError 统一做（临时停调，
		// 不是永久摘号），service 层不得重复处理。
		require.False(t, fe.RetryableOnSameAccount, "账号级故障不得走同账号重试，否则用户要再等一整个上游超时")
	}
}

func TestCursorInternalErrorWithBodyKeepsOriginalError(t *testing.T) {
	svc, c := newCursorRLStopService(t, cursorInternalErrorStreamBody(t, true))

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	res, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(true), "cursor-sonnet", true, time.Now())
	require.Error(t, err)
	require.Nil(t, res)

	require.True(t, errors.Is(err, ErrCursorUpstreamStream), "已发布正文必须原样返回上游错误")
	var fe *UpstreamFailoverError
	require.False(t, errors.As(err, &fe), "已发布正文不得 mid-stream 换号")
}

func TestCursorBusinessCodeDoesNotEnterAccountFailureBranch(t *testing.T) {
	// 业务码（ERROR_ 前缀）没有专门处理分支时，仍按原路径返回上游错误，绝不被
	// 账号级换号分支吞掉——否则会把「模型名错」这类请求问题当成账号故障，把整池账号冷却。
	svc, c := newCursorRLStopService(t,
		cursorRLStopStreamBody(t, "ERROR_BAD_MODEL_NAME", false))

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	_, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(false), "cursor-sonnet", false, time.Now())
	require.Error(t, err)

	var fe *UpstreamFailoverError
	require.False(t, errors.As(err, &fe), "业务码不得走账号级换号分支")
}

func TestIsCursorBusinessErrorCode(t *testing.T) {
	cases := map[string]bool{
		"ERROR_PRO_USER_RATE_LIMIT_EXCEEDED": true,
		"ERROR_UNAUTHORIZED":                 true,
		"ERROR_BAD_MODEL_NAME":               true,
		"error_bad_model_name":               true, // 大小写不敏感
		"internal":                           false,
		"unavailable":                        false,
		"unknown":                            false,
		"deadline_exceeded":                  false,
		"":                                   false,
	}
	for code, want := range cases {
		require.Equal(t, want, isCursorBusinessErrorCode(code), "code=%q", code)
	}
}

// cursorCooldownRepoStub 只记录临时停调写入的 (until, reason)，其余方法不应被调用。
type cursorCooldownRepoStub struct {
	AccountRepository // 嵌入接口：未实现的方法会 panic（不应被调用）

	calls  int
	until  time.Time
	reason string
}

func (r *cursorCooldownRepoStub) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.calls++
	r.until = until
	r.reason = reason
	return nil
}

// cursor 账号级上游故障必须一次即停号冷却 5 分钟——且**不依赖** RetryableOnSameAccount，
// 因为这个错误明确不做同账号重试（重试只会让用户再等一整个上游超时）。
func TestTempUnscheduleCursorUpstreamFailure(t *testing.T) {
	t.Run("cursor 账号级故障立即停号 5 分钟", func(t *testing.T) {
		repo := &cursorCooldownRepoStub{}
		svc := &GatewayService{accountRepo: repo}
		before := time.Now()

		svc.TempUnscheduleRetryableError(context.Background(), 362, &UpstreamFailoverError{
			StatusCode:        502,
			Scope:             GatewayFailureScopeAccount,
			Reason:            CursorUpstreamAccountFailureReason,
			NextAccountAction: NextAccountRetry,
		})

		require.Equal(t, 1, repo.calls)
		require.Contains(t, repo.reason, "cursor upstream account failure")
		require.WithinDuration(t, before.Add(cursorUpstreamFailureCooldown), repo.until, 5*time.Second,
			"冷却窗口必须是 5 分钟")
	})

	t.Run("请求级瞬时故障仍然不写账号状态", func(t *testing.T) {
		repo := &cursorCooldownRepoStub{}
		svc := &GatewayService{accountRepo: repo}

		svc.TempUnscheduleRetryableError(context.Background(), 362, &UpstreamFailoverError{
			StatusCode:             502,
			Reason:                 CursorUpstreamAccountFailureReason,
			RequestScopedTransient: true,
		})

		require.Zero(t, repo.calls, "请求级瞬时故障与账号无关，不得停号")
	})

	t.Run("非 cursor 原因的 502 且不做同账号重试时保持原行为", func(t *testing.T) {
		repo := &cursorCooldownRepoStub{}
		svc := &GatewayService{accountRepo: repo}

		svc.TempUnscheduleRetryableError(context.Background(), 362, &UpstreamFailoverError{
			StatusCode:             502,
			Reason:                 GatewayFailureReason("something_else"),
			RetryableOnSameAccount: false,
		})

		require.Zero(t, repo.calls, "不得扩大既有停号条件")
	})
}

// cursorUpstreamAccountFailureLogField 抓出 cursor_upstream_account_failure 阶段日志里
// 指定字段的值（用于断言本次纯观测补点）。
func cursorUpstreamAccountFailureLogField(t *testing.T, logs *observer.ObservedLogs, key string) string {
	t.Helper()
	entries := logs.FilterField(zap.String("phase", "cursor_upstream_account_failure"))
	require.Positive(t, entries.Len(), "必须记 cursor_upstream_account_failure 日志")
	val, _ := entries.All()[0].ContextMap()[key].(string)
	return val
}

// TestCursorUpstreamAccountFailureLogsUpstreamMessage 覆盖本卡核心补点：
// cursor_upstream_account_failure 日志透传上游已脱敏 message（upstream_message 字段），
// 让「上游自身故障」与「我们的请求触发上游 bug」可区分。
func TestCursorUpstreamAccountFailureLogsUpstreamMessage(t *testing.T) {
	svc, c := newCursorRLStopService(t, cursorInternalErrorStreamBody(t, false))

	ctx, logs := cursorPhaseObserverCtx(t, "req-msg-1")
	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	_, err := executeCursorForTest(t, svc, ctx, c, acc,
		cursorRLStopRequestBody(false), "cursor-sonnet", false, time.Now())
	require.Error(t, err)

	got := cursorUpstreamAccountFailureLogField(t, logs, "upstream_message")
	want := "Cannot read properties of undefined (reading 'toString')"
	require.Equal(t, want, got, "upstream_message 必须等于上游脱敏后的 message 原文")
}

// TestCursorUpstreamAccountFailureLogsTruncatedMessage 覆盖超长 message 被截断：
// 上游回吐超长原文时，upstream_message 必须被截断（与既有 upstream_error_preview 同口径
// truncateOpenAIWSLogValue(…, 240)），不得整段落盘。
func TestCursorUpstreamAccountFailureLogsTruncatedMessage(t *testing.T) {
	longMsg := strings.Repeat("X", 400)
	payload := `{"error":{"code":"internal","message":"` + longMsg + `"}}`
	svc, c := newCursorRLStopService(t, cursorTestEndStream(t, payload))

	ctx, logs := cursorPhaseObserverCtx(t, "req-msg-long")
	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	_, err := executeCursorForTest(t, svc, ctx, c, acc,
		cursorRLStopRequestBody(false), "cursor-sonnet", false, time.Now())
	require.Error(t, err)

	got := cursorUpstreamAccountFailureLogField(t, logs, "upstream_message")
	require.NotEqual(t, longMsg, got, "超长 message 不得整段落盘")
	require.True(t, strings.HasSuffix(got, "..."), "截断后应带省略号后缀")
	// truncateOpenAIWSLogValue(value, 240) → 内容 240 + "..." = 243 字符（与既有口径一致）。
	require.LessOrEqual(t, len(got), 240+3, "截断后不得超过 240+省略号长度")
}

// TestCursorUpstreamAccountFailureLogsEmptyMessage 覆盖「取不到 message」边界：
// 上游回吐空 message 时，upstream_message 如实记录，不填 "unknown" 之类占位符 ——
// 「没有」与「有但是空的」要能区分开（纯观测，行为不变）。
func TestCursorUpstreamAccountFailureLogsEmptyMessage(t *testing.T) {
	payload := `{"error":{"code":"internal","message":""}}`
	svc, c := newCursorRLStopService(t, cursorTestEndStream(t, payload))

	ctx, logs := cursorPhaseObserverCtx(t, "req-msg-empty")
	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	_, err := executeCursorForTest(t, svc, ctx, c, acc,
		cursorRLStopRequestBody(false), "cursor-sonnet", false, time.Now())
	require.Error(t, err)

	// 空 message 经 truncateOpenAIWSLogValue 归一为空占位 "-"；核心是**不回填业务占位符**。
	got := cursorUpstreamAccountFailureLogField(t, logs, "upstream_message")
	require.NotContains(t, got, "unknown", "不得回填 unknown 之类占位符")
	require.NotContains(t, got, "N/A", "不得回填 N/A 之类占位符")
}

// TestTempUnscheduleCursorUpstreamFailureFlapCooldown 覆盖「集体抽风」分支：
// 同一请求内已连续失败 >= CursorUpstreamFlapThreshold 个不同账号后，
// 停调时长必须从 5 分钟降到 CursorUpstreamFlapCooldown（30 秒），
// 且 reason 必须逐字节等于 flap 文案——运维要靠它区分「集体抽风」与「单体故障」。
func TestTempUnscheduleCursorUpstreamFailureFlapCooldown(t *testing.T) {
	t.Run("抽风标记为真 → 实际写入 30 秒冷却 + flap reason", func(t *testing.T) {
		repo := &cursorCooldownRepoStub{}
		svc := &GatewayService{accountRepo: repo}
		before := time.Now()

		svc.TempUnscheduleRetryableError(context.Background(), 362, &UpstreamFailoverError{
			StatusCode:                 502,
			Scope:                      GatewayFailureScopeAccount,
			Reason:                     CursorUpstreamAccountFailureReason,
			NextAccountAction:          NextAccountRetry,
			CursorUpstreamFlapDetected: true,
		})

		require.Equal(t, 1, repo.calls)
		require.WithinDuration(t, before.Add(CursorUpstreamFlapCooldown), repo.until, 5*time.Second,
			"集体抽风时的冷却窗口必须是 30 秒")
		require.Equal(t, "cursor upstream account failure (upstream flap, auto temp-unschedule 30s)", repo.reason)
	})
}

// TestTempUnscheduleCursorUpstreamFailureSingleFailureCooldown 覆盖「单体故障」分支：
// 抽风标记为假时，停调时长仍是 5 分钟，且 reason 必须与既有文案逐字节一致
// （回归保护：文案是运维与数据分析的口径，不得被抽风分支带偏）。
func TestTempUnscheduleCursorUpstreamFailureSingleFailureCooldown(t *testing.T) {
	t.Run("抽风标记为假 → 仍是 5 分钟冷却 + 原 reason 逐字节不变", func(t *testing.T) {
		repo := &cursorCooldownRepoStub{}
		svc := &GatewayService{accountRepo: repo}
		before := time.Now()

		svc.TempUnscheduleRetryableError(context.Background(), 362, &UpstreamFailoverError{
			StatusCode:                 502,
			Scope:                      GatewayFailureScopeAccount,
			Reason:                     CursorUpstreamAccountFailureReason,
			NextAccountAction:          NextAccountRetry,
			CursorUpstreamFlapDetected: false,
		})

		require.Equal(t, 1, repo.calls)
		require.WithinDuration(t, before.Add(cursorUpstreamFailureCooldown), repo.until, 5*time.Second,
			"单体故障的冷却窗口必须是 5 分钟")
		require.Equal(t, "cursor upstream account failure (auto temp-unschedule 5m)", repo.reason)
	})
}

// TestTempUnscheduleCursorUpstreamFailureProtocolPollutionCooldown 覆盖协议污染路径：
// 该路径没有上游 code/message，与「上游集体抽风」无关，必须仍是 5 分钟 + 原 reason。
// 这里直接调用既有 helper（未被抽风改动触碰）与协议污染入口各验一次。
func TestTempUnscheduleCursorUpstreamFailureProtocolPollutionCooldown(t *testing.T) {
	t.Run("协议污染路径直接调既有 helper → 仍是 5 分钟 + 原 reason", func(t *testing.T) {
		repo := &cursorCooldownRepoStub{}
		before := time.Now()

		tempUnscheduleCursorUpstreamFailure(context.Background(), repo, 362, "[cursor-exec:inline_tool]")

		require.Equal(t, 1, repo.calls)
		require.WithinDuration(t, before.Add(cursorUpstreamFailureCooldown), repo.until, 5*time.Second,
			"协议污染路径的冷却窗口必须仍是 5 分钟")
		require.Equal(t, "cursor upstream account failure (auto temp-unschedule 5m)", repo.reason)
	})

	t.Run("协议污染入口 → 仍是 5 分钟 + 原 reason", func(t *testing.T) {
		repo := &cursorCooldownRepoStub{}
		svc := &OpenAIGatewayService{accountRepo: repo}
		before := time.Now()

		svc.tempUnscheduleCursorProtocolPollution(context.Background(), 362, "inline_tool")

		require.Equal(t, 1, repo.calls)
		require.WithinDuration(t, before.Add(cursorUpstreamFailureCooldown), repo.until, 5*time.Second,
			"协议污染路径的冷却窗口必须仍是 5 分钟")
		require.Equal(t, "cursor upstream account failure (auto temp-unschedule 5m)", repo.reason)
	})
}
