//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// 说明（2026-11-11）：客户端维度（用户全局 / 订阅分组）的等待队列已整体移除——
// 并发超限改为**立即拒绝**（429），不再排队等槽位。等待队列只保留**账号维度**，
// 因为那边等的是站点自己的资源释放，属于供给不足（503），等待仍有意义。
// 本文件因此只覆盖账号维度等待队列。

// TestDecrementAccountWaitCount_NilCache 确保 nil cache 不会 panic
func TestDecrementAccountWaitCount_NilCache(t *testing.T) {
	svc := &ConcurrencyService{cache: nil}
	svc.DecrementAccountWaitCount(context.Background(), 1)
}

// TestDecrementAccountWaitCount_CacheError 确保 cache 错误不会传播
func TestDecrementAccountWaitCount_CacheError(t *testing.T) {
	cache := &stubConcurrencyCacheForTest{}
	svc := NewConcurrencyService(cache)
	// DecrementAccountWaitCount 使用 background context，错误只记录日志不传播
	svc.DecrementAccountWaitCount(context.Background(), 1)
}

// TestWaitingQueueFlow_AccountLevel 测试账号级等待队列流程
func TestWaitingQueueFlow_AccountLevel(t *testing.T) {
	cache := &stubConcurrencyCacheForTest{waitAllowed: true}
	svc := NewConcurrencyService(cache)

	// 进入账号等待队列
	allowed, err := svc.IncrementAccountWaitCount(context.Background(), 42, 10)
	require.NoError(t, err)
	require.True(t, allowed)

	// 离开账号等待队列
	svc.DecrementAccountWaitCount(context.Background(), 42)
}

// TestWaitingQueueFull_Returns429Signal 测试等待队列满时返回 false
func TestWaitingQueueFull_Returns429Signal(t *testing.T) {
	// waitAllowed=false 模拟队列已满
	cache := &stubConcurrencyCacheForTest{waitAllowed: false}
	svc := NewConcurrencyService(cache)

	// 账号级等待队列满
	allowed, err := svc.IncrementAccountWaitCount(context.Background(), 1, 10)
	require.NoError(t, err)
	require.False(t, allowed, "账号等待队列满时应返回 false")
}

// TestWaitingQueue_FailOpen_OnCacheError 测试 Redis 故障时 fail-open
func TestWaitingQueue_FailOpen_OnCacheError(t *testing.T) {
	cache := &stubConcurrencyCacheForTest{waitErr: errors.New("redis connection refused")}
	svc := NewConcurrencyService(cache)

	// 账号级：Redis 错误时允许通过
	allowed, err := svc.IncrementAccountWaitCount(context.Background(), 1, 10)
	require.NoError(t, err, "Redis 错误不应向调用方传播")
	require.True(t, allowed, "Redis 故障时应 fail-open 放行")
}
