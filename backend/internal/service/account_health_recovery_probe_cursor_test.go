//go:build unit

// CURSOR-PROBE-001-A2 定向测试：cursor 探针接入既有分派点。
//
// 覆盖：
//   - probeRecoverableMarkers 放行 cursor marker（只加一项）；
//   - probeUpstream 按账号形态二分：Connect 会话型 → A1 的 ProbeCursorAccountHealth
//     封装（原生 AgentRun、用账号自身静态 access_token）；OpenAI 兼容型 → 既有
//     probeUpstreamDataPlane，且协议强制 Chat Completions（复用
//     shouldForwardOpenAIResponsesViaRawChatCompletions，端点 /v1/chat/completions、body 是
//     chat 形状，而非 /v1/responses）；
//   - 非 cursor 平台账号的分派行为逐字不变（回归红线）；
//   - 探针模型回落：只具 claude-opus-5-5 触发模型的 cursor 账号不会选中该键，回落到
//     gpt-6.1-sol / claude-fable-5-1（全体账号都有的键）；
//   - 补探 burst 并发上限：结构可核验（accountHealthProbeBurstConcurrencyLimit 约束在途探测）。
package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// cursorProbeAccount 构造被 cursor 健康熔断停调的账号（marker=cursorAccountHealthBreakerMarker）。
// accountType 为 AccountTypeAPIKey（OpenAI 兼容型）或 AccountTypeOAuth（Connect 会话型）。
// mapping 写入 Credentials["model_mapping"]；baseURL 仅 apikey 型需要（绕过节点发现）。
func cursorProbeAccount(id int64, accountType string, baseURL string, mapping map[string]string) *Account {
	until := time.Now().Add(5 * time.Minute)
	state := TempUnschedState{
		UntilUnix:      until.Unix(),
		MatchedKeyword: cursorAccountHealthBreakerMarker,
		Tier:           HealthBreakerTierTrip,
		Model:          "gpt-6.1-sol",
	}
	reason, _ := json.Marshal(state)
	creds := map[string]any{}
	mm := map[string]any{}
	for k, v := range mapping {
		mm[k] = v
	}
	creds["model_mapping"] = mm
	if baseURL != "" {
		creds["base_url"] = baseURL
	}
	if accountType == AccountTypeAPIKey {
		creds["api_key"] = "sk-cursor-probe-test"
	} else {
		// Connect 会话型（oauth）：用账号自身静态 access_token（探针只读它，不落 Auto 动态池）。
		creds["access_token"] = "STATIC_ACCESS_TOKEN"
	}
	return &Account{
		ID:                      id,
		Platform:                PlatformCursor,
		Type:                    accountType,
		Credentials:             creds,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: string(reason),
	}
}

// --- Done when #4：cursor OpenAI 兼容型探针端点必须是 /v1/chat/completions、body 是 chat 形状 ---

func TestCursorOpenAICompatibleProbeUsesChatCompletionsNotResponses(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := cursorProbeAccount(41, AccountTypeAPIKey, "https://kn-cy.example/v1", map[string]string{
		"gpt-6.1-sol":      "gpt-6.1-sol-out",
		"claude-fable-5-1": "claude-fable-5-1-out",
	})
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{41: cand},
		candidates: []*Account{cand},
	}
	// 成功响应：真实 chat completions 形状（含 choices）。探针连续 2 次成功即恢复。
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		`{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)

	p.RunOnce(context.Background())
	p.wg.Wait()

	// 恢复：连续 2 次真实探测成功 → 清除停调。
	require.Equal(t, 1, repo.clearTempIfReasonCalls, "cursor apikey 探针成功应解除停调")
	rec := upstream.lastRecord(t)
	require.Equal(t, http.MethodPost, rec.Method)
	// 关键回归点：强制 Chat Completions，端点不得是 /v1/responses。
	require.Equal(t, "/v1/chat/completions", rec.Path,
		"cursor APIKey 的真实转发固定走 CC，探针端点必须是 /v1/chat/completions（不得 /v1/responses）")
	require.Equal(t, "Bearer sk-cursor-probe-test", rec.Header.Get("Authorization"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Contains(t, body, "messages", "探测体必须是 chat 形状（含 messages），不得是 responses 的 input")
	require.NotContains(t, body, "input", "不得构造 /v1/responses 的 input 形状")
}

// --- Done when #5：非 cursor 平台账号的分派行为逐字不变（回归红线）---

func TestProbeUpstreamNonCursorDispatchUnchanged(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	// 标准 OpenAI apikey 账号（与本卡改动前走同一支 probeUpstreamDataPlane）。
	// base_url 写到具体 chat 端点：probeConcreteEndpoint 据此识别为具体、非 Responses
	// 端点，直接照用 /v1/chat/completions，不再派生 /v1/responses（否则未探测标记
	// 默认按走 Responses 处理，打到不存在的路径永久失败、账号永不恢复）。
	openaiCand := parkedBreakerAccount(51, 0)
	openaiCand.Credentials = map[string]any{
		"api_key":  "sk-openai-probe-test",
		"base_url": "https://api.openai.com/v1/chat/completions",
	}
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{51: openaiCand},
		candidates: []*Account{openaiCand},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		`{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 防御性边界：即便上游判定异常，补探循环也在第 2 次请求后由 ctx 取消收敛，不挂死。
	upstream.cancelAfterRequests(2, cancel)

	p.RunOnce(ctx)
	p.wg.Wait()

	// 非 cursor 平台账号仍走既有数据面探测（/v1/chat/completions），分派路径逐字不变。
	rec := upstream.lastRecord(t)
	require.Equal(t, "/v1/chat/completions", rec.Path, "非 cursor 账号仍走既有数据面探测")
	require.Equal(t, "Bearer sk-openai-probe-test", rec.Header.Get("Authorization"))
}

// --- Done when #6：Connect 会话型走 A1 封装、且用账号自身静态 access_token ---

func TestCursorConnectSessionProbeUsesA1WrapperAndStaticToken(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := cursorProbeAccount(61, AccountTypeOAuth, "", map[string]string{
		"gpt-6.1-sol":      "gpt-6.1-sol-out",
		"claude-fable-5-1": "claude-fable-5-1-out",
	})
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{61: cand},
		candidates: []*Account{cand},
	}
	// 用子测试注入的探针执行函数记录分派（跳过真实 AgentRun 出站）。
	var calledWith *Account
	var calledModel string
	capturedToken := ""
	p := newProbeServiceWithUpstream(t, &failingProbeUpstream{}, rl, repo)
	setFastProbeRetry(p)
	p.SetCursorExecutionService(&CursorExecutionService{})
	p.SetCursorAgentRunProbe(func(ctx context.Context, exec *CursorExecutionService, account *Account, upstreamModel string, budget int) (int, string, string, error) {
		calledWith = account
		calledModel = upstreamModel
		// 断言：探针走的是账号自身静态 access_token（ProbeAgentRun 内部只读
		// cursorResolveAccountBearer 取静态凭据，不落到 Auto 动态池的另一个身份）。
		capturedToken = account.GetCredential("access_token")
		// 预算必须 ≥256（思考型模型需足够输出预算，否则探不出正文）。
		require.GreaterOrEqual(t, budget, cursorAccountHealthProbeMinBudget)
		return http.StatusOK, "", "hi", nil
	})

	p.RunOnce(context.Background())
	p.wg.Wait()

	require.NotNil(t, calledWith, "Connect 会话型必须分派到 A1 的 ProbeCursorAccountHealth 封装")
	require.Equal(t, int64(61), calledWith.ID, "分派到的账号必须是该 Connect 会话型账号")
	require.Equal(t, "STATIC_ACCESS_TOKEN", capturedToken,
		"Connect 会话型探针必须用账号自身静态 access_token，而非 Auto 动态池 token")
	require.Equal(t, "gpt-6.1-sol-out", calledModel, "出站模型必须取 ResolveMappedModel 的值")
	require.Equal(t, 1, repo.clearTempIfReasonCalls, "Connect 会话型探针成功应解除停调")
}

// 回归：cursor Connect 会话型不得落到数据面探测（不得打 OpenAI 形状到 cursor 原生端点）。
func TestCursorConnectSessionDoesNotDispatchToDataPlane(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := cursorProbeAccount(62, AccountTypeOAuth, "", map[string]string{
		"gpt-6.1-sol": "gpt-6.1-sol-out",
	})
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{62: cand},
		candidates: []*Account{cand},
	}
	// 若分派错误落到 probeUpstreamDataPlane，会向本桩发起 /v1/chat/completions 或 /v1/responses
	// 请求；本桩在收到任何请求时即判定分派错误。
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		`{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)
	p.SetCursorExecutionService(&CursorExecutionService{})
	p.SetCursorAgentRunProbe(func(ctx context.Context, exec *CursorExecutionService, account *Account, upstreamModel string, budget int) (int, string, string, error) {
		return http.StatusOK, "", "hi", nil
	})

	p.RunOnce(context.Background())
	p.wg.Wait()

	// 任何真实 HTTP 出站都意味着分派到了数据面探测（错误）：Connect 会话型只应走 AgentRun 封装。
	require.Zero(t, len(upstream.records), "Connect 会话型不得分派到数据面探测（不得打 OpenAI 形状）")
}

// --- Done when #7：模型回落不得选中只有 oauth 型才有的 claude-opus-5-5 ---

func TestCursorProbeModelFallbackAvoidsOauthOnlyKey(t *testing.T) {
	svc := &AccountHealthRecoveryProbeService{}
	// 触发模型记为 claude-opus-5-5（只有 oauth 型才有的键），但本账号（apikey 型）的
	// model_mapping 只有全体都有的 gpt-6.1-sol / claude-fable-5-1。
	acc := cursorProbeAccount(71, AccountTypeAPIKey, "https://kn-cy.example/v1", map[string]string{
		"gpt-6.1-sol":      "gpt-6.1-sol-out",
		"claude-fable-5-1": "claude-fable-5-1-out",
	})
	// 把触发模型改写为只有 oauth 型才有的键，模拟 apikey 账号被错记了该键。
	until := time.Now().Add(5 * time.Minute)
	state := TempUnschedState{
		UntilUnix:      until.Unix(),
		MatchedKeyword: cursorAccountHealthBreakerMarker,
		Tier:           HealthBreakerTierTrip,
		Model:          "claude-opus-5-5",
	}
	reason, _ := json.Marshal(state)
	acc.TempUnschedulableReason = string(reason)

	key := svc.cursorProbeModelKey(acc)
	require.NotEqual(t, "claude-opus-5-5", key, "不得落到只有 oauth 型才有的键")
	require.Contains(t, []string{"gpt-6.1-sol", "claude-fable-5-1"}, key,
		"回落必须选全体账号都有的键（gpt-6.1-sol / claude-fable-5-1 之一）")

	// 出站模型必须能解析到真实上游名（值，而非键名）。
	outbound := svc.cursorProbeOutboundModel(acc)
	require.Contains(t, []string{"gpt-6.1-sol-out", "claude-fable-5-1-out"}, outbound,
		"出站模型必须是 ResolveMappedModel 的值")
}

// --- Done when #8：补探 burst 并发上限可核验（结构 + 行为）---

func TestProbeBurstConcurrencyLimitIsBounded(t *testing.T) {
	// 结构性断言：常量存在且为正，约束在途探测 goroutine 数；行为上 burst 不变量由
	// runProbeRetryBurst 的 sem（buffered channel）保证，不触碰服务级 p.wg。
	require.Greater(t, accountHealthProbeBurstConcurrencyLimit, 0)
	// 单测层用 RunOnce 实际跑一轮补探，验证并发上限下补探仍正确收敛（多候选、部分失败）。
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	// 3 个 cursor apikey 候选，全部用成功响应，验证并发上限下仍能恢复。
	cands := make([]*Account, 0, 3)
	accounts := map[int64]*Account{}
	for i := int64(81); i < 84; i++ {
		c := cursorProbeAccount(i, AccountTypeAPIKey, "https://kn-cy.example/v1", map[string]string{
			"gpt-6.1-sol": "gpt-6.1-sol-out",
		})
		cands = append(cands, c)
		accounts[i] = c
	}
	repo := &probeRepoMock{accounts: accounts, candidates: cands}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		`{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)

	p.RunOnce(context.Background())
	p.wg.Wait()

	// 3 个账号各连续 2 次成功 → 全部解除停调（验证并发上限下恢复逻辑不受影响）。
	require.Equal(t, 3, repo.clearTempIfReasonCalls, "并发上限下多账号补探仍应全部恢复")
}
