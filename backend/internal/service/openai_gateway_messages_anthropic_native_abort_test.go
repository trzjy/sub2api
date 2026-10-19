//go:build unit

package service

// D7 派发单定向回归：国产原生 Anthropic 直通路径在「响应已开始写出 + 客户端仍
// 连接」条件下，三条流内错误路径必须向客户端实际下发 Anthropic SSE error 事件
// （帧格式与 repGuardAbort 先例一致），而非只返回 Go error 让下游（Claude Code）
// 自行合成英文 "Connection lost mid-response"。
//
// 测试断言聚焦「客户端实际收到的字节流」：解析 httptest recorder 捕获的写出，
// 校验其中是否出现 event: error 及对应中文 message 常量。零发送路径（未提交 /
// sawTerminalEvent / context 取消 / 客户端断开）同样断言不出现 error 事件。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 单条非终态内容事件：用于让响应进入「已开始写出」状态，且不构成 sawTerminalEvent。
const abortTestContentEvent = "event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"

// abortTestPartialContentEvent 与 abortTestContentEvent 仅差结尾事件分隔空行：
// event: / data: 行已写出但事件未闭合（inPartialEvent=true）。用于复现 D7b 缺陷——
// 不闭合时 deliverStreamAbortError 追加的 error 帧会并入该未完成事件，被 SSE 解析器
// 合并进同一事件，中文错误无法作为独立事件解析。
const abortTestPartialContentEvent = "event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n"

// failingWriter 在第 failAfter 次成功 Write 之后开始返回错误，模拟客户端中途断开
// （writeStreamLine 命中写失败 → clientDisconnected=true）。
type failingWriter struct {
	rec       *httptest.ResponseRecorder
	failAfter int
	writes    int
}

func (w *failingWriter) Header() http.Header { return w.rec.Header() }
func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, io.ErrClosedPipe
	}
	return w.rec.Write(p)
}
func (w *failingWriter) WriteHeader(code int) { w.rec.WriteHeader(code) }
func (w *failingWriter) Flush()               {}

// errAfterContentReader 先交付 data 中的内容，随后返回 err（模拟上游读错误）。
type errAfterContentReader struct {
	data string
	sent bool
	err  error
}

func (r *errAfterContentReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, []byte(r.data)), nil
	}
	if r.err != nil {
		return 0, r.err
	}
	return 0, io.EOF
}

func (r *errAfterContentReader) Close() error { return nil }

// stallReader 先交付 data 中的内容，随后阻塞直到 release 关闭（用于触发间隔超时，
// 同时保证内容已写出使响应处于「已开始」状态）。reader 在 release 后返回 EOF，
// 让被阻塞的扫描 goroutine 正常退出，避免测试态 goroutine 泄漏。
type stallReader struct {
	data    string
	sent    bool
	release chan struct{}
}

func (r *stallReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, []byte(r.data)), nil
	}
	<-r.release
	return 0, io.EOF
}

// newAbortTestCtx 构造一个带 httptest recorder 的 gin 测试上下文，返回上下文与
// recorder（recorder 用于读取客户端实际收到的写出）。
func newAbortTestCtx(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{}`)))
	return c, rec
}

// runAbortStream 构造服务并直接驱动流式处理函数，返回函数的 error 返回值。
// gin 测试 Writer 会将写出直接透传到底层 httptest.ResponseRecorder，调用方凭
// recorder 读取客户端实际收到的字节流。
func runAbortStream(t *testing.T, body io.ReadCloser, c *gin.Context, streamIntervalSec int) error {
	t.Helper()
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	if streamIntervalSec > 0 {
		svc.cfg.Gateway.StreamDataIntervalTimeout = streamIntervalSec
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	account := nativeAnthropicTestAccount()
	startTime := time.Now()
	_, err := svc.handleNativeAnthropicStreamingResponse(
		context.Background(), resp, c, account,
		"k3", "k3", "k3", nil, startTime,
	)
	return err
}

func TestAnthropicNativeStreamAbortMissingTerminalEventSendsError(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	// 已输出部分内容（非终态），随后干净 EOF：触发 !sawTerminalEvent 分支。
	c, rec := newAbortTestCtx(t)
	body := io.NopCloser(strings.NewReader(abortTestContentEvent))

	err := runAbortStream(t, body, c, 0)
	require.Error(t, err)
	assertSSEErrorEvent(t, rec.Body.String(), "upstream_stream_truncated", infraerrors.UpstreamStreamEndedEarly)
}

// D7b 回归：上游写出未闭合 content 事件（最后一个 data: 行无结尾空行，
// inPartialEvent=true）后干净 EOF。修复前 error 帧会并入未闭合事件；修复后
// deliverStreamAbortError 先写空行闭合事件，error 帧应为独立可解析事件。
func TestAnthropicNativeStreamAbortPartialEventClosedBeforeError(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	c, rec := newAbortTestCtx(t)
	body := io.NopCloser(strings.NewReader(abortTestPartialContentEvent))

	err := runAbortStream(t, body, c, 0)
	require.Error(t, err)
	assertIndependentSSEErrorEvent(t, rec.Body.String(), infraerrors.UpstreamStreamEndedEarly)
}

// D7b 回归（读错同型）：未闭合 content 事件后上游读错误，同样应先闭合事件再发帧，
// 使 error 帧为独立事件。
func TestAnthropicNativeStreamAbortPartialEventReadError(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	c, rec := newAbortTestCtx(t)
	body := &errAfterContentReader{
		data: abortTestPartialContentEvent,
		err:  errors.New("simulated upstream read failure"),
	}

	err := runAbortStream(t, body, c, 0)
	require.Error(t, err)
	assertIndependentSSEErrorEvent(t, rec.Body.String(), infraerrors.UpstreamStreamInterrupted)
}

func TestAnthropicNativeStreamAbortReadErrorSendsError(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	// 已输出部分内容，随后上游读错误：触发 ev.err 通用分支（classify 归类）。
	c, rec := newAbortTestCtx(t)
	body := &errAfterContentReader{
		data: abortTestContentEvent,
		err:  errors.New("simulated upstream read failure"),
	}

	err := runAbortStream(t, body, c, 0)
	require.Error(t, err)
	assertSSEErrorEvent(t, rec.Body.String(), "upstream_stream_read_error", infraerrors.UpstreamStreamInterrupted)
}

func TestAnthropicNativeStreamAbortIntervalTimeoutSendsError(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	// 已输出部分内容后上游停滞：触发间隔超时（!clientDisconnected）分支。
	stall := &stallReader{
		data:    abortTestContentEvent,
		release: make(chan struct{}),
	}
	c, rec := newAbortTestCtx(t)
	body := io.NopCloser(stall)

	err := runAbortStream(t, body, c, 1)
	require.Error(t, err)
	assertSSEErrorEvent(t, rec.Body.String(), "upstream_timeout", infraerrors.UpstreamStreamIntervalTimeout)
	close(stall.release) // 释放被阻塞的扫描 goroutine
}

// R4 取消门禁回归（intervalCh 分支）：非终态内容已写出、请求 context 已取消，
// 且因无写失败 clientDisconnected 未置位，上游随后停滞触发间隔超时。修复前
// intervalCh 分支只核对 sawTerminalEvent / CN 首字节 / clientDisconnected，会在
// 客户端已取消的情况下补发 error 帧；修复后复用 EOF 缺终态分支同款
// c.Request.Context().Err() 检查：仅跳过投递，return 错误语义不变。
// 客户端字节流不应出现 event: error。
func TestAnthropicNativeStreamAbortCanceledContextIntervalTimeoutNoError(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{}`)))
	req = req.WithContext(canceledCtx)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	// 非终态内容写出（clientOutputStarted=true，sawTerminalEvent=false）后阻塞，
	// 使连接空转以触发间隔超时（interval=1s）。
	stall := &stallReader{
		data:    abortTestContentEvent,
		release: make(chan struct{}),
	}
	body := io.NopCloser(stall)

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- runAbortStream(t, body, c, 1)
	}()

	select {
	case err := <-doneCh:
		// 既有返回语义不变：间隔超时仍返回 error（仅跳过 error 帧投递）。
		require.Error(t, err, "取消场景下间隔超时应保持既有返回 error 语义")
	case <-time.After(5 * time.Second):
		close(stall.release)
		t.Fatal("间隔超时未在预期时间内返回（取消门禁可能改变了分支走向）")
	}
	close(stall.release) // 释放被阻塞的扫描 goroutine

	// 场景前提校验：内容已写出（响应已开始），证明走的是 interval 超时裁决路径。
	require.Contains(t, rec.Body.String(), "content_block_delta",
		"测试场景前提不成立：非终态内容未写出，实际输出: %q", rec.Body.String())
	require.NotContains(t, rec.Body.String(), "event: error",
		"请求 context 取消后 interval 超时不得补发 error 事件，实际输出: %q", rec.Body.String())
	require.NotContains(t, rec.Body.String(), infraerrors.UpstreamStreamIntervalTimeout,
		"请求 context 取消后不得下发间隔超时中文错误，实际输出: %q", rec.Body.String())
}

func TestAnthropicNativeStreamAbortClientDisconnectedSendsNothing(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	// 客户端中途写失败 → clientDisconnected=true，且此前已有内容写出（clientOutputStarted）。
	rec := httptest.NewRecorder()
	fw := &failingWriter{rec: rec, failAfter: 2}
	c, _ := gin.CreateTestContext(fw)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{}`)))

	// 交付一个内容事件后返回读错误：clientOutputStarted 置位后写失败置 clientDisconnected。
	body := &errAfterContentReader{
		data: abortTestContentEvent,
		err:  errors.New("simulated upstream read failure"),
	}

	_ = runAbortStream(t, body, c, 0)
	require.NotContains(t, rec.Body.String(), "event: error",
		"客户端已断开时不得下发 error 事件，实际输出: %q", rec.Body.String())
}

func TestAnthropicNativeStreamAbortNotCommittedSendsNothing(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	// 响应未提交（首字节未写出）即遇读错误：clientOutputStarted=false → 不发送，
	// 交给上层 ensure* JSON 错误（已中文化）。
	c, rec := newAbortTestCtx(t)
	body := &errAfterContentReader{
		data: "",
		err:  errors.New("simulated upstream read failure before any output"),
	}

	_ = runAbortStream(t, body, c, 0)
	require.NotContains(t, rec.Body.String(), "event: error",
		"响应未提交时不得下发 error 事件，实际输出: %q", rec.Body.String())
}

func TestAnthropicNativeStreamAbortAfterTerminalSendsNothing(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	// 已正常终态（message_stop）后尾随读错误：sawTerminalEvent=true → 不发送。
	terminal := abortTestContentEvent +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	c, rec := newAbortTestCtx(t)
	body := &errAfterContentReader{
		data: terminal,
		err:  errors.New("trailing read error after terminal"),
	}

	err := runAbortStream(t, body, c, 0)
	// sawTerminalEvent 分支返回 nil error（流已正常终止）。
	require.NoError(t, err)
	require.NotContains(t, rec.Body.String(), "event: error",
		"流已终态后尾随读错误不得下发 error 事件，实际输出: %q", rec.Body.String())
}

func TestAnthropicNativeStreamAbortContextCanceledSendsNothing(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	// 上下文取消（客户端主动终止）：不发送 error 事件。
	c, rec := newAbortTestCtx(t)
	body := &errAfterContentReader{
		data: abortTestContentEvent,
		err:  context.Canceled,
	}

	_ = runAbortStream(t, body, c, 0)
	require.NotContains(t, rec.Body.String(), "event: error",
		"上下文取消时不得下发 error 事件，实际输出: %q", rec.Body.String())
}

// B1 终态门禁回归：上游发完整终态事件（message_stop，带尾随空行）后保持连接
// 不再发任何数据，间隔超时（StreamDataIntervalTimeout）被触发。修复前 interval
// 超时会在成功终态之后补发伪 error 帧；修复后 sawTerminalEvent 门禁令 intervalCh
// 分支 continue，客户端字节流不应出现 event: error。
func TestAnthropicNativeStreamAbortTerminalStallNoError(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	terminal := abortTestContentEvent +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	// 交付终态内容后阻塞，使连接空转以触发间隔超时；测试末关闭 release 释放
	// 被阻塞的扫描 goroutine，避免 goroutine 泄漏。
	stall := &stallReader{
		data:    terminal,
		release: make(chan struct{}),
	}
	c, rec := newAbortTestCtx(t)
	body := io.NopCloser(stall)

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- runAbortStream(t, body, c, 1)
	}()

	// 等待至少一次 interval tick（interval=1s）触发：终态后停滞，interval 超时
	// 不得补发 error。随后释放 reader 让流以正常终态（EOF + sawTerminalEvent）结束。
	time.Sleep(2500 * time.Millisecond)
	close(stall.release)

	select {
	case err := <-doneCh:
		require.NoError(t, err, "终态后尾随 EOF 应正常结束（无 error 帧）")
	case <-time.After(5 * time.Second):
		t.Fatal("流式处理未在 stall 释放后返回（可能 goroutine 泄漏 / 未收敛）")
	}
	require.NotContains(t, rec.Body.String(), "event: error",
		"终态后停滞 interval 超时不得补发 error 事件，实际输出: %q", rec.Body.String())
}

// B1 取消门禁回归：请求 context 先取消（客户端断开 / 服务端终止），reader 随后
// 返回 EOF（不是 context.Canceled）。修复前该组合会补发 error 帧；修复后将读错
// 分支的 context 排除语义扩展到「context 已取消而 reader 返回 EOF」的组合：仅跳过
// 发帧，return 错误语义保持既有不变。客户端字节流不应出现 event: error。
func TestAnthropicNativeStreamAbortCanceledContextEOFNoError(t *testing.T) {
	restore := setRepetitionGuardOff(t)
	defer restore()

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{}`)))
	req = req.WithContext(canceledCtx)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	// 已输出部分内容（非终态，clientOutputStarted=true）后 reader 返回 EOF
	// （err=nil → io.EOF，非 context.Canceled），与读错分支 Canceled 排除互补。
	body := &errAfterContentReader{
		data: abortTestContentEvent,
		err:  nil,
	}

	_ = runAbortStream(t, body, c, 0)
	require.NotContains(t, rec.Body.String(), "event: error",
		"请求 context 取消后 reader 返回 EOF 不得下发 error 事件，实际输出: %q", rec.Body.String())
}

// assertSSEErrorEvent 断言客户端实际收到的写出中包含指定 code / message 的
// Anthropic SSE error 事件（与 buildAnthropicStreamErrorCodeSSE 帧格式一致）。
func assertSSEErrorEvent(t *testing.T, out, wantCode, wantMessage string) {
	t.Helper()
	require.Contains(t, out, "event: error",
		"客户端实际收到的流中缺少 event: error，实际输出: %q", out)
	require.Contains(t, out, `"code":"`+wantCode+`"`,
		"error 事件 code 不匹配，实际输出: %q", out)
	require.Contains(t, out, `"message":"`+wantMessage+`"`,
		"error 事件 message 未中文化或不符，实际输出: %q", out)
}

// sseEvent 是从客户端实际字节流中按事件边界切出的一段（event 类型 + 合并 data）。
type sseEvent struct {
	eventType string
	data      string
}

// parseSSEEvents 将客户端实际收到的字节流按 SSE 事件边界（"\n\n"）切分为事件，
// 并把每段中的 event: / data: 字段解析出来。data 多行按 SSE 规范以 "\n" 连接。
// 用于校验 error 帧是否作为独立事件出现（D7b：不闭合时会并入前一个未完成事件）。
func parseSSEEvents(t *testing.T, out string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(out, "\n\n") {
		block = strings.TrimPrefix(block, "\n")
		if strings.TrimSpace(block) == "" {
			continue
		}
		ev := sseEvent{}
		var dataLines []string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event:"); ok {
				ev.eventType = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(line, "data:"); ok {
				dataLines = append(dataLines, strings.TrimSpace(v))
			}
		}
		ev.data = strings.Join(dataLines, "\n")
		events = append(events, ev)
	}
	return events
}

// assertIndependentSSEErrorEvent 断言字节流中 error 帧为独立可解析事件（D7b 核心
// 验收）：含恰好一个 event: error 事件，其 data 为可解析 JSON 且 error.message 为
// 指定中文常量。若 data: 行未闭合，error 帧会并入前事件导致此处要么找不到事件、
// 要么事件 data 被合并而无法独立解析为期望 JSON。
func assertIndependentSSEErrorEvent(t *testing.T, out, wantMessage string) {
	t.Helper()
	events := parseSSEEvents(t, out)

	var errorEvents []sseEvent
	for _, ev := range events {
		if ev.eventType == "error" {
			errorEvents = append(errorEvents, ev)
		}
	}
	require.Len(t, errorEvents, 1,
		"字节流应恰好含一个 event: error 独立事件（D7b 事件边界），实际事件数=%d，输出=%q",
		len(errorEvents), out)
	require.NotEmpty(t, errorEvents[0].data, "error 事件 data 不应为空，输出=%q", out)

	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(errorEvents[0].data), &parsed),
		"error 事件 data 应为可解析 JSON（独立事件），实际=%q", errorEvents[0].data)
	require.Equal(t, wantMessage, parsed.Error.Message,
		"error 事件 message 应为指定中文常量，实际=%q", parsed.Error.Message)
}

// setRepetitionGuardOff 临时将重复循环熔断阈值置 0（fail-open 旁路），确保测试
// 内容行被直接写出（否则默认阈值 768 的缓冲期会持有行、阻止 clientOutputStarted
// 置位）。返回还原函数。
func setRepetitionGuardOff(t *testing.T) func() {
	t.Helper()
	old, had := os.LookupEnv(repetitionLoopThresholdEnv)
	require.NoError(t, os.Setenv(repetitionLoopThresholdEnv, "0"))
	return func() {
		if had {
			_ = os.Setenv(repetitionLoopThresholdEnv, old)
		} else {
			_ = os.Unsetenv(repetitionLoopThresholdEnv)
		}
	}
}
