package service

import "testing"

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
