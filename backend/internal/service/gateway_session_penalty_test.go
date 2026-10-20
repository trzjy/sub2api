package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPenalizeSessionAccountForCapacityShed_DeletesStickyAndRefreshesRecency
// 验证容量降载换号导流的核心行为：清 primary+legacy 双粘性键，并刷新 last_used 让 LRU 降权。
func TestPenalizeSessionAccountForCapacityShed_DeletesStickyAndRefreshesRecency(t *testing.T) {
	currentHash, legacyHash := deriveOpenAISessionHashes("sess-seed-2026-10-20")
	primaryKey := "openai:" + currentHash
	legacyKey := "openai:" + legacyHash

	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{
			primaryKey: 244,
			legacyKey:  244,
		},
	}
	deferred := &DeferredService{}

	svc := &OpenAIGatewayService{
		cache:          cache,
		deferredService: deferred,
	}

	ctx := withOpenAILegacySessionHash(context.Background(), legacyHash)
	var groupID int64 = 7
	svc.PenalizeSessionAccountForCapacityShed(ctx, &groupID, currentHash, 244)

	// primary + legacy 各调用一次 DeleteSessionAccountID
	require.Equal(t, 1, cache.deletedSessions[primaryKey], "primary 键应被删除一次")
	require.Equal(t, 1, cache.deletedSessions[legacyKey], "legacy 键应被删除一次")
	require.Empty(t, cache.sessionBindings, "粘性绑定应已被清除")

	// ScheduleLastUsedUpdate 应被调用一次，刷新降权 recency
	count := 0
	deferred.lastUsedUpdates.Range(func(k, _ any) bool {
		count++
		require.Equal(t, int64(244), k.(int64))
		return true
	})
	require.Equal(t, 1, count, "ScheduleLastUsedUpdate 应被调用一次")
}

// TestPenalizeSessionAccountForCapacityShed_NoLegacyWhenCtxMissing
// 当 ctx 未携带 legacy hash 时，只删 primary 键，不 panic。
func TestPenalizeSessionAccountForCapacityShed_NoLegacyWhenCtxMissing(t *testing.T) {
	currentHash, _ := deriveOpenAISessionHashes("sess-seed-no-legacy")
	primaryKey := "openai:" + currentHash

	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{primaryKey: 244},
	}
	svc := &OpenAIGatewayService{cache: cache, deferredService: &DeferredService{}}

	svc.PenalizeSessionAccountForCapacityShed(context.Background(), nil, currentHash, 244)

	require.Equal(t, 1, cache.deletedSessions[primaryKey], "无 legacy 时仍应删除 primary 键")
	require.Len(t, cache.deletedSessions, 1, "不应出现 legacy 删除")
}

// TestPenalizeSessionAccountForCapacityShed_NoOpOnEmptyHash
// sessionHash 为空时安全 no-op，cache 零调用、不 panic。
func TestPenalizeSessionAccountForCapacityShed_NoOpOnEmptyHash(t *testing.T) {
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:whatever": 244},
	}
	svc := &OpenAIGatewayService{cache: cache, deferredService: &DeferredService{}}

	require.NotPanics(t, func() {
		svc.PenalizeSessionAccountForCapacityShed(context.Background(), nil, "", 244)
	})
	require.Empty(t, cache.deletedSessions, "空 sessionHash 不应触碰 cache")
}

// TestPenalizeSessionAccountForCapacityShed_NoOpOnNonPositiveAccount
// accountID<=0 时安全 no-op，cache 零调用、不 panic。
func TestPenalizeSessionAccountForCapacityShed_NoOpOnNonPositiveAccount(t *testing.T) {
	currentHash, _ := deriveOpenAISessionHashes("sess-seed-bad-account")
	primaryKey := "openai:" + currentHash

	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{primaryKey: 244},
	}
	svc := &OpenAIGatewayService{cache: cache, deferredService: &DeferredService{}}

	require.NotPanics(t, func() {
		svc.PenalizeSessionAccountForCapacityShed(context.Background(), nil, currentHash, 0)
	})
	require.Empty(t, cache.deletedSessions, "accountID<=0 不应触碰 cache")
	require.Len(t, cache.sessionBindings, 1, "原绑定应保留")
}

// TestPenalizeSessionAccountForCapacityShed_NoOpOnNilService
// s 为 nil 时安全 no-op（方法通过指针接收者调用，不得解引用 cache）。
func TestPenalizeSessionAccountForCapacityShed_NoOpOnNilService(t *testing.T) {
	var svc *OpenAIGatewayService
	require.NotPanics(t, func() {
		svc.PenalizeSessionAccountForCapacityShed(context.Background(), nil, "abc", 244)
	})
}

// TestIsOpenAICapacityShedError covers mid-stream plain-error capacity-shed classification.
func TestIsOpenAICapacityShedError(t *testing.T) {
	require.True(t, IsOpenAICapacityShedError(errors.New("upstream response failed: Our servers are currently overloaded. Please try again later.")))
	require.True(t, IsOpenAICapacityShedError(errors.New("server is overloaded")))
	require.False(t, IsOpenAICapacityShedError(errors.New("The usage limit has been reached")))
	require.False(t, IsOpenAICapacityShedError(errors.New("unauthorized")))
	require.False(t, IsOpenAICapacityShedError(nil))
}
