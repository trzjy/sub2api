package cursorupstream

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// SharedSecret 是与 Python 客户端 ApiClient._SHARED_SECRET 完全一致的共享密钥明文
const SharedSecret = "CursorLoginAssistant@2026#Sec"

var (
	ErrInvalidCiphertext = errors.New("cursorupstream: invalid ciphertext length or format")
	ErrInvalidPadding    = errors.New("cursorupstream: invalid pkcs7 padding")
)

// Crypto 负责上游 AES-128-CBC 加解密 (对齐 Python ApiClient._encrypt / _decrypt)
type Crypto struct {
	key []byte
}

func NewCrypto() *Crypto {
	hash := sha256.Sum256([]byte(SharedSecret))
	key := make([]byte, 16)
	copy(key, hash[:16]) // 截取 SHA256 前 16 字节作为 AES-128 密钥
	return &Crypto{key: key}
}

// Encrypt: 返回 base64(IV || Ciphertext)
func (c *Crypto) Encrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("cursorupstream: new cipher: %w", err)
	}

	padded := pkcs7Pad(plaintext, aes.BlockSize)
	ciphertext := make([]byte, aes.BlockSize+len(padded))
	iv := ciphertext[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", fmt.Errorf("cursorupstream: read iv: %w", err)
	}

	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ciphertext[aes.BlockSize:], padded)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt: 解析 base64(IV || Ciphertext) 并解密，去除 PKCS7 填充
func (c *Crypto) Decrypt(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("cursorupstream: base64 decode: %w", err)
	}
	if len(raw) < aes.BlockSize || (len(raw)-aes.BlockSize)%aes.BlockSize != 0 {
		return nil, ErrInvalidCiphertext
	}

	iv := raw[:aes.BlockSize]
	ct := raw[aes.BlockSize:]

	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, fmt.Errorf("cursorupstream: new cipher: %w", err)
	}

	mode := cipher.NewCBCDecrypter(block, iv)
	pt := make([]byte, len(ct))
	mode.CryptBlocks(pt, ct)

	return pkcs7Unpad(pt)
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - (len(data) % blockSize)
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(data, padtext...)
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	length := len(data)
	if length == 0 {
		return nil, ErrInvalidPadding
	}
	padding := int(data[length-1])
	if padding == 0 || padding > length || padding > aes.BlockSize {
		return nil, ErrInvalidPadding
	}
	for i := length - padding; i < length; i++ {
		if data[i] != byte(padding) {
			return nil, ErrInvalidPadding
		}
	}
	return data[:length-padding], nil
}
