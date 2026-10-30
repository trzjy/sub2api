package service

// 本文件实现 **Cursor 出站正文里「内联 tool call」的形态定义与解析器**
// （派发单 TC-01；TC-01R2 修正标记字节）。
//
// 背景：corealgos（sub2api）经 Cursor 协议出站时，上游偶尔把工具调用写成
// **正文文本**而不是结构化委托帧，客户端因此只拿到「一段话」，该轮工具不执行、
// 助手空转（已取证：正文里的内联 tool call id 共 12 个，12/12 从未出现在任何
// 执行记录）。
//
// 本文件只做 **形态识别 + 解析**，不做任何归一化改写：
//   - 响应侧归一（把内联调用还原成结构化 tool_calls）归 TC-02；
//   - 请求侧同源（出站时用同一套标记）归 TC-03。
//
// 两种实测形态（**标记是裸字节，不含任何零宽字符**——实测上游正文里就是普通
// 尖括号。早期版本误把 U+200B 写进常量，导致解析器对真实上游正文永远不命中，
// 已由 TC-01R2 修正）：
//   - **形态 A**（站点自身 blob 形态，JSON 合法）：
//     `<tool_call>\n{"id":...,"name":...,"arguments":{...}}\n</tool_call>`
//   - **形态 B**（上游私有标记形态，JSON **非法**）：开标记同上，内部以
//     `<]minimax[>` 分隔，字段以标签形式出现
//     （`<toolName>` / `<arguments>` / `<code>` / `<mcpDetails>` / ...）。
//
// **误判防护**（标记是裸字节，防误判只能靠**标记形状**，不能靠不可见字符）：
//   - 只有**开闭标记成对**、且**每段内容非空**，才认定为内联形态；
//   - 只出现开标记、没有配对闭标记 → **不认定为内联形态**（按「未识别」处理，
//     而不是「识别到但解析失败」）：宁可漏判，也不能把正常正文当工具调用吃掉；
//   - 正文里单独出现 `tool_call` 字样 → 不认定为内联形态。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// 内联 tool call 的标记常量（供 TC-03 出站侧复用同一套定义，避免「喂 A、学 A、
// 吐 A」）。
//
// **常量必须是裸字节**：实测上游正文里的标记就是普通尖括号，不含任何零宽字符。
// 把 U+200B 写进常量会让解析器对真实上游正文**永远不命中**（上游正文里没有
// 零宽字符），内联调用因此全部漏判。
const (
	// cursorInlineToolCallOpen 是内联 tool call 的开标记。
	//
	// 运行时字节：U+003C + "tool_call" + U+003E（无零宽字符）。
	cursorInlineToolCallOpen = "<tool_call>"
	// cursorInlineToolCallClose 是内联 tool call 的闭标记。
	//
	// 运行时字节：U+003C + U+002F + "tool_call" + U+003E（无零宽字符）。
	cursorInlineToolCallClose = "</tool_call>"
	// cursorInlineToolCallFieldSep 是形态 B 的字段分隔符。
	//
	// 运行时字节：U+003C + U+005D + "minimax" + U+005B + U+003E（无零宽字符）。
	cursorInlineToolCallFieldSep = "<]minimax[>"
	// cursorInlineToolCallIDPrefix 是形态 B 自动生成 id 的前缀。
	cursorInlineToolCallIDPrefix = "call_inline_"
	// cursorInlineToolCallIDHashLen 是自动生成 id 里内容哈希的十六进制长度。
	cursorInlineToolCallIDHashLen = 8
)

// cursorInlineFormBTags 是形态 B 里已知的字段标签。
//
// 只列已取证的标签；未知标签一律忽略（上游加新字段不应让解析失败）。
var cursorInlineFormBTags = []string{
	"arguments",
	"code",
	"mcpDetails",
	"description",
	"namespace",
	"toolName",
}

// cursorInlineToolCall 是一条从正文里解析出的内联工具调用。
type cursorInlineToolCall struct {
	ID        string
	Name      string
	Arguments string // 对象 JSON 字符串；无参数时为 "{}"
}

// cursorInlineSegment 是一段内联片段，以及它前面的正文。
type cursorInlineSegment struct {
	before string // 片段之前的正文（原样保留）
	body   string // 开闭标记之间的内容
}

// cursorParseInlineToolCalls 从 content 中解析内联工具调用。
//
// 返回三值：
//   - calls：解析出的调用，按正文出现顺序；
//   - rest：剔除内联片段后的正文（其余部分原样保留、前后顺序不变）；
//   - recognized：是否**构成内联形态**（判据见下）。
//
// **误判防护判据**（标记是裸字节，没有零宽字符可依赖，只能靠标记形状）：
//   - 只有**开闭标记成对**、且**每段内容非空**，才构成内联形态；
//   - 只出现开标记、没有配对闭标记 → **不构成**内联形态（按「未识别」处理，
//     而不是「识别到但解析失败」）：片段边界不可信，宁可漏判也不能误判；
//   - 正文里单独出现 `tool_call` 字样 → 不构成内联形态。
//
// 调用方必须遵守的语义约定：
//   - recognized=false：content 里没有内联形态，按原样处理。
//   - recognized=true 且 calls 为空：**识别到形态但解析不出参数** → 必须失败关闭，
//     不得把正文原样放行（那等于让上游的工具调用静默消失，助手继续空转）。
//   - recognized=true 且 calls 非空：rest 已剔除全部内联片段，可安全透出。
//
// 解析器不得 panic：任何畸形输入（超长、坏 UTF-8、标记乱序）只允许返回错误值。
func cursorParseInlineToolCalls(content string) (calls []cursorInlineToolCall, rest string, recognized bool) {
	// shapeOK 记录「形态是否成立」，供 panic 兜底路径回填 recognized。
	shapeOK := false

	// 兜底防线：正常路径不该 panic，一旦发生就按已认定的形态失败关闭，
	// 绝不把半成品当成功交给调用方。
	defer func() {
		if r := recover(); r != nil {
			calls = nil
			rest = content
			recognized = shapeOK
		}
	}()

	// 形态认定：标记不成对 / 段内容为空 / 标记不配平 → 未识别，正文原样返回，
	// 一个字节都不吞。
	segments, trailing, ok := cursorInlineScanSegments(content)
	if !ok {
		return nil, content, false
	}
	shapeOK = true

	var prose strings.Builder
	for i, seg := range segments {
		call, parsed := cursorParseInlineFragment(seg.body, content, i+1)
		if !parsed {
			// 全有或全无：任一片段解析失败就整条判失败，避免调用方拿到
			// 「一半已执行、一半被吞」的中间态。
			return nil, content, true
		}
		calls = append(calls, call)
		prose.WriteString(seg.before)
	}
	prose.WriteString(trailing)
	return calls, prose.String(), true
}

// cursorInlineScanSegments 扫描正文里的内联片段，并做**形态认定**。
//
// 形态认定判据（与 cursorParseInlineToolCalls 的文档一致）：
//  1. 开标记与闭标记必须**成对**出现，且开标记后面跟得上闭标记；
//  2. 每一对开闭标记之间的**内容必须非空**；
//  3. 全文的开/闭标记数必须与解析出的片段数一致（标记两两配平，不允许落单
//     的开标记或闭标记）。
//
// 任一条不满足 → ok=false（**未认定形态**），调用方按「未识别」原样返回正文。
//
// 严格判据的取舍：落单标记说明片段边界不可信（也可能只是正文里提到了标记
// 字样），此时宁可**漏判**（这一轮工具调用没被执行）也不**误判**（把正常正文
// 当工具调用吃掉）；正文一个字节都不吞。
func cursorInlineScanSegments(content string) (segments []cursorInlineSegment, trailing string, ok bool) {
	remain := content
	for {
		open := strings.Index(remain, cursorInlineToolCallOpen)
		if open < 0 {
			break
		}
		bodyStart := open + len(cursorInlineToolCallOpen)
		closeIdx := strings.Index(remain[bodyStart:], cursorInlineToolCallClose)
		if closeIdx < 0 {
			// 落单的开标记：后面没有配对的闭标记 → 形态不成立。
			return nil, "", false
		}
		body := remain[bodyStart : bodyStart+closeIdx]
		if strings.TrimSpace(body) == "" {
			// 开闭成对但中间内容为空 → 不是内联形态。
			return nil, "", false
		}
		segments = append(segments, cursorInlineSegment{before: remain[:open], body: body})
		remain = remain[bodyStart+closeIdx+len(cursorInlineToolCallClose):]
	}
	if len(segments) == 0 {
		// 正文里没有成对标记 → 形态不成立。
		return nil, "", false
	}
	// 标记必须两两配平：正文里出现过的开/闭标记数必须都等于片段数，否则存在
	// 落单标记（形如「正文里恰好提到了 tool_call 标记」）→ 不认定形态。
	if strings.Count(content, cursorInlineToolCallOpen) != len(segments) ||
		strings.Count(content, cursorInlineToolCallClose) != len(segments) {
		return nil, "", false
	}
	return segments, remain, true
}

// cursorParseInlineFragment 解析一段内联片段正文。
//
// body 是片段正文，content 是整条正文（用于生成跨轮次不撞车的 id），
// ordinal 是该片段在本次 content 中的出现序号（1 起）。
func cursorParseInlineFragment(body, content string, ordinal int) (cursorInlineToolCall, bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return cursorInlineToolCall{}, false
	}
	// 形态 A 优先：JSON 完整可解且带工具名就走 A。
	if call, ok := cursorParseInlineFormA(trimmed, content, ordinal); ok {
		return call, true
	}
	// A 解不出（JSON 坏）再尝试形态 B。
	return cursorParseInlineFormB(trimmed, content, ordinal)
}

// cursorParseInlineFormA 解析形态 A（站点自身 blob 形态，JSON 合法）。
//
// 形态 A 是站点自己写出的格式，因此可以直接 Unmarshal；但必须解出工具名，
// 否则不算形态 A（交给形态 B 兜底，都不成则判失败）。
func cursorParseInlineFormA(body, content string, ordinal int) (cursorInlineToolCall, bool) {
	var blob struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(body), &blob); err != nil {
		return cursorInlineToolCall{}, false
	}
	name := strings.TrimSpace(blob.Name)
	if name == "" {
		return cursorInlineToolCall{}, false
	}
	args, ok := cursorNormalizeInlineArguments(blob.Arguments)
	if !ok {
		return cursorInlineToolCall{}, false
	}
	id := strings.TrimSpace(blob.ID)
	if id == "" {
		id = cursorInlineGeneratedID(content, ordinal)
	}
	return cursorInlineToolCall{ID: id, Name: name, Arguments: args}, true
}

// cursorNormalizeInlineArguments 把形态 A 的 arguments 归一成「对象 JSON 字符串」。
//
// 规则：
//   - 缺失 / 空 / null → "{}"（无参数）。
//   - JSON 对象 → **压紧后原样保留对象文本**。这里刻意不做 map 往返再序列化：
//     map 往返会丢键序、并把大整数降精度成 float64，工具入参里的大整数 ID 会被
//     悄悄改坏；json.Compact 只去空白、不动键序与数字字面量，产出的仍是同一个
//     对象的 JSON 字符串。
//   - JSON 字符串 → 原样使用该字符串（形态 A 的 arguments 也可能是「参数 JSON 的
//     字符串形态」）；解出后为空串按无参数处理。
//   - 其它 JSON 标量 / 数组 → 不接受：无法安全表达成工具入参，判为解析失败。
func cursorNormalizeInlineArguments(raw json.RawMessage) (string, bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return "{}", true
	}
	switch text[0] {
	case '{':
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return "", false
		}
		return buf.String(), true
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", false
		}
		if strings.TrimSpace(s) == "" {
			return "{}", true
		}
		return s, true
	default:
		return "", false
	}
}

// cursorParseInlineFormB 解析形态 B（上游私有标记形态，JSON **非法**）。
//
// 形态 B 的片段正文是错位的信封（`{"id":"CallDynamicTool">]` 这种引号/括号
// 错位形态），因此**绝不**对整段做 json.Unmarshal——只按标签取值。
func cursorParseInlineFormB(body, content string, ordinal int) (cursorInlineToolCall, bool) {
	fields := cursorInlineFormBFields(body)
	name := strings.TrimSpace(fields["toolName"])
	if name == "" {
		// 工具名取不到 → 这条调用无法路由，判失败（调用方失败关闭）。
		return cursorInlineToolCall{}, false
	}
	// 参数：优先 <arguments>，为空则退回 <code>——上游两种写法等价承载同一份入参
	// （实测样本里 <arguments> 为空、真正内容在 <code>）。
	argText := strings.TrimSpace(fields["arguments"])
	if argText == "" {
		argText = strings.TrimSpace(fields["code"])
	}
	args, ok := cursorInlineShapeFormBArguments(argText)
	if !ok {
		return cursorInlineToolCall{}, false
	}
	return cursorInlineToolCall{
		ID:        cursorInlineGeneratedID(content, ordinal),
		Name:      name,
		Arguments: args,
	}, true
}

// cursorInlineShapeFormBArguments 把形态 B 的参数文本归一成对象 JSON 字符串。
//
// 两种实现选择及理由：
//   - 参数文本本身是**合法 JSON 对象** → 原样用作 Arguments。上游若已经给出
//     结构化参数，就不该再套一层壳改变形状。
//   - 否则 → 包成 `{"code": <原文>}`。理由有三：
//     ① 形态 A 的等价内容就是 `{"code":…}`（站点 blob 形态里 arguments 就是
//     `{"code":"const out = {};"}`），包成同一形状后两种形态在下游是**同一种**
//     工具入参，TC-02 不必为形态 B 单独写一套取值逻辑；
//     ② 参数文本常常是 JS 源码（`<code>` 段），不是合法 JSON，直接当 Arguments
//     会让下游 json.Unmarshal 失败；
//     ③ 保证 Arguments 恒为合法 JSON 对象字符串（契约要求）。
//
// 代价（已登记为存疑）：若上游给出的是**非对象**的 `<arguments>`，按 code 承载
// 是保守近似——证据里只出现过 REPL 这一个工具、其入参名就是 code，除此之外的
// 键名无从推断，故不猜。
func cursorInlineShapeFormBArguments(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "{}", true
	}
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return trimmed, true
	}
	encoded, err := json.Marshal(map[string]string{"code": trimmed})
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// cursorInlineFormBFields 按标签切出形态 B 的字段值。
//
// 每个字段两种写法都支持：
//   - 闭形式 `<tag>value</tag>`；
//   - 开形式 `<tag>value` —— value 取到下一个分隔符 / 下一个已知标签 / 段尾为止
//     （上游的开形式值里没有闭标签，只能靠分隔符兜边界）。
//
// 上游在标签外还包一层方括号（形如 `[<tag>value]`），那层 `]` 会粘在值尾部；
// 由 cursorInlineTrimEnvelopeNoise 统一按括号收支剥掉（收支平衡的值不动，
// 避免误伤 `const a = [1,2]` 这种本身以 `]` 结尾的正常代码）。
func cursorInlineFormBFields(body string) map[string]string {
	out := make(map[string]string, len(cursorInlineFormBTags))
	for _, tag := range cursorInlineFormBTags {
		openTag := "<" + tag + ">"
		idx := strings.Index(body, openTag)
		if idx < 0 {
			continue
		}
		rest := body[idx+len(openTag):]
		var value string
		if closeIdx := strings.Index(rest, "</"+tag+">"); closeIdx >= 0 {
			value = rest[:closeIdx]
		} else {
			cut := len(rest)
			if j := strings.Index(rest, cursorInlineToolCallFieldSep); j >= 0 && j < cut {
				cut = j
			}
			for _, other := range cursorInlineFormBTags {
				if other == tag {
					continue
				}
				if j := strings.Index(rest, "<"+other+">"); j >= 0 && j < cut {
					cut = j
				}
			}
			value = rest[:cut]
		}
		out[tag] = cursorInlineTrimEnvelopeNoise(strings.TrimSpace(value))
	}
	return out
}

// cursorInlineTrimEnvelopeNoise 剥掉形态 B 片段尾部因上游信封未闭合而多出的
// 闭合符（上游那层 `[<tag>value]` 的 `]`、以及 JSON 信封残留的 `}`）。
//
// 判据：只有当「闭合符多于开启符」时才剥——收支本来就平衡的文本（正常 JS/JSON
// 代码，末尾常带 `}` 或 `]`）原样保留，避免误伤代码。
//
// 已知局限：收支统计不区分字符串字面量里的括号（如 `const s = "{";`），
// 属启发式；只在尾部确实是闭合符时才生效，不会改动其它字符。
func cursorInlineTrimEnvelopeNoise(text string) string {
	for len(text) > 0 {
		last := text[len(text)-1]
		if last != '}' && last != ']' {
			break
		}
		if cursorInlineBracketBalance(text) >= 0 {
			break
		}
		text = text[:len(text)-1]
	}
	return text
}

// cursorInlineBracketBalance 统计花括号与方括号的收支差（负数=闭合符多出）。
func cursorInlineBracketBalance(text string) int {
	balance := 0
	for _, r := range text {
		switch r {
		case '{', '[':
			balance++
		case '}', ']':
			balance--
		}
	}
	return balance
}

// cursorInlineGeneratedID 生成形态 B（无标准 id）的调用 id。
//
// 规则：`call_inline_<序号>_<内容哈希前 8 位十六进制>`，序号是该内联片段在本次
// 正文中的出现序号（1 起），哈希取**整条正文**的 SHA-256 前 8 位十六进制。
//   - **稳定**：不含随机数/时间戳，同一份正文重复解析恒得到同一 id；TC-02（响应侧）
//     与 TC-03（请求侧）各自从正文重算即可对上。
//   - **唯一**：序号保证同一份正文内不重复；正文哈希保证跨轮次、不同正文之间
//     也不会撞 id（纯序号在「每轮都恰好有一段内联调用」时会退化成同一个值，
//     那样客户端侧的 tool_call_id 匹配就会串轮）。
//
// 刻意**不用**形态 B 信封里的 `"CallDynamicTool"`：那个值对每次调用都一样，
// 直接用会让所有调用撞成同一个 id。
func cursorInlineGeneratedID(content string, ordinal int) string {
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	if len(hash) > cursorInlineToolCallIDHashLen {
		hash = hash[:cursorInlineToolCallIDHashLen]
	}
	return cursorInlineToolCallIDPrefix + strconv.Itoa(ordinal) + "_" + hash
}
