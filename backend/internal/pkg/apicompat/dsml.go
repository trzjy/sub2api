package apicompat

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// DSML = DeepSeek Markup Language —— DeepSeek-V3.2 / V4 / V4.1 的官方原生工具调用方言。
//
// 上游 CodeBuddy（/v2/chat/completions）按 DeepSeek 官方规范把工具调用写成 DSML 文本，
// 放在 CC 响应的 reasoning_content / content 字段里。本文件按官方规范把它还原成标准
// tool_calls；还原不了的部分按"看不懂就丢弃 + 记账"处理，绝不让协议标签外泄给客户端。
//
// 官方来源：
//   - DeepSeek 官方参考实现 encoding/encoding.py（HF: deepseek-ai/DeepSeek-V4.1-Flash-1017）
//   - DeepSeek 官方生产库 deepseek-recipe（Rust + Python 绑定，协议感知）
//   - vLLM 官方 vllm/parser/deepseek_v4.py（Apache-2.0，commit fb5291b3）
//
// 本实现与 vLLM 官方解析器保持一致的三条要点：
//  1. string="true" 的值原样保留，禁止 strip —— strip 会吃掉 Edit 的缩进、
//     Write 的末尾换行、命令的尾随空格，导致客户端执行的不是模型写的那个调用
//     （依据 NVIDIA TensorRT-LLM PR #21025）。
//  2. string 属性是可选的；缺失时按 string="false" 处理（先试 JSON，失败回退字面文本）。
//  3. 一次调用可以有多次 invoke（并行工具调用）。
const (
	dsmlMarker = "｜DSML｜"

	// V4 方言：标签名无前导空格。
	dsmlV4ToolStart      = "<" + dsmlMarker + "tool_calls>"
	dsmlV4ToolEnd        = "</" + dsmlMarker + "tool_calls>"
	dsmlV4InvokePrefix   = "<" + dsmlMarker + "invoke name=\""
	dsmlV4InvokeEnd      = "</" + dsmlMarker + "invoke>"
	dsmlV4ParamStart     = "<" + dsmlMarker + "parameter"
	dsmlV4ParamClose     = "</" + dsmlMarker + "parameter>"

	// V4.1 方言：标签名带前导空格（V4.1 相对 V4 的关键差异）。
	dsmlV41ToolStart    = "<" + dsmlMarker + " calls>"
	dsmlV41ToolEnd      = "</" + dsmlMarker + " calls>"
	dsmlV41InvokePrefix = "<" + dsmlMarker + " invoke name=\""
	dsmlV41InvokeEnd    = "</" + dsmlMarker + " invoke>"
	dsmlV41ParamStart   = "<" + dsmlMarker + " parameter"
	dsmlV41ParamClose   = "</" + dsmlMarker + " parameter>"
)

// dsmlToolStartVariants 是生产中出现过的外层标签变体（vLLM 官方同样收录）。
var dsmlToolStartVariants = []string{
	"<" + dsmlMarker + "toolcalls>",
	"<" + dsmlMarker + "tool>",
}

// dsmlDialect 描述一种 DSML 方言的标签拼写与已编译正则。
type dsmlDialect struct {
	name string

	toolStart     string
	toolEnd       string
	invokePrefix  string
	invokeEnd     string
	paramStart    string
	paramClose    string

	// paramHeadRe 匹配参数头（name / 可选 string 属性），用于逐个切分参数。
	paramHeadRe *regexp.Regexp
	// partialParamRe 匹配尾部未闭合参数（值吃到文本末尾）。
	partialParamRe *regexp.Regexp

	invokeRe *regexp.Regexp
}

// dsmlDialects 按检测顺序排列：V4 无空格优先（线上出现次数更多），V4.1 带空格其次。
var dsmlDialects = buildDSMLDialects()

func buildDSMLDialects() []dsmlDialect {
	specs := []struct {
		name        string
		toolStart   string
		toolEnd     string
		invokeStart string
		invokeEnd   string
		paramStart  string
		paramClose  string
	}{
		{"V4", dsmlV4ToolStart, dsmlV4ToolEnd, dsmlV4InvokePrefix, dsmlV4InvokeEnd, dsmlV4ParamStart, dsmlV4ParamClose},
		{"V4.1", dsmlV41ToolStart, dsmlV41ToolEnd, dsmlV41InvokePrefix, dsmlV41InvokeEnd, dsmlV41ParamStart, dsmlV41ParamClose},
	}
	out := make([]dsmlDialect, 0, len(specs))
	for _, s := range specs {
		out = append(out, dsmlDialect{
			name:           s.name,
			toolStart:      s.toolStart,
			toolEnd:        s.toolEnd,
			invokePrefix:   s.invokeStart,
			invokeEnd:      s.invokeEnd,
			paramStart:     s.paramStart,
			paramClose:     s.paramClose,
			paramHeadRe:    dsmlParamHeadRegex(s.paramStart),
			partialParamRe: dsmlPartialParamRegex(s.paramStart, s.paramClose),
			invokeRe: regexp.MustCompile(
				regexp.QuoteMeta(s.invokeStart) + `([^"]+)"[^>]*>(?s:(.*?))` + regexp.QuoteMeta(s.invokeEnd),
			),
		})
	}
	return out
}

// dsmlParamHeadRegex 匹配该方言的参数头（name + 可选 string 属性）。
//
// 官方 vLLM 用 Python regex 的前瞻 (?=start\s+name=) 表达「参数值到下一个参数头为止」；
// Go 标准库 regexp 不支持前瞻，这里改为：先用参数头正则定位每个参数的起点，
// 值 = 本起点到「下一个参数头起点」或「闭合标签」之间的内容。
// 语义与官方一致：string 属性可选；缺 string 时按 false 处理。
func dsmlParamHeadRegex(paramStart string) *regexp.Regexp {
	head := regexp.QuoteMeta(paramStart) + `\s+name="([^"]+)"(?:\s+string="([^"]*)")?\s*>`
	return regexp.MustCompile(head)
}

// dsmlPartialParamRegex 与 vLLM 官方 _param_patterns 的尾部残缺参数正则一致
// （值一直吃到文本末尾）。
func dsmlPartialParamRegex(paramStart, _ string) *regexp.Regexp {
	head := regexp.QuoteMeta(paramStart) + `\s+name="([^"]+)"(?:\s+string="([^"]*)")?\s*>`
	return regexp.MustCompile(head + `(?s)(.*)$`)
}

// DSMLToolCall 是一条从 DSML 文本还原出来的工具调用。
type DSMLToolCall struct {
	// Name 是工具名（invoke 的 name 属性）。
	Name string
	// Arguments 是参数的 JSON 字符串（官方 _dsml_arg_converter 的输出形状）。
	Arguments string
}

// dsmlArgConverter 按官方规则把 DSML 参数原文转成 JSON。
//
// string="true" → 原样保留（禁止 strip）；
// string="false" 或属性缺失 → 先试 JSON，失败回退字面文本。
func (d dsmlDialect) dsmlArgConverter(rawArgs string, partial bool) string {
	params := map[string]any{}
	for _, p := range d.parseParams(rawArgs, partial) {
		params[p.name] = dsmlParamValue(p.isStr, p.value)
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// dsmlParamValueEntry 是一条解析出来的参数。
type dsmlParamValueEntry struct {
	name  string
	isStr string
	value string
}

// parseParams 逐个切出参数（值取到闭合标签或下一个参数头为止）。
//
// 官方 vLLM 用 Python regex 的前瞻 (?=start\s+name=) 表达「值到下一个参数头为止」；
// Go 标准库不支持前瞻，这里改为：先用参数头正则定位每个参数的起点，
// 值 = 本起点到「下一个参数头起点」或「闭合标签」之间的内容。
func (d dsmlDialect) parseParams(rawArgs string, partial bool) []dsmlParamValueEntry {
	heads := d.paramHeadRe.FindAllStringSubmatchIndex(rawArgs, -1)
	out := make([]dsmlParamValueEntry, 0, len(heads))
	for i, h := range heads {
		name := rawArgs[h[2]:h[3]]
		isStr := ""
		if h[4] >= 0 {
			isStr = rawArgs[h[4]:h[5]]
		}
		valueStart := h[1]

		// 值的终点：下一个参数头，或本方言的闭合标签。
		valueEnd := -1
		if i+1 < len(heads) {
			valueEnd = heads[i+1][0]
		} else if idx := strings.Index(rawArgs[valueStart:], d.paramClose); idx >= 0 {
			valueEnd = valueStart + idx
		} else {
			// 没有闭合标签：尾部未闭合，值吃到文本末尾（官方 partial 语义）。
			valueEnd = len(rawArgs)
		}
		value := ""
		if valueEnd > valueStart {
			value = rawArgs[valueStart:valueEnd]
		}
		// 下一个参数头之前可能还留着本参数的闭合标签，先切掉。
		if cut := strings.LastIndex(value, d.paramClose); cut >= 0 {
			value = value[:cut]
		}
		// 官方 _match_open_param：未闭合的下一个标签要从值里切掉并持有。
		if cut := strings.Index(value, d.paramStart); cut >= 0 {
			value = value[:cut]
		}
		out = append(out, dsmlParamValueEntry{name: name, isStr: isStr, value: value})
	}
	return out
}

// dsmlParamValue 按官方规则决定单个参数的值形态。
func dsmlParamValue(isStr, value string) any {
	if isStr == "true" {
		// 原样保留：禁止 strings.TrimSpace（见文件头注释第 1 条）。
		return value
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err == nil {
		return decoded
	}
	return value
}

// ContainsDSML 报告一段文本里是否出现 DSML 标记（用于快速跳过绝大多数帧）。
func ContainsDSML(text string) bool {
	return strings.Contains(text, dsmlMarker)
}

// ParseDSMLToolCalls 从一段文本里解析出全部 DSML 工具调用。
//
// 解析不出（无 invoke 标签 / 标签不闭合）时返回 nil —— 调用方按"看不懂"处理。
func ParseDSMLToolCalls(text string) []DSMLToolCall {
	if text == "" || !ContainsDSML(text) {
		return nil
	}
	for _, d := range dsmlDialects {
		if !dsmlHasToolStart(text, d) {
			continue
		}
		matches := d.invokeRe.FindAllStringSubmatch(text, -1)
		if len(matches) == 0 {
			continue
		}
		calls := make([]DSMLToolCall, 0, len(matches))
		for _, m := range matches {
			calls = append(calls, DSMLToolCall{
				Name:      m[1],
				Arguments: d.dsmlArgConverter(m[2], false),
			})
		}
		return calls
	}
	return nil
}

// dsmlHasToolStart 报告文本里是否含该方言的外层起始标签（含生产变体）。
func dsmlHasToolStart(text string, d dsmlDialect) bool {
	if strings.Contains(text, d.toolStart) {
		return true
	}
	for _, v := range dsmlToolStartVariants {
		if strings.Contains(text, v) {
			return true
		}
	}
	return false
}

// StripDSML 从文本里摘掉 DSML 块，保证协议标签不外泄给客户端。
//
// 先摘配对块，再清掉孤立残片（只有闭合标签、没有配对开标签）。
func StripDSML(text string) string {
	if text == "" || !ContainsDSML(text) {
		return text
	}
	out := text
	for _, d := range dsmlDialects {
		out = removePairedDSMLBlocks(out, d.toolStart, d.toolEnd)
	}
	for _, v := range dsmlToolStartVariants {
		out = removePairedDSMLBlocks(out, v, v)
	}
	// 残片：孤立的 DSML 标签（开标签或闭标签单独出现）。
	out = dsmlLooseTagRe.ReplaceAllString(out, "")
	return out
}

// removePairedDSMLBlocks 摘掉一对起止标签之间的全部内容（含标签本身）。
func removePairedDSMLBlocks(text, start, end string) string {
	if !strings.Contains(text, start) {
		return text
	}
	var b strings.Builder
	rest := text
	for {
		i := strings.Index(rest, start)
		if i < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		rest = rest[i+len(start):]
		j := strings.Index(rest, end)
		if j < 0 {
			// 没有配对闭标签：丢弃剩余（属于残片），不外泄。
			break
		}
		rest = rest[j+len(end):]
	}
	return b.String()
}

// dsmlLooseTagRe 匹配任意孤立的 DSML 标签（开标签或闭标签）。
var dsmlLooseTagRe = regexp.MustCompile(`</?` + regexp.QuoteMeta(dsmlMarker) + `[^>]*>`)

// DSMLStreamBuffer 跨帧缓冲 DSML 文本。
//
// 官方做法（vLLM）：缓冲到完整 invoke 块出现才一次性吐出结构化参数，不逐帧贪心解析；
// 未闭合的下一个标签要持有，避免标签被切进字符串值里。
//
// 语义：
//   - Feed 输入一个 delta，返回 (reasoning, text)：被识别为 DSML 的部分不会作为
//     reasoning/text 透传，而是被缓冲等待凑齐。
//   - Flush 在流结束时吐出剩余缓冲（此时若仍不完整则丢弃，不外泄残片）。
type DSMLStreamBuffer struct {
	buf      strings.Builder
	inDSML   bool
	pending  string
	flushed  bool
}

// Feed 输入一个 delta，返回 (透传的思考文本, 还原出的 DSML 工具调用)。
//
// 透传的思考文本是"确认不是 DSML"的部分；DSML 原文不会出现在返回值里。
// 还原出的调用在凑齐完整 invoke 块时一次性返回（官方 vLLM 做法）。
func (b *DSMLStreamBuffer) Feed(delta string) (string, []DSMLToolCall) {
	if delta == "" {
		return "", nil
	}
	// 上一帧扣留的标记前缀要先接上再判断。
	if b.pending != "" {
		delta = b.pending + delta
		b.pending = ""
	}
	if b.inDSML {
		b.buf.WriteString(delta)
		return "", b.Calls()
	}
	// 尚未进入 DSML：先看这一帧里是否出现起始标记。
	// DSML 标签形如 <｜DSML｜...>，所以定位要连前导的 '<' 一起算进 DSML 范围。
	idx := strings.Index(delta, dsmlMarker)
	if idx < 0 {
		// 本帧尾部可能是标记开头被切断 → 扣留到下一帧判断。
		if held := dsmlTrailingMarkerPrefix(delta); held > 0 {
			b.pending = delta[len(delta)-held:]
			return delta[:len(delta)-held], nil
		}
		return delta, nil
	}
	// 标记可能跨帧切开：本帧不足以判定 → 整段（含前导 '<'）扣留到下一帧。
	if len(delta)-idx < len(dsmlMarker) {
		start := idx
		if start > 0 && delta[start-1] == '<' {
			start--
		}
		b.pending = delta[start:]
		return delta[:start], nil
	}
	// 标记前导的 '<' 也属于 DSML 范围，不能透传给客户端。
	dsmlStart := idx
	if dsmlStart > 0 && delta[dsmlStart-1] == '<' {
		dsmlStart--
	}
	b.inDSML = true
	b.buf.Reset()
	b.buf.WriteString(delta[dsmlStart:])
	return delta[:dsmlStart], b.Calls()
}

// dsmlTrailingMarkerPrefix 报告一段文本尾部是否是 DSML 标记的"未完成前缀"
// （本帧末尾可能是标记开头，但还没写完）。
func dsmlTrailingMarkerPrefix(text string) int {
	max := len(dsmlMarker) - 1
	if max > len(text) {
		max = len(text)
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(dsmlMarker, text[len(text)-n:]) {
			return n
		}
	}
	return 0
}

// Flush 流结束时收口：吐出剩余缓冲并返回攒下的 DSML 调用。
func (b *DSMLStreamBuffer) Flush() []DSMLToolCall {
	if b.flushed {
		return nil
	}
	b.flushed = true
	if b.pending != "" {
		// 残片：直接丢弃，不外泄。
		b.pending = ""
	}
	if !b.inDSML {
		return nil
	}
	text := b.buf.String()
	b.buf.Reset()
	b.inDSML = false
	return ParseDSMLToolCalls(text)
}

// Calls 返回当前缓冲中已凑齐的 DSML 调用（若有）。
func (b *DSMLStreamBuffer) Calls() []DSMLToolCall {
	if !b.inDSML {
		return nil
	}
	text := b.buf.String()
	for _, d := range dsmlDialects {
		if !dsmlHasToolStart(text, d) {
			continue
		}
		if !strings.Contains(text, d.toolEnd) {
			return nil // 未收齐
		}
		calls := ParseDSMLToolCalls(text)
		b.buf.Reset()
		b.inDSML = false
		return calls
	}
	return nil
}

// DSMLToolCallToChatToolCall 把还原出来的 DSML 调用转成 CC 形状的 tool_calls。
func DSMLToolCallToChatToolCall(call DSMLToolCall, index int) ChatToolCall {
	idx := index
	return ChatToolCall{
		Index: &idx,
		Type:  "function",
		ID:    "call_dsml_" + itoa(index),
		Function: ChatFunctionCall{
			Name:      call.Name,
			Arguments: call.Arguments,
		},
	}
}

// itoa 是无依赖的小整数转字符串（避免为这一处引入 strconv 之外的依赖）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// 编译期断言：context 保持被引用（本文件后续扩展可能用到），避免 lint 报未使用。
var _ = context.Background
