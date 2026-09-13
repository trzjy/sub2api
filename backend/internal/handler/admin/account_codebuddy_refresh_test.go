package admin

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// codeBuddyRefresherStub 记录管理端刷新是否命中 codebuddy 专用链路。
type codeBuddyRefresherStub struct {
	called  bool
	account *service.Account
}

func (s *codeBuddyRefresherStub) RefreshAccountCredentials(_ context.Context, account *service.Account) (map[string]any, error) {
	s.called = true
	s.account = account
	return nil, errors.New("stub-codebuddy-refresh")
}

// TestRefreshSingleAccount_CodeBuddyUsesDedicatedRefresher 钉住 F10 回归：
// 管理端账号「刷新」对 codebuddy 必须走专用刷新链路，而不是兜底到通用 OAuth 刷新
// （后者会打到 ChatGPT/Codex 后端被 Cloudflare 403）。
func TestRefreshSingleAccount_CodeBuddyUsesDedicatedRefresher(t *testing.T) {
	stub := &codeBuddyRefresherStub{}
	h := &AccountHandler{codeBuddyRefresher: stub}

	account := &service.Account{
		ID:          60,
		Platform:    service.PlatformCodeBuddy,
		Type:        service.AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "rt"},
	}

	_, _, err := h.refreshSingleAccount(context.Background(), account)
	require.True(t, stub.called, "codebuddy 刷新必须命中专用 refresher（F10 回归点）")
	require.Same(t, account, stub.account)
	require.ErrorContains(t, err, "stub-codebuddy-refresh")
}

// TestRefreshSingleAccount_CodeBuddyWithoutRefresherFailsClosed 验证未注入专用依
// 赖时显式报错（fail-closed），不会静默落到通用 OAuth 链路。
func TestRefreshSingleAccount_CodeBuddyWithoutRefresherFailsClosed(t *testing.T) {
	h := &AccountHandler{}

	account := &service.Account{
		ID:       61,
		Platform: service.PlatformCodeBuddy,
		Type:     service.AccountTypeOAuth,
	}

	_, _, err := h.refreshSingleAccount(context.Background(), account)
	require.ErrorContains(t, err, "codebuddy account refresher is not configured")
}
