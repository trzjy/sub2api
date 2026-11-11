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
	// Notes 是管理员填写的备注（quota_keys.notes）。属于同一行记录的非敏感元数据，
	// 与 expires_at 一并由 service 状态对象携带，使列表/详情/编辑回填读同一份投影，
	// 不再各自挑字段（多轨投影导致的展示漂移修复）。
	Notes *string `json:"notes,omitempty"`

	// Priority 是激活码优先级（quota_keys.priority），数值越小越优先，与 accounts.priority
	// 同一语义。该码名下账号的 priority 跟随此值：拉号入库时写入，修改时同事务同步
	//（用户裁定 2026-11-03：日限额码取小值优先消耗，月总限额码取大值兜底）。
	Priority int `json:"priority"`

	// UpstreamKey 是本激活码所属上游的稳定标识（引用 quota_upstreams.key）。
	// 空串 = 该行未回填（存量数据或读路径未取到）；前端据此回落显示原始标识。
	UpstreamKey string

	// Usage 是来源额度快照的展示投影（方案 §25.6 消费闭包⑤ / §25.7-B）。
	//
	// 只读：来自 quota_keys.usage_snapshot 已提交值，解析/投影唯一实现在
	// quota_key_usage_snapshot_dto.go；无快照时为 nil（省略字段），**绝不落成 0**。
	// Create/Update 等不读快照的返回路径保持 nil，JSON 形态与接入前一致。
	Usage *QuotaKeyUsageProjection `json:"usage,omitempty"`

	// Concurrency 是码级并发数（B 批上移到激活码管理，不含余额查询）。
	// nil / 省略表示沿用所属账号列的默认 3；正整数时后端同事务同步该码名下全部账号的并发数。
	// JSON 字段名与前端 frontend/src/api/admin/quotaKeys.ts 精确对齐。
	Concurrency *int `json:"concurrency,omitempty"`

	// LoadFactor 是码级负载因子（B 批上移）。
	// nil / 省略表示清空（卸载）该码名下账号的负载因子；正整数 1..10000。
	LoadFactor *int `json:"load_factor,omitempty"`

	// RateMultiplier 是码级计费倍率（B 批上移）。
	// ≥ 0，默认 1.0，允许为 0。不加 omitempty，使 0 也能稳定序列化（区别于未设置语义）。
	RateMultiplier float64 `json:"rate_multiplier"`

	// ModelMapping 是码级模型限制与映射（码级权威值，与 credentials.model_mapping 同构：
	// 白名单存恒等映射 {"m":"m"}，映射存 {"from":"to"}）。
	// nil / 省略表示码级未显式覆盖，沿用上游默认实供清单。
	ModelMapping map[string]string `json:"model_mapping,omitempty"`

	// GroupIDs 是码级权威分组 ID 列表（quota_keys.group_ids）。
	// nil = 未显式指定（拉号沿用模型推导挂组）；空切片 = 显式解绑全部分组；非空 = 码级权威。
	// 与 concurrency / load_factor / rate_multiplier / model_mapping 同为码级权威值，
	// 编辑时在 withQuotaKeyLock 同一事务内同步名下未软删账号。
	GroupIDs *[]int64 `json:"group_ids,omitempty"`
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
	// Priority 是新建激活码的优先级；0 表示未指定，仓储层按列默认值（1）落库。
	Priority int
	// UpstreamKey 是新建激活码所属上游的稳定标识（引用 quota_upstreams.key）。
	// 空串 = 未指定，由仓储层落列默认值（'shareai'）。非空时必须对应 quota_upstreams
	// 中存在的、未软删的上游标识；service 层负责校验存在性。
	UpstreamKey string

	// Concurrency 是新建激活码的码级并发数（B 批上移，不含余额查询）。
	// nil 表示沿用所属账号列的默认 3；非 nil 时后端同事务同步该码名下账号的并发数。
	Concurrency *int

	// LoadFactor 是新建激活码的码级负载因子（B 批上移）。
	// nil 表示清空（卸载）该码名下账号的负载因子；非 nil 为正整数 1..10000。
	LoadFactor *int

	// RateMultiplier 是新建激活码的码级计费倍率（B 批上移）。
	// nil 表示沿用默认 1.0；非 nil 时设为 >= 0（允许 0）。
	RateMultiplier *float64

	// ModelMapping 是新建激活码的码级模型限制与映射（与 credentials.model_mapping 同构）。
	// nil 表示沿用上游默认实供清单；非 nil 时必须至少含 1 个有效模型，空映射由服务层拒绝。
	ModelMapping map[string]string

	// GroupIDs 是新建激活码的码级权威分组；GroupIDsSet=false 表示未提供（落 NULL）。
	// GroupIDsSet=true 且 GroupIDs 为空切片时表示显式解绑全部分组；非空时为码级权威分组。
	GroupIDs    []int64
	GroupIDsSet bool
}

// UpdateQuotaKeySettingsInput 是激活码设置更新入参（供后续 Update / QuotaKeyRepository 消费，
// 支持三态清零语义）。与 CreateQuotaKeyInput 不同，本结构体区分"不修改"与"清空/清零"两态。
//
// 三态约定：
//   - Concurrency / LoadFactor 用 **int（双层指针）：
//     外层 nil = 不修改；内层 nil = 清空（Concurrency 恢复账号默认 3，LoadFactor 卸载）；
//     非 nil = 设为正值（Concurrency ≥ 1；LoadFactor 1..10000）。
//   - RateMultiplier 用 *float64：nil = 不修改；非 nil = 设为 >= 0（允许 0）。
//   - ModelMapping + ModelMappingSet：ModelMappingSet=false = 不修改；
//     ModelMappingSet=true 时 ModelMapping 必须非空，空映射由服务层拒绝，禁止落库。
//   - GroupIDs 用 *[]int64：nil = 不修改；非 nil 且指向空切片 = 解绑全部；非 nil 非空 = 覆盖。
//     group_ids 与 concurrency / load_factor / rate_multiplier / model_mapping 同为码级权威值，
//     编辑时在 withQuotaKeyLock 同一事务内同步名下未软删账号。
type UpdateQuotaKeySettingsInput struct {
	// Concurrency 外层 nil=不修改；内层 nil=清空恢复账号默认 3；非 nil=设为正值。
	Concurrency **int
	// LoadFactor 外层 nil=不修改；内层 nil=清空/卸载；非 nil=设为 1..10000。
	LoadFactor **int
	// RateMultiplier nil=不修改；非 nil=设为 >= 0（允许 0）。
	RateMultiplier *float64
	// ModelMapping 与 ModelMappingSet 配合：ModelMappingSet=false=不修改；
	// 为 true 时 ModelMapping 必须非空，空映射由服务层拒绝，禁止落库。
	ModelMapping    map[string]string
	ModelMappingSet bool
	// GroupIDs 三态：nil = 不修改；非 nil 且指向空切片 = 解绑全部；非 nil 非空 = 覆盖。
	GroupIDs *[]int64
}
