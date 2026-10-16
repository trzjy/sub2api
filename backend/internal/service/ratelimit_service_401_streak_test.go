//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oauth401StreakRepoStub 是 OAuth 401 连击阶梯测试用的 AccountRepository 桩，
// 在 mockAccountRepoForGemini 基础上捕获 SetError / SetTempUnschedulable / UpdateExtra 调用。
type oauth401StreakRepoStub struct {
	mockAccountRepoForGemini
	setErrorCalls    int
	tempCalls        int
	lastTempID       int64
	lastTempUntil    time.Time
	lastTempReason   string
	lastErrorID      int64
	lastErrorMsg     string
	updateExtraCalls int
	tempErr          error
}

func (r *oauth401StreakRepoStub) SetError(_ context.Context, id int64, errorMsg string) error {
	r.setErrorCalls++
	r.lastErrorID = id
	r.lastErrorMsg = errorMsg
	return nil
}

func (r *oauth401StreakRepoStub) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.tempCalls++
	r.lastTempID = id
	r.lastTempUntil = until
	r.lastTempReason = reason
	return r.tempErr
}

func (r *oauth401StreakRepoStub) SetRateLimited(_ context.Context, _ int64, _ time.Time) error {
	return nil
}

func (r *oauth401StreakRepoStub) UpdateCredentials(_ context.Context, _ int64, _ map[string]any) error {
	return nil
}

func (r *oauth401StreakRepoStub) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	r.updateExtraCalls++
	return nil
}

func (r *oauth401StreakRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if acc, ok := r.accountsByID[id]; ok {
		return acc, nil
	}
	return nil, nil
}

// newOAuth401StreakService 构造注入了共享连击状态机的 RateLimitService（模拟生产 wiring）。
func newOAuth401StreakService(t *testing.T) (*RateLimitService, *oauth401StreakRepoStub, *tokenCacheInvalidatorRecorder) {
	t.Helper()
	repo := &oauth401StreakRepoStub{accountsByID: map[int64]*Account{}}
	invalidator := &tokenCacheInvalidatorRecorder{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetTokenCacheInvalidator(invalidator)
	svc.SetModelTransientState(newOpenAIAccountModelTransientState(openAIModelTransientDefaultMax))
	return svc, repo, invalidator
}

func oauth401Account(id int64, platform string) *Account {
	return &Account{
		ID:          id,
		Platform:    platform,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "rt"},
	}
}

// 第 1 次 OAuth 401：不置冷却、shouldDisable=false、token 缓存被失效。
func TestRateLimitService_HandleUpstreamError_OAuth401_Streak1NoCooldown(t *testing.T) {
	svc, repo, invalidator := newOAuth401StreakService(t)
	account := oauth401Account(100, PlatformOpenAI)

	sd := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o")

	require.False(t, sd, "streak 1 must not disable the account")
	require.Equal(t, 0, repo.tempCalls, "streak 1 sets no temp-unschedulable cooldown")
	require.Equal(t, 0, repo.setErrorCalls, "streak 1 must not SetError")
	require.Len(t, invalidator.accounts, 1, "token cache must still be invalidated on every 401")
	require.Equal(t, int64(100), invalidator.accounts[0].ID)
}

// 第 2 次：SetTempUnschedulable 到 now+10s、reason 为可解析 JSON、
// MatchedKeyword == oauth_401_recoverable。
func TestRateLimitService_HandleUpstreamError_OAuth401_Streak2TenSecondCooldown(t *testing.T) {
	svc, repo, _ := newOAuth401StreakService(t)
	account := oauth401Account(100, PlatformOpenAI)

	require.False(t, svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o"))
	start := time.Now()
	sd := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o")

	require.True(t, sd)
	require.Equal(t, 1, repo.tempCalls)
	require.Equal(t, int64(100), repo.lastTempID)
	st := tempUnschedStateFromStoredReason(repo.lastTempReason, 0)
	require.Equal(t, oauth401RecoverableMarker, st.MatchedKeyword, "reason must carry the fixed recoverable marker")
	require.Equal(t, int64(401), int64(st.StatusCode))
	require.Contains(t, st.ErrorMessage, "401", "ErrorMessage must retain the original upstream message (troubleshooting info)")
	d := st.UntilUnix - start.Unix()
	require.GreaterOrEqual(t, d, int64(8))
	require.LessOrEqual(t, d, int64(14), "streak 2 cooldown must be ~10s")
}

// 第 3 次：冷却 30s。
func TestRateLimitService_HandleUpstreamError_OAuth401_Streak3ThirtySecondCooldown(t *testing.T) {
	svc, repo, _ := newOAuth401StreakService(t)
	account := oauth401Account(100, PlatformOpenAI)

	require.False(t, svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o"))
	require.True(t, svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o"))
	start := time.Now()
	sd := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o")

	require.True(t, sd)
	require.Equal(t, 2, repo.tempCalls)
	st := tempUnschedStateFromStoredReason(repo.lastTempReason, 0)
	require.Equal(t, oauth401RecoverableMarker, st.MatchedKeyword)
	d := st.UntilUnix - start.Unix()
	require.GreaterOrEqual(t, d, int64(28))
	require.LessOrEqual(t, d, int64(33), "streak 3 cooldown must be ~30s (user-decided, was 45s)")
}

// 成功一次后 streak 清零（recordSuccess 路径）。
func TestRateLimitService_HandleUpstreamError_OAuth401_SuccessClearsStreak(t *testing.T) {
	svc, repo, _ := newOAuth401StreakService(t)
	account := oauth401Account(100, PlatformOpenAI)

	require.False(t, svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o"))
	require.True(t, svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o"))

	// 成功清零连击
	svc.modelTransient.recordSuccess(100, "gpt-4o")

	// 下一次 401 从 streak 1 重新计数：不置冷却、不禁用。
	sd := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o")
	require.False(t, sd, "after a success the streak must restart at 1 (no cooldown)")
	require.Equal(t, 1, repo.tempCalls, "no new cooldown after the streak reset")
}

// r4 阻塞2 验收点：401 → 成功（经 gateway 成功出口）→ 401 从第 1 次重新计数。
func TestRateLimitService_HandleUpstreamError_OAuth401_ReCountAfterSuccess(t *testing.T) {
	repo := &oauth401StreakRepoStub{accountsByID: map[int64]*Account{}}
	transient := newOpenAIAccountModelTransientState(openAIModelTransientDefaultMax)
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetModelTransientState(transient)
	gateway := &OpenAIGatewayService{accountRepo: repo, openaiModelTransient: transient}
	account := oauth401Account(100, PlatformOpenAI)

	require.False(t, svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o"))
	require.True(t, svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o"))
	require.True(t, transient.isBlocked(100, "gpt-4o", time.Now()), "streak 2 must block the account")

	// 经生产成功出口（会过 resolveCredentialAccount 对称清键）。
	gateway.ReportOpenAIAccountScheduleResult(account, "gpt-4o", true, nil)
	require.False(t, transient.isBlocked(100, "gpt-4o", time.Now()), "success path must clear the streak")

	// 下一次 401 从 streak 1 重新计数。
	sd := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o")
	require.False(t, sd, "after success a fresh 401 restarts at streak 1")
	require.Equal(t, 1, repo.tempCalls, "no new cooldown after restart")
}

// 四个既有早退分支行为不变（token_invalidated / detail:Unauthorized / 无 refresh_token）。
func TestRateLimitService_HandleUpstreamError_OAuth401_EarlyExitsUnchanged(t *testing.T) {
	t.Run("token_invalidated_permanent", func(t *testing.T) {
		svc, repo, _ := newOAuth401StreakService(t)
		account := oauth401Account(100, PlatformOpenAI)
		body := []byte(`{"error":{"code":"token_invalidated"}}`)

		sd := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, body, "gpt-4o")

		require.True(t, sd)
		require.Equal(t, 1, repo.setErrorCalls, "token_invalidated must SetError permanently")
		require.Equal(t, 0, repo.tempCalls, "no temp-unschedulable for a permanent revoke")
	})

	t.Run("detail_unauthorized_permanent", func(t *testing.T) {
		svc, repo, _ := newOAuth401StreakService(t)
		account := oauth401Account(100, PlatformOpenAI)
		body := []byte(`{"detail":"Unauthorized"}`)

		sd := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, body, "gpt-4o")

		require.True(t, sd)
		require.Equal(t, 1, repo.setErrorCalls)
		require.Equal(t, 0, repo.tempCalls)
	})

	t.Run("no_refresh_token_permanent_no_ladder", func(t *testing.T) {
		svc, repo, _ := newOAuth401StreakService(t)
		account := &Account{
			ID:          100,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{"access_token": "expired-at"}, // 无 refresh_token
		}

		sd := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o")

		require.True(t, sd)
		require.Equal(t, 1, repo.setErrorCalls, "AT-only OAuth 401 must SetError, not enter the ladder")
		require.Equal(t, 0, repo.tempCalls, "AT-only OAuth 401 must NOT temp-unschedule")
	})
}

// 连击窗口 30 分钟过期后 streak 重置。
func TestRateLimitService_HandleUpstreamError_OAuth401_StreakWindowExpiryResets(t *testing.T) {
	svc, repo, _ := newOAuth401StreakService(t)
	st := svc.modelTransient

	st.recordFailure(100, "gpt-4o", time.Now())
	st.recordFailure(100, "gpt-4o", time.Now().Add(time.Second)) // streak 2 -> blocked
	require.True(t, st.isBlocked(100, "gpt-4o", time.Now().Add(2*time.Second)))

	// 超过 30 分钟 TTL 后的又一次失败必须重置为 streak 1。
	decision := st.recordFailure(100, "gpt-4o", time.Now().Add(openAIModelTransientStreakTTL+time.Minute))
	require.Equal(t, 1, decision.FailureStreak, "streak must reset after the TTL window")
	require.Zero(t, decision.Cooldown)
	_ = repo
}

// modelTransient 为 nil（未注入）时走旧 10 分钟冷却分支（wiring 回退语义）。
func TestRateLimitService_HandleUpstreamError_OAuth401_NilModelTransientFallback(t *testing.T) {
	repo := &oauth401StreakRepoStub{accountsByID: map[int64]*Account{}}
	invalidator := &tokenCacheInvalidatorRecorder{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetTokenCacheInvalidator(invalidator)
	// 故意不调用 SetModelTransientState -> modelTransient == nil

	account := oauth401Account(100, PlatformOpenAI)
	sd := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o")

	require.True(t, sd, "nil modelTransient must keep the old 10-minute disable behavior")
	require.Equal(t, 1, repo.tempCalls, "fallback still temp-unschedules on the first 401")
	require.Len(t, invalidator.accounts, 1)
}

// Antigravity OAuth 401 在第 2 连击置冷却的同时仍打 force-refresh 标记。
func TestRateLimitService_HandleUpstreamError_OAuth401AntigravityStreak2(t *testing.T) {
	svc, repo, _ := newOAuth401StreakService(t)
	account := &Account{
		ID:          100,
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Credentials: map[string]any{"access_token": "at", "refresh_token": "rt"},
	}

	require.False(t, svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o"))
	sd := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"), "gpt-4o")

	require.True(t, sd)
	require.Equal(t, 1, repo.tempCalls)
	require.Equal(t, 2, repo.updateExtraCalls, "antigravity force-refresh marker is applied on each 401 (idempotent)")
	assert.Equal(t, true, account.Extra[antigravityForceTokenRefreshExtraKey])
}

// r6 阻塞2 验收点：影子账号定向用例。
// 影子 401（记母账号键）→ 影子成功（经 gateway 成功出口）→ 母账号键连击归零。
func TestOpenAIGatewayService_Shadow401ThenShadowSuccessClearsParentStreak(t *testing.T) {
	repo := &oauth401StreakRepoStub{accountsByID: map[int64]*Account{}}
	transient := newOpenAIAccountModelTransientState(openAIModelTransientDefaultMax)
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetModelTransientState(transient)

	const parentID int64 = 500
	mother := &Account{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "rt-mother"}}
	repo.accountsByID[parentID] = mother

	shadowParent := parentID
	shadow := &Account{
		ID:              501,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &shadowParent,
	}

	// 影子连续 2 次 401：连击记在母账号键（500）上，第 2 次进入 10s 冷却。
	require.False(t, svc.HandleUpstreamError(context.Background(), shadow, 401, http.Header{}, []byte("unauthorized"), "gpt-4o"))
	require.True(t, svc.HandleUpstreamError(context.Background(), shadow, 401, http.Header{}, []byte("unauthorized"), "gpt-4o"))
	require.True(t, transient.isBlocked(parentID, "gpt-4o", time.Now()), "shadow 401 must block on the parent key")

	// 影子成功：对称清键必须清掉母账号键（500），否则连击永不清零。
	gateway := &OpenAIGatewayService{accountRepo: repo, openaiModelTransient: transient}
	gateway.ReportOpenAIAccountScheduleResult(shadow, "gpt-4o", true, nil)
	require.False(t, transient.isBlocked(parentID, "gpt-4o", time.Now()), "shadow success must clear the parent streak (resolveCredentialAccount symmetry)")

	// 下一次影子 401 从 streak 1 重新计数（母账号键已清零）。
	sd := svc.HandleUpstreamError(context.Background(), shadow, 401, http.Header{}, []byte("unauthorized"), "gpt-4o")
	require.False(t, sd, "after shadow success a fresh shadow 401 restarts at streak 1")
	require.Equal(t, 1, repo.tempCalls, "no new cooldown after the parent streak reset")
}
