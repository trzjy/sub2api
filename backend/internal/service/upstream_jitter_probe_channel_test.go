//go:build unit

// JIT-04 定向测试：上游抖动停调的模型级探测通道只认真实调用，cursor 按模型探测。
//
// 覆盖：
//   - Connect 会话号（cursor + type≠api_key）的 probeCursorModelScopeUpstream：
//     200+非空文本 → true；200+空文本 → false；500+文本 → false；替身 error → (false, err)；
//     并断言上游模型=scope、预算=最小预算常量、cursorExec 缺失时返回 error 不伪造成功。
//   - cursor 平台的 probeModelScopeUpstream 不再返回 errUnsupportedModelProbe（Connect 分支接管）。
//   - apikey 反代账号不会调用 Connect 分支的 cursorAgentRunProbe（替身计数 0），数据面
//     分支复用既有已测函数 probeUpstreamDataPlaneWithProtocolForModel（此处不写真实网络测试）。
//
// 未覆盖（如实登记）：probeModelScope 完整链路需要 Redis 锁与版本化写入，本文件只测
// 探测执行体与平台分派，不构造完整链路。
package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// upstreamJitterConnectAccount 构造 cursor Connect 会话号（platform=cursor && type≠api_key）。
func upstreamJitterConnectAccount(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformCursor,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "STATIC_ACCESS_TOKEN",
		},
	}
}

// --- a) Connect 会话号：真实调用探测的四种返回形态 + 传参断言 ---

func TestUpstreamJitterProbeChannelCursorConnectProbe(t *testing.T) {
	const scope = "claude-opus-4"
	probeErr := errors.New("agent run transport failure")

	tests := []struct {
		name    string
		status  int
		text    string
		err     error
		wantOK  bool
		wantErr bool
	}{
		{"200 with text", http.StatusOK, "hello", nil, true, false},
		{"200 with blank text", http.StatusOK, "   ", nil, false, false},
		{"500 with text", http.StatusInternalServerError, "boom", nil, false, false},
		{"probe error", 0, "", probeErr, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acc := upstreamJitterConnectAccount(71)
			p := &AccountHealthRecoveryProbeService{
				cursorExec: &CursorExecutionService{},
			}
			callCount := 0
			var gotExec *CursorExecutionService
			var gotAcc *Account
			var gotModel string
			var gotBudget int
			p.cursorAgentRunProbe = func(ctx context.Context, exec *CursorExecutionService, account *Account, upstreamModel string, budget int) (int, string, string, error) {
				callCount++
				gotExec = exec
				gotAcc = account
				gotModel = upstreamModel
				gotBudget = budget
				return tc.status, "", tc.text, tc.err
			}

			ok, err := p.probeCursorModelScopeUpstream(context.Background(), acc, scope)

			require.Equal(t, 1, callCount, "Connect 会话号必须发一次真实调用探测")
			require.Same(t, acc, gotAcc)
			require.Same(t, p.cursorExec, gotExec)
			require.Equal(t, scope, gotModel, "传入的 upstreamModel 必须等于 scope")
			require.Equal(t, cursorAccountHealthProbeMinBudget, gotBudget, "budget 必须等于最小输出预算常量")
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, probeErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantOK, ok)
		})
	}
}

// --- a) cursorExec 缺失时不得伪造成功 ---

func TestUpstreamJitterProbeChannelCursorConnectRequiresExec(t *testing.T) {
	acc := upstreamJitterConnectAccount(72)
	p := &AccountHealthRecoveryProbeService{}
	called := 0
	p.cursorAgentRunProbe = func(ctx context.Context, exec *CursorExecutionService, account *Account, upstreamModel string, budget int) (int, string, string, error) {
		called++
		return http.StatusOK, "", "hi", nil
	}

	ok, err := p.probeCursorModelScopeUpstream(context.Background(), acc, "claude-opus-4")

	require.False(t, ok)
	require.Error(t, err)
	require.Equal(t, 0, called, "cursorExec 未注入时不得伪造成功，也不得调用探针替身")
}

// --- b) cursor 不再返回 errUnsupportedModelProbe（Connect 分支接管平台分派） ---

func TestUpstreamJitterProbeChannelCursorUpstreamNoLongerUnsupported(t *testing.T) {
	acc := upstreamJitterConnectAccount(73)
	p := &AccountHealthRecoveryProbeService{
		cursorExec: &CursorExecutionService{},
	}
	p.cursorAgentRunProbe = func(ctx context.Context, exec *CursorExecutionService, account *Account, upstreamModel string, budget int) (int, string, string, error) {
		return http.StatusOK, "", "hi", nil
	}

	ok, err := p.probeModelScopeUpstream(context.Background(), acc, "claude-opus-4")

	require.False(t, errors.Is(err, errUnsupportedModelProbe), "cursor 已从不支持按模型探测的列表摘出")
	require.NoError(t, err)
	require.True(t, ok)
}

// --- apikey 反代分支：Connect 探针替身不得被调用；数据面分支复用既有已测函数 ---
// 不写真实网络测试：apikey 账号无 api_key 凭据时数据面探测在发请求前 fast-fail，
// 此处仅断言 Connect 分支没有被误触发（替身计数为 0）。
func TestUpstreamJitterProbeChannelAPIKeySkipsConnectProbe(t *testing.T) {
	acc := &Account{
		ID:          74,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{
			// 故意不写 api_key：数据面探测在取凭据处 fast-fail，不会发真实网络请求。
		},
	}
	require.False(t, acc.IsCursorConnectSessionAccount())
	require.True(t, acc.IsCursorOpenAICompatibleAccount())

	p := &AccountHealthRecoveryProbeService{
		cursorExec: &CursorExecutionService{},
	}
	called := 0
	p.cursorAgentRunProbe = func(ctx context.Context, exec *CursorExecutionService, account *Account, upstreamModel string, budget int) (int, string, string, error) {
		called++
		return http.StatusOK, "", "hi", nil
	}

	ok, err := p.probeModelScopeUpstream(context.Background(), acc, "claude-opus-4")

	require.Equal(t, 0, called, "apikey 反代账号不得调用 Connect 分支的 cursorAgentRunProbe")
	require.False(t, ok)
	require.Error(t, err, "无 api_key 凭据时数据面探测 fast-fail，不伪造成功")
	require.False(t, errors.Is(err, errUnsupportedModelProbe), "cursor 平台整体不再返回不支持错误")
}

// --- JIT-27: probeModelScope 抖动分支的失败重排 / 成功版本化清除 ---

type jitterProbeLockFake struct{}

func (jitterProbeLockFake) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (jitterProbeLockFake) ReleaseLeaderLock(context.Context, string, string) error { return nil }

// jitterProbeRepoAll 合并本卡两处赋值所需的接口：
//   - AccountHealthRecoveryProbeService.accountRepo: accountHealthProbeRepository
//   - RateLimitService.accountRepo（用例 2 成功分支）: AccountRepository
//
// 派发单骨架仅嵌入 accountHealthProbeRepository，无法满足 rateLimit 的 AccountRepository
// 字段；只嵌入 AccountRepository 又缺 ClearRecoveryProbeComponent 等 probe 窄面方法。
// Go 1.14+ 允许嵌入方法集重叠的接口（同名方法签名一致），故用组合接口一次覆盖。
type jitterProbeRepoAll interface {
	AccountRepository
	accountHealthProbeRepository
}

type jitterProbeRepoFake struct {
	jitterProbeRepoAll // 嵌入 nil 大接口：只满足类型，测试只触达下面显式实现的方法
	patches            []ModelProbePatch
	applied            int
}

func (r *jitterProbeRepoFake) PatchModelRateLimitEntry(_ context.Context, _ int64, _ string, _ int64, p ModelProbePatch) (bool, error) {
	r.patches = append(r.patches, p)
	return true, nil
}
func (r *jitterProbeRepoFake) WithModelRateLimitAccountLock(ctx context.Context, _ int64, fn func(context.Context) error) error {
	return fn(ctx)
}
func (r *jitterProbeRepoFake) GetModelRateLimitMeta(context.Context, int64, string) (time.Time, int64, bool, error) {
	return time.Time{}, 0, false, nil
}
func (r *jitterProbeRepoFake) CommitModelRateLimitObservationIfVersion(context.Context, int64, string, map[string]any, bool, time.Time, int64, string) (bool, error) {
	r.applied++
	return true, nil
}

// 以下两个方法补齐 modelRateLimitObservationRepository 窄面（共 5 个方法）：
// RateLimitService.observationRepo() 要求整个窄面都实现，否则成功分支的
// ApplyModelRateLimitObservationIfVersion 失败关闭、applied 恒为 0，用例 2 无法断言
// 「成功经版本化入口清除」。写法照 openai_apikey_health_breaker_test.go 的
// probeRateLimitRepo 先例：内存成功桩，嵌入的 accountHealthProbeRepository 为 nil 不被触达。
// 这两个方法派发单骨架未列出，属接口签名补齐，不放宽任何断言。
func (r *jitterProbeRepoFake) GetModelRateLimitEntry(context.Context, int64, string) (map[string]any, error) {
	return nil, nil
}
func (r *jitterProbeRepoFake) CommitModelRateLimitObservation(context.Context, int64, string, map[string]any, bool, time.Time, int64) error {
	return nil
}

func TestUpstreamJitterProbeChannelJitterBranchReschedules15s(t *testing.T) {
	repo := &jitterProbeRepoFake{}
	probe := &AccountHealthRecoveryProbeService{
		accountRepo:  repo,
		recoveryLock: jitterProbeLockFake{},
		cursorExec:   &CursorExecutionService{},
		cursorAgentRunProbe: func(context.Context, *CursorExecutionService, *Account, string, int) (int, string, string, error) {
			return 500, "boom", "", nil
		},
	}
	acc := &Account{ID: 1, Platform: PlatformCursor, Type: AccountTypeOAuth}
	task := ModelProbeTask{AccountID: 1, Scope: "m-model", Entry: map[string]any{
		"reason": UpstreamJitterModelRateLimitReason, "probe_gen": int64(7),
	}}
	probe.probeModelScope(context.Background(), acc, task, "owner")

	var reschedules []ModelProbePatch
	for _, p := range repo.patches {
		if p.Op == ModelProbePatchReschedule {
			reschedules = append(reschedules, p)
		}
	}
	require.Len(t, reschedules, 1, "失败分支必须且仅重排一次；PatchModelRateLimitEntry 签名不匹配则本断言失败")
	require.WithinDuration(t, time.Now().Add(UpstreamJitterProbeCadence), reschedules[0].Due, 3*time.Second)
	require.Zero(t, repo.applied, "失败分支不得触发版本化清除；CommitModelRateLimitObservationIfVersion 签名不匹配则本断言失败")
}

func TestUpstreamJitterProbeChannelJitterBranchClears(t *testing.T) {
	repo := &jitterProbeRepoFake{}
	probe := &AccountHealthRecoveryProbeService{
		accountRepo:  repo,
		recoveryLock: jitterProbeLockFake{},
		cursorExec:   &CursorExecutionService{},
		rateLimit:    &RateLimitService{accountRepo: repo},
		cursorAgentRunProbe: func(context.Context, *CursorExecutionService, *Account, string, int) (int, string, string, error) {
			return 200, "", "hello", nil
		},
	}
	acc := &Account{ID: 1, Platform: PlatformCursor, Type: AccountTypeOAuth}
	task := ModelProbeTask{AccountID: 1, Scope: "m-model", Entry: map[string]any{
		"reason": UpstreamJitterModelRateLimitReason, "probe_gen": int64(7),
	}}
	probe.probeModelScope(context.Background(), acc, task, "owner")

	require.Equal(t, 1, repo.applied, "成功分支必须经版本化入口清除该 scope；CommitModelRateLimitObservationIfVersion 签名不匹配则本断言失败")
	for _, p := range repo.patches {
		require.NotEqual(t, ModelProbePatchReschedule, p.Op, "成功分支不得重排")
	}
}
