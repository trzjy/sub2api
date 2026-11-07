package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
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

// kn-cy 接入既有的 QuotaKeyUpstreamClient 抽象（KC-15）。
//
// 设计约束（来自派发单）：
//   - 与 ShareAI 激活码并列，走同一套号池/挂组/额度/停调，不另起一套；
//   - 形态差异：ShareAI 是 1 激活码→多 cursor oauth 账号；kn-cy 是 1 卡密→1 账号
//     （卡密激活后卡密原文本身就是 Bearer 凭据，账号 type=apikey）；
//   - 复用既有 KnCyClient.ClaimActivation 激活、复用既有 QuotaKeyEncryptor 解卡密原文，
//     绝不自写 HTTP 客户端、绝不另写解密；
//   - 卡密原文是账号凭据，走既有账号凭据加密链落库（与 quota_keys.code_encrypted 是两处
//     不同存储，各按各的既有机制落，不互相搬运）；
//   - 任何上游错误必须返回 error（绝不可返回空账号列表当成功，这是契约失败关闭要求）；
//     卡密原文绝不出现在 error 文本、Name、也不打日志（既有 KnCyClient 已脱敏）。

const (
	// knCyGatewayBaseURL 是 kn-cy.com 的 OpenAI 兼容网关根地址，作为账号凭据
	// credentials["base_url"] 的固定值。不要散落硬编码到别处。
	knCyGatewayBaseURL = "https://kn-cy.com/v1"
	// knCyAccountPlatform 是 kn-cy 账号归属平台（与 ShareAI 并列走 cursor 链路）。
	knCyAccountPlatform = "cursor"
	// knCyAccountType 是 kn-cy 账号类型（后端常量 domain.AccountTypeAPIKey = "apikey"，
	// 注意不是 "api_key"）。
	knCyAccountType = domain.AccountTypeAPIKey
)

// knCyModelMappingModels 是 kn-cy 上游实供、且需做恒等映射的三个模型。
// 顺序固定，作为 knCyIdentityModelMapping 的键集来源，避免与 ShareAI 账号那 12 个模型混淆。
var knCyModelMappingModels = []string{
	"gpt-6.1-sol",
	"claude-fable-5-1",
	"gemini-4-argon",
}

// knCyIdentityModelMapping 返回 kn-cy 实供三个模型的恒等映射（每次新建一份，
// 避免共享可变 map 被调用方改写）。cursor 平台空 model_mapping = 无可服务模型
// （account.go 失败关闭），故必须显式写这 3 个；写多了会白发请求到上游被拒。
func knCyIdentityModelMapping() map[string]any {
	m := make(map[string]any, len(knCyModelMappingModels))
	for _, name := range knCyModelMappingModels {
		m[name] = name
	}
	return m
}

// knCyQuotaKeyUpstream 实现 QuotaKeyUpstreamClient：复用既有 KnCyClient 激活卡密，
// 把"1 卡密 → 1 账号"映射为 UpstreamAccount（type=apikey）。
type knCyQuotaKeyUpstream struct {
	client *KnCyClient
	enc    QuotaKeyEncryptor
}

// NewKnCyQuotaKeyUpstreamClient 构造 kn-cy 的 QuotaKeyUpstreamClient 实现。
//
// client 可为 nil（回退到默认端点 NewKnCyClient("")）；enc 不可为 nil——加密器缺失
// 禁止静默回落明文（与 QuotaKeyService / NewShareAIUpstreamClient 同口径）。
func NewKnCyQuotaKeyUpstreamClient(client *KnCyClient, enc QuotaKeyEncryptor) (QuotaKeyUpstreamClient, error) {
	if enc == nil {
		return nil, errors.New("kncy: encryptor must not be nil")
	}
	if client == nil {
		client = NewKnCyClient("")
	}
	return &knCyQuotaKeyUpstream{client: client, enc: enc}, nil
}

// knCyAccountName 生成不泄露卡密原文的可辨识账号名。
//
// 优先用 quota_keys.code_last4（落库时已脱敏的尾号，每码稳定）；为空时回退到对卡密做
// fnv 哈希取前 8 位十六进制（哈希非原文，符合"绝不含卡密原文"约束）。
func knCyAccountName(qk *ent.QuotaKey, secret string) string {
	if qk != nil && strings.TrimSpace(qk.CodeLast4) != "" {
		return "kncy-" + strings.TrimSpace(qk.CodeLast4)
	}
	// 回退：对卡密做 fnv 哈希取前 8 位十六进制（绝不回显原文）。
	h := fnv.New64a()
	_, _ = h.Write([]byte(secret))
	sum := fmt.Sprintf("%016x", h.Sum64())
	return "kncy-" + sum[:8]
}

// parseKnCyExpiresAt 把激活响应里的到期 Unix 时间戳解析为 *time.Time。
//
// 0 或负数视为"上游未给出可用到期时间"，返回 nil（调用方约定 nil = 保持
// quota_keys.expires_at 原值不动，不得据未知推算补日期）。
func parseKnCyExpiresAt(ts int64) *time.Time {
	if ts <= 0 {
		return nil
	}
	t := time.Unix(ts, 0).UTC()
	return &t
}

// PullBatch 实现 QuotaKeyUpstreamClient：解密卡密 → ClaimActivation 激活 →
// 返回恰好 1 个 UpstreamAccount（type=apikey）。
//
// 上游任意错误（含 ClaimActivation 失败）一律返回 error 且 Accounts 为空，
// 绝不返回成功+空列表（契约失败关闭）。卡密原文不进入 error 文本、Name 或日志。
func (c *knCyQuotaKeyUpstream) PullBatch(ctx context.Context, qk *ent.QuotaKey) (*UpstreamPullResult, error) {
	if qk == nil || strings.TrimSpace(qk.CodeEncrypted) == "" {
		return nil, errors.New("kncy: quota key has no encrypted card secret")
	}

	// 复用既有 QuotaKeyEncryptor 解出卡密原文（与 ShareAI 同一条解密链，不另写）。
	plain, err := c.enc.Decrypt([]byte(qk.CodeEncrypted))
	if err != nil {
		// 卡密原文绝不外泄：错误信息不含密文内容。
		return nil, fmt.Errorf("kncy: decrypt card secret: %w", err)
	}
	secret := string(plain)

	// 复用既有 ClaimActivation 激活卡密（幂等语义由上游保证：已激活过返回既有额度，
	// 不重复消耗）。KnCyClient 已对凭据脱敏，错误文本不含卡密原文，直接透传即可。
	act, err := c.client.ClaimActivation(ctx, secret)
	if err != nil {
		return nil, err
	}

	acct := &UpstreamAccount{
		Name:     knCyAccountName(qk, secret),
		Platform: knCyAccountPlatform,
		Type:     knCyAccountType,
		Credentials: map[string]any{
			// base_url 固定 kn-cy 网关根地址（不散落硬编码）。
			"base_url": knCyGatewayBaseURL,
			// api_key = 卡密原文：上游要求 Bearer 用卡密原文；这是账号凭据，
			// 走既有账号凭据加密链落库，与 quota_keys.code_encrypted 互不搬运。
			"api_key": secret,
			// 仅含 kn-cy 实供的三个模型恒等映射（见 knCyIdentityModelMapping 注释）。
			"model_mapping": knCyIdentityModelMapping(),
		},
		// Extra 不承载额度事实——额度事实落 quota_keys.usage_snapshot，由既有观测路径负责。
		Extra: nil,
	}

	return &UpstreamPullResult{
		// kn-cy 是 1 卡 1 号，每次调用只拉 1 个号，不实现批量循环。
		Accounts:  []*UpstreamAccount{acct},
		ExpiresAt: parseKnCyExpiresAt(act.ExpiresAt),
	}, nil
}

// 编译期断言：knCyQuotaKeyUpstream 满足 QuotaKeyUpstreamClient 契约。
var _ QuotaKeyUpstreamClient = (*knCyQuotaKeyUpstream)(nil)
