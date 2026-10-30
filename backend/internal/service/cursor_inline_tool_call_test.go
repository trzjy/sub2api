//go:build unit

package service

// 派发单 TC-01R2「修正 TC-01：内联 tool call 标记常量」的验收测试。
//
// 本卡的唯一目的：TC-01 把三个标记常量写成了**含零宽字符 U+200B** 的形式，
// 上游正文里并不存在该字符 → 解析器对真实上游正文**永远不命中**。
// 本卡把常量改成**裸字节**，并把误判防护从「零宽字符」改到「标记形状」。
//
// 覆盖矩阵：
//   - 形态 A（站点 blob 形态，JSON 合法）：id / name / arguments 解析，rest 剔除该段。
//   - 形态 A 且 arguments 为字符串：Arguments 等于该字符串（原样）。
//   - 形态 A 且 arguments 缺失 / null / 空串：Arguments 归一为 "{}"。
//   - 形态 B（上游私有标记形态，JSON 非法）：仍能取出 toolName 与 code / arguments。
//   - 形态 B 的 <arguments> 段是合法 JSON 对象：原样用作 Arguments（不套壳）。
//   - 形态 B 的闭标签写法（<toolName>REPL</toolName>）：同样能取值。
//   - 混合正文：前言 + 内联片段 → rest 只留前言。
//   - 多个内联片段：全部解析出，rest 剔除全部。
//   - **字节护栏**：常量逐字节等于生产实测码点表；按码点表重建的生产实测原文
//     （形态 A / 形态 B）能解析出 id / name / arguments。
//   - 误判防护：仅出现开标记、没有闭标记 → **不判为内联形态**（recognized=false）。
//   - 误判防护：正文只含 `tool_call` 字样 → recognized=false。
//   - 空串 / 只有空白 → recognized=false、calls 空、rest 原样。
//   - 畸形输入（开闭成对但内容解不出参数 / 空内容 / 坏 UTF-8）→ 不 panic，
//     按定义语义返回。
//   - id 稳定性 / 跨轮次唯一性。
//
// 断言只依赖本卡的新增文件，不触碰 TC-02 / TC-03 的禁区文件。

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// tc01ProdOpen / tc01ProdClose / tc01ProdSep 是按**生产实测码点表**重建的标记字节：
//
//	开标记   = U+003C + "tool_call" + U+003E
//	闭标记   = U+003C + U+002F + "tool_call" + U+003E
//	私有分隔 = U+003C + U+005D + "minimax" + U+005B + U+003E
//
// 刻意用码点转义拼出来而不是写字面量：源码里**没有位置**可以藏进零宽字符，
// 字节一旦写错就必然对不上（这正是 TC-01 的原始缺陷）。
const (
	tc01ProdOpen  = "\x3ctool_call\x3e"
	tc01ProdClose = "\x3c/tool_call\x3e"
	tc01ProdSep   = "\x3c]minimax[\x3e"
)

// tc01Open / tc01Close / tc01FieldSep 是测试用的内联标记（与生产常量同形）。
const (
	tc01Open  = tc01ProdOpen
	tc01Close = tc01ProdClose
	// tc01FieldSep 是形态 B 的字段分隔符。
	tc01FieldSep = tc01ProdSep
)

// tc01FormA 是形态 A 的完整片段（站点自身 blob 形态，JSON 合法）。
const tc01FormA = tc01Open + "\n" +
	`{"id":"call_c3a4d6c4a7f54a5e9f5c2b8d","name":"REPL","arguments":{"code":"const out = {};"}}` +
	"\n" + tc01Close

// tc01FormBBody 是形态 B 的片段正文（上游私有标记形态）。
//
// 首段 `{"id":"CallDynamicTool">]` 的引号/括号是错位的（JSON 非法，不能
// Unmarshal）；字段以标签形式出现，参数真正落在 `<code>` 段里。
func tc01FormBBody() string {
	return `{"id":"CallDynamicTool">]` + tc01FieldSep +
		`<arguments>]` + tc01FieldSep +
		`<code>const out = {};` + "\n" + `const KEY='sk-xxx';` + "\n" +
		`return out;}]` + tc01FieldSep +
		`<mcpDetails>]` + tc01FieldSep +
		`<description>只声明 Glob / 只声明 LS，看上游是否走原生 read/ls 通道被拒]` + tc01FieldSep +
		`<namespace>sub2api]` + tc01FieldSep +
		`<toolName>REPL`
}

// TestCursorParseInlineToolCall_ProductionBytes 字节护栏（锁死「字节写对」）：
//
//  1. 三个常量必须逐字节等于生产实测码点表（不含任何零宽字符）；
//  2. 按码点表重建的生产实测原文（形态 A / 形态 B）必须能解析出
//     id / name / arguments，且 rest 不再残留标记。
func TestCursorParseInlineToolCall_ProductionBytes(t *testing.T) {
	require.Equal(t, tc01ProdOpen, cursorInlineToolCallOpen,
		"开标记必须逐字节等于 U+003C + tool_call + U+003E")
	require.Equal(t, tc01ProdClose, cursorInlineToolCallClose,
		"闭标记必须逐字节等于 U+003C + U+002F + tool_call + U+003E")
	require.Equal(t, tc01ProdSep, cursorInlineToolCallFieldSep,
		"私有分隔必须逐字节等于 U+003C + U+005D + minimax + U+005B + U+003E")

	// 形态 A 生产实测原文。
	prodFormA := tc01ProdOpen + "\n" +
		`{"id":"call_c3a4d6c4a7f54a5e9f5c2b8d","name":"REPL","arguments":{"code":"const out = {};"}}` +
		"\n" + tc01ProdClose
	callsA, restA, recognizedA := cursorParseInlineToolCalls("前言。" + prodFormA + "后语。")
	require.True(t, recognizedA, "生产实测形态 A 必须命中内联形态")
	require.Len(t, callsA, 1)
	require.Equal(t, "call_c3a4d6c4a7f54a5e9f5c2b8d", callsA[0].ID)
	require.Equal(t, "REPL", callsA[0].Name)
	require.JSONEq(t, `{"code":"const out = {};"}`, callsA[0].Arguments)
	require.NotContains(t, restA, "tool_call", "rest 必须剔除内联片段")
	require.Contains(t, restA, "前言。")
	require.Contains(t, restA, "后语。")

	// 形态 B 生产实测原文。
	prodFormB := tc01ProdOpen + `{"id":"CallDynamicTool">]` + tc01ProdSep +
		`<arguments>]` + tc01ProdSep +
		`<code>const out = {};` + "\n" + `const KEY='sk-xxx';` + "\n" +
		`return out;}]` + tc01ProdSep +
		`<mcpDetails>]` + tc01ProdSep +
		`<namespace>sub2api]` + tc01ProdSep +
		`<toolName>REPL` + tc01ProdClose
	callsB, restB, recognizedB := cursorParseInlineToolCalls("前言。" + prodFormB + "后语。")
	require.True(t, recognizedB, "生产实测形态 B 必须命中内联形态")
	require.Len(t, callsB, 1)
	require.NotEmpty(t, callsB[0].ID, "形态 B 也必须产出可用 id")
	require.Equal(t, "REPL", callsB[0].Name)
	require.Contains(t, callsB[0].Arguments, "const out = {};",
		"参数必须来自 <code> 段")
	require.NotContains(t, restB, "tool_call", "rest 必须剔除内联片段")
	require.NotContains(t, restB, "minimax", "rest 必须剔除形态 B 字段分隔符")
}

// TestCursorParseInlineToolCall_FormA 形态 A：解析出 id / name / arguments，
// 且 rest 剔除整段内联片段。
func TestCursorParseInlineToolCall_FormA(t *testing.T) {
	content := "我先看一下目录。\n" + tc01FormA + "\n稍等。"

	calls, rest, recognized := cursorParseInlineToolCalls(content)

	require.True(t, recognized, "命中内联开标记必须 recognized=true")
	require.Len(t, calls, 1)
	require.Equal(t, "call_c3a4d6c4a7f54a5e9f5c2b8d", calls[0].ID)
	require.Equal(t, "REPL", calls[0].Name)
	require.JSONEq(t, `{"code":"const out = {};"}`, calls[0].Arguments)

	// rest 必须剔除整段（含标记），其余正文原样保留、前后顺序不变。
	require.NotContains(t, rest, tc01Open)
	require.NotContains(t, rest, tc01Close)
	require.NotContains(t, rest, "call_c3a4d6c4a7f54a5e9f5c2b8d")
	require.Contains(t, rest, "我先看一下目录。")
	require.Contains(t, rest, "稍等。")
	require.Less(t, strings.Index(rest, "我先看一下目录。"), strings.Index(rest, "稍等。"))
}

// TestCursorParseInlineToolCall_FormAArgumentsAsString 形态 A 且 arguments 是
// 字符串：Arguments 原样等于该字符串。
func TestCursorParseInlineToolCall_FormAArgumentsAsString(t *testing.T) {
	content := tc01Open + "\n" +
		`{"id":"call_str_args","name":"REPL","arguments":"{\"code\":\"print(1)\"}"}` +
		"\n" + tc01Close

	calls, _, recognized := cursorParseInlineToolCalls(content)

	require.True(t, recognized)
	require.Len(t, calls, 1)
	require.Equal(t, "call_str_args", calls[0].ID)
	require.Equal(t, "REPL", calls[0].Name)
	require.Equal(t, `{"code":"print(1)"}`, calls[0].Arguments)
}

// TestCursorParseInlineToolCall_FormAArgumentsMissing 形态 A 且 arguments
// 缺失 / 为空：Arguments 归一成 "{}"。
func TestCursorParseInlineToolCall_FormAArgumentsMissing(t *testing.T) {
	for _, argsField := range []string{"", `,"arguments":null`, `,"arguments":""`} {
		content := tc01Open + "\n" +
			`{"id":"call_no_args","name":"REPL"` + argsField + `}` +
			"\n" + tc01Close

		calls, _, recognized := cursorParseInlineToolCalls(content)

		require.True(t, recognized)
		require.Len(t, calls, 1)
		require.Equal(t, "{}", calls[0].Arguments)
	}
}

// TestCursorParseInlineToolCall_FormB 形态 B（JSON 非法）：
// 仍能取出 toolName 与 code，且不 panic。
func TestCursorParseInlineToolCall_FormB(t *testing.T) {
	content := "先跑一段代码。\n" + tc01Open + tc01FormBBody() + tc01Close + "\n结束。"

	calls, rest, recognized := cursorParseInlineToolCalls(content)

	require.True(t, recognized, "命中内联开标记必须 recognized=true")
	require.Len(t, calls, 1)
	require.Equal(t, "REPL", calls[0].Name, "工具名必须来自 <toolName> 段")
	require.Contains(t, calls[0].Arguments, "const out = {};", "参数必须来自 <code> 段")
	require.Contains(t, calls[0].Arguments, "const KEY='sk-xxx';")
	require.True(t, strings.HasPrefix(calls[0].ID, "call_inline_"),
		"形态 B 无标准 id，必须生成 call_inline_ 前缀的 id，实际=%q", calls[0].ID)

	// rest 剔除整段形态 B 正文，其余原样保留。
	require.NotContains(t, rest, tc01Open)
	require.NotContains(t, rest, "CallDynamicTool")
	require.NotContains(t, rest, "minimax")
	require.Contains(t, rest, "先跑一段代码。")
	require.Contains(t, rest, "结束。")
}

// TestCursorParseInlineToolCall_FormBArgumentsJSONObject 形态 B 的
// `<arguments>` 段本身是合法 JSON 对象时：原样用作 Arguments，不额外套壳。
func TestCursorParseInlineToolCall_FormBArgumentsJSONObject(t *testing.T) {
	body := `{"id":"CallDynamicTool">]` + tc01FieldSep +
		`<arguments>{"code":"print(2)"}]` + tc01FieldSep +
		`<toolName>REPL`
	content := tc01Open + body + tc01Close

	calls, _, recognized := cursorParseInlineToolCalls(content)

	require.True(t, recognized)
	require.Len(t, calls, 1)
	require.JSONEq(t, `{"code":"print(2)"}`, calls[0].Arguments)
}

// TestCursorParseInlineToolCall_FormBClosedTag 形态 B 的闭标签写法
// （`<toolName>REPL</toolName>`）同样要能取值。
func TestCursorParseInlineToolCall_FormBClosedTag(t *testing.T) {
	body := `{"id":"CallDynamicTool">]` + tc01FieldSep +
		`<toolName>REPL</toolName>` + tc01FieldSep +
		`<arguments>{"code":"print(3)"}</arguments>`
	content := tc01Open + body + tc01Close

	calls, _, recognized := cursorParseInlineToolCalls(content)

	require.True(t, recognized)
	require.Len(t, calls, 1)
	require.Equal(t, "REPL", calls[0].Name)
	require.JSONEq(t, `{"code":"print(3)"}`, calls[0].Arguments)
}

// TestCursorParseInlineToolCall_MixedProse 混合正文：前言 + 内联片段 →
// rest 只留前言。
func TestCursorParseInlineToolCall_MixedProse(t *testing.T) {
	content := "前言正文。" + tc01FormA

	calls, rest, recognized := cursorParseInlineToolCalls(content)

	require.True(t, recognized)
	require.Len(t, calls, 1)
	require.Equal(t, "前言正文。", rest)
}

// TestCursorParseInlineToolCall_Multiple 多个内联片段：全部解析出，rest 剔除全部。
func TestCursorParseInlineToolCall_Multiple(t *testing.T) {
	content := "一。" + tc01FormA + "二。" +
		tc01Open + tc01FormBBody() + tc01Close + "三。"

	calls, rest, recognized := cursorParseInlineToolCalls(content)

	require.True(t, recognized)
	require.Len(t, calls, 2)
	require.Equal(t, "call_c3a4d6c4a7f54a5e9f5c2b8d", calls[0].ID)
	require.Equal(t, "REPL", calls[0].Name)
	require.Equal(t, "REPL", calls[1].Name)
	require.NotEqual(t, calls[0].ID, calls[1].ID, "两条调用的 id 必须互不相同")
	require.NotContains(t, rest, tc01Open)
	require.NotContains(t, rest, "CallDynamicTool")
	require.Equal(t, "一。二。三。", rest)
}

// TestCursorParseInlineToolCall_OpenOnlyNoClose 误判防护：正文里**只出现开标记、
// 没有闭标记** → **不判为内联形态**（recognized=false，按「未识别」处理，
// 而不是「识别到但解析失败」）。
//
// 去掉零宽字符后，标记只能靠**形状**识别：不成对就没有内联形态可言，
// 宁可漏判也不能把正常正文当工具调用吃掉。
func TestCursorParseInlineToolCall_OpenOnlyNoClose(t *testing.T) {
	for _, content := range []string{
		tc01Open,
		"前缀。" + tc01Open,
		"前缀。" + tc01Open + "\n" + `{"id":"call_x","name":"REPL","arguments":{}}`,
		"前缀。" + tc01Open + " 只有开标记没有闭标记",
	} {
		calls, rest, recognized := cursorParseInlineToolCalls(content)

		require.False(t, recognized, "只有开标记、没有闭标记 → 不得判为内联形态")
		require.Empty(t, calls)
		require.Equal(t, content, rest, "未识别时正文必须原样返回")
	}
}

// TestCursorParseInlineToolCall_PlainWord 正文只含 `tool_call` 字样 →
// recognized=false。
func TestCursorParseInlineToolCall_PlainWord(t *testing.T) {
	content := "文档里提到 tool_call 这个词，仅此而已。"

	calls, rest, recognized := cursorParseInlineToolCalls(content)

	require.False(t, recognized)
	require.Empty(t, calls)
	require.Equal(t, content, rest)
}

// TestCursorParseInlineToolCall_Empty 空串 / 只有空白 → recognized=false、
// calls 空、rest 原样。
func TestCursorParseInlineToolCall_Empty(t *testing.T) {
	for _, content := range []string{"", "   ", "\n\t \n"} {
		calls, rest, recognized := cursorParseInlineToolCalls(content)

		require.False(t, recognized)
		require.Empty(t, calls)
		require.Equal(t, content, rest)
	}
}

// TestCursorParseInlineToolCall_MalformedNoClose 畸形输入（开标记后无闭标记）
// → **不判为内联形态**（recognized=false）、calls 空、rest 保持原文
// （一个字节都不吞）。
//
// 语义变更说明：TC-01 原语义是 recognized=true（形态已被识别）+ calls 空，
// 调用方据此失败关闭；TC-01R2 按新判据改为 recognized=false（未识别），
// 理由见 cursorInlineScanSegments 的注释：不成对就没有内联形态。
func TestCursorParseInlineToolCall_MalformedNoClose(t *testing.T) {
	content := "前缀。" + tc01Open + "\n" + `{"id":"call_x","name":"REPL","arguments":{}}`

	calls, rest, recognized := cursorParseInlineToolCalls(content)

	require.False(t, recognized, "开标记无闭标记 → 不得判为内联形态")
	require.Empty(t, calls, "未识别时不得返回半成品调用")
	require.Equal(t, content, rest, "未识别时正文必须原样返回")
}

// TestCursorParseInlineToolCall_MalformedGarbage 畸形输入（开闭成对但内容解不出
// 参数 / 空内容 / 坏 UTF-8）→ 不 panic，且按定义语义返回。
func TestCursorParseInlineToolCall_MalformedGarbage(t *testing.T) {
	for _, content := range []string{
		tc01Open + tc01Close,
		tc01Open + "not json at all" + tc01Close,
		tc01Open + tc01FormBBody() + tc01Close + tc01Open, // 尾部开标记无闭标记
		tc01Open + string([]byte{0x00, 0xff, 0xfe}) + tc01Close,
	} {
		calls, rest, recognized := cursorParseInlineToolCalls(content)

		if recognized {
			require.Empty(t, calls, "识别到形态但解析不出参数时 calls 必须为空")
			require.Equal(t, content, rest, "失败时正文必须原样返回")
		} else {
			require.Empty(t, calls)
			require.Equal(t, content, rest)
		}
	}
}

// TestCursorParseInlineToolCall_IDStable id 稳定性：同一份正文两次解析，
// 形态 A 的 id 与形态 B 生成的 id 都必须完全一致。
func TestCursorParseInlineToolCall_IDStable(t *testing.T) {
	formA := "前言。" + tc01FormA
	formB := tc01Open + tc01FormBBody() + tc01Close

	callsA1, _, _ := cursorParseInlineToolCalls(formA)
	callsA2, _, _ := cursorParseInlineToolCalls(formA)
	require.Len(t, callsA1, 1)
	require.Len(t, callsA2, 1)
	require.Equal(t, callsA1[0].ID, callsA2[0].ID, "形态 A 的 id 必须稳定")

	callsB1, _, _ := cursorParseInlineToolCalls(formB)
	callsB2, _, _ := cursorParseInlineToolCalls(formB)
	require.Len(t, callsB1, 1)
	require.Len(t, callsB2, 1)
	require.Equal(t, callsB1[0].ID, callsB2[0].ID, "形态 B 生成的 id 必须稳定")
}

// TestCursorParseInlineToolCall_IDUniqueAcrossTurns 形态 B 生成的 id 在
// 不同正文之间不得撞车：两轮正文各自只有一段形态 B 内联调用时，序号相同
// （都是第 1 段），必须靠正文哈希把它们区分开，否则客户端侧 id 匹配会串轮。
func TestCursorParseInlineToolCall_IDUniqueAcrossTurns(t *testing.T) {
	turn1 := "第一轮。" + tc01Open + tc01FormBBody() + tc01Close
	turn2 := "第二轮。" + tc01Open + tc01FormBBody() + tc01Close

	calls1, _, _ := cursorParseInlineToolCalls(turn1)
	calls2, _, _ := cursorParseInlineToolCalls(turn2)

	require.Len(t, calls1, 1)
	require.Len(t, calls2, 1)
	require.NotEqual(t, calls1[0].ID, calls2[0].ID,
		"不同正文的内联调用 id 必须不同，否则客户端侧 id 匹配会串轮")
}

// TestCursorInlineToolCallConstants 标记常量必须逐字节等于生产实测码点表
// （开闭标记对称），且不含任何零宽字符。
func TestCursorInlineToolCallConstants(t *testing.T) {
	require.Equal(t, tc01ProdOpen, cursorInlineToolCallOpen)
	require.Equal(t, tc01ProdClose, cursorInlineToolCallClose)
	require.Equal(t, tc01ProdSep, cursorInlineToolCallFieldSep)
	require.Equal(t, tc01ProdOpen, tc01Open, "测试样本与生产常量必须同形")
	require.Equal(t, tc01ProdClose, tc01Close, "测试样本与生产常量必须同形")
}
