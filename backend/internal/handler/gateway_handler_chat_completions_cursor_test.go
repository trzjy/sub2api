//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiChatCompletionsPlatformAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, platform, errorType, message string
	}{
		// Missing Cursor credentials deliberately fail at the existing forwarder;
		// reaching that error proves the handler did not discard the selected account.
		{"cursor reaches forwarder", service.PlatformCursor, "upstream_error", infraerrors.UpstreamRequestFailed},
		{"gemini reaches compat service", service.PlatformGemini, "upstream_error", infraerrors.CompatServiceNotConfigured},
		{"other mixed supply remains excluded", service.PlatformAntigravity, "server_error", infraerrors.AllAccountsExhausted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			groupID := int64(9410)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformGemini, Status: service.StatusActive}
			account := &service.Account{
				ID: 9411, Platform: tt.platform, Type: service.AccountTypeAPIKey,
				Status: service.StatusActive, Schedulable: true, Concurrency: 1,
				Credentials:   map[string]any{"model_mapping": map[string]any{"gemini-3.8-flash": "gemini-3.8-flash"}},
				Extra:         map[string]any{"mixed_scheduling": true},
				AccountGroups: []service.AccountGroup{{AccountID: 9411, GroupID: groupID}},
			}
			h, cleanup := newTestGatewayHandler(t, group, []*service.Account{account})
			defer cleanup()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"Calculate 23 * 19."}],"stream":false,"max_tokens":128}`
			ctx := context.WithValue(context.Background(), ctxkey.Group, group)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body)).WithContext(ctx)
			apiKey := &service.APIKey{
				ID: 9412, UserID: 9413, GroupID: &groupID, Group: group, Status: service.StatusActive,
				User: &service.User{ID: 9413, Concurrency: 10, Balance: 100},
			}
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})

			h.ChatCompletions(c)

			selected, ok := c.Get(opsAccountIDKey)
			require.True(t, ok, "test must reach account selection")
			require.Equal(t, account.ID, selected)
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			var response struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			require.Equal(t, tt.errorType, response.Error.Type)
			require.Equal(t, tt.message, response.Error.Message)
		})
	}
}

// TestChatCompletionsCursorSettlementReceivesTerminalResultOnce 覆盖 §25.17.5 A06：
// ChatCompletions Cursor 分支注册的唯一结算回调收到**终态真实 *ForwardResult** 且至多
// 执行一次；缺完整 usage 的终态不提交（不写零 usage 假成功）。
func TestChatCompletionsCursorSettlementReceivesTerminalResultOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	var gotFinal *service.ForwardResult
	var gotCtx context.Context
	handover := newCursorResourceHandover(nil, nil)
	handover.RegisterCursorSettlement(func(ctx context.Context, final *service.ForwardResult) {
		calls.Add(1)
		gotFinal = final
		gotCtx = ctx
	})
	require.NotNil(t, handover.SettlementFunc(), "注册后 finalizer 侧访问器必须能取出同一个钩子")

	terminal := &service.ForwardResult{
		Model:         "cursor-final",
		UpstreamModel: "cursor-upstream",
		Usage:         service.ClaudeUsage{InputTokens: 11, OutputTokens: 22, CacheReadInputTokens: 3, CacheCreationInputTokens: 4},
	}
	complete := service.CursorSettlementEvent{
		SessionID:      "unit-test",
		BillingEventID: "cursor_session:unit-test",
		Result:         terminal,
		UsageComplete:  true,
	}
	// 终态到达：唯一结算回调收到真实终态并提交一次。
	handover.Settlement()(context.Background(), complete)
	require.EqualValues(t, 1, calls.Load(), "收到完整终态只结算一次")
	require.Same(t, terminal, gotFinal, "必须把终态真实 ForwardResult 原样交给结算钩子")
	require.True(t, handover.Settled())
	require.NoError(t, handover.SettlementError())

	// 提交 ctx 携带会话级不可变账单事件 ID（§25.5 durable 键）。
	boundID, _ := gotCtx.Value(ctxkey.ClientRequestID).(string)
	require.Equal(t, "cursor_session:unit-test", boundID)
	require.Equal(t,
		service.CursorSettlementDurableRequestID("cursor_session:unit-test"),
		service.CursorSettlementDurableRequestID(boundID),
		"同一会话的重复结算落到同一个 durable 键")

	// 重复终结（Stop 与结果回传竞争）不得重复结算。
	handover.Settlement()(context.Background(), complete)
	require.EqualValues(t, 1, calls.Load(), "重复终结不得重复结算")

	// 缺完整 usage：不提交、不写零 usage 假成功。
	var incompleteCalls atomic.Int32
	incomplete := newCursorResourceHandover(nil, nil)
	incomplete.RegisterCursorSettlement(func(context.Context, *service.ForwardResult) { incompleteCalls.Add(1) })
	incomplete.Settlement()(context.Background(), service.CursorSettlementEvent{
		SessionID:       "unit-test",
		BillingEventID:  "cursor_session:unit-test",
		Result:          terminal,
		UsageComplete:   false,
		InflightRelease: func() {},
	})
	require.EqualValues(t, 0, incompleteCalls.Load(), "缺完整 usage 不得结算")
	require.False(t, incomplete.Settled())
}
