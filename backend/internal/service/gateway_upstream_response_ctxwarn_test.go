package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// 本文件用例自带状态清理与 ctx 构造，不依赖真实时间、网络或数据库。
func ctxWarnGenericResetState(t *testing.T) {
	t.Helper()
	contextWarnMu.Lock()
	contextWarnCache.Flush()
	contextWarnMu.Unlock()
}

func ctxWarnGenericContext() context.Context {
	return logger.IntoContext(context.Background(), zap.NewNop())
}

// ctxWarnGenericIndexOfType 在整段 SSE 里找指定 type 的 data 行，返回其 index。
func ctxWarnGenericIndexOfType(t *testing.T, block, typ string) (float64, bool) {
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

func TestObserveContextWarnEvent(t *testing.T) {
	ctxWarnGenericResetState(t)
	ctx := ctxWarnGenericContext()
	s := &GatewayService{}

	// message_start 携带 800000 的 usage（三类 token 求和）→ band3 命中。
	st := newContextWarnStreamState("sess-generic-observe-hit", 0)
	startData := `{"type":"message_start","message":{"usage":{"input_tokens":800000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`
	s.observeContextWarnEvent(ctx, st, startData)

	wantText := "⚠️ [corealgos] 本会话上下文已达 80 万，已经超过可靠区间，可能返回异常短回复；请压缩上下文或开新会话接力。"
	wantBlock := BuildContextWarningSSE(wantText)
	if st.block != wantBlock {
		t.Fatalf("message_start block mismatch:\n got: %q\nwant: %q", st.block, wantBlock)
	}

	// 非 message_start 不触发判定。
	other := newContextWarnStreamState("sess-generic-observe-other", 0)
	deltaData := `{"type":"message_delta","usage":{"input_tokens":800000}}`
	s.observeContextWarnEvent(ctx, other, deltaData)
	if other.block != "" || other.decided {
		t.Fatalf("non message_start must not decide, block=%q decided=%v", other.block, other.decided)
	}

	// 同一 state 二次调用不重复判定（喂更高水位，block 不应变成 85 万版本）。
	higherData := `{"type":"message_start","message":{"usage":{"input_tokens":850000}}}`
	s.observeContextWarnEvent(ctx, st, higherData)
	if st.block != wantBlock {
		t.Fatalf("second observe must not re-decide, block changed:\n got: %q\nwant: %q", st.block, wantBlock)
	}
}

func TestContextWarnGenericShiftBlock(t *testing.T) {
	st := newContextWarnStreamState("sess-generic-shift", 0)
	block := strings.Join([]string{
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
		"event: ping",
		`data: {"type":"ping"}`,
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
		idx, ok := ctxWarnGenericIndexOfType(t, out, typ)
		if !ok || idx != 1 {
			t.Fatalf("%s index = %v (present=%v), want 1", typ, idx, ok)
		}
	}
	if idx, ok := ctxWarnGenericIndexOfType(t, out, "message_delta"); !ok || idx != 9 {
		t.Fatalf("message_delta index = %v (present=%v), want 9", idx, ok)
	}
	for _, unchanged := range []string{
		`data: {"type":"message_stop"}`,
		`data: {"type":"ping"}`,
	} {
		if !strings.Contains(out, unchanged) {
			t.Fatalf("unchanged data line %q missing from:\n%s", unchanged, out)
		}
	}
}

func TestContextWarnKeepaliveIndex(t *testing.T) {
	// 未注入（无 state 或 needShift 为 false）时返回原索引。
	if got := contextWarnKeepaliveIndex(3, nil); got != 3 {
		t.Fatalf("nil state: got %d, want 3", got)
	}
	stNoInjection := newContextWarnStreamState("sess-generic-keepalive-noinj", 0)
	if got := contextWarnKeepaliveIndex(2, stNoInjection); got != 2 {
		t.Fatalf("no injection: got %d, want 2", got)
	}

	// observeUsage 命中（800000）后 takeInjection，needShift 为 true，索引 +1。
	ctxWarnGenericResetState(t)
	ctx := ctxWarnGenericContext()
	st := newContextWarnStreamState("sess-generic-keepalive-hit", 0)
	st.observeUsage(ctx, &ClaudeUsage{InputTokens: 800000})
	if inj := st.takeInjection(); inj == "" {
		t.Fatal("expected injection after 800000 usage")
	}
	if !st.needShift() {
		t.Fatal("needShift must be true after injection")
	}
	if got := contextWarnKeepaliveIndex(0, st); got != 1 {
		t.Fatalf("shifted index 0: got %d, want 1", got)
	}
	if got := contextWarnKeepaliveIndex(5, st); got != 6 {
		t.Fatalf("shifted index 5: got %d, want 6", got)
	}
}
