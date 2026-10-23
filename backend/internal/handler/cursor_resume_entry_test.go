//go:build unit

package handler

// 本文件是 **§25.2 续轮识别在资源获取之前** 的 handler 侧定向测试（派发单 W2-E）。
//
// 覆盖：
//  1. 普通请求（无结果 ID、无 previous_response_id）不被误判为续轮，直接放行普通路径；
//  2. 续轮先定位再接管：识别失败（混批/旧批/身份/通道等**其它**识别错误）在任何资源获取
//     之前明确拒绝；
//  3. 合法续轮由会话接管：ServeCursorResume 被调用一次，handler 直接返回；
//  4. 回写通道缺失：明确 503，绝不伪造成功；
//  5. 协议分流：Chat/Messages/Responses/Gemini 各走自己的探针与状态码。
//
// §25.17.4（A08）追加：**未知关联只在 Cursor 边界拒绝**——
//  6. 明确声称续轮但全部 ID 未命中（ErrCursorResumeUnknownAssociation）不得在选号前判死，
//     交原路由按供应方判定：非 Cursor 原样转发、不改请求体、不接管；
//  7. 该情形下本 HTTP 已获取的资源不被转交，仍由原 HTTP defer 释放一次；
//  8. 进入 Cursor 执行分支时由 Cursor 准入断言（执行服务侧）在此前返回 400 失效关联，
//     上游调用 0；
//  9. 其它识别错误一律不吞（各自既有状态码），也不被误判成"未知关联"这一类。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cursorResumeFakeGateway 是 CursorResumeGateway 的替身（记录调用与返回预设）。
type cursorResumeFakeGateway struct {
	identifyCalls int
	serveCalls    int
	ticket        *service.CursorResumeTicket
	identifyErr   error
	serveErr      error
	// probes 记录每次识别收到的类型化探针（用于证明"本次确实明确声称续轮"）。
	probes []*service.CursorResumeProbe
}

// IdentifyCursorResume 实现 service.CursorResumeGateway。
func (g *cursorResumeFakeGateway) IdentifyCursorResume(probe *service.CursorResumeProbe) (*service.CursorResumeTicket, error) {
	g.identifyCalls++
	g.probes = append(g.probes, probe)
	if g.identifyErr != nil {
		return nil, g.identifyErr
	}
	return g.ticket, nil
}

// ServeCursorResume 实现 service.CursorResumeGateway。
func (g *cursorResumeFakeGateway) ServeCursorResume(
	context.Context, *gin.Context, *service.CursorResumeTicket, service.CursorResumeServeInput,
) (*service.ForwardResult, error) {
	g.serveCalls++
	if g.serveErr != nil {
		return nil, g.serveErr
	}
	return &service.ForwardResult{Model: "cursor-test-model"}, nil
}

// newCursorResumeTestContext 构造最小 gin 上下文（只用于承载请求 ctx）。
func newCursorResumeTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request = req
	return c
}

// captureReject 记录 reject 回调的状态码（断言"拒绝发生在资源获取之前"）。
type captureReject struct {
	called bool
	status int
	msg    string
}

func (r *captureReject) fn() func(int, string) {
	return func(status int, message string) {
		r.called = true
		r.status = status
		r.msg = message
	}
}

const cursorResumeHandlerChatContinuation = `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ]
}`

// TestServeCursorResumePlainRequestPassesThrough 覆盖 1：普通请求不被拦截。
func TestServeCursorResumePlainRequestPassesThrough(t *testing.T) {
	c := newCursorResumeTestContext(t)
	gw := &cursorResumeFakeGateway{}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(`{"model": "m", "messages": [{"role": "user", "content": "hi"}]}`),
		service.CursorSessionIdentity{UserID: 1}, false, reject.fn())
	require.False(t, handled, "普通请求必须继续走原路径")
	require.False(t, reject.called)
	require.Zero(t, gw.identifyCalls, "无结果 ID 时不进入索引定位")
	require.Zero(t, gw.serveCalls)
	// 身份必须已挂到请求 ctx（首 HTTP 建会话时记录）。
	require.Equal(t, int64(1), service.CursorSessionIdentityFromContext(c.Request.Context()).UserID)
}

// TestServeCursorResumeRejectedBeforeResources 覆盖 2：其它识别失败（混批/旧批等）先拒绝。
//
// §25.17.4（A08）只把「全部 ID 未命中」这一类交给原路由；已命中本服务却夹入未知/跨会话/
// 旧批/重复的请求仍必须在资源获取之前明确拒绝（不得退回普通路）。
func TestServeCursorResumeRejectedBeforeResources(t *testing.T) {
	c := newCursorResumeTestContext(t)
	gw := &cursorResumeFakeGateway{
		identifyErr: &service.CursorResumeError{
			Status: http.StatusBadRequest,
			Reason: "mixed_sessions",
			Err:    service.ErrCursorResumeMixedSessions,
		},
	}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, false, reject.fn())
	require.True(t, handled, "识别失败必须由续轮分支终止请求")
	require.True(t, reject.called)
	require.Equal(t, http.StatusBadRequest, reject.status)
	require.Zero(t, gw.serveCalls, "识别失败不得进入接管/写入")
}

// TestServeCursorResumeServedBySession 覆盖 3：合法续轮由会话接管并直接返回。
func TestServeCursorResumeServedBySession(t *testing.T) {
	c := newCursorResumeTestContext(t)
	gw := &cursorResumeFakeGateway{ticket: &service.CursorResumeTicket{SessionID: "s-1", Model: "cursor-test-model"}}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, true, reject.fn())
	require.True(t, handled)
	require.False(t, reject.called)
	require.Equal(t, 1, gw.identifyCalls)
	require.Equal(t, 1, gw.serveCalls)
}

// TestServeCursorResumeIntakeUnavailable 覆盖 4：通道缺失明确 503（不伪造成功）。
func TestServeCursorResumeIntakeUnavailable(t *testing.T) {
	c := newCursorResumeTestContext(t)
	gw := &cursorResumeFakeGateway{
		ticket:   &service.CursorResumeTicket{SessionID: "s-1"},
		serveErr: &service.CursorResumeError{Status: http.StatusServiceUnavailable, Reason: "intake_unavailable"},
	}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, false, reject.fn())
	require.True(t, handled)
	require.True(t, reject.called)
	require.Equal(t, http.StatusServiceUnavailable, reject.status)
}

// TestServeCursorResumeProtocolRouting 覆盖 5：四协议各自走自己的探针。
func TestServeCursorResumeProtocolRouting(t *testing.T) {
	cases := []struct {
		name         string
		protocol     service.CursorResumeProtocol
		body         string
		wantProbeIDs int
	}{
		{
			name:         "chat",
			protocol:     service.CursorResumeProtocolChat,
			body:         cursorResumeHandlerChatContinuation,
			wantProbeIDs: 1,
		},
		{
			name:     "messages",
			protocol: service.CursorResumeProtocolMessages,
			body: `{
  "model": "m",
  "max_tokens": 8,
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "hi"}]},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "sunny"}]}]}
  ]
}`,
			wantProbeIDs: 1,
		},
		{
			name:     "responses",
			protocol: service.CursorResumeProtocolResponses,
			body: `{
  "model": "m",
  "previous_response_id": "resp_1",
  "input": [{"type": "function_call_output", "call_id": "call_1", "output": "sunny"}]
}`,
			wantProbeIDs: 1,
		},
		{
			name:     "gemini",
			protocol: service.CursorResumeProtocolGemini,
			body: `{
  "model": "m",
  "contents": [
    {"role": "user", "parts": [{"text": "hi"}]},
    {"role": "model", "parts": [{"functionCall": {"id": "call_1", "name": "get_weather", "args": {}}}]},
    {"role": "user", "parts": [{"functionResponse": {"id": "call_1", "name": "get_weather", "response": {"result": "sunny"}}}]}
  ]
}`,
			wantProbeIDs: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe, err := service.CursorProbeResumeRequest(tc.protocol, []byte(tc.body))
			require.NoError(t, err)
			require.NotNil(t, probe)
			require.Equal(t, tc.wantProbeIDs, len(probe.ResultCallIDs), "各协议必须各自识别本批结果 ID")
		})
	}
}

// ---------------------------------------------------------------------------
// §25.17.4（A08）：未知关联只在 Cursor 边界拒绝
// ---------------------------------------------------------------------------

// cursorResumeUnclaimedAssociationErr 构造执行服务侧「明确声称续轮但全部 ID 未命中」的
// 错误形态（可区分哨兵 + 既有 400 身份）。helper 只按 errors.Is 认这一类。
func cursorResumeUnclaimedAssociationErr() error {
	return &service.CursorResumeError{
		Status: http.StatusBadRequest,
		Reason: "unknown_association",
		Err:    service.ErrCursorResumeUnknownAssociation,
	}
}

// releaseCounter 记录槽位释放次数（证明「资源沿原 HTTP defer 释放一次」）。
type releaseCounter struct{ n int }

func (r *releaseCounter) release() { r.n++ }

// 覆盖 6：明确声称续轮但全部 ID 未命中 → 不在选号前判死，交原路由按供应方判定。
func TestServeCursorResumeUnknownAssociationHandedToRouting(t *testing.T) {
	c := newCursorResumeTestContext(t)
	body := []byte(cursorResumeHandlerChatContinuation)
	original := append([]byte(nil), body...)
	gw := &cursorResumeFakeGateway{identifyErr: cursorResumeUnclaimedAssociationErr()}
	reject := &captureReject{}

	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat, body,
		service.CursorSessionIdentity{UserID: 7, APIKeyID: 9}, false, reject.fn())

	require.False(t, handled, "未知关联不得在选号前判死：必须交回原路由按供应方判定")
	require.False(t, reject.called, "不得为全站写 400（其它供应商的续接请求会被误杀）")
	require.Equal(t, 1, gw.identifyCalls)
	require.Zero(t, gw.serveCalls, "零接管：不得创建/接入任何 Cursor 会话")
	require.Len(t, gw.probes, 1)
	require.NotNil(t, gw.probes[0])
	require.True(t, gw.probes[0].ClaimsContinuation(), "该请求确实明确声称续轮")
	require.Equal(t, []string{"call_1"}, gw.probes[0].ResultCallIDs)
	require.Equal(t, original, body, "请求体不得改写（非 Cursor 原样转发）")
	require.False(t, c.Writer.Written(), "本层不得写任何响应")
	require.LessOrEqual(t, c.Writer.Size(), 0, "本层不得写任何响应字节")
	// 会话身份仍挂到 ctx：非 Cursor 路径与首 HTTP 建会话语义不变。
	require.Equal(t, int64(7), service.CursorSessionIdentityFromContext(c.Request.Context()).UserID)
}

// 覆盖 7：该情形下本 HTTP 已获取的资源不转交，仍由原 HTTP defer 释放一次。
func TestServeCursorResumeUnknownAssociationKeepsHTTPOwnedResources(t *testing.T) {
	c := newCursorResumeTestContext(t)
	userCount, accountCount := &releaseCounter{}, &releaseCounter{}
	userSlot := newTransferableRelease(c.Request.Context(), "user_slot", userCount.release)
	accountSlot := newTransferableRelease(c.Request.Context(), "account_slot", accountCount.release)
	trackTransferableSlot(c, cursorSlotHandleUserKey, userSlot)
	trackTransferableSlot(c, cursorSlotHandleAccountKey, accountSlot)
	// handler 形态：已临时获取的资源一律 defer 释放（Cursor 会话未接管时这就是唯一释放点）。
	defer func() {
		userSlot.Release()
		accountSlot.Release()
	}()

	gw := &cursorResumeFakeGateway{identifyErr: cursorResumeUnclaimedAssociationErr()}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, false, reject.fn())

	require.False(t, handled)
	require.False(t, reject.called)
	require.False(t, userSlot.HandedOver(), "不得把槽位转交给会话")
	require.False(t, accountSlot.HandedOver(), "不得把槽位转交给会话")
	require.Equal(t, 0, userCount.n, "识别/接管阶段不得提前释放")

	// 原 HTTP 结束（defer 触发）后各自恰好释放一次。
	userSlot.Release()
	accountSlot.Release()
	require.Equal(t, 1, userCount.n, "用户槽沿原 defer 释放且只释放一次")
	require.Equal(t, 1, accountCount.n, "账号槽沿原 defer 释放且只释放一次")
}

// cursorAdmissionStub 是 §25.17.4 行 5「Cursor 准入边界」的替身。
//
// 本单只到 helper 边界：真正的 400 由执行服务侧的「声明续轮却无有效接管」断言在模型出站/
// 建 session/转交资源之前发出（依赖项已在报告中登记）。这里用它固定 helper 必须交出的
// 输入：未判死、未接管、请求体原样——非 Cursor 原样转发一次；Cursor 分支 400 且上游 0。
//
// 400 **不是本地写死的常量**：它由生产错误契约 service.CursorResumeHTTPStatus 从执行服务侧
// 实际给出的错误形态导出，避免测试自证自话。
type cursorAdmissionStub struct {
	upstreamCalls int
}

func (s *cursorAdmissionStub) route(
	account *service.Account, claimsContinuation, tookOver bool, identifyErr error,
) int {
	if account != nil && account.Platform == service.PlatformCursor && claimsContinuation && !tookOver {
		// Cursor 分支：执行服务侧在模型出站/建 session/转交资源之前拒绝。
		status, _ := service.CursorResumeHTTPStatus(identifyErr)
		return status
	}
	s.upstreamCalls++
	return http.StatusOK
}

// 覆盖 8：非 Cursor 原样转发 / 进入 Cursor 分支 400 且上游调用 0。
func TestServeCursorResumeUnknownAssociationCursorBoundary(t *testing.T) {
	cases := []struct {
		name         string
		account      *service.Account
		wantStatus   int
		wantUpstream int
	}{
		{
			name:         "non_cursor_forwards_unchanged",
			account:      &service.Account{ID: 2, Platform: service.PlatformOpenAI},
			wantStatus:   http.StatusOK,
			wantUpstream: 1,
		},
		{
			name:         "cursor_rejects_before_outbound",
			account:      &service.Account{ID: 3, Platform: service.PlatformCursor},
			wantStatus:   http.StatusBadRequest,
			wantUpstream: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCursorResumeTestContext(t)
			released := &releaseCounter{}
			slot := newTransferableRelease(c.Request.Context(), "user_slot", released.release)
			trackTransferableSlot(c, cursorSlotHandleUserKey, slot)
			defer slot.Release()

			gw := &cursorResumeFakeGateway{identifyErr: cursorResumeUnclaimedAssociationErr()}
			reject := &captureReject{}
			handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
				[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, false, reject.fn())
			require.False(t, handled)
			require.False(t, reject.called, "helper 不替任何供应方判死")
			require.Zero(t, gw.serveCalls)
			require.Len(t, gw.probes, 1)

			admission := &cursorAdmissionStub{}
			status := admission.route(tc.account, gw.probes[0].ClaimsContinuation(), gw.serveCalls > 0,
				cursorResumeUnclaimedAssociationErr())
			require.Equal(t, tc.wantStatus, status)
			require.Equal(t, tc.wantUpstream, admission.upstreamCalls, "Cursor 分支不得产生任何上游调用")

			slot.Release()
			require.Equal(t, 1, released.n, "本 HTTP 已获取的资源只沿原 defer 释放一次")
			require.False(t, slot.HandedOver(), "资源未被会话转交，始终归原 HTTP")
		})
	}
}

// 覆盖 9：其它识别错误一律不吞，也不被误判成「未知关联」这一类。
func TestServeCursorResumeOtherIdentifyErrorsNotSwallowed(t *testing.T) {
	cases := []struct {
		name        string
		identifyErr error
		wantStatus  int
		wantDefer   bool
	}{
		{
			name: "mixed_sessions_400",
			identifyErr: &service.CursorResumeError{
				Status: http.StatusBadRequest, Reason: "mixed_sessions", Err: service.ErrCursorResumeMixedSessions,
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "stale_batch_400",
			identifyErr: &service.CursorResumeError{
				Status: http.StatusBadRequest, Reason: "stale_previous_response_id", Err: service.ErrCursorResumeStaleBatch,
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "duplicate_results_400",
			identifyErr: &service.CursorResumeError{
				Status: http.StatusBadRequest, Reason: "duplicate_result_id", Err: service.ErrCursorResumeDuplicateResults,
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "identity_mismatch_403",
			identifyErr: &service.CursorResumeError{
				Status: http.StatusForbidden, Reason: "identity_mismatch", Err: service.ErrCursorResumeIdentityMismatch,
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "session_busy_409",
			identifyErr: &service.CursorResumeError{
				Status: http.StatusConflict, Reason: "session_busy", Err: service.ErrCursorResumeSessionBusy,
			},
			wantStatus: http.StatusConflict,
		},
		{
			name: "intake_unavailable_503",
			identifyErr: &service.CursorResumeError{
				Status: http.StatusServiceUnavailable, Reason: "intake_unavailable", Err: service.ErrCursorResumeIntakeUnavailable,
			},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:        "execution_unwired_defers_to_ordinary_contract",
			identifyErr: service.ErrCursorExecutionUnavailable,
			wantDefer:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCursorResumeTestContext(t)
			gw := &cursorResumeFakeGateway{identifyErr: tc.identifyErr}
			reject := &captureReject{}
			handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
				[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, false, reject.fn())
			require.Zero(t, gw.serveCalls, "识别失败不得进入接管/写入")
			if tc.wantDefer {
				require.False(t, handled, "非续轮错误交回普通路径按其原契约失败关闭")
				require.False(t, reject.called)
				require.False(t, cursorResumeClaimsUnknownAssociation(tc.identifyErr),
					"非续轮错误不得被归入「未知关联」这一类")
				return
			}
			require.True(t, handled, "其它识别错误必须由续轮分支明确拒绝，不得吞掉")
			require.True(t, reject.called)
			require.Equal(t, tc.wantStatus, reject.status)
			require.False(t, cursorResumeClaimsUnknownAssociation(tc.identifyErr),
				"其它识别错误不得被归入「未知关联」这一类")
		})
	}
}

// TestCursorResumeClaimsUnknownAssociation 固定分类边界：只认执行服务的可区分哨兵。
func TestCursorResumeClaimsUnknownAssociation(t *testing.T) {
	require.False(t, cursorResumeClaimsUnknownAssociation(nil))
	require.True(t, cursorResumeClaimsUnknownAssociation(service.ErrCursorResumeUnknownAssociation))
	require.True(t, cursorResumeClaimsUnknownAssociation(cursorResumeUnclaimedAssociationErr()),
		"CursorResumeError 必须通过 Unwrap 暴露哨兵身份")
	require.False(t, cursorResumeClaimsUnknownAssociation(
		&service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"}),
		"没有哨兵身份的 reason 串不构成分类依据（不按文案猜）")
	require.False(t, cursorResumeClaimsUnknownAssociation(service.ErrCursorResumeMixedSessions))
	require.False(t, cursorResumeClaimsUnknownAssociation(service.ErrCursorExecutionUnavailable))
}

// ---------------------------------------------------------------------------
// §25.17.3「A05续轮顺序」（S2b-2）：资格核验拒绝按类返回，且是终止性响应
// ---------------------------------------------------------------------------

// TestServeCursorResumeEligibilityDenialReturnsByClass 覆盖：执行服务在定位会话后、写工具
// 结果前给出的资格拒绝，handler 必须**按类**（403/400/503）终止本次请求——不得落回普通
// 路径（否则会重新选号、重新执行工具），也不得二次接管。
//
// 状态码与 reason 都不是本地写死的常量：二者由生产错误契约 service.CursorResumeHTTPStatus
// 从执行服务侧实际给出的错误形态导出（这里用同一族哨兵构造，保证不按文案猜测）。
func TestServeCursorResumeEligibilityDenialReturnsByClass(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		reason   string
		sentinel error
	}{
		{
			name:     "account_ineligible_403",
			status:   http.StatusForbidden,
			reason:   "not_schedulable",
			sentinel: service.ErrCursorResumeAccountIneligible,
		},
		{
			name:     "source_changed_400",
			status:   http.StatusBadRequest,
			reason:   "source_changed",
			sentinel: service.ErrCursorResumeSourceChanged,
		},
		{
			name:     "source_unavailable_503",
			status:   http.StatusServiceUnavailable,
			reason:   "source_unavailable",
			sentinel: service.ErrCursorResumeSourceUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCursorResumeTestContext(t)
			denial := &service.CursorResumeError{Status: tc.status, Reason: tc.reason, Err: tc.sentinel}
			gw := &cursorResumeFakeGateway{
				ticket:   &service.CursorResumeTicket{SessionID: "s-1", Model: "cursor-test-model"},
				serveErr: denial,
			}
			reject := &captureReject{}
			handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
				[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1},
				false, reject.fn())

			require.True(t, handled, "资格拒绝必须由续轮分支终止请求，不得落回普通路径")
			require.True(t, reject.called)
			require.Equal(t, tc.status, reject.status, "拒绝必须按类返回")
			require.Equal(t, tc.reason, reject.msg, "reason 必须是执行服务给出的稳定枚举，不按文案改写")
			require.Equal(t, 1, gw.serveCalls, "拒绝是终止性的：不得二次接管/重试")
			require.False(t, c.Writer.Written(), "本层不再写任何响应")
		})
	}
}
