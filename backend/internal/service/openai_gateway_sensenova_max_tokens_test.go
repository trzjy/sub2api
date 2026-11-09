//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const sensenovaTestBaseURL = "https://token.sensenova.cn/v1/chat/completions"

// sensenovaTestAccount 构造指向 sensenova 上游的 APIKey 账号。
// platform 不参与判定，仅用于覆盖生产里 deepseek/kimi 两种挂载方式。
func sensenovaTestAccount(platform string, id int64) *Account {
	return &Account{
		ID:       id,
		Name:     "sensenova",
		Platform: platform,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": sensenovaTestBaseURL,
		},
	}
}

func TestClampSensenovaUpstreamMaxTokens(t *testing.T) {
	t.Parallel()

	account := sensenovaTestAccount(PlatformDeepseek, 416)

	tests := []struct {
		name    string
		account *Account
		body    string
		want    string
		raw     bool // want 非法 JSON 时按原始字节比较
	}{
		{
			name:    "max_tokens above cap is clamped",
			account: account,
			body:    `{"model":"deepseek-v4.1-flash-1017","max_tokens":128000}`,
			want:    `{"model":"deepseek-v4.1-flash-1017","max_tokens":65536}`,
		},
		{
			name:    "max_completion_tokens above cap is clamped",
			account: account,
			body:    `{"model":"deepseek-v4.1-flash-1017","max_completion_tokens":128000}`,
			want:    `{"model":"deepseek-v4.1-flash-1017","max_completion_tokens":65536}`,
		},
		{
			name:    "both fields above cap are clamped",
			account: account,
			body:    `{"model":"m","max_tokens":80000,"max_completion_tokens":90000}`,
			want:    `{"model":"m","max_tokens":65536,"max_completion_tokens":65536}`,
		},
		{
			name:    "value equal to cap is kept",
			account: account,
			body:    `{"model":"m","max_tokens":65536}`,
			want:    `{"model":"m","max_tokens":65536}`,
		},
		{
			name:    "value below cap is kept",
			account: account,
			body:    `{"model":"m","max_completion_tokens":4096}`,
			want:    `{"model":"m","max_completion_tokens":4096}`,
		},
		{
			name:    "missing fields are left untouched",
			account: account,
			body:    `{"model":"m"}`,
			want:    `{"model":"m"}`,
		},
		{
			name:    "non-numeric max_tokens is left untouched",
			account: account,
			body:    `{"model":"m","max_tokens":"huge"}`,
			want:    `{"model":"m","max_tokens":"huge"}`,
		},
		{
			name:    "invalid json is left untouched",
			account: account,
			body:    `{"model":"m","max_tokens":`,
			want:    `{"model":"m","max_tokens":`,
			raw:     true,
		},
		{
			name:    "kimi platform with same base url is clamped",
			account: sensenovaTestAccount(PlatformKimi, 418),
			body:    `{"model":"kimi","max_tokens":200000}`,
			want:    `{"model":"kimi","max_tokens":65536}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := clampSensenovaUpstreamMaxTokens(test.account, []byte(test.body))
			if test.raw {
				require.Equal(t, test.want, string(got))
				return
			}
			require.JSONEq(t, test.want, string(got))
		})
	}

	// nil 账号与空 body 原样返回（字节不变）。
	body := []byte(`{"model":"m","max_tokens":128000}`)
	require.Equal(t, body, clampSensenovaUpstreamMaxTokens(nil, body))
	require.Equal(t, []byte(nil), clampSensenovaUpstreamMaxTokens(account, nil))
	require.Equal(t, []byte{}, clampSensenovaUpstreamMaxTokens(account, []byte{}))
}

func TestIsSensenovaBaseURL(t *testing.T) {
	t.Parallel()

	account := sensenovaTestAccount(PlatformDeepseek, 416)
	over := []byte(`{"model":"m","max_tokens":128000}`)

	hits := []string{
		"https://token.sensenova.cn/v1/chat/completions",
		"https://token.sensenova.cn/v1/chat/completions/",
		"https://TOKEN.SENSENOVA.CN/v1/chat/completions",
		"  https://token.sensenova.cn/v1/chat/completions  ",
	}
	for _, baseURL := range hits {
		t.Run("hit "+baseURL, func(t *testing.T) {
			t.Parallel()
			require.True(t, isSensenovaBaseURL(baseURL))
			account := sensenovaTestAccount(PlatformDeepseek, 416)
			account.Credentials["base_url"] = baseURL
			require.JSONEq(t, `{"model":"m","max_tokens":65536}`,
				string(clampSensenovaUpstreamMaxTokens(account, over)))
		})
	}

	misses := []string{
		"http://token.sensenova.cn/v1/chat/completions",
		"https://token.sensenova.cn/v1/chat/completions?x=1",
		"https://user:pass@token.sensenova.cn/v1/chat/completions",
		"https://api.deepseek.com",
		"https://ollama.com/v1",
		"https://token.sensenova.cn.evil.com/v1/chat/completions",
	}
	for _, baseURL := range misses {
		t.Run("miss "+baseURL, func(t *testing.T) {
			t.Parallel()
			require.False(t, isSensenovaBaseURL(baseURL))
			account := sensenovaTestAccount(PlatformDeepseek, 416)
			account.Credentials["base_url"] = baseURL
			got := clampSensenovaUpstreamMaxTokens(account, over)
			require.Equal(t, string(over), string(got))
		})
	}

	// 对照账号本身命中。
	require.True(t, isSensenovaBaseURL(account.GetOpenAIBaseURL()))
}

// TestForwardAnthropicViaRawChatCompletions_SensenovaClampsMaxTokens 端到端覆盖
// 本次事故路径：/v1/messages 入站 max_tokens=128000 经 Anthropic→CC 转换变成
// max_completion_tokens 后，出站前被压到 sensenova 上限。
func TestForwardAnthropicViaRawChatCompletions_SensenovaClampsMaxTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)

	account := sensenovaTestAccount(PlatformDeepseek, 416)
	account.Credentials["api_protocol"] = APIProtocolChatCompletions

	body := []byte(`{"model":"deepseek-v4.1-flash-1017","max_tokens":128000,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.forwardAnthropicViaRawChatCompletions(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), account, body, "")
	require.Error(t, err)
	require.Equal(t, int64(65536), gjson.GetBytes(upstream.lastBody, "max_completion_tokens").Int())

	// 回归：非 sensenova 上游（api.deepseek.com）走同一函数，出站值保持请求原值。
	official := &Account{
		ID:       312,
		Name:     "official-deepseek",
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"base_url":     "https://api.deepseek.com",
			"api_protocol": APIProtocolChatCompletions,
		},
	}
	officialUpstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	officialSvc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: officialUpstream}

	_, err = officialSvc.forwardAnthropicViaRawChatCompletions(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), official, body, "")
	require.Error(t, err)
	require.Equal(t, int64(128000), gjson.GetBytes(officialUpstream.lastBody, "max_completion_tokens").Int())
}

func TestSensenovaContextWindowCap(t *testing.T) {
	t.Parallel()
	// 商汤官方物理上下文窗口上限：256K = 262144 tokens（派发单 SENSENOVA-256K-01）。
	require.Equal(t, 262144, SensenovaContextWindowCap)
}

func TestSensenovaAccountIDs(t *testing.T) {
	t.Parallel()

	sensenovaDS := sensenovaTestAccount(PlatformDeepseek, 416)
	sensenovaKimi := sensenovaTestAccount(PlatformKimi, 418)
	other := &Account{
		ID:       200,
		Name:     "openai",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
	}

	got := sensenovaAccountIDs([]Account{*sensenovaDS, *sensenovaKimi, *other})
	require.Len(t, got, 2, "only sensenova upstream accounts must be excluded")
	require.Contains(t, got, int64(416))
	require.Contains(t, got, int64(418))
	require.NotContains(t, got, int64(200), "non-sensenova account must not be excluded")
}

func TestAccountMaxInputTokensOf(t *testing.T) {
	t.Parallel()
	if _, ok := AccountMaxInputTokensOf(nil); ok {
		t.Fatal("nil 账号应视为未声明")
	}
	none := &Account{ID: 1, Extra: map[string]any{}}
	if _, ok := AccountMaxInputTokensOf(none); ok {
		t.Fatal("未声明键应视为无上限")
	}
	zero := &Account{ID: 2, Extra: map[string]any{AccountMaxInputExtraKey: 0}}
	if _, ok := AccountMaxInputTokensOf(zero); ok {
		t.Fatal("上限 <=0 应视为非法")
	}
	declared := &Account{ID: 3, Extra: map[string]any{AccountMaxInputExtraKey: 256000}}
	if max, found := AccountMaxInputTokensOf(declared); !found || max != 256000 {
		t.Fatalf("期望 256000,true，得 %d,%v", max, found)
	}
}

func TestAccountMaxInputAccountIDs(t *testing.T) {
	t.Parallel()
	limited := Account{ID: 442, Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Extra: map[string]any{AccountMaxInputExtraKey: 256000}}
	unlimited := Account{ID: 200, Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Extra: map[string]any{}}
	invalid := Account{ID: 201, Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Extra: map[string]any{AccountMaxInputExtraKey: -1}}
	accounts := []Account{limited, unlimited, invalid}

	if got := accountMaxInputAccountIDs(accounts, 256000); len(got) != 0 {
		t.Fatalf("估算==上限不应排除，得 %v", got)
	}
	got := accountMaxInputAccountIDs(accounts, 256001)
	if _, ok := got[442]; !ok {
		t.Fatalf("估算>上限必须排除 442，得 %v", got)
	}
	if len(got) != 1 {
		t.Fatalf("未声明/非法值账号零行为变化，只应排除 442，得 %v", got)
	}
}

func TestSensenovaExcludedIDs(t *testing.T) {
	t.Parallel()

	sensenova := sensenovaTestAccount(PlatformDeepseek, 416)
	other := &Account{
		ID:       200,
		Name:     "openai",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
	}
	// 通过调度快照注入候选账号，复用 listSchedulableAccounts 的既有候选列举路径。
	svc := &OpenAIGatewayService{
		schedulerSnapshot: &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{snapshotAccounts: []*Account{sensenova, other}}},
	}

	ids, err := svc.SensenovaExcludedIDs(context.Background(), nil, PlatformOpenAI)
	require.NoError(t, err)
	require.Len(t, ids, 1, "exactly one sensenova account must be excluded")
	_, ok := ids[416]
	require.True(t, ok, "sensenova account 416 must be excluded")
	_, ok = ids[200]
	require.False(t, ok, "non-sensenova account 200 must not be excluded")
}

func TestSensenovaEmptyStreamFailover(t *testing.T) {
	t.Parallel()

	svc := &OpenAIGatewayService{}
	account := sensenovaTestAccount(PlatformDeepseek, 416)
	fe := svc.failoverSensenovaEmptyStream(context.Background(), account, "sensenova-6.8-flash-lite")
	require.NotNil(t, fe)
	// 必须触发换号重试（handler 会把当前商汤账号并入 failedAccountIDs）。
	require.True(t, fe.ShouldRetryNextAccount(), "empty stream must trigger next-account failover")
	require.Equal(t, SensenovaEmptyStreamReason, fe.Reason)
	require.Equal(t, GatewayFailureScopeRequest, fe.Scope)
	require.False(t, fe.RequestScopedTransient, "must switch account, not retry on same account")
	require.False(t, fe.RetryableOnSameAccount, "must switch account, not retry on same account")
}
