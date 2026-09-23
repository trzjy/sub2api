package errors

// 下游可见错误文案常量（中文）。
//
// SSOT：docs/downstream-error-zh-localization-plan.md §3 文案对照表。
// 规则：所有写回下游的错误 message 一律引用本文件常量，禁止在业务代码里
// 写裸文案字符串；六处上游状态映射、计费哨兵、并发文案、ops 分类 Contains
// 均引用此处。文案原则：写明责任归属（上游渠道 / 账号池 / 用户自身）+ 状态 + 动作。
//
// 契约红线：HTTP 状态码、error type/code、infraerrors 稳定 code 一律不受本文件影响，
// 本文件只承载 message 文本。
const (
	// 上游渠道侧（六处上游状态映射共用：gateway_handler / openai_gateway_handler /
	// gemini_v1beta_handler 的 mapUpstreamError，及 service 层孪生映射）。
	UpstreamRateLimited     = "上游渠道触发限速，请稍后重试"
	UpstreamOverloaded      = "上游服务过载，请稍后重试"
	UpstreamUnavailable     = "上游服务暂时不可用，请稍后重试"
	UpstreamRequestFailed   = "上游请求失败，请稍后重试"
	UpstreamAuthFailed      = "上游渠道认证失败，请联系管理员"
	UpstreamForbidden       = "上游渠道拒绝访问，请联系管理员"
	UpstreamPaymentRequired = "上游渠道余额不足或计费异常，请联系管理员"

	// 账号池侧（调度/选号失败）。
	AllAccountsRateLimited           = "当前分组所有账号均处于上游限速状态，请稍后重试"
	AllAccountsExhausted             = "该分组所有账号均请求失败，请稍后重试"
	NoAvailableAccounts              = "该分组暂无可用账号，请稍后重试"
	NoAvailableAccountsProfitControl = "该分组暂无可用账号，请联系管理员"
	// NoAvailableAccountsCompact 为 /responses/compact 不被账号池支持的专属文案
	//（compact_not_supported 语义，非账号池耗尽），与 NoAvailableAccounts 同锚点不同场景。
	NoAvailableAccountsCompact = "暂无可用账号支持 /responses/compact 请求"

	// 账号槽并发（Gemini 账号槽等待出口；ConcurrencyError.IsTimeout 区分两态）。
	AccountSlotWaitTimeout      = "上游账号并发等待超时，请稍后重试"
	AccountSlotConcurrencyLimit = "上游账号并发已达上限，请稍后重试"

	// 用户侧（计费/限流；哨兵错误稳定 code 不变，仅换 message）。
	InsufficientBalance       = "账户余额不足，请充值后重试"
	SubscriptionInvalid       = "订阅已失效或过期，请续费后重试"
	BillingServiceUnavailable = "计费服务暂时不可用，请稍后重试"
	GroupRPMExceeded          = "当前分组请求频率已达每分钟上限，请稍后重试"
	UserRPMExceeded           = "您的账号请求频率已达每分钟上限，请稍后重试"
	PlatformDailyQuota        = "该平台今日用量配额已用完，请稍后重试或联系管理员提升配额"
	PlatformWeeklyQuota       = "该平台本周用量配额已用完，请稍后重试或联系管理员提升配额"
	PlatformMonthlyQuota      = "该平台本月用量配额已用完，请稍后重试或联系管理员提升配额"

	// 并发/排队。
	GatewayQueueFull    = "当前排队请求已满，请稍后重试"
	ConcurrencyFallback = "服务暂时不可用，请稍后重试"
	// ServiceTemporarilyUnavailable 为平台服务依赖缺失（如计费/账号服务未装配）的
	// 503 兜底文案，与 ConcurrencyFallback 同文本、不同语义锚点（见方案 §9 补录）。
	ServiceTemporarilyUnavailable = "服务暂时不可用，请稍后重试"
	// ConcurrencyDefaultTemplate 为 concurrency_limit_message 设置默认值。
	// {scope} 渲染为「用户/订阅」，{limit} 渲染为触发超限的具体上限数字——
	// ConcurrencyError 构造链保证 Limit 恒 >0（方案 §2.3 核实链），占位符恒有值。
	ConcurrencyDefaultTemplate = "您的{scope}并发请求已达上限（{limit}），请降低并发或稍后重试"

	// 请求侧/配置边界（调用方用模型名做 Sprintf）。
	ModelNotInGroup = "模型 %s 不在当前分组支持范围内，请检查模型名称或联系管理员"

	// WebSearchFailed 为网络搜索后端（grok native search 等）全链路失败后的下游占位文案，
	// 替代原先直接拼接上游 err.Error() 的内部错误明细（裁项 2 全仓排查发现的一处泄漏）。
	WebSearchFailed = "网络搜索失败，请稍后重试"

	// WS 下游关闭原因（openai_ws_v2 透传；status code / code 字段不变，仅 reason 文本）。
	// 沿用「写明责任归属 + 状态 + 动作」风格，并带重连指导后缀，与上游渠道侧语义对齐。
	UpstreamRateLimitedWS             = "上游渠道触发限速，请重新连接"
	UpstreamNoSemanticOutputWS        = "上游未产出有效内容，请重新连接"
	UpstreamWSReadTimeout             = "上游连接读取超时，请重新连接"
	UpstreamWSProxyFailed             = "上游连接代理失败，请重新连接"
	UpstreamWSConnectTimeout          = "上游连接建立超时，请重新连接"
	UpstreamWSHandshakeRejected       = "上游连接握手被拒绝，请重新连接"
	UpstreamWSContinuationUnavailable = "上游连接不可用，请重新连接"
)
