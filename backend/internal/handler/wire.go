package handler

import (
	"database/sql"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"

	"github.com/google/wire"
)

// ProvideAdminHandlers creates the AdminHandlers struct
func ProvideAdminHandlers(
	dashboardHandler *admin.DashboardHandler,
	userHandler *admin.UserHandler,
	groupHandler *admin.GroupHandler,
	accountHandler *admin.AccountHandler,
	announcementHandler *admin.AnnouncementHandler,
	dataManagementHandler *admin.DataManagementHandler,
	backupHandler *admin.BackupHandler,
	oauthHandler *admin.OAuthHandler,
	openaiOAuthHandler *admin.OpenAIOAuthHandler,
	geminiOAuthHandler *admin.GeminiOAuthHandler,
	antigravityOAuthHandler *admin.AntigravityOAuthHandler,
	grokOAuthHandler *admin.GrokOAuthHandler,
	codeBuddyOAuthHandler *admin.CodeBuddyOAuthHandler,
	cnProviderHandler *admin.CNProviderHandler,
	proxyHandler *admin.ProxyHandler,
	redeemHandler *admin.RedeemHandler,
	promoHandler *admin.PromoHandler,
	settingHandler *admin.SettingHandler,
	opsHandler *admin.OpsHandler,
	systemHandler *admin.SystemHandler,
	subscriptionHandler *admin.SubscriptionHandler,
	usageHandler *admin.UsageHandler,
	userAttributeHandler *admin.UserAttributeHandler,
	errorPassthroughHandler *admin.ErrorPassthroughHandler,
	tlsFingerprintProfileHandler *admin.TLSFingerprintProfileHandler,
	pluginHandler *admin.PluginHandler,
	apiKeyHandler *admin.AdminAPIKeyHandler,
	scheduledTestHandler *admin.ScheduledTestHandler,
	channelHandler *admin.ChannelHandler,
	channelMonitorHandler *admin.ChannelMonitorHandler,
	promoIntelHandler *admin.PromoIntelHandler,
	channelMonitorTemplateHandler *admin.ChannelMonitorRequestTemplateHandler,
	contentModerationHandler *admin.ContentModerationHandler,
	promptAuditHandler *securityaudit.PromptAdminHandler,
	paymentHandler *admin.PaymentHandler,
	affiliateHandler *admin.AffiliateHandler,
	complianceHandler *admin.ComplianceHandler,
	auditLogHandler *admin.AuditLogHandler,
	xianyuHandler *admin.XianyuAdminHandler,
	pricingHandler *admin.PricingHandler,
	upstreamBillingProbe *service.UpstreamBillingProbeService,
	ollamaCloudUsage *service.OllamaCloudUsageService,
	accountBalanceProbe *service.AccountBalanceProbeService,
	adminService service.AdminService,
	httpUpstream service.HTTPUpstream,
	cfg *config.Config,
	webPlatformAutoLogin *service.WebPlatformAutoLoginService,
	usageRiskService service.UsageRiskService,
	settingService *service.SettingService,
	visionCapabilityHandler *admin.VisionCapabilityHandler,
	visionRoutingService *service.VisionRoutingService,
	entClient *dbent.Client,
	xgjAdminCfgStore admin.XianguanjiaConfigStore, // D3: 闲管家 admin 配置存储（252 表写侧）
	secretEncryptor service.SecretEncryptor, // D3: 凭证加密器（仓库既有 provider）
	db *sql.DB, // D4i: redeem_codes marker 需要 sql 句柄（与 xianguanjiaVoider 同源）
	settingRepo service.SettingRepository, // D4i: kind_id settings 存取（wire_gen 已有 settingRepository）
	xgjSupplyStore *xianguanjia.SupplyConfigStore, // D6i: 货源模式凭证存储（D6a DB 实现，Save/Get）
) *AdminHandlers {
	accountHandler.SetUpstreamBillingProbeService(upstreamBillingProbe)
	accountHandler.SetOllamaCloudUsageService(ollamaCloudUsage)
	accountHandler.SetAccountBalanceProbeService(accountBalanceProbe)
	accountHandler.SetCodeBuddyAccountRefresher(codeBuddyOAuthHandler)
	// 注入异常调用分析服务：dashboard 卡片摘要 + admin 独立路由（派发单 U4b/U5）。
	dashboardHandler.SetUsageRiskService(usageRiskService)
	// 注入网页版平台自动登录服务（service 层 provider 构造，与 TokenRefreshService
	// 共享同一实例；其 Start 由 handler 内 lazy + idempotent 触发）。
	accountHandler.SetWebPlatformAutoLoginService(webPlatformAutoLogin)
	// 注入分组视觉分流配置服务，供 group create/update 链路做同组校验。
	groupHandler.SetVisionRoutingService(visionRoutingService)
	accountHandler.SetWebLoginCaptchaHelper(service.NewLocalCaptchaHelperHTTPClient(service.LocalCaptchaHelperConfig{
		BaseURL: cfg.LocalCaptchaHelper.BaseURL,
		APIKey:  cfg.LocalCaptchaHelper.APIKey,
		Timeout: cfg.LocalCaptchaHelper.Timeout,
	}))
	adminHandlers := &AdminHandlers{
		Dashboard:              dashboardHandler,
		User:                   userHandler,
		Group:                  groupHandler,
		Account:                accountHandler,
		Announcement:           announcementHandler,
		DataManagement:         dataManagementHandler,
		Backup:                 backupHandler,
		OAuth:                  oauthHandler,
		OpenAIOAuth:            openaiOAuthHandler,
		GeminiOAuth:            geminiOAuthHandler,
		AntigravityOAuth:       antigravityOAuthHandler,
		GrokOAuth:              grokOAuthHandler,
		CodeBuddyOAuth:         codeBuddyOAuthHandler,
		CNProvider:             cnProviderHandler,
		Proxy:                  proxyHandler,
		Redeem:                 redeemHandler,
		Promo:                  promoHandler,
		Setting:                settingHandler,
		Ops:                    opsHandler,
		System:                 systemHandler,
		Subscription:           subscriptionHandler,
		Usage:                  usageHandler,
		UserAttribute:          userAttributeHandler,
		ErrorPassthrough:       errorPassthroughHandler,
		TLSFingerprintProfile:  tlsFingerprintProfileHandler,
		Plugin:                 pluginHandler,
		APIKey:                 apiKeyHandler,
		ScheduledTest:          scheduledTestHandler,
		Channel:                channelHandler,
		ChannelMonitor:         channelMonitorHandler,
		PromoIntel:             promoIntelHandler,
		ChannelMonitorTemplate: channelMonitorTemplateHandler,
		ContentModeration:      contentModerationHandler,
		PromptAudit:            promptAuditHandler,
		Payment:                paymentHandler,
		Affiliate:              affiliateHandler,
		Compliance:             complianceHandler,
		AuditLog:               auditLogHandler,
		Xianyu:                 xianyuHandler,
		Pricing:                pricingHandler,
		UsageRisk:              admin.NewUsageRiskHandler(usageRiskService, settingService),
		VisionCapability:       visionCapabilityHandler,
	}
	// D3: 闲管家 admin 凭证配置入口——与 D1 的推送验签共享同一份 ConfigStore（252 表）
	// 与 secretEncryptor；探活客户端为 nil 时 handler 内部用默认 http.Client。
	// 本段为 D3 合并标记：冲突时保留此块。
	if xgjAdminCfgStore != nil {
		adminHandlers.XianguanjiaConfig = admin.NewXianguanjiaConfigHandler(xgjAdminCfgStore, secretEncryptor, nil)
	}

	// D4i: 闲管家 admin 卡种管理 + 批量推仓入口——接线完成。
	// kind 服务 = LazyKindService（每次调用经 ClientFactory 现场构造 client，
	// 委托 D4a KindService；kind_id 存 settings 表，经 SettingsKindIDStore 适配）；
	// pusher = LazyPoolSyncService（委托 D4b PoolSyncService，marker 复用 D4b
	// NewPushedCardMarker 的 unused→delivered 原子标记）；admin 签名桥接见
	// xianguanjia_pool_bridge.go。无 active 配置时端点 fail-closed 返回
	// ErrNoActiveConfig（不再是无条件 503）。
	// 本段为 D4i 合并标记：冲突时保留此块。
	xgjKindStore := xianguanjia.NewSettingsKindIDStore(settingRepo)
	// factory 的 cfgReader 需含 GetActiveConfig（只读侧），admin.XianguanjiaConfigStore
	// 是写侧窄接口不含该方法，故按 wire_gen 既有方式从 db 现场构造只读 store。
	xgjClientFactory := xianguanjia.NewClientFactory(xianguanjia.NewConfigStore(db), secretEncryptor.Decrypt)
	xgjPushMarker := xianguanjia.NewPushedCardMarker(db)
	adminHandlers.XianguanjiaPool = admin.NewXianguanjiaPoolHandler(
		xgjKindServiceFromLazy(xianguanjia.NewLazyKindService(xgjClientFactory, xgjKindStore)),
		newXgjPoolPushBridge(xianguanjia.NewLazyPoolSyncService(xgjClientFactory, xgjPushMarker)),
	)

	// D6e/D6i: 闲管家 admin 货源模式凭证配置入口——D6a DB 实现（supplyStore 参数）。
	// 本段为 D6e 合并标记：冲突时保留此块。
	if xgjSupplyStore != nil {
		adminHandlers.XianguanjiaSupply = admin.NewXianguanjiaSupplyHandler(xgjSupplyStore, "")
	}

	// Cockpit 备份导入 preview/commit（B1c）：HMAC 无状态凭证签名器由既有服务端密钥
	// cfg.JWT.Secret 经 HKDF 派生专用子密钥；提交仓储由 ent 客户端构造。
	cockpitSigner := service.NewCockpitPreviewReceiptSigner(cfg.JWT.Secret)
	cockpitCommitRepo := repository.NewCockpitImportCommitRepository(entClient)
	cockpitCommitSvc := service.NewCockpitImportCommitServiceWithReceipt(cockpitCommitRepo, cockpitSigner)
	adminHandlers.CockpitImport = admin.NewCockpitImportHandler(cockpitCommitSvc, cockpitSigner)
	return adminHandlers
}

func ProvideGatewayHandler(
	gatewayService *service.GatewayService,
	openAIGatewayService *service.OpenAIGatewayService,
	geminiCompatService *service.GeminiMessagesCompatService,
	antigravityGatewayService *service.AntigravityGatewayService,
	userService *service.UserService,
	concurrencyService *service.ConcurrencyService,
	billingCacheService *service.BillingCacheService,
	usageService *service.UsageService,
	apiKeyService *service.APIKeyService,
	usageRecordWorkerPool *service.UsageRecordWorkerPool,
	errorPassthroughService *service.ErrorPassthroughService,
	contentModerationService *service.ContentModerationService,
	userMsgQueueService *service.UserMessageQueueService,
	cfg *config.Config,
	settingService *service.SettingService,
	coordinator *securityaudit.Coordinator,
) *GatewayHandler {
	h := NewGatewayHandler(gatewayService, openAIGatewayService, geminiCompatService, antigravityGatewayService,
		userService, concurrencyService, billingCacheService, usageService, apiKeyService, usageRecordWorkerPool,
		errorPassthroughService, contentModerationService, userMsgQueueService, cfg, settingService)
	h.securityAuditCoordinator = coordinator
	return h
}

func ProvideOpenAIGatewayHandler(
	gatewayService *service.OpenAIGatewayService,
	coreGatewayService *service.GatewayService,
	pluginManager *service.PluginManager,
	settingService *service.SettingService,
	concurrencyService *service.ConcurrencyService,
	billingCacheService *service.BillingCacheService,
	apiKeyService *service.APIKeyService,
	usageRecordWorkerPool *service.UsageRecordWorkerPool,
	errorPassthroughService *service.ErrorPassthroughService,
	contentModerationService *service.ContentModerationService,
	opsService *service.OpsService,
	grokQuotaService *service.GrokQuotaService,
	cfg *config.Config,
	coordinator *securityaudit.Coordinator,
) *OpenAIGatewayHandler {
	gatewayService.SetPluginManager(pluginManager)
	h := NewOpenAIGatewayHandler(gatewayService, coreGatewayService, settingService, concurrencyService, billingCacheService, apiKeyService,
		usageRecordWorkerPool, errorPassthroughService, contentModerationService, opsService, cfg)
	h.securityAuditCoordinator = coordinator
	h.grokMediaEligibilityProber = grokQuotaService
	return h
}

func ProvideBatchImageHandler(
	batchService *service.BatchImagePublicService,
	download *service.BatchImageDownloadService,
	cleanup *service.BatchImageCleanupService,
	openAI *OpenAIGatewayHandler,
) *BatchImageHandler {
	h := NewBatchImageHandler(batchService, download, cleanup)
	h.openAI = openAI
	return h
}

// ProvideSystemHandler creates admin.SystemHandler with UpdateService
func ProvideSystemHandler(updateService *service.UpdateService, lockService *service.SystemOperationLockService) *admin.SystemHandler {
	return admin.NewSystemHandler(updateService, lockService)
}

// ProvideSettingHandler creates SettingHandler with version from BuildInfo
func ProvideSettingHandler(settingService *service.SettingService, buildInfo BuildInfo, notificationEmailService *service.NotificationEmailService) *SettingHandler {
	h := NewSettingHandler(settingService, buildInfo.Version)
	h.SetNotificationEmailService(notificationEmailService)
	return h
}

// ProvideAdminSettingHandler creates admin.SettingHandler with notification template APIs.
func ProvideAdminSettingHandler(settingService *service.SettingService, emailService *service.EmailService, turnstileService *service.TurnstileService, aliyunCaptchaService *service.AliyunCaptchaService, opsService *service.OpsService, paymentConfigService *service.PaymentConfigService, paymentService *service.PaymentService, userAttributeService *service.UserAttributeService, notificationEmailService *service.NotificationEmailService, totpService *service.TotpService, userService *service.UserService) *admin.SettingHandler {
	h := admin.NewSettingHandler(settingService, emailService, turnstileService, opsService, paymentConfigService, paymentService, userAttributeService)
	h.SetNotificationEmailService(notificationEmailService)
	h.SetAliyunCaptchaService(aliyunCaptchaService)
	h.SetStepUpDeps(totpService, userService)
	return h
}

// ProvideHandlers creates the Handlers struct
func ProvideHandlers(
	authHandler *AuthHandler,
	userHandler *UserHandler,
	apiKeyHandler *APIKeyHandler,
	usageHandler *UsageHandler,
	redeemHandler *RedeemHandler,
	subscriptionHandler *SubscriptionHandler,
	announcementHandler *AnnouncementHandler,
	channelMonitorUserHandler *ChannelMonitorUserHandler,
	channelMonitorV2Handler *ChannelMonitorV2Handler,
	adminHandlers *AdminHandlers,
	gatewayHandler *GatewayHandler,
	openaiGatewayHandler *OpenAIGatewayHandler,
	settingHandler *SettingHandler,
	totpHandler *TotpHandler,
	passkeyHandler *PasskeyHandler,
	paymentHandler *PaymentHandler,
	paymentWebhookHandler *PaymentWebhookHandler,
	availableChannelHandler *AvailableChannelHandler,
	modelPlazaHandler *ModelPlazaHandler,
	asyncImageHandler *AsyncImageHandler,
	batchImageHandler *BatchImageHandler,
	xianyuDeliveryHandler *XianyuDeliveryHandler,
	xianguanjiaPushHandler *XianyuXianguanjiaPushHandler,
	xgjSupplyHandler *XgjSupplyHandler, // D6e: 货源模式公开接口
	supplyCatalogHandler *XianguanjiaSupplyHandler, // D6i: D6c 货源目录接口
	_ *service.IdempotencyCoordinator,
	_ *service.IdempotencyCleanupService,
	_ *service.OpenAIQuotaAutoResetService,
) *Handlers {
	return &Handlers{
		Auth:             authHandler,
		User:             userHandler,
		APIKey:           apiKeyHandler,
		Usage:            usageHandler,
		Redeem:           redeemHandler,
		Subscription:     subscriptionHandler,
		Announcement:     announcementHandler,
		ChannelMonitor:   channelMonitorUserHandler,
		ChannelMonitorV2: channelMonitorV2Handler,
		Admin:            adminHandlers,
		Gateway:          gatewayHandler,
		OpenAIGateway:    openaiGatewayHandler,
		Setting:          settingHandler,
		Totp:             totpHandler,
		Passkey:          passkeyHandler,
		Payment:          paymentHandler,
		PaymentWebhook:   paymentWebhookHandler,
		AvailableChannel: availableChannelHandler,
		ModelPlaza:       modelPlazaHandler,
		AsyncImage:       asyncImageHandler,
		BatchImage:       batchImageHandler,
		XianyuDelivery:   xianyuDeliveryHandler,
		XianguanjiaPush:  xianguanjiaPushHandler,
		XgjSupply:        xgjSupplyHandler,
		SupplyCatalog:    supplyCatalogHandler,
	}
}

// ProviderSet is the Wire provider set for all handlers
var ProviderSet = wire.NewSet(
	// D3: 闲管家 admin 配置存储（252 表写侧）：导出的 NewConfigStore provider + 接口绑定。
	xianguanjia.ProvideConfigStore,
	wire.Bind(new(admin.XianguanjiaConfigStore), new(xianguanjia.ConfigStore)),
	// D6i: 货源模式凭证存储（D6a DB 实现，需 db 与 secretEncryptor——由 wire_gen 传参）。
	xianguanjia.NewSupplyConfigStore,
	// Top-level handlers
	NewAuthHandler,
	NewUserHandler,
	NewAPIKeyHandler,
	NewUsageHandler,
	NewRedeemHandler,
	NewSubscriptionHandler,
	NewAnnouncementHandler,
	NewChannelMonitorUserHandler,
	NewChannelMonitorV2Handler,
	ProvideGatewayHandler,
	ProvideOpenAIGatewayHandler,
	NewTotpHandler,
	NewPasskeyHandler,
	ProvideSettingHandler,
	NewPaymentHandler,
	NewPaymentWebhookHandler,
	NewAvailableChannelHandler,
	NewModelPlazaHandler,
	NewAsyncImageHandler,
	ProvideBatchImageHandler,
	NewXianyuDeliveryHandler,
	admin.NewXianyuAdminHandler,

	// Admin handlers
	admin.NewDashboardHandler,
	admin.NewUserHandler,
	admin.NewGroupHandlerWithConfig,
	admin.ProvideAccountHandler,
	admin.NewAnnouncementHandler,
	admin.NewDataManagementHandler,
	admin.NewBackupHandler,
	admin.NewOAuthHandler,
	admin.NewOpenAIOAuthHandler,
	admin.NewGeminiOAuthHandler,
	admin.NewAntigravityOAuthHandler,
	admin.NewGrokOAuthHandler,
	admin.NewCodeBuddyOAuthHandler,
	admin.NewCNProviderHandler,
	admin.NewProxyHandler,
	admin.NewRedeemHandler,
	admin.NewPromoHandler,
	admin.NewVisionCapabilityHandler,
	ProvideAdminSettingHandler,
	admin.NewOpsHandler,
	ProvideSystemHandler,
	admin.NewSubscriptionHandler,
	admin.NewUsageHandler,
	admin.NewUserAttributeHandler,
	admin.NewErrorPassthroughHandler,
	admin.NewTLSFingerprintProfileHandler,
	admin.NewPluginHandler,
	admin.NewAdminAPIKeyHandler,
	admin.NewScheduledTestHandler,
	admin.NewChannelHandler,
	admin.NewChannelMonitorHandler,
	admin.NewPromoIntelHandler,
	admin.NewChannelMonitorRequestTemplateHandler,
	admin.NewContentModerationHandler,
	admin.NewPaymentHandler,
	admin.NewAffiliateHandler,
	admin.NewComplianceHandler,
	admin.NewAuditLogHandler,
	admin.NewPricingHandler,
	admin.NewUsageRiskHandler,

	// AdminHandlers and Handlers constructors
	ProvideAdminHandlers,
	ProvideHandlers,
)
