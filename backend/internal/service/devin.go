package service

// Devin 账号辅助函数（类似 codebuddy.go）

// IsDevin 报告账号是否为 Devin 平台
func (a *Account) IsDevin() bool {
	return a != nil && a.Platform == PlatformDevin
}

// GetDevinSessionToken 获取 Devin session token（存储在 credentials["token"]）
func (a *Account) GetDevinSessionToken() string {
	if a == nil {
		return ""
	}
	return a.GetCredential("token")
}

// NormalizeDevinToken 规范化 Devin token（确保有 devin-session-token$ 前缀）
func NormalizeDevinToken(token string) string {
	const prefix = "devin-session-token$"
	if len(token) > len(prefix) && token[:len(prefix)] == prefix {
		return token
	}
	return prefix + token
}
