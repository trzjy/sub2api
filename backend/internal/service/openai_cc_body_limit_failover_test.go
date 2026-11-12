//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Qoder 桥接对超过上游 256 KiB 上限的请求体本地返回 413（2026-11-01 用户裁定）。
// 锁定站点侧既有语义在 Chat Completions 路径（deepseek apikey 账号）同样成立：
// 413 换下一个号、不在同号重试、不记 account+model 瞬态冷却；对照组 502 仍记冷却。
func TestOpenAICCBodyLimit413_FailsOverWithoutTransientCooldown(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimitService}
	const model = "deepseek-v4-pro"
	body := []byte(`{"error":{"message":"request body is 517575 bytes; this upstream accepts at most 262144 bytes","type":"qoder_bridge_error","code":"request_too_large"}}`)

	account := &Account{ID: 90406, Platform: PlatformDeepseek, Type: AccountTypeAPIKey}
	for i := 0; i < 3; i++ {
		c := codeBuddy429TestContext()
		failoverErr := svc.failoverOpenAIUpstreamHTTPError(context.Background(), c, account,
			codeBuddy429UpstreamResponse(http.StatusRequestEntityTooLarge), body, "", model, "")
		require.NotNil(t, failoverErr)
		require.True(t, failoverErr.IsOpenAIRequestBodyTooLarge())
		require.True(t, failoverErr.ShouldRetryNextAccount())
		require.False(t, failoverErr.RetryableOnSameAccount)
	}
	require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, model), "413 must not cool the account down")
	require.Equal(t, 0, repo.tempCalls)

	control := &Account{ID: 90411, Platform: PlatformDeepseek, Type: AccountTypeAPIKey}
	for i := 0; i < 2; i++ {
		c := codeBuddy429TestContext()
		require.NotNil(t, svc.failoverOpenAIUpstreamHTTPError(context.Background(), c, control,
			codeBuddy429UpstreamResponse(http.StatusBadGateway), []byte(`{"error":{"message":"internal server error"}}`), "", model, ""))
	}
	require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(control, model), "502 keeps the existing cooldown behavior")
}
