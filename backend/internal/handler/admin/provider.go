package admin

import "github.com/Wei-Shaw/sub2api/internal/service"

// ProvideCNProviderHandlerWithWebConsole 构造 CNProviderHandler 并注入 Web 控制台服务。
//
// 落在 admin 包而非 service/wire.go：service 包不得反向 import handler/admin
// （admin 已 import service，反向即包依赖成环），故包装 provider 只能与
// CNProviderHandler 同包。
func ProvideCNProviderHandlerWithWebConsole(
	quota *service.CNProviderQuotaService,
	balance *service.CNProviderBalanceService,
	wcq *service.WebConsoleQuotaService,
	wca *service.WebConsoleAuthService,
	wcrl *service.WebConsoleRemoteLoginService,
) *CNProviderHandler {
	h := NewCNProviderHandler(quota, balance)
	h.SetWebConsoleServices(wcq, wca)
	h.SetWebConsoleRemoteLoginService(wcrl)
	return h
}
