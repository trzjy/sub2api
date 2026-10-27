package dto

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type User struct {
	ID            int64      `json:"id"`
	Email         string     `json:"email"`
	Username      string     `json:"username"`
	Role          string     `json:"role"`
	Balance       float64    `json:"balance"`
	FrozenBalance float64    `json:"frozen_balance"`
	Concurrency   int        `json:"concurrency"`
	Status        string     `json:"status"`
	AllowedGroups []int64    `json:"allowed_groups"`
	LastActiveAt  *time.Time `json:"last_active_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`

	// 余额不足通知
	BalanceNotifyEnabled       bool               `json:"balance_notify_enabled"`
	BalanceNotifyThresholdType string             `json:"balance_notify_threshold_type"`
	BalanceNotifyThreshold     *float64           `json:"balance_notify_threshold"`
	BalanceNotifyExtraEmails   []NotifyEmailEntry `json:"balance_notify_extra_emails"`
	TotalRecharged             float64            `json:"total_recharged"`

	// RPMLimit 用户级每分钟请求数上限（0 = 不限制），仅在所用分组未设置 rpm_limit 时作为兜底生效。
	RPMLimit int `json:"rpm_limit"`

	APIKeys       []APIKey           `json:"api_keys,omitempty"`
	Subscriptions []UserSubscription `json:"subscriptions,omitempty"`
}

// AdminUser 是管理员接口使用的 user DTO（包含敏感/内部字段）。
// 注意：普通用户接口不得返回 notes 等管理员备注信息。
type AdminUser struct {
	User

	Notes      string     `json:"notes"`
	LastUsedAt *time.Time `json:"last_used_at"`
	// GroupRates 用户专属分组倍率配置
	// map[groupID]rateMultiplier
	GroupRates map[int64]float64 `json:"group_rates,omitempty"`
	// RestrictPublicGroups 为 true 时，该用户仅可使用 allowed_groups 中列出的
	// 公开分组。这是管理侧的权限开关，不下发给用户自身的接口。
	RestrictPublicGroups bool `json:"restrict_public_groups"`
}

type APIKey struct {
	ID          int64      `json:"id"`
	UserID      int64      `json:"user_id"`
	Key         string     `json:"key"`
	Name        string     `json:"name"`
	GroupID     *int64     `json:"group_id"`
	Status      string     `json:"status"`
	IPWhitelist []string   `json:"ip_whitelist"`
	IPBlacklist []string   `json:"ip_blacklist"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	LastUsedIP  *string    `json:"last_used_ip"`
	Quota       float64    `json:"quota"`      // Quota limit in USD (0 = unlimited)
	QuotaUsed   float64    `json:"quota_used"` // Used quota amount in USD
	ExpiresAt   *time.Time `json:"expires_at"` // Expiration time (nil = never expires)
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// CurrentConcurrency is the real-time active request count for this API key.
	CurrentConcurrency int `json:"current_concurrency"`

	// Rate limit fields
	RateLimit5h   float64    `json:"rate_limit_5h"`
	RateLimit1d   float64    `json:"rate_limit_1d"`
	RateLimit7d   float64    `json:"rate_limit_7d"`
	Usage5h       float64    `json:"usage_5h"`
	Usage1d       float64    `json:"usage_1d"`
	Usage7d       float64    `json:"usage_7d"`
	Window5hStart *time.Time `json:"window_5h_start"`
	Window1dStart *time.Time `json:"window_1d_start"`
	Window7dStart *time.Time `json:"window_7d_start"`
	Reset5hAt     *time.Time `json:"reset_5h_at,omitempty"`
	Reset1dAt     *time.Time `json:"reset_1d_at,omitempty"`
	Reset7dAt     *time.Time `json:"reset_7d_at,omitempty"`

	User  *User  `json:"user,omitempty"`
	Group *Group `json:"group,omitempty"`
}

type Group struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	Platform       string  `json:"platform"`
	RateMultiplier float64 `json:"rate_multiplier"`
	IsExclusive    bool    `json:"is_exclusive"`
	Status         string  `json:"status"`

	// I18n 名称与描述的各语言译文。只有管理端输出；用户侧接口经 Localized 把
	// name/description 换成请求语言的版本后清空本字段。
	I18n domain.GroupI18n `json:"i18n,omitempty"`

	SubscriptionType          string   `json:"subscription_type"`
	DailyLimitUSD             *float64 `json:"daily_limit_usd"`
	WeeklyLimitUSD            *float64 `json:"weekly_limit_usd"`
	MonthlyLimitUSD           *float64 `json:"monthly_limit_usd"`
	LongContextPricingEnabled bool     `json:"long_context_pricing_enabled"`

	// 图片生成计费配置（仅 antigravity 平台使用）
	AllowImageGeneration         bool    `json:"allow_image_generation"`
	AllowBatchImageGeneration    bool    `json:"allow_batch_image_generation"`
	ImageRateIndependent         bool    `json:"image_rate_independent"`
	ImageRateMultiplier          float64 `json:"image_rate_multiplier"`
	BatchImageDiscountMultiplier float64 `json:"batch_image_discount_multiplier"`
	BatchImageHoldMultiplier     float64 `json:"batch_image_hold_multiplier"`
	VideoRateIndependent         bool    `json:"video_rate_independent"`
	VideoRateMultiplier          float64 `json:"video_rate_multiplier"`
	// 高峰时段倍率配置
	PeakRateEnabled    bool     `json:"peak_rate_enabled"`
	PeakStart          string   `json:"peak_start"`
	PeakEnd            string   `json:"peak_end"`
	PeakRateMultiplier float64  `json:"peak_rate_multiplier"`
	ImagePrice1K       *float64 `json:"image_price_1k"`
	ImagePrice2K       *float64 `json:"image_price_2k"`
	ImagePrice4K       *float64 `json:"image_price_4k"`
	VideoPrice480P     *float64 `json:"video_price_480p"`
	VideoPrice720P     *float64 `json:"video_price_720p"`
	VideoPrice1080P    *float64 `json:"video_price_1080p"`
	// VideoModelPrices 可选按模型族×分辨率覆盖视频每秒单价 (USD/s)。
	VideoModelPrices map[string]map[string]float64 `json:"video_model_prices,omitempty"`
	// Codex alpha/search 网页搜索单次价格（USD/次）；null 表示使用默认价 0.01
	WebSearchPricePerCall        *float64 `json:"web_search_price_per_call"`
	SearchPricePer1k             *float64 `json:"search_price_per_1k"`
	AudioRealtimePricePerMin     *float64 `json:"audio_realtime_price_per_min"`
	AudioTtsPricePerMillionChars *float64 `json:"audio_tts_price_per_million_chars"`
	AudioSttPricePerHour         *float64 `json:"audio_stt_price_per_hour"`

	// Claude Code 客户端限制
	ClaudeCodeOnly  bool   `json:"claude_code_only"`
	FallbackGroupID *int64 `json:"fallback_group_id"`
	// 无效请求兜底分组
	FallbackGroupIDOnInvalidRequest *int64 `json:"fallback_group_id_on_invalid_request"`

	// OpenAI Messages 调度开关（用户侧需要此字段判断是否展示 Claude Code 教程）
	AllowMessagesDispatch bool `json:"allow_messages_dispatch"`
	// OpenAI Live 接口开关
	AllowLive bool `json:"allow_live"`

	// 账号过滤控制（仅 OpenAI/Antigravity 平台有效）
	RequireOAuthOnly  bool `json:"require_oauth_only"`
	RequirePrivacySet bool `json:"require_privacy_set"`

	// RPMLimit 分组级每分钟请求数上限（0 = 不限制），设置后覆盖用户级 rpm_limit。
	RPMLimit int `json:"rpm_limit"`
	// Concurrency 订阅分组各自并发上限（0 = 不限制）；仅订阅类型分组生效，计费模式不看此字段。
	Concurrency int `json:"concurrency"`
	// MaxReasoningEffort Anthropic/OpenAI 请求的推理强度上限，空字符串表示不限制。
	MaxReasoningEffort string `json:"max_reasoning_effort"`
	// MaxReasoningEffortOverLimit 超过上限时的访问控制：downgrade（默认）或 deny。
	MaxReasoningEffortOverLimit string `json:"max_reasoning_effort_over_limit"`
	// ReasoningEffortMappings Anthropic/OpenAI 推理强度映射，可按模型精确名、前缀或后缀限定。
	ReasoningEffortMappings []domain.ReasoningEffortMapping `json:"reasoning_effort_mappings"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AdminGroup 是管理员接口使用的 group DTO（包含敏感/内部字段）。
// 注意：普通用户接口不得返回 model_routing/account_count/account_groups 等内部信息。
type AdminGroup struct {
	Group
	// ForceOpenAIFast 是管理端请求策略，用户侧分组 DTO 无需暴露。
	ForceOpenAIFast bool `json:"force_openai_fast"`
	// FreeOpenAIFast 是管理端计费策略，用户侧分组 DTO 无需暴露。
	FreeOpenAIFast bool `json:"free_openai_fast"`

	// AggregateDirectEnabled 是聚合直绑开关（管理端配置）：true=该聚合族分组候选池
	// 并入内部聚合上游；false=关闭（默认）。管理端专属字段——2026-10-18 用户裁定：
	// 该字段从用户/管理共享的 Group 迁出（原名 aggregate_codebuddy_enabled 的
	// JSON tag 字面含内部供给方名，用户响应不得出现），tag 同步改中性名。
	AggregateDirectEnabled bool `json:"aggregate_direct_enabled"`

	// 分组利润控制（五个 token 平台分组可启用；margin/buffer 为小数存储）。
	// 仅管理员可见：这三个字段与同响应中的 rate_multiplier 相乘即可反推出
	// 运营方的上游成本上限，属于内部经营信息，不得下放到 dto.Group。
	ProfitControlEnabled bool                          `json:"profit_control_enabled"`
	ProfitMinMargin      float64                       `json:"profit_min_margin"`
	ProfitSafetyBuffer   float64                       `json:"profit_safety_buffer"`
	ModelPricing         []service.ChannelModelPricing `json:"model_pricing"`

	// 模型路由配置（仅 anthropic 平台使用）
	ModelRouting        map[string][]int64 `json:"model_routing"`
	ModelRoutingEnabled bool               `json:"model_routing_enabled"`

	// 视觉分流配置（docs-local/capability-routing-plan.md §3.7），同构 model_routing
	VisionRouting map[string][]int64 `json:"vision_routing"`

	// MCP XML 协议注入（仅 antigravity 平台使用）
	MCPXMLInject bool `json:"mcp_xml_inject"`

	// OpenAI Messages 调度配置（仅 openai 平台使用）
	DefaultMappedModel          string                                   `json:"default_mapped_model"`
	MessagesDispatchModelConfig domain.OpenAIMessagesDispatchModelConfig `json:"messages_dispatch_model_config"`
	ModelAllowlist              service.GroupModelAllowlist              `json:"model_allowlist"`
	// 固定账号获取 Codex Model Manifest 配置（仅 openai 平台使用）。
	CodexModelsManifestConfig domain.GroupCodexModelsManifestConfig `json:"codex_models_manifest_config"`

	// 支持的模型系列（仅 antigravity 平台使用）
	SupportedModelScopes    []string       `json:"supported_model_scopes"`
	AccountGroups           []AccountGroup `json:"account_groups,omitempty"`
	AccountCount            int64          `json:"account_count,omitempty"`
	ActiveAccountCount      int64          `json:"active_account_count,omitempty"`
	RateLimitedAccountCount int64          `json:"rate_limited_account_count,omitempty"`

	// 分组排序
	SortOrder int `json:"sort_order"`
}

// w14 conflict-resolution fusion (second wave, B9 — funds_or_state)
//
// 分叉取证（blob：base 7c952af6 / ours ac76ba62 / upstream 73c02a7a）：
// base→ours = 43 增 2 删，六处自研新增：AdminGroup.AggregateDirectEnabled、
// Group.Concurrency、AdminGroup.VisionRouting、Account.BalanceProbe、
// Account.QuotaDimensions + AccountQuotaDimension 类型 + Account.BalanceLow、
// AccountListItem.QuotaDimensions/BalanceLow、RedeemCode.BatchID/GroupGrants。
// base→upstream = 仅 2 行：给 Account 与 AccountListItem 各追加一个
// OpenCodeGoUsage *service.OpenCodeGoUsageState（json 键 opencode_go_usage,omitempty），
// 位置均紧贴 OllamaCloudUsage。Account 侧与我方 BalanceProbe 同一插入点（文本冲突），
// AccountListItem 侧双方在不同 hunk（Git 可自动合并）。
// 双方在本文件均无 funds 数值（价格/倍率/汇率/冷却时长）改动，故权威表第 7 条
// 无「双方值」可登记，本卡不适用。
//
// keep OURS —— 上述我方六处新增全部原样保留（merge-plan §5 rule 1：我方有重写的符号
// 以我方语义为准；§6 自研能力矩阵的「管理后台配额单轨展示」「额度/余额熔断」
// 「聚合平台路由」「视觉分流」四条依赖它们）。任一条被静默丢弃都会直接下线对应业务
// 链路，且前端/仓库侧无补偿路径。
//
// take UPSTREAM（暂缓，跨卡依赖）—— Account/AccountListItem 的 OpenCodeGoUsage 字段：
// 1. 类型定义缺席：service.OpenCodeGoUsageState 只定义在上游独有文件
// backend/internal/service/opencode_go_usage.go:123。取证结果：该文件所在的整个
// opencode_go 子系统在本工作树一个都不在 —— service/opencode_go.go、
// service/opencode_go_usage.go、repository/account_repo_opencode_go_usage.go、
// handler/admin/account_opencode_go_usage.go、migrations/238_opencode_go_platform.sql、
// frontend/src/components/account/OpenCodeGoUsageCell.vue 均不存在；对
// b0-base→b0-upstream 的上游独有新增路径抽样 400 条，入树数 = 0。
// 即权威表第 5 条「定义随自动合并已入树」的前提在本机不成立：main HEAD 损坏
// （57a080bc 缺对象），方案登记的隔离 worktree（~/sub2api-workflow/merge-v0.2.15、
// /home/zjy/sub2api-workflow/b0-analysis）均 prunable 不存在，上游独有内容要到
// B11 真正执行 git merge 时才入树。
// 2. 单方面加本字段只会把编译错变成断言失败，并不会让最终树变绿：上游对该字段的消费点
// 全在本卡文件清单之外 —— dto/mappers.go:244-247 与 :258（AccountFromServiceShallow
// 赋值）、:426-427（redactAccountManagedExtra 屏蔽两个 OpenCodeGo extra 键）、:468
// （lite 投影）。而 mappers.go 的 owner 是 w12 卡、已按 keep-ours 交付（工作树 blob
// 45a816f5 == b0-ours，无任何 OpenCodeGo 接线）。上游独有测试
// dto/account_list_item_opencode_usage_test.go 断言 require.NotNil(item.OpenCodeGoUsage)，
// 缺 mapper 接线时该断言必失败。
// 3. 现场纪律第 4 条 + 方案 §4「每路径恰一个 owner」：本卡无权改 mappers.go / service 包，
// 也无权替 B11 决定是否整体吸收 opencode_go 子系统（含迁移 238 与前端单元格）。
// 4. 并行波次构建面：本包目前处于可干净编译状态（go build ./internal/handler/... exit 0）。
// 只加本字段会让 dto 及其全部下游（handler、handler/admin、server/routes）立刻
// undefined: service.OpenCodeGoUsageState —— 与同批 w28 对 routes.RegisterPaymentRoutes
// 末位 redisClient 实参「调用点/定义侧不同卡」的处理同理，故不在本卡单方面落地。
//
// 收敛单元（必须一次集成，缺一不可）：
// (a) 上游 opencode_go 子系统入树（B11 git merge 自动带入，含 service/repo/admin
// handler/migration 238/前端组件）；
// (b) dto/mappers.go 三处接线 + redactAccountManagedExtra 两个 extra 键
// （owner = w12 卡，需 B11 复核后重开）；
// (c) 本文件 Account（建议紧随 OllamaCloudUsage 之后、与上游同序）与 AccountListItem
// 各加 OpenCodeGoUsage *service.OpenCodeGoUsageState（json:"opencode_go_usage,omitempty"）
// （owner = 本卡 w14，届时同步更新 types_fusion_test.go 的缺席哨兵断言）；
// (d) 上游测试 dto/account_list_item_opencode_usage_test.go（75 张卡无人认领，
// 需 B11 派发或随 (a) 自动入树后一并验证）。
// 验证锚点：go build ./internal/handler/dto/ 通过，且
// go test ./internal/handler/dto/ -run 'OpenCodeGo|QuotaDimensions|AccountDTO' 全绿。
// 反向保护：types_fusion_test.go 的 TestAccountDTO_UpstreamOpenCodeGoUsageAbsent 是
// 本字段的哨兵，(c) 落地时该测试必然失败 —— 这是设计中的报警，届时按上游形态更新该
// 断言即可，不得为了让它变绿而回退 (a)(b)。

type Account struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Notes    *string `json:"notes"`
	Platform string  `json:"platform"`
	Type     string  `json:"type"`
	// Credentials 经 RedactCredentials 处理后只含非敏感子键；敏感 token / api_key / 私钥
	// 的存在性通过 CredentialsStatus（has_<key>）暴露，原始值不返回前端。
	Credentials             map[string]any                 `json:"credentials"`
	CredentialsStatus       map[string]bool                `json:"credentials_status,omitempty"`
	Extra                   map[string]any                 `json:"extra"`
	OllamaCloudUsage        *service.OllamaCloudUsageState `json:"ollama_cloud_usage,omitempty"`
	OpenCodeGoUsage         *service.OpenCodeGoUsageState  `json:"opencode_go_usage,omitempty"`
	BalanceProbe            *service.BalanceProbeConfig    `json:"balance_probe,omitempty"`
	ProxyID                 *int64                         `json:"proxy_id"`
	ProxyFallbackOriginID   *int64                         `json:"proxy_fallback_origin_id"`
	ProxyFallbackOriginName *string                        `json:"proxy_fallback_origin_name,omitempty"`
	Concurrency             int                            `json:"concurrency"`
	LoadFactor              *int                           `json:"load_factor,omitempty"`
	Priority                int                            `json:"priority"`
	RateMultiplier          float64                        `json:"rate_multiplier"`
	Status                  string                         `json:"status"`
	ErrorMessage            string                         `json:"error_message"`
	LastUsedAt              *time.Time                     `json:"last_used_at"`
	ExpiresAt               *int64                         `json:"expires_at"`
	AutoPauseOnExpired      bool                           `json:"auto_pause_on_expired"`
	CreatedAt               time.Time                      `json:"created_at"`
	UpdatedAt               time.Time                      `json:"updated_at"`

	Schedulable bool `json:"schedulable"`

	RateLimitedAt    *time.Time `json:"rate_limited_at"`
	RateLimitResetAt *time.Time `json:"rate_limit_reset_at"`
	OverloadUntil    *time.Time `json:"overload_until"`

	TempUnschedulableUntil  *time.Time `json:"temp_unschedulable_until"`
	TempUnschedulableReason string     `json:"temp_unschedulable_reason"`

	SessionWindowStart  *time.Time `json:"session_window_start"`
	SessionWindowEnd    *time.Time `json:"session_window_end"`
	SessionWindowStatus string     `json:"session_window_status"`

	// 5h窗口费用控制（仅 Anthropic OAuth/SetupToken 账号有效）
	// 从 extra 字段提取，方便前端显示和编辑
	WindowCostLimit         *float64 `json:"window_cost_limit,omitempty"`
	WindowCostStickyReserve *float64 `json:"window_cost_sticky_reserve,omitempty"`

	// 会话数量控制（仅 Anthropic OAuth/SetupToken 账号有效）
	// 从 extra 字段提取，方便前端显示和编辑
	MaxSessions           *int `json:"max_sessions,omitempty"`
	SessionIdleTimeoutMin *int `json:"session_idle_timeout_minutes,omitempty"`

	// RPM 限制（仅 Anthropic OAuth/SetupToken 账号有效）
	// 从 extra 字段提取，方便前端显示和编辑
	BaseRPM          *int    `json:"base_rpm,omitempty"`
	RPMStrategy      *string `json:"rpm_strategy,omitempty"`
	RPMStickyBuffer  *int    `json:"rpm_sticky_buffer,omitempty"`
	UserMsgQueueMode *string `json:"user_msg_queue_mode,omitempty"`

	// TLS指纹伪装（仅 Anthropic OAuth/SetupToken 账号有效）
	// 从 extra 字段提取，方便前端显示和编辑
	EnableTLSFingerprint    *bool  `json:"enable_tls_fingerprint,omitempty"`
	TLSFingerprintProfileID *int64 `json:"tls_fingerprint_profile_id,omitempty"`

	// 会话ID伪装（仅 Anthropic OAuth/SetupToken 账号有效）
	// 启用后将在15分钟内固定 metadata.user_id 中的 session ID
	// 从 extra 字段提取，方便前端显示和编辑
	EnableSessionIDMasking *bool `json:"session_id_masking_enabled,omitempty"`

	// 缓存 TTL 强制替换（仅 Anthropic OAuth/SetupToken 账号有效）
	// 启用后将所有 cache creation tokens 归入指定的 TTL 类型计费
	CacheTTLOverrideEnabled *bool   `json:"cache_ttl_override_enabled,omitempty"`
	CacheTTLOverrideTarget  *string `json:"cache_ttl_override_target,omitempty"`

	// 自定义 Base URL 中继转发（仅 Anthropic OAuth/SetupToken 账号有效）
	CustomBaseURLEnabled *bool   `json:"custom_base_url_enabled,omitempty"`
	CustomBaseURL        *string `json:"custom_base_url,omitempty"`

	// API Key 账号配额限制
	QuotaLimit       *float64 `json:"quota_limit,omitempty"`
	QuotaUsed        *float64 `json:"quota_used,omitempty"`
	QuotaDailyLimit  *float64 `json:"quota_daily_limit,omitempty"`
	QuotaDailyUsed   *float64 `json:"quota_daily_used,omitempty"`
	QuotaWeeklyLimit *float64 `json:"quota_weekly_limit,omitempty"`
	QuotaWeeklyUsed  *float64 `json:"quota_weekly_used,omitempty"`

	// 配额固定时间重置配置
	QuotaDailyResetMode  *string `json:"quota_daily_reset_mode,omitempty"`
	QuotaDailyResetHour  *int    `json:"quota_daily_reset_hour,omitempty"`
	QuotaWeeklyResetMode *string `json:"quota_weekly_reset_mode,omitempty"`
	QuotaWeeklyResetDay  *int    `json:"quota_weekly_reset_day,omitempty"`
	QuotaWeeklyResetHour *int    `json:"quota_weekly_reset_hour,omitempty"`
	QuotaResetTimezone   *string `json:"quota_reset_timezone,omitempty"`
	QuotaDailyResetAt    *string `json:"quota_daily_reset_at,omitempty"`
	QuotaWeeklyResetAt   *string `json:"quota_weekly_reset_at,omitempty"`

	// 配额通知配置
	QuotaNotifyDailyEnabled    *bool    `json:"quota_notify_daily_enabled,omitempty"`
	QuotaNotifyDailyThreshold  *float64 `json:"quota_notify_daily_threshold,omitempty"`
	QuotaNotifyWeeklyEnabled   *bool    `json:"quota_notify_weekly_enabled,omitempty"`
	QuotaNotifyWeeklyThreshold *float64 `json:"quota_notify_weekly_threshold,omitempty"`
	QuotaNotifyTotalEnabled    *bool    `json:"quota_notify_total_enabled,omitempty"`
	QuotaNotifyTotalThreshold  *float64 `json:"quota_notify_total_threshold,omitempty"`

	// 影子账号关系（spark 维度影子）
	ParentAccountID *int64 `json:"parent_account_id,omitempty"`
	QuotaDimension  string `json:"quota_dimension,omitempty"`

	// QuotaKeyID 归属的 Cursor 激活码（quota key）ID（v3.3 §4.2 / C2，C5b 回填）。
	// 仅 cursor 平台账号有值；非 cursor 平台省略。账号详情禁止内嵌完整 QuotaKey
	// 聚合对象（F7），仅暴露脱敏 ID 与标签两个标量。
	QuotaKeyID *int64 `json:"quota_key_id,omitempty"`
	// QuotaKeyLabel 归属激活码的脱敏展示标签（值 = code_masked），不含任何原文。
	QuotaKeyLabel *string `json:"quota_key_label,omitempty"`

	// 影子账号回填的母账号信息（仅影子非空，源自母账号 Credentials/Extra）
	ParentEmail                 string `json:"parent_email,omitempty"`
	ParentPlanType              string `json:"parent_plan_type,omitempty"`
	ParentPrivacyMode           string `json:"parent_privacy_mode,omitempty"`
	ParentSubscriptionExpiresAt string `json:"parent_subscription_expires_at,omitempty"`
	ParentChatGPTAccountID      string `json:"parent_chatgpt_account_id,omitempty"`

	Proxy         *Proxy         `json:"proxy,omitempty"`
	AccountGroups []AccountGroup `json:"account_groups,omitempty"`

	GroupIDs []int64  `json:"group_ids,omitempty"`
	Groups   []*Group `json:"groups,omitempty"`

	// QuotaDimensions 规范化额度维度（方案 §3.4 / C5）：由 service 层同一 resolver
	// （service.ResolveAccountQuotaDimensions）输出的逐条投影，全 scope 原样序列化，
	// 前端不再自行解释原始 extra 键。账号无任何额度快照 → 省略字段（omitempty）。
	QuotaDimensions []AccountQuotaDimension `json:"quota_dimensions,omitempty"`
	// BalanceLow 服务端解释的余额不足标记（方案 §3.4「balance_low 出口」，非维度模型
	// 成员）：账号 extra 中 {platform}_balance_low 标记键为真。false/缺省 = 无标记。
	BalanceLow bool `json:"balance_low,omitempty"`
	// BalanceProbeCapable 后端选路事实源 RelayProbeTargetURL 的布尔投影（enabled 且
	// URL 可解析/可推导）；false/缺省省略。本字段为前端是否显示余额探测单元格的唯一
	// 能力源——配错账号（enabled 但无可解析 URL 且非官方端点）整格不出现。
	BalanceProbeCapable bool `json:"balance_probe_capable,omitempty"`
}

// AccountQuotaDimension 是 service.QuotaDimension 的 DTO 投影（方案 §3.4）。枚举以小写
// 字符串序列化（kind/scope/status/servable）；observed_at 为 RFC3339 字符串，来源无有效
// 采集时间时省略。backend 不在此层重新解释 extra，枚举映射仅在 dto 层做形态转换。
type AccountQuotaDimension struct {
	Kind       string `json:"kind"`
	Scope      string `json:"scope"`
	Target     string `json:"target"`
	Status     string `json:"status"`
	Servable   string `json:"servable"`
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at,omitempty"`
}

// w14：上游在本结构体的 OllamaCloudUsage 之后同样追加了 OpenCodeGoUsage
// （opencode_go_usage），裁决为「暂缓，跨卡依赖」，完整理由与收敛单元见本文件
// type Account 上方的 w14 注释块。
//
// AccountListItem is the compact representation returned by the admin account
// list when the lite query parameter is enabled. It contains the fields used
// by the account table and runtime indicators, but intentionally omits the
// repeated account_groups and groups object graphs. Fetch /admin/accounts/:id
// for the complete Account DTO when editing or inspecting an account.
type AccountListItem struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Notes    *string `json:"notes"`
	Platform string  `json:"platform"`
	Type     string  `json:"type"`

	Credentials       map[string]any                 `json:"credentials,omitempty"`
	CredentialsStatus map[string]bool                `json:"credentials_status,omitempty"`
	Extra             map[string]any                 `json:"extra,omitempty"`
	OllamaCloudUsage  *service.OllamaCloudUsageState `json:"ollama_cloud_usage,omitempty"`
	OpenCodeGoUsage   *service.OpenCodeGoUsageState  `json:"opencode_go_usage,omitempty"`

	ProxyID                 *int64     `json:"proxy_id"`
	ProxyFallbackOriginID   *int64     `json:"proxy_fallback_origin_id"`
	ProxyFallbackOriginName *string    `json:"proxy_fallback_origin_name,omitempty"`
	Concurrency             int        `json:"concurrency"`
	LoadFactor              *int       `json:"load_factor,omitempty"`
	Priority                int        `json:"priority"`
	RateMultiplier          float64    `json:"rate_multiplier"`
	Status                  string     `json:"status"`
	ErrorMessage            string     `json:"error_message"`
	LastUsedAt              *time.Time `json:"last_used_at"`
	ExpiresAt               *int64     `json:"expires_at"`
	AutoPauseOnExpired      bool       `json:"auto_pause_on_expired"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`

	Schedulable bool `json:"schedulable"`

	RateLimitedAt    *time.Time `json:"rate_limited_at"`
	RateLimitResetAt *time.Time `json:"rate_limit_reset_at"`
	OverloadUntil    *time.Time `json:"overload_until"`

	TempUnschedulableUntil  *time.Time `json:"temp_unschedulable_until"`
	TempUnschedulableReason string     `json:"temp_unschedulable_reason"`

	SessionWindowStart  *time.Time `json:"session_window_start"`
	SessionWindowEnd    *time.Time `json:"session_window_end"`
	SessionWindowStatus string     `json:"session_window_status"`

	WindowCostLimit         *float64 `json:"window_cost_limit,omitempty"`
	WindowCostStickyReserve *float64 `json:"window_cost_sticky_reserve,omitempty"`
	MaxSessions             *int     `json:"max_sessions,omitempty"`
	SessionIdleTimeoutMin   *int     `json:"session_idle_timeout_minutes,omitempty"`
	BaseRPM                 *int     `json:"base_rpm,omitempty"`
	RPMStrategy             *string  `json:"rpm_strategy,omitempty"`
	RPMStickyBuffer         *int     `json:"rpm_sticky_buffer,omitempty"`
	UserMsgQueueMode        *string  `json:"user_msg_queue_mode,omitempty"`
	EnableTLSFingerprint    *bool    `json:"enable_tls_fingerprint,omitempty"`
	TLSFingerprintProfileID *int64   `json:"tls_fingerprint_profile_id,omitempty"`
	EnableSessionIDMasking  *bool    `json:"session_id_masking_enabled,omitempty"`
	CacheTTLOverrideEnabled *bool    `json:"cache_ttl_override_enabled,omitempty"`
	CacheTTLOverrideTarget  *string  `json:"cache_ttl_override_target,omitempty"`
	CustomBaseURLEnabled    *bool    `json:"custom_base_url_enabled,omitempty"`
	CustomBaseURL           *string  `json:"custom_base_url,omitempty"`

	QuotaLimit       *float64 `json:"quota_limit,omitempty"`
	QuotaUsed        *float64 `json:"quota_used,omitempty"`
	QuotaDailyLimit  *float64 `json:"quota_daily_limit,omitempty"`
	QuotaDailyUsed   *float64 `json:"quota_daily_used,omitempty"`
	QuotaWeeklyLimit *float64 `json:"quota_weekly_limit,omitempty"`
	QuotaWeeklyUsed  *float64 `json:"quota_weekly_used,omitempty"`

	QuotaDailyResetMode  *string `json:"quota_daily_reset_mode,omitempty"`
	QuotaDailyResetHour  *int    `json:"quota_daily_reset_hour,omitempty"`
	QuotaWeeklyResetMode *string `json:"quota_weekly_reset_mode,omitempty"`
	QuotaWeeklyResetDay  *int    `json:"quota_weekly_reset_day,omitempty"`
	QuotaWeeklyResetHour *int    `json:"quota_weekly_reset_hour,omitempty"`
	QuotaResetTimezone   *string `json:"quota_reset_timezone,omitempty"`
	QuotaDailyResetAt    *string `json:"quota_daily_reset_at,omitempty"`
	QuotaWeeklyResetAt   *string `json:"quota_weekly_reset_at,omitempty"`

	QuotaNotifyDailyEnabled    *bool    `json:"quota_notify_daily_enabled,omitempty"`
	QuotaNotifyDailyThreshold  *float64 `json:"quota_notify_daily_threshold,omitempty"`
	QuotaNotifyWeeklyEnabled   *bool    `json:"quota_notify_weekly_enabled,omitempty"`
	QuotaNotifyWeeklyThreshold *float64 `json:"quota_notify_weekly_threshold,omitempty"`
	QuotaNotifyTotalEnabled    *bool    `json:"quota_notify_total_enabled,omitempty"`
	QuotaNotifyTotalThreshold  *float64 `json:"quota_notify_total_threshold,omitempty"`

	ParentAccountID *int64 `json:"parent_account_id,omitempty"`
	QuotaDimension  string `json:"quota_dimension,omitempty"`

	// QuotaKeyID 是账号归属的 Cursor 激活码（quota key）ID（v3.3 §4.2 / C2）。
	// 仅 cursor 平台账号必填；非 cursor 平台为 NULL（omitempty 省略）。
	// 注意：账号项禁止内嵌完整 QuotaKey 聚合对象（v3.3 F7），仅暴露脱敏 ID 与标签。
	QuotaKeyID *int64 `json:"quota_key_id,omitempty"`
	// QuotaKeyLabel 是归属激活码的脱敏展示标签（值 = code_masked，如 "XXXX-****-****-1234"），
	// 仅展示用，不含任何激活码原文；非 cursor / 未归属时省略。
	QuotaKeyLabel *string `json:"quota_key_label,omitempty"`

	// QuotaDimensions 规范化额度维度（与完整 Account DTO 同字段同语义，方案 §3.4 / C5）。
	// 完整 DTO 由 service 层同一 resolver 填充，lite 投影沿用该结果做形态对齐，不重新计算。
	QuotaDimensions []AccountQuotaDimension `json:"quota_dimensions,omitempty"`
	// BalanceLow 服务端解释的余额不足标记（与完整 Account DTO 同字段同语义）。false/缺省省略。
	BalanceLow bool `json:"balance_low,omitempty"`
	// BalanceProbeCapable 后端选路事实源 RelayProbeTargetURL 的布尔投影（与完整
	// Account DTO 同字段同语义）。false/缺省省略。
	BalanceProbeCapable         bool   `json:"balance_probe_capable,omitempty"`
	ParentEmail                 string `json:"parent_email,omitempty"`
	ParentPlanType              string `json:"parent_plan_type,omitempty"`
	ParentPrivacyMode           string `json:"parent_privacy_mode,omitempty"`
	ParentSubscriptionExpiresAt string `json:"parent_subscription_expires_at,omitempty"`
	ParentChatGPTAccountID      string `json:"parent_chatgpt_account_id,omitempty"`

	Proxy    *Proxy  `json:"proxy,omitempty"`
	GroupIDs []int64 `json:"group_ids,omitempty"`
}

// QuotaKey 是 Cursor 激活码（quota key）的展示型 DTO（v3.3 §4.5 / C2）。
//
// 安全约束（v3.3 F7 / C5 DTO 契约）：
//   - 绝不返回激活码原文（code_encrypted）；仅暴露脱敏标签 code_masked。
//   - 账号列表项（AccountListItem）禁止内嵌本聚合对象，仅暴露 QuotaKeyID /
//     QuotaKeyLabel 两个脱敏标量（见上）。
//   - 顶层列表响应（quota_key_summaries）在 cursor 平台账号
//     存在时才填充本汇总数组，供前端下拉/校验归属关系。
//
// 这是 C2 的 DTO 投影骨架；完整字段由 C5 的 QuotaKey DTO 契约最终定型，本结构
// 仅承载展示所需的最小安全子集。
type QuotaKey struct {
	ID           int64  `json:"id"`
	CodeMasked   string `json:"code_masked,omitempty"`
	Status       string `json:"status,omitempty"`
	Exhausted    bool   `json:"exhausted,omitempty"`
	AccountCount int64  `json:"account_count,omitempty"`
	WorkingCount int64  `json:"working_count,omitempty"`

	// 行级元数据（与详情端点内嵌的 service.QuotaKeyStatus 同名同值）。
	// 列表/详情/账号页汇总读同一份 service 投影，禁止各端点各自挑字段。
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	LastRefreshAt    *time.Time `json:"last_refresh_at,omitempty"`
	LastRefreshError string     `json:"last_refresh_error,omitempty"`
	Notes            *string    `json:"notes,omitempty"`
}

type AccountGroup struct {
	AccountID int64     `json:"account_id"`
	GroupID   int64     `json:"group_id"`
	Priority  int       `json:"priority"`
	CreatedAt time.Time `json:"created_at"`

	Account *Account `json:"account,omitempty"`
	Group   *Group   `json:"group,omitempty"`
}

type Proxy struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Protocol  string    `json:"protocol"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Username  string    `json:"username"`
	Password  string    `json:"-"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	ExpiresAt      *time.Time `json:"expires_at"`
	FallbackMode   string     `json:"fallback_mode"`
	BackupProxyID  *int64     `json:"backup_proxy_id"`
	ExpiryWarnDays int        `json:"expiry_warn_days"`
}

type ProxyWithAccountCount struct {
	Proxy
	AccountCount   int64  `json:"account_count"`
	LatencyMs      *int64 `json:"latency_ms,omitempty"`
	LatencyStatus  string `json:"latency_status,omitempty"`
	LatencyMessage string `json:"latency_message,omitempty"`
	IPAddress      string `json:"ip_address,omitempty"`
	Country        string `json:"country,omitempty"`
	CountryCode    string `json:"country_code,omitempty"`
	Region         string `json:"region,omitempty"`
	City           string `json:"city,omitempty"`
	QualityStatus  string `json:"quality_status,omitempty"`
	QualityScore   *int   `json:"quality_score,omitempty"`
	QualityGrade   string `json:"quality_grade,omitempty"`
	QualitySummary string `json:"quality_summary,omitempty"`
	QualityChecked *int64 `json:"quality_checked,omitempty"`
}

// AdminProxy 是管理员接口使用的 proxy DTO（包含密码等敏感字段）。
// 注意：普通接口不得使用此 DTO。
type AdminProxy struct {
	Proxy
	Password string `json:"password,omitempty"`
}

// AdminProxyWithAccountCount 是管理员接口使用的带账号统计的 proxy DTO。
type AdminProxyWithAccountCount struct {
	AdminProxy
	AccountCount   int64  `json:"account_count"`
	LatencyMs      *int64 `json:"latency_ms,omitempty"`
	LatencyStatus  string `json:"latency_status,omitempty"`
	LatencyMessage string `json:"latency_message,omitempty"`
	IPAddress      string `json:"ip_address,omitempty"`
	Country        string `json:"country,omitempty"`
	CountryCode    string `json:"country_code,omitempty"`
	Region         string `json:"region,omitempty"`
	City           string `json:"city,omitempty"`
	QualityStatus  string `json:"quality_status,omitempty"`
	QualityScore   *int   `json:"quality_score,omitempty"`
	QualityGrade   string `json:"quality_grade,omitempty"`
	QualitySummary string `json:"quality_summary,omitempty"`
	QualityChecked *int64 `json:"quality_checked,omitempty"`
}

type ProxyAccountSummary struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Platform string  `json:"platform"`
	Type     string  `json:"type"`
	Notes    *string `json:"notes,omitempty"`
}

type RedeemCode struct {
	ID        int64      `json:"id"`
	Code      string     `json:"code"`
	Type      string     `json:"type"`
	Value     float64    `json:"value"`
	Status    string     `json:"status"`
	UsedBy    *int64     `json:"used_by"`
	UsedAt    *time.Time `json:"used_at"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	GroupID      *int64 `json:"group_id"`
	ValidityDays int    `json:"validity_days"`

	// BatchID 仅 welfare 类型有值：所属福利批次。
	BatchID *int64 `json:"batch_id,omitempty"`
	// GroupGrants 仅 welfare 类型兑换成功后填充：分组权益清单（展示用）。
	GroupGrants []service.WelfareGroupGrant `json:"group_grants,omitempty"`

	// Notes is only populated for admin_balance/admin_concurrency types
	// so users can see why they were charged or credited
	Notes *string `json:"notes,omitempty"`

	User  *User  `json:"user,omitempty"`
	Group *Group `json:"group,omitempty"`
}

// AdminRedeemCode 是管理员接口使用的 redeem code DTO（包含 notes 等字段）。
// 注意：普通用户接口不得返回 notes 等内部信息。
type AdminRedeemCode struct {
	RedeemCode

	Notes string `json:"notes"`
}

type NullableTimeField struct {
	Set   bool
	Value *time.Time
}

func (f *NullableTimeField) UnmarshalJSON(data []byte) error {
	f.Set = true
	if bytes.Equal(data, []byte("null")) {
		f.Value = nil
		return nil
	}
	var value time.Time
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	f.Value = &value
	return nil
}

type NullableInt64Field struct {
	Set   bool
	Value *int64
}

func (f *NullableInt64Field) UnmarshalJSON(data []byte) error {
	f.Set = true
	if bytes.Equal(data, []byte("null")) {
		f.Value = nil
		return nil
	}
	var value int64
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	f.Value = &value
	return nil
}

type BatchUpdateRedeemCodeFields struct {
	Status    *string            `json:"status,omitempty"`
	ExpiresAt NullableTimeField  `json:"expires_at,omitempty"`
	Notes     *string            `json:"notes,omitempty"`
	GroupID   NullableInt64Field `json:"group_id,omitempty"`

	Type  *string  `json:"type,omitempty"`
	Value *float64 `json:"value,omitempty"`
}

type BatchUpdateRedeemCodesRequest struct {
	IDs    []int64                     `json:"ids" binding:"required,min=1"`
	Fields BatchUpdateRedeemCodeFields `json:"fields" binding:"required"`
}

// UsageLog 是普通用户接口使用的 usage log DTO（不包含管理员字段）。
type UsageLog struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	APIKeyID  int64  `json:"api_key_id"`
	AccountID int64  `json:"account_id"`
	RequestID string `json:"request_id"`
	Model     string `json:"model"`
	// ServiceTier records the OpenAI service tier used for billing, e.g. "priority" / "flex".
	ServiceTier *string `json:"service_tier,omitempty"`
	// ReasoningEffort is the client-requested effort (mapping-hidden, like Model).
	// OpenAI: "low"/"medium"/"high"/"xhigh"; Claude: "low"/"medium"/"high"/"max".
	// Historical rows without requested_reasoning_effort fall back to the stored effective value.
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	// InboundEndpoint is the client-facing API endpoint path, e.g. /v1/chat/completions.
	InboundEndpoint *string `json:"inbound_endpoint,omitempty"`
	// UpstreamEndpoint is the normalized upstream endpoint path, e.g. /v1/responses.
	UpstreamEndpoint *string `json:"upstream_endpoint,omitempty"`

	GroupID        *int64 `json:"group_id"`
	SubscriptionID *int64 `json:"subscription_id"`

	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`

	CacheCreation5mTokens int `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens int `json:"cache_creation_1h_tokens"`

	InputCost                 float64 `json:"input_cost"`
	OutputCost                float64 `json:"output_cost"`
	CacheCreationCost         float64 `json:"cache_creation_cost"`
	CacheReadCost             float64 `json:"cache_read_cost"`
	TotalCost                 float64 `json:"total_cost"`
	ActualCost                float64 `json:"actual_cost"`
	RateMultiplier            float64 `json:"rate_multiplier"`
	LongContextBillingApplied bool    `json:"long_context_billing_applied"`

	BillingType  int8   `json:"billing_type"`
	RequestType  string `json:"request_type"`
	Stream       bool   `json:"stream"`
	OpenAIWSMode bool   `json:"openai_ws_mode"`
	// NativeCompactionV2 is true only for requests positively identified at
	// runtime as the native OpenAI remote compaction v2 wire.
	NativeCompactionV2 bool `json:"native_compaction_v2"`
	DurationMs         *int `json:"duration_ms"`
	FirstTokenMs       *int `json:"first_token_ms"`

	// 图片生成字段
	ImageCount         int            `json:"image_count"`
	ImageSize          *string        `json:"image_size"`
	ImageInputSize     *string        `json:"image_input_size"`
	ImageOutputSize    *string        `json:"image_output_size"`
	ImageInputTokens   int            `json:"image_input_tokens"`
	ImageInputCost     float64        `json:"image_input_cost"`
	ImageOutputTokens  int            `json:"image_output_tokens"`
	ImageOutputCost    float64        `json:"image_output_cost"`
	ImageSizeSource    *string        `json:"image_size_source"`
	ImageSizeBreakdown map[string]int `json:"image_size_breakdown"`
	MediaType          *string        `json:"media_type"`

	// User-Agent
	UserAgent *string `json:"user_agent"`
	// IPAddress is visible to the owner of the usage record.
	IPAddress *string `json:"ip_address,omitempty"`
	// SessionID is the explicit client-provided request correlation identifier
	// (e.g. the session_id / X-Session-Id headers). Omitted when absent.
	SessionID *string `json:"session_id,omitempty"`

	// Cache TTL Override 标记
	CacheTTLOverridden bool `json:"cache_ttl_overridden"`

	// BillingMode 计费模式：token/image
	BillingMode *string `json:"billing_mode,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	User         *User             `json:"user,omitempty"`
	APIKey       *APIKey           `json:"api_key,omitempty"`
	Group        *Group            `json:"group,omitempty"`
	Subscription *UserSubscription `json:"subscription,omitempty"`
}

// AdminUsageLog 是管理员接口使用的 usage log DTO（包含管理员字段）。
type AdminUsageLog struct {
	UsageLog

	// UpstreamModel is the actual model sent to the upstream provider after mapping.
	// Omitted when no mapping was applied (requested model was used as-is).
	UpstreamModel *string `json:"upstream_model,omitempty"`
	// UpstreamReasoningEffort is the effort actually forwarded after group policy /
	// model-family remapping. Omitted when it matches the client-requested value.
	UpstreamReasoningEffort *string `json:"upstream_reasoning_effort,omitempty"`
	// UpstreamResponseModel is the raw model declared by the upstream response.
	UpstreamResponseModel *string `json:"upstream_response_model,omitempty"`
	// UpstreamModelMismatch is nil when the upstream did not declare a model.
	UpstreamModelMismatch *bool `json:"upstream_model_mismatch,omitempty"`

	// ChannelID 渠道 ID
	ChannelID *int64 `json:"channel_id,omitempty"`
	// ModelMappingChain 模型映射链，如 "a→b→c"
	ModelMappingChain *string `json:"model_mapping_chain,omitempty"`
	// UpstreamRequestID 是直接上游声明的请求标识，仅管理端可见。
	UpstreamRequestID *string `json:"upstream_request_id,omitempty"`
	// BillingTier 计费层级标签（per_request/image 模式）
	BillingTier *string `json:"billing_tier,omitempty"`

	// AccountRateMultiplier 账号计费倍率快照（nil 表示按 1.0 处理）
	AccountRateMultiplier *float64 `json:"account_rate_multiplier"`
	// AccountStatsCost 自定义定价规则计算的账号统计费用（nil 表示使用默认公式）
	AccountStatsCost *float64 `json:"account_stats_cost,omitempty"`

	// IPAddress 用户请求 IP
	IPAddress *string `json:"ip_address,omitempty"`

	// Account 最小账号信息（避免泄露敏感字段）
	Account *AccountSummary `json:"account,omitempty"`
}

type UsageCleanupFilters struct {
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time"`
	UserID      *int64    `json:"user_id,omitempty"`
	APIKeyID    *int64    `json:"api_key_id,omitempty"`
	AccountID   *int64    `json:"account_id,omitempty"`
	GroupID     *int64    `json:"group_id,omitempty"`
	Model       *string   `json:"model,omitempty"`
	RequestType *string   `json:"request_type,omitempty"`
	Stream      *bool     `json:"stream,omitempty"`
	BillingType *int8     `json:"billing_type,omitempty"`
}

type UsageCleanupTask struct {
	ID           int64               `json:"id"`
	Status       string              `json:"status"`
	Filters      UsageCleanupFilters `json:"filters"`
	CreatedBy    int64               `json:"created_by"`
	DeletedRows  int64               `json:"deleted_rows"`
	ErrorMessage *string             `json:"error_message,omitempty"`
	CanceledBy   *int64              `json:"canceled_by,omitempty"`
	CanceledAt   *time.Time          `json:"canceled_at,omitempty"`
	StartedAt    *time.Time          `json:"started_at,omitempty"`
	FinishedAt   *time.Time          `json:"finished_at,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	UpdatedAt    time.Time           `json:"updated_at"`
}

// AccountSummary is a minimal account info for usage log display.
// It intentionally excludes sensitive fields like Credentials, Proxy, etc.
type AccountSummary struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Setting struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

type UserSubscription struct {
	ID      int64 `json:"id"`
	UserID  int64 `json:"user_id"`
	GroupID int64 `json:"group_id"`

	StartsAt  time.Time `json:"starts_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Status    string    `json:"status"`

	DailyWindowStart   *time.Time `json:"daily_window_start"`
	WeeklyWindowStart  *time.Time `json:"weekly_window_start"`
	MonthlyWindowStart *time.Time `json:"monthly_window_start"`

	DailyUsageUSD   float64 `json:"daily_usage_usd"`
	WeeklyUsageUSD  float64 `json:"weekly_usage_usd"`
	MonthlyUsageUSD float64 `json:"monthly_usage_usd"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`

	User  *User  `json:"user,omitempty"`
	Group *Group `json:"group,omitempty"`
}

// AdminUserSubscription 是管理员接口使用的订阅 DTO（包含分配信息/备注等字段）。
// 注意：普通用户接口不得返回 assigned_by/assigned_at/notes/assigned_by_user 等管理员字段。
type AdminUserSubscription struct {
	UserSubscription

	AssignedBy *int64    `json:"assigned_by"`
	AssignedAt time.Time `json:"assigned_at"`
	Notes      string    `json:"notes"`

	AssignedByUser *User `json:"assigned_by_user,omitempty"`
}

type BulkAssignResult struct {
	SuccessCount  int                     `json:"success_count"`
	CreatedCount  int                     `json:"created_count"`
	ReusedCount   int                     `json:"reused_count"`
	FailedCount   int                     `json:"failed_count"`
	Subscriptions []AdminUserSubscription `json:"subscriptions"`
	Errors        []string                `json:"errors"`
	Statuses      map[string]string       `json:"statuses,omitempty"`
}

// PromoCode 注册优惠码
type PromoCode struct {
	ID          int64      `json:"id"`
	Code        string     `json:"code"`
	BonusAmount float64    `json:"bonus_amount"`
	MaxUses     int        `json:"max_uses"`
	UsedCount   int        `json:"used_count"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Notes       string     `json:"notes"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// PromoCodeUsage 优惠码使用记录
type PromoCodeUsage struct {
	ID          int64     `json:"id"`
	PromoCodeID int64     `json:"promo_code_id"`
	UserID      int64     `json:"user_id"`
	BonusAmount float64   `json:"bonus_amount"`
	UsedAt      time.Time `json:"used_at"`

	User *User `json:"user,omitempty"`
}
