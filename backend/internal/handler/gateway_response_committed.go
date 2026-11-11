package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// gatewayResponseCommitted 报告本次响应是否已经提交（HTTP 状态码已固化为 200）。
//
// 这是「错误该写成流内 SSE 帧，还是写成带真实状态码的 JSON」这条规则的**唯一事实源**。
//
// 2026-11-11 根因修复（并发满立即拒绝改造的连带回归）：该规则此前在 10+ 处各自拼装
// 且口径互不一致——OpenAI 路径有一处完整三合一（openai_gateway_handler.go 的
// committed := streamStarted || c.Writer.Written() || service.IsResponseCommitted(c)），
// Anthropic 路径三处只看 streamStarted，其余各处靠局部
// `if c.Writer.Written() { streamStarted = true }` 就地同步。
//
// 「客户端维度并发满立即拒绝」改造删掉了准入阶段的 SSE ping 之后，Anthropic 路径的
// streamStarted 实质恒为 false（唯一按指针传入的等待路径已不再置真）。而 CF524 上游
// 心跳在请求入口安装、15s 后就提交 SSE 200，**早于**账号维度等待超时（30s/45s）：
//   - 15s：心跳提交 SSE 200，响应头已固化；
//   - 30s/45s：账号维度等待超时 → 错误出口看到 streamStarted=false → 走 JSON 分支；
//   - 结果：真实 503 丢失（客户端只看到 200），且 JSON 错误体被塞进已开的 SSE 流。
//
// 三个信号取或，任一为真即视为已提交：
//  1. streamStarted：调用方已知的「已开始流式输出」标志。
//  2. c.Writer.Written()：响应头/字节已物理写出——CF524 心跳提交 SSE 200 即属此列。
//  3. service.IsResponseCommitted(c)：service 层已向客户端提交过响应。
//
// 判据只有这一个函数；任何新增的错误出口都调它，不许再就地拼装。
func gatewayResponseCommitted(c *gin.Context, streamStarted bool) bool {
	if streamStarted {
		return true
	}
	if c == nil || c.Writer == nil {
		return false
	}
	if c.Writer.Written() {
		return true
	}
	return service.IsResponseCommitted(c)
}

// gatewayResponseCommittedAsSSE 报告本次响应是否已提交为 **SSE 流**形态
// （即：此时错误只能以流内帧回传，不能再写独立 JSON 错误体）。
//
// 与 gatewayResponseCommitted 的区别只有一处：已提交为「JSON 文档」形态的响应
// （OpenAI 图片接口的非流式空白保活——提交的是 application/json，后续错误仍须
// 写成 JSON 体，否则客户端拿到半截 JSON），不算 SSE 流。
//
// 两条路径各自的语义：
//   - Anthropic 系（/v1/messages、/v1/chat/completions、/v1/responses 的 Anthropic
//     形状）：该路径只会产出 SSE 流，不存在 JSON 中间态，故 committed 即 SSE，
//     消费方直接用 gatewayResponseCommitted。
//   - OpenAI 系：可能产出 JSON 文档形态，消费方用本函数。
//
// 判据只有这一个函数；任何新增的错误出口都调它，不许再就地拼装。
func gatewayResponseCommittedAsSSE(c *gin.Context, streamStarted bool) bool {
	if !gatewayResponseCommitted(c, streamStarted) {
		return false
	}
	if streamStarted {
		// 调用方明确知道已开始流式输出——必然是 SSE 流。
		return true
	}
	// OpenAI 图片接口的非流式空白保活提交的是 JSON 文档，不是 SSE 流。
	if service.OpenAIImagesJSONKeepalivePresent(c) {
		return false
	}
	return true
}

// gatewayStreamHeartbeatBytes 返回本请求已写出的、**仅由心跳产生**的 body 字节数。
//
// 口径收口（2026-11-11 根因修复）：心跳字节此前记在两个互不相通的地方——
//   - 准入 ping / 用户消息队列 ping 记在 handler 层 key（gatewayStreamHeartbeatBytesKey）；
//   - CF524 上游心跳记在 service 层 owner 的 CommittedBytes()。
// 两者都是「非语义输出」，缺一不可。漏掉后者会让 /v1/responses 在心跳已提交 200 后
// 换号耗尽时误判「已有语义输出」，从而漏补 response.failed 终止事件，Codex CLI 报
// "stream closed before response.completed"。
//
// 两处记账互不重叠（各自只记自己写出的字节），因此直接相加。
func gatewayStreamHeartbeatBytes(c *gin.Context) int {
	if c == nil {
		return 0
	}
	total := 0
	if value, ok := c.Get(gatewayStreamHeartbeatBytesKey); ok {
		total, _ = value.(int)
	}
	if c.Request != nil {
		if hb, _ := service.UpstreamHeartbeatFromContext(c.Request.Context()); hb != nil {
			total += int(hb.CommittedBytes())
		}
	}
	return total
}

// gatewayStreamHasOnlyHeartbeats 报告流内已写字节是否**全部**来自心跳（尚无语义输出）。
//
// 语义：已提交响应但只写过心跳/保活注释时，仍然需要补协议终止事件
// （response.failed / SSE error 帧），否则客户端拿到 silent EOF。
func gatewayStreamHasOnlyHeartbeats(c *gin.Context) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	heartbeatBytes := gatewayStreamHeartbeatBytes(c)
	if heartbeatBytes <= 0 {
		return false
	}
	size := c.Writer.Size()
	if size < 0 {
		// gin noWritten 哨兵归一为逻辑尺寸 0（口径与 service 层 sizeNow 一致）。
		size = 0
	}
	return size == heartbeatBytes
}
