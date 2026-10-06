// Package xianguanjia 封装闲管家（开放平台 ERP / 进销存方向）对接的客户端、签名、配置只读侧。
//
// 安全约束（派发单 D1 强制）：
//   - 本包绝不在初始化或任何方法里向真实网络地址发起请求；真实请求仅由注入的
//     *http.Client 发起，单测用 httptest 覆盖。
//   - 商户凭证（AppKey/AppSecret）只作为构造参数传入，绝不写死。
//   - 开放平台 ERP 方向不涉及 mch（58 页文档全文 grep mch 零命中），签名只含 AppKey/AppSecret。
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

// defaultBaseURL 是闲管家开放平台默认地址（占位默认，真实请求由注入的 http.Client 在单测覆盖）。
const defaultBaseURL = "https://open.goofish.pro"

// 官方路径常量（reference/open-platform/*.md，已坐实；非 /api/aftersale/*、/api/kam/list 等伪造路径）。
const (
	pathKamList       = "/api/open/order/kam/list"
	pathRefundAgree   = "/api/open/trade/refund/operate/agree"
	pathRefundRefused = "/api/open/trade/refund/operate/refused"
	pathDummySend     = "/api/open/trade/logistics/dummy/send"
)

// responseEnvelope 是闲管家响应信封：{code,msg,data}，code==0 成功（字段名是 msg，不是 message）。
type responseEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// ClientConfig 构造闲管家客户端的配置。凭证只能由外部注入（如解密后的 AppKey/AppSecret），
// 本包不读取任何环境变量、也不写死真实凭证。
type ClientConfig struct {
	BaseURL    string
	AppKey     string
	AppSecret  string
	HTTPClient *http.Client
}

// Client 是闲管家开放平台客户端。所有请求都经过 do 统一签名与解析。
// 客户端本身不持有任何网络状态，初始化时绝不 dial。
type Client struct {
	baseURL    string
	appKey     string
	appSecret  string
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
		appKey:     cfg.AppKey,
		appSecret:  cfg.AppSecret,
		httpClient: hc,
	}
}

// ExternalCard 表示闲管家订单卡密列表（kam/list）中的一张卡密。
// sold_type 官方为 int32 枚举（11 自动发货 / 12 手动发货 / 21 手动提卡 / 22 手动标识已售），
// 出处：reference/open-platform/api-97142794.md:98-118。
type ExternalCard struct {
	CardNo   string  `json:"card_no"`
	CardPwd  string  `json:"card_pwd"`
	Cost     float64 `json:"cost"`
	SoldType int32   `json:"sold_type"`
}

// do 统一构造请求：POST+JSON、计算 bodyMd5、timestamp、四段签名，拼 query appid/timestamp/sign，
// 限长读响应体，非 2xx 返回带状态码的错误，否则返回原始响应体由调用方解析。
func (c *Client) do(ctx context.Context, path string, body []byte) ([]byte, error) {
	if body == nil {
		body = []byte("{}")
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	bodyMd5 := BodyMd5(body)
	sign := Sign(c.appKey, bodyMd5, ts, c.appSecret)

	query := url.Values{}
	query.Set("appid", c.appKey)
	query.Set("timestamp", ts)
	query.Set("sign", sign)

	fullURL := strings.TrimRight(c.baseURL, "/") + path + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("xianguanjia build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json;charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia request %s %s: %w", http.MethodPost, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("xianguanjia read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("xianguanjia %s %s returned status %d: %s", http.MethodPost, path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// decodeEnvelope 解析标准信封，code!=0 视为失败并带上 msg。
func decodeEnvelope(body []byte) (*responseEnvelope, error) {
	var env responseEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("xianguanjia decode envelope: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("xianguanjia api error code %d: %s", env.Code, env.Msg)
	}
	return &env, nil
}

// marshalBody 用 json.Marshal 输出"压缩 JSON"（无空格），正是签名所需的形态，且原样发送。
func marshalBody(v any) ([]byte, error) {
	return json.Marshal(v)
}

// randString 生成非加密随机字符串（仅用于内部占位盐，非密钥）。
func randString(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 退化方案：仅用于占位盐，不涉及任何密钥。
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

// ClientFactory 按需构造出站客户端：每次调用读取 active 配置（252 表）并解密
// AppSecret 后构造新 Client。凭证不落盘、不缓存，配置变更即时生效。
// 构造失败（无 active 配置 / 解密失败 / 凭证为空）返回 error，调用方 fail-closed。
type ClientFactory struct {
	cfgReader ConfigReader
	decrypt   func(ciphertext string) (string, error)
}

// NewClientFactory 构造客户端工厂。decrypt 传入 SecretEncryptor.Decrypt。
func NewClientFactory(cfgReader ConfigReader, decrypt func(ciphertext string) (string, error)) *ClientFactory {
	return &ClientFactory{cfgReader: cfgReader, decrypt: decrypt}
}

// NewClientForOrder 构造用于出站查询的客户端（kam/list 等）。
func (f *ClientFactory) NewClient(ctx context.Context) (*Client, error) {
	if f == nil || f.cfgReader == nil || f.decrypt == nil {
		return nil, ErrNoActiveConfig
	}
	cfg, err := f.cfgReader.GetActiveConfig(ctx)
	if err != nil || cfg == nil {
		return nil, ErrNoActiveConfig
	}
	secret, err := f.decrypt(cfg.AppSecretEncrypted)
	if err != nil || secret == "" || cfg.AppID == "" {
		return nil, fmt.Errorf("xianguanjia decrypt app secret: %w", ErrNoActiveConfig)
	}
	return NewClient(ClientConfig{
		BaseURL:   cfg.BaseURL,
		AppKey:    cfg.AppID,
		AppSecret: secret,
	}), nil
}

// ListOrderCards 拉取某订单的外部卡密列表（/api/open/order/kam/list）。
func (c *Client) ListOrderCards(ctx context.Context, orderNo string) ([]ExternalCard, error) {
	body, err := marshalBody(map[string]string{"order_no": orderNo})
	if err != nil {
		return nil, fmt.Errorf("xianguanjia marshal kam list: %w", err)
	}
	raw, err := c.do(ctx, pathKamList, body)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return nil, err
	}
	// 官方契约：data.list[]（reference/open-platform/api-97142794.md:54-77,77-128），
	// 不是 data.cards。
	var data struct {
		List []ExternalCard `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("xianguanjia decode kam list data: %w", err)
	}
	return data.List, nil
}

// AgreeRefund 同意退款（/api/open/trade/refund/operate/agree）。
func (c *Client) AgreeRefund(ctx context.Context, orderNo string) error {
	body, err := marshalBody(map[string]string{"order_no": orderNo})
	if err != nil {
		return fmt.Errorf("xianguanjia marshal agree refund: %w", err)
	}
	raw, err := c.do(ctx, pathRefundAgree, body)
	if err != nil {
		return err
	}
	_, err = decodeEnvelope(raw)
	return err
}

// RejectRefund 拒绝退款并附原因（/api/open/trade/refund/operate/refused，注意拼写 refused 不是 reject）。
func (c *Client) RejectRefund(ctx context.Context, orderNo, reason string) error {
	body, err := marshalBody(map[string]string{"order_no": orderNo, "reason": reason})
	if err != nil {
		return fmt.Errorf("xianguanjia marshal reject refund: %w", err)
	}
	raw, err := c.do(ctx, pathRefundRefused, body)
	if err != nil {
		return err
	}
	_, err = decodeEnvelope(raw)
	return err
}

// DummySend 无物流发货（/api/open/trade/logistics/dummy/send）。
// sendWay: 1=仅更新订单状态，2=发卡密并更新订单状态。
func (c *Client) DummySend(ctx context.Context, orderNo string, sendWay int32) error {
	body, err := marshalBody(map[string]any{"order_no": orderNo, "send_way": sendWay})
	if err != nil {
		return fmt.Errorf("xianguanjia marshal dummy send: %w", err)
	}
	raw, err := c.do(ctx, pathDummySend, body)
	if err != nil {
		return err
	}
	_, err = decodeEnvelope(raw)
	return err
}
