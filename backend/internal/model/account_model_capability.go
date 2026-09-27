// Package model 定义服务层使用的数据模型。
package model

import "time"

// AccountModelCapability 是 (账号, 上游模型, 协议) 三元组上的上游能力标记。
//
// 方案来源：docs/capability-routing-plan.md §3.3。同一账号在不同模型、同一模型在
// 不同协议下的能力不同（例如 infer 在 chat_completions 下不解析图片，返回 200+正常
// 文本，网关拿不到错误信号），因此能力标记必须细化到三元组并独立表存储。
type AccountModelCapability struct {
	// ID 自增主键
	ID int64 `json:"id"`

	// AccountID 账号主键（对应 accounts.id）
	AccountID int64 `json:"account_id"`

	// UpstreamModel 上游模型名（发送到上游的原始模型标识）
	UpstreamModel string `json:"upstream_model"`

	// Protocol 协议维度：chat_completions / responses / anthropic。
	// 该维度不可省，否则会复现跨协议污染。
	Protocol string `json:"protocol"`

	// SupportsVision 是否支持视觉（读图）
	SupportsVision bool `json:"supports_vision"`

	// Source 来源：detect=一键检测落库 / manual=人工覆盖
	Source string `json:"source"`

	// DetectedAt 最近一次检测时间；纯人工覆盖可为空
	DetectedAt *time.Time `json:"detected_at,omitempty"`

	// UpdatedAt 更新时间
	UpdatedAt time.Time `json:"updated_at"`
}

// 能力来源常量
const (
	CapabilitySourceDetect = "detect"
	CapabilitySourceManual = "manual"
)

// 协议常量
const (
	CapabilityProtocolChatCompletions = "chat_completions"
	CapabilityProtocolResponses       = "responses"
	CapabilityProtocolAnthropic       = "anthropic"
)
