// cockpit_preview_receipt.go —— cockpit 导入 preview/commit 无状态 HMAC 凭证（方案 §1.4-1.8，B1c）。
//
// 设计要点（contract §5：生产代码零兜底、失败关闭）：
//   - preview 解析出原始字节流摘要后签发一张无状态签名凭证（token）；commit 时校验该凭证、
//     重算原始字节流 SHA-256 与凭证内比对，再调既有 B1b 写入器。preview 零副作用。
//   - HMAC key 由既有服务端主密钥（config.JWT.Secret）经 HKDF 派生 cockpit-preview-receipt
//     专用子密钥，禁止硬编码密钥、禁止新增配置项；主密钥为空则 signer 置为未配置态，
//     Sign/Verify 一律失败关闭。
//   - 凭证绑定：操作者主体 + raw_sha256 + 解析器版本 + 签发时间 + TTL。Verify 任一不满足
//     → PREVIEW_RECEIPT_INVALID。解析语义变更必须 bump cockpitImportParserVersion，旧凭证因此自然失效。

package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

// cockpitImportParserVersion 解析器版本常量（本单定义）。
// bump 纪律：解析语义（字段校验规则、三态分类口径、同键分组规则）发生任何不兼容变更时必 bump；
// bump 后旧凭证因 Verify 校验 parser_version 一致而自然失效，强制重新 preview 后再 commit。
const cockpitImportParserVersion = 1

// CockpitImportParserVersion 暴露当前解析器版本（§1.4 版本钉死/升级纪律）。
// 测试与需要读取当前版本的其他包通过此访问器读取，避免直接依赖未导出常量。
func CockpitImportParserVersion() int { return cockpitImportParserVersion }

// cockpitPreviewReceiptTTL 凭证有效期（15 分钟）。
// 取值依据：preview 与 commit 通常为同一管理会话内连续操作；15 分钟覆盖合理的人工确认窗口，
// 同时把重放窗口限制在可接受范围；过短影响可用性、过长放大重放风险。
const cockpitPreviewReceiptTTL = 15 * time.Minute

// cockpitPreviewReceiptPurpose 是 HKDF 派生专用 key 的用途标签（密钥定义域分离）。
const cockpitPreviewReceiptPurpose = "cockpit-preview-receipt/v1"

// ErrCockpitPreviewReceiptInvalid 凭证无效（签名/主体/过期/版本任一不满足，或缺失）。
const ErrCockpitPreviewReceiptInvalid = "PREVIEW_RECEIPT_INVALID"

// ErrCockpitPreviewManifestMismatch 提交字节流与凭证内 raw_sha256 不一致。
const ErrCockpitPreviewManifestMismatch = "MANIFEST_MISMATCH"

// ErrCockpitPreviewParseTimeout 解析在 context deadline 内未完成。
const ErrCockpitPreviewParseTimeout = "PREVIEW_PARSE_TIMEOUT"

// CockpitPreviewReceipt 是无状态签名凭证的载荷（不含任何上传内容或凭证材料）。
type CockpitPreviewReceipt struct {
	Operator      string `json:"op"`  // 操作者主体（admin user id 字符串）
	RawSHA256     string `json:"sha"` // 原始字节流 SHA-256（§1.5 明文允许的摘要）
	ParserVersion int    `json:"pv"`  // 解析器版本
	IssuedAt      int64  `json:"iat"` // 签发时间（unix 秒）
	ExpiresAt     int64  `json:"exp"` // 过期时间（unix 秒）
}

// CockpitPreviewReceiptSigner 对 preview 凭证做 HMAC 无状态签名与校验。
// HMAC key 由既有服务端主密钥（config.JWT.Secret）经 HKDF 派生专用子密钥，不存储主密钥。
type CockpitPreviewReceiptSigner struct {
	key        []byte
	configured bool
}

// NewCockpitPreviewReceiptSigner 由既有服务端主密钥派生 cockpit-preview-receipt 专用子密钥。
// masterSecret 必须非空（生产由 config.JWT.Secret 提供）；为空则 signer 处于未配置态，
// Sign/Verify 一律失败关闭，避免以空密钥签发弱凭证。
func NewCockpitPreviewReceiptSigner(masterSecret string) *CockpitPreviewReceiptSigner {
	if masterSecret == "" {
		return &CockpitPreviewReceiptSigner{}
	}
	// HKDF-SHA256：以 masterSecret 为 IKM、用途标签为 info，派生 32 字节专用子密钥。
	derived := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, []byte(masterSecret), nil, []byte(cockpitPreviewReceiptPurpose)), derived); err != nil {
		// 派生失败属于不可恢复的配置错误，置为未配置态由调用方失败关闭。
		return &CockpitPreviewReceiptSigner{}
	}
	return &CockpitPreviewReceiptSigner{key: derived, configured: true}
}

// Configured 报告 signer 是否持有有效派生密钥。
func (s *CockpitPreviewReceiptSigner) Configured() bool { return s != nil && s.configured }

// Issue 为给定操作者 + raw_sha256 签发一张 TTL 内的凭证 token。
func (s *CockpitPreviewReceiptSigner) Issue(operator, rawSHA256 string) (string, error) {
	if !s.Configured() {
		return "", errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	now := time.Now().Unix()
	r := &CockpitPreviewReceipt{
		Operator:      operator,
		RawSHA256:     rawSHA256,
		ParserVersion: cockpitImportParserVersion,
		IssuedAt:      now,
		ExpiresAt:     now + int64(cockpitPreviewReceiptTTL.Seconds()),
	}
	return s.Sign(r)
}

// Sign 对凭证载荷做 HMAC-SHA256 签名，输出 base64url(payload).base64url(sig)。
func (s *CockpitPreviewReceiptSigner) Sign(r *CockpitPreviewReceipt) (string, error) {
	if !s.Configured() {
		return "", errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	if r == nil {
		return "", errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify 校验 token 的签名、主体一致、未过期、解析器版本一致；任一不满足 → 错误（消息即
// ErrCockpitPreviewReceiptInvalid，失败关闭，不区分具体原因以避免侧信道）。
func (s *CockpitPreviewReceiptSigner) Verify(token, operator string) (*CockpitPreviewReceipt, error) {
	if !s.Configured() {
		return nil, errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	if token == "" {
		return nil, errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return nil, errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	var r CockpitPreviewReceipt
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	if r.Operator != operator {
		return nil, errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	if r.ParserVersion != cockpitImportParserVersion {
		return nil, errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	if r.ExpiresAt <= 0 || time.Now().Unix() >= r.ExpiresAt {
		return nil, errors.New(ErrCockpitPreviewReceiptInvalid)
	}
	return &r, nil
}
