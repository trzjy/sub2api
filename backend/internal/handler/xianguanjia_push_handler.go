package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
)

// xianguanjiaMaxBodyBytes 限制闲管家推送 body 大小。
const xianguanjiaMaxBodyBytes = 64 * 1024

// XianguanjiaSecretConfig 闲管家商户凭证配置。凭证只能由外部注入（环境变量），绝不写死。
type XianguanjiaSecretConfig struct {
	AppID     string
	AppSecret string
	MchID     string
	MchSecret string
}

// XianguanjiaSignatureVerifier 校验闲管家推送请求签名。
type XianguanjiaSignatureVerifier struct {
	cfg XianguanjiaSecretConfig
}

// NewXianguanjiaSignatureVerifier 构造签名校验器。
func NewXianguanjiaSignatureVerifier(cfg XianguanjiaSecretConfig) *XianguanjiaSignatureVerifier {
	return &XianguanjiaSignatureVerifier{cfg: cfg}
}

// Verify 校验请求签名。取 params 中的 sign，用 xianguanjia.Sign 重算（排除 sign 自身）后
// 做 constant-time 比较。任一密钥为空直接返回 false。
func (v *XianguanjiaSignatureVerifier) Verify(params url.Values, body []byte) bool {
	if v == nil || v.cfg.AppSecret == "" || v.cfg.MchSecret == "" {
		return false
	}
	provided := params.Get("sign")
	if provided == "" {
		return false
	}
	cp := url.Values{}
	for k, vv := range params {
		if k == "sign" {
			continue
		}
		cp[k] = vv
	}
	expected := xianguanjia.Sign(cp, body, v.cfg.AppSecret, v.cfg.MchSecret)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

// XianguanjiaRefundVoider 作废流依赖的只读/作废接口。*service.XianyuDeliveryService 已实现这两个方法。
type XianguanjiaRefundVoider interface {
	GetClaimAccountID(ctx context.Context, orderNo string) (string, error)
	ProcessRefundEvent(ctx context.Context, orderNo, accountID, status string) (string, error)
}

// XianguanjiaIdempotencyStore 推送幂等去重接口。xianguanjia.PushIdempotencyStore 实现它。
type XianguanjiaIdempotencyStore interface {
	Record(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error)
}

// XianyuXianguanjiaPushHandler 闲管家推送 webhook 处理器。
type XianyuXianguanjiaPushHandler struct {
	svc      XianguanjiaRefundVoider
	extStore xianguanjia.ExternalCardStore
	idem     XianguanjiaIdempotencyStore
	verifier *XianguanjiaSignatureVerifier
}

// NewXianyuXianguanjiaPushHandler 构造推送处理器。
func NewXianyuXianguanjiaPushHandler(
	svc XianguanjiaRefundVoider,
	extStore xianguanjia.ExternalCardStore,
	idem XianguanjiaIdempotencyStore,
	verifier *XianguanjiaSignatureVerifier,
) *XianyuXianguanjiaPushHandler {
	return &XianyuXianguanjiaPushHandler{svc: svc, extStore: extStore, idem: idem, verifier: verifier}
}

// xianguanjiaPushBody 是闲管家推送的 JSON 结构。
type xianguanjiaPushBody struct {
	OrderNo      string                     `json:"order_no"`
	RefundStatus string                     `json:"refund_status"`
	OrderStatus  string                     `json:"order_status"`
	ModifyTime   string                     `json:"modify_time"`
	Cards        []xianguanjia.ExternalCard `json:"cards"`
}

// Push 处理闲管家推送（POST /api/v1/internal/webhook/xianguanjia）。
//
// 流程：校验签名 → 解析 body → 幂等去重 → 落库卡密映射 → 退款/关闭状态触发作废流。
// 成功统一返回 2xx（Success），因为闲管家重试 3 次依赖 2xx 停止。
func (h *XianyuXianguanjiaPushHandler) Push(c *gin.Context) {
	ctx := c.Request.Context()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, xianguanjiaMaxBodyBytes))
	if err != nil {
		response.Error(c, http.StatusBadRequest, "read body failed")
		return
	}
	params := c.Request.URL.Query()
	if h.verifier == nil || !h.verifier.Verify(params, body) {
		response.Error(c, http.StatusBadRequest, "verify failed")
		return
	}
	var req xianguanjiaPushBody
	if err := json.Unmarshal(body, &req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	req.OrderNo = strings.TrimSpace(req.OrderNo)
	if req.OrderNo == "" {
		response.BadRequest(c, "order_no is required")
		return
	}

	// 幂等去重：首次返回 true 继续；重复返回 false 直接 2xx 让闲管家停止重试。
	ok, err := h.idem.Record(ctx, req.OrderNo, req.RefundStatus, req.OrderStatus, req.ModifyTime)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "record receipt failed")
		return
	}
	if !ok {
		response.Success(c, gin.H{"message": "duplicate"})
		return
	}

	// 记录卡密映射：extStore 出错记 warn 但继续，记录失败不阻断主流程（注释说明：留痕即可，不靠本步成败）。
	for _, card := range req.Cards {
		row := xianguanjia.ExternalCardRow{
			OrderNo:          req.OrderNo,
			CardNo:           strings.TrimSpace(card.CardNo),
			CardPwdEncrypted: xianguanjia.EncryptCardPwd(card.CardPwd),
			SoldType:         strings.TrimSpace(card.SoldType),
		}
		if err := h.extStore.UpsertExternalCard(ctx, row); err != nil {
			slog.Warn("xianguanjia push upsert external card failed",
				"order_no", req.OrderNo, "card_no", row.CardNo, "err", err)
		}
	}

	// 状态映射：退款(refund_status=5)或订单关闭(order_status=23)触发作废流。
	if req.RefundStatus == "5" || req.OrderStatus == "23" {
		accountID, err := h.svc.GetClaimAccountID(ctx, req.OrderNo)
		if err != nil {
			// 无领取记录（或查不到）→ 无需作废；返回 2xx 让对方停止重试。
			if errors.Is(err, service.ErrXianyuDeliveryClaimNotFound) || accountID == "" {
				slog.Warn("xianguanjia push: no claim found, skip void", "order_no", req.OrderNo)
				response.Success(c, gin.H{"message": "recorded"})
				return
			}
			// 查领取记录失败：返回 2xx（Worker 可重报，不靠 5xx 重试），错误留痕。
			slog.Error("xianguanjia push get claim account failed", "order_no", req.OrderNo, "err", err)
			response.Success(c, gin.H{"message": "recorded"})
			return
		}
		// 作废失败同样返回 2xx 停止重试，错误留痕（Worker 可重报）。
		if _, err := h.svc.ProcessRefundEvent(ctx, req.OrderNo, accountID, "refunded"); err != nil {
			slog.Error("xianguanjia push process refund failed", "order_no", req.OrderNo, "err", err)
		}
		response.Success(c, gin.H{"message": "recorded"})
		return
	}

	// 其余状态：仅落库记录（已在上面完成）。
	response.Success(c, gin.H{"message": "recorded"})
}
