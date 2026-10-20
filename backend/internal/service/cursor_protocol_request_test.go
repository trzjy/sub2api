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
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
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
	require.Equal(t, uint64(cursorConversationModeChat), mode.Varint)
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

// TestCursorConvertRequestDropsOutOfScopeFields 覆盖首版范围外字段被丢弃。
func TestCursorProtocolRequestDropsOutOfScopeFields(t *testing.T) {
	t.Run("请求侧 tools 被丢弃", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)},
				{Role: "user", Content: json.RawMessage(`"again"`)}},
			Tools: []apicompat.ChatTool{{
				Type:     "function",
				Function: &apicompat.ChatFunction{Name: "get_weather"},
			}},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		// 不得把工具名写进任何字段（mcp_tools 保持空 message）。
		require.NotContains(t, string(body), "get_weather", "tools must be dropped, not translated")
	})

	t.Run("tool 结果消息被丢弃而非报错", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"hi"`)},
				{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"result"`)},
				{Role: "assistant", Content: json.RawMessage(`"done"`)},
				{Role: "user", Content: json.RawMessage(`"next"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		require.Equal(t, "next", cursorActionTextOf(t, run))
		require.Len(t, cursorBlobsOf(t, run), 2)
		require.NotContains(t, string(body), `"result"`)
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
		const golden = "00000000900a8d010a02500212530a510a4f0a255265706c792077697468204f4b206f6e6c792e20446f206e6f742075" +
			"736520746f6f6c732e122433393239663737322d376531312d346664622d396661382d30323261616566323863636220" +
			"021a0a0a0867726f6b2d342e3722002a2436363762386131622d383266652d343963622d616431642d63613833386232" +
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
		const golden = "00000001ef0aec030a4650020a2081ff990a60f004dce704c6c48d416b63619bc8b5668f5724b6eaa3841eae110f0a20" +
			"bca0f709942ce5c35b912c254c19daf3a0743993730c4ef386f4db35f6aa03cd12760a740a720a485768617420776173" +
			"2074686520636f646520776f72643f205265706c7920776974682074686520636f646520776f7264206f6e6c792e2044" +
			"6f206e6f742075736520746f6f6c732e122463333764366265652d393032652d343835632d623432312d336234383262" +
			"34616335626320021a0a0a0867726f6b2d342e3722002a2466373334373430642d326238662d346337302d626335642d" +
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
	body, convID, err := cursorBuildAgentRun(req, "grok-4.7")
	require.NoError(t, err)
	require.Equal(t, "conv-A", convID)
	run := cursorAgentRunOf(t, body)
	got, ok := cursorFindField(run, cursorFldRunConversationID)
	require.True(t, ok)
	require.Equal(t, convID, got.String())
}
