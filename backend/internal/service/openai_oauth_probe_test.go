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
	// 真实终态帧：有 output 数组 + usage（站内既有 Codex completed 帧形状）。
	completedSSE := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":11,\"output_tokens\":5,\"total_tokens\":16}}}\n\n" +
		"data: [DONE]\n\n"
	// 只有 usage、没有 output 数组的终态帧：仍是真实完成（不是空完成帧）。
	usageOnlySSE := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_usage\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
		"data: [DONE]\n\n"
	// 空 completed 帧：只有 id/status，无 output、无 usage、无 error —— 上游静默拒绝
	// 的形状（站内转发侧据此 failover，issue #5009），不得判健康。
	emptyCompletedSSE := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\"}}\n\n" +
		"data: [DONE]\n\n"
	errorSSE := "event: error\n" +
		"data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}\n\n"

	tests := []struct {
		name string
		code int
		body string
		want bool
	}{
		{name: "completed SSE with output and usage", code: http.StatusOK, body: completedSSE, want: true},
		{name: "completed SSE with usage only", code: http.StatusOK, body: usageOnlySSE, want: true},
		{name: "empty completed SSE is silent refusal", code: http.StatusOK, body: emptyCompletedSSE, want: false},
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

// TestOpenAIOAuthProbeResponseOKAcceptsPriorUsageWithEmptyCompleted 是
// SUGGESTION-1 的承重用例：有效 usage 出现在**前序帧**、终态帧本身是空完成帧
// （只有 id/status、无 output/usage/error）时，探针必须判**健康**（true）。
//
// 依据是 openAIResponsesCompletedEventIsEmpty 的文档契约：「The accumulated usage
// is consulted too, because OpenAI may deliver usage on an earlier event.」
// 转发侧调用点传的就是 SSE 帧上累计出来的 usage（parseSSEUsageBytesWithType）。
//
// 若探针传 nil 判定（改前行为），该流会被误判成「上游静默拒绝」⇒ 账号继续停调，
// 修复达不到「自动回池」的目的。
func TestOpenAIOAuthProbeResponseOKAcceptsPriorUsageWithEmptyCompleted(t *testing.T) {
	// 前序帧带真实 usage（OpenAI 允许把 usage 放在更早的事件上）。
	priorUsageSSE := "event: response.in_progress\n" +
		"data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_prior\",\"status\":\"in_progress\"},\"usage\":{\"input_tokens\":11,\"output_tokens\":5,\"total_tokens\":16}}\n\n" +
		// 终态帧是空完成帧：只有 id/status，无 output、无 usage、无 error。
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_prior\",\"status\":\"completed\"}}\n\n" +
		"data: [DONE]\n\n"

	if !probeOpenAIOAuthResponseOK(http.StatusOK, []byte(priorUsageSSE)) {
		t.Fatal("probeOpenAIOAuthResponseOK = false for a stream whose earlier frame carried real usage (want true: not a silent upstream refusal)")
	}
}

// TestOpenAIOAuthProbeResponseOKRejectsEmptyCompleted 是 BLOCKER-2 的承重用例：
// 空 completed 帧（只有 id/status、无 output/usage/error）必须判 false，
// 否则一次探测即可误清不该解除的停调。
//
// 注意：此用例的前序帧**没有**任何 usage，故累计 usage 为零 → 判定原语仍判空完成帧。
func TestOpenAIOAuthProbeResponseOKRejectsEmptyCompleted(t *testing.T) {
	emptyCompletedSSE := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\"}}\n\n" +
		"data: [DONE]\n\n"

	if probeOpenAIOAuthResponseOK(http.StatusOK, []byte(emptyCompletedSSE)) {
		t.Fatal("probeOpenAIOAuthResponseOK = true for an empty completed frame (silent upstream refusal), want false")
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
	// 字典序取首个（可复现）：gpt-5.6-sol < gpt-6-astra。
	if got != "gpt-5.6-sol" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want %q (lexicographically first gpt- candidate)", got, "gpt-5.6-sol")
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

// TestOpenAIOAuthProbeSelectModelNoMappingNoCatalogReturnsEmpty 验证既无
// model_mapping、也无上游目录快照时返回空串——绝不回落 openai.DefaultTestModel
// 即 "gpt-5.4"。后者在 ChatGPT 订阅号的 Codex 上游不存在（生产实证：400
// model_not_found "The 'gpt-5.4' model is not supported when using Codex with a
// ChatGPT account."），探针恒失败 ⇒ 账号恒判不健康 ⇒ 永不解封。
//
// 空串由调用方 probeOpenAIOAuthDataPlane 走既有 degraded 语义
// （"probe account has no model to probe" ⇒ 到期恢复，不伪造失败），
// 与「无模型可探」的真实语义一致。
func TestOpenAIOAuthProbeSelectModelNoMappingNoCatalogReturnsEmpty(t *testing.T) {
	acc := &Account{
		ID:          990,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
	}

	got := selectOpenAIOAuthProbeModel(acc, "")
	if got != "" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want \"\" for an account without model_mapping and without upstream catalog snapshot", got)
	}
	if got == openai.DefaultTestModel || got == "gpt-5.4" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, must never fall back to the unsupported placeholder %q", got, openai.DefaultTestModel)
	}
}

// oauthProbeCatalogSnapshotModels 生产 990/991 的上游目录原文（证据：
// ~/.sub2api-acceptance/oauth-probe-credential-fix-20261122/02-upstream-catalog.md，
// 由 admin 的 sync-upstream 写回 extra.upstream_model_metadata）。
var oauthProbeCatalogSnapshotModels = []string{
	"codex-auto-review",
	"gpt-5.6-luna",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-6-astra",
	"gpt-6-luna",
	"gpt-6-sol",
	"gpt-6.1-sol",
	"gpt-6.1-terra",
	"gpt-6.2-astra",
	"gpt-6.2-sol",
	"gpt-reserve",
}

// oauthProbeAccountWithCatalogSnapshot 造一个「无 model_mapping、但 extra 里已有
// 上游目录快照」的 OpenAI OAuth 号。
func oauthProbeAccountWithCatalogSnapshot(models []string) *Account {
	acc := &Account{
		ID:          990,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
	}
	metadata := make(map[string]UpstreamModelMetadata, len(models))
	for _, id := range models {
		metadata[id] = UpstreamModelMetadata{ID: id}
	}
	acc.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Source:   "openai_codex",
		SyncedAt: "2026-11-22T00:00:00Z",
		Models:   metadata,
	})
	return acc
}

// TestOpenAIOAuthProbeSelectModelIgnoresStaleUpstreamSnapshot 是 BLOCKER-1 的承重用例：
// 能力元数据快照（extra.upstream_model_metadata）**不等于上游实时目录**——它含管理员
// 映射目标、models.dev 补齐项与旧快照，可能保留已下架模型。若探针拿它当模型来源，
// 「映射已移除 gpt-5.4、快照仍留 gpt-5.4 与 gpt-5.6-luna」时枚举快照键排序取首个
// `gpt-` 候选必定选中 gpt-5.4（字典序小于 gpt-5.6-luna）⇒ 上游恒 400 model_not_found
// （生产实证）⇒ 账号恒判不健康。
//
// 故快照不得作探针模型来源：无 mapping 时必须返回空串（走既有 degraded 语义），
// 有 mapping 时只能取 mapping 里的模型。
func TestOpenAIOAuthProbeSelectModelIgnoresStaleUpstreamSnapshot(t *testing.T) {
	// 场景一：无 mapping、快照仍留已下架的 gpt-5.4 与 gpt-5.6-luna。
	// 快照内容 = 生产 990/991 的上游目录原文 + 管理员映射目标补齐的 gpt-5.4
	//（生产实证：映射已移除该模型，但落库快照仍留它）。
	staleSnapshot := append([]string{"gpt-5.4"}, oauthProbeCatalogSnapshotModels...)
	acc := oauthProbeAccountWithCatalogSnapshot(staleSnapshot)

	got := selectOpenAIOAuthProbeModel(acc, "")
	if got != "" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want \"\" (upstream metadata snapshot is not a servable-model catalog)", got)
	}
	if got == "gpt-5.4" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, must never pick the removed model kept in the stale snapshot", got)
	}

	// 场景二：mapping 里有有效模型、快照里留着已下架模型 —— 只能选 mapping 里的。
	acc2 := oauthProbeAccountWithMapping(map[string]any{
		"client-alias-a": "gpt-6-astra",
	})
	acc2.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Source: "openai_codex",
		Models: map[string]UpstreamModelMetadata{
			"gpt-5.4":      {ID: "gpt-5.4"},
			"gpt-5.6-luna": {ID: "gpt-5.6-luna"},
		},
	})

	got2 := selectOpenAIOAuthProbeModel(acc2, "")
	if got2 != "gpt-6-astra" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want %q (only model_mapping may supply the probe model)", got2, "gpt-6-astra")
	}
	if got2 == "gpt-5.4" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, must never pick the removed model kept in the stale snapshot", got2)
	}
}

// TestOpenAIOAuthProbeSelectModelRejectsNonGPTMappingTargets 验证 c) 分支只接受 `gpt-`
// 前缀的通用文本模型：映射目标是账号配置，不能独立证明它仍在上游实时目录；
// 非 `gpt-` 命名的取值（codex-auto-review 一类账号自定义模型）不得被选为探针模型。
func TestOpenAIOAuthProbeSelectModelRejectsNonGPTMappingTargets(t *testing.T) {
	// 映射里全是非 gpt- 取值 ⇒ 视为无候选（返回空串，绝不回落默认模型）。
	acc := oauthProbeAccountWithMapping(map[string]any{
		"client-alias-a": "codex-auto-review",
		"client-alias-b": "my-custom-text-model",
	})

	got := selectOpenAIOAuthProbeModel(acc, "")
	if got != "" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want \"\" when every mapping target lacks the gpt- prefix", got)
	}
	if got == openai.DefaultTestModel || got == "gpt-5.4" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, must never fall back to the unsupported placeholder %q", got, openai.DefaultTestModel)
	}

	// 混合映射：非 gpt- 取值不得入选，选中的必须是 gpt- 那个。
	acc2 := oauthProbeAccountWithMapping(map[string]any{
		"client-alias-a": "codex-auto-review",
		"client-alias-b": "gpt-6-astra",
	})
	if got2 := selectOpenAIOAuthProbeModel(acc2, ""); got2 != "gpt-6-astra" {
		t.Fatalf("selectOpenAIOAuthProbeModel = %q, want %q (non gpt- mapping targets must be skipped)", got2, "gpt-6-astra")
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
