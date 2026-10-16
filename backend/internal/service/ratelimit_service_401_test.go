//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type rateLimitAccountRepoStub struct {
	mockAccountRepoForGemini
	setErrorCalls          int
	tempCalls              int
	rateLimitedCalls       int
	updateCredentialsCalls int
	updateExtraCalls       int
	lastCredentials        map[string]any
	lastExtraUpdates       map[string]any
	lastErrorMsg           string
	lastTempReason         string
	lastTempUntil          time.Time
	lastErrorID            int64
	lastTempID             int64
	lastRateLimitedID      int64
	lastRateLimitedAt      time.Time
	tempErr                error
}

func (r *rateLimitAccountRepoStub) SetError(ctx context.Context, id int64, errorMsg string) error {
	r.setErrorCalls++
	r.lastErrorID = id
	r.lastErrorMsg = errorMsg
	return nil
}

func (r *rateLimitAccountRepoStub) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	r.tempCalls++
	r.lastTempID = id
	r.lastTempReason = reason
	r.lastTempUntil = until
	return r.tempErr
}

func (r *rateLimitAccountRepoStub) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	r.rateLimitedCalls++
	r.lastRateLimitedID = id
	r.lastRateLimitedAt = resetAt
	return nil
}

func (r *rateLimitAccountRepoStub) UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error {
	r.updateCredentialsCalls++
	r.lastCredentials = shallowCopyMap(credentials)
	return nil
}

func (r *rateLimitAccountRepoStub) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.updateExtraCalls++
	r.lastExtraUpdates = shallowCopyMap(updates)
	return nil
}

type tokenCacheInvalidatorRecorder struct {
	accounts []*Account
	err      error
}

type openAI403CounterCacheStub struct {
	counts         []int64
	resetCalls     []int64
	err            error
	incrementCalls int
}

func (s *openAI403CounterCacheStub) IncrementOpenAI403Count(_ context.Context, _ int64, _ int) (int64, error) {
	s.incrementCalls++
	if s.err != nil {
		return 0, s.err
	}
	if len(s.counts) == 0 {
		return 1, nil
	}
	count := s.counts[0]
	s.counts = s.counts[1:]
	return count, nil
}

func (s *openAI403CounterCacheStub) ResetOpenAI403Count(_ context.Context, accountID int64) error {
	s.resetCalls = append(s.resetCalls, accountID)
	return nil
}

func (r *tokenCacheInvalidatorRecorder) InvalidateToken(ctx context.Context, account *Account) error {
	r.accounts = append(r.accounts, account)
	return r.err
}

func TestRateLimitService_HandleUpstreamError_OAuth401SetsTempUnschedulable(t *testing.T) {
	t.Run("gemini", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		invalidator := &tokenCacheInvalidatorRecorder{}
		service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		service.SetTokenCacheInvalidator(invalidator)
		account := &Account{
			ID:       100,
			Platform: PlatformGemini,
			Type:     AccountTypeOAuth,
			Credentials: map[string]any{
				"refresh_token":              "rt-100",
				"temp_unschedulable_enabled": true,
				"temp_unschedulable_rules": []any{
					map[string]any{
						"error_code":       401,
						"keywords":         []any{"unauthorized"},
						"duration_minutes": 30,
						"description":      "custom rule",
					},
				},
			},
		}

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"))

		require.True(t, shouldDisable)
		require.Equal(t, 0, repo.setErrorCalls)
		require.Equal(t, 1, repo.tempCalls)
		require.Len(t, invalidator.accounts, 1)
	})

	t.Run("antigravity_401_sets_temp_unschedulable", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		invalidator := &tokenCacheInvalidatorRecorder{}
		service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		service.SetTokenCacheInvalidator(invalidator)
		account := &Account{
			ID:       100,
			Platform: PlatformAntigravity,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Credentials: map[string]any{
				"access_token":  "expired-at",
				"refresh_token": "rt-100",
			},
		}

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"))

		require.True(t, shouldDisable)
		require.Equal(t, 0, repo.setErrorCalls, "Antigravity OAuth 401 must keep status=active so refresh worker can recover it")
		require.Equal(t, 1, repo.tempCalls)
		require.Equal(t, int64(100), repo.lastTempID)
		require.Contains(t, repo.lastTempReason, "invalid or expired credentials")
		require.Equal(t, 1, repo.updateExtraCalls)
		require.Equal(t, true, repo.lastExtraUpdates[antigravityForceTokenRefreshExtraKey])
		require.Equal(t, "401_invalid", repo.lastExtraUpdates[antigravityForceTokenRefreshReasonExtraKey])
		require.Equal(t, true, account.Extra[antigravityForceTokenRefreshExtraKey])
		require.Len(t, invalidator.accounts, 1)
		require.Equal(t, int64(100), invalidator.accounts[0].ID)
	})
}

// TestRateLimitService_HandleUpstreamError_SparkShadow401RedirectsToParent 外审第9轮:影子无独立凭据,
// 401(母账号 token 问题)必须重定向到凭据 owner(母账号)——母账号 temp-unschedulable + token cache 失效,
// 影子不得被永久禁用(否则母账号可恢复的 token 问题会把影子永久打死)。
func TestRateLimitService_HandleUpstreamError_SparkShadow401RedirectsToParent(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	repo.accountsByID = map[int64]*Account{}
	invalidator := &tokenCacheInvalidatorRecorder{}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetTokenCacheInvalidator(invalidator)

	const parentID = int64(500)
	mother := &Account{
		ID:          parentID,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "rt-mother"},
	}
	repo.accountsByID[parentID] = mother

	shadowParent := parentID
	shadow := &Account{
		ID:              501,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &shadowParent,
		QuotaDimension:  QuotaDimensionSpark,
		// 影子不持凭据:GetCredential("refresh_token") == ""
	}

	shouldDisable := service.HandleUpstreamError(context.Background(), shadow, 401, http.Header{}, []byte("unauthorized"))

	require.True(t, shouldDisable)
	require.Equal(t, 0, repo.setErrorCalls, "spark shadow must not be permanently disabled on a parent-token 401")
	require.Equal(t, 1, repo.tempCalls)
	require.Equal(t, parentID, repo.lastTempID, "temp-unschedulable must target the credential owner (parent)")
	require.Len(t, invalidator.accounts, 1)
	require.Equal(t, parentID, invalidator.accounts[0].ID, "token cache invalidation must target the parent")
}

// TestRateLimitService_HandleUpstreamError_OAuth401InvalidatorError
// OpenAI OAuth 401 缓存失效出错时仍走 temp_unschedulable。
// 注意：401 handler 不再回写 credentials(避免请求开始时的快照整列覆盖 DB
// 把另一个 worker 刚刷新出来的新 refresh_token 回滚为旧值),
// 因此 updateCredentialsCalls 应当为 0。
func TestRateLimitService_HandleUpstreamError_OAuth401InvalidatorError(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	invalidator := &tokenCacheInvalidatorRecorder{err: errors.New("boom")}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetTokenCacheInvalidator(invalidator)
	account := &Account{
		ID:       101,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"refresh_token": "rt-101",
		},
	}

	shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"))

	require.True(t, shouldDisable)
	require.Equal(t, 0, repo.setErrorCalls)
	require.Equal(t, 1, repo.tempCalls)
	require.Equal(t, 0, repo.updateCredentialsCalls)
	require.Len(t, invalidator.accounts, 1)
}

func TestRateLimitService_HandleUpstreamError_NonOAuth401(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	invalidator := &tokenCacheInvalidatorRecorder{}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetTokenCacheInvalidator(invalidator)
	account := &Account{
		ID:       102,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
	}

	shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"))

	require.True(t, shouldDisable)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Empty(t, invalidator.accounts)
}

func TestRateLimitService_HandleUpstreamError_NonOAuthModel401StillDisablesCredentials(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       104,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
	}

	shouldDisable := service.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusUnauthorized,
		http.Header{},
		[]byte(`{"error":{"message":"invalid or expired credentials"}}`),
		"definitely-not-real",
	)

	require.True(t, shouldDisable)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Equal(t, 0, repo.tempCalls)
}

// TestRateLimitService_HandleUpstreamError_OAuth401DoesNotOverwriteCredentials
// 回归测试:确保 401 handler 不再使用请求开始时的 account 快照写回 credentials。
// 原实现会通过 persistAccountCredentials → UpdateCredentials → SetCredentials
// 整列覆盖 credentials JSONB,在另一个 worker 刚刷新完 refresh_token 的窄窗口内
// 会把新 refresh_token 回滚为快照中的旧值,导致下一周期拿 invalid_grant 被错误 disable。
func TestRateLimitService_HandleUpstreamError_OAuth401DoesNotOverwriteCredentials(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       103,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "token",
			"refresh_token": "rt-103",
		},
	}

	shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"))

	require.True(t, shouldDisable)
	require.Equal(t, 0, repo.updateCredentialsCalls, "401 handler must not write credentials back from the request-start snapshot")
	require.Equal(t, 0, repo.updateExtraCalls, "OpenAI 401 must not set Antigravity force-refresh marker")
	require.Equal(t, 1, repo.tempCalls, "401 handler should still set temp-unschedulable cooldown")
	require.Nil(t, repo.lastCredentials, "no credentials should have been persisted")
}

// 缺少 refresh_token 的 OAuth 账号 401 应直接 SetError 永久禁用，
// 不再走 10 分钟冷却（冷却期内无人能刷新它，结束后还会被选中再 502 一次）。
func TestRateLimitService_HandleUpstreamError_OAuth401NoRefreshTokenSetsError(t *testing.T) {
	t.Run("openai_no_refresh_token", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		invalidator := &tokenCacheInvalidatorRecorder{}
		service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		service.SetTokenCacheInvalidator(invalidator)
		account := &Account{
			ID:       2881,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Credentials: map[string]any{
				"access_token": "expired-at",
				// no refresh_token
			},
		}

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"))

		require.True(t, shouldDisable)
		require.Equal(t, 1, repo.setErrorCalls, "AT-only OAuth 401 must SetError")
		require.Equal(t, 0, repo.tempCalls, "AT-only OAuth 401 must NOT temp-unschedule")
		require.Equal(t, 0, repo.updateCredentialsCalls, "no point forcing expires_at when refresh is impossible")
		require.Contains(t, repo.lastErrorMsg, "refresh_token missing")
		require.Len(t, invalidator.accounts, 1, "cache should still be invalidated")
	})

	t.Run("openai_blank_refresh_token_treated_as_missing", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		account := &Account{
			ID:       2882,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Credentials: map[string]any{
				"access_token":  "expired-at",
				"refresh_token": "   ",
			},
		}

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"))

		require.True(t, shouldDisable)
		require.Equal(t, 1, repo.setErrorCalls)
		require.Equal(t, 0, repo.tempCalls)
	})

	t.Run("antigravity_no_refresh_token_sets_error", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		invalidator := &tokenCacheInvalidatorRecorder{}
		service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		service.SetTokenCacheInvalidator(invalidator)
		account := &Account{
			ID:       2883,
			Platform: PlatformAntigravity,
			Type:     AccountTypeOAuth,
			Credentials: map[string]any{
				"access_token": "expired-at",
			},
		}

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"))

		require.True(t, shouldDisable)
		require.Equal(t, 1, repo.setErrorCalls, "Antigravity OAuth without refresh_token cannot self-recover")
		require.Equal(t, 0, repo.tempCalls)
		require.Contains(t, repo.lastErrorMsg, "refresh_token missing")
		require.Len(t, invalidator.accounts, 1)
	})
}

// TestRateLimitService_HandleUpstreamError_OAuth401EmptyModelKeyFallsBackToCooldown
// AC-G2R6 must_fix 第3条（主功能漏洞）：无 requestedModel 的 OAuth 401 不得进入失败连击阶梯
// （阶梯无法定位模型维度、且会对整账号无差别连击误伤），必须直接走既有 OAuth401CooldownMinutes
// 冷却（失败关闭，同 modelTransient==nil 的回退分支），并记 warn 日志一行。本测试断言该空键路径：
// 不进阶梯、一次即 10 分钟冷却 + shouldDisable，reason 为明文（绝不含连击阶梯的
// oauth_401_recoverable 标记）。
func TestRateLimitService_HandleUpstreamError_OAuth401EmptyModelKeyFallsBackToCooldown(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	invalidator := &tokenCacheInvalidatorRecorder{}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetTokenCacheInvalidator(invalidator)
	account := &Account{
		ID:       2001,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"refresh_token": "rt-2001",
		},
	}

	// 关键：不传入 requestedModel（模拟定时刷新/保活触发或上游在非请求上下文返回 401）。
	shouldDisable := service.HandleUpstreamError(
		context.Background(),
		account,
		401,
		http.Header{},
		[]byte(`{"error":{"message":"Invalid token"}}`),
	)

	require.True(t, shouldDisable, "空键 401 必须失败关闭（shouldDisable）")
	require.Equal(t, 1, repo.tempCalls, "空模型键必须走冷却分支并置 temp-unschedulable")
	require.Equal(t, int64(2001), repo.lastTempID)
	// 连击阶梯未注入（modelTransient==nil）→ 与空键分支同款回退：OAuth401CooldownMinutes
	// 未配置 → 默认 10 分钟冷却。
	require.False(t, repo.lastTempUntil.IsZero(), "必须写入 temp-unschedulable 截止时间")
	elapsed := repo.lastTempUntil.Sub(time.Now())
	require.GreaterOrEqual(t, elapsed, 9*time.Minute, "空键冷却不得低于 9 分钟（默认 10 分钟）")
	require.LessOrEqual(t, elapsed, 11*time.Minute, "空键冷却不得高于 11 分钟（默认 10 分钟）")
	// 冷却分支 reason 为明文 msg，绝不应是连击阶梯写入的 TempUnschedState JSON
	//（含 oauth_401_recoverable 标记），据此确认未进入阶梯。
	require.NotContains(t, repo.lastTempReason, oauth401RecoverableMarker,
		"空模型键不得进入失败连击阶梯（不得写入 oauth_401_recoverable 标记）")
	require.Len(t, invalidator.accounts, 1, "token 缓存仍需失效以便后续刷新")
}
