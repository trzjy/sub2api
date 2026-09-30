//go:build unit

package handler

// CF524 P2-B（ChatCompletions 入口）定向测试：§3.4 矩阵 CC 侧闭包断言 + §3.3 防线等价
// 回归 + 踩坑 #1 基线同时点断言。复用包内既有 unit 测试基建 newCF524TestContext /
// cf524TestCfg / installOpenAIHeartbeatForTest（openai_heartbeat_failover_guard_test.go、
// gateway_handler_cf524_g2b_test.go，均 //go:build unit）。
//
// 组合论证说明（§3.4 矩阵 CC 侧）：CC handler 选号循环前安装点代码为
//   (&GatewayHandler{cfg: h.cfg}).cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, requestStart, reqStream)
// 下方测试以完全相同的调用形态装配 gin test context + helper，断言其安装后的快照/owner
// 产物，从而钉死"CC 入口到达选号循环前请求 context 必有快照（流式另有 owner）"。完整
// handler 调用链（鉴权/body 解析/产能槽）不在此卡单测范围，由 P1/P2 共享符号既有用例
// 与组合论证覆盖。

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestCF524P2B_Install_SnapshotPresent_NonStreaming 钉死 §3.4 矩阵 CC 侧（非流式）：
// 以与 openai_chat_completions.go 安装点同形调用 helper，请求 context 必有预算快照
// （护栏生效），且非流式不装心跳 owner。
func TestCF524P2B_Install_SnapshotPresent_NonStreaming(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/chat/completions")

	stop := (&OpenAIGatewayHandler{cfg: cf524TestCfg(30, 15)}).cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, time.Now(), false)
	require.NotNil(t, stop)
	require.NotPanics(t, func() { stop() }, "非流式 stopFunc 必须可安全调用")

	_, hasSnapshot := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, hasSnapshot, "CC 非流式入口须安装预算快照（护栏生效）")
	_, hasHB := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.False(t, hasHB, "CC 非流式不得安装心跳 owner")
}

// TestCF524P2B_Install_SnapshotAndOwner_Streaming 钉死 §3.4 矩阵 CC 侧（流式）：
// 流式请求 context 必有预算快照且恰一个心跳 owner。
func TestCF524P2B_Install_SnapshotAndOwner_Streaming(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/chat/completions")

	stop := (&OpenAIGatewayHandler{cfg: cf524TestCfg(30, 1)}).cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, time.Now(), true)
	require.NotNil(t, stop)
	defer func() { require.NotPanics(t, func() { stop() }) }()

	_, hasSnapshot := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, hasSnapshot, "CC 流式入口须安装预算快照")
	hb, hasHB := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.True(t, hasHB, "CC 流式入口须安装恰一个心跳 owner")
	require.NotNil(t, hb)
	waitHeartbeatCommittedForTest(t, hb)
}

// TestCF524P2B_GuardEquivalence_NoHeartbeat 钉死 §3.3 防线等价回归：非流式/无心跳请求的
// 写后禁 failover 判定与改造前 `Size() != before` 逐一等价（无第二分支）。
//   - 未写场景（size==before）→ 判定 clean（true）；
//   - 已写场景（size!=before）→ 判定不 clean（false），允许 failover。
func TestCF524P2B_GuardEquivalence_NoHeartbeat(t *testing.T) {
	// 未写：clean（true）⟺ 旧判定 Size()==before（不禁止 failover 的反面）。
	cClean, _ := newCF524TestContext(t, "/v1/chat/completions")
	require.Equal(t, -1, cClean.Writer.Size(), "前置：零写出须为 gin noWritten 哨兵态")
	require.True(t, cf524OpenAIHeartbeatFailoverGuardStillClean(cClean, -1, 0),
		"无心跳未写场景须判定 clean（与改造前 Size()==before 等价）")

	// 已写：不 clean（false）⟺ 旧判定 Size()!=before（允许 failover）。
	cDirty, _ := newCF524TestContext(t, "/v1/chat/completions")
	_, err := cDirty.Writer.Write([]byte("semantic"))
	require.NoError(t, err)
	require.False(t, cf524OpenAIHeartbeatFailoverGuardStillClean(cDirty, -1, 0),
		"无心跳已写场景须判定不 clean（与改造前 Size()!=before 等价，允许 failover）")
}

// TestCF524P2B_GuardBaselineSamePoint 钉死踩坑 #1（基线同时点捕获）：hb owner 存在时
// 基线为捕获时点 CommittedBytes，前置心跳字节不重复计入——增量严格相等下纯前置心跳
// 不得误判为语义写出，基线之后真实语义写出必须破坏增量相等。
func TestCF524P2B_GuardBaselineSamePoint(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/chat/completions")
	// installOpenAIHeartbeatForTest 仅提交 SSE 200 头（committed=0、writer body=0），
	// 等价 handler 入口安装点（槽位等待前）。
	hb := installOpenAIHeartbeatForTest(t, c)

	// 与 CC 入口调用点同型：Forward 前同时点捕获 writerSizeBeforeForward 与基线。
	writerSizeBeforeForward := c.Writer.Size()      // 0（仅头，无 body）
	hbCommittedBaseline := int(hb.CommittedBytes()) // 0

	// 模拟前置心跳字节：槽位等待期间心跳续写一帧注释帧（committed 与 writer body 同步 +N）。
	require.NoError(t, hb.Resume(), "Resume 须立即续写一帧，制造前置心跳字节")
	require.Greater(t, hb.CommittedBytes(), int64(hbCommittedBaseline), "Resume 须使 CommittedBytes 递增")
	require.Greater(t, c.Writer.Size(), writerSizeBeforeForward, "前置心跳帧须落到 writer body")

	// 踩坑 #1：前置心跳字节由 heartbeatDelta 扣除，不得误判为语义写出→clean（允许换号）。
	require.True(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, writerSizeBeforeForward, hbCommittedBaseline),
		"前置心跳字节须由 heartbeatDelta 扣除，不得误判为语义写出（基线同时点捕获）")

	// 基线之后写语义字节：writtenDelta != heartbeatDelta → 不 clean（禁止换号不回退）。
	_, err := c.Writer.Write([]byte("x"))
	require.NoError(t, err)
	require.False(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, writerSizeBeforeForward, hbCommittedBaseline),
		"基线之后语义写出必须破坏增量相等 → 不 clean")
}
