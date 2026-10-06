//go:build live

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TestLiveNonstreamToStream 对真实生产上游(corealgos.com,OpenAI 兼容中转)做
// 非流式→流式转换端到端实测(方案 docs-local/nonstream-to-stream-conversion-plan.md §4.8)。
//
// 单测全部用 httptest 假上游;本卡补真实网络 SSE 的端到端证据:谓词命中 → 上游强制
// 流式 → 聚合 → 非流式 JSON 回写。仅当三个环境变量齐备时执行,否则 t.Skip(零网络行为):
//
//	N2S_LIVE_BASE_URL  上游 base_url(必须 https,无默认值)
//	N2S_LIVE_API_KEY   上游 api_key(无默认值,绝不落盘/打印)
//	N2S_LIVE_MODEL     上游模型名(无默认值)
//
// 真实注入由验收方完成;执行会话仅用占位环境变量验证 Skip 路径。
func TestLiveNonstreamToStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	baseURL := strings.TrimSpace(os.Getenv("N2S_LIVE_BASE_URL"))
	apiKey := strings.TrimSpace(os.Getenv("N2S_LIVE_API_KEY"))
	model := strings.TrimSpace(os.Getenv("N2S_LIVE_MODEL"))
	if baseURL == "" || apiKey == "" || model == "" {
		t.Skip("live upstream test skipped: set N2S_LIVE_BASE_URL / N2S_LIVE_API_KEY / N2S_LIVE_MODEL to run")
	}

	// 安全护栏:真实密钥只允许经 https 出站,禁止明文 http 误传。
	parsed, err := url.Parse(baseURL)
	require.NoError(t, err, "N2S_LIVE_BASE_URL must be a valid URL")
	require.Equal(t, "https", strings.ToLower(parsed.Scheme),
		"N2S_LIVE_BASE_URL must use https; refusing to transmit the live api_key over plaintext")

	// 构造开启非流式→流式转换的真实 config（与单测同构,仅做最小必要字段）。
	// NonstreamToStreamDisabled 零值 false 即启用：此处省略该字段，依赖零值安全。
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:           false,
				AllowInsecureHTTP: false,
			},
		},
	}

	// platform=other + apikey:命中 shouldConvertNonstreamToStream 的
	// `account.Platform == PlatformOther` 分支(无官方 CN 端点概念),保证走完整转换路径。
	account := &Account{
		ID:          1,
		Name:        "n2s-live-upstream",
		Platform:    PlatformOther,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  apiKey,
			"base_url": baseURL,
		},
	}

	// 真实网络出站 + 记录出站请求体/上游原始响应体,供请求形态证据。
	upstream := newLiveCaptureUpstream()
	svc := &OpenAIGatewayService{
		cfg:          cfg,
		httpUpstream: upstream,
	}

	// stream:false 的 CC body,max_tokens=64,prompt 固定短语。
	body := []byte(`{"messages":[{"role":"user","content":"Reply with exactly the word: pong"}],"stream":false,"max_tokens":64}`)
	body, err = sjson.SetBytes(body, "model", model)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	result, err := svc.forwardAsRawChatCompletions(ctx, c, account, body, "")
	require.NoError(t, err, "convert-and-forward against live upstream must succeed")
	require.NotNil(t, result)

	// ---- 1. HTTP 200 ----
	require.Equal(t, http.StatusOK, rec.Code)

	// ---- 2. 下游为合法非流式 CC JSON ----
	downstream := rec.Body.String()
	require.True(t, json.Valid([]byte(downstream)), "downstream body must be valid JSON")
	require.Equal(t, "chat.completion", gjson.Get(downstream, "object").String())
	require.Equal(t, "assistant", gjson.Get(downstream, "choices.0.message.role").String())
	require.NotEmpty(t, gjson.Get(downstream, "choices.0.message.content").String())
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.NotContains(t, downstream, "data: [DONE]", "converted response must be non-streaming JSON")

	// ---- 3. usage.total_tokens > 0(下游 JSON 与 ForwardResult 一致) ----
	require.Greater(t, gjson.Get(downstream, "usage.total_tokens").Int(), int64(0),
		"converted non-streaming response must carry a positive total_tokens")
	require.Positive(t, result.Usage.InputTokens+result.Usage.OutputTokens,
		"ForwardResult usage must be non-zero")

	// ---- 4. ForwardResult 非流式契约 ----
	require.False(t, result.Stream, "converted response must report Stream=false")

	// ---- 5. 请求形态证据:上游被强制流式 ----
	upstreamBody := upstream.capturedRequestBody()
	require.True(t, gjson.GetBytes(upstreamBody, "stream").Bool(),
		"upstream must receive stream=true (conversion forced)")
	require.True(t, gjson.GetBytes(upstreamBody, "stream_options.include_usage").Bool(),
		"upstream must receive stream_options.include_usage=true")
	require.Equal(t, model, gjson.GetBytes(upstreamBody, "model").String())

	// ---- 报告输出(内容可展示,密钥不可) ----
	t.Logf("live base host=%s path=%s", upstream.capturedHost(), upstream.capturedPath())
	t.Logf("upstream request form: stream=%v include_usage=%v model=%s max_tokens=%d",
		gjson.GetBytes(upstreamBody, "stream").Bool(),
		gjson.GetBytes(upstreamBody, "stream_options.include_usage").Bool(),
		gjson.GetBytes(upstreamBody, "model").String(),
		gjson.GetBytes(upstreamBody, "max_tokens").Int())

	t.Logf("downstream object=%s role=%s content=%q finish_reason=%s",
		gjson.Get(downstream, "object").String(),
		gjson.Get(downstream, "choices.0.message.role").String(),
		gjson.Get(downstream, "choices.0.message.content").String(),
		gjson.Get(downstream, "choices.0.finish_reason").String())

	t.Logf("usage: prompt=%d completion=%d total=%d",
		gjson.Get(downstream, "usage.prompt_tokens").Int(),
		gjson.Get(downstream, "usage.completion_tokens").Int(),
		gjson.Get(downstream, "usage.total_tokens").Int())

	t.Logf("ForwardResult: Stream=%v Model=%s BillingModel=%s UpstreamModel=%s Duration=%s Usage{in=%d out=%d}",
		result.Stream, result.Model, result.BillingModel, result.UpstreamModel, result.Duration,
		result.Usage.InputTokens, result.Usage.OutputTokens)

	// 上游原始 SSE 中的 usage 帧(stream 计费字段证据:include_usage 生效后上游回传 usage)。
	t.Logf("upstream raw SSE usage frame: %s", upstream.capturedUsageFrame())
}

// liveCaptureUpstream 是 live 测试专用的真实网络 HTTPUpstream 实现:
// 记录出站请求体与出站 URL,并把上游响应体 tee 到缓冲区留存原始 SSE(证据用)。
// 不复用 repository.NewHTTPUpstream(repository 反向依赖 service,会构成 import cycle),
// 只用标准库 http.Client 做真实出站。
type liveCaptureUpstream struct {
	client *http.Client

	mu       sync.Mutex
	reqBody  []byte
	reqURL   *url.URL
	respBody bytes.Buffer
}

func newLiveCaptureUpstream() *liveCaptureUpstream {
	return &liveCaptureUpstream{client: &http.Client{Timeout: 120 * time.Second}}
}

func (u *liveCaptureUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	if req != nil {
		u.mu.Lock()
		u.reqURL = req.URL
		u.mu.Unlock()
	}
	if req != nil && req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		u.mu.Lock()
		u.reqBody = b
		u.mu.Unlock()
		req.Body = io.NopCloser(bytes.NewReader(b))
	}

	resp, err := u.client.Do(req)
	if resp != nil && resp.Body != nil {
		resp.Body = &liveTeeReadCloser{
			reader: io.TeeReader(resp.Body, u),
			closer: resp.Body,
		}
	}
	return resp, err
}

func (u *liveCaptureUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

// Write 实现 io.Writer,把 tee 到的上游字节写入保持的缓冲区(加锁保护)。
func (u *liveCaptureUpstream) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.respBody.Write(p)
}

// liveTeeReadCloser 在读取上游响应体时同步留存副本,同时保留 Close 语义。
type liveTeeReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func (r *liveTeeReadCloser) Read(p []byte) (int, error) { return r.reader.Read(p) }
func (r *liveTeeReadCloser) Close() error               { return r.closer.Close() }

func (u *liveCaptureUpstream) capturedRequestBody() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]byte(nil), u.reqBody...)
}

func (u *liveCaptureUpstream) capturedHost() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.reqURL == nil {
		return ""
	}
	return u.reqURL.Host
}

func (u *liveCaptureUpstream) capturedPath() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.reqURL == nil {
		return ""
	}
	return u.reqURL.Path
}

// capturedUsageFrame 从留存的上游原始 SSE 中抽取最后一个含 usage 对象的 data 帧,
// 作为「上游被强制流式后回传 stream 计费 usage」的结构证据。
func (u *liveCaptureUpstream) capturedUsageFrame() string {
	u.mu.Lock()
	raw := u.respBody.String()
	u.mu.Unlock()

	last := ""
	for _, line := range strings.Split(raw, "\n") {
		payload, ok := extractOpenAISSEDataLine(line)
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" || trimmed == "[DONE]" {
			continue
		}
		if gjson.Get(trimmed, "usage").Exists() {
			last = trimmed
		}
	}
	return last
}
