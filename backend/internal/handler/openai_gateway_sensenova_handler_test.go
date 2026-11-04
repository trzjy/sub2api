//go:build unit

package handler

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// sensenovaHandlerRepoStub 是 applySensenovaSizeExclusion 在 >256K 分支下
// 列举可调度候选所需的极简账号仓库桩（仅实现本路径触达的方法）。
type sensenovaHandlerRepoStub struct {
	service.AccountRepository
	accounts []service.Account
}

func (r sensenovaHandlerRepoStub) ListSchedulableUngroupedByPlatform(_ context.Context, _ string) ([]service.Account, error) {
	return r.accounts, nil
}

func (r sensenovaHandlerRepoStub) ListSchedulableByPlatform(_ context.Context, _ string) ([]service.Account, error) {
	return r.accounts, nil
}

func newSensenovaTestAccount(id int64) *service.Account {
	return &service.Account{
		ID:       id,
		Name:     "sensenova",
		Platform: service.PlatformDeepseek,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://token.sensenova.cn/v1/chat/completions",
		},
	}
}

func TestSensenovaSizeExclusion(t *testing.T) {
	t.Parallel()

	sensenova := newSensenovaTestAccount(416)
	other := &service.Account{
		ID:       200,
		Name:     "openai",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
	}

	gw := service.NewOpenAIGatewayService(
		sensenovaHandlerRepoStub{accounts: []service.Account{*sensenova, *other}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw}
	reqLog := zap.NewNop()

	// Case A：中小请求（≤256K）不排除任何商汤账号。
	smallBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	excluded := h.applySensenovaSizeExclusion(context.Background(), reqLog, nil, service.PlatformDeepseek, "gpt-4o", service.TierRoutingProtocolChatCompletions, smallBody)
	require.Empty(t, excluded, "small request must not exclude any sensenova account")

	// Case 估算失败：请求体非法 JSON，遵循失败开放返回空集（绝不因估算失败拦请求）。
	badBody := []byte("{not valid json")
	excluded = h.applySensenovaSizeExclusion(context.Background(), reqLog, nil, service.PlatformDeepseek, "gpt-4o", service.TierRoutingProtocolChatCompletions, badBody)
	require.Empty(t, excluded, "estimate failure must fail-open (empty exclusion)")

	// Case B：超大请求（>256K）必须识别并排除商汤账号（416），保留非商汤账号（200）。
	// 注：重复短语会被 BPE 高效压缩，130000 次仅约 260K tokens，故放大到 200000 次
	// 确保估算稳定超过 262144 阈值。
	huge := strings.Repeat("hello world ", 200000) // ~2.4M 字符，估算远超 262144 tokens
	bigBody := []byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, huge))
	excluded = h.applySensenovaSizeExclusion(context.Background(), reqLog, nil, service.PlatformDeepseek, "gpt-4o", service.TierRoutingProtocolChatCompletions, bigBody)
	require.Len(t, excluded, 1, "exactly one sensenova account must be excluded for oversized request")
	_, ok := excluded[416]
	require.True(t, ok, "sensenova account 416 must be excluded")
	_, ok = excluded[200]
	require.False(t, ok, "non-sensenova account 200 must not be excluded")
}
