//go:build unit

package service

// 外审 P2：火山探测取 model_mapping 首个上游模型，但 mapping 值可能失效/当前套餐不支持，
// 单一失效 mapping 会让每次刷新都失败。本文件验证探测按候选模型依次尝试，并在首个模型
// model-not-found 时回落到下一候选（或默认公开模型）。

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// volcanoFallbackUpstream 按请求体里的 model 路由响应：首个失效映射模型返回 400
// model-not-found，默认回落模型返回 200 并带 OpenAI 兼容限流响应头。
type volcanoFallbackUpstream struct {
	calls int
}

func (u *volcanoFallbackUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	body, _ := io.ReadAll(req.Body)
	model := gjson.GetBytes(body, "model").String()
	if model == "ark-bad-model" {
		return &http.Response{
			StatusCode: 400,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"ModelNotFound","message":"The model ark-bad-model does not exist"}}`)),
			Request:    req,
		}, nil
	}
	header := http.Header{}
	header.Set("x-ratelimit-reset-requests", strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10))
	header.Set("x-ratelimit-limit-requests", "100")
	header.Set("x-ratelimit-remaining-requests", "40")
	return &http.Response{
		StatusCode: 200,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(`{"choices":[]}`)),
		Request:    req,
	}, nil
}

func (u *volcanoFallbackUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

// volcanoFallbackRepo 记录 UpdateExtra 调用次数。
type volcanoFallbackRepo struct {
	AccountRepository
	updateExtraCalls int
}

func (r *volcanoFallbackRepo) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	r.updateExtraCalls++
	return nil
}

// TestVolcanoProbeFallbackOnModelNotFound 验证首个映射模型不可用（model-not-found 4xx）
// 时回落到默认公开模型并最终成功，且每次候选仅发一次请求。
func TestVolcanoProbeFallbackOnModelNotFound(t *testing.T) {
	repo := &volcanoFallbackRepo{}
	upstream := &volcanoFallbackUpstream{}
	cfg := cnProbeAllowlistConfig("ark.cn-beijing.volces.com")
	svc := NewCNProviderQuotaService(repo, nil, upstream, cfg)

	account := &Account{
		ID:        1,
		Platform:  PlatformDeepseek,
		Type:      AccountTypeAPIKey,
		Status:    StatusActive,
		Credentials: map[string]any{
			"api_key":       "ark-test-key",
			"base_url":      "https://ark.cn-beijing.volces.com/api/coding",
			"model_mapping": map[string]any{"gpt-4o": "ark-bad-model"},
		},
	}

	res, err := svc.queryUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.Success, "expected fallback to succeed, got error: %s", res.Error)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, 2, upstream.calls, "should have tried both candidate models")
	require.Equal(t, 1, repo.updateExtraCalls, "successful candidate should persist snapshot once")
	require.Len(t, res.Tiers, 2, "5h + weekly tiers expected")
}

// TestVolcanoProbeModels 验证候选模型列表：按 key 确定性排序去重，并追加默认回落模型。
func TestVolcanoProbeModels(t *testing.T) {
	t.Parallel()
	// 多个映射：按 key 排序（gpt-4o < gpt-4o-mini），默认模型追加在末位。
	acc := &Account{
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-4o-mini": "ark-y", "gpt-4o": "ark-x"},
		},
	}
	require.Equal(t, []string{"ark-x", "ark-y", volcanoQuotaProbeDefaultModel}, volcanoProbeModels(acc))

	// 重复值（不同 key 映射到同一上游模型）去重，默认模型仍追加一次。
	dup := &Account{
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"a": "ark-x", "b": "ark-x"},
		},
	}
	require.Equal(t, []string{"ark-x", volcanoQuotaProbeDefaultModel}, volcanoProbeModels(dup))

	// 默认模型已在映射中：不重复追加。
	overlap := &Account{
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"a": volcanoQuotaProbeDefaultModel},
		},
	}
	require.Equal(t, []string{volcanoQuotaProbeDefaultModel}, volcanoProbeModels(overlap))

	// 无映射：仅默认模型。
	require.Equal(t, []string{volcanoQuotaProbeDefaultModel}, volcanoProbeModels(&Account{Platform: PlatformDeepseek}))
}

// TestIsVolcanoModelNotFoundError 验证“模型不可用”类错误的判定（仅 400/404 且 error 指向模型）。
func TestIsVolcanoModelNotFoundError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"model-not-found code", http.StatusBadRequest, `{"error":{"code":"ModelNotFound","message":"model xxx not found"}}`, true},
		{"invalid_request_error with model msg", http.StatusBadRequest, `{"error":{"code":"invalid_request_error","message":"model xxx does not exist"}}`, true},
		{"plain 404", http.StatusNotFound, `{"error":{"message":"The model ` + "`xxx`" + ` is not available"}}`, true},
		{"non-model invalid_request", http.StatusBadRequest, `{"error":{"code":"invalid_request_error","message":"missing required field"}}`, false},
		{"rate limited 429", http.StatusTooManyRequests, `{"error":{"message":"model xxx rate limited"}}`, false},
		{"server error 500", http.StatusInternalServerError, `{"error":{"code":"ModelNotFound","message":"x"}}`, false},
		{"auth error 401", http.StatusUnauthorized, `{"error":{"message":"model xxx not found"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isVolcanoModelNotFoundError([]byte(tc.body), tc.status))
		})
	}
}
