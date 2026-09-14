package provider

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func xunhupayTestConfig() map[string]string {
	return map[string]string{
		"appId":     "test_appid_123",
		"appSecret": "test_appsecret",
		"apiBase":   "https://api.xunhupay.com",
		"notifyUrl": "https://corealgos.com/api/v1/payment/webhook/xunhupay",
		"returnUrl": "https://corealgos.com/payment/result",
	}
}

func TestXunhupaySignDeterministic(t *testing.T) {
	t.Parallel()
	params := map[string]string{
		"appid":           "201906187599",
		"trade_order_id":  "ORDER123",
		"total_fee":       "10.00",
		"nonce_str":       "abc",
		"time":            "1700000000",
	}
	secret := "mysecret"
	sign1 := xunhupaySign(params, secret)
	sign2 := xunhupaySign(params, secret)
	if sign1 != sign2 {
		t.Fatalf("xunhupaySign should be deterministic: %q != %q", sign1, sign2)
	}
	if len(sign1) != 32 {
		t.Fatalf("MD5 hex should be 32 chars, got %d", len(sign1))
	}
	// Official reference example: appid=test123&nonce_str=abc&time=1700000000&total_fee=9.90 + mysecret
	ref := map[string]string{
		"appid":     "test123",
		"nonce_str": "abc",
		"time":      "1700000000",
		"total_fee": "9.90",
	}
	refSign := xunhupaySign(ref, "mysecret")
	// Independent recomputation of the documented canonical string.
	want := md5Hex("appid=test123&nonce_str=abc&time=1700000000&total_fee=9.90mysecret")
	if refSign != want {
		t.Fatalf("reference signature mismatch: got %q want %q", refSign, want)
	}
}

func TestXunhupaySignOrderIndependent(t *testing.T) {
	t.Parallel()
	secret := "s3cret"
	p1 := map[string]string{"a": "1", "b": "2", "c": "3"}
	p2 := map[string]string{"c": "3", "a": "1", "b": "2"}
	if xunhupaySign(p1, secret) != xunhupaySign(p2, secret) {
		t.Fatal("xunhupaySign should be key-order independent")
	}
}

func TestXunhupaySignExcludesHashAndEmpty(t *testing.T) {
	t.Parallel()
	secret := "s3cret"
	base := map[string]string{"a": "1", "b": "2"}
	withExtras := map[string]string{"a": "1", "b": "2", "hash": "should_be_ignored", "empty": ""}
	if xunhupaySign(base, secret) != xunhupaySign(withExtras, secret) {
		t.Fatal("xunhupaySign should exclude the hash field and empty values")
	}
}

func TestXunhupayVerifySign(t *testing.T) {
	t.Parallel()
	params := map[string]string{
		"appid":          "201906187599",
		"trade_order_id": "ORDER456",
		"total_fee":      "25.00",
		"status":         "OD",
	}
	secret := "secret"
	sign := xunhupaySign(params, secret)
	params["hash"] = sign
	if !xunhupayVerifySign(params, secret, sign) {
		t.Fatal("xunhupayVerifySign should accept a valid signature")
	}
	// Tampered amount must fail.
	tampered := map[string]string{
		"appid":          "201906187599",
		"trade_order_id": "ORDER456",
		"total_fee":      "99.99",
		"status":         "OD",
		"hash":           sign,
	}
	if xunhupayVerifySign(tampered, secret, sign) {
		t.Fatal("xunhupayVerifySign should reject tampered params")
	}
}

func TestXunhupayVerifyNotificationSuccess(t *testing.T) {
	t.Parallel()
	p, err := NewXunhupay("inst1", xunhupayTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{
		"appid":           "test_appid_123",
		"trade_order_id":  "ORDER789",
		"total_fee":       "10.50",
		"transaction_id":  "TX999",
		"open_order_id":   "HPJ2024",
		"order_title":     "充值",
		"status":          "OD",
		"time":            "1700000000",
		"nonce_str":       "xyz",
	}
	params["hash"] = xunhupaySign(params, "test_appsecret")

	raw := url.Values{}
	for k, v := range params {
		raw.Set(k, v)
	}

	notif, err := p.VerifyNotification(t.Context(), raw.Encode(), nil)
	if err != nil {
		t.Fatalf("VerifyNotification: %v", err)
	}
	if notif.Status != payment.ProviderStatusSuccess {
		t.Fatalf("status = %q, want success", notif.Status)
	}
	if notif.OrderID != "ORDER789" {
		t.Fatalf("orderID = %q, want ORDER789", notif.OrderID)
	}
	if notif.TradeNo != "TX999" {
		t.Fatalf("tradeNo = %q, want TX999", notif.TradeNo)
	}
	if notif.Amount != 10.50 {
		t.Fatalf("amount = %v, want 10.50", notif.Amount)
	}
	if notif.Metadata["appid"] != "test_appid_123" {
		t.Fatalf("metadata appid = %q", notif.Metadata["appid"])
	}
}

func TestXunhupayVerifyNotificationRejectsTampered(t *testing.T) {
	t.Parallel()
	p, err := NewXunhupay("inst1", xunhupayTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{
		"appid":          "test_appid_123",
		"trade_order_id": "ORDER789",
		"total_fee":      "10.50",
		"status":         "OD",
	}
	params["hash"] = xunhupaySign(params, "test_appsecret")
	// Tamper with fee after signing.
	params["total_fee"] = "999.00"

	raw := url.Values{}
	for k, v := range params {
		raw.Set(k, v)
	}
	if _, err := p.VerifyNotification(t.Context(), raw.Encode(), nil); err == nil {
		t.Fatal("VerifyNotification should reject tampered signature")
	}
}

func TestXunhupayVerifyNotificationPendingStatus(t *testing.T) {
	t.Parallel()
	p, err := NewXunhupay("inst1", xunhupayTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{
		"appid":          "test_appid_123",
		"trade_order_id": "ORDER790",
		"total_fee":      "10.50",
		"status":         "WP",
	}
	params["hash"] = xunhupaySign(params, "test_appsecret")
	raw := url.Values{}
	for k, v := range params {
		raw.Set(k, v)
	}
	notif, err := p.VerifyNotification(t.Context(), raw.Encode(), nil)
	if err != nil {
		t.Fatalf("VerifyNotification: %v", err)
	}
	if notif.Status != payment.ProviderStatusFailed {
		t.Fatalf("status = %q, want failed for WP", notif.Status)
	}
}

func TestXunhupayCreatePayment(t *testing.T) {
	t.Parallel()
	var gotBody url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotBody = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"success","data":{"url":"https://pay.xunhupay.com/p","url_qrcode":"https://pay.xunhupay.com/q","open_order_id":"HPJ2024"}}`))
	}))
	defer srv.Close()

	cfg := xunhupayTestConfig()
	cfg["apiBase"] = srv.URL
	p, err := NewXunhupay("inst1", cfg)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := p.CreatePayment(t.Context(), payment.CreatePaymentRequest{
		OrderID:     "ORDER1",
		Amount:      "19.90",
		PaymentType: payment.TypeWxpay,
		Subject:     "账号充值",
		IsMobile:    true,
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if resp.TradeNo != "HPJ2024" {
		t.Fatalf("tradeNo = %q, want HPJ2024", resp.TradeNo)
	}
	if resp.PayURL != "https://pay.xunhupay.com/p" {
		t.Fatalf("payUrl = %q", resp.PayURL)
	}
	if resp.QRCode != "https://pay.xunhupay.com/q" {
		t.Fatalf("qrCode = %q", resp.QRCode)
	}
	if gotBody.Get("appid") != "test_appid_123" {
		t.Fatalf("appid = %q", gotBody.Get("appid"))
	}
	if gotBody.Get("total_fee") != "19.90" {
		t.Fatalf("total_fee = %q", gotBody.Get("total_fee"))
	}
	if gotBody.Get("trade_order_id") != "ORDER1" {
		t.Fatalf("trade_order_id = %q", gotBody.Get("trade_order_id"))
	}
	if gotBody.Get("type") != "WAP" {
		t.Fatalf("type = %q, want WAP for mobile", gotBody.Get("type"))
	}
	if gotBody.Get("notify_url") != cfg["notifyUrl"] {
		t.Fatalf("notify_url = %q", gotBody.Get("notify_url"))
	}
	// Verify request signature with the app secret.
	if !xunhupayVerifySign(formToMap(gotBody), "test_appsecret", gotBody.Get("hash")) {
		t.Fatal("CreatePayment request signature invalid")
	}
}

func TestXunhupayCreatePaymentError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":1,"errmsg":"invalid sign!"}`))
	}))
	defer srv.Close()
	cfg := xunhupayTestConfig()
	cfg["apiBase"] = srv.URL
	p, err := NewXunhupay("inst1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.CreatePayment(t.Context(), payment.CreatePaymentRequest{
		OrderID: "ORDER1", Amount: "1.00", PaymentType: payment.TypeWxpay, Subject: "充值",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid sign") {
		t.Fatalf("expected error containing 'invalid sign', got %v", err)
	}
}

func TestXunhupayQueryOrderPaid(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"success","data":{"open_order_id":"HPJ2024","out_trade_order":"ORDER1","status":"OD","total_amount":"10.00","transaction_id":"TX1"}}`))
	}))
	defer srv.Close()
	cfg := xunhupayTestConfig()
	cfg["apiBase"] = srv.URL
	p, err := NewXunhupay("inst1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.QueryOrder(t.Context(), "ORDER1")
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if resp.Status != payment.ProviderStatusPaid {
		t.Fatalf("status = %q, want paid", resp.Status)
	}
	if resp.Amount != 10.00 {
		t.Fatalf("amount = %v, want 10.00", resp.Amount)
	}
}

func TestXunhupayQueryOrderPending(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"success","data":{"status":"WP","total_amount":"10.00"}}`))
	}))
	defer srv.Close()
	cfg := xunhupayTestConfig()
	cfg["apiBase"] = srv.URL
	p, err := NewXunhupay("inst1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.QueryOrder(t.Context(), "ORDER1")
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if resp.Status != payment.ProviderStatusPending {
		t.Fatalf("status = %q, want pending", resp.Status)
	}
}

func TestXunhupayRefund(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/payment/refund.html" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.PostForm.Get("trade_order_id") == "" {
			t.Errorf("expected trade_order_id in refund request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"success","refund_status":"CD"}`))
	}))
	defer srv.Close()
	cfg := xunhupayTestConfig()
	cfg["apiBase"] = srv.URL
	p, err := NewXunhupay("inst1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Refund(t.Context(), payment.RefundRequest{
		OrderID: "ORDER1",
		Amount:  "10.00",
		Reason:  "用户申请退款",
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if resp.Status != payment.ProviderStatusSuccess {
		t.Fatalf("status = %q, want success", resp.Status)
	}
}

func TestXunhupayRefundInProgress(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"success","refund_status":"RD"}`))
	}))
	defer srv.Close()
	cfg := xunhupayTestConfig()
	cfg["apiBase"] = srv.URL
	p, err := NewXunhupay("inst1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Refund(t.Context(), payment.RefundRequest{OrderID: "ORDER1", Amount: "10.00"})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if resp.Status != payment.ProviderStatusPending {
		t.Fatalf("status = %q, want pending for RD", resp.Status)
	}
}

func TestXunhupayRefundNotFoundFallsThrough(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("trade_order_id") != "" {
			// First attempt (by order id) fails with order not found.
			_, _ = w.Write([]byte(`{"errcode":1,"errmsg":"订单编号不存在"}`))
			return
		}
		// Second attempt (by open order id) succeeds.
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"success","refund_status":"CD"}`))
	}))
	defer srv.Close()
	cfg := xunhupayTestConfig()
	cfg["apiBase"] = srv.URL
	p, err := NewXunhupay("inst1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Refund(t.Context(), payment.RefundRequest{
		OrderID: "ORDER1",
		TradeNo: "HPJ2024",
		Amount:  "10.00",
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if resp.Status != payment.ProviderStatusSuccess {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

func TestXunhupayNewMissingConfig(t *testing.T) {
	t.Parallel()
	cfg := xunhupayTestConfig()
	delete(cfg, "appSecret")
	if _, err := NewXunhupay("inst1", cfg); err == nil {
		t.Fatal("NewXunhupay should fail without appSecret")
	}
}

func TestXunhupayMerchantIdentityMetadata(t *testing.T) {
	t.Parallel()
	p := &Xunhupay{config: map[string]string{"appId": "201906187599"}}
	md := p.MerchantIdentityMetadata()
	if md["appid"] != "201906187599" {
		t.Fatalf("metadata appid = %q", md["appid"])
	}
}

func TestXunhupaySupportedTypes(t *testing.T) {
	t.Parallel()
	p, err := NewXunhupay("inst1", xunhupayTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	types := p.SupportedTypes()
	if len(types) != 1 || types[0] != payment.TypeWxpay {
		t.Fatalf("supported types = %v, want [wxpay]", types)
	}
}

// --- helpers ---

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func formToMap(v url.Values) map[string]string {
	out := make(map[string]string, len(v))
	for k := range v {
		out[k] = v.Get(k)
	}
	return out
}
