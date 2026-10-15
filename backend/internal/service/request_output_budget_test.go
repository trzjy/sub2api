package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMaxRequestedOutputTokens 钉住字段识别：三种协议字段任一存在都生效，
// 取最大值（换字段名不能绕过上限），负数与非数字不参与。
func TestMaxRequestedOutputTokens(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantField string
		wantValue int64
		wantOK    bool
	}{
		{name: "chat completions max_tokens", body: `{"model":"m","max_tokens":4096}`, wantField: "max_tokens", wantValue: 4096, wantOK: true},
		{name: "chat completions max_completion_tokens", body: `{"model":"m","max_completion_tokens":8192}`, wantField: "max_completion_tokens", wantValue: 8192, wantOK: true},
		{name: "responses max_output_tokens", body: `{"model":"m","max_output_tokens":16384}`, wantField: "max_output_tokens", wantValue: 16384, wantOK: true},
		{name: "sentinel 999999999", body: `{"model":"m","max_tokens":999999999}`, wantField: "max_tokens", wantValue: 999999999, wantOK: true},
		{name: "absent", body: `{"model":"m","messages":[]}`, wantOK: false},
		{name: "zero is a real request", body: `{"model":"m","max_tokens":0}`, wantField: "max_tokens", wantValue: 0, wantOK: true},
		{name: "negative ignored", body: `{"model":"m","max_tokens":-1}`, wantOK: false},
		{name: "string ignored", body: `{"model":"m","max_tokens":"4096"}`, wantOK: false},
		{name: "takes max across fields", body: `{"model":"m","max_tokens":128000,"max_output_tokens":999999999}`, wantField: "max_output_tokens", wantValue: 999999999, wantOK: true},
		{name: "invalid json", body: `{"model":`, wantOK: false},
		{name: "empty body", body: ``, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			field, value, ok := MaxRequestedOutputTokens([]byte(tc.body))
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantField, field)
			require.Equal(t, tc.wantValue, value)
		})
	}
}

// TestCheckRequestOutputBudgetRejectsSentinel 钉住事故形态：max_tokens=999999999
// 必须在出站前被拒（400），而不是等上游 400→502。
func TestCheckRequestOutputBudgetRejectsSentinel(t *testing.T) {
	limits := RequestOutputBudgetLimitsOrDefault(0, 0)
	require.Equal(t, DefaultMaxOutputTokensCeiling, limits.MaxOutputTokens)
	require.Equal(t, DefaultModelContextWindow, limits.ContextWindow)

	// 生产事故的真实请求形态：小 prompt（103 token 量级）+ 哨兵值 max_tokens。
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":999999999}`)
	verdict := CheckRequestOutputBudget("deepseek-v4.1-flash", TierRoutingProtocolChatCompletions, body, limits)

	require.True(t, verdict.Rejected, "max_tokens=999999999 must be rejected before dispatch")
	require.Equal(t, "max_tokens", verdict.Field)
	require.Equal(t, int64(999999999), verdict.Requested)
	require.Equal(t, DefaultMaxOutputTokensCeiling, verdict.Limit)
	require.Contains(t, verdict.Reason, "131072", "reason must state the actionable limit")
	require.Contains(t, verdict.Reason, "max_tokens")
	// 对外文案不得泄露内部细节（上游名/分组/账号）。
	require.NotContains(t, verdict.Reason, "deepseek")
	require.NotContains(t, verdict.Reason, "inferaiapi")
}

// TestCheckRequestOutputBudgetSentinelViaEveryFieldName 换字段名不能绕过上限：
// 各协议字段各自被超限时都被拒。
//
// RequestOutputTokenFields 里带点号的项是 gjson 路径（Gemini 原生的
// generationConfig.maxOutputTokens 是**嵌套**字段，原生请求体顶层没有
// maxOutputTokens），因此按路径层级构造 JSON 而不是当扁平 key 写。
func TestCheckRequestOutputBudgetSentinelViaEveryFieldName(t *testing.T) {
	limits := RequestOutputBudgetLimitsOrDefault(0, 0)
	for _, field := range RequestOutputTokenFields {
		t.Run(field, func(t *testing.T) {
			var body []byte
			if idx := strings.Index(field, "."); idx >= 0 {
				parent, child := field[:idx], field[idx+1:]
				body = []byte(fmt.Sprintf(
					`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"%s":{"%s":999999999}}`,
					parent, child))
			} else {
				body = []byte(fmt.Sprintf(
					`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"%s":999999999}`, field))
			}
			verdict := CheckRequestOutputBudget("gpt-5.5", TierRoutingProtocolChatCompletions, body, limits)
			require.True(t, verdict.Rejected)
			require.Equal(t, field, verdict.Field)
		})
	}
}

// TestCheckRequestOutputBudgetNoRegressionForNormalValues 无回归：正常业务的
// max_tokens 取值（含 Claude Code CLI 真实默认 128000、aggressive 256000、
// ollama 上限 65535）全部原样放行。
func TestCheckRequestOutputBudgetNoRegressionForNormalValues(t *testing.T) {
	limits := RequestOutputBudgetLimitsOrDefault(0, 0)
	for _, value := range []int64{
		1, 16, 100, 1024, 4096, 8192, 16384, 32000,
		50000, 64000, // antigravity 既有 clamp 上限
		65535, // ollama 上游硬上限
		70000, 80000, 81920, 100000,
		128000, // Claude Code CLI 真实默认（gateway_claude_oauth_body.go:276）
		131072, // 恰好等于上限：边界内侧必须放行
	} {
		t.Run(fmt.Sprintf("max_tokens=%d", value), func(t *testing.T) {
			body := []byte(fmt.Sprintf(
				`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}],"max_tokens":%d}`, value))
			verdict := CheckRequestOutputBudget("claude-sonnet-4", TierRoutingProtocolMessages, body, limits)
			require.False(t, verdict.Rejected, "max_tokens=%d must pass unchanged", value)
			require.Empty(t, verdict.Reason)
		})
	}
}

// TestCheckRequestOutputBudgetNoMaxTokensUnchanged 未写 max_tokens 的请求行为不变。
func TestCheckRequestOutputBudgetNoMaxTokensUnchanged(t *testing.T) {
	limits := RequestOutputBudgetLimitsOrDefault(0, 0)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`)
	verdict := CheckRequestOutputBudget("gpt-5.5", TierRoutingProtocolChatCompletions, body, limits)
	require.False(t, verdict.Rejected)
	require.Empty(t, verdict.Reason)
}

// TestCheckRequestOutputBudgetContextOverflowBlockedBeforeDispatch 钉住第二道：
// max_tokens 本身合法，但 input 估算 + max_tokens 超出 context 窗口时，
// 必须在出站前被拦（上游会 400→502）。
func TestCheckRequestOutputBudgetContextOverflowBlockedBeforeDispatch(t *testing.T) {
	// 窗口收窄到 4096 以便用小请求体稳定构造「装不下」场景。
	limits := RequestOutputBudgetLimits{MaxOutputTokens: 131072, ContextWindow: 4096}

	// 约 3000 token 的输入 + 2048 max_tokens：合法 max_tokens，但总长超窗。
	prompt := strings.Repeat("token ", 3000)
	body := []byte(fmt.Sprintf(
		`{"model":"gpt-5.5","messages":[{"role":"user","content":%q}],"max_tokens":2048}`, prompt))

	verdict := CheckRequestOutputBudget("gpt-5.5", TierRoutingProtocolChatCompletions, body, limits)
	require.True(t, verdict.Rejected, "input + max_tokens over context window must be blocked before dispatch")
	require.Equal(t, int64(4096), verdict.Limit)
	require.Contains(t, verdict.Reason, "context limit")

	// 同一请求体在宽窗口下必须放行（证明拦截来自窗口判定而非其他副作用）。
	wide := RequestOutputBudgetLimits{MaxOutputTokens: 131072, ContextWindow: 1000000}
	require.False(t, CheckRequestOutputBudget("gpt-5.5", TierRoutingProtocolChatCompletions, body, wide).Rejected)
}

// TestCheckRequestOutputBudgetContextCheckCoversAllProtocols 三种协议形态的
// context 预检都生效（估算器按协议收敛，护栏不能只覆盖其中一种）。
func TestCheckRequestOutputBudgetContextCheckCoversAllProtocols(t *testing.T) {
	limits := RequestOutputBudgetLimits{MaxOutputTokens: 131072, ContextWindow: 4096}
	prompt := strings.Repeat("token ", 3000)
	for _, tc := range []struct {
		name     string
		protocol TierRoutingRequestProtocol
		body     string
	}{
		{
			name:     "chat_completions",
			protocol: TierRoutingProtocolChatCompletions,
			body:     fmt.Sprintf(`{"model":"gpt-5.5","messages":[{"role":"user","content":%q}],"max_tokens":2048}`, prompt),
		},
		{
			name:     "messages",
			protocol: TierRoutingProtocolMessages,
			body:     fmt.Sprintf(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":%q}],"max_tokens":2048}`, prompt),
		},
		{
			name:     "responses",
			protocol: TierRoutingProtocolResponses,
			body:     fmt.Sprintf(`{"model":"gpt-5.5","input":%q,"max_output_tokens":2048}`, prompt),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := "gpt-5.5"
			if tc.protocol == TierRoutingProtocolMessages {
				model = "claude-sonnet-4"
			}
			verdict := CheckRequestOutputBudget(model, tc.protocol, []byte(tc.body), limits)
			require.True(t, verdict.Rejected, "%s protocol must enforce the context window", tc.name)
		})
	}
}

// TestCheckRequestOutputBudgetByteUpperBoundShortCircuit 小请求体不触发 tokenize：
// len(body) 是 input token 上界，上界装得下就必须放行（性能护栏的语义钉子）。
func TestCheckRequestOutputBudgetByteUpperBoundShortCircuit(t *testing.T) {
	limits := RequestOutputBudgetLimits{MaxOutputTokens: 131072, ContextWindow: 1000000}
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"max_tokens":4096}`)
	require.Less(t, int64(len(body))+int64(4096), limits.ContextWindow)

	verdict := CheckRequestOutputBudget("gpt-5.5", TierRoutingProtocolChatCompletions, body, limits)
	require.False(t, verdict.Rejected)
}

// TestCheckRequestOutputBudgetFailsOpenOnEstimateError 估算失败不改变既有行为：
// 护栏是准入加固而非路由决策，不能因估算器解析不了就拒绝合法请求。
func TestCheckRequestOutputBudgetFailsOpenOnEstimateError(t *testing.T) {
	limits := RequestOutputBudgetLimits{MaxOutputTokens: 131072, ContextWindow: 8}
	// 极小窗口 + 无法估算的协议形态（估算器对该输入报错）=> 放行。
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"max_tokens":2048}`)
	verdict := CheckRequestOutputBudget("gpt-5.5", TierRoutingProtocolChatCompletions, body, limits)

	// 该 body 实际可估算，故仍应按窗口拒绝——用真正无法解析的输入验证 fail-open。
	require.True(t, verdict.Rejected)

	unparseable := []byte(`{"messages":"not-an-array","max_tokens":2048}`)
	failOpen := CheckRequestOutputBudget("gpt-5.5", TierRoutingProtocolChatCompletions, unparseable, limits)
	require.False(t, failOpen.Rejected, "estimator failure must fail open")
}

// TestCheckRequestOutputBudgetDisabledLimits 阈值设为 0 时该项检查关闭。
func TestCheckRequestOutputBudgetDisabledLimits(t *testing.T) {
	limits := RequestOutputBudgetLimits{MaxOutputTokens: 0, ContextWindow: 0}
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"max_tokens":999999999}`)
	require.False(t, CheckRequestOutputBudget("gpt-5.5", TierRoutingProtocolChatCompletions, body, limits).Rejected)
}

// TestRequestOutputBudgetLimitsOrDefault 配置缺省回落默认值而非「不检查」。
func TestRequestOutputBudgetLimitsOrDefault(t *testing.T) {
	require.Equal(t,
		RequestOutputBudgetLimits{MaxOutputTokens: DefaultMaxOutputTokensCeiling, ContextWindow: DefaultModelContextWindow},
		RequestOutputBudgetLimitsOrDefault(0, 0))
	require.Equal(t,
		RequestOutputBudgetLimits{MaxOutputTokens: DefaultMaxOutputTokensCeiling, ContextWindow: DefaultModelContextWindow},
		RequestOutputBudgetLimitsOrDefault(-1, -1))
	require.Equal(t,
		RequestOutputBudgetLimits{MaxOutputTokens: 4096, ContextWindow: 8192},
		RequestOutputBudgetLimitsOrDefault(4096, 8192))
}

// TestDefaultMaxOutputTokensCeilingAccommodatesRealClients 上限取值依据的回归钉子：
// 必须容得下 Claude Code CLI 的真实默认 128000（gateway_claude_oauth_body.go:276），
// 且必须远低于事故哨兵值 999999999。
func TestDefaultMaxOutputTokensCeilingAccommodatesRealClients(t *testing.T) {
	const claudeCodeRealDefault = int64(128000)
	require.GreaterOrEqual(t, DefaultMaxOutputTokensCeiling, claudeCodeRealDefault,
		"ceiling must accommodate the real Claude Code CLI default max_tokens")
	require.Less(t, DefaultMaxOutputTokensCeiling, int64(999999999),
		"ceiling must reject the production sentinel value")
	require.Equal(t, int64(1000000), DefaultModelContextWindow,
		"context window must match configuredCodexDeepSeekV4Context used by the incident model")
}
