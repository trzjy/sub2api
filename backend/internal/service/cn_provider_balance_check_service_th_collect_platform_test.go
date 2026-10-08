package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---- DISPATCH T5：kimi 平台 TH（tokenharbor.ai）号接入周期快照刷新链 ----
//
// 复现生产场景：14 个 TH 号 platform 挂在 kimi 下，而 collect 闭包仅遍历
// platforms()={kimi,deepseek}（openai 平台由专项分支单独收集）。修复前这些号
// 漏过 TH 判定、掉进 kimi payg 探测链（误耗 Pass token 且永不刷新快照）。
// 修复：collect 闭包内 Kira 判定之后插入 TH base_url 判定，路由到 thTargets。

// buildKimTHEnv 构造单 kimi 平台 TH 账号（base_url 指向 tokenharbor.ai）的 runOnce 环境。
// 返回 fake TH、上游桩、repo 与未执行 runOnce 的 svc，由调用方决定 runOnce 时机
//（测试 2 需以 require.NotPanics 包裹，验证账号不进 paygTargets）。
//   - platform：账号 platform（默认传入 PlatformKimi 复现生产场景）；
//   - extra：账号快照/退避态（同时注入 ListByPlatform 与 byID，供收集门判定与
//     thTargets 探测前复核复用）；
//   - cfg：服务配置（零值 → 年龄门关闭，始终收集；thKiraGateConfig → 间隔生效）。
func buildKimTHEnv(t *testing.T, platform string, extra map[string]any, cfg *config.Config) (*tokenHarborFakeTH, *tokenHarborFakeUpstream, *cnRunOnceExtraRepo, *CNProviderBalanceCheckService) {
	t.Helper()
	fakeTH := newTokenHarborFakeTH(t, false)
	now := time.Now().UTC()
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()
	thUpstream := &tokenHarborFakeUpstream{server: fakeTH.server}

	acc := tokenHarborTestAccount(621)
	acc.Platform = platform
	acc.Status = StatusActive
	acc.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	if extra != nil {
		acc.Extra = extra
	} else {
		acc.Extra = map[string]any{}
	}

	repo := &cnRunOnceExtraRepo{
		byPlatform: map[string][]Account{platform: {*acc}},
		byID:       map[int64]*Account{621: acc},
	}
	thSvc := NewTokenHarborPassService(repo, nil, thUpstream)
	thSvc.baseURL = fakeTH.server.URL
	thSvc.now = func() time.Time { return now }

	svc := &CNProviderBalanceCheckService{accountRepo: repo, httpUpstream: thUpstream, cfg: cfg}
	svc.SetTokenHarborPassService(thSvc)
	return fakeTH, thUpstream, repo, svc
}

// 测试 1：kimi 平台 + TH base_url 的 active 账号 → refreshTokenHarborAccount 被调用
//（th_pass_snapshot / th_usage_snapshot 双落库路径触发，convergeTHStockRecovery 接线点
// 由既有 Lifecycle 测试覆盖，本测试聚焦 collect 路由正确性）。
func TestCNBalanceCheckRunOnce_KimiPlatformTHRefreshesSnapshot(t *testing.T) {
	fakeTH, _, repo, svc := buildKimTHEnv(t, PlatformKimi, nil, &config.Config{})
	svc.runOnce()

	loginPosts, billingHits := fakeTH.stats()
	require.Equal(t, 1, loginPosts, "kimi 平台 TH 号必须进 thTargets 并被刷新（login=1）")
	require.Equal(t, 1, billingHits, "kimi 平台 TH 号必须进 thTargets 并被刷新（billing=1）")
	require.Equal(t, 1, fakeTH.usageCSVHits, "kimi 平台 TH 号必须进 thTargets 并被刷新（usage=1）")
	require.Len(t, repo.extraWrites, 2, "th_pass_snapshot + th_usage_snapshot 双落库")
	keys := map[string]bool{}
	for _, w := range repo.extraWrites {
		for k := range w {
			keys[k] = true
		}
	}
	require.True(t, keys[TokenHarborPassSnapshotExtraKey], "必须落 th_pass_snapshot")
	require.True(t, keys[TokenHarborUsageSnapshotExtraKey], "必须落 th_usage_snapshot")
}

// 测试 2：同一账号不被 checkOne 探测（不进 paygTargets）。
// 故意不注入 balanceService：kimi payg 路径 checkOne → s.balanceService.QueryBalance
// 解引用 nil 必 panic；账号正确路由到 thTargets 时 payg 循环为空、绝不会触达 checkOne。
// （kimi 余额端点固定 api.moonshot.cn 与 base_url 无关，本测试零外部网络依赖。）
func TestCNBalanceCheckRunOnce_KimiPlatformTHNotProbedByPayg(t *testing.T) {
	fakeTH, _, _, svc := buildKimTHEnv(t, PlatformKimi, nil, &config.Config{})
	require.NotPanics(t, func() { svc.runOnce() },
		"kimi 平台 TH 号必须走 thTargets，不得进 paygTargets（否则 checkOne nil balanceService panic）")
	// 仍走 TH 刷新路径（确认不是「既不刷新也不探测」的空转，与测试 1 同口径）。
	require.Equal(t, 1, fakeTH.usageCSVHits, "TH 刷新仍发生（账号确实进 thTargets）")
}

// 测试 3：年龄门命中（th_usage_snapshot 新鲜）时 kimi TH 号跳过收集（与 openai 专项
// 分支同口径：收集期年龄门在 append 进 thTargets 之前生效）。
func TestCNBalanceCheckRunOnce_KimiPlatformTHAgeGateSkipsCollect(t *testing.T) {
	now := time.Now().UTC()
	extra := map[string]any{
		TokenHarborUsageSnapshotExtraKey: TokenHarborUsageSnapshot{FetchedAt: now.Add(-5 * time.Minute)},
	}
	fakeTH, _, repo, svc := buildKimTHEnv(t, PlatformKimi, extra, thKiraGateConfig())
	svc.runOnce()

	require.Equal(t, 0, fakeTH.usageCSVHits, "年龄门命中（th_usage_snapshot 新鲜）时 kimi TH 号必须跳过收集（usage 命中=0）")
	loginPosts, _ := fakeTH.stats()
	require.Equal(t, 0, loginPosts, "年龄门命中时不得发起登录探测（login=0）")
	require.Len(t, repo.extraWrites, 0, "年龄门命中时不落任何 TH 快照")
}

// 测试 4（回归）：openai 平台 TH 号仍由专项分支（ListByPlatform(PlatformOpenAI)）收集，
// 且因 platforms() 不含 openai，collect 闭包不会重复收集（注入 openai 专项分支既用例
// TestCNProviderBalanceCheckRunOnce_TokenHarbor* 由白名单统一回归，本测试聚焦「不重叠」）。
func TestCNBalanceCheckRunOnce_OpenAIPlatformTHCollectedAndNotDoubleCounted(t *testing.T) {
	// 同一账号同时挂在 openai（专项分支）与 kimi（collect 闭包遍历）：生产不存在此重叠，
	// 此处仅验证 collect 闭包对 openai 平台账号「不」二次收口——openai 账号仅专项分支收。
	fakeTH := newTokenHarborFakeTH(t, false)
	now := time.Now().UTC()
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()
	thUpstream := &tokenHarborFakeUpstream{server: fakeTH.server}

	// openai 平台 TH 号（专项分支入口）。
	openAITh := tokenHarborTestAccount(621)
	openAITh.Platform = PlatformOpenAI
	openAITh.Status = StatusActive
	openAITh.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	openAITh.Extra = map[string]any{}

	repo := &cnRunOnceExtraRepo{
		byPlatform: map[string][]Account{PlatformOpenAI: {*openAITh}},
		byID:       map[int64]*Account{621: openAITh},
	}
	thSvc := NewTokenHarborPassService(repo, nil, thUpstream)
	thSvc.baseURL = fakeTH.server.URL
	thSvc.now = func() time.Time { return now }

	svc := &CNProviderBalanceCheckService{accountRepo: repo, httpUpstream: thUpstream, cfg: &config.Config{}}
	svc.SetTokenHarborPassService(thSvc)
	svc.runOnce()

	// openai 专项分支收集一次（usage 命中=1），collect 闭包不遍历 openai 故无二次。
	require.Equal(t, 1, fakeTH.usageCSVHits, "openai 平台 TH 号由专项分支收集一次")
}

// ---- 外审 R1 发现项 1 定向补测：首刷路径不被双门误挡 ----
// 外审阻断点：必须证明「快照缺失」「损坏退避键」「有效退避」三态在 kimi 平台
// TH 号上的收集/跳过语义与方案 §3 消费者闭包一致，否则"快照永不刷新"核心缺口
// 可能因门误挡而未闭合。

// 测试 5：快照缺失（Extra 无任何 TH 快照键）→ 年龄门不生效（ok=false 不过新），
// 必须收集首刷。生产映射：现有 14 号快照停更，部署后首轮周期即走本路径。
func TestCNBalanceCheckRunOnce_KimiPlatformTHMissingSnapshotFirstCollect(t *testing.T) {
	fakeTH, _, _, svc := buildKimTHEnv(t, PlatformKimi, nil, thKiraGateConfig())
	// extra 为空 map：无 th_usage_snapshot → TokenHarborUsageSnapshotFromExtra ok=false
	// → 年龄门不跳过；无退避键 → 退避门不跳过。间隔配置 60min 生效也不得挡首刷。
	svc.runOnce()
	require.Equal(t, 1, fakeTH.usageCSVHits, "快照缺失时年龄门必须放行首刷（usage=1）")
}

// 测试 6：损坏退避键（th_probe_backoff_until 非正整数）→ 失败关闭跳过 + 告警
//（方案 §9.2 用户裁定既有语义，kimi 平台同样生效，无 TH 专属兜底）。
func TestCNBalanceCheckRunOnce_KimiPlatformTHCorruptedBackoffSkips(t *testing.T) {
	extra := map[string]any{thProbeBackoffUntilExtraKey: "not-a-number"}
	fakeTH, _, _, svc := buildKimTHEnv(t, PlatformKimi, extra, thKiraGateConfig())
	svc.runOnce()
	require.Equal(t, 0, fakeTH.usageCSVHits, "损坏退避键必须失败关闭跳过（usage=0）")
}

// 测试 7：有效未来退避截止 → 跳过（真实退避语义，非损坏误挡）。
func TestCNBalanceCheckRunOnce_KimiPlatformTHActiveBackoffSkips(t *testing.T) {
	extra := map[string]any{thProbeBackoffUntilExtraKey: time.Now().Add(10 * time.Minute).Unix()}
	fakeTH, _, _, svc := buildKimTHEnv(t, PlatformKimi, extra, thKiraGateConfig())
	svc.runOnce()
	require.Equal(t, 0, fakeTH.usageCSVHits, "有效退避期内必须跳过收集（usage=0）")
}
