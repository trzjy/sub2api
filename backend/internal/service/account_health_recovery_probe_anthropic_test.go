package service

// 恢复探针统一协议分派回归（412 / 1006 根因 + 协议矩阵）。
// 账号级 transport/quota、模型级 rate-limit 三个分量必须经同一事实源
// probeProtocolFamily + buildRecoveryProbeRequestSpec 分派：原生 Anthropic、
// 多协议显式 anthropic 走 /v1/messages；Gemini APIKey 走 generateContent；
// Grok APIKey 走专用 base + api_key；OpenAI 兼容走 chat/responses。
// 无安全数据面能力的协议（Antigravity 等）失败关闭，不做 OpenAI fallback。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

const (
	anthropicProbeFakeKey = "sk-anthropic-probe-test-only"
	geminiProbeFakeKey    = "fake-gemini-probe-key"
	grokProbeFakeKey      = "fake-grok-probe-key"
)

func anthropicRecoveryProbeAccount(id int64, withAPIKey bool) *Account {
	credentials := map[string]any{
		"base_url":      "https://anthropic-probe.example.com",
		"model_mapping": map[string]any{"claude-x": "claude-upstream-x"},
	}
	if withAPIKey {
		credentials["api_key"] = anthropicProbeFakeKey
	}
	return &Account{
		ID:          id,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: credentials,
		Concurrency: 1,
	}
}

// multiProtocolAnthropicAccount 是多协议 APIKey 平台显式 api_protocol=anthropic：
// 协议优先于平台默认（GetAPIProtocol 已覆盖），必须走 anthropic family。
func multiProtocolAnthropicAccount(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformZhipu,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_protocol":  string(APIProtocolAnthropic),
			"base_url":      "https://anthropic-probe.example.com",
			"api_key":       anthropicProbeFakeKey,
			"model_mapping": map[string]any{"claude-x": "claude-upstream-x"},
		},
		Concurrency: 1,
	}
}

func geminiAPIKeyRecoveryProbeAccount(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformGemini,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       geminiProbeFakeKey,
			"base_url":      "https://gemini-probe.example.com",
			"model_mapping": map[string]any{"gemini-x": "gemini-upstream-x"},
		},
		Concurrency: 1,
	}
}

// grok APIKey 账号：GetOpenAIProtocolAPIKey / GetOpenAIBaseURL 对 grok 均为空，
// 必须靠共享 builder 里的 GetGrokBaseURL / GetCredential("api_key") 回退。
func grokAPIKeyRecoveryProbeAccount(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformGrok,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       grokProbeFakeKey,
			"base_url":      "https://grok-probe.example.com",
			"model_mapping": map[string]any{"grok-x": "grok-upstream-x"},
		},
		Concurrency: 1,
	}
}

func anthropicProbeService(upstream HTTPUpstream) *AccountHealthRecoveryProbeService {
	return &AccountHealthRecoveryProbeService{cfg: &config.Config{}, httpUpstream: upstream}
}

func anthropicProbeRecordCount(upstream *codeBuddyProbeUpstreamStub) int {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return len(upstream.records)
}

const anthropicTextSSE = "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"h\"}}\n\n"

// 原生 Anthropic：分派到 /v1/messages，带 anthropic-version 与 x-api-key，
// body.model 为映射后的上游模型；200 + content_block_delta 文本增量 → ok=true。
func TestAnthropicRecoveryProbeUsesMessagesEndpointAndSucceedsOnText(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, anthropicTextSSE)
	p := anthropicProbeService(upstream)

	ok, err := p.probeUpstream(context.Background(), anthropicRecoveryProbeAccount(412, true))
	require.NoError(t, err)
	require.True(t, ok, "Anthropic 数据面返回文本增量必须判健康")

	rec := upstream.lastRecord(t)
	require.Equal(t, http.MethodPost, rec.Method)
	require.Equal(t, "/v1/messages", rec.Path, "必须打 /v1/messages，不是 OpenAI chat/responses 端点")
	require.Equal(t, "2023-06-01", rec.Header.Get("anthropic-version"))
	require.Equal(t, anthropicProbeFakeKey, rec.Header.Get("x-api-key"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Equal(t, "claude-upstream-x", body["model"], "body.model 必须是 model_mapping 映射后的上游模型")
	require.Equal(t, true, body["stream"])
	require.Equal(t, float64(16), body["max_tokens"])
}

// 多协议显式 api_protocol=anthropic：协议优先于平台默认，仍走 /v1/messages。
func TestRecoveryProbeMultiProtocolExplicitAnthropic(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, anthropicTextSSE)
	p := anthropicProbeService(upstream)

	ok, err := p.probeUpstream(context.Background(), multiProtocolAnthropicAccount(413))
	require.NoError(t, err)
	require.True(t, ok)

	rec := upstream.lastRecord(t)
	require.Equal(t, "/v1/messages", rec.Path)
	require.Equal(t, anthropicProbeFakeKey, rec.Header.Get("x-api-key"))
	require.Empty(t, rec.Header.Get("Authorization"), "不得落 OpenAI Bearer 分支")
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Equal(t, "claude-upstream-x", body["model"])
}

// Gemini APIKey：走 streamGenerateContent SSE，key 在 query，body 是 contents 而非 messages。
func TestRecoveryProbeGeminiAPIKeyUsesGenerateContent(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"h\"}]}}]}\n\n")
	p := anthropicProbeService(upstream)

	ok, err := p.probeUpstream(context.Background(), geminiAPIKeyRecoveryProbeAccount(414))
	require.NoError(t, err)
	require.True(t, ok, "Gemini SSE 文本增量必须判健康")

	rec := upstream.lastRecord(t)
	require.Equal(t, "/v1beta/models/gemini-upstream-x:streamGenerateContent", rec.Path)
	require.Empty(t, rec.Header.Get("Authorization"), "Gemini key 走 query，不是 OpenAI Bearer")
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Contains(t, body, "contents")
	require.NotContains(t, body, "messages", "不得落 OpenAI chat 形状")
}

// Grok APIKey：共享 builder 必须用 GetGrokBaseURL + credentials.api_key，
// 端点 /v1/responses（能力标记缺省），body.model 不二次映射。
func TestRecoveryProbeGrokAPIKeyUsesGrokEndpointAndKey(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"id":"resp-1","object":"response"}`)
	p := anthropicProbeService(upstream)

	ok, err := p.probeUpstream(context.Background(), grokAPIKeyRecoveryProbeAccount(415))
	require.NoError(t, err)
	require.True(t, ok)

	rec := upstream.lastRecord(t)
	require.Equal(t, "/v1/responses", rec.Path)
	require.Equal(t, "Bearer "+grokProbeFakeKey, rec.Header.Get("Authorization"),
		"grok 必须用 credentials.api_key，不得报 probe account has no api key")
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Equal(t, "grok-upstream-x", body["model"])
	require.Contains(t, body, "input")
	require.NotContains(t, body, "messages")
}

// 200 但无文本增量（只有 message_start / [DONE]）→ 失败关闭。
func TestAnthropicRecoveryProbeFailsWithoutTextDelta(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\"}}\n\ndata: [DONE]\n\n")
	p := anthropicProbeService(upstream)

	ok, err := p.probeUpstream(context.Background(), anthropicRecoveryProbeAccount(412, true))
	require.NoError(t, err)
	require.False(t, ok, "2xx 但无 content_block_delta 文本必须失败关闭")
}

// 无 api_key 的 Anthropic 账号 → 配置类 error 失败关闭，且不发出任何请求。
func TestAnthropicRecoveryProbeNoAPIKeyReturnsErrorWithoutRequest(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, anthropicTextSSE)
	p := anthropicProbeService(upstream)

	ok, err := p.probeUpstream(context.Background(), anthropicRecoveryProbeAccount(1006, false))
	require.Error(t, err, "无 api key 必须返回配置错误（降级到期恢复）")
	require.False(t, ok)
	require.Zero(t, anthropicProbeRecordCount(upstream), "失败关闭时不得发出探测请求")
}

// 非 Anthropic 的 OpenAI apikey 账号仍走原数据面，行为不变。
func TestOpenAIApiKeyRecoveryProbeUnchanged(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"content":"hi"}}]}`)
	p := anthropicProbeService(upstream)
	acc := &Account{
		ID:       999,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "sk-openai-probe-test-only",
			"base_url":      "https://openai-probe.example.com",
			"model_mapping": map[string]any{"probe-model": "probe-upstream-model"},
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}

	ok, err := p.probeUpstream(context.Background(), acc)
	require.NoError(t, err)
	require.True(t, ok, "OpenAI 数据面成功体必须判健康")

	rec := upstream.lastRecord(t)
	require.Equal(t, "/v1/chat/completions", rec.Path, "OpenAI 账号端点不变")
	require.Equal(t, "Bearer sk-openai-probe-test-only", rec.Header.Get("Authorization"))
	require.Empty(t, rec.Header.Get("anthropic-version"), "OpenAI 探测不得带 anthropic 头")
}
