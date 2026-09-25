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
}

func (f *fakeCNQuotaProber) QueryUsage(ctx context.Context, accountID int64) (*CNProviderQuotaProbeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, accountID)
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

// 对照：同地址返回有限数字余额且低于阈值时仍正常停调。
func TestCNProviderBalanceCheckRelayLowBalancePaused(t *testing.T) {
	repo := &cnBalancePauseRepo{account: &Account{
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

	require.Equal(t, cnBalancePaused, outcome)
	require.True(t, repo.pauseCalled)
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

// R18-F2：探测 200 + 余额文案 → 停调，且断言实际进入余额分支（四断言）。
func TestCNProviderBalanceCheckProbeOne_InsufficientBalancePauses(t *testing.T) {
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

	// 1) 命中余额不足 → 停调。
	require.Equal(t, 1, repo.pauseCount, "insufficient balance response must pause")
	// 2) 写入 _balance_low 快照标记。
	require.True(t, repo.balanceLow, "must mark balance_low snapshot")
	// 3) reason 前缀为 cn_balance_low，且不是配额窗口 reason。
	require.True(t, strings.HasPrefix(account.TempUnschedulableReason, cnBalanceLowReasonPrefix))
	require.False(t, IsAccountSchedulingThresholdReason(account.TempUnschedulableReason))
	// 4) 期限 ≈ 2× 检测周期（默认 10min → 20min）。
	require.NotNil(t, account.TempUnschedulableUntil)
	require.WithinDuration(t, time.Now().Add(20*time.Minute), *account.TempUnschedulableUntil, 2*time.Minute)
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

// 先停调（余额前缀 reason）→ 探测 200 健康 → 前缀精确清除。
func TestCNProviderBalanceCheckProbeOne_PauseThenClearPrefix(t *testing.T) {
	account := &Account{
		ID: 73, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-zhipu"},
	}
	repo := &cnProbeRepo{account: account}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &CNProviderBalanceCheckService{accountRepo: repo, rateLimitSvc: rl, httpUpstream: &cnProbeCaptureUpstream{}, cfg: &config.Config{}}

	// 第一次：余额不足 → 停调。
	svc.httpUpstream = &cnProbeCaptureUpstream{statusCode: http.StatusOK, body: `{"error":{"message":"insufficient balance"}}`}
	svc.probeOne(context.Background(), account)
	require.Equal(t, 1, repo.pauseCount)
	require.True(t, strings.HasPrefix(account.TempUnschedulableReason, cnBalanceLowReasonPrefix))

	// 第二次：健康 200（无余额文案）→ 精确清除余额前缀。
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
			PlatformKimi:    {kimiPayg, kimiCoding},
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
		accountRepo:  repo,
		balanceService: balanceSvc,
		quotaService: prober,
		rateLimitSvc: rl,
		httpUpstream: probeUpstream,
		cfg:          &config.Config{},
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
		accountRepo:  repo,
		balanceService: balanceSvc,
		quotaService: prober,
		rateLimitSvc: rl,
		httpUpstream: probeUpstream,
		cfg:          &config.Config{},
	}
	svc.runOnce()
	require.Empty(t, probeUpstream.probedIDs, "native-success payg must not be probed")
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

// probeQuota：落快照后重载账号并应用调度阈值停调（无阈值 → 不停调，既有语义保持）。
func TestCNProviderBalanceCheckProbeQuota_ReloadsAndNoPauseWithoutThreshold(t *testing.T) {
	account := &Account{
		ID: 401, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"account_mode": "coding"},
	}
	repo := &cnProbeRepo{account: account}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil) // 无 settingService → 阈值评估不触发
	svc := &CNProviderBalanceCheckService{
		accountRepo: repo,
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
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hUsed):   100,
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hReset):  resetAt.Format(time.RFC3339),
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
		accountRepo: repo,
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
