//go:build unit

package service

// CURSOR-PROBE-001-A4 单测：拒钥永久停用转接 + 换号耗尽对外映射。
//
// 覆盖：
//  1. tempUndisableCursorCredential 内部只调 SetError（断言被调用且参数正确）；
//  2. 守卫同型（s==nil / accountRepo==nil / accountID<=0 直接返回，不触达 SetError）；
//  3. 换号耗尽：拒钥 reason 命中 cursor 池耗尽对外映射（断言不落 AllAccountsExhausted）；
//  4. 回归红线：非拒钥的既有 reason 映射行为不变。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// keyRejectedFakeRepo 是仅实现 SetError 的 AccountRepository 替身。
// 通过嵌入接口满足 AccountRepository 类型（其余方法在测试中不会被触达）；
// 只覆盖 SetError 以记录调用实参供断言。
type keyRejectedFakeRepo struct {
	AccountRepository // 嵌入接口：未覆盖的方法运行时为 nil，本测试不调用
	setErrorCalls    int
	lastID           int64
	lastMsg          string
}

func (f *keyRejectedFakeRepo) SetError(_ context.Context, id int64, errorMsg string) error {
	f.setErrorCalls++
	f.lastID = id
	f.lastMsg = errorMsg
	return nil
}

func TestTempUndisableCursorCredential_CallsSetError(t *testing.T) {
	repo := &keyRejectedFakeRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}

	const accountID int64 = 123
	const reason = "cursor upstream rejected the key (expired or quota exhausted)"
	svc.tempUndisableCursorCredential(context.Background(), accountID, reason)

	require.Equal(t, 1, repo.setErrorCalls, "必须恰好调用一次既有原语 SetError")
	require.Equal(t, accountID, repo.lastID, "SetError 的 accountID 必须透传")
	require.Equal(t, reason, repo.lastMsg, "SetError 的 errorMsg 必须是脱敏后的 reason")
}

func TestTempUndisableCursorCredential_GuardNilRepo(t *testing.T) {
	// accountRepo 为 nil：守卫应直接返回，不触达 SetError（不 panic）。
	svc := &OpenAIGatewayService{accountRepo: nil}
	svc.tempUndisableCursorCredential(context.Background(), 1, "should-not-reach")
	// 无法对 nil 接口调用 SetError 做计数，这里只验证不 panic 且函数正常返回。
}

func TestTempUndisableCursorCredential_GuardInvalidAccountID(t *testing.T) {
	repo := &keyRejectedFakeRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}

	// accountID <= 0：守卫应直接返回，SetError 不被调用。
	svc.tempUndisableCursorCredential(context.Background(), 0, "reason")
	svc.tempUndisableCursorCredential(context.Background(), -5, "reason")

	require.Equal(t, 0, repo.setErrorCalls, "accountID<=0 时不得调用 SetError")
}

func TestTempUndisableCursorCredential_GuardNilService(t *testing.T) {
	// s == nil：方法接收者为 nil 时守卫直接返回（Go 允许 nil 接收者调用方法）。
	var svc *OpenAIGatewayService
	svc.tempUndisableCursorCredential(context.Background(), 1, "reason")
	// 不 panic 即通过。
}

func TestIsCursorPoolUnavailableReasonWithKeyRejected_MapsKeyRejected(t *testing.T) {
	// 拒钥 reason 必须命中 cursor 池耗尽映射（⇒ handler 走 CUR-RES-11，不落 AllAccountsExhausted）。
	require.True(t, IsCursorPoolUnavailableReasonWithKeyRejected(cursorKeyRejectedReason),
		"拒钥 reason 必须纳入对外 cursor 池耗尽映射，避免落通用 AllAccountsExhausted")
}

func TestIsCursorPoolUnavailableReasonWithKeyRejected_RegressionExistingReasons(t *testing.T) {
	// 回归红线：既有两个 reason 的对外映射行为必须不变。
	require.True(t, IsCursorPoolUnavailableReasonWithKeyRejected(CursorQuotaExhaustedReason),
		"额度耗尽 reason 映射行为不得改变")
	require.True(t, IsCursorPoolUnavailableReasonWithKeyRejected(CursorRateLimitedReason),
		"限流 reason 映射行为不得改变")

	// 与既有函数逐字等价：对既有两个 reason，扩展版与原版结果一致。
	require.Equal(t,
		IsCursorPoolUnavailableReason(CursorQuotaExhaustedReason),
		IsCursorPoolUnavailableReasonWithKeyRejected(CursorQuotaExhaustedReason))
	require.Equal(t,
		IsCursorPoolUnavailableReason(CursorRateLimitedReason),
		IsCursorPoolUnavailableReasonWithKeyRejected(CursorRateLimitedReason))
}

func TestIsCursorPoolUnavailableReasonWithKeyRejected_OtherReasonUnmapped(t *testing.T) {
	// 一个与 cursor 池耗尽/拒钥都无关的原因，不得被误映射。
	other := GatewayFailureReason("some_unrelated_reason")
	require.False(t, IsCursorPoolUnavailableReasonWithKeyRejected(other),
		"无关 reason 不得被纳入 cursor 池耗尽映射")
	require.False(t, IsCursorPoolUnavailableReason(other),
		"既有函数对本 reason 的结果也必须为 false（回归）")
}

func TestIsCursorPoolUnavailableReasonWithKeyRejected_NotStopReason(t *testing.T) {
	// 安全复核：拒钥 reason 不在「写停调」白名单内。
	// 停调白名单（failover_loop.go:277-279）走 IsCursorQuotaExhaustedReason/CursorRateLimitedReason/
	// CursorUpstreamAccountFailureReason，而非 IsCursorPoolUnavailableReason 系列（仅对外响应）。
	// 这里断言：把拒钥纳入对外映射**不会**让 IsCursorPoolUnavailableReason 误判为停调相关的
	// 既有 reason——即对外映射与停调判据相互独立。
	require.False(t, IsCursorPoolUnavailableReason(cursorKeyRejectedReason),
		"对外映射函数本体不得把拒钥 reason 判为既有停调相关 reason（维持现状，由扩展版单独纳入）")
}
