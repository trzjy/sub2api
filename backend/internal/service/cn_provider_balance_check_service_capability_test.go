package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// ---- BAL-GATE-D2 B1/B2：余额周期链能力门控（后端 check service） ----
//
// 本文件覆盖派发单 Done when 2/3/4：
//   - 目标唯一性（kimi/deepseek + 有效 relay URL 一轮只进 relay 目标；coding 双能力并存）；
//   - helper=false 分域（kimi/deepseek→payg、zhipu/minimax→probe、其他平台→零目标）；
//   - B2 全量补收轮（非枚举平台 relay 账号被收集；负向断言零目标零出站零快照；
//     补收不把未知平台账号归入 paygTargets）。
// B3 无代码动作，本文件不出现任何 cn_balance_low 停调新写入/清除路径。

// capabilityRepo 记录 runOnce 周期内对账号仓库的写动作（UpdateExtra / Clear /
// SetTempUnschedulable），供断言"relay 刷新不调阈值停调 / reconcile / Clear 类动作"。
type capabilityRepo struct {
	AccountRepository
	mu          sync.Mutex
	byPlatform  map[string][]Account
	byID        map[int64]*Account
	extraWrites []map[string]any
	cleared     int
	paused      int
}

func (r *capabilityRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	return r.byPlatform[platform], nil
}

// ListActive 返回各平台账号并集（B2 全量补收轮使用；本 repo 仅含显式构造账号）。
func (r *capabilityRepo) ListActive(_ context.Context) ([]Account, error) {
	var all []Account
	seen := map[int64]bool{}
	for _, accs := range r.byPlatform {
		for _, a := range accs {
			if !seen[a.ID] {
				seen[a.ID] = true
				all = append(all, a)
			}
		}
	}
	return all, nil
}

func (r *capabilityRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	return r.byID[id], nil
}

func (r *capabilityRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extraWrites = append(r.extraWrites, updates)
	return nil
}

func (r *capabilityRepo) ClearTempUnschedulable(_ context.Context, _ int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleared++
	return nil
}

func (r *capabilityRepo) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused++
	return nil
}

// ClearRateLimit 补齐 reconcileCNProviderRateLimits 调用（本测试账号无 RateLimitedAt，
// 不会被触及；no-op 仅防 nil 接口 panic）。
func (r *capabilityRepo) ClearRateLimit(_ context.Context, _ int64) error { return nil }

// TH 会话 no-op（补齐 AccountRepository 接口）。
func (r *capabilityRepo) StoreTokenHarborSession(_ context.Context, _ int64, _ string, _ time.Time) error {
	return nil
}
func (r *capabilityRepo) LoadTokenHarborSession(_ context.Context, _ int64) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (r *capabilityRepo) ClearTokenHarborSession(_ context.Context, _ int64) error { return nil }

// capabilityRelayUpstream 记录每次探测请求的 URL，按调用方注入的响应返回，用于断言
// relay 刷新目标的出站次数与路径（含 relay / native / probe 路径区分）。
type capabilityRelayUpstream struct {
	mu         sync.Mutex
	statusCode int
	body       string
	err        error
	calls      []string
}

func (u *capabilityRelayUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, req.URL.String())
	u.mu.Unlock()
	if u.err != nil {
		return nil, u.err
	}
	return &http.Response{
		StatusCode: u.statusCode,
		Body:       io.NopCloser(strings.NewReader(u.body)),
		Header:     make(http.Header),
	}, nil
}

func (u *capabilityRelayUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, "", 0, 0)
}

func (u *capabilityRelayUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *capabilityRelayUpstream) contains(substr string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range u.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// Done when 2（目标唯一性）：kimi payg + 有效 relay URL → 一轮仅进 relay 目标
//（一次出站命中 relay URL、一次快照落库、零停调/零清除）。
func TestCNBalanceCapability_KimiPaygRelayUniqueTarget(t *testing.T) {
	relayURL := "https://relay.example.com/v1/usage"
	kimiPayg := Account{
		ID: 137, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"api_key":  "sk-kimi",
			"base_url": "https://relay.example.com/v1",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true, "url": relayURL, "bearer_auth": true,
			},
		},
	}
	repo := &capabilityRepo{
		byPlatform: map[string][]Account{PlatformKimi: {kimiPayg}},
		byID:       map[int64]*Account{137: &kimiPayg},
	}
	upstream := &capabilityRelayUpstream{statusCode: http.StatusOK, body: `{"remaining":1.0,"unit":"USD","isValid":true}`}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, &config.Config{})
	svc := &CNProviderBalanceCheckService{accountRepo: repo, balanceService: balanceSvc, cfg: &config.Config{}}

	svc.runOnce()

	// 一次出站，且必须命中 relay 选路 URL（不进 payg 原生探测）。
	require.Equal(t, 1, upstream.count(), "kimi+relay 账号一轮仅一次出站（relay 刷新）")
	require.Equal(t, relayURL, upstream.calls[0], "出站必须命中 relay 选路 URL")
	// 一次快照落库（balance_low=false 由 balance service 写入；relay 刷新不写其他 extra 键）。
	require.Len(t, repo.extraWrites, 1, "relay 刷新仅落一次快照")
	// 零停调 / 零清除：阈值停调已退役 + relay 刷新不调 Clear/SetTempUnschedulable。
	require.Equal(t, 0, repo.paused, "relay 刷新不得写停调")
	require.Equal(t, 0, repo.cleared, "relay 刷新不得调 ClearTempUnschedulable")
}

// Done when 2（coding 双能力）：coding 账号同时可 relay 选路 → 同时保留 quotaTargets
//（额度探测）与 relay 刷新目标（快照刷新），两者互不干扰。
func TestCNBalanceCapability_CodingDualCapabilityQuotaAndRelay(t *testing.T) {
	relayURL := "https://relay.example.com/v1/usage"
	kimiCoding := Account{
		ID: 138, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode": "coding",
			"api_key":      "sk-kimi",
			"base_url":     "https://relay.example.com/v1",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true, "url": relayURL, "bearer_auth": true,
			},
		},
	}
	repo := &capabilityRepo{
		byPlatform: map[string][]Account{PlatformKimi: {kimiCoding}},
		byID:       map[int64]*Account{138: &kimiCoding},
	}
	prober := &fakeCNQuotaProber{}
	upstream := &capabilityRelayUpstream{statusCode: http.StatusOK, body: `{"remaining":5.0,"unit":"USD","isValid":true}`}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, &config.Config{})
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		balanceService: balanceSvc,
		quotaService: prober,
		cfg:          &config.Config{},
	}

	svc.runOnce()

	// 双能力：额度探测（quotaTargets）与 relay 刷新（relayTargets）都发生。
	require.Equal(t, []int64{138}, prober.probed, "coding 账号必须进额度探测（quotaTargets）")
	require.Equal(t, 1, upstream.count(), "coding 可 relay 账号必须同时进 relay 刷新目标（一次出站）")
	require.Equal(t, relayURL, upstream.calls[0], "relay 刷新出站必须命中 relay 选路 URL")
	// relay 刷新成功不改写 quota/rate-limit 状态：零停调/零清除。
	require.Equal(t, 0, repo.paused)
	require.Equal(t, 0, repo.cleared)
}

// Done when 3（helper=false 分域）：kimi/deepseek→payg 原生、zhipu/minimax→probe、
// 其他平台→零目标；且均不进 relay 目标（无 balance_probe → 零 relay 出站）。
func TestCNBalanceCapability_HelperFalseDomains(t *testing.T) {
	kimiPayg := Account{
		ID: 201, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-kimi"},
	}
	zhipuPayg := Account{
		ID: 202, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-zhipu"},
	}
	otherPayg := Account{
		ID: 203, Platform: "other", Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-other"},
	}
	repo := &capabilityRepo{
		byPlatform: map[string][]Account{
			PlatformKimi:   {kimiPayg},
			PlatformZhipu:  {zhipuPayg},
			"other":        {otherPayg},
		},
		byID: map[int64]*Account{201: &kimiPayg, 202: &zhipuPayg, 203: &otherPayg},
	}
	// 路径感知上游：relay(/v1/usage) → 余额 JSON；chat/completions → 探针成功；
	// 其余（kimi 原生 /user/balance）→ 余额 JSON。
	upstream := &capabilityPathUpstream{}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, &config.Config{})
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	// quotaService 必须非 nil：zhipu/minimax 由平台轮专项分支（quotaService!=nil 块）
	// 收集进 probeTargets；缺失则该分支走 Kira 补收 fallback，zhipu 不被收集。
	svc := &CNProviderBalanceCheckService{
		accountRepo: repo, balanceService: balanceSvc, httpUpstream: upstream, rateLimitSvc: rl,
		quotaService: &fakeCNQuotaProber{}, cfg: &config.Config{},
	}

	svc.runOnce()

	// 零 relay 出站（helper=false 账号绝不进 relay 目标）。
	require.False(t, upstream.contains("/v1/usage"), "helper=false 账号不得进 relay 目标（零 relay 出站）")
	// kimi/deepseek payg → 原生余额端点（api.moonshot.cn /user/balance，非 chat/completions）。
	require.True(t, upstream.contains("api.moonshot.cn"), "kimi payg 必须走原生余额端点")
	// zhipu/minimax payg → 最小完成请求探测（chat/completions）。
	require.True(t, upstream.contains("chat/completions"), "zhipu payg 必须走最小完成请求探测")
	// 其他平台（无 balance_probe）→ 零目标零出站（collect 仅遍历
	// kimi/deepseek/zhipu/minimax/openai，other 不在其列；B2 也因 helper=false 跳过）。
	require.Len(t, upstream.calls, 2, "other 平台账号零目标零出站（仅 kimi 原生 + zhipu 探针两次请求，不含 other）")
}

// capabilityPathUpstream 路径感知测试上游（relay / native / probe 三种响应）。
type capabilityPathUpstream struct {
	mu    sync.Mutex
	calls []string
}

func (u *capabilityPathUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, req.URL.String())
	u.mu.Unlock()
	var body string
	switch {
	case strings.Contains(req.URL.Path, "chat/completions"):
		body = `{"choices":[{"message":{"content":"hi"}}]}`
	case strings.Contains(req.URL.Path, "/v1/usage"):
		body = `{"remaining":1.0,"unit":"USD","isValid":true}`
	default:
		body = `{"data":{"available_balance":99.0}}`
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func (u *capabilityPathUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, "", 0, 0)
}

func (u *capabilityPathUpstream) contains(substr string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range u.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// Done when 4（B2 补收轮正向）：非枚举平台（other）的可 relay 选路账号被收集进
// relay 目标（一次出站命中 relay URL、一次快照落库）。
func TestCNBalanceCapability_B2CollectsOtherPlatformRelayAccount(t *testing.T) {
	relayURL := "https://relay2.example.com/v1/usage"
	otherRelay := Account{
		ID: 301, Platform: "other", Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"api_key":  "sk-other",
			"base_url": "https://relay2.example.com/v1",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true, "url": relayURL, "bearer_auth": true,
			},
		},
	}
	repo := &capabilityRepo{
		byPlatform: map[string][]Account{"other": {otherRelay}},
		byID:       map[int64]*Account{301: &otherRelay},
	}
	upstream := &capabilityRelayUpstream{statusCode: http.StatusOK, body: `{"remaining":2.0,"unit":"USD","isValid":true}`}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, &config.Config{})
	svc := &CNProviderBalanceCheckService{accountRepo: repo, balanceService: balanceSvc, cfg: &config.Config{}}

	svc.runOnce()

	// other 平台账号不被 platforms() 遍历，仅 B2 补收轮经 ListActive 收集进 relay 目标。
	require.Equal(t, 1, upstream.count(), "B2 必须将 other 平台 relay 账号收进 relay 目标（一次出站）")
	require.Equal(t, relayURL, upstream.calls[0], "出站必须命中 relay 选路 URL")
	require.Len(t, repo.extraWrites, 1, "relay 刷新仅落一次快照")
	require.Equal(t, 0, repo.paused)
	require.Equal(t, 0, repo.cleared)
}

// Done when 4（负向断言 1）：other 平台 + probe disabled（balance_probe 未启用）→
// 零目标零出站零快照。
func TestCNBalanceCapability_B2OtherPlatformProbeDisabledNoTarget(t *testing.T) {
	otherDisabled := Account{
		ID: 302, Platform: "other", Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-other"},
	}
	repo := &capabilityRepo{
		byPlatform: map[string][]Account{"other": {otherDisabled}},
		byID:       map[int64]*Account{302: &otherDisabled},
	}
	upstream := &capabilityRelayUpstream{statusCode: http.StatusOK, body: `{"remaining":2.0,"unit":"USD","isValid":true}`}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, &config.Config{})
	svc := &CNProviderBalanceCheckService{accountRepo: repo, balanceService: balanceSvc, cfg: &config.Config{}}

	svc.runOnce()

	require.Equal(t, 0, upstream.count(), "probe disabled 账号零出站")
	require.Len(t, repo.extraWrites, 0, "probe disabled 账号零快照")
	require.Equal(t, 0, repo.paused)
	require.Equal(t, 0, repo.cleared)
}

// Done when 4（负向断言 2）：other 平台 + enabled 但无可用 URL（url 空且无可推导
// base_url）→ RelayProbeTargetURL=false → 零目标零出站零快照。
func TestCNBalanceCapability_B2OtherPlatformEnabledNoURLNoTarget(t *testing.T) {
	otherNoURL := Account{
		ID: 303, Platform: "other", Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"api_key": "sk-other",
			// 仅启用、但未给 URL；base_url 缺失 → 无可推导 relay 端点。
			BalanceProbeConfigCredentialKey: map[string]any{"enabled": true},
		},
	}
	repo := &capabilityRepo{
		byPlatform: map[string][]Account{"other": {otherNoURL}},
		byID:       map[int64]*Account{303: &otherNoURL},
	}
	upstream := &capabilityRelayUpstream{statusCode: http.StatusOK, body: `{"remaining":2.0,"unit":"USD","isValid":true}`}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, &config.Config{})
	svc := &CNProviderBalanceCheckService{accountRepo: repo, balanceService: balanceSvc, cfg: &config.Config{}}

	svc.runOnce()

	require.Equal(t, 0, upstream.count(), "enabled 但无可用 URL 账号零出站")
	require.Len(t, repo.extraWrites, 0, "enabled 但无可用 URL 账号零快照")
}

// Done when 4（负向断言 3）：补收轮不把未知平台账号归入 paygTargets。other 平台
// relay 账号走 relay 刷新（relay URL 出站），绝不进 kimi 原生 /user/balance 或
// chat/completions 探针路径。
func TestCNBalanceCapability_B2OtherPlatformNotMisroutedToPayg(t *testing.T) {
	relayURL := "https://relay2.example.com/v1/usage"
	otherRelay := Account{
		ID: 304, Platform: "other", Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"api_key":  "sk-other",
			"base_url": "https://relay2.example.com/v1",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true, "url": relayURL, "bearer_auth": true,
			},
		},
	}
	repo := &capabilityRepo{
		byPlatform: map[string][]Account{"other": {otherRelay}},
		byID:       map[int64]*Account{304: &otherRelay},
	}
	upstream := &capabilityRelayUpstream{statusCode: http.StatusOK, body: `{"remaining":2.0,"unit":"USD","isValid":true}`}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, &config.Config{})
	svc := &CNProviderBalanceCheckService{accountRepo: repo, balanceService: balanceSvc, cfg: &config.Config{}}

	svc.runOnce()

	// 仅 relay URL 出站；不得误归 payg（kimi 原生）或 probe（chat/completions）。
	require.True(t, upstream.contains("/v1/usage"), "other 平台 relay 账号必须走 relay 刷新")
	require.False(t, upstream.contains("/user/balance"), "补收不得把未知平台账号归入 kimi 原生 payg")
	require.False(t, upstream.contains("chat/completions"), "补收不得把未知平台账号归入 zhipu 探针")
}
