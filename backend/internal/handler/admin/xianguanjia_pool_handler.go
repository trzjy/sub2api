package admin

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// D4d: 闲管家 admin 卡种管理 + 批量推仓入口。
// 职责：建卡种（POST /pool/kind）、查询当前 kind_id（GET /pool/kind）、批量推仓（POST /pool/push）。
//
// 占位说明（集成对接点）：service 层 kind/pool 实现由并行单元 D4a/D4b 交付。
// 本单元按派发单约定签名定义最小注入接口；D4a/D4b 落地后由主会话在 wire 处
// 注入真实实现（若签名一致，可直接 wire.Bind；若类型不同需做薄适配）。
// 集成前端点注入 nil，各端点 fail-closed 返回 503，不会打到外部接口。

// XianguanjiaKindService 抽象卡种服务（D4a 交付实现）。
// 约定签名：KindCreate(ctx, name string, categoryID int64) (int64, error)。
type XianguanjiaKindService interface {
	// KindCreate 建卡种，返回新 kind_id。categoryID<=0 表示不指定分类。
	KindCreate(ctx context.Context, name string, categoryID int64) (int64, error)
	// KindCurrent 返回当前 kind_id；未设置时返回 0（由 D4a 一并提供，用于 GET 端点）。
	KindCurrent(ctx context.Context) (int64, error)
}

// XgjCardPair 是单张卡密对（占位类型；若 D4b 的 CardPair 字段一致，集成时直接替换/别名）。
type XgjCardPair struct {
	CardNo  string `json:"card_no"`
	CardPwd string `json:"card_pwd"`
}

// XgjPushResult 是批量推仓结果（占位类型；D4b 交付 PushResult 后集成时替换/别名）。
type XgjPushResult struct {
	Total     int      `json:"total"`
	Succeeded int      `json:"succeeded"`
	Failed    int      `json:"failed"`
	Failures  []string `json:"failures,omitempty"`
}

// XianguanjiaPoolPusher 抽象批量推仓（D4b 交付实现）。
// 约定签名：PushCards(ctx, kindID int64, cards []CardPair) (PushResult, error)。
type XianguanjiaPoolPusher interface {
	PushCards(ctx context.Context, kindID int64, cards []XgjCardPair) (XgjPushResult, error)
}

// xgjPushMaxBatch 单次批量推仓上限，防止超大请求拖垮外部接口。
const xgjPushMaxBatch = 500

// XianguanjiaPoolHandler admin 闲管家卡种/推仓处理器。
type XianguanjiaPoolHandler struct {
	kindSvc XianguanjiaKindService
	pusher  XianguanjiaPoolPusher
}

// NewXianguanjiaPoolHandler 构造处理器。kindSvc/pusher 任一为 nil 时对应端点返回 503（fail-closed）。
func NewXianguanjiaPoolHandler(kindSvc XianguanjiaKindService, pusher XianguanjiaPoolPusher) *XianguanjiaPoolHandler {
	return &XianguanjiaPoolHandler{kindSvc: kindSvc, pusher: pusher}
}

// xgjPoolKindCreateRequest 是 POST /pool/kind 请求体。
type xgjPoolKindCreateRequest struct {
	Name       string `json:"name"`
	CategoryID int64  `json:"category_id"`
}

// KindCreate 建卡种：name 必填，category_id 可选。
func (h *XianguanjiaPoolHandler) KindCreate(c *gin.Context) {
	if h == nil || h.kindSvc == nil {
		response.Error(c, http.StatusServiceUnavailable, "xianguanjia kind service unavailable")
		return
	}
	var req xgjPoolKindCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		response.Error(c, http.StatusBadRequest, "name is required")
		return
	}
	if req.CategoryID < 0 {
		response.Error(c, http.StatusBadRequest, "category_id must be non-negative")
		return
	}
	kindID, err := h.kindSvc.KindCreate(c.Request.Context(), req.Name, req.CategoryID)
	if err != nil {
		slog.Error("xianguanjia kind create failed", "err", err)
		response.Error(c, http.StatusInternalServerError, "create xianguanjia kind failed")
		return
	}
	response.Success(c, gin.H{"kind_id": kindID})
}

// KindGet 返回当前 kind_id（未设置时 kind_id=0）。
func (h *XianguanjiaPoolHandler) KindGet(c *gin.Context) {
	if h == nil || h.kindSvc == nil {
		response.Error(c, http.StatusServiceUnavailable, "xianguanjia kind service unavailable")
		return
	}
	kindID, err := h.kindSvc.KindCurrent(c.Request.Context())
	if err != nil {
		slog.Error("xianguanjia kind current read failed", "err", err)
		response.Error(c, http.StatusInternalServerError, "read xianguanjia kind failed")
		return
	}
	response.Success(c, gin.H{"kind_id": kindID})
}

// xgjPoolPushRequest 是 POST /pool/push 请求体。
type xgjPoolPushRequest struct {
	KindID int64         `json:"kind_id"`
	Cards  []XgjCardPair `json:"cards"`
}

// PoolPush 批量推仓：kind_id 必填，cards 非空且每张卡 card_no 必填。
func (h *XianguanjiaPoolHandler) PoolPush(c *gin.Context) {
	if h == nil || h.pusher == nil {
		response.Error(c, http.StatusServiceUnavailable, "xianguanjia pool push service unavailable")
		return
	}
	var req xgjPoolPushRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.KindID <= 0 {
		response.Error(c, http.StatusBadRequest, "kind_id is required")
		return
	}
	if len(req.Cards) == 0 {
		response.Error(c, http.StatusBadRequest, "cards is required")
		return
	}
	if len(req.Cards) > xgjPushMaxBatch {
		response.Error(c, http.StatusBadRequest, "too many cards in one batch")
		return
	}
	for i := range req.Cards {
		req.Cards[i].CardNo = strings.TrimSpace(req.Cards[i].CardNo)
		req.Cards[i].CardPwd = strings.TrimSpace(req.Cards[i].CardPwd)
		if req.Cards[i].CardNo == "" {
			response.Error(c, http.StatusBadRequest, "card_no is required for every card")
			return
		}
	}
	result, err := h.pusher.PushCards(c.Request.Context(), req.KindID, req.Cards)
	if err != nil {
		slog.Error("xianguanjia pool push failed", "kind_id", req.KindID, "cards", len(req.Cards), "err", err)
		response.Error(c, http.StatusInternalServerError, "push cards to xianguanjia pool failed")
		return
	}
	response.Success(c, result)
}
