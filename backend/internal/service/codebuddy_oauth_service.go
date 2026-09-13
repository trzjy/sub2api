package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
)

// CodeBuddy 上游协议路径与共享 UA（非官方公开 API）。站点相关的域名/Origin/Referer
// 已收敛到 codebuddy_site.go 的站点表，由 codeBuddyEndpointsFor(site) 选取；
// 改动需同步 codebuddy_token_refresher.go 与前端 OAuth 流程文案。
const (
	CodeBuddyClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	codeBuddyAuthStatePath    = "/v2/plugin/auth/state?platform=CLI"
	codeBuddyAuthTokenPath    = "/v2/plugin/auth/token?state="
	codeBuddyLoginAcctPath    = "/v2/plugin/login/account?state="
	codeBuddyTokenRefreshPath = "/v2/plugin/auth/token/refresh"
)

// ErrCodeBuddyLoginPending 表示用户尚未在浏览器完成登录（auth/token 端点业务 code 非 0）。
var ErrCodeBuddyLoginPending = errors.New("codebuddy: login pending")

// CodeBuddyOAuthService 处理 CodeBuddy OAuth 设备授权登录与 token 刷新。
// 上游签发 state 且无 PKCE，本服务不持久化会话：state 由管理端透传。
//
// B3：登录/poll/刷新按账号（或管理端选定的）代理构造独立 http.Client。登录 IP
// 与日常调用 IP 不一致是风控的典型信号，故统一走账号代理；无代理账号行为不变。
type CodeBuddyOAuthService struct {
	httpClient *http.Client
	proxyRepo  ProxyRepository
}

func NewCodeBuddyOAuthService(proxyRepo ProxyRepository) *CodeBuddyOAuthService {
	return &CodeBuddyOAuthService{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		proxyRepo:  proxyRepo,
	}
}

const (
	// codeBuddyProxyDialTimeout 代理 TCP 连接超时（含代理握手），代理不通时快速失败。
	codeBuddyProxyDialTimeout = 5 * time.Second
	// codeBuddyProxyTLSHandshakeTimeout 代理 TLS 握手超时。
	codeBuddyProxyTLSHandshakeTimeout = 5 * time.Second
)

// newCodeBuddyHTTPClient 按代理 URL 构造 http.Client（参照 antigravity.NewClient）。
// proxyURL 为空时返回不挂 Transport 的直连 client，与改造前行为一致。
func newCodeBuddyHTTPClient(proxyURL string) (*http.Client, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	_, parsed, err := proxyurl.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if parsed != nil {
		transport := &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: codeBuddyProxyDialTimeout,
			}).DialContext,
			TLSHandshakeTimeout: codeBuddyProxyTLSHandshakeTimeout,
		}
		if err := proxyutil.ConfigureTransportProxy(transport, parsed); err != nil {
			return nil, fmt.Errorf("configure proxy: %w", err)
		}
		client.Transport = transport
	}
	return client, nil
}

// resolveProxyURL 把代理 ID 解析为代理 URL；无代理返回空串（直连）。
// 代理仓库缺失/查不到/查询失败均返回可读错误，避免静默直连导致 IP 不一致。
func (s *CodeBuddyOAuthService) resolveProxyURL(ctx context.Context, proxyID *int64) (string, error) {
	if proxyID == nil {
		return "", nil
	}
	if s.proxyRepo == nil {
		return "", infraerrors.New(http.StatusBadRequest, "CODEBUDDY_OAUTH_PROXY_NOT_AVAILABLE", "proxy repository is not available")
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil {
		if errors.Is(err, ErrProxyNotFound) {
			return "", infraerrors.New(http.StatusBadRequest, "CODEBUDDY_OAUTH_PROXY_NOT_FOUND", "configured proxy was not found")
		}
		return "", infraerrors.New(http.StatusServiceUnavailable, "CODEBUDDY_OAUTH_PROXY_LOOKUP_FAILED", "proxy lookup is temporarily unavailable")
	}
	if proxy == nil {
		return "", infraerrors.New(http.StatusBadRequest, "CODEBUDDY_OAUTH_PROXY_NOT_FOUND", "configured proxy was not found")
	}
	return proxy.URL(), nil
}

// clientForProxy 返回本次请求应使用的 client：无代理复用共享直连 client，
// 有代理则按代理构造独立 client。
func (s *CodeBuddyOAuthService) clientForProxy(proxyURL string) (*http.Client, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return s.httpClient, nil
	}
	return newCodeBuddyHTTPClient(proxyURL)
}

// clientForProxyID 解析代理 ID 并构造对应 client（无代理 → 共享直连 client）。
func (s *CodeBuddyOAuthService) clientForProxyID(ctx context.Context, proxyID *int64) (*http.Client, error) {
	proxyURL, err := s.resolveProxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	return s.clientForProxy(proxyURL)
}

// CodeBuddyAuthURLResult 生成授权链接的结果
type CodeBuddyAuthURLResult struct {
	AuthURL string `json:"auth_url"`
	State   string `json:"state"`
}

// CodeBuddyTokenInfo token 信息
type CodeBuddyTokenInfo struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain,omitempty"`
	UID          string `json:"uid,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
}

type codeBuddyEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// setCommonHeaders 按站点写入指纹请求头（Origin/Referer 随站点域变化，UA 两站共用）。
func (s *CodeBuddyOAuthService) setCommonHeaders(req *http.Request, originReferer string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", CodeBuddyClientUA)
}

// doJSON 请求上游并解 {code,msg,data} 信封，code != 0 视为业务错误。
// client 为本次请求使用的 HTTP client（无代理时为共享直连 client）。
// originReferer 为站点 Origin/Referer（headers 为 nil 时的默认头来源）。
// loginPending 为 true 时业务错误归类为 ErrCodeBuddyLoginPending（auth/token 轮询场景）。
func (s *CodeBuddyOAuthService) doJSON(ctx context.Context, client *http.Client, method, fullURL, originReferer string, headers func(*http.Request), body io.Reader, loginPending bool) (json.RawMessage, error) {
	if client == nil {
		client = s.httpClient
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		headers(req)
	} else {
		s.setCommonHeaders(req, originReferer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		// 轮询场景仅 4xx 归类为「登录未完成」；5xx 是上游故障，必须如实上报，
		// 否则上游宕机时管理端会被误导为"未完成登录"而无限重试。
		if loginPending && resp.StatusCode < 500 {
			return nil, ErrCodeBuddyLoginPending
		}
		return nil, fmt.Errorf("codebuddy: http %d: %s", resp.StatusCode, truncateCodeBuddy(raw))
	}
	var env codeBuddyEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("codebuddy: 解析响应失败: %w", err)
	}
	if env.Code != 0 {
		if loginPending {
			return nil, ErrCodeBuddyLoginPending
		}
		return nil, fmt.Errorf("codebuddy: code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, nil
}

func truncateCodeBuddy(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// codeBuddyStateURL 构造带 state 的上游 URL。state 来自管理端请求体（不可信
// 输入）且实际字符集未知，故 URL 编码后写入 query，注入防护不依赖格式假设。
func codeBuddyStateURL(base, path, state string) string {
	u, err := url.Parse(base + path)
	if err != nil {
		return base + path + url.QueryEscape(state)
	}
	q := u.Query()
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String()
}

// GenerateAuthURL 生成 CodeBuddy OAuth 授权链接（上游签发 state，服务端无会话）。
// site 选择站点 URL 表（空/未知 → cn）；proxyID 为本次登录选定的代理（nil = 直连），
// 与后续 poll/账号日常调用保持同一出口 IP。poll 必须传相同 site。
func (s *CodeBuddyOAuthService) GenerateAuthURL(ctx context.Context, site string, proxyID *int64) (*CodeBuddyAuthURLResult, error) {
	client, err := s.clientForProxyID(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	ep := codeBuddyEndpointsFor(site)
	data, err := s.doJSON(ctx, client, http.MethodPost, ep.UpstreamBase+codeBuddyAuthStatePath, ep.OriginReferer, nil, bytes.NewReader([]byte("{}")), false)
	if err != nil {
		return nil, fmt.Errorf("获取授权 state 失败: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("codebuddy: 授权响应缺少 state 或 authUrl")
	}
	return &CodeBuddyAuthURLResult{AuthURL: st.AuthURL, State: st.State}, nil
}

// PollToken 轮询登录结果：auth/token 为权威登录状态端点，未完成登录时返回
// ErrCodeBuddyLoginPending；完成后附带拉取 uid/nickname（失败不阻塞）。
// site 必须与 GenerateAuthURL 一致，否则 state 在另一站点不存在、登录永远 pending。
// proxyID 必须与 GenerateAuthURL 一致，否则登录请求的出口 IP 与首次登录不一致。
func (s *CodeBuddyOAuthService) PollToken(ctx context.Context, state, site string, proxyID *int64) (*CodeBuddyTokenInfo, error) {
	state = strings.TrimSpace(state)
	if state == "" || len(state) > 128 {
		return nil, fmt.Errorf("state 不能为空且长度不得超过 128")
	}

	client, err := s.clientForProxyID(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	ep := codeBuddyEndpointsFor(site)

	tokData, err := s.doJSON(ctx, client, http.MethodGet,
		codeBuddyStateURL(ep.UpstreamBase, codeBuddyAuthTokenPath, state), ep.OriginReferer, nil, nil, true)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokData, &tok); err != nil || tok.AccessToken == "" {
		return nil, ErrCodeBuddyLoginPending
	}

	info := &CodeBuddyTokenInfo{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		Domain:       tok.Domain,
	}
	if info.ExpiresIn > 0 {
		info.ExpiresAt = time.Now().Unix() + info.ExpiresIn
	}

	// login/account 获取 uid/nickname/enterprise_id（容错，失败不阻塞登录）
	acctHeaders := func(req *http.Request) {
		s.setCommonHeaders(req, ep.OriginReferer)
		req.Header.Set("Authorization", "Bearer "+info.AccessToken)
	}
	if acctData, acctErr := s.doJSON(ctx, client, http.MethodGet,
		codeBuddyStateURL(ep.UpstreamBase, codeBuddyLoginAcctPath, state), ep.OriginReferer, acctHeaders, nil, false); acctErr == nil {
		var acct struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		_ = json.Unmarshal(acctData, &acct)
		info.UID = acct.UID
		info.EnterpriseID = acct.EnterpriseID
		info.Nickname = acct.Nickname
	}

	return info, nil
}

// RefreshToken 用 refresh_token 换新 access_token。
// X-Refresh-Token 仅允许出现在刷新请求中，绝不出现在 chat 请求（上游安全红线）。
// site 选择站点 URL 表；proxyID 为账号绑定的代理（nil = 直连），使 token 轮换与日常调用同一出口 IP。
func (s *CodeBuddyOAuthService) RefreshToken(ctx context.Context, refreshToken, uid, enterpriseID, domain, site string, proxyID *int64) (*CodeBuddyTokenInfo, error) {
	client, err := s.clientForProxyID(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	return s.refreshTokenWithClient(ctx, client, refreshToken, uid, enterpriseID, domain, site)
}

func (s *CodeBuddyOAuthService) refreshTokenWithClient(ctx context.Context, client *http.Client, refreshToken, uid, enterpriseID, domain, site string) (*CodeBuddyTokenInfo, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("无可用的 refresh_token")
	}
	ep := codeBuddyEndpointsFor(site)
	headers := func(req *http.Request) {
		s.setCommonHeaders(req, ep.OriginReferer)
		req.Header.Set("X-Refresh-Token", refreshToken)
		if enterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", enterpriseID)
		}
		// Phase 0 校准（D8）：intl 实测不带 X-Auth-Refresh-Source 仍 200，该头非必需，
		// 按其值（workbuddy 为 CN 品牌标识）属多余指纹面，故 intl 不发。
		// CN 保留原样以保证"无 site=cn 行为不变"（CN 是否必需由 Phase 2 cn 回归确认）。
		if NormalizeCodeBuddySite(site) == CodeBuddySiteCN {
			req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
		}
	}
	data, err := s.doJSON(ctx, client, http.MethodPost, ep.UpstreamBase+codeBuddyTokenRefreshPath, ep.OriginReferer, headers, nil, false)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		// 无 accessToken 意味着 refresh token 已失效。错误消息必须携带
		// invalid_refresh_token 关键词：token_refresh_service 的
		// isNonRetryableRefreshError 靠关键词子串匹配判定"停止重试、标记
		// 账号需重新授权"，纯中文消息会被当成可重试错误反复打上游。
		return nil, fmt.Errorf("codebuddy: invalid_refresh_token，需重新 OAuth 登录")
	}
	info := &CodeBuddyTokenInfo{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		Domain:       tok.Domain,
	}
	if info.ExpiresIn > 0 {
		info.ExpiresAt = time.Now().Unix() + info.ExpiresIn
	}
	// 响应缺省字段保留旧值
	if info.RefreshToken == "" {
		info.RefreshToken = refreshToken
	}
	if info.Domain == "" {
		info.Domain = domain
	}
	info.UID = uid
	info.EnterpriseID = enterpriseID
	return info, nil
}

// RefreshAccountToken 刷新指定账户的 token（按账号代理出站，与日常调用同一出口 IP）。
func (s *CodeBuddyOAuthService) RefreshAccountToken(ctx context.Context, account *Account) (*CodeBuddyTokenInfo, error) {
	if account.Platform != PlatformCodeBuddy || account.Type != AccountTypeOAuth {
		return nil, fmt.Errorf("非 CodeBuddy OAuth 账户")
	}
	client, err := s.accountClient(ctx, account)
	if err != nil {
		return nil, err
	}
	return s.refreshTokenWithClient(ctx, client,
		account.GetCredential("refresh_token"),
		account.GetCredential("uid"),
		account.GetCredential("enterprise_id"),
		account.GetCredential("domain"),
		account.CodeBuddySite(),
	)
}

// accountClient 解析账号代理并构造 client：优先用已加载的 account.Proxy，
// 缺失时按 account.ProxyID 查代理仓库；无代理返回共享直连 client。
func (s *CodeBuddyOAuthService) accountClient(ctx context.Context, account *Account) (*http.Client, error) {
	if account != nil && account.Proxy != nil {
		return s.clientForProxy(account.Proxy.URL())
	}
	var proxyID *int64
	if account != nil {
		proxyID = account.ProxyID
	}
	return s.clientForProxyID(ctx, proxyID)
}

// BuildAccountCredentials 构建账户凭证
func (s *CodeBuddyOAuthService) BuildAccountCredentials(tokenInfo *CodeBuddyTokenInfo) map[string]any {
	creds := map[string]any{
		"access_token": tokenInfo.AccessToken,
	}
	if tokenInfo.RefreshToken != "" {
		creds["refresh_token"] = tokenInfo.RefreshToken
	}
	if tokenInfo.ExpiresAt > 0 {
		creds["expires_at"] = strconv.FormatInt(tokenInfo.ExpiresAt, 10)
	}
	if tokenInfo.Domain != "" {
		creds["domain"] = tokenInfo.Domain
	}
	if tokenInfo.UID != "" {
		creds["uid"] = tokenInfo.UID
	}
	if tokenInfo.EnterpriseID != "" {
		creds["enterprise_id"] = tokenInfo.EnterpriseID
	}
	if tokenInfo.Nickname != "" {
		creds["nickname"] = tokenInfo.Nickname
	}
	return creds
}
