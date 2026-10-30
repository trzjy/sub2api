package main

// 派发单 CR-D：Wire 十缺项闭合（方案 §25.14-A）中与 Cursor 无关的八项的 cmd 层装配函数。
//
// 原则（与方案 §25.14-A 一致）：
//   - 只把现有 wire_gen.go 里真实存在的构造表达式搬回 provider 声明，不新造实例、不改业务语义；
//   - 私有接口（service.usageRiskRetentionCleaner）不导出，靠本文件唯一 wrapper 收口；
//   - 不更换密钥来源、不生成第二份密钥；同一依赖只解析一次，保持实例共享；
//   - 本文件是纯装配代码（无状态、无副作用），随包常驻编译，便于单测与审阅。
//
// 本文件落位：
//  1. service.usageRiskRetentionCleaner → provideDashboardAggregationService
//  4. xianguanjia.SupplySecretEncryptor → provideSupplySecretEncryptor
//  5. *handler.XianyuXianguanjiaPushHandler（含 config/clientFactory/signature/idempotency/voider）
//  6. *handler.XgjSupplyHandler
//  7. handler.XianguanjiaSupplyOrderService（接口绑定见 cmd/server/wire.go）
//  8. *handler.XianguanjiaSupplyHandler
//
// 第 2/3 项（ChannelFreshnessRefresher 绑定、ProvideFreshnessAlertService 登记）落在
// internal/service/wire.go 的 ProviderSet。

import (
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/Wei-Shaw/sub2api/internal/usageriskadapter"
)

// provideDashboardAggregationService 是 usage_risk 保留期清理器的唯一装配点（缺项 1）。
//
// service.usageRiskRetentionCleaner 是 service 包内私有接口，不导出、不新造清理器；
// 这里沿用现有适配器的 *adapter（usageriskadapter.NewRetentionCleaner），把它作为该私有
// 接口的实参传给既有 service.ProvideDashboardAggregationService。setting、DB、Leader 锁与
// Start 行为全部保留在原 provider 内，本函数不加任何分支或默认值。
func provideDashboardAggregationService(
	repo service.DashboardAggregationRepository,
	timingWheel *service.TimingWheelService,
	lockCache service.LeaderLockCache,
	db *sql.DB,
	cfg *config.Config,
	settingRepo service.SettingRepository,
	settingService *service.SettingService,
	usageRiskRepository repository.UsageRiskRepository,
) *service.DashboardAggregationService {
	cleaner := usageriskadapter.NewRetentionCleaner(usageRiskRepository)
	return service.ProvideDashboardAggregationService(repo, timingWheel, lockCache, db, cfg, settingRepo, settingService, cleaner)
}

// provideSupplySecretEncryptor 把既有 service.SecretEncryptor 装配成结构兼容的
// xianguanjia.SupplySecretEncryptor（缺项 4）。
//
// repository.NewAESEncryptor 的 provider 返回类型是 service.SecretEncryptor，Wire 无法按
// 具体指针类型绑定，故用本纯装配函数收口：直接返回同一实例，不换密钥来源、不再生成密钥。
func provideSupplySecretEncryptor(encryptor service.SecretEncryptor) xianguanjia.SupplySecretEncryptor {
	return encryptor
}

// provideXianguanjiaIdempotencyStore 装配推送幂等回执存储（缺项 5 的 idempotency 部分）。
// 具体实现类型未导出，故此处按 handler 侧窄接口返回，实例仍由 xianguanjia 包构造。
func provideXianguanjiaIdempotencyStore(db *sql.DB) handler.XianguanjiaIdempotencyStore {
	return xianguanjia.NewPushIdempotencyStore(db)
}

// provideXianguanjiaSignatureVerifier 装配推送验签器（缺项 5 的 signature 部分）。
// 读侧配置与解密器均复用既有实例（secretEncryptor 同一份密钥）。
func provideXianguanjiaSignatureVerifier(
	cfgStore xianguanjia.ConfigStore,
	decrypt service.SecretEncryptor,
) *handler.XianguanjiaSignatureVerifier {
	return handler.NewXianguanjiaSignatureVerifier(cfgStore, decrypt)
}

// provideXianguanjiaClientFactory 装配出站客户端工厂（缺项 5 的 clientFactory 部分）。
// 动态读 252 配置 + 解密，凭证不落盘；解密函数直接取同一 encryptor 的 Decrypt。
func provideXianguanjiaClientFactory(
	cfgStore xianguanjia.ConfigStore,
	encryptor service.SecretEncryptor,
) *xianguanjia.ClientFactory {
	return xianguanjia.NewClientFactory(cfgStore, encryptor.Decrypt)
}

// provideXianguanjiaRefundCardVoidService 装配按 card_no 精准作废服务（缺项 5 的 voider 部分）。
// claim 追回锚点复用同一 XianyuDeliveryService 实例（used 码防双扣）。
func provideXianguanjiaRefundCardVoidService(
	db *sql.DB,
	factory *xianguanjia.ClientFactory,
	claims *service.XianyuDeliveryService,
) *xianguanjia.RefundCardVoidService {
	return xianguanjia.NewRefundCardVoidService(factory, xianguanjia.NewCodeCardVoidRepository(db), claims)
}

// provideXianyuXianguanjiaPushHandler 装配闲管家推送 webhook handler（缺项 5）。
// 四个依赖即原生成文件的 config/clientFactory/signature/idempotency/voider，实例共享不变。
func provideXianyuXianguanjiaPushHandler(
	voider handler.XianguanjiaRefundVoider,
	idem handler.XianguanjiaIdempotencyStore,
	verifier *handler.XianguanjiaSignatureVerifier,
	factory *xianguanjia.ClientFactory,
) *handler.XianyuXianguanjiaPushHandler {
	return handler.NewXianyuXianguanjiaPushHandler(voider, idem, verifier, factory)
}

// provideXgjSupplyHandler 装配货源公开接口 handler（缺项 6）。
// 保留原 SupplyConfigStore 与现行构造参数（gateway 为空串，与原生成文件一致）；
// 不借接线改鉴权或货源语义。
func provideXgjSupplyHandler(store *xianguanjia.SupplyConfigStore) *handler.XgjSupplyHandler {
	return handler.NewXgjSupplyHandler(store, "")
}

// provideXianguanjiaSupplyOrderService 装配卡密订单服务（缺项 7）。
// DB、card 生成/池、goods、refund、redeem 实例与原生成文件逐一对应；
// nil 位为既有 pwdResolve 占位（现场生成卡密，无卡密口令解析）。不改退款/扣费事务。
func provideXianguanjiaSupplyOrderService(
	db *sql.DB,
	client *ent.Client,
	redeemService *service.RedeemService,
) *xianguanjia.SupplyOrderService {
	return xianguanjia.NewSupplyOrderService(
		xianguanjia.NewSupplyOrderStore(db),
		xianguanjia.NewSupplyCardGenerator(db),
		xianguanjia.NewSupplyCardPool(db),
		xianguanjia.NewSupplyGoodsSource(db),
		nil,
		xianguanjia.NewSupplyRefundStore(client, redeemService),
		redeemService,
	)
}

// provideStandbyController 是主力渠道自适应备用切换控制器的唯一装配点（第 3 片）。
//
// 这里一次装配四个部件，因为它们共享同一份运行时上下文（DB、时区、间隔）：
//   - repository.NewStandbyHealthRepository → service.StandbyHealthSource（只读查询）
//   - repository.NewStandbyCoveredRepository → service.StandbyCoveredModelSource
//   - service.NewStandbyHealthCollector（按全局时区做 hour-of-day 分桶）
//   - service.NewProcessStandbyController（注册表挂成进程级单例，调度层读同一实例）
//
// **失败开放点（全片唯一一处，故意为之）**：开关关闭，或任一构造依赖不成立时，
// 返回 nil 并只记 error 日志，绝不阻断主服务启动。失败方向是"禁用 standby"——
// 控制器不启动、不写注册表，进程级注册表保持空表 = 所有模型 closed，
// 调度层 standbyRoutingOpen 恒为 false，行为与未接入时逐位一致。
// 允许失败开放的边界：备用切换是延迟劣化时的加速手段，不是请求主链路；
// 缺它只会退化为现状，不会造成请求失败，而阻断启动会放大成整站不可用。
func provideStandbyController(cfg *config.Config, db *sql.DB) *service.StandbyController {
	if cfg == nil || db == nil {
		slog.Error("standby_controller_disabled", "reason", "nil config or nil database")
		return nil
	}
	if !cfg.Standby.Enabled {
		slog.Info("standby_controller_disabled", "reason", "standby.enabled=false")
		return nil
	}

	tz := strings.TrimSpace(cfg.Timezone)
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	location, err := time.LoadLocation(tz)
	if err != nil {
		slog.Error("standby_controller_disabled", "reason", "invalid timezone", "timezone", tz, "error", err)
		return nil
	}

	interval := time.Duration(cfg.Standby.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = service.StandbyDefaultPollInterval
	}

	healthSource := repository.NewStandbyHealthRepository(db)
	covered := repository.NewStandbyCoveredRepository(db)
	collector := service.NewStandbyHealthCollector(healthSource, service.StandbyCollectTimeout, location)
	controller := service.NewProcessStandbyController(collector, covered, service.StandbyControllerOptions{
		Interval:          interval,
		ForceOpenModels:   cfg.Standby.ForceOpenModels,
		ForceClosedModels: cfg.Standby.ForceClosedModels,
	})

	slog.Info("standby_controller_enabled", "interval", interval.String(), "timezone", tz,
		"force_open_models", cfg.Standby.ForceOpenModels,
		"force_closed_models", cfg.Standby.ForceClosedModels,
	)
	return controller
}

// provideXianguanjiaSupplyHandler 装配货源目录 handler（缺项 8）。
// 复用原 SupplyConfigStore 构造既有 SupplyCatalogService，goods source 与生成文件一致（db）。
func provideXianguanjiaSupplyHandler(
	store *xianguanjia.SupplyConfigStore,
	db *sql.DB,
) *handler.XianguanjiaSupplyHandler {
	catalog := xianguanjia.NewSupplyCatalogService(store, xianguanjia.NewSupplyGoodsSource(db))
	return handler.NewXianguanjiaSupplyHandler(catalog)
}
