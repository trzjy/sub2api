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

// largeRequestOnlyRepoStub 是 applyLargeRequestOnlyExclusion 在小请求分支下
// 列举可调度候选所需的极简账号仓库桩（仅实现本路径触达的方法）。
type largeRequestOnlyRepoStub struct {
	service.AccountRepository
	accounts []service.Account
}

func (r largeRequestOnlyRepoStub) ListSchedulableUngroupedByPlatform(_ context.Context, _ string) ([]service.Account, error) {
	return r.accounts, nil
}

func (r largeRequestOnlyRepoStub) ListSchedulableByPlatform(_ context.Context, _ string) ([]service.Account, error) {
	return r.accounts, nil
}

func newLargeRequestOnlyTestAccount(id int64, enabled bool) *service.Account {
	extra := map[string]any{}
	if enabled {
		extra[service.LargeRequestOnlyExtraKey] = true
	}
	return &service.Account{
		ID:       id,
		Name:     fmt.Sprintf("lro-%d", id),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
		Extra: extra,
	}
}

// TestApplyLargeRequestOnlyExclusionSmallRequest 验证估算 input <= 阈值（256000，
// 小请求）时：开启「仅接大请求」开关的账号必须进入排除集合，未开启账号保留。
func TestApplyLargeRequestOnlyExclusionSmallRequest(t *testing.T) {
	t.Parallel()

	enabled := newLargeRequestOnlyTestAccount(101, true)
	plain := newLargeRequestOnlyTestAccount(202, false)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*enabled, *plain}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw}
	reqLog := zap.NewNop()

	// 不含图短请求：估算远低于 256000 阈值。
	smallBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	estimated, excluded := h.applyLargeRequestOnlyExclusion(
		context.Background(), reqLog, nil, service.PlatformOpenAI, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, smallBody)

	require.Greater(t, estimated, 0, "estimate must succeed for a small request")
	require.LessOrEqual(t, estimated, service.LargeRequestOnlyInputThreshold, "small request estimate must stay below the threshold")
	require.Len(t, excluded, 1, "exactly one large-request-only account must be excluded")
	_, ok := excluded[101]
	require.True(t, ok, "large-request-only account 101 must be excluded for a small request")
	_, ok = excluded[202]
	require.False(t, ok, "non-large-request-only account 202 must not be excluded")
}

// TestApplyLargeRequestOnlyExclusionLargeRequest 验证估算 input > 阈值（大请求）时
// 不排除任何账号：全部账号参与候选，仅返回估算 input。
func TestApplyLargeRequestOnlyExclusionLargeRequest(t *testing.T) {
	t.Parallel()

	enabled := newLargeRequestOnlyTestAccount(101, true)
	plain := newLargeRequestOnlyTestAccount(202, false)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*enabled, *plain}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw}
	reqLog := zap.NewNop()

	// 重复短语会被 BPE 高效压缩，放大到与 sensenova 测试同量级确保远超 256000 阈值。
	huge := strings.Repeat("hello world ", 200000)
	bigBody := []byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, huge))
	estimated, excluded := h.applyLargeRequestOnlyExclusion(
		context.Background(), reqLog, nil, service.PlatformOpenAI, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, bigBody)

	require.Greater(t, estimated, service.LargeRequestOnlyInputThreshold, "large request estimate must exceed the threshold")
	require.Empty(t, excluded, "large request must not exclude any large-request-only account")
}

// TestApplyLargeRequestOnlyExclusionEstimateFailed 验证估算失败（非法 body）时必须
// 失败开放：返回估算 0 + 空排除集合，且不 panic（绝不因估算失败拦请求）。
func TestApplyLargeRequestOnlyExclusionEstimateFailed(t *testing.T) {
	t.Parallel()

	enabled := newLargeRequestOnlyTestAccount(101, true)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*enabled}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw}
	reqLog := zap.NewNop()

	badBody := []byte("{not valid json")
	estimated, excluded := h.applyLargeRequestOnlyExclusion(
		context.Background(), reqLog, nil, service.PlatformOpenAI, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, badBody)

	require.Zero(t, estimated, "estimate failure must report estimated input as 0")
	require.Empty(t, excluded, "estimate failure must fail-open with an empty exclusion set")
}
