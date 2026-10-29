//go:build unit

package service

// 本文件是 Cursor 执行部件的**批次封口与会话索引**定向测试（方案 §25.2 / §25.3 /
// §25.14-D，派发单 W2-A1 + W2-A1b）。
//
// 覆盖：
//  1. listed 先于最后一个 started：必须等齐才封口，不按到达顺序假设；
//  2. 同名并行调用分属不同 model_call_id：各自归属自己的批次，不串批；
//  3. 无 ID 的 listed 在多开放批次下无法归属 → 明确协议错误，不按最后一条猜；
//  4. 对外 started 无完整 f11 委托 → 批不完整（明确失败）；
//  5. 重复委托 ID → 冲突拒绝；
//  6. 内部元工具（get_mcp_tools 等）与外部工具分开登记但共同参与完整性判断；
//  7. 会话表 + **唯一**索引实现（CR-B CursorSessionIndex）：关闭会话删除全部关联，Stop 清空；
//  8. tool_requests_listed.call_count 真实接线（W2-A1b）：f27 帧解析 → 归属 → 按数封口，
//     四类定向场景（等齐封口 / count 大于已见 started / 双开放批明确拒绝 / listed 乱序先到），
//     以及"缺 call_count 即失败关闭、不留降级路径"。
//
// 全部是纯内存组件测试：无 DB、无网络、无 DB 事务。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func cursorBatchDelegation(id, name string) cursorToolCallCapture {
	return cursorToolCallCapture{ID: id, Name: name, ArgsJSON: `{"city":"Beijing"}`}
}

// TestCursorBatchSealWaitsForListedTotal 覆盖 §25.3：listed 先于最后 started 时必须等齐。
func TestCursorBatchSealWaitsForListedTotal(t *testing.T) {
	d := newCursorToolDelegations()

	// 本批声明 2 条：listed 先到（真实证据 29：listed 可早于第二个 started）。
	require.NoError(t, d.noteToolRequestsListed("mc-1", 2))
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_a", true))
	require.NoError(t, d.noteDelegation("mc-1", cursorBatchDelegation("call_a", "get_weather")))

	// 只到 1 条：不得封口（不得按首个 mcp_args 或静默时间结束批次）。
	err := d.seal()
	require.ErrorIs(t, err, ErrCursorBatchIncomplete,
		"listed=2 但 started=1 时必须判为批次未集齐")

	// 第二条 started + 委托到达后才集齐。
	d2 := newCursorToolDelegations()
	require.NoError(t, d2.noteToolRequestsListed("mc-1", 2))
	require.NoError(t, d2.noteToolCallStarted("mc-1", "call_a", true))
	require.NoError(t, d2.noteDelegation("mc-1", cursorBatchDelegation("call_a", "get_weather")))
	require.NoError(t, d2.noteToolCallStarted("mc-1", "call_b", true))
	require.NoError(t, d2.noteDelegation("mc-1", cursorBatchDelegation("call_b", "get_weather")))
	require.NoError(t, d2.seal())
	require.Len(t, d2.snapshot(), 2)
}

// TestCursorBatchParallelSameNameDifferentModelCallID 覆盖 §25.3 / 证据 28：
// 同名并行调用分属不同 model_call_id，必须各归各批、互不串批。
func TestCursorBatchParallelSameNameDifferentModelCallID(t *testing.T) {
	d := newCursorToolDelegations()

	// 两批各 1 条同名工具（参数不同，例：北京/上海）。
	require.NoError(t, d.noteToolRequestsListed("mc-A", 1))
	require.NoError(t, d.noteToolRequestsListed("mc-B", 1))
	require.NoError(t, d.noteToolCallStarted("mc-A", "call_A", true))
	require.NoError(t, d.noteToolCallStarted("mc-B", "call_B", true))
	require.NoError(t, d.noteDelegation("mc-A", cursorToolCallCapture{ID: "call_A", Name: "get_weather", ArgsJSON: `{"city":"Beijing"}`}))
	require.NoError(t, d.noteDelegation("mc-B", cursorToolCallCapture{ID: "call_B", Name: "get_weather", ArgsJSON: `{"city":"Shanghai"}`}))

	require.NoError(t, d.seal(), "两个批次各自集齐后必须整体封口成功")
	calls := d.snapshot()
	require.Len(t, calls, 2)

	// 参数必须保持各自的原始 map 解码结果（不串批、不丢精度）。
	require.Equal(t, "call_A", calls[0].ID)
	require.JSONEq(t, `{"city":"Beijing"}`, calls[0].ArgsJSON)
	require.Equal(t, "call_B", calls[1].ID)
	require.JSONEq(t, `{"city":"Shanghai"}`, calls[1].ArgsJSON)
}

// TestCursorBatchAmbiguousListedRejected 覆盖 §25.3：多开放批次下无 ID 的 listed
// 无法归属唯一批次 → 明确协议错误，不按最后一条猜测。
func TestCursorBatchAmbiguousListedRejected(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolCallStarted("mc-A", "call_A", true))
	require.NoError(t, d.noteToolCallStarted("mc-B", "call_B", true))

	err := d.noteToolRequestsListed("", 2)
	require.ErrorIs(t, err, ErrCursorBatchAmbiguous,
		"存在多个开放批次时，无 model_call_id 的 listed 必须明确拒绝")
}

// TestCursorBatchUnattributedListedBindsSingleOpenBatch 覆盖唯一开放批次的合法归属。
func TestCursorBatchUnattributedListedBindsSingleOpenBatch(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolCallStarted("", "call_only", true))
	require.NoError(t, d.noteToolRequestsListed("", 1))
	require.NoError(t, d.noteDelegation("", cursorBatchDelegation("call_only", "get_weather")))
	require.NoError(t, d.seal())
	require.Len(t, d.snapshot(), 1)
}

// TestCursorBatchExternalCallWithoutDelegationFails 覆盖 §25.3：
// 每个对外 started 调用都必须有完整 f11 委托与已解码参数。
func TestCursorBatchExternalCallWithoutDelegationFails(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_missing", true))
	err := d.seal()
	require.ErrorIs(t, err, ErrCursorBatchIncomplete)
	require.Contains(t, err.Error(), "call_missing")
}

// TestCursorBatchInternalMetaToolParticipatesWithoutHandoff 覆盖 §25.3：
// 内部元工具（get_mcp_tools）参与同批完整性判断，但**不交给客户端执行**。
func TestCursorBatchInternalMetaToolParticipatesWithoutHandoff(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolRequestsListed("mc-internal", 1))
	require.NoError(t, d.noteToolCallStarted("mc-internal", "", false)) // 内部元工具
	require.NoError(t, d.seal(), "内部元工具不产生对外委托，也不得因此判为不完整")
	require.Empty(t, d.snapshot(), "元工具绝不能出现在对外工具清单里")
}

// TestCursorBatchDuplicateDelegationRejected 覆盖 §25.2：重复委托 ID 属于冲突。
func TestCursorBatchDuplicateDelegationRejected(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteDelegation("mc-1", cursorBatchDelegation("call_dup", "get_weather")))
	err := d.noteDelegation("mc-1", cursorBatchDelegation("call_dup", "get_weather"))
	require.ErrorIs(t, err, ErrCursorProtocol)
	require.Contains(t, err.Error(), "duplicate tool delegation")
}

// TestCursorBatchSealedBatchNotReused 覆盖 §25.3：上一批封口后，同 model_call_id
// 的新信号必须开新批，不得把旧批的 count 跨代复用（证据 29 的"最近 count"陷阱）。
func TestCursorBatchSealedBatchNotReused(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolRequestsListed("mc-1", 1))
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_1", true))
	require.NoError(t, d.noteDelegation("mc-1", cursorBatchDelegation("call_1", "get_weather")))
	require.NoError(t, d.seal())

	// 同 model_call_id 的第二代：listed 必须重新声明（B2：未收到 listed 不得交接，
	// 否则按 fail-closed 处理）；旧批的 count 绝不跨代复用（证据 29 的"最近 count"陷阱）。
	require.NoError(t, d.noteToolRequestsListed("mc-1", 1))
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_2", true))
	require.NoError(t, d.noteDelegation("mc-1", cursorBatchDelegation("call_2", "get_weather")))
	require.NoError(t, d.seal())
	require.Len(t, d.snapshot(), 2, "两代委托都必须保留（各自封口，不互相覆盖）")
}

// TestCursorSessionStoreCloseClearsIndex 覆盖 §25.2 / §25.14-D：finalizer 删除该会话全部关联。
//
// 断言落在**唯一索引实现**（CR-B CursorSessionIndex）上：本服务不再自建索引表。
func TestCursorSessionStoreCloseClearsIndex(t *testing.T) {
	store := newCursorSessionStore()
	record := store.start(cursorSessionSpec{protocol: cursorProtocolChat, requestedModel: "m"})
	require.Equal(t, 1, store.sessionCount())

	require.NoError(t, store.registerToolCall(record.id, "mc-1", "call_1"))
	require.NoError(t, store.registerToolCall(record.id, "mc-1", "call_2"))
	require.Equal(t, 2, store.indexCount(), "登记必须落到 CR-B 统一索引上")
	require.NotNil(t, store.lookupSession(cursorProtocolChat, CursorIDKindCall, "call_1"))

	// 键含协议：另一协议下的同名 ID 不得串号（键三元组的协议维度生效）。
	require.Nil(t, store.lookupSession(cursorProtocolResponses, CursorIDKindCall, "call_1"))

	// 同一键重复登记：交给 CR-B 判冲突，绝不覆盖既有归属。
	require.ErrorIs(t, store.registerToolCall(record.id, "mc-2", "call_1"), ErrCursorSessionIndexConflict)

	store.close(record.id)
	require.Zero(t, store.sessionCount())
	require.Zero(t, store.indexCount(), "关闭会话必须删除该会话的全部关联（不只当前 pending）")
	require.Nil(t, store.lookupSession(cursorProtocolChat, CursorIDKindCall, "call_1"))
}

// TestCursorSessionStoreStopClearsAll 覆盖 §25.2：Stop 清空全部会话与（统一）索引。
func TestCursorSessionStoreStopClearsAll(t *testing.T) {
	store := newCursorSessionStore()
	a := store.start(cursorSessionSpec{protocol: cursorProtocolResponses})
	b := store.start(cursorSessionSpec{protocol: cursorProtocolAnthropic})
	require.NoError(t, store.registerToolCall(a.id, "mc-a", "call_a"))
	require.NoError(t, store.registerToolCall(b.id, "mc-b", "call_b"))
	require.Equal(t, 2, store.sessionCount())
	require.Equal(t, 2, store.indexCount())

	store.stop()
	require.Zero(t, store.sessionCount())
	require.Zero(t, store.indexCount(), "Stop 必须清空 CR-B 索引的全部条目")

	// §25.17.7 A09：Stop 之后 sessions.start 明确不可用（零 AgentRun），返回 nil，
	// 不落表、不留可续轮状态（已在 finishCursorSession / 兜底全清中移除）。
	late := store.start(cursorSessionSpec{protocol: cursorProtocolChat})
	require.Nil(t, late, "Stop 之后 sessions.start 必须明确不可用（零 AgentRun）")
	require.Zero(t, store.sessionCount())
	// 已收口的会话不得在统一索引里留下游离条目。
	require.Zero(t, store.indexCount())
}

// ---------------------------------------------------------------------------
// 8. tool_requests_listed.call_count 真实接线（W2-A1b）
//
// 字段依据：design-evidence.md（02-native-schemas.json）
// agent.v1.ToolRequestsListedUpdate 内层**只有** no:1 call_count（uint32），无 model_call_id。
// 下列测试一律走**真实帧构造 + 真实解析器**（不直接调用内部方法），确保字段号接线正确。
// ---------------------------------------------------------------------------

// cursorTestToolStartedFrame 构造 interaction_update{2 tool_call_started{15 ToolCall{1 call_id}}} 帧。
func cursorTestToolStartedFrame(t *testing.T, callID string) []byte {
	t.Helper()
	toolCall := cursorAppendStringField(nil, cursorFldToolCallID, callID)
	update := cursorAppendBytesField(nil, cursorFldUpdateToolCallStarted, toolCall)
	return cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update)
}

// cursorTestToolRequestsListedFrame 构造 interaction_update{27 tool_requests_listed{1 call_count}} 帧。
func cursorTestToolRequestsListedFrame(t *testing.T, count uint64) []byte {
	t.Helper()
	listed := cursorAppendVarintField(nil, cursorFldToolRequestsListedCallCount, count)
	update := cursorAppendBytesField(nil, cursorFldUpdateToolRequestsListed, listed)
	return cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update)
}

// cursorTestParseToolStream 用**本地**委托记录解析整段流并返回该记录（唯一解码点在 parser）。
//
// 独立 parser 路径不经由 duplex，必须显式挂载请求 f4 原始字节，否则
// cursorRetypeArgsByToolDefs 取不到 get_weather 的 input_schema → schema_absent 失败关闭。
// 这里复用现有编码器 cursorTestBuildToolDef 构造 get_weather 原始 f4 definition（与生产
// cursorBuildMcpTools 同形），不加新编码、不放宽 schema_absent。
func cursorTestParseToolStream(t *testing.T, body []byte) *cursorToolDelegations {
	t.Helper()
	delegations := newLocalCursorToolDelegations()
	parser := cursorNewStreamParser("m")
	parser.attachToolDelegations(delegations)
	parser.attachToolSchemas([][]byte{
		cursorTestBuildToolDef(t, "get_weather",
			`{"type":"object","properties":{"city":{"type":"string"}}}`),
	})
	_, err := parser.cursorParseStream(body)
	require.NoError(t, err)
	return delegations
}

// TestCursorBatchListedCallCountSealsWhenMatched 覆盖 §25.3（a）：单批 listed 等齐即封口。
//
// 时序取证据 29：started → listed → started（listed 夹在两个 started 之间也算乱序到达）。
func TestCursorBatchListedCallCountSealsWhenMatched(t *testing.T) {
	argsA := cursorToolTestBuildMcpArgs("call_a", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Beijing"}})
	argsB := cursorToolTestBuildMcpArgs("call_b", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Shanghai"}})
	body := cursorToolTestStreamBody(
		cursorTestToolStartedFrame(t, "call_a"),
		cursorToolTestExecFrame(true, argsA),
		cursorTestToolRequestsListedFrame(t, 2), // 本批声明 2 条
		cursorTestToolStartedFrame(t, "call_b"),
		cursorToolTestExecFrame(true, argsB),
		cursorToolTestTurnEndedFrame(10, 5),
	)
	delegations := cursorTestParseToolStream(t, body)

	require.NoError(t, cursorSealSessionBatch(delegations), "listed=2 且 started=2 必须封口")
	calls := delegations.snapshot()
	require.Len(t, calls, 2)
	require.Equal(t, "call_a", calls[0].ID)
	require.JSONEq(t, `{"city":"Beijing"}`, calls[0].ArgsJSON)
	require.Equal(t, "call_b", calls[1].ID)
	require.JSONEq(t, `{"city":"Shanghai"}`, calls[1].ArgsJSON)
}

// TestCursorBatchListedCountAboveStartedStaysOpen 覆盖 §25.3（b）：
// call_count 大于已见 started 时**不满封**（等齐），绝不静默补齐。
func TestCursorBatchListedCountAboveStartedStaysOpen(t *testing.T) {
	argsA := cursorToolTestBuildMcpArgs("call_a", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Beijing"}})
	argsB := cursorToolTestBuildMcpArgs("call_b", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Shanghai"}})
	body := cursorToolTestStreamBody(
		cursorTestToolRequestsListedFrame(t, 3), // 声明 3 条
		cursorTestToolStartedFrame(t, "call_a"),
		cursorToolTestExecFrame(true, argsA),
		cursorTestToolStartedFrame(t, "call_b"),
		cursorToolTestExecFrame(true, argsB),
		cursorToolTestTurnEndedFrame(10, 5),
	)
	delegations := cursorTestParseToolStream(t, body)

	err := cursorSealSessionBatch(delegations)
	require.ErrorIs(t, err, ErrCursorBatchIncomplete, "listed=3 但只到 2 条时必须等齐")
	require.Contains(t, err.Error(), "listed=3")
}

// TestCursorBatchTwoOpenBatchesListedRejectedWithoutWrites 覆盖 §25.3（c）：
// 无 ID 的 listed 在 ≥2 个开放批时**明确协议错误**，且不产生任何对外委托（零上游写入）。
func TestCursorBatchTwoOpenBatchesListedRejectedWithoutWrites(t *testing.T) {
	delegations := newCursorToolDelegations()
	require.NoError(t, delegations.noteToolCallStarted("mc-A", "call_A", true))
	require.NoError(t, delegations.noteToolCallStarted("mc-B", "call_B", true))

	err := delegations.noteToolRequestsListed("", 1)
	require.ErrorIs(t, err, ErrCursorBatchAmbiguous, "多开放批下无 ID listed 必须明确拒绝")
	require.Empty(t, delegations.snapshot(), "归属含糊时绝不把任何委托交给客户端")
}

// TestCursorBatchListedBeforeStartedSealsOutOfOrder 覆盖 §25.3（d）：
// listed 先到、started 后到（证据 29 的真实乱序）仍必须能等齐封口。
func TestCursorBatchListedBeforeStartedSealsOutOfOrder(t *testing.T) {
	args := cursorToolTestBuildMcpArgs("call_only", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Beijing"}})
	body := cursorToolTestStreamBody(
		cursorTestToolRequestsListedFrame(t, 1), // listed 先到
		cursorTestToolStartedFrame(t, "call_only"),
		cursorToolTestExecFrame(true, args),
		cursorToolTestTurnEndedFrame(10, 5),
	)
	delegations := cursorTestParseToolStream(t, body)

	require.NoError(t, cursorSealSessionBatch(delegations), "listed 先到也必须等齐后封口")
	require.Len(t, delegations.snapshot(), 1)
}

// TestCursorBatchListedWithoutCallCountFailsClosed 覆盖 §25.3 + W2-A1b 纪律：
// f27 缺 call_count 即明确协议错误（失败关闭），不退回"只登记出现"的降级路径。
func TestCursorBatchListedWithoutCallCountFailsClosed(t *testing.T) {
	emptyListed := cursorAppendBytesField(nil, cursorFldUpdateToolRequestsListed, nil)
	body := cursorToolTestStreamBody(
		cursorTestToolStartedFrame(t, "call_a"),
		cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, emptyListed),
		cursorToolTestTurnEndedFrame(10, 5),
	)
	parser := cursorNewStreamParser("m")
	_, err := parser.cursorParseStream(body)
	require.ErrorIs(t, err, ErrCursorProtocol, "缺 call_count 必须失败关闭")
	require.Contains(t, err.Error(), "call_count")
}

// ---------------------------------------------------------------------------
// RL-STOP：cursor 上游限流/额度耗尽 → 账号停止调度（schedulable=false）+ 本请求换号
//
// cursor 账号是"用完即丢"的一次性额度：上游一旦返回限流/额度耗尽（EndStream code
// ERROR_PRO_USER_RATE_LIMIT_EXCEEDED），该账号必须立刻停止调度，不能靠重试自愈；同一请求
// 应换到别的账号继续，全部耗尽才把 429 回给客户端。下游 handler 按 *UpstreamFailoverError
// 的 ShouldRetryNextAccount() 决定换号还是耗尽。
//
// 复用的测试替身（同包 unit 构建）：cursorOutboundUpstream / newCursorOutboundTestService /
// cursorOutboundAccount / executeCursorForTest / cursorTestDeviceIDs（cursor_gateway_forward_test.go）、
// cursorTextDeltaFrame / cursorTestEndStream（cursor_protocol_response_test.go）。
// ---------------------------------------------------------------------------

const (
	cursorRLStopRateLimitCode = "ERROR_PRO_USER_RATE_LIMIT_EXCEEDED"
	cursorRLStopOtherCode     = "ERROR_INTERNAL"
)

// cursorRLStopEndStreamPayload 构造 EndStream 的错误载荷（复刻 RL-CODE 派发单的 code 位置：
// details[].debug.error）。
func cursorRLStopEndStreamPayload(code string) string {
	return `{"error":{"code":"unauthenticated","message":"rate limited",` +
		`"details":[{"type":"aiserver.v1.ErrorDetails","debug":{"error":"` + code + `",` +
		`"details":{"title":"","detail":"rate limited"}}}]}}`
}

// cursorRLStopStreamBody 构造一条以 EndStream 错误收尾的 Cursor 响应流。
//
// withBody=true 时先发一帧文本正文（pump 首次 publish 正文即置位 bodyPublished），
// 用于验证"已发布字节禁止 mid-stream 换号"。
func cursorRLStopStreamBody(t *testing.T, code string, withBody bool) []byte {
	t.Helper()
	var out []byte
	if withBody {
		out = append(out, cursorTextDeltaFrame(t, "Hi")...)
	}
	out = append(out, cursorTestEndStream(t, cursorRLStopEndStreamPayload(code))...)
	return out
}

// cursorRLStopHandlerCall 记录停号回调的调用次数与最近一次 accountID（并发安全）。
type cursorRLStopHandlerCall struct {
	mu        sync.Mutex
	calls     int
	accountID int64
	reason    string
}

func (c *cursorRLStopHandlerCall) fn(ctx context.Context, accountID int64, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.accountID = accountID
	c.reason = reason
}

func (c *cursorRLStopHandlerCall) snapshot() (int, int64, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.accountID, c.reason
}

// newCursorRLStopService 装配指向假中转、并带一个 cursor 账号的执行部件。
func newCursorRLStopService(t *testing.T, upstreamBody []byte) (*OpenAIGatewayService, *gin.Context) {
	t.Helper()
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: upstreamBody}
	return newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))
}

func cursorRLStopRequestBody(stream bool) []byte {
	return []byte(`{"model":"cursor-sonnet","stream":` + boolToJSON(stream) +
		`,"messages":[{"role":"user","content":"hi"}]}`)
}

func boolToJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestCursorExecuteRateLimitNoBodySwitchesAccount 覆盖 RL-STOP ①：EndStream rate-limit 错误 +
// 未发布字节 → Execute 返回 *UpstreamFailoverError（429 / Reason / ClientStatusCode 固定值 /
// ShouldRetryNextAccount()=true），停号回调恰好被调用 1 次且 accountID 正确。
func TestCursorExecuteRateLimitNoBodySwitchesAccount(t *testing.T) {
	svc, c := newCursorRLStopService(t, cursorRLStopStreamBody(t, cursorRLStopRateLimitCode, false))

	var calls cursorRLStopHandlerCall
	svc.cursorExecution.SetAccountExhaustedHandler(calls.fn)

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	res, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(true), "cursor-sonnet", true, time.Now())
	require.Error(t, err)
	require.Nil(t, res, "限流未发布字节不得返回可计费结果")

	var fe *UpstreamFailoverError
	require.ErrorAs(t, err, &fe, "限流未发布字节必须替换为可换号 failover")
	require.Equal(t, 429, fe.StatusCode)
	require.Equal(t, GatewayFailureReason("cursor_quota_exhausted"), fe.Reason)
	require.Equal(t, 429, fe.ClientStatusCode)
	require.Equal(t, GatewayFailureScopeAccount, fe.Scope)
	require.True(t, fe.ShouldRetryNextAccount(), "未耗尽必须允许换号（NextAccountRetry）")

	n, aid, reason := calls.snapshot()
	require.Equal(t, 1, n, "停号回调必须恰好调用 1 次")
	require.Equal(t, int64(1), aid, "回调 accountID 必须等于本请求账号")
	require.Equal(t, "cursor upstream quota exhausted (rate limited)", reason)
}

// TestCursorExecuteRateLimitWithBodyKeepsOriginalError 覆盖 RL-STOP ②：同一限流错误但已发布
// 字节 → 停号回调仍调用 1 次，返回原 ErrCursorUpstreamStream（errors.As 不命中 failover，
// 即禁止 mid-stream 换号）。
func TestCursorExecuteRateLimitWithBodyKeepsOriginalError(t *testing.T) {
	svc, c := newCursorRLStopService(t, cursorRLStopStreamBody(t, cursorRLStopRateLimitCode, true))

	var calls cursorRLStopHandlerCall
	svc.cursorExecution.SetAccountExhaustedHandler(calls.fn)

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	res, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(true), "cursor-sonnet", true, time.Now())
	require.Error(t, err)
	require.Nil(t, res)

	require.ErrorIs(t, err, ErrCursorUpstreamStream, "已发布字节必须原样返回上游错误")
	var fe *UpstreamFailoverError
	require.False(t, errors.As(err, &fe), "已发布字节不得替换成 failover（禁止 mid-stream 换号）")

	n, _, _ := calls.snapshot()
	require.Equal(t, 1, n, "已发布字节时停号回调仍须调用 1 次")
}

// TestCursorExecuteOtherErrorCodeDoesNotSwitchAccount 覆盖 RL-STOP ③：其它 code（ERROR_INTERNAL）
// → 回调不调用、错误原样（不替换成 failover、仍可被 ErrCursorUpstreamStream 识别）。
func TestCursorExecuteOtherErrorCodeDoesNotSwitchAccount(t *testing.T) {
	svc, c := newCursorRLStopService(t, cursorRLStopStreamBody(t, cursorRLStopOtherCode, false))

	var calls cursorRLStopHandlerCall
	svc.cursorExecution.SetAccountExhaustedHandler(calls.fn)

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	res, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(true), "cursor-sonnet", true, time.Now())
	require.Error(t, err)
	require.Nil(t, res)

	n, _, _ := calls.snapshot()
	require.Zero(t, n, "非限流 code 不得调用停号回调")

	var fe *UpstreamFailoverError
	require.False(t, errors.As(err, &fe), "非限流 code 不得替换成 failover")
	require.ErrorIs(t, err, ErrCursorUpstreamStream, "非限流 code 原样返回上游错误")
}

// TestCursorExecuteRateLimitNilHandlerNoPanic 覆盖 RL-STOP ④：未注入回调（nil）→ 不 panic、
// 行为同①的错误替换（仍返回 *UpstreamFailoverError）。
func TestCursorExecuteRateLimitNilHandlerNoPanic(t *testing.T) {
	svc, c := newCursorRLStopService(t, cursorRLStopStreamBody(t, cursorRLStopRateLimitCode, false))
	// 不注入回调：保持 nil，验证安全 no-op。

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	res, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(true), "cursor-sonnet", true, time.Now())
	require.Error(t, err)
	require.Nil(t, res)

	var fe *UpstreamFailoverError
	require.ErrorAs(t, err, &fe, "未注入回调时仍须替换为可换号 failover")
	require.Equal(t, 429, fe.StatusCode)
	require.Equal(t, GatewayFailureReason("cursor_quota_exhausted"), fe.Reason)
}
