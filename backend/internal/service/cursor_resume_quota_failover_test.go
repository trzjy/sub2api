//go:build unit

package service

// 本文件覆盖 2026-11-04 用户裁定：**续轮中账号额度耗尽 → 自动换号 + 重建会话，用户无感**
// （docs-local/cursor-resume-quota-rebuild-failover-plan-20261104.md）。
//
// 定向用例：
//  1. 续轮 serve 命中额度哨兵 + 本 HTTP 未发布字节 → 返回 *CursorResumeRebuildError
//     （可重建哨兵，不是用户可见的 502），且停号回调恰好调用一次；
//  2. 同一额度哨兵但本 HTTP 已发布字节 → 不重建，原 error 返回（锁定项 3）；
//  3. Execute 准入断言：ctx 有重建标记 → 放行；claimsContinuation + historyComplete
//     无标记 → 放行（孤儿重建）；historyComplete=false → 仍 400。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// freshResumeQuotaFixture 构造"首 HTTP 交付工具批 → 会话 waiting_tools"的前置状态。
func freshResumeQuotaFixture(t *testing.T, script cursorFullChainScript) (*CursorExecutionService, string) {
	t.Helper()
	upstream := newCursorFullChainUpstream(script)
	svc := newCursorFullChainService(t, upstream)
	_, _, err := runFirstHTTP(context.Background(), svc, &cursorTestHandover{})
	require.NoError(t, err, "首 HTTP 必须正常交付工具批并跨 HTTP 存活")
	body := cursorFullChainResumeBody(script.calls, map[string]string{script.calls[0].CallID: "sunny, 25C"})
	return svc, body
}

// TestCursorResumeQuotaExhaustedReturnsRebuildSentinel 覆盖 1：
// 续轮 serve 命中「今日额度已用完」+ 本 HTTP 未发布任何字节 → 可重建哨兵 + 停号一次。
func TestCursorResumeQuotaExhaustedReturnsRebuildSentinel(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 101, IDField: []byte("exec-101")}}
	svc, body := freshResumeQuotaFixture(t, cursorFullChainScript{
		calls:     calls,
		afterText: cursorQuotaExhaustedLiveSample,
		usage:     [4]uint64{12601, 15, 128, 0},
	})

	var stopped cursorRLStopHandlerCall
	svc.SetAccountExhaustedHandler(stopped.fn)

	w, result, err := resumeSession(t, svc, body)
	require.Error(t, err)
	require.Nil(t, result, "失败不得返回可计费结果")

	var rebuild *CursorResumeRebuildError
	require.ErrorAs(t, err, &rebuild, "未发布字节的额度耗尽必须包装为可换号重建哨兵")
	require.Equal(t, int64(1), rebuild.DeadAccountID)
	require.Equal(t, CursorQuotaExhaustedReason, rebuild.Reason)
	require.ErrorIs(t, err, ErrCursorQuotaExhaustedInStream, "Unwrap 必须保留原错误供分类")
	require.Empty(t, w.Body.String(), "本 HTTP 未发布任何语义字节")

	n, accountID, reason := stopped.snapshot()
	require.Equal(t, 1, n, "停号回调必须恰好调用一次")
	require.Equal(t, int64(1), accountID)
	require.Contains(t, reason, "resume", "续轮路径的停号原因必须可区分")
}

// TestCursorResumeQuotaExhaustedPublishedKeepsOriginalError 覆盖 2：
// 本 HTTP 已先写出正常正文、随后命中额度哨兵 → 不重建，原错误返回（禁止流拼接腐化）。
func TestCursorResumeQuotaExhaustedPublishedKeepsOriginalError(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 101, IDField: []byte("exec-101")}}
	svc, body := freshResumeQuotaFixture(t, cursorFullChainScript{
		calls:         calls,
		afterLeadText: "working on it",
		afterText:     cursorQuotaExhaustedLiveSample,
		usage:         [4]uint64{12601, 15, 128, 0},
	})

	var stopped cursorRLStopHandlerCall
	svc.SetAccountExhaustedHandler(stopped.fn)

	w, result, err := resumeSession(t, svc, body)
	require.Error(t, err)
	require.Nil(t, result)

	var rebuild *CursorResumeRebuildError
	require.False(t, errors.As(err, &rebuild), "已发布字节后禁止换号重建")
	require.ErrorIs(t, err, ErrCursorQuotaExhaustedInStream)
	require.True(t, strings.Contains(w.Body.String(), "working on it"),
		"已发布正文必须真实写给客户端（证明重建判据针对本 HTTP 字节）")
	// 停号与是否重建无关：账号确实 exhausted，仍须停号一次。
	n, _, _ := stopped.snapshot()
	require.Equal(t, 1, n)
}

// admissionTestPlan 构造一个"声明续轮"的计划：results 非空、historyCallIDs 为空。
//
// historyComplete 由用例显式指定：true = 正文可独立重建（chat/messages/gemini 口径）；
// false = 历史不完整（Responses 只有 previous_response_id 的口径）。
func admissionTestPlan(historyComplete bool) *cursorContextPlan {
	return &cursorContextPlan{
		results:         []CursorToolResult{{CallID: "call_1", Text: []string{"ok"}}},
		historyComplete: historyComplete,
	}
}

// TestCursorExecuteAdmissionRebuildExceptions 覆盖 3：Execute 入口准入断言的三个分支。
func TestCursorExecuteAdmissionRebuildExceptions(t *testing.T) {
	svc, c := newCursorRLStopService(t, cursorRLStopStreamBody(t, cursorRLStopOtherCode, false))
	account := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")

	execute := func(t *testing.T, ctx context.Context, plan *cursorContextPlan) error {
		t.Helper()
		req := &CursorExecutionRequest{
			// Chat 留空（只给 model）：准入通过后会在转换阶段失败，便于证明"不是准入 400"。
			Chat:      &apicompat.ChatCompletionsRequest{Model: "cursor-sonnet"},
			Model:     "cursor-sonnet",
			Stream:    true,
			StartTime: time.Now(),
			Protocol:  cursorProtocolResponses,
			Plan:      plan,
		}
		_, err := svc.cursorExecution.Execute(ctx, c, account, req)
		return err
	}

	assertPassedAdmission := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err, "用例只证明准入放行，后续转换/鉴权可按既有语义失败")
		var resumeErr *CursorResumeError
		if errors.As(err, &resumeErr) {
			require.NotEqual(t, "unknown_association", resumeErr.Reason,
				"不得再以 unknown_association 拒绝可重建请求")
		}
	}

	t.Run("ctx_rebuild_marker_allows_incomplete_history", func(t *testing.T) {
		ctx := WithCursorRebuild(context.Background(), CursorRebuildInfo{
			DeadAccountID: 9,
			Reason:        CursorQuotaExhaustedReason,
		})
		assertPassedAdmission(t, execute(t, ctx, admissionTestPlan(false)))
	})

	t.Run("orphan_with_complete_history_allowed", func(t *testing.T) {
		assertPassedAdmission(t, execute(t, context.Background(), admissionTestPlan(true)))
	})

	t.Run("incomplete_history_still_400", func(t *testing.T) {
		err := execute(t, context.Background(), admissionTestPlan(false))
		var resumeErr *CursorResumeError
		require.ErrorAs(t, err, &resumeErr)
		require.Equal(t, 400, resumeErr.Status)
		require.Equal(t, "unknown_association", resumeErr.Reason)
	})
}

// TestCursorContextPlanHistoryComplete 锁定四协议的 historyComplete 口径（方案 §2.3）。
func TestCursorContextPlanHistoryComplete(t *testing.T) {
	var chatReq apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"cursor-sonnet",
		"messages":[{"role":"user","content":"hi"}]
	}`), &chatReq))
	chatPlan, err := cursorPlanFromChat(&chatReq, "cursor-sonnet")
	require.NoError(t, err)
	require.True(t, chatPlan.historySupportsRebuild(), "chat 客户端全量回传历史")

	// Responses：只有 previous_response_id、无历史 → 不可重建。
	var responsesReq apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"cursor-sonnet",
		"previous_response_id":"resp_1",
		"input":[{"type":"function_call_output","call_id":"call_x","output":"ok"}]
	}`), &responsesReq))
	responsesPlan, err := cursorPlanFromResponses(&responsesReq, "cursor-sonnet")
	require.NoError(t, err)
	require.True(t, responsesPlan.claimsContinuation())
	require.False(t, responsesPlan.historySupportsRebuild(),
		"仅有 previous_response_id 且无成对 function_call → 不得重建（会丢上下文）")

	// Responses：正文内成对 function_call + function_call_output → 可重建。
	var pairedReq apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"cursor-sonnet",
		"input":[
			{"type":"function_call","call_id":"call_x","name":"get_weather","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_x","output":"ok"}
		]
	}`), &pairedReq))
	pairedPlan, err := cursorPlanFromResponses(&pairedReq, "cursor-sonnet")
	require.NoError(t, err)
	require.True(t, pairedPlan.historySupportsRebuild())
}

// TestCursorRebuildExcludedIDs 锁定 2026-11-04 收口 M2：
// 重建请求的死亡账号必须并入排除集合，且不得改写调用方传入的 map
// （FailoverState 自己维护那份集合，服务层偷偷加进去会让它的耗尽判定失真）。
func TestCursorRebuildExcludedIDs(t *testing.T) {
	t.Run("no_rebuild_keeps_original_map", func(t *testing.T) {
		original := map[int64]struct{}{7: {}}
		got := cursorRebuildExcludedIDs(context.Background(), original)
		require.Len(t, got, 1)
		_, ok := got[7]
		require.True(t, ok)
	})

	t.Run("rebuild_adds_dead_account_without_mutation", func(t *testing.T) {
		original := map[int64]struct{}{7: {}}
		ctx := WithCursorRebuild(context.Background(), CursorRebuildInfo{DeadAccountID: 344, Reason: CursorQuotaExhaustedReason})
		got := cursorRebuildExcludedIDs(ctx, original)
		require.Len(t, got, 2)
		_, hasDead := got[344]
		require.True(t, hasDead, "死亡账号必须被排除")
		require.Len(t, original, 1, "调用方的 map 不得被修改")
	})

	t.Run("already_excluded_is_idempotent", func(t *testing.T) {
		original := map[int64]struct{}{344: {}}
		ctx := WithCursorRebuild(context.Background(), CursorRebuildInfo{DeadAccountID: 344})
		got := cursorRebuildExcludedIDs(ctx, original)
		require.Len(t, got, 1)
	})

	t.Run("zero_dead_account_is_ignored", func(t *testing.T) {
		ctx := WithCursorRebuild(context.Background(), CursorRebuildInfo{})
		got := cursorRebuildExcludedIDs(ctx, nil)
		require.Empty(t, got)
	})
}
