package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 本文件覆盖方案 §7.5 #1 掩码身份泄漏探针 job 的三入口断言与告警/恢复闭环。
// HTTP 侧用 httptest.Server 模拟本站网关响应，探针 base URL 经 SetProbeBaseURL 注入。

// ---------- fakes ----------

type fakeMaskedProbeSettings struct {
	enabled  bool
	apiKeyID int64
	interval int
}

func (f *fakeMaskedProbeSettings) GetMaskedIdentityProbeEnabled(context.Context) bool {
	return f.enabled
}

func (f *fakeMaskedProbeSettings) GetMaskedIdentityProbeAPIKeyID(context.Context) int64 {
	return f.apiKeyID
}

func (f *fakeMaskedProbeSettings) GetMaskedIdentityProbeIntervalMinutes(context.Context) int {
	if f.interval <= 0 {
		return defaultMaskedIdentityProbeIntervalMinutes
	}
	return f.interval
}

// maskedProbeDimsMatch 判定告警维度是否包含期望维度（模拟仓储 DimensionExact 语义）。
func maskedProbeDimsMatch(have, want map[string]any) bool {
	for k, wv := range want {
		hv, ok := have[k]
		if !ok {
			return false
		}
		if hs, ok := hv.(string); ok {
			if ws, ok := wv.(string); ok {
				if hs != ws {
					return false
				}
				continue
			}
		}
		if hs, ok := hv.(int64); ok {
			if ws, ok := wv.(int64); ok {
				if hs != ws {
					return false
				}
				continue
			}
		}
	}
	return true
}

type fakeMaskedProbeAlertStore struct {
	mu      sync.Mutex
	events  []*OpsAlertEvent
	nextID  int64
	created int
}

func (f *fakeMaskedProbeAlertStore) CreateAlertEvent(_ context.Context, e *OpsAlertEvent) (*OpsAlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *e
	cp.ID = f.nextID
	if cp.FiredAt.IsZero() {
		cp.FiredAt = time.Now()
	}
	f.events = append(f.events, &cp)
	f.created++
	return &cp, nil
}

func (f *fakeMaskedProbeAlertStore) GetActiveFreshnessAlert(_ context.Context, dims map[string]any) (*OpsAlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.events {
		if ev.Status == OpsAlertStatusFiring && maskedProbeDimsMatch(ev.Dimensions, dims) {
			return ev, nil
		}
	}
	return nil, nil
}

func (f *fakeMaskedProbeAlertStore) UpdateAlertEventStatus(_ context.Context, eventID int64, status string, resolvedAt *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.events {
		if ev.ID == eventID {
			ev.Status = status
			ev.ResolvedAt = resolvedAt
			return nil
		}
	}
	return nil
}

func (f *fakeMaskedProbeAlertStore) firingWithKind(kind string) []*OpsAlertEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*OpsAlertEvent, 0, 2)
	for _, ev := range f.events {
		if ev.Status == OpsAlertStatusFiring {
			if k, _ := ev.Dimensions[maskedProbeDimKind].(string); k == kind {
				out = append(out, ev)
			}
		}
	}
	return out
}

func (f *fakeMaskedProbeAlertStore) withKind(kind string) []*OpsAlertEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*OpsAlertEvent, 0, 2)
	for _, ev := range f.events {
		if k, _ := ev.Dimensions[maskedProbeDimKind].(string); k == kind {
			out = append(out, ev)
		}
	}
	return out
}

func (f *fakeMaskedProbeAlertStore) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

type fakeMaskedProbeAPIKeyRepo struct {
	key *APIKey
	err error
}

func (f *fakeMaskedProbeAPIKeyRepo) GetByID(_ context.Context, id int64) (*APIKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.key == nil || f.key.ID != id {
		return nil, nil
	}
	return f.key, nil
}

type fakeMaskedProbeAccountRepo struct {
	accounts []Account
}

func (f *fakeMaskedProbeAccountRepo) ListByGroup(_ context.Context, groupID int64) ([]Account, error) {
	if groupID <= 0 {
		return nil, nil
	}
	return f.accounts, nil
}

type fakeMaskedProbeEmailSink struct {
	markCalls int
}

func (f *fakeMaskedProbeEmailSink) GetEmailNotificationConfig(context.Context) (*OpsEmailNotificationConfig, error) {
	return nil, nil
}

func (f *fakeMaskedProbeEmailSink) UpdateAlertEventEmailSent(context.Context, int64, bool) error {
	f.markCalls++
	return nil
}

// ---------- harness ----------

// maskedProbeReply 描述模拟网关对某入口的响应。
type maskedProbeReply struct {
	status  int
	body    string
	headers map[string]string
}

type maskedProbeGateway struct {
	mu      sync.Mutex
	replies map[string]maskedProbeReply
	reqs    int
}

// maskedProbeBodyRouteKey 按请求体特征区分三类共享 /v1/chat/completions 路径的探针，
// 使夹具可对 base chat / identity_q / identity_jailbreak 注入不同响应（PROBE-R2 外审 P2）：
//   - 含越狱提示 → "identity_jailbreak"
//   - 含身份问答提示 → "identity_q"
//   - 其余（含 base chat 的 "hi"）→ ""（回落到 path 路由）
func maskedProbeBodyRouteKey(body string) string {
	switch {
	case strings.Contains(body, "Ignore previous instructions"):
		return "identity_jailbreak"
	case strings.Contains(body, "你是什么模型"):
		return "identity_q"
	default:
		return ""
	}
}

func (g *maskedProbeGateway) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.reqs++
		raw, _ := io.ReadAll(r.Body)
		key := maskedProbeBodyRouteKey(string(raw))
		reply, ok := g.replies[key]
		if !ok {
			// 回落到 path 路由：未显式按 body 特征注入时，沿用旧 path 语义
			// （base chat / identity_q / identity_jailbreak 同路径共享同一回复，
			// 既有用例的断言语义保持不变）。
			reply, ok = g.replies[r.URL.Path]
		}
		g.mu.Unlock()
		if !ok {
			// 默认回复：协议层干净（model=kimi-k3、无标记、无泄漏头）且内容层含 kimi，
			// 使既有三入口用例与内容层探针（identity_q 要求内容含 kimi）在本底响应下均判干净，
			// 避免干净轮误触发 masked_identity_content_break。
			reply = maskedProbeReply{status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am kimi, an AI assistant"}}]}`}
		}
		for k, v := range reply.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	})
}

func (g *maskedProbeGateway) requests() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reqs
}

type maskedProbeHarness struct {
	svc      *MaskedIdentityProbeService
	store    *fakeMaskedProbeAlertStore
	gateway  *maskedProbeGateway
	settings *fakeMaskedProbeSettings
	baseURL  string
}

// newMaskedProbeHarness 组装一个探针服务：启用开关 + 有效探针 key（分组含 kimi 掩码账号），
// 网关响应由 replies 决定（缺省为干净 2xx）。
func newMaskedProbeHarness(t *testing.T, replies map[string]maskedProbeReply) *maskedProbeHarness {
	t.Helper()

	gw := &maskedProbeGateway{replies: replies}
	srv := httptest.NewServer(gw.handler())
	t.Cleanup(srv.Close)

	groupID := int64(77)
	store := &fakeMaskedProbeAlertStore{}
	keyRepo := &fakeMaskedProbeAPIKeyRepo{key: &APIKey{
		ID:      4242,
		Key:     "sk-probe-masked-identity",
		Status:  StatusActive,
		GroupID: &groupID,
	}}
	accRepo := &fakeMaskedProbeAccountRepo{accounts: []Account{
		{ID: 901, Platform: PlatformKimi, Status: StatusActive, Extra: map[string]any{"mask_upstream_identity": true}},
	}}
	settings := &fakeMaskedProbeSettings{enabled: true, apiKeyID: 4242, interval: 5}

	svc := &MaskedIdentityProbeService{
		settings:    settings,
		apiKeyRepo:  keyRepo,
		accountRepo: accRepo,
		alerts:      store,
		emailOps:    &fakeMaskedProbeEmailSink{},
		httpClient:  &http.Client{Timeout: 5 * time.Second},
		nowFunc:     func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) },
	}
	svc.SetProbeBaseURL(srv.URL)
	return &maskedProbeHarness{svc: svc, store: store, gateway: gw, settings: settings, baseURL: srv.URL}
}

// ---------- 用例 ----------

func TestMaskedIdentityProbe_BodyLeakFiresCriticalEventWithEntryDimension(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"served by Qwen upstream"}}]}`},
	})

	h.svc.RunOnce(context.Background())

	// 三入口 + 2 个内容层探针（identity_q / identity_jailbreak）均走 /v1/chat/completions，
	// 与既有三入口共享该路径的 qwen 回复：base chat 与 identity_q 同维度（chat_completions）
	// 去重为 1 个泄漏事件，identity_jailbreak 为独立维度事件，共 2 个泄漏事件。
	require.Equal(t, 5, h.gateway.requests(), "三入口 + 2 内容层探针都应发出真实请求")
	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	require.Len(t, leaks, 2, "chat_completions 与 identity_jailbreak 各自产出泄漏事件")
	require.Equal(t, maskedIdentityProbeSeverityCritical, leaks[0].Severity)
	require.Equal(t, maskedIdentityProbeEntryChat, leaks[0].Dimensions[maskedProbeDimEntry])
	require.Equal(t, PlatformKimi, leaks[0].Dimensions[maskedProbeDimPlatform])
	require.Contains(t, strings.ToLower(leaks[0].Description), "qwen")

	// 同一维度持续泄漏不重复轰炸：第二轮不再创建新事件（2 个泄漏事件均保持 1 活跃）。
	h.svc.RunOnce(context.Background())
	require.Len(t, h.store.withKind(maskedProbeKindLeak), 2)
	require.Equal(t, 2, h.store.createdCount())
}

func TestMaskedIdentityProbe_HeaderLeakFiresCriticalEventWithEntryDimension(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/responses": {status: http.StatusOK, body: `{"response":{"model":"kimi-k3"}}`, headers: map[string]string{"X-Th-Plan": "pro"}},
		"/v1/messages":  {status: http.StatusOK, body: `{"model":"kimi-k3","content":[{"type":"text","text":"hi"}]}`, headers: map[string]string{"Server": "Vercel"}},
	})

	h.svc.RunOnce(context.Background())

	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	require.Len(t, leaks, 2, "两个泄漏入口各自独立报告")
	entries := map[string]bool{}
	for _, ev := range leaks {
		entries[ev.Dimensions[maskedProbeDimEntry].(string)] = true
		require.Equal(t, maskedIdentityProbeSeverityCritical, ev.Severity)
	}
	require.True(t, entries[maskedIdentityProbeEntryResponses])
	require.True(t, entries[maskedIdentityProbeEntryMessages])
	require.Empty(t, h.store.withKind(maskedProbeKindProbeFailed))
}

func TestMaskedIdentityProbe_ModelMismatchFiresCriticalEvent(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k2.5","choices":[{"message":{"content":"ok"}}]}`},
	})

	h.svc.RunOnce(context.Background())

	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	// chat_completions（base + identity_q 同维度去重）与 identity_jailbreak 各 1 个泄漏事件。
	require.Len(t, leaks, 2)
	require.Equal(t, maskedIdentityProbeEntryChat, leaks[0].Dimensions[maskedProbeDimEntry])
	require.Contains(t, leaks[0].Description, "kimi-k2.5")
}

func TestMaskedIdentityProbe_CleanResponseResolvesActiveEventAndCreatesNone(t *testing.T) {
	h := newMaskedProbeHarness(t, nil)

	// 预置一个 firing 的泄漏告警（模拟上一轮阳性）。
	_, err := h.store.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status:      OpsAlertStatusFiring,
		Severity:    maskedIdentityProbeSeverityCritical,
		Title:       "掩码账号身份泄漏",
		Description: "历史泄漏",
		Dimensions:  maskedIdentityProbeLeakDims(maskedIdentityProbeEntryChat),
		FiredAt:     time.Now(),
	})
	require.NoError(t, err)

	h.svc.RunOnce(context.Background())

	require.Empty(t, h.store.firingWithKind(maskedProbeKindLeak), "全净时活跃泄漏事件应被 resolve")
	resolved := h.store.withKind(maskedProbeKindLeak)
	require.Len(t, resolved, 1)
	require.Equal(t, OpsAlertStatusResolved, resolved[0].Status)
	require.NotNil(t, resolved[0].ResolvedAt)
	require.Equal(t, 1, h.store.createdCount(), "全净不应创建新事件")
	require.Empty(t, h.store.withKind(maskedProbeKindConfigMissing))
	require.Empty(t, h.store.withKind(maskedProbeKindProbeFailed))
}

func TestMaskedIdentityProbe_ConfigMissingFiresWarningWithoutHTTPAndResolvesOnRecovery(t *testing.T) {
	h := newMaskedProbeHarness(t, nil)
	h.settings.apiKeyID = 0 // 未配置探针 key

	h.svc.RunOnce(context.Background())

	require.Equal(t, 0, h.gateway.requests(), "配置缺失时不发任何 HTTP 请求")
	warnings := h.store.firingWithKind(maskedProbeKindConfigMissing)
	require.Len(t, warnings, 1)
	require.Equal(t, maskedIdentityProbeSeverityWarning, warnings[0].Severity)

	// 配置恢复后自动 resolve，并开始真实探测（不发新告警）。
	h.settings.apiKeyID = 4242
	h.svc.RunOnce(context.Background())
	require.NotEmpty(t, h.gateway.requests(), "配置恢复后应发出请求")
	require.Empty(t, h.store.firingWithKind(maskedProbeKindConfigMissing), "配置恢复后配置缺失告警应关闭")
}

func TestMaskedIdentityProbe_ConsecutiveFailuresFireWarningAndResolveOnSuccess(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusInternalServerError, body: `{"error":"upstream down"}`},
		"/v1/responses":        {status: http.StatusInternalServerError, body: `{"error":"upstream down"}`},
		"/v1/messages":         {status: http.StatusInternalServerError, body: `{"error":"upstream down"}`},
	})

	h.svc.RunOnce(context.Background())
	h.svc.RunOnce(context.Background())
	require.Empty(t, h.store.withKind(maskedProbeKindProbeFailed), "未达 3 轮不产出探针失败告警")
	require.Empty(t, h.store.withKind(maskedProbeKindLeak), "非 2xx 是探针失败而不是泄漏")

	h.svc.RunOnce(context.Background())
	failed := h.store.firingWithKind(maskedProbeKindProbeFailed)
	require.Len(t, failed, 1)
	require.Equal(t, maskedIdentityProbeSeverityWarning, failed[0].Severity)

	// 恢复：任一入口 2xx 即清零连续失败并自动 resolve。
	h.gateway.mu.Lock()
	h.gateway.replies = nil
	h.gateway.mu.Unlock()
	h.svc.RunOnce(context.Background())
	require.Empty(t, h.store.firingWithKind(maskedProbeKindProbeFailed))
	require.Len(t, h.store.withKind(maskedProbeKindProbeFailed), 1, "不重复创建同类事件")
}

func TestMaskedIdentityProbe_DisabledIsNoop(t *testing.T) {
	h := newMaskedProbeHarness(t, nil)
	h.settings.enabled = false

	h.svc.RunOnce(context.Background())

	require.Equal(t, 0, h.gateway.requests())
	require.Equal(t, 0, h.store.createdCount())
}

func TestMaskedIdentityProbe_NonMaskedGroupConfigFiresWarningWithoutHTTP(t *testing.T) {
	h := newMaskedProbeHarness(t, nil)
	groupID := int64(77)
	h.svc.accountRepo = &fakeMaskedProbeAccountRepo{accounts: []Account{
		{ID: 902, Platform: PlatformKimi, Status: StatusActive}, // kimi 但非掩码账号
	}}
	h.svc.apiKeyRepo = &fakeMaskedProbeAPIKeyRepo{key: &APIKey{ID: 4242, Key: "sk-probe", Status: StatusActive, GroupID: &groupID}}

	h.svc.RunOnce(context.Background())

	require.Equal(t, 0, h.gateway.requests(), "非掩码配置不发请求")
	require.Len(t, h.store.firingWithKind(maskedProbeKindConfigMissing), 1)
	require.Empty(t, h.store.withKind(maskedProbeKindLeak))
}

// --- 外审整改（QK-OBS-R2）新增用例 ---

func TestMaskedIdentityProbe_LargeBodyMarkerAtTailIsDetected(t *testing.T) {
	// 全量读取 + 全量扫描：body 远超原 256KiB 截断窗口，marker 只出现在末尾。
	padding := strings.Repeat("a", 300<<10)
	body := `{"model":"kimi-k3","choices":[{"message":{"content":"` + padding + ` served by QWEN upstream"}}]}`
	require.Greater(t, len(body), 256<<10, "构造体必须越过硬编码截断窗口才有证明力")

	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: body},
	})

	h.svc.RunOnce(context.Background())

	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	// chat_completions（base + identity_q 同维度去重）与 identity_jailbreak 各 1 个泄漏事件。
	require.Len(t, leaks, 2, "末尾 marker 必须被全量扫描检出")
	require.Equal(t, maskedIdentityProbeEntryChat, leaks[0].Dimensions[maskedProbeDimEntry])
	require.Equal(t, maskedIdentityProbeSeverityCritical, leaks[0].Severity)
	require.Contains(t, strings.ToLower(leaks[0].Description), "qwen")
}

func TestMaskedIdentityProbe_ResponsesNestedModelLeakIsDetectedDespiteTopLevelModel(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		// 顶层 model 正确，response.model 泄漏：顶层不得掩盖嵌套字段。
		"/v1/responses": {status: http.StatusOK, body: `{"model":"kimi-k3","response":{"model":"qwen3.8-flash"}}`},
	})

	h.svc.RunOnce(context.Background())

	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	require.Len(t, leaks, 1)
	require.Equal(t, maskedIdentityProbeEntryResponses, leaks[0].Dimensions[maskedProbeDimEntry])

	// 直接断言 model 全位判定本身（换成不含 body marker 的泄漏值，排除 marker 命中干扰）：
	// 顶层正确 + response.model 泄漏必须判阳性，且 reason 带上字段路径与实际值。
	finding := assertMaskedIdentityProbeResponse(maskedIdentityProbeEntryResponses, http.Header{},
		[]byte(`{"model":"kimi-k3","response":{"model":"glm-4.6"}}`))
	require.Equal(t, maskedProbeOutcomeLeak, finding.outcome)
	require.Contains(t, finding.reason, "response.model")
	require.Contains(t, finding.reason, "glm-4.6")
}

func TestMaskedIdentityProbe_MessagesNestedModelLeakIsDetectedDespiteTopLevelModel(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		// 顶层 model 正确，message.model 泄漏。
		"/v1/messages": {status: http.StatusOK, body: `{"model":"kimi-k3","message":{"model":"qwen3.8-flash"}}`},
	})

	h.svc.RunOnce(context.Background())

	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	require.Len(t, leaks, 1)
	require.Equal(t, maskedIdentityProbeEntryMessages, leaks[0].Dimensions[maskedProbeDimEntry])

	finding := assertMaskedIdentityProbeResponse(maskedIdentityProbeEntryMessages, http.Header{},
		[]byte(`{"model":"kimi-k3","message":{"model":"glm-4.6"}}`))
	require.Equal(t, maskedProbeOutcomeLeak, finding.outcome)
	require.Contains(t, finding.reason, "message.model")
	require.Contains(t, finding.reason, "glm-4.6")
}

func TestMaskedIdentityProbe_BaseURLUnavailableFiresWarningAndResolvesOnRecovery(t *testing.T) {
	h := newMaskedProbeHarness(t, nil)
	h.svc.SetProbeBaseURL("") // 注入地址与 cfg 监听地址都不可用

	h.svc.RunOnce(context.Background())

	require.Equal(t, 0, h.gateway.requests(), "base URL 不可用时不应发请求")
	warnings := h.store.firingWithKind(maskedProbeKindBaseURLUnavailable)
	require.Len(t, warnings, 1)
	require.Equal(t, maskedIdentityProbeSeverityWarning, warnings[0].Severity)
	require.Equal(t, maskedProbeKindBaseURLUnavailable, warnings[0].Dimensions[maskedProbeDimKind])

	// 持续不可用不重复轰炸。
	h.svc.RunOnce(context.Background())
	require.Len(t, h.store.withKind(maskedProbeKindBaseURLUnavailable), 1)

	// 恢复（base URL 非空）的下一轮自动 resolve，且与 config_missing 互不替代。
	h.svc.SetProbeBaseURL(h.baseURL)
	h.svc.RunOnce(context.Background())
	require.NotEmpty(t, h.gateway.requests(), "恢复后应发出请求")
	resolved := h.store.withKind(maskedProbeKindBaseURLUnavailable)
	require.Len(t, resolved, 1, "不重复创建同类事件")
	require.Equal(t, OpsAlertStatusResolved, resolved[0].Status)
	require.NotNil(t, resolved[0].ResolvedAt)
	require.Empty(t, h.store.firingWithKind(maskedProbeKindBaseURLUnavailable))
}

func TestMaskedIdentityProbe_ConfigMissingResetsFailureStreak(t *testing.T) {
	down := map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusInternalServerError, body: `{"error":"upstream down"}`},
		"/v1/responses":        {status: http.StatusInternalServerError, body: `{"error":"upstream down"}`},
		"/v1/messages":         {status: http.StatusInternalServerError, body: `{"error":"upstream down"}`},
	}
	h := newMaskedProbeHarness(t, down)

	// 先 2 轮全入口失败（streak=2，未达阈值 3）。
	h.svc.RunOnce(context.Background())
	h.svc.RunOnce(context.Background())
	require.Empty(t, h.store.withKind(maskedProbeKindProbeFailed))

	// 1 轮 config-missing：不探测路径把 streak 归零。
	h.settings.apiKeyID = 0
	h.svc.RunOnce(context.Background())
	require.Len(t, h.store.firingWithKind(maskedProbeKindConfigMissing), 1)

	// 恢复后再失败 1 轮：streak=1 < 3，不得触发连续失败告警。
	h.settings.apiKeyID = 4242
	h.svc.RunOnce(context.Background())
	require.Empty(t, h.store.withKind(maskedProbeKindProbeFailed), "streak 已归零，总数 1 不应触发连续失败告警")
	require.Empty(t, h.store.firingWithKind(maskedProbeKindConfigMissing), "配置恢复后配置缺失告警应关闭")
}

func TestMaskedIdentityProbe_StopWithoutStartIsSafe(t *testing.T) {
	var nilSvc *MaskedIdentityProbeService
	nilSvc.Stop() // nil 接收者守卫

	svc := &MaskedIdentityProbeService{}
	svc.Stop() // 未 Start 就 Stop：stopCh/runCancel 均为 nil，不炸不死锁
	svc.Stop()
}

func TestMaskedIdentityProbe_StartStopLifecycle(t *testing.T) {
	h := newMaskedProbeHarness(t, nil)
	h.settings.enabled = false

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.svc.Start(ctx)
	h.svc.Start(ctx) // 重复 Start 不应重复起 goroutine
	h.svc.Stop()
	h.svc.Stop() // 重复 Stop 幂等
	require.Equal(t, 0, h.gateway.requests())
}

// --- 外审整改第 2 轮（QK-OBS-R3）新增用例 ---

func TestMaskedIdentityProbe_BodyOverHardLimitFailsClosed(t *testing.T) {
	// 构造越过硬上限（8MiB）的 2xx 响应：超限必须「拒绝判定」（failed），
	// 既不能因截断后扫前缀判 clean，也不能判 leak。
	padding := strings.Repeat("a", 9<<20)
	body := `{"model":"kimi-k3","choices":[{"message":{"content":"` + padding + `"}}]}`
	require.Greater(t, len(body), maskedIdentityProbeMaxBodyBytes, "构造体必须越过硬上限才有证明力")

	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: body},
		"/v1/responses":        {status: http.StatusOK, body: body},
		"/v1/messages":         {status: http.StatusOK, body: body},
	})

	entry := maskedIdentityProbeEntries()[0]
	result := h.svc.probeEntry(context.Background(), h.baseURL, "sk-probe-masked-identity", entry)
	require.Equal(t, maskedProbeOutcomeFailed, result.outcome, "超限响应失败关闭：不判泄漏也不判干净")
	require.Contains(t, result.reason, "exceeds limit")

	// 超限走的是失败链：既无泄漏事件，连续 3 轮后产出探针失败警告。
	h.svc.RunOnce(context.Background())
	require.Empty(t, h.store.withKind(maskedProbeKindLeak), "超限不得判泄漏")
	h.svc.RunOnce(context.Background())
	h.svc.RunOnce(context.Background())
	failed := h.store.firingWithKind(maskedProbeKindProbeFailed)
	require.Len(t, failed, 1, "连续 3 轮超限应产出探针失败警告")
	require.Equal(t, maskedIdentityProbeSeverityWarning, failed[0].Severity)
	require.Empty(t, h.store.withKind(maskedProbeKindLeak))
}

func TestMaskedIdentityProbe_BodyAtHardLimitIsFullyScanned(t *testing.T) {
	// 边界：恰好等于上限（≤max）必须照旧全量扫描——末尾 marker 仍要检出，
	// 防止"上限"退化成截断放行。
	tail := " served by QWEN upstream\"}]}"
	prefix := `{"model":"kimi-k3","choices":[{"message":{"content":"`
	padding := strings.Repeat("a", maskedIdentityProbeMaxBodyBytes-len(prefix)-len(tail))
	body := prefix + padding + tail
	require.Equal(t, maskedIdentityProbeMaxBodyBytes, len(body))

	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: body},
	})

	h.svc.RunOnce(context.Background())
	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	// chat_completions（base + identity_q 同维度去重）与 identity_jailbreak 各 1 个泄漏事件。
	require.Len(t, leaks, 2, "等于上限的响应必须全量扫描，末尾 marker 不得逃脱")
	require.Equal(t, maskedIdentityProbeEntryChat, leaks[0].Dimensions[maskedProbeDimEntry])
}

func TestMaskedIdentityProbe_NonStringModelFieldFailsClosedAsLeak(t *testing.T) {
	// 顶层 model 为 JSON null（非字符串位不可验证）+ response.model 正确：
	// 非字符串位失败关闭 → 判泄漏（不是跳过放行）。
	finding := assertMaskedIdentityProbeResponse(maskedIdentityProbeEntryResponses, http.Header{},
		[]byte(`{"model":null,"response":{"model":"kimi-k3"}}`))
	require.Equal(t, maskedProbeOutcomeLeak, finding.outcome)
	require.Contains(t, finding.reason, "model", "reason 应提及该 model 位")
	require.Contains(t, finding.reason, maskedProbeNonStringModelValue)

	// 经完整一轮：该入口产出严重级泄漏告警（嵌套位正确也不得掩盖非字符串位）。
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/responses": {status: http.StatusOK, body: `{"model":null,"response":{"model":"kimi-k3"}}`},
	})
	h.svc.RunOnce(context.Background())
	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	require.Len(t, leaks, 1)
	require.Equal(t, maskedIdentityProbeEntryResponses, leaks[0].Dimensions[maskedProbeDimEntry])
	require.Equal(t, maskedIdentityProbeSeverityCritical, leaks[0].Severity)
}

func TestMaskedIdentityProbe_KeyAndBaseURLPrerequisitesAreIndependentlyMaintained(t *testing.T) {
	h := newMaskedProbeHarness(t, nil)
	h.settings.apiKeyID = 0   // key 缺失
	h.svc.SetProbeBaseURL("") // base URL 同时不可用

	h.svc.RunOnce(context.Background())

	require.Equal(t, 0, h.gateway.requests(), "两项前置条件均不满足时不发请求")
	require.Len(t, h.store.firingWithKind(maskedProbeKindConfigMissing), 1, "key 缺失事件独立 firing")
	require.Len(t, h.store.firingWithKind(maskedProbeKindBaseURLUnavailable), 1, "base URL 事件独立 firing，不被 key 缺失埋掉")

	// 仅恢复 base URL（key 仍缺）：base_url 事件 resolve，config_missing 仍 firing。
	h.svc.SetProbeBaseURL(h.baseURL)
	h.svc.RunOnce(context.Background())
	require.Empty(t, h.store.firingWithKind(maskedProbeKindBaseURLUnavailable), "base URL 恢复后其事件应 resolve")
	require.Len(t, h.store.firingWithKind(maskedProbeKindConfigMissing), 1, "key 仍缺，配置缺失事件仍 firing")
	require.Equal(t, 0, h.gateway.requests(), "key 仍缺仍不探测")
}

// --- 外审整改第 3 轮（QK-OBS-R3B）新增用例 ---

func TestMaskedIdentityProbe_BodyReadErrorFailsClosed(t *testing.T) {
	// 构造 body 中途读取失败：声明 Content-Length 远大于实际写出字节数后直接关连接，
	// 客户端读取时得到 unexpected EOF（ReadAll 返回部分数据 + err）。
	partial := `{"model":"kimi-k3","choices":[{"message":{"content":"partial`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr != nil {
			t.Errorf("hijack failed: %v", hijackErr)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n")
		_, _ = buf.WriteString(partial)
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)

	h := newMaskedProbeHarness(t, nil)
	h.svc.SetProbeBaseURL(srv.URL)

	entry := maskedIdentityProbeEntries()[0]
	result := h.svc.probeEntry(context.Background(), srv.URL, "sk-probe-masked-identity", entry)
	require.Equal(t, maskedProbeOutcomeFailed, result.outcome, "读取错误必须失败关闭：不得把部分 body 当完整 body 继续断言")
	require.Contains(t, result.reason, "read response body")

	// 部分 body 不得被判 clean/leak：整轮跑完后无泄漏事件可读（也没算作干净恢复之外
	// 的任何判定），错误闭环只走失败链。
	h.svc.RunOnce(context.Background())
	require.Empty(t, h.store.withKind(maskedProbeKindLeak), "读取错误不得对部分 body 作出泄漏判定")
	h.svc.RunOnce(context.Background())
	h.svc.RunOnce(context.Background())
	failed := h.store.firingWithKind(maskedProbeKindProbeFailed)
	require.Len(t, failed, 1, "连续 3 轮读取错误应经既有失败链产出探针失败警告")
	require.Equal(t, maskedIdentityProbeSeverityWarning, failed[0].Severity)
}

func TestMaskedIdentityProbe_DisabledRoundResetsFailureStreak(t *testing.T) {
	down := map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusInternalServerError, body: `{"error":"upstream down"}`},
		"/v1/responses":        {status: http.StatusInternalServerError, body: `{"error":"upstream down"}`},
		"/v1/messages":         {status: http.StatusInternalServerError, body: `{"error":"upstream down"}`},
	}
	h := newMaskedProbeHarness(t, down)

	// 2 轮全入口失败（streak=2）。
	h.svc.RunOnce(context.Background())
	h.svc.RunOnce(context.Background())
	require.Empty(t, h.store.withKind(maskedProbeKindProbeFailed))

	// 禁用一轮（空转，不发请求）：streak 归零。
	h.settings.enabled = false
	beforeDisabled := h.gateway.requests()
	h.svc.RunOnce(context.Background())
	require.Equal(t, beforeDisabled, h.gateway.requests(), "禁用轮不发请求")

	// 重新启用后失败 1 轮：streak=1 < 3，不触发连续失败告警。
	h.settings.enabled = true
	h.svc.RunOnce(context.Background())
	require.Empty(t, h.store.withKind(maskedProbeKindProbeFailed), "禁用已重置 streak，重新启用后不得沿用禁用前的失败轮数")

	// 计数本身仍在工作：再累计 2 轮达到 3 轮即产出探针失败告警。
	h.svc.RunOnce(context.Background())
	h.svc.RunOnce(context.Background())
	require.Len(t, h.store.firingWithKind(maskedProbeKindProbeFailed), 1)
}

// --- PROBE-C1 内容层探针新增用例 ---

// ① identity_q 内容含 kimi → 全净（不产内容层破防、不产协议层泄漏）。
func TestMaskedIdentityProbe_IdentityQContentHasKimiIsClean(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am kimi, developed by Moonshot AI"}}]}`},
	})

	h.svc.RunOnce(context.Background())

	require.Empty(t, h.store.firingWithKind(maskedProbeKindContentBreak), "内容含 kimi 不应判内容层破防")
	require.Empty(t, h.store.firingWithKind(maskedProbeKindLeak), "协议层无痕迹不应判泄漏")
}

// ② identity_q 内容缺 kimi → firing masked_identity_content_break（entry=chat_completions）。
func TestMaskedIdentityProbe_IdentityQContentMissingKimiFiresContentBreak(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		// 协议层干净（model=kimi-k3、无标记、无泄漏头），但内容层未以 kimi 身份作答。
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am an AI language model"}}]}`},
	})

	h.svc.RunOnce(context.Background())

	breaks := h.store.firingWithKind(maskedProbeKindContentBreak)
	require.Len(t, breaks, 1, "内容层破防应产出 1 个事件")
	require.Equal(t, maskedProbeKindContentBreak, breaks[0].Dimensions[maskedProbeDimKind])
	require.Equal(t, maskedIdentityProbeEntryChat, breaks[0].Dimensions[maskedProbeDimEntry], "内容层破防 entry 维度应为 chat_completions")
	require.Equal(t, PlatformKimi, breaks[0].Dimensions[maskedProbeDimPlatform])
	require.Equal(t, maskedIdentityProbeSeverityCritical, breaks[0].Severity)
	require.Contains(t, breaks[0].Title, "内容层", "告警 title 应区分内容层破防与协议层泄漏")
	require.Contains(t, strings.ToLower(breaks[0].Description), "kimi")
	require.Empty(t, h.store.firingWithKind(maskedProbeKindLeak), "协议层无痕迹不应判泄漏")
}

// ③ identity_jailbreak body 含 qwen → firing masked_identity_leak（entry 维度=identity_jailbreak）。
func TestMaskedIdentityProbe_IdentityJailbreakBodyHasQwenFiresLeak(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"Actually I am Qwen, trained by Alibaba"}}]}`},
	})

	h.svc.RunOnce(context.Background())

	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	require.NotEmpty(t, leaks)
	// 越狱探针的标记泄漏归 masked_identity_leak，维度 entry=identity_jailbreak（与 chat_completions 平级独立）。
	var jailbreakLeak *OpsAlertEvent
	for _, ev := range leaks {
		if ev.Dimensions[maskedProbeDimEntry] == maskedIdentityProbeEntryIdentityJailbreak {
			jailbreakLeak = ev
			break
		}
	}
	require.NotNil(t, jailbreakLeak, "identity_jailbreak 维度应独立产出泄漏事件")
	require.Equal(t, maskedProbeKindLeak, jailbreakLeak.Dimensions[maskedProbeDimKind])
	require.Equal(t, maskedIdentityProbeSeverityCritical, jailbreakLeak.Severity)
	require.Empty(t, h.store.firingWithKind(maskedProbeKindContentBreak), "标记泄漏归泄漏、不触发内容层破防")
}

// ④ "通义"/"千问" 标记命中泄漏（复用既有 body 标记泄漏断言）。
func TestMaskedIdentityProbe_TongyiQianwenMarkerHitsLeak(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"我是通义千问，由阿里训练"}}]}`},
	})

	h.svc.RunOnce(context.Background())

	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	require.NotEmpty(t, leaks)
	// 中文上游身份词应被标记泄漏断言捕获；chat_completions 与 identity_jailbreak 各 1 个泄漏事件。
	require.Len(t, leaks, 2)
	var seenJailbreak, seenTongyi bool
	for _, ev := range leaks {
		if ev.Dimensions[maskedProbeDimEntry] == maskedIdentityProbeEntryIdentityJailbreak {
			seenJailbreak = true
		}
		if strings.Contains(ev.Description, "通义") || strings.Contains(ev.Description, "千问") {
			seenTongyi = true
		}
	}
	require.True(t, seenJailbreak, "identity_jailbreak 维度应产出泄漏事件")
	require.True(t, seenTongyi, "泄漏原因应点名 通义/千问 标记")
}

// 内容层破防在 clean 轮自动 resolve（与协议层泄漏同机制）。
func TestMaskedIdentityProbe_ContentBreakResolvesOnCleanRound(t *testing.T) {
	// 第一轮：内容缺 kimi → 内容层破防 firing。
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am an AI language model"}}]}`},
	})
	h.svc.RunOnce(context.Background())
	require.Len(t, h.store.firingWithKind(maskedProbeKindContentBreak), 1, "首轮应 firing 内容层破防")

	// 第二轮：内容含 kimi → clean，活跃破防事件被 resolve，不重复创建。
	h.gateway.mu.Lock()
	h.gateway.replies = map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am kimi, trained by Moonshot"}}]}`},
	}
	h.gateway.mu.Unlock()
	h.svc.RunOnce(context.Background())

	require.Empty(t, h.store.firingWithKind(maskedProbeKindContentBreak), "clean 轮应 resolve 内容层破防")
	resolved := h.store.withKind(maskedProbeKindContentBreak)
	require.Len(t, resolved, 1, "不重复创建同类事件")
	require.Equal(t, OpsAlertStatusResolved, resolved[0].Status)
	require.NotNil(t, resolved[0].ResolvedAt)
}

// --- PROBE-R2 交错回归（外审 P1 实锤：同 entry 互误 resolve 竞态） ---

// ① 同轮 base chat 泄漏 + identity_q clean → 泄漏事件保持 firing（不误关闭）。
// 验证：base chat 与 identity_q 共享 entry=chat_completions，base chat 阳性时即使
// identity_q 这一轮 clean，也不应立即 resolve 掉该维度的活跃泄漏告警；且不得周期性先关再建。
func TestMaskedIdentityProbe_SameRoundBaseChatLeakIdentityQCleanKeepsLeakFiring(t *testing.T) {
	// base chat（path 回落）泄漏；identity_q（body 特征）内容含 kimi → clean。
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"served by Qwen upstream"}}]}`},
		"identity_q":           {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am kimi, developed by Moonshot"}}]}`},
		"identity_jailbreak":   {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am kimi, an AI assistant"}}]}`},
	})

	// 预置上一轮已 firing 的 chat_completions 泄漏告警，模拟长期活跃阳性。
	active, err := h.store.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status:      OpsAlertStatusFiring,
		Severity:    maskedIdentityProbeSeverityCritical,
		Title:       "掩码账号身份泄漏",
		Description: "历史泄漏",
		Dimensions:  maskedIdentityProbeLeakDims(maskedIdentityProbeEntryChat),
		FiredAt:     time.Now(),
	})
	require.NoError(t, err)

	h.svc.RunOnce(context.Background())

	// 同轮内只有 fire、没有 resolve：活跃泄漏事件应继续保持 firing，且不得重复创建。
	firing := h.store.firingWithKind(maskedProbeKindLeak)
	require.Len(t, firing, 1, "chat_completions 仅 1 个活跃泄漏事件（去重）")
	require.Equal(t, active.ID, firing[0].ID, "预置的活跃泄漏事件不得被同轮 clean 误 resolve")
	require.Equal(t, OpsAlertStatusFiring, firing[0].Status, "同轮 base chat 阳性不应关闭该维度告警")
	require.Equal(t, 1, h.store.createdCount(), "误关闭后再 recreate 会造成周期性重复告警，此处不得新增事件")
	require.Empty(t, h.store.firingWithKind(maskedProbeKindContentBreak), "identity_q clean 不应触发内容层破防")
}

// ② 同轮 base chat clean + identity_q 内容破防 → content_break firing 且不被同轮 resolve。
// 验证：base chat 这一轮 clean，不得把同维度（chat_completions）刚 fire 的内容层破防告警顺手关掉。
func TestMaskedIdentityProbe_SameRoundBaseChatCleanIdentityQBreakKeepsContentBreakFiring(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am kimi, an AI assistant"}}]}`},
		"identity_q":           {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am an AI language model"}}]}`},
		"identity_jailbreak":   {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am kimi, an AI assistant"}}]}`},
	})

	// 预置上一轮已 firing 的 chat_completions 内容层破防告警。
	active, err := h.store.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status:      OpsAlertStatusFiring,
		Severity:    maskedIdentityProbeSeverityCritical,
		Title:       "掩码账号内容层身份破防",
		Description: "历史内容层破防",
		Dimensions:  maskedIdentityProbeContentBreakDims(maskedIdentityProbeEntryChat),
		FiredAt:     time.Now(),
	})
	require.NoError(t, err)

	h.svc.RunOnce(context.Background())

	// 同轮内只有 fire、没有 resolve：内容层破防事件应保持 firing，不误关闭、不重建。
	firing := h.store.firingWithKind(maskedProbeKindContentBreak)
	require.Len(t, firing, 1, "chat_completions 仅 1 个活跃内容层破防事件")
	require.Equal(t, active.ID, firing[0].ID, "预置的活跃内容层破防事件不得被同轮 base chat clean 误 resolve")
	require.Equal(t, OpsAlertStatusFiring, firing[0].Status, "同轮 identity_q 破防不应被 base chat clean 关闭")
	require.Equal(t, 1, h.store.createdCount(), "误关闭后 recreate 会造成周期性重复告警，此处不得新增事件")
	require.Empty(t, h.store.firingWithKind(maskedProbeKindLeak), "协议层无痕迹不应判泄漏")
}

// ③ 交错轮恢复：泄漏轮后全 clean 轮 → 正常 resolve（不重复创建）。
// 验证：阳性轮产生活跃泄漏；下一轮全部 clean（含 base chat 与 identity_q 都 clean）后
// 安全地 resolve，不重复创建同类事件。
func TestMaskedIdentityProbe_LeakRoundThenCleanRoundResolvesWithoutRecreate(t *testing.T) {
	h := newMaskedProbeHarness(t, nil)

	// 第一轮：base chat 泄漏（path 回落命中 qwen 回复），identity_q 同路径亦泄漏但同维度去重。
	h.gateway.mu.Lock()
	h.gateway.replies = map[string]maskedProbeReply{
		"/v1/chat/completions": {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"served by Qwen upstream"}}]}`},
		"identity_jailbreak":   {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am kimi, an AI assistant"}}]}`},
	}
	h.gateway.mu.Unlock()
	h.svc.RunOnce(context.Background())

	leaks := h.store.firingWithKind(maskedProbeKindLeak)
	require.Len(t, leaks, 1, "chat_completions 泄漏去重为 1 个活跃事件")
	require.Equal(t, maskedIdentityProbeEntryChat, leaks[0].Dimensions[maskedProbeDimEntry])
	require.Equal(t, 1, h.store.createdCount(), "第一轮仅创建 1 个泄漏事件")

	// 第二轮：全 clean（base chat / identity_q 均内容含 kimi，协议层干净）。
	h.gateway.mu.Lock()
	h.gateway.replies = nil // 回落到默认 clean 回复
	h.gateway.mu.Unlock()
	h.svc.RunOnce(context.Background())

	require.Empty(t, h.store.firingWithKind(maskedProbeKindLeak), "全 clean 轮应 resolve 活跃泄漏事件")
	resolved := h.store.withKind(maskedProbeKindLeak)
	require.Len(t, resolved, 1, "不重复创建同类事件")
	require.Equal(t, OpsAlertStatusResolved, resolved[0].Status)
	require.NotNil(t, resolved[0].ResolvedAt)
	require.Equal(t, 1, h.store.createdCount(), "全 clean 轮不得重建已存在的泄漏事件")
}

// ④ 盲区（failed）不关闭活跃告警（失败关闭，QK-OBS 先例）。
// 验证：该 entry 出现 failed 结果时不得 resolve；同轮另一探针 clean 也不应把盲区误判为可 resolve。
func TestMaskedIdentityProbe_FailedBlindSpotDoesNotResolveActiveAlert(t *testing.T) {
	h := newMaskedProbeHarness(t, map[string]maskedProbeReply{
		"/v1/responses":        {status: http.StatusOK, body: `{"response":{"model":"kimi-k3"}}`},
		"/v1/messages":         {status: http.StatusOK, body: `{"model":"kimi-k3","content":[{"type":"text","text":"hi"}]}`},
		"identity_jailbreak":   {status: http.StatusOK, body: `{"model":"kimi-k3","choices":[{"message":{"content":"I am kimi, an AI assistant"}}]}`},
		// base chat 与 identity_q 共享 /v1/chat/completions：显式令该路径 500，使 chat_completions
		// 维度的两个探针结果均为 failed（观测盲区）。
		"/v1/chat/completions": {status: http.StatusInternalServerError, body: `{"error":"upstream down"}`},
	})

	// 预置上一轮已 firing 的 chat_completions 泄漏告警。
	active, err := h.store.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status:      OpsAlertStatusFiring,
		Severity:    maskedIdentityProbeSeverityCritical,
		Title:       "掩码账号身份泄漏",
		Description: "历史泄漏",
		Dimensions:  maskedIdentityProbeLeakDims(maskedIdentityProbeEntryChat),
		FiredAt:     time.Now(),
	})
	require.NoError(t, err)

	h.svc.RunOnce(context.Background())

	// 盲区轮：该 entry 全部 result=failed，不得 resolve 活跃告警；也不得 recreate。
	firing := h.store.firingWithKind(maskedProbeKindLeak)
	require.Len(t, firing, 1)
	require.Equal(t, active.ID, firing[0].ID, "盲区（failed）不得关闭活跃泄漏告警")
	require.Equal(t, OpsAlertStatusFiring, firing[0].Status)
	require.Equal(t, 1, h.store.createdCount(), "盲区轮不得重建事件")
}
