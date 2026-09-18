// Package service 中的 WebLoginSessionStore 实现网页版半自动登录（短信码）
// 的临时会话存储。
//
// 说明：本存储基于进程内内存（gocache），仅在【单容器/单副本部署】下安全可用。
// 若采用多副本（多个 backend 实例）部署，短信码提交请求可能落到未持有该会话的
// 实例，导致无法解析、无法续登。多副本场景须替换为 Redis 等集中式存储
// （保持相同的 Create/Resolve/Delete 接口即可）。
//
// 安全：会话仅存于内存，重启即失效，绝不落库；会话内不保存密码等敏感明文
// 之外的凭据（仅保留登录标识 email/phone 用于关联账号）。
package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	gocache "github.com/patrickmn/go-cache"
)

// ErrWebAutoLoginSessionExpired 是 Resolve 在会话不存在或已过期时返回的哨兵错误。
// 调用方应使用 errors.Is 判断，不要依赖字符串。
var ErrWebAutoLoginSessionExpired = errors.New("web login (sms) session expired or not found")

// webAutoLoginSessionTTL 半自动登录会话默认 TTL（10 分钟）。
const webAutoLoginSessionTTL = 10 * time.Minute

// webAutoLoginSessionCleanup 内存清理扫描间隔（1 分钟）。
const webAutoLoginSessionCleanup = time.Minute

// WebLoginSession 是单个网页半自动登录会话的缓存条目。
type WebLoginSession struct {
	Platform    string
	AccountID   int64
	LoginEmail  string
	LoginPhone  string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// WebLoginSessionStore 网页半自动登录会话存储（内存实现）。
type WebLoginSessionStore struct {
	cache *gocache.Cache
}

// NewWebLoginSessionStore 创建内存版半自动登录会话存储。
func NewWebLoginSessionStore() *WebLoginSessionStore {
	return &WebLoginSessionStore{
		cache: gocache.New(webAutoLoginSessionTTL, webAutoLoginSessionCleanup),
	}
}

// Create 为指定平台/账号创建一个半自动登录会话，返回会话 Token、过期时间。
// Token 使用 crypto/rand 生成 32 字节十六进制字符串。
func (s *WebLoginSessionStore) Create(platform string, accountID int64, loginEmail, loginPhone string) (token string, expires time.Time, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	token = hex.EncodeToString(buf)
	now := time.Now()
	expires = now.Add(webAutoLoginSessionTTL)
	s.cache.Set(token, &WebLoginSession{
		Platform:   platform,
		AccountID:  accountID,
		LoginEmail: loginEmail,
		LoginPhone: loginPhone,
		CreatedAt:  now,
		ExpiresAt:  expires,
	}, gocache.DefaultExpiration)
	return token, expires, nil
}

// Resolve 解析会话 Token，返回条目。未找到或已过期返回 ErrWebAutoLoginSessionExpired。
func (s *WebLoginSessionStore) Resolve(token string) (*WebLoginSession, error) {
	if token == "" {
		return nil, ErrWebAutoLoginSessionExpired
	}
	val, ok := s.cache.Get(token)
	if !ok {
		return nil, ErrWebAutoLoginSessionExpired
	}
	entry, ok := val.(*WebLoginSession)
	if !ok {
		return nil, ErrWebAutoLoginSessionExpired
	}
	return entry, nil
}

// Delete 删除会话（best-effort）。
func (s *WebLoginSessionStore) Delete(token string) {
	s.cache.Delete(token)
}
