package service

// DSG-G1：伪装流式净化覆盖守卫缓冲 + 剥离后重编号 的流式回归测试。
//
// 仅验证 handleNativeAnthropicStreamingResponse 这一条字节级透传路径：
//   - 缓冲期（repGuardHold → repGuardRelease）净化；
//   - EOF 期（缓冲未达阈值 → flushGuardBuffer）净化；
//   - 剥离 thinking 后 content block 从 0 连续重编号；
//   - 非伪装账号逐字节透传不变。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// disguiseRewriteStripAccount 造一个同时开启 L1 模型回写与 L3 思考剥离的账号。
func disguiseRewriteStripAccount() *Account {
	return &Account{
		ID:       1,
		Platform: PlatformZhipu,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			IdentityDisguiseExtraKey: map[string]any{
				"profile":         "deepseek",
				"rewrite_model":   true,
				"strip_reasoning": true,
			},
		},
	}
}

// runNativeDisguiseStream 跑一次原生 Anthropic 流式转发，返回客户端收到的完整
// SSE 文本、结果与错误。upstreamModel 同时充当 originalModel（客户端请求名）。
func runNativeDisguiseStream(t *testing.T, account *Account, stream, clientModel string) (string, *OpenAIForwardResult, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize,
			},
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(stream)),
		Header:     http.Header{},
	}
	res, err := svc.handleNativeAnthropicStreamingResponse(
		context.Background(), resp, c, account, clientModel, clientModel, clientModel, nil, time.Now())
	_ = resp.Body.Close()
	return rec.Body.String(), res, err
}

// thinkingThenTextStream 构造一段含 message_start（真实上游模型名）+ thinking 块 +
// text 块的 SSE 流；thinking 用 index 0、text 用 index 1。text 内容可指定（用于
// 触发守卫 release 的不同增量）。带 message_stop 终态，保证流正常结束无错误帧。
func thinkingThenTextStream(textDeltas ...string) string {
	parts := []string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"spark-x2.6-4b","usage":{"input_tokens":10,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"我是词元星火"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"x"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		"",
	}
	for _, d := range textDeltas {
		parts = append(parts, "event: content_block_delta")
		parts = append(parts, `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"`+d+`"}}`)
		parts = append(parts, "")
	}
	parts = append(parts,
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	)
	return strings.Join(parts, "\n")
}

// 缓冲期净化：设置极小阈值，3 个不同文本增量触发 repGuardRelease；缓冲释放时
// 客户端应收到回写后的模型名、且 thinking 块已被剥离。
func TestNativeStreamDisguise_BufferReleasePurified(t *testing.T) {
	t.Setenv("REPETITION_LOOP_THRESHOLD", "3")

	out, res, err := runNativeDisguiseStream(t, disguiseRewriteStripAccount(),
		thinkingThenTextStream("A", "B", "C"), "deepseek-flash")
	require.NoError(t, err)
	require.NotNil(t, res)

	require.NotContains(t, out, "spark-x2.6-4b", "缓冲释放路径必须回写模型名")
	require.Contains(t, out, "deepseek-flash", "客户端应只看到请求模型名")
	require.NotContains(t, out, "我是词元星火", "thinking 块必须被剥离")
	require.NotContains(t, out, "thinking_delta", "thinking 增量必须被剥离")
	require.NotContains(t, out, `"index":1`, "剥离后首个可见块必须重编号为 0")
	require.Contains(t, out, `"index":0`, "剥离后文本块 index 应为 0")
	require.Contains(t, out, "A", "文本增量应透传")
	require.Contains(t, out, "C", "release 当行也应透传并净化")
}

// EOF flush 净化：阈值设很高，短流缓冲未达阈值，EOF 时 flushGuardBuffer 写出
// 的必须是净化后的行（模型名回写 + thinking 剥离 + 重编号）。
func TestNativeStreamDisguise_EOFFlushPurified(t *testing.T) {
	t.Setenv("REPETITION_LOOP_THRESHOLD", "1000")

	out, res, err := runNativeDisguiseStream(t, disguiseRewriteStripAccount(),
		thinkingThenTextStream("Hello"), "deepseek-flash")
	require.NoError(t, err)
	require.NotNil(t, res)

	require.NotContains(t, out, "spark-x2.6-4b", "EOF flush 路径必须回写模型名")
	require.Contains(t, out, "deepseek-flash")
	require.NotContains(t, out, "我是词元星火", "EOF flush 路径 thinking 必须被剥离")
	require.NotContains(t, out, `"index":1`, "EOF flush 路径剥离后必须重编号为 0")
	require.Contains(t, out, `"index":0`)
}

// 重编号：thinking index 0、text index 1 的输入，剥离后文本的
// start/delta/stop 全部为 index 0。
func TestNativeStreamDisguise_RenumberFromZero(t *testing.T) {
	t.Setenv("REPETITION_LOOP_THRESHOLD", "1000")

	out, _, err := runNativeDisguiseStream(t, disguiseRewriteStripAccount(),
		thinkingThenTextStream("Hi"), "deepseek-flash")
	require.NoError(t, err)

	// 解析每一个 content_block_* 事件的 index，断言剥离后从 0 连续且不含原 1。
	require.NotContains(t, out, `"index":1`, "原 index 1（thinking）应被移除")
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !gjson.Valid(payload) {
			continue
		}
		typ := gjson.Get(payload, "type").String()
		if typ == "content_block_start" || typ == "content_block_delta" || typ == "content_block_stop" {
			idx := gjson.Get(payload, "index").Int()
			require.Equal(t, int64(0), idx, "剥离后所有 content block 必须为 index 0，得到 %s", payload)
		}
	}
}

// 回归：非伪装账号逐字节透传不变（用既有用例模式断言原样输出）。
func TestNativeStreamDisguise_NoDisguiseByteIdentical(t *testing.T) {
	t.Setenv("REPETITION_LOOP_THRESHOLD", "1000")

	account := &Account{ID: 1, Platform: PlatformZhipu, Type: AccountTypeAPIKey}
	stream := thinkingThenTextStream("Hello")

	out, res, err := runNativeDisguiseStream(t, account, stream, "deepseek-flash")
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, stream, out, "非伪装账号必须逐字节透传")

	// 原样：上游模型名与 thinking 内容不得被改动，index 保持 0/1。
	require.Contains(t, out, "spark-x2.6-4b")
	require.Contains(t, out, "我是词元星火")
	require.Contains(t, out, `"index":1`)
}
