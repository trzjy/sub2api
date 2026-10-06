// Package credcrypt 提供账号凭证的静态加密（AES-256-GCM）。
//
// 设计定案（docs/security-ban-prevention-plan.md A3-E1）：
//   - 密钥纯环境变量管理，不接 KMS，不预留 KeyProvider 抽象。
//   - CRED_ENCRYPTION_KEY：主密钥（写），32 字节 base64（openssl rand -base64 32）。
//   - CRED_ENCRYPTION_KEY_OLD：旧密钥（只读解密用，支持轮换），可选。
//   - 密文值前缀标记 Prefix（"enc:v1:"），读路径凭此前缀识别密文形态。
//   - 密钥未配置时行为退化为明文 passthrough（兼容现有部署），由配置加载处
//     输出启动警告；密钥格式非法时配置加载 fail-fast。
//
// 加密/解密函数收敛在本包内。E2 写路径对命中的敏感子键调用 Encrypt，读路径
// 对任意值调用 Decrypt（无前缀原样返回，灰度期双形态共存）；E3 存量迁移复用
// 同一组 API。
package credcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
)

const (
	// Prefix 是密文值的前缀标记，格式为 "enc:v1:" + base64(nonce || ciphertext)。
	Prefix = "enc:v1:"

	// KeyLen 是 AES-256 密钥长度（字节）。
	KeyLen = 32

	// EnvKey 是主密钥环境变量名（写路径使用）。
	EnvKey = "CRED_ENCRYPTION_KEY"
	// EnvKeyOld 是旧密钥环境变量名（仅解密用，支持密钥轮换），可选。
	EnvKeyOld = "CRED_ENCRYPTION_KEY_OLD"
)

// ErrNotConfigured 在未配置任何密钥但尝试解密带前缀密文时返回。
// 明文 passthrough 模式下遇 enc:v1: 前缀密文属于不可恢复的坏状态：
// 说明数据曾被加密，但当前进程缺少密钥，原样放行会把密文当明文发给上游。
var ErrNotConfigured = errors.New("credcrypt: no key configured but value is encrypted")

// Cipher 持有一组加解密密钥。primary 用于加密与解密，old 仅用于解密
// （轮换窗口内读取旧密钥加密的存量密文）。
type Cipher struct {
	primary cipher.AEAD
	old     cipher.AEAD // 可为 nil
}

// ParseKey 解析 base64 编码的 32 字节密钥。格式非法（base64 解码失败或
// 解码后长度非 32 字节）时返回错误，供配置加载处 fail-fast。
func ParseKey(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("credcrypt: key is not valid base64: %w", err)
	}
	if len(raw) != KeyLen {
		return nil, fmt.Errorf("credcrypt: key must decode to %d bytes, got %d", KeyLen, len(raw))
	}
	return raw, nil
}

// NewCipher 构造 Cipher。primary 必填且必须为 32 字节；old 可选（nil 表示
// 未配置旧密钥），非 nil 时同样必须为 32 字节。
func NewCipher(primary, old []byte) (*Cipher, error) {
	primaryAEAD, err := newAEAD(primary, "primary")
	if err != nil {
		return nil, err
	}
	var oldAEAD cipher.AEAD
	if old != nil {
		oldAEAD, err = newAEAD(old, "old")
		if err != nil {
			return nil, err
		}
	}
	return &Cipher{primary: primaryAEAD, old: oldAEAD}, nil
}

func newAEAD(key []byte, role string) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("credcrypt: %s key must be %d bytes, got %d", role, KeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credcrypt: build %s aes cipher: %w", role, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credcrypt: build %s gcm: %w", role, err)
	}
	return aead, nil
}

// IsEncrypted 判断值是否带有密文前缀标记。
func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, Prefix)
}

// Encrypt 加密明文，返回带 Prefix 前缀的密文值。nonce 随机生成，
// 同一明文多次加密产出不同密文。
func (c *Cipher) Encrypt(plain string) (string, error) {
	nonce := make([]byte, c.primary.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("credcrypt: generate nonce: %w", err)
	}
	sealed := c.primary.Seal(nonce, nonce, []byte(plain), nil)
	return Prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密值。无前缀的值原样返回（灰度期明文形态兼容）；带前缀的值
// 依次尝试主密钥、旧密钥解密，均失败时返回错误（含 GCM 认证失败，即密文
// 被篡改或密钥不匹配）。
func (c *Cipher) Decrypt(value string) (string, error) {
	if !IsEncrypted(value) {
		return value, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, Prefix))
	if err != nil {
		return "", fmt.Errorf("credcrypt: ciphertext is not valid base64: %w", err)
	}
	if plain, err := openWith(c.primary, raw); err == nil {
		return string(plain), nil
	}
	if c.old != nil {
		if plain, err := openWith(c.old, raw); err == nil {
			return string(plain), nil
		}
	}
	return "", errors.New("credcrypt: decrypt failed (wrong key or tampered ciphertext)")
}

func openWith(aead cipher.AEAD, raw []byte) ([]byte, error) {
	if len(raw) < aead.NonceSize() {
		return nil, errors.New("credcrypt: ciphertext too short")
	}
	nonce, sealed := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	return aead.Open(nil, nonce, sealed, nil)
}

// defaultCipher 是进程级默认 Cipher，由 Configure 在配置加载阶段设置。
var defaultCipher atomic.Pointer[Cipher]

// Configure 解析并安装进程级默认 Cipher。
//
//   - primaryB64 为空且 oldB64 为空：清除默认 Cipher，返回 (false, nil)，
//     后续包级 Encrypt/Decrypt 退化为明文 passthrough。
//   - primaryB64 为空但 oldB64 非空：配置自相矛盾，返回错误（fail-fast）。
//   - 任一密钥格式非法：返回错误（fail-fast）。
//   - 成功：返回 (true, nil)。
func Configure(primaryB64, oldB64 string) (bool, error) {
	primaryB64 = strings.TrimSpace(primaryB64)
	oldB64 = strings.TrimSpace(oldB64)
	if primaryB64 == "" {
		if oldB64 != "" {
			return false, fmt.Errorf("credcrypt: %s is set but %s is empty", EnvKeyOld, EnvKey)
		}
		defaultCipher.Store(nil)
		configureMACSeed("")
		return false, nil
	}
	primary, err := ParseKey(primaryB64)
	if err != nil {
		return false, fmt.Errorf("credcrypt: invalid %s: %w", EnvKey, err)
	}
	var old []byte
	if oldB64 != "" {
		old, err = ParseKey(oldB64)
		if err != nil {
			return false, fmt.Errorf("credcrypt: invalid %s: %w", EnvKeyOld, err)
		}
	}
	c, err := NewCipher(primary, old)
	if err != nil {
		return false, err
	}
	defaultCipher.Store(c)
	configureMACSeed(primaryB64)
	return true, nil
}

// Enabled 报告进程级默认 Cipher 是否已安装（即密钥是否已配置）。
func Enabled() bool {
	return defaultCipher.Load() != nil
}

// Encrypt 使用进程级默认 Cipher 加密。未配置密钥时原样返回明文
// （passthrough，兼容未启用加密的现有部署）。
func Encrypt(plain string) (string, error) {
	c := defaultCipher.Load()
	if c == nil {
		return plain, nil
	}
	return c.Encrypt(plain)
}

// Decrypt 使用进程级默认 Cipher 解密。无前缀的值始终原样返回；带前缀的
// 值在未配置密钥时返回 ErrNotConfigured，密钥不匹配或密文被篡改时返回错误。
func Decrypt(value string) (string, error) {
	c := defaultCipher.Load()
	if c == nil {
		if IsEncrypted(value) {
			return "", ErrNotConfigured
		}
		return value, nil
	}
	return c.Decrypt(value)
}

// macSeedDomain 是 MAC 派生的域分隔前缀，避免加密密钥在 AES-GCM 与 HMAC
// 两种原语下原样复用。
const macSeedDomain = "sub2api:credentials-mac:v1:"

// macUnconfiguredSeed 是未配置主密钥时的 MAC 种子。此时凭证本就是明文，
// MAC 仅承担 SQL 级相等比较职责，不承担保密职责。
const macUnconfiguredSeed = macSeedDomain + "unconfigured"

// macSeed 是 MAC 的派生种子，随 Configure 同步更新（主密钥 base64 或固定
// 未配置种子）。用指针快照保证 MACKey 读取到与当前 Cipher 一致的种子。
var macSeed atomic.Pointer[string]

// MACKey 返回 credentials 指纹（HMAC-SHA256）使用的 32 字节密钥。
//
// 已配置主密钥时从主密钥做域分隔派生；未配置时从固定种子派生。注意：
// 配置主密钥前后派生出的 MAC 不同——存量行的 MAC 由写入路径/E3 迁移按
// 当时的密钥状态维护，主密钥轮换后旧 MAC 不再匹配，相关 CAS 守卫按
// "不匹配"处理（安全侧失败），行被下次写入或 E3 迁移刷新。
func MACKey() []byte {
	seed := macUnconfiguredSeed
	if p := macSeed.Load(); p != nil {
		seed = *p
	}
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

func configureMACSeed(primaryB64 string) {
	seed := macUnconfiguredSeed
	if strings.TrimSpace(primaryB64) != "" {
		seed = macSeedDomain + strings.TrimSpace(primaryB64)
	}
	macSeed.Store(&seed)
}
