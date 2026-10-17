//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// ---------- fakes ----------

// 账号仓储 fake：embedding AccountRepository（未实现的接口方法在调用时 panic，用于捕获
// 原语对账号仓储的越界写调用）。recordingAccountRepo 额外记录调用以断言零写副作用。
type recordingAccountRepo struct {
	AccountRepository // 嵌入：未覆写的方法调用会 panic（nil 接口），正好用于"禁止副作用"捕获
	mu                sync.Mutex
	calls             []string
	byID              map[int64]*Account
}

func (r *recordingAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "GetByID")
	r.mu.Unlock()
	if a, ok := r.byID[id]; ok {
		return a, nil
	}
	return nil, nil
}

// 账号仓储 fake：供 worker RunOnce 集成测试使用（active 账号 + GetByID）。
type fakeAccountRepo struct {
	AccountRepository
	active []Account
	byID   map[int64]*Account
}

func (r *fakeAccountRepo) ListActive(_ context.Context) ([]Account, error) { return r.active, nil }
func (r *fakeAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if a, ok := r.byID[id]; ok {
		return a, nil
	}
	return nil, nil
}

// 分组仓储 fake。
type fakeGroupRepo struct {
	GroupRepository
	active []Group
}

func (r *fakeGroupRepo) ListActive(_ context.Context) ([]Group, error) { return r.active, nil }

// 探测状态存储 fake（内存）。
type fakeProbeStore struct {
	mu      sync.Mutex
	states  map[ChannelMonitorCatalogKey]*ProbeState
	fresh   map[ChannelMonitorCatalogKey]struct{}
	upserts []*ProbeState
}

func newFakeProbeStore() *fakeProbeStore {
	return &fakeProbeStore{
		states: make(map[ChannelMonitorCatalogKey]*ProbeState),
		fresh:  make(map[ChannelMonitorCatalogKey]struct{}),
	}
}

func (s *fakeProbeStore) GetProbeState(_ context.Context, platform, model string, groupID int64) (*ProbeState, error) {
	k := ChannelMonitorCatalogKey{Platform: platform, Model: model, GroupID: groupID}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.states[k]; ok {
		return st, nil
	}
	return nil, nil
}

func (s *fakeProbeStore) UpsertProbeState(_ context.Context, st *ProbeState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := ChannelMonitorCatalogKey{Platform: st.Platform, Model: st.Model, GroupID: st.GroupID}
	s.states[k] = st
	s.upserts = append(s.upserts, st)
	return nil
}

func (s *fakeProbeStore) FreshBusinessChannels(_ context.Context, _ time.Time) (map[ChannelMonitorCatalogKey]struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[ChannelMonitorCatalogKey]struct{}, len(s.fresh))
	for k := range s.fresh {
		out[k] = struct{}{}
	}
	return out, nil
}

// 账号副作用写方法名集合（原语禁止调用）。
var accountWriteMethods = map[string]bool{
	"SetError": true, "ClearError": true, "SetTempUnschedulable": true,
	"ClearTempUnschedulable": true, "SetRateLimited": true, "ClearRateLimit": true,
	"SetOverloaded": true, "UpdateExtra": true, "Update": true, "Delete": true,
}

// ---------- 测试 ①：原语对假上游返回 ok / !ok 两分支 ----------

func TestChannelMonitorProbe_AccountDirect_OKAndNotOK(t *testing.T) {
	// ok 分支：假上游返回 200。
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer okSrv.Close()

	acc := &Account{
		ID:          1,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"base_url": okSrv.URL, "api_key": "k"},
	}
	repo := &recordingAccountRepo{}
	ok, detail, unsupported, _, _ := ProbeAccountDirect(context.Background(), repo, acc, "gpt-4", okSrv.Client())
	if unsupported {
		t.Fatalf("expected supported probe for 200 upstream, got unsupported detail=%q", detail)
	}
	if !ok {
		t.Fatalf("expected ok=true for 200 upstream, got ok=%v detail=%q", ok, detail)
	}

	// !ok 分支：假上游返回 500。
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	acc2 := &Account{
		ID:          2,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"base_url": failSrv.URL, "api_key": "k"},
	}
	ok2, detail2, unsupported2, _, _ := ProbeAccountDirect(context.Background(), &recordingAccountRepo{}, acc2, "gpt-4", failSrv.Client())
	if unsupported2 {
		t.Fatalf("expected authoritative (non-unsupported) failure for 500 upstream, got unsupported detail=%q", detail2)
	}
	if ok2 {
		t.Fatalf("expected ok=false for 500 upstream, got ok=true detail=%q", detail2)
	}
	if detail2 == "" {
		t.Fatalf("expected non-empty detail for 500 upstream")
	}
}

// ---------- 测试 ②：原语不产生账号写副作用 ----------

func TestChannelMonitorProbe_AccountDirect_NoAccountWriteSideEffect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 普通账号（非影子）：resolveCredentialAccount 应立即返回，零账号仓储调用。
	acc := &Account{
		ID:          10,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"base_url": srv.URL, "api_key": "k"},
	}
	repo := &recordingAccountRepo{}
	if _, _, _, _, _ = ProbeAccountDirect(context.Background(), repo, acc, "gpt-4", srv.Client()); len(repo.calls) != 0 {
		t.Fatalf("expected zero account repo calls for non-shadow account, got %v", repo.calls)
	}

	// 影子账号：仅解影射读母账号（GetByID），不调用任何写方法。
	parentID := int64(99)
	shadow := &Account{
		ID:              20,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeAPIKey,
		Status:          StatusActive,
		ParentAccountID: &parentID,
		Credentials:     map[string]any{"base_url": srv.URL, "api_key": "k"},
	}
	parent := &Account{
		ID:          parentID,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"base_url": srv.URL, "api_key": "k"},
	}
	repo2 := &recordingAccountRepo{byID: map[int64]*Account{parentID: parent}}
	if _, _, _, _, _ = ProbeAccountDirect(context.Background(), repo2, shadow, "gpt-4", srv.Client()); len(repo2.calls) == 0 {
		t.Fatalf("expected GetByID call for shadow account, got none")
	}
	for _, c := range repo2.calls {
		if accountWriteMethods[c] {
			t.Fatalf("primitive must not call account write method %q, calls=%v", c, repo2.calls)
		}
	}
}

// ---------- 测试 ②b：原语对不可直连探测账号返回非权威 unsupported（不落 last_ok=false） ----------

func TestChannelMonitorProbe_AccountDirect_UnsupportedNonAuthoritative(t *testing.T) {
	// gemini OAuth 账号：IsGemini 为真但非 apikey/upstream → canDirectProbe 为假 → unsupported。
	acc := &Account{
		ID:       1,
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
	}
	ok, detail, unsupported, _, _ := ProbeAccountDirect(context.Background(), &recordingAccountRepo{}, acc, "gemini-1.5-pro", nil)
	if !unsupported {
		t.Fatalf("expected unsupported=true for gemini oauth account, got ok=%v detail=%q", ok, detail)
	}
	if ok {
		t.Fatalf("unsupported probe must not report ok=true, got detail=%q", detail)
	}
}

// ---------- 测试 ③：候选筛选（有新鲜业务观测跳过、next_due 未到跳过） ----------

func TestChannelMonitorProbe_PlanBatch_CandidateFiltering(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	k1 := ChannelMonitorCatalogKey{Platform: "openai", Model: "a", GroupID: 1}
	k2 := ChannelMonitorCatalogKey{Platform: "openai", Model: "b", GroupID: 1}
	k3 := ChannelMonitorCatalogKey{Platform: "anthropic", Model: "c", GroupID: 2}

	catalog := ChannelMonitorCatalog{
		Keys: map[ChannelMonitorCatalogKey]struct{}{k1: {}, k2: {}, k3: {}},
		CandidateAccountIDs: map[ChannelMonitorCatalogKey][]int64{
			k1: {11}, k2: {12}, k3: {13},
		},
	}
	fresh := map[ChannelMonitorCatalogKey]struct{}{k1: {}} // k1 有新鲜业务观测 → 跳过
	states := map[ChannelMonitorCatalogKey]*ProbeState{
		k2: {NextDue: now.Add(10 * time.Minute)}, // k2 next_due 未到 → 跳过
	}
	// 候选账号均可直连探测（openai apikey；anthropic 平台默认 chat_completions 协议，
	// 不落 anthropic 分支，故此处统一用 openai apikey 表示"可探测"）。
	accountByID := map[int64]*Account{
		11: {ID: 11, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
		12: {ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
		13: {ID: 13, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
	}

	tasks := planProbeBatch(now, catalog, fresh, states, accountByID, 100)
	if len(tasks) != 1 {
		t.Fatalf("expected exactly 1 task, got %d: %+v", len(tasks), tasks)
	}
	if tasks[0].Key != k3 || tasks[0].SampledAccountID != 13 {
		t.Fatalf("expected task for k3/account 13, got %+v", tasks[0])
	}
}

// ---------- 测试 ④：冷却（ok→30min、!ok→2h） ----------

func TestChannelMonitorProbe_ComputeNextDue(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := computeNextDue(now, true); !got.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("ok cooldown: want +30m, got %v", got.Sub(now))
	}
	if got := computeNextDue(now, false); !got.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("fail cooldown: want +2h, got %v", got.Sub(now))
	}
}

// RunOnce 端到端验证冷却：探测结果落入 probe_state 的 next_due 符合冷却窗口。
func TestChannelMonitorProbe_RunOnce_RecordsCooldown(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newFakeProbeStore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	groupID := int64(100)
	acc := Account{
		ID:          10,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"base_url": srv.URL, "api_key": "k", "model_mapping": map[string]any{"gpt-4": "gpt-4"}},
		GroupIDs:    []int64{groupID},
	}
	grp := Group{ID: groupID, Status: StatusActive}

	repo := &fakeAccountRepo{active: []Account{acc}, byID: map[int64]*Account{10: &acc}}
	grepo := &fakeGroupRepo{active: []Group{grp}}
	cfg := &config.Config{Probe: config.ProbeConfig{Enabled: true, MaxPerTick: 2}}

	w := NewChannelMonitorProbeWorker(store, repo, grepo, nil, cfg)
	w.clock = func() time.Time { return now }
	// 注入探测结果：ok=true（权威成功）。
	w.probeFn = func(_ context.Context, _ AccountRepository, _ *Account, _ string, _ *http.Client) (bool, string, bool, *int, *int) {
		return true, "ok", false, nil, nil
	}

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(store.upserts))
	}
	st := store.upserts[0]
	if !st.LastOK {
		t.Fatalf("expected LastOK=true, got false")
	}
	if !st.NextDue.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("expected NextDue +30m, got %v", st.NextDue.Sub(now))
	}

	// 第二次：注入 !ok（权威失败），验证 2h 冷却。
	store2 := newFakeProbeStore()
	w2 := NewChannelMonitorProbeWorker(store2, repo, grepo, nil, cfg)
	w2.clock = func() time.Time { return now }
	w2.probeFn = func(_ context.Context, _ AccountRepository, _ *Account, _ string, _ *http.Client) (bool, string, bool, *int, *int) {
		return false, "upstream status=500", false, nil, nil
	}
	if err := w2.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce(!ok) failed: %v", err)
	}
	if len(store2.upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(store2.upserts))
	}
	if store2.upserts[0].LastOK {
		t.Fatalf("expected LastOK=false, got true")
	}
	if !store2.upserts[0].NextDue.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("expected NextDue +2h, got %v", store2.upserts[0].NextDue.Sub(now))
	}
}

// ---------- 测试 ⑤：max_per_tick 限速 ----------

func TestChannelMonitorProbe_PlanBatch_MaxPerTickLimit(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	keys := []ChannelMonitorCatalogKey{
		{Platform: "openai", Model: "a", GroupID: 1},
		{Platform: "openai", Model: "b", GroupID: 1},
		{Platform: "openai", Model: "c", GroupID: 1},
	}
	catalog := ChannelMonitorCatalog{Keys: map[ChannelMonitorCatalogKey]struct{}{}, CandidateAccountIDs: map[ChannelMonitorCatalogKey][]int64{}}
	accountByID := make(map[int64]*Account, len(keys))
	for i, k := range keys {
		catalog.Keys[k] = struct{}{}
		id := int64(i + 1)
		catalog.CandidateAccountIDs[k] = []int64{id}
		accountByID[id] = &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive}
	}

	// maxPerTick=2 → 最多 2 条。
	tasks := planProbeBatch(now, catalog, nil, nil, accountByID, 2)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks for maxPerTick=2, got %d", len(tasks))
	}

	// maxPerTick 充足 → 全部 3 条。
	tasksAll := planProbeBatch(now, catalog, nil, nil, accountByID, 10)
	if len(tasksAll) != 3 {
		t.Fatalf("expected 3 tasks for maxPerTick=10, got %d", len(tasksAll))
	}
}

// ---------- 测试 ⑥：候选选取——可探测账号优先（按 ID 升序取首个可探测） ----------

func TestChannelMonitorProbe_PlanBatch_ProbeableAccountPriority(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	k := ChannelMonitorCatalogKey{Platform: "openai", Model: "a", GroupID: 1}
	catalog := ChannelMonitorCatalog{
		Keys:                map[ChannelMonitorCatalogKey]struct{}{k: {}},
		CandidateAccountIDs: map[ChannelMonitorCatalogKey][]int64{k: {5, 6}},
	}
	// ID 5 不可探测（gemini OAuth），ID 6 可探测（openai apikey）。
	accountByID := map[int64]*Account{
		5: {ID: 5, Platform: PlatformGemini, Type: AccountTypeOAuth, Status: StatusActive},
		6: {ID: 6, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
	}
	tasks := planProbeBatch(now, catalog, nil, nil, accountByID, 10)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d: %+v", len(tasks), tasks)
	}
	if tasks[0].SampledAccountID != 6 {
		t.Fatalf("expected probeable account 6 selected (priority over non-probeable 5), got %d", tasks[0].SampledAccountID)
	}
}

// ---------- 测试 ⑦：候选选取——渠道无任可探测账号 → 跳过（无数据态，不写行） ----------

func TestChannelMonitorProbe_PlanBatch_SkipChannelWithoutProbeableAccount(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	k := ChannelMonitorCatalogKey{Platform: "gemini", Model: "a", GroupID: 1}
	catalog := ChannelMonitorCatalog{
		Keys:                map[ChannelMonitorCatalogKey]struct{}{k: {}},
		CandidateAccountIDs: map[ChannelMonitorCatalogKey][]int64{k: {7, 8}},
	}
	// 两个候选均为 gemini OAuth（不可直连探测）。
	accountByID := map[int64]*Account{
		7: {ID: 7, Platform: PlatformGemini, Type: AccountTypeOAuth, Status: StatusActive},
		8: {ID: 8, Platform: PlatformGemini, Type: AccountTypeOAuth, Status: StatusActive},
	}
	tasks := planProbeBatch(now, catalog, nil, nil, accountByID, 10)
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks (channel skipped, no probeable account), got %d: %+v", len(tasks), tasks)
	}
}

// ---------- 测试 ⑧：RunOnce 对不可探测账号/unsupported 不写 probe_state 行 ----------

func TestChannelMonitorProbe_RunOnce_SkipUnsupportedNoRow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	groupID := int64(100)
	grp := Group{ID: groupID, Status: StatusActive}
	grepo := &fakeGroupRepo{active: []Group{grp}}
	cfg := &config.Config{Probe: config.ProbeConfig{Enabled: true, MaxPerTick: 2}}

	// 场景 A：渠道仅含不可探测账号（gemini OAuth）→ 候选选取阶段即跳过，不写行。
	onlyNonProbeable := Account{
		ID:          5,
		Platform:    PlatformGemini,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Credentials: map[string]any{"model_mapping": map[string]any{"gemini-1.5-pro": "gemini-1.5-pro"}},
		GroupIDs:    []int64{groupID},
	}
	repoA := &fakeAccountRepo{active: []Account{onlyNonProbeable}, byID: map[int64]*Account{5: &onlyNonProbeable}}
	storeA := newFakeProbeStore()
	wA := NewChannelMonitorProbeWorker(storeA, repoA, grepo, nil, cfg)
	wA.clock = func() time.Time { return now }
	wA.probeFn = func(_ context.Context, _ AccountRepository, _ *Account, _ string, _ *http.Client) (bool, string, bool, *int, *int) {
		return false, "should not be called", false, nil, nil
	}
	if err := wA.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce(A) failed: %v", err)
	}
	if len(storeA.upserts) != 0 {
		t.Fatalf("expected 0 upserts for channel with no probeable account, got %d", len(storeA.upserts))
	}

	// 场景 B：渠道含可探测账号，但探测原语竞态返回 unsupported（非权威）→ worker 跳过写行，
	// 与"无数据"语义一致，绝不落 last_ok=false。
	probeable := Account{
		ID:          6,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-4": "gpt-4"}},
		GroupIDs:    []int64{groupID},
	}
	repoB := &fakeAccountRepo{active: []Account{probeable}, byID: map[int64]*Account{6: &probeable}}
	storeB := newFakeProbeStore()
	wB := NewChannelMonitorProbeWorker(storeB, repoB, grepo, nil, cfg)
	wB.clock = func() time.Time { return now }
	wB.probeFn = func(_ context.Context, _ AccountRepository, _ *Account, _ string, _ *http.Client) (bool, string, bool, *int, *int) {
		return false, "unsupported account platform=... for direct probe", true, nil, nil
	}
	if err := wB.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce(B) failed: %v", err)
	}
	if len(storeB.upserts) != 0 {
		t.Fatalf("expected 0 upserts when probe returns unsupported (non-authoritative), got %d", len(storeB.upserts))
	}
}

// ---------- 测试 ⑨（定向 B1）：anthropic apikey 账号可直连探测 + 流式 + 200 → ok ----------

func TestChannelMonitorProbe_AnthropicAPIKeyProbeable(t *testing.T) {
	// 假上游：校验请求形状并回 SSE 文本增量。
	got := struct {
		method, url, apiKey, authz, body string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method = r.Method
		got.url = r.URL.String()
		got.apiKey = r.Header.Get("x-api-key")
		got.authz = r.Header.Get("Authorization")
		got.body = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: "+`{"type":"message_start"}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: "+`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: "+`{"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	acc := &Account{
		ID:          1,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"base_url": srv.URL, "api_key": "secret-key"},
	}

	// B1 修复点：原生 anthropic 平台账号此前 GetAPIProtocol 回落 chat_completions，
	// 导致 canDirectProbe 永为 false → 此处必须为 true。
	if !canDirectProbe(acc) {
		t.Fatalf("expected canDirectProbe=true for anthropic apikey account (B1 fix)")
	}
	spec, err := buildProbeRequestSpec(acc, "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("buildProbeRequestSpec failed: %v", err)
	}
	if spec.method != http.MethodPost {
		t.Fatalf("expected POST method, got %q", spec.method)
	}
	if !strings.HasSuffix(spec.url, "/v1/messages?beta=true") {
		t.Fatalf("expected url suffix /v1/messages?beta=true, got %q", spec.url)
	}
	// B1 修复点：anthropic 请求须携带 api_key 认证头（x-api-key 或 Authorization: Bearer）。
	if spec.headers["X-Api-Key"] != "secret-key" && !strings.Contains(spec.headers["Authorization"], "secret-key") {
		t.Fatalf("expected api_key auth header in probe spec, got %v", spec.headers)
	}
	var body map[string]any
	if err := json.Unmarshal(spec.body, &body); err != nil {
		t.Fatalf("request body not json: %v", err)
	}
	if stream, ok := body["stream"].(bool); !ok || !stream {
		t.Fatalf("expected body stream:true, got %v", body["stream"])
	}

	// 端到端：假上游 200 + SSE 文本增量 → ok=true，FirstTokenMs/TotalMs 非空且 total>=first。
	ok, detail, unsupported, firstTokenMs, totalMs := ProbeAccountDirect(context.Background(), &recordingAccountRepo{}, acc, "claude-3-5-sonnet", srv.Client())
	if unsupported {
		t.Fatalf("expected supported probe, got unsupported detail=%q", detail)
	}
	if !ok {
		t.Fatalf("expected ok=true for SSE upstream, got detail=%q", detail)
	}
	if firstTokenMs == nil {
		t.Fatalf("expected FirstTokenMs non-nil for SSE text delta")
	}
	if totalMs == nil || *totalMs < *firstTokenMs {
		t.Fatalf("expected TotalMs >= FirstTokenMs, total=%v first=%v", totalMs, firstTokenMs)
	}
}

// ---------- 测试 ⑨b（PROBE-URL-1）：openai 探针复用业务同源版本感知 helper，消除 /v1/v1 双段 404 ----------

func TestChannelMonitorProbe_OpenAIChatCompletionsSpecURL(t *testing.T) {
	model := "gpt-4"

	cases := []struct {
		name string
		base string
		want string
	}{
		{
			// a) base 无版本段：追加完整 endpoint。
			name: "no version suffix",
			base: "https://upstream.example",
			want: "https://upstream.example/v1/chat/completions",
		},
		{
			// b) 核心回归：base 已带 /v1，不得出现 /v1/v1 双段。
			name: "base has /v1",
			base: "https://upstream.example/v1",
			want: "https://upstream.example/v1/chat/completions",
		},
		{
			// c) base 带非 v1 版本段（如 /openai/v2）：对齐 openai_endpoint_url 既有 "version" 用例语义。
			name: "base has other version suffix",
			base: "https://upstream.example/openai/v2",
			want: "https://upstream.example/openai/v2/chat/completions",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			acc := &Account{
				ID:          1,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Credentials: map[string]any{"base_url": c.base, "api_key": "k"},
			}
			spec, err := buildOpenAIChatCompletionsSpec(acc, model)
			if err != nil {
				t.Fatalf("buildOpenAIChatCompletionsSpec failed: %v", err)
			}
			if spec.url != c.want {
				t.Fatalf("base=%q: expected url %q, got %q", c.base, c.want, spec.url)
			}
			// 核心回归断言：带 /v1 的 base 不得产生 /v1/v1 双段。
			if strings.Contains(spec.url, "/v1/v1") {
				t.Fatalf("base=%q: url must not contain /v1/v1, got %q", c.base, spec.url)
			}
		})
	}
}

// ---------- 测试 ⑩（定向 B1）：cursor oauth（或任一不可探协议）仍 unsupported ----------

func TestChannelMonitorProbe_CursorOAuthUnsupported(t *testing.T) {
	// cursor oauth：非 anthropic / 非 OpenAI 兼容 / 非 gemini → 不可直连探测。
	acc := &Account{ID: 1, Platform: domain.PlatformCursor, Type: AccountTypeOAuth, Status: StatusActive}
	if canDirectProbe(acc) {
		t.Fatalf("expected cursor oauth account NOT probeable")
	}
	if _, err := buildProbeRequestSpec(acc, "claude"); err == nil {
		t.Fatalf("expected buildProbeRequestSpec to error for cursor oauth account")
	}
	// 端到端原语也须返回 unsupported（非权威，绝不落 last_ok=false）。
	ok, detail, unsupported, _, _ := ProbeAccountDirect(context.Background(), &recordingAccountRepo{}, acc, "claude", nil)
	if !unsupported {
		t.Fatalf("expected unsupported=true for cursor oauth, got ok=%v detail=%q", ok, detail)
	}
	if ok {
		t.Fatalf("unsupported probe must not report ok=true")
	}
}

// ---------- 测试 ⑪（定向 B3）：流式 TTFT 计时 ----------

func TestChannelMonitorProbe_StreamingTTFT(t *testing.T) {
	// 假上游：先发 message_start（起始帧），延迟 100ms 再发文本增量，再发 message_stop。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: "+`{"type":"message_start"}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(100 * time.Millisecond)
		fmt.Fprint(w, "data: "+`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: "+`{"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	acc := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Credentials: map[string]any{"base_url": srv.URL, "api_key": "k"}}

	ok, detail, unsupported, firstTokenMs, totalMs := ProbeAccountDirect(context.Background(), &recordingAccountRepo{}, acc, "claude", srv.Client())
	if unsupported || !ok {
		t.Fatalf("expected ok probe, got ok=%v unsupported=%v detail=%q", ok, unsupported, detail)
	}
	// 首帧计时 = 错误实现必须红：FirstTokenMs 应接近文本增量延迟（~100ms），而非 ~0。
	if firstTokenMs == nil {
		t.Fatalf("expected FirstTokenMs non-nil when text delta arrives")
	}
	if *firstTokenMs < 80 {
		t.Fatalf("expected FirstTokenMs >= 80ms (text delta after 100ms sleep), got %d", *firstTokenMs)
	}
	if totalMs == nil || *totalMs < *firstTokenMs {
		t.Fatalf("expected TotalMs >= FirstTokenMs, total=%v first=%v", totalMs, firstTokenMs)
	}

	// 全程无文本增量（仅 message_start）→ FirstTokenMs 必须为 nil。
	noText := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: "+`{"type":"message_start"}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer noText.Close()
	acc2 := &Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Credentials: map[string]any{"base_url": noText.URL, "api_key": "k"}}
	ok2, _, unsupported2, firstTokenMs2, totalMs2 := ProbeAccountDirect(context.Background(), &recordingAccountRepo{}, acc2, "claude", noText.Client())
	if unsupported2 || !ok2 {
		t.Fatalf("expected ok probe for no-text upstream, got ok=%v unsupported=%v", ok2, unsupported2)
	}
	if firstTokenMs2 != nil {
		t.Fatalf("expected FirstTokenMs=nil when no text delta, got %v", *firstTokenMs2)
	}
	if totalMs2 == nil {
		t.Fatalf("expected TotalMs non-nil for no-text upstream")
	}
}

// ---------- 测试 ⑫（定向 B2）：候选选取优先 Schedulable ----------

func TestChannelMonitorProbe_SelectProbeableAccountIDSchedulable(t *testing.T) {
	// 候选 [10,20] 均 openai apikey 可探：10 非 schedulable，20 schedulable=true。
	// B2：优先选 schedulable 的 20，即使 ID 更大。
	accountByID := map[int64]*Account{
		10: {ID: 10, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
		20: {ID: 20, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true},
	}
	if got := selectProbeableAccountID([]int64{10, 20}, accountByID); got != 20 {
		t.Fatalf("expected schedulable account 20 selected, got %d", got)
	}

	// 候选均非 schedulable → 退回最低 ID（10）。
	accountByID2 := map[int64]*Account{
		10: {ID: 10, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
		20: {ID: 20, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
	}
	if got := selectProbeableAccountID([]int64{10, 20}, accountByID2); got != 10 {
		t.Fatalf("expected fallback lowest-ID 10, got %d", got)
	}

	// 候选含不可探账号（gemini oauth）与 schedulable 可探账号：选 schedulable 可探的 30。
	accountByID3 := map[int64]*Account{
		5:  {ID: 5, Platform: PlatformGemini, Type: AccountTypeOAuth, Status: StatusActive},
		30: {ID: 30, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true},
	}
	if got := selectProbeableAccountID([]int64{5, 30}, accountByID3); got != 30 {
		t.Fatalf("expected schedulable probeable 30, got %d", got)
	}
}

// ---------- 测试 ⑬（定向 B5）：leader 锁 TTL 推导 ----------

func TestProbeLeaderLockTTLFor(t *testing.T) {
	// maxPerTick=2 → 2×5min + 5min = 15min（恰为原常量下限）。
	if got := probeLeaderLockTTLFor(2); got != 15*time.Minute {
		t.Fatalf("expected 15m for maxPerTick=2, got %v", got)
	}
	// maxPerTick=1 → 公式 10min 低于下限 → 兜底 15min。
	if got := probeLeaderLockTTLFor(1); got != 15*time.Minute {
		t.Fatalf("expected 15m floor for maxPerTick=1, got %v", got)
	}
	// maxPerTick=3 → 3×5min + 5min = 20min（无兜底介入）。
	if got := probeLeaderLockTTLFor(3); got != 20*time.Minute {
		t.Fatalf("expected 20m for maxPerTick=3, got %v", got)
	}
	// 非正 maxPerTick 按默认 2 处理 → 15min。
	if got := probeLeaderLockTTLFor(0); got != 15*time.Minute {
		t.Fatalf("expected 15m default for maxPerTick=0, got %v", got)
	}
}

// ---------- 测试 ⑭（FIX-1）：200 响应中途读取失败 → 失败关闭（ok=false, detail 含 stream read error） ----------

// readErrorAfterReader 在累计读出 errAfter 字节后以非 EOF 错误结束，用于模拟 200 响应体
// 中途断流（连接中断 / 读超时）。errAfter 之前的数据正常可读，之后返回错误而非 io.EOF。
type readErrorAfterReader struct {
	data     []byte
	pos      int
	errAfter int
}

func (r *readErrorAfterReader) Read(p []byte) (int, error) {
	if r.pos >= r.errAfter {
		return 0, fmt.Errorf("simulated connection reset mid-stream")
	}
	n := copy(p, r.data[r.pos:r.errAfter])
	r.pos += n
	return n, nil
}

// fixedResponseTransport 直接返回预设 *http.Response，不经真实网络，便于注入异常响应体。
type fixedResponseTransport struct {
	resp *http.Response
}

func (t *fixedResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return t.resp, nil
}

func TestChannelMonitorProbe_StreamReadErrorMid200(t *testing.T) {
	// 假上游：HTTP 200，但响应体在读出部分数据后发生非 EOF 读取错误（连接中断/读超时）。
	body := &readErrorAfterReader{
		data:     []byte("data: " + `{"type":"message_start"}`),
		errAfter: 10,
	}
	resp := &http.Response{
		StatusCode:    200,
		Status:        "200 OK",
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(body),
		ContentLength: -1,
	}
	client := &http.Client{Transport: &fixedResponseTransport{resp: resp}}

	acc := &Account{
		ID:          1,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"base_url": "http://fake", "api_key": "k"},
	}
	ok, detail, unsupported, _, _ := ProbeAccountDirect(context.Background(), &recordingAccountRepo{}, acc, "claude", client)
	if unsupported {
		t.Fatalf("expected supported probe, got unsupported detail=%q", detail)
	}
	// FIX-1：读取失败不得以 ok=true 写入。
	if ok {
		t.Fatalf("expected ok=false for mid-stream read error, got ok=true detail=%q", detail)
	}
	if !strings.Contains(detail, "stream read error") {
		t.Fatalf("expected detail to contain 'stream read error', got %q", detail)
	}
}

// ---------- 测试 ⑮（FIX-2）：SSE 单行超长无换行（> probeSSELineLimit）→ 失败关闭（ok=false, detail 含 sse line limit exceeded） ----------

func TestChannelMonitorProbe_SSELineLimitExceeded(t *testing.T) {
	// 假上游：HTTP 200，但单行 SSE 数据远超 probeSSELineLimit 且全程无换行 → 命中单行上限。
	longLine := strings.Repeat("a", probeSSELineLimit*2)
	resp := &http.Response{
		StatusCode:    200,
		Status:        "200 OK",
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(longLine)),
		ContentLength: -1,
	}
	client := &http.Client{Transport: &fixedResponseTransport{resp: resp}}

	acc := &Account{
		ID:          1,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"base_url": "http://fake", "api_key": "k"},
	}
	ok, detail, unsupported, _, _ := ProbeAccountDirect(context.Background(), &recordingAccountRepo{}, acc, "claude", client)
	if unsupported {
		t.Fatalf("expected supported probe, got unsupported detail=%q", detail)
	}
	// FIX-2：超长无换行行不得跳行后把 2xx 记成功。
	if ok {
		t.Fatalf("expected ok=false for sse line limit exceeded, got ok=true detail=%q", detail)
	}
	if !strings.Contains(detail, "sse line limit exceeded") {
		t.Fatalf("expected detail to contain 'sse line limit exceeded', got %q", detail)
	}
}
