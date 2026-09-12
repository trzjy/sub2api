package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CodeBuddy 上游协议常量（copilot.tencent.com，非官方公开 API）。
// 端点与请求头指纹对齐官方 CLI（workbuddy 设备授权流），改动需同步
// codebuddy_token_refresher.go 与前端 OAuth 流程文案。
const (
	CodeBuddyUpstreamBaseURL = "https://copilot.tencent.com"
	CodeBuddyOriginReferer   = "https://www.codebuddy.cn"
	CodeBuddyClientUA        = "CLI/2.63.2 CodeBuddy/2.63.2"

	codeBuddyAuthStatePath    = "/v2/plugin/auth/state?platform=CLI"
	codeBuddyAuthTokenPath    = "/v2/plugin/auth/token?state="
	codeBuddyLoginAcctPath    = "/v2/plugin/login/account?state="
	codeBuddyTokenRefreshPath = "/v2/plugin/auth/token/refresh"
)

// ErrCodeBuddyLoginPending 表示用户尚未在浏览器完成登录（auth/token 端点业务 code 非 0）。
var ErrCodeBuddyLoginPending = errors.New("codebuddy: login pending")

// CodeBuddyOAuthService 处理 CodeBuddy OAuth 设备授权登录与 token 刷新。
// 上游签发 state 且无 PKCE，本服务不持久化会话：state 由管理端透传。
type CodeBuddyOAuthService struct {
	httpClient *http.Client
}

func NewCodeBuddyOAuthService() *CodeBuddyOAuthService {
	return &CodeBuddyOAuthService{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
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

// setCommonHeaders 对齐官方 CLI 的指纹请求头（Origin/Referer/UA）。
func (s *CodeBuddyOAuthService) setCommonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", CodeBuddyOriginReferer)
	req.Header.Set("Referer", CodeBuddyOriginReferer+"/")
	req.Header.Set("User-Agent", CodeBuddyClientUA)
}

// doJSON 请求上游并解 {code,msg,data} 信封，code != 0 视为业务错误。
// loginPending 为 true 时业务错误归类为 ErrCodeBuddyLoginPending（auth/token 轮询场景）。
func (s *CodeBuddyOAuthService) doJSON(ctx context.Context, method, fullURL string, headers func(*http.Request), body io.Reader, loginPending bool) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		headers(req)
	} else {
		s.setCommonHeaders(req)
	}
	codeBuddyCaptureOutboundHeaders(req)
	resp, err := s.httpClient.Do(req)
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
func (s *CodeBuddyOAuthService) GenerateAuthURL(ctx context.Context) (*CodeBuddyAuthURLResult, error) {
	data, err := s.doJSON(ctx, http.MethodPost, CodeBuddyUpstreamBaseURL+codeBuddyAuthStatePath, nil, bytes.NewReader([]byte("{}")), false)
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
func (s *CodeBuddyOAuthService) PollToken(ctx context.Context, state string) (*CodeBuddyTokenInfo, error) {
	state = strings.TrimSpace(state)
	if state == "" || len(state) > 128 {
		return nil, fmt.Errorf("state 不能为空且长度不得超过 128")
	}

	tokData, err := s.doJSON(ctx, http.MethodGet,
		codeBuddyStateURL(CodeBuddyUpstreamBaseURL, codeBuddyAuthTokenPath, state), nil, nil, true)
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
		s.setCommonHeaders(req)
		req.Header.Set("Authorization", "Bearer "+info.AccessToken)
	}
	if acctData, acctErr := s.doJSON(ctx, http.MethodGet,
		codeBuddyStateURL(CodeBuddyUpstreamBaseURL, codeBuddyLoginAcctPath, state), acctHeaders, nil, false); acctErr == nil {
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
func (s *CodeBuddyOAuthService) RefreshToken(ctx context.Context, refreshToken, uid, enterpriseID, domain string) (*CodeBuddyTokenInfo, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("无可用的 refresh_token")
	}
	headers := func(req *http.Request) {
		s.setCommonHeaders(req)
		req.Header.Set("X-Refresh-Token", refreshToken)
		if enterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", enterpriseID)
		}
		req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
	}
	data, err := s.doJSON(ctx, http.MethodPost, CodeBuddyUpstreamBaseURL+codeBuddyTokenRefreshPath, headers, nil, false)
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

// RefreshAccountToken 刷新指定账户的 token
func (s *CodeBuddyOAuthService) RefreshAccountToken(ctx context.Context, account *Account) (*CodeBuddyTokenInfo, error) {
	if account.Platform != PlatformCodeBuddy || account.Type != AccountTypeOAuth {
		return nil, fmt.Errorf("非 CodeBuddy OAuth 账户")
	}
	return s.RefreshToken(ctx,
		account.GetCredential("refresh_token"),
		account.GetCredential("uid"),
		account.GetCredential("enterprise_id"),
		account.GetCredential("domain"),
	)
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
