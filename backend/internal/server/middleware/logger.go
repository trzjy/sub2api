package middleware

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Logger 请求日志中间件
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 开始时间
		startTime := time.Now()

		// 请求路径
		path := c.Request.URL.Path

		// 处理请求
		c.Next()

		// 跳过健康检查等高频探针路径的日志
		if path == "/health" || path == "/setup/status" {
			return
		}

		endTime := time.Now()
		latency := endTime.Sub(startTime)

		method := c.Request.Method
		statusCode := c.Writer.Status()
		clientIP := ip.GetClientIP(c)
		protocol := c.Request.Proto
		accountID, hasAccountID := c.Request.Context().Value(ctxkey.AccountID).(int64)
		platform, _ := c.Request.Context().Value(ctxkey.Platform).(string)
		model, _ := c.Request.Context().Value(ctxkey.Model).(string)
		reason, rejected := GetIngressRejectReason(c)
		if rejected {
			recordIngressReject(c, reason)
			allowed, droppedSummary := globalIngressRejectAccessSampler.allow(endTime)
			if droppedSummary > 0 {
				logger.FromContext(c.Request.Context()).Info("ingress rejection access logs dropped",
					zap.String("component", "http.access"),
					zap.Uint64("dropped_count", droppedSummary),
					zap.Bool(logger.OpsSystemLogSkipField, true),
				)
			}
			if !allowed {
				return
			}
		}

		fields := []zap.Field{
			zap.String("component", "http.access"),
			zap.Int("status_code", statusCode),
			zap.Int64("latency_ms", latency.Milliseconds()),
			zap.String("client_ip", clientIP),
			zap.String("protocol", protocol),
			zap.String("method", method),
			zap.String("path", path),
		}
		if rejected {
			fields = append(fields,
				zap.String("ingress_reject_reason", string(reason)),
				zap.Bool(logger.OpsSystemLogSkipField, true),
			)
		}
		if hasAccountID && accountID > 0 {
			fields = append(fields, zap.Int64("account_id", accountID))
		}
		if platform != "" {
			fields = append(fields, zap.String("platform", platform))
		}
		if model != "" {
			fields = append(fields, zap.String("model", model))
		}
		// CF524 G5 判别器：四个非 OpenAI 挂点收到上游响应头时记录时刻，经本既有请求级
		// 完成/访问日志输出相对入口毫秒数（request_id 已在本日志链中）。禁止新建事件流
		// （v8.1 收敛）。未收到头（header-wait 悬挂）时不落该字段——主验收据此区分
		// header-wait 墙簇与 body-hang 残留。OpenAI 原生路径不写该 context key，零变化。
		if headersMs, ok := service.UpstreamHeadersReceivedMs(c, startTime); ok {
			fields = append(fields, zap.Int64("upstream_headers_received_ms", headersMs))
		}
		// CF524 G5：请求出口补发整条换号链未解析的 guard 终态关联事件（同 request_id）。
		service.ResolvePendingGuardObservations(c)

		l := logger.FromContext(c.Request.Context()).With(fields...)
		l.Info("http request completed", zap.Time("completed_at", endTime))

		if len(c.Errors) > 0 {
			l.Warn("http request contains gin errors", zap.String("errors", c.Errors.String()))
		}
	}
}
