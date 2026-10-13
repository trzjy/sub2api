package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wireAnthropic marshals an AnthropicRequest the way the gateway forwards it
// upstream (/v1/messages) and returns the decoded map so tests can assert on
// the exact outbound JSON contract (thinking.type / output_config.effort / ...).
func wireAnthropic(t *testing.T, req *AnthropicRequest) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// opus55Request builds a minimal Responses request for the given model/effort.
func opus55Request(model, effort string) *ResponsesRequest {
	return &ResponsesRequest{
		Model:     model,
		Input:     json.RawMessage(`[{"role":"user","content":"hello"}]`),
		Reasoning: &ResponsesReasoning{Effort: effort},
	}
}

func TestResponsesToAnthropic_Opus55_AdaptiveThinking(t *testing.T) {
	// 复现派发单错误路径：claude-opus-5-5 在 effort != low 时，原实现产出
	// thinking.type=enabled + budget_tokens，命中上游 "requires adaptive thinking" 400。
	// 修复后须产出 thinking.type=adaptive 且 output_config.effort，且不带 budget_tokens。
	for _, effort := range []string{"medium", "high", "max"} {
		t.Run("effort_"+effort, func(t *testing.T) {
			req := opus55Request("claude-opus-5-5", effort)
			out, err := ResponsesToAnthropicRequest(req)
			require.NoError(t, err)

			wire := wireAnthropic(t, out)

			// output_config.effort 必须存在且携带原始 effort。
			require.Contains(t, wire, "output_config")
			var oc map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(wire["output_config"], &oc))
			require.Contains(t, oc, "effort")
			assert.JSONEq(t, `"`+mapResponsesEffortToAnthropic(effort)+`"`, string(oc["effort"]))

			// thinking.type 必须为 adaptive，且不得带 budget_tokens、不得为 enabled。
			require.Contains(t, wire, "thinking")
			var th map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(wire["thinking"], &th))
			require.Contains(t, th, "type")
			assert.JSONEq(t, `"adaptive"`, string(th["type"]))
			assert.NotContains(t, th, "budget_tokens",
				"adaptive thinking must not carry budget_tokens (upstream rejects it)")
			assert.NotContains(t, string(wire["thinking"]), `"enabled"`)
		})
	}
}

func TestResponsesToAnthropic_Opus55_LowEffortNoThinking(t *testing.T) {
	// low effort：不应产生 thinking 块（仅 output_config.effort=low）。
	req := opus55Request("claude-opus-5-5", "low")
	out, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)

	wire := wireAnthropic(t, out)
	assert.NotContains(t, wire, "thinking", "low effort must not emit a thinking block")
	require.Contains(t, wire, "output_config")
}

func TestResponsesToAnthropic_NonOpus_KeepsEnabledThinking(t *testing.T) {
	// 负例：其他 Claude 模型必须维持现状语义——thinking.type=enabled + budget_tokens。
	// 覆盖派发单点名模型 claude-opus-4-8 与 claude-sonnet-5。
	for _, tc := range []struct {
		model  string
		effort string
	}{
		{"claude-opus-4-8", "high"},
		{"claude-sonnet-5", "high"},
		{"claude-opus-4-6", "medium"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			req := opus55Request(tc.model, tc.effort)
			out, err := ResponsesToAnthropicRequest(req)
			require.NoError(t, err)

			wire := wireAnthropic(t, out)
			require.Contains(t, wire, "thinking", "non-Opus5.5 must keep thinking block")
			var th map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(wire["thinking"], &th))
			require.Contains(t, th, "type")
			assert.JSONEq(t, `"enabled"`, string(th["type"]),
				"non-Opus5.5 must not leak adaptive params")
			assert.Contains(t, th, "budget_tokens",
				"non-Opus5.5 enabled thinking must keep budget_tokens")
			require.Contains(t, wire, "output_config")
		})
	}
}

func TestResponsesToAnthropic_Opus55Adaptive_UpstreamContract(t *testing.T) {
	// 端到端断言：出站 JSON 同时满足上游契约的两个可选项之一
	// ("thinking.type=adaptive and output_config.effort")。
	req := opus55Request("claude-opus-5-5", "high")
	out, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)

	raw, err := json.Marshal(out)
	require.NoError(t, err)

	// 契约要素齐备。
	assert.Contains(t, string(raw), `"type":"adaptive"`)
	assert.Contains(t, string(raw), `"output_config"`)
	assert.Contains(t, string(raw), `"effort":"high"`)
	// 不得出现 enabled 或 budget_tokens（二者都是 Opus5.5 非法参数）。
	assert.NotContains(t, string(raw), `"type":"enabled"`)
	assert.NotContains(t, string(raw), `budget_tokens`)
}

func TestModelRequiresAdaptiveThinking(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"claude-opus-5-5", true},
		{"claude-opus-5-5-20261001", true},
		{"us.anthropic.claude-opus-5-5-v1", true},
		{"claude-opus-4-8", false},
		{"claude-opus-4-6", false},
		{"claude-sonnet-5", false},
		{"claude-sonnet-4-5", false},
		{"gpt-5.2", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			assert.Equal(t, tc.want, modelRequiresAdaptiveThinking(tc.model))
		})
	}
}
