//go:build unit

package service

// 恢复探针统一协议分派：账号 quota 三 kind、模型级 scope 不二次映射、
// Antigravity 双分量、失败关闭的定向回归。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

// 账号级 quota 的 quota / actual / both 三种 kind 都必须走同一协议分派，
// 不得落 OpenAI 数据面（Anthropic 账号的 api_key 不在 GetOpenAIProtocolAPIKey 里）。
func TestRecoveryProbeQuotaKindsUseAnthropicProtocol(t *testing.T) {
	for _, kind := range []string{recoveryKindQuota, recoveryKindActual, recoveryKindBoth} {
		t.Run(kind, func(t *testing.T) {
			upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, anthropicTextSSE)
			p := anthropicProbeService(upstream)

			ok, err := p.probeQuotaComponent(context.Background(), anthropicRecoveryProbeAccount(501, true), kind)
			require.NoError(t, err)
			require.True(t, ok)

			rec := upstream.lastRecord(t)
			require.Equal(t, "/v1/messages", rec.Path, "kind=%s 必须走 anthropic，不得落 OpenAI", kind)
			require.Equal(t, anthropicProbeFakeKey, rec.Header.Get("x-api-key"))
			require.Empty(t, rec.Header.Get("Authorization"), "kind=%s 不得落 OpenAI Bearer", kind)
			if kind == recoveryKindBoth {
				require.Equal(t, 2, anthropicProbeRecordCount(upstream), "both 必须两项都探")
			}
		})
	}
}

// 模型级 scope 已是上游模型名：即使账号 model_mapping 把该名字映射到别的模型，
// *WithModel 变体也不得二次映射，发给上游的 model 必须等于写入的 scope。
func TestModelScopeProbeDoesNotRemapScope(t *testing.T) {
	scope := "claude-upstream-x"
	acc := &Account{
		ID:       601,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://anthropic-probe.example.com",
			"api_key":  anthropicProbeFakeKey,
			"model_mapping": map[string]any{
				"claude-x": scope,
				scope:      "WRONG-DOUBLE-MAPPED",
			},
		},
		Concurrency: 1,
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, anthropicTextSSE)
	p := anthropicProbeService(upstream)

	ok, err := p.probeModelScopeUpstream(context.Background(), acc, scope)
	require.NoError(t, err)
	require.True(t, ok)

	rec := upstream.lastRecord(t)
	require.Equal(t, "/v1/messages", rec.Path)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Equal(t, scope, body["model"], "scope 不得被 GetMappedModel 二次映射")
}

// Antigravity transport 无安全已有数据面能力：family 为空 → 明确 error 失败关闭，
// 不发出任何请求、不清除停调；不做 OpenAI fallback。
func TestAntigravityTransportProbeFailsClosed(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"choices":[]}`)
	p := anthropicProbeService(upstream)
	acc := &Account{
		ID:       701,
		Platform: PlatformAntigravity,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "fake-antigravity-key",
			"model_mapping": map[string]any{"gemini-x": "gemini-upstream-x"},
		},
		Concurrency: 1,
	}

	ok, err := p.probeUpstream(context.Background(), acc)
	require.Error(t, err, "Antigravity transport 必须返回明确 error（不确定，保持停调）")
	require.Contains(t, err.Error(), "unsupported account")
	require.False(t, ok)
	require.Zero(t, anthropicProbeRecordCount(upstream), "失败关闭不得发 OpenAI fallback 请求")
}

// Antigravity quota 分量保持专用 ProbeUserQuota，不能只测 AI credits。
func TestAntigravityQuotaComponentUsesProbeUserQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1internal:retrieveUserQuota", r.URL.Path)
		require.Equal(t, "Bearer fake-antigravity-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"buckets":[{"modelId":"gemini-3.8-flash","tokenType":"REQUESTS","remainingFraction":0.25,"resetTime":"2026-01-02T00:00:00Z"}]}`))
	}))
	defer server.Close()

	oldBaseURLs := antigravity.BaseURLs
	oldAvailability := antigravity.DefaultURLAvailability
	t.Cleanup(func() {
		antigravity.BaseURLs = oldBaseURLs
		antigravity.DefaultURLAvailability = oldAvailability
	})
	antigravity.BaseURLs = []string{server.URL}
	antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)

	acc := &Account{
		ID:       702,
		Platform: PlatformAntigravity,
		Type:     AccountTypeUpstream,
		Credentials: map[string]any{
			"api_key": "fake-antigravity-key",
		},
	}
	p := &AccountHealthRecoveryProbeService{
		cfg:               &config.Config{},
		antigravityQuota:  NewAntigravityQuotaFetcher(nil, &config.Config{}),
		antigravityTokens: NewAntigravityTokenProvider(nil, nil, nil),
	}

	ok, err := p.probeQuotaComponent(context.Background(), acc, recoveryKindQuota)
	require.NoError(t, err)
	require.True(t, ok, "个人额度 bucket 有余量必须判恢复")
}

// 错误响应与空响应均失败关闭（ok=false，无 error），不清除停调。
func TestRecoveryProbeFailClosedOnBadResponses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"upstream 500", http.StatusInternalServerError, anthropicTextSSE},
		{"2xx empty body", http.StatusOK, ""},
		{"2xx non-sse body", http.StatusOK, `{"error":{"message":"upstream failure"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newCodeBuddyProbeUpstreamStub(t, tc.status, tc.body)
			p := anthropicProbeService(upstream)
			ok, err := p.probeUpstream(context.Background(), anthropicRecoveryProbeAccount(801, true))
			require.NoError(t, err)
			require.False(t, ok, "非成功响应必须失败关闭")
		})
	}
}

// 缺凭据的 OpenAI 兼容账号仍按配置类 error 失败关闭（保持既有 degraded 语义）。
func TestRecoveryProbeOpenAIWithoutAPIKeyFailsClosed(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"choices":[]}`)
	p := anthropicProbeService(upstream)
	acc := &Account{
		ID:          802,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{"m": "up-m"}},
		Extra:       map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}
	ok, err := p.probeUpstream(context.Background(), acc)
	require.Error(t, err)
	require.False(t, ok)
	require.Zero(t, anthropicProbeRecordCount(upstream))
}
