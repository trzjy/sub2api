package service

import (
	"testing"

	"github.com/tidwall/gjson"
)

func mustPrepare(t *testing.T, src string, opts CodeBuddyRewriteOptions) string {
	t.Helper()
	out, err := PrepareCodeBuddyBody([]byte(src), opts)
	if err != nil {
		t.Fatalf("PrepareCodeBuddyBody error: %v", err)
	}
	return string(out)
}

func TestCodeBuddyRewrite_ForceStream(t *testing.T) {
	in := `{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":false}`
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{})
	if got := gjson.Get(out, "stream").Bool(); !got {
		t.Fatalf("expected stream=true, got %v", got)
	}
}

func TestCodeBuddyRewrite_ToolChoiceObjectToString(t *testing.T) {
	// 对象形式 → 400 code=11101，必须归一为字符串。
	in := `{"model":"x","messages":[],"tools":[{"type":"function"}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{})
	if got := gjson.Get(out, "tool_choice").String(); got != "get_weather" {
		t.Fatalf("expected tool_choice=getString, got %q", got)
	}

	// {type:"auto"} 对象 → "auto" 字符串。
	in2 := `{"model":"x","messages":[],"tool_choice":{"type":"auto"}}`
	out2 := mustPrepare(t, in2, CodeBuddyRewriteOptions{})
	if got := gjson.Get(out2, "tool_choice").String(); got != "auto" {
		t.Fatalf("expected tool_choice=auto, got %q", got)
	}

	// 已为字符串则不改写。
	in3 := `{"model":"x","messages":[],"tool_choice":"none"}`
	out3 := mustPrepare(t, in3, CodeBuddyRewriteOptions{})
	if got := gjson.Get(out3, "tool_choice").String(); got != "none" {
		t.Fatalf("expected tool_choice=none unchanged, got %q", got)
	}
}

func TestCodeBuddyRewrite_DeveloperRoleToSystem(t *testing.T) {
	in := `{"model":"x","messages":[{"role":"developer","content":"sys"}]}`
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{})
	if got := gjson.Get(out, "messages.0.role").String(); got != "system" {
		t.Fatalf("expected developer→system, got %q", got)
	}
}

func TestCodeBuddyRewrite_DeepSeekThinkingInjection(t *testing.T) {
	in := `{"model":"deepseek-reasoner","messages":[{"role":"user","content":"hi"}]}`
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{Model: "deepseek-reasoner"})
	if typ := gjson.Get(out, "thinking.type").String(); typ != "enabled" {
		t.Fatalf("expected thinking.type=enabled for deepseek, got %q", typ)
	}

	// 非 DeepSeek 模型不注入。
	in2 := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	out2 := mustPrepare(t, in2, CodeBuddyRewriteOptions{Model: "gpt-4o"})
	if gjson.Get(out2, "thinking").Exists() {
		t.Fatalf("expected no thinking for non-deepseek, got %s", out2)
	}
}

func TestCodeBuddyRewrite_ReasoningEffortDowngrade(t *testing.T) {
	// 请求 xhigh，模型仅支持 [low, high] → 降到 high。
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{SupportedEfforts: []string{"low", "high"}})
	if got := gjson.Get(out, "reasoning_effort").String(); got != "high" {
		t.Fatalf("expected reasoning_effort downgraded to high, got %q", got)
	}

	// 支持全档位时不降级。
	in2 := `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`
	out2 := mustPrepare(t, in2, CodeBuddyRewriteOptions{SupportedEfforts: []string{"off", "low", "medium", "high", "xhigh", "max"}})
	if got := gjson.Get(out2, "reasoning_effort").String(); got != "xhigh" {
		t.Fatalf("expected reasoning_effort kept xhigh, got %q", got)
	}

	// 请求 max 高于唯一支持档位 low → 降级到 low（按档位序取不超过请求的最高支持档）。
	in3 := `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`
	out3 := mustPrepare(t, in3, CodeBuddyRewriteOptions{SupportedEfforts: []string{"low"}})
	if got := gjson.Get(out3, "reasoning_effort").String(); got != "low" {
		t.Fatalf("expected reasoning_effort downgraded to low, got %q", got)
	}
}

func TestCodeBuddyRewrite_ReasoningContentBackfill(t *testing.T) {
	// 首条固定为 system（D6 规则会保证），assistant 位于 index 1。
	in := `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"assistant","content":"ok","reasoning":"some chain of thought"}]}`
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{})
	if got := gjson.Get(out, "messages.1.reasoning_content").String(); got != "some chain of thought" {
		t.Fatalf("expected reasoning_content backfilled, got %q", got)
	}

	// 已有 reasoning_content 则不覆盖。
	in2 := `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"assistant","content":"ok","reasoning":"r","reasoning_content":"existing"}]}`
	out2 := mustPrepare(t, in2, CodeBuddyRewriteOptions{})
	if got := gjson.Get(out2, "messages.1.reasoning_content").String(); got != "existing" {
		t.Fatalf("expected reasoning_content unchanged, got %q", got)
	}
}

func TestCodeBuddyRewrite_SanitizeDefaultOff(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"system","content":"You are a helpful assistant powered by Codex"}]}`

	// 默认 Sanitize=false 时不脱敏。
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{Sanitize: false})
	if gjson.Get(out, "messages.0.content").String() != "You are a helpful assistant powered by Codex" {
		t.Fatalf("expected no sanitize when disabled, got %s", out)
	}

	// 开启时脱敏：Anthropic 完整短语被删除，OpenAI 被替换为 the model vendor。
	in2 := `{"model":"m","messages":[{"role":"system","content":"As an AI assistant created by Anthropic and OpenAI"}]}`
	out2 := mustPrepare(t, in2, CodeBuddyRewriteOptions{Sanitize: true})
	if got := gjson.Get(out2, "messages.0.content").String(); got != " and the model vendor" {
		t.Fatalf("expected sanitized content, got %q", got)
	}
}

func TestCodeBuddyRewrite_SanitizeContentArray(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"I am Claude, nice to meet you"}]}]}`
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{Sanitize: true})
	if got := gjson.Get(out, "messages.0.content.0.text").String(); got != ", nice to meet you" {
		t.Fatalf("expected sanitized multi-part content, got %q", got)
	}
}

func TestCodeBuddyRewrite_CustomSanitizePatterns(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"system","content":"SECRET_MARKER_42 present"}]}`
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{
		Sanitize: true,
		SanitizePatterns: []CodeBuddySanitizePattern{
			{Substring: "SECRET_MARKER_42", Replacement: "[redacted]"},
		},
	})
	if got := gjson.Get(out, "messages.0.content").String(); got != "[redacted] present" {
		t.Fatalf("expected custom pattern applied, got %q", got)
	}
}

// TestCodeBuddyRewrite_EnsureSystemFirst 覆盖 Phase 0 D6：首条消息非 system 时前置最小
// system（intl 上游 11128：first message is not system prompt）；首条已是 system 时不动。
func TestCodeBuddyRewrite_EnsureSystemFirst(t *testing.T) {
	// 首条为 user → 前置 system，原消息顺序不变。
	in := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{})
	if got := gjson.Get(out, "messages.0.role").String(); got != "system" {
		t.Fatalf("expected injected system first, got role %q", got)
	}
	if got := gjson.Get(out, "messages.0.content").String(); got != codeBuddyDefaultSystemPrompt {
		t.Fatalf("expected default system prompt, got %q", got)
	}
	if got := gjson.Get(out, "messages.1.role").String(); got != "user" {
		t.Fatalf("expected original first message preserved at index 1, got %q", got)
	}

	// 首条已是 system → 不新增、原文返回（无改写时不重排字节）。
	in2 := `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`
	out2 := mustPrepare(t, in2, CodeBuddyRewriteOptions{})
	if got := gjson.Get(out2, "messages.#").Int(); got != 2 {
		t.Fatalf("expected message count unchanged, got %d", got)
	}
}

// TestCodeBuddyRewrite_DeveloperFirstBecomesSystemWithoutInjection 验证规则 3 先把
// developer 归一为 system，规则 3b 不再重复注入。
func TestCodeBuddyRewrite_DeveloperFirstBecomesSystemWithoutInjection(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"developer","content":"guidance"}]}`
	out := mustPrepare(t, in, CodeBuddyRewriteOptions{})
	if got := gjson.Get(out, "messages.#").Int(); got != 1 {
		t.Fatalf("expected no extra system injection after developer normalization, got %d messages", got)
	}
	if got := gjson.Get(out, "messages.0.role").String(); got != "system" {
		t.Fatalf("expected developer normalized to system, got %q", got)
	}
	if got := gjson.Get(out, "messages.0.content").String(); got != "guidance" {
		t.Fatalf("expected content preserved, got %q", got)
	}
}
