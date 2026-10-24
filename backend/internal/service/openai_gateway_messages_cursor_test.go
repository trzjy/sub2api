//go:build unit

package service

// 派发单 B5：/v1/messages 对 Cursor 账号接入既有 Anthropic 执行入口。
//
// 修复前：Cursor 账号（Platform==PlatformCursor）在 ForwardAsAnthropic 里没有专属分支，
// 落到通用 OpenAI 路径 → prepareCodexAccountIdentitySource → OpenAITokenProvider
// （要求 Platform==openai && Type==oauth）→ 直接 502 "not an openai oauth account"。
//
// 修复后：在 codebuddy/web 分支之后、通用 OpenAI 路径之前新增 Cursor 分支，直接走既有
// cursorExecuteAnthropic（Anthropic Messages → Cursor 原生协议）→ cursorForwardResultToOpenAI。
//
// 本文件只验证分流（不触碰 cursor_execution_service.go / gateway_forward.go 等禁区），
// 三条硬性质对应 Done when D1/D2/D3：
//   D1  cursor 账号不再落到 OAuth token provider；执行部件未装配时应返回
//      ErrCursorExecutionUnavailable 而非 oauth 错误；装配后应真正走到 Cursor 执行入口。
//   D2  openai OAuth 账号行为不变，仍走原始 OpenAI 上游路径，不得被 cursor 分支拦截。
//   D3  既有 cursor 单测不回归（由手控基线比对保证，不在此文件内穷举）。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestForwardAsAnthropic_CursorAccountUnavailableExecutionFailsClosed 覆盖 D1 的
// 失败关闭语义：Cursor 账号 + 未装配 cursorExecution → 必须落到 cursor 执行入口并因
// 未装配返回 ErrCursorExecutionUnavailable，**绝不得**回退到 OAuth token provider
// 并产出 "not an openai oauth account"。这直接钉死修复前 502 的根因被消除。
func TestForwardAsAnthropic_CursorAccountUnavailableExecutionFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const body = `{"model":"cursor-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	// 只给最小装配：不挂 cursorExecution（模拟未装配），不给 httpUpstream（cursor 分支
	// 在抵达通用 OpenAI 上游之前即收口，不应触碰 httpUpstream）。
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
			},
		},
	}
	account := &Account{
		ID:       8001,
		Platform: PlatformCursor,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
	}
	require.True(t, isCursorAccount(account), "前置谓词：cursor 账号应被判为 cursor")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, []byte(body), "", "")
	require.Error(t, err, "未装配执行部件时不应成功")
	require.ErrorIs(t, err, ErrCursorExecutionUnavailable,
		"Cursor 账号必须走 cursor 执行入口并在未装配时失败关闭，而非落到 OAuth token provider")
	require.NotContains(t, err.Error(), "not an openai oauth account",
		"修复前 502 根因（OAuth token provider）不得再出现")
	require.NotContains(t, err.Error(), "cursor: parse anthropic messages request",
		"body 可正常解析，不得因解析失败掩盖分流正确性")
}

// TestForwardAsAnthropic_CursorAccountWiredExecutionReachesEntry 覆盖 D1 的正向路径：
// Cursor 账号 + 已装配 cursorExecution（指向假 Cursor 上游）→ ForwardAsAnthropic 真正
// 走到 cursorExecuteAnthropic 执行入口并返回结果（不再 502），且出站请求发往 Cursor
// 原生协议上游（而非通用 OpenAI 上游 / OAuth token provider）。
func TestForwardAsAnthropic_CursorAccountWiredExecutionReachesEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const body = `{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`

	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		1: {ID: 1, CodeEncrypted: "ACTIVATION-CODE-ONE"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	svc, c := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))
	require.NotNil(t, svc.cursorExecution, "测试执行部件应已注入网关")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))

	account := cursorOutboundAccount(1, 1, "BEARER-ACCOUNT")

	res, err := svc.ForwardAsAnthropic(context.Background(), c, account, []byte(body), "", "")
	require.NoError(t, err, "Cursor 账号应经由 cursor 执行入口成功返回，不再 502")
	require.NotNil(t, res, "应返回 OpenAI 形状的结果")
	// 反证：cursor 执行入口确实被调用——出站请求已发往 Cursor 原生协议上游。
	require.NotEmpty(t, up.requests, "cursor 执行入口必须发起出站请求（证明走了 Cursor 原生协议，而非通用 OpenAI 路径）")
}

// TestForwardAsAnthropic_OpenAIOAuthAccountNotRoutedToCursor 覆盖 D2：openai OAuth 账号
// 不得被新加的 cursor 分支拦截，仍走原始 OpenAI 上游路径（行为不变）。核心回归点是：
// 返回错误绝不可能是 ErrCursorExecutionUnavailable，也绝不经过 cursor 分支。
func TestForwardAsAnthropic_OpenAIOAuthAccountNotRoutedToCursor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const body = `{"model":"gpt-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(`data: {"id":"r1","object":"response","model":"gpt-5","status":"completed"}` + "\n\n" + `data: [DONE]` + "\n\n")),
	}}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
			},
		},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          7001,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Credentials: map[string]any{"access_token": "openai-oauth-token"},
	}
	require.False(t, isCursorAccount(account), "前置谓词：openai 账号不得被判为 cursor")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, []byte(body), "", "")
	// 核心回归：openai OAuth 账号绝不得被 cursor 分支拦截。
	require.NotErrorIs(t, err, ErrCursorExecutionUnavailable,
		"openai OAuth 账号不得被新加的 cursor 分支拦截")
	if err != nil {
		require.NotContains(t, err.Error(), "cursor:",
			"openai 账号不得进入 cursor 分支（不得出现 cursor 分支专属错误）")
		require.NotContains(t, err.Error(), "not an openai oauth account",
			"openai OAuth 账号本就满足 token provider 前置条件，不得产出该错误")
	}
	// 正证：openai 账号走原始路径，应触达通用 OpenAI 上游（而非被 cursor 分支收口）。
	require.NotNil(t, upstream.lastReq,
		"openai 账号必须走原始 OpenAI 上游路径并发起请求")
}
