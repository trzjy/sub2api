//go:build unit

package service

// §3.1 native resume builder 的字节级定向测试：完整工具历史尾必须恢复成
// resume_action + 真实 user turn 锚点，最后一条真实结果不截断、不改写成新指令。
// 普通 user 尾的黄金字节在本文件里单独钉住，避免"修恢复时顺手改坏普通路径"。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// cursorRebuildToolCall 构造一条 assistant tool_call。
func cursorRebuildToolCall(id, name, args string) apicompat.ChatToolCall {
	return apicompat.ChatToolCall{
		ID: id, Type: "function",
		Function: apicompat.ChatFunctionCall{Name: name, Arguments: args},
	}
}

// cursorRebuildToolMsg 构造一条 tool 结果消息。
func cursorRebuildToolMsg(id, result string) apicompat.ChatMessage {
	return apicompat.ChatMessage{
		Role: "tool", ToolCallID: id, Content: json.RawMessage(`"` + result + `"`),
	}
}

// cursorRebuildDecoded 是 native resume AgentRun 解码后的关键锚点。
type cursorRebuildDecoded struct {
	run       []cursorPBField
	state     []cursorPBField
	rootIDs   [][]byte
	turnIDs   [][]byte
	action    []cursorPBField
	blobs     map[string][]byte
	userText  string
	userMsgID string
}

// cursorDecodeNativeResume 解码 native resume 请求并校验 action/turn/user 三层锚点。
func cursorDecodeNativeResume(t *testing.T, body []byte) cursorRebuildDecoded {
	t.Helper()
	run := cursorAgentRunOf(t, body)

	stateField, ok := cursorFindField(run, cursorFldRunConversationState)
	require.True(t, ok, "run_request must carry conversation_state")
	state, err := cursorDecodeProtobuf(stateField.Bytes)
	require.NoError(t, err)
	rootIDs := make([][]byte, 0)
	for _, f := range cursorAllFields(state, cursorFldStateRootPromptBlobs) {
		rootIDs = append(rootIDs, f.Bytes)
	}
	turnIDs := make([][]byte, 0)
	for _, f := range cursorAllFields(state, cursorFldStateTurns) {
		turnIDs = append(turnIDs, f.Bytes)
	}
	require.Len(t, turnIDs, 1, "native resume must reference exactly one turn blob")

	actionField, ok := cursorFindField(run, cursorFldRunAction)
	require.True(t, ok, "run_request must carry action")
	action, err := cursorDecodeProtobuf(actionField.Bytes)
	require.NoError(t, err)
	_, hasUserAction := cursorFindField(action, cursorFldActionUserMessage)
	require.False(t, hasUserAction, "resume must not carry user_message_action")
	resume, ok := cursorFindField(action, cursorFldActionResumeAction)
	require.True(t, ok, "resume_action (f2) must be present")
	require.Empty(t, resume.Bytes, "resume_action must be an empty message")
	require.Equal(t, cursorFldActionResumeAction, resume.Number)

	blobs := make(map[string][]byte)
	for _, pair := range cursorBlobsOf(t, run) {
		sum := sha256.Sum256(pair[1])
		require.Equal(t, sum[:], pair[0], "blob id must equal sha256(value)")
		blobs[string(pair[0])] = pair[1]
	}
	for _, id := range rootIDs {
		require.Contains(t, blobs, string(id), "every root id must resolve in pre_fetched_blobs")
	}
	require.Contains(t, blobs, string(turnIDs[0]), "turn id must resolve in pre_fetched_blobs")

	turnFields, err := cursorDecodeProtobuf(blobs[string(turnIDs[0])])
	require.NoError(t, err)
	agentTurn, ok := cursorFindField(turnFields, cursorFldTurnAgentConversationTurn)
	require.True(t, ok, "turn blob must carry ConversationTurnStructure.f1")
	agentFields, err := cursorDecodeProtobuf(agentTurn.Bytes)
	require.NoError(t, err)
	userBlobID, ok := cursorFindField(agentFields, cursorFldAgentTurnUserMessageBlob)
	require.True(t, ok, "agent turn must carry user message blob id")
	require.Contains(t, blobs, string(userBlobID.Bytes), "user id must resolve in pre_fetched_blobs")
	userFields, err := cursorDecodeProtobuf(blobs[string(userBlobID.Bytes)])
	require.NoError(t, err)
	text, ok := cursorFindField(userFields, cursorFldUserMsgText)
	require.True(t, ok, "user blob must carry real user text")
	msgID, ok := cursorFindField(userFields, cursorFldUserMsgMessageID)
	require.True(t, ok, "user blob must carry message id")
	mode, ok := cursorFindField(userFields, cursorFldUserMsgMode)
	require.True(t, ok)
	require.EqualValues(t, cursorConversationModeAgent, mode.Varint)

	return cursorRebuildDecoded{
		run: run, state: state, rootIDs: rootIDs, turnIDs: turnIDs,
		action: action, blobs: blobs, userText: text.String(), userMsgID: msgID.String(),
	}
}

// TestCursorNativeRebuildDecodesFullChain 验证单工具完整历史的 native resume 全链：
// action.f2 空 message、无 f1；全量历史（含最后结果）进 root；三层 blob 内容寻址且
// 最后结果 nonce 原样保留。
func TestCursorNativeRebuildDecodesFullChain(t *testing.T) {
	cursorWithFixedUUID(t, "conv-native-1", "msg-native-1")
	const nonce = "nonce-fullchain-7f3a"
	req := &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
		{Role: "user", Content: json.RawMessage(`"weather please"`)},
		{Role: "assistant", ToolCalls: []apicompat.ChatToolCall{
			cursorRebuildToolCall("call_1", "get_weather", `{"city":"hangzhou"}`),
		}},
		cursorRebuildToolMsg("call_1", "sunny, 25C "+nonce),
	}}

	body, convID, err := cursorBuildAgentRun(req, "cursor-wire-model", cursorSkipLedger{})
	require.NoError(t, err)
	require.Equal(t, "conv-native-1", convID, "conversation_id remains the new AgentRun id")

	d := cursorDecodeNativeResume(t, body)
	require.Len(t, d.rootIDs, 3, "user + assistant + final tool result all stay in root")
	require.Equal(t, "weather please", d.userText, "anchor must be the real user, not a rewritten result")
	require.Equal(t, "msg-native-1", d.userMsgID)

	assistantBlob := string(d.blobs[string(d.rootIDs[1])])
	require.Contains(t, assistantBlob, `"toolCallId":"call_1"`, "original call must survive in history")
	require.Contains(t, assistantBlob, `"toolName":"get_weather"`)
	lastBlob := string(d.blobs[string(d.rootIDs[2])])
	require.Contains(t, lastBlob, `"type":"tool-result"`, "final result stays a tool result")
	require.Contains(t, lastBlob, nonce, "final real result nonce must be preserved verbatim")
	require.NotContains(t, string(d.blobs[string(d.turnIDs[0])]), nonce,
		"nonce belongs to the history result, not to the synthetic user turn")
}

// TestCursorNativeRebuildSingleAndMultiToolWithTailInstruction 覆盖单/多工具结果 +
// 尾 instruction：结果保序、最后结果 nonce 保留、instruction 合入真实 user 锚点，
// 且没有任何结果被改写成新的 user_message_action。
func TestCursorNativeRebuildSingleAndMultiToolWithTailInstruction(t *testing.T) {
	cases := []struct {
		name        string
		calls       []string
		results     []string
		instruction string
		wantUser    string
	}{
		{
			name: "single result", calls: []string{"call_1"},
			results: []string{"result-one-N1"}, wantUser: "weather please",
		},
		{
			name: "single result with tail instruction", calls: []string{"call_1"},
			results: []string{"result-one-N2"}, instruction: "TAIL-ONE",
			wantUser: "weather pleaseTAIL-ONE",
		},
		{
			name: "two results with tail instruction", calls: []string{"call_a", "call_b"},
			results: []string{"result-a-N3", "result-b-N4"}, instruction: "TAIL-TWO",
			wantUser: "weather pleaseTAIL-TWO",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cursorWithFixedUUID(t, "conv-seq", "msg-seq")
			msgs := []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"weather please"`)},
			}
			toolCalls := make([]apicompat.ChatToolCall, 0, len(c.calls))
			for _, id := range c.calls {
				toolCalls = append(toolCalls, cursorRebuildToolCall(id, "get_weather",
					`{"city":"city-`+id+`"}`))
			}
			msgs = append(msgs, apicompat.ChatMessage{Role: "assistant", ToolCalls: toolCalls})
			for i, id := range c.calls {
				msgs = append(msgs, cursorRebuildToolMsg(id, c.results[i]))
			}
			if c.instruction != "" {
				msgs = append(msgs, apicompat.ChatMessage{
					Role: "system", Content: json.RawMessage(`"` + c.instruction + `"`),
				})
			}
			body, _, err := cursorBuildAgentRun(&apicompat.ChatCompletionsRequest{Messages: msgs},
				"cursor-wire-model", cursorSkipLedger{})
			require.NoError(t, err)

			d := cursorDecodeNativeResume(t, body)
			require.Len(t, d.rootIDs, 2+len(c.calls), "every tool result is a separate history blob")
			require.Equal(t, c.wantUser, d.userText, "tail instruction merges into the real user anchor")
			for i, id := range c.calls {
				blob := string(d.blobs[string(d.rootIDs[2+i])])
				require.Contains(t, blob, `"toolCallId":"`+id+`"`, "results keep original order")
				require.Contains(t, blob, c.results[i], "each real result nonce survives")
			}
		})
	}
}

// TestCursorProtocolRequestGoldenUserTail 钉住普通 user 尾的逐字节输出：native resume
// 只改 tool 尾，不得改变 33 号活体样本的黄金字节。
func TestCursorProtocolRequestGoldenUserTail(t *testing.T) {
	const golden = "00000000900a8d010a02500112530a510a4f0a255265706c792077697468204f4b206f6e6c792e20446f206e6f742075" +
		"736520746f6f6c732e122433393239663737322d376531312d346664622d396661382d30323261616566323863636220" +
		"011a0a0a0867726f6b2d342e3722002a2436363762386131622d383266652d343963622d616431642d63613833386232" +
		"3939636434"
	want, err := hex.DecodeString(golden)
	require.NoError(t, err)
	cursorWithFixedUUID(t,
		"667b8a1b-82fe-49cb-ad1d-ca838b299cd4",
		"3929f772-7e11-4fdb-9fa8-022aaef28ccb")
	got, err := cursorConvertRequest(&apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(
			`"Reply with OK only. Do not use tools."`)}},
	}, "grok-4.7")
	require.NoError(t, err)
	require.Equal(t, want, got, "plain user tail must stay byte-for-byte unchanged")

	run := cursorAgentRunOf(t, got)
	actionField, ok := cursorFindField(run, cursorFldRunAction)
	require.True(t, ok)
	action, err := cursorDecodeProtobuf(actionField.Bytes)
	require.NoError(t, err)
	_, hasResume := cursorFindField(action, cursorFldActionResumeAction)
	require.False(t, hasResume, "plain user tail must not emit resume_action")
	_, hasUser := cursorFindField(action, cursorFldActionUserMessage)
	require.True(t, hasUser, "plain user tail must emit user_message_action")
	stateField, ok := cursorFindField(run, cursorFldRunConversationState)
	require.True(t, ok)
	state, err := cursorDecodeProtobuf(stateField.Bytes)
	require.NoError(t, err)
	require.Empty(t, cursorAllFields(state, cursorFldStateTurns), "plain user tail must not emit state.f8")
}
