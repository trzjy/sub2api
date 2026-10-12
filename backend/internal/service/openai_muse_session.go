package service

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const museRelayUserAgent = "sub2api-relay/1.0"

// isOpenCodeHostURL 报告 rawURL 是否指向 OpenCode 官方 API origin
// （https scheme + hostname opencode.ai，任意路径）。作为 applyOpenCodeSessionHeader
// 与 applyMuseSessionHeader 共用的 host 判定单一来源，避免第二份拷贝。
func isOpenCodeHostURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "https") && strings.EqualFold(parsed.Hostname(), "opencode.ai")
}

// applyMuseSessionHeader 为 muse 平台账号（或 base_url 指向 OpenCode 官方 origin 的
// 伪装账号）的上游请求注入 OpenCode Go 端点必需的 x-opencode-session 头与网关中继
// 身份 User-Agent。其余账号零改动。
//
// 门控（OC-T1b 扩展）：platform == muse，或账号 base_url host 为 opencode.ai（任一路径）
// 时注入；两者皆不满足则直接返回。
//
// 语义（muse-3 派发单，原样保留）：
//   - 客户端已带 x-opencode-session 则原样保留，作为每会话稳定 ID；
//   - 客户端未带则每请求生成 UUID 塞入，满足上游 MissingSessionID 校验；
//   - User-Agent 覆写为 sub2api-relay/1.0，确保上游识别为合法中继调用方。
func applyMuseSessionHeader(c *gin.Context, account *Account, headers http.Header) {
	if c == nil || c.Request == nil || account == nil || headers == nil {
		return
	}
	// 伪装账号（platform=kimi/deepseek/zhipu + opencode.ai base_url）与 muse 同口径注入，
	// 消除 OpenCode Go 端点 chat/completions 的 400 MissingSessionID；其余账号零变化。
	if account.Platform != PlatformMuse && !isOpenCodeHostURL(account.GetOpenAIBaseURL()) {
		return
	}

	sessionID := strings.TrimSpace(c.GetHeader("x-opencode-session"))
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	// 清掉既有（可能来自客户端透传或 applyOpenCodeSessionHeader）的同义头，再写入，
	// 避免大小写不一致的重复头。
	for key := range headers {
		if strings.EqualFold(key, "x-opencode-session") {
			delete(headers, key)
		}
	}
	headers.Set("x-opencode-session", sessionID)
	headers.Set("user-agent", museRelayUserAgent)
}
