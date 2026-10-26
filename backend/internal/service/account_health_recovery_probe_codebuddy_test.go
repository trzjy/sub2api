//go:build unit

package service

// AC-B1 探测侧接线回归测试（方案 §三 卡 B）：
//   - 直绑 CodeBuddy 账号（platform=codebuddy、parent_account_id=nil、type=oauth）
//     进入 probeUpstream 后走 billing-meter 探测：不落 buildOpenAIAPIKeyModelsRequest
//     的 apikey 检查，也不依赖 extra.shadow_model；2xx → 恢复，401/403 → 仍停调。
//   - 影子账号路径不回归（仍走 chat 探测 + 解析母账号凭据）。
//   - probeRecoverableMarkers 含 oauth_401_recoverable；卡 A 写入的 JSON reason
//     使 isHealthBreakerTrip 返回 true；MatchedKeyword 填上游错误全文时负例不识别。
//
// 复用同包既有夹具（均位于无构建标签的测试文件中，unit 标签构建时一并编译）：
//   - codeBuddyProbeUpstreamStub / newCodeBuddyProbeUpstreamStub /
//     codeBuddyProbeValidSSE / codeBuddyProbeShadowAccount /
//     newProbeServiceWithUpstream（codebuddy_probe_recovery_test.go）；
//   - codebuddyProbeParentAccount（account_test_service_codebuddy_test.go）；
//   - probeRepoMock / probeRateLimitRepo / parkedBreakerAccount /
//     setFastProbeRetry / newSettingService（openai_apikey_health_breaker_test.go）。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// acB1DirectCodeBuddyAccount 构造被健康熔断停调的 CodeBuddy 直绑账号
// （platform=codebuddy、oauth 型、parent_account_id=nil、自身携带 access_token）。
func acB1DirectCodeBuddyAccount(id int64) *Account {
	until := time.Now().Add(5 * time.Minute)
	state := TempUnschedState{
		UntilUnix:      until.Unix(),
		MatchedKeyword: openAIAPIKeyHealthBreakerReason,
		Tier:           HealthBreakerTierTrip,
	}
	reason, _ := json.Marshal(state)
	return &Account{
		ID:                      id,
		Platform:                PlatformCodeBuddy,
		Type:                    AccountTypeOAuth,
		Concurrency:             1,
		Credentials:             map[string]any{"access_token": "DIRECT_TOKEN_SECRET"},
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: string(reason),
	}
}

// TestCodeBuddyDirectProbeUsesBillingMeterNotModels 直绑账号进入 probeUpstream 后
// 必须走 billing-meter 探测，不落 /models 的 apikey 检查、也不依赖 shadow_model。
func TestCodeBuddyDirectProbeUsesBillingMeterNotModels(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	direct := acB1DirectCodeBuddyAccount(258)
	// 故意不设置任何 extra（无 shadow_model），验证直绑不依赖该键。
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{258: direct},
		candidates: []*Account{direct},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"code":0}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)

	// C1：连续 2 次 2xx 成功才恢复（首次成功后由异步补探补齐第二次）。
	p.RunOnce(context.Background())
	p.wg.Wait()

	require.Equal(t, 1, repo.clearTempIfReasonCalls, "直绑 billing 探测连续 2xx 必须恢复停调")
	rec := upstream.lastRecord(t)
	require.Equal(t, http.MethodPost, rec.Method)
	require.Equal(t, codeBuddyBillingMeterPath, rec.Path, "直绑必须走 billing-meter 端点，而非 chat 或 /models")
	require.NotEqual(t, "/v1/models", rec.Path, "直绑不得落到 apikey /models 路径（否则触发 apikey 检查/降级）")
	require.Equal(t, "Bearer DIRECT_TOKEN_SECRET", rec.Header.Get("Authorization"), "直绑用自身凭据")
	require.Equal(t, "SaaS", rec.Header.Get("X-Product"))
	require.Equal(t, CodeBuddyClientUA, rec.Header.Get("User-Agent"))
	// 身份头占位：直绑无 uid/enterprise_id/domain 时用 X-No-*: 1 占位。
	require.Equal(t, "1", rec.Header.Get("X-No-User-Id"))
	require.Equal(t, "1", rec.Header.Get("X-No-Enterprise-Id"))
	require.Equal(t, "1", rec.Header.Get("X-No-Domain"))
}

// TestCodeBuddyDirectProbe401KeepsParked 直绑 billing 探测遇 401/403 仍停调
// （卡 B 判定：401/403 = 仍停调），不得恢复也不得误触降级到期。
func TestCodeBuddyDirectProbe401KeepsParked(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	direct := acB1DirectCodeBuddyAccount(259)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{259: direct},
		candidates: []*Account{direct},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusUnauthorized, `{"code":40101}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)
	// 永久失败：第 2 次探测请求后取消 ctx 划定补探边界（C1 补探无轮数上限）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream.cancelAfterRequests(2, cancel)

	p.RunOnce(ctx)
	p.wg.Wait() // 等待异步补探按 ctx 取消收敛（AC-C1）

	require.Zero(t, repo.clearTempIfReasonCalls, "直绑 billing 探测 401 不得恢复停调")
	require.Equal(t, 2, repo.bumpTempIfReasonCalls, "失败后 attempt 计数 +1（首探 + 1 次补探，代际守卫 BumpTempUnschedulableReasonIfGeneration）")
}

// TestCodeBuddyShadowProbePathNoRegression 影子账号经 probeUpstream 后仍走 chat 探测
// （不回归到 billing meter），且仍解析母账号凭据。
func TestCodeBuddyShadowProbePathNoRegression(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)
	shadow := codeBuddyProbeShadowAccount(130, 109)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{109: parent, 130: shadow},
		candidates: []*Account{shadow},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, codeBuddyProbeValidSSE)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)

	// C1：连续 2 次成功才恢复（首次成功后由异步补探补齐第二次）。
	p.RunOnce(context.Background())
	p.wg.Wait()

	require.Equal(t, 1, repo.clearTempIfReasonCalls, "影子 chat 探测连续成功仍恢复停调")
	rec := upstream.lastRecord(t)
	require.Equal(t, "/v2/chat/completions", rec.Path, "影子必须仍走 chat 探测（不回归到 billing meter）")
	require.Equal(t, "Bearer PARENT_TOKEN_SECRET", rec.Header.Get("Authorization"), "影子仍解析母账号凭据")
}

// TestCodeBuddyRecoverableMarkersIncludesOAuth401 marker 列表须含 oauth_401_recoverable，
// 且卡 A 写入的 JSON reason（MatchedKeyword = oauth401RecoverableMarker）能使
// isHealthBreakerTrip 返回 true。
func TestCodeBuddyRecoverableMarkersIncludesOAuth401(t *testing.T) {
	require.Contains(t, probeRecoverableMarkers, oauth401RecoverableMarker,
		"probeRecoverableMarkers 须含卡 A 的 oauth_401_recoverable marker")
	require.Equal(t, "oauth_401_recoverable", oauth401RecoverableMarker,
		"不得字面量重复，须引用 ratelimit_service.go 同常量")

	// 卡 A 写入的 JSON reason（MatchedKeyword = oauth401RecoverableMarker）须被认领。
	until := time.Now().Add(5 * time.Minute)
	state := TempUnschedState{
		UntilUnix:      until.Unix(),
		MatchedKeyword: oauth401RecoverableMarker,
		Tier:           HealthBreakerTierTrip,
	}
	reason, _ := json.Marshal(state)
	acc := &Account{
		ID:                      401,
		Platform:                PlatformCodeBuddy,
		Type:                    AccountTypeOAuth,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: string(reason),
	}
	p := &AccountHealthRecoveryProbeService{}
	require.True(t, p.isHealthBreakerTrip(acc, time.Now()),
		"卡 A 写入的 oauth_401_recoverable JSON 须被 isHealthBreakerTrip 认领")
}

// TestCodeBuddyOAuth401MarkerNegativeFullErrorMessage 负例：MatchedKeyword 填上游错误全文
// 时不得被识别为可恢复 marker（精确等值比较，避免把任意错误文案当 marker）。
func TestCodeBuddyOAuth401MarkerNegativeFullErrorMessage(t *testing.T) {
	until := time.Now().Add(5 * time.Minute)
	state := TempUnschedState{
		UntilUnix:      until.Unix(),
		MatchedKeyword: "upstream returned 401 unauthorized: access_token expired",
		Tier:           HealthBreakerTierTrip,
	}
	reason, _ := json.Marshal(state)
	acc := &Account{
		ID:                      402,
		Platform:                PlatformCodeBuddy,
		Type:                    AccountTypeOAuth,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: string(reason),
	}
	p := &AccountHealthRecoveryProbeService{}
	require.False(t, p.isHealthBreakerTrip(acc, time.Now()),
		"MatchedKeyword 填上游错误全文时不得被识别为可恢复 marker（负例）")
}

// TestCodeBuddyDirectProbe2xxBodyFailureKeepsParked 直绑 billing-meter 探测收到 HTTP 200
// 但响应体业务校验不过（AC-G2R-1：复用 evaluateCodeBuddyProbeBody 判据）→ 探测失败、
// 保持停调，不得仅凭状态码误清熔断。
func TestCodeBuddyDirectProbe2xxBodyFailureKeepsParked(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	direct := acB1DirectCodeBuddyAccount(263)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{263: direct},
		candidates: []*Account{direct},
	}
	// 2xx 但业务信封 code 非 0（认证/业务错误随 200 返回）：不得判恢复。
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"code":40101,"message":"unauthorized"}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream.cancelAfterRequests(2, cancel)

	p.RunOnce(ctx)
	p.wg.Wait() // 等待异步补探按 ctx 取消收敛（AC-C1）

	require.Zero(t, repo.clearTempIfReasonCalls, "直绑 2xx 但 body 业务失败不得恢复停调（AC-G2R-1）")
	// 首探 + 1 次补探均因 body 失败而 parked（证明探测确实执行且失败关闭）。
	require.Equal(t, 2, repo.bumpTempIfReasonCalls, "2xx+body 失败应仍走失败簿记（首探 + 补探）")
}

// TestCodeBuddyDirectProbe2xxEmptyBodyKeepsParked 直绑 billing-meter 探测收到 HTTP 200
// 但空 body（AC-G2R-1 同源缺陷：空 body 被 gjson 误判为健康）→ 探测失败、保持停调。
func TestCodeBuddyDirectProbe2xxEmptyBodyKeepsParked(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	direct := acB1DirectCodeBuddyAccount(264)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{264: direct},
		candidates: []*Account{direct},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, "")
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream.cancelAfterRequests(2, cancel)

	p.RunOnce(ctx)
	p.wg.Wait()

	require.Zero(t, repo.clearTempIfReasonCalls, "直绑 2xx 空 body 不得恢复停调（AC-G2R-1）")
	require.Equal(t, 2, repo.bumpTempIfReasonCalls, "空 body 应仍走失败簿记")
}

// TestCodeBuddyProbeInFlightSkipsSweep 同账号补探在途时（上一轮异步补探仍未完成），
// 新一轮 sweep 必须完全跳过该账号：不首探、不入队补探（AC-G2R-2）。
func TestCodeBuddyProbeInFlightSkipsSweep(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	direct := acB1DirectCodeBuddyAccount(265)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{265: direct},
		candidates: []*Account{direct},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"code":0}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)

	// 模拟同账号补探在途（上一轮异步补探尚未完成）。
	p.markRetryInFlight(direct.ID)

	probeCalls := 0
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		probeCalls++
		return true, nil
	})

	p.RunOnce(context.Background())

	require.Zero(t, probeCalls, "同账号补探在途时本轮 sweep 完全跳过该账号（不首探、不入队补探，AC-G2R-2）")
	require.Zero(t, repo.clearTempIfReasonCalls, "在途期间不得误恢复")
	require.Zero(t, repo.bumpTempIfReasonCalls, "在途期间不应有首探 attempt 记账")
}

// TestCodeBuddyProbeStopDuringRetrySleepNoHang Stop 在补探 goroutine 睡眠期间调用时，
// 必须唤醒补探 goroutine（close 其已捕获的 stop channel 引用），wg.Wait 不得挂死
// （AC-G2R-3，带超时断言）。
func TestCodeBuddyProbeStopDuringRetrySleepNoHang(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	direct := acB1DirectCodeBuddyAccount(266)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{266: direct},
		candidates: []*Account{direct},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"code":0}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)

	// 长补探间隔：让补探 goroutine 进入睡眠，便于在 sleep 期间调用 Stop。
	p.retryInterval = time.Second

	// 模拟服务已启动（Stop 需要非 nil 的 stopCh 才会 close + wg.Wait）。
	p.startMu.Lock()
	p.stopCh = make(chan struct{})
	p.startMu.Unlock()

	// 首探失败（override 返回 false）→ 入队补探 → 补探 goroutine 捕获 stopCh 引用后
	// 进入 1s 睡眠。
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		return false, nil
	})
	p.RunOnce(context.Background())

	// 短暂等待确保补探 goroutine 已启动并进入 sleep（已捕获 stopCh）。
	time.Sleep(100 * time.Millisecond)

	// Stop 在补探睡眠期间调用：必须唤醒补探 goroutine，wg.Wait 不得挂死。
	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 在补探 sleep 期间调用后 wg.Wait 挂死（AC-G2R-3 未修复）")
	}

	// Stop 唤醒补探后，补探被中断（未执行真实补探），仅首探 attempt 记账一次。
	require.Equal(t, 1, repo.bumpTempIfReasonCalls, "Stop 应中断补探睡眠，仅首探 attempt 记账")
	require.Zero(t, repo.clearTempIfReasonCalls, "补探被中断，账号仍停调")
}
