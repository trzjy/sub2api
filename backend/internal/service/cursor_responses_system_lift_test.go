package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// codexDeveloperInput 是 codex（OpenAI Responses 客户端）的真实输入形态（2026-11-12 生产抓包）：
//
//	input[0] = additional_tools（工具声明，namespace 嵌套）
//	input[1..] = 若干 role=developer 的消息（agent 人设 + 工具使用指引，含 14KB）
//	input[last] = role=user 的消息（用户请求）
//
// 其中 developer 消息是**客户端的系统设定**，必须提到系统位（config.system），
// 不能当成对话历史。

const codexDeveloperInput = `[
  {"type":"additional_tools","role":"developer","tools":[
    {"type":"namespace","name":"functions","tools":[
      {"type":"custom","name":"exec","description":"Run JavaScript code","format":{"type":"text"}}
    ]}
  ]},
  {"type":"message","role":"developer","content":[{"type":"input_text","text":"You are Codex, an agent based on GPT-6. You and the user share one workspace. Use only the tools provided by the client."}]},
  {"type":"message","role":"user","content":[{"type":"input_text","text":"read the README"}]}
]`

func cursorResponsesPlanForTest(t *testing.T, input string) *cursorContextPlan {
	t.Helper()
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-6-astra","input":`+input+`}`), &req))
	plan, err := cursorPlanFromResponses(&req, "gpt-6-astra")
	require.NoError(t, err)
	return plan
}

// TestCursorResponsesPlan_LiftsDeveloperToSystemBit 锁住「Responses 路径把客户端系统设定
// 提到系统位」。
//
// 根因（2026-11-12 生产实证）：Responses 路径**从不**写 config.system，
// codex 的 role=developer 消息（含 14KB agent 设定）被降级成
// responses.message.developer 历史项 —— 上游 cursor 模型把它当成「对话里的一段话」
// 而不是系统指令，于是按生态惯性去试原生 shell/read/grep，被按设计拒绝后要试错
// 几轮才学会用客户端工具。
//
// Chat / Messages / Gemini 三条路径都写 config.system —— Responses 是唯一漏网的一条。
func TestCursorResponsesPlan_LiftsDeveloperToSystemBit(t *testing.T) {
	plan := cursorResponsesPlanForTest(t, codexDeveloperInput)

	require.NotNil(t, plan.configSystem, "Responses 路径必须写 config.system")
	require.Positive(t, plan.configSystem.ItemCount(),
		"config.system 必须含客户端的 developer 设定（否则上游模型收不到系统指令）")

	// 历史里**不再**有 developer 项（去重：同一段文本不能既在系统位又在历史里）。
	trace := plan.history.TraceString()
	require.NotContains(t, trace, "responses.message.developer",
		"developer 消息提到系统位后不得再进历史（否则续轮骨架比对项数不符）")

	// user 消息照旧进历史（对话内容不受影响）。
	require.Contains(t, trace, "responses.message.user",
		"user 消息仍应作为对话内容进历史")
}

// TestCursorResponsesPlan_SystemRoleAlsoLifted 锁住 role=system 的同类处理。
func TestCursorResponsesPlan_SystemRoleAlsoLifted(t *testing.T) {
	plan := cursorResponsesPlanForTest(t, `[
	  {"type":"message","role":"system","content":[{"type":"input_text","text":"SYSTEM RULES HERE"}]},
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]`)

	require.NotNil(t, plan.configSystem)
	require.Positive(t, plan.configSystem.ItemCount())
	require.NotContains(t, plan.history.TraceString(), "responses.message.system",
		"system 消息提到系统位后不得再进历史")
}

// TestCursorResponsesPlan_SkipsUndecodableSystem 锁住「读不出来就丢这一条 + 记账，
// 绝不整轮拒」——与项目既有产品口径一致（客户把 400 当成"模型不稳定"）。
func TestCursorResponsesPlan_SkipsUndecodableSystem(t *testing.T) {
	plan := cursorResponsesPlanForTest(t, `[
	  {"type":"message","role":"developer","content":"not-an-array-of-blocks"},
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]`)

	// 整轮没被拒，且 user 消息照常进历史。
	require.NotNil(t, plan.history)
	require.Contains(t, plan.history.TraceString(), "responses.message.user",
		"一条畸形 developer 消息不该让整轮任务失败")
}

// TestCursorResponsesPlan_NoDoubleCount 锁住「同一段文本只出现一次」。
//
// 提到系统位就 continue，历史里就不该再有它 —— 否则续轮时客户端回放同样的
// developer 消息，本站会再次提取到 config.system（正常），但历史项数会与
// 客户端不一致，骨架比对就会出现无谓的漂移留痕。
func TestCursorResponsesPlan_NoDoubleCount(t *testing.T) {
	plan := cursorResponsesPlanForTest(t, codexDeveloperInput)

	trace := plan.history.TraceString()
	require.Zero(t, strings.Count(trace, "responses.message.developer"),
		"developer 消息不得同时出现在历史里")
}
