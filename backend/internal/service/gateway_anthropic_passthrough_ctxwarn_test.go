package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// 用例自带状态清理与 ctx 构造，避免依赖其它测试文件里的辅助函数。
func ctxWarnResetState(t *testing.T) {
	t.Helper()
	contextWarnMu.Lock()
	contextWarnCache.Flush()
	contextWarnMu.Unlock()
}

func ctxWarnTestContext() context.Context {
	return logger.IntoContext(context.Background(), zap.NewNop())
}

// ctxWarnIndexOfType 在整段 SSE 里找指定 type 的 data 行，返回其 index。
func ctxWarnIndexOfType(t *testing.T, block, typ string) (float64, bool) {
	t.Helper()
	for _, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		if got, _ := obj["type"].(string); got != typ {
			continue
		}
		idx, ok := obj["index"].(float64)
		return idx, ok
	}
	t.Fatalf("no data line for type %q in:\n%s", typ, block)
	return 0, false
}

func TestContextWarnStreamStateObserveUsageAndTakeInjection(t *testing.T) {
	ctxWarnResetState(t)
	st := newContextWarnStreamState("sess-ctxwarn-hit", 0)

	// 三类 token 求和命中 800000（band3）。
	st.observeUsage(ctxWarnTestContext(), &ClaudeUsage{
		InputTokens:              500000,
		CacheReadInputTokens:     200000,
		CacheCreationInputTokens: 100000,
	})

	wantText := "⚠️ [corealgos] 本会话上下文已达 80 万，已经超过可靠区间，可能返回异常短回复；请压缩上下文或开新会话接力。"
	wantBlock := BuildContextWarningSSE(wantText)
	if st.block != wantBlock {
		t.Fatalf("block mismatch:\n got: %q\nwant: %q", st.block, wantBlock)
	}
	if first := st.takeInjection(); first != wantBlock {
		t.Fatalf("first takeInjection:\n got: %q\nwant: %q", first, wantBlock)
	}
	if second := st.takeInjection(); second != "" {
		t.Fatalf("second takeInjection must be empty, got %q", second)
	}
}

func TestContextWarnStreamStateBelowThreshold(t *testing.T) {
	ctxWarnResetState(t)
	st := newContextWarnStreamState("sess-ctxwarn-low", 0)
	st.observeUsage(ctxWarnTestContext(), &ClaudeUsage{InputTokens: 649999})

	if st.block != "" {
		t.Fatalf("below threshold must not produce block: %q", st.block)
	}
	if got := st.takeInjection(); got != "" {
		t.Fatalf("below threshold takeInjection must be empty: %q", got)
	}
	if st.needShift() {
		t.Fatal("below threshold needShift must be false")
	}
}

func TestContextWarnStreamStateDecidedOnce(t *testing.T) {
	ctxWarnResetState(t)
	ctx := ctxWarnTestContext()
	st := newContextWarnStreamState("sess-ctxwarn-once", 0)

	st.observeUsage(ctx, &ClaudeUsage{InputTokens: 800000})
	band3Block := st.block
	if band3Block == "" {
		t.Fatal("first observeUsage must produce block")
	}
	// 第二次喂更高水位（band4）：若重新判定，block 会变成 85 万版本。
	st.observeUsage(ctx, &ClaudeUsage{InputTokens: 850000})
	if st.block != band3Block {
		t.Fatalf("observeUsage must decide only once, block changed:\n got: %q\nwant: %q", st.block, band3Block)
	}
}

func TestContextWarnStreamStateNeedShift(t *testing.T) {
	ctxWarnResetState(t)
	st := newContextWarnStreamState("sess-ctxwarn-shift", 0)

	if st.needShift() {
		t.Fatal("initial needShift must be false")
	}
	st.observeUsage(ctxWarnTestContext(), &ClaudeUsage{InputTokens: 800000})
	if st.needShift() {
		t.Fatal("needShift must stay false before the injection is written")
	}
	if got := st.takeInjection(); got == "" {
		t.Fatal("expected injection block after hit")
	}
	if !st.needShift() {
		t.Fatal("needShift must be true after injection")
	}
}

func TestContextWarnStreamStateShiftEventBlock(t *testing.T) {
	st := newContextWarnStreamState("s", 0)
	block := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":800000}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","index":9,"delta":{"stop_reason":"end_turn"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	out := st.shiftEventBlock(block)

	inLines := strings.Split(block, "\n")
	outLines := strings.Split(out, "\n")
	if len(inLines) != len(outLines) {
		t.Fatalf("line count changed: %d -> %d", len(inLines), len(outLines))
	}
	for i := range inLines {
		if inLines[i] == "" && outLines[i] != "" {
			t.Fatalf("line %d: empty line changed to %q", i, outLines[i])
		}
		if strings.HasPrefix(inLines[i], "event:") && outLines[i] != inLines[i] {
			t.Fatalf("line %d: event line changed %q -> %q", i, inLines[i], outLines[i])
		}
	}

	for _, typ := range []string{"content_block_start", "content_block_delta", "content_block_stop"} {
		idx, ok := ctxWarnIndexOfType(t, out, typ)
		if !ok || idx != 1 {
			t.Fatalf("%s index = %v (present=%v), want 1", typ, idx, ok)
		}
	}
	if idx, ok := ctxWarnIndexOfType(t, out, "message_delta"); !ok || idx != 9 {
		t.Fatalf("message_delta index = %v (present=%v), want 9", idx, ok)
	}
	if !strings.Contains(out, `data: {"type":"message_stop"}`) {
		t.Fatalf("message_stop line changed:\n%s", out)
	}
	if !strings.Contains(out, `data: {"type":"message_start","message":{"usage":{"input_tokens":800000}}}`) {
		t.Fatalf("message_start line changed:\n%s", out)
	}
}
