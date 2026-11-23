package service

import (
	"context"
	"testing"
)

// TestStickyBindingDroppedOnMissClassification 锁定「哪条原因销毁绑定」的唯一判定。
// 这条判定的价值在于：临时原因一旦被误判成永久，会话就被永久迁移到新账号；
// 反过来永久原因被误判成临时，会话会反复重试一个死号。
func TestStickyBindingDroppedOnMissClassification(t *testing.T) {
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

// TestReleaseStickyBindingOnMiss 验证实际副作用：临时原因不动 Redis 绑定，永久原因才删。
func TestReleaseStickyBindingOnMiss(t *testing.T) {
	groupID := int64(115)
	key := openaiSessionCacheKeyForTest("h-transient")
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{key: 982}}
	svc := &OpenAIGatewayService{cache: cache}
	ctx := context.Background()

	// 限流（临时）：绑定必须留在原地，是否改写由转发成功后的覆盖写入决定。
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

// openaiSessionCacheKeyForTest 复算缓存键，避免测试里手写前缀漂移。
func openaiSessionCacheKeyForTest(sessionHash string) string {
	svc := &OpenAIGatewayService{}
	return svc.openAISessionCacheKey(sessionHash)
}
