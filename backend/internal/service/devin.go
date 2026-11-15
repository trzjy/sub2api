package service

// Devin 账号辅助函数（类似 codebuddy.go）

// IsDevin 报告账号是否为 Devin 平台
func (a *Account) IsDevin() bool {
	return a != nil && a.Platform == PlatformDevin
}

// GetDevinToken 获取 Devin token（存储在 credentials["api_key"]）
func (a *Account) GetDevinToken() string {
	if a == nil {
		return ""
	}
	return a.GetCredential("api_key")
}

// GetDevinSessionToken 获取 Devin session token（兼容旧调用方，委托 GetDevinToken）
func (a *Account) GetDevinSessionToken() string {
	return a.GetDevinToken()
}

// NormalizeDevinToken 规范化 Devin token（确保有 devin-session-token$ 前缀）
func NormalizeDevinToken(token string) string {
	const prefix = "devin-session-token$"
	if len(token) > len(prefix) && token[:len(prefix)] == prefix {
		return token
	}
	return prefix + token
}
