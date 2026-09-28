package service

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// 本文件钉死方案 §2.1（B2）关于聚合直绑逐模型目录闸门的全部回归用例：
// 全分组命中/未命中、目录不可用三态、快照命中后目录失效仍排除（§2.1-3 快照后强制重执行）、
// 判定顺序（先 GetMappedModel 映射后比较）、查询预算（≤4 并发、前4固定、并列按 ID 升序、
// not_probed 计数）、聚合放行（platform_mismatch 越过）、快照 bucket 键同步 + 版本 bump 失效。

// fakeCodeBuddyCatalogProvider 是注入式目录提供者，用于确定性地驱动闸门判定。
type fakeCodeBuddyCatalogProvider struct {
	mu           sync.Mutex
	catalogs     map[int64][]string // 账号 ID -> 上游模型目录
	errAccounts  map[int64]bool     // 返回 error（目录不可达/超时/非集合结构）
	emptyAccounts map[int64]bool    // 返回空集合
	nilAccounts  map[int64]bool     // 返回 nil（无法判定为非集合结构）
	probed       map[int64]bool     // 记录实际被探测的账号 ID
}

func newFakeCatalog(catalogs map[int64][]string) *fakeCodeBuddyCatalogProvider {
	return &fakeCodeBuddyCatalogProvider{
		catalogs:      catalogs,
		errAccounts:   map[int64]bool{},
		emptyAccounts: map[int64]bool{},
		nilAccounts:   map[int64]bool{},
		probed:        map[int64]bool{},
	}
}

func (f *fakeCodeBuddyCatalogProvider) UpstreamModelIDs(ctx context.Context, account *Account) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.probed == nil {
		f.probed = map[int64]bool{}
	}
	f.probed[account.ID] = true
	if f.errAccounts[account.ID] {
		return nil, errCodeBuddyCatalogUnavailable
	}
	if f.nilAccounts[account.ID] {
		return nil, nil
	}
	if f.emptyAccounts[account.ID] {
		return []string{}, nil
	}
	return f.catalogs[account.ID], nil
}

func (f *fakeCodeBuddyCatalogProvider) probedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, 0, len(f.probed))
	for id := range f.probed {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// newCodeBuddyScheduler 构造注入目录提供者的调度器（生产默认实现由 catalogProvider() 回落）。
func newCodeBuddyScheduler(provider codeBuddyCatalogProvider) *defaultOpenAIAccountScheduler {
	return &defaultOpenAIAccountScheduler{codeBuddyCatalogProvider: provider}
}

// cbAccount 构造一个 codebuddy 候选账号（含可选的客户端名 → 上游名 model_mapping）。
func cbAccount(id int64, priority int, mapping map[string]string) Account {
	creds := map[string]any{}
	if mapping != nil {
		m := map[string]any{}
		for k, v := range mapping {
			m[k] = v
		}
		creds["model_mapping"] = m
	}
	return Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Priority:    priority,
		Type:        AccountTypeOAuth,
		Credentials: creds,
		Extra:       map[string]any{},
	}
}

// TestCodebuddyAggregateGate_PerGroupHitAndMiss 覆盖方案 §2.1-1 全分组逐模型命中/未命中。
func TestCodebuddyAggregateGate_PerGroupHitAndMiss(t *testing.T) {
	aggPlatforms := []string{PlatformDeepseek, PlatformZhipu, PlatformKimi, PlatformMiniMax, PlatformOther, PlatformCodeBuddy}
	for _, p := range aggPlatforms {
		hit := cbAccount(1, 0, nil)
		miss := cbAccount(2, 0, nil)
		provider := newFakeCatalog(map[int64][]string{
			1: {"m"},   // 命中
			2: {"x"},   // 未命中
		})
		s := newCodeBuddyScheduler(provider)
		req := OpenAIAccountScheduleRequest{Platform: p, RequestedModel: "m"}
		verdicts := s.evaluateCodeBuddyCatalogGate(context.Background(), req, []Account{hit, miss})

		if v, ok := verdicts[1]; !ok || v != codeBuddyCatalogAllowed {
			t.Errorf("platform=%s: 命中账号应计 Allowed，got ok=%v v=%v", p, ok, v)
		}
		if v, ok := verdicts[2]; !ok || v != codeBuddyCatalogModelMissing {
			t.Errorf("platform=%s: 未命中账号应计 ModelMissing，got ok=%v v=%v", p, ok, v)
		}
	}

	// 非聚合平台（openai）请求不触发闸门：codebuddy 账号不应被纳入 verdict。
	provider := newFakeCatalog(nil)
	s := newCodeBuddyScheduler(provider)
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, RequestedModel: "m"}
	verdicts := s.evaluateCodeBuddyCatalogGate(context.Background(), req, []Account{cbAccount(9, 0, nil)})
	if len(verdicts) != 0 {
		t.Errorf("非聚合平台不应触发目录闸门，got verdicts=%v", verdicts)
	}
}

// TestCodebuddyAggregateGate_CatalogUnavailableThreeStates 覆盖目录不可用三态
// （错误/空集合/非集合结构）→ upstream_catalog_unavailable，禁兜底。
func TestCodebuddyAggregateGate_CatalogUnavailableThreeStates(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*fakeCodeBuddyCatalogProvider)
		wantErr bool
	}{
		{"error", func(f *fakeCodeBuddyCatalogProvider) { f.errAccounts[1] = true }, true},
		{"empty", func(f *fakeCodeBuddyCatalogProvider) { f.emptyAccounts[1] = true }, false},
		{"non-collection", func(f *fakeCodeBuddyCatalogProvider) { f.nilAccounts[1] = true }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeCatalog(map[int64][]string{1: {"m"}})
			c.setup(f)
			s := newCodeBuddyScheduler(f)
			req := OpenAIAccountScheduleRequest{Platform: PlatformDeepseek, RequestedModel: "m"}
			verdicts := s.evaluateCodeBuddyCatalogGate(context.Background(), req, []Account{cbAccount(1, 0, nil)})
			v, ok := verdicts[1]
			if !ok {
				t.Fatalf("账号 1 应有 verdict")
			}
			if v != codeBuddyCatalogUnavailable {
				t.Errorf("目录不可用三态均应计 Unavailable，got %v", v)
			}
		})
	}
}

// TestCodebuddyAggregateGate_SnapshotHitButCatalogExpiredExcluded 覆盖 §2.1-3 快照后强制重执行：
// 账号已在候选超集（快照命中），但选号时当前目录失效/模型不在当前目录 → 仍排除，快照命中不豁免。
func TestCodebuddyAggregateGate_SnapshotHitButCatalogExpiredExcluded(t *testing.T) {
	// 场景①：快照命中后目录不可用 → upstream_catalog_unavailable。
	f1 := newFakeCatalog(map[int64][]string{1: {"m"}})
	f1.errAccounts[1] = true
	s1 := newCodeBuddyScheduler(f1)
	req1 := OpenAIAccountScheduleRequest{Platform: PlatformDeepseek, RequestedModel: "m"}
	v1 := s1.evaluateCodeBuddyCatalogGate(context.Background(), req1, []Account{cbAccount(1, 0, nil)})[1]
	if v1 != codeBuddyCatalogUnavailable {
		t.Errorf("快照命中后目录不可用应计 Unavailable，got %v", v1)
	}

	// 场景②：快照命中后请求模型不在当前目录 → model_not_in_upstream_catalog。
	f2 := newFakeCatalog(map[int64][]string{1: {"current-model"}})
	s2 := newCodeBuddyScheduler(f2)
	req2 := OpenAIAccountScheduleRequest{Platform: PlatformDeepseek, RequestedModel: "m"}
	v2 := s2.evaluateCodeBuddyCatalogGate(context.Background(), req2, []Account{cbAccount(1, 0, nil)})[1]
	if v2 != codeBuddyCatalogModelMissing {
		t.Errorf("快照命中后模型不在当前目录应计 ModelMissing，got %v", v2)
	}
}

// TestCodebuddyAggregateGate_AliasMappingOrder 覆盖 §2.1-1 判定顺序：
// 先按账号级 GetMappedModel 映射为上游名，再与目录精确比较（禁客户端名直比、禁映射跳过比较）。
func TestCodebuddyAggregateGate_AliasMappingOrder(t *testing.T) {
	// 别名映射 + 目录命中（可选）：gpt-4 -> deepseek-chat，目录含 deepseek-chat → 允许。
	fA := newFakeCatalog(map[int64][]string{1: {"deepseek-chat"}})
	sA := newCodeBuddyScheduler(fA)
	accA := cbAccount(1, 0, map[string]string{"gpt-4": "deepseek-chat"})
	vA := sA.evaluateCodeBuddyCatalogGate(context.Background(), OpenAIAccountScheduleRequest{Platform: PlatformDeepseek, RequestedModel: "gpt-4"}, []Account{accA})[1]
	if vA != codeBuddyCatalogAllowed {
		t.Errorf("别名映射后目录命中应计 Allowed，got %v", vA)
	}

	// 别名映射后目录未命中：gpt-4 -> deepseek-chat，目录不含 → ModelMissing（禁映射后跳过比较）。
	fB := newFakeCatalog(map[int64][]string{1: {"other"}})
	sB := newCodeBuddyScheduler(fB)
	accB := cbAccount(1, 0, map[string]string{"gpt-4": "deepseek-chat"})
	vB := sB.evaluateCodeBuddyCatalogGate(context.Background(), OpenAIAccountScheduleRequest{Platform: PlatformDeepseek, RequestedModel: "gpt-4"}, []Account{accB})[1]
	if vB != codeBuddyCatalogModelMissing {
		t.Errorf("别名映射后目录未命中应计 ModelMissing，got %v", vB)
	}

	// 未配置映射的直名命中：请求 deepseek-chat，目录含 → Allowed。
	fC := newFakeCatalog(map[int64][]string{1: {"deepseek-chat"}})
	sC := newCodeBuddyScheduler(fC)
	accC := cbAccount(1, 0, nil)
	vC := sC.evaluateCodeBuddyCatalogGate(context.Background(), OpenAIAccountScheduleRequest{Platform: PlatformDeepseek, RequestedModel: "deepseek-chat"}, []Account{accC})[1]
	if vC != codeBuddyCatalogAllowed {
		t.Errorf("直名命中应计 Allowed，got %v", vC)
	}

	// 未配置映射的直名未命中：请求 deepseek-chat，目录不含 → ModelMissing。
	fD := newFakeCatalog(map[int64][]string{1: {"foo"}})
	sD := newCodeBuddyScheduler(fD)
	accD := cbAccount(1, 0, nil)
	vD := sD.evaluateCodeBuddyCatalogGate(context.Background(), OpenAIAccountScheduleRequest{Platform: PlatformDeepseek, RequestedModel: "deepseek-chat"}, []Account{accD})[1]
	if vD != codeBuddyCatalogModelMissing {
		t.Errorf("直名未命中应计 ModelMissing，got %v", vD)
	}
}

// TestCodebuddyAggregateGate_BudgetTop4Deterministic 覆盖 §2.1-1 查询集合固定性：
// N>4 时仅前 4 被探测，其余显式计 upstream_catalog_not_probed（非静默跳过）。
func TestCodebuddyAggregateGate_BudgetTop4Deterministic(t *testing.T) {
	accounts := make([]Account, 0, 6)
	catalogs := map[int64][]string{}
	for id := int64(1); id <= 6; id++ {
		accounts = append(accounts, cbAccount(id, 0, nil)) // 同优先级 → 按 ID 升序确定性排序
		catalogs[id] = []string{"m"}
	}
	f := newFakeCatalog(catalogs)
	s := newCodeBuddyScheduler(f)
	req := OpenAIAccountScheduleRequest{Platform: PlatformDeepseek, RequestedModel: "m"}
	verdicts := s.evaluateCodeBuddyCatalogGate(context.Background(), req, accounts)

	for id := int64(1); id <= 4; id++ {
		if v, ok := verdicts[id]; !ok || v != codeBuddyCatalogAllowed {
			t.Errorf("候选 %d 应被探测且 Allowed，got ok=%v v=%v", id, ok, v)
		}
	}
	for id := int64(5); id <= 6; id++ {
		if v, ok := verdicts[id]; !ok || v != codeBuddyCatalogNotProbed {
			t.Errorf("候选 %d 超出预算应计 NotProbed，got ok=%v v=%v", id, ok, v)
		}
	}
	// 实际探测集合必须恰好为排序前 4（ID 1,2,3,4）。
	got := f.probedIDs()
	want := []int64{1, 2, 3, 4}
	if strings.Join(idsStr(got), ",") != strings.Join(idsStr(want), ",") {
		t.Errorf("实际探测集合应固定为前4，got=%v want=%v", got, want)
	}
}

// TestCodebuddyAggregateGate_BudgetTieBreakByAccountID 覆盖 §2.1-1 并列优先级 tie-breaker：
// 优先级相同按账号 ID 升序唯一稳定排序，前 4 集合逐位确定。
func TestCodebuddyAggregateGate_BudgetTieBreakByAccountID(t *testing.T) {
	ids := []int64{30, 10, 20, 5, 15, 25} // 全部 Priority=0，按 ID 升序应为 5,10,15,20,25,30
	accounts := make([]Account, 0, len(ids))
	catalogs := map[int64][]string{}
	for _, id := range ids {
		accounts = append(accounts, cbAccount(id, 0, nil))
		catalogs[id] = []string{"m"}
	}
	f := newFakeCatalog(catalogs)
	s := newCodeBuddyScheduler(f)
	req := OpenAIAccountScheduleRequest{Platform: PlatformDeepseek, RequestedModel: "m"}
	verdicts := s.evaluateCodeBuddyCatalogGate(context.Background(), req, accounts)

	probed := f.probedIDs()
	wantProbed := []int64{5, 10, 15, 20}
	if strings.Join(idsStr(probed), ",") != strings.Join(idsStr(wantProbed), ",") {
		t.Errorf("并列优先级应按 ID 升序取前4，got=%v want=%v", probed, wantProbed)
	}
	for _, id := range []int64{5, 10, 15, 20} {
		if verdicts[id] != codeBuddyCatalogAllowed {
			t.Errorf("候选 %d 应 Allowed，got %v", id, verdicts[id])
		}
	}
	for _, id := range []int64{25, 30} {
		if verdicts[id] != codeBuddyCatalogNotProbed {
			t.Errorf("候选 %d 应 NotProbed，got %v", id, verdicts[id])
		}
	}
}

// TestCodebuddyAggregateGate_BudgetExactlyFourAllProbed 覆盖 §2.1-1：恰好 4 个候选全部探测，无 not_probed。
func TestCodebuddyAggregateGate_BudgetExactlyFourAllProbed(t *testing.T) {
	accounts := make([]Account, 0, 4)
	catalogs := map[int64][]string{}
	for id := int64(1); id <= 4; id++ {
		accounts = append(accounts, cbAccount(id, 0, nil))
		catalogs[id] = []string{"m"}
	}
	f := newFakeCatalog(catalogs)
	s := newCodeBuddyScheduler(f)
	req := OpenAIAccountScheduleRequest{Platform: PlatformDeepseek, RequestedModel: "m"}
	verdicts := s.evaluateCodeBuddyCatalogGate(context.Background(), req, accounts)

	for id := int64(1); id <= 4; id++ {
		if v, ok := verdicts[id]; !ok || v != codeBuddyCatalogAllowed {
			t.Errorf("候选 %d 应 Allowed，got ok=%v v=%v", id, ok, v)
		}
	}
	// 无 not_probed。
	for _, v := range verdicts {
		if v == codeBuddyCatalogNotProbed {
			t.Errorf("恰好4候选不应出现 NotProbed，verdicts=%v", verdicts)
		}
	}
	probed := f.probedIDs()
	want := []int64{1, 2, 3, 4}
	if strings.Join(idsStr(probed), ",") != strings.Join(idsStr(want), ",") {
		t.Errorf("恰好4应全探测，got=%v want=%v", probed, want)
	}
}

// TestCodebuddyAggregateGate_PlatformMismatchAllowOnAggregated 覆盖 §2.1-2 调度过滤放行：
// 聚合分组请求下放行 codebuddy 账号越过 platform_mismatch；非聚合平台不放行。
func TestCodebuddyAggregateGate_PlatformMismatchAllowOnAggregated(t *testing.T) {
	cb := &Account{ID: 1, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth}
	nonCB := &Account{ID: 2, Platform: PlatformDeepseek, Type: AccountTypeAPIKey}

	aggPlatforms := []string{PlatformDeepseek, PlatformZhipu, PlatformKimi, PlatformMiniMax, PlatformOther, PlatformCodeBuddy}
	for _, p := range aggPlatforms {
		req := OpenAIAccountScheduleRequest{Platform: p}
		if !codeBuddyPlatformMismatchAllowed(cb, req) {
			t.Errorf("聚合平台 %s 应放行 codebuddy 账号的 platform_mismatch", p)
		}
	}

	// 非聚合平台（openai）不放行 codebuddy 账号。
	if codeBuddyPlatformMismatchAllowed(cb, OpenAIAccountScheduleRequest{Platform: PlatformOpenAI}) {
		t.Errorf("非聚合平台 openai 不应放行 codebuddy 账号的 platform_mismatch")
	}
	// 非 codebuddy 账号（deepseek）即便在聚合平台也不走此放行。
	if codeBuddyPlatformMismatchAllowed(nonCB, OpenAIAccountScheduleRequest{Platform: PlatformDeepseek}) {
		t.Errorf("非 codebuddy 账号不应走聚合 platform_mismatch 放行")
	}
}

// TestCodebuddyAggregateGate_SnapshotBucketKeySyncAndVersionBump 覆盖 §2.1-3 快照层：
// bucket 键与读取路径同步聚合标签、版本 bump 失效、聚合分组分类。
func TestCodebuddyAggregateGate_SnapshotBucketKeySyncAndVersionBump(t *testing.T) {
	// 分类：聚合分组判定。
	if !isCodeBuddyAggregatedPlatform(PlatformDeepseek) ||
		!isCodeBuddyAggregatedPlatform(PlatformZhipu) ||
		!isCodeBuddyAggregatedPlatform(PlatformKimi) ||
		!isCodeBuddyAggregatedPlatform(PlatformMiniMax) ||
		!isCodeBuddyAggregatedPlatform(PlatformOther) ||
		!isCodeBuddyAggregatedPlatform(PlatformCodeBuddy) {
		t.Errorf("国产 OpenAI 兼容族 + codebuddy 均应判定为聚合分组")
	}
	if isCodeBuddyAggregatedPlatform(PlatformOpenAI) || isCodeBuddyAggregatedPlatform(PlatformGrok) {
		t.Errorf("openai/grok 不属于聚合分组")
	}

	// 读取路径（bucketFor）对聚合非 codebuddy 平台打 @agg 标签。
	gid := int64(42)
	bucket := (&SchedulerSnapshotService{}).bucketFor(&gid, PlatformDeepseek, SchedulerModeSingle)
	wantPlatform := PlatformDeepseek + "@agg" + strconv.Itoa(schedulerSnapshotAggregationVersion)
	if bucket.Platform != wantPlatform {
		t.Errorf("聚合平台 bucket 应带 @agg 标签，got %q want %q", bucket.Platform, wantPlatform)
	}
	if bucket.GroupID != gid {
		t.Errorf("bucket GroupID 应保留，got %d", bucket.GroupID)
	}

	// 标签仅影响缓存键，查询平台还原为真实平台名。
	if got := schedulerAggregationQueryPlatform(bucket.Platform); got != PlatformDeepseek {
		t.Errorf("schedulerAggregationQueryPlatform 应还原真实平台，got %q", got)
	}
	// codebuddy 平台自身不打标签。
	cbBucket := (&SchedulerSnapshotService{}).bucketFor(&gid, PlatformCodeBuddy, SchedulerModeSingle)
	if cbBucket.Platform != PlatformCodeBuddy {
		t.Errorf("codebuddy 平台自身不应打 @agg 标签，got %q", cbBucket.Platform)
	}
	if got := schedulerAggregationQueryPlatform(PlatformCodeBuddy); got != PlatformCodeBuddy {
		t.Errorf("codebuddy 平台 queryPlatform 应不变，got %q", got)
	}

	// 版本 bump 失效机制：聚合标签内嵌版本号，版本变更会使旧缓存键（无标签/旧版本）自然失效。
	// 当前键含版本后缀，与无标签旧键（纯平台名）不同，证明旧快照不会被继续命中。
	if !strings.HasSuffix(schedulerAggregationBucketPlatform(PlatformDeepseek), "@agg"+strconv.Itoa(schedulerSnapshotAggregationVersion)) {
		t.Errorf("聚合 bucket 平台键应内嵌版本标签")
	}
	if schedulerAggregationBucketPlatform(PlatformDeepseek) == PlatformDeepseek {
		t.Errorf("聚合平台 bucket 键必须与无标签旧键不同以触发失效")
	}

	// 非聚合平台键不变。
	if schedulerAggregationBucketPlatform(PlatformOpenAI) != PlatformOpenAI {
		t.Errorf("非聚合平台 bucket 键不应变更")
	}
}

func idsStr(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return out
}
