package service

// 上游重复 token 循环熔断（repetition-loop breaker）。
//
// 方案：docs/repetition-loop-breaker-plan.md。智谱官方 glm 账号对长上下文
// 请求偶发退化输出：从本轮第一个 delta 起无限流同一 token（如 `!`），单笔
// 3.2万~4.8万 token、流 7~27 分钟才被 max_tokens 掐断。本组件在流式转发层
// 逐 delta 比较，同一 delta 连续重复 ≥ N 次（N 默认 768，env
// REPETITION_LOOP_THRESHOLD 可调，0 表示禁用）判定为循环。
//
// 两阶段防护（v1 覆盖 CN Anthropic 原生直通路径
// handleNativeAnthropicStreamingResponse，即实测命中路径）：
//   - 缓冲期静默 failover：流开始后先缓冲不转发，缓冲满 N 个 delta 仍无循环
//     → flush 转直通；缓冲期内命中 → 丢弃缓冲、掐上游、按 UpstreamFailoverError
//     走既有 maxAccountSwitches 换号重发（客户端无感）。
//   - 直通期命中 → 掐上游 + 客户端 error 事件（code=upstream_repetition_loop）
//     + ops_error_logs + 账号健康错误统计（feeds 既有健康熔断）。
//
// TODO(repetition-breaker): 其余流式路径（chat_completions、responses、
// CC/Responses×anthropic 转换链）暂未接入本检测器，后续按同一组件模式接入。

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// repetitionLoopErrorCode 是循环熔断对外的 error_code，用于客户端识别与
// ops_error_logs 事后统计。
const repetitionLoopErrorCode = "upstream_repetition_loop"

// defaultRepetitionLoopThreshold 与方案文档一致：同一 token 连续 768 次
// 在自然文本/代码中不存在合法场景。
const defaultRepetitionLoopThreshold = 768

// repetitionLoopThresholdEnv 环境变量名。
const repetitionLoopThresholdEnv = "REPETITION_LOOP_THRESHOLD"

// repetitionLoopThreshold 读取阈值配置；未配置/非法/负值回退默认 768，
// 0 显式表示禁用检测。
func repetitionLoopThreshold() int {
	raw := strings.TrimSpace(os.Getenv(repetitionLoopThresholdEnv))
	if raw == "" {
		return defaultRepetitionLoopThreshold
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultRepetitionLoopThreshold
	}
	return n
}

// repetitionLoopDetector 逐 delta 比较同一 token 的连续重复次数。
// 非并发安全：仅在流式转发单 goroutine 内使用。
type repetitionLoopDetector struct {
	threshold int
	last      string
	count     int
}

// observe 记录一个文本增量，返回连续重复计数是否已达阈值。
// threshold<=0 恒返回 false（禁用）。
func (d *repetitionLoopDetector) observe(delta string) bool {
	if d.threshold <= 0 {
		return false
	}
	if delta == d.last {
		d.count++
	} else {
		d.last = delta
		d.count = 1
	}
	return d.count >= d.threshold
}

// repGuardAction 是 guard 对单个上游 SSE 行的裁决。
type repGuardAction int

const (
	// repGuardHold：缓冲期持有该行，调用方跳过写出（继续读上游）。
	repGuardHold repGuardAction = iota
	// repGuardWrite：调用方按原逻辑写出该行。
	repGuardWrite
	// repGuardRelease：缓冲满转直通，调用方先 flush 缓冲行再按原逻辑写当前行。
	repGuardRelease
	// repGuardFailover：缓冲期命中循环，缓冲已丢弃；调用方掐上游并返回
	// failover 错误走既有换号重试。
	repGuardFailover
	// repGuardAbort：直通期命中循环；调用方掐上游并向客户端下发 error 事件。
	repGuardAbort
)

// streamRepetitionGuard 是流式转发层的循环熔断状态机：缓冲期逐行持有并
// 观测文本增量，缓冲满 N 个 delta 一次性放行转直通；任一阶段检测到同一
// delta 连续重复达阈值即裁决中止。
type streamRepetitionGuard struct {
	threshold int
	detector  repetitionLoopDetector
	buffering bool
	buffer    []string
	// bufferedDeltas 是缓冲期内已观测的文本增量数（与 buffer 行数不同：
	// 一个 delta 事件可能伴随 event:/空行等多个 SSE 行），缓冲满 threshold
	// 个 delta 即转直通。
	bufferedDeltas int
	tripped        bool
}

// newStreamRepetitionGuard 构造 guard；阈值 <=0（禁用）时返回 nil，调用方
// 跳过全部检测逻辑（fail-open：配置关闭即完全旁路）。
func newStreamRepetitionGuard() *streamRepetitionGuard {
	threshold := repetitionLoopThreshold()
	if threshold <= 0 {
		return nil
	}
	return &streamRepetitionGuard{
		threshold: threshold,
		detector:  repetitionLoopDetector{threshold: threshold},
		buffering: true,
	}
}

// onLine 处理一个上游 SSE 行（含行尾换行前的原始文本），返回裁决。
// fail-open：任何 panic 恢复后放行原流，不影响可用性。
func (g *streamRepetitionGuard) onLine(line string) (action repGuardAction) {
	if g == nil || g.tripped {
		return repGuardWrite
	}
	defer func() {
		if r := recover(); r != nil {
			// fail-open：检测器异常时放行原流，丢弃缓冲语义退化为直通。
			g.buffering = false
			g.buffer = nil
			g.tripped = false
			action = repGuardWrite
		}
	}()

	data, ok := extractAnthropicSSEDataLine(line)
	if !ok {
		if g.buffering {
			g.buffer = append(g.buffer, line)
			return repGuardHold
		}
		return repGuardWrite
	}

	delta := anthropicStreamDeltaTextFn(data)
	if delta == "" {
		if g.buffering {
			g.buffer = append(g.buffer, line)
			return repGuardHold
		}
		return repGuardWrite
	}

	if g.detector.observe(delta) {
		g.tripped = true
		g.buffer = nil
		if g.buffering {
			return repGuardFailover
		}
		return repGuardAbort
	}

	if g.buffering {
		if g.bufferedDeltas+1 >= g.threshold {
			// 当前 delta 是缓冲期内第 threshold 个且无循环：转直通。
			// 当前行不入缓冲，由调用方先 flush 缓冲再按原逻辑写出。
			g.buffering = false
			return repGuardRelease
		}
		g.buffer = append(g.buffer, line)
		g.bufferedDeltas++
		return repGuardHold
	}
	return repGuardWrite
}

// drain 返回并清空缓冲行。缓冲期命中（repGuardFailover）后调用返回 nil，
// 缓冲已丢弃、客户端零输出，可静默换号重发。
func (g *streamRepetitionGuard) drain() []string {
	if g == nil {
		return nil
	}
	lines := g.buffer
	g.buffer = nil
	return lines
}

// anthropicStreamDeltaText 从 Anthropic SSE data 行提取文本增量：
// content_block_delta 的 text_delta.text / thinking_delta.thinking。
// 非 delta 事件返回空串。快速预筛避免对每行做完整 JSON 解析。
func anthropicStreamDeltaText(data string) string {
	if !strings.Contains(data, `"content_block_delta"`) {
		return ""
	}
	var event struct {
		Type  string `json:"type"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return ""
	}
	if event.Type != "content_block_delta" {
		return ""
	}
	switch event.Delta.Type {
	case "text_delta":
		return event.Delta.Text
	case "thinking_delta":
		return event.Delta.Thinking
	}
	return ""
}

// anthropicStreamDeltaTextFn 是提取函数的间接层，供 fail-open 单测注入 panic。
var anthropicStreamDeltaTextFn = anthropicStreamDeltaText

// newRepetitionLoopFailoverError 构造缓冲期循环命中的 failover 错误：
// 走既有 upstream-error 换号路径（handler 的 maxAccountSwitches 循环）。
// SafeToFailoverAfterWrite=true：缓冲期客户端未收到任何语义字节，仅可能
// 收到网关注入的 keepalive ping 等非语义字节，同流换号重发安全。
func newRepetitionLoopFailoverError() *UpstreamFailoverError {
	return &UpstreamFailoverError{
		StatusCode:               http.StatusBadGateway,
		SafeToFailoverAfterWrite: true,
	}
}

// buildAnthropicStreamErrorCodeSSE 构造带 error_code 的 Anthropic SSE error
// 事件，供直通期循环命中时向客户端下发可识别错误。
func buildAnthropicStreamErrorCodeSSE(errType, code, message string) string {
	payload, err := json.Marshal(gin.H{
		"type": "error",
		"error": gin.H{
			"type":  errType,
			"code":  code,
			"message": message,
		},
	})
	if err != nil {
		return "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"" + errType + "\",\"code\":\"" + code + "\",\"message\":\"upstream error\"}}\n\n"
	}
	return "event: error\ndata: " + string(payload) + "\n\n"
}
