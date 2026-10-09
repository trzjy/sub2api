package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---- 派发单 C4 / R19-F5：F5 快照收集豁免（额度生命周期归属账号不停刷）----
//
// 挂载点 = C0 定位结论 cn_provider_balance_check_service.go:205（IsActive 收集闸门）。
// F1 冻结（SetTempUnschedulable）不翻 status，现状已被收集（平台分支不查 Schedulable）
// ——正例 2 锁定该语义防回归；F3 暂停（status=error + schedulable=false + 持
// http_403_recovery 记录）是唯一需要豁免的类——正例 1。
// 负例 1/3（error 无 F3 键）与负例 2（手工停调 payg）证明豁免未过宽。

// f5HTTP403RecoveryExtra 构造持 F3 恢复记录的账号 Extra（http_403_recovery 键，
// 键内含 until，镜像 http403_recovery.go 的 http403RecoveryRecord 形态）。
func f5HTTP403RecoveryExtra(until time.Time) map[string]any {
	return map[string]any{
		http403RecoveryExtraKey: map[string]any{
			"until":          until.UTC().Format(time.RFC3339),
			"generation":     int64(7),
			"reason":         "f3 content policy",
			"owner":          http403RecoveryOwnerF3,
			"state_revision": int64(1),
		},
	}
}

// f5THCollectEnv 构造单 kimi 平台 TH 账号的 runOnce 环境（镜像
// buildKimTHEnv，额外开放 status / schedulable / temp_unschedulable 覆盖）。
func f5THCollectEnv(t *testing.T, status string, schedulable bool, tempUntil *time.Time, extra map[string]any) (*tokenHarborFakeTH, *CNProviderBalanceCheckService) {
	t.Helper()
	fakeTH := newTokenHarborFakeTH(t, false)
	now := time.Now().UTC()
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()
	thUpstream := &tokenHarborFakeUpstream{server: fakeTH.server}

	acc := tokenHarborTestAccount(881)
	acc.Platform = PlatformKimi
	acc.Status = status
	acc.Schedulable = schedulable
	acc.TempUnschedulableUntil = tempUntil
	acc.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	if extra != nil {
		acc.Extra = extra
	} else {
		acc.Extra = map[string]any{}
	}

	repo := &cnRunOnceExtraRepo{
		byPlatform: map[string][]Account{PlatformKimi: {*acc}},
		byID:       map[int64]*Account{881: acc},
	}
	thSvc := NewTokenHarborPassService(repo, nil, thUpstream)
	thSvc.baseURL = fakeTH.server.URL
	thSvc.now = func() time.Time { return now }

	svc := &CNProviderBalanceCheckService{accountRepo: repo, httpUpstream: thUpstream, cfg: &config.Config{}}
	svc.SetTokenHarborPassService(thSvc)
	return fakeTH, svc
}

// 正例 1（F3 豁免）：status=error + schedulable=false + extra 持 http_403_recovery
// 键的 TH 账号 → 通过 :205 → 进 thTargets → refreshTokenHarborAccount 被调
// （usage CSV 快照刷新命中）。
func TestCNBalanceCheckF5Exempt_F3ErrorTHAccountCollected(t *testing.T) {
	extra := f5HTTP403RecoveryExtra(time.Now().Add(time.Hour))
	fakeTH, svc := f5THCollectEnv(t, StatusError, false, nil, extra)

	svc.runOnce()

	require.Equal(t, 1, fakeTH.usageCSVHits,
		"持 F3 恢复记录的 error TH 账号必须豁免 :205 并被收集（快照刷新）")
}

// 正例 2（F1 类回归，C0 §1.4 用例 1）：status=active + TempUnschedulableUntil=未来
// 的 TH 账号 → 仍被收集（冻结不阻断快照刷新，现状语义锁定防回归）。
func TestCNBalanceCheckF5Exempt_F1FrozenTHAccountStillCollected(t *testing.T) {
	future := time.Now().Add(time.Hour)
	fakeTH, svc := f5THCollectEnv(t, StatusActive, true, &future, nil)

	svc.runOnce()

	require.Equal(t, 1, fakeTH.usageCSVHits,
		"F1 冻结（temp_unschedulable 未翻 status）的 TH 账号必须仍被收集")
}

// 负例 1 + 负例 3（401 类 / transport-breaker 语义同 error 无键）：status=error 且
// 不持 http_403_recovery 键的 TH 账号 → 仍被 :205 排除，不进 thTargets。
// 两账号分列以显式覆盖两类"非 F3 error"来源（401/breaker 与 transport 均表现为
// status=error 且无 F3 键），报告登记合并表述但覆盖两类。
func TestCNBalanceCheckF5Exempt_ErrorAccountWithoutF3KeyExcluded(t *testing.T) {
	// 负例 1（401 类）与负例 3（transport/breaker 类）：均 status=error、无 F3 键。
	// 单账号构造即代表该等价类（无 F3 键的 error 账号）；此处覆盖 2 个独立 ID 以证明
	// 非 F3 error 账号（无论其 error 来源）一律不豁免。
	now := time.Now().UTC()
	fakeTH := newTokenHarborFakeTH(t, false)
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()
	thUpstream := &tokenHarborFakeUpstream{server: fakeTH.server}

	build := func(id int64) Account {
		acc := tokenHarborTestAccount(id)
		acc.Platform = PlatformKimi
		acc.Status = StatusError
		acc.Schedulable = false
		acc.Credentials["base_url"] = "https://tokenharbor.ai/v1"
		acc.Extra = map[string]any{}
		return *acc
	}
	unauthorized := build(882) // 负例 1：401 类 error，无 F3 键
	transport := build(883)    // 负例 3：transport/breaker 类 error，无 F3 键

	repo := &cnRunOnceExtraRepo{
		byPlatform: map[string][]Account{PlatformKimi: {unauthorized, transport}},
		byID:       map[int64]*Account{882: &unauthorized, 883: &transport},
	}
	thSvc := NewTokenHarborPassService(repo, nil, thUpstream)
	thSvc.baseURL = fakeTH.server.URL
	thSvc.now = func() time.Time { return now }

	svc := &CNProviderBalanceCheckService{accountRepo: repo, httpUpstream: thUpstream, cfg: &config.Config{}}
	svc.SetTokenHarborPassService(thSvc)
	svc.runOnce()

	require.Equal(t, 0, fakeTH.usageCSVHits,
		"无 F3 键的 error TH 账号（401/breaker/transport 类）必须仍被 :205 排除，豁免不得过宽")
}

// 负例 2（手工停调 payg）：schedulable=false 的 kimi payg 账号 → 仍被 :257 排除，
// 不进 payg 检查队列（checkOne → QueryBalance → GetByID 不被触达）。
func TestCNBalanceCheckF5Exempt_ManuallyDisabledPaygNotCollected(t *testing.T) {
	kimiPaygDisabled := Account{
		ID: 884, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: false,
		Credentials: map[string]any{"api_key": "sk"},
	}
	repo := &fakeCNCheckRepo{byPlatform: map[string][]Account{PlatformKimi: {kimiPaygDisabled}}}
	loadRepo := &recordingCNBalanceLoadRepo{}
	balanceSvc := NewCNProviderBalanceService(loadRepo, nil, nil, &config.Config{})
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		cfg:            &config.Config{},
	}

	svc.runOnce()

	require.Empty(t, loadRepo.getByIDIDs,
		"手工停调（Schedulable=false）的 kimi payg 必须仍被 :257 排除，不得进 payg 检查队列")
}

// 直接单测豁免判定：仅"键存在 + 可解析 + until 非空"才豁免（失败关闭）。
func TestAccountHoldsHTTP403Recovery(t *testing.T) {
	require.False(t, accountHoldsHTTP403Recovery(nil), "nil 账号不豁免")
	require.False(t, accountHoldsHTTP403Recovery(&Account{}), "无 extra 不豁免")
	require.False(t, accountHoldsHTTP403Recovery(&Account{Extra: map[string]any{}}), "键缺失不豁免")
	require.False(t, accountHoldsHTTP403Recovery(&Account{Extra: map[string]any{http403RecoveryExtraKey: nil}}), "键为 null 不豁免")
	require.False(t, accountHoldsHTTP403Recovery(&Account{Extra: map[string]any{http403RecoveryExtraKey: "garbage"}}), "键不可解析（损坏）不豁免")
	require.False(t, accountHoldsHTTP403Recovery(&Account{Extra: map[string]any{
		http403RecoveryExtraKey: map[string]any{"until": ""},
	}}), "until 为空不豁免")

	until := time.Now().Add(time.Hour)
	require.True(t, accountHoldsHTTP403Recovery(&Account{Extra: f5HTTP403RecoveryExtra(until)}),
		"合法 F3 恢复记录（键存在 + until 非空）必须豁免")
}

// ---- 派发单 C4-r2 / R19-F5：同文件其余 IsActive 收集闸门的 F3 豁免收口 ----
//
// C4 仅覆盖 :205（collect 闭包）收集闸门；本文件的另两处同类闸门（:304
// quotaService==nil fallback 收集循环、:317 openai 平台 TH 专项分支）此前仍按
// !IsActive() 排除 F3 暂停的 TH/Kira 账号，F5「冻结/error 不停刷」对该子集未闭合。
// 两用例分别锁定这两处闸门：F3 键持有者被收集、无键 error 账号仍排除（不过宽）。

// f5OpenAIThCollectEnv 构造单 openai 平台 TH 账号的 runOnce 环境（:317 openai 专项
// 收集闸门覆盖用；与 f5THCollectEnv 同基建，仅 platform 换成 openai）。
func f5OpenAIThCollectEnv(t *testing.T, id int64, status string, schedulable bool, extra map[string]any) (*tokenHarborFakeTH, *CNProviderBalanceCheckService) {
	t.Helper()
	fakeTH := newTokenHarborFakeTH(t, false)
	now := time.Now().UTC()
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()
	thUpstream := &tokenHarborFakeUpstream{server: fakeTH.server}

	acc := tokenHarborTestAccount(id)
	acc.Platform = PlatformOpenAI
	acc.Status = status
	acc.Schedulable = schedulable
	acc.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	if extra != nil {
		acc.Extra = extra
	} else {
		acc.Extra = map[string]any{}
	}

	repo := &cnRunOnceExtraRepo{
		byPlatform: map[string][]Account{PlatformOpenAI: {*acc}},
		byID:       map[int64]*Account{id: acc},
	}
	thSvc := NewTokenHarborPassService(repo, nil, thUpstream)
	thSvc.baseURL = fakeTH.server.URL
	thSvc.now = func() time.Time { return now }

	svc := &CNProviderBalanceCheckService{accountRepo: repo, httpUpstream: thUpstream, cfg: &config.Config{}}
	svc.SetTokenHarborPassService(thSvc)
	return fakeTH, svc
}

// 用例 1（:317 openai 平台 TH 专项闸门）：F3 键持有者被收集（快照刷新命中）；
// 无键 error 账号仍被排除（豁免不得过宽）。两方向各用独立环境，避免聚合命中数
// 无法区分"哪一个账号被收集"。
func TestCNBalanceCheckF5Exempt_OpenAIThGateF3CollectedNonF3Excluded(t *testing.T) {
	// 正例：openai 平台 TH 号 status=error 但持 F3 恢复记录 → 豁免 :317 → 刷新快照。
	fakeF3, svcF3 := f5OpenAIThCollectEnv(t, 885, StatusError, false,
		f5HTTP403RecoveryExtra(time.Now().Add(time.Hour)))
	svcF3.runOnce()
	require.Equal(t, 1, fakeF3.usageCSVHits,
		"持 F3 恢复记录的 openai 平台 error TH 账号必须豁免 :317 闸门并被收集（快照刷新）")

	// 负例：无 F3 键的 error TH 号 → 仍被 :317 排除（不刷新）。
	fakePlain, svcPlain := f5OpenAIThCollectEnv(t, 886, StatusError, false, map[string]any{})
	svcPlain.runOnce()
	require.Equal(t, 0, fakePlain.usageCSVHits,
		"无 F3 键的 openai 平台 error TH 账号必须仍被 :317 排除，豁免不得过宽")
}

// 用例 2（:304 quotaService==nil fallback 收集循环）：F3 键持有者被收集
// （dashboard 余额探测请求发出即证明收集）；无键 error 账号仍排除（无请求）。
// 用可区分 JWT 断言被收集者确为 F3 账号。
func TestCNBalanceCheckF5Exempt_FallbackKiraGateF3CollectedNonF3Excluded(t *testing.T) {
	buildKira := func(id int64, jwt string, extra map[string]any) Account {
		return Account{ID: id, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusError, Schedulable: false,
			Credentials: map[string]any{
				"account_mode":  AccountModePayG,
				"base_url":      "https://kiraai.vn/api/v1",
				"kira_jwt":      jwt,
				"kira_email":    "user@example.com",
				"kira_password": "pw-secret",
			},
			Extra: extra}
	}
	f3Kira := buildKira(887, "f3-jwt", f5HTTP403RecoveryExtra(time.Now().Add(time.Hour)))
	plainKira := buildKira(888, "plain-jwt", map[string]any{})

	repo := &cnRunOnceExtraRepo{byPlatform: map[string][]Account{PlatformZhipu: {f3Kira, plainKira}}}
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
			return http.StatusOK, kiraUsageSummaryFixture
		}
		return http.StatusNotFound, `{}`
	}}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, nil)
	// quotaService 缺位 → zhipu/minimax 平台走 :304 fallback 补收分支。
	svc := &CNProviderBalanceCheckService{accountRepo: repo, balanceService: balanceSvc, cfg: &config.Config{}}
	svc.runOnce()

	require.Len(t, upstream.requests, 1,
		"仅 F3 键持有者必须被 fallback 收集，无键 error 账号仍排除")
	require.Equal(t, "Bearer f3-jwt", upstream.requests[0].Header.Get("Authorization"),
		"被收集的必须是持 F3 恢复记录的账号")
}
