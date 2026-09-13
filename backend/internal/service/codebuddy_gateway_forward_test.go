package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// healthyCodeBuddyGatewayTestAccount 构造一个用于转发测试的 codebuddy OAuth 账号，
// 其 access_token 直接落库在 credentials（与 CodeBuddyTokenRefresher 行为一致）。
func healthyCodeBuddyGatewayTestAccount(id int64, token string) *Account {
	return &Account{
		ID:          id,
		Name:        "codebuddy",
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":  token,
			"refresh_token": "refresh-token",
			"uid":           "u-1",
			"enterprise_id": "e-1",
			"domain":        "tencent.com",
		},
	}
}

// TestForwardCodeBuddy_ContentAuditRetryReplacesSystemThenSucceeds 覆盖 §2.6 行 6 的
// 完整降级重试语义：默认配置（sanitize 开启）下，首次带指纹 system 触发 400 审核拦截，
// 重试除强制脱敏外还替换/去除 system 消息（最后手段），最终 200 成功。
//
// 关键验证点：默认脱敏规则的白名单无法命中测试指纹（"Claude Code runtime fingerprint
// present" 不在 codeBuddyDefaultSanitizePatterns 中），因此旧实现（重试仅强制 Sanitize=true）
// 会产生与首轮完全相同的 body 而再次 400；本实现额外 ReplaceSystem 使重试 body 发生改变并成功。
func TestForwardCodeBuddy_ContentAuditRetryReplacesSystemThenSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const fingerprint = "Claude Code runtime fingerprint present"
	body := []byte(`{
		"model":"codebuddy-model",
		"stream":false,
		"messages":[
			{"role":"system","content":"` + fingerprint + `"},
			{"role":"user","content":"hi"}
		]
	}`)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	// 首轮：400 + 审核关键词（blocked by security policy）。
	audit400 := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"code":400,"msg":"blocked by security policy: prohibited fingerprint"}`)),
	}
	// 重试轮：200 + 标准 chat.completion JSON（非 SSE，由 handleNonStreamingResponse 走 JSON 分支聚合）。
	ok200 := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl-1","object":"chat.completion","model":"codebuddy-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)),
	}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{audit400, ok200}}

	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:           false,
				AllowInsecureHTTP: true,
			},
		},
		Gateway: config.GatewayConfig{
			CodeBuddy: config.GatewayCodeBuddyConfig{
				SanitizeEnabled: true, // 默认配置：脱敏开启
			},
		},
	}

	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}

	account := healthyCodeBuddyGatewayTestAccount(7701, "access-token")

	result, err := svc.forwardCodeBuddy(
		context.Background(), c, account, body, "codebuddy-model", false, time.Now(),
	)

	require.NoError(t, err)
	require.NotNil(t, result)

	// 触发了一次审核重试：共发出两轮请求。
	require.Len(t, upstream.bodies, 2, "expected exactly one content-audit retry (2 requests total)")

	first := string(upstream.bodies[0])
	second := string(upstream.bodies[1])

	// 首轮已脱敏，但测试指纹不在默认脱敏白名单中，故首轮 body 仍含指纹。
	require.Contains(t, first, fingerprint, "first attempt should still carry the fingerprint (default sanitize did not match it)")

	// 重试轮作为最后手段替换/去除了 system 消息，指纹不再出现，且请求体随之改变。
	require.NotEqual(t, first, second, "retry body must differ from first attempt (system replacement is the last-resort fix)")
	require.NotContains(t, second, fingerprint, "retry must have replaced/removed the system message so the fingerprint is gone")

	// 最终响应成功聚合（模型名回填为原始请求模型）。
	require.Equal(t, "codebuddy-model", result.Model)
}

// TestForwardCodeBuddy_NoRetryWhenFirstSucceeds 验证正常 200 路径不触发重试（仅一轮请求），
// 且出站请求头不含 X-Refresh-Token（§2.4 安全红线）。
func TestForwardCodeBuddy_NoRetryWhenFirstSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"codebuddy-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	ok200 := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl-2","object":"chat.completion","model":"codebuddy-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)),
	}
	upstream := &httpUpstreamRecorder{resp: ok200}

	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:           false,
				AllowInsecureHTTP: true,
			},
		},
		Gateway: config.GatewayConfig{
			CodeBuddy: config.GatewayCodeBuddyConfig{
				SanitizeEnabled: true,
			},
		},
	}

	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}

	account := healthyCodeBuddyGatewayTestAccount(7702, "access-token")

	result, err := svc.forwardCodeBuddy(
		context.Background(), c, account, body, "codebuddy-model", false, time.Now(),
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 1, "successful first attempt must not retry")

	// §2.4 安全红线：chat 转发请求绝不携带 X-Refresh-Token。
	require.Empty(t, upstream.lastReq.Header.Get("X-Refresh-Token"), "chat request must never carry X-Refresh-Token")
	// §2.4 指纹头对齐：Authorization Bearer 与 User-Agent。
	require.Equal(t, "Bearer access-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, CodeBuddyClientUA, upstream.lastReq.Header.Get("User-Agent"))
}

// TestRedactCodeBuddyUpstreamErrorBody 钉住 A2 脱敏规则：已知 uid/nickname/enterprise_id
// 精确替换，"user <digits>" 兜底替换；短值（<4）不做精确替换以免误伤。
func TestRedactCodeBuddyUpstreamErrorBody(t *testing.T) {
	account := &Account{Credentials: map[string]any{
		"uid":            "123456",
		"nickname":       "CodeFarmerX",
		"enterprise_id":  "e-90001",
		"session_remark": "ab", // 短值不参与精确替换
	}}

	cases := []struct {
		name  string
		body  string
		deny  []string
		allow []string
	}{
		{
			name:  "offline session carries uid and nickname",
			body:  `{"code":400,"msg":"Offline user session for user 123456 (CodeFarmerX, ent e-90001)"}`,
			deny:  []string{"123456", "CodeFarmerX", "e-90001"},
			allow: []string{`"code":400`, "Offline user session"},
		},
		{
			name:  "unknown uid falls back to user-digits pattern",
			body:  `{"code":400,"msg":"session expired for user 987654321"}`,
			deny:  []string{"987654321"},
			allow: []string{"user ****"},
		},
		{
			name:  "business error code survives",
			body:  `{"code":12153,"msg":"offline user session not found"}`,
			deny:  nil,
			allow: []string{"12153", "offline user session not found"},
		},
		{
			name:  "rate limit text untouched",
			body:  `{"code":6004,"msg":"rate limit exceeded, 将在 2026-09-13 12:00:00 重置"}`,
			deny:  nil,
			allow: []string{"6004", "2026-09-13 12:00:00"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(redactCodeBuddyUpstreamErrorBody([]byte(tc.body), account))
			for _, d := range tc.deny {
				require.NotContains(t, out, d)
			}
			for _, a := range tc.allow {
				require.Contains(t, out, a)
			}
		})
	}

	require.Empty(t, redactCodeBuddyUpstreamErrorBody(nil, account))
	require.Equal(t, "x", string(redactCodeBuddyUpstreamErrorBody([]byte("x"), nil)))
}

// TestForwardCodeBuddy_UpstreamErrorRedactsAccountIdentifiers 端到端验证 A2 修复：
// 上游 400 错误文案携带账号 uid/nickname/enterprise_id 时，回传给下游 API 调用方的
// 响应体不含任何账号标识明文，状态码与错误类型语义保留。
//
// 覆盖两种上游信封：error.message 形（extractUpstreamErrorMessage 可提取，会真实
// 透传给下游）与 codebuddy 信封 {"code":..,"msg":..}（提取为空走兜底文案，回归钉住
// 该路径同样不泄漏）。
func TestForwardCodeBuddy_UpstreamErrorRedactsAccountIdentifiers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	bodies := map[string]string{
		"openai_error_envelope": `{"error":{"type":"invalid_request_error","message":"Offline user session for user 123456 (CodeFarmerX, ent e-90001)"}}`,
		"codebuddy_envelope":    `{"code":400,"msg":"Offline user session for user 123456 (CodeFarmerX, ent e-90001)"}`,
	}
	for name, upstreamBody := range bodies {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"model":"codebuddy-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

			err400 := &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(upstreamBody)),
			}
			upstream := &httpUpstreamRecorder{resp: err400}

			cfg := &config.Config{
				Security: config.SecurityConfig{
					URLAllowlist: config.URLAllowlistConfig{
						Enabled:           false,
						AllowInsecureHTTP: true,
					},
				},
				Gateway: config.GatewayConfig{
					CodeBuddy: config.GatewayCodeBuddyConfig{SanitizeEnabled: true},
				},
			}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}

			account := healthyCodeBuddyGatewayTestAccount(7703, "access-token")
			account.Credentials["uid"] = "123456"
			account.Credentials["nickname"] = "CodeFarmerX"
			account.Credentials["enterprise_id"] = "e-90001"

			_, err := svc.forwardCodeBuddy(context.Background(), c, account, body, "codebuddy-model", false, time.Now())
			require.Error(t, err, "upstream 400 must surface as a forward error")

			downstream := recorder.Body.String()
			// 泄漏面断言：下游响应体不含 uid / nickname / enterprise_id 明文。
			require.NotContains(t, downstream, "123456")
			require.NotContains(t, downstream, "CodeFarmerX")
			require.NotContains(t, downstream, "e-90001")
			// 语义保留：状态码与错误类型不变。
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Contains(t, downstream, "invalid_request_error")
			if name == "openai_error_envelope" {
				// 错误文案主体仍可读（仅标识被替换为占位符）。
				require.Contains(t, downstream, "Offline user session")
			}
		})
	}
}

// TestForwardAsChatCompletions_RoutesCodeBuddyThroughFingerprintPath 钉住 /v1/chat/completions
// 的第二层平台分发：codebuddy 必须走 forwardCodeBuddy（§2.4 指纹头 + §2.5 改写管线），
// 而不是通用 OpenAI 路径——后者对 OAuth 账号会打到 ChatGPT/Codex 后端并被 403 阻断。
//
// 活体验收 F6 第二轮：ForwardAsChatCompletions 只给 Grok 做了平台分支，codebuddy 漏了，
// 导致路由白名单修好后仍被通用路径截走。
func TestForwardAsChatCompletions_RoutesCodeBuddyThroughFingerprintPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"hy3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	ok200 := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n")),
	}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{ok200}}
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := healthyCodeBuddyGatewayTestAccount(7710, "access-token")

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotEmpty(t, upstream.requests, "codebuddy chat 必须发出上游请求")

	req := upstream.requests[0]
	// 必须命中 CodeBuddy 上游端点（通用路径会指向 ChatGPT/Codex 后端）。
	require.Equal(t, "https://copilot.tencent.com", req.URL.Scheme+"://"+req.URL.Host)
	require.Equal(t, "/v2/chat/completions", req.URL.Path)
	// §2.4 指纹头必须齐备。
	require.Equal(t, "https://www.codebuddy.cn", req.Header.Get("Origin"))
	require.Equal(t, "https://www.codebuddy.cn/", req.Header.Get("Referer"))
	require.Equal(t, "SaaS", req.Header.Get("X-Product"))
	require.Equal(t, "u-1", req.Header.Get("X-User-Id"))
	require.Equal(t, "tencent.com", req.Header.Get("X-Domain"))
	require.Equal(t, "Bearer access-token", req.Header.Get("Authorization"))
	require.Empty(t, req.Header.Get("X-Refresh-Token"), "§2.4 安全红线：chat 不带 X-Refresh-Token")
}

// TestForwardCodeBuddy_NonStreamAggregatesSSEToJSON 钉住 F9：入站 stream:false 时，
// 上游被强制流式返回的 chat.completion.chunk SSE 必须聚合为单条 chat.completion JSON，
// 而不是原样回写 SSE（通用 handleSSEToJSON 只覆盖 Codex/Responses 形状）。
func TestForwardCodeBuddy_NonStreamAggregatesSSEToJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"hy3","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	sse := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"PONG\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := healthyCodeBuddyGatewayTestAccount(7711, "access-token")

	result, err := svc.forwardCodeBuddy(context.Background(), c, account, body, "hy3", false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Contains(t, recorder.Header().Get("Content-Type"), "application/json",
		"非流式响应必须是 JSON，不能是 text/event-stream")
	out := recorder.Body.String()
	require.NotContains(t, out, "data:", "不得原样回写 SSE 帧")

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got), "响应必须是单条 chat.completion JSON")
	require.Equal(t, "chat.completion", got["object"])
	choice := got["choices"].([]any)[0].(map[string]any)
	require.Equal(t, "stop", choice["finish_reason"])
	message := choice["message"].(map[string]any)
	require.Equal(t, "PONG", message["content"])
	require.Equal(t, float64(2), got["usage"].(map[string]any)["total_tokens"])
}
