package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// jitterObserverStub 是 upstreamJitterObserver 的最小替身：可配置 Observe 的返回值，
// 并记录每次调用收到的 accountID / modelScope / failoverErr 供断言。
type jitterObserverStub struct {
	admitted   bool
	excludeIDs []int64

	calls []jitterObserveCall
}

type jitterObserveCall struct {
	accountID  int64
	modelScope string
	fe         *service.UpstreamFailoverError
}

func (o *jitterObserverStub) Observe(_ context.Context, accountID int64, modelScope string, fe *service.UpstreamFailoverError) (bool, []int64) {
	o.calls = append(o.calls, jitterObserveCall{
		accountID:  accountID,
		modelScope: modelScope,
		fe:         fe,
	})
	return o.admitted, o.excludeIDs
}

// nilGuardProviderStub 实现 upstreamJitterGuardProvider，但恒返回 nil guard，
// 用于验证 upstreamJitterGuardFor 返回的是 nil 接口而非 typed-nil。
type nilGuardProviderStub struct{}

func (nilGuardProviderStub) NewUpstreamJitterGuard(_ int64) *service.UpstreamJitterGuard {
	return nil
}

// newJitterTestFailoverErr 返回本文件所有用例共用的换号错误（无同账号重试）。
func newJitterTestFailoverErr() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:        502,
		Reason:            service.CursorUpstreamAccountFailureReason,
		NextAccountAction: service.NextAccountRetry,
	}
}

// 用例 a：准入成立时不整号停调，并把守门器给出的兄弟账号并入请求排除集。
func TestUpstreamJitterFailoverAdmittedExcludesSiblings(t *testing.T) {
	ctx := context.Background()
	const currentAccountID int64 = 7

	observer := &jitterObserverStub{admitted: true, excludeIDs: []int64{9, 10}}
	unscheduler := &mockTempUnscheduler{}
	fs := NewFailoverState(false)
	fs.Jitter = observer

	action := fs.HandleFailoverError(ctx, unscheduler, currentAccountID, "openai", 0, newJitterTestFailoverErr(), "mapped-model")

	require.Equal(t, FailoverContinue, action)
	require.Empty(t, unscheduler.calls, "准入成立时不得调用 TempUnscheduleRetryableError")
	require.Contains(t, fs.FailedAccountIDs, int64(9))
	require.Contains(t, fs.FailedAccountIDs, int64(10))
	require.Contains(t, fs.FailedAccountIDs, currentAccountID, "当前账号仍须进入本请求排除集")
	require.Len(t, observer.calls, 1)
	require.Equal(t, currentAccountID, observer.calls[0].accountID)
	require.Equal(t, "mapped-model", observer.calls[0].modelScope)
}

// 用例 b：Observe 返回 admitted=false 时保持既有的整号停调行为。
func TestUpstreamJitterFailoverNotAdmittedTempUnschedules(t *testing.T) {
	ctx := context.Background()

	observer := &jitterObserverStub{admitted: false}
	unscheduler := &mockTempUnscheduler{}
	fs := NewFailoverState(false)
	fs.Jitter = observer

	action := fs.HandleFailoverError(ctx, unscheduler, 7, "openai", 0, newJitterTestFailoverErr(), "mapped-model")

	require.Equal(t, FailoverContinue, action)
	require.Len(t, unscheduler.calls, 1, "未准入时须保持既有停号行为")
	require.Equal(t, int64(7), unscheduler.calls[0].accountID)
}

// 用例 c：未挂载守门器（Jitter == nil）时行为与改前逐字一致。
func TestUpstreamJitterFailoverNilObserverKeepsLegacyBehavior(t *testing.T) {
	ctx := context.Background()

	unscheduler := &mockTempUnscheduler{}
	fs := NewFailoverState(false)
	require.Nil(t, fs.Jitter)

	action := fs.HandleFailoverError(ctx, unscheduler, 7, "openai", 0, newJitterTestFailoverErr(), "mapped-model")

	require.Equal(t, FailoverContinue, action)
	require.Len(t, unscheduler.calls, 1, "nil 守门器时须与改前一致地整号停调")
	require.Equal(t, int64(7), unscheduler.calls[0].accountID)
}

// 用例 d：传入带首尾空白的 modelScope 时，守门器收到裁剪后的值。
func TestUpstreamJitterFailoverTrimsModelScope(t *testing.T) {
	ctx := context.Background()

	observer := &jitterObserverStub{admitted: false}
	unscheduler := &mockTempUnscheduler{}
	fs := NewFailoverState(false)
	fs.Jitter = observer

	fs.HandleFailoverError(ctx, unscheduler, 7, "openai", 0, newJitterTestFailoverErr(), " mapped-model ")

	require.Len(t, observer.calls, 1)
	require.Equal(t, "mapped-model", observer.calls[0].modelScope, "首尾空白须被裁掉")
}

// 用例 e：服务或守门器不可用时返回 nil 接口（而非 typed-nil）。
func TestUpstreamJitterFailoverGuardForNilResults(t *testing.T) {
	require.True(t, upstreamJitterGuardFor(nil, nil) == nil, "nil provider 必须返回 nil 接口")

	got := upstreamJitterGuardFor(nilGuardProviderStub{}, &service.APIKey{})
	require.True(t, got == nil, "provider 返回 nil guard 时必须返回 nil 接口而非 typed-nil")
}
