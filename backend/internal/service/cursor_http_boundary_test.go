//go:build unit

package service

// 本文件是 **§25.17.2（A01/A02/A03）同一执行/交付边界的定向测试**。
//
// 覆盖派发单"必须证明"里可在本层直接证明的部分：
//  1. 每 HTTP 一份唯一输出响应 ID（Responses 流/非流均用实际 id，可被续轮定位）；
//  2. 工具批交付检查点：封批/约束/索引冲突失败必须走错误出口，绝不发成功结束标记；
//  3. nonstream 与 stream 共用同一读循环与同一检查点——完整工具批一到就交付并结束本
//     HTTP，不再等模型 EOF（A03 补查：旧 read-all 路径会让模型等一个永不到达的结果）；
//  4. 续轮 stream → nonstream 切换使用本 HTTP 选项，AgentRun 恒 1、结算恒 1；
//  5. Gemini/Messages 落点：工具声明/配置/调用/结果真实进入转换，ID 保留；
//  6. 短写显式归为 io.ErrShortWrite。

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// runFirstHTTPMode 走真实 Execute 发首 HTTP（stream/nonstream 由参数决定）。
func runFirstHTTPMode(
	ctx context.Context, svc *CursorExecutionService, handover CursorSessionHandover,
	stream bool, toolChoiceMode string,
) (*httptest.ResponseRecorder, *ForwardResult, error) {
	var ccReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal([]byte(cursorFullChainFirstBody), &ccReq); err != nil {
		return nil, nil, err
	}
	ccReq.Stream = stream
	plan, err := cursorPlanFromChat(&ccReq, "cursor-sonnet")
	if err != nil {
		return nil, nil, err
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if handover != nil {
		ctx = WithCursorSessionHandover(ctx, handover)
	}
	result, err := svc.Execute(ctx, c, cursorFullChainAccount(), &CursorExecutionRequest{
		Chat:           &ccReq,
		Model:          "cursor-sonnet",
		Stream:         stream,
		StartTime:      time.Now(),
		Protocol:       cursorProtocolChat,
		Identity:       cursorFullChainIdentity,
		Plan:           plan,
		ToolChoiceMode: toolChoiceMode,
	})
	return w, result, err
}

// resumeSessionMode 走真实续轮入口（stream 由参数决定）。
func resumeSessionMode(
	t *testing.T, svc *CursorExecutionService, body string, stream bool,
) (*httptest.ResponseRecorder, error) {
	t.Helper()
	probe, err := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(body))
	require.NoError(t, err)
	ticket, err := svc.IdentifyResume(probe)
	require.NoError(t, err)
	require.NotNil(t, ticket, "续轮必须定位到唯一待完成批")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	_, serveErr := svc.ResumeCursorSession(context.Background(), c, ticket, cursorFullChainAccount(),
		CursorResumeServeInput{Body: []byte(body), Stream: stream, Identity: cursorFullChainIdentity})
	return w, serveErr
}

// responsesFirstHTTP 发一个 Responses 首 HTTP（真实入口 cursorExecuteResponses）。
func responsesFirstHTTP(
	t *testing.T, svc *CursorExecutionService, stream bool,
) (*httptest.ResponseRecorder, error) {
	t.Helper()
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"cursor-sonnet","stream":`+boolJSON(stream)+
		`,"input":"weather please","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`), &req))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	_, err := cursorExecuteResponses(context.Background(), c, svc, cursorFullChainAccount(), &req)
	return w, err
}

// boolJSON 返回 JSON 布尔字面量。
func boolJSON(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// pendingResponseID 读取当前待完成批登记的 Responses 响应 ID。
func pendingResponseID(t *testing.T, record *cursorSessionRecord) string {
	t.Helper()
	record.mu.Lock()
	defer record.mu.Unlock()
	require.NotNil(t, record.pending)
	return record.pending.responseID
}

// chunkIDs 取出 SSE 流里所有 chat.completion.chunk / response 对象的 id（按出现顺序去重）。
func chunkIDs(body string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			continue
		}
		candidate := ""
		if id, ok := doc["id"].(string); ok {
			candidate = id
		}
		if resp, ok := doc["response"].(map[string]any); ok {
			if id, ok := resp["id"].(string); ok {
				candidate = id
			}
		}
		if candidate == "" {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

// ===========================================================================
// 1. Responses 流式：本 HTTP 的实际 response.id 被登记且贯穿全部帧（A01）
// ===========================================================================

func TestCursorResponsesStreamUsesActualResponseID(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 101, IDField: []byte("exec-101")}}
	svc := newCursorFullChainService(t, newCursorFullChainUpstream(cursorFullChainScript{calls: calls, hangAfterPhase1: true}))

	w, err := responsesFirstHTTP(t, svc, true)
	require.NoError(t, err, "工具批交付后首 HTTP 必须正常返回（不 finalize）")
	out := w.Body.String()
	require.Contains(t, out, "get_weather", "Responses 流式必须把工具调用交付给客户端")

	record := onlySession(t, svc)
	defer svc.finishCursorSession(record, "audit_cleanup")
	registered := pendingResponseID(t, record)
	require.NotEmpty(t, registered, "已交付的 Responses response.id 必须登记，供 previous_response_id 续接")
	require.NotEqual(t, "cursor-chat", registered, "不得使用固定常量作为响应 ID")

	ids := chunkIDs(out)
	require.NotEmpty(t, ids)
	for _, id := range ids {
		require.Equal(t, registered, id, "本 HTTP 全部帧必须共用同一实际响应 ID")
	}
	require.Contains(t, out, registered, "客户端收到的 id 必须是登记的那个")
}

// ===========================================================================
// 2. Responses 非流式：同样取实际 response.id（不依赖只有 stream 才有的 respState）
// ===========================================================================

func TestCursorResponsesNonStreamUsesActualResponseID(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 101, IDField: []byte("exec-101")}}
	svc := newCursorFullChainService(t, newCursorFullChainUpstream(cursorFullChainScript{calls: calls, hangAfterPhase1: true}))

	w, err := responsesFirstHTTP(t, svc, false)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), "非流式必须是一个合法 JSON 响应: %s", w.Body.String())
	id, _ := out["id"].(string)
	require.NotEmpty(t, id)
	require.NotEqual(t, "cursor-chat", id)
	outputs, _ := out["output"].([]any)
	require.NotEmpty(t, outputs, "非流式必须交付工具输出项")

	record := onlySession(t, svc)
	defer svc.finishCursorSession(record, "audit_cleanup")
	require.Equal(t, id, pendingResponseID(t, record), "非流式 response.id 必须与实际登记一致")
}

// ===========================================================================
// 3. nonstream 首 HTTP：完整工具批一到就交付（不再等模型 EOF；A03 补查）
// ===========================================================================

func TestCursorNonStreamFirstHTTPDeliversToolBatchBeforeTerminal(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 111, IDField: []byte("exec-111")}}
	// hangAfterPhase1：上游交付批次后**永不**放终态（模型在等真实结果）。
	svc := newCursorFullChainService(t, newCursorFullChainUpstream(cursorFullChainScript{calls: calls, hangAfterPhase1: true}))

	done := make(chan struct{})
	var w *httptest.ResponseRecorder
	var err error
	go func() {
		w, _, err = runFirstHTTPMode(t.Context(), svc, &cursorTestHandover{}, false, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("nonstream 首 HTTP 必须在完整工具批处返回，不得等到模型终态/180s")
	}
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Equal(t, "tool_calls", out["choices"].([]any)[0].(map[string]any)["finish_reason"])

	record := onlySession(t, svc)
	defer svc.finishCursorSession(record, "audit_cleanup")
	record.mu.Lock()
	state := record.state
	record.mu.Unlock()
	require.Equal(t, cursorSessionWaitingTools, state, "交付批后会话必须进入 waiting_tools 并跨 HTTP 存活")
}

// ===========================================================================
// 4. tool_choice 违规：必须在错误出口结束，绝不发成功结束标记（A02）
// ===========================================================================

func TestCursorToolChoiceViolationFailsHTTP(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 101, IDField: []byte("exec-101")}}
	svc := newCursorFullChainService(t, newCursorFullChainUpstream(cursorFullChainScript{calls: calls, hangAfterPhase1: true}))

	w, _, err := runFirstHTTPMode(t.Context(), svc, &cursorTestHandover{}, true, "none")
	require.Error(t, err, "上游产出被禁止的工具批时 HTTP 不得报成功")
	out := w.Body.String()
	require.NotContains(t, out, `"tool_calls"`, "违反 tool_choice 的工具批绝不能交付给客户端")
	require.NotContains(t, out, cursorSSEDoneSentinel, "失败时不得写成功结束标记")

	// 失败链的最终状态：会话与索引（含本批 ID）必须被唯一 finalizer 全部清掉，
	// 绝不留下"已交付"的半批（会话可能已被 pump 收口，故只断言可观察副作用）。
	require.Eventually(t, func() bool {
		return svc.sessions.sessionCount() == 0 && svc.sessions.indexCount() == 0
	}, 5*time.Second, 10*time.Millisecond, "失败必须沿唯一 finalizer 终结会话并清理全部关联")
}

// ===========================================================================
// 5. 索引冲突：整批登记失败即错误出口，不半交付
// ===========================================================================

func TestCursorBatchIndexConflictFailsHTTP(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_dup", Name: "get_weather", City: "hangzhou", ExecID: 121, IDField: []byte("exec-121")}}
	svc := newCursorFullChainService(t, newCursorFullChainUpstream(cursorFullChainScript{calls: calls, hangAfterPhase1: true}))

	// 另一个会话先占用同名 call ID：本次整批必须冲突失败（不得覆盖已有归属）。
	other := svc.sessions.start(cursorSessionSpec{
		protocol:       cursorProtocolChat,
		requestedModel: "cursor-sonnet",
		upstreamModel:  "claude-opus-4-8-low",
		accountID:      1,
		identity:       CursorSessionIdentity{UserID: 99},
	})
	require.NoError(t, svc.sessions.registerSessionID(other.id, "other-batch", CursorIDKindCall, "call_dup"))

	w, _, err := runFirstHTTPMode(t.Context(), svc, &cursorTestHandover{}, true, "")
	require.Error(t, err, "索引冲突必须让本 HTTP 失败")
	require.NotContains(t, w.Body.String(), `"call_dup"`, "冲突的批不得交付给客户端")
	// finalizer 在 pump goroutine 里收口：等它把本会话与索引清干净（只留先占会话）。
	require.Eventually(t, func() bool {
		return svc.sessions.sessionCount() == 1 && svc.sessions.indexCount() == 1
	}, 5*time.Second, 10*time.Millisecond, "本会话必须被终结清理（只留先占会话及其索引）")
}

// ===========================================================================
// 6. 短写：显式归为 io.ErrShortWrite（§25.17.2-6）
// ===========================================================================

// cursorShortWriter 每次只写一半字节且不返回错误。
type cursorShortWriter struct{ written int }

func (w *cursorShortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.written += len(p) / 2
	return len(p) / 2, nil
}

func TestCursorWriteAllShortWriteIsError(t *testing.T) {
	err := cursorWriteAll(&cursorShortWriter{}, []byte("0123456789"))
	require.ErrorIs(t, err, io.ErrShortWrite, "n<len 且 err=nil 必须归为 io.ErrShortWrite")
}

// ===========================================================================
// 7. 续轮 stream → nonstream 切换：用本 HTTP 选项、AgentRun 恒 1、结算恒 1
// ===========================================================================

func TestCursorResumeSwitchesStreamToNonStream(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 131, IDField: []byte("exec-131")}}
	upstream := newCursorFullChainUpstream(cursorFullChainScript{
		calls:     calls,
		afterText: "Hangzhou is sunny",
		usage:     [4]uint64{120, 7, 3, 0},
	})
	svc := newCursorFullChainService(t, upstream)
	settlement := &cursorFullChainSettlement{}
	handover := &cursorTestHandover{settlement: settlement.hook}

	w1, _, err1 := runFirstHTTPMode(t.Context(), svc, handover, true, "")
	require.NoError(t, err1)
	require.Contains(t, w1.Body.String(), `"call_1"`)

	w2, err2 := resumeSessionMode(t, svc, cursorFullChainResumeBody(calls, map[string]string{"call_1": "sunny, 25C"}), false)
	require.NoError(t, err2, "stream→nonstream 续轮必须成功")
	var out map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &out), "续轮 nonstream 必须是一个合法 JSON: %s", w2.Body.String())
	message := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	require.Equal(t, "Hangzhou is sunny", message["content"])
	require.NotContains(t, w2.Body.String(), `"tool_calls"`, "已交付批次不得在续轮重播")
	require.Equal(t, 1, upstream.runCount(), "续轮不得新建 AgentRun")
	calls0, complete0 := settlement.snapshot()
	require.Equal(t, 1, calls0)
	require.Equal(t, 1, complete0)
}

// ===========================================================================
// 8. Gemini 落点：工具声明/配置/调用/结果真实进入转换（A03 探针 3）
// ===========================================================================

func TestCursorGeminiToolDeclarationSurvivesNativeAdapter(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"weather please"}]}],"tools":[{"functionDeclarations":[{"name":"get_weather","description":"weather","parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`)
	cc, err := cursorGeminiRequestToChatCompletions(body, "cursor-sonnet", true)
	require.NoError(t, err)
	var req apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal(cc, &req))
	require.Len(t, req.Tools, 1, "原生 Gemini 工具声明必须进入执行部件")
	require.Equal(t, "get_weather", req.Tools[0].Function.Name)
	require.Equal(t, "weather", req.Tools[0].Function.Description)
	require.JSONEq(t, `"auto"`, string(req.ToolChoice), "AUTO 必须映射为 auto")

	mode, _, err := cursorGeminiToolChoiceMode(body)
	require.NoError(t, err)
	require.Equal(t, "auto", mode)
}

// TestReauditGeminiAllowedSubsetReachesActualToolDeclaration 是验收报告 R4 的反例转绿探针。
//
// 反例：声明 allowed_tool + forbidden_tool 两个工具，ANY 仅允许前者；修正前实际转换仍携带
// 两个工具及 required，禁止本轮使用的工具被暴露给模型。修正后实际转换 JSON 必须只含被允许的
// 声明子集，且 tool_choice 保持 required 语义。
func TestReauditGeminiAllowedSubsetReachesActualToolDeclaration(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"use a tool"}]}],` +
		`"tools":[{"functionDeclarations":[` +
		`{"name":"allowed_tool","description":"allowed","parameters":{"type":"object"}},` +
		`{"name":"forbidden_tool","description":"forbidden","parameters":{"type":"object"}}` +
		`]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["allowed_tool"]}}}`)

	cc, err := cursorGeminiRequestToChatCompletions(body, "cursor-sonnet", true)
	require.NoError(t, err, "ANY+allowedFunctionNames 必须是合法出站请求")

	var req apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal(cc, &req), "转换结果必须是合法 Chat 请求: %s", cc)

	// 实际转换 JSON 断言（不只校验名称存在）：只含被允许的声明子集。
	require.Len(t, req.Tools, 1, "ANY+allowedFunctionNames 必须只暴露允许子集，禁止带 forbidden_tool")
	require.Equal(t, "allowed_tool", req.Tools[0].Function.Name)
	require.JSONEq(t, `"required"`, string(req.ToolChoice), "ANY 单名/子集仍保持 required 语义")

	// 反向确认 forbidden_tool 不在实际上游声明里。
	require.NotContains(t, string(cc), "forbidden_tool", "禁止使用的工具不得进入实际上游声明")

	// 模式/允许集合语义经出站前校验通过（零 400）。
	mode, _, err := cursorGeminiToolChoiceMode(body)
	require.NoError(t, err)
	require.Equal(t, "required", mode)
}

func TestCursorGeminiToolConfigAndFunctionPartsMapping(t *testing.T) {
	// NONE → none；ANY → required。
	for raw, want := range map[string]string{
		`{"mode":"NONE"}`: "none",
		`{"mode":"ANY"}`:  "required",
	} {
		body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"get_weather"}]}],"toolConfig":{"functionCallingConfig":` + raw + `}}`)
		mode, _, err := cursorGeminiToolChoiceMode(body)
		require.NoError(t, err)
		require.Equal(t, want, mode)
	}
	// 未知模式 / 未声明名称：明确错误且未出站。
	unknown := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"a"}]}],"toolConfig":{"functionCallingConfig":{"mode":"MAYBE"}}}`)
	_, _, err := cursorGeminiToolChoiceMode(unknown)
	require.ErrorIs(t, err, ErrCursorToolChoiceInvalid)
	undeclared := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"a"}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["b"]}}}`)
	_, _, err = cursorGeminiToolChoiceMode(undeclared)
	require.ErrorIs(t, err, ErrCursorToolChoiceInvalid)

	// functionCall / functionResponse 的 ID/名称/参数真实进入转换。
	contents := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"weather"}]},
		{"role":"model","parts":[{"functionCall":{"id":"call_9","name":"get_weather","args":{"city":"hz"}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"call_9","name":"get_weather","response":{"temp":25}}}]}
	]}`)
	cc, err := cursorGeminiRequestToChatCompletions(contents, "cursor-sonnet", false)
	require.NoError(t, err)
	var req apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal(cc, &req))
	require.Len(t, req.Messages, 3)
	require.Len(t, req.Messages[1].ToolCalls, 1)
	require.Equal(t, "call_9", req.Messages[1].ToolCalls[0].ID)
	require.Equal(t, "get_weather", req.Messages[1].ToolCalls[0].Function.Name)
	require.JSONEq(t, `{"city":"hz"}`, req.Messages[1].ToolCalls[0].Function.Arguments)
	require.Equal(t, "tool", req.Messages[2].Role)
	require.Equal(t, "call_9", req.Messages[2].ToolCallID)
	require.JSONEq(t, `{"temp":25}`, string(req.Messages[2].Content))
}

// ===========================================================================
// 9. Gemini 原生出口：工具批交付保留原 call ID（经同一执行器 Gemini writer）
// ===========================================================================

func TestCursorGeminiNativeStreamDeliversFunctionCallWithID(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_g1", Name: "get_weather", City: "hangzhou", ExecID: 141, IDField: []byte("exec-141")}}
	svc := newCursorFullChainService(t, newCursorFullChainUpstream(cursorFullChainScript{calls: calls, hangAfterPhase1: true}))

	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"weather please"}]}],"tools":[{"functionDeclarations":[{"name":"get_weather","parameters":{"type":"object"}}]}]}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	_, err := cursorExecuteGeminiNative(context.Background(), c, svc, cursorFullChainAccount(), "cursor-sonnet", true, body)
	require.NoError(t, err)

	out := w.Body.String()
	require.Contains(t, out, `"functionCall"`, "Gemini 流式必须把工具批写成 functionCall parts")
	require.Contains(t, out, `"call_g1"`, "functionCall 必须保留原 call ID")
	require.Contains(t, out, "get_weather")
	require.NotContains(t, out, "[DONE]", "Gemini SSE 没有 [DONE] 哨兵")
	require.NotContains(t, out, "usageMetadata", "工具批检查点不得写 usageMetadata（中间不造最终 token）")
}

// ===========================================================================
// 10. S1b-1：Responses 工具结果 item ID 登记进**同一**统一索引
//
// 要求（§25.17.2「实际ID」）：
//   - 键含 item 种类，与 call/response 同一索引，不建第二索引；
//   - 登记的 item ID 必须是客户端**实际收到**的那个（非流式取自实际构建的响应对象，
//     不读 stream respState、不另生成一份）；
//   - finalizer 清会话时把 item 种类一并清掉。
// ===========================================================================

// indexItemIDsForSession 返回该会话在统一索引里登记的 item 种类 ID（测试断言用）。
func indexItemIDsForSession(svc *CursorExecutionService, sessionID string) []string {
	var ids []string
	for key := range svc.sessions.index.EntriesForSession(sessionID) {
		if key.Kind == CursorIDKindItem {
			ids = append(ids, key.ID)
		}
	}
	return ids
}

// pendingBatchID 读取当前待完成批的批次 ID。
func pendingBatchID(t *testing.T, record *cursorSessionRecord) string {
	t.Helper()
	record.mu.Lock()
	defer record.mu.Unlock()
	require.NotNil(t, record.pending)
	return record.pending.batchID
}

func TestCursorResponsesItemIDsRegisteredFromActualDelivery(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "nonstream", true: "stream"}[stream]
		t.Run(name, func(t *testing.T) {
			calls := []cursorFullChainCall{{CallID: "call_item", Name: "get_weather", City: "hangzhou", ExecID: 161, IDField: []byte("exec-161")}}
			svc := newCursorFullChainService(t, newCursorFullChainUpstream(cursorFullChainScript{calls: calls, hangAfterPhase1: true}))

			w, err := responsesFirstHTTP(t, svc, stream)
			require.NoError(t, err)
			body := w.Body.String()
			require.Contains(t, body, "call_item", "工具批必须真实交付给客户端")

			record := onlySession(t, svc)
			batchID := pendingBatchID(t, record)
			require.NotEmpty(t, batchID)

			itemIDs := indexItemIDsForSession(svc, record.id)
			require.NotEmpty(t, itemIDs, "对外工具结果 item ID 必须登记进统一索引（键含 item 种类）")
			for _, itemID := range itemIDs {
				require.NotEqual(t, "cursor-chat", itemID, "不得登记固定常量")
				require.Contains(t, body, itemID, "登记的 item ID 必须是客户端实际收到的那个")
				entry, locErr := svc.sessions.index.Locate(CursorSessionIndexKey{
					Protocol: "responses", Kind: CursorIDKindItem, ID: itemID,
				})
				require.NoError(t, locErr)
				require.Equal(t, record.id, entry.SessionID, "item 归属必须是本会话")
				require.Equal(t, batchID, entry.BatchID, "item 归属必须是当前交付批次")
			}

			// call/response 仍在同一索引的原有种类下（新增 item 种类不挤占、不覆盖）。
			callEntry, callErr := svc.sessions.index.Locate(CursorSessionIndexKey{
				Protocol: "responses", Kind: CursorIDKindCall, ID: "call_item",
			})
			require.NoError(t, callErr)
			require.Equal(t, record.id, callEntry.SessionID)
			_, respErr := svc.sessions.index.Locate(CursorSessionIndexKey{
				Protocol: "responses", Kind: CursorIDKindResponse, ID: pendingResponseID(t, record),
			})
			require.NoError(t, respErr)

			// finalizer 清会话：call / response / item 必须**全清**（不只删当前 pending）。
			svc.finishCursorSession(record, "audit_cleanup")
			require.Zero(t, svc.sessions.sessionCount())
			require.Zero(t, svc.sessions.indexCount(), "finalizer 必须把 item 种类一并清掉")
			require.Empty(t, svc.sessions.index.EntriesForSession(record.id))
		})
	}
}

// ===========================================================================
// 11. S1b-2：waiting_tools 期间的新模型正文 = 显式协议失败（不重放不缓存）
// ===========================================================================

func TestCursorContentWhileWaitingToolsFailsProtocol(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_w", Name: "get_weather", City: "hangzhou", ExecID: 171, IDField: []byte("exec-171")}}
	ready := make(chan struct{})
	upstream := newCursorFullChainUpstream(cursorFullChainScript{
		calls:                 calls,
		hangAfterPhase1:       true,
		injectAfterBatchText:  "late chatter after tool batch",
		injectAfterBatchReady: ready,
	})
	svc := newCursorFullChainService(t, upstream)

	w, err := responsesFirstHTTP(t, svc, true)
	require.NoError(t, err)
	out := w.Body.String()
	require.Contains(t, out, "call_w", "首批工具批必须正常交付")

	record := onlySession(t, svc)
	// 等交付确认（waiting_tools）后再注入：契约违约只发生在"等待真实结果"期间。
	require.Eventually(t, func() bool {
		record.mu.Lock()
		defer record.mu.Unlock()
		return record.state == cursorSessionWaitingTools
	}, 5*time.Second, 5*time.Millisecond, "交付批后会话必须进入 waiting_tools")
	close(ready)

	require.Eventually(t, func() bool {
		record.mu.Lock()
		defer record.mu.Unlock()
		return record.state == cursorSessionClosed
	}, 5*time.Second, 5*time.Millisecond, "等待期间的正文必须让会话沿错误出口终结")

	record.mu.Lock()
	reason := record.interruptReason
	record.mu.Unlock()
	require.Equal(t, "content_while_waiting_tools", reason, "必须记录明确的协议失败原因")

	// 零客户端成功帧：违约正文绝不出现在任何已写出的客户端字节里。
	require.NotContains(t, out, "late chatter", "违约正文不得交付给客户端")
	require.Eventually(t, func() bool {
		return svc.sessions.sessionCount() == 0 && svc.sessions.indexCount() == 0
	}, 5*time.Second, 5*time.Millisecond, "失败必须沿唯一 finalizer 清空会话与全部索引关联")
}

// ===========================================================================
// 12. S1b-3：Responses custom / namespace / tool_search 元数据跨 HTTP 不丢
//
// 首个 HTTP 交付一批 custom/namespace/tool_search 调用；续轮（previous_response_id +
// 本批真实结果）再交付**第二批**同类调用。第二个 HTTP 的写出侧必须仍用同一套工具映射：
// 一旦续轮丢了 Tools，这些条目会降级成普通 function_call（§24.2-5 / §25.14-D）。
// ===========================================================================

const cursorResponsesClientToolsJSON = `[` +
	`{"type":"custom","name":"apply_patch","description":"freeform patch"},` +
	`{"type":"namespace","name":"fs","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},` +
	`{"type":"tool_search"}` +
	`]`

// responsesBodyWithTools 组装首次 Responses 请求体（非流式）。
func responsesBodyWithTools(stream bool) string {
	return `{"model":"cursor-sonnet","stream":` + boolJSON(stream) + `,"input":"patch please","tools":` +
		cursorResponsesClientToolsJSON + `}`
}

// responsesContinuationBody 组装续轮请求体：previous_response_id + 本批真实结果 + 同一套工具声明。
func responsesContinuationBody(previousResponseID string, callIDs []string) string {
	outputs := make([]string, 0, len(callIDs))
	for _, callID := range callIDs {
		outputs = append(outputs, `{"type":"function_call_output","call_id":"`+callID+`","output":"done"}`)
	}
	return `{"model":"cursor-sonnet","stream":false,"previous_response_id":"` + previousResponseID +
		`","input":[` + strings.Join(outputs, ",") + `],"tools":` + cursorResponsesClientToolsJSON + `}`
}

func TestCursorResponsesToolMetadataSurvivesContinuation(t *testing.T) {
	firstBatch := []cursorFullChainCall{
		{CallID: "call_custom_1", Name: "apply_patch", City: "hz", ExecID: 181, IDField: []byte("exec-181")},
		{CallID: "call_ns_1", Name: "fs__read", City: "hz", ExecID: 182, IDField: []byte("exec-182")},
		{CallID: "call_search_1", Name: "tool_search", City: "hz", ExecID: 183, IDField: []byte("exec-183")},
	}
	secondBatch := []cursorFullChainCall{
		{CallID: "call_custom_2", Name: "apply_patch", City: "hz", ExecID: 191, IDField: []byte("exec-191")},
		{CallID: "call_ns_2", Name: "fs__read", City: "hz", ExecID: 192, IDField: []byte("exec-192")},
		{CallID: "call_search_2", Name: "tool_search", City: "hz", ExecID: 193, IDField: []byte("exec-193")},
	}
	svc := newCursorFullChainService(t, newCursorFullChainUpstream(cursorFullChainScript{
		calls:       firstBatch,
		secondBatch: secondBatch,
	}))

	// ---- HTTP1：首批交付即带正确的工具类型 ----
	var first apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(responsesBodyWithTools(false)), &first))
	w1 := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(w1)
	_, err := cursorExecuteResponses(context.Background(), c1, svc, cursorFullChainAccount(), &first)
	require.NoError(t, err)
	out1 := w1.Body.String()
	require.Contains(t, out1, `"custom_tool_call"`, "首个 HTTP 必须把 apply_patch 交付成 custom_tool_call")
	require.Contains(t, out1, `"namespace":"fs"`, "首个 HTTP 必须保留 namespace 归属")
	require.Contains(t, out1, `"tool_search_call"`, "首个 HTTP 必须把 tool_search 交付成 tool_search_call")

	record := onlySession(t, svc)
	previousResponseID := pendingResponseID(t, record)
	require.NotEmpty(t, previousResponseID)

	// ---- HTTP2：续轮（previous_response_id + 本批真实结果）再交付一批同类调用 ----
	continuation := responsesContinuationBody(previousResponseID, []string{"call_custom_1", "call_ns_1", "call_search_1"})
	probe, probeErr := CursorProbeResumeRequest(CursorResumeProtocolResponses, []byte(continuation))
	require.NoError(t, probeErr)
	ticket, ticketErr := svc.IdentifyResume(probe)
	require.NoError(t, ticketErr)
	require.NotNil(t, ticket, "续轮必须定位到唯一待完成批")

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	_, serveErr := svc.ResumeCursorSession(context.Background(), c2, ticket, cursorFullChainAccount(),
		CursorResumeServeInput{Body: []byte(continuation), Stream: false})
	require.NoError(t, serveErr, "续轮必须成功（工具声明与首轮一致）")
	out2 := w2.Body.String()

	// 元数据跨 HTTP 不丢：三种类型都必须**按原类型**还原，而不是降级成 function_call。
	require.Contains(t, out2, `"custom_tool_call"`, "续轮不得把 custom 工具降级成 function_call")
	require.Contains(t, out2, `"name":"apply_patch"`)
	require.Contains(t, out2, `"namespace":"fs"`, "续轮不得丢掉 namespace 归属")
	require.Contains(t, out2, `"name":"read"`)
	require.Contains(t, out2, `"tool_search_call"`, "续轮不得把 tool_search 降级成 function_call")
	require.Contains(t, out2, "call_ns_2", "续轮第二批必须真实交付")

	defer svc.finishCursorSession(record, "audit_cleanup")
}
