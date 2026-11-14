package service

import (
	"context"
	"testing"
	"time"
)

// TestStickyBindingDroppedOnMissClassification 锁定「哪条原因销毁绑定」的唯一判定。
// 这条判定的价值在于：临时原因一旦被误判成永久，整个会话就被永久迁移到新账号，
// 缓存全废且回不去；反过来永久原因被误判成临时，会话会反复重试一个死号。
func TestStickyBindingDroppedOnMissClassification(t *testing.T) {
	t.Setenv("STICKY_KEEP_BINDING_ON_TRANSIENT", "")
	resetStickyKeepBindingCacheForTest()

	permanent := []stickyBindingMissReason{
		stickyMissAccountGone,
		stickyMissWebAccessMode,
		stickyMissIllegalAccessMode,
		stickyMissPlatformMismatch,
		stickyMissGroupMismatch,
		stickyMissModelUnsupported,
		stickyMissTransportUnsupported,
	}
	transient := []stickyBindingMissReason{
		stickyMissRateLimited,
		stickyMissUnschedulable,
		stickyMissConcurrencyFull,
		stickyMissPriorityTier,
		stickyMissRPMRed,
		stickyMissFreeQuotaOver,
		stickyMissTeamRateLimited,
		stickyMissModelQuotaBlocked,
		stickyMissStickyEscape,
		stickyMissChannelRestricted,
		stickyMissSizeScopeExcluded,
		stickyMissRuntimeBlocked,
		stickyMissShadowParentUnhealthy,
		stickyMissUnknown,
	}

	for _, reason := range permanent {
		if !stickyBindingDroppedOnMiss(reason) {
			t.Fatalf("永久原因 %q 必须销毁绑定", reason)
		}
	}
	for _, reason := range transient {
		if stickyBindingDroppedOnMiss(reason) {
			t.Fatalf("临时原因 %q 必须保留绑定", reason)
		}
	}
}

// TestStickyBindingRollbackSwitchRestoresOldBehavior 回滚开关必须能恢复旧行为。
func TestStickyBindingRollbackSwitchRestoresOldBehavior(t *testing.T) {
	t.Setenv("STICKY_KEEP_BINDING_ON_TRANSIENT", "0")
	resetStickyKeepBindingCacheForTest()
	t.Cleanup(func() {
		resetStickyKeepBindingCacheForTest()
	})

	if !stickyBindingDroppedOnMiss(stickyMissRateLimited) {
		t.Fatal("回滚开关开启时，临时原因也应销毁绑定（旧行为）")
	}
}

// TestReleaseStickyBindingOnMiss 验证实际副作用：临时原因不动 Redis 绑定，永久原因才删。
func TestReleaseStickyBindingOnMiss(t *testing.T) {
	t.Setenv("STICKY_KEEP_BINDING_ON_TRANSIENT", "")
	resetStickyKeepBindingCacheForTest()

	groupID := int64(115)
	key := openaiSessionCacheKeyForTest("h-transient")
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{key: 982}}
	svc := &OpenAIGatewayService{cache: cache}
	ctx := context.Background()

	// 限流（临时）：绑定必须留在原地，下一轮才能回到 982。
	svc.releaseStickyBindingOnMiss(ctx, &groupID, "h-transient", stickyMissRateLimited)
	if got, ok := cache.sessionBindings[key]; !ok || got != 982 {
		t.Fatalf("临时原因不应删除绑定，got=%d ok=%v", got, ok)
	}

	// 账号被删（永久）：绑定必须清掉，否则会话永远卡在死号上。
	svc.releaseStickyBindingOnMiss(ctx, &groupID, "h-transient", stickyMissAccountGone)
	if _, ok := cache.sessionBindings[key]; ok {
		t.Fatal("永久原因必须删除绑定")
	}
}

// TestStickyBindingMustSurviveFallback 绑定账号仍存在时禁止改写绑定。
func TestStickyBindingMustSurviveFallback(t *testing.T) {
	t.Setenv("STICKY_KEEP_BINDING_ON_TRANSIENT", "")
	resetStickyKeepBindingCacheForTest()

	groupID := int64(115)
	key := openaiSessionCacheKeyForTest("h-keep")
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{key: 442}}
	svc := &OpenAIGatewayService{
		cache:       cache,
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{{ID: 442, Priority: 1}}},
	}
	ctx := context.Background()

	if !svc.stickyBindingMustSurviveFallback(ctx, &groupID, "h-keep") {
		t.Fatal("绑定账号仍在，本次回退不得改写绑定")
	}
	if svc.stickyBindingMustSurviveFallback(ctx, &groupID, "h-none") {
		t.Fatal("没有绑定时不应阻止写入新绑定")
	}
}

// openaiSessionCacheKeyForTest 复算缓存键，避免测试里手写前缀漂移。
func openaiSessionCacheKeyForTest(sessionHash string) string {
	svc := &OpenAIGatewayService{}
	return svc.openAISessionCacheKey(sessionHash)
}

// TestLoadAwarenessTransientRateLimitKeepsBinding 是本次修复的核心行为断言：
// 原号临时限流 → 本次借道到别的号，但绑定必须仍然指向原号，下一轮原号恢复后自动回粘。
// 旧行为会在这里把绑定改写到 2，会话从此永久迁移、缓存全废。
func TestLoadAwarenessTransientRateLimitKeepsBinding(t *testing.T) {
	t.Setenv("STICKY_KEEP_BINDING_ON_TRANSIENT", "")
	resetStickyKeepBindingCacheForTest()

	groupID := int64(0)
	sessionHash := "sess-transient"
	resetAt := time.Now().Add(time.Hour)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, RateLimitResetAt: &resetAt},
			{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1},
		},
	}
	cache := &stubGatewayCache{sessionBindings: map[string]int64{"openai:" + sessionHash: 1}}
	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, sessionHash, "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil {
		t.Fatal("expected a selection")
	}
	if selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}
	if selection.Account.ID != 2 {
		t.Fatalf("本次应借道到账号 2，got %d", selection.Account.ID)
	}
	if got := cache.sessionBindings["openai:"+sessionHash]; got != 1 {
		t.Fatalf("临时限流不得改写绑定，期望仍为 1，got %d", got)
	}
}

