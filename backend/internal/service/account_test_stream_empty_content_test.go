//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 账号测试面板对「上游 200 但零正文」的判定：必须报失败，并且失败原因里
// 要带「思考事件数 / stop_reason / 原始响应的 data 行」三个可定位信息。
//
// 这组断言锁的是 2026-11-15 线上实测暴露的两个坑：
//  1. 首版只把非 data 行记进「原始响应开头」，报出来是一串空的 event: 标签，
//     运维看不出是「只有思考」还是「上游回了个空壳」；
//  2. 面板若把空串增量算成正文，空壳响应会被判成测试通过。
func TestProcessClaudeStreamReportsEmptyContentWithDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)

	thinkingOnly := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"spark-x2.6","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"用户在问"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"很长的问题"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":1024}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/984/test", nil)

	s := &AccountTestService{}
	err := s.processClaudeStream(c, strings.NewReader(thinkingOnly))
	if err == nil {
		t.Fatalf("零正文流必须报错，实际返回 nil；响应体=%s", w.Body.String())
	}

	msg := err.Error()
	for _, want := range []string{"没有任何正文", "思考事件 2 个", "stop_reason=max_tokens", `"type":"thinking"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("失败原因缺少 %q：%s", want, msg)
		}
	}
	// 绝不能出现「测试通过」：那正是旧行为的症状。
	if strings.Contains(w.Body.String(), `"test_complete"`) {
		t.Fatalf("零正文流不允许回 test_complete：%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Fatalf("零正文流必须以 error 事件结尾：%s", w.Body.String())
	}
}

// 空壳增量（text_delta 但 text 为空串）不算正文：否则空壳响应会被判成通过。
func TestProcessClaudeStreamTreatsEmptyTextDeltaAsNoContent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	shellOnly := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","content":[]}}`,
		``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":""}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/984/test", nil)

	s := &AccountTestService{}
	if err := s.processClaudeStream(c, strings.NewReader(shellOnly)); err == nil {
		t.Fatalf("空壳文本块必须报错，实际返回 nil；响应体=%s", w.Body.String())
	}
}

// 有正文时必须照旧报成功 —— 闸门不能把正常测试也拦下来。
func TestProcessClaudeStreamStillSucceedsWithText(t *testing.T) {
	gin.SetMode(gin.TestMode)

	withText := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","content":[]}}`,
		``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"pong"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/984/test", nil)

	s := &AccountTestService{}
	if err := s.processClaudeStream(c, strings.NewReader(withText)); err != nil {
		t.Fatalf("有正文的流不该报错：%v；响应体=%s", err, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"test_complete"`) {
		t.Fatalf("有正文的流应以 test_complete 收尾：%s", w.Body.String())
	}
}
