//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
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
	ok, detail, unsupported := ProbeAccountDirect(context.Background(), repo, acc, "gpt-4", okSrv.Client())
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
	ok2, detail2, unsupported2 := ProbeAccountDirect(context.Background(), &recordingAccountRepo{}, acc2, "gpt-4", failSrv.Client())
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
	if _, _, _ = ProbeAccountDirect(context.Background(), repo, acc, "gpt-4", srv.Client()); len(repo.calls) != 0 {
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
	if _, _, _ = ProbeAccountDirect(context.Background(), repo2, shadow, "gpt-4", srv.Client()); len(repo2.calls) == 0 {
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
	ok, detail, unsupported := ProbeAccountDirect(context.Background(), &recordingAccountRepo{}, acc, "gemini-1.5-pro", nil)
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
	w.probeFn = func(_ context.Context, _ AccountRepository, _ *Account, _ string, _ *http.Client) (bool, string, bool) {
		return true, "ok", false
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
	w2.probeFn = func(_ context.Context, _ AccountRepository, _ *Account, _ string, _ *http.Client) (bool, string, bool) {
		return false, "upstream status=500", false
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
	wA.probeFn = func(_ context.Context, _ AccountRepository, _ *Account, _ string, _ *http.Client) (bool, string, bool) {
		return false, "should not be called", false
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
	wB.probeFn = func(_ context.Context, _ AccountRepository, _ *Account, _ string, _ *http.Client) (bool, string, bool) {
		return false, "unsupported account platform=... for direct probe", true
	}
	if err := wB.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce(B) failed: %v", err)
	}
	if len(storeB.upserts) != 0 {
		t.Fatalf("expected 0 upserts when probe returns unsupported (non-authoritative), got %d", len(storeB.upserts))
	}
}
