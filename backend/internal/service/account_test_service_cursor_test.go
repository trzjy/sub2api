//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

// cursorAccountTestAccount 构造一个 cursor OAuth 账号（model_mapping 与用户级 Bearer）。
func cursorAccountTestAccount(id int64, quotaKeyID int64, mapping map[string]any, bearer string) *Account {
	return &Account{
		ID: id, Platform: PlatformCursor, Type: AccountTypeOAuth, Status: StatusActive,
		QuotaKeyID: qkID(quotaKeyID),
		Credentials: map[string]any{
			"model_mapping": mapping,
			"access_token":  bearer,
		},
	}
}

// cursorAccountTestEvents 解析 SSE recorder 输出为事件列表。
func cursorAccountTestEvents(t *testing.T, raw string) []TestEvent {
	t.Helper()
	var events []TestEvent
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev TestEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev))
		events = append(events, ev)
	}
	return events
}

// ① Cursor 账号"开始测试"必须进入 Cursor 原生 AgentRun 出站链：请求打到
// cursorPathAgentRun、携带账号 Bearer + x-jg-auth（与正式转发 forwardCursor 同源），
// 绝不落入 claude 兜底（§20.2-C）。
func TestAccountTestService_CursorRoutesToAgentRunProbe(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	gw, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	account := cursorAccountTestAccount(70, 11, map[string]any{"cursor-sonnet": "upstream-sonnet"}, "BEARER-CURSOR-TEST")
	svc := &AccountTestService{accountRepo: &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}},
	}}
	svc.SetOpenAIGatewayService(gw)
	ctx, recorder := newTestContext()

	err := svc.testCursorAccountConnection(ctx, account, "cursor-sonnet")
	require.NoError(t, err)

	// 出站 = 恰好一次 AgentRun；凭证归属本账号。
	require.Equal(t, []string{cursorPathAgentRun}, up.paths())
	req := up.at(0)
	require.Equal(t, "Bearer BEARER-CURSOR-TEST", req.header.Get("Authorization"))
	require.NotEmpty(t, req.header.Get("x-jg-auth"), "device-level auth must ride the probe")

	// SSE 判定成功。
	events := cursorAccountTestEvents(t, recorder.Body.String())
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, "test_complete", last.Type)
	require.True(t, last.Success)
}

// ② modelID 为空 → 与 GetAvailableModels 同口径：取排序后首个映射键作为默认测试模型。
func TestAccountTestService_CursorDefaultsToSortedFirstMappingKey(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	gw, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	account := cursorAccountTestAccount(71, 11, map[string]any{
		"muse-spark-1.3":    "muse-spark-1.3",
		"claude-sonnet-5-5": "claude-sonnet-5-5",
	}, "BEARER-CURSOR-DEFAULT")
	svc := &AccountTestService{accountRepo: &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}},
	}}
	svc.SetOpenAIGatewayService(gw)
	ctx, recorder := newTestContext()

	err := svc.testCursorAccountConnection(ctx, account, "")
	require.NoError(t, err)

	events := cursorAccountTestEvents(t, recorder.Body.String())
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, "test_complete", last.Type)
	require.True(t, last.Success)
	// content 事件的 Model 字段 = 排序后首个映射键（claude-sonnet-5-5 < muse-spark-1.3）。
	var content *TestEvent
	for i := range events {
		if events[i].Type == "content" {
			content = &events[i]
		}
	}
	require.NotNil(t, content, "probe must emit a content event")
	require.Equal(t, "claude-sonnet-5-5", content.Model)
}

// ③ 请求模型未命中映射 → 失败关闭，不向上游发任何请求。
func TestAccountTestService_CursorFailsClosedOnUnmappedModel(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	gw, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	account := cursorAccountTestAccount(72, 11, map[string]any{"cursor-sonnet": "upstream-sonnet"}, "BEARER-CURSOR-UNMAPPED")
	svc := &AccountTestService{accountRepo: &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}},
	}}
	svc.SetOpenAIGatewayService(gw)
	ctx, recorder := newTestContext()

	err := svc.testCursorAccountConnection(ctx, account, "not-mapped-model")
	require.Error(t, err)
	require.Equal(t, 0, up.count(), "unmapped model must fail closed before any upstream call")

	events := cursorAccountTestEvents(t, recorder.Body.String())
	require.NotEmpty(t, events)
	require.Equal(t, "error", events[len(events)-1].Type)
}

// ④ 流内激活码终态拒绝模板 → 失败，不冒充成功（与正式转发同判据）。
func TestAccountTestService_CursorKeyRejectedIsFailure(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorRejectedStreamBody(t)}
	gw, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	account := cursorAccountTestAccount(73, 11, map[string]any{"cursor-sonnet": "upstream-sonnet"}, "BEARER-CURSOR-REJECTED")
	svc := &AccountTestService{accountRepo: &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}},
	}}
	svc.SetOpenAIGatewayService(gw)
	ctx, recorder := newTestContext()

	err := svc.testCursorAccountConnection(ctx, account, "cursor-sonnet")
	require.Error(t, err)

	events := cursorAccountTestEvents(t, recorder.Body.String())
	require.NotEmpty(t, events)
	require.Equal(t, "error", events[len(events)-1].Type)
	require.Contains(t, events[len(events)-1].Error, "cursor key rejected")
}

// ⑤ 账号 Bearer 缺失 → 失败关闭（与正式转发 I2 红线同契约），不发上游。
func TestAccountTestService_CursorFailsClosedWithoutBearer(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	gw, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	account := &Account{
		ID: 74, Platform: PlatformCursor, Type: AccountTypeOAuth, Status: StatusActive,
		QuotaKeyID: qkID(11),
		Credentials: map[string]any{
			"model_mapping": map[string]any{"cursor-sonnet": "upstream-sonnet"},
		},
	}
	svc := &AccountTestService{accountRepo: &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}},
	}}
	svc.SetOpenAIGatewayService(gw)
	ctx, _ := newTestContext()

	err := svc.testCursorAccountConnection(ctx, account, "cursor-sonnet")
	require.Error(t, err)
	require.Contains(t, err.Error(), "access_token")
	require.Equal(t, 0, up.count(), "no bearer → fail closed, no upstream call")
}

// TestAccountTestService_CursorRoutedThroughTestAccountConnection：TestAccountConnection
// 总入口对 PlatformCursor 分流到 Cursor 原生探活（此前落入 claude 兜底按 Anthropic 调用）。
func TestAccountTestService_CursorRoutedThroughTestAccountConnection(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	gw, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	account := cursorAccountTestAccount(75, 11, map[string]any{"cursor-sonnet": "upstream-sonnet"}, "BEARER-CURSOR-ROUTE")
	svc := &AccountTestService{accountRepo: &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}},
	}}
	svc.SetOpenAIGatewayService(gw)
	ctx, recorder := newTestContext()
	initAccountTestLogger(ctx, account.ID, "cursor-sonnet", "")

	err := svc.TestAccountConnection(ctx, account.ID, "cursor-sonnet", "", "")
	require.NoError(t, err)
	require.Equal(t, []string{cursorPathAgentRun}, up.paths())

	events := cursorAccountTestEvents(t, recorder.Body.String())
	require.NotEmpty(t, events)
	require.Equal(t, "test_complete", events[len(events)-1].Type)
	require.True(t, events[len(events)-1].Success)
}

// ⑦ 上游只返回正常 EndStream 帧、无任何文本帧 → 明确失败（O02 假成功修正）。
// 解析器允许空流（cursor_protocol_response.go :477/:727 可达，u7-review 07 已复现），
// 旧逻辑用 completion JSON 非空误判成功，此处必须失败关闭，绝不出现 success=true。
func TestAccountTestService_CursorEmptyStreamFailsClosed(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	// 仅 EndStream 帧，无任何文本帧 → 空流。
	up := &cursorOutboundUpstream{body: cursorTestEndStream(t, `{}`)}
	gw, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	account := cursorAccountTestAccount(76, 11, map[string]any{"cursor-sonnet": "upstream-sonnet"}, "BEARER-CURSOR-EMPTY")
	svc := &AccountTestService{accountRepo: &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}},
	}}
	svc.SetOpenAIGatewayService(gw)
	ctx, recorder := newTestContext()

	err := svc.testCursorAccountConnection(ctx, account, "cursor-sonnet")
	require.Error(t, err, "empty upstream text must fail closed, not masquerade as success")
	require.Contains(t, err.Error(), "no text frames")

	events := cursorAccountTestEvents(t, recorder.Body.String())
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, "error", last.Type, "empty stream must emit an error event")
	// 整个事件流中绝不允许出现 success=true 的 test_complete。
	for _, ev := range events {
		if ev.Type == "test_complete" {
			require.False(t, ev.Success, "empty stream must never claim success")
		}
	}
}

// ⑧ 上游返回真实文本帧 → content 事件的 Text 必须是真实解析文本，而非固定文案
// "Cursor AgentRun probe ok"（O02 假成功修正）。
func TestAccountTestService_CursorContentEchoesRealText(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	// 上游替身返回真实文本帧 "PING-RESULT-123"。
	var pingStream []byte
	pingStream = append(pingStream, cursorTextDeltaFrame(t, "PING-RESULT-123")...)
	pingStream = append(pingStream, cursorTestEndStream(t, `{}`)...)
	up := &cursorOutboundUpstream{body: pingStream}
	gw, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	account := cursorAccountTestAccount(77, 11, map[string]any{"cursor-sonnet": "upstream-sonnet"}, "BEARER-CURSOR-PING")
	svc := &AccountTestService{accountRepo: &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}},
	}}
	svc.SetOpenAIGatewayService(gw)
	ctx, recorder := newTestContext()

	err := svc.testCursorAccountConnection(ctx, account, "cursor-sonnet")
	require.NoError(t, err)

	events := cursorAccountTestEvents(t, recorder.Body.String())
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, "test_complete", last.Type)
	require.True(t, last.Success)

	var content *TestEvent
	for i := range events {
		if events[i].Type == "content" {
			content = &events[i]
		}
	}
	require.NotNil(t, content, "probe must emit a content event")
	require.Equal(t, "PING-RESULT-123", content.Text, "content event must carry the real parsed text, not fixed copy")
	require.NotEqual(t, "Cursor AgentRun probe ok", content.Text, "fixed success copy must be removed")
}
