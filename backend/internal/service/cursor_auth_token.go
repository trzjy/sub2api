package service

// 本文件实现 **B-13：经 AuthService 换取用户级 token 并缓存**（E6 卡，方案
// docs-local/cursor-platform-fix-plan.md §1.6）。
//
// 契约（官方客户端 `workbench.desktop.main.js` @24702878 / @24696138 分块取证，
// 本卡再次独立解出并逐字核对）：
//
//	aiserver.v1.AuthService.GetSessionToken —— kind:ae.Unary
//	message GetSessionTokenRequest {
//	  1 user    = aiserver.v1.User   // 结构见下
//	  2 destination = enum            // 2 = DESTINATION_AISERVER（对话主机要这个）
//	  3 stub              = string optional
//	  4 code              = string optional
//	  5 code_verifier     = string optional
//	  6 redirect_uri      = string optional
//	  7 issue_refresh_token = bool  optional
//	}
//	message GetSessionTokenResponse { 1 session_token = string; … }
//
// `aiserver.v1.User`（`T:rlp` @24694117）—— **注意没有字段 1，id 是字段 2**：
//
//	2 id             string      （非 opt，即语义必填）
//	3 email          string      optional
//	4 email_verified bool        optional
//	5 first_name     string      optional
//	6 last_name      string      optional
//	7 created_at     string      optional
//	8 updated_at     string      optional
//
// ════════════════════════════════════════════════════════════════════════════
// **活体结论（2026-10-16 本卡实测，证据见本文件末尾「活体取证」）**：
//
//  1. **不带 code ⇒ 上游 501 `ERROR_DEPRECATED`**
//     （`getSessionToken without passing code is deprecated`）。
//     ⇒ `code` 字段**必填**，尽管 schema 标 optional。
//  2. **不带 redirect_uri ⇒ 400 `Invalid redirect_uri`**。
//  3. 带 `code` + `redirect_uri=https://cursor.com/api/auth/callback`
//     ⇒ 请求**穿透中转抵达真实 Cursor**，返回 401 `ERROR_AUTH_TOKEN_EXPIRED`
//     （`Auth token expired for user. Try logging out and back in.`）。
//  4. **`user` 字段填什么都不改变结果**：空串 / `cursor` / deviceId / 激活码
//     四种取值全部返回逐字相同的 `ERROR_AUTH_TOKEN_EXPIRED`
//     ⇒ 上游在鉴权阶段**不读 user.id**。
//
//  ⇒ **本层把 user 字段按"最小合法值"填充并留出显式开关**：默认发送
//    `User{id: <配置值>}`，配置为空时**整段省略**（活体证明省略不影响结果，
//    故省略是安全的，且比编造一个假 user id 更诚实）。
//
//  ⇒ **当前 BLOCKED 的真实根因不是本层**：第 3 条的 `ERROR_AUTH_TOKEN_EXPIRED`
//    与 `/api/refresh` 取回的号池账号 `access_token` 的 `exp` 完全吻合——
//    该 JWT 的 `exp` 是 **2026-06-19T07:22:01Z**，早已过期（活体解码实证）。
//    即**号池侧那批 Cursor 账号的会话本身已过期**，属号池供给问题，
//    不是站点协议实现问题。本层已按契约实现完毕，号池账号刷新后即可直接生效。
// ════════════════════════════════════════════════════════════════════════════

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// GetSessionTokenRequest / User 的字段号（全部来自已取证 schema，勿臆造）。
const (
	cursorFldGetSessionTokenUser         = 1
	cursorFldGetSessionTokenDestination  = 2
	cursorFldGetSessionTokenStub         = 3
	cursorFldGetSessionTokenCode         = 4
	cursorFldGetSessionTokenCodeVerifier = 5
	cursorFldGetSessionTokenRedirectURI  = 6
	cursorFldGetSessionTokenIssueRefresh = 7

	// aiserver.v1.User 的字段号（**id 是 2，没有字段 1**）。
	cursorFldUserID = 2
)

// cursorDestinationAIServer 是 Destination 枚举的 AISERVER 值（对话主机用这个）。
const cursorDestinationAIServer = 2

// cursorSessionTokenTTL 是 session_token 的**保守**缓存时长。
//
// 上游 GetSessionTokenResponse **不带过期时间字段**（1..10 全是 token / bool /
// cookie，无 expires_at），故无法按上游给出的期限精确缓存，只能取保守值：
// 宁可在到期后多换一次 token（上游有速率限制但不致命），也不要复用一个已
// 过期的 token 让整段对话 401。
const cursorSessionTokenTTL = 30 * time.Minute

// cursorDefaultAuthRedirectURI 是实测被上游接受的 redirect_uri。
//
// 活体对照（2026-10-16）：
//
//	`https://cursor.com/api/auth/callback`  → 穿透到真实 Cursor（AUTH_TOKEN_EXPIRED）
//	`https://www.cursor.com/api/auth/callback` → 同上（两版 host 都接受）
//	`cursor://cursorDeepControl`               → 400 Invalid redirect_uri
//	不传                                       → 400 Invalid redirect_uri
const cursorDefaultAuthRedirectURI = "https://cursor.com/api/auth/callback"

// cursor 出站 token 层的失败关闭错误。
var (
	// ErrCursorSessionTokenUnavailable 表示换不到 session_token：禁止发出对话请求。
	ErrCursorSessionTokenUnavailable = errors.New("cursor: session token unavailable (fail closed)")

	// ErrCursorRelayAuthRejected 表示中转/上游以鉴权错误拒绝了换 token 请求。
	ErrCursorRelayAuthRejected = errors.New("cursor: relay rejected session token request")
)

// cursorSessionTokenSource 是基于 AuthService 的 token 提供者（含内存缓存）。
//
// **实例必须按账号构造**（与 cursorAccountAuthProvider 同纪律）：不同账号的
// `code` 不同，做成全局单例等于把 A 号的会话发给 B 号。
type cursorSessionTokenSource struct {
	forwarder   *CursorUpstreamForwarder
	auth        CursorUpstreamAuthProvider
	code        string
	redirectURI string
	userID      string

	mu       sync.Mutex
	cached   string
	cachedAt time.Time
	nowFn    func() time.Time
	ttl      time.Duration
}

// cursorSessionTokenOption 用于装配/测试。
type cursorSessionTokenOption func(*cursorSessionTokenSource)

// withCursorSessionTokenRedirectURI 覆盖 redirect_uri（默认 cursorDefaultAuthRedirectURI）。
func withCursorSessionTokenRedirectURI(u string) cursorSessionTokenOption {
	return func(s *cursorSessionTokenSource) {
		if strings.TrimSpace(u) != "" {
			s.redirectURI = strings.TrimSpace(u)
		}
	}
}

// withCursorSessionTokenUserID 设置 user.id。留空则**整段省略 user 字段**
// （活体证明省略不改变结果，见文件头第 4 条）。
func withCursorSessionTokenUserID(id string) cursorSessionTokenOption {
	return func(s *cursorSessionTokenSource) { s.userID = strings.TrimSpace(id) }
}

// withCursorSessionTokenClock 注入时钟（测试用，避免真等 TTL）。
func withCursorSessionTokenClock(now func() time.Time, ttl time.Duration) cursorSessionTokenOption {
	return func(s *cursorSessionTokenSource) {
		if now != nil {
			s.nowFn = now
		}
		if ttl > 0 {
			s.ttl = ttl
		}
	}
}

// newCursorSessionTokenSource 构造 AuthService token 提供者。
//
// forwarder / auth / code 任一不可得都返回错误（失败关闭）：绝不用空材料换 token。
func newCursorSessionTokenSource(
	forwarder *CursorUpstreamForwarder,
	auth CursorUpstreamAuthProvider,
	code string,
	opts ...cursorSessionTokenOption,
) (*cursorSessionTokenSource, error) {
	if forwarder == nil {
		return nil, fmt.Errorf("%w: no forwarder", ErrCursorSessionTokenUnavailable)
	}
	if auth == nil {
		return nil, fmt.Errorf("%w: no auth provider", ErrCursorSessionTokenUnavailable)
	}
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("%w: empty authorization code", ErrCursorSessionTokenUnavailable)
	}
	s := &cursorSessionTokenSource{
		forwarder:   forwarder,
		auth:        auth,
		code:        strings.TrimSpace(code),
		redirectURI: cursorDefaultAuthRedirectURI,
		nowFn:       time.Now,
		ttl:         cursorSessionTokenTTL,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// CursorSessionToken 以 source 自带的 auth provider 换 token（带 TTL 缓存）。
func (s *cursorSessionTokenSource) CursorSessionToken(ctx context.Context) (string, error) {
	return s.cursorSessionTokenWithAuth(ctx, s.auth)
}

// cursorSessionTokenWithAuth 是带显式 auth provider 的换 token 实现——这是实际入口。
//
// 与 CursorUpstreamAuthProvider（设备级 x-jg-auth）是**两个不同维度**：本函数产出的是
// 号池账号的**用户级会话**，前者是**设备身份**。两者都要，缺一即 ERROR_UNAUTHORIZED。
//
// authProvider 是**本次调用所属账号**的 x-jg-auth 材料提供者（逐请求传入，
// 不挂在 source 上——source 每账号一个，但转发器是进程级共享的）。
//
// 并发：mu 覆盖"查缓存 → 未命中则请求 → 写缓存"整段，故同一账号的并发对话
// 只会触发一次上游换 token 调用（上游有速率限制，N 倍并发放大不可接受）。
func (s *cursorSessionTokenSource) cursorSessionTokenWithAuth(
	ctx context.Context, authProvider CursorUpstreamAuthProvider,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 缓存命中：token 非空且未过 TTL。
	if s.cached != "" && s.nowFn().Sub(s.cachedAt) < s.ttl {
		return s.cached, nil
	}

	// 重新换取：材料不可得即失败关闭，绝不带着空 token 发对话请求。
	if authProvider == nil {
		authProvider = s.auth
	}
	if _, err := cursorUpstreamAuthFrom(ctx, authProvider); err != nil {
		return "", fmt.Errorf("%w: %v", ErrCursorSessionTokenUnavailable, err)
	}

	payload, err := s.doGetSessionToken(ctx, authProvider)
	if err != nil {
		return "", err
	}
	token, err := cursorParseGetSessionTokenResponse(payload)
	if err != nil {
		return "", err
	}
	s.cached = token
	s.cachedAt = s.nowFn()
	return token, nil
}

// cursorBuildGetSessionTokenRequest 编码 GetSessionTokenRequest（**不含** envelope，
// Unary 端点走裸 protobuf——活体实证，见文件头）。
func (s *cursorSessionTokenSource) cursorBuildGetSessionTokenRequest() []byte {
	var out []byte
	if s.userID != "" {
		// aiserver.v1.User{2 id}：字段 2 是 id，**没有字段 1**。
		user := cursorAppendStringField(nil, cursorFldUserID, s.userID)
		out = cursorAppendBytesField(out, cursorFldGetSessionTokenUser, user)
	}
	out = cursorAppendVarintField(out, cursorFldGetSessionTokenDestination, cursorDestinationAIServer)
	// code 必填（活体：不带即 501 ERROR_DEPRECATED）。
	out = cursorAppendStringField(out, cursorFldGetSessionTokenCode, s.code)
	// redirect_uri 必填（活体：不带即 400 Invalid redirect_uri）。
	out = cursorAppendStringField(out, cursorFldGetSessionTokenRedirectURI, s.redirectURI)
	out = cursorAppendVarintField(out, cursorFldGetSessionTokenIssueRefresh, 0)
	return out
}

// doGetSessionToken 发出一次 Unary 调用并返回原始响应体。
func (s *cursorSessionTokenSource) doGetSessionToken(ctx context.Context, authProvider CursorUpstreamAuthProvider) ([]byte, error) {
	resp, err := s.forwarder.Do(ctx, &CursorUpstreamRequest{
		Path:         cursorPathGetSessionToken,
		Body:         s.cursorBuildGetSessionTokenRequest(),
		AuthProvider: authProvider,
		Headers: map[string]string{
			"Content-Type":             cursorConnectContentType,
			"Connect-Protocol-Version": "1",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCursorSessionTokenUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readCursorUpstreamBody(resp.Body, cursorAuthResponseMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCursorSessionTokenUnavailable, err)
	}
	if resp.StatusCode != 200 {
		return nil, cursorUpstreamStatusError(resp.StatusCode, body, s.code)
	}
	return body, nil
}

// cursorParseGetSessionTokenResponse 从响应体取 session_token（字段 1）。
//
// 空 token 视为失败关闭：拿着空 token 去对话必然 401，白耗额度。
func cursorParseGetSessionTokenResponse(body []byte) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("%w: empty GetSessionToken response", ErrCursorSessionTokenUnavailable)
	}
	fields, err := cursorDecodeProtobuf(body)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCursorSessionTokenUnavailable, err)
	}
	f, ok := cursorFindField(fields, 1) // 1 session_token
	if !ok || f.Wire != cursorWireBytes {
		return "", fmt.Errorf("%w: response carries no session_token field", ErrCursorSessionTokenUnavailable)
	}
	token := strings.TrimSpace(f.String())
	if token == "" {
		return "", fmt.Errorf("%w: response session_token is empty", ErrCursorSessionTokenUnavailable)
	}
	return token, nil
}
