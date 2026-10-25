//go:build unit

package service

// 派发单 B8b（外审建议 S1 收尾）：把封口失败原因写进终态日志安全字段 seal_blocked_reason。
//
// 本文件只验证 D1：构造一个"未封口即终止"的会话（started + mcp_args 委托但缺 listed，
// 随后 EOF），直接驱动 runCursorAgentRunPump 走到终止分支，断言：
//   - response_parse_done（§3.1，读循环终止分支，取 stream.lastSealErrReason()）带
//     seal_blocked_reason 且值含 "listed"——这是 D1 明确要求的"终止路径日志"断言；
//   - §3.2 落地证据：pump 终止前把同一原因搬运到 session.sealBlockedReason，供
//     settleCursorSession 在 session_settle_skipped（usage_missing: turn_ended_missing）
//     处随同一安全字段写出（见 §3.2 改动）。
//
// 取证方式：仓内已有 zap observer 日志断言辅助（cursor_request_phase_test.go 的
// cursorPhaseObserverCtx / phaseCount / observedContainsText），response_parse_done 走
// 请求级 observer 故可直接断言；§3.2 的 session_settle_skipped 以 context.Background()
// 走全局 logger（结算在请求作用域之外，属设计），不在请求级 observer 范围内，故以
// "值已正确搬运到 record" 佐证其落点，而非退化到仅断言 stream.lastSealErrReason() 非空。

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest/observer"
)

// sealReasonFromPhase 取某阶段事件（phase 字段）的 seal_blocked_reason 字段值（字符串化）。
// 若该阶段事件不存在或不带该字段，返回空串。
func sealReasonFromPhase(t *testing.T, logs *observer.ObservedLogs, phase string) string {
	t.Helper()
	for _, e := range logs.All() {
		if e.ContextMap()["phase"] != phase {
			continue
		}
		v, ok := e.ContextMap()["seal_blocked_reason"]
		if !ok {
			return ""
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// TestCursorSealLogWritesBlockedReasonOnUnsealedTermination 覆盖 D1：未封口即终止的会话，
// 终止路径两条阶段日志都必须带 seal_blocked_reason，且值含 "listed" 具体阻塞类别。
func TestCursorSealLogWritesBlockedReasonOnUnsealedTermination(t *testing.T) {
	// ① observer logger 挂到会话 logCtx，使 response_parse_done 经 phaseContext() 落盘到 observer。
	ctx, logs := cursorPhaseObserverCtx(t, "req-seal-log-1")

	// ② 真实执行部件（sessions 索引已初始化，finalizer 可安全跑完）。
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: []byte{}, status: 0}
	gw, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))
	svc := gw.cursorExecution
	require.NotNil(t, svc, "执行部件必须可用")

	// ③ 帧序列：started + mcp_args 委托（有 delegation 但缺 listed）→ 应触发 sealable 失败；
	//    随后 EOF → pump 终止（不交接、不提前结束，沿用 B8 既有帧辅助）。
	argsA := cursorToolTestBuildMcpArgs("call_a", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Beijing"}})

	var frames []byte
	frames = append(frames, cursorSealDiagEnvelope(cursorSealDiagStartedFrame(t, "call_a", "mc1"))...)
	frames = append(frames, cursorSealDiagEnvelope(cursorToolTestExecFrame(true, argsA))...)

	resp := &CursorUpstreamResponse{Body: io.NopCloser(bytes.NewReader(frames))}
	parser := cursorNewStreamParser("m")
	req := &CursorExecutionRequest{Model: "m", Stream: false}
	output := newCursorSessionOutput()

	session := &cursorSessionRecord{
		id:           "test-seal-log",
		spec:         cursorSessionSpec{requestedModel: "m", upstreamModel: "up-m"},
		state:        cursorSessionRunning,
		toolCallIDs:  make(map[string]struct{}),
		finalizeDone: make(chan struct{}),
		logCtx:       ctx,
	}
	session.setHTTPOptions(cursorHTTPOptions{model: "m", stream: false})

	// ④ 直接驱动读循环（同步跑完，含 finalizer）。
	svc.runCursorAgentRunPump(session, resp, req, parser, output)

	// ⑤ 断言：response_parse_done 带 seal_blocked_reason 且值含 listed（D1 终止路径日志）。
	require.GreaterOrEqual(t, phaseCount(logs, "response_parse_done"), 1,
		"未封口终止必须记 response_parse_done")
	parseDoneReason := sealReasonFromPhase(t, logs, "response_parse_done")
	require.Contains(t, parseDoneReason, "listed",
		"response_parse_done 的 seal_blocked_reason 必须含具体阻塞类别 listed")

	// ⑥ §3.2 落地证据：pump 终止前把同一原因搬运到 session.sealBlockedReason，
	// 供 settleCursorSession 在 session_settle_skipped 处随同一安全字段写出。
	// （该阶段事件以 context.Background() 走全局 logger，不在请求级 observer 范围内，
	//   故此处断言"值已正确搬运"而非直接读该条日志；写字段的代码见 §3.2 改动。）
	require.NotEmpty(t, session.sealBlockedReason,
		"封口失败原因必须被搬运到 session.sealBlockedReason 供结算路径复盘")
	require.Contains(t, session.sealBlockedReason, "listed",
		"搬运到记录的封口失败原因必须含具体阻塞类别 listed")
}
