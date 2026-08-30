package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestXianyuWorkerClientHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/health", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"backend":true,"websocket":true,"database":true}}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	health, err := client.Health(context.Background())
	require.NoError(t, err)
	require.True(t, health.Backend)
	require.True(t, health.WebSocket)
	require.True(t, health.Database)
}

func TestXianyuWorkerClientHealthAcceptsWorkerStringDatabase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"status":"running","database":"connected"}}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	health, err := client.Health(context.Background())
	require.NoError(t, err)
	require.True(t, health.Backend)
	require.True(t, health.Database)
	require.False(t, health.WebSocket)
}

func TestXianyuWorkerClientHealthAcceptsBareResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"backend":true,"websocket":true,"database":true}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	health, err := client.Health(context.Background())
	require.NoError(t, err)
	require.True(t, health.Backend)
	require.True(t, health.WebSocket)
	require.True(t, health.Database)
}

func TestXianyuWorkerClientListAccountsUsesWorkerProjectionRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/internal/cookies/details", r.URL.Path)
		require.Equal(t, "token", r.Header.Get("X-Worker-Token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"查询成功","data":[{"account_id":"a1","nickname":"n1","enabled":true,"status":"active"}]}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	accounts, err := client.ListAccounts(context.Background())
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, "a1", accounts[0].AccountID)
	require.True(t, accounts[0].Enabled)
}

func TestXianyuWorkerClientCreateAndQueryLoginSessionUsesSessionID(t *testing.T) {
	var createBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/internal/qr-login/generate":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&createBody))
			_, _ = w.Write([]byte(`{"success":true,"message":"二维码生成成功","data":{"session_id":"sess-1","qr_code_url":"data:image/png;base64,xx","status":"waiting"}}`))
		case "/api/v1/internal/qr-login/status/sess-1":
			_, _ = w.Write([]byte(`{"success":true,"message":"查询成功","data":{"status":"success","session_id":"sess-1"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	created, err := client.CreateLoginSession(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "sess-1", created.SessionID)
	require.Equal(t, "waiting", created.Status)

	status, err := client.QueryLoginSession(context.Background(), "sess-1")
	require.NoError(t, err)
	require.Equal(t, "success", status.Status)
	require.Equal(t, "sess-1", status.SessionID)
}

func TestXianyuWorkerClientEnableDisableUsesStatusEndpoint(t *testing.T) {
	var enableBody map[string]bool
	var disableBody map[string]bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.Equal(t, "/api/v1/internal/cookies/a1/status", r.URL.Path)
		switch r.Method {
		case http.MethodPut:
			var body map[string]bool
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if body["enabled"] {
				enableBody = body
			} else {
				disableBody = body
			}
			_, _ = w.Write([]byte(`{"success":true,"message":"账号状态已更新"}`))
		default:
			t.Fatalf("unexpected method: %s", r.Method)
		}
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	require.NoError(t, client.EnableAccount(context.Background(), "a1"))
	require.NoError(t, client.DisableAccount(context.Background(), "a1"))
	require.True(t, enableBody["enabled"])
	require.False(t, disableBody["enabled"])
}

func TestXianyuWorkerClientRefreshCookieUsesRenewLogin(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/internal/cookies/renew-login", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"批量续期完成","data":{"results":[{"account_id":"a1","success":true}],"success_count":1,"failed_count":0}}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	status, err := client.RefreshCookie(context.Background(), "a1")
	require.NoError(t, err)
	require.Equal(t, []any{"a1"}, body["account_ids"])
	require.NotNil(t, status)
}

func TestXianyuWorkerClientListProductsUsesItemsProjection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/internal/items/cookie/a1", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"查询成功","data":[{"item_id":"i1","title":"t1","spec_name":"s","spec_value":"v"}]}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	products, err := client.ListProducts(context.Background(), "a1")
	require.NoError(t, err)
	require.Len(t, products, 1)
	require.Equal(t, "i1", products[0].ItemID)
	require.Equal(t, "s", products[0].SpecName)
	require.Equal(t, "v", products[0].SpecValue)
}

func TestXianyuWorkerClientResendDeliveryUsesBackendForwardAndWaitsReceipt(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/internal/messages/send", r.URL.Path)
		require.Equal(t, "token", r.Header.Get("X-Worker-Token"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"消息发送成功","data":{"send_status":"success","send_fail_reason":null}}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	result, err := client.ResendDelivery(context.Background(), "a1", "o1", "i1", "b1", "c1", "CODE", 0)
	require.NoError(t, err)
	receipt, _ := normalizeSendReceipt(result)
	require.Equal(t, "sent_explicit_success", receipt)
	require.Equal(t, "CODE", body["message"])
	require.Equal(t, "c1", body["chat_id"])
	require.Equal(t, "a1", body["account_id"])
	require.Equal(t, true, body["wait_result"])
	require.NotContains(t, body, "content")
	require.Equal(t, "o1", body["order_no"])
}

func TestXianyuWorkerClientResendDeliveryRejectsConfirmedFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"message":"消息发送失败","data":{"send_status":"failed"}}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	result, err := client.ResendDelivery(context.Background(), "a1", "o1", "i1", "b1", "c1", "CODE", 0)
	require.NoError(t, err)
	receipt, _ := normalizeSendReceipt(result)
	require.Equal(t, "rejected", receipt)
}

func TestXianyuWorkerClientMapErrorToUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":503,"message":"db down"}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	_, err := client.Health(context.Background())
	var workerErr *XianyuWorkerError
	require.ErrorAs(t, err, &workerErr)
	require.Equal(t, http.StatusServiceUnavailable, workerErr.StatusCode)
}

func TestXianyuWorkerClientUnreachableReturnsUnhealthy(t *testing.T) {
	client := NewXianyuWorkerClient("http://127.0.0.1:1", "token", 1)
	_, err := client.Health(context.Background())
	require.ErrorIs(t, err, ErrXianyuWorkerUnhealthy)
}

func TestXianyuWorkerClientEmptyConfigRejected(t *testing.T) {
	client := NewXianyuWorkerClient("", "token", 1)
	_, err := client.Health(context.Background())
	require.ErrorIs(t, err, ErrXianyuDeliveryNotConfigured)
}
