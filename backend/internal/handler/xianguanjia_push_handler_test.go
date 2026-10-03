package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
)

// 以下密钥均为单测占位，绝非真实商户凭证。
const (
	testAppID     = "fake-app-id"
	testAppSecret = "fake-app-secret"
)

// fakeVoider 实现 XianguanjiaRefundVoider，用于验证作废流调用。
type fakeVoider struct {
	mu                sync.Mutex
	getClaimErr       error
	getClaimAccountID string
	processCalled     int
	processOrderNo    string
	processAccountID  string
	processStatus     string
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

// fakeDecryptor 实现 service.SecretEncryptor，单测中把密文原样当作明文返回（内存配置直接存明文 AppSecret）。
type fakeDecryptor struct{}

func (fakeDecryptor) Encrypt(plaintext string) (string, error) { return plaintext, nil }
func (fakeDecryptor) Decrypt(ciphertext string) (string, error) { return ciphertext, nil }

func newTestPushHandler(voider XianguanjiaRefundVoider, cfg *xianguanjia.Config) *XianyuXianguanjiaPushHandler {
	reader := xianguanjia.NewConfigStoreMemory(cfg)
	verifier := NewXianguanjiaSignatureVerifier(reader, fakeDecryptor{})
	idem := xianguanjia.NewPushIdempotencyStoreMemory()
	return NewXianyuXianguanjiaPushHandler(voider, idem, verifier)
}

func doPush(t *testing.T, h *XianyuXianguanjiaPushHandler, body []byte, ts int64, signValid bool) *httptest.ResponseRecorder {
	t.Helper()
	bodyMd5 := xianguanjia.BodyMd5(body)
	tsStr := strconv.FormatInt(ts, 10)
	sign := xianguanjia.Sign(testAppID, bodyMd5, tsStr, testAppSecret)
	if !signValid {
		sign = "deadbeef"
	}
	params := url.Values{}
	params.Set("appid", testAppID)
	params.Set("timestamp", tsStr)
	params.Set("sign", sign)
	req := httptest.NewRequest(http.MethodPost, "/webhook/xianguanjia?"+params.Encode(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h.Push(c)
	return w
}

func parseResult(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var r struct {
		Result string `json:"result"`
		Msg    string `json:"msg"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("response not JSON: %s", w.Body.String())
	}
	return r.Result, r.Msg
}

// activeConfig 返回一份正常的 active 内存配置（AppSecret 以明文存放，由 fakeDecryptor 还原）。
func activeConfig() *xianguanjia.Config {
	return &xianguanjia.Config{AppID: testAppID, AppSecretEncrypted: testAppSecret, Status: "active"}
}

func TestPushValidNormalStatus(t *testing.T) {
	v := &fakeVoider{getClaimAccountID: "acct-1"}
	h := newTestPushHandler(v, activeConfig())
	// int32 字段以数字形式给出（官方契约：数字而非字符串），无 cards 字段。
	body := []byte(`{"seller_id":123,"user_name":"u","order_no":"O1","order_type":7,"order_status":12,"refund_status":0,"modify_time":1636077365,"product_id":99,"item_id":88}`)
	w := doPush(t, h, body, time.Now().Unix(), true)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	result, _ := parseResult(t, w)
	if result != "success" {
		t.Fatalf("expected result=success, got %q body=%s", result, w.Body.String())
	}
	if v.processCalled != 0 {
		t.Fatalf("voider should not be called for normal status, got %d", v.processCalled)
	}
}

func TestPushBadSign(t *testing.T) {
	v := &fakeVoider{getClaimAccountID: "acct-1"}
	h := newTestPushHandler(v, activeConfig())
	body := []byte(`{"order_no":"O1","order_status":12,"refund_status":0,"modify_time":1636077365}`)
	w := doPush(t, h, body, time.Now().Unix(), false)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	result, _ := parseResult(t, w)
	if result != "fail" {
		t.Fatalf("expected result=fail on bad sign, got %q", result)
	}
	if v.processCalled != 0 {
		t.Fatalf("voider should not be called on bad sign")
	}
}

func TestPushExpiredTimestampRejected(t *testing.T) {
	v := &fakeVoider{}
	h := newTestPushHandler(v, activeConfig())
	body := []byte(`{"order_no":"O1","order_status":12,"refund_status":0,"modify_time":1636077365}`)
	expired := time.Now().Unix() - 400 // 超出 300 秒窗口
	w := doPush(t, h, body, expired, true)
	result, _ := parseResult(t, w)
	if result != "fail" {
		t.Fatalf("expected result=fail for expired timestamp, got %q", result)
	}
}

func TestPushFreshTimestampBoundary(t *testing.T) {
	v := &fakeVoider{}
	h := newTestPushHandler(v, activeConfig())
	body := []byte(`{"order_no":"O1","order_status":12,"refund_status":0,"modify_time":1636077365}`)
	// 边界内（恰好 300 秒前）应放行。
	w := doPush(t, h, body, time.Now().Unix()-300, true)
	result, _ := parseResult(t, w)
	if result != "success" {
		t.Fatalf("expected result=success at 300s boundary, got %q", result)
	}
	// 超出 1 秒（301 秒前）应拒收。
	w2 := doPush(t, h, body, time.Now().Unix()-301, true)
	r2, _ := parseResult(t, w2)
	if r2 != "fail" {
		t.Fatalf("expected result=fail at 301s, got %q", r2)
	}
}

func TestPushMissingConfigFailClosed(t *testing.T) {
	v := &fakeVoider{}
	// nil 配置模拟"无 active 配置"——必须 fail-closed 拒绝。
	h := newTestPushHandler(v, nil)
	body := []byte(`{"order_no":"O1","order_status":12,"refund_status":0,"modify_time":1636077365}`)
	w := doPush(t, h, body, time.Now().Unix(), true)
	result, _ := parseResult(t, w)
	if result != "fail" {
		t.Fatalf("expected result=fail when config missing (fail-closed), got %q", result)
	}
}

func TestPushIdempotencyThreeTimes(t *testing.T) {
	v := &fakeVoider{getClaimAccountID: "acct-1"}
	h := newTestPushHandler(v, activeConfig())
	body := []byte(`{"order_no":"O1","order_status":12,"refund_status":0,"modify_time":1636077365}`)
	ts := time.Now().Unix()
	for i := 0; i < 3; i++ {
		w := doPush(t, h, body, ts, true)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d expected 200, got %d", i, w.Code)
		}
	}
	if v.processCalled != 0 {
		t.Fatalf("voider should not be called for normal status, got %d", v.processCalled)
	}
}

func TestPushRefundTriggersVoid(t *testing.T) {
	v := &fakeVoider{getClaimAccountID: "acct-1"}
	h := newTestPushHandler(v, activeConfig())
	body := []byte(`{"order_no":"O1","order_status":12,"refund_status":5,"modify_time":1636077365}`)
	w := doPush(t, h, body, time.Now().Unix(), true)
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
	h := newTestPushHandler(v, activeConfig())
	body := []byte(`{"order_no":"O1","order_status":12,"refund_status":5,"modify_time":1636077365}`)
	w := doPush(t, h, body, time.Now().Unix(), true)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if v.processCalled != 0 {
		t.Fatalf("voider ProcessRefundEvent should not be called when no claim, got %d", v.processCalled)
	}
}

// TestPushBodyInt32Deserialization 验证推送 body 的 int32 字段能正确从数字反序列化，
// 而官方为字符串时（旧实现错误）会报错。
func TestPushBodyInt32Deserialization(t *testing.T) {
	body := []byte(`{"seller_id":123,"user_name":"u","order_no":"O1","order_type":7,"order_status":23,"refund_status":5,"modify_time":1636077365,"product_id":99,"item_id":88}`)
	var req xianguanjiaPushBody
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal int32 fields failed: %v", err)
	}
	if req.SellerID != 123 || req.OrderStatus != 23 || req.RefundStatus != 5 || req.ModifyTime != 1636077365 {
		t.Fatalf("int32 fields mismatch: %+v", req)
	}
	// 官方字段为数字；若上游给出字符串（旧实现假设），必须报错以暴露契约偏差。
	bad := []byte(`{"order_no":"O1","order_status":"23","refund_status":"5"}`)
	if err := json.Unmarshal(bad, &req); err == nil {
		t.Fatalf("expected error for string int32 fields, got nil (契约偏差未暴露)")
	}
}
