package service

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// codeBuddyRefreshWindow CodeBuddy token 提前刷新窗口。
// 上游 access_token 有效期较长（expiresIn 秒级下发），统一在到期前 24h 刷新。
const codeBuddyRefreshWindow = 24 * time.Hour

// CodeBuddyTokenRefresher 实现 TokenRefresher 接口
type CodeBuddyTokenRefresher struct {
	codeBuddyOAuthService *CodeBuddyOAuthService
}

func NewCodeBuddyTokenRefresher(codeBuddyOAuthService *CodeBuddyOAuthService) *CodeBuddyTokenRefresher {
	return &CodeBuddyTokenRefresher{
		codeBuddyOAuthService: codeBuddyOAuthService,
	}
}

// CacheKey 返回用于分布式锁的缓存键
func (r *CodeBuddyTokenRefresher) CacheKey(account *Account) string {
	uid := account.GetCredential("uid")
	if uid != "" {
		return "codebuddy:" + uid
	}
	return "codebuddy:account:" + strconv.FormatInt(account.ID, 10)
}

// CanRefresh 检查是否可以刷新此账户
func (r *CodeBuddyTokenRefresher) CanRefresh(account *Account) bool {
	return account.Platform == PlatformCodeBuddy && account.Type == AccountTypeOAuth
}

// NeedsRefresh 检查账户是否需要刷新（到期前 24h 窗口，忽略全局配置）
func (r *CodeBuddyTokenRefresher) NeedsRefresh(account *Account, _ time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return false
	}
	return time.Until(*expiresAt) < codeBuddyRefreshWindow
}

// Refresh 执行 token 刷新
func (r *CodeBuddyTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	tokenInfo, err := r.codeBuddyOAuthService.RefreshAccountToken(ctx, account)
	if err != nil {
		return nil, err
	}
	newCredentials := r.codeBuddyOAuthService.BuildAccountCredentials(tokenInfo)
	// 保留新 credentials 中不存在的旧字段（uid/nickname/domain 等）
	newCredentials = MergeCredentials(account.Credentials, newCredentials)
	if tokenInfo.ExpiresAt == 0 {
		fmt.Printf("[CodeBuddyTokenRefresher] Account %d: 上游未返回 expiresIn，保留旧 expires_at\n", account.ID)
	}
	return newCredentials, nil
}
