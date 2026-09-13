package repository

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
)

// 本文件是凭证静态加密（docs/security-ban-prevention-plan.md A3-E2）在
// accountRepository 边界上的实现：
//
//   - 写路径：prepareCredentialsForStorage 对 SensitiveCredentialKeys 命中的
//     非空字符串子键加密（enc:v1: 前缀），同时计算整份凭证与 api_key 的
//     HMAC 指纹，供 ent/raw SQL 写入 credentials_mac / credentials_api_key_mac。
//   - 读路径：decryptCredentialsMap 遇前缀解密、无前缀原样返回（灰度期双形态
//     共存）。解密失败按错误上抛（fail loud），绝不把密文当明文放行。
//   - SQL 级比较：GCM nonce 随机导致密文之间不可比，所有"凭证是否一致"的
//     SQL 判断改用指纹列等值比对。指纹始终按明文计算（加密未启用的部署
//     照常维护），hmac.Key 未配置主密钥时使用固定派生种子。
//
// 空字符串值不加密（保持 ""），使既有 SQL 的"refresh_token 是否为空"类守卫
// 在加密后语义不变。

// preparedCredentials 是一次写路径准备的结果。
type preparedCredentials struct {
	// storage 是加密后的持久化形态（新 map，不修改入参）。
	storage map[string]any
	// mac 是规范化明文 JSON 的 HMAC-SHA256 hex，写入 credentials_mac。
	mac string
	// apiKeyMAC 是明文 api_key 的 HMAC-SHA256 hex，写入 credentials_api_key_mac；
	// api_key 缺失、非字符串或为空时为 nil（SQL 侧存 NULL）。
	apiKeyMAC *string
}

// canonicalCredentialsJSON 返回凭证 map 的规范化 JSON（encoding/json 对 map
// key 排序，序列化确定）。写路径与守卫两侧都用它，保证指纹可比。
func canonicalCredentialsJSON(in map[string]any) ([]byte, error) {
	if in == nil {
		in = map[string]any{}
	}
	return json.Marshal(in)
}

func credentialsMACOfJSON(canonical []byte) string {
	sum := hmac.New(sha256.New, credcrypt.MACKey())
	sum.Write(canonical)
	return hex.EncodeToString(sum.Sum(nil))
}

// credentialsMACOf 计算凭证 map（明文）的整份指纹。
func credentialsMACOf(in map[string]any) (string, error) {
	canonical, err := canonicalCredentialsJSON(in)
	if err != nil {
		return "", fmt.Errorf("canonicalize credentials: %w", err)
	}
	return credentialsMACOfJSON(canonical), nil
}

// credentialsMACOfJSONString 从已序列化的凭证 JSON 字符串（如
// GrokCredentialMutationSnapshot.CredentialsJSON）计算整份指纹。字符串会先
// 做 canonicalJSON 归一，保证与 map 序列化同构可比；归一失败返回错误。
func credentialsMACOfJSONString(raw string) (string, error) {
	canonical := canonicalJSON(raw)
	if canonical == "" {
		return "", fmt.Errorf("canonicalize credentials json failed")
	}
	return credentialsMACOfJSON([]byte(canonical)), nil
}

// credentialsApiKeyMACOf 返回凭证 map 中明文 api_key 的指纹；api_key 缺失、
// 非字符串或为空时返回 (nil, nil)。
func credentialsApiKeyMACOf(in map[string]any) (*string, error) {
	apiKey, ok := in["api_key"].(string)
	if !ok || apiKey == "" {
		return nil, nil
	}
	mac := credentialsMACOfJSON([]byte(apiKey))
	return &mac, nil
}

// encryptCredentialsValue 对单个敏感子键值加密。空值原样返回（见文件头注释）；
// 已带密文前缀的值不重复加密（幂等，防御重复加密路径）。
func encryptCredentialsValue(plain string) (string, error) {
	if plain == "" || credcrypt.IsEncrypted(plain) {
		return plain, nil
	}
	return credcrypt.Encrypt(plain)
}

// prepareCredentialsForStorage 准备一次凭证写入：加密敏感子键 + 计算指纹。
func prepareCredentialsForStorage(in map[string]any) (preparedCredentials, error) {
	plain := normalizeJSONMap(in)
	mac, err := credentialsMACOf(plain)
	if err != nil {
		return preparedCredentials{}, err
	}
	apiKeyMAC, err := credentialsApiKeyMACOf(plain)
	if err != nil {
		return preparedCredentials{}, err
	}
	storage := make(map[string]any, len(plain))
	for key, value := range plain {
		storage[key] = value
	}
	if credcrypt.Enabled() {
		for _, key := range service.SensitiveCredentialKeys {
			str, ok := storage[key].(string)
			if !ok {
				continue
			}
			encrypted, err := encryptCredentialsValue(str)
			if err != nil {
				return preparedCredentials{}, fmt.Errorf("encrypt credentials key %q: %w", key, err)
			}
			storage[key] = encrypted
		}
	}
	return preparedCredentials{storage: storage, mac: mac, apiKeyMAC: apiKeyMAC}, nil
}

// decryptCredentialsMap 解密读出的凭证：敏感子键遇 enc:v1: 前缀解密，无前缀
// 原样返回。返回新 map，不修改入参；nil 入参返回 (nil, nil)。
func decryptCredentialsMap(in map[string]any) (map[string]any, error) {
	if in == nil {
		return nil, nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	for _, key := range service.SensitiveCredentialKeys {
		str, ok := out[key].(string)
		if !ok || !credcrypt.IsEncrypted(str) {
			continue
		}
		plain, err := credcrypt.Decrypt(str)
		if err != nil {
			return nil, fmt.Errorf("decrypt credentials key %q: %w", key, err)
		}
		out[key] = plain
	}
	return out, nil
}
