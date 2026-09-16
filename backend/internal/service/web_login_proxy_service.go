// Package service 中的 WebLoginProxy 上游映射服务。
//
// 安全红线：反向代理的上游地址【绝不由用户传入】。所有上游 origin 在本文件内以
// 固定映射表硬编码（SSRF 防护）。下游 Handler 只负责把 platform 名映射为这里定义
// 的上游，任何用户可控的 URL 都不会被信任。
package service

import (
	"errors"
	"net/url"
	"strings"
)

// webLoginUpstream 是固定上游的定义。
type webLoginUpstream struct {
	// Origin 上游根地址（含协议），用于反代转发与 Location/Referer 重写。
	Origin string
	// CookieName 该平台用于登录态判定的关键 Cookie 名，空串表示不捕获（仅内嵌）。
	CookieName string
}

// webLoginUpstreams 固定上游映射表。Platform 名是白名单 key，不在表内一律拒绝。
var webLoginUpstreams = map[string]webLoginUpstream{
	"web-zhipu":   {Origin: "https://chatglm.cn", CookieName: "chatglm_token"},
	"web-deepseek": {Origin: "https://chat.deepseek.com", CookieName: "ds_session_id"},
	"web-kimi":    {Origin: "https://www.kimi.com", CookieName: ""},
}

// ErrWebLoginUnknownPlatform 表示 platform 不在固定映射表内。
var ErrWebLoginUnknownPlatform = errors.New("unknown web login platform")

// ErrWebLoginInvalidPath 表示请求路径非法（非 / 开头或含路径穿越）。
var ErrWebLoginInvalidPath = errors.New("invalid web login proxy path")

// WebLoginProxyPlatformInfo 返回平台的上游 origin、关键 Cookie 名、是否存在。
// Handler 据此做转发、Cookie 捕获判定与重定向白名单。
func WebLoginProxyPlatformInfo(platform string) (origin string, cookieName string, ok bool) {
	u, found := webLoginUpstreams[platform]
	if !found {
		return "", "", false
	}
	return u.Origin, u.CookieName, true
}

// WebLoginProxyRedirectAllowlist 返回允许出现在上游响应 Location / 页面绝对 URL
// 中的 host 白名单（仅四个硬编码 host）。用于 SSRF 防护：任何重定向或页面内绝对
// URL 若指向非白名单 host，一律拦截或透传为相对路径。
func WebLoginProxyRedirectAllowlist() map[string]bool {
	return map[string]bool{
		"chatglm.cn":            true,
		"chat.deepseek.com":     true,
		"www.kimi.com":          true,
		"platform.deepseek.com": true,
	}
}

// BuildUpstreamURL 根据固定映射构造上游请求 URL。
//   - platform 必须在映射表内，否则返回 ErrWebLoginUnknownPlatform。
//   - path 必须以 "/" 开头，且不得包含 ".." 路径穿越片段。
//   - query 为原始查询字符串（可为空），原样拼接。
func BuildUpstreamURL(platform, path, query string) (*url.URL, error) {
	u, ok := webLoginUpstreams[platform]
	if !ok {
		return nil, ErrWebLoginUnknownPlatform
	}
	if path == "" || !strings.HasPrefix(path, "/") {
		return nil, ErrWebLoginInvalidPath
	}
	// 拒绝路径穿越：任何 ".." 片段都视为非法。
	if strings.Contains(path, "..") {
		return nil, ErrWebLoginInvalidPath
	}
	target, err := url.Parse(u.Origin)
	if err != nil {
		return nil, err
	}
	target.Path = path
	target.RawQuery = query
	return target, nil
}
