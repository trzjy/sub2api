package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateWebBaseURL(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		raw      string
		wantErr  bool
		want     string
	}{
		// 合法官方域名。
		{"deepseek official", PlatformDeepseek, "https://chat.deepseek.com", false, "https://chat.deepseek.com"},
		{"deepseek platform subdomain", PlatformDeepseek, "https://platform.deepseek.com", false, "https://platform.deepseek.com"},
		{"zhipu chatglm", PlatformZhipu, "https://chatglm.cn", false, "https://chatglm.cn"},
		{"zhipu bigmodel", PlatformZhipu, "https://open.bigmodel.cn", false, "https://open.bigmodel.cn"},
		{"kimi www", PlatformKimi, "https://www.kimi.com", false, "https://www.kimi.com"},
		{"kimi moonshot", PlatformKimi, "https://api.moonshot.cn", false, "https://api.moonshot.cn"},
		// 任意主机拒绝。
		{"deepseek arbitrary host", PlatformDeepseek, "https://evil.com", true, ""},
		{"zhipu arbitrary host", PlatformZhipu, "https://evil.example.com", true, ""},
		// 明文 http 拒绝。
		{"deepseek http plaintext", PlatformDeepseek, "http://chat.deepseek.com", true, ""},
		{"kimi http plaintext", PlatformKimi, "http://kimi.com", true, ""},
		// 内网 IP 拒绝。
		{"deepseek private ip", PlatformDeepseek, "https://10.0.0.1", true, ""},
		{"deepseek loopback", PlatformDeepseek, "https://127.0.0.1", true, ""},
		{"deepseek linklocal", PlatformDeepseek, "https://169.254.169.254", true, ""},
		// 空值合法（调用方回落默认）。
		{"empty allowed", PlatformDeepseek, "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateWebBaseURL(c.platform, c.raw)
			if c.wantErr {
				require.Error(t, err, "raw=%q", c.raw)
				return
			}
			require.NoError(t, err, "raw=%q", c.raw)
			require.Equal(t, c.want, got)
		})
	}
}

func TestGetWebBaseURL_FailClosedOnIllegal(t *testing.T) {
	// 非法覆盖（明文 http）→ 返回空，转发方按空 base_url 失败关闭。
	illegal := &Account{Platform: PlatformDeepseek, Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "base_url": "http://evil.com"}}
	require.Equal(t, "", illegal.GetWebBaseURL())

	// 内网 IP 覆盖 → 空。
	privateIP := &Account{Platform: PlatformZhipu, Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "base_url": "https://192.168.1.10"}}
	require.Equal(t, "", privateIP.GetWebBaseURL())

	// 合法官方域名覆盖 → 透传。
	legal := &Account{Platform: PlatformDeepseek, Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "base_url": "https://chat.deepseek.com"}}
	require.Equal(t, "https://chat.deepseek.com", legal.GetWebBaseURL())

	// 空 base_url → 平台默认官方 origin。
	def := &Account{Platform: PlatformDeepseek, Credentials: map[string]any{"access_mode": AccountAccessModeWeb}}
	require.Equal(t, DefaultWebDeepseekBaseURL, def.GetWebBaseURL())

	// 非 web 平台 → 空。
	nonWeb := &Account{Platform: PlatformDeepseek, Credentials: map[string]any{"base_url": "https://api.openai.com"}}
	require.Equal(t, "", nonWeb.GetWebBaseURL())
}
