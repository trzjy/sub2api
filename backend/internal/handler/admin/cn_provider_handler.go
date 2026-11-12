package admin

import (
	"errors"
	"net/http"
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
	// webConsoleRemoteLoginService 是服务端登录会话（扫码/短信）编排；
	// 与 webConsoleAuthService 的本机 helper 通道并存，未注入时对应端点返回 not enabled。
	webConsoleRemoteLoginService *service.WebConsoleRemoteLoginService
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

// ---------------------------------------------------------------------------
// 服务端登录会话（扫码 / 短信 / 滑块）
//
// 与 /web-console-auth/*（本机 helper 弹窗）并存：本组接口把登录画面搬到
// 服务端，管理端网页直接显示画面并转发点击/输入，不要求用户在本机装工具。
// ---------------------------------------------------------------------------

// SetWebConsoleRemoteLoginService 注入服务端登录会话编排（可选）。
func (h *CNProviderHandler) SetWebConsoleRemoteLoginService(svc *service.WebConsoleRemoteLoginService) {
	if h == nil {
		return
	}
	h.webConsoleRemoteLoginService = svc
}

// StartWebConsoleLogin 打开一扇服务端登录画面，返回首帧截图。
func (h *CNProviderHandler) StartWebConsoleLogin(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.webConsoleRemoteLoginService == nil {
		response.BadRequest(c, "web console remote login service is not enabled")
		return
	}
	frame, err := h.webConsoleRemoteLoginService.Start(c.Request.Context(), accountID)
	if err != nil {
		writeWebConsoleRemoteLoginError(c, err)
		return
	}
	response.Success(c, frame)
}

// GetWebConsoleLoginFrame 拉取最新画面。
func (h *CNProviderHandler) GetWebConsoleLoginFrame(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.webConsoleRemoteLoginService == nil {
		response.BadRequest(c, "web console remote login service is not enabled")
		return
	}
	frame, err := h.webConsoleRemoteLoginService.Frame(c.Request.Context(), accountID, c.Param("sessionId"))
	if err != nil {
		writeWebConsoleRemoteLoginError(c, err)
		return
	}
	response.Success(c, frame)
}

// webConsoleLoginActionRequest 是一次远程操作的请求体。
// kind 决定使用哪个字段：tap(x,y) / type(text) / key(key) / scroll(delta_y) / reload。
type webConsoleLoginActionRequest struct {
	Kind   string  `json:"kind"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Text   string  `json:"text"`
	Key    string  `json:"key"`
	DeltaY float64 `json:"delta_y"`
}

// PostWebConsoleLoginAction 把点击 / 输入 / 回车 / 滚动 / 重载转发到登录页面。
func (h *CNProviderHandler) PostWebConsoleLoginAction(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.webConsoleRemoteLoginService == nil {
		response.BadRequest(c, "web console remote login service is not enabled")
		return
	}
	var req webConsoleLoginActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid action payload")
		return
	}

	ctx := c.Request.Context()
	sessionID := c.Param("sessionId")
	switch req.Kind {
	case "tap":
		err = h.webConsoleRemoteLoginService.Tap(ctx, accountID, sessionID, req.X, req.Y)
	case "type":
		err = h.webConsoleRemoteLoginService.Type(ctx, accountID, sessionID, req.Text)
	case "key":
		err = h.webConsoleRemoteLoginService.PressKey(ctx, accountID, sessionID, req.Key)
	case "scroll":
		err = h.webConsoleRemoteLoginService.Scroll(ctx, accountID, sessionID, req.DeltaY)
	case "reload":
		err = h.webConsoleRemoteLoginService.Reload(ctx, accountID, sessionID)
	default:
		response.BadRequest(c, "Unknown action kind")
		return
	}
	if err != nil {
		writeWebConsoleRemoteLoginError(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true})
}

// FinishWebConsoleLogin 取回登录后的会话凭据并立即探测一次。
func (h *CNProviderHandler) FinishWebConsoleLogin(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.webConsoleRemoteLoginService == nil {
		response.BadRequest(c, "web console remote login service is not enabled")
		return
	}
	result, err := h.webConsoleRemoteLoginService.Finish(c.Request.Context(), accountID, c.Param("sessionId"))
	if err != nil {
		writeWebConsoleRemoteLoginError(c, err)
		return
	}
	response.Success(c, result)
}

// DeleteWebConsoleLogin 取消登录并释放服务端浏览器上下文。
func (h *CNProviderHandler) DeleteWebConsoleLogin(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.webConsoleRemoteLoginService == nil {
		response.BadRequest(c, "web console remote login service is not enabled")
		return
	}
	if err := h.webConsoleRemoteLoginService.Close(c.Request.Context(), accountID, c.Param("sessionId")); err != nil {
		writeWebConsoleRemoteLoginError(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true})
}

// writeWebConsoleRemoteLoginError 把服务层错误映射成明确状态码，
// 让前端能区分「等用户操作」「会话过期」「通道未配置」。
func writeWebConsoleRemoteLoginError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrRemoteLoginNotApplicable):
		response.BadRequest(c, "web console remote login not applicable")
	case errors.Is(err, service.ErrRemoteLoginNotConfigured):
		response.BadRequest(c, "web quota worker is not configured")
	case errors.Is(err, service.ErrRemoteLoginSessionUnknown):
		c.JSON(http.StatusGone, gin.H{"error": "login session expired or not found"})
	case errors.Is(err, service.ErrRemoteLoginStepFailed):
		c.JSON(http.StatusBadGateway, gin.H{"error": "login step failed"})
	default:
		response.ErrorFrom(c, err)
	}
}
