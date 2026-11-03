//go:build unit

package service

// 「同一会话换模型来续轮」的定向测试（2026-11-04 用户裁定）。
//
// 原先 model_changed 直接 400，理由是"同一次生成不能换模型"——上游那条会话是用旧模型开的，
// 换模型就等于往别人的会话里灌结果，这个**防串号**的目的仍然成立。但它被用成了"禁止用户
// 换模型继续聊"，线上实测 1 秒内 6 次同源 400（会话存 sonnet、正文要 opus）。
//
// 裁定后的规则：
//   - 正文带完整历史 → 终结旧会话，按新模型重开一局（账号不排除：它没坏）；
//   - 历史不完整 → 维持 400（凭残缺正文重开会丢上下文，比报错更糟）。
//
// 防串号依然成立：新局是全新会话，旧会话被终结，A 模型的结果永远灌不进 B 模型的会话。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorResumeTestChatResumeWithModel 与 cursorResumeTestChatResume 相同，只把模型换成别的：
// 模拟"会话是用 cursor-test-model 开的，客户端换到 cursor-other-model 继续"。
const cursorResumeTestChatResumeWithModel = `{
  "model": "cursor-other-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`

// 换模型 + 完整历史：不得回 400，必须给出"按新模型重建"的信号，且旧会话已终结、零上游写入。
func TestCursorResumeModelChangedRebuildsWithCompleteHistory(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")

	_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatResumeWithModel), false)

	var rebuild *CursorResumeRebuildError
	require.ErrorAs(t, err, &rebuild, "换模型且有完整历史：必须转成重建，不得回 400")
	require.Equal(t, CursorResumeModelChangedReason, rebuild.Reason)
	require.True(t, rebuild.KeepAccount, "账号没坏，只是换了模型：不得把它排除出候选")
	require.Equal(t, int64(42), rebuild.DeadAccountID)
	// 旧会话的工具结果一条都不许写回上游。
	require.Equal(t, 0, fixture.intake.callCount(), "重建路径零上游写入")
	// 旧会话必须已经终结干净，否则新局会和它抢并发槽/预填。
	require.Equal(t, cursorSessionClosed, cursorSessionStateOf(fixture.record))
	// 错误仍可沿既有错误链取回状态与原因（日志/排查用）。
	status, reason := CursorResumeHTTPStatus(err)
	require.Equal(t, 400, status)
	require.Equal(t, "model_changed", reason)
}

// 历史不完整时不许重开：仍按原 400 拒绝，且**不动**会话（它还在等工具结果）。
func TestResumeModelChangedOutcomeKeepsDenialWhenHistoryIncomplete(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore()}
	record := svc.sessions.start(cursorSessionSpec{
		protocol:       cursorProtocolChat,
		requestedModel: "cursor-test-model",
		upstreamModel:  "cursor-upstream-cursor-test-model",
		accountID:      42,
		quotaKeyID:     7,
	})
	ticket := &CursorResumeTicket{
		SessionID: record.id,
		BatchID:   "mc-1",
		session:   record,
		probe:     &CursorResumeProbe{Plan: &cursorContextPlan{historyComplete: false}},
	}

	err := svc.resumeModelChangedOutcome(context.Background(), record, ticket,
		&Account{ID: 42, Platform: PlatformCursor}, "cursor-test-model", "cursor-other-model", "body")

	var rebuild *CursorResumeRebuildError
	require.NotErrorAs(t, err, &rebuild, "历史不完整不得重建（会丢上下文）")
	status, reason := CursorResumeHTTPStatus(err)
	require.Equal(t, 400, status)
	require.Equal(t, "model_changed", reason)
	require.NotEqual(t, cursorSessionClosed, cursorSessionStateOf(record), "拒绝不得终结会话")
}

// probe 缺失（老调用方/异常路径）同样按"历史未知 = 不重建"处理，绝不因为拿不到判据就重开。
func TestResumeModelChangedOutcomeTreatsMissingProbeAsIncomplete(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore()}
	record := svc.sessions.start(cursorSessionSpec{protocol: cursorProtocolChat, accountID: 42})

	err := svc.resumeModelChangedOutcome(context.Background(), record, &CursorResumeTicket{SessionID: record.id},
		&Account{ID: 42}, "a", "b", "body")

	var rebuild *CursorResumeRebuildError
	require.NotErrorAs(t, err, &rebuild)
	require.Equal(t, 400, mustCursorResumeStatus(t, err))
}

// KeepAccount 的落点：换模型重建时不得把健康的号排除出候选（否则每次换模型都白换一个号）。
func TestCursorRebuildExcludedIDsKeepsHealthyAccountOnModelChange(t *testing.T) {
	ctx := WithCursorRebuild(context.Background(), CursorRebuildInfo{
		DeadAccountID: 344,
		Reason:        CursorResumeModelChangedReason,
		KeepAccount:   true,
	})
	base := map[int64]struct{}{7: {}}

	got := cursorRebuildExcludedIDs(ctx, base)
	require.Equal(t, base, got, "账号没坏：排除集合必须原样返回")
	require.NotContains(t, got, int64(344))
}
