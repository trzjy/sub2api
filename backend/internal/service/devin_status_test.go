//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// encodeDevinStatusResponseForTest 用生产编码器组装合成响应字节，模拟：
//
//	#1.13.16 balanceMicro, #1.13.17 dailyReset, #1.13.18 weeklyReset,
//	#1.13.1.2 plan, fallback #2.2 plan
func encodeDevinStatusResponseForTest(balanceMicro, dailySec, weeklySec uint64, plan, fallbackPlan string) []byte {
	e := &ProtoEncoder{}
	e.Message(1, func(lvl1 *ProtoEncoder) {
		lvl1.Message(13, func(billing *ProtoEncoder) {
			if balanceMicro != 0 {
				billing.Uint64(16, balanceMicro)
			}
			if dailySec != 0 {
				billing.Uint64(17, dailySec)
			}
			if weeklySec != 0 {
				billing.Uint64(18, weeklySec)
			}
			if plan != "" {
				billing.Message(1, func(planMsg *ProtoEncoder) {
					planMsg.String(2, plan)
				})
			}
		})
	})
	if fallbackPlan != "" {
		e.Message(2, func(lvl2 *ProtoEncoder) {
			lvl2.String(2, fallbackPlan)
		})
	}
	return e.Bytes()
}

// TestDevinStatusDecodeBilling 断言账单块各字段按微美元/epoch 秒正确解码。
func TestDevinStatusDecodeBilling(t *testing.T) {
	const (
		balance   = uint64(10_000_000) // $10
		dailySec  = uint64(1_700_000_000)
		weeklySec = uint64(1_700_604_800)
	)
	data := encodeDevinStatusResponseForTest(balance, dailySec, weeklySec, "  PRO  ", "")

	got, err := decodeGetUserStatusResponse(data)
	require.NoError(t, err)
	require.NotNil(t, got.BalanceUSD)
	require.InDelta(t, 10.0, *got.BalanceUSD, 1e-9, "balanceMicro 必须除以 1e6，不可当美元直接用")
	require.NotNil(t, got.DailyResetAt)
	require.Equal(t, int64(dailySec), got.DailyResetAt.Unix())
	require.NotNil(t, got.WeeklyResetAt)
	require.Equal(t, int64(weeklySec), got.WeeklyResetAt.Unix())
	require.Equal(t, "pro", got.Plan, "plan 应 trim + lower")
}

// TestDevinStatusPlanFallback 断言主 plan 缺失时回退到 #2.2。
func TestDevinStatusPlanFallback(t *testing.T) {
	data := encodeDevinStatusResponseForTest(0, 0, 0, "", "  Free_Tier ")
	got, err := decodeGetUserStatusResponse(data)
	require.NoError(t, err)
	require.Equal(t, "free_tier", got.Plan)
	require.Nil(t, got.BalanceUSD)
}

// TestDevinStatusBalanceBounds 断言超大值与负数 balanceMicro 视为未知。
func TestDevinStatusBalanceBounds(t *testing.T) {
	cases := []struct {
		name      string
		micro     uint64
		wantKnown bool
	}{
		{name: "valid", micro: maxDevinBalanceMicro, wantKnown: true},
		{name: "too large", micro: maxDevinBalanceMicro + 1, wantKnown: false},
		{name: "negative as int64", micro: uint64(1) << 63, wantKnown: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := encodeDevinStatusResponseForTest(tc.micro, 0, 0, "", "")
			got, err := decodeGetUserStatusResponse(data)
			require.NoError(t, err)
			if tc.wantKnown {
				require.NotNil(t, got.BalanceUSD)
				require.InDelta(t, float64(tc.micro)/1e6, *got.BalanceUSD, 1e-6)
			} else {
				require.Nil(t, got.BalanceUSD)
			}
		})
	}
}

// TestDevinStatusFetchSuccess 断言请求构造（body/headers）与成功响应解码。
func TestDevinStatusFetchSuccess(t *testing.T) {
	const token = "mytoken"
	const normalized = "devin-session-token$mytoken"

	payload := encodeDevinStatusResponseForTest(15_000_000, 1_700_000_000, 0, "Pro", "")
	var gotMethod, gotPath, gotBody string
	var gotHeader http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	client := &DevinStatusClient{BaseURL: srv.URL, HTTPClient: srv.Client()}
	status, err := client.Fetch(context.Background(), token)
	require.NoError(t, err)

	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, DevinUserStatusPath, gotPath)
	require.Equal(t, "application/proto", gotHeader.Get("Content-Type"))
	require.Equal(t, "1", gotHeader.Get("Connect-Protocol-Version"))
	require.Equal(t, "*/*", gotHeader.Get("Accept"))
	require.Equal(t, "Basic "+normalized+"-"+normalized, gotHeader.Get("Authorization"))

	// 请求体应为 encodeGetUserJwtRequest(metadata)，field 1 内含 apiKey=normalized。
	bodyFields, err := scanStatusFields([]byte(gotBody))
	require.NoError(t, err)
	metaBytes, ok := firstBytesField(bodyFields, 1)
	require.True(t, ok, "请求体应包含 field 1 metadata")
	metaFields, err := scanStatusFields(metaBytes)
	require.NoError(t, err)
	apiKey, ok := firstBytesField(metaFields, 3)
	require.True(t, ok)
	require.Equal(t, normalized, string(apiKey))

	require.NotNil(t, status.BalanceUSD)
	require.InDelta(t, 15.0, *status.BalanceUSD, 1e-9)
	require.Equal(t, "pro", status.Plan)
	require.False(t, status.FetchedAt.IsZero(), "FetchedAt 应被填充")
}

// TestDevinStatusFetchNon200 断言非 200 返回错误并带上 body 前 200 字节。
func TestDevinStatusFetchNon200(t *testing.T) {
	body := strings.Repeat("A", 200) + strings.Repeat("B", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := &DevinStatusClient{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := client.Fetch(context.Background(), "tok")
	require.Error(t, err)
	require.Contains(t, err.Error(), "500")
	require.Contains(t, err.Error(), strings.Repeat("A", 200))
	require.NotContains(t, err.Error(), "B", "错误只应包含 body 前 200 字节")
}

// TestDevinStatusNewClientDefaults 断言默认客户端的 base/timeout。
func TestDevinStatusNewClientDefaults(t *testing.T) {
	c := NewDevinStatusClient()
	require.Equal(t, DevinAPIURL, c.BaseURL)
	require.NotNil(t, c.HTTPClient)
	require.Equal(t, DevinDefaultTimeout, c.HTTPClient.Timeout)
}
