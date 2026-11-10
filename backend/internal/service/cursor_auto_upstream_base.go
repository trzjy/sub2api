package service

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// EnvCursorAutoUpstreamBaseURL 是 Cursor Auto 上游卡密服务的基址环境变量名。
// 上游基址只来自部署环境配置；未配置即失败关闭，不回落代码内默认地址。
const EnvCursorAutoUpstreamBaseURL = "CURSOR_AUTO_UPSTREAM_BASE_URL"

// ResolveCursorAutoUpstreamBaseURL 从部署环境解析 Cursor Auto 上游基址。
//
// 校验口径：非空、绝对 http(s) URL、带主机名、无内嵌凭据。
// 不限制明文 http：上游实际只提供 http，运维显式配置该值即视为授权使用该端点；
// 仍然禁止回落到代码内任何默认地址（未配置即失败关闭）。
// 返回值去掉尾部斜杠。
func ResolveCursorAutoUpstreamBaseURL() (string, error) {
	raw := strings.TrimSpace(os.Getenv(EnvCursorAutoUpstreamBaseURL))
	if raw == "" {
		return "", fmt.Errorf("%s is not configured", EnvCursorAutoUpstreamBaseURL)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid %s: %w", EnvCursorAutoUpstreamBaseURL, err)
	}

	if u.Host == "" {
		return "", fmt.Errorf("invalid %s: missing host", EnvCursorAutoUpstreamBaseURL)
	}

	if u.User != nil {
		return "", fmt.Errorf("invalid %s: embedded credentials are not allowed", EnvCursorAutoUpstreamBaseURL)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid %s: only http/https are allowed", EnvCursorAutoUpstreamBaseURL)
	}

	return strings.TrimRight(u.String(), "/"), nil
}
