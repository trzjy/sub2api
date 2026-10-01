package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const museRelayUserAgent = "sub2api-relay/1.0"

// applyMuseSessionHeader 为 muse 平台的上游请求注入 OpenCode Go 端点必需的
// x-opencode-session 头与网关中继身份 User-Agent。仅对 muse 平台生效，其他平台
// 零改动。
//
// 语义（muse-3 派发单）：
//   - 客户端已带 x-opencode-session 则原样保留，作为每会话稳定 ID；
//   - 客户端未带则每请求生成 UUID 塞入，满足上游 MissingSessionID 校验；
//   - User-Agent 覆写为 sub2api-relay/1.0，确保上游识别为合法中继调用方。
func applyMuseSessionHeader(c *gin.Context, account *Account, headers http.Header) {
	if c == nil || c.Request == nil || account == nil || headers == nil {
		return
	}
	if account.Platform != PlatformMuse {
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
