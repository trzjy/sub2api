package admin

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
)

// D6e: 闲管家「货源模式」（虚拟货源提卡）admin 凭证配置入口。
// 职责：GET 脱敏展示货源授权凭证、PUT 保存。secret 永不回显明文，只回末 4 位。
//
// 与 D3 config handler 同风格（脱敏回显 + 首存必填 + 留空保留）。
// 加密落库由注入的 SupplyConfigStore 实现负责（接口层为明文语义）。
//
// 依赖说明：SupplyConfigStore 按总单约定假定由 D6a 提供；D6a 未落地时
// service/xianguanjia/supply_config.go 提供内存占位（集成点见证据 d6e.md）。

// xianguanjiaSupplyDefaultGateway 是货源网关默认基址（前端只读展示）。
const xianguanjiaSupplyDefaultGateway = "https://corealgos.com"

// XianguanjiaSupplyHandler admin 货源模式配置处理器。
// XianguanjiaSupplyConfigStore 是货源配置读写的窄接口（D6a 的 *SupplyConfigStore 实现）。
type XianguanjiaSupplyConfigStore interface {
	Get(ctx context.Context) (*xianguanjia.SupplyConfig, error)
	Save(ctx context.Context, cfg xianguanjia.SupplyConfig) error
}

// XianguanjiaSupplyHandler admin 货源模式配置处理器。
type XianguanjiaSupplyHandler struct {
	store   XianguanjiaSupplyConfigStore
	gateway string
}

// NewXianguanjiaSupplyHandler 构造处理器。store 为 nil 时各端点返回 503（fail-closed）。
func NewXianguanjiaSupplyHandler(store XianguanjiaSupplyConfigStore, gateway string) *XianguanjiaSupplyHandler {
	if strings.TrimSpace(gateway) == "" {
		gateway = xianguanjiaSupplyDefaultGateway
	}
	return &XianguanjiaSupplyHandler{store: store, gateway: gateway}
}

// xianguanjiaSupplyConfigResponse 是 GET 返回的脱敏配置：两个 secret 只回是否已设置 + 末 4 位。
type xianguanjiaSupplyConfigResponse struct {
	Configured    bool   `json:"configured"`
	SupplyAppID   string `json:"supply_app_id"`
	AppSecretSet  bool   `json:"app_secret_set"`
	AppSecretTail string `json:"app_secret_tail"`
	MchID         string `json:"mch_id"`
	MchSecretSet  bool   `json:"mch_secret_set"`
	MchSecretTail string `json:"mch_secret_tail"`
	Gateway       string `json:"gateway"`
}

// Get 返回当前货源配置（两个 secret 均脱敏，只回末 4 位）。
func (h *XianguanjiaSupplyHandler) Get(c *gin.Context) {
	if h == nil || h.store == nil {
		response.Error(c, http.StatusServiceUnavailable, "xianguanjia supply config store unavailable")
		return
	}
	cfg, err := h.store.Get(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "read xianguanjia supply config failed")
		return
	}
	if cfg == nil {
		// 未配置：返回空态，前端据此显示首次录入表单。
		response.Success(c, xianguanjiaSupplyConfigResponse{
			Configured: false,
			Gateway:    h.gateway,
		})
		return
	}
	response.Success(c, xianguanjiaSupplyConfigResponse{
		Configured:    cfg.SupplyAppID != "" && cfg.SupplyAppSecret != "" && cfg.MchID != "" && cfg.MchSecret != "",
		SupplyAppID:   cfg.SupplyAppID,
		AppSecretSet:  cfg.SupplyAppSecret != "",
		AppSecretTail: supplySecretTail(cfg.SupplyAppSecret),
		MchID:         cfg.MchID,
		MchSecretSet:  cfg.MchSecret != "",
		MchSecretTail: supplySecretTail(cfg.MchSecret),
	})
}

// supplySecretTail 取明文末 4 位展示（只含 4 字符，不含完整凭证）。
func supplySecretTail(plain string) string {
	if len(plain) > 4 {
		return plain[len(plain)-4:]
	}
	return plain
}

// xianguanjiaSupplyConfigPutRequest 是 PUT 请求体。
type xianguanjiaSupplyConfigPutRequest struct {
	SupplyAppID string `json:"supply_app_id"`
	AppSecret   string `json:"app_secret"`
	MchID       string `json:"mch_id"`
	MchSecret   string `json:"mch_secret"`
	Gateway     string `json:"gateway"`
}

// Put 保存货源配置：app_secret/mch_secret 留空表示保留原值（仅更新其他字段）；
// 首次保存（无已存密文）时对应 secret 留空报 400。
func (h *XianguanjiaSupplyHandler) Put(c *gin.Context) {
	if h == nil || h.store == nil {
		response.Error(c, http.StatusServiceUnavailable, "xianguanjia supply config store unavailable")
		return
	}
	var req xianguanjiaSupplyConfigPutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid request body")
		return
	}
	req.SupplyAppID = strings.TrimSpace(req.SupplyAppID)
	req.AppSecret = strings.TrimSpace(req.AppSecret)
	req.MchID = strings.TrimSpace(req.MchID)
	req.MchSecret = strings.TrimSpace(req.MchSecret)
	req.Gateway = strings.TrimSpace(req.Gateway)
	if req.SupplyAppID == "" {
		response.Error(c, http.StatusBadRequest, "supply_app_id is required")
		return
	}
	if req.MchID == "" {
		response.Error(c, http.StatusBadRequest, "mch_id is required")
		return
	}

	existing, err := h.store.Get(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "read xianguanjia supply config failed")
		return
	}
	appSecret := req.AppSecret
	mchSecret := req.MchSecret
	if existing != nil {
		if appSecret == "" {
			appSecret = existing.SupplyAppSecret
		}
		if mchSecret == "" {
			mchSecret = existing.MchSecret
		}
	}
	if appSecret == "" {
		response.Error(c, http.StatusBadRequest, "app_secret is required on first save")
		return
	}
	if mchSecret == "" {
		response.Error(c, http.StatusBadRequest, "mch_secret is required on first save")
		return
	}

	gateway := req.Gateway
	if gateway == "" {
		gateway = h.gateway
	}
	cfg := xianguanjia.SupplyConfig{
		SupplyAppID:     req.SupplyAppID,
		SupplyAppSecret: appSecret,
		MchID:     req.MchID,
		MchSecret: mchSecret,
	}
	if err := h.store.Save(c.Request.Context(), cfg); err != nil {
		response.Error(c, http.StatusInternalServerError, "save xianguanjia supply config failed")
		return
	}
	response.Success(c, gin.H{"saved": true})
}
