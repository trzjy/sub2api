//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

type modelScopeRateLimitRepo struct {
	*probeRateLimitRepo
	clearedScopes []string
}

type modelProbeSchedulePatchRecorder struct {
	*probeRepoMock
	accountID   int64
	scope       string
	expectedGen int64
	patch       ModelProbePatch
}

type modelScopeBalanceProbeRepo struct {
	AccountRepository
	account *Account
}

func (r *modelScopeBalanceProbeRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *modelProbeSchedulePatchRecorder) PatchModelRateLimitEntry(_ context.Context, accountID int64, scope string, expectedGen int64, patch ModelProbePatch) (bool, error) {
	r.accountID = accountID
	r.scope = scope
	r.expectedGen = expectedGen
	r.patch = patch
	return true, nil
}

func (r *modelScopeRateLimitRepo) CommitModelRateLimitObservationIfVersion(ctx context.Context, id int64, scope string, entry map[string]any, clear bool, lastEventAt time.Time, revision int64, expectedLastEventAt string) (bool, error) {
	r.clearedScopes = append(r.clearedScopes, scope)
	return true, nil
}

func TestModelScopeRecoveryUsesScopeDataPlaneInsteadOfAccountBalance(t *testing.T) {
	const scope = "scope-model"
	account := &Account{
		ID:       701,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "scope-test-key",
			"base_url": "https://api.openai.com/v1",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true,
				"url":     "https://balance.example.test/usage",
			},
			"model_mapping": map[string]any{
				"request-model": scope,
				scope:           "wrong-remapped-model",
			},
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}
	repo := &probeRepoMock{accounts: map[int64]*Account{account.ID: account}}
	rateRepo := &modelScopeRateLimitRepo{probeRateLimitRepo: &probeRateLimitRepo{}}
	rl := NewRateLimitService(rateRepo, nil, &config.Config{}, nil, nil)
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	p.recoveryLock = &fakeLeaderLockCache{}
	// BalanceProbeConfig is enabled, but no balance service is wired. A successful
	// scope recovery therefore demonstrates that account-level balance is not its proof.
	p.probeModelScope(context.Background(), account, ModelProbeTask{
		AccountID: account.ID,
		Scope:     scope,
		Entry:     map[string]any{"probe_gen": int64(1), "precise_reset": false},
	}, "scope-test")

	require.Equal(t, []string{scope}, rateRepo.clearedScopes)
	rec := upstream.lastRecord(t)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Equal(t, scope, body["model"], "scope is already mapped and must not be remapped")
}

func TestModelScopeCodeBuddyDirectProbeUsesRequestedScope(t *testing.T) {
	account := acB1DirectCodeBuddyAccount(702)
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, codeBuddyProbeValidSSE)
	p := newProbeServiceWithUpstream(t, upstream, NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil),
		&probeRepoMock{accounts: map[int64]*Account{account.ID: account}})

	ok, err := p.probeModelScopeUpstream(context.Background(), account, "model-scope")
	require.NoError(t, err)
	require.True(t, ok)
	rec := upstream.lastRecord(t)
	require.Equal(t, codeBuddyChatCompletionsPath, rec.Path)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Equal(t, "model-scope", body["model"])
	require.Equal(t, "Bearer DIRECT_TOKEN_SECRET", rec.Header.Get("Authorization"))
}

func TestModelScopeCursorAPIKeyProbeUsesRequestedScope(t *testing.T) {
	const scope = "mapped-scope"
	account := &Account{
		ID:       703,
		Platform: PlatformCursor,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "cursor-scope-key",
			"base_url": "https://cursor.example.test/v1",
			"model_mapping": map[string]any{
				"request-model": scope,
				scope:           "wrong-remapped-model",
			},
		},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
	p := newProbeServiceWithUpstream(t, upstream, NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil),
		&probeRepoMock{accounts: map[int64]*Account{account.ID: account}})

	ok, err := p.probeModelScopeUpstream(context.Background(), account, scope)
	require.NoError(t, err)
	require.True(t, ok)
	rec := upstream.lastRecord(t)
	require.Equal(t, "/v1/chat/completions", rec.Path)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Equal(t, scope, body["model"], "Cursor scope is already mapped and must not be remapped")
}

func TestModelScopeCursorConnectProbeUsesRequestedScope(t *testing.T) {
	const scope = "mapped-connect-scope"
	account := cursorProbeAccount(704, AccountTypeOAuth, "", map[string]string{
		"request-model": scope,
		scope:           "wrong-remapped-model",
	})
	p := &AccountHealthRecoveryProbeService{cursorExec: &CursorExecutionService{}}
	var probedModel string
	p.SetCursorAgentRunProbe(func(_ context.Context, _ *CursorExecutionService, _ *Account, model string, _ int) (int, string, string, error) {
		probedModel = model
		return http.StatusOK, "", "ok", nil
	})

	ok, err := p.probeModelScopeUpstream(context.Background(), account, scope)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, scope, probedModel, "Connect scope is already mapped and must not be remapped")
}

func TestModelScopeOpenAIImageProbeUsesDedicatedBalanceEndpoint(t *testing.T) {
	account := &Account{
		ID:       705,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "image-balance-key",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true,
				"url":     "https://balance.example.test/v1/usage",
			},
		},
	}
	repo := &modelScopeBalanceProbeRepo{account: account}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"remaining":12}`)
	p := &AccountHealthRecoveryProbeService{
		balanceProbe: NewAccountBalanceProbeService(repo, upstream),
	}

	ok, err := p.probeModelScopeUpstream(context.Background(), account, openAIImageGenerationRateLimitKey)
	require.NoError(t, err)
	require.True(t, ok)
	rec := upstream.lastRecord(t)
	require.Equal(t, http.MethodGet, rec.Method)
	require.Equal(t, "/v1/usage", rec.Path)
	require.Empty(t, rec.Body)
	require.Equal(t, "image-balance-key", rec.Header.Get("Authorization"))
}

func TestModelScopeOpenAIImageProbeKeepsExhaustedBalanceUnavailable(t *testing.T) {
	account := &Account{
		ID:       706,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "image-balance-key",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true,
				"url":     "https://balance.example.test/v1/usage",
			},
		},
	}
	repo := &modelScopeBalanceProbeRepo{account: account}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"remaining":0}`)
	p := &AccountHealthRecoveryProbeService{
		balanceProbe: NewAccountBalanceProbeService(repo, upstream),
	}

	ok, err := p.probeModelScopeUpstream(context.Background(), account, openAIImageGenerationRateLimitKey)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestModelScopeAICreditsUsesEnabledCreditBalance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1internal:loadCodeAssist", r.URL.Path)
		require.Equal(t, "Bearer antigravity-api-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"paidTier":{"id":"g1-pro-tier","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"12.5","minimumCreditAmountForUsage":"5"}]}}`))
	}))
	defer server.Close()

	originalBaseURLs := append([]string(nil), antigravity.BaseURLs...)
	antigravity.BaseURLs = []string{server.URL}
	t.Cleanup(func() { antigravity.BaseURLs = originalBaseURLs })

	account := &Account{
		ID:       708,
		Platform: PlatformAntigravity,
		Type:     AccountTypeUpstream,
		Credentials: map[string]any{
			"api_key": "antigravity-api-key",
		},
	}
	p := &AccountHealthRecoveryProbeService{
		antigravityQuota:  NewAntigravityQuotaFetcher(nil, &config.Config{}),
		antigravityTokens: NewAntigravityTokenProvider(nil, nil, nil),
	}

	ok, err := p.probeModelScopeUpstream(context.Background(), account, creditsExhaustedKey)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestUnownedModelProbeCandidateDropsOnlyItsSchedule(t *testing.T) {
	repo := &modelProbeSchedulePatchRecorder{probeRepoMock: &probeRepoMock{}}
	p := &AccountHealthRecoveryProbeService{accountRepo: repo}
	task := ModelProbeTask{
		AccountID: 707,
		Scope:     clinePassRateLimitKey,
		Entry:     map[string]any{"probe_gen": float64(23)},
	}

	p.dropModelProbeSchedule(context.Background(), task)

	require.Equal(t, task.AccountID, repo.accountID)
	require.Equal(t, task.Scope, repo.scope)
	require.Equal(t, int64(23), repo.expectedGen)
	require.Equal(t, ModelProbePatchDropSchedule, repo.patch.Op)
}
