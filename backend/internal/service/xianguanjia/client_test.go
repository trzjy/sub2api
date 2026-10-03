package xianguanjia

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 以下密钥均为单测占位，绝非真实商户凭证。
const (
	fakeAppKey     = "fake-app-key"
	fakeAppSecret  = "fake-app-secret"
)

// independentMd5 用最直白的方式重算四段逗号 md5，作为 Sign 的"独立向量"对照。
func independentMd5(appKey, bodyMd5, ts, appSecret string) string {
	raw := appKey + "," + bodyMd5 + "," + ts + "," + appSecret
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func newFakeClient(t *testing.T) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 全部接口必须是 POST。
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// 服务端独立重算签名并比对，验证客户端确实按四段官方公式签名。
		rawBody, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		q := r.URL.Query()
		bodyMd5 := BodyMd5(rawBody)
		expected := Sign(q.Get("appid"), bodyMd5, q.Get("timestamp"), fakeAppSecret)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(q.Get("sign"))) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"sign error"}`))
			return
		}
		// query 必须只含 appid/timestamp/sign（无 mch/nonce/app_id）。
		if q.Get("mch_id") != "" || q.Get("nonce") != "" || q.Get("app_id") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case pathKamList:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"cards":[{"card_no":"C1","card_pwd":"P1","cost":9.9,"sold_type":"1"}]}}`))
		case pathRefundAgree:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		case pathRefundRefused:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		case pathDummySend:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(ClientConfig{
		BaseURL:    srv.URL,
		AppKey:     fakeAppKey,
		AppSecret:  fakeAppSecret,
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

func TestAgreeRefund(t *testing.T) {
	c, _ := newFakeClient(t)
	if err := c.AgreeRefund(context.Background(), "ORDER1"); err != nil {
		t.Fatalf("AgreeRefund returned error: %v", err)
	}
}

func TestRejectRefund(t *testing.T) {
	c, _ := newFakeClient(t)
	if err := c.RejectRefund(context.Background(), "ORDER1", "reason-x"); err != nil {
		t.Fatalf("RejectRefund returned error: %v", err)
	}
}

func TestDummySend(t *testing.T) {
	c, _ := newFakeClient(t)
	if err := c.DummySend(context.Background(), "ORDER1", 2); err != nil {
		t.Fatalf("DummySend returned error: %v", err)
	}
}

func TestSignFourSegmentVector(t *testing.T) {
	appKey := "ak"
	bodyMd5 := BodyMd5([]byte(`{"order_no":"O1"}`))
	ts := "1700000000"
	appSecret := "sk"
	got := Sign(appKey, bodyMd5, ts, appSecret)
	want := independentMd5(appKey, bodyMd5, ts, appSecret)
	if got != want {
		t.Fatalf("Sign four-segment vector mismatch: got %q want %q", got, want)
	}
	// 篡改任意一段都应改变结果。
	if Sign("ak2", bodyMd5, ts, appSecret) == got {
		t.Fatalf("changing appKey should change sign")
	}
	if Sign(appKey, Md5Hex([]byte(`{"order_no":"O2"}`)), ts, appSecret) == got {
		t.Fatalf("changing bodyMd5 should change sign")
	}
	if Sign(appKey, bodyMd5, "1700000001", appSecret) == got {
		t.Fatalf("changing timestamp should change sign")
	}
	if Sign(appKey, bodyMd5, ts, "sk2") == got {
		t.Fatalf("changing appSecret should change sign")
	}
}

func TestBodyMd5UsesCompactJSON(t *testing.T) {
	// 空 body → md5("{}")
	if got, want := BodyMd5(nil), independentMd5Body("{}"); got != want {
		t.Fatalf("empty body md5 mismatch: got %q want %q", got, want)
	}
	if got, want := BodyMd5([]byte("")), independentMd5Body("{}"); got != want {
		t.Fatalf("empty body md5 mismatch: got %q want %q", got, want)
	}
	// 压缩 JSON（无空格）的 md5。
	compact := `{"a":1,"b":2}`
	if got, want := BodyMd5([]byte(compact)), independentMd5Body(compact); got != want {
		t.Fatalf("compact json md5 mismatch: got %q want %q", got, want)
	}
}

func independentMd5Body(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
