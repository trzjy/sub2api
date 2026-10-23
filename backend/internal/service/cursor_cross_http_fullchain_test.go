//go:build unit

package service

// 本文件是 **W2-F：跨 HTTP 真实结果回写通道的端到端定向测试**。
//
// 与 cursor_cross_http_writeback_test.go（组件级）互补：这里走**完整生产链**，
// 不打桩任何环节——
//
//	HTTP1: 真实 Execute（gin writer + 真实 httptest 上游 + 真实原生双工/读循环）
//	        → 交付工具批 → 会话进入 waiting_tools 并**跨 HTTP 存活**（不 finalize）
//	HTTP2: 真实 ResumeCursorSession（探针 → 统一索引 → 整批接管 → 摘要校验）
//	        → CursorSessionIntake 生产实现把真实结果按 §25.3 四层编码写回原 exec 关联
//	        → 上游收到真实结果后继续跑 → turn_ended 真实 usage → finalizer 唯一结算
//
// 覆盖派发单要求：单工具两 HTTP 全链、同名并行两工具、唯一结算断言、180s 到期终结、
// 附着期间断连终结。旧链语义（文本补历史 / 伪 McpError / half-close 冒充）在这里是
// **反证**：一旦回退到"收到 mcp_args 就回 McpResult.error 或直接收尾"，下列断言立刻红。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 原生帧构造（不依赖 *testing.T，可在服务端 handler goroutine 里安全使用）
// ---------------------------------------------------------------------------

// fcTextDelta 构造 interaction_update{1 text_delta{1 text}}。
func fcTextDelta(text string) []byte {
	delta := cursorAppendStringField(nil, cursorFldTextDeltaText, text)
	update := cursorAppendBytesField(nil, cursorFldUpdateTextDelta, delta)
	return cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update)
}

// fcToolStarted 构造 interaction_update{2 tool_call_started{15 ToolCall{1 call_id}}}。
//
// 字段形状取解析器契约（cursorStreamParser.noteToolCallStarted）：f2 内层 f15 是 ToolCall
// 的 mcp_tool_call 分支，其 f1 为客户端要执行的 call_id；不带 f15 的视为内部元工具
// （get_mcp_tools 等），只参与同批完整性判断。端到端必须走**对外分支**，否则批次里
// 根本没有可交付的调用。
func fcToolStarted(callID string) []byte {
	toolCall := cursorAppendStringField(nil, cursorFldToolCallID, callID)
	started := cursorAppendBytesField(nil, cursorFldToolCallStartedToolCall, toolCall)
	update := cursorAppendBytesField(nil, cursorFldUpdateToolCallStarted, started)
	return cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update)
}

// fcToolRequestsListed 构造 interaction_update{27 tool_requests_listed{1 call_count}}。
func fcToolRequestsListed(count uint64) []byte {
	listed := cursorAppendVarintField(nil, cursorFldToolRequestsListedCallCount, count)
	update := cursorAppendBytesField(nil, cursorFldUpdateToolRequestsListed, listed)
	return cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update)
}

// fcTurnEnded 构造 interaction_update{14 turn_ended{input/output/cache_read/cache_write}}。
//
// 四计费字段 f1–f4 全部显式编码（§25.17.5 A06①：proto optional 存在性与"真实 0"
// 是两回事，cache_write=0 也必须出现在 wire 上，缺失会被 R1 完整性判定视为不完整
// 而拒绝结算）。故 f4 即便为 0 也照常写入（非 0 值同样由编码产出）。
func fcTurnEnded(in, out, cacheRead, cacheWrite uint64) []byte {
	usage := cursorAppendVarintField(nil, cursorFldTurnEndedInputTokens, in)
	usage = cursorAppendVarintField(usage, cursorFldTurnEndedOutputTokens, out)
	usage = cursorAppendVarintField(usage, cursorFldTurnEndedCacheReadTokens, cacheRead)
	usage = cursorAppendVarintField(usage, cursorFldTurnEndedCacheWriteTokens, cacheWrite)
	update := cursorAppendBytesField(nil, cursorFldUpdateTurnEnded, usage)
	return cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update)
}

// fcExecMcpArgs 构造 exec{mcp_args}：AgentServerMessage.f2 ExecServerMessage{f1 exec_id, f15 id, f11 args}。
func fcExecMcpArgs(execID uint64, idField, args []byte) []byte {
	exec := cursorAppendVarintField(nil, 1, execID)
	exec = cursorAppendBytesField(exec, 15, idField)
	exec = cursorAppendBytesField(exec, 11, args)
	return cursorAppendBytesField(nil, cursorFldServerExecMessage, exec)
}

// fcEndStream 构造 EndStream 终态 envelope（flags bit1，空载荷）。
func fcEndStream() []byte { return []byte{0x02, 0, 0, 0, 0} }

// joinFrames 把若干帧载荷拼成带 envelope 的字节流。
func joinFrames(payloads ...[]byte) []byte {
	var out []byte
	for _, p := range payloads {
		out = append(out, cursorEncodeEnvelope(p)...)
	}
	return out
}

// ---------------------------------------------------------------------------
// 假上游：既是下游响应写端，也是真实结果回写的读端
// ---------------------------------------------------------------------------

// cursorFullChainCall 描述一个由模型委托、由客户端真实执行的工具调用。
type cursorFullChainCall struct {
	CallID  string
	Name    string
	City    string
	ExecID  uint64
	IDField []byte
}

// cursorFullChainScript 是假上游的剧本。
type cursorFullChainScript struct {
	// calls 是本轮交付的工具批。
	calls []cursorFullChainCall
	// afterText 是收到真实结果后模型续跑产出的文本。
	afterText string
	// usage 是 turn_ended 的真实用量（input / output / cacheRead / cacheWrite）。
	usage [4]uint64
	// hangAfterPhase1 为 true 时上游交付批次后就挂着（用于 deadline / 断连场景）。
	hangAfterPhase1 bool
	// textOnly 为 true 时 phase1 只吐文本（不交付工具批），用于"附着期间断连"：
	// 首 HTTP 会一直挂在 drain 上（保持附着），直到客户端断连。
	textOnly bool
	// secondBatch 是**续轮恢复后**交付的第二批工具调用（非空时不再产出终态文本）。
	//
	// 用于证明"工具元数据跨 HTTP 保留"：第一批交付 → 客户端回写真实结果 → 模型在第二个
	// HTTP 里再交付同类型的调用；第二个 HTTP 的写出侧必须仍能还原 custom / namespace /
	// tool_search 类型（否则会降级成普通 function_call）。
	secondBatch []cursorFullChainCall
	// injectAfterBatchText 是**不等真实结果**就产出的正文（违反原生阶段契约的注入）。
	//
	// 非空时在交付首批后（见 injectAfterBatchReady）吐一段正文增量，再按 hangAfterPhase1
	// 挂着：读循环必须在 waiting_tools 期间把它判为显式协议失败。
	injectAfterBatchText string
	// injectAfterBatchReady 就绪后才注入（测试在观察到 waiting_tools 后关闭它），
	// 避免注入早于交付确认而把用例变成时间竞态。
	injectAfterBatchReady <-chan struct{}
}

// cursorFullChainUpstream 扮演 AgentRun 端点。
type cursorFullChainUpstream struct {
	script cursorFullChainScript

	mu         sync.Mutex
	runCalls   int
	upBuf      []byte
	readOff    int
	clientData [][]byte // 上行每条 envelope 的 Data（初始 AgentRun 请求 + 回写）
	writebacks [][]byte // 其中承载 McpResult 的那些（按到达顺序）

	writebackCh chan struct{}
	writebackMu sync.Once
}

func newCursorFullChainUpstream(script cursorFullChainScript) *cursorFullChainUpstream {
	if len(script.calls) == 0 {
		panic("cursor full chain script needs at least one call")
	}
	return &cursorFullChainUpstream{script: script, writebackCh: make(chan struct{})}
}

func (u *cursorFullChainUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/Run") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		u.mu.Lock()
		u.runCalls++
		u.mu.Unlock()

		w.Header().Set("Content-Type", cursorConnectContentType)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		go u.readClientStream(r.Body)

		u.writeStream(w, flusher, u.phase1())
		if u.script.injectAfterBatchText != "" {
			if u.script.injectAfterBatchReady != nil {
				select {
				case <-u.script.injectAfterBatchReady:
				case <-r.Context().Done():
					return
				}
			}
			u.writeStream(w, flusher, joinFrames(fcTextDelta(u.script.injectAfterBatchText)))
		}
		if u.script.hangAfterPhase1 {
			// 交付批次后挂着：不放终态，靠会话 ctx 的 180s 期限 / 客户端断连收尾。
			<-r.Context().Done()
			return
		}
		select {
		case <-u.writebackCh:
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Second):
			return
		}
		if len(u.script.secondBatch) > 0 {
			// 续轮的第二批：交付后挂着（等下一轮真实结果，或由测试收尾）。
			u.writeStream(w, flusher, u.batchFrames(u.script.secondBatch, ""))
			<-r.Context().Done()
			return
		}
		u.writeStream(w, flusher, u.phase2())
	})
}

// phase1 交付一个完整工具批：started×N → listed(N) → f11 委托×N。
//
// listed 刻意排在所有 started 之后、委托之前：只按"第一个 mcp_args"就交接的实现会在
// started 未等齐时提前封口 → 直接红（§25.3 要求 listed 数等齐才封口）。
func (u *cursorFullChainUpstream) phase1() []byte {
	if u.script.textOnly {
		return joinFrames(fcTextDelta("thinking"))
	}
	return u.batchFrames(u.script.calls, "checking the weather")
}

// batchFrames 构造一个完整工具批（started×N → listed(N) → f11 委托×N），leadText 非空时
// 先吐一段文本增量。第二批与首批走同一构造，避免两套帧语义。
func (u *cursorFullChainUpstream) batchFrames(calls []cursorFullChainCall, leadText string) []byte {
	var payloads [][]byte
	if leadText != "" {
		payloads = append(payloads, fcTextDelta(leadText))
	}
	for _, call := range calls {
		payloads = append(payloads, fcToolStarted(call.CallID))
	}
	payloads = append(payloads, fcToolRequestsListed(uint64(len(calls))))
	for _, call := range calls {
		args := cursorToolTestBuildMcpArgs(call.CallID, call.Name, []cursorToolTestMcpArgEntry{
			{key: "city", kind: "string", val: call.City},
		})
		payloads = append(payloads, fcExecMcpArgs(call.ExecID, call.IDField, args))
	}
	return joinFrames(payloads...)
}

// phase2 是模型收到真实结果后的续跑：文本 + 真实 usage + 正常终态。
func (u *cursorFullChainUpstream) phase2() []byte {
	return append(joinFrames(
		fcTextDelta(u.script.afterText),
		fcTurnEnded(u.script.usage[0], u.script.usage[1], u.script.usage[2], u.script.usage[3]),
	), fcEndStream()...)
}

// writeStream 把已带 envelope 的字节整体写出并 Flush（逐帧可读，客户端不会等到流结束）。
func (u *cursorFullChainUpstream) writeStream(w http.ResponseWriter, flusher http.Flusher, frames []byte) {
	if len(frames) == 0 {
		return
	}
	_, _ = w.Write(frames)
	if flusher != nil {
		flusher.Flush()
	}
}

// readClientStream 读上行帧：初始 AgentRun 请求 + 后续的真实结果回写。
func (u *cursorFullChainUpstream) readClientStream(body io.ReadCloser) {
	defer func() { _ = body.Close() }()
	tmp := make([]byte, 32*1024)
	for {
		n, err := body.Read(tmp)
		if n > 0 {
			u.consumeClientBytes(tmp[:n])
		}
		if err != nil {
			return
		}
	}
}

// consumeClientBytes 累积上行字节并切出完整 envelope（半帧留到下次）。
func (u *cursorFullChainUpstream) consumeClientBytes(chunk []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.upBuf = append(u.upBuf, chunk...)
	for u.readOff < len(u.upBuf) {
		frame, next, err := cursorReadEnvelopeFrame(u.upBuf, u.readOff)
		if err != nil {
			return // 半帧：等后续字节
		}
		data := append([]byte(nil), frame.Data...)
		u.clientData = append(u.clientData, data)
		if payload, ok := cursorFullChainMcpResultPayload(data); ok {
			u.writebacks = append(u.writebacks, payload)
			if len(u.writebacks) >= len(u.script.calls) {
				u.writebackMu.Do(func() { close(u.writebackCh) })
			}
		}
		u.readOff = next
	}
}

// cursorFullChainMcpResultPayload 判断一条上行 envelope 是否承载 McpResult（真实结果回写），
// 是则返回其 **ExecClientMessage 载荷**（§25.3 四层编码的外层消息，便于逐层解码）。
func cursorFullChainMcpResultPayload(data []byte) ([]byte, bool) {
	fields, err := cursorDecodeProtobuf(data)
	if err != nil {
		return nil, false
	}
	exec, ok := cursorFindField(fields, cursorFldClientMessage)
	if !ok {
		return nil, false
	}
	execFields, err := cursorDecodeProtobuf(exec.Bytes)
	if err != nil {
		return nil, false
	}
	if !cursorFindFieldOK(execFields, cursorFldClientMessage) {
		return nil, false
	}
	return append([]byte(nil), exec.Bytes...), true
}

func (u *cursorFullChainUpstream) runCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.runCalls
}

// writebackPayloads 返回捕获到的真实结果回写（ExecClientMessage 载荷，按到达顺序）。
func (u *cursorFullChainUpstream) writebackPayloads() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([][]byte, len(u.writebacks))
	copy(out, u.writebacks)
	return out
}

// clientFirstField 返回第一条上行帧的消息类型字段（必须是 AgentRun 请求 = f1）。
func (u *cursorFullChainUpstream) clientFirstField() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.clientData) == 0 {
		return -1
	}
	fields, err := cursorDecodeProtobuf(u.clientData[0])
	if err != nil || len(fields) == 0 {
		return -1
	}
	return fields[0].Number
}

// clientRequestData 返回第一条上行帧（AgentRun 请求体）。
func (u *cursorFullChainUpstream) clientRequestData() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.clientData) == 0 {
		return nil
	}
	return u.clientData[0]
}

// ---------------------------------------------------------------------------
// 与生产同构的执行部件
// ---------------------------------------------------------------------------

// newCursorFullChainService 装配一个指向假上游的执行部件（依赖与生产同构，仅 transport 为替身）。
func newCursorFullChainService(t *testing.T, upstream *cursorFullChainUpstream) *CursorExecutionService {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ts := httptest.NewUnstartedServer(upstream.handler())
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)

	nodes := NewCursorUpstreamNodesResolver(WithCursorNodesCacheTTL(time.Hour))
	nodes.cached = CursorUpstreamNodes{HTTP1Base: ts.URL, HTTP2Base: ts.URL}
	nodes.cachedAt = time.Now()

	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{
		7: {ID: 7, CodeEncrypted: "ACTIVATION-CODE-7"},
	}}
	return newCursorTestExecutionService(t, repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo),
		NewCursorUpstreamForwarder(
			WithCursorUpstreamNodes(nodes),
			WithCursorUpstreamHTTPClient(ts.Client()),
		))
}

// cursorFullChainIdentity 是本次会话的所有者身份。
var cursorFullChainIdentity = CursorSessionIdentity{UserID: 11, APIKeyID: 22, GroupID: 33}

// cursorFullChainFirstBody 是首 HTTP 的 Chat 请求体（声明一个工具）。
const cursorFullChainFirstBody = `{
  "model": "cursor-sonnet",
  "stream": true,
  "messages": [{"role": "user", "content": "weather please"}],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`

// cursorFullChainResumeBody 返回续轮 Chat 请求体（重复上下文 + 本批真实结果）。
func cursorFullChainResumeBody(calls []cursorFullChainCall, results map[string]string) string {
	var toolCalls, toolMsgs []string
	for _, call := range calls {
		toolCalls = append(toolCalls, `{"id":"`+call.CallID+`","type":"function","function":{"name":"`+
			call.Name+`","arguments":"{\"city\":\"`+call.City+`\"}"}}`)
	}
	for _, call := range calls {
		toolMsgs = append(toolMsgs, `{"role":"tool","tool_call_id":"`+call.CallID+`","content":"`+
			results[call.CallID]+`"}`)
	}
	return `{
  "model": "cursor-sonnet",
  "stream": true,
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "checking the weather", "tool_calls": [` + strings.Join(toolCalls, ",") + `]},
    ` + strings.Join(toolMsgs, ",\n    ") + `
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
}

// cursorFullChainAccount 返回映射到 wire 模型的 cursor 账号。
func cursorFullChainAccount() *Account {
	return &Account{
		ID: 1, Platform: PlatformCursor, QuotaKeyID: qkID(7),
		Credentials: map[string]any{
			"model_mapping": map[string]any{"cursor-sonnet": "claude-opus-4-8-low"},
			"access_token":  cursorE2EAccountBearer,
		},
	}
}

// cursorFullChainSettlement 记录结算事件（唯一结算断言的来源）。
type cursorFullChainSettlement struct {
	mu       sync.Mutex
	events   []CursorSettlementEvent
	complete int
	calls    int
}

func (s *cursorFullChainSettlement) hook(_ context.Context, ev CursorSettlementEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.events = append(s.events, ev)
	if ev.UsageComplete {
		s.complete++
	}
}

func (s *cursorFullChainSettlement) snapshot() (calls int, complete int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.complete
}

// runFirstHTTP 走真实 Execute 发首 HTTP（交付工具批后应立即返回，会话跨 HTTP 存活）。
//
// 不使用 require（本函数可能在测试 goroutine 里调用）：错误一律回传。
func runFirstHTTP(
	ctx context.Context, svc *CursorExecutionService, handover CursorSessionHandover,
) (*httptest.ResponseRecorder, *ForwardResult, error) {
	var ccReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal([]byte(cursorFullChainFirstBody), &ccReq); err != nil {
		return nil, nil, err
	}
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
		Chat:      &ccReq,
		Model:     "cursor-sonnet",
		Stream:    true,
		StartTime: time.Now(),
		Protocol:  cursorProtocolChat,
		Identity:  cursorFullChainIdentity,
		Plan:      plan,
	})
	return w, result, err
}

// onlySession 返回当前唯一会话（测试断言用；多于/少于一个即失败）。
func onlySession(t *testing.T, svc *CursorExecutionService) *cursorSessionRecord {
	t.Helper()
	svc.sessions.mu.Lock()
	defer svc.sessions.mu.Unlock()
	require.Len(t, svc.sessions.sessions, 1, "本次链路只应存在一个会话")
	for _, record := range svc.sessions.sessions {
		return record
	}
	return nil
}

// waitForSingleSession 在并发场景下等待会话出现（避免与 Execute 内部登记竞争）。
func waitForSingleSession(t *testing.T, svc *CursorExecutionService) *cursorSessionRecord {
	t.Helper()
	var record *cursorSessionRecord
	require.Eventually(t, func() bool {
		svc.sessions.mu.Lock()
		defer svc.sessions.mu.Unlock()
		for _, r := range svc.sessions.sessions {
			record = r
			return true
		}
		return false
	}, 5*time.Second, 5*time.Millisecond, "会话必须在发起 AgentRun 时登记")
	require.NotNil(t, record)
	return record
}

// resumeSession 走真实续轮入口：探针 → 统一索引定位 → 整批接管 + 摘要 + 串行写入 + drain。
func resumeSession(
	t *testing.T, svc *CursorExecutionService, body string,
) (*httptest.ResponseRecorder, *ForwardResult, error) {
	t.Helper()
	probe, err := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(body))
	require.NoError(t, err)
	ticket, err := svc.IdentifyResume(probe)
	require.NoError(t, err)
	require.NotNil(t, ticket, "续轮必须定位到唯一待完成批")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	result, serveErr := svc.ResumeCursorSession(context.Background(), c, ticket, cursorFullChainAccount(),
		CursorResumeServeInput{Body: []byte(body), Stream: true, Identity: cursorFullChainIdentity})
	return w, result, serveErr
}

// ===========================================================================
// 1. 单工具两 HTTP 全链（含唯一结算）
// ===========================================================================

func TestCursorCrossHTTPFullChainSingleTool(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 101, IDField: []byte("exec-101")}}
	upstream := newCursorFullChainUpstream(cursorFullChainScript{
		calls:     calls,
		afterText: "Hangzhou is sunny, 25C",
		usage:     [4]uint64{12601, 15, 128, 0},
	})
	svc := newCursorFullChainService(t, upstream)

	userSlot := newCursorTestResource("user_slot")
	accountSlot := newCursorTestResource("account_slot")
	settlement := &cursorFullChainSettlement{}
	handover := &cursorTestHandover{
		resources:  []CursorTransferableResource{userSlot, accountSlot},
		settlement: settlement.hook,
	}

	// ---- HTTP1：交付工具批 → waiting_tools 且跨 HTTP 存活 ----
	w1, result1, err1 := runFirstHTTP(t.Context(), svc, handover)
	require.NoError(t, err1, "首 HTTP 交付工具批后必须正常返回（不 finalize）")
	require.NotNil(t, result1)

	out1 := w1.Body.String()
	require.Contains(t, out1, `"tool_calls"`, "首 HTTP 必须把工具交接吐给客户端")
	require.Contains(t, out1, `"call_1"`)
	require.Contains(t, out1, `"finish_reason":"tool_calls"`)
	require.Contains(t, out1, cursorSSEDoneSentinel)
	require.NotContains(t, out1, "Hangzhou is sunny", "首 HTTP 不得包含续轮才产生的文本")

	record := onlySession(t, svc)
	record.mu.Lock()
	state1 := record.state
	hasIntake := record.intake != nil
	record.mu.Unlock()
	require.Equal(t, cursorSessionWaitingTools, state1, "交付批后会话必须进入 waiting_tools")
	require.True(t, hasIntake, "交付批时必须装配 CursorSessionIntake 生产实现")

	calls0, complete0 := settlement.snapshot()
	require.Zero(t, calls0, "首 HTTP（无真实 usage）绝不能结算")
	require.Zero(t, complete0)
	require.EqualValues(t, 0, userSlot.releases.Load(), "资源跨 HTTP 保留在会话里")
	require.EqualValues(t, 0, accountSlot.releases.Load())

	// 旧链反证：此刻上游只收到初始 AgentRun 请求，**没有任何** McpResult 回包。
	// 上游读取 goroutine 与 HTTP 返回并发，故用 Eventually 等待其解析完首帧（避免负载下误判）。
	require.Eventually(t, func() bool { return upstream.clientFirstField() == cursorFldAgentRunRequest },
		5*time.Second, 5*time.Millisecond, "首帧必须是 AgentRun 请求")
	require.Empty(t, upstream.writebackPayloads(), "mcp_args 委托阶段不得回任何 McpResult（旧链已斩）")

	// ---- HTTP2：续轮写回真实结果 → 模型续跑 → turn_ended → 唯一结算 ----
	w2, result2, err2 := resumeSession(t, svc, cursorFullChainResumeBody(calls, map[string]string{
		"call_1": "sunny, 25C",
	}))
	require.NoError(t, err2, "续轮必须成功：通道已装配且结果整批合法")
	require.NotNil(t, result2)
	require.Equal(t, 1, upstream.runCount(), "续轮不得新建 AgentRun（同一原生连接）")

	// 真实回写的字节形状：四层编码 + 原 exec 关联。
	writebacks := upstream.writebackPayloads()
	require.Len(t, writebacks, 1, "单工具必须恰好写回一条真实结果")
	decoded := decodeWritebackExec(t, writebacks[0])
	require.False(t, decoded.isError, "客户端成功结果不得写成错误")
	require.Equal(t, []string{"sunny, 25C"}, decoded.texts)
	require.EqualValues(t, 101, decoded.execID, "必须写回原 exec_id")
	require.Equal(t, []byte("exec-101"), decoded.idField, "必须原样带回原 f15 id")

	// 续轮 HTTP 只输出"自本次附着以来"的新内容。
	out2 := w2.Body.String()
	require.Contains(t, out2, "Hangzhou is sunny, 25C", "续轮必须收到模型续跑的真实文本")
	require.NotContains(t, out2, `"call_1"`, "已交付的 tool_calls 不得在续轮重播（不重放）")

	// 唯一结算：turn_ended 真实 usage → noteTurnUsage → finalizer 只结算一次。
	calls1, complete1 := settlement.snapshot()
	require.Equal(t, 1, calls1, "整条链路只允许一次结算事件")
	require.Equal(t, 1, complete1, "必须带完整真实 usage 结算一次")

	settlement.mu.Lock()
	settled := settlement.events[0]
	settlement.mu.Unlock()
	require.EqualValues(t, 12601, settled.Result.Usage.InputTokens)
	require.EqualValues(t, 15, settled.Result.Usage.OutputTokens)
	require.EqualValues(t, 128, settled.Result.Usage.CacheReadInputTokens)
	require.Equal(t, record.BillingEventID(), settled.BillingEventID, "结算必须使用会话级不可变账单事件 ID")

	require.EqualValues(t, 1, userSlot.releases.Load(), "finalizer 释放用户槽一次")
	require.EqualValues(t, 1, accountSlot.releases.Load(), "finalizer 释放账号槽一次")

	// pump 在投递终态事件后才执行 finalizer 的收尾（结算/释放/清索引），二者并发：
	// 用 Eventually 等待唯一 finalizer 走完，避免负载下把"正在收尾"误判成"未终结"。
	require.Eventually(t, func() bool {
		record.mu.Lock()
		defer record.mu.Unlock()
		return record.state == cursorSessionClosed
	}, 5*time.Second, 5*time.Millisecond, "续轮真实终态后会话必须被唯一 finalizer 收口")
	require.Eventually(t, func() bool { return svc.sessions.indexCount() == 0 },
		5*time.Second, 5*time.Millisecond, "finalizer 必须清理全部索引关联")
}

// ===========================================================================
// 2. 同名并行两工具两 HTTP 全链
// ===========================================================================

func TestCursorCrossHTTPFullChainParallelSameName(t *testing.T) {
	calls := []cursorFullChainCall{
		{CallID: "call_a", Name: "get_weather", City: "hangzhou", ExecID: 201, IDField: []byte("exec-201")},
		{CallID: "call_b", Name: "get_weather", City: "shanghai", ExecID: 202, IDField: []byte("exec-202")},
	}
	upstream := newCursorFullChainUpstream(cursorFullChainScript{
		calls:     calls,
		afterText: "Hangzhou sunny, Shanghai rainy",
		usage:     [4]uint64{900, 40, 12, 0},
	})
	svc := newCursorFullChainService(t, upstream)

	settlement := &cursorFullChainSettlement{}
	handover := &cursorTestHandover{
		resources:  []CursorTransferableResource{newCursorTestResource("user_slot")},
		settlement: settlement.hook,
	}

	w1, _, err1 := runFirstHTTP(t.Context(), svc, handover)
	require.NoError(t, err1)
	out1 := w1.Body.String()
	require.Contains(t, out1, `"call_a"`, "同名并行两工具必须都交付给客户端")
	require.Contains(t, out1, `"call_b"`)
	require.Contains(t, out1, `"finish_reason":"tool_calls"`)

	record := onlySession(t, svc)
	record.mu.Lock()
	state1 := record.state
	record.mu.Unlock()
	require.Equal(t, cursorSessionWaitingTools, state1, "整批（listed=2）等齐后才进入 waiting_tools")
	require.Empty(t, upstream.writebackPayloads())

	w2, _, err2 := resumeSession(t, svc, cursorFullChainResumeBody(calls, map[string]string{
		"call_a": "hangzhou sunny",
		"call_b": "shanghai rainy",
	}))
	require.NoError(t, err2)

	// 两条真实结果各自成帧、各自带原 exec 关联（同名不按名字猜配对）。
	writebacks := upstream.writebackPayloads()
	require.Len(t, writebacks, 2, "同名并行两工具必须各写回一条结果")
	first := decodeWritebackExec(t, writebacks[0])
	second := decodeWritebackExec(t, writebacks[1])
	require.EqualValues(t, 201, first.execID)
	require.Equal(t, []byte("exec-201"), first.idField)
	require.Equal(t, []string{"hangzhou sunny"}, first.texts)
	require.EqualValues(t, 202, second.execID)
	require.Equal(t, []byte("exec-202"), second.idField)
	require.Equal(t, []string{"shanghai rainy"}, second.texts)

	require.Contains(t, w2.Body.String(), "Hangzhou sunny, Shanghai rainy")

	calls0, complete0 := settlement.snapshot()
	require.Equal(t, 1, calls0, "并行两工具也只允许一次结算")
	require.Equal(t, 1, complete0)
}

// ===========================================================================
// 3. 工具失败文案逐字回写（不得被本层改写成"成功以外"的内容）
// ===========================================================================

func TestCursorCrossHTTPFullChainToolFailureText(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_err", Name: "get_weather", City: "hangzhou", ExecID: 301, IDField: []byte("exec-301")}}
	upstream := newCursorFullChainUpstream(cursorFullChainScript{
		calls:     calls,
		afterText: "工具失败，改为文字说明",
		usage:     [4]uint64{500, 20, 0, 0},
	})
	svc := newCursorFullChainService(t, upstream)
	settlement := &cursorFullChainSettlement{}
	handover := &cursorTestHandover{settlement: settlement.hook}

	_, _, err1 := runFirstHTTP(t.Context(), svc, handover)
	require.NoError(t, err1)

	_, _, err2 := resumeSession(t, svc, cursorFullChainResumeBody(calls, map[string]string{
		"call_err": "tool failed: timeout",
	}))
	require.NoError(t, err2)

	writebacks := upstream.writebackPayloads()
	require.Len(t, writebacks, 1)
	decoded := decodeWritebackExec(t, writebacks[0])
	require.Equal(t, []string{"tool failed: timeout"}, decoded.texts, "工具正文必须逐字回写，不得改写")
	require.EqualValues(t, 301, decoded.execID)

	calls0, complete0 := settlement.snapshot()
	require.Equal(t, 1, calls0)
	require.Equal(t, 1, complete0)
}

// ===========================================================================
// 4. 180s 到期终结（用被压缩的时钟走真实期限路径）
// ===========================================================================

func TestCursorCrossHTTPFullChainDeadlineTerminates(t *testing.T) {
	calls := []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 401, IDField: []byte("exec-401")}}
	upstream := newCursorFullChainUpstream(cursorFullChainScript{
		calls:           calls,
		hangAfterPhase1: true, // 交付批后永不放终态：只能由期限终结
	})
	svc := newCursorFullChainService(t, upstream)
	// 压缩时钟：首次 AgentRun 发起时固定在 600ms 后到期。期限语义不变（仍是"单一
	// 180s 总预算、跨 HTTP 共用、续轮不重置"），只把 180s 换算成可测的时长。
	base := time.Now()
	svc.now = func() time.Time { return base.Add(-cursorAgentRunTotalBudget + 600*time.Millisecond) }

	settlement := &cursorFullChainSettlement{}
	userSlot := newCursorTestResource("user_slot")
	handover := &cursorTestHandover{
		resources:  []CursorTransferableResource{userSlot},
		settlement: settlement.hook,
	}

	_, _, err1 := runFirstHTTP(t.Context(), svc, handover)
	require.NoError(t, err1, "首 HTTP 必须在期限内正常返回（交付工具批）")

	record := onlySession(t, svc)
	require.False(t, record.Deadline().IsZero(), "首次 AgentRun 必须固定期限")

	// 等真实期限到达：读循环的传输被会话 ctx 取消 → finalizer 终结会话。
	require.Eventually(t, func() bool {
		record.mu.Lock()
		defer record.mu.Unlock()
		return record.state == cursorSessionClosed
	}, 10*time.Second, 10*time.Millisecond, "到期必须终结会话（不无限保活）")

	// 未取得真实 usage：不估算、不写零、不结算假成功（只登记中断，不扣费）。
	calls0, complete0 := settlement.snapshot()
	require.Zero(t, complete0, "未收到 turn_ended 绝不能结算（不伪造 usage）")
	require.LessOrEqual(t, calls0, 1, "至多一次中断登记，不重复结算")
	if calls0 == 1 {
		settlement.mu.Lock()
		ev := settlement.events[0]
		settlement.mu.Unlock()
		require.False(t, ev.UsageComplete, "到期终结不得超过一次真实 usage 结算")
		require.NotEmpty(t, ev.UsageMissingReason, "必须如实记录未取得 usage 的原因")
	}
	require.EqualValues(t, 1, userSlot.releases.Load(), "终结时资源必须释放一次")
	require.Zero(t, svc.sessions.indexCount(), "终结后索引清零")
	require.Empty(t, upstream.writebackPayloads(), "期间没有任何真实结果被写回（零重放）")

	// 终结后不得再续轮（无重连重放路径）：统一索引已清空，这批 ID 不再命中任何会话，
	// 探针拿到可区分的「未知关联」哨兵（A08 失败关闭语义），绝不会被猜成"最近会话"而重放。
	probe, perr := CursorProbeResumeRequest(CursorResumeProtocolChat,
		[]byte(cursorFullChainResumeBody(calls, map[string]string{"call_1": "sunny"})))
	require.NoError(t, perr)
	ticket, terr := svc.IdentifyResume(probe)
	require.ErrorIs(t, terr, ErrCursorResumeUnknownAssociation,
		"终止态后提交的结果 ID 全部未登记 → 必须返回可区分的未知关联哨兵")
	require.Nil(t, ticket, "会话终结后同一批结果不得再定位到续轮目标（零重放）")
	require.Equal(t, 1, upstream.runCount(), "终结后不得出现第二次 AgentRun（无重连）")
}

// ===========================================================================
// 5. 附着期间断连终结（一个会话只终结一次）
// ===========================================================================

func TestCursorCrossHTTPFullChainAttachDisconnectTerminates(t *testing.T) {
	upstream := newCursorFullChainUpstream(cursorFullChainScript{
		calls:           []cursorFullChainCall{{CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 501}},
		hangAfterPhase1: true, // 吐文本后挂着：首 HTTP 保持附着，直到客户端断连
		textOnly:        true,
	})
	svc := newCursorFullChainService(t, upstream)

	settlement := &cursorFullChainSettlement{}
	userSlot := newCursorTestResource("user_slot")
	handover := &cursorTestHandover{
		resources:  []CursorTransferableResource{userSlot},
		settlement: settlement.hook,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := runFirstHTTP(ctx, svc, handover)
		done <- err
	}()

	// 等到本 HTTP 确实附着在会话上（pump 在读、会话已登记），再模拟客户端断连。
	record := waitForSingleSession(t, svc)
	require.Eventually(t, func() bool {
		record.mu.Lock()
		defer record.mu.Unlock()
		return record.attached && record.output != nil
	}, 5*time.Second, 5*time.Millisecond, "首 HTTP 必须已附着在会话上")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("断连后 Execute 必须返回")
	}

	// 附着期间的真实断连 → 唯一 finalizer 终结会话（只终结一次）。
	require.Eventually(t, func() bool {
		record.mu.Lock()
		defer record.mu.Unlock()
		return record.state == cursorSessionClosed
	}, 5*time.Second, 10*time.Millisecond, "附着期间断连必须终结会话")

	// 断连终结不产生任何结算（无真实 usage），也不得出现重复结算。
	calls0, complete0 := settlement.snapshot()
	require.Zero(t, complete0, "断连不产生假成功结算")
	require.LessOrEqual(t, calls0, 1)
	require.EqualValues(t, 1, userSlot.releases.Load(), "终结时资源只释放一次")
	require.Zero(t, svc.sessions.indexCount())
	require.Zero(t, upstream.runCount()-1, "断连后不得重连重放（只有首 HTTP 一次 AgentRun）")
	require.Empty(t, upstream.writebackPayloads(), "断连后不得写回任何结果（零重放）")
	_ = atomic.Int32{}
}

// ===========================================================================
// 6. §25.17.3「A05续轮顺序」（S2b-2）：两个 ServeCursorResume 入口接资格核验
// ===========================================================================
//
// 本段只钉接线语义，不重测资格判定本身（判定由 verifyResumeEligibility 的定向测试覆盖）：
//
//	① 两个生产入口（GatewayService / OpenAIGatewayService 的 ServeCursorResume）都在
//	   "定位到已登记会话 + 取回本次读取的原账号"之后、写工具结果之前做同一处核验；
//	② 拒绝按类返回（403/400/503），**零工具结果写入**，旧会话沿**原 finalizer** 终结
//	   （唯一结算规则、槽位释放与索引清理都来自 finishCursorSession，无平行收尾）；
//	③ 并发 1 且旧会话仍占槽的合法续轮必须成功，且**零新 Acquire**（不换号、不重建
//	   AgentRun、不重计费）。

// cursorResumeGateAccountRepo 是两入口"本次读取的原账号"的唯一替身：只覆盖 GetByID，
// 其余方法沿用接口（嵌入即满足 AccountRepository，绝不另造第二套账号读取链）。
type cursorResumeGateAccountRepo struct {
	AccountRepository
	account *Account
}

func (r *cursorResumeGateAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	if r.account == nil {
		return nil, errors.New("account not found")
	}
	return r.account, nil
}

// cursorResumeGateEligibleAccount 构造"本次读取"的原账号：cursor 平台、可用、并发 1、
// 仍挂在会话身份所属分组、绑定来源 7 且模型映射未变（资格应通过）。
func cursorResumeGateEligibleAccount() *Account {
	account := cursorFullChainAccount()
	account.Status = StatusActive
	account.Schedulable = true
	account.Concurrency = 1
	account.AccountGroups = []AccountGroup{{
		AccountID: account.ID,
		GroupID:   cursorFullChainIdentity.GroupID,
	}}
	account.GroupIDs = []int64{cursorFullChainIdentity.GroupID}
	return account
}

// installCursorResumeGateSource 安装来源读取器：状态 active + 新鲜额度快照，A04 共同
// 来源门放行（读取器与生产同一窄接口，不另加放行条件）。
func installCursorResumeGateSource(t *testing.T) {
	t.Helper()
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{7: domain.QuotaKeyStatusActive},
		snapshots: map[int64][]byte{7: []byte(fmt.Sprintf(
			`{"total":400,"used":86,"status_code":200,"checked_at":%q,"error_code":""}`,
			time.Now().Add(-time.Minute).Format(time.RFC3339),
		))},
	})
}

// cursorResumeGateEntryNames 是本段必须逐一遍历的两个生产入口。
var cursorResumeGateEntryNames = []string{"gateway_service", "openai_gateway_service"}

// cursorResumeGateServe 返回指定生产入口的 ServeCursorResume（同一执行部件 + 同一份
// "本次读取"账号），用于证明两个入口都接了同一处核验。
func cursorResumeGateServe(
	t *testing.T, name string, svc *CursorExecutionService, repo AccountRepository,
) func(context.Context, *gin.Context, *CursorResumeTicket, CursorResumeServeInput) (*ForwardResult, error) {
	t.Helper()
	switch name {
	case "gateway_service":
		return (&GatewayService{cursorExecution: svc, accountRepo: repo}).ServeCursorResume
	case "openai_gateway_service":
		return (&OpenAIGatewayService{cursorExecution: svc, accountRepo: repo}).ServeCursorResume
	default:
		t.Fatalf("unknown resume entry %q", name)
		return nil
	}
}

// cursorResumeGateFixture 是一次"首 HTTP 已交付工具批、会话跨 HTTP 存活"的完整夹具。
type cursorResumeGateFixture struct {
	svc         *CursorExecutionService
	quotaKeys   *identityCursorQuotaKeys
	upstream    *cursorFullChainUpstream
	record      *cursorSessionRecord
	settlement  *cursorFullChainSettlement
	userSlot    *cursorTestResource
	accountSlot *cursorTestResource
	body        string
	probe       *CursorResumeProbe
}

// newCursorResumeGateFixture 走真实首 HTTP（真实 Execute + 假上游 + 真实原生双工）。
func newCursorResumeGateFixture(t *testing.T) *cursorResumeGateFixture {
	t.Helper()
	installCursorResumeGateSource(t)

	calls := []cursorFullChainCall{{
		CallID: "call_1", Name: "get_weather", City: "hangzhou", ExecID: 101, IDField: []byte("exec-101"),
	}}
	upstream := newCursorFullChainUpstream(cursorFullChainScript{
		calls:     calls,
		afterText: "Hangzhou is sunny, 25C",
		usage:     [4]uint64{12601, 15, 128, 0},
	})
	svc := newCursorFullChainService(t, upstream)
	quotaKeys, ok := svc.quotaKeys.(*identityCursorQuotaKeys)
	require.True(t, ok, "夹具必须能触达来源行（换码用例需要）")

	settlement := &cursorFullChainSettlement{}
	userSlot := newCursorTestResource("user_slot")
	accountSlot := newCursorTestResource("account_slot")
	handover := &cursorTestHandover{
		resources:  []CursorTransferableResource{userSlot, accountSlot},
		settlement: settlement.hook,
	}

	_, _, err := runFirstHTTP(t.Context(), svc, handover)
	require.NoError(t, err, "首 HTTP 交付工具批后必须正常返回（不 finalize）")

	record := onlySession(t, svc)
	record.mu.Lock()
	state := record.state
	record.mu.Unlock()
	require.Equal(t, cursorSessionWaitingTools, state, "交付批后会话必须进入 waiting_tools 并跨 HTTP 存活")
	require.Eventually(t, func() bool { return upstream.clientFirstField() == cursorFldAgentRunRequest },
		5*time.Second, 5*time.Millisecond, "首帧必须是 AgentRun 请求")
	require.Empty(t, upstream.writebackPayloads(), "首 HTTP 阶段不得有任何结果写入")
	require.EqualValues(t, 1, userSlot.transfers.Load(), "首 HTTP 把用户槽转交给会话一次")
	require.EqualValues(t, 1, accountSlot.transfers.Load(), "首 HTTP 把账号槽转交给会话一次")

	body := cursorFullChainResumeBody(calls, map[string]string{"call_1": "sunny, 25C"})
	probe, probeErr := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(body))
	require.NoError(t, probeErr)

	return &cursorResumeGateFixture{
		svc:         svc,
		quotaKeys:   quotaKeys,
		upstream:    upstream,
		record:      record,
		settlement:  settlement,
		userSlot:    userSlot,
		accountSlot: accountSlot,
		body:        body,
		probe:       probe,
	}
}

// ticket 用真实探针定位唯一待完成批（两入口的调用前提）。
func (f *cursorResumeGateFixture) ticket(t *testing.T) *CursorResumeTicket {
	t.Helper()
	ticket, err := f.svc.IdentifyResume(f.probe)
	require.NoError(t, err)
	require.NotNil(t, ticket, "续轮必须定位到唯一待完成批")
	return ticket
}

// serve 经指定生产入口发起续轮（入口自己读账号 → 资格核验 → 唯一执行入口）。
func (f *cursorResumeGateFixture) serve(
	t *testing.T, entry string, account *Account,
) (*httptest.ResponseRecorder, *ForwardResult, error) {
	t.Helper()
	serve := cursorResumeGateServe(t, entry, f.svc, &cursorResumeGateAccountRepo{account: account})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	result, err := serve(context.Background(), c, f.ticket(t), CursorResumeServeInput{
		Body:     []byte(f.body),
		Stream:   true,
		Identity: cursorFullChainIdentity,
	})
	return w, result, err
}

// requireCursorResumeGateDenied 断言一次拒绝的完整语义：按类返回、零工具结果写入、
// 不换号不重建 AgentRun、不新占槽，旧会话沿原 finalizer 终结。
func requireCursorResumeGateDenied(
	t *testing.T, f *cursorResumeGateFixture, w *httptest.ResponseRecorder,
	result *ForwardResult, serveErr error, wantStatus int, wantReason string, wantSentinel error,
) {
	t.Helper()
	require.Error(t, serveErr)
	require.Nil(t, result, "拒绝不得返回成功结果")
	status, reason := CursorResumeHTTPStatus(serveErr)
	require.Equal(t, wantStatus, status, "拒绝必须按类返回既有状态码")
	require.Equal(t, wantReason, reason, "拒绝原因串必须是稳定枚举")
	require.ErrorIs(t, serveErr, wantSentinel)
	require.Zero(t, w.Body.Len(), "服务入口不写响应体（由 handler 按状态码拒付）")

	// 零工具结果写入：上游一条 McpResult 都没收到，也没有新建 AgentRun。
	require.Empty(t, f.upstream.writebackPayloads(), "拒绝后必须零工具结果写入")
	require.Equal(t, 1, f.upstream.runCount(), "拒绝不得新建 AgentRun（不换号、不重执行）")

	// 旧会话沿原 finalizer 终结：终态 + 索引清理 + 两槽各释放一次。
	require.Eventually(t, func() bool {
		f.record.mu.Lock()
		defer f.record.mu.Unlock()
		return f.record.state == cursorSessionClosed
	}, 5*time.Second, 5*time.Millisecond, "拒绝后旧会话必须沿原 finalizer 终结")
	require.Eventually(t, func() bool { return f.svc.sessions.indexCount() == 0 },
		5*time.Second, 5*time.Millisecond, "finalizer 必须清理全部索引关联")
	require.EqualValues(t, 1, f.userSlot.releases.Load(), "用户槽释放一次")
	require.EqualValues(t, 1, f.accountSlot.releases.Load(), "账号槽释放一次")
	require.EqualValues(t, 1, f.userSlot.transfers.Load(), "拒绝不得新占/重转交槽位")
	require.EqualValues(t, 1, f.accountSlot.transfers.Load())

	// 唯一结算规则的投影：本会话没有完整真实 usage → 只记录中断原因，不估算扣费。
	calls, complete := f.settlement.snapshot()
	require.Equal(t, 1, calls, "终结必须走唯一结算入口且只走一次")
	require.Zero(t, complete, "无完整真实 usage 时不得计费")
}

// 覆盖 ① ②：disabled 账号续轮 → 403 + 零写入 + 会话清理（两个入口逐一验证）。
func TestCursorResumeGateDeniedDisabledAccountZeroWrite(t *testing.T) {
	for _, entry := range cursorResumeGateEntryNames {
		t.Run(entry, func(t *testing.T) {
			f := newCursorResumeGateFixture(t)
			account := cursorResumeGateEligibleAccount()
			account.Status = StatusDisabled

			w, result, serveErr := f.serve(t, entry, account)
			requireCursorResumeGateDenied(t, f, w, result, serveErr,
				http.StatusForbidden, "not_schedulable", ErrCursorResumeAccountIneligible)

			// 会话已被清理：同一批结果**不得**再被重新定位（不重放、不留墓碑）。
			// A08 失败关闭语义：这批 ID 已全部不在统一索引（含重启后孤儿），探针拿到的是
			// **可区分的「未知关联」哨兵**（不是 (nil, nil)）：helper 只把这一类交回原路由
			// 判定供应方，进入 Cursor 分支则在出站前 400——不猜最近会话、不接管、不重放。
			ticket, identifyErr := f.svc.IdentifyResume(f.probe)
			require.ErrorIs(t, identifyErr, ErrCursorResumeUnknownAssociation,
				"终结后的批必须按未知关联失败关闭，不得静默当作未命中")
			identifyStatus, _ := CursorResumeHTTPStatus(identifyErr)
			require.Equal(t, http.StatusBadRequest, identifyStatus, "未知关联按既有契约 400 拒绝")
			require.Nil(t, ticket, "终结后的批不得再被定位")
			require.Zero(t, f.svc.sessions.indexCount(), "终结后不得残留任何索引关联")
		})
	}
}

// 覆盖 ① ②：换码（来源行 ID 不变、激活码明文已变）→ 400 失效关联 + 零写入。
func TestCursorResumeGateDeniedRotatedSourceCode(t *testing.T) {
	for _, entry := range cursorResumeGateEntryNames {
		t.Run(entry, func(t *testing.T) {
			f := newCursorResumeGateFixture(t)
			// 换码：会话固定的是首次鉴权那一次的来源 MAC，这里把同一行的明文换成另一串。
			f.quotaKeys.byID[7].CodeEncrypted = "ACTIVATION-CODE-ROTATED"

			w, result, serveErr := f.serve(t, entry, cursorResumeGateEligibleAccount())
			requireCursorResumeGateDenied(t, f, w, result, serveErr,
				http.StatusBadRequest, "source_changed", ErrCursorResumeSourceChanged)
		})
	}
}

// 覆盖 ③：并发 1 且旧会话仍占槽的合法续轮必须成功，且零新 Acquire / 零重建 AgentRun /
// 唯一结算一次（核对"资格核验不选号、不排队、不并发 Acquire"）。
func TestCursorResumeGateAllowsEligibleResumeSingleConcurrency(t *testing.T) {
	for _, entry := range cursorResumeGateEntryNames {
		t.Run(entry, func(t *testing.T) {
			f := newCursorResumeGateFixture(t)
			account := cursorResumeGateEligibleAccount()
			require.Equal(t, 1, account.Concurrency, "账号并发上限为 1，旧会话仍占着这个槽")

			w, result, serveErr := f.serve(t, entry, account)
			require.NoError(t, serveErr, "旧会话仍占槽不得否决自己仍占槽的合法续轮")
			require.NotNil(t, result)

			// 真实回写 + 模型续跑的真实文本：链路仍是唯一执行路径。
			require.Len(t, f.upstream.writebackPayloads(), 1, "合法续轮必须恰好写回一条真实结果")
			require.Contains(t, w.Body.String(), "Hangzhou is sunny, 25C", "续轮必须收到模型续跑的真实文本")
			require.Equal(t, 1, f.upstream.runCount(), "续轮不得新建 AgentRun")

			// 零新 Acquire：槽位只在首 HTTP 转交一次，续轮全程不再转交/新占。
			require.EqualValues(t, 1, f.userSlot.transfers.Load(), "续轮零新 Acquire（用户槽）")
			require.EqualValues(t, 1, f.accountSlot.transfers.Load(), "续轮零新 Acquire（账号槽）")

			calls, complete := f.settlement.snapshot()
			require.Equal(t, 1, calls, "整条链路只允许一次结算事件")
			require.Equal(t, 1, complete, "必须带完整真实 usage 结算一次")
			require.Eventually(t, func() bool {
				f.record.mu.Lock()
				defer f.record.mu.Unlock()
				return f.record.state == cursorSessionClosed
			}, 5*time.Second, 5*time.Millisecond, "续轮真实终态后会话必须被唯一 finalizer 收口")
			require.EqualValues(t, 1, f.userSlot.releases.Load(), "finalizer 释放用户槽一次")
			require.EqualValues(t, 1, f.accountSlot.releases.Load(), "finalizer 释放账号槽一次")
		})
	}
}

// 覆盖 ②：拒绝时旧会话**已有完整真实 usage** —— 仍按唯一结算规则结算一次后再终结
// （残留检查：核验接线不得绕开或重复既有结算路径）。
//
// usage 只经会话既有入口 noteTurnUsage 注入（与解析器收到 turn_ended 后走的是同一条
// 记账入口），不新造第二条结算入口。
func TestCursorResumeGateDeniedAfterCompleteUsageSettlesOnce(t *testing.T) {
	for _, entry := range cursorResumeGateEntryNames {
		t.Run(entry, func(t *testing.T) {
			f := newCursorResumeGateFixture(t)
			// 等价于本会话此前已经收到过真实 turn_ended：四个计费字段齐全。
			// 四个计费字段逐一 Seen（R1 Seen 语义）：F1InputSeen..F4CacheWriteSeen
			// 全置位，否则 billingFieldsComplete 判不完整、结算入口不记 complete（红线：
			// proto optional 缺失一律按"无完整 usage"处理，不结算、不写零账假成功）。
			f.record.noteTurnUsage(cursorTurnUsage{
				Seen: true, Input: 12601, Output: 15, CacheRead: 128, EndedAtMS: 1700000000000,
				F1InputSeen: true, F2OutputSeen: true, F3CacheReadSeen: true, F4CacheWriteSeen: true,
			}, "turn_ended")

			account := cursorResumeGateEligibleAccount()
			account.Status = StatusDisabled

			w, result, serveErr := f.serve(t, entry, account)
			require.Error(t, serveErr)
			require.Nil(t, result)
			status, _ := CursorResumeHTTPStatus(serveErr)
			require.Equal(t, http.StatusForbidden, status)
			require.Zero(t, w.Body.Len())
			require.Empty(t, f.upstream.writebackPayloads(), "拒绝后必须零工具结果写入")
			require.Equal(t, 1, f.upstream.runCount(), "拒绝不得新建 AgentRun")

			require.Eventually(t, func() bool {
				f.record.mu.Lock()
				defer f.record.mu.Unlock()
				return f.record.state == cursorSessionClosed
			}, 5*time.Second, 5*time.Millisecond, "结算后必须终结旧会话")
			calls, complete := f.settlement.snapshot()
			require.Equal(t, 1, calls, "完整真实 usage 只允许结算一次")
			require.Equal(t, 1, complete, "完整真实 usage 必须按唯一结算规则结算")
			require.EqualValues(t, 1, f.userSlot.releases.Load(), "用户槽释放一次")
			require.EqualValues(t, 1, f.accountSlot.releases.Load(), "账号槽释放一次")
		})
	}
}
