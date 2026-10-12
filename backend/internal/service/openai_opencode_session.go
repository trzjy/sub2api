package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const openCodeSessionHeader = "X-OpenCode-Session"

// applyOpenCodeSessionHeader forwards the caller-owned conversation identifier
// only to OpenCode's official API origin. The caller applies this after account
// header overrides so a per-conversation value cannot be replaced by a fixed
// account-wide override.
func applyOpenCodeSessionHeader(c *gin.Context, account *Account, targetURL string, headers http.Header) {
	if c == nil || c.Request == nil || account == nil || account.Type != AccountTypeAPIKey || headers == nil {
		return
	}

	// host 判定与 applyMuseSessionHeader 共用同一来源（isOpenCodeHostURL）。
	if !isOpenCodeHostURL(targetURL) {
		return
	}

	sessionID := strings.TrimSpace(c.GetHeader(openCodeSessionHeader))
	if sessionID == "" {
		return
	}
	for key := range headers {
		if strings.EqualFold(key, openCodeSessionHeader) {
			delete(headers, key)
		}
	}
	headers.Set(openCodeSessionHeader, sessionID)
}
