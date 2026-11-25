//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// upstreamJitterRepoStub 只实现窄接口 UpstreamJitterRepo，刻意不实现
// AccountRepository：守卫的调用点只依赖窄接口。
type upstreamJitterRepoStub struct {
	accounts      map[int64]*Account
	groupAccounts []Account
	siblingsByQK  map[int64][]int64

	getCalls     int
	listCalls    int
	siblingCalls int
	setCalls     []upstreamJitterSetCall

	getErr     error
	listErr    error
	setErr     error
	siblingErr error
}

type upstreamJitterSetCall struct {
	accountID int64
	scope     string
	resetAt   time.Time
	reason    []string
}

var _ UpstreamJitterRepo = (*upstreamJitterRepoStub)(nil)

func (s *upstreamJitterRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.accounts[id], nil
}

func (s *upstreamJitterRepoStub) ListByGroup(_ context.Context, _ int64) ([]Account, error) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.groupAccounts, nil
}

func (s *upstreamJitterRepoStub) SetModelRateLimit(_ context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	s.setCalls = append(s.setCalls, upstreamJitterSetCall{
		accountID: id,
		scope:     scope,
		resetAt:   resetAt,
		reason:    append([]string(nil), reason...),
	})
	return s.setErr
}

func (s *upstreamJitterRepoStub) SiblingAccountIDsByCursorUpstreamKey(_ context.Context, _, quotaKeyID int64) ([]int64, error) {
	s.siblingCalls++
	if s.siblingErr != nil {
		return nil, s.siblingErr
	}
	return s.siblingsByQK[quotaKeyID], nil
}

func jitterCursorAccount(id, quotaKeyID int64) *Account {
	return &Account{ID: id, Platform: PlatformCursor, QuotaKeyID: &quotaKeyID}
}

func jitterOpenAIAccount(id int64, baseURL string) *Account {
	return &Account{ID: id, Platform: PlatformOpenAI, Credentials: map[string]any{"base_url": baseURL}}
}

func TestUpstreamJitterIsUpstreamJitterError(t *testing.T) {
	tests := []struct {
		name string
		fe   *UpstreamFailoverError
		want bool
	}{
		{"nil error", nil, false},
		{"status 500", &UpstreamFailoverError{StatusCode: 500}, true},
		{"status 502", &UpstreamFailoverError{StatusCode: 502}, true},
		{"status 503", &UpstreamFailoverError{StatusCode: 503}, true},
		{"status 599", &UpstreamFailoverError{StatusCode: 599}, true},
		{"cursor upstream account failure", &UpstreamFailoverError{Reason: CursorUpstreamAccountFailureReason}, true},

		{"status 429", &UpstreamFailoverError{StatusCode: 429}, false},
		{"status 400", &UpstreamFailoverError{StatusCode: 400}, false},
		{"cursor rate limited", &UpstreamFailoverError{StatusCode: 500, Reason: CursorRateLimitedReason}, false},
		{"cursor quota exhausted", &UpstreamFailoverError{StatusCode: 500, Reason: CursorQuotaExhaustedReason}, false},
		{"cursor key rejected", &UpstreamFailoverError{StatusCode: 500, Reason: cursorKeyRejectedReason}, false},
		{"404 model not found", &UpstreamFailoverError{StatusCode: 500, Reason: upstreamModelNotFoundReason}, false},
		{"tokenharbor free tier", &UpstreamFailoverError{StatusCode: 500, Reason: tokenHarborFreeTierReasonPrefix + "_exhausted"}, false},
		{"exclude upstream host", &UpstreamFailoverError{StatusCode: 500, ExcludeUpstreamHost: "review.example.com"}, false},
		{"request scoped transient", &UpstreamFailoverError{StatusCode: 500, RequestScopedTransient: true}, false},
		{"credential failure", &UpstreamFailoverError{StatusCode: 500, Stage: GatewayFailureStageAccountAuth}, false},
		{"next account stop", &UpstreamFailoverError{StatusCode: 500, NextAccountAction: NextAccountStop}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsUpstreamJitterError(tt.fe), tt.name)
		})
	}
}

func TestUpstreamJitterCanProbeModelScope(t *testing.T) {
	require.False(t, CanProbeModelScope(nil))
	for _, platform := range []string{PlatformAntigravity, PlatformDevin, PlatformCodeBuddy} {
		require.False(t, CanProbeModelScope(&Account{Platform: platform}), platform)
	}
	for _, platform := range []string{PlatformOpenAI, PlatformCursor} {
		require.True(t, CanProbeModelScope(&Account{Platform: platform}), platform)
	}
	// cursor 两种形态（apikey / oauth）都纳入探测。
	require.True(t, CanProbeModelScope(&Account{Platform: PlatformCursor, Type: AccountTypeAPIKey}))
	require.True(t, CanProbeModelScope(&Account{Platform: PlatformCursor, Type: AccountTypeOAuth}))
}

func TestUpstreamJitterObserveNonJitterError(t *testing.T) {
	stub := &upstreamJitterRepoStub{
		accounts:     map[int64]*Account{1: jitterCursorAccount(1, 100)},
		siblingsByQK: map[int64][]int64{100: {1, 2, 3}},
	}
	g := newUpstreamJitterGuard(stub, 7)

	admitted, excludeIDs := g.Observe(context.Background(), 1, "model-a", &UpstreamFailoverError{StatusCode: 429})
	require.False(t, admitted)
	require.Nil(t, excludeIDs)
	require.Empty(t, stub.setCalls, "非抖动错误不得写停调")
	require.Zero(t, stub.siblingCalls)
}

func TestUpstreamJitterObserveCursorAdmission(t *testing.T) {
	const quotaKeyID int64 = 42
	const groupID int64 = 9
	const scope = "claude-sonnet-4"

	siblings := []int64{1, 2, 3, 4}
	stub := &upstreamJitterRepoStub{
		accounts: map[int64]*Account{
			1: jitterCursorAccount(1, quotaKeyID),
			2: jitterCursorAccount(2, quotaKeyID),
			3: jitterCursorAccount(3, quotaKeyID),
			4: jitterCursorAccount(4, quotaKeyID),
		},
		siblingsByQK: map[int64][]int64{quotaKeyID: siblings},
	}
	g := newUpstreamJitterGuard(stub, groupID)
	fe := &UpstreamFailoverError{StatusCode: 500, Reason: CursorUpstreamAccountFailureReason}

	start := time.Now()
	for _, id := range []int64{1, 2} {
		admitted, excludeIDs := g.Observe(context.Background(), id, scope, fe)
		require.True(t, admitted)
		require.Empty(t, excludeIDs, "账号 %d 尚未达到阈值", id)
	}

	admitted, excludeIDs := g.Observe(context.Background(), 3, scope, fe)
	require.True(t, admitted)
	require.Equal(t, []int64{4}, excludeIDs, "第三个账号触发阈值，排除兄弟集中尚未失败的账号")

	admitted, excludeIDs = g.Observe(context.Background(), 4, scope, fe)
	require.True(t, admitted)
	require.Empty(t, excludeIDs, "排除集只返回一次，不得重复")

	require.Len(t, stub.setCalls, 4)
	for i, call := range stub.setCalls {
		require.Equal(t, int64(i+1), call.accountID)
		require.Equal(t, scope, call.scope)
		require.Equal(t, []string{UpstreamJitterModelRateLimitReason}, call.reason)
		require.False(t, call.resetAt.Before(start.Add(upstreamJitterEntryObservationWindow-10*time.Second)))
		require.False(t, call.resetAt.After(time.Now().Add(upstreamJitterEntryObservationWindow+10*time.Second)))
	}
	require.Equal(t, 1, stub.siblingCalls, "兄弟账号查询应命中缓存")
}

func TestUpstreamJitterObserveStopWriteFailure(t *testing.T) {
	const quotaKeyID int64 = 77
	stub := &upstreamJitterRepoStub{
		accounts: map[int64]*Account{
			1: jitterCursorAccount(1, quotaKeyID),
			2: jitterCursorAccount(2, quotaKeyID),
			3: jitterCursorAccount(3, quotaKeyID),
		},
		siblingsByQK: map[int64][]int64{quotaKeyID: {1, 2, 3, 4}},
		setErr:       errors.New("stop write failed"),
	}
	g := newUpstreamJitterGuard(stub, 3)
	fe := &UpstreamFailoverError{StatusCode: 503}

	for _, id := range []int64{1, 2, 3} {
		admitted, excludeIDs := g.Observe(context.Background(), id, "model-b", fe)
		require.False(t, admitted, "写停调失败必须拒绝准入")
		require.Nil(t, excludeIDs)
	}
	require.Len(t, stub.setCalls, 3)
}

func TestUpstreamJitterObserveNonCursorHost(t *testing.T) {
	const baseURL = "https://api.example.com/v1"
	accountByID := map[int64]*Account{
		1: jitterOpenAIAccount(1, baseURL),
		2: jitterOpenAIAccount(2, baseURL),
		3: jitterOpenAIAccount(3, baseURL),
		4: jitterOpenAIAccount(4, baseURL),
	}
	stub := &upstreamJitterRepoStub{accounts: accountByID}
	for id := int64(1); id <= 4; id++ {
		stub.groupAccounts = append(stub.groupAccounts, *accountByID[id])
	}
	g := newUpstreamJitterGuard(stub, 11)
	fe := &UpstreamFailoverError{StatusCode: 500}

	for _, id := range []int64{1, 2} {
		admitted, excludeIDs := g.Observe(context.Background(), id, "model-c", fe)
		require.True(t, admitted)
		require.Empty(t, excludeIDs)
	}
	admitted, excludeIDs := g.Observe(context.Background(), 3, "model-c", fe)
	require.True(t, admitted)
	require.Equal(t, []int64{4}, excludeIDs, "非 cursor 平台走 ListByGroup + siblingAccountIDsByHost，第三个账号触发排除")

	require.Greater(t, stub.listCalls, 0)
	require.Zero(t, stub.siblingCalls, "非 cursor 平台不应查询 quota key 兄弟")
	require.Len(t, stub.setCalls, 3)
}

func TestUpstreamJitterMinAccountID(t *testing.T) {
	require.Equal(t, int64(99), minAccountID(nil, 99))
	require.Equal(t, int64(99), minAccountID([]int64{}, 99))
	require.Equal(t, int64(2), minAccountID([]int64{5, 2, 9}, 99))
}

func TestUpstreamJitterNewGuardNilSafety(t *testing.T) {
	var nilGateway *GatewayService
	require.Nil(t, nilGateway.NewUpstreamJitterGuard(1))

	var nilOpenAI *OpenAIGatewayService
	require.Nil(t, nilOpenAI.NewUpstreamJitterGuard(1))

	// accountRepo 为 nil 接口时，类型断言不成立，返回 nil 而非 panic。
	require.Nil(t, (&GatewayService{}).NewUpstreamJitterGuard(1))
	require.Nil(t, (&OpenAIGatewayService{}).NewUpstreamJitterGuard(1))
}
