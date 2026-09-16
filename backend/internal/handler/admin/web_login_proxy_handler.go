// Package admin 中的 WebLoginProxyHandler 实现网页版登录自动 Cookie 捕获的反向代理
// 与管理端点。
//
// 安全红线：
//   - 上游地址完全由 backend/internal/service 的固定映射决定，绝不接受用户传入 URL（SSRF 防护）。
//   - 禁止向任何日志输出 Cookie / Token / 凭证值。捕获逻辑只读取响应头中的 Set-Cookie
//     与请求 Cookie 头，绝不缓冲完整响应体（HTML 改写除外，已限制 512KB 上限）。
//   - 重定向目标 host 必须在 WebLoginProxyRedirectAllowlist 之内，否则返回 502 且响应体不含目标 URL。
package admin

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// webLoginProxyBasePathPrefix 是嵌入浏览器内访问代理的统一前缀。
// 形如 /api/v1/web-login-proxy/<token>/<上游 path>。
const webLoginProxyBasePathPrefix = "/api/v1/web-login-proxy/"

// webLoginProxyMaxBody 是 HTML 改写允许读取的最大响应体（512KB）。
const webLoginProxyMaxBody = 512 << 10

// htmlHeadTagRegex 匹配首个 <head ...> 标签（不区分大小写）。
var htmlHeadTagRegex = regexp.MustCompile(`(?i)<head[^>]*>`)

// htmlURLRewriteRules 预编译的“白名单 host 绝对 URL 属性”重写规则。
// 注意：Go 的 regexp 基于 RE2，不支持反向引用（\1 等），故对双引号与单引号分别编译。
var htmlURLRewriteRules = buildHTMLURLRewriteRules()

type htmlURLRewriteRule struct {
	re    *regexp.Regexp
	quote string
}

func buildHTMLURLRewriteRules() []htmlURLRewriteRule {
	var rules []htmlURLRewriteRule
	for host := range service.WebLoginProxyRedirectAllowlist() {
		escaped := regexp.QuoteMeta(host)
		for _, q := range []string{`"`, `'`} {
			// $1=attr="  $2=attr名  $3=https://host  $4=/path(可选)  $5=闭引号
			re := regexp.MustCompile(`(?i)((href|src|action)\s*=\s*` + q + `)(https?://` + escaped + `)(/[^` + q + `#]*)?(` + q + `)`)
			rules = append(rules, htmlURLRewriteRule{re: re, quote: q})
		}
	}
	return rules
}

// WebLoginProxyHandler 处理网页登录捕获会话的创建、查询、删除与反向代理。
type WebLoginProxyHandler struct {
	store *service.WebLoginCaptureStore
}

// NewWebLoginProxyHandler 构造处理器。
func NewWebLoginProxyHandler(store *service.WebLoginCaptureStore) *WebLoginProxyHandler {
	return &WebLoginProxyHandler{store: store}
}

// createSessionRequest 是 CreateSession 的请求体。
type createSessionRequest struct {
	Platform string `json:"platform"`
}

// CreateSession 创建一个网页登录捕获会话，返回 Token 与代理入口 URL。
//
//	POST /api/v1/admin/web-login-proxy/sessions
//	body: {"platform":"web-zhipu"}
func (h *WebLoginProxyHandler) CreateSession(c *gin.Context) {
	var req createSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Platform == "" {
		response.BadRequest(c, "platform 不能为空")
		return
	}
	if _, _, ok := service.WebLoginProxyPlatformInfo(req.Platform); !ok {
		response.BadRequest(c, "不支持的 platform")
		return
	}
	token, expires, err := h.store.Create(req.Platform)
	if err != nil {
		response.InternalError(c, "创建会话失败")
		return
	}
	response.Success(c, gin.H{
		"token":      token,
		"url":        webLoginProxyBasePathPrefix + token + "/",
		"expires_at": expires.Format(time.RFC3339),
	})
}

// GetCapture 读取会话是否已捕获到 Cookie。读后保留会话（不删除）。
//
//	GET /api/v1/admin/web-login-proxy/sessions/:token/capture
func (h *WebLoginProxyHandler) GetCapture(c *gin.Context) {
	token := c.Param("token")
	entry, err := h.store.Resolve(token)
	if err != nil {
		// 会话过期/不存在：返回 captured=false, cookie=""。
		response.Success(c, gin.H{
			"captured":   false,
			"cookie":     "",
			"expires_at": "",
		})
		return
	}
	cookie, ok := h.store.Cookie(token)
	resp := gin.H{
		"captured":   ok,
		"cookie":     cookie,
		"expires_at": entry.ExpiresAt.Format(time.RFC3339),
	}
	response.Success(c, resp)
}

// DeleteSession best-effort 删除会话。
//
//	DELETE /api/v1/admin/web-login-proxy/sessions/:token
func (h *WebLoginProxyHandler) DeleteSession(c *gin.Context) {
	token := c.Param("token")
	h.store.Delete(token)
	response.Success(c, gin.H{"deleted": true})
}

// Proxy 是网页登录捕获的核心反向代理，嵌入浏览器通过它访问上游登录页。
//
//	ANY /api/v1/web-login-proxy/:token/*path
func (h *WebLoginProxyHandler) Proxy(c *gin.Context) {
	token := c.Param("token")

	// 1. 解析会话；失败（过期/未知 Token）直接 410，绝不透传。
	entry, err := h.store.Resolve(token)
	if err != nil {
		c.String(http.StatusGone, "session expired")
		return
	}

	// 2. 方法白名单。
	switch c.Request.Method {
	case http.MethodGet, http.MethodPost, http.MethodHead, http.MethodOptions:
	default:
		c.String(http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// 3. 请求体限 1MB。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)

	// 4. 构造上游 URL（path 穿越 / 非法 → 400）。
	rawPath := c.Param("path")
	target, err := service.BuildUpstreamURL(entry.Platform, rawPath, c.Request.URL.RawQuery)
	if err != nil {
		response.BadRequest(c, "invalid proxy path")
		return
	}

	origin := target.Scheme + "://" + target.Host
	keyCookieName, _ := platformKeyCookie(entry.Platform)

	proxy := &httputil.ReverseProxy{
		// 5a. Director：设定上游 scheme/host，转发受控头，重写 Origin/Referer，删除 X-Forwarded-*。
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = target.Path
			req.URL.RawQuery = target.RawQuery
			req.RequestURI = ""
			req.Host = target.Host

			// 原样转发浏览器请求头（UA/Accept/Accept-Language/sec-ch-ua* 已由 Clone 复制）。
			// 仅重写 Origin/Referer 为上游 origin，避免暴露真实反代地址。
			req.Header.Set("Origin", origin)
			req.Header.Set("Referer", origin+target.Path)
			// 删除所有 X-Forwarded-*，防止上游据此判定真实客户端。
			for k := range req.Header {
				if strings.HasPrefix(strings.ToLower(k), "x-forwarded-") {
					req.Header.Del(k)
				}
			}
			// 出站 Cookie：仅携带已捕获累积的上游 Cookie（store.Cookie(token)），
			// 绝不原样转发入站 Cookie 头（含本站管理端 cookie，如 sub2api_session）。
			// 否则会把本站会话凭证泄漏给上游官方站点，且污染捕获结果。
			req.Header.Del("Cookie")
			if cookie, ok := h.store.Cookie(token); ok && cookie != "" {
				req.Header.Set("Cookie", cookie)
			}
		},
		// 5b. Transport：在传输层校验重定向目标 host 白名单（SSRF 防护）。
		Transport: &redirectSafeTransport{
			base:      http.DefaultTransport,
			allowlist: service.WebLoginProxyRedirectAllowlist(),
		},
		// 5c. ModifyResponse：剥离敏感响应头、重写 Set-Cookie、捕获 Cookie、重写 Location、改写 HTML。
		ModifyResponse: func(resp *http.Response) error {
			return h.modifyUpstreamResponse(c, token, keyCookieName, origin, resp, c.Request)
		},
		// 5d. ErrorHandler：上游/重定向错误统一返回 502，且响应体不含目标 URL（防泄露）。
		ErrorHandler: func(_ http.ResponseWriter, _ *http.Request, _ error) {
			c.String(http.StatusBadGateway, "bad gateway")
		},
		// 7. 流式转发。
		FlushInterval: -1,
	}

	proxy.ServeHTTP(c.Writer, c.Request)
}

// modifyUpstreamResponse 处理上游响应：头剥离、Set-Cookie 重写、Cookie 捕获、Location 重写、HTML 改写。
func (h *WebLoginProxyHandler) modifyUpstreamResponse(
	c *gin.Context,
	token, keyCookieName, origin string,
	resp *http.Response,
	req *http.Request,
) error {
	stripResponseSecurityHeaders(resp)

	// 5c-1. 重写 Set-Cookie：删 Domain=、SameSite 改 Lax；非 TLS 访问则删 Secure。
	secure := isSecureRequest(c)
	rewriteSetCookies(resp, secure)

	// 6. 捕获逻辑：合并 Set-Cookie + 请求 Cookie，命中关键 Cookie 则存储。
	captureCookies(h.store, token, keyCookieName, resp, req)
	// 滑动续期：任何上游活动都重置过期。
	h.store.Touch(token)

	// 5c-2. 重定向 Location 处理（仅白名单 host 改写，非白名单 → 502）。
	if isRedirect(resp.StatusCode) {
		if err := rewriteOrRejectLocation(c, token, resp); err != nil {
			return err
		}
	}

	// 5c-3. HTML 改写：仅 text/html 且 Content-Length ≤ 512KB。
	maybeRewriteHTML(c, token, resp)

	return nil
}

// stripResponseSecurityHeaders 删除会阻止在 iframe 中内嵌的响应头。
func stripResponseSecurityHeaders(resp *http.Response) {
	resp.Header.Del("X-Frame-Options")
	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("Cross-Origin-Resource-Policy")
	resp.Header.Del("Clear-Site-Data")
}

// rewriteSetCookies 重写每条 Set-Cookie：移除 Domain 属性、SameSite 改为 Lax、
// 若非安全请求（未经过 TLS）则移除 Secure 属性。
func rewriteSetCookies(resp *http.Response, secure bool) {
	setCookies := resp.Header.Values("Set-Cookie")
	if len(setCookies) == 0 {
		return
	}
	resp.Header.Del("Set-Cookie")
	for _, sc := range setCookies {
		resp.Header.Add("Set-Cookie", sanitizeSetCookie(sc, secure))
	}
}

// sanitizeSetCookie 重写单条 Set-Cookie 字符串：删 Domain=，SameSite=Lax，按需删 Secure。
func sanitizeSetCookie(sc string, secure bool) string {
	// 首段为 name=value。
	parts := strings.SplitN(sc, ";", 2)
	if len(parts) == 1 {
		return sc
	}
	nameVal := parts[0]
	attrs := parts[1]

	var out []string
	out = append(out, nameVal)
	for _, attr := range strings.Split(attrs, ";") {
		attr = strings.TrimSpace(attr)
		if attr == "" {
			continue
		}
		lower := strings.ToLower(attr)
		// 删除 Domain 属性。
		if strings.HasPrefix(lower, "domain=") {
			continue
		}
		// SameSite 统一改为 Lax。
		if strings.HasPrefix(lower, "samesite=") {
			out = append(out, "SameSite=Lax")
			continue
		}
		// 非安全请求删除 Secure。
		if lower == "secure" && !secure {
			continue
		}
		out = append(out, attr)
	}
	return strings.Join(out, "; ")
}

// captureCookies 把上游响应 Set-Cookie 与已捕获累积串合并为一条完整 Cookie 字符串，
// 若命中平台关键 Cookie 名则存入 store。仅读取上游响应头，绝不并入入站 Cookie 头
// （入站可能含本站管理端 cookie，如 sub2api_session，误捕获会泄漏/污染）。
// 跨多次响应的累积通过已存储的捕获结果 merge 实现，不读取本站请求头（红线）。
func captureCookies(store *service.WebLoginCaptureStore, token, keyCookieName string, resp *http.Response, req *http.Request) {
	// 仅合并“已捕获累积串 + 本次上游 Set-Cookie”，彻底排除入站 Cookie 头。
	prev, _ := store.Cookie(token)
	merged := mergeCookies(prev, resp.Cookies())
	if merged == "" {
		return
	}
	if keyCookieName != "" {
		// 本次响应直接回写关键 Cookie。
		for _, c := range resp.Cookies() {
			if c.Name == keyCookieName {
				store.SetCookie(token, merged)
				return
			}
		}
		// 关键 Cookie 已存在于累积串（上游不再回 Set-Cookie 时）也算命中。
		for _, c := range parseCookieHeader(prev) {
			if c.Name == keyCookieName {
				store.SetCookie(token, merged)
				return
			}
		}
	}
}

// mergeCookies 合并请求 Cookie 与响应 Set-Cookie，后者覆盖前者同名项。
func mergeCookies(requestCookie string, setCookies []*http.Cookie) string {
	vals := make(map[string]string)
	order := make([]string, 0)
	add := func(name, value string) {
		if _, ok := vals[name]; !ok {
			order = append(order, name)
		}
		vals[name] = value
	}
	for _, c := range parseCookieHeader(requestCookie) {
		add(c.Name, c.Value)
	}
	for _, c := range setCookies {
		add(c.Name, c.Value)
	}
	if len(order) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, name := range order {
		if i > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(name)
		sb.WriteString("=")
		sb.WriteString(vals[name])
	}
	return sb.String()
}

// parseCookieHeader 解析 Cookie 头为 name/value 列表（仅取 name=value）。
func parseCookieHeader(header string) []struct{ Name, Value string } {
	var out []struct{ Name, Value string }
	if header == "" {
		return out
	}
	for _, pair := range strings.Split(header, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		idx := strings.IndexByte(pair, '=')
		if idx < 0 {
			continue
		}
		out = append(out, struct{ Name, Value string }{
			Name:  strings.TrimSpace(pair[:idx]),
			Value: strings.TrimSpace(pair[idx+1:]),
		})
	}
	return out
}

// isRedirect 判断是否为 3xx 重定向。
func isRedirect(status int) bool {
	return status >= 300 && status < 400
}

// rewriteOrRejectLocation 处理重定向 Location：
//   - 绝对 URL 且 host 在白名单：改写为代理前缀路径。
//   - 绝对 URL 且 host 不在白名单：返回错误（由 ErrorHandler 转为 502，且不泄露 URL）。
//   - 相对 URL：保持不变。
func rewriteOrRejectLocation(c *gin.Context, token string, resp *http.Response) error {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil
	}
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" {
		// 相对 URL，保持原样。
		return nil
	}
	allowlist := service.WebLoginProxyRedirectAllowlist()
	if !allowlist[u.Host] {
		// 非白名单 host：拦截，防止 SSRF；错误不含目标 URL。
		return errors.New("upstream redirect target not allowed")
	}
	// 允许 host：改写为 /api/v1/web-login-proxy/<token>/<path>(+query)。
	rewritten := webLoginProxyBasePathPrefix + token + "/"
	if u.Path != "" {
		rewritten += strings.TrimPrefix(u.Path, "/")
	}
	if u.RawQuery != "" {
		rewritten += "?" + u.RawQuery
	}
	resp.Header.Set("Location", rewritten)
	return nil
}

// maybeRewriteHTML 在满足条件时改写 HTML：注入 <base href> 并将白名单 host 的绝对
// URL 属性替换为代理前缀路径。改写失败或超限时原样透传。
func maybeRewriteHTML(c *gin.Context, token string, resp *http.Response) {
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		return
	}
	// Content-Length 已知且超限 → 跳过。
	if resp.ContentLength > webLoginProxyMaxBody {
		return
	}
	// 读取（512KB 兜底），无论改写成败都回填 body。
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(webLoginProxyMaxBody)))
	if err != nil {
		// 读取失败，原样透传（body 已消耗，回填已读部分）。
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return
	}
	rewritten := rewriteHTMLBody(body, token)
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
}

// rewriteHTMLBody 注入 base href 并替换白名单绝对 URL 属性。失败返回原内容。
func rewriteHTMLBody(body []byte, token string) []byte {
	html := string(body)
	prefix := webLoginProxyBasePathPrefix + token

	// 1. 注入 <base href="prefix/"> 到首个 <head> 之后。
	headLoc := htmlHeadTagRegex.FindStringIndex(html)
	if headLoc != nil {
		baseTag := `<base href="` + prefix + `/">`
		html = html[:headLoc[1]] + baseTag + html[headLoc[1]:]
	}

	// 2. 替换白名单 host 的绝对 URL 属性（href/src/action）。
	for _, rule := range htmlURLRewriteRules {
		// $1=attr="  $4=/path  rule.quote=闭引号 → 重写为 attr="<prefix><path>"
		html = rule.re.ReplaceAllString(html, `$1`+prefix+`$4`+rule.quote)
	}

	return []byte(html)
}

// isSecureRequest 判断本次请求是否经过 TLS（实际 TLS 或前置代理声明 https）。
func isSecureRequest(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	return strings.EqualFold(c.Request.Header.Get("X-Forwarded-Proto"), "https")
}

// platformKeyCookie 返回平台关键 Cookie 名（空串表示不捕获）。
func platformKeyCookie(platform string) (string, bool) {
	_, name, ok := service.WebLoginProxyPlatformInfo(platform)
	return name, ok
}

// redirectSafeTransport 在传输层校验重定向目标 host 白名单，非白名单返回错误。
type redirectSafeTransport struct {
	base      http.RoundTripper
	allowlist map[string]bool
}

// RoundTrip 实现 http.RoundTripper：拦截指向非白名单 host 的 3xx 重定向。
func (t *redirectSafeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if isRedirect(resp.StatusCode) {
		loc := resp.Header.Get("Location")
		if loc != "" {
			if u, e := url.Parse(loc); e == nil && u.Host != "" && !t.allowlist[u.Host] {
				resp.Body.Close()
				// 错误体不含目标 URL（防 SSRF 泄露）。
				return nil, errors.New("upstream redirect not allowed")
			}
		}
	}
	return resp, nil
}
