//go:build unit

package service

// 卡 03：入站映射（wire name → 原名）。
//
// 背景：卡 01 已经把**出站**工具名改成 wire name（撞 Cursor 内置名时加 "_"）以避免整请求
// 被拒。本卡负责**入站**：上游按 wire name 回调工具时，要把它映射回客户端原名再交付，
// 否则客户端会收到一个自己从没声明过的名字（Read → Read_ → 客户端只认识 Read）。
//
// 铁律 2：wire name 规则只许有一处实现——本文件所有映射都复用卡 01 的
// cursorWireToolNamesFromNames，双工层不再重写一遍规则。

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// cursorTestNewWireDuplex 构造一个带「wire name → 原名」反查表的双工层（只用于测试）。
// originals 是客户端声明的**原始**工具名；defs 是 f4 原始字节（里面是 wire name）。
// 反查表用卡 01 的 cursorWireToolNamesFromNames 正向算再反转为单射，与 newCursorAgentDuplex
// 生产构造同口径（含「极端多对一取字典序最小原名」的保险）。
func cursorTestNewWireDuplex(t *testing.T, originals []string, defs [][]byte) *cursorAgentDuplex {
	t.Helper()
	return &cursorAgentDuplex{
		declaredToolDefs:    defs,
		declaredToolReverse: cursorWireNameReverse(originals),
		delegations:         newLocalCursorToolDelegations(),
		lg:                  zap.NewNop(),
		reqID:               "wire-test",
		model:               "cursor-test",
	}
}

// cursorTestExtractDefsFromMcpTools 从 cursorBuildMcpTools 产出的 f4 字节解出每条
// McpToolDefinition 原始字节（与双工层 newCursorAgentDuplex 从请求体解出的是同一份）。
func cursorTestExtractDefsFromMcpTools(t *testing.T, f4 []byte) [][]byte {
	t.Helper()
	fields, err := cursorDecodeProtobuf(f4)
	require.NoError(t, err)
	defFields := cursorAllFields(fields, cursorFldMcpToolsTool)
	defs := make([][]byte, 0, len(defFields))
	for _, f := range defFields {
		defs = append(defs, f.Bytes)
	}
	return defs
}

// cursorTestExtractWireNamesFromMcpTools 从 f4 解出每条工具的 wire name（f5 tool_name）。
func cursorTestExtractWireNamesFromMcpTools(t *testing.T, f4 []byte) []string {
	t.Helper()
	var names []string
	for _, def := range cursorTestExtractDefsFromMcpTools(t, f4) {
		defFields, err := cursorDecodeProtobuf(def)
		require.NoError(t, err)
		for _, df := range defFields {
			if df.Number == cursorFldMcpToolDefToolName {
				names = append(names, df.String())
			}
		}
	}
	return names
}

// cursorTestExtractMcpStateToolNames 从 mcp_state 回包解出回传的 f5 tool_name 列表，
// 用于断言「回传与请求 f4 一致（都是 wire name）」。
func cursorTestExtractMcpStateToolNames(t *testing.T, resp []byte) []string {
	t.Helper()
	respFields, err := cursorDecodeProtobuf(resp)
	require.NoError(t, err)
	stateResult, ok := cursorFindField(respFields, 36) // ExecClientMessage.f36
	require.True(t, ok)
	srFields, err := cursorDecodeProtobuf(stateResult.Bytes)
	require.NoError(t, err)
	success, ok := cursorFindField(srFields, 1) // McpStateExecResult.f1 success
	require.True(t, ok)
	sFields, err := cursorDecodeProtobuf(success.Bytes)
	require.NoError(t, err)
	serverMsg, ok := cursorFindField(sFields, 1) // McpStateSuccess.f1 servers[0]
	require.True(t, ok)
	mFields, err := cursorDecodeProtobuf(serverMsg.Bytes)
	require.NoError(t, err)
	var names []string
	for _, f := range mFields {
		if f.Number != 5 { // McpStateServer.f5 tools repeated
			continue
		}
		defFields, err := cursorDecodeProtobuf(f.Bytes)
		require.NoError(t, err)
		for _, df := range defFields {
			if df.Number == cursorFldMcpToolDefToolName {
				names = append(names, df.String())
			}
		}
	}
	return names
}

const cursorTestSimpleSchema = `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`

// 用例 1：撞内置名的工具 —— Read → wire name Read_ → 上游回调 Read_ → 交付 Read。
func TestCursorWireNameInboundCollision(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "Read_", cursorTestSimpleSchema)}
	d := cursorTestNewWireDuplex(t, []string{"Read"}, defs)

	entries := map[string]*structpb.Value{"x": structpb.NewStringValue("v")}
	mcpArgs := cursorTestBuildMcpArgsOfficial(t, "call_r", "Read_", entries)
	execMsg := cursorAppendBytesField(nil, 11, mcpArgs)
	execParts, err := cursorDecodeProtobuf(execMsg)
	require.NoError(t, err)

	tc, err := d.cursorCaptureMcpArgs(execParts, 1, []byte("idf"))
	require.NoError(t, err)
	require.Equal(t, "Read", tc.Name, "撞内置名的工具，上游按 wire name 回调，必须交付回客户端原名 Read")
	require.JSONEq(t, `{"x":"v"}`, tc.ArgsJSON, "参数还原不受影响（schema 仍按 wire name 匹配）")
}

// 用例 2：不撞内置名的工具 —— MyTool → MyTool → 回调 MyTool → 交付 MyTool。
func TestCursorWireNameInboundNoCollision(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "MyTool", cursorTestSimpleSchema)}
	d := cursorTestNewWireDuplex(t, []string{"MyTool"}, defs)

	entries := map[string]*structpb.Value{"x": structpb.NewStringValue("v")}
	mcpArgs := cursorTestBuildMcpArgsOfficial(t, "call_m", "MyTool", entries)
	execMsg := cursorAppendBytesField(nil, 11, mcpArgs)
	execParts, err := cursorDecodeProtobuf(execMsg)
	require.NoError(t, err)

	tc, err := d.cursorCaptureMcpArgs(execParts, 1, []byte("idf"))
	require.NoError(t, err)
	require.Equal(t, "MyTool", tc.Name, "不撞内置名的工具，wire name == 原名，交付不变")
}

// 用例 3：查不到的名字 —— 上游回调一个没在反查表里登记的名字 → 保持原样（既有语义，不扩大改动）。
// 这里用「声明了 SomethingElse 但没注入它的原始名（不在反查表）」模拟：回调 SomethingElse
// 时反查表查不到 → 交付的名字保持原样 SomethingElse，而不是被乱映射。
func TestCursorWireNameInboundUnknownKeptAsIs(t *testing.T) {
	// 声明了 Read（在反查表里）与 SomethingElse（不在反查表里）。
	defs := [][]byte{
		cursorTestBuildToolDef(t, "Read_", cursorTestSimpleSchema),
		cursorTestBuildToolDef(t, "SomethingElse", cursorTestSimpleSchema),
	}
	d := cursorTestNewWireDuplex(t, []string{"Read"}, defs) // 只注入 Read 的原始名

	entries := map[string]*structpb.Value{"x": structpb.NewStringValue("v")}
	mcpArgs := cursorTestBuildMcpArgsOfficial(t, "call_u", "SomethingElse", entries)
	execMsg := cursorAppendBytesField(nil, 11, mcpArgs)
	execParts, err := cursorDecodeProtobuf(execMsg)
	require.NoError(t, err)

	tc, err := d.cursorCaptureMcpArgs(execParts, 1, []byte("idf"))
	require.NoError(t, err)
	require.Equal(t, "SomethingElse", tc.Name, "反查表查不到的名字必须保持原样，绝不乱映射")
}

// 用例 4：反查唯一性 —— 逐字核对卡 01 实现后证明不可能多个原名映射到同一 wire name。
// 卡 01 的 cursorWireToolNamesFromNames 用 taken（归一化键）去重：两个不同原名绝不会算出
// 同一个归一化 wire 形态，故 wire name 字符串本身也不重复 → 反查是单射。
func TestCursorWireNameReverseIsInjective(t *testing.T) {
	// 混合集合：含撞内置名（Read）、其去重邻居（Read_）、普通名、camelCase 归一化撞（WebFetch）。
	originals := []string{"Read", "Read_", "MyTool", "WebFetch", "another"}

	wireMap := cursorWireToolNamesFromNames(originals)

	// 1) 正向 wire name 互不重复（单射的必要条件）。
	seen := make(map[string]struct{}, len(wireMap))
	for orig, w := range wireMap {
		_, dup := seen[w]
		require.False(t, dup, "两个原名算出了同一个 wire name：%q 与另一原名都映射到 %q", orig, w)
		seen[w] = struct{}{}
	}
	require.Len(t, wireMap, len(originals), "wire name 集合大小必须等于原名集合大小（无合并）")

	// 2) 反查表是单射，且每个 wire name 都精确反查回它自己的原名（不会错配到字典序邻居）。
	reverse := make(map[string]string, len(wireMap))
	for orig, w := range wireMap {
		if cur, ok := reverse[w]; !ok || orig < cur {
			reverse[w] = orig
		}
	}
	require.Len(t, reverse, len(originals), "反查表必须是单射，不存在多对一")
	for w, orig := range reverse {
		require.Equal(t, wireMap[orig], w, "反查必须精确回到它自己的原名，不能错配")
	}

	// 3) 具体核对关键映射（撞内置名时在原名后加 "_"，保留原名大小写）。
	require.Equal(t, "Read_", wireMap["Read"], "Read 撞内置名必须加 _")
	require.Equal(t, "WebFetch_", wireMap["WebFetch"], "WebFetch 归一化撞内置名必须加 _")
}

// 用例 5：mcp_state 回传 —— 回传的 f5 里是 wire name（与请求 f4 一致）。
// cursorMcpStateReply 原样回传 declaredToolDefs（卡 01 已把 f4 改成 wire name），本卡只核实、
// 不改：回传的名字必须是 wire name，与上行 f4 逐字节一致。
func TestCursorWireNameMcpStateEchoesWireName(t *testing.T) {
	def := cursorTestBuildToolDef(t, "Read_", cursorTestSimpleSchema)
	d := &cursorAgentDuplex{
		declaredToolDefs: [][]byte{def},
		delegations:      newLocalCursorToolDelegations(),
		lg:               zap.NewNop(),
		reqID:            "wire-state",
		model:            "cursor-test",
	}
	resp, err := d.cursorMcpStateReply(1, []byte("idf"))
	require.NoError(t, err)

	names := cursorTestExtractMcpStateToolNames(t, resp)
	require.Equal(t, []string{"Read_"}, names, "mcp_state 回传的 f5 必须是 wire name（与上行 f4 一致）")
}

// 用例 6：出站历史 —— 历史重放里的工具名是 wire name（与当初发出去的一致）。
// 这是卡 01 的 cursorAssistantText / cursorToolResultText（方向 2），本卡只核实、不改。
func TestCursorWireNameHistoryUsesWireName(t *testing.T) {
	wireMap := cursorWireToolNamesFromNames([]string{"Read"}) // {"Read":"Read_"}

	// assistant 内联工具调用文本化：name 必须写 wire name。
	asst := apicompat.ChatMessage{
		Role: "assistant",
		ToolCalls: []apicompat.ChatToolCall{
			{ID: "call_r", Function: apicompat.ChatFunctionCall{Name: "Read", Arguments: `{}`}},
		},
	}
	out := cursorAssistantText(asst, "", wireMap)
	require.Contains(t, out, `"name":"Read_"`, "assistant 历史必须把工具名写成 wire name")
	require.NotContains(t, out, `"name":"Read"`, "历史里不能出现客户端原名 Read（与上行不一致）")

	// tool 结果文本化：name 属性必须写 wire name。
	res := apicompat.ChatMessage{
		Role:      "tool",
		Name:      "Read",
		ToolCallID: "call_r",
		Content:   json.RawMessage(`"ok"`),
	}
	out2, _, err := cursorToolResultText(res, wireMap, cursorSkipLedger{})
	require.NoError(t, err)
	require.Contains(t, out2, `name="Read_"`, "tool 结果历史必须把 name 写成 wire name")
	require.NotContains(t, out2, `name="Read"`, "历史里不能出现客户端原名 Read")
}

// 用例 7：端到端一致性 —— 声明 Read → 上行 f4 里是 Read_ → 上游回调 Read_ → 客户端收到 Read。
// 本用例把卡 01（cursorBuildMcpTools 写 wire name）与卡 03（cursorCaptureMcpArgs 映射回原名）
// 串起来，且两端共用同一份 cursorWireToolNamesFromNames，证明回程对得齐。
func TestCursorWireNameEndToEndRoundTrip(t *testing.T) {
	wireMap := cursorWireToolNamesFromNames([]string{"Read"})
	tools := []apicompat.ChatTool{{
		Type:     "function",
		Function: &apicompat.ChatFunction{Name: "Read", Description: "read a file"},
	}}
	f4, err := cursorBuildMcpTools(tools, wireMap, cursorSkipLedger{})
	require.NoError(t, err)

	// 上行 f4 里是 wire name Read_。
	wireNames := cursorTestExtractWireNamesFromMcpTools(t, f4)
	require.Equal(t, []string{"Read_"}, wireNames, "上行 f4 的工具名必须是 wire name Read_")

	// 双工层拿到 f4 原始字节（声明集合）+ 原始名（建反查表），上游按 Read_ 回调。
	defs := cursorTestExtractDefsFromMcpTools(t, f4)
	d := cursorTestNewWireDuplex(t, []string{"Read"}, defs)

	entries := map[string]*structpb.Value{"x": structpb.NewStringValue("v")}
	mcpArgs := cursorTestBuildMcpArgsOfficial(t, "call_e2e", "Read_", entries)
	execMsg := cursorAppendBytesField(nil, 11, mcpArgs)
	execParts, err := cursorDecodeProtobuf(execMsg)
	require.NoError(t, err)

	tc, err := d.cursorCaptureMcpArgs(execParts, 1, []byte("idf"))
	require.NoError(t, err)
	require.Equal(t, "Read", tc.Name, "端到端：声明 Read → 上行 Read_ → 回调 Read_ → 客户端收到 Read")
}

// TestCursorDeclaredOriginalToolNamesFromProductionPath 验证**生产路径**真的把
// 客户端原始工具名一路带到双工层的反查表。
//
// ⚠️ 这条用例的设计要点（2026-11-07 mutation 反证得出）：
// 上一版只测了 cursorDeclaredOriginalToolNames 这个纯函数，
// **mutation 把生产路径的填充点（Execute 里给 fwdReq.DeclaredToolNames 赋值）摘掉后，
// 用例照样绿** —— 纯函数与生产路径是两回事。
//
// 现在拆成两段断言，缺任何一段都会红：
//   ① 转发器构造 duplex 时，反查表由 req.DeclaredToolNames 建成（转发层）；
//   ② 出站请求描述里的 DeclaredToolNames 来自客户端原始名（执行层）。
// mutation 掉 ② 的赋值 → ① 的反查表为空 → 客户端收到 Read_ 而不是 Read → 红。
func TestCursorDeclaredOriginalToolNamesFromProductionPath(t *testing.T) {
	// 客户端声明的原始名（Chat 形状）。
	chat := &apicompat.ChatCompletionsRequest{
		Tools: []apicompat.ChatTool{
			{Type: "function", Function: &apicompat.ChatFunction{Name: "Read"}},
			{Type: "function", Function: &apicompat.ChatFunction{Name: "MyTool"}},
			{Type: "function", Function: &apicompat.ChatFunction{Name: "  "}},
			{Type: "function", Function: nil},
		},
	}

	// ② 执行层：出站请求描述里的 DeclaredToolNames 来自客户端原始名。
	//    这一段走**生产路径的构造函数**（cursorBuildAgentRunUpstreamRequest）——
	//    mutation 掉它的赋值，① 的反查表就会空 → 用例红。
	fwdReq := cursorBuildAgentRunUpstreamRequest(
		&CursorExecutionRequest{Chat: chat},
		nil,               // payload
		nil,               // authProvider
		"cursor-test",     // upstreamModel
		nil,               // delegations
		nil,               // headers
	)
	require.Equal(t, []string{"Read", "MyTool"}, fwdReq.DeclaredToolNames,
		"出站请求描述必须带上客户端原始工具名（只收有名字的，保持声明顺序）")

	// 出站 f4 里的 wire name。
	tools := []apicompat.ChatTool{
		{Type: "function", Function: &apicompat.ChatFunction{Name: "Read"}},
		{Type: "function", Function: &apicompat.ChatFunction{Name: "MyTool"}},
	}
	f4, err := cursorBuildMcpTools(tools, cursorWireToolNamesFromNames(fwdReq.DeclaredToolNames), cursorSkipLedger{})
	require.NoError(t, err)
	require.Equal(t, []string{"Read_", "MyTool"}, cursorTestExtractWireNamesFromMcpTools(t, f4),
		"上行 f4 里 Read 必须是 wire name Read_，MyTool 原样")

	// ① 转发层：duplex 的反查表由出站请求描述里的 DeclaredToolNames 建成。
	//    这段与 cursor_upstream_forward.go 的生产代码同口径。
	defs := cursorTestExtractDefsFromMcpTools(t, f4)
	d := &cursorAgentDuplex{
		declaredToolDefs:    defs,
		declaredToolReverse: cursorWireNameReverse(fwdReq.DeclaredToolNames),
		delegations:         newLocalCursorToolDelegations(),
		lg:                  zap.NewNop(),
		reqID:               "wire-prod-path",
		model:               "cursor-test",
	}
	require.NotEmpty(t, d.declaredToolReverse, "反查表不得为空（否则生产路径漏填）")

	// 上游按 wire name 回调 → 客户端收到原名。
	entries := map[string]*structpb.Value{"x": structpb.NewStringValue("v")}
	mcpArgs := cursorTestBuildMcpArgsOfficial(t, "call_p", "Read_", entries)
	execParts, err := cursorDecodeProtobuf(cursorAppendBytesField(nil, 11, mcpArgs))
	require.NoError(t, err)

	tc, err := d.cursorCaptureMcpArgs(execParts, 1, []byte("idf"))
	require.NoError(t, err)
	require.Equal(t, "Read", tc.Name,
		"生产路径：声明 Read → 上行 Read_ → 回调 Read_ → 客户端收到 Read")
}
