//go:build unit

package service

// §3.2 定向调试的组件级测试：两个出站 wire 边界 + 上游参数拒绝通知取证。
//
// 全部走真实实现：Execute builder → 真实出站请求；cursorAgentRunIntake → 原生双工
// io.Pipe 回写；pump 终态 → 固定模板检测。只断言不可逆摘要/字段枚举与"原响应不变"，
// 不造生产 stub、不新增配置或状态。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

var (
	cursorNoticeTemplateOnly   = cursorParameterNoticeTemplate
	cursorNoticeWithEvent      = cursorParameterNoticeTemplate + " [8] (1795667813078-ac3caa14)"
	cursorNoticeBadEventSuffix = cursorParameterNoticeTemplate + " [8] (123-abc)"
)

// cursorNoticeBody 构造"普通非结构化成功"的上游流：空起始帧 + 文本帧 + 真实 usage + EndStream。
func cursorNoticeBody(t *testing.T, texts ...string) []byte {
	t.Helper()
	var out []byte
	out = append(out, cursorTextDeltaFrame(t, "")...)
	for _, text := range texts {
		out = append(out, cursorTextDeltaFrame(t, text)...)
	}
	out = append(out, cursorTurnEndedFrame(t, [2]uint64{1, 11}, [2]uint64{2, 7})...)
	out = append(out, cursorTestEndStream(t, `{}`)...)
	return out
}

// ---------------------------------------------------------------------------
// 出站边界①：agent_run_wire_evidence（真实 builder 字节）
// ---------------------------------------------------------------------------

func TestCursorAgentRunWireEvidenceRealOutboundFieldsAndHash(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-wire-evidence")
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"}})
	svc, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	privatePrompt := "PRIVATE-PROMPT-SENTENCE"
	privateTool := "PRIVATE_TOOL_NAME"
	privateSchema := "PRIVATE_SCHEMA_MARKER"
	reqBody := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"` + privatePrompt + `"}],` +
		`"tools":[{"type":"function","function":{"name":"` + privateTool + `","description":"private description",` +
		`"parameters":{"type":"object","properties":{"q":{"type":"string","description":"` + privateSchema + `"}}}}}]}`)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	_, err := executeCursorForTest(t, svc, ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"),
		reqBody, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)
	require.Equal(t, 1, up.count())

	sent := up.at(0).body
	sum := sha256.Sum256(sent)
	frame, off, err := cursorReadEnvelopeFrame(sent, 0)
	require.NoError(t, err)
	require.Equal(t, len(sent), off)
	clientFields, err := cursorDecodeProtobuf(frame.Data)
	require.NoError(t, err)
	runMsg, ok := cursorFindField(clientFields, cursorFldAgentRunRequest)
	require.True(t, ok)
	runFields, err := cursorDecodeProtobuf(runMsg.Bytes)
	require.NoError(t, err)
	expectedTop := make([]string, 0, len(runFields))
	for _, f := range runFields {
		expectedTop = append(expectedTop, fmt.Sprintf("%d:%d", f.Number, f.Wire))
	}
	mcp, ok := cursorFindField(runFields, cursorFldRunMcpTools)
	require.True(t, ok, "tools must be declared in AgentRun.f4")
	mcpSum := sha256.Sum256(mcp.Bytes)

	require.Equal(t, 1, phaseCount(logs, "agent_run_wire_evidence"))
	require.Equal(t, fmt.Sprint(expectedTop), frameEvidencePhaseField(logs, "agent_run_wire_evidence", "wire_fields"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "agent_run_wire_evidence", "agent_run_parse_ok"))
	require.Equal(t, "user_message", frameEvidencePhaseField(logs, "agent_run_wire_evidence", "action"))
	require.Equal(t, "1", frameEvidencePhaseField(logs, "agent_run_wire_evidence", "state_mode"))
	require.Equal(t, "1", frameEvidencePhaseField(logs, "agent_run_wire_evidence", "mcp_tool_count"))
	require.Equal(t, fmt.Sprint(len(sent)), frameEvidencePhaseField(logs, "agent_run_wire_evidence", "payload_bytes"))
	require.Equal(t, hex.EncodeToString(sum[:]), frameEvidencePhaseField(logs, "agent_run_wire_evidence", "payload_sha256"))
	require.Equal(t, fmt.Sprint(len(mcp.Bytes)), frameEvidencePhaseField(logs, "agent_run_wire_evidence", "mcp_def_bytes"))
	require.Equal(t, hex.EncodeToString(mcpSum[:]), frameEvidencePhaseField(logs, "agent_run_wire_evidence", "mcp_def_sha256"))
	require.Equal(t, "upstream-sonnet", frameEvidencePhaseField(logs, "agent_run_wire_evidence", "model_id"))
	require.Equal(t, "1", frameEvidencePhaseField(logs, "agent_run_wire_evidence", "account_id"))
	require.Equal(t, "upstream-sonnet", frameEvidencePhaseField(logs, "agent_run_wire_evidence", "upstream_model"))
	require.Equal(t, "req-wire-evidence", frameEvidencePhaseField(logs, "agent_run_wire_evidence", "request_id"))

	phaseLogs := logs.FilterField(zap.String("phase", "agent_run_wire_evidence"))
	for _, private := range []string{privatePrompt, privateTool, privateSchema, "private description",
		"BEARER-ACCOUNT-A", "ACTIVATION-CODE-ELEVEN"} {
		require.False(t, observedContainsText(phaseLogs, private), private)
	}
}

// ---------------------------------------------------------------------------
// 出站边界②：tool_result_wire_evidence（真实回写帧 + 当前 ctx request_id）
// ---------------------------------------------------------------------------

func TestCursorToolResultWireEvidenceRealWritebackFieldsAndHash(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-current-http")
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	delegations := newCursorToolDelegations()
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "c1", Name: "PRIVATE_TOOL_A", ExecID: 7, IDField: []byte{0xaa}}))
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "c2", Name: "PRIVATE_TOOL_B", ExecID: 8}))
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "c3", Name: "PRIVATE_TOOL_C", ExecID: 9}))

	pr, pw := io.Pipe()
	d := newWritebackDuplex(pr, pw, delegations)
	require.NoError(t, svc.installIntake(record.id, &cursorAgentRunIntake{
		svc: svc, session: record, duplex: d, delegations: delegations,
	}))
	stop := captureDuplex(t, d)
	err := record.intake.DeliverToolResults(ctx, record.id, cursorSessionLevelBatchID, []CursorToolResult{
		{CallID: "c1", Text: []string{"PRIVATE-TOOL-TEXT"}, Structured: []byte(`{"private":true}`)},
		{CallID: "c2", Text: []string{"PRIVATE-ERROR-TEXT"}, IsError: true},
		{CallID: "c3", Images: []cursorTurnImage{{data: []byte("PRIVATE-IMAGE-BYTES"), mimeType: "image/png"}}},
	})
	require.NoError(t, err)
	wire := stop()

	frames := splitWritebackEnvelopes(t, wire)
	require.Len(t, frames, 3)
	expected := make([]string, 0, len(frames))
	for _, f := range frames {
		sum := sha256.Sum256(f)
		expected = append(expected, fmt.Sprintf("%d:%s", len(f), hex.EncodeToString(sum[:])))
	}

	require.Equal(t, 1, phaseCount(logs, "tool_result_wire_evidence"))
	require.Equal(t, record.id, frameEvidencePhaseField(logs, "tool_result_wire_evidence", "session_id"))
	require.Equal(t, cursorSessionLevelBatchID, frameEvidencePhaseField(logs, "tool_result_wire_evidence", "batch_id"))
	require.Equal(t, "42", frameEvidencePhaseField(logs, "tool_result_wire_evidence", "account_id"))
	require.Equal(t, "cursor-test-upstream", frameEvidencePhaseField(logs, "tool_result_wire_evidence", "upstream_model"))
	require.Equal(t, "req-current-http", frameEvidencePhaseField(logs, "tool_result_wire_evidence", "request_id"))
	require.Equal(t, "3", frameEvidencePhaseField(logs, "tool_result_wire_evidence", "result_count"))
	require.Equal(t, "3", frameEvidencePhaseField(logs, "tool_result_wire_evidence", "frame_count"))
	require.Equal(t, "1", frameEvidencePhaseField(logs, "tool_result_wire_evidence", "is_error_count"))
	require.Equal(t, "1", frameEvidencePhaseField(logs, "tool_result_wire_evidence", "images_count"))
	require.Equal(t, "1", frameEvidencePhaseField(logs, "tool_result_wire_evidence", "structured_count"))
	require.Equal(t, fmt.Sprint(expected), frameEvidencePhaseField(logs, "tool_result_wire_evidence", "frame_bytes_sha256"))

	phaseLogs := logs.FilterField(zap.String("phase", "tool_result_wire_evidence"))
	for _, private := range []string{"PRIVATE-TOOL-TEXT", "PRIVATE-ERROR-TEXT", "PRIVATE-IMAGE-BYTES",
		"PRIVATE_TOOL_A", "PRIVATE_TOOL_B", "PRIVATE_TOOL_C"} {
		require.False(t, observedContainsText(phaseLogs, private), private)
	}
	fieldMap := phaseLogs.All()[0].ContextMap()
	require.NotContains(t, fieldMap, "call_id")
	require.NotContains(t, fieldMap, "exec_id")
}

// ---------------------------------------------------------------------------
// pump 终态：upstream_parameter_notice（固定模板 + 严格事件号 + 失败关闭）
// ---------------------------------------------------------------------------

func TestCursorParameterNoticeStreamAndNonStreamFailClosed(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			ctx, logs := cursorPhaseObserverCtx(t, "req-notice-evidence")
			settlement := &cursorFullChainSettlement{}
			ctx = WithCursorSessionHandover(ctx, &cursorTestHandover{settlement: settlement.hook})
			up := &cursorOutboundUpstream{body: cursorNoticeBody(t, cursorNoticeWithEvent)}
			repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"}})
			svc, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			res, err := executeCursorForTest(t, svc, ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"),
				cursorRLStopRequestBody(stream), "cursor-sonnet", stream, time.Now())
			require.ErrorIs(t, err, ErrCursorUpstreamParameterUnsupported)
			require.Nil(t, res, "参数拒绝不得返回成功结果")
			require.NotContains(t, w.Body.String(), string(cursorSSEDoneSentinel), "参数拒绝不得发成功终结")

			require.Equal(t, 1, phaseCount(logs, "upstream_parameter_notice"))
			require.Equal(t, "true", frameEvidencePhaseField(logs, "upstream_parameter_notice", "notice_is_entire_reply"))
			require.Equal(t, "1795667813078-ac3caa14", frameEvidencePhaseField(logs, "upstream_parameter_notice", "event_id"))
			require.Equal(t, cursorNoticeTemplateOnly, frameEvidencePhaseField(logs, "upstream_parameter_notice", "template"))
			require.Equal(t, "true", frameEvidencePhaseField(logs, "upstream_parameter_notice", "usage_seen"))
			require.Equal(t, "11", frameEvidencePhaseField(logs, "upstream_parameter_notice", "usage_input"))
			require.Equal(t, "7", frameEvidencePhaseField(logs, "upstream_parameter_notice", "usage_output"))
			require.Equal(t, "1", frameEvidencePhaseField(logs, "upstream_parameter_notice", "account_id"))
			require.Equal(t, "req-notice-evidence", frameEvidencePhaseField(logs, "upstream_parameter_notice", "request_id"))
			require.NotEmpty(t, frameEvidencePhaseField(logs, "upstream_parameter_notice", "session_id"))
			require.Equal(t, "upstream-sonnet", frameEvidencePhaseField(logs, "upstream_parameter_notice", "upstream_model"))
			require.Equal(t, 1, phaseCount(logs, "response_parse_done"))
			require.Equal(t, "parameter_unsupported", frameEvidencePhaseField(logs, "response_parse_done", "outcome"))
			require.Eventually(t, func() bool {
				calls, complete := settlement.snapshot()
				return calls == 1 && complete == 0
			}, time.Second, time.Millisecond, "parameter rejection must skip normal settlement")
			require.Less(t, phaseIndex(logs, "upstream_parameter_notice"), phaseIndex(logs, "response_parse_done"))
		})
	}
}

func TestCursorParameterNoticeCrossFrameAccumulatedText(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-notice-crossframe")
	mid := len(cursorNoticeWithEvent) / 2
	up := &cursorOutboundUpstream{body: cursorNoticeBody(t,
		cursorNoticeWithEvent[:mid], cursorNoticeWithEvent[mid:])}
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"}})
	svc, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	_, err := executeCursorForTest(t, svc, ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"),
		cursorRLStopRequestBody(false), "cursor-sonnet", false, time.Now())
	require.ErrorIs(t, err, ErrCursorUpstreamParameterUnsupported)
	require.NotContains(t, w.Body.String(), string(cursorSSEDoneSentinel))
	require.Equal(t, 1, phaseCount(logs, "upstream_parameter_notice"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "upstream_parameter_notice", "notice_is_entire_reply"))
	require.Equal(t, "1795667813078-ac3caa14", frameEvidencePhaseField(logs, "upstream_parameter_notice", "event_id"))
}

func TestCursorParameterNoticeQuotedCandidateHasNoSideEffects(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-notice-quoted")
	private := "PRIVATE-ORDINARY-REPLY-KEY"
	quoted := `模型先说 "` + cursorNoticeWithEvent + `"，` + private + ` 继续正常回答`
	up := &cursorOutboundUpstream{body: cursorNoticeBody(t, quoted)}
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"}})
	svc, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	res, err := executeCursorForTest(t, svc, ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"),
		cursorRLStopRequestBody(false), "cursor-sonnet", false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, res, "引用通知绝不能改变成功结果")
	require.Contains(t, w.Body.String(), cursorNoticeTemplateOnly)
	require.Contains(t, w.Body.String(), private)

	require.Equal(t, 1, phaseCount(logs, "upstream_parameter_notice"))
	require.Equal(t, "false", frameEvidencePhaseField(logs, "upstream_parameter_notice", "notice_is_entire_reply"))
	require.Equal(t, "", frameEvidencePhaseField(logs, "upstream_parameter_notice", "event_id"))
	require.Equal(t, cursorNoticeTemplateOnly, frameEvidencePhaseField(logs, "upstream_parameter_notice", "template"))
	phaseLogs := logs.FilterField(zap.String("phase", "upstream_parameter_notice"))
	require.False(t, observedContainsText(phaseLogs, private), "周围普通正文绝不能落进通知日志")
	require.Equal(t, 1, phaseCount(logs, "response_parse_done"))
}

func TestCursorParameterNoticeMalformedEventIDSuffixIsCandidate(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-notice-malformed")
	fields, ok := cursorParameterNoticeEvidenceFields(cursorNoticeBadEventSuffix, cursorTurnUsage{})
	require.True(t, ok)
	cursorPhaseLog(ctx, zap.InfoLevel, "test_notice", "m", fields...)
	require.Equal(t, "false", frameEvidencePhaseField(logs, "test_notice", "notice_is_entire_reply"))
	require.Equal(t, "", frameEvidencePhaseField(logs, "test_notice", "event_id"))
	require.Equal(t, cursorNoticeTemplateOnly, frameEvidencePhaseField(logs, "test_notice", "template"))
}

func TestCursorParameterNoticeOrdinaryReplyHasNoNoticePhase(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-ordinary")
	up := &cursorOutboundUpstream{body: cursorNoticeBody(t, "ordinary PRIVATE-NOTICE-REPLY text")}
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"}})
	svc, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	_, err := executeCursorForTest(t, svc, ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"),
		cursorRLStopRequestBody(false), "cursor-sonnet", false, time.Now())
	require.NoError(t, err)
	require.Contains(t, w.Body.String(), "ordinary PRIVATE-NOTICE-REPLY text")
	require.Zero(t, phaseCount(logs, "upstream_parameter_notice"))
}

func TestCursorAgentRunWireEvidenceResumeActionAndParseFailure(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-wire-helper")

	// 构造真实 wire 形状的 resume AgentRun（action.f2 空 message）。
	state := cursorAppendVarintField(nil, cursorFldStateMode, cursorConversationModeAgent)
	action := cursorAppendBytesField(nil, cursorFldActionResumeAction, nil)
	run := cursorAppendBytesField(nil, cursorFldRunConversationState, state)
	run = cursorAppendBytesField(run, cursorFldRunAction, action)
	run = cursorAppendBytesField(run, cursorFldRunRequestedModel,
		cursorAppendStringField(nil, cursorFldRequestedModelID, "wire-model-id"))
	run = cursorAppendBytesField(run, cursorFldRunMcpTools, nil)
	payload := cursorEncodeEnvelope(cursorAppendBytesField(nil, cursorFldAgentRunRequest, run))

	cursorPhaseLog(ctx, zap.InfoLevel, "test_wire", "m", cursorAgentRunWireEvidenceFields(payload)...)
	require.Equal(t, "resume", frameEvidencePhaseField(logs, "test_wire", "action"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_wire", "agent_run_parse_ok"))
	require.Equal(t, "true", frameEvidencePhaseField(logs, "test_wire", "state_parse_ok"))
	require.Equal(t, "wire-model-id", frameEvidencePhaseField(logs, "test_wire", "model_id"))
	require.Equal(t, "0", frameEvidencePhaseField(logs, "test_wire", "mcp_tool_count"))

	// 解析失败只落 false，不 panic、不返回错误。
	ctx2, logs2 := cursorPhaseObserverCtx(t, "req-wire-bad")
	cursorPhaseLog(ctx2, zap.InfoLevel, "test_wire_bad", "m",
		cursorAgentRunWireEvidenceFields([]byte{0xff, 0xff, 0xff})...)
	require.Equal(t, "false", frameEvidencePhaseField(logs2, "test_wire_bad", "agent_run_parse_ok"))
}
