package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---- D-TH-05B：TH/Kira 收集期年龄门/退避门 + per-provider 探测间隔配置 ----

// thKiraGateConfig 构造带 B 卡默认探测间隔（60 分钟）的 config，隔离既有零值 config。
func thKiraGateConfig() *config.Config {
	return &config.Config{
		Gateway: config.GatewayConfig{
			CNProviders: config.GatewayCNProvidersConfig{
				ThProbeIntervalMinutes:   60,
				KiraProbeIntervalMinutes: 60,
			},
		},
	}
}

// 测试 1：TH 年龄门（th_usage_snapshot.fetched_at）。
func TestCNBalanceCheckTHAgeGate(t *testing.T) {
	now := time.Now().UTC()
	svc := &CNProviderBalanceCheckService{cfg: thKiraGateConfig()}

	fresh := &Account{ID: 1, Extra: map[string]any{
		TokenHarborUsageSnapshotExtraKey: TokenHarborUsageSnapshot{FetchedAt: now.Add(-5 * time.Minute)},
	}}
	stale := &Account{ID: 2, Extra: map[string]any{
		TokenHarborUsageSnapshotExtraKey: TokenHarborUsageSnapshot{FetchedAt: now.Add(-70 * time.Minute)},
	}}
	none := &Account{ID: 3, Extra: map[string]any{}}

	require.True(t, svc.shouldSkipTokenHarborCollect(now, fresh), "5分钟前快照必须跳过（年龄门）")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, stale), "70分钟前快照必须收集（超出间隔）")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, none), "无快照必须收集")
}

// 测试 2：TH 退避门（extra th_probe_backoff_until，unix 秒）。
func TestCNBalanceCheckTHBackoffGate(t *testing.T) {
	now := time.Now().UTC()
	svc := &CNProviderBalanceCheckService{cfg: thKiraGateConfig()}

	future := &Account{ID: 1, Extra: map[string]any{
		// 退避键优先于年龄门：即使快照够新鲜/缺失，退避未过期仍跳过。
		thProbeBackoffUntilExtraKey: float64(now.Add(10 * time.Minute).Unix()),
	}}
	past := &Account{ID: 2, Extra: map[string]any{
		thProbeBackoffUntilExtraKey: float64(now.Add(-10 * time.Minute).Unix()),
	}}
	missing := &Account{ID: 3, Extra: map[string]any{}}
	nullv := &Account{ID: 4, Extra: map[string]any{
		thProbeBackoffUntilExtraKey: nil,
	}}

	require.True(t, svc.shouldSkipTokenHarborCollect(now, future), "未来退避必须跳过")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, past), "过期退避必须收集")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, missing), "缺失退避必须收集")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, nullv), "null 退避必须收集")

	// 退避门与年龄门叠加：未来退避 + 陈旧快照 → 仍跳过。
	staleAndBackedOff := &Account{ID: 5, Extra: map[string]any{
		TokenHarborUsageSnapshotExtraKey: TokenHarborUsageSnapshot{FetchedAt: now.Add(-70 * time.Minute)},
		thProbeBackoffUntilExtraKey:      float64(now.Add(10 * time.Minute).Unix()),
	}}
	require.True(t, svc.shouldSkipTokenHarborCollect(now, staleAndBackedOff), "未来退避 + 陈旧快照仍跳过")
}

// 测试 3：Kira 仅年龄门（kira_usage_snapshot.fetched_at，RFC3339）。
func TestCNBalanceCheckKiraAgeGate(t *testing.T) {
	now := time.Now().UTC()
	svc := &CNProviderBalanceCheckService{cfg: thKiraGateConfig()}

	fresh := &Account{ID: 1, Extra: map[string]any{
		kiraUsageSnapshotExtraKey: map[string]any{"fetched_at": now.Add(-5 * time.Minute).Format(time.RFC3339)},
	}}
	stale := &Account{ID: 2, Extra: map[string]any{
		kiraUsageSnapshotExtraKey: map[string]any{"fetched_at": now.Add(-70 * time.Minute).Format(time.RFC3339)},
	}}
	none := &Account{ID: 3, Extra: map[string]any{}}
	bad := &Account{ID: 4, Extra: map[string]any{
		kiraUsageSnapshotExtraKey: map[string]any{"fetched_at": "not-a-time"},
	}}

	require.True(t, svc.shouldSkipKiraCollect(now, fresh), "5分钟前 kira 快照必须跳过")
	require.False(t, svc.shouldSkipKiraCollect(now, stale), "70分钟前 kira 快照必须收集")
	require.False(t, svc.shouldSkipKiraCollect(now, none), "无 kira 快照必须收集")
	require.False(t, svc.shouldSkipKiraCollect(now, bad), "不可解析 kira 快照必须收集（不过新）")
}

// 测试 4：预算联动 —— 年龄门过滤后 thTargets 数量变化，超时公式按过滤后数量
// 计算（公式不改），仅被收集账号触发快照刷新（落库 + 上游命中各 1 次）。
func TestCNBalanceCheckRunOnce_THCollectFilterBudget(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	now := time.Now().UTC()
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()
	thUpstream := &tokenHarborFakeUpstream{server: fakeTH.server}

	// fresh：th_usage_snapshot 距今 5 分钟 → 年龄门跳过（目标切片不计入）。
	fresh := tokenHarborTestAccount(701)
	fresh.Platform = PlatformOpenAI
	fresh.Status = StatusActive
	fresh.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	fresh.Extra = map[string]any{
		TokenHarborUsageSnapshotExtraKey: TokenHarborUsageSnapshot{FetchedAt: now.Add(-5 * time.Minute)},
	}
	// stale：无快照 → 收集。
	stale := tokenHarborTestAccount(702)
	stale.Platform = PlatformOpenAI
	stale.Status = StatusActive
	stale.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	stale.Extra = map[string]any{}

	repo := &cnRunOnceExtraRepo{
		byPlatform: map[string][]Account{PlatformOpenAI: {*fresh, *stale}},
		byID:       map[int64]*Account{701: fresh, 702: stale},
	}
	thSvc := NewTokenHarborPassService(repo, nil, thUpstream)
	thSvc.baseURL = fakeTH.server.URL
	thSvc.now = func() time.Time { return now }

	svc := &CNProviderBalanceCheckService{accountRepo: repo, httpUpstream: thUpstream, cfg: thKiraGateConfig()}
	svc.SetTokenHarborPassService(thSvc)
	svc.runOnce()

	// 仅 stale 被收集：fresh 被年龄门过滤，目标切片长度 = 1。
	require.Equal(t, 1, fakeTH.usageCSVHits, "fresh TH 账号必须被年龄门过滤，仅 stale 收集（usage 命中=1）")
	loginPosts, _ := fakeTH.stats()
	require.Equal(t, 1, loginPosts, "pass 快照刷新仅 stale 命中（login=1）")

	usageWrites := 0
	for _, w := range repo.extraWrites {
		if _, ok := w[TokenHarborUsageSnapshotExtraKey]; ok {
			usageWrites++
		}
	}
	require.Equal(t, 1, usageWrites, "th_usage_snapshot 仅 stale 落库一次（目标切片长度=1）")
}

// Kira 年龄门在 runOnce 主收集循环中生效：够新鲜的 Kira 账号不进 kiraTargets
// （不触发 dashboard 用量刷新请求）。
func TestCNBalanceCheckRunOnce_KiraAgeGateFiltersCollect(t *testing.T) {
	now := time.Now().UTC()
	freshKira := Account{ID: 801, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode":  AccountModePayG,
			"base_url":      "https://kiraai.vn/api/v1",
			"kira_jwt":      "stale-jwt",
			"kira_email":    "user@example.com",
			"kira_password": "pw-secret",
		}}
	freshKira.Extra = map[string]any{
		kiraUsageSnapshotExtraKey: map[string]any{"fetched_at": now.Add(-5 * time.Minute).Format(time.RFC3339)},
	}
	staleKira := Account{ID: 802, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode":  AccountModePayG,
			"base_url":      "https://kiraai.vn/api/v1",
			"kira_jwt":      "stale-jwt",
			"kira_email":    "user@example.com",
			"kira_password": "pw-secret",
		}}
	staleKira.Extra = map[string]any{}

	repo := &cnRunOnceExtraRepo{byPlatform: map[string][]Account{PlatformKimi: {freshKira, staleKira}}}
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
			return http.StatusOK, kiraUsageSummaryFixture
		}
		return http.StatusNotFound, `{}`
	}}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, nil)
	svc := &CNProviderBalanceCheckService{accountRepo: repo, balanceService: balanceSvc, cfg: thKiraGateConfig()}
	svc.runOnce()

	// 仅 staleKira 进 kiraTargets → 仅 1 次 dashboard 用量刷新请求。
	require.Len(t, upstream.requests, 1, "Kira 年龄门必须过滤掉 freshKira，仅 staleKira 刷新")
	require.Equal(t, "/api/user/usage", upstream.requests[0].URL.Path)
}

// ---- D-TH-05B 补充：TH 收集循环竞态收口（方案 §9.1，2026-10-08 用户裁定） ----

// thCollectRaceRepo 在 cnRunOnceExtraRepo 之上覆盖 GetByID，使其可模拟库内最新态
// 与读失败（不修改既有 fake 定义，仅在 gates 测试文件内增补覆盖方法）。
type thCollectRaceRepo struct {
	*cnRunOnceExtraRepo
	getByIDErr error
}

func (r *thCollectRaceRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.getByIDErr != nil {
		return nil, r.getByIDErr
	}
	return r.cnRunOnceExtraRepo.GetByID(context.Background(), id)
}

// thCollectRaceSetup 构造单 TH 账号 runOnce 竞态收口测试环境。
//   - platformAccount：ListByPlatform 返回的快照加载态（进 thTargets 判定用）；
//   - freshAccount：GetByID 返回的库内最新态（探测前复核用）；nil 时复用 platformAccount；
//   - getByIDErr：模拟 GetByID 失败。
//
// 返回 fake TH 及其 usage CSV 命中数（探测指示器）。
func thCollectRaceSetup(t *testing.T, platformAccount, freshAccount *Account, getByIDErr error) *tokenHarborFakeTH {
	t.Helper()
	fakeTH := newTokenHarborFakeTH(t, false)
	now := time.Now().UTC()
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()
	thUpstream := &tokenHarborFakeUpstream{server: fakeTH.server}

	id := platformAccount.ID
	if platformAccount.Platform == "" {
		platformAccount.Platform = PlatformOpenAI
	}
	platformAccount.Status = StatusActive
	if platformAccount.Credentials == nil {
		platformAccount.Credentials = map[string]any{}
	}
	platformAccount.Credentials["base_url"] = "https://tokenharbor.ai/v1"

	inner := &cnRunOnceExtraRepo{
		byPlatform: map[string][]Account{PlatformOpenAI: {*platformAccount}},
		byID:       map[int64]*Account{},
	}
	if freshAccount != nil {
		inner.byID[id] = freshAccount
	} else {
		inner.byID[id] = platformAccount
	}
	repo := &thCollectRaceRepo{cnRunOnceExtraRepo: inner, getByIDErr: getByIDErr}

	thSvc := NewTokenHarborPassService(repo, nil, thUpstream)
	thSvc.baseURL = fakeTH.server.URL
	thSvc.now = func() time.Time { return now }

	svc := &CNProviderBalanceCheckService{accountRepo: repo, httpUpstream: thUpstream, cfg: thKiraGateConfig()}
	svc.SetTokenHarborPassService(thSvc)
	svc.runOnce()
	return fakeTH
}

// a) 快照无退避但库内最新有未过期退避 → 探测前复核门命中 → 不探测。
func TestCNBalanceCheckRunOnce_THCollectRaceBackoffInDB(t *testing.T) {
	now := time.Now().UTC()
	platform := tokenHarborTestAccount(901) // 快照加载态：无退避、无快照
	platform.Extra = map[string]any{}
	fresh := tokenHarborTestAccount(901) // 库内最新态：未过期退避
	fresh.Extra = map[string]any{
		thProbeBackoffUntilExtraKey: float64(now.Add(10 * time.Minute).Unix()),
	}

	fakeTH := thCollectRaceSetup(t, platform, fresh, nil)
	require.Equal(t, 0, fakeTH.usageCSVHits, "库内最新有未过期退避，竞态复核必须跳过探测（usage 命中=0）")
}

// b) GetByID 报错 → 失败关闭，跳过本轮该账号（下周期自愈）。
func TestCNBalanceCheckRunOnce_THCollectRaceGetByIDError(t *testing.T) {
	platform := tokenHarborTestAccount(902)
	platform.Extra = map[string]any{}

	fakeTH := thCollectRaceSetup(t, platform, nil, errors.New("simulated db error"))
	require.Equal(t, 0, fakeTH.usageCSVHits, "GetByID 报错必须跳过本轮探测（usage 命中=0）")
}

// c) 库内干净（无退避无快照）→ 正常探测。
func TestCNBalanceCheckRunOnce_THCollectRaceClean(t *testing.T) {
	platform := tokenHarborTestAccount(903)
	platform.Extra = map[string]any{}

	fakeTH := thCollectRaceSetup(t, platform, nil, nil)
	require.Equal(t, 1, fakeTH.usageCSVHits, "库内干净必须正常探测（usage 命中=1）")
}

// d) 复核命中年龄门（库内最新快照足够新鲜）→ 不探测。
func TestCNBalanceCheckRunOnce_THCollectRaceAgeGate(t *testing.T) {
	now := time.Now().UTC()
	platform := tokenHarborTestAccount(904) // 快照加载态：无快照（build 门放行）
	platform.Extra = map[string]any{}
	fresh := tokenHarborTestAccount(904) // 库内最新态：5 分钟前快照（年龄门命中）
	fresh.Extra = map[string]any{
		TokenHarborUsageSnapshotExtraKey: TokenHarborUsageSnapshot{FetchedAt: now.Add(-5 * time.Minute)},
	}

	fakeTH := thCollectRaceSetup(t, platform, fresh, nil)
	require.Equal(t, 0, fakeTH.usageCSVHits, "复核命中年龄门必须跳过探测（usage 命中=0）")
}

// ---- D-TH-05B 补充：损坏退避键三态语义（方案 §9.2，2026-10-08 用户裁定） ----

// e)~j) 三态：损坏跳过 / 缺失-null-过期收集 / 合法未过期跳过。
func TestCNBalanceCheckTHBackoffGateCorrupted(t *testing.T) {
	now := time.Now().UTC()
	svc := &CNProviderBalanceCheckService{cfg: thKiraGateConfig()}

	// e) until="abc" → 损坏 → 跳过。
	e := &Account{ID: 1, Extra: map[string]any{thProbeBackoffUntilExtraKey: "abc"}}
	// f) until=-5 → 损坏 → 跳过。
	f := &Account{ID: 2, Extra: map[string]any{thProbeBackoffUntilExtraKey: float64(-5)}}
	// g) until=null → 未退避 → 收集。
	g := &Account{ID: 3, Extra: map[string]any{thProbeBackoffUntilExtraKey: nil}}
	// h) 键缺失 → 未退避 → 收集。
	h := &Account{ID: 4, Extra: map[string]any{}}
	// i) 合法过期值 → 收集。
	iAcct := &Account{ID: 5, Extra: map[string]any{thProbeBackoffUntilExtraKey: float64(now.Add(-10 * time.Minute).Unix())}}
	// j) 合法未过期值 → 跳过。
	jAcct := &Account{ID: 6, Extra: map[string]any{thProbeBackoffUntilExtraKey: float64(now.Add(10 * time.Minute).Unix())}}

	require.True(t, svc.shouldSkipTokenHarborCollect(now, e), "e) 损坏(abc) 必须跳过")
	require.True(t, svc.shouldSkipTokenHarborCollect(now, f), "f) 损坏(-5) 必须跳过")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, g), "g) null 必须收集")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, h), "h) 缺失必须收集")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, iAcct), "i) 过期必须收集")
	require.True(t, svc.shouldSkipTokenHarborCollect(now, jAcct), "j) 未过期必须跳过")

	// 底层三态：损坏态 corrupted=true；缺失/null corrupted=false 且 ok=false。
	_, _, eCorrupt := thProbeBackoffUntilFromExtra(e)
	require.True(t, eCorrupt, "e) 底层应识别为损坏态")
	_, _, fCorrupt := thProbeBackoffUntilFromExtra(f)
	require.True(t, fCorrupt, "f) 底层应识别为损坏态")
	_, gOk, gCorrupt := thProbeBackoffUntilFromExtra(g)
	require.False(t, gOk)
	require.False(t, gCorrupt, "g) null 非损坏态")
	_, hOk, hCorrupt := thProbeBackoffUntilFromExtra(h)
	require.False(t, hOk)
	require.False(t, hCorrupt, "h) 缺失非损坏态")

	// 损坏态必须产出单行 WARN 告警（含账号 id 与键形态，不打印完整原始值）。
	orig := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(orig)
	svc.shouldSkipTokenHarborCollect(now, e)
	require.Contains(t, buf.String(), "th collect backoff key corrupted", "损坏态必须发出告警单行")
	require.Contains(t, buf.String(), "account=1", "告警须含账号 id")
}

// TestThProbeBackoffUntilFromExtra_Boundary 表格驱动覆盖 thProbeBackoffUntilFromExtra
// 的三态解析边界（生产实现见 thProbeBackoffUntilFromExtra
// （cn_provider_balance_check_service.go））。
// 三态语义：键缺失/null → (zero,false,false)；可解析且 >0 → ok；键存在非 null 但
// 不可解析或 ≤0 → corrupted。
func TestThProbeBackoffUntilFromExtra_Boundary(t *testing.T) {
	const key = thProbeBackoffUntilExtraKey
	tests := []struct {
		name        string
		input       any
		wantOK      bool
		wantUntil   time.Time
		wantCorrupt bool
	}{
		{
			// TrimSpace 生效：空白 string 去空白后可解析。
			name:        "string with whitespace",
			input:       " 123 ",
			wantOK:      true,
			wantUntil:   time.Unix(123, 0),
			wantCorrupt: false,
		},
		{
			// ParseInt 失败（ErrSyntax/ErrRange 均含）→ parsed=false → corrupted。
			name:        "string overflow int64",
			input:       "99999999999999999999",
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			name:        "string negative",
			input:       "-5",
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			// float64 截断语义，与既有 float64 路径一致：1.5 → 1。
			name:        "float64 truncate",
			input:       float64(1.5),
			wantOK:      true,
			wantUntil:   time.Unix(1, 0),
			wantCorrupt: false,
		},
		{
			// json.Number 非法 → Int64() 失败 → parsed=false → corrupted。
			name:        "json.Number illegal",
			input:       json.Number("abc"),
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			name:        "json.Number legal",
			input:       json.Number("456"),
			wantOK:      true,
			wantUntil:   time.Unix(456, 0),
			wantCorrupt: false,
		},
		{
			// json.Number 越界 → Int64() 返回 ErrRange → parsed=false → corrupted。
			name:        "json.Number overflow int64",
			input:       json.Number("99999999999999999999"),
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			// 不支持类型走 default 分支 → parsed=false → corrupted。
			name:        "unsupported bool",
			input:       true,
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			// +Inf 由显式判定覆盖，全平台一致：越界不可表示为 int64 → corrupted。
			name:        "float64 +Inf",
			input:       math.Inf(1),
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			// NaN 必须 IsNaN 显式判（比较恒 false）；显式判定，全平台一致 → corrupted。
			name:        "float64 NaN",
			input:       math.NaN(),
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			// 大正数越界 int64：v >= MaxInt64 → 显式判定 corrupted，全平台一致。
			name:        "float64 overflow",
			input:       float64(1e30),
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			// -Inf 由 v < MinInt64 覆盖：显式判定 corrupted，全平台一致。
			name:        "float64 -Inf",
			input:       math.Inf(-1),
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			// guard 放行的最大可表示 float64：9223372036854774784 = 2^63-1024，
			// 小于 MaxInt64(2^63-1)，转换结果在 int64 域内 → ok。
			name:        "float64 max in-range (2^63-1024)",
			input:       float64(9223372036854774784),
			wantOK:      true,
			wantUntil:   time.Unix(9223372036854774784, 0),
			wantCorrupt: false,
		},
		{
			// float64 中不存在 1<<63-1 独立值，该字面量即 2^63，越出 int64
			// 转换域 → v >= MaxInt64 判损坏。
			name:        "float64 2^63 (out of range)",
			input:       float64(9223372036854775808),
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			// guard 放行（恰在域内）但 sec=MinInt64 < 0，由既有 sec<=0 判损坏——
			// 负时间戳不是合法退避截止。
			name:        "float64 -2^63 (truncates negative)",
			input:       float64(-9223372036854775808),
			wantOK:      false,
			wantCorrupt: true,
		},
		{
			// 截断语义（非四舍五入、非解析失败）：0.5 → 0 → sec<=0 → corrupted。
			name:        "float64 0.5 truncates to zero",
			input:       float64(0.5),
			wantOK:      false,
			wantCorrupt: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acc := &Account{ID: 1, Extra: map[string]any{key: tc.input}}
			gotUntil, gotOK, gotCorrupt := thProbeBackoffUntilFromExtra(acc)
			require.Equal(t, tc.wantOK, gotOK, "ok 不符")
			require.Equal(t, tc.wantCorrupt, gotCorrupt, "corrupted 不符")
			if tc.wantOK {
				require.Equal(t, tc.wantUntil, gotUntil, "until 不符")
			}
		})
	}
}
