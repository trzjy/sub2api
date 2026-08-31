package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	BalanceProbeConfigCredentialKey = "balance_probe"
	balanceProbeTimeout             = 10 * time.Second
	balanceProbeMaxBodyBytes        = 256 * 1024
	oneAPIDefaultQuotaPerUSD        = 500000.0
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
	if a == nil {
		return config
	}
	rawValue, configured := a.Credentials[BalanceProbeConfigCredentialKey]
	raw, ok := rawValue.(map[string]any)
	if !ok {
		// One-API/New-API compatible relays expose the same authenticated
		// /api/user/self endpoint. Opt API-key accounts in automatically when
		// no explicit balance_probe setting exists.
		if !configured && a != nil && a.Type == AccountTypeAPIKey && strings.TrimSpace(a.GetCredential("base_url")) != "" {
			config.Enabled = true
			config.BearerAuth = true
		}
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
		base := strings.TrimRight(account.GetCredential("base_url"), "/")
		url = base + "/v1/usage"
		if strings.HasSuffix(base, "/v1") {
			url = base + "/usage"
		}
	}
	if !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("balance probe URL must use https")
	}
	return url, nil
}

// probeURLs returns the standard relay endpoint first, followed by the
// legacy OpenAI-compatible usage endpoint. Explicit URLs remain single-target.
func (config BalanceProbeConfig) probeURLs(account *Account) ([]string, error) {
	if strings.TrimSpace(config.URL) != "" {
		probeURL, err := config.normalizedURL(account)
		if err != nil {
			return nil, err
		}
		return []string{probeURL}, nil
	}
	base := strings.TrimSpace(account.GetCredential("base_url"))
	if base == "" {
		return nil, fmt.Errorf("balance probe base URL is empty")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("balance probe URL must use https")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	path := basePath
	if strings.HasSuffix(path, "/v1") {
		path = strings.TrimSuffix(path, "/v1")
	}
	parsed.Path = strings.TrimRight(path, "/") + "/api/user/self"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	standard := parsed.String()
	legacyPath := basePath
	if !strings.HasSuffix(legacyPath, "/v1") {
		legacyPath += "/v1"
	}
	legacyParsed := *parsed
	legacyParsed.Path = legacyPath + "/usage"
	legacy := legacyParsed.String()
	if standard == legacy {
		return []string{standard}, nil
	}
	return []string{standard, legacy}, nil
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

	probeURLs, err := config.probeURLs(account)
	if err != nil {
		return nil, err
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	var last *BalanceProbeResult
	for i, probeURL := range probeURLs {
		callCtx, cancel := context.WithTimeout(ctx, balanceProbeTimeout)
		request, requestErr := http.NewRequestWithContext(callCtx, http.MethodGet, probeURL, nil)
		if requestErr != nil {
			cancel()
			return nil, fmt.Errorf("build balance probe request: %w", requestErr)
		}
		request.Header.Set("Accept", "application/json")
		if config.BearerAuth {
			request.Header.Set("Authorization", "Bearer "+apiKey)
		} else {
			request.Header.Set("Authorization", apiKey)
		}
		account.ApplyHeaderOverrides(request.Header)
		response, requestErr := s.httpUpstream.Do(request, proxyURL, account.ID, maxInt(account.Concurrency, 1))
		if requestErr != nil {
			cancel()
			if i+1 < len(probeURLs) {
				continue
			}
			return nil, fmt.Errorf("balance probe request failed: %w", requestErr)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, balanceProbeMaxBodyBytes+1))
		_ = response.Body.Close()
		cancel()
		if readErr != nil {
			return nil, fmt.Errorf("read balance probe response: %w", readErr)
		}
		if len(body) > balanceProbeMaxBodyBytes {
			return nil, fmt.Errorf("balance probe response too large")
		}
		result := parseBalanceProbeResponse(response.StatusCode, body)
		if result.Success || i+1 == len(probeURLs) {
			return result, nil
		}
		last = result
	}
	return last, nil
}

func parseBalanceProbeResponse(statusCode int, body []byte) *BalanceProbeResult {
	result := &BalanceProbeResult{StatusCode: statusCode, FetchedAt: time.Now(), Unit: "USD", Valid: true}
	if statusCode < 200 || statusCode >= 300 {
		result.Error = fmt.Sprintf("API error (HTTP %d)", statusCode)
		return result
	}
	if !json.Valid(body) {
		result.Error = "invalid balance probe response"
		return result
	}
	// Supports generic usage responses and One-API/New-API's data.quota.
	remaining := firstJSONNumber(body,
		"remaining", "quota.remaining", "balance", "data.remaining", "data.remaining_quota",
		"data.quota", "data.remain_quota", "data.balance", "data.user.quota", "data.user.remain_quota")
	if remaining == nil {
		result.Error = "balance field not found"
		return result
	}
	if value := gjson.GetBytes(body, "unit").String(); value != "" {
		result.Unit = value
	} else if value := gjson.GetBytes(body, "data.unit").String(); value != "" {
		result.Unit = value
	} else if gjson.GetBytes(body, "data.quota").Exists() || gjson.GetBytes(body, "data.remain_quota").Exists() || gjson.GetBytes(body, "data.user.quota").Exists() || gjson.GetBytes(body, "data.user.remain_quota").Exists() {
		// One-API/New-API stores wallet quota as integer units (500,000 = $1).
		*remaining /= oneAPIDefaultQuotaPerUSD
		result.Unit = "USD"
	}
	if value := gjson.GetBytes(body, "is_active"); value.Exists() {
		result.Valid = value.Bool()
	} else if value := gjson.GetBytes(body, "data.status"); value.Exists() {
		result.Valid = value.String() == "active"
	} else if value := gjson.GetBytes(body, "isValid"); value.Exists() {
		result.Valid = value.Bool()
	}
	result.Success = true
	result.Remaining = remaining
	return result
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
