package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// TestCreateAccountRequestDeserializesQuotaKeyID 锁定 v3.4 §4.4 建号 DTO 契约：
// 请求体携带 quota_key_id 时反序列化为 *int64，供 Create 透传到 service 建号入参。
func TestCreateAccountRequestDeserializesQuotaKeyID(t *testing.T) {
	var req CreateAccountRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"name": "cursor-1",
		"platform": "cursor",
		"type": "apikey",
		"credentials": {"api_key": "sk-test"},
		"quota_key_id": 42
	}`), &req))

	require.NotNil(t, req.QuotaKeyID, "quota_key_id must be deserialized into a non-nil pointer")
	require.Equal(t, int64(42), *req.QuotaKeyID)

	// 未携带时保持 nil（非 cursor 平台允许 NULL，cursor 由 service 层校验拒绝）。
	var noKey CreateAccountRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"name": "openai-1",
		"platform": "openai",
		"type": "apikey",
		"credentials": {"api_key": "sk-test"}
	}`), &noKey))
	require.Nil(t, noKey.QuotaKeyID)

	// 序列化侧：nil 时按 omitempty 省略，不向前端回显空字段。
	encoded, err := json.Marshal(noKey)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "quota_key_id")
}

// TestAccountHandlerCreatePassesQuotaKeyIDToService 锁定 D-CURSOR-025 缺陷 2：
// handler Create 必须把 req.QuotaKeyID 透传到 service 建号入参，否则 cursor 账号
// 会落到 DB CHECK（accounts_cursor_quota_key_required）被拒或归属丢失。
func TestAccountHandlerCreatePassesQuotaKeyIDToService(t *testing.T) {
	adminSvc := newStubAdminService()
	router := setupAccountMixedChannelRouter(adminSvc)

	body, err := json.Marshal(map[string]any{
		"name":         "cursor-with-quota-key",
		"platform":     domain.PlatformCursor,
		"type":         "apikey",
		"credentials":  map[string]any{"api_key": "sk-test"},
		"quota_key_id": 42,
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "cursor account with quota_key_id must be created")
	require.Len(t, adminSvc.createdAccounts, 1)
	captured := adminSvc.createdAccounts[0]
	require.Equal(t, domain.PlatformCursor, captured.Platform)
	require.NotNil(t, captured.QuotaKeyID, "quota_key_id must reach the service create input")
	require.Equal(t, int64(42), *captured.QuotaKeyID)
}

// TestAccountHandlerCreateCursorWithoutQuotaKeyIDReturns400 锁定 cursor 建号缺
// quota_key_id 的响应码：service 层（validateAccountQuotaKeyBinding）返回 400 后
// handler 必须原样映射为 HTTP 400，不得吞掉或泛化为 500。
func TestAccountHandlerCreateCursorWithoutQuotaKeyIDReturns400(t *testing.T) {
	adminSvc := newStubAdminService()
	adminSvc.createAccountErr = infraerrors.New(http.StatusBadRequest, "ACCOUNT_QUOTA_KEY_REQUIRED",
		"cursor platform accounts must be bound to a quota key (quota_key_id is required)")
	router := setupAccountMixedChannelRouter(adminSvc)

	body, err := json.Marshal(map[string]any{
		"name":        "cursor-no-quota-key",
		"platform":    domain.PlatformCursor,
		"type":        "apikey",
		"credentials": map[string]any{"api_key": "sk-test"},
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "cursor account without quota_key_id must be 400")
	require.Len(t, adminSvc.createdAccounts, 1)
	require.Nil(t, adminSvc.createdAccounts[0].QuotaKeyID,
		"missing quota_key_id must reach the service as nil so it can be rejected")
}
