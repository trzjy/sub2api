//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// flusherResponseRecorder 在 httptest.ResponseRecorder 之上实现 http.Flusher，
// 以满足 handleCCFailoverExhausted 流内错误帧的 flush 路径
// （httptest.NewRecorder 默认不实现 http.Flusher，会导致帧不被写出）。
type flusherResponseRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flusherResponseRecorder) Flush() {}

func newFlusherTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(&flusherResponseRecorder{rec})
	return c, rec
}

// TestM1bHandleCCFailoverExhausted_StreamStartedWritesErrorFrame 覆盖派发单 M1-b 的核心验收点：
// 流已开始 + 预算耗尽时，客户端必须收到一个协议合规的流内错误帧（不是零字节 EOF），
// 且该帧不含 content/delta 污染、不含任何类取消触发词、携带可区分的错误类型标识。
func TestM1bHandleCCFailoverExhausted_StreamStartedWritesErrorFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, rec := newFlusherTestContext()
	h := &GatewayHandler{}

	// 预算耗尽：构造 FailoverExhausted 错误（首字节护栏超时，上游从未回状态码），
	// 流已开始（心跳已提交 SSE 200）。
	lastErr := &service.UpstreamFailoverError{
		StatusCode: http.StatusGatewayTimeout,
	}

	h.handleCCFailoverExhausted(c, lastErr, true)

	body := rec.Body.String()

	// (a) 客户端必须收到流内错误帧：非空且是 SSE data 帧。
	if body == "" {
		t.Fatalf("M1b(a): expected a stream-in error frame to be written, got empty body (silent EOF not fixed)")
	}
	if !strings.Contains(body, "data:") {
		t.Fatalf("M1b(a): expected an SSE data frame, got: %q", body)
	}

	// (c) 帧必须携带可区分的错误类型标识（等于 service.UpstreamFirstByteTimeoutType）。
	if !strings.Contains(body, service.UpstreamFirstByteTimeoutType) {
		t.Fatalf("M1b(c): error frame missing distinguishable type %q: %q", service.UpstreamFirstByteTimeoutType, body)
	}

	// (b) 帧文案不得含任何 forbidden 子串（大小写不敏感）。
	// 来源：客户端 isCancelledLikeErrorMessage 规则 + 派发单禁用词表。
	forbidden := []string{
		"abort",
		"aborted",
		"aborterror",
		"cancel",
		"cancelled",
		"canceled",
		"operation was aborted",
		"empty response output from model",
	}
	lower := strings.ToLower(body)
	for _, w := range forbidden {
		if strings.Contains(lower, strings.ToLower(w)) {
			t.Fatalf("M1b(b): error frame contains forbidden substring %q: %q", w, body)
		}
	}

	// (d) 帧不得污染 assistant 内容（不含 content / delta 字段）。
	if strings.Contains(lower, "\"content\"") || strings.Contains(lower, "\"delta\"") {
		t.Fatalf("M1b(d): error frame must not contain assistant content/delta fields: %q", body)
	}

	// 文案必须取 SSOT（M1-e 的流内错误帧文案），证明未自写裸字符串。
	if !strings.Contains(body, infraerrors.UpstreamFirstByteTimeoutStreamFrame) {
		t.Fatalf("M1b: error frame message must reference SSOT %q: %q", infraerrors.UpstreamFirstByteTimeoutStreamFrame, body)
	}
}

// TestM1bHandleCCFailoverExhausted_StreamNotStartedWritesJSON 守卫既有行为：
// 流未开始时仍走正常 JSON 错误体路径（本次不动），不得写出 SSE data 错误帧。
func TestM1bHandleCCFailoverExhausted_StreamNotStartedWritesJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, rec := newFlusherTestContext()
	h := &GatewayHandler{}

	lastErr := &service.UpstreamFailoverError{
		StatusCode: http.StatusGatewayTimeout,
	}

	h.handleCCFailoverExhausted(c, lastErr, false)

	body := rec.Body.String()
	if strings.Contains(body, "data:") {
		t.Fatalf("M1b regression: stream-not-started path must not emit SSE data frame: %q", body)
	}
	if !strings.Contains(body, "error") {
		t.Fatalf("M1b regression: stream-not-started path must emit JSON error body: %q", body)
	}
}
