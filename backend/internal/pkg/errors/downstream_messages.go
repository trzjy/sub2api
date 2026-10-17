package errors

// 下游可见错误文案常量（中文）。
//
// SSOT：docs-local/downstream-error-zh-localization-plan.md §3 文案对照表。
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
	// ConcurrencyLimitReachedNoLimit 为并发上限触发但上限值拿不到（含不限 0，已核实不可达）
	// 时的防御性回退文案：不含 {limit} 占位符，避免向用户暴露裸占位符。与 ConcurrencyDefaultTemplate
	// 同语义锚点（user/group 可配文案缺失场景），仅用于 limit<=0 回退。
	ConcurrencyLimitReachedNoLimit = "您的并发请求已达上限，请稍后重试"
	// ImageConcurrencyLimitReached: 图片生成并发上限固定文案（429 rate_limit_error；稳定 code gatewayConcurrencyLimitCode 不变）。
	ImageConcurrencyLimitReached = "图片生成并发已达上限，请稍后重试"

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

	// ===== 二期扩展（2026-10-18 用户批准，方案：docs-local/downstream-error-zh-localization-phase2-plan.md）=====
	// 措辞总规则：写明【谁的问题 + 什么状态 + 怎么办】；凡暴露上游真身/账号类型/
	// 内部分组术语/账号池架构的，按泛化措辞（§0.5 防泄露裁定），不直译。

	// 认证。
	InvalidAPIKey            = "无效的 API 密钥"
	APIKeyGroupRequired      = "该 API 密钥必须绑定分组"
	APIKeyNotAssignedToGroup = "API 密钥未分配到分组，请联系管理员"
	// APIKeyMissing: gateway_web_search.go 请求未携带 API 密钥（401 authentication_error 固定文案）。
	APIKeyMissing = "缺少 API 密钥"

	// 用户侧（计费/限额规整 + 兜底）。
	APIKeyRate5hExceeded = "API 密钥 5 小时限额已用完"
	APIKeyRate1dExceeded = "API 密钥每日限额已用完"
	APIKeyRate7dExceeded = "API 密钥 7 天限额已用完"
	BillingErrorFallback = "计费错误"

	// WS 连接管理（JSON 429 与客户端关闭 reason）。
	WSConnectionsTooMany  = "WebSocket 连接数已达上限，请稍后重试"
	WSIngressCapacityLost = "网关接入容量租约已失效，请重新连接"
	WSAccountNotEligible  = "该账号不再适用于此连接，请重新连接"
	// UpstreamWebSocketPrewarmFailed: openai WS 预热错误事件且上游未给出错误消息时的回退文案。
	UpstreamWebSocketPrewarmFailed = "上游 WebSocket 预热失败"
	// UpstreamWebSocketError: openai WS v2 错误事件且上游未给出错误消息时的回退文案。
	UpstreamWebSocketError = "上游 WebSocket 连接错误"

	// 安全审计兜底。
	SecurityAuditBlockedFallback = "请求被内容策略拦截"

	// 上游特判泛化（防泄露：不出现厂商名/账号类型/账号池架构）。
	UpstreamPayloadTooLarge   = "请求体过大"
	UpstreamAccessUnavailable = "上游访问暂时不可用，请稍后重试"
	UpstreamEmptyCompletion   = "上游返回了空回复，请稍后重试"
	// UpstreamCyberPolicyBlocked: 上游网络安全策略拦截的客户端回退文案（cyber 致命不可重试，400 invalid_request_error）。
	UpstreamCyberPolicyBlocked = "请求被上游网络安全策略拦截"
	// ResourceNotFound: gemini compat 404 not_found_error 固定文案（Resource not found 裸串补漏）。
	ResourceNotFound = "资源不存在"
	// UpstreamCompactResponseFailed: 上游 compact/SSE 终态失败且无错误消息时的回退文案（502 upstream_error）。
	UpstreamCompactResponseFailed = "上游 compact 响应失败，请稍后重试"

	// Responses 续传（泛化：不暴露 API-key vs 订阅账号架构）。
	PreviousResponseIDUnsupported = "previous_response_id 暂不支持当前请求方式，请检查账号配置"

	// 平台能力/分组（泛化：不暴露 TypeSafe 内部术语）。
	SystemOneGroupRestricted = "该请求类型仅支持特定分组配置，请联系管理员"

	// 模型目录服务（泛化：不暴露内部配置概念）。
	ModelDiscoveryUnavailable = "模型列表服务暂不可用，请联系管理员"

	// 请求校验（400 invalid_request_error 固定文案；err.Error() 详情直传点除外）。
	RequestBodyReadFailed             = "读取请求体失败"
	RequestBodyEmpty                  = "请求体为空"
	RequestBodyParseFailed            = "请求体解析失败"
	RequestBodyTooLargeFormat         = "请求体过大，上限为 %s"
	ModelRequired                     = "缺少 model 字段"
	ModelNotSupportedByComposite      = "该模型不被复合分组支持"
	ModelNotSupportedOnChatCompletio  = "该模型不支持 Chat Completions 端点"
	ReasoningEffortPolicyFailed       = "应用推理力度策略失败"
	CompactNormalizeFailed            = "规范化 compact 请求体失败"
	PreviousResponseIDBadFormat       = "previous_response_id 必须是 response.id（resp_ 开头），不能是消息 id"
	PreviousResponseIDUnavailable     = "该用户不可使用 previous_response_id"
	FunctionCallOutputNeedsCallID     = "HTTP 请求上 function_call_output 必须携带 call_id；经 previous_response_id 续传仅 Responses WebSocket v2 支持"
	FunctionCallOutputItemRefMismatch = "function_call_output 的 item_reference id 必须与每个 call_id 一致；经 previous_response_id 续传仅 Responses WebSocket v2 支持"
	RequestIDRequired                 = "缺少 request_id"
	InvalidDaysRange                  = "days 参数无效，允许范围 1-90"
	TierRoutingEstimateFailed         = "分层路由估价失败"
	WebSocketUpgradeRequired          = "需要 WebSocket 升级请求（Upgrade: websocket）"
	SeedanceRequiresJSON              = "Seedance 要求 Content-Type 为 application/json"
	SeedanceRequiresGroup             = "Seedance 需要 OpenAI 或复合分组"
	StreamingImageAsyncUnsupported    = "流式图片请求不能提交为异步任务"
	InvalidStreamFieldType            = "流字段类型无效"
	ImageGenerationNotEnabled         = "该分组未启用图片生成"
	// QueryRequired: gateway_web_search.go 请求缺少 query/input 字段（400 invalid_request_error 固定文案）。
	QueryRequired = "缺少 query 字段"
	// SearchGrokGroupOnly: gateway_web_search.go 搜索端点仅支持 Grok 分组（400 invalid_request_error 固定文案）。
	SearchGrokGroupOnly = "该搜索端点仅支持 Grok 分组"
	// CodexInstructionsRequired: codex passthrough 请求缺少 instructions 字段（403 forbidden_error 固定文案；防泄露泛化不提上游真身）。
	CodexInstructionsRequired = "该请求缺少必需的 instructions 字段"

	// 平台能力/端点不支持（404/403 固定文案；Live/Seedance/Codex 为产品功能名，保留）。
	RealtimeAPINotSupported          = "该平台不支持 Realtime API"
	VoiceAPINotSupported             = "该平台不支持语音 API"
	LiveNotSupportedForPlatform      = "该平台不支持 Live"
	LiveNotEnabledForGroup           = "该分组未启用 Live"
	LiveOnlyOpenAIModels             = "复合分组的 Live 仅支持 OpenAI 模型"
	LiveCallNotFound                 = "Live 通话不存在"
	LiveCallOwnershipDenied          = "该 Live 通话属于其他身份"
	LiveConcurrencyLimitReached      = "Live 并发已达上限，请稍后重试"
	LiveConcurrencyUnavailable       = "Live 并发槽暂不可用，请稍后重试"
	LiveUpstreamRequestFailed        = "Live 上游请求失败，请稍后重试"
	LiveUnavailable                  = "Live 暂不可用，请稍后重试"
	ImagesAPINotSupported            = "该平台不支持 Images API"
	VideoRequestNotFound             = "视频请求不存在"
	AsyncImageTasksDisabled          = "异步图片任务未开启"
	CodexManifestGroupRestricted     = "Codex 模型清单仅支持 OpenAI 与复合分组"
	CodexAlphaGroupRestricted        = "Codex alpha 搜索仅支持 OpenAI 与复合分组"
	CodexAlphaModelRestricted        = "复合分组的 Codex alpha 搜索仅支持 OpenAI 模型"
	BillingInfoSimpleModeUnsupported = "简易模式下不支持账单信息"

	// 内部错误（500/502 固定文案；联系管理员指运营侧排查）。
	UserContextNotFound        = "用户上下文缺失，请联系管理员"
	GetUserInfoFailed          = "获取用户信息失败，请联系管理员"
	InternalServerErrorGeneric = "服务器内部错误，请稍后重试"
	BillingInfoUnavailable     = "账单信息不可用，请稍后重试"
	ImageGatewayUnavailable    = "图片网关暂不可用，请稍后重试"
	ModelMappingApplyFailed    = "应用模型映射失败，请联系管理员"
	CompositeModelRouteFailed  = "应用复合模型路由失败，请联系管理员"
	CodexManifestBuildFailed   = "构建 Codex 模型清单失败，请稍后重试"
	ModelCatalogueEncodeFailed = "模型目录编码失败，请联系管理员"
	ModelCatalogueInvalid      = "模型目录无效，请联系管理员"

	// 兼容服务/特判上游（泛化：不出现厂商名 TypeSafe/Grok/Antigravity/Gemini——
	// 2026-10-18 §0.5 防泄露裁定，执行期新增发现的同类泄露文案同规则处理）。
	CompatServiceNotConfigured  = "上游兼容服务未配置，请联系管理员"
	CompatUpstreamRequestFailed = "上游请求失败，请稍后重试"
	RealtimeUpstreamUnavailable = "实时上游暂不可用，请稍后重试"

	// 以下两处为执行期发现、派发单未列对应常量的同类泄露/固定文案：
	// 按 §防泄露裁定与 §2「Live 族全部固定文案→常量」最小新增，仅替换 message 文本。
	// UpstreamResponseTooLarge: gateway_systemone.go 上游响应超网关大小限制（原串泄露 TypeSafe）。
	UpstreamResponseTooLarge = "上游响应超出网关大小限制"
	// LiveUpstreamRejected: openai_live.go Live 上游拒绝请求（4xx invalid_request_error 固定文案）。
	LiveUpstreamRejected = "Live 上游拒绝了该请求，请检查请求参数"
	// CompatTokenProviderNotConfigured: antigravity_gateway_compat.go token provider 未装配（原串泄露 Antigravity）。
	CompatTokenProviderNotConfigured = "上游令牌提供方未配置，请联系管理员"

	// 上游解析/断连/请求校验族（二期补漏：service 层 antigravity/gemini/openai compat 多处；
	// 仅替换 message 文本，状态码/type/reason 不变）。
	// InvalidRequestBody: antigravity/gemini compat 请求体无效（400 invalid_request_error）。
	InvalidRequestBody = "请求体无效"
	// InvalidRequest: antigravity/gemini compat 无效的请求（400 invalid_request_error）。
	InvalidRequest = "无效的请求"
	// ClientDisconnectedBeforeResponse: 客户端在响应前断开连接（client_disconnected，499）。
	ClientDisconnectedBeforeResponse = "客户端在响应前断开连接"
	// UpstreamResponseParseFailed: 上游响应解析失败（502 upstream_error）。
	UpstreamResponseParseFailed = "上游响应解析失败"
	// UpstreamStreamIntervalTimeout: 上游流式数据间隔超时（502 upstream_timeout）。
	UpstreamStreamIntervalTimeout = "上游流式数据间隔超时"
	// UpstreamResponseLineTooLong: 上游响应行过长（502 response_too_large）。
	UpstreamResponseLineTooLong = "上游响应行过长"
)
