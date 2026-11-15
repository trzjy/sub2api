//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDevinConvert_BuildParams_SystemNotInPrompts(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"sys-1"},` +
		`{"role":"developer","content":"dev-1"},` +
		`{"role":"user","content":"hello"}]}`)

	p, err := BuildDevinChatParams(body, "model-uid", "cascade-1")
	require.NoError(t, err)
	require.Equal(t, "model-uid", p.ModelUID)
	require.Equal(t, "cascade-1", p.CascadeID)
	require.Equal(t, "sys-1\n\ndev-1", p.SystemPrompt)
	require.Len(t, p.Prompts, 1)
	require.Equal(t, ChatMessageSourceUser, p.Prompts[0].Source)
	require.Equal(t, "hello", p.Prompts[0].Prompt)
	require.Equal(t, formatDevinMessageID("cascade-1", 2, "user"), p.Prompts[0].MessageID)
}

func TestDevinConvert_BuildParams_ToolResultSourceAndError(t *testing.T) {
	body := []byte(`{"messages":[{"role":"tool","tool_call_id":"call_1","content":"the result","is_error":true}]}`)

	p, err := BuildDevinChatParams(body, "m", "cascade-2")
	require.NoError(t, err)
	require.Len(t, p.Prompts, 1)
	got := p.Prompts[0]
	require.Equal(t, ChatMessageSourceTool, got.Source)
	require.Equal(t, uint32(4), got.Source)
	require.Equal(t, "call_1", got.ToolCallId)
	require.Equal(t, "the result", got.Prompt)
	require.True(t, got.ToolResultIsError)
}

func TestDevinConvert_BuildParams_AssistantThinkingAndToolCalls(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":"sure",` +
		`"reasoning_content":"let me think",` +
		`"tool_calls":[` +
		`{"id":"call_a","type":"function","function":{"name":"search","arguments":"{\"q\":1, \"n\":2}"}},` +
		`{"id":"call_b","type":"function","function":{"name":"noop","arguments":""}}]}]}`)

	p, err := BuildDevinChatParams(body, "m", "cascade-3")
	require.NoError(t, err)
	require.Len(t, p.Prompts, 1)
	got := p.Prompts[0]
	require.Equal(t, ChatMessageSourceSystem, got.Source)
	require.Equal(t, uint32(2), got.Source)
	require.Equal(t, "bot-"+formatDevinMessageID("cascade-3", 0, "assistant"), got.MessageID)
	require.True(t, strings.HasPrefix(got.MessageID, "bot-"))
	require.Equal(t, "let me think", got.Thinking)
	require.Equal(t, "sure", got.Prompt)
	require.Len(t, got.ToolCalls, 2)
	require.Equal(t, "call_a", got.ToolCalls[0].ID)
	require.Equal(t, "search", got.ToolCalls[0].Name)
	require.JSONEq(t, `{"q":1,"n":2}`, got.ToolCalls[0].ArgumentsJSON)
	require.Equal(t, "call_b", got.ToolCalls[1].ID)
	require.JSONEq(t, `{}`, got.ToolCalls[1].ArgumentsJSON)
}

func TestDevinConvert_BuildParams_InvalidArgumentsRejected(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":"",` +
		`"tool_calls":[{"id":"x","function":{"name":"f","arguments":"{not json"}}]}]}`)
	_, err := BuildDevinChatParams(body, "m", "c")
	require.Error(t, err)
	require.Contains(t, err.Error(), "arguments")
}

func TestDevinConvert_BuildParams_ImageInputRejected(t *testing.T) {
	cases := []string{
		`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image","image":"..."}]}]}`,
	}
	for _, raw := range cases {
		_, err := BuildDevinChatParams([]byte(raw), "m", "c")
		require.Error(t, err)
		require.Contains(t, err.Error(), "devin: image input is not supported")
	}
}

func TestDevinConvert_BuildParams_ContentArrayTextJoined(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"hello "},` +
		`{"type":"text","text":"world"}]}]}`)
	p, err := BuildDevinChatParams(body, "m", "c")
	require.NoError(t, err)
	require.Equal(t, "hello world", p.Prompts[0].Prompt)
}

func TestDevinConvert_BuildParams_ToolChoiceMapping(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantOption string
		wantTool   string
		wantNil    bool
	}{
		{name: "required", raw: `"required"`, wantOption: "any"},
		{name: "auto", raw: `"auto"`, wantOption: "auto"},
		{name: "none", raw: `"none"`, wantOption: "none"},
		{name: "function object", raw: `{"type":"function","function":{"name":"my_tool"}}`, wantTool: "my_tool"},
		{name: "absent", raw: ``, wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"messages":[]}`
			if tc.raw != "" {
				raw = `{"messages":[],"tool_choice":` + tc.raw + `}`
			}
			p, err := BuildDevinChatParams([]byte(raw), "m", "c")
			require.NoError(t, err)
			if tc.wantNil {
				require.Nil(t, p.ToolChoice)
				return
			}
			require.NotNil(t, p.ToolChoice)
			require.Equal(t, tc.wantOption, p.ToolChoice.OptionName)
			require.Equal(t, tc.wantTool, p.ToolChoice.ToolName)
		})
	}
}

func TestDevinConvert_BuildParams_NumericParamsDistinguishUnsetFromZero(t *testing.T) {
	pZero, err := BuildDevinChatParams([]byte(`{"messages":[],"temperature":0,"top_p":0,"max_tokens":0}`), "m", "c")
	require.NoError(t, err)
	require.NotNil(t, pZero.Temperature)
	require.Equal(t, float64(0), *pZero.Temperature)
	require.NotNil(t, pZero.TopP)
	require.Equal(t, float64(0), *pZero.TopP)
	require.NotNil(t, pZero.MaxTokens)
	require.Equal(t, 0, *pZero.MaxTokens)

	pUnset, err := BuildDevinChatParams([]byte(`{"messages":[]}`), "m", "c")
	require.NoError(t, err)
	require.Nil(t, pUnset.Temperature)
	require.Nil(t, pUnset.TopP)
	require.Nil(t, pUnset.MaxTokens)

	pNonZero, err := BuildDevinChatParams([]byte(`{"messages":[],"temperature":0.7,"top_p":0.9,"max_completion_tokens":321}`), "m", "c")
	require.NoError(t, err)
	require.InDelta(t, 0.7, *pNonZero.Temperature, 1e-9)
	require.InDelta(t, 0.9, *pNonZero.TopP, 1e-9)
	require.Equal(t, 321, *pNonZero.MaxTokens)
}

func TestDevinConvert_BuildParams_StopAndTools(t *testing.T) {
	body := []byte(`{"messages":[],"stop":["END","STOP"],` +
		`"tools":[{"type":"function","function":{"name":"f","description":"desc",` +
		`"parameters":{"type":"object","properties":{"x":{"type":"string"}}}}},` +
		`{"type":"function","function":{"name":"g"}}]}`)
	p, err := BuildDevinChatParams(body, "m", "c")
	require.NoError(t, err)
	require.Equal(t, []string{"END", "STOP"}, p.Stop)
	require.Len(t, p.Tools, 2)
	require.Equal(t, "f", p.Tools[0].Name)
	require.Equal(t, "desc", p.Tools[0].Description)
	require.JSONEq(t, `{"type":"object","properties":{"x":{"type":"string"}}}`, p.Tools[0].JSONSchemaString)
	require.False(t, p.Tools[0].Strict)
	require.Equal(t, "g", p.Tools[1].Name)
	require.JSONEq(t, `{"type":"object"}`, p.Tools[1].JSONSchemaString)

	pStr, err := BuildDevinChatParams([]byte(`{"messages":[],"stop":"<e>"}`), "m", "c")
	require.NoError(t, err)
	require.Equal(t, []string{"<e>"}, pStr.Stop)
}

func TestDevinConvert_BuildParams_GeneratedCascadeIDIsV4(t *testing.T) {
	p, err := BuildDevinChatParams([]byte(`{"messages":[]}`), "m", "")
	require.NoError(t, err)
	require.Len(t, p.CascadeID, 36)
	require.Equal(t, byte('4'), p.CascadeID[14])
	require.Contains(t, "89ab", string(p.CascadeID[19]))
}

func TestDevinConvert_BuildParams_InvalidJSONRejected(t *testing.T) {
	_, err := BuildDevinChatParams([]byte(`{not json`), "m", "c")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid")
}

func TestDevinConvert_MessageIDIsDeterministic(t *testing.T) {
	require.Equal(t, formatDevinMessageID("cascade", 0, "user"), devinMessageID("cascade", 0, "user"))
	require.NotEqual(t, devinMessageID("cascade", 0, "user"), devinMessageID("cascade", 1, "user"))
	require.Regexp(t, `^[0-9a-f]{8}-0000-0000-0000-000000000000$`, devinMessageID("cascade", 0, "user"))
}

func TestDevinConvert_Accumulator_AnonymousFragmentMerged(t *testing.T) {
	acc := &ToolCallAccumulator{}
	out, err := acc.Add([]ChatToolCall{
		{ID: "call_1", Name: "search", ArgumentsJSON: `{"a":`},
		{ArgumentsJSON: `1}`}, // 匿名片段归当前活跃调用
	})
	require.NoError(t, err)
	require.Len(t, out, 2)
	require.Equal(t, `{"a":`, out[0].ArgumentsJSON)
	require.Equal(t, `1}`, out[1].ArgumentsJSON)
	require.Equal(t, 0, out[0].Index)
	require.Equal(t, 0, out[1].Index)
	require.Equal(t, `{"a":1}`, acc.calls[0].call.ArgumentsJSON)
	fin, err := acc.Finish()
	require.NoError(t, err)
	require.Empty(t, fin) // 所有参数字节已随增量发出，不重放
}

func TestDevinConvert_Accumulator_NamedFragmentStartsNewCall(t *testing.T) {
	acc := &ToolCallAccumulator{}
	out, err := acc.Add([]ChatToolCall{
		{Name: "first", ArgumentsJSON: `{}`},
		{Name: "second", ArgumentsJSON: `{}`}, // 有 name 无 id 且当前已有 name → 新调用
	})
	require.NoError(t, err)
	require.Len(t, out, 2)
	require.Equal(t, "first", out[0].Name)
	require.Equal(t, 0, out[0].Index)
	require.Equal(t, "second", out[1].Name)
	require.Equal(t, 1, out[1].Index)
}

func TestDevinConvert_Accumulator_NameChangeRejected(t *testing.T) {
	acc := &ToolCallAccumulator{}
	_, err := acc.Add([]ChatToolCall{{ID: "call_1", Name: "first"}})
	require.NoError(t, err)
	_, err = acc.Add([]ChatToolCall{{ID: "call_1", Name: "second"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "name changed")
}

func TestDevinConvert_Accumulator_InvalidJSONFlagRejected(t *testing.T) {
	acc := &ToolCallAccumulator{}
	_, err := acc.Add([]ChatToolCall{{ID: "call_1", Name: "f", InvalidJSONErr: "bad"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid")
}

func TestDevinConvert_Accumulator_ZeroArgsFinishEmitsBraces(t *testing.T) {
	acc := &ToolCallAccumulator{}
	out, err := acc.Add([]ChatToolCall{{ID: "call_1", Name: "noop"}})
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, "", out[0].ArgumentsJSON) // 只有头，暂无参数字节

	fin, err := acc.Finish()
	require.NoError(t, err)
	require.Len(t, fin, 1)
	require.Equal(t, "{}", fin[0].ArgumentsJSON)
	require.Equal(t, "noop", fin[0].Name)
}

func TestDevinConvert_Accumulator_FinishValidatesObject(t *testing.T) {
	acc := &ToolCallAccumulator{}
	_, err := acc.Add([]ChatToolCall{{ID: "a", Name: "f", ArgumentsJSON: `[1,2]`}})
	require.NoError(t, err)
	_, err = acc.Finish()
	require.Error(t, err)
	require.Contains(t, err.Error(), "JSON object")

	acc2 := &ToolCallAccumulator{}
	_, err = acc2.Add([]ChatToolCall{{ID: "a", Name: "f", ArgumentsJSON: `{"x":`}})
	require.NoError(t, err)
	_, err = acc2.Finish()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid")

	acc3 := &ToolCallAccumulator{}
	_, err = acc3.Add([]ChatToolCall{{ArgumentsJSON: `{}`}}) // 无 id 无 name 无头
	require.Error(t, err)
	require.Contains(t, err.Error(), "no call header")
}

func TestDevinConvert_StopReasonMapping(t *testing.T) {
	cases := []struct {
		reason   int32
		hasTools bool
		want     string
	}{
		{reason: 0, hasTools: true, want: "tool_calls"},
		{reason: 3, hasTools: true, want: "tool_calls"}, // 工具调用优先
		{reason: 3, hasTools: false, want: "length"},
		{reason: 10, hasTools: false, want: "stop"},
		{reason: 0, hasTools: false, want: "stop"},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, DevinStopReasonToOpenAI(tc.reason, tc.hasTools))
	}
}

func TestDevinConvert_UsageMapping(t *testing.T) {
	require.Equal(t, OpenAIUsage{}, DevinUsageToOpenAI(nil))
	got := DevinUsageToOpenAI(&ChatUsage{Input: 10, Output: 20, CacheWrite: 5, CacheRead: 7})
	require.Equal(t, 10, got.InputTokens)
	require.Equal(t, 20, got.OutputTokens)
	require.Equal(t, 5, got.CacheCreationInputTokens)
	require.Equal(t, 7, got.CacheReadInputTokens)
}

func TestDevinConvert_AnthropicBodyToChatBody(t *testing.T) {
	out, err := AnthropicBodyToChatBody([]byte(`{"model":"claude-x","max_tokens":64,` +
		`"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.True(t, gjson.ValidBytes(out))
	require.Equal(t, "user", gjson.GetBytes(out, "messages.0.role").String())
	require.Equal(t, "hi", gjson.GetBytes(out, "messages.0.content").String())

	_, err = AnthropicBodyToChatBody([]byte(`{not json`))
	require.Error(t, err)
}

// formatDevinMessageID 是测试内对 FNV-1a 32 位格式化结果的独立实现，
// 用于交叉验证生产函数 devinMessageID 的格式与取值。
func formatDevinMessageID(cascadeID string, index int, role string) string {
	const offset32 = 2166136261
	const prime32 = 16777619
	seed := cascadeID + "\x00" + devinTestItoa(index) + "\x00" + role
	var h uint32 = offset32
	for i := 0; i < len(seed); i++ {
		h ^= uint32(seed[i])
		h *= prime32
	}
	return strings.ToLower(devinTestHex8(h)) + "-0000-0000-0000-000000000000"
}

func devinTestItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func devinTestHex8(v uint32) string {
	const digits = "0123456789abcdef"
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = digits[v&0xF]
		v >>= 4
	}
	return string(b[:])
}
