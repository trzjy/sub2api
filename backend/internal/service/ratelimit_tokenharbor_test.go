//go:build unit

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 生产报错可见片段（截图逐字摘录，截断处不断言）与官方文档措辞仅用于识别，
// 绝不用于拼凑恢复时间。

type tokenHarborRateLimitCall struct {
	accountID int64
	scope     string
	resetAt   time.Time
	reason    string
}

type tokenHarborAccountRepoStub struct {
	mockAccountRepoForGemini
	rateLimitedCalls    int
	tempCalls           int
	modelRateLimitCalls []tokenHarborRateLimitCall
}

func (r *tokenHarborAccountRepoStub) SetRateLimited(_ context.Context, _ int64, _ time.Time) error {
	r.rateLimitedCalls++
	return nil
}

func (r *tokenHarborAccountRepoStub) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, _ string) error {
	r.tempCalls++
	return nil
}

func (r *tokenHarborAccountRepoStub) SetModelRateLimit(_ context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	call := tokenHarborRateLimitCall{accountID: id, scope: scope, resetAt: resetAt}
	if len(reason) > 0 {
		call.reason = reason[0]
	}
	r.modelRateLimitCalls = append(r.modelRateLimitCalls, call)
	return nil
}

func tokenHarborDeepSeekAccount() *Account {
	return &Account{
		ID:          146,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"base_url": "https://tokenharbor.ai/v1",
		},
	}
}

// 用户生产报错全文（逐字摘录）。
const tokenHarborFreeExhaustedBody = `{"error":{"message":"You've used this period's free allowance. Your next rolling 7-day period starts on 7 Oct 2026 at 14:44 UTC. Use the paid model 'deepseek-v4.1-flash' to keep going, or subscribe to a Token Harbor Pass for a recurring included allowance across more models. https://tokenharbor.ai/pricing","type":"free_tier_limit_reached","code":"free_tier_limit_reached"}}`

// tokenHarborFreeExhaustedBodyAt 按实测格式生成指定周期起点的正文（仅日期
// 取值不同，用于不断言过期点位的测试）。
func tokenHarborFreeExhaustedBodyAt(start time.Time) string {
	return `{"error":{"message":"You've used this period's free allowance. Your next rolling 7-day period starts on ` +
		start.UTC().Format("2 Jan 2006 at 15:04 UTC") +
		`. Use the paid model 'deepseek-v4.1-flash' to keep going, or subscribe to a Token Harbor Pass for a recurring included allowance across more models. https://tokenharbor.ai/pricing","type":"free_tier_limit_reached","code":"free_tier_limit_reached"}}`
}

func TestIsTokenHarborFreeTierExhausted(t *testing.T) {
	positives := map[string]string{
		"production": tokenHarborFreeExhaustedBody,
		"docs":       "Your free-tier allowance is exhausted for this rolling period",
		"type_only":  `{"error":{"type":"free_tier_limit_reached","code":"free_tier_limit_reached"}}`,
	}
	for name, body := range positives {
		require.True(t, isTokenHarborFreeTierExhausted([]byte(body)), name)
	}
	negatives := map[string]string{
		"empty":               "",
		"plain_busy":          `{"error":{"message":"too many requests, please retry later"}}`,
		"balance":             `{"error":{"message":"insufficient balance, please top up"}}`,
		"free_word_only":      `{"error":{"message":"feel free to retry"}}`,
		"allowance_word_only": `{"error":{"message":"allowance check passed"}}`,
		"bare_mention":        `{"error":{"message":"free_tier models list"}}`,
	}
	for name, body := range negatives {
		require.False(t, isTokenHarborFreeTierExhausted([]byte(body)), name)
	}
}

func TestParseTokenHarborPeriodStart(t *testing.T) {
	now := time.Now()
	resetAt, ok := parseTokenHarborPeriodStart([]byte(tokenHarborFreeExhaustedBodyAt(now.Add(48*time.Hour))), now)
	require.True(t, ok)
	require.WithinDuration(t, now.Add(48*time.Hour).Truncate(time.Minute), resetAt, time.Minute)

	// 过去时间不得采用（宁可复探，不写过期倒数）。
	_, ok = parseTokenHarborPeriodStart([]byte(tokenHarborFreeExhaustedBodyAt(now.Add(-time.Hour))), now)
	require.False(t, ok)

	// 无起点正文不得采用。
	_, ok = parseTokenHarborPeriodStart([]byte(`{"error":{"message":"too many requests"}}`), now)
	require.False(t, ok)
}

func TestRateLimitService_HandleUpstreamError_TokenHarborFreeTierUsesModelScope(t *testing.T) {
	repo := &tokenHarborAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := tokenHarborDeepSeekAccount()

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(tokenHarborFreeExhaustedBodyAt(time.Now().Add(48 * time.Hour))),
		"deepseek-v4.1-flash:free",
	)

	require.False(t, handled, "账号不得被停调：付费直连路由仍可用")
	require.Zero(t, repo.rateLimitedCalls, "不得写整号限流")
	require.Zero(t, repo.tempCalls, "不得写临时停调")
	require.Len(t, repo.modelRateLimitCalls, 1)
	call := repo.modelRateLimitCalls[0]
	require.Equal(t, account.ID, call.accountID)
	require.Equal(t, "deepseek-v4.1-flash:free", call.scope)
	require.True(t, strings.HasPrefix(call.reason, tokenHarborFreeTierReasonPrefix))
	require.NotContains(t, call.reason, "precise reset unknown")
	require.WithinDuration(t, time.Now().Add(48*time.Hour).Truncate(time.Minute), call.resetAt, 2*time.Minute)
}

func TestRateLimitService_HandleUpstreamError_TokenHarborFreeTierHonorsRetryAfter(t *testing.T) {
	repo := &tokenHarborAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := tokenHarborDeepSeekAccount()

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{"Retry-After": []string{"120"}},
		[]byte(`{"error":{"message":"free-tier allowance exhausted for this rolling period","type":"free_tier_limit_reached"}}`),
		"deepseek-v4.1-flash:free",
	)

	require.False(t, handled)
	require.Zero(t, repo.rateLimitedCalls)
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
	call := repo.modelRateLimitCalls[0]
	require.NotContains(t, call.reason, "precise reset unknown")
	require.WithinDuration(t, time.Now().Add(120*time.Second), call.resetAt, 5*time.Second)
}

func TestRateLimitService_HandleUpstreamError_TokenHarborFreeTierWithoutSignalProbes(t *testing.T) {
	repo := &tokenHarborAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := tokenHarborDeepSeekAccount()

	// 仅类型码、无时间信号：不伪造倒数，复探间隔等上游成功证明恢复。
	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"type":"free_tier_limit_reached","code":"free_tier_limit_reached"}}`),
		"deepseek-v4.1-flash:free",
	)

	require.False(t, handled)
	require.Zero(t, repo.rateLimitedCalls)
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
	call := repo.modelRateLimitCalls[0]
	require.Contains(t, call.reason, "precise reset unknown")
	require.WithinDuration(t, time.Now().Add(tokenHarborFreeTierProbeCooldown), call.resetAt, 5*time.Second)
}

func TestRateLimitService_HandleUpstreamError_TokenHarborFreeTierExtractsPaidModel(t *testing.T) {
	repo := &tokenHarborAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := tokenHarborDeepSeekAccount()

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(tokenHarborFreeExhaustedBody),
	)

	require.False(t, handled)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "deepseek-v4.1-flash", repo.modelRateLimitCalls[0].scope)
}

func TestRateLimitService_HandleUpstreamError_TokenHarborFreeTierWithoutModelSkipsAllWrites(t *testing.T) {
	repo := &tokenHarborAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := tokenHarborDeepSeekAccount()

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"message":"free-tier allowance exhausted for this rolling period","type":"free_tier_limit"}}`),
	)

	// 无模型名不猜，也不扩大为整号限流：零写入，本次换号。
	require.False(t, handled)
	require.Empty(t, repo.modelRateLimitCalls)
	require.Zero(t, repo.rateLimitedCalls)
	require.Zero(t, repo.tempCalls)
}

func TestRateLimitService_HandleUpstreamError_NonTokenHarborUpstreamFallsThrough(t *testing.T) {
	repo := &tokenHarborAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := tokenHarborDeepSeekAccount()
	account.Credentials = map[string]any{
		"base_url": "https://api.deepseek.com/v1",
	}

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(tokenHarborFreeExhaustedBody),
		"deepseek-chat",
	)

	// 非 TokenHarbor 上游不得套用：不写模型级，走既有链。
	require.False(t, handled)
	require.Empty(t, repo.modelRateLimitCalls)
}
