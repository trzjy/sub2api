package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// openAIWSVisionHintResult 是 router wrapper 从 handler 返回后读取的
// 请求级含图 hint 观测结果。gin.Context 在 handler 返回后仍可 c.Get，
// wrapper 持有该 *gin.Context 引用，故可在连接结束后读取 hint。
type openAIWSVisionHintResult struct {
	hasImageInput bool
	known         bool
}

// newOpenAIWSVisionHintHarness 构建真实 WS 拨号 + 上游桩的端到端 harness。
// 与既有 openai_ws_v2_passthrough_cyber_test.go 的 harness 同构，但 wrapper
// 在 h.ResponsesWebSocket(c) 返回后读取 GetOpenAIHasImageInputHint(c) 并
// 经 channel 传递供断言（httptest dial 拿不到 *gin.Context 引用，因此不能
// 在测试主 goroutine 直接读 c）。
type openAIWSVisionHintHarness struct {
	clientConn  *coderws.Conn
	handlerDone <-chan struct{}
	hintCh      chan openAIWSVisionHintResult
}

func newOpenAIWSVisionHintHarness(t *testing.T, upstreamURL string) *openAIWSVisionHintHarness {
	t.Helper()
	gatewayCache := testutil.NewRedisGatewayCache(t)

	settingRepo := &contentModerationHandlerSettingRepo{values: map[string]string{
		service.SettingKeyRiskControlEnabled: "true",
	}}
	moderationSvc := service.NewContentModerationService(settingRepo, &contentModerationHandlerTestRepo{}, nil, nil, nil, nil, nil, nil)
	settingSvc := service.NewSettingService(settingRepo, nil)

	groupID := int64(4302)
	account := service.Account{
		ID:          9952,
		Name:        "openai-ws-vision-hint",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": upstreamURL},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
		},
	}
	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 3

	accountRepo := &openAIWSUsageHandlerAccountRepoStub{account: account}
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo, usageRepo, nil, nil, nil, nil, gatewayCache, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCacheSvc, nil, &service.DeferredService{},
		nil, nil, nil, nil, nil, settingSvc, nil, nil,
	)
	concurrencyCache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := &OpenAIGatewayHandler{
		gatewayService:           gatewaySvc,
		billingCacheService:      billingCacheSvc,
		apiKeyService:            &service.APIKeyService{},
		contentModerationService: moderationSvc,
		concurrencyHelper:        NewConcurrencyHelper(service.NewConcurrencyService(concurrencyCache), SSEPingFormatNone, time.Second),
	}

	apiKey := &service.APIKey{
		ID:      1852,
		Name:    "ws-vision-hint-key",
		Key:     "sk-handler-ws-vision-hint-test",
		GroupID: &groupID,
		User:    &service.User{ID: 1752, Status: service.StatusActive},
	}
	handlerDone := make(chan struct{})
	hintCh := make(chan openAIWSVisionHintResult, 1)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", func(c *gin.Context) {
		h.ResponsesWebSocket(c)
		// 链路依据：ResponsesWebSocket 内部在选号处（openai_gateway_handler.go
		// 约 :2643）读取 GetOpenAIHasImageInputHint(c) 的返回值，直接作为
		// hasImageInput 实参传给 SelectAccountWithSchedulerForCapability。
		// hint=true 即 requireVision=true。handler 返回后 gin.Context 仍有效，
		// 此处读取并下发供断言（httptest 拨号拿不到 *gin.Context 引用）。
		hasImageInput, known := service.GetOpenAIHasImageInputHint(c)
		hintCh <- openAIWSVisionHintResult{hasImageInput: hasImageInput, known: known}
		close(handlerDone)
	})
	handlerServer := httptest.NewServer(router)
	t.Cleanup(handlerServer.Close)

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(handlerServer.URL, "http")+"/openai/v1/responses", nil)
	cancelDial()
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientConn.CloseNow() })

	return &openAIWSVisionHintHarness{
		clientConn:  clientConn,
		handlerDone: handlerDone,
		hintCh:      hintCh,
	}
}

// waitUpstreamOnceRound 是上游桩 helper：Accept → 读首帧 → 回写完成事件 →
// 阻塞等连接关闭退出。
func waitUpstreamOnceRound(t *testing.T, w http.ResponseWriter, r *http.Request, completedEvent []byte) {
	t.Helper()
	conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
	_, _, err = conn.Read(readCtx)
	cancelRead()
	require.NoError(t, err)

	writeCtx, cancelWrite := context.WithTimeout(r.Context(), 3*time.Second)
	err = conn.Write(writeCtx, coderws.MessageText, completedEvent)
	cancelWrite()
	require.NoError(t, err)

	// 阻塞直到客户端关闭连接（handler 把关闭传播到上游后退出）。
	_, _, _ = conn.Read(r.Context())
}

// sendAndReceiveCompleted 发送首帧并读取上游返回的第一个事件。
func sendAndReceiveCompleted(t *testing.T, conn *coderws.Conn, payload string) string {
	t.Helper()
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err := conn.Write(writeCtx, coderws.MessageText, []byte(payload))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, event, err := conn.Read(readCtx)
	cancelRead()
	require.NoError(t, err)
	return string(event)
}

// waitHandlerHint 等待 handler 返回并被 wrapper 记录 hint，返回读取结果。
func (h *openAIWSVisionHintHarness) waitHandlerHint(t *testing.T) openAIWSVisionHintResult {
	t.Helper()
	select {
	case result := <-h.hintCh:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("websocket handler did not record hint")
		return openAIWSVisionHintResult{}
	}
}

// TestOpenAIResponsesWebSocketVisionHint_ImageInput 覆盖派发单核心断言 1：
// WS 首帧 body 含图片输入（responses 原生 input_image）⇒ hint known=true,
// hasImageInput=true。选号 requireVision 由该 hint 直接驱动（见 harness 注释的
// 链路依据），故 hint=true 即 requireVision=true。
func TestOpenAIResponsesWebSocketVisionHint_ImageInput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamDone := make(chan struct{})
	completedEvent := []byte(`{"type":"response.completed","response":{"id":"resp_vision_hint_image","model":"gpt-5.1","usage":{"input_tokens":5,"output_tokens":1}}}`)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		waitUpstreamOnceRound(t, w, r, completedEvent)
	}))
	defer upstreamServer.Close()
	harness := newOpenAIWSVisionHintHarness(t, upstreamServer.URL)

	firstPayload := `{"type":"response.create","model":"gpt-5.1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`
	event := sendAndReceiveCompleted(t, harness.clientConn, firstPayload)
	require.Contains(t, event, "resp_vision_hint_image")

	// 关闭客户端连接，让 handler 退出并在 wrapper 中记录 hint。
	require.NoError(t, harness.clientConn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case <-harness.handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("websocket handler did not exit")
	}
	select {
	case <-upstreamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream websocket did not exit")
	}

	result := harness.waitHandlerHint(t)
	require.True(t, result.known, "含图首帧必须留下 known=true 的请求级 hint")
	require.True(t, result.hasImageInput, "含图首帧必须判定 hasImageInput=true")
}

// TestOpenAIResponsesWebSocketVisionHint_TextOnly 覆盖派发单核心断言 2：
// WS 首帧为纯文本 ⇒ hint known=true, hasImageInput=false。
func TestOpenAIResponsesWebSocketVisionHint_TextOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamDone := make(chan struct{})
	completedEvent := []byte(`{"type":"response.completed","response":{"id":"resp_vision_hint_text","model":"gpt-5.1","usage":{"input_tokens":3,"output_tokens":1}}}`)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		waitUpstreamOnceRound(t, w, r, completedEvent)
	}))
	defer upstreamServer.Close()
	harness := newOpenAIWSVisionHintHarness(t, upstreamServer.URL)

	firstPayload := `{"type":"response.create","model":"gpt-5.1","input":"hello"}`
	event := sendAndReceiveCompleted(t, harness.clientConn, firstPayload)
	require.Contains(t, event, "resp_vision_hint_text")

	require.NoError(t, harness.clientConn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case <-harness.handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("websocket handler did not exit")
	}
	select {
	case <-upstreamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream websocket did not exit")
	}

	result := harness.waitHandlerHint(t)
	require.True(t, result.known, "纯文本首帧也必须留下 known=true 的请求级 hint")
	require.False(t, result.hasImageInput, "纯文本首帧必须判定 hasImageInput=false")
}