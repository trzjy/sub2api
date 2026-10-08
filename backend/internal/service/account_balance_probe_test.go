package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type balanceProbeUpstream struct {
	request *http.Request
}

func (u *balanceProbeUpstream) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.request = request
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}
	response.Body = http.NoBody
	return response, nil
}

func (u *balanceProbeUpstream) DoWithTLS(request *http.Request, _ string, _ int64, _ int, _ any) (*http.Response, error) {
	return u.Do(request, "", 0, 0)
}

func TestBalanceProbeConfig(t *testing.T) {
	account := &Account{Credentials: map[string]any{
		"balance_probe": map[string]any{
			"enabled":     true,
			"url":         " https://example.test/v1/usage ",
			"bearer_auth": true,
		},
		"base_url": "https://example.test",
	}}

	config := account.BalanceProbeConfig()
	require.True(t, config.Enabled)
	require.Equal(t, "https://example.test/v1/usage", config.URL)
	require.True(t, config.BearerAuth)
}

func TestBalanceProbeConfigDefaultsToUsageEndpoint(t *testing.T) {
	account := &Account{Credentials: map[string]any{"base_url": "https://example.test/"}}

	config := account.BalanceProbeConfig()
	url, err := config.normalizedURL(account)
	require.NoError(t, err)
	require.Equal(t, "https://example.test/v1/usage", url)
}

func TestBalanceProbeNormalizedURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		config  BalanceProbeConfig
		want    string
	}{
		{
			name:    "base_url without /v1",
			baseURL: "https://api.example.com",
			config:  BalanceProbeConfig{},
			want:    "https://api.example.com/v1/usage",
		},
		{
			name:    "base_url ends with /v1",
			baseURL: "https://api.example.com/v1",
			config:  BalanceProbeConfig{},
			want:    "https://api.example.com/v1/usage",
		},
		{
			name:    "base_url ends with /v1/ trailing slash",
			baseURL: "https://api.example.com/v1/",
			config:  BalanceProbeConfig{},
			want:    "https://api.example.com/v1/usage",
		},
		{
			name:    "explicit url config used as-is",
			baseURL: "https://api.example.com",
			config:  BalanceProbeConfig{URL: " https://explicit.test/custom/path "},
			want:    "https://explicit.test/custom/path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{Credentials: map[string]any{"base_url": tt.baseURL}}
			url, err := tt.config.normalizedURL(account)
			require.NoError(t, err)
			require.Equal(t, tt.want, url)
		})
	}
}

func TestFirstJSONNumberPriority(t *testing.T) {
	body := []byte(`{"balance":3,"quota":{"remaining":2},"remaining":1}`)
	require.NotNil(t, firstJSONNumber(body, "remaining", "quota.remaining", "balance"))
	require.Equal(t, 1.0, *firstJSONNumber(body, "remaining", "quota.remaining", "balance"))
}

func TestBalanceProbeUpstreamMockContract(t *testing.T) {
	upstream := &balanceProbeUpstream{}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test/v1/usage", nil)
	require.NoError(t, err)
	response, err := upstream.Do(request, "", 1, 1)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
}
