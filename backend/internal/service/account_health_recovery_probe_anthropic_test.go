package service

// 原生 Anthropic 平台账号恢复探针分派回归（412 / 1006 根因）。
// 这类账号 credentials 确有 api_key，但 GetOpenAIProtocolAPIKey() 不覆盖 Anthropic
// 平台，落 OpenAI 数据面恒报 "probe account has no api key" 并重排 probe_due_at，
// 账号永不回池。修复在 probeUpstream 内分派到 probeAnthropicDataPlane，复用
// buildAnthropicMessagesSpec（/v1/messages）与 probeSSEDataHasText 两个单一事实源。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

const anthropicProbeFakeKey = "sk-anthropic-probe-test-only"

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

func anthropicProbeService(upstream HTTPUpstream) *AccountHealthRecoveryProbeService {
	return &AccountHealthRecoveryProbeService{cfg: &config.Config{}, httpUpstream: upstream}
}

func anthropicProbeRecordCount(upstream *codeBuddyProbeUpstreamStub) int {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return len(upstream.records)
}

// a) + b) 分派到 /v1/messages，带 anthropic-version 与 x-api-key，body.model 为映射后的
// 上游模型；200 + content_block_delta 文本增量 → ok=true。
func TestAnthropicRecoveryProbeUsesMessagesEndpointAndSucceedsOnText(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"h\"}}\n\n")
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

// c) 200 但无文本增量（只有 message_start / [DONE]）→ 失败关闭。
func TestAnthropicRecoveryProbeFailsWithoutTextDelta(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\"}}\n\ndata: [DONE]\n\n")
	p := anthropicProbeService(upstream)

	ok, err := p.probeUpstream(context.Background(), anthropicRecoveryProbeAccount(412, true))
	require.NoError(t, err)
	require.False(t, ok, "2xx 但无 content_block_delta 文本必须失败关闭")
}

// d) 无 api_key 的 Anthropic 账号 → 从 buildAnthropicMessagesSpec 传播 error，
// 且不发出任何 OpenAI 数据面请求。
func TestAnthropicRecoveryProbeNoAPIKeyReturnsErrorWithoutRequest(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"h\"}}\n\n")
	p := anthropicProbeService(upstream)

	ok, err := p.probeUpstream(context.Background(), anthropicRecoveryProbeAccount(1006, false))
	require.Error(t, err, "无 api key 必须返回配置错误（降级到期恢复）")
	require.False(t, ok)
	require.Zero(t, anthropicProbeRecordCount(upstream), "不得发出 OpenAI 数据面请求")
}

// e) 非 Anthropic 的 OpenAI apikey 账号仍走原数据面，行为不变。
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
