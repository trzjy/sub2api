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

func TestXianyuWorkerClientListAccounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/accounts", r.URL.Path)
		require.Equal(t, "token", r.Header.Get("X-Worker-Token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"account_id":"a1","nickname":"n1","cookie_status":"valid","task_status":"running"}]`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	accounts, err := client.ListAccounts(context.Background())
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, "a1", accounts[0].AccountID)
	require.Equal(t, "running", accounts[0].TaskStatus)
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

func TestXianyuWorkerClientListProducts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/accounts/a1/products", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"item_id":"i1","title":"t1","spec_name":"s","spec_value":"v"}]`))
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

func TestXianyuWorkerClientResendDeliveryAcceptsWrappedSendStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/accounts/a1/send-message", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"success":true,"data":{"send_status":"success"}}}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	result, err := client.ResendDelivery(context.Background(), "a1", "o1", "i1", "b1", "c1", "CODE")
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "success", result.Data.SendStatus)
}

func TestXianyuWorkerClientResendDeliveryUsesWorkerContract(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/accounts/a1/send-message", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"send_status":"success"}}`))
	}))
	defer srv.Close()

	client := NewXianyuWorkerClient(srv.URL, "token", 5*time.Second)
	_, err := client.ResendDelivery(context.Background(), "a1", "o1", "i1", "b1", "c1", "CODE")
	require.NoError(t, err)
	require.Equal(t, "CODE", body["message"])
	require.Equal(t, "c1", body["chat_id"])
	require.NotContains(t, body, "content")
}
