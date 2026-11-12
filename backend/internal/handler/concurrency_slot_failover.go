package handler

import "github.com/gin-gonic/gin"

// 账号槽位等待失败 → 换同组下一个账号。
//
// 现状（2026-11-12 生产实证：当日 47 条 503「上游账号并发已达上限，请稍后重试」）：
// 账号槽位等不到时，handler 直接写 503 返回，同组其它空闲账号一次都不试。
// 生产上同组的其它账号（goaichat 组 240/332/334）在同一时段都有成功量 —— 这不是
// 整组供给不足，而是「选中的账号此刻正被别人占满」被误报成了供给不足。
//
// 用户锁定裁定（2026-11-11）「账号维度（供给不足）保留等待语义不变」锁的是**终点契约**：
// 候选耗尽后仍返回 503 + 同一文案 + gateway_concurrency_limit，而不是「等待前不许换号」。
// 本改动只把「死等一个满载账号」改成「等不到就换同组下一个账号再等」：
//   - 等待优先的语义完全保留（粘性 / 上游 prompt cache 收益不受影响）；
//   - 只有在「已经等不到」这一刻才换号，不存在「本来能等到的账号被提前跳过」；
//   - 候选耗尽后仍是 503 + 同一文案 + 同一 code，客户端可观测行为不变。
//
// 判定与计数收敛在本文件，Anthropic 链（FailoverState.FailedAccountIDs）与
// OpenAI 链（failedAccountIDs map）共用同一份实现，不各写一条分支。

// concurrencySlotWaitExhaustedKey 标记「本次请求已经等过一次账号槽位且没等到」。
//
// 用户 2026-11-12 裁定：**换号后不等**。等满一次（sticky 45s / fallback 30s）之后，
// 换到下一个账号就**不再排队等待**——下一个账号多半是空闲的（刚被排除的那个才是满的），
// 等它是白等；而且这样总等待时长 = 一次超时，不会放大成 N 倍。
//
// 用请求级标记而不是逐入口传参：所有入口共用一份语义，不会各写一遍。
const concurrencySlotWaitExhaustedKey = "concurrency_slot_wait_exhausted"

// markConcurrencySlotWaitExhausted 标记「本次请求已经等过一次且没等到」。
func markConcurrencySlotWaitExhausted(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(concurrencySlotWaitExhaustedKey, true)
}

// concurrencySlotWaitExhausted 读「本次请求是否已经等过一次账号槽位且没等到」。
func concurrencySlotWaitExhausted(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool(concurrencySlotWaitExhaustedKey)
}

// recordConcurrencySlotFailure 把槽位等不到的账号记入本请求排除集合，
// 返回是否应继续换同组下一个账号（恒 true —— 耗尽由「选号返回无候选」判定）。
//
// 排除集合的写入与传递链路是既有的：Anthropic 链用 FailoverState.FailedAccountIDs，
// OpenAI 链用各自的 failedAccountIDs map，两者都已在选号内部逐层过滤
// （service 侧 isExcluded / ExcludedIDs）。
//
// 注意语义边界：槽位满是**瞬时**状态（几十秒后可能就空了），把它写进「已失败账号」
// 集合只影响**本次请求**的候选集，不停号、不改账号状态。
func recordConcurrencySlotFailure(failedAccountIDs map[int64]struct{}, accountID int64, attempts *int) bool {
	if failedAccountIDs == nil || attempts == nil {
		return false
	}
	failedAccountIDs[accountID] = struct{}{}
	*attempts++
	// 恒返回 true：候选耗尽由「选号返回无候选」判定，不以尝试次数代替耗尽
	// （外审第三轮 must_fix #1）。attempts 只作死循环兜底计数。
	return true
}

// concurrencySlotExhaustedResponse 给出槽位换号耗尽后的终态错误三元组。
//
// 输出与「槽位等不到」的既有出口**逐字节一致**：503 + api_error +
// gateway_concurrency_limit + 中文账号槽并发文案。
//
// account 维度不渲染可配模板、也不使用 {limit} 占位符
// （见 concurrency_error_response.go 的 renderConcurrencyLimitMessage），
// 因此这里不需要还原触发时的具体上限值。
func concurrencySlotExhaustedResponse() (int, string, string, string) {
	return concurrencyErrorResponse(&ConcurrencyError{SlotType: "account"}, "account", "")
}

// concurrencySlotWriteErrorKey 暂存「本端点写错误响应的回调」。
//
// 各端点的错误格式不同（Responses 走 handleStreamingAwareErrorWithCode，
// chat/alpha_search/embeddings/images/grok 各自有自己的 writeError），槽位换号
// 耗尽时必须沿用**本端点**的格式，否则同一个 503 在不同入口长得不一样。
// 这里只暂存本请求内的一个回调，不跨请求、不进日志。
const concurrencySlotWriteErrorKey = "concurrency_slot_write_error"

// setConcurrencySlotWriteError 暂存本端点的错误写出回调。
func setConcurrencySlotWriteError(c *gin.Context, writeError func(status int, errType, code, message string)) {
	if c == nil || writeError == nil {
		return
	}
	c.Set(concurrencySlotWriteErrorKey, writeError)
}

// writeConcurrencySlotExhausted 写出槽位换号耗尽后的终态错误。
//
// 优先用本端点自己的 writeError（与该端点其它错误的格式一致），
// 没有暂存时退回 OpenAI 链的默认写法。
func writeConcurrencySlotExhausted(c *gin.Context, h *OpenAIGatewayHandler, streamStarted bool) {
	writeConcurrencySlotExhaustedWith(c, func(status int, errType, code, message string) {
		h.handleStreamingAwareErrorWithCode(c, status, errType, code, message, streamStarted, false)
	})
}

// writeConcurrencySlotExhaustedWith 用调用方给定的写出方式写槽位换号耗尽终态。
//
// 供不走统一流式错误出口的端点使用（如 alpha_search / embeddings / grok_media
// 走 h.errorResponse），保证终态格式与该端点其它错误一致。
func writeConcurrencySlotExhaustedWith(c *gin.Context, writeError func(status int, errType, code, message string)) {
	status, errType, code, message := concurrencySlotExhaustedResponse()
	if c != nil {
		if v, ok := c.Get(concurrencySlotWriteErrorKey); ok {
			if fn, ok := v.(func(status int, errType, code, message string)); ok && fn != nil {
				fn(status, errType, code, message)
				return
			}
		}
	}
	if writeError != nil {
		writeError(status, errType, code, message)
	}
}
