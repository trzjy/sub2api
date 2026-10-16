package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
)

// 本文件实现真实的 shareai 拉号客户端（D-CURSOR-038 v3）。
//
// 设计约束（来自派发单与 01-protocol-doc.md）：
//   - 激活码明文经既有 QuotaKeyEncryptor 解密；明文/凭据绝不落日志（失败关闭，无兜底）。
//   - device-id 采用 DJB2 变种（h*31+c，8 位大写 hex），由激活码确定性派生（每码稳定）。
//   - 拉号主端点 /api/refresh；首次激活可能无 metadata → 补一次 /api/activate 再 refresh。
//   - code==0 且含 metadata → zk01 解密 → UpstreamAccount；
//     code==1（含"每5分钟内仅可刷新1次"）→ 冷却（可识别错误，不标 exhausted）；
//     其它 code / HTTP 错误 → 失败关闭。
//   - 解析 key_info.expired_at（【激活码】到期时间）并随 UpstreamPullResult.KeyExpiresAt
//     上抛，供落库写 quota_keys.expires_at（B-11）。注意与号池账号的 account_expire_in
//     区分：后者在 metadata 内、描述的是账号，不是激活码。
//   - 取号链路本身（device-id 派生 / 主备端点顺序 / activate→refresh 时序 / 业务码分类）
//     一律未改；无兜底降级。

const (
	defaultPrimaryBase  = "https://cur-api.shareai.pro/quota"
	defaultFallbackBase = "http://111.231.65.234:8081/quota"
	defaultHTTPTimeout  = 180 * time.Second
)

// ErrUpstreamCooldown 是可被调用方识别的冷却错误（区别于 exhausted / 失败关闭）。
// 拉号方按既有退避处理；本客户端绝不将其误判为 exhausted 或下游账号耗尽。
var ErrUpstreamCooldown = errors.New("shareai: upstream cooldown, retry later")

// errNoMetadata 是内部哨兵：refresh 成功但无 metadata（需先 activate）。
var errNoMetadata = errors.New("shareai: refresh succeeded but no metadata (need activate)")

// ShareAIClientOption 用于测试或自定义装配。
type ShareAIClientOption func(*shareAIUpstreamClient)

// WithEndpoints 覆盖主/备 base（测试用）。
func WithEndpoints(primary, fallback string) ShareAIClientOption {
	return func(c *shareAIUpstreamClient) {
		if primary != "" {
			c.primaryBase = primary
		}
		if fallback != "" {
			c.fallbackBase = fallback
		}
	}
}

// WithHTTPClient 覆盖 HTTP 客户端（测试用，可注入短超时）。
func WithHTTPClient(hc *http.Client) ShareAIClientOption {
	return func(c *shareAIUpstreamClient) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// shareAIUpstreamClient 实现 QuotaKeyUpstreamClient：向上游 shareai 拉取账号。
type shareAIUpstreamClient struct {
	enc          QuotaKeyEncryptor
	httpClient   *http.Client
	primaryBase  string
	fallbackBase string
}

// NewShareAIUpstreamClient 构造真实拉号客户端。
// enc 为 nil 时返回错误：加密器缺失禁止静默回落明文（与 QuotaKeyService 同口径）。
func NewShareAIUpstreamClient(enc QuotaKeyEncryptor, opts ...ShareAIClientOption) (QuotaKeyUpstreamClient, error) {
	if enc == nil {
		return nil, errors.New("shareai: encryptor must not be nil")
	}
	c := &shareAIUpstreamClient{
		enc:          enc,
		httpClient:   &http.Client{Timeout: defaultHTTPTimeout},
		primaryBase:  defaultPrimaryBase,
		fallbackBase: defaultFallbackBase,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// shareAIResponse 是 shareai 统一响应包（{code,msg,data}）。
type shareAIResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Metadata  string          `json:"metadata"`
		Mail      string          `json:"mail"`
		Timestamp json.RawMessage `json:"timestamp"` // 数字或字符串，统一规整为字符串
		OemInfo   json.RawMessage `json:"oem_info"`
		KeyInfo   shareAIKeyInfo  `json:"key_info"`
	} `json:"data"`
}

// shareAIKeyInfo 是 data.key_info（激活码自身的额度/到期信息）。
//
// 关键区分：key_info.expired_at 是【激活码】的到期时间（quota_keys.expires_at 的事实源）；
// 而 buildAccount 解出的 account_expire_in 是【号池账号】的有效期，两者不可混用。
// 整段其余字段（group / quota / device_count …）当前无消费方，保持已声明但不读取。
type shareAIKeyInfo struct {
	ExpiredAt *string `json:"expired_at"`
}

// parseKeyExpiresAt 解析 key_info.expired_at 为 quota_keys.expires_at 的写入值。
//
// 返回 nil 表示"上游未给出可用到期时间"，调用方据此保持 expires_at 原值不动
// （绝不据未知推算补一个假日期）。解析失败同样按未知处理，不让格式异常阻断拉号主流程。
func parseKeyExpiresAt(raw *string) *time.Time {
	if raw == nil {
		return nil
	}
	s := strings.TrimSpace(*raw)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// PullBatch 实现 QuotaKeyUpstreamClient：解密激活码 → refresh（必要时 activate）→ 解密 metadata。
func (c *shareAIUpstreamClient) PullBatch(ctx context.Context, qk *ent.QuotaKey) (*UpstreamPullResult, error) {
	if qk == nil || strings.TrimSpace(qk.CodeEncrypted) == "" {
		return nil, errors.New("shareai: quota key has no encrypted activation code")
	}

	plain, err := c.enc.Decrypt([]byte(qk.CodeEncrypted))
	if err != nil {
		// 明文绝不外泄：错误信息不含 code 内容。
		return nil, fmt.Errorf("shareai: decrypt activation code: %w", err)
	}
	code := string(plain)
	did := deviceID(code)

	acct, expiresAt, err := c.refresh(ctx, code, did)
	if errors.Is(err, errNoMetadata) {
		// 首次激活：先 activate 再 refresh。
		if actErr := c.activate(ctx, code, did); actErr != nil {
			return nil, actErr
		}
		acct, expiresAt, err = c.refresh(ctx, code, did)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	return &UpstreamPullResult{Accounts: []*UpstreamAccount{acct}, ExpiresAt: expiresAt}, nil
}

// refresh 调用 /api/refresh 并解析响应。
// 第二个返回值是 key_info.expired_at 解析出的激活码到期时间（nil = 上游未给出）。
func (c *shareAIUpstreamClient) refresh(ctx context.Context, code, did string) (*UpstreamAccount, *time.Time, error) {
	resp, err := c.fetch(ctx, "/api/refresh", code, did)
	if err != nil {
		return nil, nil, err
	}
	acct, err := c.handleResponse(resp)
	if err != nil {
		return nil, nil, err
	}
	return acct, parseKeyExpiresAt(resp.Data.KeyInfo.ExpiredAt), nil
}

// activate 调用 /api/activate（首次激活，无 metadata 时补一次）。
func (c *shareAIUpstreamClient) activate(ctx context.Context, code, did string) error {
	resp, err := c.fetch(ctx, "/api/activate", code, did)
	if err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("shareai: activate code %d: %s", resp.Code, resp.Msg)
	}
	return nil
}

// handleResponse 把 shareai 业务响应翻译为账号或错误。
func (c *shareAIUpstreamClient) handleResponse(resp *shareAIResponse) (*UpstreamAccount, error) {
	switch resp.Code {
	case 0:
		if strings.TrimSpace(resp.Data.Metadata) == "" {
			return nil, errNoMetadata
		}
		return c.buildAccount(resp.Data.Metadata, resp.Data.Mail, resp.Data.Timestamp)
	case 1:
		// 冷却（如"每5分钟内仅可刷新1次"）：可识别错误，不标 exhausted。
		return nil, fmt.Errorf("%w: %s", ErrUpstreamCooldown, resp.Msg)
	default:
		return nil, fmt.Errorf("shareai: business code %d: %s", resp.Code, resp.Msg)
	}
}

// buildAccount 解密 metadata 并映射为 UpstreamAccount。
func (c *shareAIUpstreamClient) buildAccount(metadata, mail string, tsRaw json.RawMessage) (*UpstreamAccount, error) {
	ts := normalizeTimestamp(tsRaw)
	out, err := decryptZk01(metadata, mail, ts)
	if err != nil {
		return nil, fmt.Errorf("shareai: decrypt metadata: %w", err)
	}
	var dec struct {
		Mail            string `json:"mail"`
		AccessToken     string `json:"access_token"`
		RefreshToken    string `json:"refresh_token"`
		Role            int    `json:"role"`
		AccountExpireIn *int64 `json:"account_expire_in"`
		Mode            int    `json:"mode"`
	}
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		return nil, fmt.Errorf("shareai: decode decrypted account: %w", err)
	}

	name := dec.Mail
	if name == "" {
		name = mail
	}
	return &UpstreamAccount{
		Name:     name,
		Platform: "cursor",
		Type:     "oauth",
		Credentials: map[string]any{
			"mail":              dec.Mail,
			"access_token":      dec.AccessToken,
			"refresh_token":     dec.RefreshToken,
			"account_expire_in": dec.AccountExpireIn,
		},
		Extra: map[string]any{
			"role": dec.Role,
			"mode": dec.Mode,
		},
	}, nil
}

// fetch 先打主端点，传输/HTTP 错误再退备端点；业务码错误直接返回（不重试）。
func (c *shareAIUpstreamClient) fetch(ctx context.Context, path, code, did string) (*shareAIResponse, error) {
	var lastErr error
	for _, base := range []string{c.primaryBase, c.fallbackBase} {
		resp, err := c.httpDo(ctx, base+path, code, did)
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("shareai: all endpoints failed for %s: %w", path, lastErr)
}

// httpDo 执行一次 HTTP 请求并解析统一响应包。仅当 HTTP 2xx 时解析；否则返回传输错误。
func (c *shareAIUpstreamClient) httpDo(ctx context.Context, url, code, did string) (*shareAIResponse, error) {
	body := bytes.NewReader([]byte("{}"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("shareai: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("key", code)
	req.Header.Set("device-id", did)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shareai: http %s: %w", url, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("shareai: http %s status %d: %s", url, resp.StatusCode, truncate(string(raw), 256))
	}

	var r shareAIResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("shareai: decode %s: %w", url, err)
	}
	return &r, nil
}

// deviceID 按协议文档的 DJB2 变种（h*31+c）生成 8 位大写 hex device-id。
// 由激活码确定性派生，保证每码稳定（"本地可重放"）。
func deviceID(code string) string {
	var h int32
	for i := 0; i < len(code); i++ {
		h = (h << 5) - h + int32(code[i])
	}
	v := int64(h)
	if v < 0 {
		v = -v
	}
	return fmt.Sprintf("%08X", uint32(v))
}

// normalizeTimestamp 把 metadata.timestamp（可能是数字或字符串）规整为字符串。
// zk01 要求 timestamp 作为字符串传入，传数字会 wasm memory access out of bounds。
func normalizeTimestamp(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return strconv.FormatInt(int64(f), 10)
	}
	return strings.Trim(string(raw), "\"")
}
