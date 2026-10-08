package service

import (
	"context"
	"errors"
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

// 周期任务对 coding plan 账号的额度探测行为（runOnce 集成路径）：
//   - kimi coding 账号（含已被阈值停调的）→ 额度探测被调用；
//   - 智谱 coding 账号 → 额度探测被调用（智谱不进 kimi/deepseek 余额循环）；
//   - payg 账号不经过额度探测（走余额路径，本测试不放 payg 账号避免真实网络）；
//   - 非激活账号完全跳过。

// fakeCNQuotaProber 需要并发安全：runOnce 以 cnQuotaProbeConcurrency 并发调用 QueryUsage。
type fakeCNQuotaProber struct {
	mu     sync.Mutex
	probed []int64
	// result 可选注入：非 nil 时返回该结果（Kira 耗尽交状态机用例构造耗尽快照）。
	result *CNProviderQuotaProbeResult
}

func (f *fakeCNQuotaProber) QueryUsage(ctx context.Context, accountID int64) (*CNProviderQuotaProbeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, accountID)
	if f.result != nil {
		return f.result, nil
	}
	return &CNProviderQuotaProbeResult{Success: true, Persisted: true}, nil
}

type fakeCNCheckRepo struct {
	AccountRepository
	byPlatform map[string][]Account
}

func (r *fakeCNCheckRepo) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return r.byPlatform[platform], nil
}

func TestCNProviderBalanceCheckRunOnceProbesCodingPlanQuota(t *testing.T) {
	kimiActive := Account{ID: 1, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"account_mode": "coding"}}
	// 已被阈值停调的 coding 账号也要刷新快照（决定是否续停）。
	kimiPaused := Account{ID: 2, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: false,
		Credentials: map[string]any{"account_mode": "coding"}}
	// 非激活账号跳过。
	kimiInactive := Account{ID: 3, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusDisabled,
		Credentials: map[string]any{"account_mode": "coding"}}
	zhipuCoding := Account{ID: 4, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"account_mode": "coding"}}
	minimaxCoding := Account{ID: 5, Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"account_mode": "coding"}}

	repo := &fakeCNCheckRepo{byPlatform: map[string][]Account{
		PlatformKimi:    {kimiActive, kimiPaused, kimiInactive},
		PlatformZhipu:   {zhipuCoding},
		PlatformMiniMax: {minimaxCoding},
	}}
	prober := &fakeCNQuotaProber{}
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		quotaService: prober,
		cfg:          &config.Config{},
	}

	svc.runOnce()

	require.ElementsMatch(t, []int64{1, 2, 4, 5}, prober.probed)
}

// runOnceZhipuQuota 在 quotaService 缺失时安全跳过（Start 门控不启动的老部署路径）。
func TestCNProviderBalanceCheckRunOnceWithoutQuotaService(t *testing.T) {
	repo := &fakeCNCheckRepo{byPlatform: map[string][]Account{
		PlatformZhipu: {{ID: 4, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive,
			Credentials: map[string]any{"account_mode": "coding"}}},
	}}
	svc := &CNProviderBalanceCheckService{accountRepo: repo, cfg: &config.Config{}}
	require.NotPanics(t, func() { svc.runOnce() })
}

// recordingCNBalanceLoadRepo 记录余额探测服务的 GetByID 调用：payg 账号进入
// payg 检查队列必然触发 loadPayGAccount → GetByID，以此断言"未入队"这一内部
// 状态。GetByID 直接报错，保证不发生真实外呼。
type recordingCNBalanceLoadRepo struct {
	AccountRepository
	getByIDIDs []int64
}

func (r *recordingCNBalanceLoadRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.getByIDIDs = append(r.getByIDIDs, id)
	return nil, errors.New("not found")
}

// 挂在国产平台下、base_url 指向官方 ollama.com 的账号由 Ollama Cloud 用量窗口
// 负责：CN 探测端点由 base_url 衍生，ollama.com 会被出站 URL 白名单拒绝
// （CN_BALANCE_URL_REJECTED），周期任务必须整体跳过——不进额度目标，也不进
// payg 检查队列。
func TestCNProviderBalanceCheckRunOnceSkipsOllamaCloudUsageAccounts(t *testing.T) {
	// 对照组：普通 kimi coding 账号仍进额度探测。
	kimiCoding := Account{ID: 1, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"account_mode": "coding"}}
	// ollama.com 挂 kimi：coding 模式不进额度目标。
	ollamaKimiCoding := Account{ID: 2, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"account_mode": "coding", "base_url": "https://ollama.com"}}
	// ollama.com 挂 kimi：payg 模式不进 payg 检查队列。
	ollamaKimiPayg := Account{ID: 3, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"base_url": "https://ollama.com", "api_key": "sk-ollama"}}

	loadRepo := &recordingCNBalanceLoadRepo{}
	repo := &fakeCNCheckRepo{byPlatform: map[string][]Account{
		PlatformKimi: {kimiCoding, ollamaKimiCoding, ollamaKimiPayg},
	}}
	prober := &fakeCNQuotaProber{}
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: NewCNProviderBalanceService(loadRepo, nil, nil, &config.Config{}),
		quotaService:   prober,
		cfg:            &config.Config{},
	}

	svc.runOnce()

	require.Equal(t, []int64{1}, prober.probed)
	require.Empty(t, loadRepo.getByIDIDs, "ollama 账号不得进入 payg 检查队列")
}

// 双币种（deepseek CNY+USD）停调判定：任一币种达标即不停调，全部低于阈值才停；
// 无明细时退回主币种（兼容旧结果）。
func TestAllCNBalancesBelowThreshold(t *testing.T) {
	dualLow := &CNProviderBalanceResult{
		Balance:  1.0,
		Currency: "CNY",
		Balances: []CNProviderBalanceEntry{
			{Currency: "CNY", Balance: 1.0},
			{Currency: "USD", Balance: 0.5},
		},
	}
	require.True(t, allCNBalancesBelowThreshold(dualLow, 5.0))

	dualMixed := &CNProviderBalanceResult{
		Balance:  1.0,
		Currency: "CNY",
		Balances: []CNProviderBalanceEntry{
			{Currency: "CNY", Balance: 1.0},
			{Currency: "USD", Balance: 20.0},
		},
	}
	require.False(t, allCNBalancesBelowThreshold(dualMixed, 5.0))

	// 无明细：按主币种判定（旧行为）。
	singleLow := &CNProviderBalanceResult{Balance: 1.0, Currency: "CNY"}
	require.True(t, allCNBalancesBelowThreshold(singleLow, 5.0))
	singleOK := &CNProviderBalanceResult{Balance: 10.0, Currency: "CNY"}
	require.False(t, allCNBalancesBelowThreshold(singleOK, 5.0))
}

// cnBalancePauseRepo 在 cnBalanceProbeRepo 基础上记录 SetTempUnschedulable 调用。
type cnBalancePauseRepo struct {
	cnBalanceProbeRepo
	pauseCalled bool
}

func (r *cnBalancePauseRepo) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, _ string) error {
	r.pauseCalled = true
	return nil
}

// 订阅制不限量账号（同程序中转，remaining<0 → Unlimited）周期检测不得停调：
// 无数字余额可比，若按阈值比较 -1 会把订阅 key 误停。
func TestCNProviderBalanceCheckUnlimitedSubscriptionNeverPaused(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: &Account{
		ID: 52, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode": AccountModePayG,
			"api_key":      "sk-test",
			"base_url":     "https://inferaiapi.example.com",
			"api_protocol": "anthropic",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true, "url": "https://inferaiapi.example.com/v1/usage", "bearer_auth": true,
			},
		},
	}}
	balanceSvc := NewCNProviderBalanceService(repo, nil, &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"unrestricted","remaining":-1,"unit":"USD","isValid":true,"planName":"DeepSeek订阅"}`,
	}, nil)
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		cfg:            &config.Config{},
	}

	outcome := svc.checkOne(context.Background(), repo.account, 1.0)

	require.Equal(t, cnBalanceNoChange, outcome)
}

// 断言①（2026-10-08 用户裁定）：余额 0（低于阈值）探测成功 → 不写任何
// TempUnschedulable，不再因「余额低于阈值」停调。
func TestCNProviderBalanceCheckLowBalanceNoLongerPaused(t *testing.T) {
	repo := &cnProbeRepo{account: &Account{
		ID: 56, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode": AccountModePayG,
			"api_key":      "sk-test",
			"base_url":     "https://inferaiapi.example.com",
			"api_protocol": "anthropic",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true, "url": "https://inferaiapi.example.com/v1/usage", "bearer_auth": true,
			},
		},
	}}
	balanceSvc := NewCNProviderBalanceService(repo, nil, &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"remaining":0.01,"unit":"USD","isValid":true}`,
	}, nil)
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		cfg:            &config.Config{},
	}

	outcome := svc.checkOne(context.Background(), repo.account, 1.0)

	require.Equal(t, cnBalanceNoChange, outcome)
	require.Equal(t, 0, repo.pauseCount, "余额低于阈值不得再触发停调")
	require.Nil(t, repo.account.TempUnschedulableUntil)
}

// 断言②（存量清零语义）：账号已有 cn_balance_low 前缀停调、余额仍为 0（低于阈值），
// 探测成功后停调必须被无条件清除（不以余额健康为前提）。
func TestCNProviderBalanceCheckLowBalanceExistingPauseClearedOnProbeSuccess(t *testing.T) {
	until := time.Now().Add(time.Hour)
	repo := &cnProbeRepo{account: &Account{
		ID: 57, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode": AccountModePayG,
			"api_key":      "sk-test",
			"base_url":     "https://inferaiapi.example.com",
			"api_protocol": "anthropic",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true, "url": "https://inferaiapi.example.com/v1/usage", "bearer_auth": true,
			},
		},
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: cnBalanceLowReasonPrefix + "余额 0.00 USD 低于阈值 1.00",
	}}
	balanceSvc := NewCNProviderBalanceService(repo, nil, &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"remaining":0.01,"unit":"USD","isValid":true}`, // 余额仍低于阈值
	}, nil)
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		cfg:            &config.Config{},
	}

	outcome := svc.checkOne(context.Background(), repo.account, 1.0)

	require.Equal(t, cnBalanceCleared, outcome)
	require.True(t, repo.cleared, "存量 cn_balance_low 停调必须被清除")
	require.Equal(t, 0, repo.pauseCount, "清除后不得重新写入停调")
	require.Nil(t, repo.account.TempUnschedulableUntil)
}

// ---- QB-1：CN 供应商余额周期探测扩展 + coding 快照阈值停调接线 ----

// cnProbeCaptureUpstream 记录探测请求（按 accountID + path），用于断言哪些账号进入了
// 最小完成请求探测（chat/completions）路径，且未做真实外呼。
type cnProbeCaptureUpstream struct {
	statusCode  int
	body        string
	err         error
	probedIDs   []int64
	probedPaths []string
}

func (u *cnProbeCaptureUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	if u.err != nil {
		return nil, u.err
	}
	if strings.Contains(req.URL.Path, "chat/completions") {
		u.probedIDs = append(u.probedIDs, accountID)
		u.probedPaths = append(u.probedPaths, req.URL.Path)
	}
	return &http.Response{
		StatusCode: u.statusCode,
		Body:       io.NopCloser(strings.NewReader(u.body)),
		Header:     make(http.Header),
	}, nil
}

func (u *cnProbeCaptureUpstream) DoWithTLS(req *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, "", accountID, 0)
}

// cnProbeRepo 同时记录余额分支的停调/清除与落快照，供 probeOne / probeQuota 断言。
type cnProbeRepo struct {
	AccountRepository
	account    *Account
	pauseCount int
	cleared    bool
	balanceLow bool
	getByID    int
}

func (r *cnProbeRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	r.getByID++
	return r.account, nil
}

// ListByPlatform runOnce 收集用（本测试只断言 Kira 分支行为，返回空集即可）。
func (r *cnProbeRepo) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
	if r.account != nil {
		return []Account{*r.account}, nil
	}
	return nil, nil
}

func (r *cnProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if v, ok := updates[cnExtraKey(r.account.Platform, cnBalanceExtraSuffixLow)]; ok {
		if b, ok := v.(bool); ok {
			r.balanceLow = b
		}
	}
	return nil
}

func (r *cnProbeRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.pauseCount++
	r.account.TempUnschedulableUntil = &until
	r.account.TempUnschedulableReason = reason
	return nil
}

func (r *cnProbeRepo) ClearTempUnschedulable(_ context.Context, _ int64) error {
	r.cleared = true
	r.account.TempUnschedulableUntil = nil
	r.account.TempUnschedulableReason = ""
	return nil
}

// R18-F2 → D-QL-002 语义迁移：探测 200 + 余额文案 → 写 _balance_low 响应式信号
// 标记；停调语义移交额度耗尽状态机（确认探针 → 官方恢复时间），响应式入口不再
// 做 2×interval 滚动停调。zhipu 不在状态机管辖内 → 本路径不停调。
func TestCNProviderBalanceCheckProbeOne_InsufficientBalanceMarksAndDelegates(t *testing.T) {
	account := &Account{
		ID: 71, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-zhipu"},
	}
	repo := &cnProbeRepo{account: account}
	upstream := &cnProbeCaptureUpstream{statusCode: http.StatusOK, body: `{"error":{"message":"余额不足，请充值"}}`}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		rateLimitSvc: rl,
		httpUpstream: upstream,
		cfg:          &config.Config{},
	}

	svc.probeOne(context.Background(), account)

	// 1) 命中余额不足 → 写入 _balance_low 快照标记（响应式信号键保留，方案 §7）。
	require.True(t, repo.balanceLow, "must mark balance_low snapshot")
	// 2) 滚动冷却已退役：响应式入口自身不停调（停调到期时间=状态机给定值）。
	require.Equal(t, 0, repo.pauseCount, "reactive entry must not park; parking belongs to the quota lifecycle state machine")
	require.Nil(t, account.TempUnschedulableUntil)
	require.False(t, IsAccountSchedulingThresholdReason(account.TempUnschedulableReason))
}

// 探测 429（无余额文案）/ 401 / 超时 → 不停调不清除。
func TestCNProviderBalanceCheckProbeOne_NoPauseOnNonBalance(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		err        error
	}{
		{"429_no_balance_msg", http.StatusTooManyRequests, `{"error":{"message":"rate limit exceeded"}}`, nil},
		{"401", http.StatusUnauthorized, `{"error":{"message":"unauthorized"}}`, nil},
		{"timeout", 0, "", context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{
				ID: 72, Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{"api_key": "sk-mm"},
			}
			repo := &cnProbeRepo{account: account}
			upstream := &cnProbeCaptureUpstream{statusCode: tc.statusCode, body: tc.body, err: tc.err}
			rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc := &CNProviderBalanceCheckService{accountRepo: repo, rateLimitSvc: rl, httpUpstream: upstream, cfg: &config.Config{}}
			svc.probeOne(context.Background(), account)
			require.Equal(t, 0, repo.pauseCount, "must not pause")
			require.False(t, repo.cleared, "must not clear")
		})
	}
}

// 余额前缀停调（滚动冷却退役后的存量/他入口写入）→ 健康 200 探测精确清除 + F1 翻标记。
func TestCNProviderBalanceCheckProbeOne_PauseThenClearPrefix(t *testing.T) {
	account := &Account{
		ID: 73, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-zhipu"},
	}
	repo := &cnProbeRepo{account: account}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &CNProviderBalanceCheckService{accountRepo: repo, rateLimitSvc: rl, httpUpstream: &cnProbeCaptureUpstream{}, cfg: &config.Config{}}

	// 预置一笔余额前缀停调（历史存量形态）+ balance_low 标记。
	require.NoError(t, repo.SetTempUnschedulable(
		context.Background(), account.ID, time.Now().Add(time.Hour), cnBalanceLowReasonPrefix+"余额 0 VND 低于阈值 0.00",
	))
	require.NoError(t, repo.UpdateExtra(context.Background(), account.ID, map[string]any{
		cnExtraKey(account.Platform, cnBalanceExtraSuffixLow): true,
	}))

	// 健康 200（无余额文案）→ 精确清除余额前缀停调。
	svc.httpUpstream = &cnProbeCaptureUpstream{statusCode: http.StatusOK, body: `{"choices":[{"message":{"content":"hi"}}]}`}
	svc.probeOne(context.Background(), account)
	require.True(t, repo.cleared, "healthy probe must clear balance_low prefix")
	// F1：健康探测后 balance_low 快照标记翻为 false（停调时为 true）。
	require.False(t, repo.balanceLow, "healthy probe must flip balance_low snapshot marker to false")
}

// 他因 reason 不被清除：健康探测只清本服务写入的余额前缀。
func TestCNProviderBalanceCheckProbeOne_OtherReasonNotCleared(t *testing.T) {
	account := &Account{
		ID: 74, Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-mm"},
	}
	until := time.Now().Add(time.Hour)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = "some_other_subsystem_reason"
	repo := &cnProbeRepo{account: account}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		rateLimitSvc: rl,
		httpUpstream: &cnProbeCaptureUpstream{statusCode: http.StatusOK, body: `{"choices":[{"message":{"content":"hi"}}]}`},
		cfg:          &config.Config{},
	}
	svc.probeOne(context.Background(), account)
	require.False(t, repo.cleared, "other-reason pause must not be cleared by healthy probe")
}

// 模型解析为空 → 跳过 + 日志（不发起外呼、不停调）。MiniMax 无默认 web 模型，
// resolveWebTestModel 返回空，probeOne 按派发单"结果为空 → 本轮跳过（承态）"延后。
func TestCNProviderBalanceCheckProbeOne_ModelEmptySkips(t *testing.T) {
	account := &Account{
		ID: 75, Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-mm"},
	}
	repo := &cnProbeRepo{account: account}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	upstream := &cnProbeCaptureUpstream{statusCode: http.StatusOK, body: `{}`}
	svc := &CNProviderBalanceCheckService{accountRepo: repo, rateLimitSvc: rl, httpUpstream: upstream, cfg: &config.Config{}}
	svc.probeOne(context.Background(), account)
	require.Empty(t, upstream.probedIDs, "empty resolved model must skip without probe")
	require.Equal(t, 0, repo.pauseCount)
}

// runOnce：智谱 / MiniMax payg 进最小完成请求探测；coding / ollama / 管理端停用 不进。
func TestCNProviderBalanceCheckRunOnce_ZhipuMiniMaxPaygProbed(t *testing.T) {
	zhipuPayg := Account{ID: 201, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk"}}
	minimaxPayg := Account{ID: 202, Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk"}}
	zhipuCoding := Account{ID: 203, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"account_mode": "coding"}}
	ollamaZhipuPayg := Account{ID: 204, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk", "base_url": "https://ollama.com"}}
	// 管理端手动停用（Schedulable=false）的智谱 payg：不进探测（与临时停调区分——
	// 临时停调账号 Schedulable 仍为 true，须继续收集以支持充值后探测恢复）。
	zhipuPaygDisabled := Account{ID: 205, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: false,
		Credentials: map[string]any{"api_key": "sk"}}

	repo := &cnProbeRunOnceRepo{
		byPlatform: map[string][]Account{
			PlatformZhipu:   {zhipuPayg, zhipuCoding, ollamaZhipuPayg, zhipuPaygDisabled},
			PlatformMiniMax: {minimaxPayg},
		},
		byID: map[int64]*Account{201: &zhipuPayg, 202: &minimaxPayg, 203: &zhipuCoding, 204: &ollamaZhipuPayg, 205: &zhipuPaygDisabled},
	}
	prober := &fakeCNQuotaProber{}
	probeUpstream := &cnProbeCaptureUpstream{statusCode: http.StatusOK, body: `{"choices":[{"message":{"content":"hi"}}]}`}
	// 本测试无 kimi/deepseek 账号，balanceService 不会被调用。
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		quotaService: prober,
		rateLimitSvc: rl,
		httpUpstream: probeUpstream,
		cfg:          &config.Config{},
	}
	svc.runOnce()
	// 智谱 payg（glm-5.3-flash 可解析）进入最小完成请求探测；MiniMax payg 虽同样入
	// 探测队列，但 DefaultWebModelIDs(minimax) 为空 → resolveWebTestModel 返回空，
	// 按派发单"模型为空→本轮跳过（下周期重试）"延后，不发起外呼。coding / ollama 不进。
	require.ElementsMatch(t, []int64{201}, probeUpstream.probedIDs)
	require.NotContains(t, probeUpstream.probedIDs, int64(203), "coding plan must not enter probe")
	require.NotContains(t, probeUpstream.probedIDs, int64(204), "ollama account must be skipped")
	require.NotContains(t, probeUpstream.probedIDs, int64(205), "admin-disabled (Schedulable=false) account must not enter probe")
}

// runOnce：kimi/deepseek payg 原生余额端点失败时，同周期转最小完成请求探测；
// 原生成功账号绝不探测；coding 仅进额度探测。
func TestCNProviderBalanceCheckRunOnce_NativeFailProbesKimiDeepseek(t *testing.T) {
	kimiPayg := Account{ID: 301, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk"}}
	deepseekPayg := Account{ID: 302, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk"}}
	kimiCoding := Account{ID: 303, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"account_mode": "coding"}}

	repo := &cnProbeRunOnceRepo{
		byPlatform: map[string][]Account{
			PlatformKimi:     {kimiPayg, kimiCoding},
			PlatformDeepseek: {deepseekPayg},
		},
		byID: map[int64]*Account{301: &kimiPayg, 302: &deepseekPayg, 303: &kimiCoding},
	}
	prober := &fakeCNQuotaProber{}
	// 原生余额端点返回 500 → 原生探测失败 → 转最小完成请求探测。
	nativeUpstream := &cnProbeCaptureUpstream{statusCode: http.StatusInternalServerError, body: `{"error":"internal"}`}
	balanceSvc := NewCNProviderBalanceService(repo, nil, nativeUpstream, &config.Config{})
	probeUpstream := &cnProbeCaptureUpstream{statusCode: http.StatusOK, body: `{"choices":[{"message":{"content":"hi"}}]}`}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		quotaService:   prober,
		rateLimitSvc:   rl,
		httpUpstream:   probeUpstream,
		cfg:            &config.Config{},
	}
	svc.runOnce()
	require.ElementsMatch(t, []int64{301, 302}, probeUpstream.probedIDs)
}

// runOnce：kimi/deepseek payg 原生余额成功 → 不进最小完成请求探测。
func TestCNProviderBalanceCheckRunOnce_NativeSuccessNoProbe(t *testing.T) {
	kimiPayg := Account{ID: 311, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk"}}
	repo := &cnProbeRunOnceRepo{
		byPlatform: map[string][]Account{PlatformKimi: {kimiPayg}},
		byID:       map[int64]*Account{311: &kimiPayg},
	}
	prober := &fakeCNQuotaProber{}
	// 原生余额探测返回健康 200 → checkOne 走余额健康分支，不转探测。
	nativeUpstream := &cnProbeCaptureUpstream{statusCode: http.StatusOK, body: `{"data":{"available_balance":99.0}}`}
	balanceSvc := NewCNProviderBalanceService(repo, nil, nativeUpstream, &config.Config{})
	probeUpstream := &cnProbeCaptureUpstream{statusCode: http.StatusOK, body: `{"choices":[{"message":{"content":"hi"}}]}`}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		quotaService:   prober,
		rateLimitSvc:   rl,
		httpUpstream:   probeUpstream,
		cfg:            &config.Config{},
	}
	svc.runOnce()
	require.Empty(t, probeUpstream.probedIDs, "native-success payg must not be probed")
}

// ---- 外审 F5：Kira 判定先于 IsCodingPlan() 短路 ----

// quotaService==nil 的 fallback 收集：zhipu 平台下 coding 模式的 Kira 账号必须
// 被收集进 Kira 快照刷新链（余额探测请求发出即证明被收集）；非 Kira 的 coding
// 账号维持跳过（无任何上游请求）。
func TestCNProviderBalanceCheckRunOnce_FallbackCollectsKiraCodingAccount(t *testing.T) {
	kiraCoding := Account{ID: 601, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode":  "coding",
			"base_url":      "https://kiraai.vn/api/v1",
			"kira_jwt":      "stale-jwt",
			"kira_email":    "user@example.com",
			"kira_password": "pw-secret",
		}}
	zhipuCoding := Account{ID: 602, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"account_mode": "coding"}}
	repo := &cnRunOnceExtraRepo{byPlatform: map[string][]Account{
		PlatformZhipu: {kiraCoding, zhipuCoding},
	}}
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
			return http.StatusOK, kiraUsageSummaryFixture
		}
		return http.StatusNotFound, `{}`
	}}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, nil)
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		cfg:            &config.Config{},
	}
	require.NotPanics(t, func() { svc.runOnce() })
	// Kira coding 账号被收集：余额探测（dashboard JWT 链）请求已发出。
	require.Len(t, upstream.requests, 1, "Kira coding 账号必须被 fallback 收集进快照刷新链")
	require.Equal(t, "/api/user/usage", upstream.requests[0].URL.Path)
}

// Kira 账号周期链：快照刷新（quota + balance 双探）+ 耗尽信号交状态机；
// 不再做独立周期停调/清除（打摆源退役）。未耗尽/状态机未注入时不交。
func TestCNProviderBalanceCheckRunOnce_KiraSnapshotRefreshHandsOverToLifecycle(t *testing.T) {
	kiraAccount := Account{ID: 611, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode":  AccountModePayG,
			"base_url":      "https://kiraai.vn/api/v1",
			"kira_jwt":      "stale-jwt",
			"kira_email":    "user@example.com",
			"kira_password": "pw-secret",
		}}
	repo := &cnProbeRunOnceRepo{
		byPlatform: map[string][]Account{PlatformKimi: {kiraAccount}},
		byID:       map[int64]*Account{611: &kiraAccount},
	}
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
			return http.StatusOK, kiraUsageSummaryFixture
		}
		return http.StatusNotFound, `{}`
	}}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, nil)
	handover := &fakeQuotaLifecycleHandover{}
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		cfg:            &config.Config{},
	}
	svc.SetQuotaLifecycleHandover(handover)

	// 快照未产出（quotaService 缺位）→ 不交状态机。
	svc.runOnce()
	require.Empty(t, handover.calls)
	require.Len(t, upstream.requests, 1, "余额快照刷新必须发生（VND）")

	// 耗尽快照 → 交状态机一次。
	prober := &fakeCNQuotaProber{result: exhaustedKiraQuotaResult()}
	svc.quotaService = prober
	svc.runOnce()
	require.Equal(t, []int64{611}, prober.probed, "Kira 账号必须进额度探测（kira_usage_snapshot 刷新）")
	require.Equal(t, []int64{611}, handover.calls, "免费池耗尽信号必须交状态机")

	// 未耗尽快照 → 不交。
	prober.result = recoveredKiraQuotaResult()
	svc.runOnce()
	require.Equal(t, []int64{611}, handover.calls, "未耗尽不得交状态机")
}

// Kira/TH 分支不做独立周期停调/清除：即使快照刷新失败/成功，也不触发
// SetTempUnschedulable / ClearTempUnschedulable（打摆源退役）。
func TestCNProviderBalanceCheckRunOnce_KiraNoIndependentPauseOrClear(t *testing.T) {
	kiraAccount := Account{ID: 612, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode":  AccountModePayG,
			"base_url":      "https://kiraai.vn/api/v1",
			"kira_jwt":      "stale-jwt",
			"kira_email":    "user@example.com",
			"kira_password": "pw-secret",
		}}
	repo := &cnProbeRepo{account: &kiraAccount}
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		return http.StatusInternalServerError, `{"error":"boom"}`
	}}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, nil)
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		cfg:            &config.Config{},
	}
	svc.runOnce()
	require.Equal(t, 0, repo.pauseCount, "Kira 分支不得做独立周期停调")
	require.False(t, repo.cleared, "Kira 分支不得做独立周期清除（恢复交状态机 sweep）")
}

// TH 账号周期链：快照刷新（pass + usage CSV 双落库），不做任何停调/清除。
func TestCNProviderBalanceCheckRunOnce_TokenHarborSnapshotRefresh(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	// D-QL-009 F2：同锚注入（tokenharbor_pass_service.go now 字段，既有注入面），
	// 夹具与快照窗口取时同一次锚点，消除跨 UTC 00:00 竞态。
	now := time.Now().UTC()
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()
	thUpstream := &tokenHarborFakeUpstream{server: fakeTH.server}

	thAccount := tokenHarborTestAccount(621)
	thAccount.Platform = PlatformOpenAI
	thAccount.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	repo := &cnRunOnceExtraRepo{
		byPlatform: map[string][]Account{PlatformOpenAI: {*thAccount}},
		byID:       map[int64]*Account{621: thAccount},
	}
	thSvc := NewTokenHarborPassService(repo, nil, thUpstream)
	thSvc.baseURL = fakeTH.server.URL
	thSvc.now = func() time.Time { return now }

	svc := &CNProviderBalanceCheckService{accountRepo: repo, httpUpstream: thUpstream, cfg: &config.Config{}}
	svc.SetTokenHarborPassService(thSvc)
	svc.runOnce()

	loginPosts, billingHits := fakeTH.stats()
	require.Equal(t, 1, loginPosts)
	require.Equal(t, 1, billingHits, "TH 账号必须被周期链收集并刷新 pass 快照")
	require.Equal(t, 1, fakeTH.usageCSVHits, "TH 账号必须被周期链收集并刷新 usage 快照")
	require.Len(t, repo.extraWrites, 2, "th_pass_snapshot + th_usage_snapshot 双落库")
	keys := map[string]bool{}
	for _, w := range repo.extraWrites {
		for k := range w {
			keys[k] = true
		}
	}
	require.True(t, keys[TokenHarborPassSnapshotExtraKey])
	require.True(t, keys[TokenHarborUsageSnapshotExtraKey])
}

// exhaustedKiraQuotaResult 构造免费池耗尽的 Kira 用量快照结果（used>=limit）。
func exhaustedKiraQuotaResult() *CNProviderQuotaProbeResult {
	used, limit := 6_000_000.0, 6_000_000.0
	return &CNProviderQuotaProbeResult{
		Success: true,
		Snapshot: &CNProviderSnapshotOutput{
			Window: "daily", UsedTokens: &used, LimitTokens: &limit,
		},
	}
}

// recoveredKiraQuotaResult 构造未耗尽的 Kira 用量快照结果（used<limit）。
func recoveredKiraQuotaResult() *CNProviderQuotaProbeResult {
	used, limit := 1_000_000.0, 6_000_000.0
	return &CNProviderQuotaProbeResult{
		Success: true,
		Snapshot: &CNProviderSnapshotOutput{
			Window: "daily", UsedTokens: &used, LimitTokens: &limit,
		},
	}
}

// fakeQuotaLifecycleHandover 记录 OnUpstreamQuotaExhausted 交接的测试替身。
type fakeQuotaLifecycleHandover struct {
	calls []int64
}

func (f *fakeQuotaLifecycleHandover) OnUpstreamQuotaExhausted(_ context.Context, account *Account, _ string) error {
	f.calls = append(f.calls, account.ID)
	return nil
}

// cnRunOnceExtraRepo 支持 ListByPlatform + GetByID + UpdateExtra 记录（TH 刷新用）。
type cnRunOnceExtraRepo struct {
	AccountRepository
	byPlatform  map[string][]Account
	byID        map[int64]*Account
	extraWrites []map[string]any
}

func (r *cnRunOnceExtraRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	return r.byPlatform[platform], nil
}

func (r *cnRunOnceExtraRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	return r.byID[id], nil
}

func (r *cnRunOnceExtraRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.extraWrites = append(r.extraWrites, updates)
	return nil
}

// D-TH-05A 新增仓库方法的 no-op 桩（补齐 AccountRepository 接口编译/运行断言所需）。
func (r *cnRunOnceExtraRepo) StoreTokenHarborSession(_ context.Context, _ int64, _ string, _ time.Time) error {
	return nil
}

func (r *cnRunOnceExtraRepo) LoadTokenHarborSession(_ context.Context, _ int64) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func (r *cnRunOnceExtraRepo) ClearTokenHarborSession(_ context.Context, _ int64) error {
	return nil
}

// cnProbeRunOnceRepo 支持 ListByPlatform + GetByID，供 runOnce 集成测试。
type cnProbeRunOnceRepo struct {
	AccountRepository
	byPlatform map[string][]Account
	byID       map[int64]*Account
}

func (r *cnProbeRunOnceRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	return r.byPlatform[platform], nil
}

func (r *cnProbeRunOnceRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	return r.byID[id], nil
}

func (r *cnProbeRunOnceRepo) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	return nil
}

// D-TH-05A 新增仓库方法的 no-op 桩（补齐 AccountRepository 接口编译/运行断言所需）。
func (r *cnProbeRunOnceRepo) StoreTokenHarborSession(_ context.Context, _ int64, _ string, _ time.Time) error {
	return nil
}

func (r *cnProbeRunOnceRepo) LoadTokenHarborSession(_ context.Context, _ int64) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func (r *cnProbeRunOnceRepo) ClearTokenHarborSession(_ context.Context, _ int64) error {
	return nil
}

// probeQuota：落快照后重载账号并应用调度阈值停调（无阈值 → 不停调，既有语义保持）。
func TestCNProviderBalanceCheckProbeQuota_ReloadsAndNoPauseWithoutThreshold(t *testing.T) {
	account := &Account{
		ID: 401, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"account_mode": "coding"},
	}
	repo := &cnProbeRepo{account: account}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil) // 无 settingService → 阈值评估不触发
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		quotaService: &fakeCNQuotaProber{},
		rateLimitSvc: rl,
		cfg:          &config.Config{},
	}
	svc.probeQuota(context.Background(), account.ID, PlatformZhipu)
	require.Equal(t, 1, repo.getByID, "probeQuota must reload account via GetByID after snapshot")
	require.Equal(t, 0, repo.pauseCount, "no threshold configured -> no pause")
}

// probeQuota：配置阈值触顶 → 停调；快照刷新降值（恢复）→ 不续停。
func TestCNProviderBalanceCheckProbeQuota_ThresholdBreachPausesAndRecovers(t *testing.T) {
	resetAt := time.Now().UTC().Add(6 * time.Hour)
	account := &Account{
		ID: 402, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"account_mode": "coding"},
		Extra: map[string]any{
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hUsed):  100,
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hReset): resetAt.Format(time.RFC3339),
		},
	}
	repo := &cnProbeRepo{account: account}

	// 内存 SettingsRepository：配置 zhipu 阈值 99%。
	settingRepo := &cnProbeSettingsRepo{data: map[string]string{
		SettingKeyAccountSchedulingThresholds: `{"zhipu":99}`,
	}}
	// 避免包级阈值缓存跨用例污染。
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	rl.SetSettingService(NewSettingService(settingRepo, &config.Config{}))

	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		quotaService: &fakeCNQuotaProber{},
		rateLimitSvc: rl,
		cfg:          &config.Config{},
	}

	// 触顶 → 停调。
	svc.probeQuota(context.Background(), account.ID, PlatformZhipu)
	require.Equal(t, 1, repo.pauseCount)
	require.True(t, IsAccountSchedulingThresholdReason(account.TempUnschedulableReason))

	// 快照刷新降值（恢复）→ 重新探测不续停。
	account.Extra[cnExtraKey(PlatformZhipu, cnExtraSuffix5hUsed)] = 50
	svc.probeQuota(context.Background(), account.ID, PlatformZhipu)
	require.Equal(t, 1, repo.pauseCount, "recovered snapshot must not re-pause")
}

// cnProbeSettingsRepo 极简内存 SettingsRepository，供阈值停调集成测试。
type cnProbeSettingsRepo struct {
	data map[string]string
}

func (r *cnProbeSettingsRepo) Get(_ context.Context, key string) (*Setting, error) {
	if v, ok := r.data[key]; ok {
		return &Setting{Key: key, Value: v}, nil
	}
	return nil, nil
}
func (r *cnProbeSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	return r.data[key], nil
}
func (r *cnProbeSettingsRepo) Set(_ context.Context, key, value string) error {
	r.data[key] = value
	return nil
}
func (r *cnProbeSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := r.data[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}
func (r *cnProbeSettingsRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	for k, v := range settings {
		r.data[k] = v
	}
	return nil
}
func (r *cnProbeSettingsRepo) GetAll(_ context.Context) (map[string]string, error) {
	return r.data, nil
}
func (r *cnProbeSettingsRepo) Delete(_ context.Context, key string) error {
	delete(r.data, key)
	return nil
}

// ---- D-TH-04：TH 会话缓存双实例合并（共享单实例注入） ----

// 探测链与周期链共用同一 TH 探测实例：quota 服务构造时自建的 thPassService
// 经 THPassService() 访问器取出、经 wire 注入 balance 服务后，balance 服务
// tokenHarborPass() 应指向同一指针（同一 6h 会话缓存，登录次数减半）。
// 同时锁死未注入时 tokenHarborPass() 懒装配回退（非 nil）不回归。
func TestTokenHarborSharedInstance(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	repo := &tokenHarborRepoStub{}

	// 1) quota 服务构造时自建 TH 探测链，访问器返回非 nil。
	q := NewCNProviderQuotaService(repo, nil, upstream, nil)
	require.NotNil(t, q.THPassService(), "quota 服务 THPassService() 必须返回自建实例")

	// 2) 注入 balance 服务后，tokenHarborPass() 与 quota 实例指针相等（证明共享）。
	q.THPassService().baseURL = fakeTH.server.URL
	s := NewCNProviderBalanceCheckService(repo, nil, q, nil, upstream, nil, &config.Config{}, 0)
	s.SetTokenHarborPassService(q.THPassService())
	require.Same(t, q.THPassService(), s.tokenHarborPass(),
		"周期链与探测链必须共用同一 TH 实例（同一会话缓存）")

	// 3) 未注入时 tokenHarborPass() 懒装配仍返回非 nil（锁死既有回退行为）。
	lazy := NewCNProviderBalanceCheckService(repo, nil, nil, nil, upstream, nil, &config.Config{}, 0)
	require.NotNil(t, lazy.tokenHarborPass(), "未注入时懒装配必须返回非 nil 实例")
}
