package admin

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
)

// D3: 闲管家（开放平台 ERP 方向）admin 凭证配置入口。
// 职责：GET 脱敏展示配置、PUT 加密落库、POST 按官方签名探活。
// 本 handler 不持有任何真实凭证；AppSecret 经注入的 SecretEncryptor 加密后落 252 表。

// xianguanjiaConfigDefaultBaseURL 是闲管家开放平台默认地址。
const xianguanjiaConfigDefaultBaseURL = "https://open.goofish.pro"

// XianguanjiaConfigStore 是 admin 配置入口依赖的存储能力（由 252 表 ConfigStore 提供）。
type XianguanjiaConfigStore interface {
	GetConfigRow(ctx context.Context) (*xianguanjia.Config, error)
	UpsertConfig(ctx context.Context, up xianguanjia.ConfigUpsert) error
	UpdateHealth(ctx context.Context, healthStatus string, checkedAt time.Time) error
}

// XianguanjiaProbeClient 抽象探活 HTTP 客户端（生产注入 &http.Client，测试注入自定义）。
type XianguanjiaProbeClient interface {
	Probe(ctx context.Context, baseURL, appKey, appSecret string) error
}

// XianguanjiaConfigHandler admin 闲管家配置处理器。
type XianguanjiaConfigHandler struct {
	store    XianguanjiaConfigStore
	encrypt  service.SecretEncryptor
	probeCli XianguanjiaProbeClient
}

// NewXianguanjiaConfigHandler 构造处理器。store/encrypt 任一为 nil 时各端点返回 503（fail-closed）。
func NewXianguanjiaConfigHandler(store XianguanjiaConfigStore, encrypt service.SecretEncryptor, probeCli XianguanjiaProbeClient) *XianguanjiaConfigHandler {
	return &XianguanjiaConfigHandler{store: store, encrypt: encrypt, probeCli: probeCli}
}

// httpClientProbe 默认探活客户端：转发到 xianguanjia.ProbeAuthorizeList（官方四段签名）。
type httpClientProbe struct {
	client *http.Client
}

func (p httpClientProbe) Probe(ctx context.Context, baseURL, appKey, appSecret string) error {
	return xianguanjia.ProbeAuthorizeList(ctx, p.client, baseURL, appKey, appSecret)
}

// xianguanjiaConfigResponse 是 GET 返回的脱敏配置：AppSecret 只回是否已设置 + 末 4 位。
type xianguanjiaConfigResponse struct {
	Configured    bool   `json:"configured"`
	BaseURL       string `json:"base_url"`
	AppID         string `json:"app_id"`
	AppSecretSet  bool   `json:"app_secret_set"`
	AppSecretTail string `json:"app_secret_tail"`
	PushURL       string `json:"push_url"`
	Status        string `json:"status"`
	HealthStatus  string `json:"health_status"`
}

// Get 返回当前配置（AppSecret 脱敏：不回明文/密文，只回末 4 位）。
func (h *XianguanjiaConfigHandler) Get(c *gin.Context) {
	if h == nil || h.store == nil {
		response.Error(c, http.StatusServiceUnavailable, "xianguanjia config store unavailable")
		return
	}
	cfg, err := h.store.GetConfigRow(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "read xianguanjia config failed")
		return
	}
	if cfg == nil {
		// 未配置：返回空态（configured=false），前端据此显示首次录入表单。
		response.Success(c, xianguanjiaConfigResponse{
			Configured:   false,
			BaseURL:      xianguanjiaConfigDefaultBaseURL,
			Status:       "disabled",
			HealthStatus: "unknown",
		})
		return
	}
	tail := secretTail(h.decryptForTail(cfg.AppSecretEncrypted), cfg.AppSecretEncrypted)
	response.Success(c, xianguanjiaConfigResponse{
		Configured:    cfg.AppID != "" && cfg.AppSecretEncrypted != "",
		BaseURL:       cfg.BaseURL,
		AppID:         cfg.AppID,
		AppSecretSet:  cfg.AppSecretEncrypted != "",
		AppSecretTail: tail,
		PushURL:       cfg.PushURL,
		Status:        cfg.Status,
		HealthStatus:  cfg.HealthStatus,
	})
}

// decryptForTail 解密密文用于取末 4 位；解密失败回退空串（由 secretTail 用密文兜底）。
func (h *XianguanjiaConfigHandler) decryptForTail(encrypted string) string {
	if h == nil || h.encrypt == nil || encrypted == "" {
		return ""
	}
	plain, err := h.encrypt.Decrypt(encrypted)
	if err != nil || plain == "" {
		return ""
	}
	return plain
}

// secretTail 取末 4 位展示：优先明文末 4 位，解密不可用时回退密文末 4 位（均为 4 字符，不含完整凭证）。
func secretTail(plain, encrypted string) string {
	src := plain
	if src == "" {
		src = encrypted
	}
	if len(src) > 4 {
		return src[len(src)-4:]
	}
	return src
}

// xianguanjiaConfigPutRequest 是 PUT 请求体。
type xianguanjiaConfigPutRequest struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
	PushURL   string `json:"push_url"`
	BaseURL   string `json:"base_url"`
}

// Put 保存配置：AppSecret 用 SecretEncryptor 加密落 app_secret_encrypted，status 置 active。
// AppSecret 留空表示保留原值（仅更新其他字段）；首次保存（无已存密文）时留空报 400。
func (h *XianguanjiaConfigHandler) Put(c *gin.Context) {
	if h == nil || h.store == nil || h.encrypt == nil {
		response.Error(c, http.StatusServiceUnavailable, "xianguanjia config store unavailable")
		return
	}
	var req xianguanjiaConfigPutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid request body")
		return
	}
	req.AppID = strings.TrimSpace(req.AppID)
	req.AppSecret = strings.TrimSpace(req.AppSecret)
	req.PushURL = strings.TrimSpace(req.PushURL)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	if req.AppID == "" {
		response.Error(c, http.StatusBadRequest, "app_id is required")
		return
	}

	existing, err := h.store.GetConfigRow(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "read xianguanjia config failed")
		return
	}
	encSecret := ""
	if existing != nil {
		encSecret = existing.AppSecretEncrypted
	}
	if req.AppSecret != "" {
		enc, encErr := h.encrypt.Encrypt(req.AppSecret)
		if encErr != nil {
			slog.Error("xianguanjia config encrypt failed", "err", encErr)
			response.Error(c, http.StatusInternalServerError, "encrypt app secret failed")
			return
		}
		encSecret = enc
	}
	if encSecret == "" {
		response.Error(c, http.StatusBadRequest, "app_secret is required on first save")
		return
	}
	baseURL := req.BaseURL
	if baseURL == "" {
		baseURL = xianguanjiaConfigDefaultBaseURL
	}
	up := xianguanjia.ConfigUpsert{
		BaseURL:            baseURL,
		AppID:              req.AppID,
		AppSecretEncrypted: encSecret,
		PushURL:            req.PushURL,
		Status:             "active",
	}
	if err := h.store.UpsertConfig(c.Request.Context(), up); err != nil {
		slog.Error("xianguanjia config upsert failed", "err", err)
		response.Error(c, http.StatusInternalServerError, "save xianguanjia config failed")
		return
	}
	response.Success(c, gin.H{"saved": true})
}

// HealthCheck 用已存凭证按官方签名对 /api/open/user/authorize/list 探活，
// 更新 health_status / last_checked_at。凭证未配置时返回 400，不打外网。
func (h *XianguanjiaConfigHandler) HealthCheck(c *gin.Context) {
	if h == nil || h.store == nil || h.encrypt == nil {
		response.Error(c, http.StatusServiceUnavailable, "xianguanjia config store unavailable")
		return
	}
	cfg, err := h.store.GetConfigRow(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "read xianguanjia config failed")
		return
	}
	if cfg == nil || cfg.AppID == "" || cfg.AppSecretEncrypted == "" {
		response.Error(c, http.StatusBadRequest, "xianguanjia config not configured")
		return
	}
	appSecret, err := h.encrypt.Decrypt(cfg.AppSecretEncrypted)
	if err != nil || appSecret == "" {
		response.Error(c, http.StatusInternalServerError, "decrypt app secret failed")
		return
	}
	ctx := c.Request.Context()
	probeCli := h.probeCli
	if probeCli == nil {
		probeCli = httpClientProbe{}
	}
	probeErr := probeCli.Probe(ctx, cfg.BaseURL, cfg.AppID, appSecret)
	health := "healthy"
	if probeErr != nil {
		health = "unhealthy"
		slog.Warn("xianguanjia health check failed", "err", probeErr)
	}
	if err := h.store.UpdateHealth(ctx, health, time.Now()); err != nil {
		slog.Error("xianguanjia health check persist failed", "err", err)
		response.Error(c, http.StatusInternalServerError, "persist health status failed")
		return
	}
	response.Success(c, gin.H{"health_status": health})
}
