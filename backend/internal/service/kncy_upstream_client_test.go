//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
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

// TestKnCyClient_DefaultBaseURL 回归测试：防止默认管理端 API 主机再次被写成带
// api. 前缀的根主机（生产实测该 host 一律 404 Invalid URL）。
//
// 正确默认值必须恰好为 https://kn-cy.com（无 api. 前缀）。
// 与模型出站网关 knCyGatewayBaseURL（https://kn-cy.com/v1）不是同一回事。
func TestKnCyClient_DefaultBaseURL(t *testing.T) {
	c := NewKnCyClient("")
	if c.baseURL != "https://kn-cy.com" {
		t.Fatalf("NewKnCyClient(\"\").baseURL = %q, want https://kn-cy.com", c.baseURL)
	}
	// 显式传入的 baseURL 不应被默认值覆盖。
	c2 := NewKnCyClient("https://example.test")
	if c2.baseURL != "https://example.test" {
		t.Fatalf("NewKnCyClient(override).baseURL = %q, want https://example.test", c2.baseURL)
	}
}

// ---- KnCy QuotaKeyUpstreamClient（KC-15）定向测试 ----

// fakeQuotaKeyEncryptor 是测试用占位加密器：明文直通（模拟 credcrypt 未配置主密钥时的
// passthrough 退化）。仅用于单测，绝不承载真实密钥。
type fakeQuotaKeyEncryptor struct{}

func (fakeQuotaKeyEncryptor) Encrypt(plain []byte) ([]byte, error)  { return plain, nil }
func (fakeQuotaKeyEncryptor) Decrypt(cipher []byte) ([]byte, error) { return cipher, nil }

// newKnCyQuotaKeyUpstreamWithServer 用 httptest 服务端构造 kn-cy 的 PullBatch 客户端。
func newKnCyQuotaKeyUpstreamWithServer(t *testing.T, handler http.HandlerFunc) (*knCyQuotaKeyUpstream, *httptest.Server) {
	t.Helper()
	srv := newKnCyTestServer(t, handler)
	kc := NewKnCyClient(srv.URL)
	cli, err := NewKnCyQuotaKeyUpstreamClient(kc, fakeQuotaKeyEncryptor{})
	if err != nil {
		t.Fatalf("NewKnCyQuotaKeyUpstreamClient() error = %v", err)
	}
	up, ok := cli.(*knCyQuotaKeyUpstream)
	if !ok {
		t.Fatalf("NewKnCyQuotaKeyUpstreamClient() returned %T, want *knCyQuotaKeyUpstream", cli)
	}
	return up, srv
}

// activationHandler 是激活成功的标准响应（expires_at 由调用方指定，0 表示上游未给出）。
func activationHandler(expiresAt int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"status":          "active",
				"remaining_quota": 850.5,
				"total_quota":     1000,
				"expires_at":      expiresAt,
				"unit":            "tokens",
			},
		})
	}
}

// TestKnCyQuotaKeyUpstream_PullBatch_Success 验证激活成功返回恰好 1 个账号，
// Type=apikey、Platform=cursor、model_mapping 恰含 3 个模型恒等映射、ExpiresAt 正确解析。
func TestKnCyQuotaKeyUpstream_PullBatch_Success(t *testing.T) {
	const (
		secret   = "fake-card-secret"
		codeLast = "a1b2"
	)
	up, _ := newKnCyQuotaKeyUpstreamWithServer(t, activationHandler(1767225600))
	qk := &ent.QuotaKey{CodeEncrypted: secret, CodeLast4: codeLast}

	res, err := up.PullBatch(context.Background(), qk)
	if err != nil {
		t.Fatalf("PullBatch() error = %v", err)
	}
	if res == nil || len(res.Accounts) != 1 {
		t.Fatalf("PullBatch().Accounts = %v, want exactly 1", res)
	}
	acct := res.Accounts[0]
	if acct.Platform != "cursor" {
		t.Errorf("Account.Platform = %q, want cursor", acct.Platform)
	}
	if acct.Type != domain.AccountTypeAPIKey {
		t.Errorf("Account.Type = %q, want %q", acct.Type, domain.AccountTypeAPIKey)
	}
	if acct.Name != "kncy-"+codeLast {
		t.Errorf("Account.Name = %q, want kncy-%s", acct.Name, codeLast)
	}
	if acct.Extra != nil {
		t.Errorf("Account.Extra = %v, want nil", acct.Extra)
	}

	baseURL, _ := acct.Credentials["base_url"].(string)
	if baseURL != knCyGatewayBaseURL {
		t.Errorf("Credentials[base_url] = %q, want %q", baseURL, knCyGatewayBaseURL)
	}
	apiKey, _ := acct.Credentials["api_key"].(string)
	if apiKey != secret {
		t.Errorf("Credentials[api_key] = %q, want card secret", apiKey)
	}
	mm, ok := acct.Credentials["model_mapping"].(map[string]any)
	if !ok {
		t.Fatalf("Credentials[model_mapping] missing or wrong type: %v", acct.Credentials["model_mapping"])
	}
	if len(mm) != 3 {
		t.Errorf("model_mapping size = %d, want 3", len(mm))
	}
	for _, m := range []string{"gpt-6.1-sol", "claude-fable-5-1", "gemini-4-argon"} {
		v, ok := mm[m]
		if !ok {
			t.Errorf("model_mapping missing %q", m)
			continue
		}
		if v != m {
			t.Errorf("model_mapping[%q] = %v, want identity %q", m, v, m)
		}
	}

	if res.ExpiresAt == nil {
		t.Fatalf("ExpiresAt = nil, want parsed from 1767225600")
	}
	want := time.Unix(1767225600, 0).UTC()
	if !res.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", res.ExpiresAt, want)
	}
}

// TestKnCyQuotaKeyUpstream_PullBatch_ClaimFails 验证 ClaimActivation 失败时
// PullBatch 返回 error 且 Accounts 为空（失败关闭，绝不返回成功+空列表）。
func TestKnCyQuotaKeyUpstream_PullBatch_ClaimFails(t *testing.T) {
	up, _ := newKnCyQuotaKeyUpstreamWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// 故意回显凭据原文，验证错误信息脱敏。
		_, _ = w.Write([]byte(`{"status":"error","message":"invalid Authorization: Bearer fake-card-secret"}`))
	})
	qk := &ent.QuotaKey{CodeEncrypted: "fake-card-secret", CodeLast4: "a1b2"}

	res, err := up.PullBatch(context.Background(), qk)
	if err == nil {
		t.Fatal("PullBatch() error = nil, want non-nil")
	}
	if res != nil && len(res.Accounts) != 0 {
		t.Errorf("PullBatch() returned %d accounts on failure, want 0", len(res.Accounts))
	}
	// 故障关闭须为显式错误；不得回显卡密原文。
	if strings.Contains(err.Error(), "fake-card-secret") {
		t.Errorf("PullBatch() error leaks card secret: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("PullBatch() error missing masked digest: %q", err.Error())
	}
}

// TestKnCyQuotaKeyUpstream_PullBatch_EmptyExpiresAt 验证上游未给到期时间（expires_at=0）
// 时 ExpiresAt 为 nil（调用方保持原值不动，不得推算补日期）。
func TestKnCyQuotaKeyUpstream_PullBatch_EmptyExpiresAt(t *testing.T) {
	up, _ := newKnCyQuotaKeyUpstreamWithServer(t, activationHandler(0))
	qk := &ent.QuotaKey{CodeEncrypted: "fake-card-secret", CodeLast4: "a1b2"}

	res, err := up.PullBatch(context.Background(), qk)
	if err != nil {
		t.Fatalf("PullBatch() error = %v", err)
	}
	if len(res.Accounts) != 1 {
		t.Fatalf("PullBatch().Accounts = %d, want 1", len(res.Accounts))
	}
	if res.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil when upstream gives 0", res.ExpiresAt)
	}
}

// TestKnCyQuotaKeyUpstream_PullBatch_NoSecret 验证未配置加密卡密时返回错误。
func TestKnCyQuotaKeyUpstream_PullBatch_NoSecret(t *testing.T) {
	up, _ := newKnCyQuotaKeyUpstreamWithServer(t, activationHandler(1767225600))

	if _, err := up.PullBatch(context.Background(), nil); err == nil {
		t.Fatal("PullBatch(nil) error = nil, want non-nil")
	}
	if _, err := up.PullBatch(context.Background(), &ent.QuotaKey{}); err == nil {
		t.Fatal("PullBatch(empty code) error = nil, want non-nil")
	}
}

// TestKnCyQuotaKeyUpstream_PullBatch_NameNoLeak 验证账号 Name 不含卡密原文：
// CodeLast4 为空时回退到哈希前 8 位，仍不回显原文。
func TestKnCyQuotaKeyUpstream_PullBatch_NameNoLeak(t *testing.T) {
	up, _ := newKnCyQuotaKeyUpstreamWithServer(t, activationHandler(1767225600))
	qk := &ent.QuotaKey{CodeEncrypted: "fake-card-secret"} // CodeLast4 空 → 走哈希回退

	res, err := up.PullBatch(context.Background(), qk)
	if err != nil {
		t.Fatalf("PullBatch() error = %v", err)
	}
	if len(res.Accounts) != 1 {
		t.Fatalf("PullBatch().Accounts = %d, want 1", len(res.Accounts))
	}
	name := res.Accounts[0].Name
	if !strings.HasPrefix(name, "kncy-") {
		t.Errorf("Account.Name = %q, want prefix kncy-", name)
	}
	if strings.Contains(name, "fake-card-secret") {
		t.Errorf("Account.Name leaks card secret: %q", name)
	}
}

// TestKnCyQuotaKeyUpstream_RealSupplyAndTarget 验证 KC-26 的上游自报口径：
//   - RealSupplyModelsFor 仅返回 kn-cy 实供的 3 个模型（gpt-6.1-sol / claude-fable-5-1 / gemini-4-argon），
//     不混入 ShareAI 的 16 个静态清单；
//   - TargetAccountsPerKeyFor 返回 1（1 卡 1 号），与 shareai 的 5 区分。
func TestKnCyQuotaKeyUpstream_RealSupplyAndTarget(t *testing.T) {
	up, err := NewKnCyQuotaKeyUpstreamClient(NewKnCyClient(""), fakeQuotaKeyEncryptor{})
	if err != nil {
		t.Fatalf("NewKnCyQuotaKeyUpstreamClient() error = %v", err)
	}

	models, err := up.RealSupplyModelsFor(upstreamKeyKnCy)
	if err != nil {
		t.Fatalf("RealSupplyModelsFor() error = %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("RealSupplyModelsFor() returned %d models, want 3: %v", len(models), models)
	}
	want := map[string]bool{"gpt-6.1-sol": false, "claude-fable-5-1": false, "gemini-4-argon": false}
	for _, m := range models {
		if _, ok := want[m]; !ok {
			t.Errorf("RealSupplyModelsFor() unexpected model %q", m)
		}
		want[m] = true
	}
	for m, seen := range want {
		if !seen {
			t.Errorf("RealSupplyModelsFor() missing model %q", m)
		}
	}

	target, err := up.TargetAccountsPerKeyFor(upstreamKeyKnCy)
	if err != nil {
		t.Fatalf("TargetAccountsPerKeyFor() error = %v", err)
	}
	if target != 1 {
		t.Errorf("TargetAccountsPerKeyFor() = %d, want 1 (1 card = 1 account)", target)
	}
}
