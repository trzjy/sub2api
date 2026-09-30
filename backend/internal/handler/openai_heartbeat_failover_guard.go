package handler

// 文件：internal/handler/openai_heartbeat_failover_guard.go
//
// CF524 扩展 P2-A（方案 docs/cf524-openai-gateway-guard-extension-plan.md v1.2
// §3.1/§3.3）：OpenAI 处理器家族（ChatCompletions / Responses / Messages 三入口）
// 共享的两个符号，供 P2 后续卡（CC 入口 / gateway_handler 双入口）编译期依赖。
//
// 本文件零改动既有文件（gateway_handler.go / openai_chat_completions.go /
// openai_gateway_handler.go / cf524_observation.go / upstream_heartbeat.go），
// 仅新增以下两个符号，语义与既有实现逐字同源（无第二套机制/配置/schema）：
//  ① cf524OpenAIHeartbeatFailoverGuardStillClean —— 写后禁 failover 防线的共享
//     判定函数（同型移植 gateway_handler.go:2140 heartbeatFailoverGuardStillClean，
//     判定逻辑唯一；Responses 路径的 compact 调整后口径由调用方先归一后传入）。
//  ② cf524InstallUpstreamBudgetAndHeartbeatOpenAI —— 入口安装 helper（包装既有
//     cf524InstallUpstreamBudgetAndHeartbeat 等价语义；安装条件与主方案逐字相同，
//     并增加 compact keepalive 互斥这一唯一新增判定）。
//
// 踩坑对照：#1 基线同时点捕获由调用方保证，本函数/helper 只做比较/安装；
// #2/#3 双侧 normalizeSize(<0→0)，任何 Size 结论对照 gin v1.9.1 response_writer
// 源码（不口头推断）；#8 失败关闭无兜底；#13 同请求同时刻仅一个拍频机制。

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// cf524OpenAICompactKeepaliveKey 是 body-signal compact 请求下游 SSE 心跳器在
// gin context 中的事实源 key（与 service.openAICompactSSEKeepaliveKey 同值；因该
// 常量在 service 包未导出，本 helper 仅消费其"是否写入"的安装状态，使用字面量同值
// key 读取，禁止在本 helper 重复推导 compact 标记）。
//
// 调用顺序不变量（方案 §3.1 确认审 R2 P1）：Responses 入口中
// service.StartOpenAICompactSSEKeepalive 必须先于本 helper 执行；helper 执行时
// key 必已写入（含 no-op 退出——no-op 不写 key 即事实源为"未安装"）。
const cf524OpenAICompactKeepaliveKey = "openai_compact_sse_keepalive"

// cf524OpenAIHeartbeatFailoverGuardStillClean 是 OpenAI 家族"写后禁 failover"防线的
// 共享判定（CF524 例外判定，与 gateway_handler.go:2140 heartbeatFailoverGuardStillClean
// 逐字同型）：心跳注释帧非语义写出，不得把心跳已提交字节误判为"流已写出"。
//
// 判定采用基线增量**严格相等**：调用方在捕获 writerSizeBeforeForward 的**同时点**捕获
// 心跳已提交字节基线 hbCommittedBaseline（owner 存在时 CommittedBytes()，否则 0），
// 本函数只做 `(归一后 size - 归一后 before) == (committed - hbCommittedBaseline)` 比较。
// owner 不存在 / 自基线零心跳提交时 delta=0，退化与现状判定 `Size()==before` 完全一致
// （同一路径，无第二分支）。
//
// G5d 哨兵归一：gin.Size() 的 noWritten 哨兵为 -1，首次 Write/Flush 经 WriteHeaderNow()
// 把 size 从 -1 翻转为 0 再累加。该翻转发生在捕获点之后、组件快照之外，不对称来源会令
// 纯心跳在零写出捕获场景被误判为语义写出、错误禁止 failover（第二轮外审 P1 回归）。
// 故进入判定先把 writerSizeBeforeForward 与实测 size 归一为逻辑尺寸（<0→0），所有比较
// 用归一值。无心跳分支 `size==before` 结果与现状逐一等价（-1==-1 ⟺ 0==0；-1+S==-1 恒假
// ⟺ S+0==0 恒假）。
//
// Responses 路径口径：调用方须先把 OpenAICompactKeepaliveAdjustedWrittenSize(c) 返回值
// 过同一 normalizeSize(<0→0) 再作为 writerSizeBeforeForward 传入（compact keepalive 字节
// 与心跳字节两个扣除来源独立、不得合并计数）。本函数自身不感知 compact 标记。
func cf524OpenAIHeartbeatFailoverGuardStillClean(c *gin.Context, writerSizeBeforeForward, hbCommittedBaseline int) bool {
	if c == nil || c.Writer == nil {
		return true
	}
	// gin noWritten(-1) 逻辑尺寸归一（注释见上）。
	normalizeSize := func(v int) int {
		if v < 0 {
			return 0
		}
		return v
	}
	size := normalizeSize(c.Writer.Size())
	before := normalizeSize(writerSizeBeforeForward)
	hb, _ := service.UpstreamHeartbeatFromContext(c.Request.Context())
	if hb == nil {
		return size == before
	}
	committed := int(hb.CommittedBytes())
	if committed == hbCommittedBaseline {
		// 自基线以来零心跳提交：退化为既有 size==before 相等判定（同一路径）。
		return size == before
	}
	writtenDelta := size - before
	heartbeatDelta := committed - hbCommittedBaseline
	return writtenDelta == heartbeatDelta
}

// cf524InstallUpstreamBudgetAndHeartbeatOpenAI 是 OpenAI 家族三入口（ChatCompletions /
// Responses / Messages）在认证后、选号循环前调用的请求入口安装 helper。包装既有
// cf524InstallUpstreamBudgetAndHeartbeat 的等价语义；安装条件与主方案逐字相同：
//   - guardSeconds<=0 不安装（未接入/测试桩，与"无快照不兜底"契约一致）；
//   - 非流式（reqStream==false）/ 心跳禁用（hb<=0）只装快照、不起搏心跳 owner；
//   - Flush 缺失 → 失败关闭（Start 返回错误、零写出）+ 记 warn，不静默降级为无心跳。
//
// compact 互斥（方案 §3.1 唯一新增判定）：只消费 compact keepalive 的"真实安装状态"
// （gin context key cf524OpenAICompactKeepaliveKey）——已安装 → 本 helper 不再装心跳
// owner（快照照装），避免同 writer 双拍频数据竞争（踩坑 #13）；key 不存在 / no-op（未
// 写入 key）→ 正常安装恰一个心跳 owner。禁止在本 helper 重复推导
// openAICompactClientWantsStream / compact 标记——调用方保证 keepalive start 先于本 helper。
func (h *GatewayHandler) cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c *gin.Context, requestStart time.Time, reqStream bool) (stopFunc func()) {
	if c == nil || c.Request == nil {
		return func() {}
	}
	guardSeconds, heartbeatSeconds := h.cf524GuardConfig()
	if guardSeconds <= 0 {
		// 无配置（未接入/测试桩）时不安装：与挂点"无快照不兜底"契约一致。
		return func() {}
	}
	snapshot := service.NewRequestBudgetSnapshot(guardSeconds, heartbeatSeconds, requestStart, reqStream)
	ctx := service.WithRequestBudgetSnapshot(c.Request.Context(), snapshot)

	// compact 互斥：只消费 compact keepalive 的真实安装状态（不重复推导 compact 标记）。
	// 已安装 → 本 helper 不再装心跳 owner（快照照装）；key 不存在 / no-op → 正常装一个。
	if _, compactInstalled := c.Get(cf524OpenAICompactKeepaliveKey); compactInstalled {
		c.Request = c.Request.WithContext(ctx)
		return func() {}
	}

	if !reqStream || heartbeatSeconds <= 0 {
		// 非流式 / 心跳禁用：只装快照（护栏仍生效），不起搏心跳。
		c.Request = c.Request.WithContext(ctx)
		return func() {}
	}

	hb := service.NewUpstreamHeartbeat(ctx, c.Writer, snapshot, time.Now())
	ctx = service.WithUpstreamHeartbeat(ctx, hb)
	c.Request = c.Request.WithContext(ctx)

	// 起搏：Start 与后续 Resume 同型，首次安装即开始等待首帧窗口。
	// Flush 能力缺失时 Start 返回错误、零写出——失败关闭，不静默降级；错误可见性：
	// 记 warn 日志（含 path/request_id 可关联字段）后继续既有流程，不终止请求。
	if err := hb.Start(); err != nil {
		logHeartbeatStartFailure(c.Request.Context(), c.Request.URL.Path, err)
	}

	return func() { hb.Stop() }
}
