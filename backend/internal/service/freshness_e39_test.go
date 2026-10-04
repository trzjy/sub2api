//go:build unit

// E39：账号恢复后触发渠道维陈旧收敛。
// 覆盖：
//   - ChannelMonitorService.RefreshChannelFreshnessForAccount 窄面（有关联 monitor → 逐个评估；
//     无关联 / freshnessAlerts 未注入 / 非法 accountID → no-op nil）。
//   - 四个生产恢复点逐一接入：探测恢复（probe）、管理端 ClearAccountError（admin）、
//     国产余额周期检测（checkOne 与 probeOne 两处 ClearTempUnschedulable 出口）。
//   - 收敛失败：恢复成功结论不被改写（Warn 语义，调用方最佳努力）。
package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---------- E39：ChannelMonitorService.RefreshChannelFreshnessForAccount 窄面 ----------

// e39MonitorRepo 是 ChannelMonitorRepository 的内存桩，仅实现 E39 需要的 ListByAccountID。
type e39MonitorRepo struct {
	ChannelMonitorRepository
	byAccount map[int64][]*ChannelMonitor
}

func (r *e39MonitorRepo) ListByAccountID(_ context.Context, accountID int64) ([]*ChannelMonitor, error) {
	return r.byAccount[accountID], nil
}

// e39FreshnessEvaluator 是 ChannelFreshnessEvaluator 的内存桩，记录 EvaluateChannelFreshness 调用。
type e39FreshnessEvaluator struct {
	calls []int64
	err   error
}

func (m *e39FreshnessEvaluator) EvaluateChannelFreshness(_ context.Context, channelID int64) error {
	m.calls = append(m.calls, channelID)
	return m.err
}

func TestChannelMonitor_RefreshFreshnessForAccount_LinkedMonitorsEvaluated(t *testing.T) {
	repo := &e39MonitorRepo{byAccount: map[int64][]*ChannelMonitor{
		7: {{ID: 100}, {ID: 101}},
	}}
	eval := &e39FreshnessEvaluator{}
	svc := &ChannelMonitorService{repo: repo, freshnessAlerts: eval}

	require.NoError(t, svc.RefreshChannelFreshnessForAccount(context.Background(), 7))
	require.Equal(t, []int64{100, 101}, eval.calls, "E39：有关联 monitor → 逐个触发 EvaluateChannelFreshness")
}

func TestChannelMonitor_RefreshFreshnessForAccount_NoMonitorsNoOp(t *testing.T) {
	repo := &e39MonitorRepo{byAccount: map[int64][]*ChannelMonitor{}}
	eval := &e39FreshnessEvaluator{}
	svc := &ChannelMonitorService{repo: repo, freshnessAlerts: eval}

	require.NoError(t, svc.RefreshChannelFreshnessForAccount(context.Background(), 7))
	require.Empty(t, eval.calls, "E39：无关联 monitor → no-op，不评估")
}

func TestChannelMonitor_RefreshFreshnessForAccount_FreshnessNilNoOp(t *testing.T) {
	repo := &e39MonitorRepo{byAccount: map[int64][]*ChannelMonitor{7: {{ID: 100}}}}
	svc := &ChannelMonitorService{repo: repo} // freshnessAlerts 未注入

	require.NoError(t, svc.RefreshChannelFreshnessForAccount(context.Background(), 7))
}

func TestChannelMonitor_RefreshFreshnessForAccount_InvalidAccountNoOp(t *testing.T) {
	eval := &e39FreshnessEvaluator{}
	svc := &ChannelMonitorService{freshnessAlerts: eval}

	require.NoError(t, svc.RefreshChannelFreshnessForAccount(context.Background(), 0))
	require.Empty(t, eval.calls, "E39：非法 accountID → no-op")
}

// ---------- E39：四个生产恢复点接入 ----------

// e39ChannelFreshnessRefresher 是 ChannelFreshnessRefresher 的内存桩，记录触发调用。
type e39ChannelFreshnessRefresher struct {
	calls []int64
	err   error
}

func (r *e39ChannelFreshnessRefresher) RefreshChannelFreshnessForAccount(_ context.Context, accountID int64) error {
	if r == nil {
		return nil
	}
	r.calls = append(r.calls, accountID)
	return r.err
}

// 探测恢复出口（account_health_recovery_probe_service.go:362 一带）：恢复成功后触发收敛。
// 直接驱动 probeAccount（绕过 RunOnce 的候选列举门禁，确定性到达恢复出口）。
func TestRecoveryProbe_ChannelFreshnessOnRecover(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)
	shadow := codeBuddyProbeShadowAccount(130, 109)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{109: parent, 130: shadow},
		candidates: []*Account{shadow},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, 200, codeBuddyProbeValidSSE)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	refresher := &e39ChannelFreshnessRefresher{}
	p.SetChannelFreshnessRefresher(refresher)

	p.probeAccount(context.Background(), shadow, 10)

	require.Equal(t, 1, rlRepo.clearTempCalls, "E39：探测恢复必须清停调")
	require.Equal(t, []int64{130}, refresher.calls, "E39：恢复成功后触发渠道收敛")
}

// 探测恢复出口：收敛失败不得改写恢复成功结论（Warn 语义）。
func TestRecoveryProbe_ChannelFreshnessFailureKeepsRecovery(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)
	shadow := codeBuddyProbeShadowAccount(130, 109)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{109: parent, 130: shadow},
		candidates: []*Account{shadow},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, 200, codeBuddyProbeValidSSE)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	refresher := &e39ChannelFreshnessRefresher{err: errors.New("freshness boom")}
	p.SetChannelFreshnessRefresher(refresher)

	p.probeAccount(context.Background(), shadow, 10)

	require.Equal(t, 1, rlRepo.clearTempCalls, "E39：收敛失败不得改写恢复成功结论")
	require.Equal(t, []int64{130}, refresher.calls)
}

// 管理端 ClearAccountError 出口（admin_account.go:1638 一带）：恢复成功后触发收敛。
func TestFreshness_AdminClearAccountError_TriggersChannelFreshness(t *testing.T) {
	until := time.Now().Add(10 * time.Minute)
	repo := &accountRepoStubForClearAccountError{
		account: &Account{
			ID:                      31,
			Platform:                PlatformOpenAI,
			Type:                    AccountTypeOAuth,
			Status:                  StatusError,
			TempUnschedulableUntil:  &until,
			TempUnschedulableReason: "missing refresh token",
		},
	}
	blocker := &runtimeBlockRecorder{}
	refresher := &e39ChannelFreshnessRefresher{}
	svc := &adminServiceImpl{accountRepo: repo, runtimeBlocker: blocker, channelFreshness: refresher}

	_, err := svc.ClearAccountError(context.Background(), 31)
	require.NoError(t, err)
	require.Equal(t, 1, repo.clearTempUnschedCalls, "E39：ClearAccountError 必须清停调")
	require.Equal(t, []int64{31}, refresher.calls, "E39：恢复成功后触发渠道收敛")
}

// 管理端出口：收敛失败不得改写恢复成功结论。
func TestFreshness_AdminClearAccountError_FreshnessFailureKeepsRecovery(t *testing.T) {
	until := time.Now().Add(10 * time.Minute)
	repo := &accountRepoStubForClearAccountError{
		account: &Account{
			ID:                      31,
			Platform:                PlatformOpenAI,
			Type:                    AccountTypeOAuth,
			Status:                  StatusError,
			TempUnschedulableUntil:  &until,
			TempUnschedulableReason: "missing refresh token",
		},
	}
	blocker := &runtimeBlockRecorder{}
	refresher := &e39ChannelFreshnessRefresher{err: errors.New("freshness boom")}
	svc := &adminServiceImpl{accountRepo: repo, runtimeBlocker: blocker, channelFreshness: refresher}

	_, err := svc.ClearAccountError(context.Background(), 31)
	require.NoError(t, err)
	require.Equal(t, 1, repo.clearTempUnschedCalls, "E39：收敛失败不得改写恢复成功结论")
	require.Equal(t, []int64{31}, refresher.calls)
}

// 国产余额周期检测 checkOne 出口（cn_provider_balance_check_service.go:423 一带）：余额健康恢复后触发收敛。
func TestBalanceCheck_CheckOne_BalanceRecoveredTriggersChannelFreshness(t *testing.T) {
	until := time.Now().Add(time.Hour)
	account := &Account{
		ID:          80,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
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
	}
	repo := &cnProbeRepo{account: account}
	balanceSvc := NewCNProviderBalanceService(repo, nil, &cnBalanceResponseUpstream{
		statusCode: 200,
		body:       `{"mode":"unrestricted","remaining":-1,"unit":"USD","isValid":true}`,
	}, nil)
	refresher := &e39ChannelFreshnessRefresher{}
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		cfg:            &config.Config{},
		channelFreshness: refresher,
	}

	outcome := svc.checkOne(context.Background(), repo.account, 1.0)
	require.Equal(t, cnBalanceCleared, outcome)
	require.True(t, repo.cleared, "E39：余额健康必须清除停调")
	require.Equal(t, []int64{80}, refresher.calls, "E39：恢复成功后触发渠道收敛")
}

// 国产余额周期检测 checkOne 出口：收敛失败不得改写恢复成功结论。
func TestBalanceCheck_CheckOne_FreshnessFailureKeepsRecovery(t *testing.T) {
	until := time.Now().Add(time.Hour)
	account := &Account{
		ID:          80,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
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
	}
	repo := &cnProbeRepo{account: account}
	balanceSvc := NewCNProviderBalanceService(repo, nil, &cnBalanceResponseUpstream{
		statusCode: 200,
		body:       `{"mode":"unrestricted","remaining":-1,"unit":"USD","isValid":true}`,
	}, nil)
	refresher := &e39ChannelFreshnessRefresher{err: errors.New("freshness boom")}
	svc := &CNProviderBalanceCheckService{
		accountRepo:    repo,
		balanceService: balanceSvc,
		cfg:            &config.Config{},
		channelFreshness: refresher,
	}

	outcome := svc.checkOne(context.Background(), repo.account, 1.0)
	require.Equal(t, cnBalanceCleared, outcome)
	require.True(t, repo.cleared, "E39：收敛失败不得改写恢复成功结论")
	require.Equal(t, []int64{80}, refresher.calls)
}

// 国产余额最小完成请求探测 probeOne 出口（cn_provider_balance_check_service.go:357 一带）：
// 健康 200 探测清除余额前缀停调后触发收敛。
func TestBalanceCheck_ProbeOne_RecoversTriggersChannelFreshness(t *testing.T) {
	until := time.Now().Add(time.Hour)
	account := &Account{
		ID:          73,
		Platform:    PlatformZhipu,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-zhipu"},
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: cnBalanceLowReasonPrefix + "余额不足",
	}
	repo := &cnProbeRepo{account: account}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	refresher := &e39ChannelFreshnessRefresher{}
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		rateLimitSvc: rl,
		httpUpstream: &cnProbeCaptureUpstream{statusCode: 200, body: `{"choices":[{"message":{"content":"hi"}}]}`},
		cfg:          &config.Config{},
		channelFreshness: refresher,
	}

	svc.probeOne(context.Background(), account)

	require.True(t, repo.cleared, "E39：健康探测必须清除余额前缀停调")
	require.Equal(t, []int64{73}, refresher.calls, "E39：恢复成功后触发渠道收敛")
}

// 国产余额最小完成请求探测 probeOne 出口：收敛失败不得改写恢复成功结论。
func TestBalanceCheck_ProbeOne_FreshnessFailureKeepsRecovery(t *testing.T) {
	until := time.Now().Add(time.Hour)
	account := &Account{
		ID:          73,
		Platform:    PlatformZhipu,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-zhipu"},
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: cnBalanceLowReasonPrefix + "余额不足",
	}
	repo := &cnProbeRepo{account: account}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	refresher := &e39ChannelFreshnessRefresher{err: errors.New("freshness boom")}
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		rateLimitSvc: rl,
		httpUpstream: &cnProbeCaptureUpstream{statusCode: 200, body: `{"choices":[{"message":{"content":"hi"}}]}`},
		cfg:          &config.Config{},
		channelFreshness: refresher,
	}

	svc.probeOne(context.Background(), account)

	require.True(t, repo.cleared, "E39：收敛失败不得改写恢复成功结论")
	require.Equal(t, []int64{73}, refresher.calls)
}
