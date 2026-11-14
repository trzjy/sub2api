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

// ---------------------------------------------------------------------------
// 时间戳兼容：Unix 秒/毫秒数字、纯数字字符串、RFC3339（CARD-WCQ-FLEXTS）
// ---------------------------------------------------------------------------

func wcqEpochRFC3339(sec int64) string {
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// compshare：Python 适配器透传的 Unix 秒级整数与纯数字字符串。
func TestWebConsoleQuotaFlexTimestamp_CompshareUnixSeconds(t *testing.T) {
	data := `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,` +
		`"limit_5h":100,"used_5h":10,"reset_5h":1794344400,` +
		`"limit_week":500,"used_week":100,"reset_week":1794758400,` +
		`"limit_month":2000,"used_month":500,"reset_month":0}]}`

	s, err := BuildWebConsoleQuotaSnapshot("compshare", []byte(data), "", nil, wcqNow)
	require.NoError(t, err)
	require.NotNil(t, s)
	require.Equal(t, "matched", s.PlanMatchStatus)
	require.False(t, s.Exhausted)
	require.Len(t, s.Tiers, 3)
	require.Equal(t, wcqEpochRFC3339(1794344400), s.Tiers[0].ResetAt)
	require.Equal(t, "2026-11-10T21:00:00Z", s.Tiers[0].ResetAt)
	require.Equal(t, wcqEpochRFC3339(1794758400), s.Tiers[1].ResetAt)
	require.Equal(t, "", s.Tiers[2].ResetAt)
}

// compshare：纯数字字符串与 RFC3339 字符串混合。
func TestWebConsoleQuotaFlexTimestamp_CompshareNumericString(t *testing.T) {
	data := `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,` +
		`"limit_5h":100,"used_5h":10,"reset_5h":"1794344400",` +
		`"limit_week":500,"used_week":100,"reset_week":"2026-11-15T16:00:00Z",` +
		`"limit_month":2000,"used_month":500,"reset_month":""}]}`

	s, err := BuildWebConsoleQuotaSnapshot("compshare", []byte(data), "", nil, wcqNow)
	require.NoError(t, err)
	require.Len(t, s.Tiers, 3)
	require.Equal(t, "2026-11-10T21:00:00Z", s.Tiers[0].ResetAt)
	require.Equal(t, "2026-11-15T16:00:00Z", s.Tiers[1].ResetAt)
	require.Equal(t, "", s.Tiers[2].ResetAt)
}

// compshare：5h 耗尽且整数 reset_5h 在未来 → Exhausted 且 Until 为该 UTC 时间。
func TestWebConsoleQuotaFlexTimestamp_CompshareExhaustedIntegerReset(t *testing.T) {
	// 1794398400 = 2026-11-11T12:00:00Z（= wcqNow），取其后 1 小时
	const reset5h = int64(1794402000)
	require.Greater(t, reset5h, wcqNow.Unix())

	data := `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,` +
		`"limit_5h":100,"used_5h":120,"reset_5h":1794402000,` +
		`"limit_week":500,"used_week":100,"reset_week":0,` +
		`"limit_month":2000,"used_month":500,"reset_month":0}]}`

	s, err := BuildWebConsoleQuotaSnapshot("compshare", []byte(data), "", nil, wcqNow)
	require.NoError(t, err)
	require.True(t, s.Exhausted)
	require.False(t, s.UntilIndefinite)
	require.Equal(t, wcqEpochRFC3339(reset5h), s.Until)
	require.Equal(t, "2026-11-11T13:00:00Z", s.Until)
}

// compshare：毫秒级整数正确换算（不得变成公元 58800 年）。
func TestWebConsoleQuotaFlexTimestamp_CompshareMillis(t *testing.T) {
	data := `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,` +
		`"limit_5h":100,"used_5h":10,"reset_5h":1794344400000,` +
		`"limit_week":500,"used_week":100,"reset_week":"1794344400123",` +
		`"limit_month":2000,"used_month":500,"reset_month":0}]}`

	s, err := BuildWebConsoleQuotaSnapshot("compshare", []byte(data), "", nil, wcqNow)
	require.NoError(t, err)
	require.Len(t, s.Tiers, 3)
	require.Equal(t, "2026-11-10T21:00:00Z", s.Tiers[0].ResetAt)
	require.Equal(t, "2026-11-10T21:00:00Z", s.Tiers[1].ResetAt)
	require.Equal(t, "", s.Tiers[2].ResetAt)
	// 毫秒值若被误当秒会得到 58830 年，这里显式排除
	for _, tier := range s.Tiers {
		require.NotContains(t, tier.ResetAt, "5883")
	}
}

// sensenova：window_5h.reset_at 与 nearest_grant_expiry 为整数 Unix 秒。
func TestWebConsoleQuotaFlexTimestamp_SensenovaUnixSeconds(t *testing.T) {
	const reset5h = int64(1794402000) // 2026-11-11T13:00:00Z
	const expiry = int64(1794758400)  // 2026-11-15T16:00:00Z
	data := `{"auth_state":"ok","pools":[{"id":"g","pool_type":"general",` +
		`"window_5h":{"limit":100,"remaining":0,"reset_at":1794402000},` +
		`"window_7d":{"limit":0,"remaining":0,"reset_at":0},` +
		`"grant_balance":0,"nearest_grant_expiry":1794758400}]}`

	s, err := BuildWebConsoleQuotaSnapshot("sensenova", []byte(data), "", map[string]string{"a": "ds-pro"}, wcqNow)
	require.NoError(t, err)
	require.NotNil(t, s)
	require.Len(t, s.Pools, 1)
	require.Equal(t, wcqEpochRFC3339(expiry), s.Pools[0].NearestGrantExpiry)
	require.NotNil(t, s.Pools[0].Window5h)
	require.Equal(t, wcqEpochRFC3339(reset5h), s.Pools[0].Window5h.ResetAt)

	require.True(t, s.Exhausted)
	require.False(t, s.UntilIndefinite)
	require.Equal(t, "2026-11-11T13:00:00Z", s.Until)
	require.Len(t, s.Tiers, 2)
	require.Equal(t, "2026-11-11T13:00:00Z", s.Tiers[0].ResetAt)
}

// 非法 JSON 类型（布尔/对象/数组）不得静默吞错。
func TestWebConsoleQuotaFlexTimestamp_InvalidType(t *testing.T) {
	for _, bad := range []string{`true`, `{"a":1}`, `[1]`} {
		data := `{"auth_state":"ok","plans":[{"plan_name":"Basic","expire_at":1893456000,"plan_status":1,` +
			`"limit_5h":100,"used_5h":10,"reset_5h":` + bad + `}]}`
		_, err := BuildWebConsoleQuotaSnapshot("compshare", []byte(data), "", nil, wcqNow)
		require.Error(t, err, "bad payload: %s", bad)
	}
}

// ---------------------------------------------------------------------------
// iflytek（讯飞星辰 MaaS，方案 §3.2 ④⑤）
// ---------------------------------------------------------------------------

// 零值样本（派发单输入契约）：余额 0、Token Plan 全 0、无 Coding Plan。
func TestBuildWebConsoleQuotaSnapshot_Iflytek_ZeroBalanceNoPlan(t *testing.T) {
	data := `{
		"auth_state":"ok",
		"auth_reason":"",
		"auth_error":"",
		"account_label":"139****877",
		"balance":{"balance":0,"delinquent":0,"virtualBalance":0},
		"token_plan_quota":{"quotas":[{"seatType":1,"seatTypeName":"标准成员","totalCount":0,"assignedCount":0,"remainingCount":0}]},
		"token_plan_seats":{"seats":[],"total":0},
		"coding_plan_list":{"page":1,"size":10,"total":0,"rows":[]}
	}`
	s, err := BuildWebConsoleQuotaSnapshot(providerIflytek, []byte(data), "", nil, wcqNow)
	require.NoError(t, err)
	require.NotNil(t, s)

	require.NotNil(t, s.Balance)
	require.Equal(t, 0.0, *s.Balance)
	require.Equal(t, "CNY", s.BalanceCurrency)
	require.NotNil(t, s.Delinquent)
	require.Equal(t, 0.0, *s.Delinquent)
	require.NotNil(t, s.VirtualBalance)
	require.Equal(t, 0.0, *s.VirtualBalance)

	require.NotNil(t, s.TokenPlan)
	require.Equal(t, 0.0, s.TokenPlan.RemainingCount)
	require.Len(t, s.TokenPlan.BySeat, 1)

	require.Empty(t, s.CodingPlanTiers)
	require.False(t, s.Exhausted)
}

// 余额非零、Token Plan 多档席位：字段与汇总正确。
func TestBuildWebConsoleQuotaSnapshot_Iflytek_BalanceAndTokenPlan(t *testing.T) {
	data := `{
		"auth_state":"ok",
		"account_label":"139****877",
		"balance":{"balance":12.5,"delinquent":0,"virtualBalance":3.0},
		"token_plan_quota":{"quotas":[
			{"seatType":1,"seatTypeName":"标准成员","totalCount":10,"assignedCount":4,"remainingCount":6},
			{"seatType":2,"seatTypeName":"高级成员","totalCount":5,"assignedCount":2,"remainingCount":3}
		]},
		"token_plan_seats":{"seats":[],"total":0},
		"coding_plan_list":{"page":1,"size":10,"total":0,"rows":[]}
	}`
	s, err := BuildWebConsoleQuotaSnapshot(providerIflytek, []byte(data), "", nil, wcqNow)
	require.NoError(t, err)
	require.NotNil(t, s)

	require.NotNil(t, s.Balance)
	require.Equal(t, 12.5, *s.Balance)
	require.Equal(t, "CNY", s.BalanceCurrency)
	require.NotNil(t, s.Delinquent)
	require.Equal(t, 0.0, *s.Delinquent)
	require.NotNil(t, s.VirtualBalance)
	require.Equal(t, 3.0, *s.VirtualBalance)

	require.NotNil(t, s.TokenPlan)
	require.Equal(t, 15.0, s.TokenPlan.TotalCount)
	require.Equal(t, 6.0, s.TokenPlan.AssignedCount)
	require.Equal(t, 9.0, s.TokenPlan.RemainingCount)
	require.Len(t, s.TokenPlan.BySeat, 2)

	require.Empty(t, s.CodingPlanTiers)
	require.False(t, s.Exhausted)
}

// Coding Plan 一条含 rp5h/rpw/package：3 条 tier，窗口名与 used 正确。
// 另测嵌套 codingPlanUsageDTO 回退路径。
func TestBuildWebConsoleQuotaSnapshot_Iflytek_CodingPlanTiers(t *testing.T) {
	t.Run("top_level_fields", func(t *testing.T) {
		data := `{
			"auth_state":"ok",
			"balance":{"balance":0,"delinquent":0,"virtualBalance":0},
			"token_plan_quota":{"quotas":[]},
			"coding_plan_list":{"page":1,"size":10,"total":1,"rows":[
				{
					"rp5hLimit":100,"rp5hUsage":30,
					"rpwLimit":500,"rpwUsage":200,
					"packageLimit":1000,"packageUsage":0,"packageLeft":700
				}
			]}
		}`
		s, err := BuildWebConsoleQuotaSnapshot(providerIflytek, []byte(data), "", nil, wcqNow)
		require.NoError(t, err)
		require.NotNil(t, s)
		require.Len(t, s.CodingPlanTiers, 3)

		byWindow := map[string]WebConsoleQuotaTier{}
		for _, tr := range s.CodingPlanTiers {
			byWindow[tr.Window] = tr
		}
		require.Contains(t, byWindow, "5h")
		require.Equal(t, 100.0, byWindow["5h"].Limit)
		require.Equal(t, 30.0, byWindow["5h"].Used)
		require.Contains(t, byWindow, "week")
		require.Equal(t, 500.0, byWindow["week"].Limit)
		require.Equal(t, 200.0, byWindow["week"].Used)
		require.Contains(t, byWindow, "month")
		require.Equal(t, 1000.0, byWindow["month"].Limit)
		// packageUsage=0 → used = max(packageLimit-packageLeft,0) = 300
		require.Equal(t, 300.0, byWindow["month"].Used)
		require.False(t, s.Exhausted)
	})

	t.Run("nested_dto_fallback", func(t *testing.T) {
		data := `{
			"auth_state":"ok",
			"balance":{"balance":0,"delinquent":0,"virtualBalance":0},
			"token_plan_quota":{"quotas":[]},
			"coding_plan_list":{"page":1,"size":10,"total":1,"rows":[
				{
					"codingPlanUsageDTO":{
						"rp5hLimit":100,"rp5hUsage":30,
						"rpwLimit":500,"rpwUsage":200,
						"packageLimit":1000,"packageUsage":0,"packageLeft":700
					}
				}
			]}
		}`
		s, err := BuildWebConsoleQuotaSnapshot(providerIflytek, []byte(data), "", nil, wcqNow)
		require.NoError(t, err)
		require.NotNil(t, s)
		require.Len(t, s.CodingPlanTiers, 3)

		byWindow := map[string]WebConsoleQuotaTier{}
		for _, tr := range s.CodingPlanTiers {
			byWindow[tr.Window] = tr
		}
		require.Equal(t, 100.0, byWindow["5h"].Limit)
		require.Equal(t, 30.0, byWindow["5h"].Used)
		require.Equal(t, 500.0, byWindow["week"].Limit)
		require.Equal(t, 200.0, byWindow["week"].Used)
		require.Equal(t, 1000.0, byWindow["month"].Limit)
		require.Equal(t, 300.0, byWindow["month"].Used)
	})

	t.Run("zero_limit_window_omitted", func(t *testing.T) {
		data := `{
			"auth_state":"ok",
			"balance":{"balance":0,"delinquent":0,"virtualBalance":0},
			"token_plan_quota":{"quotas":[]},
			"coding_plan_list":{"page":1,"size":10,"total":1,"rows":[
				{
					"rp5hLimit":0,"rp5hUsage":0,
					"rpwLimit":500,"rpwUsage":200,
					"packageLimit":1000,"packageUsage":400,"packageLeft":600
				}
			]}
		}`
		s, err := BuildWebConsoleQuotaSnapshot(providerIflytek, []byte(data), "", nil, wcqNow)
		require.NoError(t, err)
		require.NotNil(t, s)
		// 5h limit=0 不输出；week、month 输出
		require.Len(t, s.CodingPlanTiers, 2)
		for _, tr := range s.CodingPlanTiers {
			require.NotEqual(t, "5h", tr.Window)
		}
		// packageUsage=400>0 → used=400
		var month WebConsoleQuotaTier
		for _, tr := range s.CodingPlanTiers {
			if tr.Window == "month" {
				month = tr
			}
		}
		require.Equal(t, 400.0, month.Used)
	})
}

// 非法 JSON 返回 error 且不 panic。
func TestBuildWebConsoleQuotaSnapshot_Iflytek_BadJSON(t *testing.T) {
	_, err := BuildWebConsoleQuotaSnapshot(providerIflytek, []byte("not-json"), "", nil, wcqNow)
	require.Error(t, err)
}
