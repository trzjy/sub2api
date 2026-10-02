package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
)

// 以下密钥均为单测占位，绝非真实商户凭证。
const (
	testAppID     = "fake-app-id"
	testAppSecret = "fake-app-secret"
	testMchID     = "fake-mch-id"
	testMchSecret = "fake-mch-secret"
)

// fakeVoider 实现 XianguanjiaRefundVoider，用于验证作废流调用。
type fakeVoider struct {
	mu                 sync.Mutex
	getClaimErr        error
	getClaimAccountID  string
	processCalled      int
	processOrderNo     string
	processAccountID   string
	processStatus      string
}

func (f *fakeVoider) GetClaimAccountID(ctx context.Context, orderNo string) (string, error) {
	if f.getClaimErr != nil {
		return "", f.getClaimErr
	}
	return f.getClaimAccountID, nil
}

func (f *fakeVoider) ProcessRefundEvent(ctx context.Context, orderNo, accountID, status string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.processCalled++
	f.processOrderNo = orderNo
	f.processAccountID = accountID
	f.processStatus = status
	return "ok", nil
}

func newTestPushHandler(voider XianguanjiaRefundVoider) (*XianyuXianguanjiaPushHandler, xianguanjia.ExternalCardStore) {
	extStore := xianguanjia.NewExternalCardStoreMemory()
	idem := xianguanjia.NewPushIdempotencyStoreMemory()
	verifier := NewXianguanjiaSignatureVerifier(XianguanjiaSecretConfig{
		AppID:     testAppID,
		AppSecret: testAppSecret,
		MchID:     testMchID,
		MchSecret: testMchSecret,
	})
	return NewXianyuXianguanjiaPushHandler(voider, extStore, idem, verifier), extStore
}

func doPush(t *testing.T, h *XianyuXianguanjiaPushHandler, body []byte, signValid bool) *httptest.ResponseRecorder {
	t.Helper()
	params := url.Values{}
	params.Set("app_id", testAppID)
	params.Set("mch_id", testMchID)
	params.Set("timestamp", "123")
	params.Set("nonce", "n1")
	sign := xianguanjia.Sign(params, body, testAppSecret, testMchSecret)
	if !signValid {
		sign = "deadbeef"
	}
	params.Set("sign", sign)
	path := "/webhook/xianguanjia?" + params.Encode()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h.Push(c)
	return w
}

func TestPushValidNormalStatus(t *testing.T) {
	v := &fakeVoider{getClaimAccountID: "acct-1"}
	h, extStore := newTestPushHandler(v)
	body := []byte(`{"order_no":"O1","refund_status":"0","order_status":"1","modify_time":"t1","cards":[{"card_no":"C1","card_pwd":"P1","sold_type":"1"}]}`)
	w := doPush(t, h, body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	cards, err := extStore.GetExternalCards(context.Background(), "O1")
	if err != nil {
		t.Fatalf("GetExternalCards error: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	plain, err := xianguanjia.DecryptCardPwd(cards[0].CardPwdEncrypted)
	if err != nil {
		t.Fatalf("DecryptCardPwd error: %v", err)
	}
	if plain != "P1" {
		t.Fatalf("card pwd mismatch: %q", plain)
	}
	if v.processCalled != 0 {
		t.Fatalf("voider should not be called for normal status, got %d", v.processCalled)
	}
}

func TestPushBadSign(t *testing.T) {
	v := &fakeVoider{getClaimAccountID: "acct-1"}
	h, extStore := newTestPushHandler(v)
	body := []byte(`{"order_no":"O1","refund_status":"0","order_status":"1","modify_time":"t1","cards":[{"card_no":"C1","card_pwd":"P1","sold_type":"1"}]}`)
	w := doPush(t, h, body, false)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if v.processCalled != 0 {
		t.Fatalf("voider should not be called on bad sign")
	}
	cards, _ := extStore.GetExternalCards(context.Background(), "O1")
	if len(cards) != 0 {
		t.Fatalf("extStore should be empty on bad sign, got %d", len(cards))
	}
}

func TestPushIdempotencyThreeTimes(t *testing.T) {
	v := &fakeVoider{getClaimAccountID: "acct-1"}
	h, extStore := newTestPushHandler(v)
	body := []byte(`{"order_no":"O1","refund_status":"0","order_status":"1","modify_time":"t1","cards":[{"card_no":"C1","card_pwd":"P1","sold_type":"1"}]}`)
	for i := 0; i < 3; i++ {
		w := doPush(t, h, body, true)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d expected 200, got %d", i, w.Code)
		}
	}
	cards, _ := extStore.GetExternalCards(context.Background(), "O1")
	if len(cards) != 1 {
		t.Fatalf("expected exactly 1 card (idempotent), got %d", len(cards))
	}
	if v.processCalled != 0 {
		t.Fatalf("voider should not be called for normal status, got %d", v.processCalled)
	}
}

func TestPushRefundTriggersVoid(t *testing.T) {
	v := &fakeVoider{getClaimAccountID: "acct-1"}
	h, _ := newTestPushHandler(v)
	body := []byte(`{"order_no":"O1","refund_status":"5","order_status":"1","modify_time":"t1"}`)
	w := doPush(t, h, body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if v.processCalled != 1 {
		t.Fatalf("expected ProcessRefundEvent called once, got %d", v.processCalled)
	}
	if v.processOrderNo != "O1" || v.processAccountID != "acct-1" || v.processStatus != "refunded" {
		t.Fatalf("ProcessRefundEvent args mismatch: %+v", v)
	}
}

func TestPushRefundNoClaim(t *testing.T) {
	v := &fakeVoider{getClaimErr: service.ErrXianyuDeliveryClaimNotFound}
	h, _ := newTestPushHandler(v)
	body := []byte(`{"order_no":"O1","refund_status":"5","order_status":"1","modify_time":"t1"}`)
	w := doPush(t, h, body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if v.processCalled != 0 {
		t.Fatalf("voider ProcessRefundEvent should not be called when no claim, got %d", v.processCalled)
	}
}
