package service

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// XianGuanJiaClient 封装主程序到闲管家开放平台（open.goofish.pro）的调用。
//
// 对接模型：闲鱼账号由商家在闲管家后台预先绑定并授权，开放平台按"商家维度"
// 提供查询店铺（授权账号）、商品、订单、推送通知等能力。主程序通过 app_id/
// app_secret（+ mch_id/mch_secret 货源商户）签名后调用。
//
// 签名规则（官方）：
//
//	bodyMd5 = md5(JSON Body)              // 无 body 时 md5("{}")
//	sign    = md5("app_id,app_secret,bodyMd5,timestamp,mch_id,mch_secret")
//
// 签名与时间戳、seller_id 作为 query 参数随请求发送；全部接口返回
// { code, msg, data } 信封，code == 0 表示成功。
//
// 所有接口失败必须返回可展示的错误，不得静默成功。
type XianGuanJiaClient struct {
	httpClient *http.Client
	baseURL    string
	appID      string
	appSecret  string
	mchID      string
	mchSecret  string
	timeout    time.Duration
}

// NewXianGuanJiaClient 创建闲管家开放平台客户端。
// baseURL 为空时使用官方生产环境 https://open.goofish.pro；timeout <= 0 时默认 15s。
func NewXianGuanJiaClient(baseURL, appID, appSecret, mchID, mchSecret string, timeout time.Duration) *XianGuanJiaClient {
	if baseURL == "" {
		baseURL = "https://open.goofish.pro"
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &XianGuanJiaClient{
		httpClient: &http.Client{Timeout: timeout, Transport: transport},
		baseURL:    strings.TrimRight(baseURL, "/"),
		appID:      appID,
		appSecret:  appSecret,
		mchID:      mchID,
		mchSecret:  mchSecret,
		timeout:    timeout,
	}
}

// XianGuanJiaError 表示闲管家开放平台返回的可展示错误。
type XianGuanJiaError struct {
	StatusCode int
	Code       string // 闲管家业务错误码（0 成功；401 签名错误；403 IP 白名单；1000-1209 业务错误）
	Message    string
}

func (e *XianGuanJiaError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Code != "" {
		return fmt.Sprintf("xianguanjia error %d %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("xianguanjia error %d: %s", e.StatusCode, e.Message)
}

// ==================== 开放平台签名（官方 MD5 算法） ====================

// sign 计算开放平台请求签名：md5("app_id,app_secret,bodyMd5,timestamp,mch_id,mch_secret")。
// body 为空时按官方规则 bodyMd5 = md5("{}")。
func (c *XianGuanJiaClient) sign(timestamp int64, body any) string {
	bodyStr := "{}"
	if body != nil {
		if raw, err := json.Marshal(body); err == nil {
			bodyStr = string(raw)
		}
	}
	bodyMd5 := md5Hex(bodyStr)
	signStr := fmt.Sprintf("%s,%s,%s,%d,%s,%s", c.appID, c.appSecret, bodyMd5, timestamp, c.mchID, c.mchSecret)
	return md5Hex(signStr)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// buildQuery 构造带签名参数的 query：appid/timestamp/sign（+seller_id 可选）。
// sellerID 用于"商家ID（仅商务对接传入，自研/第三方ERP对接忽略即可）"，一般可空。
func (c *XianGuanJiaClient) buildQuery(timestamp int64, sign, sellerID string) string {
	q := url.Values{}
	q.Set("appid", c.appID)
	q.Set("timestamp", strconv.FormatInt(timestamp, 10))
	if sign != "" {
		q.Set("sign", sign)
	}
	if sellerID != "" {
		q.Set("seller_id", sellerID)
	}
	return q.Encode()
}

// doRequest 执行一次开放平台调用（POST + JSON body + 签名 query）。
func (c *XianGuanJiaClient) doRequest(ctx context.Context, path string, body any, sellerID string) ([]byte, error) {
	if c == nil || c.baseURL == "" || c.httpClient == nil {
		return nil, ErrXianyuDeliveryNotConfigured
	}
	var rawBody []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal xianguanjia request: %w", err)
		}
		rawBody = raw
	}
	timestamp := time.Now().Unix()
	sign := c.sign(timestamp, body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path+"?"+c.buildQuery(timestamp, sign, sellerID), bytes.NewReader(rawBody))
	if err != nil {
		return nil, fmt.Errorf("build xianguanjia request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if len(rawBody) == 0 {
		// 官方要求无 body 时也传 "{}"（bodyMd5 = md5("{}")）。
		req.Body = io.NopCloser(strings.NewReader("{}"))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrXianyuWorkerTimeout
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, ErrXianyuWorkerTimeout
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return nil, ErrXianyuWorkerUnreachable
		}
		return nil, ErrXianyuWorkerUncertain
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read xianguanjia response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.mapError(resp.StatusCode, payload)
	}
	return payload, nil
}

// do 执行一次开放平台调用并把响应 data 子对象解码到 out。
// 信封：{ code, msg, data }；code == 0 成功。
func (c *XianGuanJiaClient) do(ctx context.Context, path string, body any, out any, sellerID string) error {
	payload, err := c.doRequest(ctx, path, body, sellerID)
	if err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ErrXianyuWorkerMalformed
	}
	if envelope.Code != 0 {
		return &XianGuanJiaError{StatusCode: http.StatusOK, Code: strconv.Itoa(envelope.Code), Message: envelope.Msg}
	}
	if out == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return ErrXianyuWorkerMalformed
	}
	return nil
}

func (c *XianGuanJiaClient) mapError(statusCode int, payload []byte) error {
	var wrapped struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	msg := strings.TrimSpace(string(payload))
	if len(payload) > 0 && json.Unmarshal(payload, &wrapped) == nil {
		if wrapped.Msg != "" {
			msg = strings.TrimSpace(wrapped.Msg)
		}
	}
	return &XianGuanJiaError{
		StatusCode: statusCode,
		Code:       strconv.Itoa(wrapped.Code),
		Message:    msg,
	}
}

// ==================== 店铺（授权账号） ====================

// XianGuanJiaShop 表示闲管家侧已授权的一个闲鱼店铺（账号）。
type XianGuanJiaShop struct {
	AuthorizeID      int64  `json:"authorize_id"`
	AuthorizeExpires int64  `json:"authorize_expires"`
	UserIdentity     string `json:"user_identity"` // 闲鱼号唯一标识（H8Kx1...）
	UserName         string `json:"user_name"`     // 闲鱼会员名（tb924...）
	UserNick         string `json:"user_nick"`     // 闲鱼号昵称
	ShopName         string `json:"shop_name"`
	IsPro            bool   `json:"is_pro"`
	IsValid          bool   `json:"is_valid"`
	ValidEndTime     int64  `json:"valid_end_time"`
	ItemBizTypes     string `json:"item_biz_types"`
}

// ListShops 查询已授权的闲鱼店铺列表（对应 xianyu_accounts 投影数据源）。
func (c *XianGuanJiaClient) ListShops(ctx context.Context) ([]XianGuanJiaShop, error) {
	var out struct {
		List []XianGuanJiaShop `json:"list"`
	}
	if err := c.do(ctx, "/api/open/user/authorize/list", nil, &out, ""); err != nil {
		return nil, err
	}
	return out.List, nil
}

// ==================== 商品 ====================

// XianGuanJiaProduct 表示闲管家侧在售商品（基础信息）。
type XianGuanJiaProduct struct {
	ProductID   int64  `json:"product_id"`
	ItemID      int64  `json:"item_id"`
	Title       string `json:"title"`
	ProductType int    `json:"product_type"`
	SaleStatus  int    `json:"sale_status"` // 1 待发布 2 销售中 3 已下架
	Price       string `json:"price"`
	Stock       int    `json:"stock"`
}

// ListProducts 拉取商品列表（分页；status 为空返回全部未删除商品）。
// 对应 Worker 侧在售商品投影能力。
func (c *XianGuanJiaClient) ListProducts(ctx context.Context, pageNo, pageSize int, saleStatus int) ([]XianGuanJiaProduct, error) {
	if pageNo <= 0 {
		pageNo = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	body := map[string]any{
		"page_no":   pageNo,
		"page_size": pageSize,
	}
	if saleStatus > 0 {
		body["sale_status"] = saleStatus
	}
	var out struct {
		List []XianGuanJiaProduct `json:"list"`
	}
	if err := c.do(ctx, "/api/open/product/list", body, &out, ""); err != nil {
		return nil, err
	}
	return out.List, nil
}

// GetProductDetail 查询商品详情（含描述正文等完整信息）。
func (c *XianGuanJiaClient) GetProductDetail(ctx context.Context, productID int64) (map[string]any, error) {
	var out map[string]any
	if err := c.do(ctx, "/api/open/product/detail", map[string]any{"product_id": productID}, &out, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// ==================== 订单（对账数据源） ====================

// XianGuanJiaOrder 表示闲管家侧订单。
type XianGuanJiaOrder struct {
	OrderNo      string `json:"order_no"`
	OrderStatus  int    `json:"order_status"`  // 11 待付款 12 待发货 21 已发货 22 已完成 23 已退款 24 已关闭
	RefundStatus int    `json:"refund_status"` // 0 未申请 5 退款成功 ...
	OrderType    int    `json:"order_type"`
	ProductID    int64  `json:"product_id"`
	ItemID       int64  `json:"item_id"`
	UserName     string `json:"user_name"`
	BuyerNick    string `json:"buyer_nick"`
	BuyerID      string `json:"buyer_id"`
	Amount       string `json:"amount"`
	Quantity     int    `json:"quantity"`
	ModifyTime   int64  `json:"modify_time"`
}

// ListOrders 分页拉取订单列表（发货对账任务用；可带 update_time 区间增量）。
func (c *XianGuanJiaClient) ListOrders(ctx context.Context, pageNo, pageSize int, since time.Time) ([]XianGuanJiaOrder, error) {
	if pageNo <= 0 {
		pageNo = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	body := map[string]any{
		"page_no":   pageNo,
		"page_size": pageSize,
	}
	if !since.IsZero() {
		body["update_time"] = map[string]any{
			"start": since.Unix(),
		}
	}
	var out struct {
		List []XianGuanJiaOrder `json:"list"`
	}
	if err := c.do(ctx, "/api/open/order/list", body, &out, ""); err != nil {
		return nil, err
	}
	return out.List, nil
}

// GetOrderCards 查询订单卡密列表（虚拟商品自动发货后用于对账/补发）。
func (c *XianGuanJiaClient) GetOrderCards(ctx context.Context, orderNo string) ([]string, error) {
	var out struct {
		Cards []string `json:"cards"`
	}
	if err := c.do(ctx, "/api/open/order/cards", map[string]any{"order_no": orderNo}, &out, ""); err != nil {
		return nil, err
	}
	return out.Cards, nil
}

// ==================== 推送回调（pushUrl） ====================

// XianGuanJiaOrderNotify 是闲管家订单推送通知的载荷（pushUrl 回调 body）。
// 订单信息/订单状态/退款状态发生变更时推送；失败最多重试三次，响应需返回 result=success。
type XianGuanJiaOrderNotify struct {
	SellerID      int64  `json:"seller_id"`
	UserName      string `json:"user_name"`
	OrderNo       string `json:"order_no"`
	OrderType     int    `json:"order_type"`
	OrderStatus   int    `json:"order_status"`
	RefundStatus  int    `json:"refund_status"`
	ModifyTime    int64  `json:"modify_time"`
	ProductID     int64  `json:"product_id"`
	ItemID        int64  `json:"item_id"`
	AddressStatus int    `json:"address_status"`
}

// ==================== 健康检查 ====================

// Health 检查闲管家开放平台连通性（调店铺列表接口探活）。
func (c *XianGuanJiaClient) Health(ctx context.Context) (*XianyuWorkerHealth, error) {
	if _, err := c.ListShops(ctx); err != nil {
		return nil, err
	}
	return &XianyuWorkerHealth{Backend: true, WebSocket: true, Database: true}, nil
}
