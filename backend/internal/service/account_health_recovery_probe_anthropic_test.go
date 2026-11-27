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
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
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

// Gemini Upstream 型账号与 APIKey 同属可探测族：同样走 streamGenerateContent，
// 不得因 Type=upstream 落 OpenAI 分支或失败关闭。
func TestRecoveryProbeGeminiUpstreamUsesGenerateContent(t *testing.T) {
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"h\"}]}}]}\n\n")
	p := anthropicProbeService(upstream)
	acc := geminiAPIKeyRecoveryProbeAccount(416)
	acc.Type = AccountTypeUpstream

	ok, err := p.probeUpstream(context.Background(), acc)
	require.NoError(t, err)
	require.True(t, ok, "Gemini Upstream 文本增量必须判健康")

	rec := upstream.lastRecord(t)
	require.Equal(t, "/v1beta/models/gemini-upstream-x:streamGenerateContent", rec.Path,
		"Upstream 型 Gemini 必须走 generateContent，不得落 OpenAI 端点")
	require.Empty(t, rec.Header.Get("Authorization"), "Gemini key 走 query，不是 OpenAI Bearer")
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Contains(t, body, "contents")
	require.NotContains(t, body, "messages")
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

	// 凭据/端点必须来自 Grok 专用源，不得依赖 OpenAI helper 先试再 fallback：
	// 直接断言共享 helper 的取值与 GetGrokBaseURL/credentials.api_key 逐字一致。
	acc := grokAPIKeyRecoveryProbeAccount(415)
	require.Equal(t, strings.TrimSpace(acc.GetGrokBaseURL()), openAIProbeBaseURL(acc),
		"Grok base 必须取自 GetGrokBaseURL，不得先取 GetOpenAIBaseURL")
	require.Equal(t, strings.TrimSpace(acc.GetCredential("api_key")), openAIProbeAPIKey(acc),
		"Grok key 必须取自 credentials.api_key，不得先取 GetOpenAIProtocolAPIKey")
	require.Empty(t, acc.GetOpenAIBaseURL(), "前提坐实：Grok 号在 OpenAI base helper 上为空")
	require.Empty(t, acc.GetOpenAIProtocolAPIKey(), "前提坐实：Grok 号在 OpenAI key helper 上为空")
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

// truncatedProbeBody 先吐出 payload 字节，随后返回非 EOF 读错误，
// 模拟「已给出文本增量、但流中途连接中断」的截断 SSE 响应。
type truncatedProbeBody struct {
	payload []byte
	off     int
	err     error
	errored bool
}

func (b *truncatedProbeBody) Read(p []byte) (int, error) {
	if b.off < len(b.payload) {
		n := copy(p, b.payload[b.off:])
		b.off += n
		return n, nil
	}
	b.errored = true
	return 0, b.err
}

func (b *truncatedProbeBody) Close() error { return nil }

// truncatedProbeUpstream 直接回放一个固定状态的响应，不经过 httptest 真网络栈，
// 便于往 body 里注入读错误。
type truncatedProbeUpstream struct {
	status int
	body   io.ReadCloser
	calls  int
}

func (s *truncatedProbeUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return s.response(), nil
}

func (s *truncatedProbeUpstream) DoWithTLS(_ *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.response(), nil
}

func (s *truncatedProbeUpstream) response() *http.Response {
	s.calls++
	return &http.Response{StatusCode: s.status, Header: make(http.Header), Body: s.body}
}

// FIX-1 对齐：SSE 先给出合法 content_block_delta 文本帧、随后读取报错的截断流
// 必须失败关闭（ok=false）；否则会把部分字节里的文本增量误判为恢复成功并清除停调。
func TestAnthropicRecoveryProbeFailsOnTruncatedStream(t *testing.T) {
	body := &truncatedProbeBody{
		payload: []byte(anthropicTextSSE),
		err:     errors.New("simulated upstream stream interruption"),
	}
	upstream := &truncatedProbeUpstream{status: http.StatusOK, body: body}
	p := anthropicProbeService(upstream)

	ok, err := p.probeUpstream(context.Background(), anthropicRecoveryProbeAccount(412, true))
	require.NoError(t, err)
	require.False(t, ok, "截断流即便先吐出文本增量也必须失败关闭，不得清除停调")

	require.Equal(t, 1, upstream.calls, "探测必须真实发出")
	require.True(t, body.errored, "必须实际触发注入的读错误，避免测试空过")
}

// 模型级 scope 已是上游模型名：Gemini 的 generateContent 路径不得再按账号
// model_mapping 二次映射，否则会打到 WRONG-DOUBLE-MAPPED 这个不存在的模型。
func TestModelScopeGeminiProbeDoesNotRemapScope(t *testing.T) {
	const scope = "gemini-upstream-x"
	acc := geminiAPIKeyRecoveryProbeAccount(417)
	acc.Credentials["model_mapping"] = map[string]any{
		"gemini-x": scope,
		scope:      "WRONG-DOUBLE-MAPPED",
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"h\"}]}}]}\n\n")
	p := anthropicProbeService(upstream)

	ok, err := p.probeModelScopeUpstream(context.Background(), acc, scope)
	require.NoError(t, err)
	require.True(t, ok)

	rec := upstream.lastRecord(t)
	require.Equal(t, "/v1beta/models/"+scope+":streamGenerateContent", rec.Path,
		"scope 不得被二次映射成 WRONG-DOUBLE-MAPPED")
}

// 同上，Grok 模型级 scope 请求 body.model 必须等于写入的 scope，不二次映射。
func TestModelScopeGrokProbeDoesNotRemapScope(t *testing.T) {
	const scope = "grok-upstream-x"
	acc := grokAPIKeyRecoveryProbeAccount(418)
	acc.Credentials["model_mapping"] = map[string]any{
		"grok-x": scope,
		scope:    "WRONG-DOUBLE-MAPPED",
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"id":"resp-1","object":"response"}`)
	p := anthropicProbeService(upstream)

	ok, err := p.probeModelScopeUpstream(context.Background(), acc, scope)
	require.NoError(t, err)
	require.True(t, ok)

	rec := upstream.lastRecord(t)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Equal(t, scope, body["model"], "scope 不得被 GetMappedModel 二次映射")
}

// ---------- Anthropic 探针协议族误分派修复：Bedrock / Vertex ServiceAccount 排除 ----------

// anthropicBedrockProbeAccount 构造 PlatformAnthropic + Bedrock 账号（真实出站走
// forwardBedrock 的 SigV4 专用链路，不属于普通 /v1/messages 探测的能力面）。
func anthropicBedrockProbeAccount(id int64) *Account {
	acc := anthropicRecoveryProbeAccount(id, true)
	acc.Type = AccountTypeBedrock
	return acc
}

// anthropicServiceAccountProbeAccount 构造 PlatformAnthropic + ServiceAccount（Vertex）账号，
// 真实出站走 buildUpstreamRequestAnthropicVertex（服务账号 OAuth2）。
func anthropicServiceAccountProbeAccount(id int64) *Account {
	acc := anthropicRecoveryProbeAccount(id, true)
	acc.Type = AccountTypeServiceAccount
	return acc
}

// 协议族矩阵：普通 Anthropic APIKey/Upstream 与显式 api_protocol=anthropic 仍为 anthropic 族；
// Bedrock / ServiceAccount 必须归空族，渠道直连与恢复探针共用同一判定。
func TestAnthropicProbeProtocolFamilyExcludesBedrockAndServiceAccount(t *testing.T) {
	upstreamAcc := func() *Account {
		// Upstream 型 GetBaseURL() 恒为空，base 只经 GetAnthropicProtocolBaseURL 取得，
		// 而该 getter 要求 IsAnthropicProtocol() 为真（多协议平台显式 api_protocol=anthropic）。
		// 平台原生 anthropic 的 GetAPIProtocol 恒回落 chat_completions，不是本用例的形态。
		acc := multiProtocolAnthropicAccount(2)
		acc.Type = AccountTypeUpstream
		return acc
	}
	cases := []struct {
		name      string
		acc       *Account
		family    string
		canDirect bool
	}{
		{"anthropic apikey", anthropicRecoveryProbeAccount(1, true), "anthropic", true},
		{"anthropic upstream", upstreamAcc(), "anthropic", true},
		{"explicit anthropic apikey", multiProtocolAnthropicAccount(3), "anthropic", true},
		{"bedrock", anthropicBedrockProbeAccount(4), "", false},
		{"service account", anthropicServiceAccountProbeAccount(5), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.family, probeProtocolFamily(tc.acc))
			require.Equal(t, tc.canDirect, canDirectProbe(tc.acc))

			spec, err := buildProbeRequestSpec(tc.acc, "claude-x")
			recoverSpec, family, recoverErr := buildRecoveryProbeRequestSpec(tc.acc, "claude-upstream-x", false)
			if tc.canDirect {
				require.NoError(t, err)
				require.NotNil(t, spec)
				require.Contains(t, spec.url, "/v1/messages")
				require.NoError(t, recoverErr)
				require.Equal(t, "anthropic", family)
				require.NotNil(t, recoverSpec)
				return
			}
			require.Error(t, err, "被排除类型不得构造普通直连请求")
			require.Nil(t, spec)
			require.Error(t, recoverErr, "被排除类型恢复探针必须失败关闭")
			require.Contains(t, recoverErr.Error(), "unsupported")
			require.Empty(t, family)
			require.Nil(t, recoverSpec)
		})
	}
}

// 构造器设防：即便绕过 probeProtocolFamily，也不得为 Bedrock / ServiceAccount 拼出
// 普通 /v1/messages；错误必须带 platform/type 便于定位。
func TestAnthropicMessagesSpecRejectsBedrockAndServiceAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		acc  *Account
	}{
		{"bedrock", anthropicBedrockProbeAccount(6)},
		{"service account", anthropicServiceAccountProbeAccount(7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := buildAnthropicMessagesSpec(tc.acc, "claude-x")
			require.Error(t, err)
			require.Nil(t, spec)
			require.Contains(t, err.Error(), "unsupported")
			require.Contains(t, err.Error(), tc.acc.Platform)
			require.Contains(t, err.Error(), string(tc.acc.Type))

			spec, err = buildAnthropicMessagesSpecWithModel(tc.acc, "claude-upstream-x")
			require.Error(t, err)
			require.Nil(t, spec)
			require.Contains(t, err.Error(), "unsupported")
			require.Contains(t, err.Error(), string(tc.acc.Type))
		})
	}
}

// 假上游断言：被排除类型经 probeUpstream 不发出任何请求（记录数为 0）。
func TestAnthropicRecoveryProbeExcludedTypesSendNoRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		acc  *Account
	}{
		{"bedrock", anthropicBedrockProbeAccount(8)},
		{"service account", anthropicServiceAccountProbeAccount(9)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, anthropicTextSSE)
			p := anthropicProbeService(upstream)
			ok, err := p.probeUpstream(context.Background(), tc.acc)
			require.Error(t, err, "被排除类型必须失败关闭（unsupported）")
			require.False(t, ok)
			require.Zero(t, anthropicProbeRecordCount(upstream), "不得向假上游发出任何请求")
		})
	}
}

// anthropicProtoRecorderRepo 是恢复探针窄接口的测试替身（仅本文件使用）。本文件无 build tag，
// 故不嵌入 //go:build unit 文件里的 oauthProbeFakeRepo——那样会让默认构建编译失败。
// 只记录清除/重排调用，不连真实 DB，不引入生产桩或新恢复机制。
type anthropicProtoRecorderRepo struct {
	accounts        map[int64]*Account
	clearCalls      int
	rescheduleCalls int
	lastComponent   string
}

func (r *anthropicProtoRecorderRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if acc, ok := r.accounts[id]; ok {
		return acc, nil
	}
	return nil, errors.New("account not found")
}

func (r *anthropicProtoRecorderRepo) ListTempUnschedulableAccounts(context.Context, time.Time, int) ([]*Account, error) {
	return nil, nil
}

func (r *anthropicProtoRecorderRepo) ClearTempUnschedulableIfGeneration(context.Context, int64, time.Time) (bool, error) {
	return false, nil
}

func (r *anthropicProtoRecorderRepo) BumpTempUnschedulableReasonIfGeneration(context.Context, int64, time.Time, string) (bool, error) {
	return false, nil
}

func (r *anthropicProtoRecorderRepo) ListProbeDueAccounts(context.Context, time.Time, int) ([]*Account, error) {
	return nil, nil
}

func (r *anthropicProtoRecorderRepo) ClearRecoveryProbeComponent(context.Context, int64, string, int64) (bool, error) {
	r.clearCalls++
	return false, nil
}

func (r *anthropicProtoRecorderRepo) RescheduleRecoveryProbeComponent(_ context.Context, _ int64, component string, _ int64, _ time.Time) (bool, error) {
	r.rescheduleCalls++
	r.lastComponent = component
	return true, nil
}

// 恢复失败路径不清除停调：构造处于停调窗口的目标账号，走既有 runTransportComponent，
// 断言 clear=0、reschedule=1（既有机制，不改状态机）。
func TestAnthropicRecoveryProbeExcludedTypeReschedulesWithoutClear(t *testing.T) {
	const gen int64 = 11
	for _, tc := range []struct {
		name string
		acc  *Account
	}{
		{"bedrock", anthropicBedrockProbeAccount(10)},
		{"service account", anthropicServiceAccountProbeAccount(11)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc := tc.acc
			until := time.Now().Add(5 * time.Minute)
			acc.TempUnschedulableUntil = &until
			upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, anthropicTextSSE)
			repo := &anthropicProtoRecorderRepo{accounts: map[int64]*Account{acc.ID: acc}}
			p := &AccountHealthRecoveryProbeService{cfg: &config.Config{}, httpUpstream: upstream, accountRepo: repo}

			p.runTransportComponent(context.Background(), acc, gen, recoveryModeTransient)

			require.Zero(t, repo.clearCalls, "失败关闭时不得清除停调")
			require.Equal(t, 1, repo.rescheduleCalls, "失败关闭必须走既有重排机制")
			require.Equal(t, recoveryProbeComponentTransport, repo.lastComponent)
			require.Zero(t, anthropicProbeRecordCount(upstream), "不得发出普通 /v1/messages 请求")
		})
	}
}

// ---------- Anthropic OAuth / SetupToken 按 unsupported 收口（非普通直连探测族） ----------

// anthropicOAuthProbeAccount 构造 PlatformAnthropic + OAuth 账号。credentials 必须带
// access_token：若未收口，构造器会走裸 Bearer 分支成功构造，测试才能坐实收口生效，
// 而不是因为缺凭据碰巧失败。
func anthropicOAuthProbeAccount(id int64) *Account {
	acc := anthropicRecoveryProbeAccount(id, true)
	acc.Type = AccountTypeOAuth
	acc.Credentials["access_token"] = "fake-anthropic-oauth-access-token"
	return acc
}

// anthropicSetupTokenProbeAccount 构造 PlatformAnthropic + SetupToken 账号（同样带
// access_token，确保测的是协议族收口而非凭据缺失）。
func anthropicSetupTokenProbeAccount(id int64) *Account {
	acc := anthropicRecoveryProbeAccount(id, true)
	acc.Type = AccountTypeSetupToken
	acc.Credentials["access_token"] = "fake-anthropic-setup-access-token"
	return acc
}

// 协议族矩阵扩展：Anthropic OAuth / SetupToken 归空族、canDirectProbe=false；
// 渠道直连构造器与恢复探针构造器都失败关闭，错误含 unsupported。
func TestAnthropicProbeProtocolFamilyExcludesOAuthAndSetupToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		acc  *Account
	}{
		{"anthropic oauth", anthropicOAuthProbeAccount(20)},
		{"anthropic setup token", anthropicSetupTokenProbeAccount(21)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Empty(t, probeProtocolFamily(tc.acc))
			require.False(t, canDirectProbe(tc.acc))

			spec, err := buildProbeRequestSpec(tc.acc, "claude-x")
			require.Error(t, err, "收口类型不得构造普通直连请求")
			require.Nil(t, spec)

			recoverSpec, family, recoverErr := buildRecoveryProbeRequestSpec(tc.acc, "claude-upstream-x", false)
			require.Error(t, recoverErr, "恢复探针必须失败关闭")
			require.Contains(t, recoverErr.Error(), "unsupported")
			require.Empty(t, family)
			require.Nil(t, recoverSpec)
		})
	}
}

// 构造器设防：绕过 probeProtocolFamily 直接调用，OAuth / SetupToken 也必须被拦下，
// 错误含 unsupported + platform/type 便于定位。
func TestAnthropicMessagesSpecRejectsOAuthAndSetupToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		acc  *Account
	}{
		{"anthropic oauth", anthropicOAuthProbeAccount(22)},
		{"anthropic setup token", anthropicSetupTokenProbeAccount(23)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := buildAnthropicMessagesSpec(tc.acc, "claude-x")
			require.Error(t, err)
			require.Nil(t, spec)
			require.Contains(t, err.Error(), "unsupported")
			require.Contains(t, err.Error(), tc.acc.Platform)
			require.Contains(t, err.Error(), string(tc.acc.Type))

			spec, err = buildAnthropicMessagesSpecWithModel(tc.acc, "claude-upstream-x")
			require.Error(t, err)
			require.Nil(t, spec)
			require.Contains(t, err.Error(), "unsupported")
			require.Contains(t, err.Error(), string(tc.acc.Type))
		})
	}
}

// 假上游断言：OAuth / SetupToken 经 probeUpstream 不发出任何请求（记录数为 0）。
func TestAnthropicRecoveryProbeExcludedOAuthTypesSendNoRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		acc  *Account
	}{
		{"anthropic oauth", anthropicOAuthProbeAccount(24)},
		{"anthropic setup token", anthropicSetupTokenProbeAccount(25)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, anthropicTextSSE)
			p := anthropicProbeService(upstream)
			ok, err := p.probeUpstream(context.Background(), tc.acc)
			require.Error(t, err, "收口类型必须失败关闭（unsupported）")
			require.False(t, ok)
			require.Zero(t, anthropicProbeRecordCount(upstream), "不得向假上游发出任何裸请求")
		})
	}
}

// 恢复失败不清除停调：复用 recorder stub 走 runTransportComponent，
// 断言 clear=0、reschedule=1（既有状态机，不改停调时长/清除条件）。
func TestAnthropicRecoveryProbeExcludedOAuthTypeReschedulesWithoutClear(t *testing.T) {
	const gen int64 = 12
	for _, tc := range []struct {
		name string
		acc  *Account
	}{
		{"anthropic oauth", anthropicOAuthProbeAccount(26)},
		{"anthropic setup token", anthropicSetupTokenProbeAccount(27)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc := tc.acc
			until := time.Now().Add(5 * time.Minute)
			acc.TempUnschedulableUntil = &until
			upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, anthropicTextSSE)
			repo := &anthropicProtoRecorderRepo{accounts: map[int64]*Account{acc.ID: acc}}
			p := &AccountHealthRecoveryProbeService{cfg: &config.Config{}, httpUpstream: upstream, accountRepo: repo}

			p.runTransportComponent(context.Background(), acc, gen, recoveryModeTransient)

			require.Zero(t, repo.clearCalls, "失败关闭时不得清除停调")
			require.Equal(t, 1, repo.rescheduleCalls, "失败关闭必须走既有重排机制")
			require.Equal(t, recoveryProbeComponentTransport, repo.lastComponent)
			require.Zero(t, anthropicProbeRecordCount(upstream), "不得发出裸 Bearer 请求")
		})
	}
}
