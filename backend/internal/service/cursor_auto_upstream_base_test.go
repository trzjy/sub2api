package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolveCursorAutoUpstreamBaseURL 表驱动验证上游基址解析口径：
// 非空、绝对 http(s) URL、带主机名、无内嵌凭据；未配置即失败关闭；
// 不限制明文 http；不回落默认地址；返回去尾部斜杠。
func TestResolveCursorAutoUpstreamBaseURL(t *testing.T) {
	cases := []struct {
		name      string
		value     string
		wantErr   bool
		wantValue string
	}{
		{name: "empty", value: "", wantErr: true},
		{name: "whitespace-only", value: "   ", wantErr: true},
		{name: "http-ip-port", value: "http://82.157.20.83:9098", wantErr: false, wantValue: "http://82.157.20.83:9098"},
		{name: "ftp-scheme", value: "ftp://cursor.example.com", wantErr: true},
		{name: "https-no-host", value: "https:///api", wantErr: true},
		{name: "https-embedded-creds", value: "https://user:pass@cursor.example.com", wantErr: true},
		{name: "https-trailing-slash", value: "https://cursor.example.com/", wantErr: false, wantValue: "https://cursor.example.com"},
		{name: "http-private-ip", value: "http://10.0.0.5:9098", wantErr: false, wantValue: "http://10.0.0.5:9098"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.value == "" {
				// 确保未配置：清空该环境变量
				t.Setenv(EnvCursorAutoUpstreamBaseURL, "")
			} else {
				t.Setenv(EnvCursorAutoUpstreamBaseURL, c.value)
			}

			got, err := ResolveCursorAutoUpstreamBaseURL()
			if c.wantErr {
				require.Error(t, err, "期望解析失败但成功返回: %q", c.value)
				return
			}
			require.NoError(t, err, "期望解析成功但报错: %q -> %v", c.value, err)
			require.Equal(t, c.wantValue, got, "返回值与期望不符")
		})
	}
}
