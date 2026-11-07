package apicompat

import "testing"

// feedAll 依次喂入所有 delta 并累计双通道输出，最后调用一次 Flush 收尾，
// 返回累计的 (reasoning, text)。仅用于那些"喂完即收尾"的用例。
func feedAll(s *InlineThinkStripper, deltas ...string) (reasoning, text string) {
	for _, d := range deltas {
		r, tx := s.Feed(d)
		reasoning += r
		text += tx
	}
	fr, ft := s.Flush()
	return reasoning + fr, text + ft
}

// 用例 1：<think> 单帧完整。
func TestInlineThinkStripper_ThinkSingleFrame(t *testing.T) {
	s := &InlineThinkStripper{}
	r, tx := feedAll(s, "<think>secret</think>visible")
	if r != "secret" {
		t.Fatalf("reasoning = %q, want %q", r, "secret")
	}
	if tx != "visible" {
		t.Fatalf("text = %q, want %q", tx, "visible")
	}
}

// 用例 2：<think> 跨帧切割。
func TestInlineThinkStripper_ThinkCrossFrame(t *testing.T) {
	s := &InlineThinkStripper{}
	r, tx := feedAll(s, "<think>sec", "ret</think>visible")
	if r != "secret" {
		t.Fatalf("reasoning = %q, want %q", r, "secret")
	}
	if tx != "visible" {
		t.Fatalf("text = %q, want %q", tx, "visible")
	}
}

// 用例 3：<thinking> 单帧完整。
func TestInlineThinkStripper_ThinkingSingleFrame(t *testing.T) {
	s := &InlineThinkStripper{}
	r, tx := feedAll(s, "<thinking>deep</thinking>text")
	if r != "deep" {
		t.Fatalf("reasoning = %q, want %q", r, "deep")
	}
	if tx != "text" {
		t.Fatalf("text = %q, want %q", tx, "text")
	}
}

// 用例 4：<thinking> 跨帧切割。
func TestInlineThinkStripper_ThinkingCrossFrame(t *testing.T) {
	s := &InlineThinkStripper{}
	r, tx := feedAll(s, "<thinking>de", "ep</thinking>text")
	if r != "deep" {
		t.Fatalf("reasoning = %q, want %q", r, "deep")
	}
	if tx != "text" {
		t.Fatalf("text = %q, want %q", tx, "text")
	}
}

// 用例 5：<think 前缀扣留（共同前缀不泄漏）。
// 第一帧结尾停在 "<think" 共同前缀处：必须扣留在缓冲、绝不把标签碎片漏成正文；
// 第二帧补全后它被正确识别为开标签，整段推理被剥离。
func TestInlineThinkStripper_PrefixHoldNoLeak(t *testing.T) {
	s := &InlineThinkStripper{}
	// 第一帧：正文 + 截断的开标签前缀 "<think"。
	r1, tx1 := s.Feed("abc<think")
	if r1 != "" {
		t.Fatalf("frame1 reasoning = %q, want empty (prefix must be held)", r1)
	}
	if tx1 != "abc" {
		t.Fatalf("frame1 text = %q, want %q", tx1, "abc")
	}
	// 第二帧：补全开标签并闭合，整段推理应被剥离，正文接续。
	r2, tx2 := s.Feed(">secret</think>tail")
	if r2 != "secret" {
		t.Fatalf("frame2 reasoning = %q, want %q", r2, "secret")
	}
	if tx2 != "tail" {
		t.Fatalf("frame2 text = %q, want %q", tx2, "tail")
	}
	// 累计：标签碎片未泄漏，正文前后两段拼接正确。
	fr, ft := s.Flush()
	if fr != "" || ft != "" {
		t.Fatalf("flush = (%q,%q), want empty", fr, ft)
	}
}

// 用例 6：两种变体各自独立（两段不同变体）。
func TestInlineThinkStripper_TwoVariantsIndependent(t *testing.T) {
	s := &InlineThinkStripper{}
	r, tx := feedAll(s, "<think>a</think>mid<thinking>b</thinking>end")
	if r != "ab" {
		t.Fatalf("reasoning = %q, want %q", r, "ab")
	}
	if tx != "midend" {
		t.Fatalf("text = %q, want %q", tx, "midend")
	}
}

// 用例 7：无标签正文直接透传。
func TestInlineThinkStripper_PassthroughNoTag(t *testing.T) {
	s := &InlineThinkStripper{}
	r, tx := feedAll(s, "plain text here")
	if r != "" {
		t.Fatalf("reasoning = %q, want empty", r)
	}
	if tx != "plain text here" {
		t.Fatalf("text = %q, want %q", tx, "plain text here")
	}
}

// 用例 8：Flush 吐出未闭合推理尾段（不丢、不重复）。
// 未闭合推理段在流式过程中已随帧增量吐出（消除假死），Flush 只负责收尾守卫，
// 不再重复吐出已交付的内容；且尾段只吐一次。
func TestInlineThinkStripper_FlushUnclosedReasoningTail(t *testing.T) {
	s := &InlineThinkStripper{}
	// 未闭合：正文可见 + 进入推理段但未见到闭标签。
	r, tx := s.Feed("visible<think>unclosed")
	if r != "unclosed" {
		t.Fatalf("reasoning = %q, want %q", r, "unclosed")
	}
	if tx != "visible" {
		t.Fatalf("text = %q, want %q", tx, "visible")
	}
	// Flush 收尾：不重复吐出已交付的推理尾段。
	fr, ft := s.Flush()
	if fr != "" {
		t.Fatalf("flush reasoning = %q, want empty (no double emit)", fr)
	}
	if ft != "" {
		t.Fatalf("flush text = %q, want empty", ft)
	}
	// 只吐一次：再次 Flush 不得再产出任何内容。
	fr2, ft2 := s.Flush()
	if fr2 != "" || ft2 != "" {
		t.Fatalf("second flush = (%q,%q), want empty (only-once guard)", fr2, ft2)
	}
}
