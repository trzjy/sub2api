//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// 本文件是 CR-C「Responses 真实入口回归锁定」单（方案
// /root/cursor-refactor-docs/cursor-integration-implementation-plan.md §24.5 与
// §25.8 第 1 步）交付的回归测试，并在 W2-A1 收敛重构后按**新契约转绿**。
//
// 锁定的生产缺陷（W2-A1 已修复）：
//
//	OpenAIGatewayHandler.Responses → OpenAIGatewayService.Forward 的 Cursor 分支
//	把**入站 Responses 形状 body** 直接当 Chat Completions 解码（该结构只有 messages），
//	Messages 为空 → cursorConvertMessages 抛 `no convertible messages`，请求**未出站**，
//	客户端收 502。
//
// 新契约（§24.2-1）：Cursor 执行入口只接收类型化 apicompat.ChatCompletionsRequest；
// Responses 入口必须按 Responses 协议解码一次，再用既有共享转换器转成 Chat
// Completions。因此本文件断言：
//
//  1. 有效 Codex 形状请求（string input / array input + custom/namespace 工具）不再
//     因"形状未被转换"被拒绝，且**确实出站**到 Cursor 原生端点；
//  2. 任何情况下都不得落到 OpenAI HTTP 出站（cursor 账号在通用路径之前分流）；
//  3. 无效工具选择在协议内明确失败且未出站（§24.2-7）。
//
// 测试资源：httptest 回环 + 包内替身，无 DB、无真实网络。

const (
	cursorResponsesRegressionQuotaKeyID = int64(780001)
	cursorResponsesRegressionGroupID    = int64(780010)
	cursorResponsesRegressionAccountID  = int64(780011)
	cursorResponsesRegressionModel      = "gpt-5.1-codex"

	// cursorResponsesRegressionShapeRejection 是旧链锁定的缺陷错误串（新契约下不得出现）。
	cursorResponsesRegressionShapeRejection = "no convertible messages"
)

type cursorResponsesRegressionAccountRepo struct {
	service.AccountRepository
	accounts []service.Account
}

func (r cursorResponsesRegressionAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}

func (r cursorResponsesRegressionAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, _ string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r cursorResponsesRegressionAccountRepo) ListSchedulableByGroupIDAndPlatforms(_ context.Context, _ int64, _ []string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r cursorResponsesRegressionAccountRepo) ListSchedulableByPlatform(_ context.Context, _ string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r cursorResponsesRegressionAccountRepo) ListSchedulableUngroupedByPlatform(_ context.Context, _ string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

// ListModelAvailabilityCandidates 供「选号失败 → classifyNoAccountError →
// DiagnoseModelAvailabilityForPlatform」诊断链复用同一份账号视图。
//
// 该方法此前只由内嵌的 service.AccountRepository 接口提升而来，接口为 nil 时提升方法会在
// 调用瞬间 panic（nil 接口解引用），把真实失败掩盖成 responses_panic_recovered。此处显式实现，
// 让诊断读到本夹具的账号事实，失败以断言形式暴露而不是 panic。
func (r cursorResponsesRegressionAccountRepo) ListModelAvailabilityCandidates(_ context.Context, _ *int64, _ []string, _ bool) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

type cursorResponsesRegressionHTTPUpstream struct {
	service.HTTPUpstream
	mu    sync.Mutex
	calls int
}

func (u *cursorResponsesRegressionHTTPUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls++
	u.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       http.NoBody,
	}, nil
}

func (u *cursorResponsesRegressionHTTPUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// cursorResponsesRegressionNodeTransport 提供节点发现（固定基址，不打网络）。
type cursorResponsesRegressionNodeTransport struct{}

func (cursorResponsesRegressionNodeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := `{"http1":"http://127.0.0.1:18999","http2":"http://127.0.0.1:18999"}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		Request:    req,
	}, nil
}

// cursorResponsesRegressionAgentTransport 是 cursor AgentRun 出站替身：返回合法帧流并计数。
type cursorResponsesRegressionAgentTransport struct {
	mu       sync.Mutex
	attempts int
}

func (tr *cursorResponsesRegressionAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.mu.Lock()
	tr.attempts++
	tr.mu.Unlock()
	body := append(cursorRegressionTextDeltaFrame("Hello"), cursorRegressionTurnEndedFrame()...)
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/connect+proto"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

func (tr *cursorResponsesRegressionAgentTransport) attemptCount() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.attempts
}

func cursorRegressionEnvelope(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = 0
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func cursorRegressionAppendVarintField(b []byte, field int, v uint64) []byte {
	b = append(b, byte(field<<3|0))
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func cursorRegressionAppendBytesField(b []byte, field int, payload []byte) []byte {
	b = append(b, byte(field<<3|2))
	n := len(payload)
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	b = append(b, byte(n))
	return append(b, payload...)
}

// cursorRegressionTextDeltaFrame 构造 AgentServerMessage.f1(InteractionUpdate).f1(TextDelta).f1(text)。
func cursorRegressionTextDeltaFrame(text string) []byte {
	delta := cursorRegressionAppendBytesField(nil, 1, []byte(text))
	update := cursorRegressionAppendBytesField(nil, 1, delta)
	return cursorRegressionEnvelope(cursorRegressionAppendBytesField(nil, 1, update))
}

// cursorRegressionTurnEndedFrame 构造 turn_ended（真实 usage 载体）。
func cursorRegressionTurnEndedFrame() []byte {
	turn := cursorRegressionAppendVarintField(nil, 1, 11)
	turn = cursorRegressionAppendVarintField(turn, 2, 5)
	update := cursorRegressionAppendBytesField(nil, 14, turn)
	return cursorRegressionEnvelope(cursorRegressionAppendBytesField(nil, 1, update))
}

// 鉴权三件套替身：给出一条可解析的 quota key 行（device 已持久化分支），
// 使执行部件能真实走到出站；其余方法由内嵌接口兜底（本用例不会调用）。
type cursorResponsesRegressionQuotaKeys struct {
	service.QuotaKeyRepository
	key *ent.QuotaKey
}

func (r cursorResponsesRegressionQuotaKeys) GetByID(_ context.Context, _ int64) (*ent.QuotaKey, error) {
	return r.key, nil
}

func (r cursorResponsesRegressionQuotaKeys) ListAccountsByQuotaKey(_ context.Context, _ int64) ([]*ent.Account, error) {
	return nil, nil
}

type cursorResponsesRegressionQuotaKeyEnc struct {
	service.QuotaKeyEncryptor
}

func (cursorResponsesRegressionQuotaKeyEnc) Decrypt([]byte) ([]byte, error) {
	return []byte("ACTIVATION-CODE-REGRESSION"), nil
}

type cursorResponsesRegressionDeviceIDs struct {
	service.CursorDeviceIDResolver
}

func (cursorResponsesRegressionDeviceIDs) DeviceIDFor(context.Context, int64) (string, error) {
	return "regression-device-id", nil
}

// cursorResponsesRegressionQuotaKeySource 是来源门（A04-G）两个窄读取器的替身：
// 归属激活码状态 + 额度快照。
//
// A04-G 之后「绑定激活码的 cursor 账号」必须能读到「码未终态 + 快照新鲜且真实剩余 > 0」
// 才进候选池，否则被明确剔除（fixture 未装读取器时表现为 pool=1 / ineligible=1）。
// 本替身给出的正是"来源可用"这一合法事实，使准入结论回到账号/协议适配本身。
type cursorResponsesRegressionQuotaKeySource struct{}

func (cursorResponsesRegressionQuotaKeySource) CursorQuotaKeyStatus(context.Context, int64) (string, error) {
	return domain.QuotaKeyStatusActive, nil
}

func (cursorResponsesRegressionQuotaKeySource) CursorQuotaKeyUsageSnapshot(context.Context, int64) ([]byte, error) {
	checkedAt := time.Now().UTC().Format(time.RFC3339)
	return []byte(`{"total":400,"used":86,"status_code":200,"checked_at":"` + checkedAt + `","error_code":""}`), nil
}

// installCursorResponsesRegressionSourceGate 装配来源门的两个读取器，并在用例结束时
// 还原为未安装（包级单例，避免污染同包其它用例）。
func installCursorResponsesRegressionSourceGate(t *testing.T) {
	t.Helper()
	source := cursorResponsesRegressionQuotaKeySource{}
	service.InstallCursorQuotaKeyStateProvider(source)
	service.InstallCursorQuotaKeyUsageSnapshotProvider(source)
	t.Cleanup(func() {
		service.InstallCursorQuotaKeyStateProvider(nil)
		service.InstallCursorQuotaKeyUsageSnapshotProvider(nil)
	})
}

// newCursorResponsesRegressionFixture 装配 handler（真实网关 + 真实协议转换 + 唯一执行部件）。
func newCursorResponsesRegressionFixture(t *testing.T) (*OpenAIGatewayHandler, *cursorResponsesRegressionHTTPUpstream, *cursorResponsesRegressionAgentTransport) {
	t.Helper()

	quotaKeyID := cursorResponsesRegressionQuotaKeyID
	account := service.Account{
		ID:            cursorResponsesRegressionAccountID,
		Name:          "cursor-responses-regression",
		Platform:      service.PlatformCursor,
		Type:          service.AccountTypeAPIKey,
		Status:        service.StatusActive,
		Schedulable:   true,
		Concurrency:   0,
		QuotaKeyID:    &quotaKeyID,
		GroupIDs:      []int64{cursorResponsesRegressionGroupID},
		AccountGroups: []service.AccountGroup{{AccountID: cursorResponsesRegressionAccountID, GroupID: cursorResponsesRegressionGroupID}},
		Credentials: map[string]any{
			"access_token": "cursor-regression-token",
			"model_mapping": map[string]any{
				cursorResponsesRegressionModel: cursorResponsesRegressionModel,
			},
		},
	}

	repo := cursorResponsesRegressionAccountRepo{accounts: []service.Account{account}}
	upstream := &cursorResponsesRegressionHTTPUpstream{}
	transport := &cursorResponsesRegressionAgentTransport{}
	cfg := &config.Config{RunMode: config.RunModeSimple}

	// A04-G 来源门：本账号绑定了归属激活码（QuotaKeyID），来源门要求「码状态 + 新鲜额度
	// 快照」两个读取器齐备且证明仍有剩余，账号才有资格进入候选池。
	installCursorResponsesRegressionSourceGate(t)

	// 唯一执行部件（§24.2-2 直接 DI）：鉴权依赖为替身，transport 为假出站。
	nodes := service.NewCursorUpstreamNodesResolver(
		service.WithCursorNodesEndpoint("http://127.0.0.1:18998/nodes"),
		service.WithCursorNodesHTTPClient(&http.Client{Transport: cursorResponsesRegressionNodeTransport{}}),
		service.WithCursorNodesCacheTTL(0),
	)
	forwarder := service.NewCursorUpstreamForwarder(
		service.WithCursorUpstreamNodes(nodes),
		service.WithCursorUpstreamHTTPClient(&http.Client{Transport: transport}),
	)
	deviceID := "regression-device-id"
	cursorExec, err := service.NewCursorExecutionService(
		cursorResponsesRegressionQuotaKeys{key: &ent.QuotaKey{
			ID:            cursorResponsesRegressionQuotaKeyID,
			CodeEncrypted: "encrypted-activation-code",
			DeviceID:      &deviceID,
		}},
		cursorResponsesRegressionQuotaKeyEnc{},
		cursorResponsesRegressionDeviceIDs{},
		forwarder,
	)
	require.NoError(t, err)
	t.Cleanup(cursorExec.Stop)

	gatewayService := service.NewOpenAIGatewayService(
		repo,
		nil, // usageLogRepo
		nil, // usageBillingRepo
		nil, // userRepo
		nil, // userSubRepo
		nil, // userGroupRateRepo
		nil, // cache
		cfg,
		nil, // schedulerSnapshot
		nil, // concurrencyService
		nil, // billingService
		nil, // rateLimitService
		nil, // billingCacheService
		upstream,
		nil, // deferredService
		nil, // openAITokenProvider
		nil, // grokTokenProvider
		nil, // resolver
		nil, // channelService
		nil, // balanceNotifyService
		nil, // settingService
		nil, // userPlatformQuotaRepo
		nil, // visionRouting
		cursorExec,
	)

	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	concurrencyService := service.NewConcurrencyService(&fakeConcurrencyCache{})

	h := NewOpenAIGatewayHandler(
		gatewayService,
		nil, // coreGatewayService
		nil, // settingService
		concurrencyService,
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, // usageRecordWorkerPool
		nil, // errorPassthroughService
		nil, // contentModerationService
		nil, // opsService
		cfg,
	)
	return h, upstream, transport
}

func newCursorResponsesRegressionContext(t *testing.T, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	groupID := cursorResponsesRegressionGroupID
	group := &service.Group{
		ID:       groupID,
		Hydrated: true,
		Platform: service.PlatformOpenAI,
		Status:   service.StatusActive,
	}
	apiKey := &service.APIKey{
		ID:      780012,
		UserID:  780013,
		GroupID: &groupID,
		Group:   group,
		Status:  service.StatusActive,
		User:    &service.User{ID: 780013, Concurrency: 10, Balance: 100},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	return c, rec
}

type cursorResponsesRegressionSnapshot struct {
	status              int
	body                string
	forwardErrors       []string
	openAIUpstreamCalls int
	cursorAttempts      int
}

func forwardFailureLogErrors(observed *observer.ObservedLogs) []string {
	var messages []string
	for _, entry := range observed.FilterMessage("openai.forward_failed").All() {
		for _, field := range entry.Context {
			if field.Key != "error" {
				continue
			}
			if err, ok := field.Interface.(error); ok && err != nil {
				messages = append(messages, err.Error())
				continue
			}
			if field.String != "" {
				messages = append(messages, field.String)
			}
		}
	}
	return messages
}

func captureCursorResponsesRegressionSnapshot(t *testing.T, body []byte) cursorResponsesRegressionSnapshot {
	t.Helper()

	gin.SetMode(gin.TestMode)

	observerCore, observed := observer.New(zap.DebugLevel)
	requestCtx := logger.IntoContext(context.Background(), zap.New(observerCore))

	h, upstream, transport := newCursorResponsesRegressionFixture(t)
	c, rec := newCursorResponsesRegressionContext(t, body)
	c.Request = c.Request.WithContext(requestCtx)

	h.Responses(c)

	snapshot := cursorResponsesRegressionSnapshot{
		status:              rec.Code,
		body:                rec.Body.String(),
		forwardErrors:       forwardFailureLogErrors(observed),
		openAIUpstreamCalls: upstream.callCount(),
		cursorAttempts:      transport.attemptCount(),
	}
	t.Logf("status=%d body=%s", snapshot.status, snapshot.body)
	t.Logf("forward_failed errors=%v", snapshot.forwardErrors)
	t.Logf("openai_http_upstream_calls=%d cursor_outbound_attempts=%d",
		snapshot.openAIUpstreamCalls, snapshot.cursorAttempts)
	for _, entry := range observed.All() {
		if entry.Level >= zap.WarnLevel {
			t.Logf("log level=%s msg=%s fields=%v", entry.Level, entry.Message, entry.ContextMap())
		}
	}
	return snapshot
}

func snapshotHasShapeRejection(snapshot cursorResponsesRegressionSnapshot) bool {
	for _, message := range snapshot.forwardErrors {
		if bytes.Contains([]byte(message), []byte(cursorResponsesRegressionShapeRejection)) {
			return true
		}
	}
	return false
}

// requireCursorResponsesRegressionGreen 是绿锁（W2-A1 转绿后的契约）。
func requireCursorResponsesRegressionGreen(t *testing.T, snapshot cursorResponsesRegressionSnapshot) {
	t.Helper()

	require.Zero(t, snapshot.openAIUpstreamCalls, "cursor 账号的请求不得落到 OpenAI HTTP 出站")
	require.False(t, snapshotHasShapeRejection(snapshot),
		"Responses input 必须被转换成 Chat messages；status=%d body=%s forward_failed=%v",
		snapshot.status, snapshot.body, snapshot.forwardErrors)
	require.Equal(t, 1, snapshot.cursorAttempts,
		"有效请求必须真实走一次 Cursor AgentRun 出站；status=%d body=%s", snapshot.status, snapshot.body)
	require.Equal(t, http.StatusOK, snapshot.status,
		"有效 Codex 形状请求不得再 502；body=%s forward_failed=%v", snapshot.body, snapshot.forwardErrors)
	require.Contains(t, snapshot.body, "Hello", "真实文本必须写回客户端；body=%s", snapshot.body)
}

// TestCursorResponsesRegression_StringInputWithToolsStream 锁定 Codex 最常见入站形状。
func TestCursorResponsesRegression_StringInputWithToolsStream(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.1-codex",
		"instructions": "You are Codex, a coding agent. Follow the repository conventions.",
		"input": "List the files in the repository root and summarize what each one does.",
		"tools": [
			{
				"type": "function",
				"name": "shell",
				"description": "Run a shell command in the sandbox.",
				"parameters": {
					"type": "object",
					"properties": {"command": {"type": "string", "description": "command to run"}},
					"required": ["command"],
					"additionalProperties": false
				},
				"strict": false
			}
		],
		"tool_choice": "auto",
		"parallel_tool_calls": true,
		"stream": true,
		"store": false
	}`)

	snapshot := captureCursorResponsesRegressionSnapshot(t, body)
	requireCursorResponsesRegressionGreen(t, snapshot)
}

// TestCursorResponsesRegression_ArrayInputWithCustomAndNamespaceTools 锁定第二类形状。
func TestCursorResponsesRegression_ArrayInputWithCustomAndNamespaceTools(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.1-codex",
		"instructions": "You are Codex, a coding agent. Follow the repository conventions.",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "Read README.md and apply the patch it suggests."}
				]
			},
			{
				"type": "function_call",
				"name": "shell",
				"arguments": "{\"command\":\"cat README.md\"}",
				"call_id": "call_regression_1"
			},
			{
				"type": "function_call_output",
				"call_id": "call_regression_1",
				"output": "# exec-card-crc\n\nApply the documented patch.\n"
			}
		],
		"tools": [
			{
				"type": "custom",
				"name": "apply_patch",
				"description": "Apply a patch to the working tree.",
				"format": {
					"type": "grammar",
					"syntax": "lark",
					"definition": "start: begin_patch hunk+ end_patch"
				}
			},
			{
				"type": "namespace",
				"name": "mcp__exec_card",
				"description": "Workspace helper tools.",
				"tools": [
					{
						"type": "function",
						"name": "ping",
						"description": "Ping the workspace helper.",
						"parameters": {"type": "object", "properties": {}, "additionalProperties": false}
					}
				]
			}
		],
		"tool_choice": "auto",
		"stream": false,
		"store": false
	}`)

	snapshot := captureCursorResponsesRegressionSnapshot(t, body)
	requireCursorResponsesRegressionGreen(t, snapshot)
	require.True(t, json.Valid([]byte(snapshot.body)), "非流式 Responses 响应必须是合法 JSON；body=%s", snapshot.body)
	assert.Equal(t, "response", gjson.Get(snapshot.body, "object").String(), "body=%s", snapshot.body)
}

// TestCursorResponsesRegression_InvalidToolChoiceNeverLeaves 锁定 §24.2-7：
// 无效工具选择在适配边界明确失败且**未出站**。
func TestCursorResponsesRegression_InvalidToolChoiceNeverLeaves(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.1-codex",
		"input": "say hi",
		"tools": [
			{"type": "function", "name": "shell", "parameters": {"type": "object", "properties": {}}}
		],
		"tool_choice": {"type": "function", "function": {"name": "not_declared"}},
		"stream": false
	}`)

	snapshot := captureCursorResponsesRegressionSnapshot(t, body)

	require.Zero(t, snapshot.openAIUpstreamCalls, "无效工具选择不得落到 OpenAI HTTP 出站")
	require.Zero(t, snapshot.cursorAttempts, "无效工具选择必须在出站前失败（未出站）")
	require.GreaterOrEqual(t, snapshot.status, http.StatusBadRequest,
		"无效工具选择必须是失败响应；body=%s", snapshot.body)
}
