package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// oauthAuditCaptureRepository 捕获审计中间件异步落库的记录，供断言使用。
type oauthAuditCaptureRepository struct {
	mu   sync.Mutex
	logs []*service.AuditLog
}

func (r *oauthAuditCaptureRepository) BatchInsert(_ context.Context, logs []*service.AuditLog) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, logs...)
	return int64(len(logs)), nil
}
func (r *oauthAuditCaptureRepository) Insert(_ context.Context, log *service.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
	return nil
}
func (r *oauthAuditCaptureRepository) List(context.Context, *service.AuditLogFilter) (*service.AuditLogList, error) {
	return &service.AuditLogList{}, nil
}
func (r *oauthAuditCaptureRepository) GetByID(context.Context, int64) (*service.AuditLog, error) {
	return nil, service.ErrAuditLogNotFound
}
func (r *oauthAuditCaptureRepository) Count(context.Context) (int64, error) { return 0, nil }
func (r *oauthAuditCaptureRepository) TruncateAll(context.Context) error    { return nil }
func (r *oauthAuditCaptureRepository) DeleteBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (r *oauthAuditCaptureRepository) snapshot() []*service.AuditLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*service.AuditLog(nil), r.logs...)
}

// codeBuddyOAuthServiceStub 返回固定凭证信息，避免 handler 审计测试发起上游网络请求。
type codeBuddyOAuthServiceStub struct {
	authURL   *service.CodeBuddyAuthURLResult
	tokenInfo *service.CodeBuddyTokenInfo
}

func (s *codeBuddyOAuthServiceStub) GenerateAuthURL(context.Context) (*service.CodeBuddyAuthURLResult, error) {
	return s.authURL, nil
}

func (s *codeBuddyOAuthServiceStub) PollToken(context.Context, string) (*service.CodeBuddyTokenInfo, error) {
	return s.tokenInfo, nil
}

func (s *codeBuddyOAuthServiceStub) RefreshToken(context.Context, string, string, string, string) (*service.CodeBuddyTokenInfo, error) {
	return s.tokenInfo, nil
}

// antigravityOAuthServiceStub 同上。
type antigravityOAuthServiceStub struct {
	authURL   *service.AntigravityAuthURLResult
	tokenInfo *service.AntigravityTokenInfo
}

func (s *antigravityOAuthServiceStub) GenerateAuthURL(context.Context, *int64) (*service.AntigravityAuthURLResult, error) {
	return s.authURL, nil
}

func (s *antigravityOAuthServiceStub) ExchangeCode(context.Context, *service.AntigravityExchangeCodeInput) (*service.AntigravityTokenInfo, error) {
	return s.tokenInfo, nil
}

func (s *antigravityOAuthServiceStub) ValidateRefreshToken(context.Context, string, *int64) (*service.AntigravityTokenInfo, error) {
	return s.tokenInfo, nil
}

// newOAuthAuditTestRouter 组装带认证身份与真实审计中间件的路由。
func newOAuthAuditTestRouter(auditRepo *oauthAuditCaptureRepository, register func(*gin.Engine)) (*gin.Engine, *service.AuditLogService) {
	auditService := service.NewAuditLogService(auditRepo, nil)
	auditService.Start()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 77})
		c.Set(string(middleware.ContextKeyUserRole), "admin")
		c.Next()
	})
	router.Use(gin.HandlerFunc(middleware.NewAuditLogMiddleware(auditService)))
	register(router)
	return router, auditService
}

func postJSON(router *gin.Engine, path, body string) {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)
}

// TestCodeBuddyOAuthEndpointsAuditCredentialOpsWithoutTokenPlaintext 验证 A1：
// codebuddy 三个 OAuth 端点产生带固定动作名与目标标识的审计记录，且记录体不出现
// refresh_token / access_token 明文。
func TestCodeBuddyOAuthEndpointsAuditCredentialOpsWithoutTokenPlaintext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &oauthAuditCaptureRepository{}

	handler := &CodeBuddyOAuthHandler{codeBuddyOAuthService: &codeBuddyOAuthServiceStub{
		authURL: &service.CodeBuddyAuthURLResult{AuthURL: "https://www.codebuddy.cn/auth", State: "state-cb"},
		tokenInfo: &service.CodeBuddyTokenInfo{
			AccessToken:  "access-canary-cb",
			RefreshToken: "refresh-canary-cb",
		},
	}}

	router, auditService := newOAuthAuditTestRouter(repo, func(r *gin.Engine) {
		r.POST("/api/v1/admin/codebuddy/oauth/auth-url", handler.GenerateAuthURL)
		r.POST("/api/v1/admin/codebuddy/oauth/poll", handler.PollToken)
		r.POST("/api/v1/admin/codebuddy/oauth/refresh-token", handler.RefreshToken)
	})

	postJSON(router, "/api/v1/admin/codebuddy/oauth/auth-url", `{}`)
	postJSON(router, "/api/v1/admin/codebuddy/oauth/poll", `{"state":"state-cb"}`)
	postJSON(router, "/api/v1/admin/codebuddy/oauth/refresh-token",
		`{"refresh_token":"refresh-canary-cb","uid":"uid-cb","enterprise_id":"ent-cb","domain":"tencent.com"}`)
	auditService.Stop()

	logs := repo.snapshot()
	require.Len(t, logs, 3)

	byAction := make(map[string]*service.AuditLog, len(logs))
	for _, entry := range logs {
		byAction[entry.Action] = entry
		// 认证上下文写入的操作者必须落库，实现「谁」的可追溯。
		require.NotNil(t, entry.ActorUserID)
		require.Equal(t, int64(77), *entry.ActorUserID)
		// token 明文绝不允许出现在任何审计字段。
		require.NotContains(t, entry.RequestBody, "refresh-canary-cb")
		require.NotContains(t, entry.RequestBody, "access-canary-cb")
	}

	require.Equal(t, "state-cb", byAction[service.AuditActionCodeBuddyOAuthAuthURL].Extra["oauth_state"])
	require.Equal(t, "state-cb", byAction[service.AuditActionCodeBuddyOAuthPoll].Extra["oauth_state"])
	require.Equal(t, "uid-cb", byAction[service.AuditActionCodeBuddyOAuthRefresh].Extra["target_uid"])
	require.Equal(t, http.StatusOK, byAction[service.AuditActionCodeBuddyOAuthRefresh].StatusCode)
}

// TestAntigravityOAuthEndpointsAuditCredentialOpsWithoutTokenPlaintext 验证 A1：
// antigravity 三个 OAuth 端点产生带固定动作名与目标标识的审计记录，code / refresh_token
// 明文不落审计。
func TestAntigravityOAuthEndpointsAuditCredentialOpsWithoutTokenPlaintext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &oauthAuditCaptureRepository{}

	handler := &AntigravityOAuthHandler{antigravityOAuthService: &antigravityOAuthServiceStub{
		authURL: &service.AntigravityAuthURLResult{AuthURL: "https://accounts.google.com/o/oauth2/auth", SessionID: "sess-1", State: "state-ag"},
		tokenInfo: &service.AntigravityTokenInfo{
			AccessToken:  "access-canary-ag",
			RefreshToken: "refresh-canary-ag",
		},
	}}

	router, auditService := newOAuthAuditTestRouter(repo, func(r *gin.Engine) {
		r.POST("/api/v1/admin/antigravity/oauth/auth-url", handler.GenerateAuthURL)
		r.POST("/api/v1/admin/antigravity/oauth/exchange-code", handler.ExchangeCode)
		r.POST("/api/v1/admin/antigravity/oauth/refresh-token", handler.RefreshToken)
	})

	postJSON(router, "/api/v1/admin/antigravity/oauth/auth-url", `{"proxy_id":12}`)
	postJSON(router, "/api/v1/admin/antigravity/oauth/exchange-code",
		`{"session_id":"sess-1","state":"state-ag","code":"code-canary-ag","proxy_id":12}`)
	postJSON(router, "/api/v1/admin/antigravity/oauth/refresh-token",
		`{"refresh_token":"refresh-canary-ag","proxy_id":12}`)
	auditService.Stop()

	logs := repo.snapshot()
	require.Len(t, logs, 3)

	byAction := make(map[string]*service.AuditLog, len(logs))
	for _, entry := range logs {
		byAction[entry.Action] = entry
		require.NotContains(t, entry.RequestBody, "refresh-canary-ag")
		require.NotContains(t, entry.RequestBody, "access-canary-ag")
		require.NotContains(t, entry.RequestBody, "code-canary-ag")
	}

	require.EqualValues(t, 12, byAction[service.AuditActionAntigravityOAuthAuthURL].Extra["proxy_id"])
	exchange := byAction[service.AuditActionAntigravityOAuthExchange]
	require.Equal(t, "state-ag", exchange.Extra["oauth_state"])
	require.EqualValues(t, 12, exchange.Extra["proxy_id"])
	require.EqualValues(t, 12, byAction[service.AuditActionAntigravityOAuthRefresh].Extra["proxy_id"])
}
