package xianguanjia

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// fakeSecret 仅为单测使用的占位密钥，绝非真实商户凭证。
const (
	fakeAppID     = "fake-app-id"
	fakeAppSecret = "fake-app-secret"
	fakeMchID     = "fake-mch-id"
	fakeMchSecret = "fake-mch-secret"
)

func newFakeClient(t *testing.T) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case xianyuExternalCardListPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"cards":[{"card_no":"C1","card_pwd":"P1","cost":9.9,"sold_type":"1"}]}}`))
		case xianyuAftersaleListPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"list":[{"aftersale_id":"A1","order_no":"OA","status":"refunding","refund_amount":5.5,"reason":"r"}]}}`))
		case xianyuAftersaleDetailPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"aftersale_id":"A1","order_no":"OA","status":"refunding","refund_amount":5.5,"reason":"r"}}`))
		case xianyuAftersaleAgreeRefundPath, xianyuAftersaleRejectRefundPath:
			_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(ClientConfig{
		BaseURL:    srv.URL,
		AppID:      fakeAppID,
		AppSecret:  fakeAppSecret,
		MchID:      fakeMchID,
		MchSecret:  fakeMchSecret,
		HTTPClient: srv.Client(),
	})
	return c, srv
}

func TestListOrderCards(t *testing.T) {
	c, _ := newFakeClient(t)
	cards, err := c.ListOrderCards(context.Background(), "ORDER1")
	if err != nil {
		t.Fatalf("ListOrderCards returned error: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if cards[0].CardNo != "C1" || cards[0].CardPwd != "P1" || cards[0].SoldType != "1" {
		t.Fatalf("card fields mismatch: %+v", cards[0])
	}
	if cards[0].Cost != 9.9 {
		t.Fatalf("card cost mismatch: %v", cards[0].Cost)
	}
}

func TestListAftersaleOrders(t *testing.T) {
	c, _ := newFakeClient(t)
	orders, err := c.ListAftersaleOrders(context.Background(), time.Time{}, 10)
	if err != nil {
		t.Fatalf("ListAftersaleOrders returned error: %v", err)
	}
	if len(orders) != 1 || orders[0].OrderNo != "OA" || orders[0].AftersaleID != "A1" {
		t.Fatalf("aftersale orders mismatch: %+v", orders)
	}
}

func TestGetAftersaleOrder(t *testing.T) {
	c, _ := newFakeClient(t)
	o, err := c.GetAftersaleOrder(context.Background(), "A1")
	if err != nil {
		t.Fatalf("GetAftersaleOrder returned error: %v", err)
	}
	if o == nil || o.OrderNo != "OA" {
		t.Fatalf("aftersale detail mismatch: %+v", o)
	}
}

func TestAgreeRejectRefund(t *testing.T) {
	c, _ := newFakeClient(t)
	if err := c.AgreeRefund(context.Background(), "A1"); err != nil {
		t.Fatalf("AgreeRefund returned error: %v", err)
	}
	if err := c.RejectRefund(context.Background(), "A1", "reason"); err != nil {
		t.Fatalf("RejectRefund returned error: %v", err)
	}
}

func TestSignSelfConsistent(t *testing.T) {
	params := url.Values{}
	params.Set("app_id", fakeAppID)
	params.Set("mch_id", fakeMchID)
	params.Set("timestamp", "123")
	params.Set("nonce", "n")
	body := []byte(`{"order_no":"O1"}`)

	s1 := Sign(params, body, fakeAppSecret, fakeMchSecret)
	s2 := Sign(params, body, fakeAppSecret, fakeMchSecret)
	if s1 != s2 {
		t.Fatalf("Sign should be deterministic: %q != %q", s1, s2)
	}

	// 篡改 body 后签名应不同。
	s3 := Sign(params, []byte(`{"order_no":"O2"}`), fakeAppSecret, fakeMchSecret)
	if s3 == s1 {
		t.Fatalf("Sign should change when body changes")
	}
}
