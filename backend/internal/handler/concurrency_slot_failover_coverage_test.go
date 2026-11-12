package handler

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSlotFailureEntryPointsAlsoHandleExhausted 锁定「接入槽位换号的入口，
// 必须同时处理候选耗尽终态」。
//
// 背景（第二轮外审 must_fix）：第一轮整改只改了 OpenAI 链的候选耗尽出口，
// 漏了 Anthropic 链（gateway_handler.go / gateway_handler_responses.go /
// gateway_handler_chat_completions.go）、SystemOne、Gemini v1beta、web_search、
// grok_audio、openai_images 等入口 —— 槽位失败导致候选耗尽时，那些入口仍会落 502，
// 破坏「账号维度并发终态恒为 503」的契约。
//
// 本测试是源码级锁定：凡是调用 recordConcurrencySlotFailure（= 接入了槽位换号）的
// 文件，必须同时出现「slotFullCount > 0」的耗尽终态分支。
// 将来新增入口只写换号、漏写耗尽终态，会在这里被拦住。
//
// 已知例外（登记在案，不走本断言）：
//   - OpenAI WebSocket 链（openai_gateway_handler.go 的 WS 分支）：终态表达是
//     **WebSocket 关闭帧 + 关闭码**，不是 HTTP 状态码，没有可承载 503 的位置；
//     其「选号无候选」出口写 closeOpenAIClientWS(TryAgainLater)。该入口的槽位换号
//     终态表达需单独设计，已登记为本轮未修边界（见外审 packet §〇）。
func TestSlotFailureEntryPointsAlsoHandleExhausted(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, readErr := os.ReadFile(name)
		require.NoError(t, readErr, "读取 %s 失败", name)
		src := string(raw)
		if !strings.Contains(src, "recordConcurrencySlotFailure(") ||
			strings.Contains(src, "func recordConcurrencySlotFailure(") {
			// 跳过：本文件是唯一事实源本身（只有定义，没有调用点）。
			continue
		}
		checked++
		require.Contains(t, src, "slotFullCount > 0",
			"%s 接入了槽位换号(recordConcurrencySlotFailure)却没写候选耗尽终态分支"+
				"（slotFullCount > 0）——槽位失败导致候选耗尽时会落到 502，破坏账号并发终点契约",
			name)
	}
	require.Greater(t, checked, 0, "未找到任何接入槽位换号的文件，测试本身失效")
}
