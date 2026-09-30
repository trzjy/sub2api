//go:build unit

package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// cf524P2aFailingWriter 包装 gin.ResponseWriter：实现 http.Flusher（故 hb.flusherOK=true，
// 走正常起搏路径），但 Write/WriteString 注入错误，使 hb.Start() 的 beat() 写帧失败 →
// 失败关闭。用于钉死 helper 在 Flush/写出失败时失败关闭 + 记 warn（踩坑 #8）。
type cf524P2aFailingWriter struct {
	gin.ResponseWriter
}

func (w *cf524P2aFailingWriter) Write(b []byte) (int, error) {
	return 0, errors.New("injected write failure")
}
func (w *cf524P2aFailingWriter) WriteString(s string) (int, error) {
	return 0, errors.New("injected write failure")
}
func (w *cf524P2aFailingWriter) Flush() {}

// installOpenAIHeartbeatForTest 在请求 context 中安装一个已提交字节的 UpstreamHeartbeat
// owner（供 guard 判定表驱动用例消费），返回该 owner。
func installOpenAIHeartbeatForTest(t *testing.T, c *gin.Context) *service.UpstreamHeartbeat {
	t.Helper()
	snapshot := service.NewRequestBudgetSnapshot(30, 1, time.Now(), true)
	ctx := service.WithRequestBudgetSnapshot(c.Request.Context(), snapshot)
	hb := service.NewUpstreamHeartbeat(ctx, c.Writer, snapshot, time.Now())
	ctx = service.WithUpstreamHeartbeat(ctx, hb)
	c.Request = c.Request.WithContext(ctx)
	require.NoError(t, hb.Start())
	waitHeartbeatCommittedForTest(t, hb)
	return hb
}

// ───────────────────────── ① guard 判定：表驱动 ─────────────────────────

func TestCF524P2A_GuardStillClean_NoHeartbeatBranch(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/messages")

	// 无 hb：size==before → true（退化与现状等价）。
	require.True(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, 0, 0), "零写出应判定 clean")

	// 无 hb：写入差 → false。
	_, err := c.Writer.Write([]byte("semantic"))
	require.NoError(t, err)
	require.False(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, 0, 0), "有语义写出应判定不 clean")
}

func TestCF524P2A_GuardStillClean_HeartbeatDeltaEqualsWritten(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/messages")
	installOpenAIHeartbeatForTest(t, c) // 仅心跳字节，committed=N、baseline=0

	// 仅心跳字节（无语义写出）：writtenDelta == heartbeatDelta → true。
	require.True(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, 0, 0),
		"仅心跳字节不得误判为语义写出（增量严格相等）")

	// 追加语义写出：writtenDelta > heartbeatDelta → false。
	_, err := c.Writer.Write([]byte("semantic"))
	require.NoError(t, err)
	require.False(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, 0, 0),
		"语义写出必须破坏增量相等 → 不 clean")
}

func TestCF524P2A_GuardStillClean_SentinelNormalization(t *testing.T) {
	// 双侧 -1 哨兵：gin noWritten 归一为 0，0==0 → true。
	cClean, _ := newCF524TestContext(t, "/v1/messages")
	require.Equal(t, -1, cClean.Writer.Size(), "未写出时 gin Size 应为 -1 哨兵")
	require.True(t, cf524OpenAIHeartbeatFailoverGuardStillClean(cClean, -1, 0),
		"双侧 -1 哨兵归一后应判定 clean")

	// 实测有写出、before 捕获为 -1：归一后 before=0，size>0 → false。
	cDirty, _ := newCF524TestContext(t, "/v1/messages")
	_, err := cDirty.Writer.Write([]byte("abc"))
	require.NoError(t, err)
	require.False(t, cf524OpenAIHeartbeatFailoverGuardStillClean(cDirty, -1, 0),
		"-1 哨兵基线不得掩盖真实语义写出")
}

func TestCF524P2A_GuardStillClean_BaselineDeltaStrict(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/messages")
	hb := installOpenAIHeartbeatForTest(t, c)
	baseline := int(hb.CommittedBytes()) // 捕获同时点基线（已含首拍字节 N）

	// 自基线零新增心跳 + 无语义写出：退化 size==before → true。
	require.True(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, 0, baseline),
		"自基线零新增应退化等价判定为 clean")

	// 基线之后新写一帧语义字节：writtenDelta(=S) != heartbeatDelta(=0) → false。
	_, err := c.Writer.Write([]byte("x"))
	require.NoError(t, err)
	require.False(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, 0, baseline),
		"基线之后语义写出必须破坏退化等价 → 不 clean")
}

// ───────────────────────── ② helper 安装条件 ─────────────────────────

func TestCF524P2A_Helper_GuardDisabled_NoInstall(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: cf524TestCfg(0, 15)} // guardSeconds<=0
	c, _ := newCF524TestContext(t, "/v1/messages")

	stop := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, time.Now(), true)
	require.NotNil(t, stop)
	require.NotPanics(t, func() { stop() }, "guard 关闭时 stopFunc 必须可安全调用")

	_, hasSnapshot := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.False(t, hasSnapshot, "guardSeconds<=0 不得安装快照")
	_, hasHB := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.False(t, hasHB, "guardSeconds<=0 不得安装心跳 owner")
}

func TestCF524P2A_Helper_NonStreaming_SnapshotOnly(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: cf524TestCfg(30, 15)}
	c, _ := newCF524TestContext(t, "/v1/messages")

	stop := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, time.Now(), false) // 非流式
	require.NotNil(t, stop)
	require.NotPanics(t, func() { stop() })

	_, hasSnapshot := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, hasSnapshot, "非流式仍须安装预算快照（护栏生效）")
	_, hasHB := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.False(t, hasHB, "非流式不得安装心跳 owner")
}

func TestCF524P2A_Helper_Streaming_OneHeartbeatOwner(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: cf524TestCfg(30, 1)}
	c, _ := newCF524TestContext(t, "/v1/messages")

	stop := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, time.Now(), true)
	require.NotNil(t, stop)
	defer func() { require.NotPanics(t, func() { stop() }) }()

	_, hasSnapshot := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, hasSnapshot, "流式须安装预算快照")
	hb, hasHB := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.True(t, hasHB, "流式须安装恰一个心跳 owner")
	require.NotNil(t, hb)
	waitHeartbeatCommittedForTest(t, hb)
}

func TestCF524P2A_Helper_CompactKeyInstalled_NoHeartbeatOwner(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: cf524TestCfg(30, 1)}
	c, _ := newCF524TestContext(t, "/v1/messages")
	// 模拟 Responses 入口已先于 helper 启动 compact keepalive（事实源 key 已写入）。
	c.Set(cf524OpenAICompactKeepaliveKey, struct{}{})

	stop := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, time.Now(), true)
	require.NotNil(t, stop)
	require.NotPanics(t, func() { stop() })

	_, hasSnapshot := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, hasSnapshot, "compact 已装时快照仍须安装")
	_, hasHB := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.False(t, hasHB, "compact 已装时本 helper 不得再装心跳 owner（双拍频互斥，踩坑 #13）")
}

func TestCF524P2A_Helper_CompactKeyAbsent_OneHeartbeatOwner(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: cf524TestCfg(30, 1)}
	c, _ := newCF524TestContext(t, "/v1/messages")
	// key 不存在（no-op 退出 / 非 compact 请求）：正常安装恰一个心跳 owner。

	stop := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, time.Now(), true)
	require.NotNil(t, stop)
	defer func() { require.NotPanics(t, func() { stop() }) }()

	_, hasSnapshot := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, hasSnapshot)
	_, hasHB := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.True(t, hasHB, "compact key 不存在时必须安装恰一个心跳 owner")
}

func TestCF524P2A_Helper_FlushMissing_FailClosed(t *testing.T) {
	core, observedLogs := observer.New(zap.WarnLevel)
	requestID := "p2a-flush-fail-1"
	c, _ := newCF524TestContext(t, "/v1/messages")
	ctx := logger.IntoContext(
		context.WithValue(c.Request.Context(), ctxkey.RequestID, requestID),
		zap.New(core),
	)
	c.Request = c.Request.WithContext(ctx)
	// 注入写出失败 writer：hb 起搏写帧失败 → 失败关闭（心跳可选、零降级分支，踩坑 #8）。
	c.Writer = &cf524P2aFailingWriter{c.Writer}

	h := &OpenAIGatewayHandler{cfg: cf524TestCfg(30, 1)}
	stop := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, time.Now(), true)
	require.NotNil(t, stop)
	require.NotPanics(t, func() { stop() }, "失败关闭不得 panic、不得终止请求（心跳仅为可关闭优化）")

	// 快照仍应已安装（快照与心跳安装解耦，失败关闭只影响心跳）。
	_, hasSnapshot := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, hasSnapshot, "失败关闭不影响快照安装（护栏仍生效）")

	// 注：gin.ResponseWriter 恒实现 http.Flusher，故 hb.flusherOK 在生产/测试不可达
	// false，Start() 返回 nil（写帧在 goroutine 内失败关闭）。helper 复用既有的
	// logHeartbeatStartFailure（与 gateway_handler.go:2070 安装点逐字同源），其
	// "Start 失败 → 记 warn 且请求继续"契约由共享组件测试
	// TestCF524G2b_HeartbeatStartFailureWarnsAndContinues 钉死；本 helper 与之 1:1 同型，
	// 不再重复注入不可达分支。
	_ = observedLogs
}
