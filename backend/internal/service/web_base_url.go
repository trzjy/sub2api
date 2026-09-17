package service

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// webBaseURLSuffixes 是每个 web 平台（web-deepseek / web-zhipu / web-kimi）允许的
// 官方域名后缀。credentials.base_url 只允许指向这些官方域名，杜绝 SSRF / 凭证外送
// （docs/web-login-proxy-security-fix-plan.md 阶段 B #6）。归并后官方平台 + web access
// mode 账号复用同一后缀集合（经 webProviderKeyForAccount 归一为 web-* 键）。
var webBaseURLSuffixes = map[string][]string{
	PlatformWebDeepseek: {"deepseek.com"},
	PlatformWebZhipu:    {"bigmodel.cn", "chatglm.cn"},
	PlatformWebKimi:     {"kimi.com", "moonshot.cn"},
}

// isWebZhipuAccount 报告账号是否应走 forwardWebZhipu：旧 web-zhipu 平台（迁移前无
// access_mode，平台本身即判定），或官方 zhipu 平台 + web access mode（归并后新形态）。
// API 模式 zhipu 账号返回 false（隔离红线 §6：API 模式账号绝不误入网页协议链）。
func isWebZhipuAccount(a *Account) bool {
	if a == nil {
		return false
	}
	if a.Platform == PlatformWebZhipu {
		return true // 旧平台兼容
	}
	return a.IsWebAccessMode() && a.IsZhipu()
}

// isWebDeepseekAccount 同构：旧 web-deepseek 平台，或官方 deepseek 平台 + web access mode。
func isWebDeepseekAccount(a *Account) bool {
	if a == nil {
		return false
	}
	if a.Platform == PlatformWebDeepseek {
		return true // 旧平台兼容
	}
	return a.IsWebAccessMode() && a.IsDeepseek()
}

// isWebKimiAccount 同构：旧 web-kimi 平台，或官方 kimi 平台 + web access mode。
func isWebKimiAccount(a *Account) bool {
	if a == nil {
		return false
	}
	if a.Platform == PlatformWebKimi {
		return true // 旧平台兼容
	}
	return a.IsWebAccessMode() && a.IsKimi()
}

// isWebReverseAccount 是网页逆向账号的兼容并集判定（归并后取代裸 IsWebProvider）：
// 旧 web-* 平台（平台本身即判定），或官方平台（zhipu/deepseek/kimi）且 access_mode=web
// 的账号均命中（/v1/messages 分派口径）。
func isWebReverseAccount(a *Account) bool {
	if a == nil {
		return false
	}
	if IsWebProvider(a.Platform) {
		return true // 旧平台兼容
	}
	return a.IsWebAccessMode() && (a.IsZhipu() || a.IsDeepseek() || a.IsKimi())
}

// webProviderKeyForAccount 把账号归一为 web 宇宙平台键（PlatformWeb*），用于模型目录 /
// base_url 域名后缀查表（归并后官方平台 + web access mode 复用旧 web-* 目录）。
func webProviderKeyForAccount(a *Account) string {
	if a != nil {
		switch {
		case a.Platform == PlatformWebZhipu || a.IsZhipu():
			return PlatformWebZhipu
		case a.Platform == PlatformWebDeepseek || a.IsDeepseek():
			return PlatformWebDeepseek
		case a.Platform == PlatformWebKimi || a.IsKimi():
			return PlatformWebKimi
		}
	}
	return ""
}

// ValidateWebBaseURL 校验网页逆向账号的 base_url 是否为官方域名（fail-closed）。
//
// 规则：
//   - 空串：合法，返回 ("", nil)，表示由调用方回落平台默认官方 origin；
//   - 必须 https 协议（明文 http 一律拒绝）；
//   - host 必须是对应平台的官方域名后缀，且不能是内网/保留/环回地址字面量；
//   - 解析失败或非法 → 返回错误。
//
// 返回规范化后的 base_url（去掉末尾多余斜杠）。
func ValidateWebBaseURL(platform, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// 空值走平台默认，由 GetWebBaseURL 回落。
		return "", nil
	}
	suffixes, ok := webBaseURLSuffixes[platform]
	if !ok {
		return "", fmt.Errorf("platform %q does not support web base_url override", platform)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid web base_url %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("web base_url must use https, got %q", raw)
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return "", fmt.Errorf("web base_url missing host: %q", raw)
	}
	// 拒绝内网 / 环回 / 保留地址字面量（SSRF 防护）。仅允许官方域名（FQDN）。
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
			return "", fmt.Errorf("web base_url host %q is not an allowed official domain", host)
		}
	}
	for _, suf := range suffixes {
		if host == suf || strings.HasSuffix(host, "."+suf) {
			return strings.TrimRight(raw, "/"), nil
		}
	}
	return "", fmt.Errorf("web base_url host %q is not an allowed official domain for %s", host, platform)
}

// GetWebBaseURL 解析网页逆向账号（web-deepseek / web-zhipu / web-kimi）的上游
// base_url（docs/web-reverse-embedded-login-plan.md §3.3）：credentials.base_url
// 覆盖优先，否则回落平台默认官方域名。非网页平台或为空返回 ""（上层失败关闭）。
//
// 安全约束（阶段 B #6）：base_url 覆盖值经 ValidateWebBaseURL 校验，非法
// （内网 IP、http:// 明文、非官方主机）一律 fail-closed 返回空串，转发方按
// 空 base_url 失败关闭，绝不把请求发往非官方主机。
func (a *Account) GetWebBaseURL() string {
	// 兼容并集判定（归并后取代裸 IsWebProvider）：旧 web-* 平台或官方平台 + web access
	// mode 的账号走网页 base_url 链；API 模式账号一律回落空串（不进入网页协议）。
	if a == nil || !isWebReverseAccount(a) {
		return ""
	}
	key := webProviderKeyForAccount(a)
	if raw := strings.TrimSpace(a.GetCredential("base_url")); raw != "" {
		// 非法值 fail-closed：返回空串，转发方按空 base_url 失败。
		if validated, err := ValidateWebBaseURL(key, raw); err == nil {
			return validated
		}
		return ""
	}
	switch key {
	case PlatformWebDeepseek:
		return DefaultWebDeepseekBaseURL
	case PlatformWebZhipu:
		return DefaultWebZhipuBaseURL
	case PlatformWebKimi:
		return DefaultWebKimiBaseURL
	default:
		return ""
	}
}
