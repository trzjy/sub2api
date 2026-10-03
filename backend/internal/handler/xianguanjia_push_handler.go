package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
)

// xianguanjiaMaxBodyBytes 限制闲管家推送 body 大小。
const xianguanjiaMaxBodyBytes = 64 * 1024

// timestampFreshnessWindowSec 是闲管家官方规定的推送 timestamp 新鲜度窗口（"5分钟内有效"=300 秒）。
// 出处：reference/open-platform/api-93586387.md:36（53 个含 timestamp 页面一致）。
const timestampFreshnessWindowSec = 300

// timestampMaxFutureSkewSec 允许的最大时钟前移（防双方时钟轻微不一致误杀合法请求）。
const timestampMaxFutureSkewSec = 5

// 官方推送状态枚举（reference/open-platform/api-93586387.md:209-279）。
const (
	// xianguanjiaRefundStatusSuccess 退款成功（卖家同意或超时自动退款）。
	xianguanjiaRefundStatusSuccess int32 = 5
	// xianguanjiaOrderStatusRefunded 订单已退款。
	xianguanjiaOrderStatusRefunded int32 = 23
	// xianguanjiaOrderStatusClosed 订单已关闭。
	//
	// 【业务裁定待定项（D2 保守默认）】order_status=24 是否触发作废官方未规定、
	// 用户未裁定。当前保守默认：仅记录日志、落回执（响应 success 停止重试）、
	// **不作废任何卡**。若后续裁定 24 也作废，在此增加分支调用
	// RefundCardVoider 即可（作废动作自身幂等，重复推送无害）。
	xianguanjiaOrderStatusClosed int32 = 24
)

// XianguanjiaSignatureVerifier 校验闲管家推送请求签名（入站）。
//
// 与出站同一套 md5 逗号算法，但只用应用维度密钥（AppKey/AppSecret），
// 且 mch_id/mch_secret 不参与（开放平台文档 mch 零命中；推送 query 只列 appid/timestamp/sign）。
// 出处：reference/open-platform/doc-8472075.md:51-52（FAQ 推送签名规则）。
type XianguanjiaSignatureVerifier struct {
	cfgReader xianguanjia.ConfigReader
	decrypt   service.SecretEncryptor
}

// NewXianguanjiaSignatureVerifier 构造签名校验器。
// cfgReader 读取 active 配置（252 表），decrypt 用于解密 AppSecret（secretEncryptor）。
// 二者任一缺失、或配置为空/密钥为空时，Verify 直接返回 false（fail-closed，绝不放行）。
func NewXianguanjiaSignatureVerifier(cfgReader xianguanjia.ConfigReader, decrypt service.SecretEncryptor) *XianguanjiaSignatureVerifier {
	return &XianguanjiaSignatureVerifier{cfgReader: cfgReader, decrypt: decrypt}
}

// Verify 校验推送签名：读 active 配置（fail-closed）→ 解密 AppSecret → 校验 timestamp 新鲜度 → 四段签名比对。
func (v *XianguanjiaSignatureVerifier) Verify(ctx context.Context, params url.Values, body []byte) bool {
	if v == nil || v.cfgReader == nil || v.decrypt == nil {
		return false
	}
	cfg, err := v.cfgReader.GetActiveConfig(ctx)
	if err != nil || cfg == nil {
		// 配置缺失/读取失败：fail-closed。
		return false
	}
	appSecret, err := v.decrypt.Decrypt(cfg.AppSecretEncrypted)
	if err != nil || appSecret == "" || cfg.AppID == "" {
		return false
	}
	// timestamp 新鲜度校验（官方规定 5 分钟内有效）。
	ts, err := strconv.ParseInt(params.Get("timestamp"), 10, 64)
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	if ts > now+int64(timestampMaxFutureSkewSec) {
		return false // 时间戳过于未来
	}
	if now-ts > int64(timestampFreshnessWindowSec) {
		return false // 超出 300 秒窗口
	}
	bodyMd5 := xianguanjia.BodyMd5(body)
	expected := xianguanjia.Sign(cfg.AppID, bodyMd5, params.Get("timestamp"), appSecret)
	provided := params.Get("sign")
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

// XianguanjiaRefundVoider 退款作废执行器。*xianguanjia.RefundCardVoidService 实现。
// 语义契约：error != nil = 临时性失败（须 fail+不落回执让闲管家重试）；
// error == nil 时按 xianguanjia.VoidOutcome 分流（VoidNoCard = 查无卡，留痕+success）。
type XianguanjiaRefundVoider interface {
	VoidRefundedCards(ctx context.Context, orderNo string) (xianguanjia.VoidOutcome, error)
}

// XianguanjiaIdempotencyStore 推送幂等回执存储。
// 语义（D2 资金安全核心）：回执只在处理成功后落库（CommitReceipt），
// 失败不落回执 → 闲管家重推时 HasReceipt=false → 重做作废。
type XianguanjiaIdempotencyStore interface {
	CommitReceipt(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error)
	HasReceipt(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error)
}

// XianyuXianguanjiaPushHandler 闲管家推送 webhook 处理器。
type XianyuXianguanjiaPushHandler struct {
	svc      XianguanjiaRefundVoider
	idem     XianguanjiaIdempotencyStore
	verifier *XianguanjiaSignatureVerifier
}

// NewXianyuXianguanjiaPushHandler 构造推送处理器。
func NewXianyuXianguanjiaPushHandler(
	svc XianguanjiaRefundVoider,
	idem XianguanjiaIdempotencyStore,
	verifier *XianguanjiaSignatureVerifier,
) *XianyuXianguanjiaPushHandler {
	return &XianyuXianguanjiaPushHandler{svc: svc, idem: idem, verifier: verifier}
}

// xianguanjiaPushBody 是闲管家推送的 JSON 结构（官方 OpenAPI schema）。
// 注意：order_status/refund_status/modify_time 均为 int32（数字，不是字符串）；body 无 cards 字段。
type xianguanjiaPushBody struct {
	SellerID     int64  `json:"seller_id"`
	UserName     string `json:"user_name"`
	OrderNo      string `json:"order_no"`
	OrderType    int32  `json:"order_type"`
	OrderStatus  int32  `json:"order_status"`
	RefundStatus int32  `json:"refund_status"`
	ModifyTime   int32  `json:"modify_time"`
	ProductID    int64  `json:"product_id"`
	ItemID       int64  `json:"item_id"`
}

// pushRespond 统一输出官方推送响应体（result 必填：success/fail；msg 必填）。
// HTTP 始终 200：官方以 result 字段判定成败（api-93586387.md:280-330）。
func pushRespond(c *gin.Context, result, msg string) {
	c.JSON(http.StatusOK, gin.H{"result": result, "msg": msg})
}

// Push 处理闲管家推送（POST /api/v1/webhook/xianguanjia）。
//
// 流程：验签+timestamp → 解析 body → 查幂等回执（有则去重）→ 状态分流处理 →
// 处理成功才落回执。失败返回 result=fail（不落回执），官方契约「失败最多重试
// 3 次」驱动重推，重推时因无回执会重新执行作废——外审 F1 资金损失链的闭环修复。
// 对方超时 3 秒；作废链路含出站调用，超时由 http.Client 控制，推送 goroutine 快速返回。
func (h *XianyuXianguanjiaPushHandler) Push(c *gin.Context) {
	ctx := c.Request.Context()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, xianguanjiaMaxBodyBytes))
	if err != nil {
		response.Error(c, http.StatusBadRequest, "read body failed")
		return
	}
	params := c.Request.URL.Query()
	if h.verifier == nil || !h.verifier.Verify(ctx, params, body) {
		// 验签/timestamp 失败：官方约定失败体。不落任何回执。
		pushRespond(c, "fail", "签名失败或时间戳过期")
		return
	}
	var req xianguanjiaPushBody
	if err := json.Unmarshal(body, &req); err != nil {
		pushRespond(c, "fail", "invalid body: "+err.Error())
		return
	}
	orderNo := strings.TrimSpace(req.OrderNo)
	if orderNo == "" {
		pushRespond(c, "fail", "order_no is required")
		return
	}
	refundStatus := strconv.Itoa(int(req.RefundStatus))
	orderStatus := strconv.Itoa(int(req.OrderStatus))
	modifyTime := strconv.Itoa(int(req.ModifyTime))

	// 幂等去重（只查不写）：已有回执 = 该组合此前已处理成功，直接 success 停止重试。
	dup, err := h.idem.HasReceipt(ctx, orderNo, refundStatus, orderStatus, modifyTime)
	if err != nil {
		// 查重失败：无法判断是否处理过。作废动作自身幂等（expired 条件更新 /
		// claim refund_handled_at 锚点），重做无害；按「未处理」继续走完流程，
		// 成功后落回执（CommitReceipt 对已存在回执返回 false，同样 success）。
		slog.Error("xianguanjia push idempotency check failed, proceeding idempotently",
			"order_no", orderNo, "err", err)
	} else if dup {
		pushRespond(c, "success", "duplicate")
		return
	}

	// 状态分流。
	switch {
	case req.RefundStatus == xianguanjiaRefundStatusSuccess || req.OrderStatus == xianguanjiaOrderStatusRefunded:
		// 退款成功：按 kam/list 实际所发卡精准作废。
		outcome, verr := h.svc.VoidRefundedCards(ctx, orderNo)
		if verr != nil {
			// 临时性失败（kam/list 出站失败 / DB 失败 / 追回失败）：
			// fail + 不落回执 → 闲管家重试 → 重推重做。这是 F1 闭环点。
			slog.Error("xianguanjia refund void failed, will retry via push",
				"order_no", orderNo, "err", verr)
			pushRespond(c, "fail", "作废处理失败，请重试")
			return
		}
		if outcome == xianguanjia.VoidNoCard {
			// 查无此卡：重试无意义，success 停止重试；VoidRefundedCards 内已留痕（warn）。
			slog.Warn("xianguanjia refund void: no matching card for refunded order, recorded for audit",
				"order_no", orderNo, "refund_status", refundStatus, "order_status", orderStatus)
		}
	case req.OrderStatus == xianguanjiaOrderStatusClosed:
		// 订单已关闭（未付款关闭/超时关闭等）：官方未规定是否作废，业务未裁定，
		// 保守默认仅记录日志、不作废（见 xianguanjiaOrderStatusClosed 常量注释）。
		slog.Info("xianguanjia push: order closed, no void action (policy: observe only)",
			"order_no", orderNo)
	default:
		// 其余状态（付款/发货中等）：仅落回执记录。
	}

	// 处理成功，落回执（幂等提交：并发重复时他人已提交，返回 false 同样视为成功）。
	if _, err := h.idem.CommitReceipt(ctx, orderNo, refundStatus, orderStatus, modifyTime); err != nil {
		// 回执落库失败：业务动作已成功但去重锚点缺失。若这是重推场景（回执已存在），
		// CommitReceipt 不会报错；报错说明真失败 → fail 让闲管家重推，重推会重做
		// 作废（幂等无害），并再次尝试落回执。
		slog.Error("xianguanjia push commit receipt failed, will retry via push",
			"order_no", orderNo, "err", err)
		pushRespond(c, "fail", "回执记录失败，请重试")
		return
	}
	pushRespond(c, "success", "recorded")
}
