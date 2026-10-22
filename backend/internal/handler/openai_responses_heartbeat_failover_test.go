//go:build unit

package handler

// Responses 心跳误阻断换号回归与行为矩阵（方案
// docs-local/responses-heartbeat-failover-fix-plan.md §3.3/§6.2）：
//   - 先以旧签名在修复前运行并按预期断言失败（复现实现漂移；证据
//     docs-local/responses-heartbeat-failover-logs/01-failing-regression-before-fix.txt），
//     修复后随共享签名迁移扩展为完整矩阵测试。
//   - 全部使用真实 UpstreamHeartbeat owner 产生和核算心跳，禁止手写注释伪造累计数；
//     hb.Stop() / waitHeartbeatCommittedForTest 保证 heartbeat 与 handler 写入不并发
//     （既有 stop-and-wait 生命周期，不用任意 sleep）。
//   - 新增用例统一使用 TestOpenAIResponsesHeartbeatFailover 前缀。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// heartbeatOnlyTimeout 构造 CN 首字节超时同型的可换号错误（NextAccountRetry、
// SafeToFailoverAfterWrite=false——生产 CN 504 的默认形态）。
func heartbeatOnlyTimeout() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:        http.StatusGatewayTimeout,
		NextAccountAction: service.NextAccountRetry,
	}
}

// TestOpenAIResponsesHeartbeatFailover_HeartbeatOnlyMustAllowFailover 钉死核心缺陷回归：
// 真实 heartbeat owner 仅提交心跳注释（无语义输出）时，实际 Responses failover guard
// （openAIForwardMayFailover）必须允许进入既有换号流程。
func TestOpenAIResponsesHeartbeatFailover_HeartbeatOnlyMustAllowFailover(t *testing.T) {
	c, rec := newCF524TestContext(t, "/v1/responses")
	before := cf524NormalizeWrittenSize(service.OpenAICompactKeepaliveAdjustedWrittenSize(c))
	timeout := heartbeatOnlyTimeout()
	require.True(t, openAIForwardMayFailover(c, before, 0, timeout), "control: no bytes must permit failover")

	// 真实 heartbeat owner：安装 + 起搏提交一帧注释（stop-and-wait 保证无并发写）。
	hb := installOpenAIHeartbeatForTest(t, c)
	hb.Stop()
	require.NoError(t, hb.Resume(), "Resume 须立即续写一帧心跳注释")
	hb.Stop()
	require.Greater(t, hb.CommittedBytes(), int64(0), "heartbeat owner must have committed bytes")
	require.Equal(t, ": keep-alive\n\n", rec.Body.String(), "only a heartbeat comment was written")

	require.True(t, openAIForwardMayFailover(c, before, 0, timeout),
		"Responses must not treat pure heartbeat bytes as semantic output")
}

// TestOpenAIResponsesHeartbeatFailover_SemanticOutputStillBlocksPlainFailover 钉死
// 写后禁重放边界：真实内容输出后，普通可换号错误（特殊标记 false）不得重放、不得切次号。
func TestOpenAIResponsesHeartbeatFailover_SemanticOutputStillBlocksPlainFailover(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/responses")
	before := cf524NormalizeWrittenSize(service.OpenAICompactKeepaliveAdjustedWrittenSize(c))

	// 真实 heartbeat owner 先提交心跳（错误历史场景），再由 handler 写语义事件。
	hb := installOpenAIHeartbeatForTest(t, c)
	hb.Stop()
	baseline := int(hb.CommittedBytes())

	// 语义输出必须是有效内容事件而非任意注释。
	_, err := fmt.Fprint(c.Writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
	require.NoError(t, err)
	hb.Stop()

	timeout := heartbeatOnlyTimeout()
	require.False(t, openAIForwardMayFailover(c, before, baseline, timeout),
		"semantic output after baseline must block plain failover replay")
	// 既有特许：SafeToFailoverAfterWrite=true 时仍允许（原有授权边界不回退）。
	privileged := &service.UpstreamFailoverError{SafeToFailoverAfterWrite: true}
	require.True(t, openAIForwardMayFailover(c, before, baseline, privileged),
		"SafeToFailoverAfterWrite grant must survive heartbeat-baseline guard")
}

// TestOpenAIResponsesHeartbeatFailover_MatrixCorners 钉死 §3.3 矩阵四角：
// 无 owner/无输出、-1/0 哨兵、compact-only、非零心跳基线。
func TestOpenAIResponsesHeartbeatFailover_MatrixCorners(t *testing.T) {
	timeout := heartbeatOnlyTimeout()

	// ① 无输出、无 owner（含 -1/0 哨兵组合）：与原来相同，clean。
	cNone, _ := newCF524TestContext(t, "/v1/responses")
	require.Equal(t, -1, service.OpenAICompactKeepaliveAdjustedWrittenSize(cNone))
	require.True(t, openAIForwardMayFailover(cNone, -1, 0, timeout))
	require.True(t, openAIForwardMayFailover(cNone, 0, 0, timeout))

	// ② compact-only 保活：adjusted 口径扣除后无语义字节 → clean，不重复扣字节。
	cCompact, _ := newCF524TestContext(t, "/v1/responses")
	cCompact.Set(cf524OpenAICompactKeepaliveKey, struct{}{}) // 模拟 compact 已装（互斥，无 hb owner）
	beforeCompact := cf524NormalizeWrittenSize(service.OpenAICompactKeepaliveAdjustedWrittenSize(cCompact))
	require.True(t, openAIForwardMayFailover(cCompact, beforeCompact, 0, timeout))

	// ③ 非零 attempt 前心跳累计值：基线扣除后本轮无新增 → clean（前轮累计不抵本轮）。
	cPrev, _ := newCF524TestContext(t, "/v1/responses")
	hbPrev := installOpenAIHeartbeatForTest(t, cPrev)
	hbPrev.Stop()
	require.NoError(t, hbPrev.Resume()) // 显式提交一帧（头提交 committed=0，帧经 Resume 立写）
	hbPrev.Stop()
	prevCommitted := int(hbPrev.CommittedBytes())
	require.Greater(t, prevCommitted, 0)
	require.True(t, openAIForwardMayFailover(cPrev, cf524NormalizeWrittenSize(service.OpenAICompactKeepaliveAdjustedWrittenSize(cPrev)), prevCommitted, timeout),
		"nonzero baseline with no new heartbeat delta must stay clean")

	// ④ 连续两次尝试按增量核算：第二次 attempt 基线取当次 Forward 前值，
	//    第二次期间新增心跳帧由增量扣除，仍 clean。
	hbPrev.Stop()
	require.NoError(t, hbPrev.Resume()) // 第二次 attempt 期间新增一帧心跳
	hbPrev.Stop()
	beforeSecond := cf524NormalizeWrittenSize(service.OpenAICompactKeepaliveAdjustedWrittenSize(cPrev))
	baselineSecond := int(hbPrev.CommittedBytes())
	require.Greater(t, baselineSecond, prevCommitted)
	require.True(t, openAIForwardMayFailover(cPrev, beforeSecond, baselineSecond, timeout),
		"second attempt must judge by per-attempt delta, not request-total")

	// 第二次 attempt 期间既有心跳又有语义输出：增量不再相等 → 不 clean。
	_, err := fmt.Fprint(cPrev.Writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n")
	require.NoError(t, err)
	hbPrev.Stop()
	require.False(t, openAIForwardMayFailover(cPrev, beforeSecond, baselineSecond, timeout),
		"semantic output in the same attempt must break the delta equality")
}

// TestOpenAIResponsesHeartbeatFailover_NextAccountStopNotBypassed 钉死停止条件不被绕过：
// NextAccountStop（预算耗尽同型）在 clean 时仍必须终止，不得因 clean 判定继续换号。
func TestOpenAIResponsesHeartbeatFailover_NextAccountStopNotBypassed(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/responses")
	// clean 判定只解除"写出边界误拦"；换号仍须通过 ShouldRetryNextAccount 等既有门禁
	// （openAIForwardMayFailover 本身不消费 NextAccountAction，停止语义由 handler 循环
	// 判定——此处钉死 guard 返回 true 不会改变错误的停止语义载体）。
	hb := installOpenAIHeartbeatForTest(t, c)
	hb.Stop()
	before := cf524NormalizeWrittenSize(service.OpenAICompactKeepaliveAdjustedWrittenSize(c))
	stopErr := &service.UpstreamFailoverError{
		StatusCode:        http.StatusGatewayTimeout,
		NextAccountAction: service.NextAccountStop,
	}
	// guard 层面 clean=true 允许到达既有门禁；门禁读 NextAccountAction 终止。
	require.True(t, openAIForwardMayFailover(c, before, 0, stopErr),
		"clean write-boundary must not itself veto failover")
	require.False(t, stopErr.ShouldRetryNextAccount(),
		"NextAccountStop must keep short-circuiting the retry loop (stop condition not bypassed)")
}

// ───────── 真实 handler 两账号路径（§6.2：经真实 Responses handler → Forward → 失败门 → 次号） ─────────
//
// 路径事实（实施前逐点核源）：
//   - fixture 账号 openai_passthrough=true + APIKey + 504：passthrough 透传分支
//     shouldFailoverOpenAIPassthroughResponse 对 APIKey 账号放行 504（5xx 集合），
//     返回 NextAccountRetry 的 UpstreamFailoverError（openai_gateway_passthrough.go:787/:815）。
//   - 心跳在 Forward 中真实起搏：forwardOpenAIPassthrough 经 doOpenAIUpstreamWithGuard
//     每次尝试 hb.Resume()；桩在首号 Do 内等待 CommittedBytes>0（心跳已提交）再返回 504，
//     保证判定时基线之后的增量恰为心跳字节；service 层错误分支已 stop-and-wait，
//     handler 判定时无并发写。
//   - 耗尽终态：心跳已提交 SSE 200 → streamStarted=true → 流内 response.failed
//     恰一次（writeResponsesFailedSSE），非 JSON 502。

// openAIResponsesHeartbeatFailoverUpstream 记录账号序列；首号 Do 内等待真实 heartbeat
// owner 提交字节后才返回预置应答（answers 按 Do 次序消费，缺省 504 可换号失败）。
type openAIResponsesHeartbeatFailoverUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	answers    []*http.Response
}

func (u *openAIResponsesHeartbeatFailoverUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	first := len(u.accountIDs) == 1
	u.mu.Unlock()
	if first {
		// 等待真实心跳 owner 提交字节（轮询 + 2s deadline，同 waitHeartbeatCommittedForTest
		// 语义）。owner 由 handler 入口安装在本请求 context（经 WithoutCancel 保持）。
		hb, _ := service.UpstreamHeartbeatFromContext(req.Context())
		if hb != nil {
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if hb.CommittedBytes() > 0 {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
	}
	u.mu.Lock()
	var resp *http.Response
	if len(u.answers) > 0 {
		resp = u.answers[0]
		u.answers = u.answers[1:]
	}
	u.mu.Unlock()
	if resp != nil {
		return resp, nil
	}
	return heartbeatFailoverTimeoutResponse()
}

func heartbeatFailoverTimeoutResponse() (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusGatewayTimeout,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"type":"upstream_error","message":"upstream first byte timeout"}}`)),
	}, nil
}

func (u *openAIResponsesHeartbeatFailoverUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

// newOpenAIResponsesHeartbeatStreamContext 构造流式 Responses 请求 context，并把与生产
// 一致的 guard/心跳配置注入 fixture handler（复用既有账号 repo stub；账号带
// openai_passthrough=true，见 openai_ws_failover_account_repo…… 同型）。
func newOpenAIResponsesHeartbeatStreamContext(t *testing.T, handler *OpenAIGatewayHandler, groupID int64) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses",
		bytes.NewBufferString(`{"model":"gpt-5.6-sol","input":"hello","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      1805,
		GroupID: &groupID,
		User:    &service.User{ID: 1705, Status: service.StatusActive},
		Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 1705, Concurrency: 0})
	_ = handler
	return c, rec
}

// heartbeatSSESuccess 是次号返回的合法成功 SSE 流（完整事件序列，usage 齐全）。
const heartbeatSSESuccess = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_hb_ok"}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_hb_ok","model":"gpt-5.6-sol","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"

// newOpenAIResponsesHeartbeatGatewayHandler 装配真实 handler + 真实网关服务：
// 两账号 APIKey passthrough fixture + 非零 guard/心跳配置（心跳 owner 必须真实安装），
// 复用既有 openAIWSFailoverHandlerAccountRepoStub 账号桩与并发全开 mock。
func newOpenAIResponsesHeartbeatGatewayHandler(t *testing.T, upstream service.HTTPUpstream) *OpenAIGatewayHandler {
	t.Helper()
	accounts := []service.Account{
		{
			ID: 9915, Name: "hb-failover-first", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
			Concurrency: 1, Priority: 1,
			Credentials: map[string]any{"api_key": "sk-hb-1", "base_url": "https://hb-unused.invalid"},
			Extra:       map[string]any{"openai_passthrough": true},
		},
		{
			ID: 9916, Name: "hb-failover-second", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
			Concurrency: 1, Priority: 2,
			Credentials: map[string]any{"api_key": "sk-hb-2", "base_url": "https://hb-unused.invalid"},
			Extra:       map[string]any{"openai_passthrough": true},
		},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	// 与生产一致的 guard/心跳配置：安装点据此装配预算快照 + 心跳 owner。
	cfg.Gateway.UpstreamFirstByteGuardSeconds = 30
	cfg.Gateway.UpstreamHeartbeatDelaySeconds = 1

	repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	gatewaySvc := service.NewOpenAIGatewayService(
		repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCacheSvc, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, /* cursorExecution */ // visionRouting
	)
	h := NewOpenAIGatewayHandler(
		gatewaySvc, nil, nil, service.NewConcurrencyService(nil), billingCacheSvc,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil, cfg,
	)
	return h
}

// TestOpenAIResponsesHeartbeatFailover_RealHandlerFirstHeartbeatTimeoutSecondSucceeds
// 真实 handler 端到端：流式请求，首号 Forward 期间仅真实心跳提交后返回可换号 504，
// 次号成功。断言：账号序列 [9915,9916]、无提前 response.failed、成功终态恰好一次。
func TestOpenAIResponsesHeartbeatFailover_RealHandlerFirstHeartbeatTimeoutSecondSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIResponsesHeartbeatFailoverUpstream{
		answers: []*http.Response{
			{
				StatusCode: http.StatusGatewayTimeout,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"type":"upstream_error","message":"upstream first byte timeout"}}`)),
			},
			{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewBufferString(heartbeatSSESuccess)),
			},
		},
	}
	handler := newOpenAIResponsesHeartbeatGatewayHandler(t, upstream)
	c, rec := newOpenAIResponsesHeartbeatStreamContext(t, handler, 4205)

	handler.Responses(c)

	require.Equal(t, []int64{9915, 9916}, upstream.calls(),
		"heartbeat-only failure must not block switching to the second account")
	require.NotContains(t, rec.Body.String(), "response.failed", "no premature failure event")
	require.Equal(t, 1, strings.Count(rec.Body.String(), "data: {\"type\":\"response.completed\""), "exactly one success terminal data event")
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestOpenAIResponsesHeartbeatFailover_RealHandlerBothFailBoundedTerminate
// 真实 handler：两号都在心跳提交后返回可换号 504——原有上限内结束；心跳已提交
// SSE 200，终态为流内 response.failed 恰一次，无 JSON 混入 SSE、无无限重试。
func TestOpenAIResponsesHeartbeatFailover_RealHandlerBothFailBoundedTerminate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIResponsesHeartbeatFailoverUpstream{}
	handler := newOpenAIResponsesHeartbeatGatewayHandler(t, upstream)
	c, rec := newOpenAIResponsesHeartbeatStreamContext(t, handler, 4205)

	handler.Responses(c)

	require.Equal(t, []int64{9915, 9916}, upstream.calls(), "both accounts tried once within existing bounds")
	require.Equal(t, http.StatusOK, rec.Code, "heartbeat committed SSE 200; terminal must be in-stream")
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed\n"),
		"in-stream terminal failure written exactly once")
	require.NotContains(t, rec.Body.String(), `"object":"chat.completion"`, "no stray JSON mixed into SSE")
}
