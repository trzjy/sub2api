package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 402 与 OpenAI 网关口径一致：上游余额/额度耗尽应触发账号切换（如 TeamoRouter
// 免费池耗尽时天卡流量应落到低优先级兜底账号，而不是把 402 透传给用户）。
func TestShouldFailoverUpstreamError_Includes402(t *testing.T) {
	svc := &GatewayService{}

	for _, code := range []int{401, 402, 403, 405, 429, 529, 500, 502, 503, 504} {
		assert.True(t, svc.shouldFailoverUpstreamError(code), "status %d should trigger failover", code)
	}

	for _, code := range []int{200, 201, 400, 404, 408, 422} {
		assert.False(t, svc.shouldFailoverUpstreamError(code), "status %d should NOT trigger failover", code)
	}
}
