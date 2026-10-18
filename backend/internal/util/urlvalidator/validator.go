package urlvalidator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ValidationOptions struct {
	AllowedHosts     []string
	RequireAllowlist bool
	AllowPrivate     bool
	// AllowQuery 放宽 query 拒绝口径：默认 false（现行为，拒绝 query）。
	// 仅当调用方校验的是「已构造端点 / query-合并型 URL」而非拼接基址时设 true。
	// fragment 拒绝不受此门控，两函数恒拒绝。
	AllowQuery bool
}

// ValidateHTTPURL validates an outbound HTTP/HTTPS URL.
//
// It provides a single validation entry point that supports:
// - scheme 校验（https 或可选允许 http）
// - 可选 allowlist（支持 *.example.com 通配）
// - allow_private_hosts 策略（阻断 localhost/私网字面量 IP）
//
// 另外，base_url 是拼接端点的基址，调用方会在其后用 "?" 追加参数；query 与
// fragment 都不适合作为端点拼接的基址——query 会与调用方参数拼成畸形 URL，
// fragment 则会让追加的参数落到 fragment 里，均在校验边界直接拒绝。
//
// 该 query 拒绝口径仅对「拼接基址」边界成立；对于已构造端点或 query-合并型
// URL（探测目标、图片下载、Codex manifest 的 base_url 等），query 合法，由
// opts.AllowQuery=true 收窄为可选，零值仍拒绝（现行为逐位不变）。
// fragment 拒绝不受 AllowQuery 门控，恒拒绝。
//
// 注意：DNS Rebinding 防护（解析后 IP 校验）应在实际发起请求时执行，避免 TOCTOU。
func ValidateHTTPURL(raw string, allowInsecureHTTP bool, opts ValidationOptions) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("url is required")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid url: %s", trimmed)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && (!allowInsecureHTTP || scheme != "http") {
		return "", fmt.Errorf("invalid url scheme: %s", parsed.Scheme)
	}

	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host == "" {
		return "", errors.New("invalid host")
	}
	if !opts.AllowPrivate && isBlockedHost(host) {
		return "", fmt.Errorf("host is not allowed: %s", host)
	}

	if port := parsed.Port(); port != "" {
		num, err := strconv.Atoi(port)
		if err != nil || num <= 0 || num > 65535 {
			return "", fmt.Errorf("invalid port: %s", port)
		}
	}

	if !opts.AllowQuery && (parsed.RawQuery != "" || parsed.ForceQuery) {
		return "", fmt.Errorf("base url must not contain query string: %s", trimmed)
	}
	// 未转义的 '#' 恒为 fragment 分隔符；url.Parse 对 "https://host/v1#" 会丢弃
	// 分隔符并给出空 Fragment，故直接对原始串判定，出现分隔符即拒绝。
	// fragment 拒绝不受 AllowQuery 门控，恒拒绝。
	if strings.Contains(trimmed, "#") {
		return "", fmt.Errorf("base url must not contain fragment: %s", trimmed)
	}

	allowlist := normalizeAllowlist(opts.AllowedHosts)
	if opts.RequireAllowlist && len(allowlist) == 0 {
		return "", errors.New("allowlist is not configured")
	}
	if len(allowlist) > 0 && !isAllowedHost(host, allowlist) {
		return "", fmt.Errorf("host is not allowed: %s", host)
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

// ValidateURLFormat 做最小格式校验：仅保证 URL 可解析且 scheme 合规，不做白名单/私网/SSRF 校验。
//
// 同 ValidateHTTPURL，base_url 是拼接端点的基址，不适合自带 query 或 fragment：
// 前者会与调用方参数拼成畸形 URL，后者会让追加的参数落到 fragment 里，故在校验边界拒绝。
// 该 query 拒绝为可选，由 ValidateURLFormatOpts 暴露；本函数委托零值 opts（现行为）。
func ValidateURLFormat(raw string, allowInsecureHTTP bool) (string, error) {
	return ValidateURLFormatOpts(raw, allowInsecureHTTP, ValidationOptions{})
}

// ValidateURLFormatOpts 为 ValidateURLFormat 的可选门控版本：当 opts.AllowQuery=true 时
// 放行 query（用于已构造端点 / query-合并型 URL），否则同 ValidateURLFormat 拒绝 query。
// fragment 拒绝不受 AllowQuery 门控，恒拒绝。
func ValidateURLFormatOpts(raw string, allowInsecureHTTP bool, opts ValidationOptions) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("url is required")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid url: %s", trimmed)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && (!allowInsecureHTTP || scheme != "http") {
		return "", fmt.Errorf("invalid url scheme: %s", parsed.Scheme)
	}

	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return "", errors.New("invalid host")
	}

	if port := parsed.Port(); port != "" {
		num, err := strconv.Atoi(port)
		if err != nil || num <= 0 || num > 65535 {
			return "", fmt.Errorf("invalid port: %s", port)
		}
	}

	if !opts.AllowQuery && (parsed.RawQuery != "" || parsed.ForceQuery) {
		return "", fmt.Errorf("base url must not contain query string: %s", trimmed)
	}
	// 与 ValidateHTTPURL 同口径：未转义的 '#' 恒为 fragment 分隔符，出现即拒绝。
	if strings.Contains(trimmed, "#") {
		return "", fmt.Errorf("base url must not contain fragment: %s", trimmed)
	}

	return strings.TrimRight(trimmed, "/"), nil
}

func ValidateHTTPSURL(raw string, opts ValidationOptions) (string, error) {
	return ValidateHTTPURL(raw, false, opts)
}

// ValidateResolvedIP 验证 DNS 解析后的 IP 地址是否安全
// 用于防止 DNS Rebinding 攻击：在实际 HTTP 请求时调用此函数验证解析后的 IP
func ValidateResolvedIP(host string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("dns resolution failed: %w", err)
	}

	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("resolved ip %s is not allowed", ip.String())
		}
	}
	return nil
}

func normalizeAllowlist(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(values))
	for _, v := range values {
		entry := strings.ToLower(strings.TrimSpace(v))
		if entry == "" {
			continue
		}
		if host, _, err := net.SplitHostPort(entry); err == nil {
			entry = host
		}
		normalized = append(normalized, entry)
	}
	return normalized
}

func isAllowedHost(host string, allowlist []string) bool {
	for _, entry := range allowlist {
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, "*.") {
			suffix := strings.TrimPrefix(entry, "*.")
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}

// IsBlockedHost 报告 host 是否为 localhost、*.localhost，或回环、私网、链路本地、未指定地址的字面量 IP。
// 只判断字面量，域名的解析结果由 ValidateResolvedIP 校验。
func IsBlockedHost(host string) bool {
	return isBlockedHost(strings.ToLower(strings.TrimSpace(host)))
}

func isBlockedHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return true
		}
	}
	return false
}
