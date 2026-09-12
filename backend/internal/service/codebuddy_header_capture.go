package service

import (
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// codeBuddyCaptureOutboundHeaders 打印 CodeBuddy 出站请求头，用于活体验收
// §2.4 指纹比对与 X-Auth-Refresh-Source 头裁决。
//
// 仅存在于临时插桩构建（分支 tmp/codebuddy-header-capture），不属于产品代码；
// 验收完成后随镜像回滚一并移除。凭据类头（Authorization / X-Refresh-Token /
// Cookie）只记录长度，绝不落明文，满足「审计不得包含 token 明文」约束。
func codeBuddyCaptureOutboundHeaders(req *http.Request) {
	if req == nil {
		return
	}
	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := strings.Join(req.Header.Values(k), ", ")
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "X-Refresh-Token", "Cookie":
			v = "<redacted len=" + strconv.Itoa(len(v)) + ">"
		}
		parts = append(parts, k+"="+v)
	}
	slog.Info("CODEBUDDY_OUTBOUND_HEADERS",
		"method", req.Method,
		"url", req.URL.String(),
		"header_names", strings.Join(keys, ","),
		"headers", strings.Join(parts, "; "),
	)
}
