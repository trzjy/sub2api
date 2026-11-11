package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func withTestBaseURL(t *testing.T, url string) {
	t.Helper()
	oldBaseURLs := append([]string(nil), BaseURLs...)
	oldAvailability := DefaultURLAvailability
	t.Cleanup(func() {
		BaseURLs = oldBaseURLs
		DefaultURLAvailability = oldAvailability
	})
	BaseURLs = []string{url}
	DefaultURLAvailability = NewURLAvailability(time.Minute)
}

func TestRetrieveUserQuota_Success(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"buckets":[{"modelId":"gemini-3.8-flash","tokenType":"REQUESTS","remainingFraction":0.25,"resetTime":"2026-01-02T00:00:00Z"}]}`))
	}))
	defer server.Close()
	withTestBaseURL(t, server.URL)

	client, err := NewClient("")
	require.NoError(t, err)

	resp, raw, err := client.RetrieveUserQuota(context.Background(), "tok", "proj", 1<<20)
	require.NoError(t, err)

	require.Equal(t, "/v1internal:retrieveUserQuota", gotPath)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "Bearer tok", gotAuth)
	require.Equal(t, "proj", gotBody["project"])

	require.Len(t, resp.Buckets, 1)
	require.Equal(t, "gemini-3.8-flash", resp.Buckets[0].ModelID)
	require.Equal(t, 0.25, resp.Buckets[0].RemainingFraction)
	require.Equal(t, "2026-01-02T00:00:00Z", resp.Buckets[0].ResetTime)
	require.NotNil(t, raw)
}

func TestRetrieveUserQuota_Forbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"VALIDATION_REQUIRED"}}`))
	}))
	defer server.Close()
	withTestBaseURL(t, server.URL)

	client, err := NewClient("")
	require.NoError(t, err)

	_, _, err = client.RetrieveUserQuota(context.Background(), "tok", "proj", 1<<20)
	require.Error(t, err)

	var forbidden *ForbiddenError
	require.True(t, errors.As(err, &forbidden))
	require.Equal(t, http.StatusForbidden, forbidden.StatusCode)
}

func TestRetrieveUserQuota_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer server.Close()
	withTestBaseURL(t, server.URL)

	client, err := NewClient("")
	require.NoError(t, err)

	_, _, err = client.RetrieveUserQuota(context.Background(), "tok", "proj", 1<<20)
	require.ErrorContains(t, err, "retrieveUserQuota")
}
