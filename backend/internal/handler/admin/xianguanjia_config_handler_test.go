package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 以下密钥均为单测占位，绝非真实商户凭证。
const xgjTestAppSecret = "fake-xgj-app-secret"

// fakeXgjConfigStore 实现 XianguanjiaConfigStore，记录写入并模拟 DB 行。
type fakeXgjConfigStore struct {
	mu        sync.Mutex
	row       *xianguanjia.Config
	upserts   []xianguanjia.ConfigUpsert
	healths   []string
	upsertErr error
	healthErr error
}

func (s *fakeXgjConfigStore) GetConfigRow(ctx context.Context) (*xianguanjia.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row == nil {
		return nil, nil
	}
	cp := *s.row
	return &cp, nil
}

func (s *fakeXgjConfigStore) UpsertConfig(ctx context.Context, up xianguanjia.ConfigUpsert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upserts = append(s.upserts, up)
	status := up.Status
	if status == "" {
		status = "active"
	}
	s.row = &xianguanjia.Config{
		BaseURL:            up.BaseURL,
		AppID:              up.AppID,
		AppSecretEncrypted: up.AppSecretEncrypted,
		PushURL:            up.PushURL,
		Status:             status,
		HealthStatus:       "unknown",
	}
	return nil
}

func (s *fakeXgjConfigStore) UpdateHealth(ctx context.Context, healthStatus string, checkedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.healthErr != nil {
		return s.healthErr
	}
	s.healths = append(s.healths, healthStatus)
	if s.row != nil {
		s.row.HealthStatus = healthStatus
	}
	return nil
}

// fakeXgjDecryptor 可反转的对称假加密器（前缀包裹），用于验证加密回读一致。
type fakeXgjDecryptor struct{ mu sync.Mutex }

func (d *fakeXgjDecryptor) Encrypt(plaintext string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return "enc::" + plaintext, nil
}

func (d *fakeXgjDecryptor) Decrypt(ciphertext string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !strings.HasPrefix(ciphertext, "enc::") {
		return "", errors.New("bad ciphertext")
	}
	return strings.TrimPrefix(ciphertext, "enc::"), nil
}

// fakeXgjProbe 可编程探活结果。
type fakeXgjProbe struct {
	err   error
	calls []struct{ baseURL, appKey, appSecret string }
}

func (p *fakeXgjProbe) Probe(ctx context.Context, baseURL, appKey, appSecret string) error {
	p.calls = append(p.calls, struct{ baseURL, appKey, appSecret string }{baseURL, appKey, appSecret})
	return p.err
}

func newXgjTestHandler(store *fakeXgjConfigStore, probe XianguanjiaProbeClient) *XianguanjiaConfigHandler {
	return NewXianguanjiaConfigHandler(store, &fakeXgjDecryptor{}, probe)
}

func doXgjRequest(t *testing.T, h *XianguanjiaConfigHandler, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/admin/xianguanjia/config", h.Get)
	r.PUT("/admin/xianguanjia/config", h.Put)
	r.POST("/admin/xianguanjia/health-check", h.HealthCheck)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeXgjBody(t *testing.T, w *httptest.ResponseRecorder) (code int, data json.RawMessage) {
	t.Helper()
	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "body: %s", w.Body.String())
	return env.Code, env.Data
}

// GET 未配置时返回空态（configured=false），且不含 secret 字段内容。
func TestXgjConfigGetEmptyState(t *testing.T) {
	h := newXgjTestHandler(&fakeXgjConfigStore{}, &fakeXgjProbe{})
	w := doXgjRequest(t, h, http.MethodGet, "/admin/xianguanjia/config", "")
	require.Equal(t, http.StatusOK, w.Code)
	code, data := decodeXgjBody(t, w)
	require.Equal(t, 0, code)
	var resp struct {
		Configured   bool   `json:"configured"`
		AppSecretSet bool   `json:"app_secret_set"`
		BaseURL      string `json:"base_url"`
	}
	require.NoError(t, json.Unmarshal(data, &resp))
	require.False(t, resp.Configured)
	require.False(t, resp.AppSecretSet)
	require.Equal(t, "https://open.goofish.pro", resp.BaseURL)
	require.NotContains(t, w.Body.String(), "app_secret_encrypted")
}

// GET 脱敏：不回明文/密文，只回末 4 位。
func TestXgjConfigGetMasksSecret(t *testing.T) {
	store := &fakeXgjConfigStore{row: &xianguanjia.Config{
		BaseURL:            "https://open.goofish.pro",
		AppID:              "1782787246589381",
		AppSecretEncrypted: "enc::" + xgjTestAppSecret,
		Status:             "active",
		HealthStatus:       "unknown",
	}}
	h := newXgjTestHandler(store, &fakeXgjProbe{})
	w := doXgjRequest(t, h, http.MethodGet, "/admin/xianguanjia/config", "")
	require.Equal(t, http.StatusOK, w.Code)
	code, data := decodeXgjBody(t, w)
	require.Equal(t, 0, code)
	var resp xianguanjiaConfigResponse
	require.NoError(t, json.Unmarshal(data, &resp))
	require.True(t, resp.Configured)
	require.True(t, resp.AppSecretSet)
	// 明文与密文都不出现
	require.NotContains(t, w.Body.String(), xgjTestAppSecret)
	require.NotContains(t, w.Body.String(), "enc::")
	// 只回末 4 位
	require.Equal(t, xgjTestAppSecret[len(xgjTestAppSecret)-4:], resp.AppSecretTail)
	require.LessOrEqual(t, len(resp.AppSecretTail), 4)
}

// PUT 加密落库：存储收到的是密文，解密回读与原文一致；status 置 active。
func TestXgjConfigPutEncryptsAndPersists(t *testing.T) {
	store := &fakeXgjConfigStore{}
	h := newXgjTestHandler(store, &fakeXgjProbe{})
	body := `{"app_id":"test-app-key","app_secret":"` + xgjTestAppSecret + `","push_url":"https://corealgos.com/api/v1/webhook/xianguanjia","base_url":"https://open.goofish.pro"}`
	w := doXgjRequest(t, h, http.MethodPut, "/admin/xianguanjia/config", body)
	require.Equal(t, http.StatusOK, w.Code)
	code, _ := decodeXgjBody(t, w)
	require.Equal(t, 0, code)

	require.Len(t, store.upserts, 1)
	up := store.upserts[0]
	require.Equal(t, "active", up.Status)
	require.Equal(t, "test-app-key", up.AppID)
	// 落库的是密文，不是明文
	require.NotEqual(t, xgjTestAppSecret, up.AppSecretEncrypted)
	require.True(t, strings.HasPrefix(up.AppSecretEncrypted, "enc::"))
	// 加密回读一致
	dec := &fakeXgjDecryptor{}
	plain, err := dec.Decrypt(up.AppSecretEncrypted)
	require.NoError(t, err)
	require.Equal(t, xgjTestAppSecret, plain)
}

// PUT 已有配置且 secret 留空：保留原密文。
func TestXgjConfigPutKeepsSecretWhenBlank(t *testing.T) {
	store := &fakeXgjConfigStore{row: &xianguanjia.Config{
		AppID:              "old-key",
		AppSecretEncrypted: "enc::old-secret",
		Status:             "active",
	}}
	h := newXgjTestHandler(store, &fakeXgjProbe{})
	w := doXgjRequest(t, h, http.MethodPut, "/admin/xianguanjia/config", `{"app_id":"new-key","app_secret":"","push_url":""}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, store.upserts, 1)
	require.Equal(t, "enc::old-secret", store.upserts[0].AppSecretEncrypted)
}

// PUT 首次保存且 secret 留空：400。
func TestXgjConfigPutFirstSaveRequiresSecret(t *testing.T) {
	store := &fakeXgjConfigStore{}
	h := newXgjTestHandler(store, &fakeXgjProbe{})
	w := doXgjRequest(t, h, http.MethodPut, "/admin/xianguanjia/config", `{"app_id":"k","app_secret":""}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Empty(t, store.upserts)
}

// PUT 缺 app_id：400。
func TestXgjConfigPutRequiresAppID(t *testing.T) {
	h := newXgjTestHandler(&fakeXgjConfigStore{}, &fakeXgjProbe{})
	w := doXgjRequest(t, h, http.MethodPut, "/admin/xianguanjia/config", `{"app_id":"","app_secret":"s"}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// health-check 探活成功：healthy + 更新状态；探活失败：unhealthy。
func TestXgjConfigHealthCheckUpdatesStatus(t *testing.T) {
	store := &fakeXgjConfigStore{row: &xianguanjia.Config{
		AppID: "k", AppSecretEncrypted: "enc::s", Status: "active",
	}}

	probe := &fakeXgjProbe{}
	h := newXgjTestHandler(store, probe)
	w := doXgjRequest(t, h, http.MethodPost, "/admin/xianguanjia/health-check", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, []string{"healthy"}, store.healths)
	require.Len(t, probe.calls, 1)

	probe2 := &fakeXgjProbe{err: errors.New("boom")}
	h2 := newXgjTestHandler(store, probe2)
	w2 := doXgjRequest(t, h2, http.MethodPost, "/admin/xianguanjia/health-check", "")
	require.Equal(t, http.StatusOK, w2.Code)
	require.Equal(t, []string{"healthy", "unhealthy"}, store.healths)
}

// 未配置时 health-check 返回 400 且不发起探活。
func TestXgjConfigHealthCheckNotConfigured(t *testing.T) {
	store := &fakeXgjConfigStore{}
	probe := &fakeXgjProbe{}
	h := newXgjTestHandler(store, probe)
	w := doXgjRequest(t, h, http.MethodPost, "/admin/xianguanjia/health-check", "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Empty(t, probe.calls)
	require.Empty(t, store.healths)
}

// 依赖未注入：fail-closed 返回 503。
func TestXgjConfigHandlerNilDeps(t *testing.T) {
	h := NewXianguanjiaConfigHandler(nil, nil, nil)
	w := doXgjRequest(t, h, http.MethodGet, "/admin/xianguanjia/config", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// 探活客户端走真实签名逻辑（httptest mock 服务端校验 query 三参数），不打真实接口。
func TestXgjProbeAuthorizeListMockServer(t *testing.T) {
	var gotQuery url.Values
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"list":[]}}`))
	}))
	defer srv.Close()

	probe := httpClientProbe{client: srv.Client()}
	err := probe.Probe(context.Background(), srv.URL, "test-key", "test-secret")
	require.NoError(t, err)
	require.Equal(t, "test-key", gotQuery.Get("appid"))
	require.NotEmpty(t, gotQuery.Get("timestamp"))
	require.NotEmpty(t, gotQuery.Get("sign"))
	require.Equal(t, "{}", string(gotBody))
}

var _ service.SecretEncryptor = (*fakeXgjDecryptor)(nil)
