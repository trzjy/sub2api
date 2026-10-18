package urlvalidator

import "testing"

func TestValidateURLFormat(t *testing.T) {
	if _, err := ValidateURLFormat("", false); err == nil {
		t.Fatalf("expected empty url to fail")
	}
	if _, err := ValidateURLFormat("://bad", false); err == nil {
		t.Fatalf("expected invalid url to fail")
	}
	if _, err := ValidateURLFormat("http://example.com", false); err == nil {
		t.Fatalf("expected http to fail when allow_insecure_http is false")
	}
	if _, err := ValidateURLFormat("https://example.com", false); err != nil {
		t.Fatalf("expected https to pass, got %v", err)
	}
	if _, err := ValidateURLFormat("http://example.com", true); err != nil {
		t.Fatalf("expected http to pass when allow_insecure_http is true, got %v", err)
	}
	if _, err := ValidateURLFormat("https://example.com:bad", true); err == nil {
		t.Fatalf("expected invalid port to fail")
	}

	// 验证末尾斜杠被移除
	normalized, err := ValidateURLFormat("https://example.com/", false)
	if err != nil {
		t.Fatalf("expected trailing slash url to pass, got %v", err)
	}
	if normalized != "https://example.com" {
		t.Fatalf("expected trailing slash to be removed, got %s", normalized)
	}

	// 验证多个末尾斜杠被移除
	normalized, err = ValidateURLFormat("https://example.com///", false)
	if err != nil {
		t.Fatalf("expected multiple trailing slashes to pass, got %v", err)
	}
	if normalized != "https://example.com" {
		t.Fatalf("expected all trailing slashes to be removed, got %s", normalized)
	}

	// 验证带路径的 URL 末尾斜杠被移除
	normalized, err = ValidateURLFormat("https://example.com/api/v1/", false)
	if err != nil {
		t.Fatalf("expected trailing slash url with path to pass, got %v", err)
	}
	if normalized != "https://example.com/api/v1" {
		t.Fatalf("expected trailing slash to be removed from path, got %s", normalized)
	}

	// base url 不得带 query / fragment（调用方会在其后拼接 "?" 参数，否则产出双 ? 畸形 URL）
	for _, raw := range []string{
		"https://host/v1?token=abc",
		"https://host/v1#frag",
		"https://host/v1?token=abc#frag",
		// 空 query / 空 fragment：出现分隔符即拒绝（url.Parse 下 RawQuery/Fragment 均为空，需靠 ForceQuery 与原始串兜底）
		"https://host/v1?",
		"https://host/v1#",
		// fragment 后接伪 query：由 '#' 判定拒绝，'?' 判定不得误伤 fragment 内的 '?'
		"https://host/path#frag?x=1",
	} {
		if _, err := ValidateURLFormat(raw, false); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}

func TestValidateHTTPURL(t *testing.T) {
	if _, err := ValidateHTTPURL("http://example.com", false, ValidationOptions{}); err == nil {
		t.Fatalf("expected http to fail when allow_insecure_http is false")
	}
	if _, err := ValidateHTTPURL("http://example.com", true, ValidationOptions{}); err != nil {
		t.Fatalf("expected http to pass when allow_insecure_http is true, got %v", err)
	}
	if _, err := ValidateHTTPURL("https://example.com", false, ValidationOptions{RequireAllowlist: true}); err == nil {
		t.Fatalf("expected require allowlist to fail when empty")
	}
	if _, err := ValidateHTTPURL("https://example.com", false, ValidationOptions{AllowedHosts: []string{"api.example.com"}}); err == nil {
		t.Fatalf("expected host not in allowlist to fail")
	}
	if _, err := ValidateHTTPURL("https://api.example.com", false, ValidationOptions{AllowedHosts: []string{"api.example.com"}}); err != nil {
		t.Fatalf("expected allowlisted host to pass, got %v", err)
	}
	if _, err := ValidateHTTPURL("https://sub.api.example.com", false, ValidationOptions{AllowedHosts: []string{"*.example.com"}}); err != nil {
		t.Fatalf("expected wildcard allowlist to pass, got %v", err)
	}
	if _, err := ValidateHTTPURL("https://localhost", false, ValidationOptions{AllowPrivate: false}); err == nil {
		t.Fatalf("expected localhost to be blocked when allow_private_hosts is false")
	}

	// 三平台网页登录/转发的官方域名须通过 allowlist（web 登录链 validateUpstreamURL 口径）
	webLoginHosts := []string{
		"https://chat.deepseek.com",
		"https://chatglm.cn",
		"https://auth.kimi.com",
		"https://www.kimi.com",
	}
	for _, raw := range webLoginHosts {
		if _, err := ValidateHTTPURL(raw, false, ValidationOptions{
			AllowedHosts: []string{"api.kimi.com", "chat.deepseek.com", "chatglm.cn", "auth.kimi.com", "www.kimi.com"},
		}); err != nil {
			t.Fatalf("expected web login host %s to pass allowlist, got %v", raw, err)
		}
	}

	// base url 不得带 query / fragment（调用方会在其后拼接 "?" 参数，否则产出双 ? 畸形 URL）
	for _, raw := range []string{
		"https://host/v1?token=abc",
		"https://host/v1#frag",
		"https://host/v1?token=abc#frag",
		// 空 query / 空 fragment：出现分隔符即拒绝（url.Parse 下 RawQuery/Fragment 均为空，需靠 ForceQuery 与原始串兜底）
		"https://host/v1?",
		"https://host/v1#",
		// fragment 后接伪 query：由 '#' 判定拒绝，'?' 判定不得误伤 fragment 内的 '?'
		"https://host/path#frag?x=1",
	} {
		if _, err := ValidateHTTPURL(raw, false, ValidationOptions{}); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}

// TestValidateHTTPURLAllowQuery 验证 AllowQuery 门控：true 放行 query、恒拒 fragment；
// 零值（默认）仍拒绝 query，逐位不变。
func TestValidateHTTPURLAllowQuery(t *testing.T) {
	// AllowQuery=true：query 合法通过，fragment 仍拒。
	if _, err := ValidateHTTPURL("https://host/v1?token=abc", false, ValidationOptions{AllowQuery: true}); err != nil {
		t.Fatalf("expected query to pass with AllowQuery=true, got %v", err)
	}
	// 空 query（ForceQuery）/ 末尾 '?' 在 AllowQuery=true 时放行。
	if _, err := ValidateHTTPURL("https://host/v1?", false, ValidationOptions{AllowQuery: true}); err != nil {
		t.Fatalf("expected empty query to pass with AllowQuery=true, got %v", err)
	}
	// fragment 不受 AllowQuery 门控，恒拒绝。
	for _, raw := range []string{
		"https://host/v1#frag",
		"https://host/v1?token=abc#frag",
		"https://host/v1#",
		"https://host/path#frag?x=1",
	} {
		if _, err := ValidateHTTPURL(raw, false, ValidationOptions{AllowQuery: true}); err == nil {
			t.Fatalf("expected fragment to be rejected regardless of AllowQuery, got %q accepted", raw)
		}
	}
	// 零值：query 仍拒绝（现行为不变）。
	if _, err := ValidateHTTPURL("https://host/v1?token=abc", false, ValidationOptions{}); err == nil {
		t.Fatalf("expected query to be rejected with zero-value opts")
	}
}

// TestValidateURLFormatOpts 验证 ValidateURLFormatOpts 的 AllowQuery 门控与零值委托一致。
func TestValidateURLFormatOpts(t *testing.T) {
	// 零值委托：与 ValidateURLFormat(..., false) 行为一致，query 拒。
	if _, err := ValidateURLFormatOpts("https://host/v1?token=abc", false, ValidationOptions{}); err == nil {
		t.Fatalf("expected query to be rejected with zero-value opts")
	}
	// 零值与 ValidateURLFormat 等价。
	if got, err := ValidateURLFormatOpts("https://example.com/", false, ValidationOptions{}); err != nil {
		t.Fatalf("expected https to pass with zero-value opts, got %v", err)
	} else if got != "https://example.com" {
		t.Fatalf("expected trailing slash removed, got %s", got)
	}
	// AllowQuery=true：query 通过、fragment 仍拒。
	if _, err := ValidateURLFormatOpts("https://host/v1?token=abc", false, ValidationOptions{AllowQuery: true}); err != nil {
		t.Fatalf("expected query to pass with AllowQuery=true, got %v", err)
	}
	for _, raw := range []string{
		"https://host/v1#frag",
		"https://host/v1#",
		"https://host/path#frag?x=1",
	} {
		if _, err := ValidateURLFormatOpts(raw, false, ValidationOptions{AllowQuery: true}); err == nil {
			t.Fatalf("expected fragment to be rejected regardless of AllowQuery, got %q accepted", raw)
		}
	}
}

func TestIsBlockedHost(t *testing.T) {
	for _, host := range []string{
		"localhost", "LOCALHOST", "foo.localhost", " 127.0.0.1 ",
		"127.0.0.1", "::1", "10.0.0.5", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "0.0.0.0", "::", "fe80::1", "fc00::1", "::ffff:127.0.0.1",
	} {
		if !IsBlockedHost(host) {
			t.Fatalf("expected %q to be blocked", host)
		}
	}
	for _, host := range []string{"example.com", "cdn.example.com", "93.184.216.34", "8.8.8.8", "2606:4700:4700::1111", ""} {
		if IsBlockedHost(host) {
			t.Fatalf("expected %q to be allowed", host)
		}
	}
}
