//go:build unit

package service

// CUR-SVC5：AgentRun 出站 MCP 白名单头测试。
// 用 httptest 上游截获实际出站头与体，覆盖有工具 / 无工具 / none 选择 / 只读路径四种情形。
// 自包含 fixture（不复用其它测试文件的辅助函数），避免并行卡改动导致本文件失稳。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const cursorOutboundWantAllowedTools = "mcp_tool_call,get_mcp_tools_tool_call,list_mcp_resources_tool_call,read_mcp_resource_tool_call,mcp_auth_tool_call"

// cursorOutboundTestAuth 是固定鉴权材料的测试替身。
type cursorOutboundTestAuth struct{}

func (cursorOutboundTestAuth) CursorUpstreamAuth(context.Context) (string, error) {
	return "OUTBOUND-TEST-CIPHER", nil
}

// cursorOutboundCapture 记录上游实际收到的头与体（handler 与测试 goroutine 间用 mutex 同步）。
type cursorOutboundCapture struct {
	mu     sync.Mutex
	header http.Header
	body   string
}

func (c *cursorOutboundCapture) snapshot() (http.Header, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.header.Clone(), c.body
}

// newCursorOutboundForwarder 造一个节点指向 httptest 上游、可截获头/体的转发器。
func newCursorOutboundForwarder(t *testing.T) (*CursorUpstreamForwarder, *cursorOutboundCapture) {
	t.Helper()
	capture := &cursorOutboundCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只做一次读：AgentRun 的请求体是 io.Pipe（不会自行关闭），ReadAll 会一直等；
		// duplex 在 Do 中已把 initial 帧写入 pipe，一次 Read 即可截获该帧。
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		capture.mu.Lock()
		capture.header = r.Header.Clone()
		capture.body = string(buf[:n])
		capture.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	nodes := NewCursorUpstreamNodesResolver(WithCursorNodesCacheTTL(time.Minute))
	nodes.cached = CursorUpstreamNodes{HTTP1Base: srv.URL, HTTP2Base: srv.URL, Dynamic: true}
	nodes.cachedAt = time.Now()

	f := NewCursorUpstreamForwarder(
		WithCursorUpstreamHTTPClient(srv.Client()),
		WithCursorUpstreamNodes(nodes),
		WithCursorUpstreamAuthProvider(cursorOutboundTestAuth{}),
	)
	return f, capture
}

// cursorOutboundAgentRunBody 构造 AgentRun 请求体：tools 决定 f4 的工具声明；
// toolChoice 为 `"none"` 时不声明任何工具（与 cursorConvertRequest 口径一致）。
func cursorOutboundAgentRunBody(t *testing.T, tools []apicompat.ChatTool, toolChoice json.RawMessage) []byte {
	t.Helper()
	body, err := cursorConvertRequest(&apicompat.ChatCompletionsRequest{
		Messages:   []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:      tools,
		ToolChoice: toolChoice,
	}, "grok-4.7")
	if err != nil {
		t.Fatalf("cursorConvertRequest: %v", err)
	}
	return body
}

func TestCursorSDKTransportAllowedToolsHeader(t *testing.T) {
	headerKey := http.CanonicalHeaderKey(cursorAgentAllowedToolsHeader)

	t.Run("with-tools", func(t *testing.T) {
		f, capture := newCursorOutboundForwarder(t)
		body := cursorOutboundAgentRunBody(t, []apicompat.ChatTool{
			{Type: "function", Function: &apicompat.ChatFunction{Name: "get_weather"}},
			{Type: "function", Function: &apicompat.ChatFunction{Name: "search"}},
		}, nil)
		resp, err := f.Do(context.Background(), &CursorUpstreamRequest{Path: cursorPathAgentRun, Body: body})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()

		header, captured := capture.snapshot()
		if got := header.Get(cursorAgentAllowedToolsHeader); got != cursorOutboundWantAllowedTools {
			t.Errorf("allowed-tools = %q, want comma-joined five names %q", got, cursorOutboundWantAllowedTools)
		}
		if vals, ok := header[headerKey]; !ok || len(vals) != 1 {
			t.Errorf("allowed-tools header must be present exactly once, got %v (present=%v)", vals, ok)
		}
		if len(captured) == 0 {
			t.Error("upstream must receive the AgentRun body")
		}
	})

	t.Run("no-tools", func(t *testing.T) {
		f, capture := newCursorOutboundForwarder(t)
		body := cursorOutboundAgentRunBody(t, nil, nil)
		resp, err := f.Do(context.Background(), &CursorUpstreamRequest{Path: cursorPathAgentRun, Body: body})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()

		header, _ := capture.snapshot()
		vals, ok := header[headerKey]
		if !ok {
			t.Fatal("no-tools: header must be present with an explicit empty value")
		}
		if len(vals) != 1 || vals[0] != "" {
			t.Errorf("no-tools: allowed-tools = %q, want single empty value", vals)
		}
	})

	t.Run("tool-choice-none", func(t *testing.T) {
		f, capture := newCursorOutboundForwarder(t)
		body := cursorOutboundAgentRunBody(t,
			[]apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{Name: "get_weather"}}},
			json.RawMessage(`"none"`),
		)
		resp, err := f.Do(context.Background(), &CursorUpstreamRequest{Path: cursorPathAgentRun, Body: body})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()

		header, _ := capture.snapshot()
		vals, ok := header[headerKey]
		if !ok {
			t.Fatal("none: header must be present with an explicit empty value")
		}
		if len(vals) != 1 || vals[0] != "" {
			t.Errorf("none: allowed-tools = %q, want single empty value", vals)
		}
	})

	t.Run("read-only-path", func(t *testing.T) {
		f, capture := newCursorOutboundForwarder(t)
		body := []byte(`{"model":"cursor-small","messages":[{"role":"user","content":"hi"}]}`)
		resp, err := f.Do(context.Background(), &CursorUpstreamRequest{Path: cursorPathAvailableModels, Body: body})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()

		header, captured := capture.snapshot()
		if _, ok := header[headerKey]; ok {
			t.Errorf("read-only path must not carry allowed-tools header, got %q", header.Get(cursorAgentAllowedToolsHeader))
		}
		if captured != string(body) {
			t.Errorf("read-only body mutated: got %q, want %q", captured, body)
		}
	})
}
