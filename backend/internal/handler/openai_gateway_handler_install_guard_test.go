//go:build unit

package handler

// CF524 P2-C 定向测试（派发单 cf524-openai-p2c-gw-entries.md）。
//
// 变更严格限于 openai_gateway_handler.go + 本文件。Responses/Messages 两入口为重 handler
// （需完整 service 装配），按派发单"组合论证/探针断言"采用探针式测试：直接驱动本卡落入的
// P2-A 符号——cf524InstallUpstreamBudgetAndHeartbeatOpenAI（*OpenAIGatewayHandler 复刻版，
// 见 openai_gateway_handler.go 顶部注释）、cf524OpenAIHeartbeatFailoverGuardStillClean（包级
// 自由函数，P2-A）与防线归一函数 cf524NormalizeWrittenSize——组合论证：
//   ① 安装点顺序不变量（§3.1：Responses 链 compact keepalive start 须先于安装 helper）；
//   ② 单 owner 断言（compact 已装 → 0 心跳 owner；未装/no-op → 恰 1 个）；
//   ③ 目标侧闭包（§3.4：两入口到达选号循环前 context 必有快照，流式另有 owner）；
//   ④ 踩坑 #12 的 -1/0 归一回归（OpenAICompactKeepaliveAdjustedWrittenSize 返回 -1 经归一后
//      与 0 等价、比较结果不变；compact 扣除与心跳 CommittedBytes 扣除两个来源独立不合并）；
//   ⑤ 防线等价回归（非流式/无心跳请求判定与改造前逐一等价）。
//
// 测试辅助函数 cf524TestCfg / newCF524TestContext / waitHeartbeatCommittedForTest /
// installOpenAIHeartbeatForTest 定义于同包既有 unit 测试文件，直接复用，不重复定义。

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// ───────── ① 调用顺序不变量（§3.1）：Responses 链 keepalive start 必须先于安装 helper ─────────
// 探针论证：安装 helper 仅消费 compact keepalive 状态 key（cf524OpenAICompactKeepaliveKey）决定
// 是否装心跳 owner；该 key 由 service.StartOpenAICompactSSEKeepalive（Responses :454）写入。
// 因此 key 存在 ⟺ keepalive 已先执行；helper 行为随 key 切换即钉死"keepalive 先、helper 后"
// 的不可变顺序（不变量非实现期分支）。
func TestCF524P2C_InstallOrder_CompactKeyPrecedesHelper(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: cf524TestCfg(30, 1)}

	// 尚未启动 compact keepalive（key 不存在，对应 no-op / 非 compact 请求）：
	// helper 正常安装恰一个心跳 owner——顺序不变量下此分支即"keepalive 未抢先"的安全默认。
	cAbsent, _ := newCF524TestContext(t, "/v1/responses")
	stop := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(cAbsent, time.Now(), true)
	require.NotNil(t, stop)
	require.NotPanics(t, func() { stop() })
	_, hasHB := service.UpstreamHeartbeatFromContext(cAbsent.Request.Context())
	require.True(t, hasHB, "compact key 不存在时安装恰一个心跳 owner")

	// 已启动 compact keepalive（key 已写入，对应 Responses :454 先于本 helper 执行）：
	// helper 消费 key，不再装心跳 owner（避免同 writer 双拍频，踩坑 #13）。
	cInstalled, _ := newCF524TestContext(t, "/v1/responses")
	cInstalled.Set(cf524OpenAICompactKeepaliveKey, struct{}{})
	stop2 := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(cInstalled, time.Now(), true)
	require.NotNil(t, stop2)
	require.NotPanics(t, func() { stop2() })
	_, hasHB2 := service.UpstreamHeartbeatFromContext(cInstalled.Request.Context())
	require.False(t, hasHB2, "compact keepalive 已装时本 helper 不得再装心跳 owner（顺序不变量保证）")
}

// ───────── ② 单 owner 断言 ─────────
func TestCF524P2C_SingleOwnerInvariant(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: cf524TestCfg(30, 1)}

	// 未安装 compact / no-op：恰一个心跳 owner（快照照装）。
	c, _ := newCF524TestContext(t, "/v1/messages")
	stop := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, time.Now(), true)
	require.NotPanics(t, func() { stop() })
	_, hasHB := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.True(t, hasHB, "未安装 compact → 恰一个心跳 owner")
	_, hasSnap := service.RequestBudgetSnapshotFromContext(c.Request.Context())
	require.True(t, hasSnap, "快照照装")

	// 已安装 compact：0 个本方案心跳 owner（互斥）。
	c2, _ := newCF524TestContext(t, "/v1/messages")
	c2.Set(cf524OpenAICompactKeepaliveKey, struct{}{})
	stop2 := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c2, time.Now(), true)
	require.NotPanics(t, func() { stop2() })
	_, hasHB2 := service.UpstreamHeartbeatFromContext(c2.Request.Context())
	require.False(t, hasHB2, "已安装 compact → 0 个心跳 owner")
}

// ───────── ③ 目标侧闭包断言（§3.4 矩阵 Responses/Messages 侧）─────────
// 两入口请求到达选号循环前 context 必有快照（流式另有 owner）。本卡在 Responses/Messages 号
// 循环前调用 cf524InstallUpstreamBudgetAndHeartbeatOpenAI；探针证明：调用即装快照、流式另装
// owner；不调用则无快照（护栏 no-op 可证，非目标侧必无快照）。
func TestCF524P2C_SnapshotClosureBeforeAccountLoop(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: cf524TestCfg(30, 1)}

	// 未调用安装点：context 无快照（无快照透传不静默绕过目标请求）。
	cNone, _ := newCF524TestContext(t, "/v1/responses")
	_, hasSnap := service.RequestBudgetSnapshotFromContext(cNone.Request.Context())
	require.False(t, hasSnap, "未安装 → 无快照（no-op 可证）")

	// 非流式：仅快照、无 owner（到达号循环前 context 必有快照）。
	cNonStream, _ := newCF524TestContext(t, "/v1/responses")
	stop := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(cNonStream, time.Now(), false)
	require.NotPanics(t, func() { stop() })
	_, hasSnap2 := service.RequestBudgetSnapshotFromContext(cNonStream.Request.Context())
	require.True(t, hasSnap2, "非流式到达号循环前 context 必有快照")
	_, hasHB := service.UpstreamHeartbeatFromContext(cNonStream.Request.Context())
	require.False(t, hasHB, "非流式无 owner")

	// 流式：快照 + owner 均在（流式另有 owner）。
	cStream, _ := newCF524TestContext(t, "/v1/responses")
	stop2 := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(cStream, time.Now(), true)
	defer func() { require.NotPanics(t, func() { stop2() }) }()
	_, hasSnap3 := service.RequestBudgetSnapshotFromContext(cStream.Request.Context())
	require.True(t, hasSnap3, "流式到达号循环前 context 必有快照")
	hb, hasHB2 := service.UpstreamHeartbeatFromContext(cStream.Request.Context())
	require.True(t, hasHB2, "流式另有 owner")
	require.NotNil(t, hb)
	waitHeartbeatCommittedForTest(t, hb)
}

// ───────── ④ 踩坑 #12：-1/0 归一回归 与 两扣除来源独立不合并 ─────────
// OpenAICompactKeepaliveAdjustedWrittenSize 无语义字节时返回 -1（gin noWritten 哨兵同源），
// 与 G5d 归一 0 约定不同源。调用方（Responses :780）先过 cf524NormalizeWrittenSize(<0→0) 再
// 进入防线比较，使 -1 与 0 在"无语义字节"语义上等价、比较结果不变。compact 字节由
// OpenAICompactKeepaliveAdjustedWrittenSize 扣除、心跳字节由 CommittedBytes 基线增量扣除，
// 两个扣除来源各自独立、不得合并计数（Responses 链 compact 与心跳互斥，心跳 owner 不安装）。
func TestCF524P2C_CompactAdjustedSentinelNormalization(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/responses")

	// 无语义字节时 OpenAICompactKeepaliveAdjustedWrittenSize 返回 -1 哨兵（踩坑 #12）。
	require.Equal(t, -1, service.OpenAICompactKeepaliveAdjustedWrittenSize(c),
		"无语义字节应返回 -1 哨兵")
	// 归一：-1 与 0 在"无语义字节"语义上等价。
	require.Equal(t, 0, cf524NormalizeWrittenSize(-1))
	require.Equal(t, 0, cf524NormalizeWrittenSize(0))

	// 基线过归一后进入 cf524OpenAIHeartbeatFailoverGuardStillClean，比较结果不变：
	// 无心跳 owner（Responses 互斥 → 无 owner）时 -1 与 0 基线判定同为 clean。
	require.True(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, -1, 0),
		"-1 基线须判定 clean（与 0 等价）")
	require.True(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, 0, 0),
		"0 基线须判定 clean（与 -1 等价）")
}

// 调用方基线 -1（OpenAICompactKeepaliveAdjustedWrittenSize 返回）与 0（归一后）进入
// openAIForwardMayFailover 必须等价（比较结果不变）。
func TestCF524P2C_ForwardMayFailover_SentinelEquivalence(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/responses")
	failoverErr := &service.UpstreamFailoverError{SafeToFailoverAfterWrite: false}

	// 当前口径 OpenAICompactKeepaliveAdjustedWrittenSize(c) 亦为 -1；双侧归一后 0==0 → 可 failover。
	// -1 基线与 0 基线结果一致，证明归一不改变判定。
	require.True(t, openAIForwardMayFailover(c, -1, failoverErr), "-1 基线应与 0 等价 → 可 failover")
	require.True(t, openAIForwardMayFailover(c, 0, failoverErr), "0 基线应与 -1 等价 → 可 failover")
}

// 两扣除来源独立不合并：心跳判定只消费 CommittedBytes 基线增量，不并入任何 compact 扣除
// （本链无 compact）。仅心跳字节 → clean；追加语义写出 → 不 clean。
func TestCF524P2C_GuardHeartbeatDeductionIndependent(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: cf524TestCfg(30, 1)}
	c, _ := newCF524TestContext(t, "/v1/messages")
	// 安装点（非 compact）：快照 + 流式心跳 owner。
	stop := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, time.Now(), true)
	defer func() { require.NotPanics(t, func() { stop() }) }()
	hb, ok := service.UpstreamHeartbeatFromContext(c.Request.Context())
	require.True(t, ok)
	waitHeartbeatCommittedForTest(t, hb) // 仅心跳字节已提交（N>0），无语义写出

	baseline := int(hb.CommittedBytes())
	// 仅心跳字节（无语义写出）：writtenDelta == heartbeatDelta → clean。
	// 该判定只消费心跳 CommittedBytes 增量，不并入 compact 扣除（本链无 compact）。
	require.True(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, 0, baseline),
		"仅心跳字节：心跳增量独立判定为 clean（不并入 compact 扣除）")

	// 追加语义写出：writtenDelta > heartbeatDelta → 不 clean。
	_, err := c.Writer.Write([]byte("semantic"))
	require.NoError(t, err)
	require.False(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, 0, baseline),
		"语义写出破坏心跳增量相等 → 不 clean")
}

// ───────── ⑤ 防线等价回归：非流式/无心跳请求判定与改造前逐一等价 ─────────
// Messages :1455 改造前为裸 `c.Writer.Size() != writerSizeBeforeForward`；改造后为
// cf524OpenAIHeartbeatFailoverGuardStillClean(c, writerSizeBeforeForward, hbCommittedBaseline)
// （:1368 同时点捕获 hbCommittedBaseline）。无心跳 owner 时退化 size==before，经 G5d 双侧
// 归一后与改造前裸判定逐一等价。
func TestCF524P2C_MessagesGuardEquivalenceNoHeartbeat(t *testing.T) {
	c, _ := newCF524TestContext(t, "/v1/messages")

	// 无写出、无心跳 owner：
	// 改造前（裸 c.Writer.Size() != before）：-1 != -1 → false（不放弃 → clean）
	// 改造后（cf524OpenAIHeartbeatFailoverGuardStillClean，G5d 归一）：0 == 0 → true（clean）
	// 逐一等价。
	require.False(t, c.Writer.Size() != -1, "改造前裸判定：无写出应不放弃")
	require.True(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, -1, 0),
		"改造后归一判定：无写出应 clean")

	// 写出语义字节后：
	// 改造前 -1 != N → 放弃；改造后 0 != N → 不 clean。逐一等价。
	_, err := c.Writer.Write([]byte("semantic"))
	require.NoError(t, err)
	written := c.Writer.Size()
	require.True(t, written != -1, "改造前：有语义写出应放弃")
	require.False(t, cf524OpenAIHeartbeatFailoverGuardStillClean(c, -1, 0),
		"改造后：有语义写出应不 clean")
}
