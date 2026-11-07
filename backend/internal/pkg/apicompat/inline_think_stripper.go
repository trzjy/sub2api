package apicompat

// 本文件提供跨帧有状态地剥离内联思考标签（<think>/<thinking>）的共用组件，
// 从 backend/internal/service/cursor_protocol_response.go 的 cursor 侧
// parseThinkDeltas / flushThinkTail 逻辑直接移植，改为公开类型，供 cursor 通道
// 与 CC→Anthropic bridge 复用。
//
// 与 cursor 侧的差异（已由调用方自行处理，本组件不再承担）：
//   - 不做 rawContent 累加（上游注入模板匹配由调用方维护原始文本）；
//   - 不做 finalizeVisible（剥离 <think> 后残留的前导空白清理）——
//     该项为 cursor 专属语义，调用方按自身需要自己清理。
//
// 双通道纪律（与 cursor 侧一致）：
//  1. 完整开/闭标签一旦匹配即彻底吞噬——既不进 reasoning 也不进 text；
//  2. 遇到标签前缀（"<"、`<t`、"</"、`</th` 等）用 TrailingPartialTag 把"可能是半截
//     标签"的那几个字节严格扣留在内部缓冲，绝不提前外发，跨帧切碎的标签因此被 100% 吞噬；
//  3. reasoning 与 text 是两个独立通道，互不污染。

import "strings"

// ThinkTagVariant 描述一对内联思考标签（开标签 + 配对的闭标签）。
type ThinkTagVariant struct {
	// Open 是开标签（如 "<think>"）。
	Open string
	// Close 是与 Open 严格配对的闭标签（如 "</think>"）。
	Close string
}

// InlineThinkTagVariants 是所有受支持的内联思考标签变体，按优先级排列。
// <thinking> 必须排在 <think> 之前，避免短串前缀误命中。
var InlineThinkTagVariants = []ThinkTagVariant{
	{Open: "<thinking>", Close: "</thinking>"},
	{Open: "<think>", Close: "</think>"},
}

// EarliestThinkOpen 在 s 中找所有 variant 开标签的最早出现位置，
// 返回 (index, variant)；都不存在时返回 (-1, zero-value)。
func EarliestThinkOpen(s string) (int, ThinkTagVariant) {
	best := -1
	var bestV ThinkTagVariant
	for _, v := range InlineThinkTagVariants {
		idx := strings.Index(s, v.Open)
		if idx >= 0 && (best < 0 || idx < best) {
			best = idx
			bestV = v
		}
	}
	return best, bestV
}

// MaxTrailingPartialThinkOpen 返回 s 末尾可能是任意开标签前缀的最长扣留段。
func MaxTrailingPartialThinkOpen(s string) string {
	best := ""
	for _, v := range InlineThinkTagVariants {
		hold := TrailingPartialTag(s, v.Open)
		if len(hold) > len(best) {
			best = hold
		}
	}
	return best
}

// TrailingPartialTag 返回 s 末尾"可能是 tag 不完整前缀"的那一段（跨帧缓冲用）。
// 与 cursor 侧同名函数语义完全一致，提取到此处作为共用基础函数。
//
// 例：s 以 "<thi" 结尾、tag 是 "<think>" → 返回 "<thi"；以 "他说" 结尾 → ""。
func TrailingPartialTag(s, tag string) string {
	max := len(tag) - 1
	if max > len(s) {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return s[len(s)-n:]
		}
	}
	return ""
}

// InlineThinkStripper 跨帧有状态地剥离 <think>/<thinking> 内联标签。
// 每个流实例独立持有状态，不可并发使用。
type InlineThinkStripper struct {
	// buf 是跨帧前缀扣留缓冲（对应 cursor 侧的 thinkRaw）。
	buf string
	// inTag 表示当前处于标签内（段内内容作为 reasoning）。
	inTag bool
	// activeClose 是进入时命中的开标签所对应的闭标签。
	activeClose string
	// flushed 保证 Flush 的尾段只吐出一次。
	flushed bool
}

// Feed 输入一个 delta，返回 (reasoning, text)。
// reasoning = 标签内内容；text = 标签外正文。
// 跨帧截断的标签前缀被扣留在内部缓冲，等待后续 delta 补全再判断。
//
// 与 cursor 侧 parseThinkDeltas 语义完全一致，但不累加 rawContent（调用方自行管理）
// 也不做 finalizeVisible（前导空白清理，调用方自行处理）。
func (s *InlineThinkStripper) Feed(delta string) (reasoning, text string) {
	if delta == "" {
		return "", ""
	}
	s.buf += delta
	var rb, tb strings.Builder
	for {
		if s.inTag {
			idx := strings.Index(s.buf, s.activeClose)
			if idx < 0 {
				// 尚未见到闭合标签：把"可能是闭合标签前缀"的那段扣留，其余作为
				// 思考增量吐出（流式过程中增量交付，避免假死）。
				hold := TrailingPartialTag(s.buf, s.activeClose)
				cut := len(s.buf) - len(hold)
				if cut > 0 {
					rb.WriteString(s.buf[:cut])
				}
				s.buf = hold
				return rb.String(), tb.String()
			}
			// 命中闭合标签：标签之前（段内）的思考内容进 reasoning，标签本身吞噬。
			if idx > 0 {
				rb.WriteString(s.buf[:idx])
			}
			s.buf = s.buf[idx+len(s.activeClose):]
			s.inTag = false
			continue
		}
		idx, v := EarliestThinkOpen(s.buf)
		if idx >= 0 {
			// 开标签之前是可见正文，开标签本身吞噬，进入思考态。
			if idx > 0 {
				tb.WriteString(s.buf[:idx])
			}
			s.buf = s.buf[idx+len(v.Open):]
			s.inTag = true
			s.activeClose = v.Close
			continue
		}
		// 未见开标签：扣留"可能是任意开标签前缀"的那段（取最长扣留量），其余作为正文增量吐出。
		hold := MaxTrailingPartialThinkOpen(s.buf)
		cut := len(s.buf) - len(hold)
		if cut > 0 {
			tb.WriteString(s.buf[:cut])
		}
		s.buf = hold
		return rb.String(), tb.String()
	}
}

// Flush 强制吐出剩余缓冲（流结束时调用），语义对应 cursor 侧 flushThinkTail。
// 返回 (reasoning, text)；只吐一次（内部 flushed 守卫）。
func (s *InlineThinkStripper) Flush() (reasoning, text string) {
	if s.flushed {
		return "", ""
	}
	s.flushed = true
	if s.inTag {
		// 未闭合推理段：把已缓冲的思考尾巴作为 reasoning 吐出（先切除可能的闭合标签前缀），
		// 不再丢弃——消除"思考内容被静默吞噬 + 下游假死"的问题。
		closeTag := s.activeClose
		if closeTag == "" {
			closeTag = "</think>" // fallback：正常流不会走到这里
		}
		hold := TrailingPartialTag(s.buf, closeTag)
		cut := len(s.buf) - len(hold)
		reasoning := s.buf[:cut]
		s.buf = ""
		return reasoning, ""
	}
	tail := s.buf
	s.buf = ""
	return "", tail
}
