//go:build unit

package service

// 调用免费优先排序（方案 §3.5）单元测试：同平台 CN 子序列稳定分区。
//
// 覆盖：
//   - 同平台 CN 子序列 free 在前、桶内相对序稳定；
//   - 混合平台交错序列（Kira(non-free), Anthropic, Kira(free)）：两 Kira 槽位内交换，
//     Anthropic 槽位不动；
//   - 跨优先级互不影响，非 CN 账号位置与相对序均不变；
//   - unknown / exhausted / 仅付费有余（免费档已耗尽）均不算 free-remaining；
//   - 缺 Extra（无维度快照）入口 fail-open：无偏好、逐位不变；
//   - sticky 命中 free/non-free 两类账号均不重排；
//   - 各排序入口（fallback 兜底排序 / legacy 主选 / accountWithLoad 适配）消费同一分区原语。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---- 夹具 ----

func ffRecentRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func ffStaleRFC3339() string {
	return time.Now().Add(-(cnQuotaBalanceFreshnessThresholdMinutes + 5) * time.Minute).UTC().Format(time.RFC3339)
}

// ffKiraAccount 构造 Kira（kiraai.vn）上游账号。platform 传 PlatformKimi 走国产
// 供应商枚举，传 PlatformOther 走 base_url 事实源（两种都属 CN 额度账号族）。
func ffKiraAccount(id int64, platform string, priority int) *Account {
	return &Account{
		ID:          id,
		Platform:    platform,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    priority,
		Credentials: map[string]any{"api_key": "kira-test", "base_url": "https://kiraai.vn/api/v1"},
		Extra:       map[string]any{},
	}
}

// ffKiraFreeRemaining 新鲜且有剩余（used_percent<100）的免费池快照。
func ffKiraFreeRemaining(account *Account) *Account {
	account.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
		"used_percent": 10.0,
		"fetched_at":   ffRecentRFC3339(),
	}
	return account
}

// ffKiraFreeExhausted 新鲜且已耗尽（used_percent>=100）。
func ffKiraFreeExhausted(account *Account) *Account {
	account.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
		"used_percent": 100.0,
		"fetched_at":   ffRecentRFC3339(),
	}
	return account
}

// ffKiraFreeUnknown 快照过期 ⇒ 免费维度存在但 unknown（不是 remaining）。
func ffKiraFreeUnknown(account *Account) *Account {
	account.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
		"used_percent": 10.0,
		"fetched_at":   ffStaleRFC3339(),
	}
	return account
}

// ffTHPaidRemainingFreeExhausted kimi 平台 TH（tokenharbor.ai）号：付费维度
// confirmed remaining（spend_after_allowance=true + 钱包新鲜 >0），但免费维度
// confirmed exhausted（官方 free-tier 口径 exhausted=true、快照新鲜）。
//
// 新语义出处（用户 2026-10-10 §0.2 再裁定，见 account_quota_dimensions.go 文件头
// 与 resolveKiraQuotaDimensions 注释）：解析侧已删除「新鲜 VND≤0 ⇒ 免费维度
// servable=false」联动——ServableNo 常量与门内 ServableFalsified 分支保留，但当前
// 无生产者（维度解析一律 ServableUnknown）。故「servable=false ⇒ 不算 free-remaining」
// 这条旧测试前提在新语义下不可构造：VND≤0 的可服务证伪改由 Kira 确认探针 9 格判定表
// （evaluateKiraConfirmGrid）与响应式 402 权威冻结承担。
//
// 本夹具改以付费有余 + 免费档耗尽表达同一不变式的现存活形态：免费优先分区只认
// free 维度本身 confirmed remaining（hasConfirmedFreeAccountRemaining），付费
// 维度有余量不得把账号顶进 free 桶。
func ffTHPaidRemainingFreeExhausted(account *Account) *Account {
	account.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":              true,
		"spend_after_allowance": true,
		"exhausted":             true, // 官方口径：免费档已耗尽
		"plan_exhausted":        false,
		"fetched_at":            ffRecentRFC3339(),
	}
	account.Extra[TokenHarborWalletBalanceExtraKey] = 12.5
	account.Extra[TokenHarborWalletBalanceUpdatedAtExtraKey] = ffRecentRFC3339()
	return account
}

// ffNonCNAccount 非 CN 账号族（维度列表恒为空）：分区不得触碰其槽位。
func ffNonCNAccount(id int64, platform string, priority int) *Account {
	return &Account{
		ID:          id,
		Platform:    platform,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    priority,
		Extra:       map[string]any{},
	}
}

func ffIDs(accounts []*Account) []int64 {
	out := make([]int64, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, acc.ID)
	}
	return out
}

// ---- 纯分区原语 ----

func TestPartitionFreeRemainingFirst_SameCNPlatformFreeFirstAndStable(t *testing.T) {
	// 基准序列：non-free, free, non-free, free（同平台 kimi、同优先级）
	accounts := []*Account{
		ffKiraFreeExhausted(ffKiraAccount(1, PlatformKimi, 0)),
		ffKiraFreeRemaining(ffKiraAccount(2, PlatformKimi, 0)),
		ffKiraFreeExhausted(ffKiraAccount(3, PlatformKimi, 0)),
		ffKiraFreeRemaining(ffKiraAccount(4, PlatformKimi, 0)),
	}

	partitionFreeRemainingFirst(accounts)

	// free 桶 [2,4] 在前，rest 桶 [1,3] 在后，桶内基准相对序不变。
	require.Equal(t, []int64{2, 4, 1, 3}, ffIDs(accounts))
}

func TestPartitionFreeRemainingFirst_MixedPlatformInterleavedKeepsNonCNSlots(t *testing.T) {
	// Kira(non-free), Anthropic, Kira(free) —— 交错序列
	kiraNonFree := ffKiraFreeExhausted(ffKiraAccount(1, PlatformKimi, 0))
	anthropic := ffNonCNAccount(2, PlatformAnthropic, 0)
	kiraFree := ffKiraFreeRemaining(ffKiraAccount(3, PlatformKimi, 0))
	accounts := []*Account{kiraNonFree, anthropic, kiraFree}

	partitionFreeRemainingFirst(accounts)

	// 两个 Kira 槽位（索引 0 与 2）内交换；Anthropic 槽位（索引 1）与账号不动。
	require.Equal(t, []int64{3, 2, 1}, ffIDs(accounts))
	require.Same(t, anthropic, accounts[1], "非 CN 账号不得被移出其槽位")
}

func TestPartitionFreeRemainingFirst_KiraOnNonCNPlatformEnum(t *testing.T) {
	// Kira 账号 platform 常存为 other（非国产供应商枚举），仍须按 base_url 判定进槽。
	accounts := []*Account{
		ffKiraFreeExhausted(ffKiraAccount(1, PlatformOther, 0)),
		ffKiraFreeRemaining(ffKiraAccount(2, PlatformOther, 0)),
	}
	partitionFreeRemainingFirst(accounts)
	require.Equal(t, []int64{2, 1}, ffIDs(accounts))
}

func TestPartitionFreeRemainingFirst_CrossPriorityIndependent(t *testing.T) {
	accounts := []*Account{
		ffNonCNAccount(5, PlatformOpenAI, 0), // 非 CN，优先级 0
		ffKiraFreeExhausted(ffKiraAccount(1, PlatformKimi, 0)),
		ffKiraFreeRemaining(ffKiraAccount(2, PlatformKimi, 0)),
		ffNonCNAccount(6, PlatformAnthropic, 1), // 非 CN，优先级 1
		ffKiraFreeExhausted(ffKiraAccount(3, PlatformKimi, 1)),
		ffKiraFreeRemaining(ffKiraAccount(4, PlatformKimi, 1)),
	}

	partitionFreeRemainingFirst(accounts)

	// 各优先级独立分区；非 CN 账号（5/6）位置与相对序均不变。
	require.Equal(t, []int64{5, 2, 1, 6, 4, 3}, ffIDs(accounts))
}

func TestPartitionFreeRemainingFirst_UnknownExhaustedPaidRemainingNotFree(t *testing.T) {
	accounts := []*Account{
		ffKiraFreeUnknown(ffKiraAccount(1, PlatformKimi, 0)),
		ffKiraFreeExhausted(ffKiraAccount(2, PlatformKimi, 0)),
		ffTHPaidRemainingFreeExhausted(ffKiraAccount(3, PlatformKimi, 0)),
		ffKiraFreeRemaining(ffKiraAccount(4, PlatformKimi, 0)),
	}

	partitionFreeRemainingFirst(accounts)

	// 只有 free 维度 confirmed remaining 的 4 号进 free 桶，其余按基准相对序留在后桶。
	// 3 号付费维度有余量（spend_after_allowance + 钱包新鲜 >0），但免费档已耗尽 ⇒
	// 不进 free 桶（§0.2 新语义：付费余量不参与 free-remaining 判定）。
	require.Equal(t, []int64{4, 1, 2, 3}, ffIDs(accounts))
}

func TestPartitionFreeRemainingFirst_MissingExtraFailOpen(t *testing.T) {
	t.Run("extra nil", func(t *testing.T) {
		accounts := []*Account{
			ffKiraAccount(1, PlatformKimi, 0),
			ffKiraAccount(2, PlatformKimi, 0),
			ffKiraAccount(3, PlatformKimi, 0),
		}
		accounts[0].Extra = nil
		accounts[1].Extra = nil
		accounts[2].Extra = nil
		partitionFreeRemainingFirst(accounts)
		require.Equal(t, []int64{1, 2, 3}, ffIDs(accounts), "缺 Extra ⇒ 维度为空 ⇒ 无偏好")
	})

	t.Run("extra 无 CN 额度键（快照裁剪形态）", func(t *testing.T) {
		accounts := []*Account{
			ffKiraAccount(1, PlatformKimi, 0),
			ffKiraAccount(2, PlatformKimi, 0),
		}
		accounts[0].Extra = map[string]any{"codex_5h_used_percent": 50.0}
		accounts[1].Extra = map[string]any{"codex_5h_used_percent": 50.0}
		partitionFreeRemainingFirst(accounts)
		require.Equal(t, []int64{1, 2}, ffIDs(accounts))
	})

	t.Run("序列长度不足与空输入", func(t *testing.T) {
		single := []*Account{ffKiraFreeRemaining(ffKiraAccount(1, PlatformKimi, 0))}
		partitionFreeRemainingFirst(single)
		require.Equal(t, []int64{1}, ffIDs(single))
		partitionFreeRemainingFirst(nil) // 不得 panic
	})
}

func TestPartitionFreeRemainingFirstWithLoadKeepsLoadPairing(t *testing.T) {
	free := ffKiraFreeRemaining(ffKiraAccount(1, PlatformKimi, 0))
	nonFree := ffKiraFreeExhausted(ffKiraAccount(2, PlatformKimi, 0))
	other := ffNonCNAccount(3, PlatformAnthropic, 0)
	items := []accountWithLoad{
		{account: nonFree, loadInfo: &AccountLoadInfo{AccountID: nonFree.ID, LoadRate: 10}},
		{account: other, loadInfo: &AccountLoadInfo{AccountID: other.ID, LoadRate: 20}},
		{account: free, loadInfo: &AccountLoadInfo{AccountID: free.ID, LoadRate: 30}},
	}

	partitionFreeRemainingFirstWithLoad(items)

	require.Equal(t, []int64{1, 3, 2}, []int64{items[0].account.ID, items[1].account.ID, items[2].account.ID})
	// 负载信息必须与账号严格同槽移动（不得串槽）。
	require.Equal(t, 30, items[0].loadInfo.LoadRate)
	require.Equal(t, 20, items[1].loadInfo.LoadRate)
	require.Equal(t, 10, items[2].loadInfo.LoadRate)
}

// ---- 排序入口消费 ----

func TestFreeFirstSortCandidatesForFallbackRandomAndLastUsedModes(t *testing.T) {
	newAccounts := func() []*Account {
		return []*Account{
			ffKiraFreeExhausted(ffKiraAccount(1, PlatformKimi, 0)),
			ffKiraFreeRemaining(ffKiraAccount(2, PlatformKimi, 0)),
		}
	}
	svc := &GatewayService{}

	// mode=random：sortAccountsByPriorityOnly + shuffleWithinPriority 之后分区
	random := newAccounts()
	svc.sortCandidatesForFallback(random, false, "random")
	require.Equal(t, []int64{2, 1}, ffIDs(random), "random 模式下 shuffle 之后 free 仍在前")

	// mode=last_used（默认）：sortAccountsByPriorityAndLastUsed 末端分区
	lastUsed := newAccounts()
	svc.sortCandidatesForFallback(lastUsed, false, "last_used")
	require.Equal(t, []int64{2, 1}, ffIDs(lastUsed))

	// 同组（Priority+LastUsedAt）内 shuffle 多次后，free 分桶仍成立
	for i := 0; i < 50; i++ {
		accounts := newAccounts()
		svc.sortCandidatesForFallback(accounts, false, "random")
		require.Equal(t, []int64{2, 1}, ffIDs(accounts), "shuffle 后的分区必须稳定")
	}
}

func TestFreeFirstSortAccountsByPriorityAndLastUsedLegacyOrder(t *testing.T) {
	// 三者同优先级且 LastUsedAt 同为 nil ⇒ 同组内 shuffle 会打散绝对位置，
	// 断言只能取「CN 子序列内 free 在 non-free 之前」这一不变式。
	//
	// 1 号取 TH 付费有余 + 免费档耗尽账号（旧夹具为「VND=0 ⇒ servable=false」账号，
	// 该前提已随用户 2026-10-10 §0.2 再裁定作废：解析侧不再产出 servable=false，
	// 该账号在新语义下本身就是 free-remaining，无法作为 non-free 对照）。
	for i := 0; i < 50; i++ {
		accounts := []*Account{
			ffTHPaidRemainingFreeExhausted(ffKiraAccount(1, PlatformKimi, 0)),
			ffKiraFreeRemaining(ffKiraAccount(2, PlatformKimi, 0)),
			ffNonCNAccount(3, PlatformAnthropic, 0),
		}
		sortAccountsByPriorityAndLastUsed(accounts, false)
		ids := ffIDs(accounts)
		indexOf := func(id int64) int {
			for i, got := range ids {
				if got == id {
					return i
				}
			}
			t.Fatalf("account %d missing from %v", id, ids)
			return -1
		}
		require.Less(t, indexOf(2), indexOf(1), "free 必须排在 non-free 之前: %v", ids)
	}
}

func TestFreeFirstSelectBestAccountPrefersCNFreeCandidate(t *testing.T) {
	nonFree := ffKiraFreeExhausted(ffKiraAccount(1, PlatformKimi, 0))
	free := ffKiraFreeRemaining(ffKiraAccount(2, PlatformKimi, 0))
	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{*nonFree, *free}},
	}

	// 基准排序（优先级相同、LastUsedAt 相同）下 1 号原本在前；分区后 free 优先。
	selected, _, _ := svc.selectBestAccount(context.Background(), nil, PlatformKimi,
		[]Account{*nonFree, *free}, "", nil, false, "", false)
	require.NotNil(t, selected)
	require.Equal(t, free.ID, selected.ID)
}

func TestFreeFirstStickyHitNotReorderedForFreeAndNonFree(t *testing.T) {
	nonFree := ffKiraFreeExhausted(ffKiraAccount(1, PlatformKimi, 0))
	free := ffKiraFreeRemaining(ffKiraAccount(2, PlatformKimi, 0))
	repo := stubOpenAIAccountRepo{accounts: []Account{*nonFree, *free}}

	// 粘性绑定按服务派生的会话缓存键落键（openAISessionCacheKey）。
	stickySvc := func(boundAccountID int64) *OpenAIGatewayService {
		cache := &stubGatewayCache{sessionBindings: map[string]int64{}}
		svc := &OpenAIGatewayService{accountRepo: repo, cache: cache}
		cache.sessionBindings[svc.openAISessionCacheKey("sess")] = boundAccountID
		return svc
	}

	t.Run("sticky 命中 non-free 账号不被重排", func(t *testing.T) {
		svc := stickySvc(nonFree.ID)
		selected, err := svc.selectAccountForModelWithExclusions(context.Background(), nil,
			PlatformKimi, "sess", "", nil, false, 0, "", false)
		require.NoError(t, err)
		require.NotNil(t, selected)
		require.Equal(t, nonFree.ID, selected.ID, "sticky 命中优先于本排序，不得被 free 账号挤掉")
	})

	t.Run("sticky 命中 free 账号保持原样", func(t *testing.T) {
		svc := stickySvc(free.ID)
		selected, err := svc.selectAccountForModelWithExclusions(context.Background(), nil,
			PlatformKimi, "sess", "", nil, false, 0, "", false)
		require.NoError(t, err)
		require.NotNil(t, selected)
		require.Equal(t, free.ID, selected.ID)
	})

	t.Run("非 sticky 路径仍走免费优先分区", func(t *testing.T) {
		svc := &OpenAIGatewayService{accountRepo: repo}
		selected, err := svc.selectAccountForModelWithExclusions(context.Background(), nil,
			PlatformKimi, "", "", nil, false, 0, "", false)
		require.NoError(t, err)
		require.NotNil(t, selected)
		require.Equal(t, free.ID, selected.ID)
	})
}

// ---- 检查单 #2：compact 重排不得推翻免费优先（三处排序入口统一为「分区是最终排序步骤」）----
//
// 背景（终审 #2）：openai_gateway_scheduling.go 三处排序原先先分区后 prioritizeOpenAI
// CompactAccounts / tier 重排，compact 能把非 free 的 compact 账号顶到 free 账号之前。
// 修复后：三路径统一「先既有排序（含 compact），后 partitionFreeRemainingFirst(WithLoad)」。
//
// 夹具构造口径：账号 platform 取 openai（IsOpenAI() 成立，compact tier 可判定），
// base_url 指向 kiraai.vn（CN 额度账号族，参与免费优先分区）。

// ffKiraOpenAICompactAccount Kira 上游 + openai 平台账号：既进 CN 免费优先分区槽位，
// 又能走 compact tier 判定。compact 传 nil ⇒ tier=1（未探测）；true ⇒ tier=2（已知支持）；
// false ⇒ tier=0（明确不支持，requireCompact 时会被过滤出候选）。
func ffKiraOpenAICompactAccount(id int64, compact *bool) *Account {
	account := &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Credentials: map[string]any{"api_key": "kira-test", "base_url": "https://kiraai.vn/api/v1"},
		Extra:       map[string]any{},
	}
	if compact != nil {
		account.Extra["openai_compact_supported"] = *compact
	}
	return account
}

// ffCompactFreeFixture 两个同优先级候选：
//   - free（id=1）：免费池有余 ⇒ free 桶；compact tier=1（未知）；
//   - compactNonFree（id=2）：免费池已耗尽 ⇒ rest 桶；compact tier=2（已知支持）。
//
// requireCompact=true 时（修复前）compact 重排会把 compactNonFree 顶到最前。
func ffCompactFreeFixture() (free, compactNonFree *Account) {
	free = ffKiraFreeRemaining(ffKiraOpenAICompactAccount(1, nil))
	compactNonFree = ffKiraFreeExhausted(ffKiraOpenAICompactAccount(2, compactSupportedTrue()))
	return free, compactNonFree
}

// compactSupportedTrue 返回 openai_compact_supported=true 指针（tier=2）。
func compactSupportedTrue() *bool { v := true; return &v }

// ffNewCompactLoadAwareService 构造走负载感知路径（LoadBatchEnabled + 并发服务）的
// OpenAIGatewayService：三条排序路径都可由并发缓存注入切换。
func ffNewCompactLoadAwareService(cache schedulerTestConcurrencyCache, accounts ...*Account) *OpenAIGatewayService {
	list := make([]Account, 0, len(accounts))
	for _, acc := range accounts {
		list = append(list, *acc)
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: list},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(cache),
	}
}

func ffSelectLoadAware(t *testing.T, cache schedulerTestConcurrencyCache, requireCompact bool, accounts ...*Account) (*AccountSelectionResult, error) {
	t.Helper()
	svc := ffNewCompactLoadAwareService(cache, accounts...)
	groupID := int64(94001)
	return svc.selectAccountWithLoadAwareness(context.Background(), &groupID, PlatformOpenAI, "", "",
		nil, requireCompact, "", false)
}

// TestFreeFirstPartitionIsFinalStepLoadMapPath Layer 2 tryAcquireFromLoadMap 路径
// （opengw:1579 区段）：requireCompact=true 时 compact 重排之后仍必须执行免费优先分区
// ⇒ free 账号整体在 compact 账号之前（不可被 compact 推翻）。
func TestFreeFirstPartitionIsFinalStepLoadMapPath(t *testing.T) {
	free, compactNonFree := ffCompactFreeFixture()

	t.Run("requireCompact=true keeps free first", func(t *testing.T) {
		selection, err := ffSelectLoadAware(t, schedulerTestConcurrencyCache{}, true, free, compactNonFree)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, free.ID, selection.Account.ID,
			"requireCompact 的 tier 重排不得推翻免费优先（分区是最终排序步骤）")
	})

	t.Run("requireCompact=false regression keeps free first", func(t *testing.T) {
		selection, err := ffSelectLoadAware(t, schedulerTestConcurrencyCache{}, false, free, compactNonFree)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, free.ID, selection.Account.ID, "非 compact 路径回归既有免费优先")
	})
}

// TestFreeFirstPartitionIsFinalStepLoadBatchFailurePath 负载批量失败回退路径
// （opengw:1635 区段）：同样先 compact 后排分区，free 在前。
func TestFreeFirstPartitionIsFinalStepLoadBatchFailurePath(t *testing.T) {
	free, compactNonFree := ffCompactFreeFixture()
	cache := schedulerTestConcurrencyCache{loadBatchErr: errors.New("load batch unavailable")}

	t.Run("requireCompact=true keeps free first", func(t *testing.T) {
		selection, err := ffSelectLoadAware(t, cache, true, free, compactNonFree)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, free.ID, selection.Account.ID,
			"负载批量失败回退序列：分区必须在 compact 之后（最终排序步骤）")
	})

	t.Run("requireCompact=false regression keeps free first", func(t *testing.T) {
		selection, err := ffSelectLoadAware(t, cache, false, free, compactNonFree)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, free.ID, selection.Account.ID)
	})
}

// TestFreeFirstPartitionIsFinalStepFallbackWaitPath Layer 3 兜底排队路径
// （opengw:1685 区段）：抢槽全失败后按最终序返回 WaitPlan，free 在前。
func TestFreeFirstPartitionIsFinalStepFallbackWaitPath(t *testing.T) {
	free, compactNonFree := ffCompactFreeFixture()
	// 两个账号都抢不到槽 ⇒ 落到 Layer 3 兜底排队（返回 WaitPlan 而非 402/无账号）。
	cache := schedulerTestConcurrencyCache{acquireResults: map[int64]bool{free.ID: false, compactNonFree.ID: false}}

	t.Run("requireCompact=true keeps free first", func(t *testing.T) {
		selection, err := ffSelectLoadAware(t, cache, true, free, compactNonFree)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, free.ID, selection.Account.ID,
			"兜底排队序列：分区必须是最终排序步骤（compact 不得把非 free 账号顶到队首）")
		require.NotNil(t, selection.WaitPlan, "兜底路径必须返回 WaitPlan")
	})

	t.Run("requireCompact=false regression keeps free first", func(t *testing.T) {
		selection, err := ffSelectLoadAware(t, cache, false, free, compactNonFree)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, free.ID, selection.Account.ID)
	})
}
