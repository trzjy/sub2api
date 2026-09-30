//go:build unit

package service

// 文件：internal/service/cf524_observation_test.go
//
// CF524 G5 可观测事件与关联契约单测（方案 v8.2 §4 第 8 条 / 派发单 G5 v2）。
//
// 覆盖：
//   1. guard 触发 attempt 事件含全部字段（path/platform/stream/attempt/outcome/
//      elapsed_ms/request_id + phase），且 outcome 枚举三分支取值正确；
//   2. 心跳事件字段正确且每请求只发一次；
//   3. attempt 事件与终态关联事件同 request_id（含 subsequent_upstream_error 分支）；
//   4. 四个非 OpenAI 挂点的共享执行器单点埋点集成（悬挂触发 → 事件齐全；健康路径
//      零处理事件且判别器字段已记录）；
//   5. OpenAI 原生路径零事件的语义等价证明：不经过四挂点 → 无 tracker → 零事件。

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// —— slog 捕获 ——

// cf524LogCapture 是线程安全的 slog 事件收集器（JSON handler → 逐行解析）。
type cf524LogCapture struct {
	mu     sync.Mutex
	events []map[string]any
}

func (c *cf524LogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		c.events = append(c.events, ev)
	}
	return len(p), nil
}

// cf524CaptureLogs 把 slog 默认 handler 临时替换为捕获器，测试结束恢复。
func cf524CaptureLogs(t *testing.T) *cf524LogCapture {
	t.Helper()
	cap := &cf524LogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(cap, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return cap
}

func (c *cf524LogCapture) byMessage(msg string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, 0)
	for _, ev := range c.events {
		if got, _ := ev["msg"].(string); got == msg {
			out = append(out, ev)
		}
	}
	return out
}

func (c *cf524LogCapture) guardEvents() []map[string]any {
	return c.byMessage("gateway_first_byte_guard_triggered")
}

func (c *cf524LogCapture) heartbeatEvents() []map[string]any {
	return c.byMessage("gateway_upstream_heartbeat_started")
}

func (c *cf524LogCapture) all() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, len(c.events))
	copy(out, c.events)
	return out
}

// —— 测试夹具 ——

// cf524ObsGinContext 构造带 request_id 的 gin context（同 G2b 安装后的实况：请求
// context 携带 ctxkey.RequestID）。
func cf524ObsGinContext(requestID string) *gin.Context {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestID, requestID))
	return c
}

// —— 用例 1：attempt 事件字段完整 + outcome 三分支 ——

// guard 触发但剩余预算充足 → attempt 事件 outcome=failover_success，字段齐全。
func TestCF524ObservationGuardEventAttemptFieldsAndFailoverSuccess(t *testing.T) {
	cap := cf524CaptureLogs(t)
	c := cf524ObsGinContext("rid-g5-success")

	cf524ObserveGuardTriggered(c, context.Background(), PlatformKimi, true, 2, 90001, 30*time.Second)

	events := cap.guardEvents()
	require.Len(t, events, 1, "剩余预算充足时只发一条 attempt 事件（终态延后到链接束）")
	ev := events[0]
	require.Equal(t, "/v1/messages", ev["path"])
	require.Equal(t, PlatformKimi, ev["platform"])
	require.Equal(t, true, ev["stream"])
	require.Equal(t, float64(2), ev["attempt"])
	require.Equal(t, guardOutcomeFailoverSuccess, ev["outcome"])
	require.Equal(t, float64(90001), ev["elapsed_ms"])
	require.Equal(t, "rid-g5-success", ev["request_id"])
	require.Equal(t, guardEventPhaseAttempt, ev["phase"])
}

// 剩余预算 < 最小可行窗口 → attempt + 终态均为 failover_exhausted（同一次触发内闭合）。
func TestCF524ObservationGuardEventFailoverExhausted(t *testing.T) {
	cap := cf524CaptureLogs(t)
	c := cf524ObsGinContext("rid-g5-exhausted")

	cf524ObserveGuardTriggered(c, context.Background(), PlatformDeepseek, false, 1, 100, time.Second)

	events := cap.guardEvents()
	require.Len(t, events, 2, "耗尽即时终态：attempt + terminal 两条")
	require.Equal(t, guardEventPhaseAttempt, events[0]["phase"])
	require.Equal(t, guardOutcomeFailoverExhausted, events[0]["outcome"])
	require.Equal(t, guardEventPhaseTerminal, events[1]["phase"])
	require.Equal(t, guardOutcomeFailoverExhausted, events[1]["outcome"])
	require.Equal(t, "rid-g5-exhausted", events[0]["request_id"])
	require.Equal(t, "rid-g5-exhausted", events[1]["request_id"])
	require.Equal(t, false, events[0]["stream"])
}

// 客户端已断开（ctx 已取消）→ attempt + 终态均为 client_gone。
func TestCF524ObservationGuardEventClientGone(t *testing.T) {
	cap := cf524CaptureLogs(t)
	c := cf524ObsGinContext("rid-g5-clientgone")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cf524ObserveGuardTriggered(c, canceled, PlatformZhipu, true, 3, 10, 60*time.Second)

	events := cap.guardEvents()
	require.Len(t, events, 2)
	require.Equal(t, guardOutcomeClientGone, events[0]["outcome"])
	require.Equal(t, guardEventPhaseAttempt, events[0]["phase"])
	require.Equal(t, guardOutcomeClientGone, events[1]["outcome"])
	require.Equal(t, guardEventPhaseTerminal, events[1]["phase"])
}

// —— 用例 2：终态关联（subsequent_upstream_error 分支） ——

// guard 交棒换号后，下一 attempt 以非 failover 错误终结 → 终态 subsequent_upstream_error，
// 同 request_id；可继续换号的 failover 错误保持 pending 不误发终态。
func TestCF524ObservationTerminalSubsequentUpstreamError(t *testing.T) {
	cap := cf524CaptureLogs(t)
	c := cf524ObsGinContext("rid-g5-subsequent")

	cf524ObserveGuardTriggered(c, context.Background(), PlatformKimi, true, 1, 500, 30*time.Second)
	require.Len(t, cap.guardEvents(), 1)

	// 可继续换号的 failover 错误：保持 pending（不误发终态）。
	cf524ResolveGuardOutcome(c, context.Background(), &UpstreamFailoverError{NextAccountAction: NextAccountRetry})
	require.Len(t, cap.guardEvents(), 1, "可继续换号的 failover 错误不得提前落终态")

	// 换号链继续：第二次 guard 触发。
	cf524ObserveGuardTriggered(c, context.Background(), PlatformKimi, true, 2, 700, 25*time.Second)
	require.Len(t, cap.guardEvents(), 2)

	// 下一 attempt 以非 failover 错误终结 → 两条 pending 各落一条终态（同 outcome/request_id）。
	cf524ResolveGuardOutcome(c, context.Background(), context.DeadlineExceeded)
	events := cap.guardEvents()
	require.Len(t, events, 4)
	require.Equal(t, guardEventPhaseTerminal, events[2]["phase"])
	require.Equal(t, guardOutcomeSubsequentUpstreamError, events[2]["outcome"])
	require.Equal(t, guardEventPhaseTerminal, events[3]["phase"])
	require.Equal(t, guardOutcomeSubsequentUpstreamError, events[3]["outcome"])
	for _, ev := range events {
		require.Equal(t, "rid-g5-subsequent", ev["request_id"])
	}
	require.Equal(t, float64(1), events[0]["attempt"])
	require.Equal(t, float64(2), events[1]["attempt"], "attempt 事件按挂点传值记录")
}

// 已耗尽换号的 *UpstreamFailoverError（ShouldRetryNextAccount=false）必须立即落
// failover_exhausted 终态，不得让观测链断裂（pending 悬挂）。
func TestCF524ObservationTerminalFailoverExhaustedFromError(t *testing.T) {
	cap := cf524CaptureLogs(t)
	c := cf524ObsGinContext("rid-g5-fel")

	cf524ObserveGuardTriggered(c, context.Background(), PlatformKimi, true, 1, 500, 30*time.Second)
	require.Len(t, cap.guardEvents(), 1)

	cf524ResolveGuardOutcome(c, context.Background(), newUpstreamFirstByteTimeoutError(0))
	events := cap.guardEvents()
	require.Len(t, events, 2, "耗尽错误必须落终态关联事件")
	require.Equal(t, guardEventPhaseTerminal, events[1]["phase"])
	require.Equal(t, guardOutcomeFailoverExhausted, events[1]["outcome"])
	require.Equal(t, "rid-g5-fel", events[1]["request_id"])
}

// 请求最终成功（err==nil）→ 终态 failover_success。
func TestCF524ObservationTerminalFailoverSuccessOnNilError(t *testing.T) {
	cap := cf524CaptureLogs(t)
	c := cf524ObsGinContext("rid-g5-ok")

	cf524ObserveGuardTriggered(c, context.Background(), PlatformKimi, true, 1, 500, 30*time.Second)
	cf524ResolveGuardOutcome(c, context.Background(), nil)

	events := cap.guardEvents()
	require.Len(t, events, 2)
	require.Equal(t, guardOutcomeFailoverSuccess, events[0]["outcome"])
	require.Equal(t, guardEventPhaseAttempt, events[0]["phase"])
	require.Equal(t, guardOutcomeFailoverSuccess, events[1]["outcome"])
	require.Equal(t, guardEventPhaseTerminal, events[1]["phase"])
}

// 请求出口兜底（账号耗尽 → 既有 FailoverExhausted）也必须落终态，避免链断裂。
func TestCF524ObservationResolvePendingAtRequestExit(t *testing.T) {
	cap := cf524CaptureLogs(t)
	c := cf524ObsGinContext("rid-g5-exit")

	cf524ObserveGuardTriggered(c, context.Background(), PlatformKimi, true, 1, 500, 30*time.Second)
	cf524ObserveGuardTriggered(c, context.Background(), PlatformKimi, true, 2, 600, 28*time.Second)
	require.Len(t, cap.guardEvents(), 2)

	ResolvePendingGuardObservations(c)
	events := cap.guardEvents()
	require.Len(t, events, 4)
	require.Equal(t, guardOutcomeFailoverExhausted, events[2]["outcome"])
	require.Equal(t, guardOutcomeFailoverExhausted, events[3]["outcome"])
	require.Equal(t, "rid-g5-exit", events[3]["request_id"])

	// 幂等：重复调用不再产出。
	ResolvePendingGuardObservations(c)
	require.Len(t, cap.guardEvents(), 4)
}

// —— 用例 3：心跳事件 ——

func TestCF524ObservationHeartbeatEventFieldsAndOnce(t *testing.T) {
	cap := cf524CaptureLogs(t)
	c := cf524ObsGinContext("rid-g5-hb")

	cf524NoteHeartbeatStarted(c, PlatformZhipu, 60)
	cf524NoteHeartbeatStarted(c, PlatformZhipu, 60) // 幂等：每请求只一次

	events := cap.heartbeatEvents()
	require.Len(t, events, 1)
	ev := events[0]
	require.Equal(t, "/v1/messages", ev["path"])
	require.Equal(t, PlatformZhipu, ev["platform"])
	require.Equal(t, float64(60000), ev["heartbeat_delay_ms"])
	require.Equal(t, "rid-g5-hb", ev["request_id"])
}

// —— 用例 4：判别器字段（MarkUpstreamHeadersReceived / UpstreamHeadersReceivedMs） ——

func TestCF524ObservationUpstreamHeadersReceivedDiscriminator(t *testing.T) {
	c := cf524ObsGinContext("rid-g5-hdr")
	entry := time.Now().Add(-250 * time.Millisecond)

	if _, ok := UpstreamHeadersReceivedMs(c, entry); ok {
		t.Fatal("未收到响应头时不得产出判别器字段")
	}

	MarkUpstreamHeadersReceived(c, time.Now())
	MarkUpstreamHeadersReceived(c, time.Now().Add(time.Second)) // first-wins

	ms, ok := UpstreamHeadersReceivedMs(c, entry)
	require.True(t, ok)
	require.GreaterOrEqual(t, ms, int64(200))
	require.Less(t, ms, int64(900), "first-wins：第二次写入不得覆盖首个时刻")
}

// —— 用例 5：共享执行器单点埋点集成（挂点 1 悬挂形态） ——

// 上游悬挂 > guard：共享执行器发 attempt 事件 + 心跳事件 + 响应头未到达（无判别器值）。
func TestCF524ObservationMount1HangEmitsGuardAndHeartbeatEvents(t *testing.T) {
	cap := cf524CaptureLogs(t)
	upstream := &g2aMockUpstream{}
	svc := g2aService(upstream)
	body := g2aStreamBody("claude-sonnet-5")
	parsed := g2aParsed(t, body)
	_, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)
	// 模拟 RequestLogger 中间件：请求 context 携带 request_id。
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestID, "rid-g5-mount1"))

	snap := g2aSnapshot(1, 1, 30*time.Second)
	hb := g2aHeartbeat(context.Background(), obs, 60*time.Millisecond, 40*time.Millisecond)
	ctx := g2aInject(context.Background(), snap, hb)

	_, err := svc.Forward(ctx, c, g2aAnthropicOAuthAccount(), parsed)
	var fe *UpstreamFailoverError
	require.ErrorAs(t, err, &fe)
	require.True(t, fe.ShouldRetryNextAccount(), "剩余预算 ≥5s：交棒换号，终态延后")

	guardEvents := cap.guardEvents()
	require.Len(t, guardEvents, 1, "单次 attempt 触发一条 attempt 事件")
	require.Equal(t, guardEventPhaseAttempt, guardEvents[0]["phase"])
	require.Equal(t, guardOutcomeFailoverSuccess, guardEvents[0]["outcome"])
	require.Equal(t, PlatformAnthropic, guardEvents[0]["platform"])
	require.Equal(t, true, guardEvents[0]["stream"])
	require.Equal(t, float64(1), guardEvents[0]["attempt"])
	require.Equal(t, "rid-g5-mount1", guardEvents[0]["request_id"])
	require.Greater(t, guardEvents[0]["elapsed_ms"], float64(0))

	hbEvents := cap.heartbeatEvents()
	require.Len(t, hbEvents, 1, "心跳已提交 → 一条 started 事件")
	require.Equal(t, float64(1000), hbEvents[0]["heartbeat_delay_ms"])
	require.Equal(t, "rid-g5-mount1", hbEvents[0]["request_id"])

	// 响应头未到达（悬挂）：无判别器时间戳。
	_, ok := UpstreamHeadersReceivedAt(c)
	require.False(t, ok, "header-wait 悬挂不得记录 upstream_headers_received 时刻")
}

// 上游在 guard 窗口内正常返回：零 guard 事件、零心跳事件、判别器时间戳已记录（健康路径零回归）。
func TestCF524ObservationMount1HealthyPathZeroEvents(t *testing.T) {
	cap := cf524CaptureLogs(t)
	upstream := &g2aMockUpstream{steps: []g2aCall{g2aSuccessFn(g2aAnthropicSSE)}}
	svc := g2aService(upstream)
	body := g2aStreamBody("claude-sonnet-5")
	parsed := g2aParsed(t, body)
	_, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestID, "rid-g5-healthy"))

	snap := g2aSnapshot(1, 1, 30*time.Second)
	hb := g2aHeartbeat(context.Background(), obs, 600*time.Millisecond, 100*time.Millisecond)
	ctx := g2aInject(context.Background(), snap, hb)

	_, err := svc.Forward(ctx, c, g2aAnthropicOAuthAccount(), parsed)
	require.NoError(t, err)

	require.Empty(t, cap.guardEvents(), "健康路径零 guard 事件")
	require.Empty(t, cap.heartbeatEvents(), "上游 < delay 返回：零心跳事件")
	require.False(t, obs.HeaderWritten(), "上游 < delay 返回：心跳不提交（零 SSE 200）")

	// 响应头到达（未超时）→ 判别器时间戳已记录。
	_, ok := UpstreamHeadersReceivedAt(c)
	require.True(t, ok, "响应头到达必须记录判别器时刻")
}

// —— 用例 6：挂点 3（非共享执行器路径）单点埋点 ——

// 挂点 3 悬挂：护栏触发 → attempt 事件 + 请求出口终态关联；响应头未到达（无判别器值）。
func TestCF524ObservationMount3HangEmitsGuardEvent(t *testing.T) {
	cap := cf524CaptureLogs(t)
	upstream := &g2aMockUpstream{}
	svc := g2aService(upstream)
	body := []byte(`{"model":"kimi-k3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	_, c, _ := g2aGinContext(http.MethodPost, "/v1/chat/completions", body)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestID, "rid-g5-mount3"))

	ctx := g2aInject(context.Background(), g2aSnapshot(1, 0, 30*time.Second), nil)

	_, err := svc.ForwardAsChatCompletions(ctx, c, g2aAPIKeyAccount(970, PlatformKimi, "g2a-cc"), body, g2aParsed(t, body))
	var fe *UpstreamFailoverError
	require.ErrorAs(t, err, &fe)
	require.True(t, fe.ShouldRetryNextAccount())

	events := cap.guardEvents()
	require.Len(t, events, 1, "可换号：attempt 事件，终态延后到链接束")
	require.Equal(t, guardEventPhaseAttempt, events[0]["phase"])
	require.Equal(t, "/v1/chat/completions", events[0]["path"])
	require.Equal(t, PlatformKimi, events[0]["platform"])
	require.Equal(t, "rid-g5-mount3", events[0]["request_id"])

	// 模拟请求出口（既有完成日志处）：账号耗尽未再续 attempt → 终态关联补发。
	ResolvePendingGuardObservations(c)
	events = cap.guardEvents()
	require.Len(t, events, 2, "请求出口补发终态关联（同 request_id）")
	require.Equal(t, guardEventPhaseTerminal, events[1]["phase"])
	require.Equal(t, guardOutcomeFailoverExhausted, events[1]["outcome"])
	require.Equal(t, "rid-g5-mount3", events[1]["request_id"])

	// header-wait 悬挂：无判别器时间戳。
	_, ok := UpstreamHeadersReceivedAt(c)
	require.False(t, ok)
}

// —— 用例 7：OpenAI 原生路径零事件（四挂点专属语义） ——

// OpenAI 路径不经过四个非 OpenAI 挂点：不创建 tracker、不发任何新事件；即使
// 请求级完成日志出口的 ResolvePendingGuardObservations 被调用也为 no-op。
func TestCF524ObservationOpenAIPathZeroEvents(t *testing.T) {
	cap := cf524CaptureLogs(t)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestID, "rid-g5-openai"))

	ResolvePendingGuardObservations(c)
	require.Empty(t, cap.all(), "OpenAI 原生路径不得产生任何 CF524 事件")
	if tr := cf524TrackerIfAny(c); tr != nil {
		t.Fatal("OpenAI 原生路径不得创建 guard 观测 tracker")
	}
}

// —— 用例 8：nil 安全（无埋点调用方零影响） ——

func TestCF524ObservationNilSafety(t *testing.T) {
	var obs *cf524GuardObservation
	obs.observeGuardTriggered()
	obs.noteHeartbeatIfCommitted(nil)
	obs.markUpstreamHeadersReceived()

	cf524ObserveGuardTriggered(nil, context.Background(), PlatformKimi, true, 1, 1, time.Second)
	cf524ResolveGuardOutcome(nil, context.Background(), nil)
	cf524NoteHeartbeatStarted(nil, PlatformKimi, 15)
	MarkUpstreamHeadersReceived(nil, time.Now())
	ResolvePendingGuardObservations(nil)
	if _, ok := UpstreamHeadersReceivedAt(nil); ok {
		t.Fatal("nil context 不得返回值")
	}
	if _, ok := UpstreamHeadersReceivedMs(nil, time.Now()); ok {
		t.Fatal("nil context 不得产出判别器字段")
	}
}

// 未使用导入护栏（bytes 在集成夹具中经 g2aGinContext 使用，此处显式引用避免误删）。
var _ = bytes.MinRead
