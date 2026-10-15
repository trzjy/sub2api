package apicompat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The kimi accounts are fronted by a self-hosted vLLM (served as "kimi-k3"),
// whose chat endpoint rejects any system message that is not at the head of
// the conversation with 400 "System message must be at the beginning.". Codex
// injects a developer notice in the middle of tool execution ("Approved
// command prefix saved"), which used to be appended in place and stranded the
// system message between an assistant tool_calls message and its tool reply.
//
// Every system/developer message in input[] must therefore end up merged into
// the leading system message, with every other message keeping its relative
// order.

// chatRoleTrace renders the converted conversation as a compact trace so a test
// can assert the exact wire order ("system", "user", "assistant[tool:A]",
// "tool[A]").
func chatRoleTrace(messages []ChatMessage) []string {
	trace := make([]string, 0, len(messages))
	for _, m := range messages {
		entry := m.Role
		if len(m.ToolCalls) > 0 {
			ids := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				ids = append(ids, tc.ID)
			}
			entry += fmt.Sprintf("[tool:%s]", strings.Join(ids, ","))
		}
		if m.Role == "tool" {
			entry += fmt.Sprintf("[%s]", m.ToolCallID)
		}
		trace = append(trace, entry)
	}
	return trace
}

// requireLeadingSystemOnly asserts the upstream invariant: system messages may
// only occupy the leading run of the conversation.
func requireLeadingSystemOnly(t *testing.T, messages []ChatMessage) {
	t.Helper()
	for i, m := range messages {
		if m.Role != "system" {
			continue
		}
		for j := 0; j < i; j++ {
			require.NotEqualf(t, "system", messages[j].Role,
				"system message at %d follows a %s message at %d", i, messages[j].Role, j)
		}
	}
}

func systemTextsOf(messages []ChatMessage) []string {
	var out []string
	for _, m := range messages {
		if m.Role == "system" {
			out = append(out, chatMessageContentText(m.Content))
		}
	}
	return out
}

// The exact production shape: instructions + user + function_call A, a
// developer notice injected between the call and its output, then the output.
func TestSystemFirst_MidStreamDeveloperMergesIntoLeadingSystem(t *testing.T) {
	msgs := convertGolden(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]},
		{"type":"function_call","call_id":"A","name":"exec","arguments":"{}"},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"Approved command prefix saved"}]},
		{"type":"function_call_output","call_id":"A","output":"ok"}
	]`)
	assertChatInvariants(t, msgs)
	requireLeadingSystemOnly(t, msgs)

	require.Equal(t, []string{
		"system",
		"user",
		"assistant[tool:A]",
		"tool[A]",
	}, chatRoleTrace(msgs))

	// instructions stays at index 0 and the mid-stream notice is folded into
	// it rather than dropped.
	require.Equal(t, []string{
		"You are a helpful assistant.\n\nApproved command prefix saved",
	}, systemTextsOf(msgs))
}

// Every shape that can strand a system message mid-conversation must collapse
// into the leading system message, and nothing else may move.
func TestSystemFirst_SystemRolesNeverTrailTheHead(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantTrace []string
		wantHead  string
	}{
		{
			name: "developer between call and output",
			input: `[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
				{"type":"function_call","call_id":"A","name":"exec","arguments":"{}"},
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"notice"}]},
				{"type":"function_call_output","call_id":"A","output":"ok"}
			]`,
			wantTrace: []string{"system", "user", "assistant[tool:A]", "tool[A]"},
			wantHead:  "You are a helpful assistant.\n\nnotice",
		},
		{
			name: "system role between call and output",
			input: `[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
				{"type":"function_call","call_id":"A","name":"exec","arguments":"{}"},
				{"type":"message","role":"system","content":[{"type":"input_text","text":"notice"}]},
				{"type":"function_call_output","call_id":"A","output":"ok"}
			]`,
			wantTrace: []string{"system", "user", "assistant[tool:A]", "tool[A]"},
			wantHead:  "You are a helpful assistant.\n\nnotice",
		},
		{
			name: "several notices keep their relative order",
			input: `[
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"first"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"second"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]},
				{"type":"message","role":"system","content":"third"}
			]`,
			wantTrace: []string{"system", "user", "assistant"},
			wantHead:  "You are a helpful assistant.\n\nfirst\n\nsecond\n\nthird",
		},
		{
			name: "notice between two parallel calls rejoins them",
			input: `[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
				{"type":"function_call","call_id":"A","name":"exec","arguments":"{}"},
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"notice"}]},
				{"type":"function_call","call_id":"B","name":"exec","arguments":"{}"},
				{"type":"function_call_output","call_id":"A","output":"oa"},
				{"type":"function_call_output","call_id":"B","output":"ob"}
			]`,
			wantTrace: []string{"system", "user", "assistant[tool:A,B]", "tool[A]", "tool[B]"},
			wantHead:  "You are a helpful assistant.\n\nnotice",
		},
		{
			name: "blank notice carries nothing and is dropped",
			input: `[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
				{"type":"message","role":"developer","content":[]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]}
			]`,
			wantTrace: []string{"system", "user", "assistant"},
			wantHead:  "You are a helpful assistant.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := convertGolden(t, tc.input)
			assertChatInvariants(t, msgs)
			requireLeadingSystemOnly(t, msgs)
			require.Equal(t, tc.wantTrace, chatRoleTrace(msgs))
			require.Equal(t, []string{tc.wantHead}, systemTextsOf(msgs))
		})
	}
}

// Without instructions there is no leading system message to fold into; the
// notice must still land at index 0 instead of trailing the conversation.
func TestSystemFirst_NoticeBecomesLeadingSystemWithoutInstructions(t *testing.T) {
	msgs, err := responsesInputToChatMessages("", json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"notice"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]}
	]`))
	require.NoError(t, err)
	assertChatInvariants(t, msgs)
	require.Equal(t, []string{"system", "user", "assistant"}, chatRoleTrace(msgs))
	require.Equal(t, []string{"notice"}, systemTextsOf(msgs))
}

// Regression: a plain instructions + user request must convert exactly as
// before — one system message holding only the instructions text.
func TestSystemFirst_NoNoticeLeavesConversationUntouched(t *testing.T) {
	msgs := convertGolden(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]`)
	assertChatInvariants(t, msgs)
	require.Equal(t, []string{"system", "user"}, chatRoleTrace(msgs))
	require.Equal(t, []string{"You are a helpful assistant."}, systemTextsOf(msgs))
	require.JSONEq(t, `"hi"`, string(msgs[1].Content))
}

// A developer notice in a multi-turn tool session must not disturb the
// tool_call/tool reply pairing nor the assistant/user alternation.
func TestSystemFirst_NoticeKeepsToolPairingAcrossTurns(t *testing.T) {
	msgs := convertGolden(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q1"}]},
		{"type":"function_call","call_id":"A","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"A","output":"oa"},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"Approved command prefix saved"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q2"}]},
		{"type":"function_call","call_id":"B","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"B","output":"ob"}
	]`)
	assertChatInvariants(t, msgs)
	requireLeadingSystemOnly(t, msgs)
	require.Equal(t, []string{
		"system",
		"user",
		"assistant[tool:A]",
		"tool[A]",
		"user",
		"assistant[tool:B]",
		"tool[B]",
	}, chatRoleTrace(msgs))
	require.Equal(t, []string{
		"You are a helpful assistant.\n\nApproved command prefix saved",
	}, systemTextsOf(msgs))
}
