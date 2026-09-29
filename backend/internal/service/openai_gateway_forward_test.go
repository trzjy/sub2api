//go:build unit

package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestOpenAIFirstOutputHeaderGuardAdapterHeaderArrivesNoTimeout 验证响应头先到时
// 适配器包回"未超时"语义，且不取消上游请求 context（护栏零取消契约）。
func TestOpenAIFirstOutputHeaderGuardAdapterHeaderArrivesNoTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	releaseCount := 0
	release := func() { releaseCount++ }

	reqCtx, adapter := newOpenAIFirstOutputHeaderGuardAdapter(parent, release, time.Now().Add(2*time.Second))

	// 响应头先到：stopHeaderWait 必须返回 false（未超时），ctx 未被取消。
	require.False(t, adapter.stopHeaderWait(), "header arrived before deadline must not be a timeout")
	require.False(t, adapter.guard.TimedOut())
	require.False(t, adapter.guard.Cancelled(), "header-arrived path must NOT cancel upstream request context")
	require.Empty(t, reqCtx.Err())

	adapter.close()
	require.Equal(t, 1, releaseCount)
}

// TestOpenAIFirstOutputHeaderGuardAdapterTimeoutCancelsContext 验证窗口耗尽时适配
// 器取消上游请求 context 并报告超时（底层共享护栏原语行为）。
func TestOpenAIFirstOutputHeaderGuardAdapterTimeoutCancelsContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	releaseCount := 0
	release := func() { releaseCount++ }

	reqCtx, adapter := newOpenAIFirstOutputHeaderGuardAdapter(parent, release, time.Now().Add(20*time.Millisecond))

	select {
	case <-reqCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("shared guard did not cancel request context on window exhaustion")
	}
	require.True(t, adapter.guard.TimedOut())
	require.True(t, adapter.guard.Cancelled())
	// 超时后 stopHeaderWait 报告超时，应返回 first-output timeout 错误。
	require.True(t, adapter.stopHeaderWait())
	adapter.close()
	require.Equal(t, 1, releaseCount, "release must be called exactly once")
}

// TestOpenAIFirstOutputHeaderGuardAdapterCloseIdempotent 验证 close 幂等（与旧
// openAIFirstOutputHeaderGuard.close 的 sync.Once 语义一致）。
func TestOpenAIFirstOutputHeaderGuardAdapterCloseIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	releaseCount := 0
	release := func() { releaseCount++ }

	_, adapter := newOpenAIFirstOutputHeaderGuardAdapter(parent, release, time.Now().Add(2*time.Second))
	adapter.close()
	adapter.close()
	require.Equal(t, 1, releaseCount, "release must be called exactly once despite double close")
}

// TestOpenAIPathNoSharedFirstByteErrorConstructor 是 OpenAI 原生路径零变化反向测试
// （方案 v6/v7：入口预算/心跳/新错误构造器一律不进 OpenAI 路径）。OpenAI 路径仍须
// 产出 OpenAI 专属 first_output_timeout 错误体，而非共享 newUpstreamFirstByteTimeoutError
// 的 upstream_first_byte_timeout。既验证适配器端到端接入，又钉死不改语义。
func TestOpenAIPathNoSharedFirstByteErrorConstructor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &blockingOpenAIResponseHeaderUpstream{canceled: make(chan struct{})}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			OpenAIFirstOutputTimeoutSeconds: 1,
			MaxLineSize:                     defaultMaxLineSize,
		}},
		httpUpstream: upstream,
	}
	body := []byte(`{"model":"gpt-5.5","stream":true,"reasoning":{"effort":"low"},"input":"hello"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	account := &Account{
		ID: 1, Name: "oauth-test", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"},
	}

	_, err := svc.Forward(context.Background(), c, account, body)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Contains(t, string(failoverErr.ResponseBody), "first_output_timeout",
		"OpenAI path must keep its OpenAI-specific error body")
	require.NotContains(t, string(failoverErr.ResponseBody), "upstream_first_byte_timeout",
		"OpenAI path must NOT use the shared new error constructor")
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
}
