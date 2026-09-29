//go:build unit

package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cf524TestCfg 返回带合法 guard/heartbeat 的配置（heartbeat 取 1s 便于时间轴断言；
// handler 层不重复 Validate，安装点只消费值）。
func cf524TestCfg(guard, heartbeat int) *config.Config {
	return &config.Config{
		RunMode: config.RunModeSimple,
		Gateway: config.GatewayConfig{
			UpstreamFirstByteGuardSeconds: guard,
			UpstreamHeartbeatDelaySeconds: heartbeat,
		},
	}
}

func newCF524TestContext(t *testing.T, path string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}"))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

// waitHeartbeatCommittedForTest 轮询等待心跳提交（SSE 200 已写出），失败即测试失败。
func waitHeartbeatCommittedForTest(t *testing.T, hb *service.UpstreamHeartbeat) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hb.IsCommitted() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("heartbeat did not commit SSE 200 within deadline")
}

// commitHeartbeatBytesForTest 通过 Resume 立即续写一帧 keep-alive 注释帧，制造
// CommittedBytes>0 的已提交 owner（Resume 经公开 API，service 保持零改动）。
func commitHeartbeatBytesForTest(t *testing.T, hb *service.UpstreamHeartbeat) {
	t.Helper()
	waitHeartbeatCommittedForTest(t, hb)
	before := hb.CommittedBytes()
	require.NoError(t, hb.Resume())
	require.Greater(t, hb.CommittedBytes(), before, "Resume 必须立即续写一帧，CommittedBytes 递增")
}

// TestCF524G2b_InstallPoint_SnapshotAndHeartbeatInstalledAtHandlerEntry 验证入口安装点：
// handler 认证成功后，预算快照（入口单调时间起算）与请求级心跳 owner 均已在请求
// context 中（forward 侧可读到），且入口时刻落在 handler 调用窗口内。
func TestCF524G2b_InstallPoint_SnapshotAndHeartbeatInstalledAtHandlerEntry(t *testing.T) {
	groupID := int64(9201)
	accountID := int64(9202)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := &service.Account{
		ID: accountID, Name: "ag-cf524", Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "tok_xxx", "intercept_warmup_requests": true},
		Extra:       map[string]any{"mixed_scheduling": true},
		Concurrency: 1, Priority: 1, Status: service.StatusActive, Schedulable: true,
		AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}
	h, cleanup := newTestGatewayHandler(t, group, []*service.Account{account})
	defer cleanup()
	h.cfg = cf524TestCfg(30, 1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"Warmup"}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	c.Request = req
	apiKey := &service.APIKey{
		ID: 9301, UserID: 9401, GroupID: &groupID, Status: service.StatusActive,
		User: &service.User{ID: 9401, Concurrency: 10, Balance: 100}, Group: group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})

	callStart := time.Now()
	h.Messages(c)
	callEnd := time.Now()

	require.Equal(t, http.StatusOK, rec.Code, "warmup 拦截路径应正常返回（安装点不得改变既有行为）")

	snapshot, ok := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, ok, "认证成功后预算快照必须在请求 context 中")
	require.Equal(t, 30, snapshot.GuardSeconds)
	require.Equal(t, 1, snapshot.HeartbeatDelaySeconds)
	require.False(t, snapshot.EntryMonotonic.IsZero())
	require.False(t, snapshot.EntryMonotonic.Before(callStart), "入口时刻不得早于 handler 调用")
	require.False(t, snapshot.EntryMonotonic.After(callEnd), "入口时刻不得晚于 handler 返回")
	// clientStream=true 且 heartbeat>0 → 总预算 2×guard+10s。
	require.Equal(t, 2*30*time.Second+10*time.Second, snapshot.AbsoluteDeadline.Sub(snapshot.EntryMonotonic))

	hb, ok := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.True(t, ok, "流式请求认证成功后心跳 owner 必须已安装")
	require.NotNil(t, hb)
}

// TestCF524G2b_InstallPoint_AuthFailure_NoInstallNoSideEffects 验证认证失败路径零字节
// 语义不破坏：安装点在认证之后，认证失败分支不得产生 owner/快照，也不得追加任何
// 心跳字节（响应体与既有认证错误逐字节一致）。
func TestCF524G2b_InstallPoint_AuthFailure_NoInstallNoSideEffects(t *testing.T) {
	h := &GatewayHandler{cfg: cf524TestCfg(30, 1)}
	c, rec := newCF524TestContext(t, "/v1/messages")

	h.Messages(c)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.JSONEq(t, `{"type":"error","error":{"type":"authentication_error","message":"Invalid API key"}}`, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "keep-alive", "认证失败不得因安装点产生任何 SSE/心跳写出")

	_, hasSnapshot := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.False(t, hasSnapshot, "认证失败分支不得安装预算快照")
	_, hasHeartbeat := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.False(t, hasHeartbeat, "认证失败分支不得安装心跳 owner")
}

// TestCF524G2b_EntryMonotonicCoversPreForwardDelay 验证前置延迟：快照 EntryMonotonic
// 为入口时刻（非安装时刻），总预算含前置期；入口已耗时间超过 delay 时心跳立即提交。
func TestCF524G2b_EntryMonotonicCoversPreForwardDelay(t *testing.T) {
	h := &GatewayHandler{cfg: cf524TestCfg(30, 1)}
	c, _ := newCF524TestContext(t, "/v1/messages")

	entry := time.Now().Add(-2 * time.Second) // 模拟认证/排队已耗 2s（> delay=1s）
	stop := h.cf524InstallUpstreamBudgetAndHeartbeat(c, entry, true)
	defer stop()

	snapshot, ok := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, ok)
	require.Equal(t, entry, snapshot.EntryMonotonic, "EntryMonotonic 必须保留入口时刻，不得被安装时刻覆盖")

	hb, ok := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.True(t, ok)
	// 入口已耗 2s > delay 1s → initialDelay=0 → 心跳应即时提交 SSE 200。
	waitHeartbeatCommittedForTest(t, hb)
}

// TestCF524G2b_HotReloadDoesNotAffectInFlightSnapshot 验证跨热加载边界：同一请求安装后
// 修改配置（模拟热加载），进行中请求的快照值保持不变。
func TestCF524G2b_HotReloadDoesNotAffectInFlightSnapshot(t *testing.T) {
	h := &GatewayHandler{cfg: cf524TestCfg(30, 1)}
	c, _ := newCF524TestContext(t, "/v1/messages")

	stop := h.cf524InstallUpstreamBudgetAndHeartbeat(c, time.Now(), true)
	defer stop()

	snapshot, ok := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, ok)
	require.Equal(t, 30, snapshot.GuardSeconds)
	require.Equal(t, 1, snapshot.HeartbeatDelaySeconds)

	// 模拟热加载：整体替换配置（只影响后续请求）。
	h.cfg = cf524TestCfg(60, 15)

	reloaded, ok := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, ok)
	require.Equal(t, 30, reloaded.GuardSeconds, "进行中请求的快照不得随热加载变化")
	require.Equal(t, 1, reloaded.HeartbeatDelaySeconds, "进行中请求的快照不得随热加载变化")
	require.Equal(t, snapshot.AbsoluteDeadline, reloaded.AbsoluteDeadline, "不可变截止时间不得随热加载变化")
}

// TestCF524G2b_HeartbeatFailoverGuardStillClean_Branches 验证防线例外两分支（v5）：
//   - 心跳已提交（CommittedBytes>0）→ 判定干净，guard 超时后仍可 FailoverContinue 换号；
//   - 真实语义流已写出（超出心跳基线）→ 判定不干净，禁止 failover（现状防线不回退）；
//   - 无 owner → 判定与现状完全一致。
func TestCF524G2b_HeartbeatFailoverGuardStillClean_Branches(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/messages")

	writerSizeBeforeForward := c.Writer.Size()

	// 无 owner：现状判定（size==before 干净，size!=before 禁止）。
	require.True(t, heartbeatFailoverGuardStillClean(c, writerSizeBeforeForward))
	_, _ = c.Writer.WriteString("semantic-without-heartbeat")
	require.False(t, heartbeatFailoverGuardStillClean(c, writerSizeBeforeForward),
		"无心跳时真实写出必须仍禁止 failover（现状防线不回退）")

	// 心跳已提交：干净（Size 仅增长心跳注释帧字节）。
	c2, _ := newCF524TestContext(t, "/v1/messages")
	before := c2.Writer.Size()
	snapshot := service.NewRequestBudgetSnapshot(30, 1, time.Now().Add(-2*time.Second), true)
	hb := service.NewUpstreamHeartbeat(c2.Request.Context(), c2.Writer, snapshot, time.Now())
	require.NoError(t, hb.Start())
	commitHeartbeatBytesForTest(t, hb)
	c2.Request = c2.Request.WithContext(service.WithUpstreamHeartbeat(c2.Request.Context(), hb))
	defer hb.Stop()

	require.Greater(t, hb.CommittedBytes(), int64(0))
	require.Greater(t, c2.Writer.Size(), before)
	require.True(t, heartbeatFailoverGuardStillClean(c2, before),
		"心跳已提交（注释帧）不得被判为语义写出，guard 超时后仍应允许换号")

	// 防线在心跳已提交场景放行 → 既有 FailoverState 机器应给出 FailoverContinue 换号
	// （守卫通过后的既有消费路径，零改动验证）。
	fs := NewFailoverState(false)
	fs.LastFailoverErr = nil
	action := fs.HandleFailoverError(context.Background(), nil, 1, service.PlatformAnthropic, 0,
		&service.UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout, NextAccountAction: service.NextAccountRetry, SafeToFailoverAfterWrite: true})
	require.Equal(t, FailoverContinue, action, "心跳已提交且防线干净时，guard 超时必须仍换号（FailoverContinue）")

	// 真实语义流写出：超出心跳基线 → 禁止 failover。
	_, _ = c2.Writer.WriteString("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	require.False(t, heartbeatFailoverGuardStillClean(c2, before),
		"真实语义流已写出必须仍禁止 failover")
}

// TestCF524G2b_HeartbeatCommittedExhaustedEmitsSSEError 验证（v6，零改动验证）：
// 心跳已提交后直接耗尽，handler 既有 handleFailoverExhausted(streamStarted=true)
// 走 SSE error 帧分支，不写非流式 JSON。
func TestCF524G2b_HeartbeatCommittedExhaustedEmitsSSEError(t *testing.T) {
	c, rec := newCF524TestContext(t, "/v1/messages")
	snapshot := service.NewRequestBudgetSnapshot(30, 1, time.Now().Add(-2*time.Second), true)
	hb := service.NewUpstreamHeartbeat(c.Request.Context(), c.Writer, snapshot, time.Now())
	require.NoError(t, hb.Start())
	waitHeartbeatCommittedForTest(t, hb)
	defer hb.Stop()
	require.Equal(t, http.StatusOK, rec.Code, "心跳提交后 HTTP 200 已固化")

	h := &GatewayHandler{}
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:   http.StatusGatewayTimeout,
		ResponseBody: []byte(`{"error":{"type":"upstream_first_byte_timeout"}}`),
	}
	h.handleFailoverExhausted(c, failoverErr, service.PlatformAnthropic, true)

	body := rec.Body.String()
	require.True(t, strings.HasPrefix(body, "data: "), "耗尽必须追加 SSE error 帧（而非非流式 JSON）")
	require.Contains(t, body, `"type":"upstream_error"`)
	require.NotContains(t, body, `"object":`)
}

// TestCF524G2b_HeartbeatCommittedForwardErrorAlreadyCommunicatedFalse 钉死用例（v2 第 3 点）：
// 心跳已提交（CommittedBytes>0、Content-Type=text/event-stream）时
// gatewayForwardErrorAlreadyCommunicated 返回 false，handler 照常追加 SSE error 帧。
func TestCF524G2b_HeartbeatCommittedForwardErrorAlreadyCommunicatedFalse(t *testing.T) {
	c, rec := newCF524TestContext(t, "/v1/messages")
	before := c.Writer.Size()
	snapshot := service.NewRequestBudgetSnapshot(30, 1, time.Now().Add(-2*time.Second), true)
	hb := service.NewUpstreamHeartbeat(c.Request.Context(), c.Writer, snapshot, time.Now())
	require.NoError(t, hb.Start())
	commitHeartbeatBytesForTest(t, hb)
	defer hb.Stop()

	require.Greater(t, hb.CommittedBytes(), int64(0), "前置：心跳已提交字节")
	require.NotEqual(t, before, c.Writer.Size(), "前置：writer 已增长")
	require.Equal(t, "text/event-stream", strings.ToLower(c.Writer.Header().Get("Content-Type")))

	communicated := gatewayForwardErrorAlreadyCommunicated(c, before, errors.New("upstream first byte timeout"))
	require.False(t, communicated, "心跳已提交（SSE 200）时不得判定为已完整告知，须照常追加 SSE error 帧")
	require.NotContains(t, rec.Body.String(), `data: {"type":"error"`)
}

// TestCF524G2b_RequestExitStopsHeartbeat 验证请求出口 Stop：所有权 Stop 在 handler 出口
// 必被调用，心跳 goroutine 立即退出（无泄漏，超时断言）。
func TestCF524G2b_RequestExitStopsHeartbeat(t *testing.T) {
	h := &GatewayHandler{cfg: cf524TestCfg(30, 1)}
	c, _ := newCF524TestContext(t, "/v1/messages")

	stop := h.cf524InstallUpstreamBudgetAndHeartbeat(c, time.Now(), true)
	hb, ok := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.True(t, ok)
	require.NotNil(t, hb)

	done := make(chan struct{})
	go func() {
		stop() // 等价 handler 出口 defer
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("心跳 owner 未在请求出口及时 Stop（goroutine 泄漏）")
	}
}

// TestCF524G2b_HealthPathZeroRegression 验证健康路径零回归：无配置/无心跳时安装点
// 跳过（无快照/无 owner），防线判定与现状逐字一致。
func TestCF524G2b_HealthPathZeroRegression(t *testing.T) {
	// 无 cfg：安装点跳过。
	h := &GatewayHandler{}
	c, _ := newCF524TestContext(t, "/v1/messages")
	stop := h.cf524InstallUpstreamBudgetAndHeartbeat(c, time.Now(), true)
	defer stop()
	_, hasSnapshot := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	_, hasHeartbeat := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.False(t, hasSnapshot)
	require.False(t, hasHeartbeat)

	// 心跳禁用（0）：只装快照，不起搏心跳。
	h2 := &GatewayHandler{cfg: cf524TestCfg(30, 0)}
	c2, _ := newCF524TestContext(t, "/v1/messages")
	stop2 := h2.cf524InstallUpstreamBudgetAndHeartbeat(c2, time.Now(), true)
	defer stop2()
	_, hasSnapshot2 := service.RequestBudgetSnapshotFromContext(c2.Request.Context())
	_, hasHeartbeat2 := service.UpstreamHeartbeatFromContext(c2.Request.Context())
	require.True(t, hasSnapshot2, "心跳禁用仍应装快照（护栏生效）")
	require.False(t, hasHeartbeat2, "心跳禁用不得起搏心跳")

	// 非流式：只装快照。
	h3 := &GatewayHandler{cfg: cf524TestCfg(30, 1)}
	c3, _ := newCF524TestContext(t, "/v1/messages")
	stop3 := h3.cf524InstallUpstreamBudgetAndHeartbeat(c3, time.Now(), false)
	defer stop3()
	_, hasHeartbeat3 := service.UpstreamHeartbeatFromContext(c3.Request.Context())
	require.False(t, hasHeartbeat3, "非流式不得起搏心跳")
}
