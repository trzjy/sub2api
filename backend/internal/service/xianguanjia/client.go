// Package xianguanjia 封装闲管家（开放平台）对接的客户端、签名、落库与对账骨架。
//
// 安全约束（派发单 C1 强制）：
//   - 本包绝不在初始化或任何方法里向真实网络地址发起请求；真实请求仅由注入的
//     *http.Client 发起，单测用 httptest / fake RoundTripper 覆盖。
//   - 商户凭证（app_id/app_secret/mch_id/mch_secret）只作为构造参数传入，绝不写死。
//   - 除 client.go 顶部 defaultBaseURL 占位常量与注释外，open.goofish.pro 不应出现在任何位置。
package xianguanjia

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultBaseURL 是闲管家开放平台占位默认地址。
// 注意：本包绝不在初始化或任何方法里向该地址发起真实请求——真实请求由注入的
// *http.Client 在单测里用 httptest 覆盖。此常量仅作为构造默认，绝不包含真实商户凭证。
const defaultBaseURL = "https://open.goofish.pro"

const (
	xianyuExternalCardListPath      = "/api/kam/list"
	xianyuAftersaleListPath         = "/api/aftersale/list"
	xianyuAftersaleDetailPath       = "/api/aftersale/detail"
	xianyuAftersaleAgreeRefundPath  = "/api/aftersale/agree"
	xianyuAftersaleRejectRefundPath = "/api/aftersale/reject"
)

// ClientConfig 构造闲管家客户端的配置。商户凭证只能由外部注入（如环境变量），
// 本包不读取任何环境变量、也不写死真实凭证。
type ClientConfig struct {
	BaseURL    string
	AppID      string
	AppSecret  string
	MchID      string
	MchSecret  string
	HTTPClient *http.Client
}

// Client 是闲管家开放平台客户端。所有请求都经过 do 统一签名与解析。
// 客户端本身不持有任何网络状态，初始化时绝不 dial。
type Client struct {
	baseURL    string
	appID      string
	appSecret  string
	mchID      string
	mchSecret  string
	httpClient *http.Client
}

// NewClient 构造客户端。HTTPClient 为空时用 http.DefaultClient；BaseURL 为空时用占位默认。
func NewClient(cfg ClientConfig) *Client {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = defaultBaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		baseURL:    base,
		appID:      cfg.AppID,
		appSecret:  cfg.AppSecret,
		mchID:      cfg.MchID,
		mchSecret:  cfg.MchSecret,
		httpClient: hc,
	}
}

// ExternalCard 表示闲管家侧的一张卡密。
type ExternalCard struct {
	OrderNo  string  `json:"order_no"`
	CardNo   string  `json:"card_no"`
	CardPwd  string  `json:"card_pwd"`
	Cost     float64 `json:"cost"`
	SoldType string  `json:"sold_type"`
}

// AftersaleOrder 表示闲管家侧的售后/退款订单。
type AftersaleOrder struct {
	AftersaleID  string  `json:"aftersale_id"`
	OrderNo      string  `json:"order_no"`
	Status       string  `json:"status"`
	RefundAmount float64 `json:"refund_amount"`
	Reason       string  `json:"reason"`
}

// ListOrderCards 拉取某订单的外部卡密列表（对应 kam/list）。
func (c *Client) ListOrderCards(ctx context.Context, orderNo string) ([]ExternalCard, error) {
	query := url.Values{}
	query.Set("order_no", orderNo)
	body, err := c.do(ctx, http.MethodGet, xianyuExternalCardListPath, query, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code    int `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Cards []ExternalCard `json:"cards"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("xianguanjia decode list cards: %w", err)
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("xianguanjia list cards error code %d: %s", resp.Code, resp.Message)
	}
	return resp.Data.Cards, nil
}

// ListAftersaleOrders 分页拉取售后订单列表。since 为零值时不带时间过滤。
func (c *Client) ListAftersaleOrders(ctx context.Context, since time.Time, limit int) ([]AftersaleOrder, error) {
	query := url.Values{}
	if !since.IsZero() {
		query.Set("start_time", since.Format(time.RFC3339))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	body, err := c.do(ctx, http.MethodGet, xianyuAftersaleListPath, query, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code    int `json:"code"`
		Message string `json:"message"`
		Data    struct {
			List []AftersaleOrder `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("xianguanjia decode aftersales: %w", err)
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("xianguanjia list aftersales error code %d: %s", resp.Code, resp.Message)
	}
	return resp.Data.List, nil
}

// GetAftersaleOrder 拉取单个售后订单详情。
func (c *Client) GetAftersaleOrder(ctx context.Context, aftersaleID string) (*AftersaleOrder, error) {
	query := url.Values{}
	query.Set("aftersale_id", aftersaleID)
	body, err := c.do(ctx, http.MethodGet, xianyuAftersaleDetailPath, query, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code    int `json:"code"`
		Message string `json:"message"`
		Data    AftersaleOrder `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("xianguanjia decode aftersale: %w", err)
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("xianguanjia get aftersale error code %d: %s", resp.Code, resp.Message)
	}
	return &resp.Data, nil
}

// AgreeRefund 同意退款。
func (c *Client) AgreeRefund(ctx context.Context, aftersaleID string) error {
	payload, err := json.Marshal(map[string]string{"aftersale_id": aftersaleID})
	if err != nil {
		return fmt.Errorf("xianguanjia marshal agree refund: %w", err)
	}
	_, err = c.do(ctx, http.MethodPost, xianyuAftersaleAgreeRefundPath, url.Values{}, payload)
	return err
}

// RejectRefund 拒绝退款并附原因。
func (c *Client) RejectRefund(ctx context.Context, aftersaleID string, reason string) error {
	payload, err := json.Marshal(map[string]string{"aftersale_id": aftersaleID, "reason": reason})
	if err != nil {
		return fmt.Errorf("xianguanjia marshal reject refund: %w", err)
	}
	_, err = c.do(ctx, http.MethodPost, xianyuAftersaleRejectRefundPath, url.Values{}, payload)
	return err
}

// do 统一构造请求：拼 URL、加公共参数（app_id/mch_id/timestamp/nonce）、计算签名、发请求、
// 限长读响应体，非 2xx 返回带状态码的错误，否则返回原始响应体由调用方解析。
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("app_id", c.appID)
	query.Set("mch_id", c.mchID)
	query.Set("timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	query.Set("nonce", randString(16))
	// 计算签名（不含 sign 本身，sign 在签名完成后才加入）。
	sign := Sign(query, body, c.appSecret, c.mchSecret)
	query.Set("sign", sign)

	fullURL := strings.TrimRight(c.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("xianguanjia build request: %w", err)
	}
	req.URL.RawQuery = query.Encode()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("xianguanjia read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("xianguanjia %s %s returned status %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// randString 生成非加密随机字符串（仅用于 nonce，非密钥）。
func randString(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 退化方案：仅用于 nonce，不涉及任何密钥。
		for i := range b {
			b[i] = chars[int(time.Now().UnixNano())%len(chars)]
		}
		return string(b)
	}
	for i, v := range b {
		b[i] = chars[int(v)%len(chars)]
	}
	return string(b)
}
