package xianguanjia

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// memoryKindIDStore 是 KindIDStore 的内存实现，仅用于单测。
type memoryKindIDStore struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemoryKindIDStore() *memoryKindIDStore {
	return &memoryKindIDStore{m: make(map[string]string)}
}

func (s *memoryKindIDStore) GetValue(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (s *memoryKindIDStore) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

// newKindFakeServer 构造 mock 闲管家服务端：校验 POST、独立重算四段签名并比对、
// query 只允许 appid/timestamp/sign，按路径返回信封。
func newKindFakeServer(t *testing.T, handler func(w http.ResponseWriter, body []byte)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rawBody, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		q := r.URL.Query()
		expected := Sign(q.Get("appid"), BodyMd5(rawBody), q.Get("timestamp"), fakeAppSecret)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(q.Get("sign"))) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"sign error"}`))
			return
		}
		if q.Get("mch_id") != "" || q.Get("nonce") != "" || q.Get("app_id") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		handler(w, rawBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newKindClient(srv *httptest.Server) *Client {
	return NewClient(ClientConfig{
		BaseURL:    srv.URL,
		AppKey:     fakeAppKey,
		AppSecret:  fakeAppSecret,
		HTTPClient: srv.Client(),
	})
}

func TestKindCreateSuccess(t *testing.T) {
	srv := newKindFakeServer(t, func(w http.ResponseWriter, body []byte) {
		// 校验请求体字段（假设 name/category_id）。
		var req struct {
			Name       string `json:"name"`
			CategoryID int64  `json:"category_id"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Name != "测试卡种" || req.CategoryID != 7 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"msg":"bad body"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"kind_id":12345}}`))
	})
	svc := NewKindService(newKindClient(srv), nil)

	kindID, err := svc.KindCreate(context.Background(), "测试卡种", 7)
	if err != nil {
		t.Fatalf("KindCreate returned error: %v", err)
	}
	if kindID != 12345 {
		t.Fatalf("KindCreate kind_id = %d, want 12345", kindID)
	}
}

func TestKindCreateKindIDAsString(t *testing.T) {
	srv := newKindFakeServer(t, func(w http.ResponseWriter, _ []byte) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"kind_id":"6789"}}`))
	})
	svc := NewKindService(newKindClient(srv), nil)

	kindID, err := svc.KindCreate(context.Background(), "n", 1)
	if err != nil {
		t.Fatalf("KindCreate returned error: %v", err)
	}
	if kindID != 6789 {
		t.Fatalf("KindCreate kind_id = %d, want 6789", kindID)
	}
}

func TestKindCreateAPIError(t *testing.T) {
	srv := newKindFakeServer(t, func(w http.ResponseWriter, _ []byte) {
		_, _ = w.Write([]byte(`{"code":10001,"msg":"category not found"}`))
	})
	svc := NewKindService(newKindClient(srv), nil)

	_, err := svc.KindCreate(context.Background(), "n", 1)
	if err == nil {
		t.Fatal("KindCreate expected error for code!=0")
	}
	if !strings.Contains(err.Error(), "10001") || !strings.Contains(err.Error(), "category not found") {
		t.Fatalf("error should carry code and msg, got: %v", err)
	}
}

func TestKindCreateUnreachable(t *testing.T) {
	// 起一个服务端立刻关掉，保证连接被拒绝。
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	c := NewClient(ClientConfig{
		BaseURL:    srv.URL,
		AppKey:     fakeAppKey,
		AppSecret:  fakeAppSecret,
		HTTPClient: srv.Client(),
	})
	svc := NewKindService(c, nil)

	_, err := svc.KindCreate(context.Background(), "n", 1)
	if err == nil {
		t.Fatal("KindCreate expected error when server unreachable")
	}
}

func TestKindIDSaveAndGet(t *testing.T) {
	svc := NewKindService(nil, newMemoryKindIDStore())
	ctx := context.Background()

	// 未保存时读取应报未配置错误。
	if _, err := svc.GetKindID(ctx); !errors.Is(err, ErrKindIDNotConfigured) {
		t.Fatalf("GetKindID before save = %v, want ErrKindIDNotConfigured", err)
	}

	if err := svc.SaveKindID(ctx, 12345); err != nil {
		t.Fatalf("SaveKindID returned error: %v", err)
	}
	got, err := svc.GetKindID(ctx)
	if err != nil {
		t.Fatalf("GetKindID returned error: %v", err)
	}
	if got != 12345 {
		t.Fatalf("GetKindID = %d, want 12345", got)
	}

	// 覆盖写生效。
	if err := svc.SaveKindID(ctx, 54321); err != nil {
		t.Fatalf("SaveKindID overwrite returned error: %v", err)
	}
	got, err = svc.GetKindID(ctx)
	if err != nil || got != 54321 {
		t.Fatalf("GetKindID after overwrite = %d, %v; want 54321", got, err)
	}

	// 非法 kind_id 拒绝写入。
	if err := svc.SaveKindID(ctx, 0); err == nil {
		t.Fatal("SaveKindID(0) expected error")
	}
}

func TestKindCreateAndPersistRoundTrip(t *testing.T) {
	srv := newKindFakeServer(t, func(w http.ResponseWriter, _ []byte) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"kind_id":888}}`))
	})
	store := newMemoryKindIDStore()
	svc := NewKindService(newKindClient(srv), store)
	ctx := context.Background()

	kindID, err := svc.KindCreate(ctx, "卡种A", 3)
	if err != nil {
		t.Fatalf("KindCreate returned error: %v", err)
	}
	if err := svc.SaveKindID(ctx, kindID); err != nil {
		t.Fatalf("SaveKindID returned error: %v", err)
	}
	got, err := svc.GetKindID(ctx)
	if err != nil || got != 888 {
		t.Fatalf("GetKindID = %d, %v; want 888", got, err)
	}
}
