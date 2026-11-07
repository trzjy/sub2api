package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestKnCyClient 覆盖 kn-cy.com 上游客户端的两个接口：
//   - ClaimActivation：成功解析 data、400 业务错误、409 重复激活、缺 Authorization；
//   - GetSubscriptionUsage：成功解析 data、403 额度不足、非 2xx 透传状态码。
//
// 全部使用 httptest.NewServer 模拟上游端点，凭据使用 fake 值（fake-activation-code /
// fake-subscription-key），严禁硬编码真实 Key。

func newKnCyTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestKnCyClient_ClaimActivation_Success(t *testing.T) {
	srv := newKnCyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("ClaimActivation method = %s, want %s", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/api/activation/claim" {
			t.Errorf("ClaimActivation path = %s, want /api/activation/claim", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fake-activation-code" {
			t.Errorf("ClaimActivation Authorization = %q, want Bearer fake-activation-code", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"message": "activation claimed",
			"data": map[string]any{
				"status":          "active",
				"remaining_quota": 850.5,
				"total_quota":     1000,
				"expires_at":      1767225600,
				"unit":            "tokens",
			},
		})
	})

	c := NewKnCyClient(srv.URL)
	got, err := c.ClaimActivation(context.Background(), "fake-activation-code")
	if err != nil {
		t.Fatalf("ClaimActivation() error = %v", err)
	}
	if got.Status != "active" {
		t.Errorf("ClaimActivation().Status = %q, want active", got.Status)
	}
	if got.RemainingQuota != 850.5 {
		t.Errorf("ClaimActivation().RemainingQuota = %v, want 850.5", got.RemainingQuota)
	}
	if got.TotalQuota != 1000 {
		t.Errorf("ClaimActivation().TotalQuota = %v, want 1000", got.TotalQuota)
	}
	if got.ExpiresAt != 1767225600 {
		t.Errorf("ClaimActivation().ExpiresAt = %d, want 1767225600", got.ExpiresAt)
	}
	if got.Unit != "tokens" {
		t.Errorf("ClaimActivation().Unit = %q, want tokens", got.Unit)
	}
}

func TestKnCyClient_ClaimActivation_HTTP400(t *testing.T) {
	srv := newKnCyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","message":"invalid activation code"}`))
	})

	c := NewKnCyClient(srv.URL)
	_, err := c.ClaimActivation(context.Background(), "fake-activation-code")
	if err == nil {
		t.Fatal("ClaimActivation() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("ClaimActivation() error = %q, want contains status 400", err.Error())
	}
	// 凭据绝不进入错误信息。
	if strings.Contains(err.Error(), "fake-activation-code") {
		t.Errorf("ClaimActivation() error leaks credential: %q", err.Error())
	}
}

func TestKnCyClient_ClaimActivation_HTTP409(t *testing.T) {
	srv := newKnCyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":"error","message":"activation already claimed"}`))
	})

	c := NewKnCyClient(srv.URL)
	_, err := c.ClaimActivation(context.Background(), "fake-activation-code")
	if err == nil {
		t.Fatal("ClaimActivation() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("ClaimActivation() error = %q, want contains status 409", err.Error())
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("ClaimActivation() error = %q, want contains upstream message", err.Error())
	}
}

func TestKnCyClient_ClaimActivation_BusinessError(t *testing.T) {
	// 2xx 但业务 status 非成功值：同样返回明确错误。
	srv := newKnCyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"failed","message":"quota not enough"}`))
	})

	c := NewKnCyClient(srv.URL)
	_, err := c.ClaimActivation(context.Background(), "fake-activation-code")
	if err == nil {
		t.Fatal("ClaimActivation() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "quota not enough") {
		t.Errorf("ClaimActivation() error = %q, want contains business message", err.Error())
	}
}

func TestKnCyClient_ClaimActivation_EmptyCode(t *testing.T) {
	c := NewKnCyClient("http://127.0.0.1:1")
	if _, err := c.ClaimActivation(context.Background(), "  "); err == nil {
		t.Fatal("ClaimActivation(empty) error = nil, want non-nil")
	}
}

func TestKnCyClient_GetSubscriptionUsage_Success(t *testing.T) {
	srv := newKnCyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("GetSubscriptionUsage method = %s, want %s", r.Method, http.MethodGet)
		}
		if r.URL.Path != "/api/usage/subscription/" {
			t.Errorf("GetSubscriptionUsage path = %s, want /api/usage/subscription/", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fake-subscription-key" {
			t.Errorf("GetSubscriptionUsage Authorization = %q, want Bearer fake-subscription-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"total_remaining": 9800,
				"today_remaining": 1900,
				"expires_at":      1767225600,
				"unlimited":       false,
				"unit":            "tokens",
			},
		})
	})

	c := NewKnCyClient(srv.URL)
	got, err := c.GetSubscriptionUsage(context.Background(), "fake-subscription-key")
	if err != nil {
		t.Fatalf("GetSubscriptionUsage() error = %v", err)
	}
	if got.TotalRemaining != 9800 {
		t.Errorf("GetSubscriptionUsage().TotalRemaining = %v, want 9800", got.TotalRemaining)
	}
	if got.TodayRemaining != 1900 {
		t.Errorf("GetSubscriptionUsage().TodayRemaining = %v, want 1900", got.TodayRemaining)
	}
	if got.ExpiresAt != 1767225600 {
		t.Errorf("GetSubscriptionUsage().ExpiresAt = %d, want 1767225600", got.ExpiresAt)
	}
	if got.Unlimited {
		t.Errorf("GetSubscriptionUsage().Unlimited = true, want false")
	}
	if got.Unit != "tokens" {
		t.Errorf("GetSubscriptionUsage().Unit = %q, want tokens", got.Unit)
	}
}

func TestKnCyClient_GetSubscriptionUsage_HTTP403(t *testing.T) {
	srv := newKnCyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":"error","message":"insufficient quota"}`))
	})

	c := NewKnCyClient(srv.URL)
	_, err := c.GetSubscriptionUsage(context.Background(), "fake-subscription-key")
	if err == nil {
		t.Fatal("GetSubscriptionUsage() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("GetSubscriptionUsage() error = %q, want contains status 403", err.Error())
	}
	if !strings.Contains(err.Error(), "insufficient quota") {
		t.Errorf("GetSubscriptionUsage() error = %q, want contains upstream message", err.Error())
	}
	if strings.Contains(err.Error(), "fake-subscription-key") {
		t.Errorf("GetSubscriptionUsage() error leaks credential: %q", err.Error())
	}
}

func TestKnCyClient_GetSubscriptionUsage_EmptyKey(t *testing.T) {
	c := NewKnCyClient("http://127.0.0.1:1")
	if _, err := c.GetSubscriptionUsage(context.Background(), ""); err == nil {
		t.Fatal("GetSubscriptionUsage(empty) error = nil, want non-nil")
	}
}

// TestKnCyClient_ClaimActivation_Non2xxCredentialEcho 是凭据回显的回归测试：
// 当上游非 2xx 错误正文回显了 Authorization 中的凭据（如 "Bearer fake-..."）时，
// 错误信息必须对凭据脱敏，绝不能包含凭据原文。
func TestKnCyClient_ClaimActivation_Non2xxCredentialEcho(t *testing.T) {
	srv := newKnCyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		// 故意在错误正文里回显了 Authorization 中的凭据原文。
		_, _ = w.Write([]byte(`{"status":"error","message":"invalid Authorization: Bearer fake-activation-code"}`))
	})

	c := NewKnCyClient(srv.URL)
	_, err := c.ClaimActivation(context.Background(), "fake-activation-code")
	if err == nil {
		t.Fatal("ClaimActivation() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("ClaimActivation() error = %q, want contains status 401", err.Error())
	}
	// 凭据原文绝不可出现在错误信息中（回显泄露）。
	if strings.Contains(err.Error(), "fake-activation-code") {
		t.Errorf("ClaimActivation() error leaks credential: %q", err.Error())
	}
	// 脱敏后应以固定占位符呈现。
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("ClaimActivation() error missing masked digest: %q", err.Error())
	}
}

// TestKnCyClient_GetSubscriptionUsage_Non2xxCredentialEcho 同上，覆盖订阅 Key 路径。
func TestKnCyClient_GetSubscriptionUsage_Non2xxCredentialEcho(t *testing.T) {
	srv := newKnCyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":"error","message":"unauthorized key fake-subscription-key"}`))
	})

	c := NewKnCyClient(srv.URL)
	_, err := c.GetSubscriptionUsage(context.Background(), "fake-subscription-key")
	if err == nil {
		t.Fatal("GetSubscriptionUsage() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("GetSubscriptionUsage() error = %q, want contains status 401", err.Error())
	}
	if strings.Contains(err.Error(), "fake-subscription-key") {
		t.Errorf("GetSubscriptionUsage() error leaks credential: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("GetSubscriptionUsage() error missing masked digest: %q", err.Error())
	}
}
