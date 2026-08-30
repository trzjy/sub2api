package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 验证 do() 对 ApiResponse 包装响应的二次解包是否会覆盖顶层 success。
func TestReviewDoubleUnwrapKeepsTopLevelSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"消息发送成功","data":{"send_status":"success","send_fail_reason":null}}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	result, err := client.ResendDelivery(context.Background(), "a1", "o1", "i1", "b1", "c1", "CODE", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	receipt, _ := normalizeSendReceipt(result)
	t.Logf("result=%+v receipt=%q", result, receipt)
	if receipt != "sent_explicit_success" {
		t.Fatal("expected send success receipt after envelope decode")
	}
}

// 验证 dispatched=false 响应被正确解码（机器可判定的"确定未 dispatch"信号）。
func TestReviewDecodesDispatchedFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"message":"账号不存在或无权操作","data":{"dispatched":false}}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	result, err := client.ResendDelivery(context.Background(), "a1", "o1", "i1", "b1", "c1", "CODE", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	receipt, _ := normalizeSendReceipt(result)
	t.Logf("result=%+v receipt=%q", result, receipt)
	if receipt != "dispatched_definite_failure" {
		t.Fatalf("expected dispatched_definite_failure, got %q", receipt)
	}
}

// 验证 XianyuWorkerLoginSessionStatus 双字段 JSON 输出。
func TestReviewMarshalRoundTrip(t *testing.T) {
	s := XianyuWorkerLoginSessionStatus{SessionID: "s1", Status: "success", QRCodeURL: "url", AccountID: "a"}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("marshaled: %s", string(raw))
	if !bytes.Contains(raw, []byte(`"qr_code":"url"`)) {
		t.Fatal("qr_code not emitted")
	}
	if !bytes.Contains(raw, []byte(`"qr_code_url":"url"`)) {
		t.Fatal("qr_code_url not emitted")
	}
}