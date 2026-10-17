//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestRefreshAccounts_UpstreamError_ResponseCarriesOriginalMessage：端到端验证 R1-1——
// 手动刷新上游失败/终态结果经 responder 后，响应 HTTP 非 2xx（502）且 message 逐字携带
// 上游中文原文（无英文前缀/映射），绝不显示 success。冷却/过期/未知三类同形态。
func TestRefreshAccounts_UpstreamError_ResponseCarriesOriginalMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		kind service.UpstreamResultKind
		msg  string
	}{
		{service.UpstreamCooldown, "每5分钟内仅可刷新1次，请3分钟后再试"},
		{service.UpstreamExpired, "密钥额度耗尽或已过期"},
		{service.UpstreamUnknown, "some unknown text"},
	}
	for _, tc := range cases {
		var err error
		switch tc.kind {
		case service.UpstreamCooldown:
			err = service.NewUpstreamCooldownError(tc.msg)
		case service.UpstreamExpired:
			err = service.NewUpstreamExpiredError(tc.msg)
		default:
			err = service.NewUpstreamUnknownError(tc.msg)
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest(http.MethodPost, "/api/v1/admin/quota_keys/1/refresh", nil)

		respondQuotaKeyError(c, err)

		require.Equal(t, http.StatusBadGateway, w.Code, "upstream error must be non-2xx (502)")

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		message, _ := body["message"].(string)
		require.Contains(t, message, tc.msg,
			"response message must carry the original Chinese msg verbatim")
		require.NotEqual(t, "success", message, "must NOT be reported as success")
	}
}

// TestRefreshAccounts_NonUpstreamError_UnchangedMapping：非上游归类错误（如 404）仍走既有
// 默认映射，不受 R1-1 分支影响。
func TestRefreshAccounts_NonUpstreamError_UnchangedMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/api/v1/admin/quota_keys/1/refresh", nil)

	respondQuotaKeyError(c, service.ErrQuotaKeyNotFound)

	require.Equal(t, http.StatusNotFound, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "quota key not found", body["message"])
}
