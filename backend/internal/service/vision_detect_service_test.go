package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 桩
// ---------------------------------------------------------------------------

// visionDetectUsageLogRepoStub 嵌入 UsageLogRepository 接口，仅覆盖 Create。
type visionDetectUsageLogRepoStub struct {
	UsageLogRepository
	inserted bool
	err      error
	calls    int
	lastLog  *UsageLog
}

func (s *visionDetectUsageLogRepoStub) Create(_ context.Context, log *UsageLog) (bool, error) {
	s.calls++
	s.lastLog = log
	return s.inserted, s.err
}

// visionDetectAccountRepoStub 返回固定账号。
type visionDetectAccountRepoStub struct {
	AccountRepository
	account *Account
}

func (s *visionDetectAccountRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
	return s.account, nil
}

// visionDetectCapRepoStub 嵌入 AccountModelCapabilityRepository，覆盖全部方法避免 nil panic。
type visionDetectCapRepoStub struct {
	AccountModelCapabilityRepository
	upserts int
	last    *model.AccountModelCapability
}

func (s *visionDetectCapRepoStub) Get(_ context.Context, _ int64, _, _ string) (*model.AccountModelCapability, error) {
	return nil, nil
}
func (s *visionDetectCapRepoStub) Upsert(_ context.Context, cap *model.AccountModelCapability) (*model.AccountModelCapability, error) {
	s.upserts++
	s.last = cap
	return cap, nil
}
func (s *visionDetectCapRepoStub) ListByAccount(_ context.Context, _ int64) ([]*model.AccountModelCapability, error) {
	return nil, nil
}
func (s *visionDetectCapRepoStub) DeleteByAccount(_ context.Context, _ int64) error {
	return nil
}

// visionDetectCapCacheStub 嵌入 AccountModelCapabilityCache，全 no-op。
type visionDetectCapCacheStub struct {
	AccountModelCapabilityCache
}

func (s *visionDetectCapCacheStub) GetAccount(_ context.Context, _ int64) ([]*model.AccountModelCapability, bool) {
	return nil, false
}
func (s *visionDetectCapCacheStub) SetAccount(_ context.Context, _ int64, _ []*model.AccountModelCapability) error {
	return nil
}
func (s *visionDetectCapCacheStub) InvalidateAccount(_ context.Context, _ int64) error { return nil }
func (s *visionDetectCapCacheStub) NotifyUpdate(_ context.Context, _ int64, _ bool) error {
	return nil
}
func (s *visionDetectCapCacheStub) SubscribeUpdates(_ context.Context, _ func(int64, bool)) {}

// visionDetectHTTPDoerStub 模拟上游 HTTP 发送。
type visionDetectHTTPDoerStub struct {
	statusCode int
	body       string
	err        error // 非 nil 表示传输层/超时错误
	called     int   // 记录 Do 被调用次数，用于断言"doer 未触达"。
}

func (f *visionDetectHTTPDoerStub) Do(_ *http.Request) (*http.Response, error) {
	f.called++
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: f.statusCode,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

// errReader 在 Read 时始终报错，用于模拟响应体读取失败（P2-D）。
type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, errors.New("simulated read failure") }

// visionDetectBodyReadFailDoer 返回 200 但响应体读取必然失败。
type visionDetectBodyReadFailDoer struct {
	called int
}

func (d *visionDetectBodyReadFailDoer) Do(_ *http.Request) (*http.Response, error) {
	d.called++
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(errReader{}),
		Header:     make(http.Header),
	}, nil
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// captureSlog 安装一个捕获 vision_detect_result 日志的 handler，返回读取缓冲的回调。
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })
	return buf
}

func lastDetectLogEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.NotEmpty(t, lines, "应有日志输出")
	var entry map[string]any
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		if msg, _ := raw["msg"].(string); msg == "vision_detect_result" {
			entry = raw
		}
	}
	require.NotNil(t, entry, "应存在 vision_detect_result 日志")
	return entry
}

// newVisionDetectTestService 构造检测服务（固定验证码 + 桩）。
func newVisionDetectTestService(t *testing.T, code string, doer *visionDetectHTTPDoerStub, usageRepo *visionDetectUsageLogRepoStub) (*VisionDetectService, *visionDetectCapRepoStub) {
	t.Helper()
	visionDetectCaptchaCodeGenerator = func() (string, error) { return code, nil }
	t.Cleanup(func() { visionDetectCaptchaCodeGenerator = randomCaptchaCode })

	account := &Account{
		ID:          1,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test-secret", "base_url": "https://api.deepseek.com"},
	}
	accRepo := &visionDetectAccountRepoStub{account: account}
	capRepo := &visionDetectCapRepoStub{}
	capSvc := NewAccountModelCapabilityService(capRepo, &visionDetectCapCacheStub{})
	svc := NewVisionDetectService(accRepo, capSvc)
	svc.SetHTTPDoer(doer)
	if usageRepo != nil {
		svc.SetUsageLogRepository(usageRepo)
	}
	return svc, capRepo
}

const (
	visionTestAccountID = int64(1)
	visionTestModel     = "deepseek-chat-vision"
	visionTestProtocol  = model.CapabilityProtocolChatCompletions
)

// ---------------------------------------------------------------------------
// P2-4 精确匹配
// ---------------------------------------------------------------------------

func TestVisionDetectExactMatchSupported(t *testing.T) {
	buf := captureSlog(t)
	usageRepo := &visionDetectUsageLogRepoStub{}
	// 验证码 681170，响应含解释文本但数字恰等 → supported。
	doer := &visionDetectHTTPDoerStub{statusCode: 200, body: `{"choices":[{"message":{"content":"The verification code is 681170"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`}
	svc, _ := newVisionDetectTestService(t, "681170", doer, usageRepo)

	res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
	require.NoError(t, err)
	require.Equal(t, VisionDetectSupported, res.Result)

	// 能力落库
	require.Equal(t, 1, usageRepo.calls, "supported 应记平台 usage")
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, int64(0), usageRepo.lastLog.UserID)
	require.Equal(t, int64(0), usageRepo.lastLog.APIKeyID)
	require.Equal(t, visionTestAccountID, usageRepo.lastLog.AccountID)
	require.Equal(t, visionTestModel, usageRepo.lastLog.Model)
	require.NotNil(t, usageRepo.lastLog.UpstreamModel)
	require.Equal(t, visionTestModel, *usageRepo.lastLog.UpstreamModel)
	require.Equal(t, 0.0, usageRepo.lastLog.TotalCost)
	require.Equal(t, 0.0, usageRepo.lastLog.ActualCost)
	require.Equal(t, 10, usageRepo.lastLog.InputTokens)
	require.Equal(t, 5, usageRepo.lastLog.OutputTokens)

	// 日志断言
	entry := lastDetectLogEntry(t, buf)
	require.Equal(t, "supported", entry["result"])
	require.Contains(t, entry, "account_id")
	require.Contains(t, entry, "upstream_model")
	require.Contains(t, entry, "protocol")
	require.Contains(t, entry, "duration_ms")
	require.NotContains(t, entry, "captcha_code", "严禁记录验证码真值")
	require.NotContains(t, buf.String(), "sk-test-secret", "严禁记录 API key")
	require.NotContains(t, buf.String(), "681170", "严禁记录验证码真值")
}

func TestVisionDetectPrefixSuffixDigitsUnsupported(t *testing.T) {
	cases := []string{
		"681170123", // 后附加数字
		"0681170",   // 前附加数字
		"681170 999", // 多段数字
	}
	for _, body := range cases {
		buf := captureSlog(t)
		usageRepo := &visionDetectUsageLogRepoStub{}
		doer := &visionDetectHTTPDoerStub{statusCode: 200, body: `{"choices":[{"message":{"content":"` + body + `"}}]}`}
		svc, _ := newVisionDetectTestService(t, "681170", doer, usageRepo)

		res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
		require.NoError(t, err)
		require.Equal(t, VisionDetectUnsupported, res.Result, "前后附加数字应判 unsupported: %q", body)
		require.Equal(t, 1, usageRepo.calls, "unsupported 应记平台 usage")
		entry := lastDetectLogEntry(t, buf)
		require.Equal(t, "unsupported", entry["result"])
		require.NotContains(t, entry, "captcha_code")
		require.NotContains(t, buf.String(), "sk-test-secret")
	}
}

func TestVisionDetectNoOrWrongDigitUnsupported(t *testing.T) {
	cases := []string{
		"hello world",  // 无数字
		"the code is 999999", // 错数字
	}
	for _, body := range cases {
		usageRepo := &visionDetectUsageLogRepoStub{}
		doer := &visionDetectHTTPDoerStub{statusCode: 200, body: `{"choices":[{"message":{"content":"` + body + `"}}]}`}
		svc, _ := newVisionDetectTestService(t, "681170", doer, usageRepo)

		res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
		require.NoError(t, err)
		require.Equal(t, VisionDetectUnsupported, res.Result, "无/错数字应判 unsupported: %q", body)
		require.Equal(t, 1, usageRepo.calls)
	}
}

// ---------------------------------------------------------------------------
// manual_review（P2-6 日志）
// ---------------------------------------------------------------------------

func TestVisionDetectManualReview(t *testing.T) {
	buf := captureSlog(t)
	usageRepo := &visionDetectUsageLogRepoStub{}
	// 真政策措辞（明确拒答 + 验证码关键词）→ manual_review。
	doer := &visionDetectHTTPDoerStub{statusCode: 200, body: `{"choices":[{"message":{"content":"I'm sorry, I won't help with reading captchas, it's against my policy"}}]}`}
	svc, _ := newVisionDetectTestService(t, "681170", doer, usageRepo)

	res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
	require.NoError(t, err)
	require.Equal(t, VisionDetectManualReview, res.Result)
	require.Equal(t, 1, usageRepo.calls, "manual_review 应记平台 usage")
	entry := lastDetectLogEntry(t, buf)
	require.Equal(t, "manual_review", entry["result"])
	require.NotContains(t, entry, "captcha_code")
	require.NotContains(t, buf.String(), "sk-test-secret")
}

// TestVisionDetectManualReviewClassificationMatrix 验证改动点 C 两级分类基准用例
// （方案 §3.5 权威口径）：能力性措辞不落 manual_review（isVisionRefusalResponse=false），
// 明确政策/意愿类拒答 + 图片关键词才落 manual_review（isVisionRefusalResponse=true）。
func TestVisionDetectManualReviewClassificationMatrix(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		expected bool // true = manual_review（明确政策类拒答），false = 不落 manual_review
	}{
		{
			name:     "capability_cannot_identify_not_manual_review",
			content:  "I cannot identify the digits in the image",
			expected: false,
		},
		{
			name:     "policy_refusal_captcha_manual_review",
			content:  "I'm sorry, I won't help with reading captchas, it's against my policy",
			expected: true,
		},
		{
			name:     "zh_cannot_recognize_not_manual_review",
			content:  "我无法识别图片中的数字",
			expected: false,
		},
		{
			name:     "zh_policy_refusal_manual_review",
			content:  "根据使用政策，我拒绝识别验证码",
			expected: true,
		},
		{
			name:     "empty_text_not_manual_review",
			content:  "",
			expected: false,
		},
		{
			name:     "unrelated_text_not_manual_review",
			content:  "The weather is nice today.",
			expected: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, isVisionRefusalResponse(tc.content),
				"分类不符合改动点 C 基准用例: %q", tc.content)
		})
	}
}

// ---------------------------------------------------------------------------
// P2-7 三类 usage 记录
// ---------------------------------------------------------------------------

func TestVisionDetectUsageOnHTTPFailure(t *testing.T) {
	usageRepo := &visionDetectUsageLogRepoStub{}
	doer := &visionDetectHTTPDoerStub{statusCode: 500, body: "upstream error"}
	svc, _ := newVisionDetectTestService(t, "681170", doer, usageRepo)

	res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
	require.NoError(t, err)
	require.Equal(t, VisionDetectFailed, res.Result)
	require.Equal(t, 1, usageRepo.calls, "HTTP 失败应记平台 usage")
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, int64(0), usageRepo.lastLog.UserID)
	require.Equal(t, int64(0), usageRepo.lastLog.APIKeyID)
	require.Equal(t, visionTestAccountID, usageRepo.lastLog.AccountID)
	require.Equal(t, visionTestModel, usageRepo.lastLog.Model)
	require.Equal(t, 0.0, usageRepo.lastLog.TotalCost)
}

func TestVisionDetectUsageOnTransportFailure(t *testing.T) {
	usageRepo := &visionDetectUsageLogRepoStub{}
	doer := &visionDetectHTTPDoerStub{err: errors.New("i/o timeout")} // 网络/超时
	svc, _ := newVisionDetectTestService(t, "681170", doer, usageRepo)

	res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
	require.NoError(t, err)
	require.Equal(t, VisionDetectFailed, res.Result)
	require.Equal(t, 1, usageRepo.calls, "传输/超时失败应记平台 usage")
	require.Equal(t, visionTestAccountID, usageRepo.lastLog.AccountID)
	require.Equal(t, 0.0, usageRepo.lastLog.TotalCost)
}

// ---------------------------------------------------------------------------
// P2-7 usage 记录失败如实上抛 error
// ---------------------------------------------------------------------------

func TestVisionDetectUsageRecordErrorPropagates(t *testing.T) {
	usageRepo := &visionDetectUsageLogRepoStub{err: errors.New("db down")}
	doer := &visionDetectHTTPDoerStub{statusCode: 200, body: `{"choices":[{"message":{"content":"The verification code is 681170"}}]}`}
	svc, _ := newVisionDetectTestService(t, "681170", doer, usageRepo)

	res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
	require.Error(t, err)
	require.Nil(t, res)
	require.Contains(t, err.Error(), "record vision detect usage")
	require.Equal(t, 1, usageRepo.calls)
}

// ---------------------------------------------------------------------------
// 早返回不记 usage（P2-7 不记路径）
// ---------------------------------------------------------------------------

func TestVisionDetectEarlyReturnNoUsage(t *testing.T) {
	usageRepo := &visionDetectUsageLogRepoStub{}
	// invalid account id：进入上游调用之前的早返回，不应记 usage。
	svc, _ := newVisionDetectTestService(t, "681170", &visionDetectHTTPDoerStub{statusCode: 200, body: "x"}, usageRepo)
	_, err := svc.DetectCapability(context.Background(), 0, visionTestModel, visionTestProtocol)
	require.Error(t, err)
	require.Equal(t, 0, usageRepo.calls, "早返回不应记 usage")
}

// ---------------------------------------------------------------------------
// P1-B 代理解析失败即停：绝不退回无代理直连，未记 usage，有日志，doer 未触达
// ---------------------------------------------------------------------------

func TestVisionDetectProxyParseFailureStops(t *testing.T) {
	buf := captureSlog(t)
	usageRepo := &visionDetectUsageLogRepoStub{}
	// 账号配置代理，但代理 URL 含非法字符（空格）致解析失败。
	account := &Account{
		ID:          1,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test-secret", "base_url": "https://api.deepseek.com"},
		ProxyID:     &[]int64{7}[0],
		Proxy:       &Proxy{ID: 7, Protocol: "http", Host: "bad host", Port: 8080},
	}
	accRepo := &visionDetectAccountRepoStub{account: account}
	capRepo := &visionDetectCapRepoStub{}
	capSvc := NewAccountModelCapabilityService(capRepo, &visionDetectCapCacheStub{})
	svc := NewVisionDetectService(accRepo, capSvc)
	spy := &visionDetectHTTPDoerStub{statusCode: 200, body: "should never be used"}
	svc.SetHTTPDoer(spy)
	svc.SetUsageLogRepository(usageRepo)

	res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
	require.NoError(t, err)
	require.Equal(t, VisionDetectFailed, res.Result, "代理解析失败应返回 detect_failed")
	require.Contains(t, res.Message, "proxy configuration invalid")

	// 上游 HTTP 未被调用（doer 未触达），且不记 usage（请求未发出）。
	require.Equal(t, 0, spy.called, "代理解析失败不应触达上游 doer")
	require.Equal(t, 0, usageRepo.calls, "请求未发出不应记 usage")

	entry := lastDetectLogEntry(t, buf)
	require.Equal(t, "detect_failed", entry["result"])
}

// ---------------------------------------------------------------------------
// P2-D 请求发出后体读失败：统一失败路径——先记 usage 再记日志，最后返回 detect_failed
// ---------------------------------------------------------------------------

func TestVisionDetectResponseBodyReadFailure(t *testing.T) {
	buf := captureSlog(t)
	usageRepo := &visionDetectUsageLogRepoStub{}
	doer := &visionDetectBodyReadFailDoer{}
	svc, _ := newVisionDetectTestService(t, "681170", &visionDetectHTTPDoerStub{statusCode: 200, body: "x"}, usageRepo)
	svc.SetHTTPDoer(doer)
	svc.SetUsageLogRepository(usageRepo)

	res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
	require.NoError(t, err)
	require.Equal(t, VisionDetectFailed, res.Result, "体读失败应返回 detect_failed")

	// 请求已发出（doer 触达），随后体读失败 → 应记 usage + 日志。
	require.Equal(t, 1, doer.called, "请求应已发出（doer 已触达）")
	require.Equal(t, 1, usageRepo.calls, "体读失败应记平台 usage")
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, visionTestAccountID, usageRepo.lastLog.AccountID)

	entry := lastDetectLogEntry(t, buf)
	require.Equal(t, "detect_failed", entry["result"])
}

// ---------------------------------------------------------------------------
// P1-A 代理关系缺失（ProxyID 非空但 account.Proxy == nil）→ 终止，doer 未触达，不记 usage，有日志
// ---------------------------------------------------------------------------

func TestVisionDetectProxyRelationMissingStops(t *testing.T) {
	buf := captureSlog(t)
	usageRepo := &visionDetectUsageLogRepoStub{}
	account := &Account{
		ID:          1,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test-secret", "base_url": "https://api.deepseek.com"},
		ProxyID:     &[]int64{7}[0],
		Proxy:       nil, // 关系缺失
	}
	accRepo := &visionDetectAccountRepoStub{account: account}
	capRepo := &visionDetectCapRepoStub{}
	capSvc := NewAccountModelCapabilityService(capRepo, &visionDetectCapCacheStub{})
	svc := NewVisionDetectService(accRepo, capSvc)
	spy := &visionDetectHTTPDoerStub{statusCode: 200, body: "should never be used"}
	svc.SetHTTPDoer(spy)
	svc.SetUsageLogRepository(usageRepo)

	res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
	require.NoError(t, err)
	require.Equal(t, VisionDetectFailed, res.Result, "代理关系缺失应返回 detect_failed")
	require.Contains(t, res.Message, "proxy configuration relation missing")

	// 上游 HTTP 未被调用（doer 未触达），且不记 usage（请求未发出）。
	require.Equal(t, 0, spy.called, "代理关系缺失不应触达上游 doer")
	require.Equal(t, 0, usageRepo.calls, "请求未发出不应记 usage")
	require.Equal(t, 0, capRepo.upserts, "代理关系缺失不应落库能力标记")

	entry := lastDetectLogEntry(t, buf)
	require.Equal(t, "detect_failed", entry["result"])
	require.NotContains(t, buf.String(), "sk-test-secret", "严禁记录 API key")
}

// ---------------------------------------------------------------------------
// P2-C 200 畸形响应（畸形 JSON / 显式 error payload / 缺失 message）→ detect_failed，
// 记 usage + 日志，不写能力标记；结构有效但无码仍 unsupported（零回归）
// ---------------------------------------------------------------------------

func TestVisionDetectMalformed200Responses(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed_json", `not-json-at-all-{`},
		{"error_payload", `{"error":{"message":"model overloaded"}}`},
		{"missing_message", `{"choices":[{"message":{"content":""}}]}`},
		{"no_choices", `{"id":"x"}`},
	}
	for _, tc := range cases {
		buf := captureSlog(t)
		usageRepo := &visionDetectUsageLogRepoStub{}
		doer := &visionDetectHTTPDoerStub{statusCode: 200, body: tc.body}
		svc, capRepo := newVisionDetectTestService(t, "681170", doer, usageRepo)

		res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
		require.NoError(t, err, tc.name)
		require.Equal(t, VisionDetectFailed, res.Result, "200 畸形响应应判 detect_failed: %s", tc.name)
		// 请求已发出 → 记 usage + 日志，但不写能力标记。
		require.Equal(t, 1, doer.called, "200 畸形响应请求应已发出: %s", tc.name)
		require.Equal(t, 1, usageRepo.calls, "200 畸形响应应记平台 usage: %s", tc.name)
		require.Equal(t, 0, capRepo.upserts, "200 畸形响应不应落库能力标记: %s", tc.name)

		entry := lastDetectLogEntry(t, buf)
		require.Equal(t, "detect_failed", entry["result"], tc.name)
		require.NotContains(t, entry, "captcha_code", tc.name)
		require.NotContains(t, buf.String(), "sk-test-secret", tc.name)
	}
}

func TestVisionDetectValidNoCodeStillUnsupported(t *testing.T) {
	// ③ 结构有效但无码 → 仍 unsupported（零回归）。
	usageRepo := &visionDetectUsageLogRepoStub{}
	doer := &visionDetectHTTPDoerStub{statusCode: 200, body: `{"choices":[{"message":{"content":"I see a red apple"}}],"usage":{"prompt_tokens":12,"completion_tokens":4}}`}
	svc, capRepo := newVisionDetectTestService(t, "681170", doer, usageRepo)

	res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
	require.NoError(t, err)
	require.Equal(t, VisionDetectUnsupported, res.Result, "结构有效但无码应判 unsupported")
	require.Equal(t, 1, usageRepo.calls, "unsupported 应记平台 usage")
	require.Equal(t, 1, capRepo.upserts, "unsupported 应落库能力标记(false)")
	require.NotNil(t, res.Capability)
	require.False(t, res.Capability.SupportsVision)
}

// ---------------------------------------------------------------------------
// P2-D 缺 key 早期终态有日志（不触达上游）
// ---------------------------------------------------------------------------

func TestVisionDetectMissingKeyHasLog(t *testing.T) {
	buf := captureSlog(t)
	account := &Account{
		ID:          1,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "", "base_url": "https://api.deepseek.com"},
	}
	accRepo := &visionDetectAccountRepoStub{account: account}
	capRepo := &visionDetectCapRepoStub{}
	capSvc := NewAccountModelCapabilityService(capRepo, &visionDetectCapCacheStub{})
	svc := NewVisionDetectService(accRepo, capSvc)
	spy := &visionDetectHTTPDoerStub{statusCode: 200, body: "should never be used"}
	svc.SetHTTPDoer(spy)

	res, err := svc.DetectCapability(context.Background(), visionTestAccountID, visionTestModel, visionTestProtocol)
	require.NoError(t, err)
	require.Equal(t, VisionDetectFailed, res.Result, "缺 key 应返回 detect_failed")
	require.Contains(t, res.Message, "no API key available")
	require.Equal(t, 0, spy.called, "缺 key 不应触达上游 doer")
	require.Equal(t, 0, capRepo.upserts, "缺 key 不应落库能力标记")

	entry := lastDetectLogEntry(t, buf)
	require.Equal(t, "detect_failed", entry["result"])
	require.NotContains(t, buf.String(), "sk-test-secret", "严禁记录 API key")
}

// TestExtractVisionDetectContentReasoningContentFallback 钉死 deepseek 推理模型
// 语义（生产实证 + d792507c9 先例）：content 为空但 reasoning_content 有文本时，
// 提取回退 reasoning_content，不再误判结构无效。
func TestExtractVisionDetectContentReasoningContentFallback(t *testing.T) {
	t.Run("empty_content_falls_back_to_reasoning_content", func(t *testing.T) {
		body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"The digits in the image are 482913."}}]}`)
		got := extractVisionDetectContent(body)
		require.Equal(t, "The digits in the image are 482913.", got)

		valid, content := validateVisionDetect200Response(body)
		require.True(t, valid)
		require.Equal(t, "The digits in the image are 482913.", content)
	})

	t.Run("nonempty_content_takes_precedence", func(t *testing.T) {
		body := []byte(`{"choices":[{"message":{"content":"482913","reasoning_content":"let me look"}}]}`)
		got := extractVisionDetectContent(body)
		require.Equal(t, "482913", got)
	})

	t.Run("both_empty_still_invalid", func(t *testing.T) {
		body := []byte(`{"choices":[{"message":{"content":"","reasoning_content":""}}]}`)
		valid, content := validateVisionDetect200Response(body)
		require.False(t, valid)
		require.Equal(t, "", content)
	})

	t.Run("explicit_error_payload_still_invalid", func(t *testing.T) {
		body := []byte(`{"error":{"message":"boom"}}`)
		valid, _ := validateVisionDetect200Response(body)
		require.False(t, valid)
	})
}
