//go:build unit

package service

// P1-A1 卡的请求转换单测：OpenAI Chat Completions → agent.v1.AgentRun 请求体。
//
// 断言以**字节级回读**为主（不只断言"不报错"），确保字段号与嵌套层级真的写对了
// ——否则单测会在实现整体写错字段号时仍然全绿。
//
// 形状对齐证据 33-agent-run-request.bin（单轮，活体 200）与
// 63-agent-run-request.bin（多轮历史，活体 200，回包 "ORCHID" 证明历史被消费）。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// cursorAllFields 取出某个字段号的**全部**出现（repeated 字段需要）。
func cursorAllFields(fields []cursorPBField, number int) []cursorPBField {
	var out []cursorPBField
	for _, f := range fields {
		if f.Number == number {
			out = append(out, f)
		}
	}
	return out
}

// cursorTestF3SchemaOf 解码 McpToolDefinition.f3（google.protobuf.Value）为规范 JSON。
// 这里自带解码而不复用 cursorTestRestoreF3Schema：后者还检查旧 f6 字段，本卡已删除该常量。
func cursorTestF3SchemaOf(t *testing.T, fields []cursorPBField) string {
	t.Helper()
	f3, ok := cursorFindField(fields, cursorFldMcpToolDefInputSchemaValue)
	require.True(t, ok, "f3 input_schema Value must be present")
	v := &structpb.Value{}
	require.NoError(t, proto.Unmarshal(f3.Bytes, v))
	out, err := protojson.Marshal(v)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, json.Compact(&buf, out))
	return buf.String()
}

// cursorTestSingleMcpDefOf 取出 f4 里唯一一条 McpToolDefinition 的字段列表。
// 上游只声明 invoke_client_tool，f4 必须恰好一条，不能逐工具展开。
func cursorTestSingleMcpDefOf(t *testing.T, body []byte) []cursorPBField {
	t.Helper()
	run := cursorAgentRunOf(t, body)
	mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
	require.True(t, ok, "mcp_tools field must be present")
	mcpFields, err := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, err)
	defs := cursorAllFields(mcpFields, cursorFldMcpToolsTool)
	require.Len(t, defs, 1, "exactly one invoke_client_tool definition must be declared")
	fields, err := cursorDecodeProtobuf(defs[0].Bytes)
	require.NoError(t, err)
	return fields
}

// cursorTestTransportSchema 解析单入口 f3，返回 type / required / enum / additionalProperties / properties。
func cursorTestTransportSchema(t *testing.T, def []cursorPBField) (string, []string, []string, bool, map[string]any) {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(cursorTestF3SchemaOf(t, def)), &obj), "f3 must be a JSON object")
	typ, _ := obj["type"].(string)
	props, _ := obj["properties"].(map[string]any)
	var required []string
	if arr, ok := obj["required"].([]any); ok {
		for _, v := range arr {
			required = append(required, v.(string))
		}
	}
	var enum []string
	if toolName, ok := props[cursorTransportToolNameKey].(map[string]any); ok {
		if arr, ok := toolName["enum"].([]any); ok {
			for _, v := range arr {
				enum = append(enum, v.(string))
			}
		}
	}
	additional, _ := obj["additionalProperties"].(bool)
	return typ, required, enum, additional, props
}

// cursorAgentRunOf 拆出 envelope → AgentClientMessage → AgentRunRequest 的字段列表。
func cursorAgentRunOf(t *testing.T, body []byte) []cursorPBField {
	t.Helper()
	require.Equal(t, byte(0), body[0], "envelope flags must be 0")
	outer, err := cursorDecodeProtobuf(body[cursorEnvelopeHeaderSize:])
	require.NoError(t, err)
	run, ok := cursorFindField(outer, cursorFldAgentRunRequest)
	require.True(t, ok, "outer must carry field 1 (run_request)")
	fields, err := cursorDecodeProtobuf(run.Bytes)
	require.NoError(t, err)
	return fields
}

// cursorActionTextOf 取出 action.user_message_action.user_message.text。
func cursorActionTextOf(t *testing.T, run []cursorPBField) string {
	t.Helper()
	action, ok := cursorFindField(run, cursorFldRunAction)
	require.True(t, ok, "run_request must carry field 2 (action)")
	actionFields, err := cursorDecodeProtobuf(action.Bytes)
	require.NoError(t, err)
	uma, ok := cursorFindField(actionFields, cursorFldActionUserMessage)
	require.True(t, ok, "action must carry field 1 (user_message_action)")
	umaFields, err := cursorDecodeProtobuf(uma.Bytes)
	require.NoError(t, err)
	msg, ok := cursorFindField(umaFields, cursorFldUserMsgActionMessage)
	require.True(t, ok, "user_message_action must carry field 1 (user_message)")
	msgFields, err := cursorDecodeProtobuf(msg.Bytes)
	require.NoError(t, err)
	text, ok := cursorFindField(msgFields, cursorFldUserMsgText)
	require.True(t, ok, "user_message must carry field 1 (text)")
	return text.String()
}

// cursorModelIDOf 取出 model_details.model_id。
func cursorModelIDOf(t *testing.T, run []cursorPBField) string {
	t.Helper()
	md, ok := cursorFindField(run, cursorFldRunModelDetails)
	require.True(t, ok, "run_request must carry field 3 (model_details)")
	fields, err := cursorDecodeProtobuf(md.Bytes)
	require.NoError(t, err)
	id, ok := cursorFindField(fields, cursorFldModelDetailsID)
	require.True(t, ok)
	return id.String()
}

// cursorBlobsOf 取出 pre_fetched_blobs 的 (id, value) 列表（保序）。
func cursorBlobsOf(t *testing.T, run []cursorPBField) [][2][]byte {
	t.Helper()
	var out [][2][]byte
	for _, f := range cursorAllFields(run, cursorFldRunPreFetchedBlobs) {
		fields, err := cursorDecodeProtobuf(f.Bytes)
		require.NoError(t, err)
		id, ok := cursorFindField(fields, cursorFldBlobID)
		require.True(t, ok)
		value, ok := cursorFindField(fields, cursorFldBlobValue)
		require.True(t, ok)
		out = append(out, [2][]byte{id.Bytes, value.Bytes})
	}
	return out
}

// cursorWithFixedUUID 把标识生成替换为确定性序列（便于整包字节断言）。
func cursorWithFixedUUID(t *testing.T, ids ...string) {
	t.Helper()
	original := cursorNewUUID
	i := 0
	cursorNewUUID = func() string {
		if i >= len(ids) {
			return ids[len(ids)-1]
		}
		v := ids[i]
		i++
		return v
	}
	t.Cleanup(func() { cursorNewUUID = original })
}

// TestCursorConvertRequestBasic 覆盖基本映射：末轮 → action，之前 → 历史 blob。
func TestCursorProtocolRequestBasic(t *testing.T) {
	cursorWithFixedUUID(t, "conv-fixed-0001", "msg-fixed-0001")
	req := &apicompat.ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"what is 2+2?"`)},
			{Role: "assistant", Content: json.RawMessage(`"4"`)},
			{Role: "user", Content: json.RawMessage(`"and 3+3?"`)},
		},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	require.Equal(t, "grok-4.7", cursorModelIDOf(t, run), "model_id must carry the wire ID")
	require.Equal(t, "and 3+3?", cursorActionTextOf(t, run), "last user message becomes the action")

	// conversation_state：mode=2 + 两条历史 blob id（证据 63 的同形结构）。
	state, ok := cursorFindField(run, cursorFldRunConversationState)
	require.True(t, ok)
	stateFields, err := cursorDecodeProtobuf(state.Bytes)
	require.NoError(t, err)
	mode, ok := cursorFindField(stateFields, cursorFldStateMode)
	require.True(t, ok, "conversation_state must carry mode")
	require.Equal(t, uint64(cursorConversationModeAgent), mode.Varint)
	require.Len(t, cursorAllFields(stateFields, cursorFldStateRootPromptBlobs), 2)

	// mcp_tools 空 message 必须**存在**（活体样本同形），但不得携带任何工具。
	mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
	require.True(t, ok, "mcp_tools field must be present (empty), as in live samples")
	require.Empty(t, mcp.Bytes)

	// conversation_id 必须有值（每轮独立）。
	convID, ok := cursorFindField(run, cursorFldRunConversationID)
	require.True(t, ok)
	require.Equal(t, "conv-fixed-0001", convID.String())

	// 历史 blob：内容寻址 + 保序 + 形状与活体样本一致。
	blobs := cursorBlobsOf(t, run)
	require.Len(t, blobs, 2)
	require.Equal(t, `{"role":"user","content":"what is 2+2?"}`, string(blobs[0][1]))
	require.JSONEq(t,
		`{"role":"assistant","content":[{"type":"text","text":"4"}],"id":"1"}`,
		string(blobs[1][1]))
	for i, b := range blobs {
		sum := sha256.Sum256(b[1])
		require.Equal(t, sum[:], b[0], "blob %d id must be sha256(value)", i)
		require.Len(t, b[0], 32)
	}
}

// TestCursorConvertRequestSingleTurnHasNoHistory 覆盖单轮：无历史 blob。
func TestCursorProtocolRequestSingleTurnHasNoHistory(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)
	run := cursorAgentRunOf(t, body)
	require.Empty(t, cursorBlobsOf(t, run))
	require.Equal(t, "hi", cursorActionTextOf(t, run))
}

// TestCursorConvertRequestSystemMerge 覆盖 system 合并策略（首版裁定：并入首个 user）。
//
// 这是本卡最需要被钉死的行为：system 文本**绝不能丢**（丢 = 静默改变语义）。
func TestCursorProtocolRequestSystemMerge(t *testing.T) {
	t.Run("单条 system 前置合并进首条 user", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "system", Content: json.RawMessage(`"You are terse."`)},
				{Role: "user", Content: json.RawMessage(`"hi"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		require.Empty(t, cursorBlobsOf(t, run), "system must not become its own history turn")
		text := cursorActionTextOf(t, run)
		require.Contains(t, text, "You are terse.")
		require.Contains(t, text, "hi")
	})

	t.Run("多条 system 保序合并并加边界标记", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "system", Content: json.RawMessage(`"FIRST"`)},
				{Role: "system", Content: json.RawMessage(`"SECOND"`)},
				{Role: "user", Content: json.RawMessage(`"q"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		text := cursorActionTextOf(t, run)
		require.Less(t, indexOfSub(text, "FIRST"), indexOfSub(text, "SECOND"), "order must be preserved")
		require.Contains(t, text, "<system>", "multi-part system must carry explicit boundaries")
	})

	t.Run("developer 角色等同 system", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "developer", Content: json.RawMessage(`"DEV"`)},
				{Role: "user", Content: json.RawMessage(`"q"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		require.Contains(t, cursorActionTextOf(t, run), "DEV")
	})

	t.Run("尾部 system 新建 user 轮次承载而非丢弃", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"q"`)},
				{Role: "assistant", Content: json.RawMessage(`"a"`)},
				{Role: "system", Content: json.RawMessage(`"TAIL"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		// 末轮是 system 新建的 user 轮：它成为 action，前两轮进历史。
		require.Contains(t, cursorActionTextOf(t, run), "TAIL")
		blobs := cursorBlobsOf(t, run)
		require.Len(t, blobs, 2)
		require.Contains(t, string(blobs[0][1]), "q")
		require.Contains(t, string(blobs[1][1]), "a")
	})
}

// indexOfSub 是 strings.Index 的本地副本（避免为一个断言引入 import）。
func indexOfSub(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestCursorProtocolRequestDropsOutOfScopeFields 覆盖范围外字段的处理。
//
// 注意：本卡（O11-B）已实现工具闭环，tools 不再"丢弃"而是翻译进 f4；tool 结果消息
// 也不再丢弃，而是文本化为 user turn 的 <tool_result>。下方子测试已相应更新为新语义。
func TestCursorProtocolRequestDropsOutOfScopeFields(t *testing.T) {
	t.Run("请求侧 tools 编码为唯一单入口 invoke_client_tool", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)},
				{Role: "user", Content: json.RawMessage(`"again"`)}},
			Tools: []apicompat.ChatTool{{
				Type: "function",
				Function: &apicompat.ChatFunction{
					Name:        "get_weather",
					Description: "Get the weather.",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
				},
			}},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		def := cursorTestSingleMcpDefOf(t, body)
		require.Equal(t, cursorTransportToolName, cursorTestFieldString(def, cursorFldMcpToolDefName))
		require.Equal(t, cursorTransportToolName, cursorTestFieldString(def, cursorFldMcpToolDefToolName))
		require.Equal(t, cursorMcpProviderIdentifier, cursorTestFieldString(def, cursorFldMcpToolDefProvider))

		// f2 description 含工具目录（原名 + 原 schema 文本），且只说明运输形状。
		desc := cursorTestFieldString(def, cursorFldMcpToolDefDescription)
		require.Contains(t, desc, "Tool catalog:")
		require.Contains(t, desc, `"name":"get_weather"`)
		require.Contains(t, desc, `"parameters":{"type":"object","properties":{"city":{"type":"string"}}}`)

		// f3 只约束两个运输字段：object / required 两项 / additionalProperties=false。
		typ, required, enum, additional, props := cursorTestTransportSchema(t, def)
		require.Equal(t, "object", typ)
		require.ElementsMatch(t, []string{cursorTransportToolNameKey, cursorTransportArgumentsKey}, required)
		require.False(t, additional)
		require.ElementsMatch(t, []string{"get_weather"}, enum)
		toolName, ok := props[cursorTransportToolNameKey].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "string", toolName["type"])
		args, ok := props[cursorTransportArgumentsKey].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "string", args["type"])
		require.Equal(t, "The complete arguments object serialized as a JSON string, preserving all types and nulls.", args["description"])
	})

	t.Run("tool_choice=none 时不声明工具（等价无 tools）", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Tools: []apicompat.ChatTool{{
				Type:     "function",
				Function: &apicompat.ChatFunction{Name: "get_weather"},
			}},
			ToolChoice: json.RawMessage(`"none"`),
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
		require.True(t, ok)
		require.Empty(t, mcp.Bytes, "tool_choice=none must not declare tools")
		require.NotContains(t, string(body), "get_weather")
	})

	t.Run("tool 结果消息文本化为 user turn 的 <tool_result>", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"hi"`)},
				{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: json.RawMessage(`"sunny"`)},
				{Role: "assistant", Content: json.RawMessage(`"done"`)},
				{Role: "user", Content: json.RawMessage(`"next"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		require.Equal(t, "next", cursorActionTextOf(t, run))
		// tool 消息成为 user turn → 进历史 blob，共 3 条历史。
		require.Len(t, cursorBlobsOf(t, run), 3)
		// 文本化边界必须存在（blob 是 JSON 字符串，内部引号会被 JSON 转义为 \"）。
		require.Contains(t, string(body), "<tool_result")
		require.Contains(t, string(body), `tool_call_id=\"call_1\"`)
		require.Contains(t, string(body), `name=\"invoke_client_tool\"`)
		require.Contains(t, string(body), "sunny")
	})
}

// TestCursorConvertRequestFailsClosed 覆盖首版范围外形状一律失败关闭。
func TestCursorProtocolRequestFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		req  *apicompat.ChatCompletionsRequest
	}{
		{"nil request", nil},
		{"no messages", &apicompat.ChatCompletionsRequest{}},
		{"unknown role", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "wizard", Content: json.RawMessage(`"hi"`)}}}},
		{"missing role", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Content: json.RawMessage(`"hi"`)}}}},
		{"image content part", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(
				`[{"type":"image_url","image_url":{"url":"http://x/y.png"}}]`)}}}},
		{"numeric content", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`42`)}}}},
		{"object content", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`{"a":1}`)}}}},
		// AgentRun 的已取证入口只有 user_message_action：以 assistant 结尾的入站
		// 无落点，**不得**悄悄把 assistant 文本当用户话发出去。
		{"ends with assistant", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"hi"`)},
				{Role: "assistant", Content: json.RawMessage(`"prefill"`)},
			}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := cursorConvertRequest(c.req, "grok-4.7")
			require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
		})
	}
}

// TestCursorConvertRequestTextPartsAccepted 覆盖纯文本 content-parts 数组降级为字符串。
func TestCursorProtocolRequestTextPartsAccepted(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(
			`[{"type":"text","text":"part one "},{"type":"text","text":"part two"}]`)}},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)
	run := cursorAgentRunOf(t, body)
	require.Equal(t, "part one part two", cursorActionTextOf(t, run))
}

// TestCursorConvertRequestModelGateRejectsF1 钉死展示名不被静默改写：
// 调用方必须显式传 wire ID，本层不做任何"猜测式"模型名修正。
func TestCursorProtocolRequestModelGateRejectsF1(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	// 传入展示名：原样编码（上游会拒 ERROR_BAD_MODEL_NAME，那是**预期行为**，
	// 本层的职责是不篡改而不是替调用方猜 wire ID）。
	body, err := cursorConvertRequest(req, "gpt-5.5")
	require.NoError(t, err)
	run := cursorAgentRunOf(t, body)
	require.Equal(t, "gpt-5.5", cursorModelIDOf(t, run),
		"conversion must not rewrite the model name")

	// 空模型名则失败关闭（不得发出 model_details 缺失的请求）。
	_, err = cursorConvertRequest(req, "   ")
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
}

// TestCursorProtocolRequestMatchesLiveAgentRunSamples 用**活体请求字节**钉死请求构造。
//
// 为什么必须补这一条：上面那些断言是"自己编码、自己按同一套常量解码"的自洽断言
// ——字段号整体写错时，编解码两端一起错，测试照样全绿。这里把真实打到
// AgentService/Run 并拿到 200 的两份捕获（33 号单轮 149 字节 / 63 号多轮 500 字节，
// 见 /root/cursor-p1-evidence/）当黄金字节逐字节比对：字段号、嵌套层级、编码顺序、
// blob 内容寻址只要有一处偏了，这条就会红。
//
// 63 号样本同时是"多轮历史确实被构造出来"的实证：它的回包是 "ORCHID"，
// 只有历史被上游消费才可能答对。
func TestCursorProtocolRequestMatchesLiveAgentRunSamples(t *testing.T) {
	t.Run("33 号单轮样本（149 字节，活体 200，回包 OK）", func(t *testing.T) {
		// 注：f29 system_prompt_spec.append 的文本已改为单入口运输说明（MCP wire transport）。
		// 按当前实现重生成：f29 子消息长度 varint 为 ae 02（302 字节，旧期望 ad 02 少 1 字节），
		// 外层 run_request 长度 varint 为 bf 03（447 字节）；整包仍为 455 字节。
		const golden = "00000001c20abf030a02500112530a510a4f0a255265706c792077697468204f4b206f6e6c792e20446f206e6f742075" +
			"736520746f6f6c732e122433393239663737322d376531312d346664622d396661382d30323261616566323863636220" +
			"011a0a0a0867726f6b2d342e3722002a2436363762386131622d383266652d343963622d616431642d63613833386232" +
			"3939636434ea01ae0212ab024d43502077697265207472616e73706f72743a2054686520736f6c65204d435020746f6f" +
			"6c20696e766f6b655f636c69656e745f746f6f6c206163636570747320746f6f6c5f6e616d6520706c75732061726775" +
			"6d656e74735f6a736f6e20737472696e672e2053657269616c697a6520746865206f726967696e616c20636c69656e74" +
			"20617267756d656e7473206173207468617420737472696e672c20696e636c7564696e67206e756c6c2c20626f6f6c65" +
			"616e2c206e65737465642076616c7565732e20446f206e6f742070617373206f726967696e616c20617267756d656e74" +
			"206b6579732061742074686520746f70206c6576656c2e20546f6f6c20726573756c747320617265207265616c20636c" +
			"69656e7420657865637574696f6e20726573756c74732e"
		want, err := hex.DecodeString(golden)
		require.NoError(t, err)

		cursorWithFixedUUID(t,
			"667b8a1b-82fe-49cb-ad1d-ca838b299cd4",
			"3929f772-7e11-4fdb-9fa8-022aaef28ccb")
		body, err := cursorConvertRequest(&apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(
				`"Reply with OK only. Do not use tools."`)}},
		}, "grok-4.7")
		require.NoError(t, err)
		require.Equal(t, want, body, "must reproduce the live 33-agent-run-request.bin byte-for-byte")
	})

	t.Run("63 号多轮样本（500 字节，活体 200，回包 ORCHID）", func(t *testing.T) {
		// 注：f29 system_prompt_spec.append 的文本已改为单入口运输说明（MCP wire transport）。
		// 按当前实现重生成：f29 子消息长度 varint 为 ae 02（302 字节，旧期望 ad 02 少 1 字节），
		// 外层 run_request 长度 varint 为 9e 06（798 字节）；整包仍为 806 字节。
		const golden = "00000003210a9e060a4650010a2081ff990a60f004dce704c6c48d416b63619bc8b5668f5724b6eaa3841eae110f0a20" +
			"bca0f709942ce5c35b912c254c19daf3a0743993730c4ef386f4db35f6aa03cd12760a740a720a485768617420776173" +
			"2074686520636f646520776f72643f205265706c7920776974682074686520636f646520776f7264206f6e6c792e2044" +
			"6f206e6f742075736520746f6f6c732e122463333764366265652d393032652d343835632d623432312d336234383262" +
			"34616335626320011a0a0a0867726f6b2d342e3722002a2466373334373430642d326238662d346337302d626335642d" +
			"6266316435373336656361648a01650a2081ff990a60f004dce704c6c48d416b63619bc8b5668f5724b6eaa3841eae11" +
			"0f12417b22726f6c65223a2275736572222c22636f6e74656e74223a2254686520636f646520776f7264206973204f52" +
			"434849442e2052656d656d6265722069742e227d8a018c010a20bca0f709942ce5c35b912c254c19daf3a0743993730c" +
			"4ef386f4db35f6aa03cd12687b22726f6c65223a22617373697374616e74222c22636f6e74656e74223a5b7b22747970" +
			"65223a2274657874222c2274657874223a22492077696c6c2072656d656d6265722074686520636f646520776f726420" +
			"4f52434849442e227d5d2c226964223a2231227dea01ae0212ab024d43502077697265207472616e73706f72743a2054" +
			"686520736f6c65204d435020746f6f6c20696e766f6b655f636c69656e745f746f6f6c206163636570747320746f6f6c" +
			"5f6e616d6520706c757320617267756d656e74735f6a736f6e20737472696e672e2053657269616c697a652074686520" +
			"6f726967696e616c20636c69656e7420617267756d656e7473206173207468617420737472696e672c20696e636c7564" +
			"696e67206e756c6c2c20626f6f6c65616e2c206e65737465642076616c7565732e20446f206e6f742070617373206f72" +
			"6967696e616c20617267756d656e74206b6579732061742074686520746f70206c6576656c2e20546f6f6c2072657375" +
			"6c747320617265207265616c20636c69656e7420657865637574696f6e20726573756c74732e"
		want, err := hex.DecodeString(golden)
		require.NoError(t, err)

		cursorWithFixedUUID(t,
			"f734740d-2b8f-4c70-bc5d-bf1d5736ecad",
			"c37d6bee-902e-485c-b421-3b482b4ac5bc")
		body, err := cursorConvertRequest(&apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"The code word is ORCHID. Remember it."`)},
				{Role: "assistant", Content: json.RawMessage(`"I will remember the code word ORCHID."`)},
				{Role: "user", Content: json.RawMessage(
					`"What was the code word? Reply with the code word only. Do not use tools."`)},
			},
		}, "grok-4.7")
		require.NoError(t, err)
		require.Equal(t, want, body, "must reproduce the live 63-agent-run-request.bin byte-for-byte")
	})
}

// TestCursorBuildAgentRunReturnsConversationID 覆盖 conversation_id 回传：
// 它是这一轮在上游的唯一标识，供调用方把流内事件（含拒付哨兵）关联回请求。
func TestCursorProtocolRequestReturnsConversationID(t *testing.T) {
	cursorWithFixedUUID(t, "conv-A", "msg-B")
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body, convID, err := cursorBuildAgentRun(req, "grok-4.7", cursorSkipLedger{})
	require.NoError(t, err)
	require.Equal(t, "conv-A", convID)
	run := cursorAgentRunOf(t, body)
	got, ok := cursorFindField(run, cursorFldRunConversationID)
	require.True(t, ok)
	require.Equal(t, convID, got.String())
}

// TestCursorProtocolRequestAssistantTextInlineToolCall 钉死请求侧文本化的标记形态。
//
// 背景（TC-03）：corealgos 经 Cursor 协议出站时，客户端发来的 assistant 历史
// （含 tool_calls）会被文本化后作为 agent turn 文本发给上游。文本化产出的形态与
// 「上游偶发吐在正文里的内联 tool call」形态**同形** → 上游学去这个输出形态 →
// 产出正文 blob 而不是结构化委托帧 → 站点解析不出结构化 tool_calls → 工具不执行。
//
// 本卡（CUR-SVC1）把历史 tool_call 文本化改为单入口运输形状：name 固定
// invoke_client_tool，客户端原工具名进 tool_name，完整参数 JSON 字符串进
// arguments_json（字符串值不做 HTML 转义，保留原字节）。
//
// **核心护栏是「逐字一致」**：上游按字节解析续轮历史，标记或换行动一个字节都会
// 让续轮历史不匹配。故这里的期望值用**码点转义**拼出（源码里没有位置可以藏进
// 零宽字符，字节一旦写错必然对不上）。
const (
	// tc03ProdOpen / tc03ProdClose 是按生产实测码点表重建的标记字节：
	//
	//	开标记 = U+003C + "tool_call" + U+003E
	//	闭标记 = U+003C + U+002F + "tool_call" + U+003E
	tc03ProdOpen  = "\x3ctool_call\x3e"
	tc03ProdClose = "\x3c/tool_call\x3e"
)

// TestCursorProtocolRequestAssistantTextInlineToolCall 覆盖请求侧文本化的四点语义。
func TestCursorProtocolRequestAssistantTextInlineToolCall(t *testing.T) {
	t.Run("逐字一致：单条 tool_call 的输出与预期字面量逐字相等", func(t *testing.T) {
		msg := apicompat.ChatMessage{
			Role:    "assistant",
			Content: json.RawMessage(`"let me check"`),
			ToolCalls: []apicompat.ChatToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: apicompat.ChatFunctionCall{
					Name:      "get_weather",
					Arguments: `{"city":"Beijing"}`,
				},
			}},
		}
		got := cursorAssistantText(msg, "let me check")
		want := "let me check" + "\n" +
			tc03ProdOpen + "\n" +
			`{"id":"call_1","name":"invoke_client_tool","arguments":{"tool_name":"get_weather","arguments_json":"{\"city\":\"Beijing\"}"}}` + "\n" +
			tc03ProdClose
		require.Equal(t, want, got, "request-side history must use the single-entry transport shape")
	})

	t.Run("逐字一致：多条 tool_call 按序拼接", func(t *testing.T) {
		msg := apicompat.ChatMessage{
			Role:    "assistant",
			Content: json.RawMessage(`"y"`),
			ToolCalls: []apicompat.ChatToolCall{
				{ID: "call_a", Type: "function", Function: apicompat.ChatFunctionCall{
					Name: "a", Arguments: `{"k":1}`}},
				{ID: "call_b", Type: "function", Function: apicompat.ChatFunctionCall{
					Name: "b", Arguments: `{}`}},
			},
		}
		got := cursorAssistantText(msg, "y")
		want := "y" + "\n" +
			tc03ProdOpen + "\n" +
			`{"id":"call_a","name":"invoke_client_tool","arguments":{"tool_name":"a","arguments_json":"{\"k\":1}"}}` + "\n" +
			tc03ProdClose + "\n" +
			tc03ProdOpen + "\n" +
			`{"id":"call_b","name":"invoke_client_tool","arguments":{"tool_name":"b","arguments_json":"{}"}}` + "\n" +
			tc03ProdClose
		require.Equal(t, want, got, "each tool_call must render one blob, in order")
	})

	t.Run("空参兜底：arguments 为空补 {}", func(t *testing.T) {
		msg := apicompat.ChatMessage{
			Role:    "assistant",
			Content: json.RawMessage(`"x"`),
			ToolCalls: []apicompat.ChatToolCall{{
				ID:   "call_e",
				Type: "function",
				Function: apicompat.ChatFunctionCall{
					Name:      "noop",
					Arguments: "",
				},
			}},
		}
		got := cursorAssistantText(msg, "x")
		require.Contains(t, got, `"arguments_json":"{}"`, "empty arguments must fall back to {}")
		want := "x" + "\n" +
			tc03ProdOpen + "\n" +
			`{"id":"call_e","name":"invoke_client_tool","arguments":{"tool_name":"noop","arguments_json":"{}"}}` + "\n" +
			tc03ProdClose
		require.Equal(t, want, got)
	})

	t.Run("正文在前：blob 紧跟原 content 之后", func(t *testing.T) {
		msg := apicompat.ChatMessage{
			Role:    "assistant",
			Content: json.RawMessage(`"let me check"`),
			ToolCalls: []apicompat.ChatToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: apicompat.ChatFunctionCall{
					Name:      "get_weather",
					Arguments: `{"city":"Beijing"}`,
				},
			}},
		}
		got := cursorAssistantText(msg, "let me check")
		require.True(t, len(got) > len("let me check"), "output must carry the blob")
		require.Equal(t, "let me check", got[:len("let me check")],
			"original content must come first, unchanged")
		require.Equal(t, "\n"+tc03ProdOpen, got[len("let me check"):len("let me check")+len("\n"+tc03ProdOpen)],
			"blob must follow the content immediately")
		require.Less(t, indexOfSub(got, "let me check"), indexOfSub(got, tc03ProdOpen),
			"content must precede the blob")
	})

	t.Run("无 tool_calls：只输出原 content，不产生 blob（回归）", func(t *testing.T) {
		msg := apicompat.ChatMessage{Role: "assistant", Content: json.RawMessage(`"plain"`)}
		got := cursorAssistantText(msg, "plain")
		require.Equal(t, "plain", got, "no tool_calls means no blob at all")
		require.NotContains(t, got, tc03ProdOpen, "no blob marker must be emitted")
	})

	t.Run("标记与响应侧同源：共享常量即生产实测码点（裸字节）", func(t *testing.T) {
		require.Equal(t, tc03ProdOpen, cursorInlineToolCallOpen,
			"request side must reuse the same open marker as the response side")
		require.Equal(t, tc03ProdClose, cursorInlineToolCallClose,
			"request side must reuse the same close marker as the response side")
		// 字节护栏：标记里不得混入零宽字符（TC-01 的原始缺陷）。
		require.NotContains(t, cursorInlineToolCallOpen, "\u200b",
			"open marker must be bare bytes (no U+200B)")
		require.NotContains(t, cursorInlineToolCallClose, "\u200b",
			"close marker must be bare bytes (no U+200B)")
	})

	t.Run("字符串值不做 HTML 转义：参数里的 < > & 原字节保留", func(t *testing.T) {
		msg := apicompat.ChatMessage{
			Role: "assistant",
			ToolCalls: []apicompat.ChatToolCall{{
				ID: "call_html", Type: "function",
				Function: apicompat.ChatFunctionCall{
					Name: "html_tool", Arguments: `{"x":"<a>&b"}`,
				},
			}},
		}
		got := cursorJSONStringNoEscape(msg.ToolCalls[0].Function.Arguments)
		require.Equal(t, `"{\"x\":\"<a>&b\"}"`, got,
			"no \u003c-style escapes may enter arguments_json")
		require.Contains(t, cursorAssistantText(msg, ""), `<a>&b`)
	})

	t.Run("tool 结果名固定 invoke_client_tool，id/content 不变", func(t *testing.T) {
		msg := apicompat.ChatMessage{
			Role: "tool", ToolCallID: "call_7", Name: "get_weather",
			Content: json.RawMessage(`"sunny"`),
		}
		got, images, err := cursorToolResultText(msg, cursorSkipLedger{})
		require.NoError(t, err)
		require.Empty(t, images)
		require.Equal(t, `<tool_result tool_call_id="call_7" name="invoke_client_tool">`+"\n<content>\n"+
			"sunny"+"\n</tool_result>", got)
		require.NotContains(t, got, "get_weather", "original tool name must not leak into the transport name")
	})
}

// TestCursorMcpToolsTransportSchema 钉死单入口 f3 的形状与 enum 子集口径。
func TestCursorMcpToolsTransportSchema(t *testing.T) {
	tool := func(name, schema string) apicompat.ChatTool {
		return apicompat.ChatTool{
			Type: "function",
			Function: &apicompat.ChatFunction{
				Name: name, Parameters: json.RawMessage(schema),
			},
		}
	}
	newReq := func(choice string, tools ...apicompat.ChatTool) *apicompat.ChatCompletionsRequest {
		return &apicompat.ChatCompletionsRequest{
			Messages:   []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Tools:      tools,
			ToolChoice: json.RawMessage(choice),
		}
	}

	t.Run("全部工具：enum 等于原工具名集合（原名，不改名）", func(t *testing.T) {
		body, err := cursorConvertRequest(newReq(`"auto"`,
			tool("Read", `{}`), tool("get_weather", `{}`), tool("WebFetch", `{}`)), "grok-4.7")
		require.NoError(t, err)
		def := cursorTestSingleMcpDefOf(t, body)
		typ, required, enum, additional, _ := cursorTestTransportSchema(t, def)
		require.Equal(t, "object", typ)
		require.ElementsMatch(t, []string{cursorTransportToolNameKey, cursorTransportArgumentsKey}, required)
		require.False(t, additional)
		require.ElementsMatch(t, []string{"Read", "get_weather", "WebFetch"}, enum,
			"enum must carry the exact original client tool names, no wire renaming")
	})

	t.Run("具名子集：enum 只含被选中的工具", func(t *testing.T) {
		body, err := cursorConvertRequest(newReq(
			`{"type":"function","function":{"name":"get_weather"}}`,
			tool("get_weather", `{}`), tool("get_time", `{}`)), "grok-4.7")
		require.NoError(t, err)
		_, _, enum, _, _ := cursorTestTransportSchema(t, cursorTestSingleMcpDefOf(t, body))
		require.Equal(t, []string{"get_weather"}, enum)
	})

	t.Run("none：f4 为空 message，不声明任何工具", func(t *testing.T) {
		body, err := cursorConvertRequest(newReq(`"none"`, tool("get_weather", `{}`)), "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
		require.True(t, ok)
		require.Empty(t, mcp.Bytes, "tool_choice=none must leave f4 empty")
		require.NotContains(t, string(body), "get_weather")
	})

	t.Run("无工具：f4 为空 message", func(t *testing.T) {
		body, err := cursorConvertRequest(newReq(`"auto"`), "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
		require.True(t, ok)
		require.Empty(t, mcp.Bytes)
	})

	t.Run("运输 schema 的 number/null/数组/大整数只经原 schema 文本，不经 float64", func(t *testing.T) {
		const schema = `{"type":"object","properties":{"n":{"type":"number"},"nil":{"type":"null"},` +
			`"arr":{"type":"array","items":{"type":"integer"}},"big":{"type":"integer","default":9007199254740993}}}`
		body, err := cursorConvertRequest(newReq(`"auto"`, tool("precise", schema)), "grok-4.7")
		require.NoError(t, err)
		def := cursorTestSingleMcpDefOf(t, body)
		desc := cursorTestFieldString(def, cursorFldMcpToolDefDescription)
		require.Contains(t, desc, schema,
			"the original schema text (including big int 9007199254740993) must survive verbatim in the catalog")
		f3 := cursorTestF3SchemaOf(t, def)
		require.NotContains(t, f3, "9007199254740993",
			"f3 constrains only tool_name/arguments_json; it must not carry client schema numbers")
		require.NotContains(t, f3, "float")
	})
}
