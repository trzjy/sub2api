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
	"compress/flate"
	"compress/gzip"
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

// webLoginProxySessionCookie 是隔离 origin 识别代理会话的 Cookie 名。
// token 为能力 token（非上游凭证），等价于 URL 中的 token，仅用于让隔离 origin 的
// 同站点请求免带 token（SameSite=Lax 即可被 iframe 内同站点请求携带）。
const webLoginProxySessionCookie = "wlp_session"

// webLoginProxySessionMaxAge 对齐 store 会话 TTL（秒）。
const webLoginProxySessionMaxAge = 600

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
//	body: {"platform":"zhipu"}
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

// Proxy 是网页登录捕获的核心反向代理（URL token 路径）。主站 v1 与隔离 engine 共用
// 此路由：从 URL param 取 token，再复用共享代理逻辑。未知 token 仍返回 410。
//
//	ANY /api/v1/web-login-proxy/:token/*path
func (h *WebLoginProxyHandler) Proxy(c *gin.Context) {
	token := c.Param("token")
	h.proxyForToken(c, token, c.Param("path"))
}

// ProxyRoot 是隔离 origin 的根路径全代理：整个 origin 的所有路径都交给代理，会话不再
// 依赖 URL 中的 token，而由请求 Cookie wlp_session 识别。Cookie 缺失或无效时返回 410
// （绝不透传）。隔离 engine 通过 NoRoute 注册此 catch-all。
func (h *WebLoginProxyHandler) ProxyRoot(c *gin.Context) {
	token, err := c.Cookie(webLoginProxySessionCookie)
	if err != nil || token == "" {
		c.String(http.StatusGone, "session expired")
		return
	}
	// token 合法性由 proxyForToken 内部 Resolve 校验（无效 → 410），此处不重复解析。
	h.proxyForToken(c, token, c.Request.URL.Path)
}

// proxyForToken 是代理主体逻辑（方法白名单、1MB 限、上游 URL 构造、ReverseProxy 构造、
// 响应捕获等），由 Proxy（URL token）与 ProxyRoot（Cookie token）共用。每个响应都会
// 下发会话 Cookie wlp_session，使隔离 origin 后续同 origin 请求免带 token。
func (h *WebLoginProxyHandler) proxyForToken(c *gin.Context, token, rawPath string) {
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
	target, err := service.BuildUpstreamURL(entry.Platform, rawPath, c.Request.URL.RawQuery)
	if err != nil {
		response.BadRequest(c, "invalid proxy path")
		return
	}

	// 下发会话 Cookie wlp_session：隔离 origin 同站点识别，后续请求免带 URL token。
	// token 为能力 token（非凭证），故本 Cookie 不泄露任何上游凭证。
	setSessionCookie(c, token)

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
			// Accept-Encoding 固定 gzip：HTML 改写层只支持 gzip/deflate 解压
			// （Go 标准库无 brotli），跟随浏览器原始 Accept-Encoding 可能收到
			// br 响应导致改写层无法解压注入 <base>。
			req.Header.Set("Accept-Encoding", "gzip")
			// 删除所有 X-Forwarded-*，防止上游据此判定真实客户端。
			for k := range req.Header {
				if strings.HasPrefix(strings.ToLower(k), "x-forwarded-") {
					req.Header.Del(k)
				}
			}
			// 出站 Cookie：携带“已捕获累积串 + 入站 Cookie 中的平台白名单字段”。
			// 平台白名单字段（如 chatglm_token / chatglm_refresh_token）是官方前端
			// JS 写入浏览器、服务端不下发 Set-Cookie 的登录态，必须从入站 Cookie 头
			// 提取才能转发给上游；白名单之外的入站 Cookie（本站管理端 cookie、
			// wlp_session 等）一律丢弃，绝不原样转发整个入站 Cookie 头。
			allowed := extractAllowedCookies(req, entry.Platform)
			req.Header.Del("Cookie")
			if stored, ok := h.store.Cookie(token); ok && stored != "" {
				req.Header.Set("Cookie", mergeCookies(stored, cookiesFromPairs(allowed)))
			} else if len(allowed) > 0 {
				req.Header.Set("Cookie", joinCookiePairs(allowed))
			}
		},
		// 5b. Transport：在传输层校验重定向目标 host 白名单（SSRF 防护）。
		Transport: &redirectSafeTransport{
			base:      http.DefaultTransport,
			allowlist: service.WebLoginProxyRedirectAllowlist(),
		},
		// 5c. ModifyResponse：剥离敏感响应头、重写 Set-Cookie、捕获 Cookie、重写 Location、改写 HTML。
		ModifyResponse: func(resp *http.Response) error {
			return h.modifyUpstreamResponse(c, token, keyCookieName, entry.Platform, origin, resp, c.Request)
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

// setSessionCookie 在每个代理响应上下发 wlp_session，属性完整：
// Path=/；Secure；SameSite=Lax；HttpOnly；Max-Age=600（对齐 store TTL）。
func setSessionCookie(c *gin.Context, token string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     webLoginProxySessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   webLoginProxySessionMaxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// stripSessionCookie 从 Cookie 字符串中移除本站会话 Cookie wlp_session，
// 确保本代理会话凭证绝不会被捕获/转发为上游凭证（red-line 防御，纵深一层）。
func stripSessionCookie(cookieStr string) string {
	if cookieStr == "" {
		return ""
	}
	parts := strings.Split(cookieStr, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name := p
		if idx := strings.IndexByte(p, '='); idx >= 0 {
			name = p[:idx]
		}
		if strings.EqualFold(strings.TrimSpace(name), webLoginProxySessionCookie) {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "; ")
}

// modifyUpstreamResponse 处理上游响应：头剥离、Set-Cookie 重写、Cookie 捕获、Location 重写、HTML 改写。
func (h *WebLoginProxyHandler) modifyUpstreamResponse(
	c *gin.Context,
	token, keyCookieName, platform, origin string,
	resp *http.Response,
	req *http.Request,
) error {
	stripResponseSecurityHeaders(resp)

	// 5c-1. 重写 Set-Cookie：删 Domain=、SameSite 改 Lax；非 TLS 访问则删 Secure。
	secure := isSecureRequest(c)
	rewriteSetCookies(resp, secure)

	// 6. 捕获逻辑：合并 Set-Cookie + 入站白名单字段，命中关键 Cookie 则存储。
	captureCookies(h.store, token, keyCookieName, platform, resp, req)
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

// captureCookies 合并三路来源为一条完整 Cookie 字符串，若命中平台关键 Cookie 名则
// 存入 store：
//  1. 已捕获累积串（store.Cookie(token)，跨多次响应累积）；
//  2. 本次上游响应 Set-Cookie（resp.Cookies()）；
//  3. 入站 Cookie 头中的【平台白名单字段】（extractAllowedCookies）——官方前端 JS
//     写入型登录态（chatglm_token 等）不走 Set-Cookie，只能从浏览器携带的 Cookie
//     捕获；白名单之外的一切入站 Cookie（本站管理端 cookie、wlp_session 等）绝不
//     并入捕获（红线：误捕获会泄漏/污染）。
func captureCookies(store *service.WebLoginCaptureStore, token, keyCookieName, platform string, resp *http.Response, req *http.Request) {
	prev, _ := store.Cookie(token)
	prev = stripSessionCookie(prev)
	merged := mergeCookies(prev, resp.Cookies())
	// 并入入站 Cookie 中的平台白名单字段（JS 写入型登录态）。
	inboundAllowed := extractAllowedCookies(req, platform)
	if len(inboundAllowed) > 0 {
		merged = mergeCookies(merged, cookiesFromPairs(inboundAllowed))
	}
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
		// 关键 Cookie 存在于入站白名单字段（JS 写入型，首次捕获）也算命中。
		for _, p := range inboundAllowed {
			if p.Name == keyCookieName {
				store.SetCookie(token, merged)
				return
			}
		}
	}
}

// extractAllowedCookies 从请求 Cookie 头中提取平台白名单字段（区分大小写）。
// 白名单之外的 Cookie（含本站管理端 cookie 与 wlp_session）一律丢弃。
// 平台非法或白名单为空时返回空列表（绝不把入站 Cookie 当上游凭证）。
func extractAllowedCookies(req *http.Request, platform string) []struct{ Name, Value string } {
	allowed := service.WebLoginProxyAllowedCookieNames(platform)
	if len(allowed) == 0 {
		return nil
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = true
	}
	parsed := parseCookieHeader(req.Header.Get("Cookie"))
	out := make([]struct{ Name, Value string }, 0, len(parsed))
	for _, p := range parsed {
		if allowedSet[p.Name] {
			out = append(out, p)
		}
	}
	return out
}

// cookiesFromPairs 把 name/value 对转换为 *http.Cookie 列表（值原样，无属性）。
func cookiesFromPairs(pairs []struct{ Name, Value string }) []*http.Cookie {
	if len(pairs) == 0 {
		return nil
	}
	out := make([]*http.Cookie, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, &http.Cookie{Name: p.Name, Value: p.Value})
	}
	return out
}

// joinCookiePairs 把 name/value 对拼接为 Cookie 头字符串（仅 name=value）。
func joinCookiePairs(pairs []struct{ Name, Value string }) string {
	if len(pairs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.Name+"="+p.Value)
	}
	return strings.Join(parts, "; ")
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
// 压缩处理：上游可能返回 gzip/deflate 压缩 body（出站 Accept-Encoding 已固定 gzip，
// 但压缩仍可能出现）——压缩字节上做字符串注入必然失败，必须先解压再改写；改写后
// 以明文回传（删除 Content-Encoding，登录页 ≤512KB，明文回传开销可接受且不引入
// 重新压缩的出错面）。
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

	// 解压（gzip/deflate）：解压失败视为压缩内容原样透传，不做任何改写尝试
	// （改写前行为一致；浏览器收到时按压缩体原样处理）。
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	decompressed := false
	plain := body
	switch encoding {
	case "gzip":
		if gr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			if p, err := io.ReadAll(io.LimitReader(gr, int64(webLoginProxyMaxBody))); err == nil {
				plain = p
				decompressed = true
			}
		}
	case "deflate":
		if p, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(body)), int64(webLoginProxyMaxBody))); err == nil {
			plain = p
			decompressed = true
		}
	}
	if encoding != "" && !decompressed {
		return
	}
	// 解压成功则回传不再压缩：删除 Content-Encoding（明文回传）。
	if decompressed {
		resp.Header.Del("Content-Encoding")
	}

	rewritten := rewriteHTMLBody(plain, token)
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
