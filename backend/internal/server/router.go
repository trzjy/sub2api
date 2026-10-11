package server

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/web"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const frameSrcRefreshTimeout = 5 * time.Second

// SetupRouter 配置路由器中间件和路由
func SetupRouter(
	r *gin.Engine,
	handlers *handler.Handlers,
	jwtAuth middleware2.JWTAuthMiddleware,
	optionalJWTAuth middleware2.OptionalJWTAuthMiddleware,
	adminAuth middleware2.AdminAuthMiddleware,
	apiKeyAuth middleware2.APIKeyAuthMiddleware,
	auditLog middleware2.AuditLogMiddleware,
	stepUpAuth middleware2.StepUpAuthMiddleware,
	apiKeyService *service.APIKeyService,
	subscriptionService *service.SubscriptionService,
	opsService *service.OpsService,
	settingService *service.SettingService,
	compositeResolver *service.CompositeRouteResolver,
	cfg *config.Config,
	redisClient *redis.Client,
) *gin.Engine {
	middleware2.SetIngressRejectRecorder(opsService)
	// 缓存 iframe 页面的 origin 列表，用于动态注入 CSP frame-src
	var cachedFrameOrigins atomic.Pointer[[]string]
	emptyOrigins := []string{}
	cachedFrameOrigins.Store(&emptyOrigins)

	refreshFrameOrigins := func() {
		ctx, cancel := context.WithTimeout(context.Background(), frameSrcRefreshTimeout)
		defer cancel()
		origins, err := settingService.GetFrameSrcOrigins(ctx)
		if err != nil {
			// 获取失败时保留已有缓存，避免 frame-src 被意外清空
			return
		}
		cachedFrameOrigins.Store(&origins)
	}
	refreshFrameOrigins() // 启动时初始化

	// 应用中间件
	r.Use(middleware2.RequestLogger())
	// 将客户端 IP + UA 注入 request context，供 token 签发/会话绑定/审计日志统一读取。
	// 解析模式按请求快照：兼容开关开启时信任原始转发头，关闭时使用 server.trusted_proxies。
	r.Use(middleware2.SessionBindingContext(cfg))
	r.Use(middleware2.Logger())
	r.Use(middleware2.CORS(cfg.CORS))
	r.Use(middleware2.SecurityHeaders(cfg.Security.CSP, func() []string {
		origins := []string{}
		if p := cachedFrameOrigins.Load(); p != nil {
			origins = append(origins, *p...)
		}
		if len(origins) == 0 {
			return nil
		}
		return origins
	}))
	r.Use(middleware2.ServerTiming(cfg.Server.EnableServerTiming))

	// Serve embedded frontend with settings injection if available
	if web.HasEmbeddedFrontend() {
		frontendServer, err := web.NewFrontendServer(settingService) //nolint:staticcheck // SA4023: the !embed stub always errors; embed builds can return nil
		if err != nil {                                              //nolint:staticcheck // SA4023: see above
			log.Printf("Warning: Failed to create frontend server with settings injection: %v, using legacy mode", err)
			r.Use(web.ServeEmbeddedFrontend())
			settingService.SetOnUpdateCallback(refreshFrameOrigins)
		} else {
			// Register combined callback: invalidate HTML cache + refresh frame origins
			settingService.SetOnUpdateCallback(func() {
				frontendServer.InvalidateCache()
				refreshFrameOrigins()
			})
			r.Use(frontendServer.Middleware())
		}
	} else {
		settingService.SetOnUpdateCallback(refreshFrameOrigins)
	}

	// 注册路由
	registerRoutes(r, handlers, jwtAuth, optionalJWTAuth, adminAuth, apiKeyAuth, auditLog, stepUpAuth, apiKeyService, subscriptionService, opsService, settingService, compositeResolver, cfg, redisClient)

	return r
}

// registerRoutes 注册所有 HTTP 路由
func registerRoutes(
	r *gin.Engine,
	h *handler.Handlers,
	jwtAuth middleware2.JWTAuthMiddleware,
	optionalJWTAuth middleware2.OptionalJWTAuthMiddleware,
	adminAuth middleware2.AdminAuthMiddleware,
	apiKeyAuth middleware2.APIKeyAuthMiddleware,
	auditLog middleware2.AuditLogMiddleware,
	stepUpAuth middleware2.StepUpAuthMiddleware,
	apiKeyService *service.APIKeyService,
	subscriptionService *service.SubscriptionService,
	opsService *service.OpsService,
	settingService *service.SettingService,
	compositeResolver *service.CompositeRouteResolver,
	cfg *config.Config,
	redisClient *redis.Client,
) {
	// 通用路由（健康检查、状态等）
	routes.RegisterCommonRoutes(r)

	// API v1
	v1 := r.Group("/api/v1")

	// 面板 API 限流器：认证接口按用户 ID、公开接口按安全客户端 IP，
	// 防止高频刷管理面接口打爆数据库（阈值可在系统设置中调整）。
	panelRateLimiter := middleware2.NewPanelRateLimiter(redisClient, settingService)

	// w28 conflict-resolution fusion (second wave, B9 — semantic_conflict)
	// 双方只在 registerRoutes 尾段同一行分叉：我方（b0-ours）新增 RegisterInternalRoutes /
	// RegisterXgjSupplyRoutes 两组自研路由，并把 SecurityHeaders 的 frame-src 回调改写为
	// 「拷贝到新切片 → 空则返回 nil」；上游（b0-upstream）唯一改动是给
	// routes.RegisterPaymentRoutes 调用末位补 redisClient（为 /payment/public/orders/verify
	// 的匿名 out_trade_no 枚举做按 IP 限流）。签名侧两版 SetupRouter/registerRoutes 形参表
	// 完全一致（末位均为 redisClient *redis.Client），无 [COMPILE] 级冲突。
	//
	// keep OURS — RegisterInternalRoutes（闲管家/闲鱼服务间端点 + /webhook/xianguanjia）与
	//   RegisterXgjSupplyRoutes（D6e 货源模式公开组，套六段验签）：
	//   1. 二者均为 fork 自研能力，b0-base 与 b0-upstream 都不存在
	//      （git ls-tree b0-upstream -- backend/internal/server/routes/ 无 xgj_supply.go；
	//      上游 common.go 只有 RegisterCommonRoutes，无 RegisterInternalRoutes）。
	//      merge-plan §5 rule 1「我方有重写的符号 → 我方语义为准」，静默丢弃会直接下线
	//      闲鱼自动发货/闲管家推送与货源模式两条业务链路。
	//   2. 两组路由自身带 nil 守卫（common.go:38 `if h == nil || h.XianyuDelivery == nil`；
	//      xgj_supply.go:22 `if h == nil || h.XgjSupply == nil`），未接线部署下整组不注册，
	//      因此追加这两行对既有部署零副作用。
	//
	// keep OURS — SecurityHeaders 回调改写（router.go:65-74）：
	//   我方把 `return *p` 换成拷贝到 origins 再判空返回，语义等价（空集合仍返回 nil，
	//   非空集合返回同序同值切片），额外收益是不把 atomic.Pointer 指向的内部切片交给
	//   下游持有，避免后续刷新时被别名改写。上游未触碰该块，按 §5 rule 1 保留我方写法。
	//
	// take UPSTREAM（已收敛）— RegisterPaymentRoutes 末位 redisClient 实参：
	//   本卡已落地：payment.go 侧此刻已是 10 参形态（末位 redisClient *redis.Client，含
	//   import "time"/ratelimit、publicOrderVerifyRateLimit=20/1min 常量与
	//   public.POST("/orders/verify") 的按 IP 限流中间件），故调用点补实参即编译通过。
	//   下方收敛动作与验证锚点为历史登记，保留备查。
	//   上游在 routes/payment.go 同步做了三处配套改动：import "time" 与 ratelimit 别名、
	//   新增 publicOrderVerifyRateLimit=20/1min 常量、形参表末位加 redisClient，并给
	//   public.POST("/orders/verify") 挂 ratelimit.NewRateLimiter(redisClient).Limit(...)。
	//   本卡只认领 router.go，routes/payment.go 不在派发单文件清单内（现场纪律第 4 条），
	//   且工作树里 payment.go 仍是 9 参形态（与 b0-ours 逐字节相同，尚未有卡融合）：
	//   此时在调用点补 redisClient 会得到 not enough arguments / too many arguments 的
	//   编译错误，属权威表第 1 条同类的「调用点与定义侧不同卡」情形，故不在本卡单方面改动。
	//   上游的枚举防护是安全向的正交修复（§5 rule 1 要求显式吸收而非静默丢弃），登记如下：
	//   owner = routes/payment.go 卡（当前 75 张卡中无一张认领 payment.go，需 B11 派发）。
	//   收敛动作（payment.go 侧）：并入上述 import/常量/形参/限流中间件后，把本行改为
	//     routes.RegisterPaymentRoutes(v1, h.Payment, h.PaymentWebhook, h.Admin.Payment,
	//       jwtAuth, adminAuth, auditLog, settingService, panelRateLimiter, redisClient)
	//   并随上游补入 backend/internal/server/routes/payment_public_rate_limit_test.go。
	//   验证锚点：`go build ./internal/server/...` 通过，且
	//   `go test -tags unit ./internal/server/routes/ -run PaymentPublic` 全绿。

	// 注册各模块路由
	routes.RegisterAuthRoutes(v1, h, jwtAuth, auditLog, redisClient, settingService, panelRateLimiter)
	routes.RegisterUserRoutes(v1, h, jwtAuth, auditLog, settingService, panelRateLimiter)
	routes.RegisterModelPlazaRoutes(v1, h, optionalJWTAuth, settingService, panelRateLimiter)
	routes.RegisterAdminRoutes(v1, h, adminAuth, auditLog, stepUpAuth, settingService, panelRateLimiter)
	routes.RegisterGatewayRoutes(r, h, apiKeyAuth, apiKeyService, subscriptionService, opsService, settingService, compositeResolver, cfg)
	routes.RegisterPaymentRoutes(v1, h.Payment, h.PaymentWebhook, h.Admin.Payment, jwtAuth, adminAuth, auditLog, settingService, panelRateLimiter, redisClient)
	routes.RegisterInternalRoutes(v1, h)
	// D6e: 货源模式（虚拟货源提卡）公开组，套六段验签中间件。
	routes.RegisterXgjSupplyRoutes(v1, h)

	handler.RegisterPageRoutes(v1, cfg.Pricing.DataDir, gin.HandlerFunc(jwtAuth), gin.HandlerFunc(adminAuth), settingService)
}
