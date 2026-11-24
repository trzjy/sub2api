//go:build unit

package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func cursorDuplexReadFrame(r io.Reader) ([]byte, error) {
	h := make([]byte, 5)
	if _, err := io.ReadFull(r, h); err != nil {
		return nil, err
	}
	b := make([]byte, int(binary.BigEndian.Uint32(h[1:])))
	_, err := io.ReadFull(r, b)
	return b, err
}

// The server refuses to generate a reply until context and KV replies arrive.
// It deliberately keeps the HTTP stream open after TurnEnded.
func TestCursorAgentDuplexContextKVAndTurnEnd(t *testing.T) {
	serverErr := make(chan error, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			serverErr <- io.ErrNoProgress
			return
		}
		if r.Header.Get(cursorJgModelHeader) != "" {
			serverErr <- io.ErrUnexpectedEOF
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
		exec := cursorAppendVarintField(nil, 1, 7)
		exec = cursorAppendBytesField(exec, 15, []byte("exec-id"))
		exec = cursorAppendBytesField(exec, 10, nil)
		send(2, exec)
		if err := readReply(2, 7); err != nil {
			serverErr <- err
			return
		}
		set := cursorAppendBytesField(nil, 1, []byte("blob-id"))
		set = cursorAppendBytesField(set, 2, []byte("blob-value"))
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
		send(1, cursorAppendBytesField(nil, 1, cursorAppendStringField(nil, 1, "5017")))
		usage := cursorAppendVarintField(nil, 1, 123)
		usage = cursorAppendVarintField(usage, 2, 4)
		send(1, cursorAppendBytesField(nil, 14, usage))
		serverErr <- nil
		<-r.Context().Done()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	nodes := NewCursorUpstreamNodesResolver()
	nodes.cached = CursorUpstreamNodes{HTTP1Base: "https://unused.invalid", HTTP2Base: srv.URL}
	nodes.cachedAt = time.Now()
	f := NewCursorUpstreamForwarder(WithCursorUpstreamHTTPClient(srv.Client()), WithCursorUpstreamNodes(nodes), WithCursorUpstreamAuthProvider(staticCursorAuthProvider{value: "test-auth"}))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := f.Do(ctx, &CursorUpstreamRequest{Path: cursorPathAgentRun, Body: cursorEncodeEnvelope(cursorAppendBytesField(nil, 1, nil))})
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, <-serverErr)
	p := cursorNewStreamParser("grok-4.7")
	_, err = p.cursorParseStream(body)
	require.NoError(t, err)
	require.Equal(t, "5017", p.content)
	require.Equal(t, 123, p.usage.Input)
	require.Equal(t, 4, p.usage.Output)
}

// TestCursorAgentDuplexUnknownExecutionFailsClosed 验证「未知 exec 字段 → 既有协议错误、
// 零回包、由原 owner 终止」的失败关闭契约。本层不再发通用未知字段帧、不再猜测结果。
//
// 收尾方式：cursorExecRejectTestDuplex 起 goroutine 排干 pipe writer；失败关闭路径不写帧，
// 但仍统一 cleanup 关闭 pipe 并等待排干完成，便于断言零回包。
func TestCursorAgentDuplexUnknownExecutionFailsClosed(t *testing.T) {
	// "unknown_exec"：真正未知、且不在 11 类原生 exec 裁定里的执行字段（f17
	// list_mcp_resources_exec_args）。f17 不属于原生拒绝集合，也不在元数据白名单：
	// 必须返回错误、不写任何帧、由原 owner 终止。
	t.Run("unknown_exec", func(t *testing.T) {
		d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)
		defer cleanup()

		done, err := d.respond(cursorExecUnknownFieldPayload())
		require.Error(t, err, "未知 exec 字段必须失败关闭")
		require.False(t, done, "失败关闭不由本层标记终态")
		require.Equal(t, 1, phaseCount(logs, "exec_unsupported_fields"), "必须记一条未知字段取证日志")
		cleanup()
		require.Zero(t, buf.Len(), "未知 exec 字段必须零回包（不回猜测结果帧）")
	})
	// "unknown_blob"：f4 KV 缺 blob（f2 get 但无对应 blob）——这是 KV 操作语义错误，不是
	// 未知 exec 字段，维持失败关闭不变（返回 error）。此路径不写帧，故不会因 pipe 阻塞挂起。
	t.Run("unknown_blob", func(t *testing.T) {
		d, _, _, cleanup := cursorExecRejectTestDuplex(t)
		defer cleanup()

		payload := cursorAppendBytesField(nil, 4, cursorAppendBytesField(nil, 2, cursorAppendStringField(nil, 1, "missing")))
		_, err := d.respond(payload)
		require.Error(t, err, "KV 缺 blob 仍是失败关闭（语义未变）")
	})
}

// cursorExecUnknownFieldPayload 构造一条携带真正未知执行字段（f17）的 AgentServerMessage，
// 用于验证「未知 exec → 既有协议错误、零回包」的失败关闭语义。f17 不在本轮 11 类原生 exec
// 拒绝字段集合内，也不在 ExecServerMessage 元数据白名单内，因此走到 respond 的 default
// 未知字段分支（返回错误、不发任何帧）。
func cursorExecUnknownFieldPayload() []byte {
	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 1)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-1"))
	execMsg = cursorAppendBytesField(execMsg, 17, []byte("list-mcp-resources-args"))
	return cursorAppendBytesField(nil, cursorFldServerExecMessage, execMsg)
}

// Exercise the actual request builder and HTTP/2 exchange: history sent in f17
// must remain available when the server subsequently asks for that same blob.
func TestCursorAgentDuplexServesPrefetchedHistory(t *testing.T) {
	var request apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal([]byte(`{"messages":[{"role":"user","content":"Remember violet 731."},{"role":"assistant","content":"I will remember violet 731."},{"role":"user","content":"What should you remember?"}]}`), &request))
	initial, err := cursorConvertRequest(&request, "grok-4.7")
	require.NoError(t, err)
	serverErr := make(chan error, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchange := func() error {
			if r.ProtoMajor != 2 {
				return errors.New("history test requires HTTP/2")
			}
			payload, err := cursorDuplexReadFrame(r.Body)
			if err != nil {
				return err
			}
			fields, err := cursorDecodeProtobuf(payload)
			if err != nil {
				return err
			}
			run, ok := cursorFindField(fields, cursorFldAgentRunRequest)
			if !ok {
				return errors.New("missing run request")
			}
			fields, err = cursorDecodeProtobuf(run.Bytes)
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", cursorConnectContentType)
			count := 0
			for _, field := range fields {
				if field.Number != cursorFldRunPreFetchedBlobs {
					continue
				}
				blob, err := cursorDecodeProtobuf(field.Bytes)
				if err != nil {
					return err
				}
				key, foundKey := cursorFindField(blob, cursorFldBlobID)
				value, foundValue := cursorFindField(blob, cursorFldBlobValue)
				if !foundKey || !foundValue {
					return errors.New("missing pre-fetched history")
				}
				count++
				get := cursorAppendBytesField(nil, 1, key.Bytes)
				kv := cursorAppendBytesField(cursorAppendVarintField(nil, 1, uint64(count)), 2, get)
				if _, err := w.Write(cursorEncodeEnvelope(cursorAppendBytesField(nil, cursorFldServerKvMessage, kv))); err != nil {
					return err
				}
				w.(http.Flusher).Flush()
				reply, err := cursorDuplexReadFrame(r.Body)
				if err != nil {
					return err
				}
				// AgentClientMessage.kv_client_message.get_blob_result.blob.
				for _, number := range []int{3, 2, 1} {
					parts, err := cursorDecodeProtobuf(reply)
					if err != nil {
						return err
					}
					part, ok := cursorFindField(parts, number)
					if !ok {
						return fmt.Errorf("missing history reply field %d", number)
					}
					reply = part.Bytes
				}
				if !bytes.Equal(reply, value.Bytes) {
					return errors.New("history readback differs from run_request")
				}
			}
			if count != 2 {
				return fmt.Errorf("expected two history blobs, received %d", count)
			}
			_, err = w.Write(append(cursorTextDeltaFrame(t, "history loaded"), cursorTurnEndedFrame(t, [2]uint64{cursorFldTurnEndedInputTokens, 44}, [2]uint64{cursorFldTurnEndedOutputTokens, 2})...))
			w.(http.Flusher).Flush()
			return err
		}
		serverErr <- exchange()
		<-r.Context().Done()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	nodes := NewCursorUpstreamNodesResolver()
	nodes.cached = CursorUpstreamNodes{HTTP1Base: "https://unused.invalid", HTTP2Base: srv.URL}
	nodes.cachedAt = time.Now()
	f := NewCursorUpstreamForwarder(WithCursorUpstreamHTTPClient(srv.Client()), WithCursorUpstreamNodes(nodes), WithCursorUpstreamAuthProvider(staticCursorAuthProvider{value: "test-auth"}))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := f.Do(ctx, &CursorUpstreamRequest{Path: cursorPathAgentRun, Body: initial})
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, <-serverErr)
	parser := cursorNewStreamParser("grok-4.7")
	_, err = parser.cursorParseStream(body)
	require.NoError(t, err)
	require.Equal(t, "history loaded", parser.content)
	require.Equal(t, 44, parser.usage.Input)
}

func TestCursorSSEStreamConcurrentTerminalErrorSnapshot(t *testing.T) {
	for _, frame := range [][]byte{
		cursorTextDeltaFrame(t, cursorUpstreamRejectTemplate+" (test-turn)"),
		cursorTestEndStream(t, `{"error":{"code":"unauthenticated","message":"denied"}}`),
	} {
		stream := &cursorSSEStream{source: io.NopCloser(bytes.NewReader(frame)), parser: cursorNewStreamParser("grok-4.7")}
		readErr := make(chan error, 1)
		go func() {
			_, err := io.ReadAll(stream)
			readErr <- err
		}()
		for {
			select {
			case err := <-readErr:
				require.Error(t, err)
				require.Same(t, err, stream.terminalError())
				var rejected *CursorKeyRejectedError
				if errors.As(err, &rejected) {
					require.Equal(t, "test-turn", rejected.TurnID)
					require.Contains(t, rejected.Template, cursorUpstreamRejectTemplate)
				}
				goto nextFrame
			default:
				_ = stream.terminalError()
			}
		}
	nextFrame:
	}
}

// TestCursorAgentDuplexUnknownFieldWire0FailsClosed 证明未知 exec 字段即使 wire=0/varint
// 也不再被静默忽略：一律返回既有协议错误、零回包、由原 owner 终止。
func TestCursorAgentDuplexUnknownFieldWire0FailsClosed(t *testing.T) {
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)
	defer cleanup()

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 40)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-40"))
	execMsg = cursorAppendVarintField(execMsg, 40, 123) // wire=0 / varint 未知字段

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.Error(t, err, "未知 exec 字段（含 wire=0）一律失败关闭")
	require.False(t, done)
	require.Equal(t, 1, phaseCount(logs, "exec_unsupported_fields"),
		"未知字段必须记一条取证日志（字段号/wire，不含内容）")
	cleanup()
	require.Zero(t, buf.Len(), "未知 exec 字段零回包")
}

func TestCursorClientChecksumMatchesClientJavaScript(t *testing.T) {
	// Generated independently by the shipped P_v/L_v JavaScript, including its
	// 32-bit shifts; the synthetic device is not a real installation identity.
	require.Equal(t, "/GBifCmP098a78ad05bcf1d7bd99630f0e87b0c2d08305ca974debe656af0d377f7bd714", cursorClientChecksum("synthetic-device", time.UnixMilli(1792419990644)))
}

func TestCursorSSEStreamEmitsTextBeforeTurnEnd(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	release := make(chan struct{})
	defer close(release)
	go func() {
		defer w.Close()
		_, _ = w.Write(cursorTextDeltaFrame(t, "FIRST"))
		<-release
		_, _ = w.Write(cursorTurnEndedFrame(t, [2]uint64{cursorFldTurnEndedInputTokens, 10}, [2]uint64{cursorFldTurnEndedOutputTokens, 2}))
	}()
	s := &cursorSSEStream{source: r, parser: cursorNewStreamParser("grok-4.7")}
	reader := bufio.NewReader(s)
	first, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, first, "assistant")
	_, err = reader.ReadString('\n')
	require.NoError(t, err)
	text, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, text, "FIRST")
	require.False(t, s.done, "text must arrive before upstream sends TurnEnded")
}
