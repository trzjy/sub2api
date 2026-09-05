package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// QueryLoginSession 按 Worker session_id 查询扫码会话状态。
func TestXianyuControlServiceQueryLoginSessionUsesSessionID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/internal/qr-login/status/sess-9", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"查询成功","data":{"status":"success","session_id":"sess-9"}}`))
	}))
	defer srv.Close()

	cfg := &XianyuWorkerConfig{
		ID:                1,
		BaseURL:           srv.URL,
		APITokenEncrypted: "ENC:token",
		Status:            XianyuWorkerStatusActive,
	}
	worker := &XianyuWorkerService{
		control:    &xianyuWorkerControlStub{cfg: cfg},
		encryptor:  testSecretEncryptor{},
		forbidLoop: true,
		clientFor: func(baseURL, token string) *XianyuWorkerClient {
			return NewXianyuWorkerClient(baseURL, token, 5*time.Second)
		},
	}
	ctrl := &XianyuControlService{worker: worker}

	status, err := ctrl.QueryLoginSession(context.Background(), "sess-9")
	require.NoError(t, err)
	require.Equal(t, "success", status.Status)
	require.Equal(t, "sess-9", status.SessionID)
}

func TestXianyuControlServiceQueryLoginSessionWorkerNotConfigured(t *testing.T) {
	ctrl := &XianyuControlService{worker: nil}
	_, err := ctrl.QueryLoginSession(context.Background(), "sess-9")
	require.ErrorIs(t, err, ErrXianyuDeliveryNotConfigured)
}

func TestXianyuControlServiceQueryLoginSessionPropagatesWorkerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"session not found"}`))
	}))
	defer srv.Close()

	cfg := &XianyuWorkerConfig{
		ID:                1,
		BaseURL:           srv.URL,
		APITokenEncrypted: "ENC:token",
		Status:            XianyuWorkerStatusActive,
	}
	worker := &XianyuWorkerService{
		control:    &xianyuWorkerControlStub{cfg: cfg},
		encryptor:  testSecretEncryptor{},
		forbidLoop: true,
		clientFor: func(baseURL, token string) *XianyuWorkerClient {
			return NewXianyuWorkerClient(baseURL, token, 5*time.Second)
		},
	}
	ctrl := &XianyuControlService{worker: worker}

	_, err := ctrl.QueryLoginSession(context.Background(), "sess-9")
	require.Error(t, err)
	var we *XianyuWorkerError
	require.True(t, errors.As(err, &we))
}
