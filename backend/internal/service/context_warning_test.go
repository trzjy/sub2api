package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// 每个用例独立清空水位与抑制状态，测试串行执行，不依赖真实时间流逝。
func resetContextWarnState(t *testing.T) {
	t.Helper()
	contextWarnMu.Lock()
	contextWarnCache.Flush()
	contextWarnMu.Unlock()
}

func testWarnContext() context.Context {
	return logger.IntoContext(context.Background(), zap.NewNop())
}

func TestContextWarnBands(t *testing.T) {
	cases := []struct {
		tokens int
		band   int
		x      int
		phrase string
	}{
		{649999, 0, 0, ""},
		{650000, contextWarnBand1, 65, "即将超过可靠区间"},
		{749999, contextWarnBand1, 74, "即将超过可靠区间"},
		{750000, contextWarnBand2, 75, "即将超过可靠区间"},
		{799999, contextWarnBand2, 79, "即将超过可靠区间"},
		{800000, contextWarnBand3, 80, "已经超过可靠区间"},
		{849999, contextWarnBand3, 84, "已经超过可靠区间"},
		{850000, contextWarnBand4, 85, "已经超过可靠区间"},
	}

	for i, tc := range cases {
		resetContextWarnState(t)
		got := DecideContextWarning(testWarnContext(), "sess-band", 0, tc.tokens)
		if tc.band == 0 {
			if got != (ContextWarning{}) {
				t.Fatalf("case %d tokens=%d: want zero value, got %+v", i, tc.tokens, got)
			}
			continue
		}
		if got.Band != tc.band || got.CtxTokens != tc.tokens {
			t.Fatalf("case %d tokens=%d: band/ctx=%d/%d, want %d/%d",
				i, tc.tokens, got.Band, got.CtxTokens, tc.band, tc.tokens)
		}
		wantText := "⚠️ [corealgos] 本会话上下文已达 " + strconv.Itoa(tc.x) + " 万，" +
			tc.phrase + "，可能返回异常短回复；请压缩上下文或开新会话接力。"
		if got.Text != wantText {
			t.Fatalf("case %d tokens=%d:\n got: %q\nwant: %q", i, tc.tokens, got.Text, wantText)
		}
	}
}

func TestContextWarnCrossBandOnlyHighest(t *testing.T) {
	resetContextWarnState(t)
	ctx := testWarnContext()
	const sess = "sess-cross"

	if got := DecideContextWarning(ctx, sess, 0, 650000); got.Band != contextWarnBand1 {
		t.Fatalf("first notify: want band1, got %+v", got)
	}
	if got := DecideContextWarning(ctx, sess, 0, 660000); got != (ContextWarning{}) {
		t.Fatalf("same band must not re-notify: %+v", got)
	}
	if got := DecideContextWarning(ctx, sess, 0, 800000); got.Band != contextWarnBand3 {
		t.Fatalf("cross up: want band3, got %+v", got)
	}
	if got := DecideContextWarning(ctx, sess, 0, 810000); got != (ContextWarning{}) {
		t.Fatalf("same higher band must not re-notify: %+v", got)
	}
	if got := DecideContextWarning(ctx, sess, 0, 700000); got != (ContextWarning{}) {
		t.Fatalf("lower band must not notify: %+v", got)
	}
}

func TestContextWarnDropFloor(t *testing.T) {
	resetContextWarnState(t)
	ctx := testWarnContext()
	const sess = "sess-drop"

	if got := DecideContextWarning(ctx, sess, 0, 800000); got.Band != contextWarnBand3 {
		t.Fatalf("initial: want band3, got %+v", got)
	}
	// band=0 但 >= 600000，不清水位，仍被抑制。
	if got := DecideContextWarning(ctx, sess, 0, 620000); got != (ContextWarning{}) {
		t.Fatalf("620000 should be zero value: %+v", got)
	}
	if got := DecideContextWarning(ctx, sess, 0, 800000); got != (ContextWarning{}) {
		t.Fatalf("watermark must survive 620000: %+v", got)
	}
	// < 600000 清水位，随后可重新提醒。
	if got := DecideContextWarning(ctx, sess, 0, 500000); got != (ContextWarning{}) {
		t.Fatalf("500000 should be zero value: %+v", got)
	}
	if got := DecideContextWarning(ctx, sess, 0, 800000); got.Band != contextWarnBand3 {
		t.Fatalf("after floor drop should re-notify: %+v", got)
	}
}

func TestContextWarnThrottle(t *testing.T) {
	resetContextWarnState(t)
	ctx := testWarnContext()
	const accountID int64 = 4242
	const band = contextWarnBand3

	if got := DecideContextWarning(ctx, "sess-a", accountID, 800000); got.Band != band {
		t.Fatalf("session A: want band3, got %+v", got)
	}
	// 同账号同档、不同会话，命中抑制（与水位无关）。
	if got := DecideContextWarning(ctx, "sess-b", accountID, 800000); got != (ContextWarning{}) {
		t.Fatalf("session B should be throttled: %+v", got)
	}

	// 直接删除抑制 key 模拟 10 分钟过期。
	thrKey := "thr:" + strconv.FormatInt(accountID, 10) + ":" + strconv.Itoa(band)
	contextWarnMu.Lock()
	contextWarnCache.Delete(thrKey)
	contextWarnMu.Unlock()

	if got := DecideContextWarning(ctx, "sess-b", accountID, 800000); got.Band != band {
		t.Fatalf("after throttle expiry session B should notify: %+v", got)
	}
}

func TestContextWarnZeroValue(t *testing.T) {
	resetContextWarnState(t)
	ctx := testWarnContext()

	if got := DecideContextWarning(ctx, "", 1, 800000); got != (ContextWarning{}) {
		t.Fatalf("empty session hash: %+v", got)
	}
	if got := DecideContextWarning(ctx, "sess", 1, 0); got != (ContextWarning{}) {
		t.Fatalf("zero tokens: %+v", got)
	}
	if got := DecideContextWarning(ctx, "sess", 1, -5); got != (ContextWarning{}) {
		t.Fatalf("negative tokens: %+v", got)
	}
}

func TestBuildContextWarningSSE(t *testing.T) {
	text := "⚠️ 上下文 \"已达\" 85 万\n第二行"
	out := BuildContextWarningSSE(text)

	textJSON, _ := json.Marshal(text)
	want := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + string(textJSON) + `}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"
	if out != want {
		t.Fatalf("SSE mismatch:\n got: %q\nwant: %q", out, want)
	}
	if n := strings.Count(out, "\n\n"); n != 3 {
		t.Fatalf("want 3 event terminators, got %d", n)
	}
}

func TestBuildContextWarningSSEAt(t *testing.T) {
	text := "⚠️ 上下文 \"已达\" 85 万\n第二行"
	out := BuildContextWarningSSEAt(3, text)

	textJSON, _ := json.Marshal(text)
	want := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":` + string(textJSON) + `}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":3}` + "\n\n"
	if out != want {
		t.Fatalf("SSE mismatch:\n got: %q\nwant: %q", out, want)
	}
	if n := strings.Count(out, "\n\n"); n != 3 {
		t.Fatalf("want 3 event terminators, got %d", n)
	}
	if n := strings.Count(out, `"index":3`); n != 3 {
		t.Fatalf("want 3 events with index=3, got %d", n)
	}
	// 文本经 json.Marshal 转义：换行与引号不得原样出现在 data 行里。
	if strings.Contains(out, "\n第二行") || !strings.Contains(out, `\n第二行`) {
		t.Fatalf("text not escaped correctly: %q", out)
	}
	if !strings.Contains(out, `\"已达\"`) {
		t.Fatalf("quotes not escaped correctly: %q", out)
	}
}

func TestAppendContextWarningToJSON(t *testing.T) {
	warnText := "⚠️ 提醒 \"85\" 万"
	body := []byte(`{"model":"x","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}],"stream":true}`)
	out, err := AppendContextWarningToJSON(body, warnText)
	if err != nil {
		t.Fatal(err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(obj["content"], &content); err != nil {
		t.Fatal(err)
	}
	if len(content) != 3 {
		t.Fatalf("want 3 content blocks, got %d: %s", len(content), out)
	}
	if content[0].Text != "first" || content[1].Text != "second" {
		t.Fatalf("existing blocks order changed: %+v", content)
	}
	if content[2].Type != "text" || content[2].Text != warnText {
		t.Fatalf("warning block wrong (want tail): %+v", content[2])
	}

	var model string
	if err := json.Unmarshal(obj["model"], &model); err != nil || model != "x" {
		t.Fatalf("model lost: %q err=%v body=%s", model, err, out)
	}
	var stream bool
	if err := json.Unmarshal(obj["stream"], &stream); err != nil || !stream {
		t.Fatalf("stream lost: %v err=%v body=%s", stream, err, out)
	}

	// 含 tool_use：返回可判定错误且 body 原样不变。
	toolBody := []byte(`{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"t"}]}`)
	got, err := AppendContextWarningToJSON(toolBody, warnText)
	if err == nil {
		t.Fatal("want error for tool_use content")
	}
	if !errors.Is(err, errToolUsePresent) {
		t.Fatalf("want errToolUsePresent, got %v", err)
	}
	if string(got) != string(toolBody) {
		t.Fatalf("body must be unchanged on tool_use:\n got: %s\nwant: %s", got, toolBody)
	}

	if _, err := AppendContextWarningToJSON([]byte("not-json"), "x"); err == nil {
		t.Fatal("want error for invalid json")
	}
}

func TestShiftContentBlockIndex(t *testing.T) {
	for _, typ := range []string{"content_block_start", "content_block_delta", "content_block_stop"} {
		in := `data: {"type":"` + typ + `","index":2,"delta":{"type":"text_delta","text":"hi"}}`
		out, ok := ShiftContentBlockIndex(in)
		if !ok {
			t.Fatalf("%s: want shifted=true", typ)
		}
		if !strings.HasPrefix(out, "data: ") {
			t.Fatalf("%s: lost prefix: %q", typ, out)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(out, "data: ")), &obj); err != nil {
			t.Fatalf("%s: bad json: %v", typ, err)
		}
		if obj["index"] != float64(3) {
			t.Fatalf("%s: index=%v, want 3", typ, obj["index"])
		}
		if obj["type"] != typ {
			t.Fatalf("%s: type changed: %v", typ, obj["type"])
		}
		if _, ok := obj["delta"]; !ok {
			t.Fatalf("%s: lost delta field: %v", typ, obj)
		}
	}

	unchanged := []string{
		"event: content_block_start",
		`data: {"type":"ping","index":0}`,
		"data: not-json",
		`{"type":"content_block_start","index":0}`,
	}
	for _, in := range unchanged {
		out, ok := ShiftContentBlockIndex(in)
		if ok || out != in {
			t.Fatalf("should be unchanged: in=%q out=%q ok=%v", in, out, ok)
		}
	}
}

func TestPrependContextWarningToJSON(t *testing.T) {
	warnText := "⚠️ 提醒 \"85\" 万"
	body := []byte(`{"model":"x","content":[{"type":"text","text":"old"}],"stream":true}`)
	out, err := PrependContextWarningToJSON(body, warnText)
	if err != nil {
		t.Fatal(err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(obj["content"], &content); err != nil {
		t.Fatal(err)
	}
	if len(content) != 2 {
		t.Fatalf("want 2 content blocks, got %d: %s", len(content), out)
	}
	if content[0].Type != "text" || content[0].Text != warnText {
		t.Fatalf("warning block wrong: %+v", content[0])
	}
	if content[1].Text != "old" {
		t.Fatalf("existing block shifted wrong: %+v", content[1])
	}

	var model string
	if err := json.Unmarshal(obj["model"], &model); err != nil || model != "x" {
		t.Fatalf("model lost: %q err=%v body=%s", model, err, out)
	}
	var stream bool
	if err := json.Unmarshal(obj["stream"], &stream); err != nil || !stream {
		t.Fatalf("stream lost: %v err=%v body=%s", stream, err, out)
	}

	if _, err := PrependContextWarningToJSON([]byte("not-json"), "x"); err == nil {
		t.Fatal("want error for invalid json")
	}
}

func TestContextWarnWatermarkNoExpiration(t *testing.T) {
	resetContextWarnState(t)
	ctx := testWarnContext()
	const sess = "sess-wm-forever"

	if got := DecideContextWarning(ctx, sess, 0, 800000); got.Band != contextWarnBand3 {
		t.Fatalf("initial: want band3, got %+v", got)
	}

	wmKey := "wm:" + sess
	item, ok := contextWarnCache.Items()[wmKey]
	if !ok {
		t.Fatalf("watermark key %q missing", wmKey)
	}
	// go-cache v2.1.0: Set(..., gocache.NoExpiration) 落库为 Item.Expiration==0，
	// 这正是 go-cache 表示"永不过期"的标记（item.Expired() 恒为 false）。
	if item.Expiration != 0 {
		t.Fatalf("watermark Expiration=%d, want 0 (gocache.NoExpiration)", item.Expiration)
	}
	if item.Expired() {
		t.Fatal("watermark item must never expire")
	}
	if v, _ := item.Object.(int); v != contextWarnBand3 {
		t.Fatalf("watermark value=%v, want band3", v)
	}
}

func TestShiftContentBlockIndexPreservesPayload(t *testing.T) {
	const bigInt = "9007199254740993" // 2^53+1，经 float64 会丢精度
	in := `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","n":` + bigInt + `}}`

	out, ok := ShiftContentBlockIndex(in)
	if !ok {
		t.Fatalf("want shifted=true, out=%q", out)
	}
	if !strings.HasPrefix(out, "data: ") {
		t.Fatalf("lost prefix: %q", out)
	}

	payload := strings.TrimPrefix(out, "data: ")
	var obj struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("bad json: %v, out=%q", err, out)
	}
	if obj.Index != 1 {
		t.Fatalf("index=%d, want 1; out=%q", obj.Index, out)
	}
	if !strings.Contains(out, bigInt) {
		t.Fatalf("big integer not preserved byte-for-byte:\n got: %q\nwant to contain: %q", out, bigInt)
	}
	if !strings.Contains(payload, `"n":`+bigInt) {
		t.Fatalf("n field changed: %q", payload)
	}
}
