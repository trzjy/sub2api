package service

import (
	"time"
)

// QuotaKeyStatus 是激活码状态的脱敏视图。明文 code 绝不出现于任何字段。
type QuotaKeyStatus struct {
	ID               int64      `json:"id"`
	Status           string     `json:"status"`
	Exhausted        bool       `json:"exhausted"`
	LastRefreshAt    *time.Time `json:"last_refresh_at,omitempty"`
	LastRefreshError string     `json:"last_refresh_error,omitempty"`
	CodeValueMasked  string     `json:"code_masked"` // 仅末4位掩码，禁明文
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`

	// Usage 是来源额度快照的展示投影（方案 §25.6 消费闭包⑤ / §25.7-B）。
	//
	// 只读：来自 quota_keys.usage_snapshot 已提交值，解析/投影唯一实现在
	// quota_key_usage_snapshot_dto.go；无快照时为 nil（省略字段），**绝不落成 0**。
	// Create/Update 等不读快照的返回路径保持 nil，JSON 形态与接入前一致。
	Usage *QuotaKeyUsageProjection `json:"usage,omitempty"`
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
