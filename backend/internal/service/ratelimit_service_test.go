//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestRateLimitService_HandleUpstreamError_Cursor403QuotaInsufficientTempUnschedulable
// KNCY-F 核心用例：cursor 账号 + 403 + 上游 body 含「用户额度不足」→ 判定为「今天花完了」，
// 只走 5 分钟临时停调（SetTempUnschedulable，until ≈ now+5min），绝不可 SetError 永久摘除。
func TestRateLimitService_Handle403_CursorQuotaInsufficientTempUnschedulable(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       985,
		Platform: PlatformCursor,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
	}

	body := []byte(`{"error":{"message":"用户额度不足, 剩余额度: ＄0.000000"}}`)
	shouldDisable := service.HandleUpstreamError(
		context.Background(),
		account,
		403,
		http.Header{},
		body,
	)

	// 本次请求应换号（failover）；但账号本身不判死。
	require.True(t, shouldDisable, "上游 403 必须返回 failover 信号")
	// 关键不变量：未调用 SetError，账号 status 不落 error。
	require.Equal(t, 0, repo.setErrorCalls, "cursor 额度不足 403 不得 SetError 永久摘除")
	require.Equal(t, StatusActive, account.Status, "cursor 额度不足 403 后 status 不得变 error")
	// 走了临时停调分支。
	require.Equal(t, 1, repo.tempCalls, "cursor 额度不足 403 应写入 SetTempUnschedulable")
	require.Equal(t, int64(985), repo.lastTempID)
	require.Contains(t, repo.lastTempReason, cursorUpstreamQuotaInsufficientReason)
	require.Equal(t, cursorUpstreamQuotaInsufficientReason, account.TempUnschedulableReason)
	require.NotNil(t, account.TempUnschedulableUntil, "账号内存应被标记为临时停调")
	// until ≈ now + 5min（UTC）。
	require.False(t, repo.lastTempUntil.IsZero(), "必须写入 temp-unschedulable 截止时间")
	elapsed := repo.lastTempUntil.Sub(time.Now().UTC())
	require.GreaterOrEqual(t, elapsed, 4*time.Minute, "冷却不得低于 4 分钟（默认 5 分钟）")
	require.LessOrEqual(t, elapsed, 6*time.Minute, "冷却不得高于 6 分钟（默认 5 分钟）")
}

// TestRateLimitService_HandleUpstreamError_NonCursor403QuotaInsufficientStillHandle403
// 回归保护：非 cursor 平台即便报文含「用户额度不足」，也必须走既有 handle403 处置（行为不变）。
func TestRateLimitService_Handle403_NonCursorQuotaInsufficientStillHandle403(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       986,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}

	body := []byte(`{"error":{"message":"用户额度不足, 剩余额度: ＄0.000000"}}`)
	shouldDisable := service.HandleUpstreamError(
		context.Background(),
		account,
		403,
		http.Header{},
		body,
	)

	// 非 cursor 平台不命中新分支：必须仍走 handle403（cache 为 nil → SetError）。
	require.True(t, shouldDisable)
	require.Equal(t, 0, repo.tempCalls, "非 cursor 平台不得走 cursor 额度不足临时停调分支")
	require.Equal(t, 1, repo.setErrorCalls, "非 cursor 平台同报文仍应走既有 handle403（SetError）")
	require.NotContains(t, repo.lastErrorMsg, cursorUpstreamQuotaInsufficientReason)
}

// TestRateLimitService_HandleUpstreamError_Cursor403OtherWordingStillHandle403
// 回归保护：cursor 平台 + 403 + 其它措辞（如账号封禁）不得命中新分支，仍走既有 handle403。
func TestRateLimitService_Handle403_CursorOtherWordingStillHandle403(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       987,
		Platform: PlatformCursor,
		Type:     AccountTypeAPIKey,
	}

	body := []byte(`{"error":{"message":"account may be suspended"}}`)
	shouldDisable := service.HandleUpstreamError(
		context.Background(),
		account,
		403,
		http.Header{},
		body,
	)

	// 其它措辞不命中「额度不足」，必须仍走 handle403（SetError）。
	require.True(t, shouldDisable)
	require.Equal(t, 0, repo.tempCalls, "cursor 其它措辞 403 不得走额度不足临时停调分支")
	require.Equal(t, 1, repo.setErrorCalls, "cursor 其它措辞 403 仍应走既有 handle403（SetError）")
	require.NotEqual(t, cursorUpstreamQuotaInsufficientReason, account.TempUnschedulableReason)
}
