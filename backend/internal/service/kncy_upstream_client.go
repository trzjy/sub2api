package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 本文件实现 kn-cy.com 上游客户端（CARD-KC-08）。
//
// 设计约束（来自派发单与网关插件逆向分析结论）：
//   - 两个专有接口：
//     1. POST /api/activation/claim（Authorization: Bearer <激活码>）：激活卡密，返回
//        总额度、剩余额度、到期时间；
//     2. GET /api/usage/subscription/（Authorization: Bearer <Key>）：查询订阅用量与到期时间。
//   - 统一响应包形如 {status, message, data}，业务数据一律解析 data 字段。
//   - 非 2xx 或业务错误返回明确错误；错误信息不携带密钥/激活码原文，避免凭据落入日志。

const (
	// knCyDefaultHTTPTimeout 是 kn-cy.com 客户端默认 HTTP 超时。
	knCyDefaultHTTPTimeout = 30 * time.Second
)

// KnCyActivationData 是激活接口 data 字段（总额度/剩余额度/到期时间/单位）。
type KnCyActivationData struct {
	Status         string  `json:"status"`
	RemainingQuota float64 `json:"remaining_quota"`
	TotalQuota     float64 `json:"total_quota"`
	ExpiresAt      int64   `json:"expires_at"`
	Unit           string  `json:"unit"`
}

// KnCySubscriptionUsage 是订阅用量接口 data 字段（剩余额度/到期时间/是否不限量）。
type KnCySubscriptionUsage struct {
	TotalRemaining float64 `json:"total_remaining"`
	TodayRemaining float64 `json:"today_remaining"`
	ExpiresAt      int64   `json:"expires_at"`
	Unlimited      bool    `json:"unlimited"`
	Unit           string  `json:"unit"`
}

// KnCyClient 是 kn-cy.com 上游客户端，供管理端拉号、激活与定时额度探活使用。
type KnCyClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewKnCyClient 构造 kn-cy.com 上游客户端。baseURL 为空时回退到默认端点。
func NewKnCyClient(baseURL string) *KnCyClient {
	if baseURL == "" {
		baseURL = "https://api.kn-cy.com"
	}
	return &KnCyClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: knCyDefaultHTTPTimeout},
	}
}

// knCyResponse 是 kn-cy.com 统一响应包（{status, message, data}）。
type knCyResponse struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// ClaimActivation 用激活码向上游激活卡密，返回总额度、剩余额度与到期时间。
//
// 请求：POST {baseURL}/api/activation/claim，Header 带 Authorization: Bearer <code>。
// 非 2xx、响应不可解析、业务 status 非成功值（如 error/failed）均返回明确错误；
// 错误信息不含激活码原文。
func (c *KnCyClient) ClaimActivation(ctx context.Context, code string) (*KnCyActivationData, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("kncy: activation code is empty")
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/activation/claim", code)
	if err != nil {
		return nil, err
	}
	var out KnCyActivationData
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return nil, fmt.Errorf("kncy: decode activation data: %w", err)
	}
	return &out, nil
}

// GetSubscriptionUsage 用订阅 Key 查询上游订阅用量与到期时间。
//
// 请求：GET {baseURL}/api/usage/subscription/，Header 带 Authorization: Bearer <key>。
// 非 2xx（如 403 额度不足）、响应不可解析、业务 status 非成功值均返回明确错误；
// 错误信息不含 Key 原文。
func (c *KnCyClient) GetSubscriptionUsage(ctx context.Context, key string) (*KnCySubscriptionUsage, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("kncy: subscription key is empty")
	}
	resp, err := c.do(ctx, http.MethodGet, "/api/usage/subscription/", key)
	if err != nil {
		return nil, err
	}
	var out KnCySubscriptionUsage
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return nil, fmt.Errorf("kncy: decode subscription usage data: %w", err)
	}
	return &out, nil
}

// do 执行一次 HTTP 请求并解析统一响应包。
//
// 仅 HTTP 2xx 时解析响应体；否则返回含状态码与脱敏摘要的传输错误。
// 非 2xx 响应正文可能回显 Authorization 中的凭据，因此摘要前会先对 bearer
// 做脱敏（替换为 ***），绝不将凭据原文随错误信息回显。
// 业务 status 非成功值（空/ok/success 以外的明确失败措辞）时返回业务错误。
// 错误信息中绝不包含 bearer 凭据（code/key）原文。
func (c *KnCyClient) do(ctx context.Context, method, path, bearer string) (*knCyResponse, error) {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("kncy: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kncy: http %s: %w", url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("kncy: read response %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kncy: http %s status %d: %s", url, resp.StatusCode, kncyMaskCredential(string(raw), bearer))
	}

	var r knCyResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("kncy: decode response %s: %w", url, err)
	}
	if !kncyStatusOK(r.Status) {
		msg := strings.TrimSpace(r.Message)
		if msg == "" {
			msg = "business error"
		}
		return nil, fmt.Errorf("kncy: %s %s status %q: %s", method, path, r.Status, msg)
	}
	return &r, nil
}

// kncyStatusOK 判定业务 status 是否成功：空值视为兼容成功，明确的失败措辞视为失败。
func kncyStatusOK(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "ok", "success", "succeeded":
		return true
	default:
		return false
	}
}

// kncyTruncate 截断响应原文用于错误信息，避免超长响应撑爆日志。
func kncyTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// kncyMaskCredential 将响应正文中的 bearer 凭据脱敏后再截断，避免凭据随非 2xx
// 错误回显。bearer 为空时跳过脱敏，仅做长度截断。
func kncyMaskCredential(body, bearer string) string {
	if bearer != "" {
		body = strings.ReplaceAll(body, "Bearer "+bearer, "Bearer ***")
		body = strings.ReplaceAll(body, bearer, "***")
	}
	return kncyTruncate(body, 256)
}
