package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// repetitionBreakerDeltaData 构造一个携带 text_delta 的 Anthropic SSE data
// 载荷（不含 "data: " 前缀）。
func repetitionBreakerDeltaData(text string) string {
	payload, err := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	if err != nil {
		panic(err)
	}
	return string(payload)
}

// repetitionBreakerDeltaLine 构造完整 SSE data 行。
func repetitionBreakerDeltaLine(text string) string {
	return "data: " + repetitionBreakerDeltaData(text)
}

func TestRepetitionLoopDetectorTripsAtThreshold(t *testing.T) {
	d := repetitionLoopDetector{threshold: 5}
	for i := 1; i <= 4; i++ {
		if d.observe("!") {
			t.Fatalf("detector tripped at count %d, want trip only at %d", i, 5)
		}
	}
	if !d.observe("!") {
		t.Fatal("detector did not trip at threshold")
	}
}

func TestRepetitionLoopDetectorThresholdBoundaryNMinusOne(t *testing.T) {
	const n = 768
	d := repetitionLoopDetector{threshold: n}
	for i := 1; i < n; i++ {
		if d.observe("!") {
			t.Fatalf("detector tripped at count %d, want trip only at %d", i, n)
		}
	}
	if !d.observe("!") {
		t.Fatalf("detector did not trip at count %d", n)
	}
}

func TestRepetitionLoopDetectorNoFalsePositiveOnLegitRepetition(t *testing.T) {
	// 合法重复：交替 delta（缩进/换行）、重复关键词但中间有其他 token，
	// 以及恰好低于阈值的连续重复。
	d := repetitionLoopDetector{threshold: 768}
	for i := 0; i < 20000; i++ {
		for _, delta := range []string{"\n", "  ", "if", " ", "ok", "\n"} {
			if d.observe(delta) {
				t.Fatalf("detector tripped on legit alternating deltas at %d", i)
			}
		}
	}

	d2 := repetitionLoopDetector{threshold: 768}
	for i := 0; i < 767; i++ {
		if d2.observe("ok") {
			t.Fatal("detector tripped below threshold")
		}
	}
	if d2.observe("done") {
		t.Fatal("detector tripped after delta changed")
	}
}

func TestRepetitionLoopThresholdConfig(t *testing.T) {
	t.Setenv(repetitionLoopThresholdEnv, "")
	if got := repetitionLoopThreshold(); got != defaultRepetitionLoopThreshold {
		t.Fatalf("empty env: got %d, want %d", got, defaultRepetitionLoopThreshold)
	}
	t.Setenv(repetitionLoopThresholdEnv, "abc")
	if got := repetitionLoopThreshold(); got != defaultRepetitionLoopThreshold {
		t.Fatalf("invalid env: got %d, want %d", got, defaultRepetitionLoopThreshold)
	}
	t.Setenv(repetitionLoopThresholdEnv, "-3")
	if got := repetitionLoopThreshold(); got != defaultRepetitionLoopThreshold {
		t.Fatalf("negative env: got %d, want %d", got, defaultRepetitionLoopThreshold)
	}
	t.Setenv(repetitionLoopThresholdEnv, "100")
	if got := repetitionLoopThreshold(); got != 100 {
		t.Fatalf("custom env: got %d, want 100", got)
	}
	t.Setenv(repetitionLoopThresholdEnv, "0")
	if got := newStreamRepetitionGuard(); got != nil {
		t.Fatal("threshold 0 must disable the guard (nil)")
	}
}

func TestStreamRepetitionGuardBufferingFailover(t *testing.T) {
	t.Setenv(repetitionLoopThresholdEnv, "8")
	g := newStreamRepetitionGuard()
	if g == nil {
		t.Fatal("guard should be enabled")
	}
	line := repetitionBreakerDeltaLine("!")
	for i := 1; i < 8; i++ {
		if act := g.onLine(line); act != repGuardHold {
			t.Fatalf("delta %d: got action %d, want repGuardHold", i, act)
		}
	}
	if act := g.onLine(line); act != repGuardFailover {
		t.Fatalf("got action %d, want repGuardFailover", act)
	}
	if drained := g.drain(); len(drained) != 0 {
		t.Fatalf("buffer must be discarded on failover, got %d lines", len(drained))
	}
}

func TestStreamRepetitionGuardBufferFlushToPassthrough(t *testing.T) {
	t.Setenv(repetitionLoopThresholdEnv, "8")
	g := newStreamRepetitionGuard()
	for i := 1; i <= 7; i++ {
		line := repetitionBreakerDeltaLine(strings.Repeat("x", i)) // 各不相同
		if act := g.onLine(line); act != repGuardHold {
			t.Fatalf("delta %d: got action %d, want repGuardHold", i, act)
		}
	}
	// 第 8 个不同 delta：缓冲满转直通，先 flush 前 7 行再写当前行。
	if act := g.onLine(repetitionBreakerDeltaLine("final")); act != repGuardRelease {
		t.Fatalf("got action %d, want repGuardRelease", act)
	}
	drained := g.drain()
	if len(drained) != 7 {
		t.Fatalf("drain returned %d lines, want 7", len(drained))
	}
	if !strings.Contains(drained[0], `"text":"x"`) {
		t.Fatalf("drained line 0 mismatched: %s", drained[0])
	}
	// 直通期后续行直接放行。
	if act := g.onLine(repetitionBreakerDeltaLine("next")); act != repGuardWrite {
		t.Fatalf("passthrough line: got action %d, want repGuardWrite", act)
	}
}

func TestStreamRepetitionGuardPassthroughAbort(t *testing.T) {
	t.Setenv(repetitionLoopThresholdEnv, "5")
	g := newStreamRepetitionGuard()
	// 缓冲期 5 个不同 delta → release。
	for i := 1; i < 5; i++ {
		if act := g.onLine(repetitionBreakerDeltaLine(strings.Repeat("a", i))); act != repGuardHold {
			t.Fatalf("buffering delta %d: got action %d, want repGuardHold", i, act)
		}
	}
	if act := g.onLine(repetitionBreakerDeltaLine("b")); act != repGuardRelease {
		t.Fatalf("got action %d, want repGuardRelease", act)
	}
	// 直通期：合法行放行，随后同一 token 连续 5 次 → abort。
	if act := g.onLine(repetitionBreakerDeltaLine("c")); act != repGuardWrite {
		t.Fatalf("passthrough line: got action %d, want repGuardWrite", act)
	}
	for i := 1; i < 5; i++ {
		if act := g.onLine(repetitionBreakerDeltaLine("!")); act != repGuardWrite {
			t.Fatalf("passthrough repetition %d: got action %d, want repGuardWrite", i, act)
		}
	}
	if act := g.onLine(repetitionBreakerDeltaLine("!")); act != repGuardAbort {
		t.Fatalf("got action %d, want repGuardAbort", act)
	}
}

func TestStreamRepetitionGuardIgnoresNonDeltaLines(t *testing.T) {
	t.Setenv(repetitionLoopThresholdEnv, "4")
	g := newStreamRepetitionGuard()
	// event:/空行/非 content_block_delta 的 data 行只入缓冲，不参与 delta 计数。
	nonDelta := []string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1"}}`,
		"",
	}
	for _, line := range nonDelta {
		if act := g.onLine(line); act != repGuardHold {
			t.Fatalf("non-delta line %q: got action %d, want repGuardHold", line, act)
		}
	}
	for i := 1; i <= 3; i++ {
		if act := g.onLine(repetitionBreakerDeltaLine("t")); act != repGuardHold {
			t.Fatalf("delta %d: got action %d, want repGuardHold", i, act)
		}
	}
	if act := g.onLine(repetitionBreakerDeltaLine("u")); act != repGuardRelease {
		t.Fatalf("got action %d, want repGuardRelease (delta count must exclude non-delta lines)", act)
	}
	if drained := g.drain(); len(drained) != 3+len(nonDelta) {
		t.Fatalf("drain returned %d lines, want %d", len(drained), 3+len(nonDelta))
	}
}

func TestStreamRepetitionGuardFailOpenOnPanic(t *testing.T) {
	t.Setenv(repetitionLoopThresholdEnv, "4")
	g := newStreamRepetitionGuard()
	orig := anthropicStreamDeltaTextFn
	anthropicStreamDeltaTextFn = func(string) string { panic("boom") }
	t.Cleanup(func() { anthropicStreamDeltaTextFn = orig })

	if act := g.onLine(repetitionBreakerDeltaLine("x")); act != repGuardWrite {
		t.Fatalf("panic line: got action %d, want repGuardWrite (fail-open)", act)
	}
	// fail-open 后保持放行。
	if act := g.onLine(repetitionBreakerDeltaLine("y")); act != repGuardWrite {
		t.Fatalf("post-panic line: got action %d, want repGuardWrite", act)
	}
}

func TestAnthropicStreamDeltaText(t *testing.T) {
	if got := anthropicStreamDeltaText(`data-not-json`); got != "" {
		t.Fatalf("non-delta data: got %q, want empty", got)
	}
	if got := anthropicStreamDeltaText(repetitionBreakerDeltaData("hi")); got != "hi" {
		t.Fatalf("text_delta: got %q, want hi", got)
	}
	thinkingLine := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`
	if got := anthropicStreamDeltaText(thinkingLine); got != "hmm" {
		t.Fatalf("thinking_delta: got %q, want hmm", got)
	}
	startLine := `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	if got := anthropicStreamDeltaText(startLine); got != "" {
		t.Fatalf("content_block_start: got %q, want empty", got)
	}
}

func TestNewRepetitionLoopFailoverError(t *testing.T) {
	foErr := newRepetitionLoopFailoverError()
	if foErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want %d", foErr.StatusCode, http.StatusBadGateway)
	}
	if !foErr.SafeToFailoverAfterWrite {
		t.Fatal("SafeToFailoverAfterWrite must be true (buffering phase emits no semantic bytes)")
	}
	if !foErr.ShouldRetryNextAccount() {
		t.Fatal("must retry next account via existing failover path")
	}
}

func TestBuildAnthropicStreamErrorCodeSSE(t *testing.T) {
	sse := buildAnthropicStreamErrorCodeSSE("api_error", repetitionLoopErrorCode, "msg")
	if !strings.HasPrefix(sse, "event: error\ndata: ") {
		t.Fatalf("missing event prefix: %s", sse)
	}
	if !strings.Contains(sse, `"code":"`+repetitionLoopErrorCode+`"`) {
		t.Fatalf("missing error code: %s", sse)
	}
}
