//go:build unit

package service

// §18.5 T 阶段日志的定向测试（httptest + 短 context / 延迟 handler 驱动，不 sleep 180 秒）。
//
// 覆盖：
//   - headers 前等待 → begin 阶段事件（http_request_start / got_conn）先于 response_headers 出现；
//   - 首帧等待 → first_frame 事件在首帧到达后出现；
//   - 双工 context/KV 应答写入前后两个事件，且日志中无 KV 键值；
//   - 客户端取消 → context_cancel，且不记成功 usage（turn_ended / end_stream 不出现）；
//   - 日志含 request_id 与终止事件；正常流式仍在 TurnEnded 前输出；
//   - 敏感值零泄漏：带哨兵 KV 值的输入，断言哨兵值不出现在日志文本中。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// cursorPhaseObserverCtx 返回一个挂了 observer logger 且带 request_id 的 ctx。
func cursorPhaseObserverCtx(t *testing.T, reqID string) (context.Context, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, reqID)
	ctx = logger.IntoContext(ctx, zap.New(core))
	return ctx, logs
}

// observedContainsText 检查任何一条日志（消息 + 字段值）是否包含子串（敏感值零泄漏断言）。
func observedContainsText(logs *observer.ObservedLogs, substr string) bool {
	for _, e := range logs.All() {
		if strings.Contains(e.Message, substr) {
			return true
		}
		for _, v := range e.ContextMap() {
			if strings.Contains(fmt.Sprintf("%v", v), substr) {
				return true
			}
		}
	}
	return false
}

// phaseCount 统计某阶段事件（phase 字段）的出现次数。
// 注意：阶段日志的消息固定为 "cursor_phase_event"，阶段名在 phase 字段里。
func phaseCount(logs *observer.ObservedLogs, phase string) int {
	return logs.FilterField(zap.String("phase", phase)).Len()
}

// phaseIndex 返回某阶段事件第一次出现的位置（-1 表示未出现）。
func phaseIndex(logs *observer.ObservedLogs, phase string) int {
	for i, e := range logs.All() {
		if e.ContextMap()["phase"] == phase {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// headers 前等待：begin 阶段事件先于 response_headers 出现
// ---------------------------------------------------------------------------

func TestCursorUpstreamForwardHeadersWaitLogsBeginBeforeResponse(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "req-hdr-1")
	ctx = logger.IntoContext(ctx, zap.New(core))

	const headerDelay = 120 * time.Millisecond
	f, _ := newCursorForwarderFixture(t, staticCursorAuthProvider{value: "CIPHER"},
		func(w http.ResponseWriter, r *http.Request) {
			// 连接已建立，但故意延迟写响应头，模拟"等待上游建连/首字节"。
			time.Sleep(headerDelay)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})

	done := make(chan struct{})
	go func() {
		_, _ = f.Do(ctx, &CursorUpstreamRequest{Path: cursorPathAvailableModels, Body: []byte(`{}`)})
		close(done)
	}()

	// begin 事件（http_request_start / got_conn）应在 headers 等待期间已经落盘。
	require.Eventually(t, func() bool {
		return phaseCount(logs, "http_request_start") > 0 &&
			phaseCount(logs, "got_conn") > 0
	}, 2*time.Second, 5*time.Millisecond, "begin 阶段事件应在等待 headers 期间出现")

	// 此时尚未到 120ms 的 headers 返回点，response_headers 不应出现。
	require.Equal(t, 0, phaseCount(logs, "response_headers"),
		"response_headers 不应在 headers 等待期间出现")

	<-done
	require.GreaterOrEqual(t, phaseCount(logs, "response_headers"), 1)

	// 顺序：http_request_start 必须早于 response_headers。
	idxStart := phaseIndex(logs, "http_request_start")
	idxHeaders := phaseIndex(logs, "response_headers")
	require.Greater(t, idxHeaders, idxStart, "http_request_start 必须早于 response_headers")
}

// ---------------------------------------------------------------------------
// 首帧等待：first_frame 事件在首帧到达后出现，且早于 turn_ended
// ---------------------------------------------------------------------------

// cursorPhaseStreamServer 造一个 HTTP/2 假中转，按既定序列回包：
// （可选延迟首帧）→ 文本 → exec(context) → kv set/get → turn_ended，最后等 ctx 取消。
func cursorPhaseStreamServer(t *testing.T, firstFrameDelay time.Duration, kvValue string) (*CursorUpstreamForwarder, context.Context, *observer.ObservedLogs) {
	t.Helper()
	ctx, logs := cursorPhaseObserverCtx(t, "req-duplex-1")
	serverErr := make(chan error, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			serverErr <- io.ErrNoProgress
			return
		}
		if _, err := cursorDuplexReadFrame(r.Body); err != nil {
			serverErr <- err
			return
		}
		w.Header().Set("Content-Type", cursorConnectContentType)
		send := func(field int, b []byte) {
			_, _ = w.Write(cursorEncodeEnvelope(cursorAppendBytesField(nil, field, b)))
			w.(http.Flusher).Flush()
		}
		readReply := func(field, id int) error {
			b, err := cursorDuplexReadFrame(r.Body)
			if err != nil {
				return err
			}
			f, err := cursorDecodeProtobuf(b)
			if err != nil {
				return err
			}
			v, ok := cursorFindField(f, field)
			if !ok {
				return io.ErrUnexpectedEOF
			}
			f, err = cursorDecodeProtobuf(v.Bytes)
			if err != nil {
				return err
			}
			if cursorVarintField(f, 1) != uint64(id) {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		time.Sleep(firstFrameDelay)
		send(1, cursorAppendBytesField(nil, 1, cursorAppendStringField(nil, 1, "hello-first-frame")))
		exec := cursorAppendVarintField(nil, 1, 7)
		exec = cursorAppendBytesField(exec, 15, []byte("exec-id"))
		exec = cursorAppendBytesField(exec, 10, nil)
		send(2, exec)
		if err := readReply(2, 7); err != nil {
			serverErr <- err
			return
		}
		set := cursorAppendBytesField(nil, 1, []byte("blob-id"))
		set = cursorAppendBytesField(set, 2, []byte(kvValue))
		send(4, cursorAppendBytesField(cursorAppendVarintField(nil, 1, 8), 3, set))
		if err := readReply(3, 8); err != nil {
			serverErr <- err
			return
		}
		get := cursorAppendBytesField(nil, 1, []byte("blob-id"))
		send(4, cursorAppendBytesField(cursorAppendVarintField(nil, 1, 9), 2, get))
		if err := readReply(3, 9); err != nil {
			serverErr <- err
			return
		}
		send(1, cursorAppendBytesField(nil, 14, cursorAppendVarintField(nil, 1, 123)))
		serverErr <- nil
		<-r.Context().Done()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	nodes := NewCursorUpstreamNodesResolver()
	nodes.cached = CursorUpstreamNodes{HTTP1Base: "https://unused.invalid", HTTP2Base: srv.URL}
	nodes.cachedAt = time.Now()
	f := NewCursorUpstreamForwarder(WithCursorUpstreamHTTPClient(srv.Client()),
		WithCursorUpstreamNodes(nodes), WithCursorUpstreamAuthProvider(staticCursorAuthProvider{value: "test-auth"}))
	return f, ctx, logs
}

func TestCursorAgentDuplexFirstFrameDuringWait(t *testing.T) {
	f, ctx, logs := cursorPhaseStreamServer(t, 80*time.Millisecond, "kv-plain-value")

	resp, err := f.Do(ctx, &CursorUpstreamRequest{
		Path: cursorPathAgentRun, Body: cursorEncodeEnvelope(cursorAppendBytesField(nil, 1, nil))})
	require.NoError(t, err)
	defer resp.Body.Close()

	// 首帧延迟 80ms：前 40ms 内不应出现 first_frame；之后应出现。
	time.Sleep(40 * time.Millisecond)
	require.Equal(t, 0, phaseCount(logs, "first_frame"), "首帧到达前不应记 first_frame")

	readDone := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(resp.Body)
		close(readDone)
	}()
	require.Eventually(t, func() bool {
		return phaseCount(logs, "first_frame") > 0
	}, 2*time.Second, 5*time.Millisecond, "首帧到达后应记 first_frame")

	// 正常流式：first_frame 必须早于 turn_ended（文本在终态 usage 之前输出）。
	idxFirst := phaseIndex(logs, "first_frame")
	idxTurn := phaseIndex(logs, "turn_ended")
	require.Greater(t, idxTurn, idxFirst, "first_frame 必须早于 turn_ended")
	<-readDone
}

// ---------------------------------------------------------------------------
// 双工 context/KV 应答写入前后事件 + 无 KV 键值泄漏
// ---------------------------------------------------------------------------

func TestCursorAgentDuplexKVWriteBeforeAfterNoKeyValueLeak(t *testing.T) {
	const sentinel = "SENTINEL-KV-VALUE-D4E7C2"
	f, ctx, logs := cursorPhaseStreamServer(t, 0, sentinel)

	resp, err := f.Do(ctx, &CursorUpstreamRequest{
		Path: cursorPathAgentRun, Body: cursorEncodeEnvelope(cursorAppendBytesField(nil, 1, nil))})
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)

	// context/KV 应答写入前后各一次事件。
	require.GreaterOrEqual(t, phaseCount(logs, "kv_write_before"), 1)
	require.GreaterOrEqual(t, phaseCount(logs, "kv_write_after"), 1)
	require.GreaterOrEqual(t, phaseCount(logs, "context_write_before"), 1)
	require.GreaterOrEqual(t, phaseCount(logs, "context_write_after"), 1)

	// 只记种类（kv_set/kv_get/context_response），不记 key/value 明文。
	for _, e := range logs.FilterField(zap.String("phase", "kv_write_before")).All() {
		require.Contains(t, []string{"kv_set", "kv_get"}, e.ContextMap()["kind"])
	}

	// 敏感值零泄漏：哨兵 KV 值不得出现在任何日志文本中。
	require.False(t, observedContainsText(logs, sentinel),
		"KV 键值（哨兵值）不得出现在阶段日志中")
	// 同样不得泄漏账号 Bearer / 激活码 / device 等凭证标识。
	require.False(t, observedContainsText(logs, "Bearer"), "Bearer 不得出现在阶段日志中")
	require.False(t, observedContainsText(logs, "test-auth"), "x-jg-auth 材料不得出现在阶段日志中")
}

// ---------------------------------------------------------------------------
// 客户端取消 → context_cancel，且不记成功 usage（turn_ended / end_stream 不出现）
// ---------------------------------------------------------------------------

func TestCursorAgentDuplexCancelLogsContextCancelNoSuccessUsage(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-cancel-1")
	serverErr := make(chan error, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			serverErr <- io.ErrNoProgress
			return
		}
		if _, err := cursorDuplexReadFrame(r.Body); err != nil {
			serverErr <- err
			return
		}
		w.Header().Set("Content-Type", cursorConnectContentType)
		// 发送一条首帧后，一直等客户端取消；绝不发 TurnEnded / EndStream（即不计成功）。
		_, _ = w.Write(cursorTextDeltaFrame(t, "partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		serverErr <- nil
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	nodes := NewCursorUpstreamNodesResolver()
	nodes.cached = CursorUpstreamNodes{HTTP1Base: "https://unused.invalid", HTTP2Base: srv.URL}
	nodes.cachedAt = time.Now()
	f := NewCursorUpstreamForwarder(WithCursorUpstreamHTTPClient(srv.Client()),
		WithCursorUpstreamNodes(nodes), WithCursorUpstreamAuthProvider(staticCursorAuthProvider{value: "test-auth"}))

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := f.Do(ctx, &CursorUpstreamRequest{
		Path: cursorPathAgentRun, Body: cursorEncodeEnvelope(cursorAppendBytesField(nil, 1, nil))})
	require.NoError(t, err)
	defer resp.Body.Close()

	readDone := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(resp.Body)
		close(readDone)
	}()
	// 客户端中途取消：首帧已到，但上游未发终态。
	time.Sleep(50 * time.Millisecond)
	cancel()
	// 模拟下游消费者断开（生产 handler 在下游 ctx 取消后由 defer resp.Body.Close()
	// 关闭上游响应体）：关闭上游响应体以中断阻塞中的读取，触发 context_cancel 终态事件。
	resp.Body.Close()
	<-readDone

	require.GreaterOrEqual(t, phaseCount(logs, "context_cancel"), 1,
		"客户端取消应记 context_cancel 终态事件")
	require.Equal(t, 0, phaseCount(logs, "turn_ended"),
		"取消路径不得记 turn_ended（成功 usage 来源）")
	require.Equal(t, 0, phaseCount(logs, "end_stream"),
		"取消路径不得记正常 end_stream 终态")
	_ = serverErr
}

// ---------------------------------------------------------------------------
// forwardCursor 阶段事件（account_selected / admission_done / auth_* / convert_done /
// agent_run_begin / response_parse_done / response_write_done）+ request_id
// ---------------------------------------------------------------------------

func TestCursorRequestPhaseForwardCursorPhases(t *testing.T) {
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	svc, c := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))

	ctx, logs := cursorPhaseObserverCtx(t, "req-fwd-1")
	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	_, err := svc.forwardCursor(ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)

	for _, phase := range []string{
		"account_selected", "admission_done", "auth_begin", "auth_done",
		"convert_done", "agent_run_begin", "response_parse_done", "response_write_done",
	} {
		require.GreaterOrEqual(t, phaseCount(logs, phase), 1,
			"forwardCursor 应记阶段事件 %q", phase)
	}
	// 日志含 request_id。
	require.True(t, requestIDPresentInLogs(logs, "req-fwd-1"))
}

// requestIDPresentInLogs 断言至少一条日志带 request_id 字段且值匹配。
func requestIDPresentInLogs(logs *observer.ObservedLogs, want string) bool {
	for _, e := range logs.All() {
		if e.ContextMap()["request_id"] == want {
			return true
		}
	}
	return false
}
