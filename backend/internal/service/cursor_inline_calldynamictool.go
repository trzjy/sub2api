package service

// 本文件只在 Cursor AgentRun 的**文本帧边界**识别一种窄范围私有封装：
// 上游偶尔把工具调用写成正文而不是原生 mcp_args 委托帧，外形为
// `<invoke name="CallDynamicTool">` + `<]minimax[>` 分隔的字段段。
//
// 范围严格锁定在事故证据（sub2api-inline-toolcall-rootcause-20261127）里
// 出现的结构；它不是通用 DSML 解析器，不执行任何工具，不猜测原生 ExecID，
// 不写 cursorToolDelegations。只把内层 toolName 与本次 HTTP 声明的工具精确
// 匹配后，归一成普通结构化 Chat tool_calls，交给现有 writer 映射 tool_use。
//
// 真实字节（逐字节核对，无 U+200B）：
//   - 字段段分隔符：]<]minimax[>[
//   - 外层开标记：<invoke name="CallDynamicTool">
//   - 外层闭标记：</invoke>
// 字段段形如 `<tag>value` 或 `</tag>`，末尾的 `]` 是包装括号。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

var errCursorInlineCDTUnsupported = errors.New("cursor: malformed CallDynamicTool inline tool call")

const (
	cursorCDTFieldSep = "]<]minimax[>["
	cursorCDTOpen     = "<invoke name=\"CallDynamicTool\">"
	cursorCDTClose    = "</invoke>"
	// 完整块起止标记：开标记带前导分隔符，闭标记带前导分隔符。
	cursorCDTBlockOpen  = cursorCDTFieldSep + cursorCDTOpen
	cursorCDTBlockClose = "<]minimax[>[" + cursorCDTClose
)

// cursorInlineCDTCall 是一条归一后的结构化调用。
type cursorInlineCDTCall struct {
	ID       string
	Name     string
	ArgsJSON string
}

// cursorCDTNormalizer 跨 TextDelta 帧有状态地归一 CallDynamicTool 私有块。
//
// pending 只保留两类内容：① 可能是开标记前缀的最短后缀；② 已识别开标记、
// 尚未等到闭标记的整块。任何已判定为普通正文的字符都立即交付，不撤回。
type cursorCDTNormalizer struct {
	declared map[string]json.RawMessage // 本次请求声明的工具名 → 参数 schema
	pending  string
	inBlock  bool
	calls    []cursorInlineCDTCall
}

func newCursorCDTNormalizer(tools []apicompat.ChatTool) *cursorCDTNormalizer {
	n := &cursorCDTNormalizer{declared: make(map[string]json.RawMessage, len(tools))}
	for _, tool := range tools {
		if tool.Function == nil {
			continue
		}
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			continue
		}
		n.declared[name] = tool.Function.Parameters
	}
	return n
}

// consume 吃进一段可见正文增量，返回可安全交付的正文与本帧完整解析出的调用。
//
// 识别到完整块但格式损坏 / 目标未声明 / 参数不合 schema 时返回错误：调用方
// 必须失败关闭，不得把控制块退回正文，也不得正常 end_turn。
func (n *cursorCDTNormalizer) consume(delta string) (string, []cursorInlineCDTCall, error) {
	if n == nil {
		return delta, nil, nil
	}
	n.pending += delta
	var safe strings.Builder
	var calls []cursorInlineCDTCall
	for {
		if n.inBlock {
			end := strings.Index(n.pending, cursorCDTBlockClose)
			if end < 0 {
				// 开标记已出现、闭标记未到：整段继续扣留，等待后续帧。
				return safe.String(), calls, nil
			}
			block := n.pending[:end+len(cursorCDTBlockClose)]
			n.pending = n.pending[end+len(cursorCDTBlockClose):]
			call, err := n.parseBlock(block)
			if err != nil {
				return "", nil, err
			}
			calls = append(calls, call)
			n.inBlock = false
			continue
		}
		open := strings.Index(n.pending, cursorCDTBlockOpen)
		if open < 0 {
			// 只扣留可能构成开标记前缀的最短后缀；其余按原序原样交付。
			keep := cursorCDTPrefixSuffixLen(n.pending, cursorCDTBlockOpen)
			emit := len(n.pending) - keep
			safe.WriteString(n.pending[:emit])
			n.pending = n.pending[emit:]
			return safe.String(), calls, nil
		}
		safe.WriteString(n.pending[:open])
		n.pending = n.pending[open:]
		n.inBlock = true
	}
}

// finish 在流收尾时交付剩余正文。已识别开标记但始终没有闭标记 → 失败关闭。
func (n *cursorCDTNormalizer) finish() (string, error) {
	if n == nil {
		return "", nil
	}
	if n.inBlock {
		return "", fmt.Errorf("%w: incomplete CallDynamicTool block", errCursorInlineCDTUnsupported)
	}
	tail := n.pending
	n.pending = ""
	return tail, nil
}

// cursorCDTPrefixSuffixLen 返回 s 的最长后缀长度，该后缀同时是 marker 的前缀。
func cursorCDTPrefixSuffixLen(s, marker string) int {
	max := len(s)
	if max > len(marker)-1 {
		max = len(marker) - 1
	}
	for k := max; k > 0; k-- {
		if strings.HasSuffix(s, marker[:k]) {
			return k
		}
	}
	return 0
}

// parseBlock 解析一个完整的私有块。只接受证据支持的成对结构。
func (n *cursorCDTNormalizer) parseBlock(block string) (cursorInlineCDTCall, error) {
	if !strings.HasPrefix(block, cursorCDTBlockOpen) || !strings.HasSuffix(block, cursorCDTBlockClose) {
		return cursorInlineCDTCall{}, fmt.Errorf("%w: block boundaries", errCursorInlineCDTUnsupported)
	}
	body := block[len(cursorCDTBlockOpen) : len(block)-len(cursorCDTBlockClose)]
	parts := strings.Split(body, cursorCDTFieldSep)
	fields := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSuffix(part, "]") // 每段末尾的包装括号
		if part != "" {
			fields = append(fields, part)
		}
	}

	toolName := cursorCDTInnerToolName(fields)
	if toolName == "" {
		return cursorInlineCDTCall{}, fmt.Errorf("%w: missing toolName", errCursorInlineCDTUnsupported)
	}
	schema, declared := n.declared[toolName]
	if !declared {
		return cursorInlineCDTCall{}, fmt.Errorf("%w: tool %q not declared in this request", errCursorInlineCDTUnsupported, toolName)
	}
	args, err := cursorCDTInnerArguments(fields, schema)
	if err != nil {
		return cursorInlineCDTCall{}, err
	}
	return cursorInlineCDTCall{
		ID:       cursorNewUUID(),
		Name:     toolName,
		ArgsJSON: args,
	}, nil
}

// cursorCDTInnerToolName 取最后一个非空的内层 `<toolName>` 值。
// 事故原文里前一个 `<toolName>Bash` 的闭合标签被上游写成 `</toolTime>`，
// 后一个是完整 `<toolName>Bash</toolName>`；两者取值一致。
func cursorCDTInnerToolName(fields []string) string {
	name := ""
	for _, f := range fields {
		if !strings.HasPrefix(f, "<toolName>") {
			continue
		}
		if v := strings.TrimPrefix(f, "<toolName>"); strings.TrimSpace(v) != "" {
			name = strings.TrimSpace(v)
		}
	}
	return name
}

// cursorCDTInnerArguments 只解释第一个 `<arguments>…</arguments>` 段内的字段。
func cursorCDTInnerArguments(fields []string, schema json.RawMessage) (string, error) {
	start, end := -1, -1
	for i, f := range fields {
		if start < 0 && f == "<arguments>" {
			start = i
			continue
		}
		if start >= 0 && f == "</arguments>" {
			end = i
			break
		}
	}
	if start < 0 || end < 0 || end <= start {
		return "", fmt.Errorf("%w: arguments block", errCursorInlineCDTUnsupported)
	}
	// 空的 <arguments></arguments> 也走下面的 required 校验：工具声明了必填项
	// 时绝不能因为参数段为空就放行空对象。
	//
	// 校验范围只覆盖顶层字段的「是否在 properties 内 + 基本类型 + required」，
	// 不是完整 JSON Schema 验证（嵌套 schema、枚举、范围、pattern 等未做）；
	// 超出范围但已能解析的约束保持原值透传，由声明方与上游负责。

	var parsed struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if len(schema) > 0 {
		if err := json.Unmarshal(schema, &parsed); err != nil {
			return "", fmt.Errorf("%w: invalid declared schema: %v", errCursorInlineCDTUnsupported, err)
		}
	}

	values := make(map[string]any)
	for _, f := range fields[start+1 : end] {
		if len(f) < 3 || f[0] != '<' || f[1] == '/' {
			continue // 闭合段或非字段段
		}
		gt := strings.IndexByte(f, '>')
		if gt <= 1 {
			return "", fmt.Errorf("%w: malformed argument field", errCursorInlineCDTUnsupported)
		}
		key := f[1:gt]
		raw := f[gt+1:]
		prop, ok := parsed.Properties[key]
		if !ok {
			return "", fmt.Errorf("%w: argument %q not in declared schema", errCursorInlineCDTUnsupported, key)
		}
		val, err := cursorCDTCoerceArgument(raw, prop.Type)
		if err != nil {
			return "", fmt.Errorf("%w: argument %q: %v", errCursorInlineCDTUnsupported, key, err)
		}
		values[key] = val
	}
	for _, req := range parsed.Required {
		if _, ok := values[req]; !ok {
			return "", fmt.Errorf("%w: missing required argument %q", errCursorInlineCDTUnsupported, req)
		}
	}
	args, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("%w: marshal arguments: %v", errCursorInlineCDTUnsupported, err)
	}
	return string(args), nil
}

// cursorCDTCoerceArgument 按声明 schema 的 type 解释字段值，原样保真字符串。
func cursorCDTCoerceArgument(raw, typ string) (any, error) {
	switch typ {
	case "", "string":
		return raw, nil
	case "number":
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, err
		}
		return v, nil
	case "integer":
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, err
		}
		return v, nil
	case "boolean":
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, err
		}
		return v, nil
	case "object", "array":
		// json.Unmarshal 到 any 会把数字变成 float64：大于 2^53 的整数会丢精度。
		// 用 UseNumber 原样保留数字字面量，Marshal 时按原值写回。
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	default:
		return nil, fmt.Errorf("unsupported schema type %q", typ)
	}
}
