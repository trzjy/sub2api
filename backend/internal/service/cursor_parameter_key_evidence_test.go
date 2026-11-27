//go:build unit

package service

// 纯取证增强的定向测试：requested_model_parameter_ids 只记出站参数键名，
// 绝不记 f2 参数值（1m / medium / false）。零行为变更，不改任何既有字段语义。

import (
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

// assertNoParamValuesInPhase 断言指定 phase 的所有 string / []string 字段值都不含给定子串。
// bool 字段（如 model_id_parse_ok=false）不是参数值，不参与检查。
func assertNoParamValuesInPhase(t *testing.T, logs *observer.ObservedLogs, phase string, values ...string) {
	t.Helper()
	for _, e := range logs.All() {
		if e.ContextMap()["phase"] != phase {
			continue
		}
		for key, raw := range e.ContextMap() {
			switch v := raw.(type) {
			case string:
				for _, val := range values {
					require.NotContains(t, v, val, "string 字段 %s 泄漏了参数值 %q", key, val)
				}
			case []string:
				for _, item := range v {
					for _, val := range values {
						require.NotEqual(t, val, item, "strings 字段 %s 泄漏了参数值 %q", key, val)
					}
				}
			}
		}
	}
}

func TestCursorAgentRunWireEvidenceRequestedModelParameterIDs(t *testing.T) {
	// 真实出站编码：claude-opus-5-5 携带 context=1m / effort=medium / fast=false。
	payload := cursorParamKeyRunPayload(t, cursorRequestedModelParameters("claude-opus-5-5"), false)
	// 先坐实输入 payload 确实含三个参数值，否则"日志无值"断言无意义。
	require.Contains(t, string(payload), "1m")
	require.Contains(t, string(payload), "medium")
	require.Contains(t, string(payload), "false")

	ctx, logs := cursorPhaseObserverCtx(t, "req-param-key")
	cursorPhaseLog(ctx, zap.InfoLevel, "test_param_key", "m", cursorAgentRunWireEvidenceFields(payload)...)

	// 恰好三个键名，顺序与 wire 一致；值不出现。
	require.Equal(t, "[context effort fast]", frameEvidencePhaseField(logs, "test_param_key", "requested_model_parameter_ids"))
	require.Equal(t, "3", frameEvidencePhaseField(logs, "test_param_key", "requested_model_parameter_count"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key", "model_id_parse_ok"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key", "agent_run_parse_ok"))
	assertNoParamValuesInPhase(t, logs, "test_param_key", "1m", "medium", "false")
}

func TestCursorAgentRunWireEvidenceRequestedModelParameterIDsEmpty(t *testing.T) {
	payload := cursorParamKeyRunPayload(t, nil, false)
	ctx, logs := cursorPhaseObserverCtx(t, "req-param-key-empty")
	require.NotPanics(t, func() {
		cursorPhaseLog(ctx, zap.InfoLevel, "test_param_key_empty", "m", cursorAgentRunWireEvidenceFields(payload)...)
	})
	require.Equal(t, "[]", frameEvidencePhaseField(logs, "test_param_key_empty", "requested_model_parameter_ids"))
	require.Equal(t, "0", frameEvidencePhaseField(logs, "test_param_key_empty", "requested_model_parameter_count"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key_empty", "model_id_parse_ok"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key_empty", "agent_run_parse_ok"))
}

func TestCursorAgentRunWireEvidenceRequestedModelParameterIDsMalformed(t *testing.T) {
	// 参数子消息字节畸形：安全降级（跳过该键），不 panic，既有字段语义不变。
	payload := cursorParamKeyRunPayload(t, nil, true)
	ctx, logs := cursorPhaseObserverCtx(t, "req-param-key-malformed")
	require.NotPanics(t, func() {
		cursorPhaseLog(ctx, zap.InfoLevel, "test_param_key_malformed", "m", cursorAgentRunWireEvidenceFields(payload)...)
	})
	require.Equal(t, "1", frameEvidencePhaseField(logs, "test_param_key_malformed", "requested_model_parameter_count"))
	require.Equal(t, "[]", frameEvidencePhaseField(logs, "test_param_key_malformed", "requested_model_parameter_ids"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key_malformed", "model_id_parse_ok"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_param_key_malformed", "agent_run_parse_ok"))
}
