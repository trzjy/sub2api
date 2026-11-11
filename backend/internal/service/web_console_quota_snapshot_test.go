//go:build unit

package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fptr(v float64) *float64 { return &v }
func iptr(v int64) *int64     { return &v }

var wcqNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
var wcqFuture = wcqNow.Add(time.Hour).Format(time.RFC3339)
var wcqFuture2 = wcqNow.Add(2 * time.Hour).Format(time.RFC3339)
var wcqPast = wcqNow.Add(-time.Hour).Format(time.RFC3339)

// ---------------------------------------------------------------------------
// compshare（§4.1）
// ---------------------------------------------------------------------------

func TestBuildWebConsoleQuotaSnapshot_Compshare(t *testing.T) {
	cases := []struct {
		name string
		data string
		key  string
		want func(t *testing.T, s *WebConsoleQuotaSnapshot)
	}{
		{
			name: "no_active_plan",
			data: `{"auth_state":"ok","plans":[]}`,
			key:  "",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.Equal(t, "compshare", s.Provider)
				require.Equal(t, "no_active_plan", s.PlanMatchStatus)
				require.True(t, s.SubscriptionExpired)
				require.True(t, s.Exhausted)
				require.True(t, s.UntilIndefinite)
				require.Empty(t, s.Until)
			},
		},
		{
			name: "matched_single_not_exhausted",
			data: `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":10,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":100,"reset_week":"` + wcqFuture + `","limit_month":2000,"used_month":500,"reset_month":"` + wcqFuture + `"}]}`,
			key:  "abc",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.Equal(t, "matched", s.PlanMatchStatus)
				require.Equal(t, "Basic", s.PlanName)
				require.False(t, s.Exhausted)
				require.Empty(t, s.Until)
				require.False(t, s.UntilIndefinite)
				require.Len(t, s.Tiers, 3)
				require.Equal(t, "5h", s.Tiers[0].Window)
				require.Equal(t, 10.0, s.Tiers[0].Used)
				require.Equal(t, 100.0, s.Tiers[0].Limit)
			},
		},
		{
			name: "matched_single_exhausted_future_reset",
			data: `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":100,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":100,"reset_week":"` + wcqFuture + `","limit_month":2000,"used_month":500,"reset_month":"` + wcqFuture + `"}]}`,
			key:  "abc",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				require.Equal(t, wcqFuture, s.Until)
				require.False(t, s.UntilIndefinite)
			},
		},
		{
			name: "matched_single_exhausted_past_reset_indefinite",
			data: `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":120,"reset_5h":"` + wcqPast + `","limit_week":500,"used_week":100,"reset_week":"` + wcqPast + `","limit_month":2000,"used_month":500,"reset_month":"` + wcqPast + `"}]}`,
			key:  "abc",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				require.True(t, s.UntilIndefinite)
				require.Empty(t, s.Until)
			},
		},
		{
			name: "matched_single_exhausted_missing_reset_indefinite",
			data: `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":120,"reset_5h":"","limit_week":500,"used_week":100,"reset_week":"","limit_month":2000,"used_month":500,"reset_month":""}]}`,
			key:  "abc",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				require.True(t, s.UntilIndefinite)
			},
		},
		{
			name: "subscription_expired_by_expire_at",
			data: `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1000,"plan_status":1,"limit_5h":100,"used_5h":10,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":100,"reset_week":"` + wcqFuture + `","limit_month":2000,"used_month":500,"reset_month":"` + wcqFuture + `"}]}`,
			key:  "abc",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.SubscriptionExpired)
				require.True(t, s.Exhausted)
				require.True(t, s.UntilIndefinite)
			},
		},
		{
			name: "subscription_expired_by_plan_status",
			data: `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":0,"plan_status":2,"limit_5h":100,"used_5h":10,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":100,"reset_week":"` + wcqFuture + `","limit_month":2000,"used_month":500,"reset_month":"` + wcqFuture + `"}]}`,
			key:  "abc",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.SubscriptionExpired)
				require.True(t, s.Exhausted)
				require.True(t, s.UntilIndefinite)
			},
		},
		{
			name: "unmatched_multiple_no_hit",
			data: `{"auth_state":"ok","plans":[{"plan_name":"A","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":10,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":100,"reset_week":"` + wcqFuture + `","limit_month":2000,"used_month":500,"reset_month":"` + wcqFuture + `","key_hashes":["hashA"]},{"plan_name":"B","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":10,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":100,"reset_week":"` + wcqFuture + `","limit_month":2000,"used_month":500,"reset_month":"` + wcqFuture + `","key_hashes":["hashB"]}]}`,
			key:  "noHit",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.Equal(t, "unmatched", s.PlanMatchStatus)
				require.False(t, s.Exhausted)
				require.Empty(t, s.Until)
				require.False(t, s.UntilIndefinite)
				require.False(t, s.SubscriptionExpired)
			},
		},
		{
			name: "multiple_matched_by_key",
			data: `{"auth_state":"ok","plans":[{"plan_name":"A","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":10,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":100,"reset_week":"` + wcqFuture + `","limit_month":2000,"used_month":500,"reset_month":"` + wcqFuture + `","key_hashes":["hashA"]},{"plan_name":"B","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":10,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":100,"reset_week":"` + wcqFuture + `","limit_month":2000,"used_month":500,"reset_month":"` + wcqFuture + `","key_hashes":["hashB"]}]}`,
			key:  "hashB",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.Equal(t, "matched", s.PlanMatchStatus)
				require.Equal(t, "B", s.PlanName)
				require.False(t, s.Exhausted)
			},
		},
		{
			name: "multiple_windows_exhausted_max_reset",
			data: `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":100,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":500,"reset_week":"` + wcqPast + `","limit_month":2000,"used_month":2000,"reset_month":"` + wcqFuture2 + `"}]}`,
			key:  "abc",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				// 5h(future) 与 month(future2) 均超限，取最晚
				require.Equal(t, wcqFuture2, s.Until)
				require.False(t, s.UntilIndefinite)
			},
		},
		{
			name: "one_window_future_one_missing",
			data: `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":100,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":500,"reset_week":"","limit_month":2000,"used_month":500,"reset_month":"` + wcqFuture + `"}]}`,
			key:  "abc",
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				// R_future = {5h future, month future} 非空 → until = max(future, future)
				require.Equal(t, wcqFuture, s.Until)
				require.False(t, s.UntilIndefinite)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := BuildWebConsoleQuotaSnapshot("compshare", []byte(c.data), c.key, nil, wcqNow)
			require.NoError(t, err)
			require.NotNil(t, s)
			c.want(t, s)
		})
	}
}

// ---------------------------------------------------------------------------
// sensenova（§4.2）
// ---------------------------------------------------------------------------

func TestBuildWebConsoleQuotaSnapshot_Sensenova(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		mapping map[string]string
		want    func(t *testing.T, s *WebConsoleQuotaSnapshot)
	}{
		{
			name:    "pure_nonflash_general_blocked_no_grant",
			data:    `{"auth_state":"ok","pools":[{"id":"g","pool_type":"general","window_5h":{"limit":100,"remaining":0,"reset_at":"` + wcqFuture + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""},"grant_balance":0}]}`,
			mapping: map[string]string{"a": "ds-pro"},
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				require.Equal(t, wcqFuture, s.Until)
				require.False(t, s.UntilIndefinite)
			},
		},
		{
			name:    "pure_flash_flashlite_ok_general_blocked",
			data:    `{"auth_state":"ok","pools":[{"id":"fl","pool_type":"flash-lite","window_5h":{"limit":50,"remaining":10,"reset_at":"` + wcqFuture + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""}},{"id":"g","pool_type":"general","window_5h":{"limit":100,"remaining":0,"reset_at":"` + wcqFuture + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""},"grant_balance":0}]}`,
			mapping: map[string]string{"a": "sensenova-6.8-flash-lite"},
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				// P={flash-lite,general}; flash-lite 未阻塞 → 不耗尽
				require.False(t, s.Exhausted)
			},
		},
		{
			name:    "pure_flash_both_blocked_no_grant_min_reset",
			data:    `{"auth_state":"ok","pools":[{"id":"fl","pool_type":"flash-lite","window_5h":{"limit":50,"remaining":0,"reset_at":"` + wcqFuture + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""}},{"id":"g","pool_type":"general","window_5h":{"limit":100,"remaining":0,"reset_at":"` + wcqFuture2 + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""},"grant_balance":0}]}`,
			mapping: map[string]string{"a": "sensenova-6.8-flash-lite"},
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				// Until = min(recovery(flash-lite)=future, recovery(general)=future2)
				require.Equal(t, wcqFuture, s.Until)
				require.False(t, s.UntilIndefinite)
			},
		},
		{
			name:    "empty_mapping_both_blocked_with_grant",
			data:    `{"auth_state":"ok","pools":[{"id":"fl","pool_type":"flash-lite","window_5h":{"limit":50,"remaining":0,"reset_at":"` + wcqFuture + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""}},{"id":"g","pool_type":"general","window_5h":{"limit":100,"remaining":0,"reset_at":"` + wcqFuture2 + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""},"grant_balance":50}]}`,
			mapping: map[string]string{},
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				// P={flash-lite,general}; 全部阻塞但 G=true → 不耗尽
				require.False(t, s.Exhausted)
				require.NotNil(t, s.GrantBalance)
				require.Equal(t, 50.0, *s.GrantBalance)
			},
		},
		{
			name:    "until_indefinite_when_no_future_reset",
			data:    `{"auth_state":"ok","pools":[{"id":"g","pool_type":"general","window_5h":{"limit":100,"remaining":0,"reset_at":""},"window_7d":{"limit":0,"remaining":0,"reset_at":""},"grant_balance":0}]}`,
			mapping: map[string]string{"a": "ds-pro"},
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				require.True(t, s.UntilIndefinite)
				require.Empty(t, s.Until)
			},
		},
		{
			name:    "mixed_mapping_general_blocked_flashlite_ok",
			data:    `{"auth_state":"ok","pools":[{"id":"fl","pool_type":"flash-lite","window_5h":{"limit":50,"remaining":10,"reset_at":"` + wcqFuture + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""}},{"id":"g","pool_type":"general","window_5h":{"limit":100,"remaining":0,"reset_at":"` + wcqFuture + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""},"grant_balance":0}]}`,
			mapping: map[string]string{"a": "ds-pro", "b": "sensenova-6.8-flash-lite"},
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				// P={flash-lite,general}; flash-lite 未阻塞 → 不耗尽
				require.False(t, s.Exhausted)
			},
		},
		{
			name:    "mixed_mapping_both_blocked_no_grant",
			data:    `{"auth_state":"ok","pools":[{"id":"fl","pool_type":"flash-lite","window_5h":{"limit":50,"remaining":0,"reset_at":"` + wcqFuture2 + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""}},{"id":"g","pool_type":"general","window_5h":{"limit":100,"remaining":0,"reset_at":"` + wcqFuture + `"},"window_7d":{"limit":0,"remaining":0,"reset_at":""},"grant_balance":0}]}`,
			mapping: map[string]string{"a": "ds-pro", "b": "sensenova-6.8-flash-lite"},
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				require.Equal(t, wcqFuture, s.Until)
			},
		},
		{
			name:    "tiers_from_general_preferred",
			data:    `{"auth_state":"ok","pools":[{"id":"fl","pool_type":"flash-lite","window_5h":{"limit":50,"remaining":10,"reset_at":""},"window_7d":{"limit":0,"remaining":0,"reset_at":""}},{"id":"g","pool_type":"general","window_5h":{"limit":100,"remaining":40,"reset_at":"` + wcqFuture + `"},"window_7d":{"limit":200,"remaining":200,"reset_at":"` + wcqFuture2 + `"},"grant_balance":0}]}`,
			mapping: map[string]string{"a": "sensenova-6.8-flash-lite"},
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.Len(t, s.Tiers, 2)
				require.Equal(t, "5h", s.Tiers[0].Window)
				require.Equal(t, 100.0, s.Tiers[0].Limit)
				require.Equal(t, 60.0, s.Tiers[0].Used) // 100-40
				require.Equal(t, "7d", s.Tiers[1].Window)
				require.Equal(t, 200.0, s.Tiers[1].Limit)
				require.Equal(t, 0.0, s.Tiers[1].Used)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := BuildWebConsoleQuotaSnapshot("sensenova", []byte(c.data), "", c.mapping, wcqNow)
			require.NoError(t, err)
			require.NotNil(t, s)
			c.want(t, s)
		})
	}
}

// ---------------------------------------------------------------------------
// longcat（§4.3）
// ---------------------------------------------------------------------------

func TestBuildWebConsoleQuotaSnapshot_Longcat(t *testing.T) {
	cases := []struct {
		name string
		data string
		want func(t *testing.T, s *WebConsoleQuotaSnapshot)
	}{
		{
			name: "active_remaining_and_paygo",
			data: `{"auth_state":"ok","lots":[{"lot_id":1,"status":"ACTIVE","remaining_token":13.4,"total_token":50,"expire_time":"","is_current":true}],"paygo_balance_cent":100,"paygo_status":"ok"}`,
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.False(t, s.Exhausted)
				require.False(t, s.UntilIndefinite)
				require.NotNil(t, s.PaygoBalanceCent)
				require.Equal(t, int64(100), *s.PaygoBalanceCent)
			},
		},
		{
			name: "both_zero_exhausted_indefinite",
			data: `{"auth_state":"ok","lots":[{"lot_id":1,"status":"ACTIVE","remaining_token":0,"total_token":50,"expire_time":"","is_current":true}],"paygo_balance_cent":0,"paygo_status":"ok"}`,
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				require.True(t, s.UntilIndefinite)
			},
		},
		{
			name: "pack_zero_paygo_positive",
			data: `{"auth_state":"ok","lots":[{"lot_id":1,"status":"ACTIVE","remaining_token":0,"total_token":50,"expire_time":"","is_current":true}],"paygo_balance_cent":50,"paygo_status":"ok"}`,
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.False(t, s.Exhausted)
			},
		},
		{
			name: "pack_positive_paygo_zero",
			data: `{"auth_state":"ok","lots":[{"lot_id":1,"status":"ACTIVE","remaining_token":5,"total_token":50,"expire_time":"","is_current":true}],"paygo_balance_cent":0,"paygo_status":"ok"}`,
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.False(t, s.Exhausted)
			},
		},
		{
			name: "two_lots_one_active",
			data: `{"auth_state":"ok","lots":[{"lot_id":1,"status":"ACTIVE","remaining_token":5,"total_token":50,"expire_time":"","is_current":true},{"lot_id":2,"status":"ACTIVE","remaining_token":0,"total_token":50,"expire_time":"","is_current":false}],"paygo_balance_cent":0,"paygo_status":"ok"}`,
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.False(t, s.Exhausted)
			},
		},
		{
			name: "inactive_lot_not_counted",
			data: `{"auth_state":"ok","lots":[{"lot_id":1,"status":"EXPIRED","remaining_token":99,"total_token":50,"expire_time":"","is_current":false}],"paygo_balance_cent":0,"paygo_status":"ok"}`,
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				require.True(t, s.UntilIndefinite)
			},
		},
		{
			name: "negative_remaining_counts_zero",
			data: `{"auth_state":"ok","lots":[{"lot_id":1,"status":"ACTIVE","remaining_token":-5,"total_token":50,"expire_time":"","is_current":true}],"paygo_balance_cent":0,"paygo_status":"ok"}`,
			want: func(t *testing.T, s *WebConsoleQuotaSnapshot) {
				require.True(t, s.Exhausted)
				require.True(t, s.UntilIndefinite)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := BuildWebConsoleQuotaSnapshot("longcat", []byte(c.data), "", nil, wcqNow)
			require.NoError(t, err)
			require.NotNil(t, s)
			c.want(t, s)
		})
	}
}

// ---------------------------------------------------------------------------
// 杂项
// ---------------------------------------------------------------------------

func TestBuildWebConsoleQuotaSnapshot_UnsupportedProvider(t *testing.T) {
	_, err := BuildWebConsoleQuotaSnapshot("unknown", []byte("{}"), "", nil, wcqNow)
	require.Error(t, err)
}

func TestBuildWebConsoleQuotaSnapshot_BadJSON(t *testing.T) {
	_, err := BuildWebConsoleQuotaSnapshot("compshare", []byte("not-json"), "", nil, wcqNow)
	require.Error(t, err)
}

// 确认快照可被无损往返序列化（供后续写入 accounts.extra 使用）。
func TestBuildWebConsoleQuotaSnapshot_RoundTrip(t *testing.T) {
	data := `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,"limit_5h":100,"used_5h":100,"reset_5h":"` + wcqFuture + `","limit_week":500,"used_week":100,"reset_week":"` + wcqFuture + `","limit_month":2000,"used_month":500,"reset_month":"` + wcqFuture + `"}]}`
	s, err := BuildWebConsoleQuotaSnapshot("compshare", []byte(data), "", nil, wcqNow)
	require.NoError(t, err)
	b, err := json.Marshal(s)
	require.NoError(t, err)
	var back WebConsoleQuotaSnapshot
	require.NoError(t, json.Unmarshal(b, &back))
	require.True(t, back.Exhausted)
	require.Equal(t, wcqFuture, back.Until)
}
