package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// fakeVoider 实现 XianguanjiaRefundVoider，记录调用并可配置失败/结果。
type fakeVoider struct {
	mu            sync.Mutex
	outcome       xianguanjia.VoidOutcome
	err           error
	calls         int
	lastOrderNo   string
}

func (f *fakeVoider) VoidRefundedCards(ctx context.Context, orderNo string) (xianguanjia.VoidOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastOrderNo = orderNo
	return f.outcome, f.err
}

// fakeDecryptor 实现 service.SecretEncryptor，单测中把密文原样当作明文返回（内存配置直接存明文 AppSecret）。
type fakeDecryptor struct{}

func (fakeDecryptor) Encrypt(plaintext string) (string, error) { return plaintext, nil }
func (fakeDecryptor) Decrypt(ciphertext string) (string, error) { return ciphertext, nil }

func newTestPushHandler(voider XianguanjiaRefundVoider, cfg *xianguanjia.Config) (*XianyuXianguanjiaPushHandler, xianguanjia.PushIdempotencyStore) {
	reader := xianguanjia.NewConfigStoreMemory(cfg)
	verifier := NewXianguanjiaSignatureVerifier(reader, fakeDecryptor{})
	idem := xianguanjia.NewPushIdempotencyStoreMemory()
	return NewXianyuXianguanjiaPushHandler(voider, idem, verifier), idem
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

func refundPushBody(orderNo string, orderStatus, refundStatus int32) []byte {
	return []byte(fmt.Sprintf(`{"seller_id":123,"user_name":"u","order_no":%q,"order_type":7,"order_status":%d,"refund_status":%d,"modify_time":1636077365,"product_id":99,"item_id":88}`,
		orderNo, orderStatus, refundStatus))
}

// receiptExists 直查内存幂等存储（验证「回执是否落库」这一断言的核心）。
func receiptExists(t *testing.T, idem xianguanjia.PushIdempotencyStore, orderNo, orderStatus, refundStatus string) bool {
	t.Helper()
	exists, err := idem.HasReceipt(context.Background(), orderNo, refundStatus, orderStatus, "1636077365")
	if err != nil {
		t.Fatalf("HasReceipt failed: %v", err)
	}
	return exists
}

func TestPushValidNormalStatus(t *testing.T) {
	v := &fakeVoider{}
	h, idem := newTestPushHandler(v, activeConfig())
	// 普通状态（待发货）：不触发作废，落回执。
	body := refundPushBody("O1", 12, 0)
	w := doPush(t, h, body, time.Now().Unix(), true)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	result, _ := parseResult(t, w)
	if result != "success" {
		t.Fatalf("expected result=success, got %q body=%s", result, w.Body.String())
	}
	if v.calls != 0 {
		t.Fatalf("voider should not be called for normal status, got %d", v.calls)
	}
	if !receiptExists(t, idem, "O1", "12", "0") {
		t.Fatalf("normal status push should commit receipt")
	}
}

func TestPushBadSign(t *testing.T) {
	v := &fakeVoider{}
	h, _ := newTestPushHandler(v, activeConfig())
	body := refundPushBody("O1", 12, 0)
	w := doPush(t, h, body, time.Now().Unix(), false)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	result, _ := parseResult(t, w)
	if result != "fail" {
		t.Fatalf("expected result=fail on bad sign, got %q", result)
	}
	if v.calls != 0 {
		t.Fatalf("voider should not be called on bad sign")
	}
}

func TestPushExpiredTimestampRejected(t *testing.T) {
	v := &fakeVoider{}
	h, _ := newTestPushHandler(v, activeConfig())
	body := refundPushBody("O1", 12, 0)
	expired := time.Now().Unix() - 400 // 超出 300 秒窗口
	w := doPush(t, h, body, expired, true)
	result, _ := parseResult(t, w)
	if result != "fail" {
		t.Fatalf("expected result=fail for expired timestamp, got %q", result)
	}
}

func TestPushFreshTimestampBoundary(t *testing.T) {
	v := &fakeVoider{}
	h, _ := newTestPushHandler(v, activeConfig())
	body := refundPushBody("O1", 12, 0)
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
	h, _ := newTestPushHandler(v, nil)
	body := refundPushBody("O1", 12, 0)
	w := doPush(t, h, body, time.Now().Unix(), true)
	result, _ := parseResult(t, w)
	if result != "fail" {
		t.Fatalf("expected result=fail when config missing (fail-closed), got %q", result)
	}
}

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

// ---- D2 核心场景：资金闭环 ----

// TestPushVoidSuccessReceiptThenDedup 作废成功 → 回执落库 → 重推去重返回 success。
func TestPushVoidSuccessReceiptThenDedup(t *testing.T) {
	v := &fakeVoider{outcome: xianguanjia.VoidDone}
	h, _ := newTestPushHandler(v, activeConfig())
	body := refundPushBody("O1", 12, 5)

	// 第一次推送：作废成功，落回执。
	w := doPush(t, h, body, time.Now().Unix(), true)
	if result, _ := parseResult(t, w); result != "success" {
		t.Fatalf("first push expected success, got %q body=%s", result, w.Body.String())
	}
	if v.calls != 1 {
		t.Fatalf("expected voider called once, got %d", v.calls)
	}

	// 重推（同 order_no/refund_status/order_status/modify_time）：去重，不重做作废。
	w2 := doPush(t, h, body, time.Now().Unix(), true)
	if result, _ := parseResult(t, w2); result != "success" {
		t.Fatalf("retry push expected success, got %q", result)
	}
	if v.calls != 1 {
		t.Fatalf("duplicate push must NOT redo void, voider calls=%d", v.calls)
	}
}

// TestPushVoidFailureNoReceiptThenRetryRedoesVoid 作废失败 → result=fail → 不落回执 →
// 重推会重做作废（外审 F1 资金损失链的闭环验证）。
func TestPushVoidFailureNoReceiptThenRetryRedoesVoid(t *testing.T) {
	v := &fakeVoider{err: errors.New("kam list unreachable")}
	h, idem := newTestPushHandler(v, activeConfig())
	body := refundPushBody("O1", 12, 5)

	// 第一次推送：作废失败 → fail。
	w := doPush(t, h, body, time.Now().Unix(), true)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 (official判定靠 result 字段), got %d", w.Code)
	}
	result, msg := parseResult(t, w)
	if result != "fail" {
		t.Fatalf("void failure must return result=fail, got %q body=%s", result, w.Body.String())
	}
	if msg == "" {
		t.Fatalf("fail body must carry msg")
	}
	if v.calls != 1 {
		t.Fatalf("expected voider called once, got %d", v.calls)
	}

	// 关键断言：失败不落回执（否则重推被去重 = 作废永久丢失）。
	if receiptExists(t, idem, "O1", "12", "5") {
		t.Fatalf("FATAL: receipt recorded on void failure — 重推会被去重，作废永久丢失（F1 复现）")
	}

	// 闲管家重推（官方契约：失败最多重试 3 次）：作废恢复 → 重做成功 → 落回执。
	v.err = nil
	v.outcome = xianguanjia.VoidDone
	w2 := doPush(t, h, body, time.Now().Unix(), true)
	if result, _ := parseResult(t, w2); result != "success" {
		t.Fatalf("retry after recovery expected success, got %q", result)
	}
	if v.calls != 2 {
		t.Fatalf("retry must RE-DO void (not dedup), voider calls=%d", v.calls)
	}
	if !receiptExists(t, idem, "O1", "12", "5") {
		t.Fatalf("receipt must be recorded after successful retry")
	}
}

// TestPushKamListCardNoMatch 用真实 RefundCardVoidService + httptest mock kam/list：
// mock 返回 card_no X → 池内 X 被作废、Y 不动。
func TestPushKamListCardNoMatch(t *testing.T) {
	// mock kam/list 服务器：验证签名后返回 card_no X。
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rawBody, _ := io_ReadAll(r)
		q := r.URL.Query()
		expected := xianguanjia.Sign(q.Get("appid"), xianguanjia.BodyMd5(rawBody), q.Get("timestamp"), testAppSecret)
		if expected != q.Get("sign") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/open/order/kam/list" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"list":[{"card_no":"CARD-X","card_pwd":"pwd-x","cost":9.9,"sold_type":11}]}}`))
	}))
	t.Cleanup(srv.Close)

	// 内存池：CARD-X（应作废）、CARD-Y（不动）。
	codes := xianguanjia.NewExternalCardStoreMemory()
	_ = codes // 仅保证编译引用（ExternalCardStore 是映射表，非池卡仓库）；池卡断言走 voidRepo。
	voidRepo := newMemVoidRepo(map[string]string{
		"CARD-X": "unused",
		"CARD-Y": "unused",
	})
	factory := xianguanjia.NewClientFactory(xianguanjia.NewConfigStoreMemory(&xianguanjia.Config{
		BaseURL: srv.URL, AppID: testAppID, AppSecretEncrypted: testAppSecret, Status: "active",
	}), fakeDecryptor{}.Decrypt)
	voider := xianguanjia.NewRefundCardVoidService(factory, voidRepo, nil)

	v := &fakeVoiderDelegate{voider: voider}
	h, _ := newTestPushHandler(v, activeConfig())
	body := refundPushBody("O1", 12, 5)
	w := doPush(t, h, body, time.Now().Unix(), true)
	if result, _ := parseResult(t, w); result != "success" {
		t.Fatalf("expected success, got body=%s", w.Body.String())
	}
	got := voidRepo.statuses()
	if got["CARD-X"] != "expired" {
		t.Fatalf("CARD-X (kam/list 所发卡) must be voided, got %q", got["CARD-X"])
	}
	if got["CARD-Y"] != "unused" {
		t.Fatalf("CARD-Y (未发卡) must stay unused, got %q — 禁止匹配池内任意未售卡", got["CARD-Y"])
	}
}

// TestPushNoCardSuccessWithAuditTrail 查无此卡 → success + 留痕（日志走 slog，
// 这里验证响应语义与 outcome 分流正确）。
func TestPushNoCardSuccessWithAuditTrail(t *testing.T) {
	v := &fakeVoider{outcome: xianguanjia.VoidNoCard}
	h, idem := newTestPushHandler(v, activeConfig())
	body := refundPushBody("O1", 23, 0)
	w := doPush(t, h, body, time.Now().Unix(), true)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	result, _ := parseResult(t, w)
	if result != "success" {
		t.Fatalf("no-card is not a failure (retry pointless), expected success, got %q", result)
	}
	if v.calls != 1 {
		t.Fatalf("voider should be called once, got %d", v.calls)
	}
	// 查无卡视为处理完成 → 落回执（重推去重）。
	if !receiptExists(t, idem, "O1", "23", "0") {
		t.Fatalf("no-card outcome should commit receipt (terminal, dedup on retry)")
	}
}

// TestPushOrderStatusClosedNoVoid order_status=24（已关闭）→ 不作废仅日志、落回执。
func TestPushOrderStatusClosedNoVoid(t *testing.T) {
	v := &fakeVoider{}
	h, idem := newTestPushHandler(v, activeConfig())
	body := refundPushBody("O1", 24, 0)
	w := doPush(t, h, body, time.Now().Unix(), true)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	result, _ := parseResult(t, w)
	if result != "success" {
		t.Fatalf("expected success for closed order, got %q", result)
	}
	if v.calls != 0 {
		t.Fatalf("order_status=24 must NOT trigger void (policy: observe only), voider calls=%d", v.calls)
	}
	if !receiptExists(t, idem, "O1", "24", "0") {
		t.Fatalf("closed order should commit receipt (no retry expected)")
	}
}

// ---- 测试辅助 ----

// io_ReadAll 读取请求体（测试辅助）。
func io_ReadAll(r *http.Request) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}

// fakeVoiderDelegate 把 XianguanjiaRefundVoider 委托给真实 RefundCardVoidService。
type fakeVoiderDelegate struct {
	voider *xianguanjia.RefundCardVoidService
}

func (d *fakeVoiderDelegate) VoidRefundedCards(ctx context.Context, orderNo string) (xianguanjia.VoidOutcome, error) {
	return d.voider.VoidRefundedCards(ctx, orderNo)
}

// memVoidRepo 内存版池卡作废仓库（模拟 redeem_codes 状态机，验证按 card_no 精准匹配）。
type memVoidRepo struct {
	mu   sync.Mutex
	data map[string]string // card_no → status
}

func newMemVoidRepo(initial map[string]string) *memVoidRepo {
	d := make(map[string]string, len(initial))
	for k, v := range initial {
		d[k] = v
	}
	return &memVoidRepo{data: d}
}

func (m *memVoidRepo) VoidCodesByCardNos(ctx context.Context, cardNos []string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(cardNos))
	for _, no := range cardNos {
		status, ok := m.data[no]
		if !ok {
			out[no] = "missing"
			continue
		}
		switch status {
		case "unused", "delivered":
			m.data[no] = "expired"
			out[no] = "voided"
		case "expired":
			out[no] = "already_expired"
		case "used":
			out[no] = "in_use"
		default:
			out[no] = "disabled"
		}
	}
	return out, nil
}

func (m *memVoidRepo) statuses() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.data))
	for k, v := range m.data {
		out[k] = v
	}
	return out
}

var _ = service.ErrXianyuDeliveryClaimNotFound
