package middleware

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestOAuthCredentialRoutesHaveStableAuditActions 钉住 6 个 OAuth 凭证端点的固定动作名：
// 这些端点获取/旋转平台凭证，动作名是审计检索与告警的锚点，不可随路由推导漂移。
func TestOAuthCredentialRoutesHaveStableAuditActions(t *testing.T) {
	expected := map[string]string{
		"POST /api/v1/admin/codebuddy/oauth/auth-url":        service.AuditActionCodeBuddyOAuthAuthURL,
		"POST /api/v1/admin/codebuddy/oauth/poll":            service.AuditActionCodeBuddyOAuthPoll,
		"POST /api/v1/admin/codebuddy/oauth/refresh-token":   service.AuditActionCodeBuddyOAuthRefreshToken,
		"POST /api/v1/admin/antigravity/oauth/auth-url":      service.AuditActionAntigravityOAuthAuthURL,
		"POST /api/v1/admin/antigravity/oauth/exchange-code": service.AuditActionAntigravityOAuthExchangeCode,
		"POST /api/v1/admin/antigravity/oauth/refresh-token": service.AuditActionAntigravityOAuthRefreshToken,
	}
	for route, action := range expected {
		require.Equalf(t, action, auditActionOverrides[route], "%s must have a stable audit action", route)
	}
	// OAuth 端点的请求体含 token/code 凭证，一律不得走整体入库白名单之外的裸记录：
	// refresh_token/code 由键级脱敏覆盖，这里断言它们不被列入"整体省略"而完全丢失审计。
	for route := range expected {
		_, omitted := auditBodyOmittedRoutes[route]
		require.Falsef(t, omitted, "%s should keep a redacted body so target identifiers remain auditable", route)
	}
}

// TestCodeBuddyOAuthAuditRecordsTargetWithoutToken 全链路验证 codebuddy 凭证端点的审计记录：
// 操作者、动作、目标（state/uid/enterprise_id）、结果与状态码可见，且记录体不含任何 token 明文。
func TestCodeBuddyOAuthAuditRecordsTargetWithoutToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &auditCaptureRepository{}
	auditService := service.NewAuditLogService(repository, nil)
	auditService.Start()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 77})
		c.Set(string(ContextKeyUserRole), "admin")
		c.Next()
	})
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	// stub handler 复刻真实 handler 的审计调用（SetAuditExtra 的键与值来源一致）。
	router.POST("/api/v1/admin/codebuddy/oauth/refresh-token", func(c *gin.Context) {
		SetAuditExtra(c, map[string]any{"result": "success", "uid": "u-1001", "enterprise_id": "e-9"})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.POST("/api/v1/admin/codebuddy/oauth/poll", func(c *gin.Context) {
		SetAuditExtra(c, map[string]any{"result": "failed", "state": "state-abc"})
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false})
	})

	requests := []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/v1/admin/codebuddy/oauth/refresh-token",
			bytes.NewBufferString(`{"refresh_token":"audit-canary-refresh-token","uid":"u-1001","enterprise_id":"e-9","domain":"tencent.com"}`)),
		httptest.NewRequest(http.MethodPost, "/api/v1/admin/codebuddy/oauth/poll",
			bytes.NewBufferString(`{"state":"state-abc"}`)),
	}
	for _, request := range requests {
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
	}
	auditService.Stop()

	repository.mu.Lock()
	logs := append([]*service.AuditLog(nil), repository.logs...)
	repository.mu.Unlock()
	require.Len(t, logs, 2)

	byAction := make(map[string]*service.AuditLog, len(logs))
	for _, entry := range logs {
		byAction[entry.Action] = entry
		// 记录体任何字段都不得含 token 明文。
		require.NotContains(t, entry.RequestBody, "audit-canary")
		require.NotContains(t, entry.ActorEmail, "audit-canary")
		if entry.Extra != nil {
			for _, v := range entry.Extra {
				require.NotContains(t, v, "audit-canary")
			}
		}
		require.NotNil(t, entry.ActorUserID)
		require.EqualValues(t, 77, *entry.ActorUserID)
	}

	refresh := byAction[service.AuditActionCodeBuddyOAuthRefreshToken]
	require.NotNil(t, refresh)
	require.Equal(t, http.StatusOK, refresh.StatusCode)
	require.Equal(t, "success", refresh.Extra["result"])
	require.Equal(t, "u-1001", refresh.Extra["uid"])
	require.Equal(t, "e-9", refresh.Extra["enterprise_id"])
	// 请求体入库的是脱敏版：refresh_token 被擦除，uid 等目标标识保留可追责。
	require.Contains(t, refresh.RequestBody, "***")
	require.Contains(t, refresh.RequestBody, "u-1001")

	poll := byAction[service.AuditActionCodeBuddyOAuthPoll]
	require.NotNil(t, poll)
	require.Equal(t, http.StatusInternalServerError, poll.StatusCode)
	require.Equal(t, "failed", poll.Extra["result"])
	require.Equal(t, "state-abc", poll.Extra["state"])
	require.Contains(t, poll.RequestBody, "state-abc")
}

// TestAntigravityOAuthAuditRedactsCodeAndToken 验证 antigravity 端点审计：
// authorization code（键级脱敏精确键 "code"）与 refresh_token 均不得入审计记录体。
func TestAntigravityOAuthAuditRedactsCodeAndToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &auditCaptureRepository{}
	auditService := service.NewAuditLogService(repository, nil)
	auditService.Start()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 88})
		c.Set(string(ContextKeyUserRole), "admin")
		c.Next()
	})
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	router.POST("/api/v1/admin/antigravity/oauth/exchange-code", func(c *gin.Context) {
		SetAuditExtra(c, map[string]any{"result": "success", "session_id": "sess-1", "state": "state-xyz"})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.POST("/api/v1/admin/antigravity/oauth/refresh-token", func(c *gin.Context) {
		SetAuditExtra(c, map[string]any{"result": "failed"})
		c.JSON(http.StatusBadGateway, gin.H{"ok": false})
	})

	requests := []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/v1/admin/antigravity/oauth/exchange-code",
			bytes.NewBufferString(`{"session_id":"sess-1","state":"state-xyz","code":"audit-canary-auth-code"}`)),
		httptest.NewRequest(http.MethodPost, "/api/v1/admin/antigravity/oauth/refresh-token",
			bytes.NewBufferString(`{"refresh_token":"audit-canary-refresh-token"}`)),
	}
	for _, request := range requests {
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
	}
	auditService.Stop()

	repository.mu.Lock()
	logs := append([]*service.AuditLog(nil), repository.logs...)
	repository.mu.Unlock()
	require.Len(t, logs, 2)

	byAction := make(map[string]*service.AuditLog, len(logs))
	for _, entry := range logs {
		byAction[entry.Action] = entry
		require.NotContains(t, entry.RequestBody, "audit-canary")
	}

	exchange := byAction[service.AuditActionAntigravityOAuthExchangeCode]
	require.NotNil(t, exchange)
	require.Equal(t, http.StatusOK, exchange.StatusCode)
	require.Equal(t, "success", exchange.Extra["result"])
	require.Equal(t, "sess-1", exchange.Extra["session_id"])
	require.Equal(t, "state-xyz", exchange.Extra["state"])

	refresh := byAction[service.AuditActionAntigravityOAuthRefreshToken]
	require.NotNil(t, refresh)
	require.Equal(t, http.StatusBadGateway, refresh.StatusCode)
	require.Equal(t, "failed", refresh.Extra["result"])
}

func TestDeriveAuditAction(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   string
	}{
		{"PUT", "/api/v1/admin/accounts/:id", "admin.accounts.update"},
		{"POST", "/api/v1/admin/accounts", "admin.accounts.create"},
		{"DELETE", "/api/v1/admin/backups/:id", "admin.backups.delete"},
		{"GET", "/api/v1/admin/users/:id/api-keys", "admin.users.api_keys.read"},
		{"POST", "/api/v1/admin/redeem-codes/batch", "admin.redeem_codes.batch.create"},
	}
	for _, tc := range cases {
		if got := deriveAuditAction(tc.method, tc.path); got != tc.want {
			t.Fatalf("deriveAuditAction(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

type auditCaptureRepository struct {
	mu   sync.Mutex
	logs []*service.AuditLog
}

func (r *auditCaptureRepository) BatchInsert(_ context.Context, logs []*service.AuditLog) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, logs...)
	return int64(len(logs)), nil
}
func (r *auditCaptureRepository) Insert(_ context.Context, log *service.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
	return nil
}
func (r *auditCaptureRepository) List(context.Context, *service.AuditLogFilter) (*service.AuditLogList, error) {
	return &service.AuditLogList{}, nil
}
func (r *auditCaptureRepository) GetByID(context.Context, int64) (*service.AuditLog, error) {
	return nil, service.ErrAuditLogNotFound
}
func (r *auditCaptureRepository) Count(context.Context) (int64, error) { return 0, nil }
func (r *auditCaptureRepository) TruncateAll(context.Context) error    { return nil }
func (r *auditCaptureRepository) DeleteBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func TestPromptAuditAdminOperationsUseOmittedBodiesAndAllowlistedDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &auditCaptureRepository{}
	auditService := service.NewAuditLogService(repository, nil)
	auditService.Start()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 77})
		c.Set(string(ContextKeyUserRole), "admin")
		c.Next()
	})
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	router.PUT("/api/v1/admin/prompt-audit/config", func(c *gin.Context) {
		SetAuditExtra(c, map[string]any{
			"result": "failed", "error_code": "prompt_audit_config_conflict", "config_version": int64(9),
			"token": "audit-canary-secret", "raw_prompt": "audit-canary-prompt", "nested": map[string]any{"unsafe": true},
		})
		c.JSON(http.StatusConflict, gin.H{"ok": false})
	})
	router.POST("/api/v1/admin/prompt-audit/endpoints/probe", func(c *gin.Context) {
		SetAuditExtra(c, map[string]any{
			"result": "success", "guard_endpoint_id": "guard-1", "http_status": 200,
			"latency_ms": 12, "token_applied": true,
		})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPut, "/api/v1/admin/prompt-audit/config", bytes.NewBufferString(`{"expected_config_version":8,"token":"audit-canary-secret"}`)),
		httptest.NewRequest(http.MethodPost, "/api/v1/admin/prompt-audit/endpoints/probe", bytes.NewBufferString(`{"endpoint":{"token":"audit-canary-secret"}}`)),
	} {
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
	}
	auditService.Stop()

	repository.mu.Lock()
	logs := append([]*service.AuditLog(nil), repository.logs...)
	repository.mu.Unlock()
	require.Len(t, logs, 2)

	byAction := make(map[string]*service.AuditLog, len(logs))
	for _, entry := range logs {
		byAction[entry.Action] = entry
		require.Equal(t, "<credential-bearing body omitted>", entry.RequestBody)
		require.NotContains(t, entry.RequestBody, "audit-canary")
		require.NotContains(t, entry.Extra, "token")
		require.NotContains(t, entry.Extra, "raw_prompt")
		require.NotContains(t, entry.Extra, "nested")
	}

	config := byAction["admin.prompt_audit.config.update"]
	require.NotNil(t, config)
	require.Equal(t, http.StatusConflict, config.StatusCode)
	require.Equal(t, "failed", config.Extra["result"])
	require.Equal(t, "prompt_audit_config_conflict", config.Extra["error_code"])
	require.EqualValues(t, 9, config.Extra["config_version"])

	probe := byAction["admin.prompt_audit.endpoint.probe"]
	require.NotNil(t, probe)
	require.Equal(t, http.StatusOK, probe.StatusCode)
	require.Equal(t, "success", probe.Extra["result"])
	require.Equal(t, "guard-1", probe.Extra["guard_endpoint_id"])
	require.Equal(t, true, probe.Extra["token_applied"])
}

func TestPromptAuditMutationAuditRoutesHaveStableActionsAndOmitBodies(t *testing.T) {
	expected := map[string]string{
		"PUT /api/v1/admin/prompt-audit/config":                   "admin.prompt_audit.config.update",
		"POST /api/v1/admin/prompt-audit/endpoints/probe":         "admin.prompt_audit.endpoint.probe",
		"DELETE /api/v1/admin/prompt-audit/events/:id":            "admin.prompt_audit.event.delete",
		"POST /api/v1/admin/prompt-audit/events/batch-delete":     "admin.prompt_audit.events.batch_delete",
		"POST /api/v1/admin/prompt-audit/events/delete-preview":   "admin.prompt_audit.events.delete_preview",
		"POST /api/v1/admin/prompt-audit/events/delete-by-filter": "admin.prompt_audit.events.filter_delete",
	}
	for route, action := range expected {
		require.Equal(t, action, auditActionOverrides[route])
		_, omitted := auditBodyOmittedRoutes[route]
		require.Truef(t, omitted, "%s must not persist its credential or confirmation-bearing body", route)
	}
}

func TestPasskeyLoginAuditUsesCanonicalLoginActionAndOmitsCredentialBody(t *testing.T) {
	route := "POST /api/v1/auth/passkey/login/finish"
	require.Equal(t, service.AuditActionLogin, auditActionOverrides[route])
	require.Contains(t, auditBodyOmittedRoutes, route)
}

// Ollama 会话保存的请求体整体就是浏览器 Cookie 明文，键级脱敏清单曾漏掉裸键
// "session"，必须走整体不入库路径，防止会话凭证长期留存在 audit_logs。
func TestOllamaCloudUsageSessionRouteOmitsAuditBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.Contains(t, auditBodyOmittedRoutes, "PUT /api/v1/admin/accounts/:id/ollama-cloud-usage/session")

	repository := &auditCaptureRepository{}
	auditService := service.NewAuditLogService(repository, nil)
	auditService.Start()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 77})
		c.Set(string(ContextKeyUserRole), "admin")
		c.Next()
	})
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	router.PUT("/api/v1/admin/accounts/:id/ollama-cloud-usage/session", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/7/ollama-cloud-usage/session",
		bytes.NewBufferString(`{"session":"wos-session=audit-canary-cookie; __Secure-authjs.session-token.0=audit-canary-shard"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	auditService.Stop()

	repository.mu.Lock()
	logs := append([]*service.AuditLog(nil), repository.logs...)
	repository.mu.Unlock()
	require.Len(t, logs, 1)
	require.Equal(t, "<credential-bearing body omitted>", logs[0].RequestBody)
	require.NotContains(t, logs[0].RequestBody, "audit-canary")
}

// TestSetAuditExtra_AllowsCodeBuddySite 钉住 site 进入审计 extra 白名单：
// CodeBuddy 国际版/国内版操作必须可区分（Phase 2 验收 "审计含 intl 操作"）；
// 同时确认非白名单键（如凭证）仍被拒绝。
func TestSetAuditExtra_AllowsCodeBuddySite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	SetAuditExtra(c, map[string]any{"result": "success", "site": "intl"})

	extra, ok := c.MustGet(auditCtxKeyExtra).(map[string]any)
	require.True(t, ok)
	require.Equal(t, "intl", extra["site"])
	require.Equal(t, "success", extra["result"])

	SetAuditExtra(c, map[string]any{"access_token": "secret-should-not-pass"})
	extra2, _ := c.MustGet(auditCtxKeyExtra).(map[string]any)
	require.NotContains(t, extra2, "access_token")
}
