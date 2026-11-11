package admin

import (
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// webConsoleAuthStartResponse 是 POST /admin/accounts/:id/web-console-auth/start 的响应体，
// 与前端 frontend/src/api/admin/cnProviders.ts::startWebConsoleAuth 的冻结契约一致。
// 只回传 session_id 与剩余有效期；Binding / ExpectedUsername 等内部/PII 字段一律不回传。
type webConsoleAuthStartResponse struct {
	SessionID string `json:"session_id"`
	ExpiresIn int64  `json:"expires_in"`
}

// CNProviderHandler 暴露国产供应商（kimi/zhipu/deepseek）的额度与余额查询端点。
//
//   - GET /admin/cn-providers/accounts/:id/quota   Coding Plan 滚动窗口用量（kimi/zhipu）
//   - GET /admin/cn-providers/accounts/:id/balance  payg 账号余额（kimi/deepseek）
//
// 智谱（zhipu）无余额端点，故同一账号仅 quota 或 balance 其一可用：服务端按账号
// platform + account_mode 校验并返回明确错误（见 CNProvider*Service 的 load*Account）。
type CNProviderHandler struct {
	quotaService   *service.CNProviderQuotaService
	balanceService *service.CNProviderBalanceService

	// webConsoleQuotaService / webConsoleAuthService 为 Web 控制台额度链（compshare/sensenova/longcat）。
	// 用 Setter 注入，不改既有构造函数签名，避免破坏 wire_gen 既有调用。
	webConsoleQuotaService *service.WebConsoleQuotaService
	webConsoleAuthService  *service.WebConsoleAuthService
}

func NewCNProviderHandler(
	quotaService *service.CNProviderQuotaService,
	balanceService *service.CNProviderBalanceService,
) *CNProviderHandler {
	return &CNProviderHandler{
		quotaService:   quotaService,
		balanceService: balanceService,
	}
}

// SetWebConsoleServices 注入 Web 控制台额度链（可选；未注入时对应端点返回 not enabled）。
func (h *CNProviderHandler) SetWebConsoleServices(
	quota *service.WebConsoleQuotaService,
	auth *service.WebConsoleAuthService,
) {
	if h == nil {
		return
	}
	h.webConsoleQuotaService = quota
	h.webConsoleAuthService = auth
}

// QueryQuota 查询 Coding Plan 滚动窗口用量（5h + weekly）。
func (h *CNProviderHandler) QueryQuota(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.quotaService == nil {
		response.BadRequest(c, "cn provider quota service is not enabled")
		return
	}
	result, err := h.quotaService.QueryUsage(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// QueryBalance 查询 payg 账号余额。
func (h *CNProviderHandler) QueryBalance(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.balanceService == nil {
		response.BadRequest(c, "cn provider balance service is not enabled")
		return
	}
	result, err := h.balanceService.QueryBalance(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// ProbeWebConsoleQuota 手动触发一次 Web 控制台额度探测。
func (h *CNProviderHandler) ProbeWebConsoleQuota(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.webConsoleQuotaService == nil {
		response.BadRequest(c, "web console quota service is not enabled")
		return
	}
	result, err := h.webConsoleQuotaService.ProbeAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// StartWebConsoleAuth 发起本地浏览器交互登录（滑块/扫码/短信）。
func (h *CNProviderHandler) StartWebConsoleAuth(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.webConsoleAuthService == nil {
		response.BadRequest(c, "web console auth service is not enabled")
		return
	}
	result, err := h.webConsoleAuthService.StartInteractiveLogin(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	expiresIn := int64(0)
	if !result.ExpiresAt.IsZero() {
		if d := time.Until(result.ExpiresAt); d > 0 {
			expiresIn = int64(d.Seconds())
		}
	}
	response.Success(c, webConsoleAuthStartResponse{SessionID: result.ID, ExpiresIn: expiresIn})
}

// GetWebConsoleAuthStatus 轮询交互登录状态；成功后自动落凭据并触发一次探测。
func (h *CNProviderHandler) GetWebConsoleAuthStatus(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	sessionID := c.Param("sessionId")
	if h == nil || h.webConsoleAuthService == nil {
		response.BadRequest(c, "web console auth service is not enabled")
		return
	}
	result, err := h.webConsoleAuthService.GetInteractiveLoginStatus(c.Request.Context(), accountID, sessionID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
