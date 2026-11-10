//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type crsLargeRequestOnlyRepo struct {
	AccountRepository
	accounts map[string]*Account
	nextID   int64
}

func newCRSLargeRequestOnlyRepo(existing ...*Account) *crsLargeRequestOnlyRepo {
	repo := &crsLargeRequestOnlyRepo{accounts: make(map[string]*Account)}
	for _, account := range existing {
		if account == nil {
			continue
		}
		crsID, _ := account.Extra["crs_account_id"].(string)
		repo.accounts[crsID] = account
		if account.ID > repo.nextID {
			repo.nextID = account.ID
		}
	}
	return repo
}

func (r *crsLargeRequestOnlyRepo) Create(_ context.Context, account *Account) error {
	r.nextID++
	account.ID = r.nextID
	crsID, _ := account.Extra["crs_account_id"].(string)
	r.accounts[crsID] = account
	return nil
}

func (r *crsLargeRequestOnlyRepo) Update(_ context.Context, account *Account) error {
	crsID, _ := account.Extra["crs_account_id"].(string)
	r.accounts[crsID] = account
	return nil
}

func (r *crsLargeRequestOnlyRepo) GetByCRSAccountID(_ context.Context, crsID string) (*Account, error) {
	return r.accounts[crsID], nil
}

func (r *crsLargeRequestOnlyRepo) ListShadowsByParent(_ context.Context, _ int64) ([]*Account, error) {
	return nil, nil
}

// TestCRSSyncLargeRequestOnly CRS 同步不得因来源 Extra 而改写「仅接大请求」开关:
// 该开关只能经 SetAccountLargeRequestOnly(带池覆盖校验)变更,CRS 合并+Update 整体写回会是旁路。
// 本地原本未启用 → 保持未启用;本地已启用 → 不被 CRS 关掉(与 AdminService.UpdateAccount 同口径)。
func TestCRSSyncLargeRequestOnly(t *testing.T) {
	tests := []struct {
		name          string
		collection    string
		credentials   map[string]any
		platform      string
		accountType   string
		sourceExtra   map[string]any
		existingExtra map[string]any
		wantAction    string
		wantEnabled   bool
	}{
		{
			name:        "OpenAI OAuth create ignores source true",
			collection:  "openaiOAuthAccounts",
			credentials: map[string]any{"access_token": "oauth-token"},
			platform:    PlatformOpenAI,
			accountType: AccountTypeOAuth,
			sourceExtra: map[string]any{LargeRequestOnlyExtraKey: true},
			wantAction:  "created",
		},
		{
			name:          "OpenAI OAuth update keeps local disabled when source true",
			collection:    "openaiOAuthAccounts",
			credentials:   map[string]any{"access_token": "oauth-token"},
			platform:      PlatformOpenAI,
			accountType:   AccountTypeOAuth,
			sourceExtra:   map[string]any{LargeRequestOnlyExtraKey: true},
			existingExtra: map[string]any{"existing": true},
			wantAction:    "updated",
		},
		{
			name:          "OpenAI OAuth update keeps local true when source true",
			collection:    "openaiOAuthAccounts",
			credentials:   map[string]any{"access_token": "oauth-token"},
			platform:      PlatformOpenAI,
			accountType:   AccountTypeOAuth,
			sourceExtra:   map[string]any{LargeRequestOnlyExtraKey: true},
			existingExtra: map[string]any{LargeRequestOnlyExtraKey: true},
			wantAction:    "updated",
			wantEnabled:   true,
		},
		{
			name:          "OpenAI OAuth update keeps local true when source omits",
			collection:    "openaiOAuthAccounts",
			credentials:   map[string]any{"access_token": "oauth-token"},
			platform:      PlatformOpenAI,
			accountType:   AccountTypeOAuth,
			existingExtra: map[string]any{LargeRequestOnlyExtraKey: true},
			wantAction:    "updated",
			wantEnabled:   true,
		},
		{
			name:          "OpenAI API key update keeps local disabled when source true",
			collection:    "openaiResponsesAccounts",
			credentials:   map[string]any{"api_key": "sk-test"},
			platform:      PlatformOpenAI,
			accountType:   AccountTypeAPIKey,
			sourceExtra:   map[string]any{LargeRequestOnlyExtraKey: true},
			existingExtra: map[string]any{"existing": true},
			wantAction:    "updated",
		},
		{
			name:        "Anthropic create ignores source true",
			collection:  "claudeAccounts",
			credentials: map[string]any{"access_token": "oauth-token", "authType": "oauth"},
			platform:    PlatformAnthropic,
			accountType: AccountTypeOAuth,
			sourceExtra: map[string]any{LargeRequestOnlyExtraKey: true},
			wantAction:  "created",
		},
		{
			name:          "Anthropic update keeps local disabled when source true",
			collection:    "claudeAccounts",
			credentials:   map[string]any{"access_token": "oauth-token", "authType": "oauth"},
			platform:      PlatformAnthropic,
			accountType:   AccountTypeOAuth,
			sourceExtra:   map[string]any{LargeRequestOnlyExtraKey: true},
			existingExtra: map[string]any{"existing": true},
			wantAction:    "updated",
		},
		{
			name:          "Anthropic API key update keeps local true when source true",
			collection:    "claudeConsoleAccounts",
			credentials:   map[string]any{"api_key": "sk-ant-test"},
			platform:      PlatformAnthropic,
			accountType:   AccountTypeAPIKey,
			sourceExtra:   map[string]any{LargeRequestOnlyExtraKey: true},
			existingExtra: map[string]any{LargeRequestOnlyExtraKey: true},
			wantAction:    "updated",
			wantEnabled:   true,
		},
		{
			name:          "Gemini OAuth update keeps local disabled when source true",
			collection:    "geminiOAuthAccounts",
			credentials:   map[string]any{"refresh_token": "refresh-token"},
			platform:      PlatformGemini,
			accountType:   AccountTypeOAuth,
			sourceExtra:   map[string]any{LargeRequestOnlyExtraKey: true},
			existingExtra: map[string]any{"existing": true},
			wantAction:    "updated",
		},
		{
			name:          "Gemini API key update keeps local true when source true",
			collection:    "geminiApiKeyAccounts",
			credentials:   map[string]any{"api_key": "gemini-key"},
			platform:      PlatformGemini,
			accountType:   AccountTypeAPIKey,
			sourceExtra:   map[string]any{LargeRequestOnlyExtraKey: true},
			existingExtra: map[string]any{LargeRequestOnlyExtraKey: true},
			wantAction:    "updated",
			wantEnabled:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const crsID = "crs-large-request-only-1"
			var existing *Account
			if tt.existingExtra != nil {
				existing = &Account{
					ID:       77,
					Platform: tt.platform,
					Type:     tt.accountType,
					Extra:    mergeMap(tt.existingExtra, map[string]any{"crs_account_id": crsID}),
				}
			}
			repo := newCRSLargeRequestOnlyRepo(existing)
			result := runCRSLargeRequestOnlySync(t, repo, tt.collection, tt.credentials, tt.sourceExtra)

			require.Len(t, result.Items, 1)
			require.Equal(t, tt.wantAction, result.Items[0].Action)
			stored := repo.accounts[crsID]
			require.NotNil(t, stored)
			if tt.wantEnabled {
				require.Equal(t, true, stored.Extra[LargeRequestOnlyExtraKey],
					"CRS 同步不得关闭本地已启用的开关")
				return
			}
			require.NotContains(t, stored.Extra, LargeRequestOnlyExtraKey,
				"CRS 同步不得因来源 extra 打开开关,本地未启用须保持未启用")
		})
	}
}

func runCRSLargeRequestOnlySync(
	t *testing.T,
	repo AccountRepository,
	collection string,
	credentials map[string]any,
	sourceExtra map[string]any,
) *SyncFromCRSResult {
	t.Helper()
	account := map[string]any{
		"kind":        "crs-kind",
		"id":          "crs-large-request-only-1",
		"name":        "CRS Large Request Only",
		"isActive":    true,
		"schedulable": true,
		"credentials": credentials,
	}
	if sourceExtra != nil {
		account["extra"] = sourceExtra
	}

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/web/auth/login" {
			_, _ = response.Write([]byte(`{"success":true,"token":"admin-token"}`))
			return
		}
		require.Equal(t, "/admin/sync/export-accounts", request.URL.Path)
		require.NoError(t, json.NewEncoder(response).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{collection: []any{account}},
		}))
	}))
	t.Cleanup(server.Close)

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	svc := NewCRSSyncService(repo, nil, nil, nil, nil, cfg)
	result, err := svc.SyncFromCRS(context.Background(), SyncFromCRSInput{
		BaseURL:  server.URL,
		Username: "admin",
		Password: "password",
	})
	require.NoError(t, err)
	return result
}
