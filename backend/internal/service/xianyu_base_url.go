package service

import (
	"net"
	"net/url"
	"strings"
)

// parseWorkerBaseURL 解析并校验 Worker 内网地址格式。
// 只允许 http://<host>:<port>，host 为 Docker 主机名或 IP，
// 禁止域名、HTTPS、路径、查询参数、用户信息。
func parseWorkerBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return nil, ErrXianyuBaseURLInvalid
	}
	if u.User != nil {
		return nil, ErrXianyuBaseURLInvalid
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, ErrXianyuBaseURLInvalid
	}
	if u.Port() == "" || u.Port() == "0" {
		return nil, ErrXianyuBaseURLInvalid
	}
	if u.Path != "" && strings.Trim(u.Path, "/") != "" {
		return nil, ErrXianyuBaseURLInvalid
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrXianyuBaseURLInvalid
	}
	host := u.Hostname()
	if isPublicHostname(host) {
		return nil, ErrXianyuBaseURLInvalid
	}
	return u, nil
}

// isLoopbackHost 判断主机名是否为 loopback。
func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || strings.EqualFold(host, "127.0.0.1") ||
		strings.EqualFold(host, "::1") || strings.EqualFold(host, "0:0:0:0:0:0:0:1")
}

// isPublicHostname 判断主机名是否是公网域名或公网 IP。
func isPublicHostname(host string) bool {
	ip := net.ParseIP(host)
	if ip != nil {
		return !isPrivateOrLoopbackIP(ip)
	}
	// 域名：docker 主机名只允许 [a-z0-9_-]，禁止点号（域名）。
	return strings.Contains(host, ".")
}

// isPrivateOrLoopbackIP 判断 IP 是否私有网段或 loopback。
func isPrivateOrLoopbackIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return true
	}
	return false
}