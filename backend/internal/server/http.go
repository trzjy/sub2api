// Package server provides HTTP server initialization and configuration.
package server

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/websearch"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"golang.org/x/net/http2"
)

// ProviderSet 提供服务器层的依赖
var ProviderSet = wire.NewSet(
	ProvideRouter,
	ProvideHTTPServer,
	ProvideWebLoginProxyServer,
)

// ProvideRouter 提供路由器
func ProvideRouter(
	cfg *config.Config,
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
	redisClient *redis.Client,
) *gin.Engine {
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(middleware2.Recovery())
	configureTrustedProxies(r, cfg.Server)

	// Wire up websearch Manager builder so it initializes on startup and rebuilds on config save.
	settingService.SetWebSearchManagerBuilder(context.Background(), func(cfg *service.WebSearchEmulationConfig, proxyURLs map[int64]string) {
		if cfg == nil || !cfg.Enabled || len(cfg.Providers) == 0 {
			service.SetWebSearchManager(nil)
			return
		}
		configs := make([]websearch.ProviderConfig, 0, len(cfg.Providers))
		for _, p := range cfg.Providers {
			if p.APIKey == "" {
				continue
			}
			pc := websearch.ProviderConfig{
				Type:       p.Type,
				APIKey:     p.APIKey,
				QuotaLimit: derefInt64(p.QuotaLimit),
				ExpiresAt:  p.ExpiresAt,
			}
			if p.SubscribedAt != nil {
				pc.SubscribedAt = p.SubscribedAt
			}
			if p.ProxyID != nil {
				pc.ProxyID = *p.ProxyID
				if u, ok := proxyURLs[*p.ProxyID]; ok {
					pc.ProxyURL = u
				} else {
					// Proxy configured but not found — skip this provider to prevent direct connection.
					slog.Warn("websearch: proxy not found for provider, skipping",
						"provider", p.Type, "proxy_id", *p.ProxyID)
					continue
				}
			}
			configs = append(configs, pc)
		}
		service.SetWebSearchManager(websearch.NewManager(configs, redisClient))
	})

	return SetupRouter(r, handlers, jwtAuth, optionalJWTAuth, adminAuth, apiKeyAuth, auditLog, stepUpAuth, apiKeyService, subscriptionService, opsService, settingService, compositeResolver, cfg, redisClient)
}

func configureTrustedProxies(r *gin.Engine, cfg config.ServerConfig) {
	if cfg.TrustedProxiesConfigured {
		if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
			log.Printf("Failed to set trusted proxies: %v", err)
			_ = r.SetTrustedProxies(nil)
		}
		if len(cfg.TrustedProxies) == 0 && cfg.Mode == "release" {
			log.Printf("Warning: server.trusted_proxies is explicitly empty; forwarded client IP trust is disabled")
		}
	} else {
		if err := r.SetTrustedProxies(nil); err != nil {
			log.Printf("Failed to disable trusted proxies: %v", err)
		}
		if cfg.Mode == "release" {
			log.Printf("Warning: server.trusted_proxies is not configured; disabling the forwarded-IP compatibility switch will use direct peer addresses only")
		}
	}
}

// ProvideHTTPServer 提供 HTTP 服务器
func ProvideHTTPServer(cfg *config.Config, router *gin.Engine) *http.Server {
	httpHandler := http.Handler(router)
	server := &http.Server{
		Addr:           cfg.Server.Address(),
		Handler:        httpHandler,
		MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
		// ReadHeaderTimeout: 读取请求头的超时时间，防止慢速请求头攻击
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeout) * time.Second,
		// IdleTimeout: 空闲连接超时时间，释放不活跃的连接资源
		IdleTimeout: time.Duration(cfg.Server.IdleTimeout) * time.Second,
		// 注意：不设置 WriteTimeout，因为流式响应可能持续十几分钟
		// 不设置 ReadTimeout，因为大请求体可能需要较长时间读取
	}

	globalMaxSize := cfg.Server.MaxRequestBodySize
	if globalMaxSize <= 0 {
		globalMaxSize = cfg.Gateway.MaxBodySize
	}
	if globalMaxSize > 0 {
		httpHandler = http.MaxBytesHandler(httpHandler, globalMaxSize)
		log.Printf("Global max request body size: %d bytes (%.2f MB)", globalMaxSize, float64(globalMaxSize)/(1<<20))
	}

	// 根据配置决定是否启用 H2C
	if cfg.Server.H2C.Enabled {
		h2cConfig := cfg.Server.H2C
		if err := http2.ConfigureServer(server, &http2.Server{
			MaxConcurrentStreams:         h2cConfig.MaxConcurrentStreams,
			IdleTimeout:                  time.Duration(h2cConfig.IdleTimeout) * time.Second,
			MaxReadFrameSize:             uint32(h2cConfig.MaxReadFrameSize),
			MaxUploadBufferPerConnection: int32(h2cConfig.MaxUploadBufferPerConnection),
			MaxUploadBufferPerStream:     int32(h2cConfig.MaxUploadBufferPerStream),
		}); err != nil {
			log.Printf("Failed to configure HTTP/2 Cleartext (h2c): %v", err)
		} else {
			protocols := new(http.Protocols)
			protocols.SetHTTP1(true)
			protocols.SetUnencryptedHTTP2(true)
			server.Protocols = protocols
			log.Printf("HTTP/2 Cleartext (h2c) enabled: max_concurrent_streams=%d, idle_timeout=%ds, max_read_frame_size=%d, max_upload_buffer_per_connection=%d, max_upload_buffer_per_stream=%d",
				h2cConfig.MaxConcurrentStreams,
				h2cConfig.IdleTimeout,
				h2cConfig.MaxReadFrameSize,
				h2cConfig.MaxUploadBufferPerConnection,
				h2cConfig.MaxUploadBufferPerStream,
			)
		}
	}

	server.Handler = httpHandler
	return server
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// WebLoginProxyServer 是网页登录代理的隔离 origin HTTP 服务器（独立监听端口）。
//
// 隔离目的（docs/web-login-proxy-security-fix-plan.md 阶段 A #1）：让代理运行在与
// 主管理 API 不同的 origin 上，官方页脚本无法读取管理端 localStorage.auth_token。
//
// 安全约定：
//   - 仅注册 ANY /api/v1/web-login-proxy/:token/*path，复用主服务的同一个
//     WebLoginProxyHandler 实例（捕获存储共享，会话创建/轮询仍走主 v1 admin 端点）。
//   - 不挂载 adminAuth / auditLog 等 admin 中间件：token 即能力凭证（现状不变）。
//   - WebLoginProxyAddr 为空时 ProvideWebLoginProxyServer 返回 nil（不启用，
//     前端 iframe 回退同源，代理路由仍注册在主服务 v1，功能不缺失）。
type WebLoginProxyServer struct {
	srv *http.Server
}

// Enabled 报告是否配置了隔离代理服务器（addr 非空）。
func (w *WebLoginProxyServer) Enabled() bool {
	return w != nil && w.srv != nil
}

// Addr 返回监听地址（用于日志，不含任何敏感值）。
func (w *WebLoginProxyServer) Addr() string {
	if w == nil || w.srv == nil {
		return ""
	}
	return w.srv.Addr
}

// Start 在调用方提供的 goroutine 中启动服务器；错误通过返回暴露（不含敏感值）。
// ListenAndServe 在正常关闭时返回 http.ErrServerClosed，视为成功。
func (w *WebLoginProxyServer) Start() error {
	if w == nil || w.srv == nil {
		return nil
	}
	if err := w.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown 优雅关停，跟随主服务的 shutdown 链。
func (w *WebLoginProxyServer) Shutdown(ctx context.Context) error {
	if w == nil || w.srv == nil {
		return nil
	}
	return w.srv.Shutdown(ctx)
}

// BuildWebLoginProxyEngine 构造隔离 origin 的轻量 gin engine（仅注册代理路由）。
// 不挂载 admin/audit 等中间件，token 即能力凭证。
func BuildWebLoginProxyEngine(handlers *handler.Handlers) *gin.Engine {
	r := gin.New()
	r.Use(middleware2.Recovery())
	if handlers != nil && handlers.Admin != nil && handlers.Admin.WebLoginProxy != nil {
		r.Any("/api/v1/web-login-proxy/:token/*path", handlers.Admin.WebLoginProxy.Proxy)
	}
	return r
}

// ProvideWebLoginProxyServer 构造隔离 origin 的网页登录代理服务器。
// WEB_LOGIN_PROXY_ADDR 为空（未配置）时返回 nil（不启用）。
func ProvideWebLoginProxyServer(cfg *config.Config, handlers *handler.Handlers) *WebLoginProxyServer {
	addr := strings.TrimSpace(cfg.Server.WebLoginProxyAddr)
	if addr == "" {
		return nil
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           BuildWebLoginProxyEngine(handlers),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 * 1024,
	}
	return &WebLoginProxyServer{srv: srv}
}
