package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	BalanceProbeConfigCredentialKey = "balance_probe"
	balanceProbeTimeout             = 10 * time.Second
	balanceProbeMaxBodyBytes        = 256 * 1024
)

type BalanceProbeConfig struct {
	Enabled    bool   `json:"enabled"`
	URL        string `json:"url"`
	BearerAuth bool   `json:"bearer_auth"`
}

type BalanceProbeResult struct {
	Success    bool      `json:"success"`
	Remaining  *float64  `json:"remaining"`
	Unit       string    `json:"unit,omitempty"`
	Valid      bool      `json:"valid"`
	StatusCode int       `json:"status_code"`
	FetchedAt  time.Time `json:"fetched_at"`
	Error      string    `json:"error,omitempty"`
}

func (a *Account) BalanceProbeConfig() BalanceProbeConfig {
	config := BalanceProbeConfig{}
	rawValue, ok := a.Credentials[BalanceProbeConfigCredentialKey]
	raw, ok := rawValue.(map[string]any)
	if !ok {
		return config
	}
	if enabled, ok := raw["enabled"].(bool); ok {
		config.Enabled = enabled
	}
	if value, ok := raw["url"].(string); ok {
		config.URL = strings.TrimSpace(value)
	}
	if value, ok := raw["bearer_auth"].(bool); ok {
		config.BearerAuth = value
	}
	return config
}

func (config BalanceProbeConfig) normalizedURL(account *Account) (string, error) {
	url := strings.TrimSpace(config.URL)
	if url == "" {
		url = strings.TrimRight(account.GetCredential("base_url"), "/") + "/v1/usage"
	}
	if !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("balance probe URL must use https")
	}
	return url, nil
}

func (config BalanceProbeConfig) apiKey(account *Account) string {
	apiKey := account.GetCredential("api_key")
	if apiKey == "" {
		apiKey = account.GetOpenAIApiKey()
	}
	return apiKey
}

type AccountBalanceProbeService struct {
	accountRepo  AccountRepository
	httpUpstream HTTPUpstream
}

func NewAccountBalanceProbeService(accountRepo AccountRepository, httpUpstream HTTPUpstream) *AccountBalanceProbeService {
	return &AccountBalanceProbeService{accountRepo: accountRepo, httpUpstream: httpUpstream}
}

func ProvideAccountBalanceProbeService(accountRepo AccountRepository, httpUpstream HTTPUpstream) *AccountBalanceProbeService {
	return NewAccountBalanceProbeService(accountRepo, httpUpstream)
}

func (s *AccountBalanceProbeService) Query(ctx context.Context, accountID int64) (*BalanceProbeResult, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, fmt.Errorf("account balance probe service is not enabled")
	}
	account, err := s.account(ctx, accountID)
	if err != nil {
		return nil, err
	}
	config := account.BalanceProbeConfig()
	if !config.Enabled {
		return nil, fmt.Errorf("balance probe is not enabled")
	}
	apiKey := config.apiKey(account)
	if apiKey == "" {
		return nil, fmt.Errorf("balance probe api key is empty")
	}

	probeURL, err := config.normalizedURL(account)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, balanceProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build balance probe request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if config.BearerAuth {
		request.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		request.Header.Set("Authorization", apiKey)
	}
	account.ApplyHeaderOverrides(request.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	response, err := s.httpUpstream.Do(request, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, fmt.Errorf("balance probe request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, balanceProbeMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read balance probe response: %w", err)
	}
	if len(body) > balanceProbeMaxBodyBytes {
		return nil, fmt.Errorf("balance probe response too large")
	}

	result := &BalanceProbeResult{
		StatusCode: response.StatusCode,
		FetchedAt:  time.Now(),
		Unit:       "USD",
		Valid:      true,
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.Error = fmt.Sprintf("API error (HTTP %d)", response.StatusCode)
		return result, nil
	}
	if !json.Valid(body) {
		result.Error = "invalid balance probe response"
		return result, nil
	}

	remaining := firstJSONNumber(body, "remaining", "quota.remaining", "balance")
	if remaining == nil {
		result.Error = "balance field not found"
		return result, nil
	}
	if value := gjson.GetBytes(body, "unit").String(); value != "" {
		result.Unit = value
	} else if value := gjson.GetBytes(body, "quota.unit").String(); value != "" {
		result.Unit = value
	}
	if value := gjson.GetBytes(body, "is_active"); value.Exists() {
		result.Valid = value.Bool()
	} else if value := gjson.GetBytes(body, "isValid"); value.Exists() {
		result.Valid = value.Bool()
	}
	result.Success = true
	result.Remaining = remaining
	return result, nil
}

func (s *AccountBalanceProbeService) account(ctx context.Context, accountID int64) (*Account, error) {
	if s.accountRepo == nil {
		return nil, fmt.Errorf("account repository is not available")
	}
	return s.accountRepo.GetByID(ctx, accountID)
}

func firstJSONNumber(body []byte, paths ...string) *float64 {
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if !value.Exists() {
			continue
		}
		if number, err := strconv.ParseFloat(value.String(), 64); err == nil {
			return &number
		}
	}
	return nil
}
