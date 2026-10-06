//go:build unit

package service

import (
	"context"
	"net/http"
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
	// precise 为持久化到条目的 precise_reset 标记；nil 表示该次写入未携带该标记
	// （既有 SetModelRateLimit 路径）。
	precise *bool
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

// SetModelRateLimitWithPreciseReset 是精确恢复信号标记的持久化写入口（D4c）。
func (r *tokenHarborAccountRepoStub) SetModelRateLimitWithPreciseReset(_ context.Context, id int64, scope string, resetAt time.Time, preciseReset bool, reason string) error {
	call := tokenHarborRateLimitCall{accountID: id, scope: scope, resetAt: resetAt, reason: reason}
	precise := preciseReset
	call.precise = &precise
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

func TestTokenHarborFreeTierResetAt_PrefersBodyPeriodStart(t *testing.T) {
	now := time.Now()
	headers := http.Header{"Retry-After": []string{"120"}}

	// 正文周期起点优先于 Retry-After 头（L4 恢复时间来源优先级）。
	resetAt, ok := tokenHarborFreeTierResetAt(headers, []byte(tokenHarborFreeExhaustedBodyAt(now.Add(48*time.Hour))), now)
	require.True(t, ok)
	require.WithinDuration(t, now.Add(48*time.Hour).Truncate(time.Minute), resetAt, time.Minute)

	// 正文缺失时回退 Retry-After 头。
	resetAt, ok = tokenHarborFreeTierResetAt(headers, []byte(`{"error":{"type":"free_tier_limit_reached"}}`), now)
	require.True(t, ok)
	require.WithinDuration(t, now.Add(120*time.Second), resetAt, 5*time.Second)

	// 均无信号时不得伪造恢复倒数。
	_, ok = tokenHarborFreeTierResetAt(http.Header{}, []byte(`{"error":{"type":"free_tier_limit_reached"}}`), now)
	require.False(t, ok)
}

// D-QL-003A 回归守卫：响应式免费层耗尽处理链已删除。TH 账号收到免费层耗尽类
// 响应（402/429）不得再触发响应式停调——不写整号限流、不写临时停调、不写模型级
// 限流；额度处置统一移交 CN 额度生命周期状态机（cn_quota_lifecycle_service.go
// 的确认探针链）。
func TestRateLimitService_HandleUpstreamError_TokenHarborQuotaExhaustedNoReactivePark(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
	}{
		{name: "402_free_tier_exhausted", statusCode: http.StatusPaymentRequired},
		{name: "429_free_tier_exhausted", statusCode: http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &tokenHarborAccountRepoStub{}
			svc := &RateLimitService{accountRepo: repo}
			account := tokenHarborDeepSeekAccount()

			_ = svc.HandleUpstreamError(
				context.Background(),
				account,
				tc.statusCode,
				http.Header{},
				[]byte(tokenHarborFreeExhaustedBody),
				"deepseek-v4.1-flash:free",
			)

			// 响应式停调链已退役：零停调、零限流写入。
			require.Zero(t, repo.tempCalls, "不得写临时停调（响应式链已删除）")
			require.Zero(t, repo.rateLimitedCalls, "不得写整号限流（响应式链已删除）")
			require.Empty(t, repo.modelRateLimitCalls, "不得写模型级限流（响应式链已删除）")
		})
	}
}
