package service

import (
	"time"
)

// QuotaKeyStatus 是激活码状态的脱敏视图。明文 code 绝不出现于任何字段。
type QuotaKeyStatus struct {
	ID               int64      `json:"id"`
	Platform         string     `json:"platform"`
	Status           string     `json:"status"`
	Exhausted        bool       `json:"exhausted"`
	LastRefreshAt    *time.Time `json:"last_refresh_at,omitempty"`
	LastRefreshError string     `json:"last_refresh_error,omitempty"`
	CodeValueMasked  string     `json:"code_masked"` // 仅末4位掩码，禁明文
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

// CreateQuotaKeyInput 是 QuotaKeyRepository.Create 的入参。
//
// 契约由 service 包持有，repository 包实现时引用本类型，
// 从而避免 service -> repository 的反向导入（repository 已导入 service）。
type CreateQuotaKeyInput struct {
	CodeEncrypted string
	CodeMAC       string
	CodeLast4     string
	Status        string
	Exhausted     bool
	ExpiresAt     *time.Time
	Notes         *string
}
