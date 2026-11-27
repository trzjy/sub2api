//go:build unit

package service

// 纯取证增强的定向测试：requested_model_parameter_ids 记出站参数键名，
// requested_model_parameter_pairs 同时记键名与值（key=value，如 context=1m），
// 取证完整以钉死根因。零行为变更，不改任何既有字段语义。

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// cursorParamKeyRunPayload 构造带 requested_model 参数的 AgentRun envelope。
// parameters 为 nil 时不写参数；malformed 时额外塞一个坏的参数子消息。
func cursorParamKeyRunPayload(t *testing.T, parameters []cursorRequestedModelParameter, malformed bool) []byte {
	t.Helper()
	requested := cursorAppendStringField(nil, cursorFldRequestedModelID, "claude-opus-5-5")
	if malformed {
		requested = cursorAppendBytesField(requested, cursorFldRequestedModelParameter, []byte{0xff, 0xff, 0xff})
	}
	for _, p := range parameters {
		value := cursorAppendStringField(nil, cursorFldRequestedParameterID, p.id)
		value = cursorAppendStringField(value, cursorFldRequestedParameterValue, p.value)
		requested = cursorAppendBytesField(requested, cursorFldRequestedModelParameter, value)
	}
	run := cursorAppendBytesField(nil, cursorFldRunRequestedModel, requested)
	return cursorEncodeEnvelope(cursorAppendBytesField(nil, cursorFldAgentRunRequest, run))
}

// assertParamPairsInPhase 断言指定 phase 的 requested_model_parameter_pairs 字段确实记录了参数键值。
// 取证要全：键名与值均记入证据字段，断言参数值确实落在 pairs 里。
func assertParamPairsInPhase(t *testing.T, logs *observer.ObservedLogs, phase string, wantPairs ...string) {
	t.Helper()
	found := false
	for _, e := range logs.All() {
		if e.ContextMap()["phase"] != phase {
			continue
		}
		raw, ok := e.ContextMap()["requested_model_parameter_pairs"]
		if !ok {
			continue
		}
		found = true
		got := fmt.Sprintf("%v", raw)
		require.Equal(t, fmt.Sprintf("%v", wantPairs), got, "pairs 必须按 wire 顺序完整记录键值")
		for _, want := range wantPairs {
			require.Contains(t, got, want, "pairs 必须含参数键值 %q", want)
		}
	}
	require.True(t, found, "phase %q 缺少 requested_model_parameter_pairs 字段", phase)
}

func TestCursorAgentRunWireEvidenceRequestedModelParameterIDs(t *testing.T) {
	// 真实出站编码：claude-opus-5-5 携带 context=1m / effort=medium / fast=false。
	payload := cursorParamKeyRunPayload(t, cursorRequestedModelParameters("claude-opus-5-5"), false)
	// 先坐实输入 payload 确实含三个参数值，确保证据链可追溯。
	require.Contains(t, string(payload), "1m")
	require.Contains(t, string(payload), "medium")
	require.Contains(t, string(payload), "false")

	ctx, logs := cursorPhaseObserverCtx(t, "req-param-key")
	cursorPhaseLog(ctx, zap.InfoLevel, "test_param_key", "m", cursorAgentRunWireEvidenceFields(payload)...)

	// 恰好三个键名与三个键值对，顺序与 wire 一致且一一对应。
	require.Equal(t, "[context effort fast]", frameEvidencePhaseField(logs, "test_param_key", "requested_model_parameter_ids"))
	require.Equal(t, "[context=1m effort=medium fast=false]", frameEvidencePhaseField(logs, "test_param_key", "requested_model_parameter_pairs"))
	require.Equal(t, "3", frameEvidencePhaseField(logs, "test_param_key", "requested_model_parameter_count"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key", "model_id_parse_ok"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key", "agent_run_parse_ok"))
	assertParamPairsInPhase(t, logs, "test_param_key", "context=1m", "effort=medium", "fast=false")
}

func TestCursorAgentRunWireEvidenceRequestedModelParameterIDsEmpty(t *testing.T) {
	payload := cursorParamKeyRunPayload(t, nil, false)
	ctx, logs := cursorPhaseObserverCtx(t, "req-param-key-empty")
	require.NotPanics(t, func() {
		cursorPhaseLog(ctx, zap.InfoLevel, "test_param_key_empty", "m", cursorAgentRunWireEvidenceFields(payload)...)
	})
	require.Equal(t, "[]", frameEvidencePhaseField(logs, "test_param_key_empty", "requested_model_parameter_ids"))
	require.Equal(t, "[]", frameEvidencePhaseField(logs, "test_param_key_empty", "requested_model_parameter_pairs"))
	require.Equal(t, "0", frameEvidencePhaseField(logs, "test_param_key_empty", "requested_model_parameter_count"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key_empty", "model_id_parse_ok"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key_empty", "agent_run_parse_ok"))
	assertParamPairsInPhase(t, logs, "test_param_key_empty")
}

func TestCursorAgentRunWireEvidenceRequestedModelParameterIDsMalformed(t *testing.T) {
	// 参数子消息字节畸形：安全降级（跳过该键对），不 panic，既有字段语义不变。
	// count 保持向后兼容语义（计入畸形参数字段=1）；ids/pairs 只记有效项，二者数量一致（均为 0）。
	payload := cursorParamKeyRunPayload(t, nil, true)
	ctx, logs := cursorPhaseObserverCtx(t, "req-param-key-malformed")
	require.NotPanics(t, func() {
		cursorPhaseLog(ctx, zap.InfoLevel, "test_param_key_malformed", "m", cursorAgentRunWireEvidenceFields(payload)...)
	})
	require.Equal(t, "1", frameEvidencePhaseField(logs, "test_param_key_malformed", "requested_model_parameter_count"))
	require.Equal(t, "[]", frameEvidencePhaseField(logs, "test_param_key_malformed", "requested_model_parameter_ids"))
	require.Equal(t, "[]", frameEvidencePhaseField(logs, "test_param_key_malformed", "requested_model_parameter_pairs"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key_malformed", "model_id_parse_ok"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key_malformed", "agent_run_parse_ok"))
	assertParamPairsInPhase(t, logs, "test_param_key_malformed")
}
