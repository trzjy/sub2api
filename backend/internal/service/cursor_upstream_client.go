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

// ErrUpstreamKeyExpired 是上游确认"密钥额度耗尽或已过期"一类【终态】的可识别哨兵
// （裁定 2）。与 ErrUpstreamCooldown 严格区分：冷却是限流态（5min 窗口后可自动恢复），
// 而本哨兵对应激活码级终态——落库标 expired（复用既有 QuotaKeyStatusExpired，条件写口径），
// 不进入候选、不自动恢复。错误链保留上游 msg 原文供排障，但绝不据 msg 反推或伪造任何日期。
var ErrUpstreamKeyExpired = errors.New("shareai: upstream key expired or quota exhausted")

// ErrUpstreamKeyBlocked 是自动链路（reconciler, manual=false）对"未知上游失败"的统一内部哨兵
// （R13-2）。它对应"上游拒绝/结果未知"一类失败：拉号方已按既有语义把该 key 标 exhausted=true
// （casSetExhausted，落库行为不变），但此前自动链路裸 return nil 会被 reconciler 记成成功
// （心跳"reconciled N quota keys"把失败算成成功）。故改为透传含本哨兵的包裹错误，reconciler
// 以 errors.Is 识别为"失败但非停轮"（记失败级日志、不加 pulled 计数、返回 false、继续下一候选）。
// 注意：本哨兵区别于 ErrQuotaKeyMountFailed（上游已成功发号、本地挂载失败的非幂等边界，须停本轮）。
var ErrUpstreamKeyBlocked = errors.New("shareai: upstream key blocked (exhausted placeholder)")

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
//
// 裁定 2：业务码非零的错误不得带英文前缀进入手动响应。故复用 classifyUpstreamError 同款
// 分类语义（msg 本身限频/过期措辞归对应 Kind，否则 UpstreamUnknown）：message 取 resp.Msg
// 原文（handler 逐字回显），cause 承载内部上下文（activate 业务码 + 分类错误链，供 DB
// last_refresh_error 排障）。PullBatch 对 activate 错误原样透传，手动/handler 链路行为不变。
func (c *shareAIUpstreamClient) activate(ctx context.Context, code, did string) error {
	resp, err := c.fetch(ctx, "/api/activate", code, did)
	if err != nil {
		return err
	}
	if resp.Code != 0 {
		inner := classifyUpstreamError(resp.Msg)
		var upe *UpstreamPullError
		if errors.As(inner, &upe) {
			return &UpstreamPullError{
				Kind:    upe.Kind,
				message: upe.message,
				cause:   fmt.Errorf("shareai: activate code %d: %w", resp.Code, inner),
			}
		}
		// 兜底：classifyUpstreamError 恒返回 *UpstreamPullError，正常不会走到此处。
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
		// 裁定 2：不再无条件判冷却。按上游 msg 原文分类（保留内部错误分类能力）：
		//   - 限频语义（如"每5分钟内仅可刷新1次"）→ ErrUpstreamCooldown（内部 5min 窗口判定）；
		//   - 过期/耗尽语义（如"已过期"/"额度耗尽"）→ ErrUpstreamKeyExpired（标 expired）；
		//   - 未知结果 → 失败关闭（不判冷却、不判过期），错误链保留 msg 原文。
		// 禁止把中文 msg 反推成日期或写入任何日期字段。
		return nil, classifyUpstreamError(resp.Msg)
	default:
		// 裁定 2 / R3-2：非 code=1 业务码（如 code=2）的手动响应也必须原文无前缀。
		// 故同样返回 *UpstreamPullError：message 取 resp.Msg 原文（handler 逐字回显，
		// 不带 "business code" / "shareai:" 英文前缀），cause 承载业务码上下文供 DB
		// last_refresh_error 排障（落库文本不回显前端）。
		return nil, &UpstreamPullError{
			Kind:    UpstreamUnknown,
			message: resp.Msg,
			cause:   fmt.Errorf("shareai: business code %d: %w", resp.Code, errors.New(resp.Msg)),
		}
	}
}

// classifyUpstreamError 按上游 msg 原文对业务码=1 的错误做分类（裁定 2 / R1-3）。
//
// 判据完全基于 msg 文本语义（限频 / 过期耗尽 / 未知），绝不从 msg 反推或伪造日期。
// 返回的 *UpstreamPullError 携带 msg 原文（Message）并保留内部分类哨兵（errors.Is 命中
// ErrUpstreamCooldown / ErrUpstreamKeyExpired），供 pull / handler 提取原文与归类。
//
// R1-3 关键词匹配表（仅匹配肯定终态 / 明确限频语义，杜绝否定句误判）：
//   - 过期/耗尽（标 expired）：已过期、额度耗尽、已耗尽
//     删除裸词"过期"/"耗尽"——它们会命中"未过期"（含"过期"子串）等否定句；
//     删除裸词"失效"——"未失效"/"没失效" 含该子串，无法靠子串区分（R2-2）；
//   - 限频（冷却窗口）：仅可刷新、限频、冷却
//     删除裸词"刷新"——它会命中"刷新失败"（普通刷新失败文案），被误判为冷却。
//
// 分类顺序：过期/耗尽优先于限频（同 msg 同时命中两类时归过期）。
// 任何未命中上述肯定措辞的 msg（含否定句如"未过期""刷新失败"）一律走未知失败关闭分支。
func classifyUpstreamError(msg string) error {
	// R7-3（裁定 2 允许 + 上游原文逐字回显）：raw 保留未裁剪原文；m 仅用于匹配，message
	// 一律存 raw（含首尾空格等原文），分类 Kind 由 m 决定、不受空格影响。activate 业务码路径
	// 经本函数产出 UpstreamPullError 后直接透传 message（raw），故同一份原文到达手动响应。
	raw := msg
	m := strings.TrimSpace(msg)
	if m == "" {
		// 空 msg（trim 后为空）：原文为空无"原文"可显（裁定 2 允许改写），按明确失败描述处理。
		return &UpstreamPullError{Kind: UpstreamUnknown, message: "上游返回空消息", cause: fmt.Errorf("shareai: upstream code=1 with empty msg")}
	}
	// 过期/耗尽：仅肯定终态措辞（已过期 / 额度耗尽 / 已耗尽）。
	if strings.Contains(m, "已过期") || strings.Contains(m, "额度耗尽") || strings.Contains(m, "已耗尽") {
		return NewUpstreamExpiredError(raw)
	}
	// 限频：仅明确限频语义（仅可刷新 / 限频 / 冷却）。
	if strings.Contains(m, "仅可刷新") || strings.Contains(m, "限频") || strings.Contains(m, "冷却") {
		return NewUpstreamCooldownError(raw)
	}
	// 未知结果 → 失败关闭（非冷却非过期），错误链保留 msg 原文。
	return &UpstreamPullError{Kind: UpstreamUnknown, message: raw, cause: fmt.Errorf("shareai: upstream code=1 unknown result: %s", raw)}
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
