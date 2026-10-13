package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
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

// TestUpdateAccountRequestDeserializesQuotaKeyIDTristate 锁定更新 DTO 的三态契约（#3）：
// *int64 无法区分"字段缺失"与"显式 null"，故用 json.RawMessage 承载。
//   - 字段缺失 → 未提供（沿用现有归属）
//   - 显式 null → 清空归属（仅非 cursor 允许）
//   - 数值     → 设置为该激活码
func TestUpdateAccountRequestDeserializesQuotaKeyIDTristate(t *testing.T) {
	var absent UpdateAccountRequest
	require.NoError(t, json.Unmarshal([]byte(`{"name":"acc"}`), &absent))
	require.Empty(t, absent.QuotaKeyID, "missing field must stay empty (not provided)")
	id, set, err := parseQuotaKeyIDUpdate(absent.QuotaKeyID)
	require.NoError(t, err)
	require.False(t, set, "missing field must not be treated as an explicit update")
	require.Nil(t, id)

	var nulled UpdateAccountRequest
	require.NoError(t, json.Unmarshal([]byte(`{"quota_key_id":null}`), &nulled))
	require.JSONEq(t, "null", string(nulled.QuotaKeyID), "explicit null must be preserved")
	id, set, err = parseQuotaKeyIDUpdate(nulled.QuotaKeyID)
	require.NoError(t, err)
	require.True(t, set, "explicit null must be an explicit clear")
	require.Nil(t, id)

	var valued UpdateAccountRequest
	require.NoError(t, json.Unmarshal([]byte(`{"quota_key_id":7}`), &valued))
	id, set, err = parseQuotaKeyIDUpdate(valued.QuotaKeyID)
	require.NoError(t, err)
	require.True(t, set)
	require.NotNil(t, id)
	require.Equal(t, int64(7), *id)

	// 非法值：必须解析失败（handler 映射 400），不得静默当成清空。
	_, _, err = parseQuotaKeyIDUpdate(json.RawMessage(`"abc"`))
	require.Error(t, err)
}

// TestAccountHandlerUpdatePassesQuotaKeyIDTristateToService 锁定 handler Update 的三态
// 透传：请求体的三种形态必须原样到达 service 入参，不得塌缩成单一的"未提供/清空"。
func TestAccountHandlerUpdatePassesQuotaKeyIDTristateToService(t *testing.T) {
	doUpdate := func(body map[string]any) *service.UpdateAccountInput {
		adminSvc := newStubAdminService()
		router := setupAccountMixedChannelRouter(adminSvc)
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/55", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.NotNil(t, adminSvc.lastUpdateAccountInput, "update input must reach the service")
		return adminSvc.lastUpdateAccountInput
	}

	// 未提供 → 不修改现有归属（cursor 账号的常规编辑走这里，回归 #3）。
	got := doUpdate(map[string]any{"name": "acc-edited"})
	require.False(t, got.QuotaKeyIDSet, "absent quota_key_id must not be an explicit update")
	require.Nil(t, got.QuotaKeyID)

	// 显式 null → 清空归属（cursor 由 service 判 400，非 cursor 放行）。
	got = doUpdate(map[string]any{"quota_key_id": nil})
	require.True(t, got.QuotaKeyIDSet, "explicit null must reach the service as an explicit clear")
	require.Nil(t, got.QuotaKeyID)

	// 数值 → 设置为该激活码。
	got = doUpdate(map[string]any{"quota_key_id": 7})
	require.True(t, got.QuotaKeyIDSet)
	require.NotNil(t, got.QuotaKeyID)
	require.Equal(t, int64(7), *got.QuotaKeyID)
}

// TestAccountHandlerUpdateRejectsNonIntegerQuotaKeyID 锁定非法 quota_key_id 的响应码：
// 解析失败必须映射为 400（含 quota_key_id 提示），不得进 service 后被泛化。
func TestAccountHandlerUpdateRejectsNonIntegerQuotaKeyID(t *testing.T) {
	adminSvc := newStubAdminService()
	router := setupAccountMixedChannelRouter(adminSvc)

	body, err := json.Marshal(map[string]any{"quota_key_id": "not-a-number"})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/55", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "quota_key_id")
	require.Nil(t, adminSvc.lastUpdateAccountInput, "invalid payload must not reach the service")
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
