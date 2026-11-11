package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestConcurrencyErrorResponse(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		slotType    string
		wantStatus  int
		wantType    string
		wantCode    string
		wantMessage string
	}{
		{
			name:        "account slot concurrency returns 503 service unavailable",
			err:         &ConcurrencyError{SlotType: "account", IsTimeout: true},
			slotType:    "user",
			wantStatus:  http.StatusServiceUnavailable,
			wantType:    "api_error",
			wantCode:    gatewayConcurrencyLimitCode,
			wantMessage: infraerrors.AccountSlotConcurrencyLimit,
		},
		{
			name:        "user slot concurrency keeps 429 rate limit",
			err:         &ConcurrencyError{SlotType: "user", Limit: 5},
			slotType:    "user",
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantCode:    gatewayConcurrencyLimitCode,
			wantMessage: "您的用户并发请求已达上限（5），请降低并发或稍后重试",
		},
		{
			// 2026-11-11 生产事故固化：订阅分组并发超限必须返回真 429。
			// 事故形态是「准入等待期间发了 SSE 心跳 → 200 已提交 → 429 只能塞进流」，
			// 客户端拿 200 不会按 429 退避，运维按状态码统计也看不到（48h 内 892 次）。
			// 现已改为准入阶段静默，此处锁定 status/type/message 三件套防回退。
			name:        "subscription group concurrency keeps 429 rate limit",
			err:         &ConcurrencyError{SlotType: "group", Limit: 2},
			slotType:    "group",
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantCode:    gatewayConcurrencyLimitCode,
			wantMessage: "您的订阅并发请求已达上限（2），请降低并发或稍后重试",
		},
		{
			// 订阅分组排队超时（等到 30s 仍未拿到槽位）同样是 429，语义与并发瞬时超限一致。
			name:        "subscription group wait timeout is 429",
			err:         &ConcurrencyError{SlotType: "group", IsTimeout: true, Limit: 2},
			slotType:    "group",
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantCode:    gatewayConcurrencyLimitCode,
			wantMessage: "您的订阅并发请求已达上限（2），请降低并发或稍后重试",
		},
		{
			name:        "client cancellation is not classified as concurrency limit",
			err:         context.Canceled,
			slotType:    "user",
			wantStatus:  statusClientClosedRequest,
			wantType:    "api_error",
			wantMessage: "context canceled",
		},
		{
			name:        "deadline exceeded is service unavailable",
			err:         context.DeadlineExceeded,
			slotType:    "user",
			wantStatus:  http.StatusServiceUnavailable,
			wantType:    "api_error",
			wantMessage: infraerrors.ConcurrencyFallback,
		},
		{
			name:        "redis acquire error is service unavailable",
			err:         errors.New("redis unavailable"),
			slotType:    "user",
			wantStatus:  http.StatusServiceUnavailable,
			wantType:    "api_error",
			wantMessage: infraerrors.ConcurrencyFallback,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, errType, code, message := concurrencyErrorResponse(tt.err, tt.slotType, "")
			require.Equal(t, tt.wantStatus, status)
			require.Equal(t, tt.wantType, errType)
			require.Equal(t, tt.wantCode, code)
			require.Equal(t, tt.wantMessage, message)
		})
	}
}

func TestGoogleConcurrencyError(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantGStatus string
		wantMessage string
	}{
		{
			name:        "client cancellation is 499",
			err:         context.Canceled,
			wantStatus:  statusClientClosedRequest,
			wantGStatus: "CANCELLED",
			wantMessage: "context canceled",
		},
		{
			name:        "slot limit reached is 429",
			err:         &ConcurrencyError{SlotType: "user", Limit: 5},
			wantStatus:  http.StatusTooManyRequests,
			wantGStatus: "RESOURCE_EXHAUSTED",
			wantMessage: "您的用户并发请求已达上限（5），请降低并发或稍后重试",
		},
		{
			name:        "slot wait timeout is 429",
			err:         &ConcurrencyError{SlotType: "user", IsTimeout: true},
			wantStatus:  http.StatusTooManyRequests,
			wantGStatus: "RESOURCE_EXHAUSTED",
			wantMessage: infraerrors.ConcurrencyLimitReachedNoLimit,
		},
		{
			name:        "acquire backend error is 503",
			err:         errors.New("redis unavailable"),
			wantStatus:  http.StatusServiceUnavailable,
			wantGStatus: "INTERNAL",
			wantMessage: infraerrors.ConcurrencyFallback,
		},
	}

	gin.SetMode(gin.TestMode)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)

			googleConcurrencyError(c, tt.err, "user")

			require.Equal(t, tt.wantStatus, rec.Code)
			var body struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
					Status  string `json:"status"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, tt.wantStatus, body.Error.Code)
			require.Equal(t, tt.wantGStatus, body.Error.Status)
			require.Equal(t, tt.wantMessage, body.Error.Message)
		})
	}
}
