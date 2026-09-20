package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLocalCaptchaHelperHTTPClientFlow(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "test-secret", r.Header.Get("X-API-Key"))
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/challenge/sdk-start":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "kimi", body["platform"])
			require.Equal(t, "opaque-login", body["login_session_id"])
			require.Equal(t, "13800138000", body["phone"])
			_, _ = w.Write([]byte(`{"success":true,"session_id":"helper-1","status":"pending"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/challenge/helper-1/status":
			require.Equal(t, "kimi", r.URL.Query().Get("platform"))
			require.Equal(t, "opaque-login", r.URL.Query().Get("login_session_id"))
			require.Equal(t, "13800138000", r.URL.Query().Get("phone"))
			_, _ = w.Write([]byte(`{"success":true,"status":"ok"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/challenge/helper-1/result":
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, map[string]string{
				"platform": "kimi", "login_session_id": "opaque-login", "phone": "13800138000",
			}, body)
			_, _ = w.Write([]byte(`{"success":true,"status":"ok","data":{"validate":"v-1"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewLocalCaptchaHelperHTTPClient(LocalCaptchaHelperConfig{BaseURL: server.URL, APIKey: "test-secret", Timeout: 2})
	session, err := client.Start(context.Background(), PlatformKimi, "opaque-login", "13800138000", "")
	require.NoError(t, err)
	require.Equal(t, "helper-1", session.ID)
	status, err := client.Status(context.Background(), session, PlatformKimi, "opaque-login", "13800138000")
	require.NoError(t, err)
	require.Equal(t, "ok", status)
	result, err := client.Result(context.Background(), session, PlatformKimi, "opaque-login", "13800138000")
	require.NoError(t, err)
	require.Equal(t, "ok", result.Status)
	require.Equal(t, "v-1", result.Data["validate"])
	require.Equal(t, []string{"/challenge/sdk-start", "/challenge/helper-1/status", "/challenge/helper-1/result"}, paths)
}

func TestLocalCaptchaHelperHTTPClientFailsClosedForHTTPStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized},
		{name: "not found", statusCode: http.StatusNotFound},
		{name: "conflict", statusCode: http.StatusConflict},
		{name: "unprocessable entity", statusCode: http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(`{"success":false,"message":"request rejected"}`))
			}))
			defer server.Close()

			client := NewLocalCaptchaHelperHTTPClient(LocalCaptchaHelperConfig{BaseURL: server.URL, APIKey: "test-secret", Timeout: 2})
			_, err := client.Start(context.Background(), PlatformKimi, "opaque-login", "13800138000", "")
			require.ErrorContains(t, err, "helper HTTP "+strconv.Itoa(tc.statusCode))
			require.NotContains(t, err.Error(), "request rejected")
		})
	}
}

func TestLocalCaptchaHelperHTTPClientFailsClosedForMissingRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		response string
		call     func(*LocalCaptchaHelperHTTPClient) error
		want     string
	}{
		{
			name:     "start missing session_id",
			path:     "/challenge/sdk-start",
			response: `{"success":true,"status":"pending"}`,
			call: func(client *LocalCaptchaHelperHTTPClient) error {
				_, err := client.Start(context.Background(), PlatformKimi, "opaque-login", "13800138000", "")
				return err
			},
			want: "missing session_id",
		},
		{
			name:     "status missing status",
			path:     "/challenge/helper-1/status",
			response: `{"success":true}`,
			call: func(client *LocalCaptchaHelperHTTPClient) error {
				_, err := client.Status(context.Background(), LocalCaptchaHelperSession{ID: "helper-1"}, PlatformKimi, "opaque-login", "13800138000")
				return err
			},
			want: "missing status",
		},
		{
			name:     "result missing status",
			path:     "/challenge/helper-1/result",
			response: `{"success":true,"data":{"validate":"v-1"}}`,
			call: func(client *LocalCaptchaHelperHTTPClient) error {
				_, err := client.Result(context.Background(), LocalCaptchaHelperSession{ID: "helper-1"}, PlatformKimi, "opaque-login", "13800138000")
				return err
			},
			want: "missing status",
		},
		{
			name:     "result missing data",
			path:     "/challenge/helper-1/result",
			response: `{"success":true,"status":"ok"}`,
			call: func(client *LocalCaptchaHelperHTTPClient) error {
				_, err := client.Result(context.Background(), LocalCaptchaHelperSession{ID: "helper-1"}, PlatformKimi, "opaque-login", "13800138000")
				return err
			},
			want: "missing data",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, tc.path, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()

			client := NewLocalCaptchaHelperHTTPClient(LocalCaptchaHelperConfig{BaseURL: server.URL, APIKey: "test-secret", Timeout: 2})
			err := tc.call(client)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLocalCaptchaHelperHTTPClientRejectsUnsuccessfulStatusAndResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/challenge/helper-1/status":
			_, _ = w.Write([]byte(`{"success":false,"status":"ok","message":"challenge failed"}`))
		case "/challenge/helper-1/result":
			_, _ = w.Write([]byte(`{"success":false,"status":"ok","message":"challenge failed"}`))
		case "/challenge/helper-2/result":
			_, _ = w.Write([]byte(`{"success":true,"data":{"validate":"v-2"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewLocalCaptchaHelperHTTPClient(LocalCaptchaHelperConfig{BaseURL: server.URL, Timeout: 2})
	session := LocalCaptchaHelperSession{ID: "helper-1"}
	_, err := client.Status(context.Background(), session, PlatformKimi, "opaque-login", "13800138000")
	require.ErrorContains(t, err, "helper status failed")
	_, err = client.Result(context.Background(), session, PlatformKimi, "opaque-login", "13800138000")
	require.ErrorContains(t, err, "helper result failed")

	_, err = client.Result(context.Background(), LocalCaptchaHelperSession{ID: "helper-2"}, PlatformKimi, "opaque-login", "13800138000")
	require.ErrorContains(t, err, "helper result missing status")
}

func TestLocalCaptchaHelperHTTPClientRejectsIncompleteAndTimeout(t *testing.T) {
	client := NewLocalCaptchaHelperHTTPClient(LocalCaptchaHelperConfig{BaseURL: "http://127.0.0.1:1", Timeout: 1})
	_, err := client.Start(context.Background(), PlatformZhipu, "opaque-login", "13800138000", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "phone_code")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client = NewLocalCaptchaHelperHTTPClient(LocalCaptchaHelperConfig{BaseURL: server.URL, Timeout: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = client.Status(ctx, LocalCaptchaHelperSession{ID: "helper-1"}, PlatformKimi, "opaque-login", "13800138000")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "Client.Timeout exceeded"))
}
