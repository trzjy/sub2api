//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// c2QuotaCodesRepo 是能记录 SetTempUnschedulable 的最小 AccountRepository 替身，
// 用于验证 Devin 额度耗尽错误确实把账号写入了停调。
type c2QuotaCodesRepo struct {
	AccountRepository
	unschedCalls []c2QuotaCodesUnschedCall
}

type c2QuotaCodesUnschedCall struct {
	accountID int64
	until     time.Time
	reason    string
}

func (r *c2QuotaCodesRepo) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.unschedCalls = append(r.unschedCalls, c2QuotaCodesUnschedCall{accountID: id, until: until, reason: reason})
	return nil
}

// c2DevinAccountWithCustomCodes 构造配置了 custom_error_codes=["400"]（不含 200）的
// Devin APIKey 账号，使 ShouldHandleErrorCode(200) 返回 false —— 即会被通用状态码
// 筛选拦截的那类账号。
func c2DevinAccountWithCustomCodes() *Account {
	return &Account{
		ID:          992,
		Platform:    PlatformDevin,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"custom_error_codes_enabled": true,
			"custom_error_codes":         []any{float64(400)}, // 故意不含 200
		},
	}
}

// c2RunHandleErrorResponse 走完整的 handleErrorResponse 路径（含 ShouldHandleErrorCode
// 前置筛选），返回写出的响应记录器与函数返回的 error。
func c2RunHandleErrorResponse(t *testing.T, svc *OpenAIGatewayService, account *Account, statusCode int, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		Header:     http.Header{},
	}
	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	return rec, err
}

// TestDevinQuotaExhaustedBypassesCustomErrorCodesGate 验证 Done when #1：
// custom_error_codes 不含 200 时，Devin 额度耗尽错误仍穿过筛选，触发停调 + failover。
func TestDevinQuotaExhaustedBypassesCustomErrorCodesGate(t *testing.T) {
	repo := &c2QuotaCodesRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := c2DevinAccountWithCustomCodes()

	require.True(t, account.IsCustomErrorCodesEnabled(), "前置条件：custom_error_codes 已启用")
	require.False(t, account.ShouldHandleErrorCode(http.StatusOK), "前置条件：200 不在白名单内，通用筛选本应拦截")

	body := `{"error":{"code":"failed_precondition","message":"Your daily usage quota has been exhausted. Visit https://app.devin.ai/settings/usage to purchase on-demand usage or turn on auto-reload. (trace ID: 7f3c9a)"}}`
	rec, err := c2RunHandleErrorResponse(t, svc, account, http.StatusOK, body)

	var failoverErr *UpstreamFailoverError
	require.Error(t, err)
	require.True(t, errors.As(err, &failoverErr), "Devin 额度错误必须返回 failover 错误，实际得到: %v", err)
	require.Len(t, repo.unschedCalls, 1, "额度耗尽必须把账号写入停调")
	require.Equal(t, account.ID, repo.unschedCalls[0].accountID)
	require.True(t, repo.unschedCalls[0].until.After(time.Now()), "停调上界必须在未来")
	require.Equal(t, devinUpstreamQuotaExhaustedMarker, extractMarkerFromReason(repo.unschedCalls[0].reason))
	require.NotEqual(t, http.StatusInternalServerError, rec.Code, "不应再写旧的 500 分支")
}

// TestDevinNonQuotaErrorHonoredByCustomErrorCodesGate 验证 Done when #2：
// 非额度错误按原语义被筛选拦截（写 500、不 failover、不停调）。
func TestDevinQuotaNonQuotaErrorHonoredByCustomErrorCodesGate(t *testing.T) {
	repo := &c2QuotaCodesRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := c2DevinAccountWithCustomCodes()

	require.False(t, account.ShouldHandleErrorCode(http.StatusOK), "前置条件：200 不在白名单内")

	body := `{"error":{"code":"invalid_request_error","message":"some unrelated upstream error"}}`
	rec, err := c2RunHandleErrorResponse(t, svc, account, http.StatusOK, body)

	var failoverErr *UpstreamFailoverError
	require.Error(t, err)
	require.False(t, errors.As(err, &failoverErr), "非额度错误不得 failover")
	require.Equal(t, http.StatusInternalServerError, rec.Code, "非额度错误应保持旧的 500 响应")
	require.Empty(t, repo.unschedCalls, "非额度错误不得写入停调")
}

// extractMarkerFromReason 从停调 reason（JSON payload）里取出可恢复标记。
func extractMarkerFromReason(reason string) string {
	var state TempUnschedState
	if err := json.Unmarshal([]byte(reason), &state); err != nil {
		return ""
	}
	return state.MatchedKeyword
}
