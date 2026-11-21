//go:build unit

package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsAccountScopedUpstreamUnavailable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
		body   string
		msg    string
		want   bool
	}{
		{"503 no available accounts", http.StatusServiceUnavailable, `{"error":{"message":"No available accounts"}}`, "", true},
		{"503 quota exhausted", http.StatusServiceUnavailable, `{"error":{"code":"insufficient_quota","message":"quota exhausted"}}`, "", true},
		{"503 chinese quota", http.StatusServiceUnavailable, `{"message":"余额不足"}`, "", true},
		{"503 capacity exhausted excluded", http.StatusServiceUnavailable, `{"error":{"code":"MODEL_CAPACITY_EXHAUSTED"}}`, "", false},
		{"503 request scoped shed excluded", http.StatusServiceUnavailable, `{"error":{"message":"server is overloaded"}}`, "", false},
		{"500 not 503", http.StatusInternalServerError, `{"error":{"message":"No available accounts"}}`, "", false},
		{"200 not error", http.StatusOK, `{}`, "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, IsAccountScopedUpstreamUnavailable(tc.status, []byte(tc.body), tc.msg))
		})
	}
}

func TestIsAccountScopedTransientUpstreamError(t *testing.T) {
	t.Parallel()

	require.True(t, IsAccountScopedTransientUpstreamError(http.StatusBadGateway, []byte(`not json`), ""))
	require.True(t, IsAccountScopedTransientUpstreamError(http.StatusGatewayTimeout, []byte(`{}`), ""))
	require.False(t, IsAccountScopedTransientUpstreamError(http.StatusServiceUnavailable, []byte(`{}`), ""))
	require.False(t, IsAccountScopedTransientUpstreamError(http.StatusBadRequest, []byte(`{}`), ""))
	require.False(t, IsAccountScopedTransientUpstreamError(http.StatusBadGateway, []byte(`{"error":{"message":"server is overloaded"}}`), ""))
}

func recoveryProbeTestExtra(components map[string]any) map[string]any {
	return map[string]any{AccountRecoveryProbeExtraKey: components}
}

func TestHasPendingTransportProbe(t *testing.T) {
	t.Parallel()

	due := time.Now().Add(time.Minute).Format(time.RFC3339Nano)
	withTransport := &Account{Extra: recoveryProbeTestExtra(map[string]any{
		"transport": map[string]any{"gen": 7, "due": due, "mode": "transient"},
	})}
	require.True(t, withTransport.HasPendingTransportProbe())

	quotaOnly := &Account{Extra: recoveryProbeTestExtra(map[string]any{
		"quota": map[string]any{"gen": 8, "due": due, "mode": "noecho", "kind": "quota"},
	})}
	require.False(t, quotaOnly.HasPendingTransportProbe())

	require.False(t, (&Account{}).HasPendingTransportProbe())
	require.False(t, (*Account)(nil).HasPendingTransportProbe())
}

func TestRecoveryProbeComponent(t *testing.T) {
	t.Parallel()

	due := time.Now().Add(5 * time.Minute)
	acc := &Account{Extra: recoveryProbeTestExtra(map[string]any{
		"quota": map[string]any{"gen": 11, "due": due.Format(time.RFC3339Nano), "mode": "echo", "kind": "both"},
	})}
	gen, gotDue, mode, kind, ok := acc.RecoveryProbeComponent(recoveryProbeComponentQuota)
	require.True(t, ok)
	require.Equal(t, int64(11), gen)
	require.WithinDuration(t, due, gotDue, time.Second)
	require.Equal(t, "echo", mode)
	require.Equal(t, "both", kind)

	_, _, _, _, ok = acc.RecoveryProbeComponent(recoveryProbeComponentTransport)
	require.False(t, ok)
}

// 影子连坐边界：quota 类停调（不写 until、只有 quota 分量）不连坐影子；
// transport 类待探测期间即使 until 已过期，影子仍不可用。
func TestIsCredentialUsableForShadow_RecoveryProbeBoundary(t *testing.T) {
	t.Parallel()

	due := time.Now().Add(time.Minute).Format(time.RFC3339Nano)
	base := func() *Account {
		return &Account{Status: StatusActive, Schedulable: true}
	}

	quotaParked := base()
	quotaParked.Extra = recoveryProbeTestExtra(map[string]any{
		"quota": map[string]any{"gen": 1, "due": due, "mode": "noecho", "kind": "quota"},
	})
	require.True(t, quotaParked.IsCredentialUsableForShadow(), "quota 停调不得连坐影子")

	transportParked := base()
	transportParked.Extra = recoveryProbeTestExtra(map[string]any{
		"transport": map[string]any{"gen": 2, "due": due, "mode": "unavailable"},
	})
	require.False(t, transportParked.IsCredentialUsableForShadow(), "transport 待探测期间影子必须连坐")
}
