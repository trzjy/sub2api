//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// oauthProbeFakeRepo 只实现恢复探针的窄接口；GetByID 返回预置母账号，其余方法
// 与本轮无关，返回零值即可。绝不连真实 DB。
type oauthProbeFakeRepo struct {
	accounts map[int64]*Account
}

func (r *oauthProbeFakeRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if acc, ok := r.accounts[id]; ok {
		return acc, nil
	}
	return nil, errors.New("account not found")
}

func (r *oauthProbeFakeRepo) ListTempUnschedulableAccounts(context.Context, time.Time, int) ([]*Account, error) {
	return nil, nil
}

func (r *oauthProbeFakeRepo) ClearTempUnschedulableIfGeneration(context.Context, int64, time.Time) (bool, error) {
	return false, nil
}

func (r *oauthProbeFakeRepo) BumpTempUnschedulableReasonIfGeneration(context.Context, int64, time.Time, string) (bool, error) {
	return false, nil
}

func (r *oauthProbeFakeRepo) ListProbeDueAccounts(context.Context, time.Time, int) ([]*Account, error) {
	return nil, nil
}

func (r *oauthProbeFakeRepo) ClearRecoveryProbeComponent(context.Context, int64, string, int64) (bool, error) {
	return false, nil
}

func (r *oauthProbeFakeRepo) RescheduleRecoveryProbeComponent(context.Context, int64, string, int64, time.Time) (bool, error) {
	return false, nil
}

func oauthProbeDirectAccount() *Account {
	return &Account{
		ID:          990,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token-direct"},
	}
}

func TestOpenAIOAuthProbeBuildRequestDirect(t *testing.T) {
	acc := oauthProbeDirectAccount()
	repo := &oauthProbeFakeRepo{}

	req, err := buildOpenAIOAuthProbeRequest(context.Background(), repo, acc, "gpt-5.1")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if got, want := req.URL.String(), chatgptCodexURL; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
	if got, want := req.Method, http.MethodPost; got != want {
		t.Fatalf("method = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer oauth-token-direct"; got != want {
		t.Fatalf("Authorization = %q, want %q (must be access_token, not api_key)", got, want)
	}
	if got, want := req.Host, "chatgpt.com"; got != want {
		t.Fatalf("Host = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("accept"), "text/event-stream"; got != want {
		t.Fatalf("accept = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("OpenAI-Beta"), "responses=experimental"; got != want {
		t.Fatalf("OpenAI-Beta = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("Content-Type"), "application/json"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	payload := string(body)
	if !strings.Contains(payload, `"stream":true`) {
		t.Fatalf("payload missing stream:true: %s", payload)
	}
	if !strings.Contains(payload, `"store":false`) {
		t.Fatalf("payload missing store:false: %s", payload)
	}
}

func TestOpenAIOAuthProbeBuildRequestShadow(t *testing.T) {
	parentID := int64(991)
	parent := &Account{
		ID:          parentID,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token-parent"},
	}
	shadow := &Account{
		ID:              992,
		ParentAccountID: &parentID,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		// 影子自身的 access_token 是误导值：出站必须取母账号的。
		Credentials: map[string]any{"access_token": "shadow-token-must-not-be-used"},
	}
	repo := &oauthProbeFakeRepo{accounts: map[int64]*Account{parentID: parent}}

	req, err := buildOpenAIOAuthProbeRequest(context.Background(), repo, shadow, "gpt-5.1")
	if err != nil {
		t.Fatalf("build shadow request: %v", err)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer oauth-token-parent"; got != want {
		t.Fatalf("Authorization = %q, want mother account token %q", got, want)
	}
}

func TestOpenAIOAuthProbeResponseOK(t *testing.T) {
	completedSSE := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n" +
		"data: [DONE]\n\n"
	errorSSE := "event: error\n" +
		"data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}\n\n"

	tests := []struct {
		name string
		code int
		body string
		want bool
	}{
		{name: "completed SSE", code: http.StatusOK, body: completedSSE, want: true},
		{name: "200 with error frame", code: http.StatusOK, body: errorSSE, want: false},
		{name: "non-2xx", code: http.StatusInternalServerError, body: completedSSE, want: false},
		{name: "401 expired token", code: http.StatusUnauthorized, body: completedSSE, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := probeOpenAIOAuthResponseOK(tt.code, []byte(tt.body)); got != tt.want {
				t.Fatalf("probeOpenAIOAuthResponseOK(%d, %q) = %v, want %v", tt.code, tt.body, got, tt.want)
			}
		})
	}
}

func TestOpenAIOAuthProbePredicate(t *testing.T) {
	tests := []struct {
		name string
		acc  *Account
		want bool
	}{
		{name: "openai oauth", acc: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, want: true},
		{name: "openai setup_token", acc: &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}, want: true},
		{name: "openai apikey", acc: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, want: false},
		{name: "codebuddy oauth", acc: &Account{Platform: PlatformCodeBuddy, Type: AccountTypeOAuth}, want: false},
		{name: "nil", acc: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOpenAIOAuthProbeAccount(tt.acc); got != tt.want {
				t.Fatalf("isOpenAIOAuthProbeAccount(%+v) = %v, want %v", tt.acc, got, tt.want)
			}
		})
	}
}

// oauthProbeAccountWithMapping 造一个带 model_mapping 的 OpenAI OAuth 号。
// mapping 的键是入站（客户端请求）名，值是上游真名（站内既有约定：
// resolveOpenAIForwardModel / Account.ResolveMappedModel 都是键→上游值）。
func oauthProbeAccountWithMapping(mapping map[string]any) *Account {
	return &Account{
		ID:          990,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token", "model_mapping": mapping},
	}
}

// TestOpenAIOAuthProbeSelectModelFromMapping 验证 G1 主修：OAuth 号的探针模型取自
// 账号 model_mapping 解析出的上游真名，不再回落 openai.DefaultTestModel（"gpt-5.4"）——
// 后者在 ChatGPT 订阅号的 Codex 上游不存在（生产实证：400 model_not_found
// "The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account."），
// 探针恒失败 ⇒ 账号恒判不健康 ⇒ 永不解封。
func TestOpenAIOAuthProbeSelectModelFromMapping(t *testing.T) {
	acc := oauthProbeAccountWithMapping(map[string]any{
		"client-alias-a": "gpt-6-astra",
		"client-alias-b": "gpt-5.6-sol",
	})

	got := selectOpenAIOAuthProbeModel(acc, "")
	if got == "" {
		t.Fatal("selectOpenAIOAuthProbeModel = \"\", want a model from account model_mapping")
	}
	if got == openai.DefaultTestModel || got == "gpt-5.4" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, must not fall back to the unsupported placeholder %q", got, openai.DefaultTestModel)
	}
	upstreamValues := map[string]bool{"gpt-6-astra": true, "gpt-5.6-sol": true}
	if !upstreamValues[got] {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want one of the account model_mapping upstream values %v", got, upstreamValues)
	}
}

// TestOpenAIOAuthProbeSelectModelDeterministic 验证选取结果确定可复现：同一份
// model_mapping 反复调用必须给出同一个模型（探针行为可预期，不随 map 遍历顺序漂移）。
func TestOpenAIOAuthProbeSelectModelDeterministic(t *testing.T) {
	acc := oauthProbeAccountWithMapping(map[string]any{
		"client-alias-a": "gpt-6-astra",
		"client-alias-b": "gpt-5.6-sol",
		"client-alias-c": "gpt-6.1-sol",
	})

	first := selectOpenAIOAuthProbeModel(acc, "")
	if first == "" {
		t.Fatal("selectOpenAIOAuthProbeModel = \"\", want a model from account model_mapping")
	}
	for i := 0; i < 50; i++ {
		if got := selectOpenAIOAuthProbeModel(acc, ""); got != first {
			t.Fatalf("selectOpenAIOAuthProbeModel call #%d = %q, want stable %q", i, got, first)
		}
	}
}

// TestOpenAIOAuthProbeSelectModelSkipsImageModels 验证图片/媒体模型不会被选中：
// 文本探针打图片模型必然失败，选中 GPT 文本模型那个。
func TestOpenAIOAuthProbeSelectModelSkipsImageModels(t *testing.T) {
	acc := oauthProbeAccountWithMapping(map[string]any{
		"client-image": "gpt-image-1",
		"client-text":  "gpt-6-astra",
	})

	got := selectOpenAIOAuthProbeModel(acc, "")
	if got != "gpt-6-astra" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want %q (image model must be excluded)", got, "gpt-6-astra")
	}
}

// TestOpenAIOAuthProbeSelectModelNoMappingFallsBackToDefault 验证无 model_mapping 的
// OAuth 号仍回落 openai.DefaultTestModel（既有行为不变，不扩大影响面）。
func TestOpenAIOAuthProbeSelectModelNoMappingFallsBackToDefault(t *testing.T) {
	acc := &Account{
		ID:          990,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
	}

	if got := selectOpenAIOAuthProbeModel(acc, ""); got != openai.DefaultTestModel {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want %q for an account without model_mapping", got, openai.DefaultTestModel)
	}
}

// TestOpenAIOAuthProbeSelectModelExplicitModelWins 验证调用方显式传入的模型
// （模型级限流恢复通道的 scope 模型）优先于账号 model_mapping 的选取。
func TestOpenAIOAuthProbeSelectModelExplicitModelWins(t *testing.T) {
	acc := oauthProbeAccountWithMapping(map[string]any{
		"client-alias-a": "gpt-6-astra",
	})

	if got := selectOpenAIOAuthProbeModel(acc, "gpt-6.1-sol"); got != "gpt-6.1-sol" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want the explicit model %q", got, "gpt-6.1-sol")
	}
}

// TestOpenAIOAuthProbeSelectModelBreakerStateWins 验证熔断 state 记录的触发模型优先于
// model_mapping 选取（它是真实故障面，沿用既有语义）。
func TestOpenAIOAuthProbeSelectModelBreakerStateWins(t *testing.T) {
	acc := oauthProbeAccountWithMapping(map[string]any{
		"client-alias-a": "gpt-6-astra",
	})
	acc.TempUnschedulableReason = `{"model":"gpt-6.1-sol"}`

	if got := selectOpenAIOAuthProbeModel(acc, ""); got != "gpt-6.1-sol" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want the breaker state model %q", got, "gpt-6.1-sol")
	}
}

// TestOpenAIOAuthProbeSelectModelNonOpenAIOAuthAccount 验证非 OpenAI 平台的 OAuth 号
// 不被本选取逻辑命中（沿用既有 isOpenAIOAuthProbeAccount 谓词，不放宽）。
func TestOpenAIOAuthProbeSelectModelNonOpenAIOAuthAccount(t *testing.T) {
	acc := &Account{
		ID:          991,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "cb-token", "model_mapping": map[string]any{"a": "gpt-6-astra"}},
	}

	if got := selectOpenAIOAuthProbeModel(acc, ""); got != "" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want \"\" for a non-OpenAI OAuth account", got)
	}
}
