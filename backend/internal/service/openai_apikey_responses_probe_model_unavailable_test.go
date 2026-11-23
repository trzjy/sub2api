package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// A 400/404 that only says the probe model does not exist says nothing about
// the /v1/responses endpoint: the verdict must stay inconclusive and the
// account capability must not be overwritten with "unsupported".
func TestResponsesProbeModelUnavailableIsInconclusive(t *testing.T) {
	for _, body := range []string{
		`{"error":{"type":"model_not_found","message":"Model codex-auto-review is not supported by any configured account in this group"}}`,
		`{"error":{"code":"model_not_available"}}`,
		`{"error":{"message":"The model missing does not exist"}}`,
	} {
		require.False(t, responsesProbeVerdictIsConclusive(404, []byte(body)))
		require.True(t, decideResponsesProbeSupport(404, []byte(body)))
		updates := make(chan map[string]any, 1)
		account := Account{ID: 96, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
			"api_key":       "test",
			"base_url":      "https://upstream.example",
			"model_mapping": map[string]any{"probe-model": "probe-upstream-model"},
		}}
		repo := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, updateExtraCalls: updates}
		svc := &AccountTestService{accountRepo: repo, cfg: &config.Config{}, httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)),
		}}}
		svc.ProbeOpenAIAPIKeyResponsesSupport(context.Background(), account.ID)
		select {
		case <-updates:
			t.Fatal("a model failure must not overwrite the account endpoint capability")
		default:
		}
	}
	// A plain 404/405 still means the endpoint does not exist.
	require.True(t, responsesProbeVerdictIsConclusive(404, []byte("404 page not found")))
	require.False(t, decideResponsesProbeSupport(404, []byte("404 page not found")))
	require.False(t, decideResponsesProbeSupport(405, nil))
}

// TestSelectResponsesProbeModelPrefersGeneralTextModel 验证候选选择不按模型名形态过滤：
// 候选来自账号 model_mapping 解析出的上游真名，按字典序取首个，不因名字不是 "gpt-" 前缀
// 而丢弃（管理员可自定义命名）。图片/媒体模型被排除。
func TestSelectResponsesProbeModelPrefersGeneralTextModel(t *testing.T) {
	account := &Account{Credentials: map[string]any{"model_mapping": map[string]any{
		"codex-auto-review": "codex-auto-review", "gpt-image-2": "gpt-image-2", "gpt-5.5": "gpt-5.5",
	}}}
	// 字典序首个 = codex-auto-review；gpt-image-2 是图片模型被排除。
	require.Equal(t, "codex-auto-review", selectResponsesProbeModel(account))
}
