// Package service 中的 WebLoginCaptureStore 实现网页版登录自动 Cookie 捕获的
// 临时会话存储。
//
// 说明：本存储基于进程内内存（gocache），仅在【单容器/单副本部署】下安全可用。
// 若采用多副本（多个 backend 实例）部署，反向代理请求可能落到未持有该会话的
// 实例，导致 Token 无法解析、Cookie 无法回写。多副本场景须替换为 Redis 等
// 集中式存储（保持相同的 Create/Resolve/SetCookie/Cookie/Touch/Delete 接口即可）。
package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	gocache "github.com/patrickmn/go-cache"
)

// ErrWebLoginSessionExpired 是 Resolve 在会话不存在或已过期时返回的哨兵错误。
// 调用方应使用 errors.Is 判断，不要依赖字符串。
var ErrWebLoginSessionExpired = errors.New("web login session expired or not found")

// webLoginSessionTTL 会话默认 TTL（10 分钟）。
const webLoginSessionTTL = 10 * time.Minute

// webLoginSessionCleanup 内存清理扫描间隔（1 分钟）。
const webLoginSessionCleanup = time.Minute

// webLoginCaptureEntry 是单个网页登录捕获会话的缓存条目。
//
// baseline 是会话隔离基线：会话首个代理请求时浏览器已携带的平台 Cookie 值
// （上一会话/浏览器遗留登录态）。与基线相同的入站 Cookie 值视为遗留登录态，
// 不参与捕获，防止「新建账号实际复用上一账号网页会话」被伪装成新账号。
type webLoginCaptureEntry struct {
	Platform    string
	Cookie      string
	CapturedAt  time.Time
	ExpiresAt   time.Time
	baseline    map[string]string
	baselineSet bool
}

// WebLoginCaptureStore 网页登录捕获会话存储（内存实现）。
type WebLoginCaptureStore struct {
	cache *gocache.Cache
}

// NewWebLoginCaptureStore 创建内存版捕获会话存储。
func NewWebLoginCaptureStore() *WebLoginCaptureStore {
	return &WebLoginCaptureStore{
		cache: gocache.New(webLoginSessionTTL, webLoginSessionCleanup),
	}
}

// Create 为指定平台创建一个捕获会话，返回会话 Token、过期时间。
// Token 使用 crypto/rand 生成 32 字节十六进制字符串。
func (s *WebLoginCaptureStore) Create(platform string) (token string, expires time.Time, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	token = hex.EncodeToString(buf)
	expires = time.Now().Add(webLoginSessionTTL)
	s.cache.Set(token, &webLoginCaptureEntry{
		Platform:  platform,
		Cookie:    "",
		ExpiresAt: expires,
	}, gocache.DefaultExpiration)
	return token, expires, nil
}

// Resolve 解析会话 Token，返回条目。未找到或已过期返回 ErrWebLoginSessionExpired。
func (s *WebLoginCaptureStore) Resolve(token string) (*webLoginCaptureEntry, error) {
	if token == "" {
		return nil, ErrWebLoginSessionExpired
	}
	val, ok := s.cache.Get(token)
	if !ok {
		return nil, ErrWebLoginSessionExpired
	}
	entry, ok := val.(*webLoginCaptureEntry)
	if !ok {
		return nil, ErrWebLoginSessionExpired
	}
	return entry, nil
}

// SetCookie 写入/覆盖该会话捕获到的完整 Cookie 字符串。
func (s *WebLoginCaptureStore) SetCookie(token, cookie string) {
	val, ok := s.cache.Get(token)
	if !ok {
		return
	}
	entry, ok := val.(*webLoginCaptureEntry)
	if !ok {
		return
	}
	entry.Cookie = cookie
	entry.CapturedAt = time.Now()
	// 重新写入以保留滑动续期语义（与 Touch 一致）。
	s.cache.Set(token, entry, gocache.DefaultExpiration)
}

// Cookie 返回该会话已捕获的 Cookie 字符串；未捕获或不存在返回 ("", false)。
func (s *WebLoginCaptureStore) Cookie(token string) (string, bool) {
	val, ok := s.cache.Get(token)
	if !ok {
		return "", false
	}
	entry, ok := val.(*webLoginCaptureEntry)
	if !ok {
		return "", false
	}
	if entry.Cookie == "" {
		return "", false
	}
	return entry.Cookie, true
}

// Touch 在活动发生时重置过期时间（滑动续期）。
func (s *WebLoginCaptureStore) Touch(token string) {
	val, ok := s.cache.Get(token)
	if !ok {
		return
	}
	entry, ok := val.(*webLoginCaptureEntry)
	if !ok {
		return
	}
	entry.ExpiresAt = time.Now().Add(webLoginSessionTTL)
	s.cache.Set(token, entry, gocache.DefaultExpiration)
}

// Delete 删除会话（best-effort）。
func (s *WebLoginCaptureStore) Delete(token string) {
	s.cache.Delete(token)
}

// EnsureBaseline 在会话首个代理请求时记录入站平台 Cookie 基线（浏览器上一会话
// 遗留登录态）。已记录则原样返回既有基线（幂等，并发首个请求取先到者）。
// 返回生效基线；会话不存在/已过期返回 nil。
func (s *WebLoginCaptureStore) EnsureBaseline(token string, inbound map[string]string) map[string]string {
	val, ok := s.cache.Get(token)
	if !ok {
		return nil
	}
	entry, ok := val.(*webLoginCaptureEntry)
	if !ok {
		return nil
	}
	if entry.baselineSet {
		return entry.baseline
	}
	entry.baseline = inbound
	entry.baselineSet = true
	// 重新写入以保留存储语义（与 SetCookie/Touch 一致）。
	s.cache.Set(token, entry, gocache.DefaultExpiration)
	return entry.baseline
}

// Baseline 返回会话基线；未记录或会话不存在返回 nil。
func (s *WebLoginCaptureStore) Baseline(token string) map[string]string {
	val, ok := s.cache.Get(token)
	if !ok {
		return nil
	}
	entry, ok := val.(*webLoginCaptureEntry)
	if !ok {
		return nil
	}
	if !entry.baselineSet {
		return nil
	}
	return entry.baseline
}
