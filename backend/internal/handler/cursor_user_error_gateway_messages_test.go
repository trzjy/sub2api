//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// 本文件覆盖方案 §2.3 M3（M3f）：/v1/messages（GatewayHandler.Messages）走 Cursor 账号时，
// Forward 返回的 Cursor 链路错误按 service.CursorUserFacingError 写回中文与正确状态码
// （活体 74-messages-unknown：未知关联此前被兜底成 502「上游请求失败」）。

const (
	m3fGroupID    = int64(781010)
	m3fAccountID  = int64(781011)
	m3fQuotaKeyID = int64(781001)
	m3fModel      = "claude-sonnet-4-5"
	m3fRequestID  = "req-m3f-messages"
)

// 只带 tool_result、正文里没有对应 tool_use：声明续轮但本站无任何关联 → unknown_association。
const m3fOrphanToolResultBody = `{
  "model": "claude-sonnet-4-5",
  "max_tokens": 32,
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "hi"}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_m3f_orphan", "content": [{"type": "text", "text": "sunny"}]}]}
  ]
}`

// newM3fCursorMessagesHandler 装配 Messages 真实链路：快照调度命中 Cursor 账号，网关带唯一
// 执行部件（鉴权替身复用 Responses 回归夹具；本用例在出站前即被准入断言拒绝，不会出站）。
func newM3fCursorMessagesHandler(t *testing.T, group *service.Group, account *service.Account) *GatewayHandler {
	t.Helper()
	installCursorResponsesRegressionSourceGate(t)

	forwarder := service.NewCursorUpstreamForwarder()
	deviceID := "m3f-device-id"
	cursorExec, err := service.NewCursorExecutionService(
		cursorResponsesRegressionQuotaKeys{key: &ent.QuotaKey{ID: m3fQuotaKeyID, CodeEncrypted: "enc", DeviceID: &deviceID}},
		cursorResponsesRegressionQuotaKeyEnc{},
		cursorResponsesRegressionDeviceIDs{},
		forwarder,
	)
	require.NoError(t, err)
	t.Cleanup(cursorExec.Stop)

	schedulerSnapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
	gwSvc := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		schedulerSnapshot,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		cursorExec,
	)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	return &GatewayHandler{
		gatewayService:           gwSvc,
		billingCacheService:      billingCacheSvc,
		concurrencyHelper:        NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
		maxAccountSwitchesGemini: 1,
	}
}

func TestCursorUserErrorGatewayMessagesUnknownAssociation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: m3fGroupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	quotaKeyID := m3fQuotaKeyID
	account := &service.Account{
		ID: m3fAccountID, Name: "cursor-m3f", Platform: service.PlatformCursor, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1,
		QuotaKeyID: &quotaKeyID,
		Credentials: map[string]any{
			"access_token":  "cursor-m3f-token",
			"model_mapping": map[string]any{m3fModel: m3fModel},
		},
		AccountGroups: []service.AccountGroup{{AccountID: m3fAccountID, GroupID: m3fGroupID}},
	}
	h := newM3fCursorMessagesHandler(t, group, account)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	ctx = context.WithValue(ctx, ctxkey.RequestID, m3fRequestID)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(m3fOrphanToolResultBody)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	groupID := m3fGroupID
	apiKey := &service.APIKey{
		ID: 781012, UserID: 781013, GroupID: &groupID, Group: group, Status: service.StatusActive,
		User: &service.User{ID: 781013, Concurrency: 10, Balance: 100},
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})

	h.Messages(c)

	selected, ok := c.Get(opsAccountIDKey)
	require.True(t, ok, "必须走到选号")
	require.Equal(t, m3fAccountID, selected, "必须选中 Cursor 账号")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "invalid_request_error", body.Error.Type)
	_, wantZh := infraerrors.CursorResumeReasonCode("unknown_association")
	require.Contains(t, body.Error.Message, strings.TrimSuffix(wantZh, "。"))
	require.Contains(t, body.Error.Message, "错误码 "+infraerrors.CursorResUnknownAssociationCode)
	require.Contains(t, body.Error.Message, "请求号 "+m3fRequestID)
	require.NotEqual(t, infraerrors.UpstreamRequestFailed, body.Error.Message)
}

// 非 Cursor 账号：Messages 出口同一错误仍走原 502 兜底。非 Cursor 账号的 Forward 不会产出
// CursorResumeError，无法经 Messages 端到端制造，故按出口分支的写回顺序做最小单元断言。
func TestCursorUserErrorGatewayMessagesNonCursorUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	err := &service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"}
	for _, platform := range []string{service.PlatformAnthropic, service.PlatformAntigravity} {
		h := &GatewayHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).
			WithContext(context.WithValue(context.Background(), ctxkey.RequestID, m3fRequestID))
		account := &service.Account{ID: 781020, Platform: platform}

		wrote := h.writeCCCursorUserError(c, zap.NewNop(), account, err, false)
		require.False(t, wrote)
		require.True(t, h.ensureForwardErrorResponse(c, false))

		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Contains(t, rec.Body.String(), infraerrors.UpstreamRequestFailed)
	}
}
