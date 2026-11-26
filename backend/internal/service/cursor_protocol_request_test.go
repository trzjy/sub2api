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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
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

// cursorTestMcpDefsOf 取出 f4 里每条 McpToolDefinition 的字段列表（按声明顺序）。
// 逐工具原生声明：N 个有效工具 → N 条定义，不再拼成单入口。
func cursorTestMcpDefsOf(t *testing.T, body []byte) [][]cursorPBField {
	t.Helper()
	run := cursorAgentRunOf(t, body)
	mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
	require.True(t, ok, "mcp_tools field must be present")
	mcpFields, err := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, err)
	defs := cursorAllFields(mcpFields, cursorFldMcpToolsTool)
	out := make([][]cursorPBField, 0, len(defs))
	for _, d := range defs {
		fields, err := cursorDecodeProtobuf(d.Bytes)
		require.NoError(t, err)
		out = append(out, fields)
	}
	return out
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

	t.Run("尾部 instruction 合入最近真实 user（不新建 action）", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"q1"`)},
				{Role: "assistant", Content: json.RawMessage(`"a"`)},
				{Role: "user", Content: json.RawMessage(`"q2"`)},
				{Role: "system", Content: json.RawMessage(`"TAIL"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		// TAIL 合入最近的真实 user（q2），不是新建的 user action。
		require.Equal(t, "q2TAIL", cursorActionTextOf(t, run))
		blobs := cursorBlobsOf(t, run)
		require.Len(t, blobs, 2, "instruction 不得成为独立的历史/action 轮")
		require.Contains(t, string(blobs[0][1]), "q1")
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

// TestCursorProtocolRequestDropsOutOfScopeFields 覆盖范围外字段的处理（工具走逐工具声明，
// tool 结果走 role=tool 的真实历史结构，f29 教学提示已删除）。
func TestCursorProtocolRequestDropsOutOfScopeFields(t *testing.T) {
	t.Run("请求侧 tools 编码为逐工具原生定义", func(t *testing.T) {
		const schema = `{"type":"object","properties":{"city":{"type":"string"}}}`
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)},
				{Role: "user", Content: json.RawMessage(`"again"`)}},
			Tools: []apicompat.ChatTool{{
				Type: "function",
				Function: &apicompat.ChatFunction{
					Name:        "get_weather",
					Description: "Get the weather.",
					Parameters:  json.RawMessage(schema),
				},
			}},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		defs := cursorTestMcpDefsOf(t, body)
		require.Len(t, defs, 1)
		def := defs[0]
		require.Equal(t, "get_weather", cursorTestFieldString(def, cursorFldMcpToolDefName))
		require.Equal(t, "get_weather", cursorTestFieldString(def, cursorFldMcpToolDefToolName))
		require.Equal(t, "Get the weather.", cursorTestFieldString(def, cursorFldMcpToolDefDescription))
		require.Equal(t, cursorMcpProviderIdentifier, cursorTestFieldString(def, cursorFldMcpToolDefProvider))
		require.Equal(t, schema, cursorTestF6SchemaOf(t, def))
		_, hasF29 := cursorFindField(cursorAgentRunOf(t, body), 29)
		require.False(t, hasF29, "f29 teaching prompt must be gone")
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

	t.Run("tool 结果消息 → role=tool 的历史 blob", func(t *testing.T) {
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
		blobs := cursorBlobsOf(t, run)
		require.Len(t, blobs, 3)
		require.JSONEq(t,
			`{"role":"tool","content":[{"type":"tool-result","toolCallId":"call_1","toolName":"CallDynamicTool","result":"sunny","experimental_content":[{"type":"text","text":"sunny"}]}],"id":"call_1","providerOptions":{"cursor":{"highLevelToolCallResult":{"output":{"success":{"content":[{"text":{"text":"sunny"}}]}},"isError":false}}}}`,
			string(blobs[1][1]))
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

func TestCursorHistoryPreservesAnthropicToolResultError(t *testing.T) {
	request := &apicompat.AnthropicRequest{
		Model: "cursor-test-model",
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"question"`)},
			{Role: "assistant", Content: json.RawMessage(`[{
				"type":"tool_use","id":"call_error","name":"lookup","input":{}
			}]`)},
			{Role: "user", Content: json.RawMessage(`[{
				"type":"tool_result","tool_use_id":"call_error","is_error":true,"content":"denied"
			}]`)},
			{Role: "user", Content: json.RawMessage(`"continue"`)},
		},
	}
	converted, err := apicompat.AnthropicToChatCompletionsRequest(request)
	require.NoError(t, err)
	turns, err := cursorConvertMessages(converted.Messages, cursorSkipLedger{})
	require.NoError(t, err)
	blobs, err := cursorHistoryBlobs(turns[:3], cursorSkipLedger{})
	require.NoError(t, err)
	require.Len(t, blobs, 3)
	require.JSONEq(t, `{"role":"tool","content":[{"type":"tool-result","toolCallId":"call_error","toolName":"CallDynamicTool","result":"denied","experimental_content":[{"type":"text","text":"denied"}]}],"id":"call_error","providerOptions":{"cursor":{"highLevelToolCallResult":{"output":{"success":{"content":[{"text":{"text":"denied"}}],"isError":true}},"isError":true}}}}`, string(blobs[2].value))
}

func TestCursorHistoryPreservesGeminiFunctionResponseObject(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"question"}]},
		{"role":"model","parts":[{"functionCall":{"id":"call_error","name":"lookup","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"call_error","name":"lookup","response":{"error":{"code":7,"message":"denied"}}}}]},
		{"role":"user","parts":[{"text":"continue"}]}
	]}`)
	converted, err := cursorGeminiRequestToChatCompletions(body, "cursor-test-model", false)
	require.NoError(t, err)
	var request apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal(converted, &request))
	turns, err := cursorConvertMessages(request.Messages, cursorSkipLedger{})
	require.NoError(t, err)
	blobs, err := cursorHistoryBlobs(turns[:3], cursorSkipLedger{})
	require.NoError(t, err)
	require.Len(t, blobs, 3)
	require.JSONEq(t, `{"role":"tool","content":[{"type":"tool-result","toolCallId":"call_error","toolName":"CallDynamicTool","result":"{\"error\":{\"code\":7,\"message\":\"denied\"}}","experimental_content":[{"type":"text","text":"{\"error\":{\"code\":7,\"message\":\"denied\"}}"}]}],"id":"call_error","providerOptions":{"cursor":{"highLevelToolCallResult":{"output":{"success":{"content":[{"text":{"text":"{\"error\":{\"code\":7,\"message\":\"denied\"}}"}}]}},"isError":false}}}}`, string(blobs[2].value))
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
		// f29 system_prompt_spec 教学提示已删除，整包回到活体 149 字节（外层长度 varint 90，
		// run_request 长度 varint 8d 01）。比较方式仍为逐字节相等。
		const golden = "00000000900a8d010a02500112530a510a4f0a255265706c792077697468204f4b206f6e6c792e20446f206e6f742075" +
			"736520746f6f6c732e122433393239663737322d376531312d346664622d396661382d30323261616566323863636220" +
			"011a0a0a0867726f6b2d342e3722002a2436363762386131622d383266652d343963622d616431642d63613833386232" +
			"3939636434"
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
		// f29 教学提示已删除，整包回到活体 500 字节（外层长度 varint 1 ef，
		// run_request 长度 varint ec 03）。比较方式仍为逐字节相等。
		const golden = "00000001ef0aec030a4650010a2081ff990a60f004dce704c6c48d416b63619bc8b5668f5724b6eaa3841eae110f0a20" +
			"bca0f709942ce5c35b912c254c19daf3a0743993730c4ef386f4db35f6aa03cd12760a740a720a485768617420776173" +
			"2074686520636f646520776f72643f205265706c7920776974682074686520636f646520776f7264206f6e6c792e2044" +
			"6f206e6f742075736520746f6f6c732e122463333764366265652d393032652d343835632d623432312d336234383262" +
			"34616335626320011a0a0a0867726f6b2d342e3722002a2466373334373430642d326238662d346337302d626335642d" +
			"6266316435373336656361648a01650a2081ff990a60f004dce704c6c48d416b63619bc8b5668f5724b6eaa3841eae11" +
			"0f12417b22726f6c65223a2275736572222c22636f6e74656e74223a2254686520636f646520776f7264206973204f52" +
			"434849442e2052656d656d6265722069742e227d8a018c010a20bca0f709942ce5c35b912c254c19daf3a0743993730c" +
			"4ef386f4db35f6aa03cd12687b22726f6c65223a22617373697374616e74222c22636f6e74656e74223a5b7b22747970" +
			"65223a2274657874222c2274657874223a22492077696c6c2072656d656d6265722074686520636f646520776f726420" +
			"4f52434849442e227d5d2c226964223a2231227d"
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

// TestCursorProtocolRequestHistoryToolStructure 钉死历史工具内容的真实结构：
// assistant 写 tool-call（arguments 原 JSON 对象、namespace=provider），tool 写
// tool-result（含 id 与 providerOptions），不再有任何包装标记或字符串化参数。
func TestCursorProtocolRequestHistoryToolStructure(t *testing.T) {
	t.Run("tool-call 的 arguments 是对象且空/null 归一为 {}", func(t *testing.T) {
		turns, err := cursorConvertMessages([]apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "assistant", ToolCalls: []apicompat.ChatToolCall{
				{ID: "call_html", Type: "function", Function: apicompat.ChatFunctionCall{
					Name: "html_tool", Arguments: `{"x":"<a>&b"}`}}}},
		}, cursorSkipLedger{})
		require.NoError(t, err)
		require.Len(t, turns, 2)
		require.Len(t, turns[1].toolCalls, 1)
		require.JSONEq(t, `{"x":"<a>&b"}`, string(turns[1].toolCalls[0].args))

		blobs, err := cursorHistoryBlobs([]cursorAgentTurn{turns[1]}, cursorSkipLedger{})
		require.NoError(t, err)
		require.Len(t, blobs, 1)
		require.JSONEq(t,
			`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"call_html","toolName":"CallDynamicTool","args":{"arguments":{"x":"<a>&b"},"namespace":"sub2api","toolName":"html_tool"}}],"id":"1"}`,
			string(blobs[0].value), "< > & 必须以字面量保留，不得出现 \\u003c")

		for _, empty := range []string{"", "null", "  "} {
			turns, err := cursorConvertMessages([]apicompat.ChatMessage{{
				Role: "assistant", ToolCalls: []apicompat.ChatToolCall{{
					ID: "c", Function: apicompat.ChatFunctionCall{Name: "noop", Arguments: empty},
				}},
			}}, cursorSkipLedger{})
			require.NoError(t, err)
			require.JSONEq(t, `{}`, string(turns[0].toolCalls[0].args))
		}
	})

	t.Run("文本为空的 assistant 仍有工具调用内容项", func(t *testing.T) {
		blobs, err := cursorHistoryBlobs([]cursorAgentTurn{{
			role: "assistant", toolCalls: []cursorAgentToolCall{{callID: "c", name: "n", args: json.RawMessage(`{}`)}},
		}}, cursorSkipLedger{})
		require.NoError(t, err)
		require.Len(t, blobs, 1)
		require.JSONEq(t,
			`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"c","toolName":"CallDynamicTool","args":{"arguments":{},"namespace":"sub2api","toolName":"n"}}],"id":"1"}`,
			string(blobs[0].value))
		require.NotContains(t, string(blobs[0].value), "tool_call")
	})

	t.Run("legacy function 无 ID 保留记账且不自动配对", func(t *testing.T) {
		skipped := cursorSkipLedger{}
		turns, err := cursorConvertMessages([]apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "function", Content: json.RawMessage(`"sunny"`)},
		}, skipped)
		require.NoError(t, err)
		require.Equal(t, 1, skipped[cursorSkipLegacyFunctionNoID])
		require.Len(t, turns, 2)
		require.Equal(t, "tool", turns[1].role)
		require.Equal(t, "", turns[1].toolResults[0].callID)
	})
}

// TestCursorMcpToolsNativeDeclaration 钉死逐工具原生声明：N 个工具 → N 条定义，
// 每条 f1/f2/f4/f5/f6 与原件逐字段一致，无 f3，无 f29 教学提示。
func TestCursorMcpToolsNativeDeclaration(t *testing.T) {
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

	t.Run("全部工具：逐条原名与原 schema", func(t *testing.T) {
		body, err := cursorConvertRequest(newReq(`"auto"`,
			tool("Read", `{"a":1}`), tool("get_weather", `{}`), tool("WebFetch", `{"b":null}`)), "grok-4.7")
		require.NoError(t, err)
		defs := cursorTestMcpDefsOf(t, body)
		require.Len(t, defs, 3, "N tools must produce N definitions")
		want := []struct{ name, schema string }{
			{"Read", `{"a":1}`}, {"get_weather", `{}`}, {"WebFetch", `{"b":null}`},
		}
		for i, def := range defs {
			require.Equal(t, want[i].name, cursorTestFieldString(def, cursorFldMcpToolDefName))
			require.Equal(t, want[i].name, cursorTestFieldString(def, cursorFldMcpToolDefToolName))
			require.Equal(t, cursorMcpProviderIdentifier, cursorTestFieldString(def, cursorFldMcpToolDefProvider))
			require.Equal(t, want[i].schema, cursorTestF6SchemaOf(t, def))
		}
	})

	t.Run("具名子集：只声明被选中的工具", func(t *testing.T) {
		body, err := cursorConvertRequest(newReq(
			`{"type":"function","function":{"name":"get_weather"}}`,
			tool("get_weather", `{}`), tool("get_time", `{}`)), "grok-4.7")
		require.NoError(t, err)
		defs := cursorTestMcpDefsOf(t, body)
		require.Len(t, defs, 1)
		require.Equal(t, "get_weather", cursorTestFieldString(defs[0], cursorFldMcpToolDefName))
	})

	t.Run("none / 无工具：f4 为空 message", func(t *testing.T) {
		for _, body := range [][]byte{
			mustConvert(t, newReq(`"none"`, tool("get_weather", `{}`))),
			mustConvert(t, newReq(`"auto"`)),
		} {
			run := cursorAgentRunOf(t, body)
			mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
			require.True(t, ok)
			require.Empty(t, mcp.Bytes)
		}
	})

	t.Run("schema 的 number/null/数组/大整数走原文本，不经 float64", func(t *testing.T) {
		const schema = `{"type":"object","properties":{"n":{"type":"number"},"nil":{"type":"null"},` +
			`"arr":{"type":"array","items":{"type":"integer"}},"big":{"type":"integer","default":9007199254740993}}}`
		body, err := cursorConvertRequest(newReq(`"auto"`, tool("precise", schema)), "grok-4.7")
		require.NoError(t, err)
		defs := cursorTestMcpDefsOf(t, body)
		require.Len(t, defs, 1)
		require.Equal(t, schema, cursorTestF6SchemaOf(t, defs[0]),
			"the original schema bytes (including big int) must survive verbatim in f6")
		require.NotContains(t, string(body), "float")
	})
}

func mustConvert(t *testing.T, req *apicompat.ChatCompletionsRequest) []byte {
	t.Helper()
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)
	return body
}

// cursorTextMsg 构造一条纯文本消息（role 由调用方给定）。
func cursorTextMsg(role, text string) apicompat.ChatMessage {
	return apicompat.ChatMessage{Role: role, Content: json.RawMessage(`"` + text + `"`)}
}

// TestCursorProtocolRequestInstructionPositions 覆盖请求期 instruction 的首/中/尾
// 三种位置：一律合入最近真实 user 的文本，永不生成独立 user action。
func TestCursorProtocolRequestInstructionPositions(t *testing.T) {
	t.Run("首 instruction 合入首条真实 user", func(t *testing.T) {
		turns, err := cursorConvertMessages([]apicompat.ChatMessage{
			cursorTextMsg("system", "HEAD"),
			cursorTextMsg("user", "q"),
		}, cursorSkipLedger{})
		require.NoError(t, err)
		require.Len(t, turns, 1, "instruction 不得成为独立轮次")
		require.Equal(t, "user", turns[0].role)
		require.Equal(t, "HEADq", turns[0].text)
	})

	t.Run("中 instruction 合入其后最近真实 user", func(t *testing.T) {
		turns, err := cursorConvertMessages([]apicompat.ChatMessage{
			cursorTextMsg("user", "q1"),
			cursorTextMsg("developer", "MID"),
			cursorTextMsg("assistant", "a"),
			cursorTextMsg("user", "q2"),
		}, cursorSkipLedger{})
		require.NoError(t, err)
		require.Len(t, turns, 3, "instruction 永不生成 user action")
		require.Equal(t, "q1", turns[0].text)
		require.Equal(t, "a", turns[1].text)
		require.Equal(t, "MIDq2", turns[2].text)
	})

	t.Run("尾 instruction 合入最近真实 user", func(t *testing.T) {
		turns, err := cursorConvertMessages([]apicompat.ChatMessage{
			cursorTextMsg("user", "q1"),
			cursorTextMsg("assistant", "a"),
			cursorTextMsg("user", "q2"),
			cursorTextMsg("system", "TAIL"),
		}, cursorSkipLedger{})
		require.NoError(t, err)
		require.Len(t, turns, 3, "尾部 instruction 不得新建 user action")
		require.Equal(t, "q2TAIL", turns[2].text)
	})
}

// TestCursorProtocolRequestInstructionAfterToolResult 覆盖 [tool_result, instruction]：
// instruction 只合入随后的真实 user；前置 assistant tool-call 与 tool result 的
// call ID / 内容 / 错误标志 / 顺序全部保留到历史 blob。
func TestCursorProtocolRequestInstructionAfterToolResult(t *testing.T) {
	turns, err := cursorConvertMessages([]apicompat.ChatMessage{
		cursorTextMsg("user", "q"),
		{Role: "assistant", ToolCalls: []apicompat.ChatToolCall{
			{ID: "call_1", Type: "function", Function: apicompat.ChatFunctionCall{
				Name: "lookup", Arguments: `{"k":1}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Name: "lookup",
			Content: json.RawMessage(`"denied"`), IsError: true},
		cursorTextMsg("system", "TAIL"),
		cursorTextMsg("user", "next"),
	}, cursorSkipLedger{})
	require.NoError(t, err)

	// 角色与顺序：user, assistant(tool-call), tool(tool-result), user；
	// 中间的 system instruction 不生成任何轮次。
	require.Len(t, turns, 4)
	require.Equal(t, "user", turns[0].role)
	require.Equal(t, "assistant", turns[1].role)
	require.Equal(t, "tool", turns[2].role)
	require.Equal(t, "user", turns[3].role)
	require.Equal(t, "TAILnext", turns[3].text)

	// assistant tool-call：call ID / 名称 / 原始参数对象保真。
	require.Len(t, turns[1].toolCalls, 1)
	require.Equal(t, "call_1", turns[1].toolCalls[0].callID)
	require.Equal(t, "lookup", turns[1].toolCalls[0].name)
	require.JSONEq(t, `{"k":1}`, string(turns[1].toolCalls[0].args))

	// tool result：call ID / 结果文本 / isError 保真。
	require.Len(t, turns[2].toolResults, 1)
	require.Equal(t, "call_1", turns[2].toolResults[0].callID)
	require.Equal(t, "denied", turns[2].toolResults[0].text)
	require.True(t, turns[2].toolResults[0].isError)

	// 落到真实历史 blob 形状后仍然保真，错误标志进入 providerOptions。
	blobs, err := cursorHistoryBlobs(turns[1:3], cursorSkipLedger{})
	require.NoError(t, err)
	require.Len(t, blobs, 2)
	require.Contains(t, string(blobs[0].value), `"toolCallId":"call_1"`)
	require.Contains(t, string(blobs[0].value), `"toolName":"lookup"`)
	require.Contains(t, string(blobs[1].value), `"toolCallId":"call_1"`)
	require.Contains(t, string(blobs[1].value), `"result":"denied"`)
	require.Contains(t, string(blobs[1].value), `"isError":true`)
}

// TestCursorProtocolRequestAssistantToolCallsKeepOrder 覆盖 assistant 文本 +
// 多条 tool-call 的保序落点（text 在前，调用按原顺序跟随）。
func TestCursorProtocolRequestAssistantToolCallsKeepOrder(t *testing.T) {
	turns, err := cursorConvertMessages([]apicompat.ChatMessage{
		cursorTextMsg("user", "q"),
		{Role: "assistant", Content: json.RawMessage(`"thinking"`), ToolCalls: []apicompat.ChatToolCall{
			{ID: "c2", Type: "function", Function: apicompat.ChatFunctionCall{Name: "B", Arguments: `{"b":2}`}},
			{ID: "c1", Type: "function", Function: apicompat.ChatFunctionCall{Name: "A", Arguments: `{"a":1}`}},
		}},
	}, cursorSkipLedger{})
	require.NoError(t, err)
	require.Len(t, turns, 2)
	require.Equal(t, "thinking", turns[1].text)
	require.Len(t, turns[1].toolCalls, 2)
	require.Equal(t, "c2", turns[1].toolCalls[0].callID)
	require.Equal(t, "c1", turns[1].toolCalls[1].callID)
	require.Equal(t, "B", turns[1].toolCalls[0].name)
	require.Equal(t, "A", turns[1].toolCalls[1].name)

	blobs, err := cursorHistoryBlobs(turns[1:], cursorSkipLedger{})
	require.NoError(t, err)
	require.Len(t, blobs, 1)
	got := string(blobs[0].value)
	require.Contains(t, got, `[{"type":"text","text":"thinking"},{"type":"tool-call","toolCallId":"c2"`)
	require.Less(t, indexOfSub(got, `"c2"`), indexOfSub(got, `"c1"`), "tool-call 顺序必须保留")
}

// TestCursorProtocolRequestInstructionWithoutRealUserRejected 覆盖"没有真实 user"：
// instruction 永不生成 user action，因此这类入站由既有 CUR-REQ-06 拒绝。
func TestCursorProtocolRequestInstructionWithoutRealUserRejected(t *testing.T) {
	cases := map[string]*apicompat.ChatCompletionsRequest{
		"只有 instruction": {Messages: []apicompat.ChatMessage{
			cursorTextMsg("system", "SYS"),
		}},
		"instruction + assistant": {Messages: []apicompat.ChatMessage{
			cursorTextMsg("system", "SYS"),
			cursorTextMsg("assistant", "a"),
		}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := cursorConvertRequest(req, "grok-4.7")
			assertCursorRequestError(t, err, infraerrors.CursorReqNoContent, http.StatusBadRequest)
		})
	}
}

// TestCursorProtocolRequestInstructionRoleIsInstruction 钉死 role="instruction"
// 经唯一分类 helper 得到 instruction 规范角色：不进 unknown、不被丢弃。
func TestCursorProtocolRequestInstructionRoleIsInstruction(t *testing.T) {
	require.Equal(t, cursorSemanticInstruction, cursorClassifySemanticRole("instruction"))
	require.Equal(t, cursorSemanticInstruction, cursorClassifySemanticRole(" Instruction "))
	require.Equal(t, cursorSemanticInstruction, cursorClassifySemanticRole("SYSTEM"))
	require.Equal(t, cursorSemanticInstruction, cursorClassifySemanticRole("developer"))
	require.Equal(t, cursorSemanticUnknown, cursorClassifySemanticRole("wizard"))

	sem := cursorDecodeSemanticMessages([]apicompat.ChatMessage{
		cursorTextMsg("instruction", "RULE"),
		cursorTextMsg("user", "q"),
	})
	require.Len(t, sem, 2)
	require.Equal(t, cursorSemanticInstruction, sem[0].role)
	require.Equal(t, cursorSemanticUser, sem[1].role)

	skipped := cursorSkipLedger{}
	body, _, err := cursorBuildAgentRunFromSemantic(sem, "grok-4.7", nil, nil, skipped)
	require.NoError(t, err)
	text := cursorActionTextOf(t, cursorAgentRunOf(t, body))
	require.Contains(t, text, "RULE")
	require.Contains(t, text, "q")
	require.Zero(t, skipped[cursorSkipUnknownRole], "instruction 不得进 unknown 账本")
}

// TestCursorProtocolRequestResultContainerMarkerFromDecodeInput 证明源容器完整性标记
// 取自解码输入本身，而不是对展开后的消息做尾部扫描/结果块扁平化猜出来的。
func TestCursorProtocolRequestResultContainerMarkerFromDecodeInput(t *testing.T) {
	// Chat 单条 tool/function 消息是完整结果容器。
	toolSem := cursorDecodeSemanticMessages([]apicompat.ChatMessage{
		{Role: "tool", ToolCallID: "call_1", Name: "lookup", Content: json.RawMessage(`"sunny"`)},
	})
	require.Len(t, toolSem, 1)
	require.Equal(t, cursorSemanticTool, toolSem[0].role)
	require.Equal(t, cursorContainerAllToolResults, toolSem[0].container)
	require.Equal(t, "call_1", toolSem[0].raw.ToolCallID)

	// 含普通正文的是混合容器。
	userSem := cursorDecodeSemanticMessages([]apicompat.ChatMessage{cursorTextMsg("user", "hi")})
	require.Equal(t, cursorContainerMixed, userSem[0].container)

	// 文本为空、尾部是 tool_calls 的 assistant 仍是混合容器：不能因为它是最后一条、
	// 或内容形似结果块，就反推成纯结果容器（这正是"尾部扫描猜测"要防的错）。
	assistantSem := cursorDecodeSemanticMessages([]apicompat.ChatMessage{{
		Role: "assistant",
		ToolCalls: []apicompat.ChatToolCall{{
			ID: "c1", Type: "function",
			Function: apicompat.ChatFunctionCall{Name: "lookup", Arguments: `{}`},
		}},
	}})
	require.Equal(t, cursorSemanticAssistant, assistantSem[0].role)
	require.Equal(t, cursorContainerMixed, assistantSem[0].container)

	// 标记按消息逐条取自输入，不因上下文（后面跟了 user）而改变。
	seq := cursorDecodeSemanticMessages([]apicompat.ChatMessage{
		{Role: "tool", ToolCallID: "call_2", Name: "lookup", Content: json.RawMessage(`"x"`)},
		cursorTextMsg("user", "next"),
	})
	require.Equal(t, cursorContainerAllToolResults, seq[0].container)
	require.Equal(t, cursorContainerMixed, seq[1].container)
}

// TestCursorProtocolRequestSemanticSourceContainerIndex 证明 Chat 解码时每条语义消息的
// sourceContainerIndex 等于其在原始输入 msgs 中的下标，与语义切片下标是两个独立字段；
// 且既有的 role/container 标记不受影响。首项显式断言 0，防止零值默认掩盖"未赋值"。
func TestCursorProtocolRequestSemanticSourceContainerIndex(t *testing.T) {
	msgs := []apicompat.ChatMessage{
		cursorTextMsg("system", "HEAD"),
		cursorTextMsg("user", "q"),
		{Role: "tool", ToolCallID: "call_1", Name: "lookup", Content: json.RawMessage(`"sunny"`)},
		cursorTextMsg("assistant", "a"),
	}

	sem := cursorDecodeSemanticMessages(msgs)
	require.Len(t, sem, len(msgs))

	// 首项必须显式为 0：若解码时忘记赋值，零值默认也会"碰巧"通过，故单独钉死。
	require.Equal(t, 0, sem[0].sourceContainerIndex)
	require.Equal(t, cursorSemanticInstruction, sem[0].role)
	require.Equal(t, cursorContainerMixed, sem[0].container)

	// 逐条等于原始输入下标（含后续递增值与 tool 消息）。
	for i := range msgs {
		require.Equal(t, i, sem[i].sourceContainerIndex, "第 %d 条的源容器索引应等于原始输入下标", i)
	}

	require.Equal(t, cursorSemanticUser, sem[1].role)
	require.Equal(t, cursorContainerMixed, sem[1].container)

	// tool 消息：索引等于原始下标 2，且保留 tool 角色与纯结果容器标记、原始 call ID。
	require.Equal(t, 2, sem[2].sourceContainerIndex)
	require.Equal(t, cursorSemanticTool, sem[2].role)
	require.Equal(t, cursorContainerAllToolResults, sem[2].container)
	require.Equal(t, "call_1", sem[2].raw.ToolCallID)

	require.Equal(t, 3, sem[3].sourceContainerIndex)
	require.Equal(t, cursorSemanticAssistant, sem[3].role)
	require.Equal(t, cursorContainerMixed, sem[3].container)
}

// TestCursorProtocolRequestSemanticBuilderCoversAll 用同一语义 builder 覆盖
// 首/中/尾 instruction、无真实 user、工具调用保序与 tool-result 的 ID/内容/isError。
func TestCursorProtocolRequestSemanticBuilderCoversAll(t *testing.T) {
	sem := []cursorSemanticMessage{
		{role: cursorSemanticInstruction, raw: cursorTextMsg("system", "HEAD")},
		{role: cursorSemanticUser, raw: cursorTextMsg("user", "q1")},
		{role: cursorSemanticAssistant, raw: apicompat.ChatMessage{
			Role: "assistant",
			ToolCalls: []apicompat.ChatToolCall{
				{ID: "call_z", Type: "function", Function: apicompat.ChatFunctionCall{Name: "B", Arguments: `{"b":2}`}},
				{ID: "call_a", Type: "function", Function: apicompat.ChatFunctionCall{Name: "A", Arguments: `{"a":1}`}},
			},
		}},
		{role: cursorSemanticTool, container: cursorContainerAllToolResults, raw: apicompat.ChatMessage{
			Role: "tool", ToolCallID: "call_z", Name: "B", Content: json.RawMessage(`"z-res"`), IsError: true,
		}},
		{role: cursorSemanticTool, container: cursorContainerAllToolResults, raw: apicompat.ChatMessage{
			Role: "tool", ToolCallID: "call_a", Name: "A", Content: json.RawMessage(`"a-res"`),
		}},
		{role: cursorSemanticInstruction, raw: cursorTextMsg("developer", "MID")},
		{role: cursorSemanticUser, raw: cursorTextMsg("user", "q2")},
		{role: cursorSemanticInstruction, raw: cursorTextMsg("instruction", "TAIL")},
	}

	body, _, err := cursorBuildAgentRunFromSemantic(sem, "grok-4.7", nil, nil, cursorSkipLedger{})
	require.NoError(t, err)
	run := cursorAgentRunOf(t, body)
	require.Equal(t, "MIDq2TAIL", cursorActionTextOf(t, run),
		"尾 instruction 合入最近真实 user，不生成 user action")

	// instruction 不生成任何独立轮次：user, assistant, tool, tool 共 4 条历史。
	blobs := cursorBlobsOf(t, run)
	require.Len(t, blobs, 4)
	require.Contains(t, string(blobs[0][1]), "HEADq1", "首 instruction 合入首条真实 user")

	assistantBlob := string(blobs[1][1])
	require.Less(t, indexOfSub(assistantBlob, `"call_z"`), indexOfSub(assistantBlob, `"call_a"`),
		"assistant tool-call 必须保序")
	require.Contains(t, string(blobs[2][1]), `"toolCallId":"call_z"`)
	require.Contains(t, string(blobs[2][1]), `"result":"z-res"`)
	require.Contains(t, string(blobs[2][1]), `"isError":true`)
	require.Contains(t, string(blobs[3][1]), `"toolCallId":"call_a"`)
	require.Contains(t, string(blobs[3][1]), `"result":"a-res"`)
	require.NotContains(t, string(blobs[3][1]), `"isError":true`)

	t.Run("没有真实 user 时失败关闭", func(t *testing.T) {
		_, _, err := cursorBuildAgentRunFromSemantic([]cursorSemanticMessage{
			{role: cursorSemanticInstruction, raw: cursorTextMsg("instruction", "ONLY")},
		}, "grok-4.7", nil, nil, cursorSkipLedger{})
		assertCursorRequestError(t, err, infraerrors.CursorReqNoContent, http.StatusBadRequest)
	})
}
