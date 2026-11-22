package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 用例自带状态清理与 ctx 构造，避免依赖其它测试文件里的辅助函数。
func ctxWarnResetState(t *testing.T) {
	t.Helper()
	contextWarnMu.Lock()
	contextWarnCache.Flush()
	contextWarnMu.Unlock()
}

func ctxWarnTestContext() context.Context {
	return logger.IntoContext(context.Background(), zap.NewNop())
}

// ctxWarnIndexOfType 在整段 SSE 里找指定 type 的 data 行，返回其 index。
func ctxWarnIndexOfType(t *testing.T, block, typ string) (float64, bool) {
	t.Helper()
	for _, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		if got, _ := obj["type"].(string); got != typ {
			continue
		}
		idx, ok := obj["index"].(float64)
		return idx, ok
	}
	t.Fatalf("no data line for type %q in:\n%s", typ, block)
	return 0, false
}

func TestContextWarnStreamStateObserveUsageAndTakeInjection(t *testing.T) {
	ctxWarnResetState(t)
	st := newContextWarnStreamState("sess-ctxwarn-hit", 0)

	// 三类 token 求和命中 800000（band3）。
	st.observeUsage(ctxWarnTestContext(), &ClaudeUsage{
		InputTokens:              500000,
		CacheReadInputTokens:     200000,
		CacheCreationInputTokens: 100000,
	})

	wantText := "⚠️ [corealgos] 本会话上下文已达 80 万，已经超过可靠区间，可能返回异常短回复；请压缩上下文或开新会话接力。"
	wantBlock := BuildContextWarningSSE(wantText)
	if st.block != wantBlock {
		t.Fatalf("block mismatch:\n got: %q\nwant: %q", st.block, wantBlock)
	}
	if first := st.takeInjection(); first != wantBlock {
		t.Fatalf("first takeInjection:\n got: %q\nwant: %q", first, wantBlock)
	}
	if second := st.takeInjection(); second != "" {
		t.Fatalf("second takeInjection must be empty, got %q", second)
	}
}

func TestContextWarnStreamStateBelowThreshold(t *testing.T) {
	ctxWarnResetState(t)
	st := newContextWarnStreamState("sess-ctxwarn-low", 0)
	st.observeUsage(ctxWarnTestContext(), &ClaudeUsage{InputTokens: 649999})

	if st.block != "" {
		t.Fatalf("below threshold must not produce block: %q", st.block)
	}
	if got := st.takeInjection(); got != "" {
		t.Fatalf("below threshold takeInjection must be empty: %q", got)
	}
	if st.needShift() {
		t.Fatal("below threshold needShift must be false")
	}
}

func TestContextWarnStreamStateDecidedOnce(t *testing.T) {
	ctxWarnResetState(t)
	ctx := ctxWarnTestContext()
	st := newContextWarnStreamState("sess-ctxwarn-once", 0)

	st.observeUsage(ctx, &ClaudeUsage{InputTokens: 800000})
	band3Block := st.block
	if band3Block == "" {
		t.Fatal("first observeUsage must produce block")
	}
	// 第二次喂更高水位（band4）：若重新判定，block 会变成 85 万版本。
	st.observeUsage(ctx, &ClaudeUsage{InputTokens: 850000})
	if st.block != band3Block {
		t.Fatalf("observeUsage must decide only once, block changed:\n got: %q\nwant: %q", st.block, band3Block)
	}
}

func TestContextWarnStreamStateNeedShift(t *testing.T) {
	ctxWarnResetState(t)
	st := newContextWarnStreamState("sess-ctxwarn-shift", 0)

	if st.needShift() {
		t.Fatal("initial needShift must be false")
	}
	st.observeUsage(ctxWarnTestContext(), &ClaudeUsage{InputTokens: 800000})
	if st.needShift() {
		t.Fatal("needShift must stay false before the injection is written")
	}
	if got := st.takeInjection(); got == "" {
		t.Fatal("expected injection block after hit")
	}
	if !st.needShift() {
		t.Fatal("needShift must be true after injection")
	}
}

func TestContextWarnStreamStateShiftEventBlock(t *testing.T) {
	st := newContextWarnStreamState("s", 0)
	block := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":800000}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","index":9,"delta":{"stop_reason":"end_turn"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	out := st.shiftEventBlock(block)

	inLines := strings.Split(block, "\n")
	outLines := strings.Split(out, "\n")
	if len(inLines) != len(outLines) {
		t.Fatalf("line count changed: %d -> %d", len(inLines), len(outLines))
	}
	for i := range inLines {
		if inLines[i] == "" && outLines[i] != "" {
			t.Fatalf("line %d: empty line changed to %q", i, outLines[i])
		}
		if strings.HasPrefix(inLines[i], "event:") && outLines[i] != inLines[i] {
			t.Fatalf("line %d: event line changed %q -> %q", i, inLines[i], outLines[i])
		}
	}

	for _, typ := range []string{"content_block_start", "content_block_delta", "content_block_stop"} {
		idx, ok := ctxWarnIndexOfType(t, out, typ)
		if !ok || idx != 1 {
			t.Fatalf("%s index = %v (present=%v), want 1", typ, idx, ok)
		}
	}
	if idx, ok := ctxWarnIndexOfType(t, out, "message_delta"); !ok || idx != 9 {
		t.Fatalf("message_delta index = %v (present=%v), want 9", idx, ok)
	}
	if !strings.Contains(out, `data: {"type":"message_stop"}`) {
		t.Fatalf("message_stop line changed:\n%s", out)
	}
	if !strings.Contains(out, `data: {"type":"message_start","message":{"usage":{"input_tokens":800000}}}`) {
		t.Fatalf("message_start line changed:\n%s", out)
	}
}

// ---------- 流结尾追加语义：透传路径集成用例 ----------

// ctxWarnPassthroughSSE 把若干行拼成一份以空行收尾的 SSE 流。
func ctxWarnPassthroughSSE(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

// ctxWarnRunPassthroughStream 用一份 SSE 流驱动透传流式处理器，返回写给客户端的原文。
func ctxWarnRunPassthroughStream(t *testing.T, sse string, sessionHash string, accountID int64) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx := WithRequestSizeSessionHash(ctxWarnTestContext(), sessionHash)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	s := &GatewayService{}
	result, err := s.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, &Account{ID: accountID}, time.Now(), "claude-test")
	if err != nil {
		t.Fatalf("stream handler error: %v (result=%+v)", err, result)
	}
	return rec.Body.String()
}

func TestContextWarnPassthroughStreamAppendsAtTail(t *testing.T) {
	ctxWarnResetState(t)
	sse := ctxWarnPassthroughSSE(
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":800000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	)
	out := ctxWarnRunPassthroughStream(t, sse, "sess-pt-stream-tail", 9001)

	// 命中提醒，三个事件用"已见最大块 0 + 1 = 1"作为索引。
	if !strings.Contains(out, "已达 80 万") {
		t.Fatalf("warning text missing in output:\n%s", out)
	}
	for _, want := range []string{
		`{"type":"content_block_start","index":1,`,
		`{"type":"content_block_delta","index":1,`,
		`{"type":"content_block_stop","index":1}`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("warning block with %q missing in output:\n%s", want, out)
		}
	}
	// 上游事件零改写：content block 索引保持 0，message_start 原样透传。
	for _, want := range []string{
		`data: {"type":"content_block_start","index":0,`,
		`data: {"type":"content_block_delta","index":0,`,
		`data: {"type":"content_block_stop","index":0}`,
		`"id":"msg_1"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("upstream line %q rewritten or missing:\n%s", want, out)
		}
	}
	// 顺序：提醒位于最后一个上游 content block 之后、message_stop data 行之前。
	posStop := strings.LastIndex(out, `data: {"type":"content_block_stop","index":0}`)
	posWarn := strings.Index(out, "已达 80 万")
	posMsgStop := strings.Index(out, `data: {"type":"message_stop"}`)
	if posStop < 0 || posWarn < 0 || posMsgStop < 0 {
		t.Fatalf("missing markers: stop=%d warn=%d msgstop=%d\n%s", posStop, posWarn, posMsgStop, out)
	}
	if !(posStop < posWarn && posWarn < posMsgStop) {
		t.Fatalf("warning not appended at stream tail: stop=%d warn=%d msgstop=%d\n%s", posStop, posWarn, posMsgStop, out)
	}
}

func TestContextWarnPassthroughStreamSkipsToolUse(t *testing.T) {
	ctxWarnResetState(t)
	sse := ctxWarnPassthroughSSE(
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":800000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"x"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	)
	const sess = "sess-pt-stream-tooluse"
	const accountID int64 = 9002
	out := ctxWarnRunPassthroughStream(t, sse, sess, accountID)

	if strings.Contains(out, "已达 80 万") {
		t.Fatalf("tool_use turn must not inject warning:\n%s", out)
	}
	// 带 tool_use 的回合不推进水位：同会话同档仍应命中。
	if got := DecideContextWarning(ctxWarnTestContext(), sess, accountID, 800000); got.Band != contextWarnBand3 {
		t.Fatalf("tool_use turn must not advance watermark, got %+v", got)
	}
}

// ctxWarnRunPassthroughNonStream 用一份 JSON 响应驱动透传非流式处理器。
func ctxWarnRunPassthroughNonStream(t *testing.T, bodyStr string, sessionHash string, accountID int64) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx := WithRequestSizeSessionHash(ctxWarnTestContext(), sessionHash)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(bodyStr)),
	}
	s := &GatewayService{}
	usage, err := s.handleNonStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, &Account{ID: accountID})
	if err != nil {
		t.Fatalf("non-stream handler error: %v", err)
	}
	if usage == nil || usage.InputTokens != 800000 {
		t.Fatalf("usage mismatch: %+v", usage)
	}
	return rec.Body.String()
}

func TestContextWarnPassthroughNonStreamAppendsAtTail(t *testing.T) {
	ctxWarnResetState(t)
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":800000,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	out := ctxWarnRunPassthroughNonStream(t, body, "sess-pt-nonstream-tail", 9003)

	var obj struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("bad output json: %v\n%s", err, out)
	}
	if len(obj.Content) != 2 {
		t.Fatalf("want 2 content blocks (original + warning tail), got %d:\n%s", len(obj.Content), out)
	}
	if obj.Content[0].Text != "hello" {
		t.Fatalf("original block no longer at head: %+v", obj.Content[0])
	}
	if obj.Content[1].Type != "text" || !strings.Contains(obj.Content[1].Text, "已达 80 万") {
		t.Fatalf("warning block not appended at tail: %+v", obj.Content[1])
	}
}

func TestContextWarnPassthroughNonStreamSkipsToolUse(t *testing.T) {
	ctxWarnResetState(t)
	body := `{"id":"msg_2","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"tu_2","name":"x","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":800000,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	const sess = "sess-pt-nonstream-tooluse"
	const accountID int64 = 9004
	out := ctxWarnRunPassthroughNonStream(t, body, sess, accountID)

	if strings.Contains(out, "已达 80 万") {
		t.Fatalf("tool_use turn must not append warning:\n%s", out)
	}
	if got := DecideContextWarning(ctxWarnTestContext(), sess, accountID, 800000); got.Band != contextWarnBand3 {
		t.Fatalf("tool_use turn must not advance watermark, got %+v", got)
	}
	var obj struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("bad output json: %v\n%s", err, out)
	}
	if len(obj.Content) != 2 {
		t.Fatalf("content changed on tool_use turn: %+v\n%s", obj.Content, out)
	}
}
