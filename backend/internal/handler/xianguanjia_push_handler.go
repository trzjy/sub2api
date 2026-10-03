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
	SellerID    int64  `json:"seller_id"`
	UserName    string `json:"user_name"`
	OrderNo     string `json:"order_no"`
	OrderType   int32  `json:"order_type"`
	OrderStatus int32  `json:"order_status"`
	RefundStatus int32 `json:"refund_status"`
	ModifyTime  int32  `json:"modify_time"`
	ProductID   int64  `json:"product_id"`
	ItemID      int64  `json:"item_id"`
}

// Push 处理闲管家推送（POST /api/v1/webhook/xianguanjia，真实注册点见 routes/common.go:46）。
//
// 流程：校验签名+timestamp → 解析 body → 幂等去重 → 退款/关闭状态触发作废流。
// 成功统一返回 JSON {"result":"success","msg":"..."}；验签/timestamp 失败返回 {"result":"fail","msg":"..."}。
// 重逻辑（作废）不在此同步阻塞——对方超时 3 秒且最多重试 3 次，接收要快。
func (h *XianyuXianguanjiaPushHandler) Push(c *gin.Context) {
	ctx := c.Request.Context()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, xianguanjiaMaxBodyBytes))
	if err != nil {
		response.Error(c, http.StatusBadRequest, "read body failed")
		return
	}
	params := c.Request.URL.Query()
	if h.verifier == nil || !h.verifier.Verify(ctx, params, body) {
		// 验签/timestamp 失败：返回官方约定的失败体（result=fail），让闲管家按重试策略处理。
		c.JSON(http.StatusOK, gin.H{"result": "fail", "msg": "签名失败或时间戳过期"})
		return
	}
	var req xianguanjiaPushBody
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusOK, gin.H{"result": "fail", "msg": "invalid body: " + err.Error()})
		return
	}
	orderNo := strings.TrimSpace(req.OrderNo)
	if orderNo == "" {
		c.JSON(http.StatusOK, gin.H{"result": "fail", "msg": "order_no is required"})
		return
	}

	// 幂等去重：首次返回 true 继续；重复返回 false 直接返回 success 让闲管家停止重试。
	ok, err := h.idem.Record(ctx, orderNo,
		strconv.Itoa(int(req.RefundStatus)),
		strconv.Itoa(int(req.OrderStatus)),
		strconv.Itoa(int(req.ModifyTime)))
	if err != nil {
		slog.Error("xianguanjia push record receipt failed", "order_no", orderNo, "err", err)
		c.JSON(http.StatusOK, gin.H{"result": "success", "msg": "recorded"})
		return
	}
	if !ok {
		c.JSON(http.StatusOK, gin.H{"result": "success", "msg": "duplicate"})
		return
	}

	// 状态映射：退款成功(refund_status=5)或订单已退款(order_status=23)触发作废流。
	// 注：order_status=24(已关闭)是否也作废属业务决策，不在契约内（BLOCKED 顶回项），本批不触发。
	if req.RefundStatus == 5 || req.OrderStatus == 23 {
		accountID, err := h.svc.GetClaimAccountID(ctx, orderNo)
		if err != nil {
			if errors.Is(err, service.ErrXianyuDeliveryClaimNotFound) || accountID == "" {
				c.JSON(http.StatusOK, gin.H{"result": "success", "msg": "recorded"})
				return
			}
			// 查领取记录失败：返回 success 停止重试，错误留痕（Worker 可重报）。
			slog.Error("xianguanjia push get claim account failed", "order_no", orderNo, "err", err)
			c.JSON(http.StatusOK, gin.H{"result": "success", "msg": "recorded"})
			return
		}
		// 作废失败同样返回 success 停止重试，错误留痕（Worker 可重报）。
		if _, err := h.svc.ProcessRefundEvent(ctx, orderNo, accountID, "refunded"); err != nil {
			slog.Error("xianguanjia push process refund failed", "order_no", orderNo, "err", err)
		}
		c.JSON(http.StatusOK, gin.H{"result": "success", "msg": "recorded"})
		return
	}

	// 其余状态：仅落库记录（已在上面完成）。
	c.JSON(http.StatusOK, gin.H{"result": "success", "msg": "recorded"})
}
