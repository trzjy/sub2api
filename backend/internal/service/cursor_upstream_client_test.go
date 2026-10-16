//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
)

// fakeEncryptor 是测试替身：Identity（CodeEncrypted 字段即明文激活码）。
// 仅用于验证逻辑，不接触真实 credcrypt 密钥体系。
type fakeEncryptor struct{}

func (fakeEncryptor) Encrypt(b []byte) ([]byte, error) { return b, nil }
func (fakeEncryptor) Decrypt(b []byte) ([]byte, error) { return b, nil }

// errEncryptor 的 Decrypt 永远失败，用于验证明文绝不外泄。
type errEncryptor struct{}

func (errEncryptor) Encrypt(b []byte) ([]byte, error) { return b, nil }
func (errEncryptor) Decrypt(b []byte) ([]byte, error) { return nil, errors.New("boom decrypt") }

// loadUpstreamVector 读取权威向量（与 wasm 测试同源）。
//
// 依次尝试取证目录的多个落点：生产取证机用 /root/cursor-evidence/，开发机用
// ~/.sub2api-acceptance/ 下的同名副本；都找不到才 Skip（绝不用桩数据伪造向量）。
func loadUpstreamVector(t *testing.T) (metadata, mail, timestamp, expMail, expAT, expRT string, expRole int, expMode int) {
	t.Helper()
	candidates := []string{"/root/cursor-evidence/06-zk01-vector.json"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			home+"/.sub2api-acceptance/sub2api-cursor-20261013/10-t3-shareai-contract/06-zk01-vector.json")
	}
	var raw []byte
	var readErr error
	for _, p := range candidates {
		raw, readErr = os.ReadFile(p)
		if readErr == nil {
			break
		}
	}
	if readErr != nil {
		t.Skipf("vector file unavailable: %v", readErr)
	}
	var v struct {
		Input struct {
			Metadata  string `json:"metadata"`
			Mail      string `json:"mail"`
			Timestamp string `json:"timestamp"`
		} `json:"input"`
		Expected struct {
			Mail         string `json:"mail"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Role         int    `json:"role"`
			Mode         int    `json:"mode"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	return v.Input.Metadata, v.Input.Mail, v.Input.Timestamp,
		v.Expected.Mail, v.Expected.AccessToken, v.Expected.RefreshToken,
		v.Expected.Role, v.Expected.Mode
}

func newTestClient(t *testing.T, handler http.Handler) QuotaKeyUpstreamClient {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	c, err := NewShareAIUpstreamClient(fakeEncryptor{},
		WithEndpoints(ts.URL, ts.URL),
		WithHTTPClient(ts.Client()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// 1) 端到端：code=0 且含 metadata → zk01 解密 → 账号字段逐一匹配权威向量。
func TestPullBatch_Code0WithMetadata(t *testing.T) {
	md, mail, ts, expMail, expAT, expRT, expRole, expMode := loadUpstreamVector(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0, "msg": "ok",
			"data": map[string]interface{}{
				"metadata":  md,
				"mail":      mail,
				"timestamp": ts,
			},
		})
	})
	c := newTestClient(t, handler)
	res, err := c.PullBatch(context.Background(), &ent.QuotaKey{CodeEncrypted: "TESTCODE"})
	if err != nil {
		t.Fatalf("PullBatch: %v", err)
	}
	if len(res.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d", len(res.Accounts))
	}
	a := res.Accounts[0]
	if a.Name != expMail {
		t.Errorf("Name=%q want %q", a.Name, expMail)
	}
	if a.Platform != "cursor" {
		t.Errorf("Platform=%q want cursor", a.Platform)
	}
	if a.Type != "oauth" {
		t.Errorf("Type=%q want oauth", a.Type)
	}
	if a.Credentials["mail"] != expMail {
		t.Errorf("Credentials.mail=%v want %q", a.Credentials["mail"], expMail)
	}
	if a.Credentials["access_token"] != expAT {
		t.Errorf("Credentials.access_token mismatch")
	}
	if a.Credentials["refresh_token"] != expRT {
		t.Errorf("Credentials.refresh_token mismatch")
	}
	if a.Extra["role"] != expRole {
		t.Errorf("Extra.role=%v want %d", a.Extra["role"], expRole)
	}
	if a.Extra["mode"] != expMode {
		t.Errorf("Extra.mode=%v want %d", a.Extra["mode"], expMode)
	}
}

// 2) 冷却：code=1（如"每5分钟内仅可刷新1次"）→ ErrUpstreamCooldown（可识别，不标 exhausted）。
func TestPullBatch_Cooldown(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1, "msg": "每5分钟内仅可刷新1次", "data": nil,
		})
	})
	c := newTestClient(t, handler)
	_, err := c.PullBatch(context.Background(), &ent.QuotaKey{CodeEncrypted: "TESTCODE"})
	if err == nil {
		t.Fatal("want cooldown error, got nil")
	}
	if !errors.Is(err, ErrUpstreamCooldown) {
		t.Errorf("want ErrUpstreamCooldown, got %v", err)
	}
}

// 3) 非0业务码（如 code=2）→ 业务错误（非 cooldown、非 noMetadata）。
func TestPullBatch_NonZero(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 2, "msg": "invalid activation code", "data": nil,
		})
	})
	c := newTestClient(t, handler)
	_, err := c.PullBatch(context.Background(), &ent.QuotaKey{CodeEncrypted: "TESTCODE"})
	if err == nil {
		t.Fatal("want business error, got nil")
	}
	if errors.Is(err, ErrUpstreamCooldown) {
		t.Errorf("non-zero code must NOT be cooldown: %v", err)
	}
	if errors.Is(err, errNoMetadata) {
		t.Errorf("non-zero code must NOT be noMetadata: %v", err)
	}
}

// 4) HTTP 5xx → 传输错误，且不误判为 cooldown / noMetadata。
func TestPullBatch_HTTP5xx(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("upstream boom"))
	})
	c := newTestClient(t, handler)
	_, err := c.PullBatch(context.Background(), &ent.QuotaKey{CodeEncrypted: "TESTCODE"})
	if err == nil {
		t.Fatal("want http error, got nil")
	}
	if errors.Is(err, ErrUpstreamCooldown) {
		t.Errorf("5xx must NOT be cooldown: %v", err)
	}
}

// timeoutRT 是一个返回超时错误的 RoundTripper，用于稳定地模拟传输超时
// （真实阻塞 handler 上的短超时在 Go 中不可靠，故用 mock）。
type timeoutRT struct{}

func (timeoutRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}

// 5) 超时 → 传输错误（主备均超时，最终失败关闭，且不误判为 cooldown）。
func TestPullBatch_Timeout(t *testing.T) {
	c, err := NewShareAIUpstreamClient(fakeEncryptor{},
		WithEndpoints("http://primary.invalid/quota", "http://fallback.invalid/quota"),
		WithHTTPClient(&http.Client{Transport: timeoutRT{}}))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = c.PullBatch(context.Background(), &ent.QuotaKey{CodeEncrypted: "TESTCODE"})
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if errors.Is(err, ErrUpstreamCooldown) {
		t.Errorf("timeout must NOT be cooldown: %v", err)
	}
}

// 6) 激活路径：refresh 成功但无 metadata → 补 activate → 再 refresh 含 metadata → 成功。
func TestPullBatch_ActivateThenRefresh(t *testing.T) {
	md, mail, ts, expMail, _, _, _, _ := loadUpstreamVector(t)
	var mu sync.Mutex
	var refreshCount int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/activate":
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 0, "msg": "ok"})
		case "/api/refresh":
			mu.Lock()
			refreshCount++
			n := refreshCount
			mu.Unlock()
			if n == 1 {
				// 首次无 metadata，触发 activate。
				json.NewEncoder(w).Encode(map[string]interface{}{
					"code": 0, "msg": "ok",
					"data": map[string]interface{}{"metadata": "", "mail": mail, "timestamp": ts},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 0, "msg": "ok",
				"data": map[string]interface{}{"metadata": md, "mail": mail, "timestamp": ts},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	c := newTestClient(t, handler)
	res, err := c.PullBatch(context.Background(), &ent.QuotaKey{CodeEncrypted: "TESTCODE"})
	if err != nil {
		t.Fatalf("PullBatch: %v", err)
	}
	if len(res.Accounts) != 1 || res.Accounts[0].Name != expMail {
		t.Fatalf("activate-then-refresh account mismatch: %+v", res.Accounts)
	}
	mu.Lock()
	if refreshCount != 2 {
		mu.Unlock()
		t.Errorf("want 2 refresh calls, got %d", refreshCount)
	}
	mu.Unlock()
}

// 7) 主端点失败 → 自动回退备端点成功。
func TestPullBatch_PrimaryFailsFallbackOK(t *testing.T) {
	md, mail, ts, _, _, _, _, _ := loadUpstreamVector(t)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(primary.Close)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0, "msg": "ok",
			"data": map[string]interface{}{"metadata": md, "mail": mail, "timestamp": ts},
		})
	}))
	t.Cleanup(fallback.Close)

	c, err := NewShareAIUpstreamClient(fakeEncryptor{},
		WithEndpoints(primary.URL, fallback.URL),
		WithHTTPClient(primary.Client()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	res, err := c.PullBatch(context.Background(), &ent.QuotaKey{CodeEncrypted: "TESTCODE"})
	if err != nil {
		t.Fatalf("PullBatch: %v", err)
	}
	if len(res.Accounts) != 1 {
		t.Fatalf("want 1 account from fallback, got %d", len(res.Accounts))
	}
}

// 8) 空激活码 → 直接失败（不触网络）。
func TestPullBatch_EmptyCode(t *testing.T) {
	c, err := NewShareAIUpstreamClient(fakeEncryptor{})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = c.PullBatch(context.Background(), &ent.QuotaKey{CodeEncrypted: ""})
	if err == nil {
		t.Fatal("want error for empty code, got nil")
	}
}

// 9) 解密激活码失败 → 错误但不泄露明文。
func TestPullBatch_EncryptorError_NoLeak(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"code": 0, "msg": "ok"})
	}))
	t.Cleanup(ts.Close)
	c, err := NewShareAIUpstreamClient(errEncryptor{},
		WithEndpoints(ts.URL, ts.URL),
		WithHTTPClient(ts.Client()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = c.PullBatch(context.Background(), &ent.QuotaKey{CodeEncrypted: "PLAINTEXT_LEAK_PROBE"})
	if err == nil {
		t.Fatal("want decrypt error, got nil")
	}
	if strings.Contains(err.Error(), "PLAINTEXT_LEAK_PROBE") {
		t.Errorf("activation code plaintext leaked into error: %v", err)
	}
}

// 10) 加密器缺失 → 构造即失败（禁止静默回落明文）。
func TestNewShareAIUpstreamClient_NilEncryptor(t *testing.T) {
	if _, err := NewShareAIUpstreamClient(nil); err == nil {
		t.Fatal("want error for nil encryptor, got nil")
	}
}

// ---------------------------------------------------------------------------
// B-11：key_info.expired_at（【激活码】到期时间）解析。
// 取证事实见 10-t3-shareai-contract/03-refresh-1.json：refresh 响应的
// data.key_info.expired_at = "2026-10-14T07:53:25.000Z"，是 quota_keys.expires_at
// 的唯一事实源（区别于 metadata 内 account_expire_in —— 那是号池账号的有效期）。
// ---------------------------------------------------------------------------

// pullBatchWithKeyInfo 起一个替身上游，按 keyInfo 覆盖字段返回 refresh 响应。
func pullBatchWithKeyInfo(t *testing.T, keyInfo map[string]interface{}) (*UpstreamPullResult, error) {
	t.Helper()
	md, mail, ts, _, _, _, _, _ := loadUpstreamVector(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0, "msg": "ok",
			"data": map[string]interface{}{
				"metadata":  md,
				"mail":      mail,
				"timestamp": ts,
				"key_info":  keyInfo,
			},
		})
	})
	return newTestClient(t, handler).PullBatch(context.Background(), &ent.QuotaKey{CodeEncrypted: "TESTCODE"})
}

// 11) key_info.expired_at 正常解析 → 上抛为 UpstreamPullResult.ExpiresAt。
func TestPullBatch_ParsesKeyInfoExpiredAt(t *testing.T) {
	res, err := pullBatchWithKeyInfo(t, map[string]interface{}{
		"expired_at": "2026-10-14T07:53:25.000Z",
	})
	if err != nil {
		t.Fatalf("PullBatch: %v", err)
	}
	if res.ExpiresAt == nil {
		t.Fatal("ExpiresAt must be parsed from key_info.expired_at")
	}
	if got := res.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"); got != "2026-10-14T07:53:25Z" {
		t.Errorf("ExpiresAt = %s, want 2026-10-14T07:53:25Z", got)
	}
}

// 12) 上游未给 key_info → ExpiresAt 为 nil（绝不含激推算出的假日期）。
func TestPullBatch_NoKeyInfo_ExpiresAtNil(t *testing.T) {
	res, err := pullBatchWithKeyInfo(t, map[string]interface{}{})
	if err != nil {
		t.Fatalf("PullBatch: %v", err)
	}
	if res.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil when key_info absent", res.ExpiresAt)
	}
}

// 13) expired_at 为 null / 空串 / 非法格式 → 一律 nil，且不阻断拉号主流程。
func TestPullBatch_ExpiredAtUnusable_ExpiresAtNil(t *testing.T) {
	cases := map[string]interface{}{
		"null":       nil,
		"empty":      "",
		"bad_format": "not-a-timestamp",
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := pullBatchWithKeyInfo(t, map[string]interface{}{"expired_at": v})
			if err != nil {
				t.Fatalf("unusable expired_at must not fail the pull: %v", err)
			}
			if len(res.Accounts) != 1 {
				t.Fatalf("want 1 account, got %d", len(res.Accounts))
			}
			if res.ExpiresAt != nil {
				t.Errorf("ExpiresAt = %v, want nil", res.ExpiresAt)
			}
		})
	}
}

// 14) parseKeyExpiresAt 直接单测：只接受 RFC3339，其余一律 nil。
func TestParseKeyExpiresAt(t *testing.T) {
	s := func(v string) *string { return &v }
	if got := parseKeyExpiresAt(nil); got != nil {
		t.Errorf("nil input => %v, want nil", got)
	}
	if got := parseKeyExpiresAt(s("  ")); got != nil {
		t.Errorf("blank input => %v, want nil", got)
	}
	if got := parseKeyExpiresAt(s("2026-10-14 07:53:25")); got != nil {
		t.Errorf("non-RFC3339 => %v, want nil", got)
	}
	got := parseKeyExpiresAt(s("2026-10-14T07:53:25.000Z"))
	if got == nil {
		t.Fatal("valid RFC3339 => nil, want parsed time")
	}
	if y, m, d := got.UTC().Date(); y != 2026 || m != 10 || d != 14 {
		t.Errorf("parsed date = %d-%d-%d, want 2026-10-14", y, m, d)
	}
}
