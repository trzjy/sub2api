package server

import "testing"

// TestNormalizeProxyFrameOrigin 校验 web 登录代理 origin → CSP frame-src 值的规范化：
// 仅接受显式 https origin；空值/明文 http/无 host/带路径一律不注入（fail-closed）。
func TestNormalizeProxyFrameOrigin(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"https with port", "https://corealgos.com:8443", "https://corealgos.com:8443"},
		{"https default port", "https://corealgos.com", "https://corealgos.com"},
		{"spaces trimmed", "  https://corealgos.com:8443  ", "https://corealgos.com:8443"},
		{"path stripped", "https://corealgos.com:8443/base/", "https://corealgos.com:8443"},
		{"plain http rejected", "http://corealgos.com:8443", ""},
		{"no host rejected", "https://", ""},
		{"not a url rejected", "corealgos.com:8443", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeProxyFrameOrigin(tt.raw); got != tt.want {
				t.Errorf("normalizeProxyFrameOrigin(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
