//go:build unit

package service

// CUR-SVC6A1：AgentRun 出站 SDK 身份 + 旧内联标记纯文本化的验收测试。
//
// 第一条走真实 build→Do 路径，从 httptest 上游截获出站头（复用同包 SVC5 fixture）；
// 第二条用自包含 SSE fixture 断言：旧内联标记不再被解析成 tool_calls，原样留在正文。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// TestCursorSDKTransportOutboundIdentity 钉死 AgentRun 出站的 SDK 身份：
// x-cursor-client-type=sdk、x-cursor-client-version=sdk-1.5.0；
// 全局 cursorClientVersion（模型目录同步）保持 3.24.12 不变。
func TestCursorSDKTransportOutboundIdentity(t *testing.T) {
	f, capture := newCursorOutboundForwarder(t)
	payload := cursorOutboundAgentRunBody(t, nil, nil)
	headers := cursorAgentRunHeaders("BEARER-SDK-IDENTITY", "device-sdk-identity", time.Now())
	fwdReq := cursorBuildAgentRunUpstreamRequest(
		&CursorExecutionRequest{
			Chat: &apicompat.ChatCompletionsRequest{
				Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			},
		},
		payload,
		cursorOutboundTestAuth{},
		"cursor-sdk-test",
		nil,
		headers,
	)

	resp, err := f.Do(context.Background(), fwdReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	got, _ := capture.snapshot()
	require.Equal(t, "sdk", got.Get("x-cursor-client-type"))
	require.Equal(t, "sdk-1.5.0", got.Get("x-cursor-client-version"))
	require.Equal(t, "3.24.12", cursorClientVersion, "全局 IDE 版本不得被 SDK 身份改动")

	allowedKey := http.CanonicalHeaderKey(cursorAgentAllowedToolsHeader)
	vals, present := got[allowedKey]
	require.True(t, present, "无工具请求也必须显式携带 allowed-tools 头")
	require.Equal(t, []string{""}, vals)
}

// cursorSDKInlineOpen / cursorSDKInlineClose 按码点拼出旧内联工具调用标记，
// 避免手打尖括号时混入不可见字符。
const (
	cursorSDKInlineOpen  = "\x3ctool_call\x3e"
	cursorSDKInlineClose = "\x3c/tool_call\x3e"
)

// cursorSDKInlineSSE 把一段正文包成 AgentRun 上游的单 chunk SSE（末尾 [DONE]）。
func cursorSDKInlineSSE(content string) []byte {
	chunk := map[string]any{
		"id":     "resp-inline-text",
		"object": "chat.completion.chunk",
		"model":  "cursor-sdk-test",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": content},
			"finish_reason": nil,
		}},
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		panic(err)
	}
	var sb strings.Builder
	sb.WriteString("data: ")
	sb.Write(raw)
	sb.WriteString("\n\ndata: [DONE]\n\n")
	return []byte(sb.String())
}

// TestCursorSDKTransportInlineTextStaysText 钉死：聚合路径删除内联解析分支后，
// 旧内联标记（含完整可解析的 JSON blob）只是普通正文，不产 tool_calls。
func TestCursorSDKTransportInlineTextStaysText(t *testing.T) {
	text := "前言。" + cursorSDKInlineOpen + "\n" +
		`{"id":"call_sdk","name":"REPL","arguments":{"code":"const out = {};"}}` + "\n" +
		cursorSDKInlineClose + "后语。"
	raw, err := cursorAggregateSSEToCompletion(context.Background(), cursorSDKInlineSSE(text))
	require.NoError(t, err)

	var out struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []any   `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Choices, 1)
	msg := out.Choices[0].Message
	require.NotNil(t, msg.Content)
	require.Equal(t, text, *msg.Content, "旧内联标记必须原样作为正文交付")
	require.Empty(t, msg.ToolCalls, "旧内联标记不得再产出 tool_calls")
}
