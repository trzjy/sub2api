package service

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// 本文件用例自带状态，不依赖真实时间、网络或数据库。
func TestContextWarnAppendStateObserve(t *testing.T) {
	st := &contextWarnAppendState{maxIndex: -1}
	if st.maxIndex != -1 {
		t.Fatalf("initial maxIndex = %d, want -1", st.maxIndex)
	}

	// content_block_start(index 0/2) 后 maxIndex 取较大值 2。
	st.observe(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	st.observe(`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`)
	if st.maxIndex != 2 {
		t.Fatalf("maxIndex = %d, want 2", st.maxIndex)
	}

	// 含 tool_use 的 content_block_start 置位 sawToolUse。
	st.observe(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"bash","input":{}}}`)
	if !st.sawToolUse {
		t.Fatal("tool_use content block must set sawToolUse = true")
	}

	// message_delta / message_stop 不改变状态。
	st.observe(`{"type":"message_delta","usage":{"input_tokens":900000}}`)
	st.observe(`{"type":"message_stop"}`)
	if st.maxIndex != 2 {
		t.Fatalf("maxIndex changed by message_delta/message_stop: got %d, want 2", st.maxIndex)
	}
	if !st.sawToolUse {
		t.Fatal("sawToolUse changed by message_delta/message_stop")
	}
}

// TestContextWarnDeltaUsage 验证 message_delta 的临时最终用量等于"当前 usage
// 合并本事件 patch 后"的值，且真实 usage 不被写回修改。
func TestContextWarnDeltaUsage(t *testing.T) {
	usage := &ClaudeUsage{
		InputTokens:              500000,
		OutputTokens:             100,
		CacheCreationInputTokens: 100000,
		CacheReadInputTokens:     50000,
		CacheCreation5mTokens:    100000,
		CacheCreation1hTokens:    0,
	}
	before := *usage
	patch := &sseUsagePatch{
		inputTokens:              700000,
		hasInputTokens:           true,
		outputTokens:             900,
		hasOutputTokens:          true,
		cacheCreationInputTokens: 120000,
		hasCacheCreationInput:    true,
		cacheReadInputTokens:     80000,
		hasCacheReadInput:        true,
	}

	tmp := contextWarnDeltaUsage(usage, patch)

	want := before
	mergeSSEUsagePatch(&want, patch)
	if tmp != want {
		t.Fatalf("temp usage = %+v, want current usage merged with patch = %+v", tmp, want)
	}
	// 关键字段确实被 patch 覆盖，而不是仍为原值。
	if tmp.InputTokens != patch.inputTokens || tmp.OutputTokens != patch.outputTokens ||
		tmp.CacheCreationInputTokens != patch.cacheCreationInputTokens ||
		tmp.CacheReadInputTokens != patch.cacheReadInputTokens {
		t.Fatalf("patch not reflected in temp usage: %+v", tmp)
	}
	// 原 usage 绝不被写回修改。
	if *usage != before {
		t.Fatalf("real usage mutated: got %+v, want %+v", *usage, before)
	}

	// patch 为 nil 时返回原 usage 的副本，同样不写回。
	tmpNil := contextWarnDeltaUsage(usage, nil)
	if tmpNil != before {
		t.Fatalf("temp usage with nil patch = %+v, want %+v", tmpNil, before)
	}
	if *usage != before {
		t.Fatalf("real usage mutated by nil-patch path: %+v", *usage)
	}
}

// sseEventForTest 是从 SSE 流中解析出的单个事件（仅保留断言所需字段）。
type sseEventForTest struct {
	eventType string
	data      string
}

// parseSSEEventsForTest 按空行切分 SSE 流为事件列表，忽略注释与纯空块。
func parseSSEEventsForTest(t *testing.T, stream string) []sseEventForTest {
	t.Helper()
	var events []sseEventForTest
	for _, raw := range strings.Split(stream, "\n\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var ev sseEventForTest
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "event:"):
				ev.eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				ev.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if ev.eventType != "" || ev.data != "" {
			events = append(events, ev)
		}
	}
	return events
}

// TestContextWarnDeltaGateOrder 验证解析后的 SSE 事件顺序恒为
// "提醒块 → 完整 message_delta 事件"，且未命中提醒时输出块原样保留。
func TestContextWarnDeltaGateOrder(t *testing.T) {
	deltaBlocks := []string{
		"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":900}}` + "\n\n",
	}
	warningText := "⚠️ [corealgos] 本会话上下文已达 80 万，即将超过可靠区间，可能返回异常短回复；请压缩上下文或开新会话接力。"

	ordered := orderedContextWarningDeltaBlocks(4, warningText, deltaBlocks)
	if len(ordered) != len(deltaBlocks)+1 {
		t.Fatalf("block count = %d, want %d", len(ordered), len(deltaBlocks)+1)
	}

	// 解析实际写出的 SSE 字节流并按事件顺序断言。
	events := parseSSEEventsForTest(t, strings.Join(ordered, ""))
	wantTypes := []string{"content_block_start", "content_block_delta", "content_block_stop", "message_delta"}
	if len(events) != len(wantTypes) {
		t.Fatalf("SSE event count = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].eventType != want {
			t.Fatalf("event[%d].event = %q, want %q", i, events[i].eventType, want)
		}
	}
	// 三个提醒事件使用 maxIndex+1 = 5 作为块索引。
	for i := 0; i < 3; i++ {
		if got := gjson.Get(events[i].data, "index").Int(); got != 5 {
			t.Fatalf("warning event[%d] index = %d, want 5", i, got)
		}
	}
	// 提醒文案落在 content_block_delta 的 text_delta 中。
	if got := gjson.Get(events[1].data, "delta.text").String(); got != warningText {
		t.Fatalf("warning text = %q, want %q", got, warningText)
	}
	// 完整 message_delta 事件原样保留：delta 与 usage 字段都在。
	last := events[3]
	if gjson.Get(last.data, "delta.stop_reason").String() != "end_turn" ||
		gjson.Get(last.data, "usage.output_tokens").Int() != 900 {
		t.Fatalf("message_delta event not preserved verbatim: %s", last.data)
	}

	// 未命中提醒时，outputBlocks 原样返回，顺序不被改变。
	unchanged := orderedContextWarningDeltaBlocks(4, "", deltaBlocks)
	if len(unchanged) != len(deltaBlocks) || unchanged[0] != deltaBlocks[0] {
		t.Fatalf("no-warning path changed output blocks: %+v", unchanged)
	}
}
