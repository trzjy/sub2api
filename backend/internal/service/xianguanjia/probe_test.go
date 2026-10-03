package xianguanjia

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ProbeAuthorizeList：mock 服务端校验签名三参数与信封解析；失败路径分别覆盖非 2xx 与坏信封。
func TestProbeAuthorizeListSuccess(t *testing.T) {
	var gotQuery url.Values
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
		// 用官方算法重算签名并比对（服务端视角）。
		want := Sign("k1", BodyMd5(buf[:n]), gotQuery.Get("timestamp"), "s1")
		if gotQuery.Get("sign") != want {
			t.Errorf("sign mismatch: got %s want %s", gotQuery.Get("sign"), want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"list":[]}}`))
	}))
	defer srv.Close()

	if err := ProbeAuthorizeList(context.Background(), srv.Client(), srv.URL, "k1", "s1"); err != nil {
		t.Fatalf("probe returned error: %v", err)
	}
	if gotQuery.Get("appid") != "k1" {
		t.Fatalf("appid = %q, want k1", gotQuery.Get("appid"))
	}
	if ts := gotQuery.Get("timestamp"); len(ts) != 10 {
		t.Fatalf("timestamp = %q, want unix seconds", ts)
	}
	if string(gotBody) != "{}" {
		t.Fatalf("body = %q, want {}", string(gotBody))
	}
}

func TestProbeAuthorizeListHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"msg":"ip not allowed","data":{}}`))
	}))
	defer srv.Close()
	err := ProbeAuthorizeList(context.Background(), srv.Client(), srv.URL, "k", "s")
	if err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("want status 403 error, got %v", err)
	}
}

func TestProbeAuthorizeListBadEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":401,"msg":"sign error","data":{}}`))
	}))
	defer srv.Close()
	err := ProbeAuthorizeList(context.Background(), srv.Client(), srv.URL, "k", "s")
	if err == nil || !strings.Contains(err.Error(), "code 401") {
		t.Fatalf("want envelope code 401 error, got %v", err)
	}
}

func TestProbeAuthorizeListMissingCreds(t *testing.T) {
	err := ProbeAuthorizeList(context.Background(), http.DefaultClient, "https://example.invalid", "", "s")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("want not-configured error, got %v", err)
	}
}

// ConfigStore 写侧单测：编译期验证 configStoreDB 满足 ConfigStore 接口（DB 行为由 admin 层 fake 覆盖，DB 交互由 sqlmock 覆盖）。
func TestConfigStoreDBImplementsConfigStore(t *testing.T) {
	var _ ConfigStore = NewConfigStore(nil)
}

// UpsertConfig 空参容错：nil receiver / nil db 返回错误。
func TestConfigStoreUpsertUnavailable(t *testing.T) {
	var s *configStoreDB
	if err := s.UpsertConfig(context.Background(), ConfigUpsert{}); err == nil {
		t.Fatal("want error for nil store")
	}
	if err := s.UpdateHealth(context.Background(), "healthy", time.Now()); err == nil {
		t.Fatal("want error for nil store")
	}
	if _, err := s.GetConfigRow(context.Background()); err == nil {
		t.Fatal("want error for nil store")
	}
}

