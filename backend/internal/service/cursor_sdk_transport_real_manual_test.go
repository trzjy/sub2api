//go:build manual

package service

// 真实上游验收 harness（Chat 单协议）。
//
// 本文件仅在本地持有真实凭据时手动运行：执行机不运行真实请求，只以 -tags manual 编译。
// 真实上游使用生产默认节点（NewCursorUpstreamForwarder 不注入 nodes）。
// 运行方式：导入 CURSOR_REAL_BEARER / CURSOR_REAL_CODE / CURSOR_REAL_DEVICE /
// CURSOR_REAL_WIRE_MODEL 后执行 go test -tags manual -run TestCursorSDKTransportRealUpstreamManual。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cursorRealSSEText 将真实上游的 SSE body 按 data: 行解析，拼接
// choices[0].delta.content。续轮文本可能被拆成多个 delta，原始 body 中
// 不出现连续 nonce，因此不能直接用 strings.Contains(body, nonce) 断言。
func cursorRealSSEText(t *testing.T, body string) string {
	t.Helper()
	var sb strings.Builder
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			sb.WriteString(choice.Delta.Content)
		}
	}
	return sb.String()
}

func (s *cursorFullChainSettlement) completeEvent() (CursorSettlementEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if ev.UsageComplete {
			return ev, true
		}
	}
	return CursorSettlementEvent{}, false
}

func TestCursorSDKTransportRealUpstreamManual(t *testing.T) {
	bearer := os.Getenv("CURSOR_REAL_BEARER")
	code := os.Getenv("CURSOR_REAL_CODE")
	device := os.Getenv("CURSOR_REAL_DEVICE")
	wire := os.Getenv("CURSOR_REAL_WIRE_MODEL")
	if bearer == "" || code == "" || device == "" || wire == "" {
		t.Skip("real upstream env not set")
	}
	gin.SetMode(gin.TestMode)
	var nb [6]byte
	_, _ = rand.Read(nb[:])
	nonce := "REALOK-" + hex.EncodeToString(nb[:])
	prompt := `Call the tool echo_value exactly once with value "` + nonce + `". Wait for its result, then reply with the exact value.`

	qk := &ent.QuotaKey{ID: 7, CodeEncrypted: code, DeviceID: &device}
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{7: qk}}
	svc := newCursorTestExecutionService(t, repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo), NewCursorUpstreamForwarder())
	account := &Account{ID: 1, Platform: PlatformCursor, QuotaKeyID: qkID(7), Credentials: map[string]any{
		"access_token":  bearer,
		"model_mapping": map[string]any{"real-chat": wire},
	}}
	settle := &cursorFullChainSettlement{}
	ctx := WithCursorSessionHandover(context.Background(), &cursorTestHandover{settlement: settle.hook})
	tools := []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
		Name: "echo_value", Description: "Echo a value",
		Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`),
	}}}
	ccReq := apicompat.ChatCompletionsRequest{
		Model: "real-chat", Stream: true,
		ToolChoice: json.RawMessage(`"required"`),
		Messages:   []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(strconv.Quote(prompt))}},
		Tools:      tools,
	}
	plan, err := cursorPlanFromChat(&ccReq, "real-chat")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	res, err := svc.Execute(ctx, c, account, &CursorExecutionRequest{
		Chat: &ccReq, Model: "real-chat", Stream: true, StartTime: time.Now(),
		Protocol: cursorProtocolChat, Identity: cursorFullChainIdentity, Plan: plan, ToolChoiceMode: "required",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	body := w.Body.String()
	tcID, tcName, tcArgs := cursorTestExtractToolCallFromSSE(t, []byte(body))
	require.Equal(t, "echo_value", tcName, "delivered tool name must be the client name")
	var av struct {
		Value string `json:"value"`
	}
	require.NoError(t, json.Unmarshal([]byte(tcArgs), &av))
	require.Equal(t, nonce, av.Value)

	resumeBody := `{"model":"real-chat","stream":true,"messages":[` +
		`{"role":"user","content":` + strconv.Quote(prompt) + `},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":` + strconv.Quote(tcID) +
		`,"type":"function","function":{"name":"echo_value","arguments":` + strconv.Quote(string(tcArgs)) + `}}]},` +
		`{"role":"tool","tool_call_id":` + strconv.Quote(tcID) + `,"content":` + strconv.Quote(nonce) + `}],` +
		`"tools":[{"type":"function","function":{"name":"echo_value","description":"Echo a value","parameters":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}}}]}`
	probe, err := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(resumeBody))
	require.NoError(t, err)
	ticket, err := svc.IdentifyResume(probe)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	_, err = svc.ResumeCursorSession(context.Background(), c2, ticket, account, CursorResumeServeInput{
		Body: []byte(resumeBody), Stream: true, Identity: cursorFullChainIdentity,
	})
	require.NoError(t, err)
	finalText := cursorRealSSEText(t, w2.Body.String())
	require.Contains(t, finalText, nonce, "model must echo the real tool result")
	calls, complete := settle.snapshot()
	require.GreaterOrEqual(t, calls, 1)
	require.Equal(t, 1, complete, "exactly one real-usage settlement")

	ev, ok := settle.completeEvent()
	require.True(t, ok, "must have a complete real-usage settlement event")
	require.NotNil(t, ev.Result)
	u := ev.Result.Usage
	require.Greater(t, u.InputTokens, 0)
	require.Greater(t, u.OutputTokens, 0)
	t.Logf("EVENT usages input=%d output=%d cache_read=%d cache_write=%d", u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)

	sum := sha256.Sum256([]byte(tcArgs))
	t.Logf("EVENT tool_call name=%s args_sha256=%s", tcName, hex.EncodeToString(sum[:8]))
	t.Logf("EVENT resume nonce_echoed=%v", strings.Contains(finalText, nonce))
	t.Logf("EVENT settlement calls=%d complete=%d", calls, complete)
}
