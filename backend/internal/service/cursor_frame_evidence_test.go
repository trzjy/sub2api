//go:build unit

package service

// 05 证据生成器（CUR-EXEC-03 帧级分类）：**手动触发取证，不等自然流量**。
//
// 用户授权"代码写完你要手动触发取证，不要等用户自然触发"。生产当前零 Cursor 续轮
// 流量（容器刚重建），因此本文件走**生产代码路径**（runCursorAgentRunPump 读循环 +
// cursorPhaseLog 落盘），把每一类坏帧真实喂进去，捕获真实日志行：
//   - response_parse_done 的 outcome / protocol_frame_class / protocol_frame_index
//   - cursor_user_error 的 error_class / protocol_frame_class（经 CursorUserFacingError）
//
// 断言即证据：每个类别都必须产生 `protocol_frame:<class>`，且帧序号符合预期；
// 同时断言用户可见错误码仍是 CUR-EXEC-03（拆帧不影响对外）。

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest/observer"
)

// frameEvidencePhaseField 取某条 phase 事件的某个字段值（字符串化；不存在返回空串）。
func frameEvidencePhaseField(logs *observer.ObservedLogs, phase, key string) string {
	for _, e := range logs.All() {
		if e.ContextMap()["phase"] != phase {
			continue
		}
		if v, ok := e.ContextMap()[key]; ok {
			return fmt.Sprintf("%v", v)
		}
	}
	return ""
}

// frameEvidenceRunPump 把一段原始帧字节喂给真实读循环，返回 observer 抓到的日志。
func frameEvidenceRunPump(t *testing.T, frames []byte) *observer.ObservedLogs {
	t.Helper()
	ctx, logs := cursorPhaseObserverCtx(t, "req-frame-evidence")

	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: []byte{}, status: 0}
	gw, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))
	svc := gw.cursorExecution
	require.NotNil(t, svc)

	resp := &CursorUpstreamResponse{Body: io.NopCloser(bytes.NewReader(frames))}
	parser := cursorNewStreamParser("m")
	parser.attachToolSchemas(cursorSealDiagDeclaredToolDefs(t))
	req := &CursorExecutionRequest{Model: "m", Stream: false}
	output := newCursorSessionOutput()

	session := &cursorSessionRecord{
		id:           "test-frame-evidence",
		spec:         cursorSessionSpec{requestedModel: "m", upstreamModel: "up-m"},
		state:        cursorSessionRunning,
		toolCallIDs:  make(map[string]struct{}),
		finalizeDone: make(chan struct{}),
		logCtx:       ctx,
	}
	session.setHTTPOptions(cursorHTTPOptions{model: "m", stream: false})
	svc.runCursorAgentRunPump(session, resp, req, parser, output)
	return logs
}

// TestFrameEvidenceEnvelopeTruncated 信封截断（第 1 帧就坏）→ frame_envelope + index=1。
func TestFrameEvidenceEnvelopeTruncated(t *testing.T) {
	logs := frameEvidenceRunPump(t, []byte{0x00, 0x00, 0x00}) // 头都不够 5 字节
	require.GreaterOrEqual(t, phaseCount(logs, "response_parse_done"), 1)
	require.Equal(t, "protocol_frame:frame_envelope",
		frameEvidencePhaseField(logs, "response_parse_done", "outcome"))
	require.Equal(t, "1",
		frameEvidencePhaseField(logs, "response_parse_done", "protocol_frame_index"),
		"第一帧就坏，帧序号必须是 1")
	require.Equal(t, "frame_envelope",
		frameEvidencePhaseField(logs, "response_parse_done", "protocol_frame_class"))
}

// TestFrameEvidenceEnvelopePromisedTooMany 声明长度 > 实收 → frame_envelope。
func TestFrameEvidenceEnvelopePromisedTooMany(t *testing.T) {
	// 头声明 100 字节 payload，实际只有 3 字节。
	buf := make([]byte, cursorEnvelopeHeaderSize+3)
	buf[0] = 0
	binary.BigEndian.PutUint32(buf[1:5], 100)
	logs := frameEvidenceRunPump(t, buf)
	require.Equal(t, "protocol_frame:frame_envelope",
		frameEvidencePhaseField(logs, "response_parse_done", "outcome"))
}

// TestFrameEvidenceProtobufBadWire 正常读到第 1 帧，但帧内 protobuf wire type 非法
// → frame_protobuf + index=1（信封 OK、内容坏）。
func TestFrameEvidenceProtobufBadWire(t *testing.T) {
	// 帧 payload = {0x09,...}（field 1, wire type 1 = fixed64，本协议不支持）。
	payload := []byte{0x09, 0, 0, 0, 0, 0, 0, 0, 0}
	logs := frameEvidenceRunPump(t, cursorEncodeEnvelope(payload))
	require.Equal(t, "protocol_frame:frame_protobuf",
		frameEvidencePhaseField(logs, "response_parse_done", "outcome"))
	require.Equal(t, "1",
		frameEvidencePhaseField(logs, "response_parse_done", "protocol_frame_index"))
}

// TestFrameEvidenceSecondFrameBad 第一帧正常正文、第二帧 protobuf 坏
// → frame_protobuf + index=2（证明帧序号真的在数，不是恒为 1）。
func TestFrameEvidenceSecondFrameBad(t *testing.T) {
	var frames []byte
	frames = append(frames, cursorTextDeltaFrame(t, "hello")...) // 第 1 帧：合法正文
	frames = append(frames, cursorEncodeEnvelope([]byte{0x09, 0, 0, 0, 0, 0, 0, 0, 0})...)
	logs := frameEvidenceRunPump(t, frames)
	require.Equal(t, "protocol_frame:frame_protobuf",
		frameEvidencePhaseField(logs, "response_parse_done", "outcome"))
	require.Equal(t, "2",
		frameEvidencePhaseField(logs, "response_parse_done", "protocol_frame_index"),
		"第 1 帧合法、第 2 帧坏，帧序号必须是 2")
}

// TestFrameEvidenceMessageExtraAfterEndStream EndStream 之后再来一帧
// → frame_message（结构合法但时序违约）。
//
// 注意：这条判定只在**批式解析路径** cursorParseStream 里做。流式读循环 next() 见到
// EndStream 即置 done、不再读后续帧（这是既有的正确行为：客户端不会收到多余帧）。
// 故此处直接驱动批式解析器，走生产同一条校验分支。
func TestFrameEvidenceMessageExtraAfterEndStream(t *testing.T) {
	var body []byte
	body = append(body, cursorTestEndStream(t, `{}`)...)    // 第 1 帧：EndStream
	body = append(body, cursorTextDeltaFrame(t, "late")...) // 第 2 帧：多出来的

	parser := cursorNewStreamParser("m")
	_, err := parser.cursorParseStream(body)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCursorProtocol)
	require.Equal(t, "protocol_frame:frame_message", cursorStreamErrClass(err))
	require.Equal(t, "cursor: malformed protocol frame (fail closed): received extra message after EndStreamResponse",
		err.Error())
}

// TestFrameEvidenceEndStreamPayloadNotJSON EndStream payload 非 JSON
// → frame_endstream_payload（终态帧本身畸形，区别于业务报错）。
func TestFrameEvidenceEndStreamPayloadNotJSON(t *testing.T) {
	logs := frameEvidenceRunPump(t, cursorTestEndStream(t, `not-json`))
	require.Equal(t, "protocol_frame:frame_endstream_payload",
		frameEvidencePhaseField(logs, "response_parse_done", "outcome"))
}

// TestFrameEvidenceUserErrorCarriesFrameClass 用户可见错误出口（cursor_user_error）
// 也必须带帧级字段，且对外错误码仍是 CUR-EXEC-03。
func TestFrameEvidenceUserErrorCarriesFrameClass(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-frame-user-error")
	_, _, envErr := cursorReadEnvelopeFrame([]byte{0x00, 0x00}, 0)
	annotated := cursorAnnotateFrame(envErr, 5)

	status, code, _, ok := CursorUserFacingError(ctx, annotated)
	require.True(t, ok)
	require.Equal(t, 502, status)
	require.Equal(t, "CUR-EXEC-03", code, "拆帧不得改变对外错误码")

	// 直接驱动 service 侧出口，确认它真的把帧级字段写进 cursor_user_error。
	account := &Account{ID: 1, Platform: PlatformCursor}
	c := frameEvidenceGinContext(t, ctx)
	_, msg, hit := cursorCompatUserError(c, account, annotated)
	require.True(t, hit)
	require.NotEmpty(t, msg)

	var found bool
	for _, e := range logs.All() {
		if e.Message != "cursor_user_error" {
			continue
		}
		m := e.ContextMap()
		if m["error_class"] == "protocol_frame:frame_envelope" && m["protocol_frame_class"] == "frame_envelope" {
			found = true
			require.EqualValues(t, 5, m["protocol_frame_index"])
		}
	}
	require.True(t, found, "cursor_user_error 必须带帧级字段（error_class=protocol_frame:<class> + protocol_frame_class + protocol_frame_index）")
}

// TestFrameEvidenceNoFrameContentLeak 帧级诊断绝不含帧内容明文。
func TestFrameEvidenceNoFrameContentLeak(t *testing.T) {
	const marker = "SECRET-PAYLOAD-MARKER"
	// 构造一帧：payload 是会被判定为 inline tool call 的正文（含 marker）。
	payload := cursorAppendStringField(nil,
		cursorFldTextDeltaText, marker+" <function_calls>")
	delta := cursorAppendBytesField(nil, cursorFldUpdateTextDelta, payload)
	frame := cursorAgentFrame(cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, delta))

	logs := frameEvidenceRunPump(t, frame)
	require.False(t, observedContainsText(logs, marker),
		"帧级诊断与终态日志都不得把帧内容/正文写进日志（敏感内容零泄漏）")
}

// frameEvidenceGinContext 构造带 observer logger 的 gin context（供 cursorCompatUserError 用）。
func frameEvidenceGinContext(t *testing.T, ctx context.Context) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request = c.Request.WithContext(ctx)
	return c
}
