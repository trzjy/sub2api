package service

import (
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
)

// QuotaKeyEncryptor 封装激活码原文的静态加密/解密。
//
// 实现必须复用既有加密链 pkg/credcrypt（与 account_credentials_crypt.go 同款），
// 禁止新写加密机制、密钥轮换或字段封装（合同 §5 禁过度设计）。
type QuotaKeyEncryptor interface {
	Encrypt(plain []byte) ([]byte, error)
	Decrypt(cipher []byte) ([]byte, error)
}

// credcryptEncryptor 复用进程级默认 Cipher（由 credcrypt.Configure 在配置加载阶段安装）。
// 未配置主密钥时 credcrypt 退化为明文 passthrough（兼容现有部署）；
// 加密器本身为 nil 时由服务层 panic（禁止静默回落明文）。
type credcryptEncryptor struct{}

// NewCredcryptEncryptor 返回基于 pkg/credcrypt 的 QuotaKeyEncryptor 实现。
func NewCredcryptEncryptor() QuotaKeyEncryptor {
	return credcryptEncryptor{}
}

func (credcryptEncryptor) Encrypt(plain []byte) ([]byte, error) {
	out, err := credcrypt.Encrypt(string(plain))
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

func (credcryptEncryptor) Decrypt(cipher []byte) ([]byte, error) {
	out, err := credcrypt.Decrypt(string(cipher))
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}
