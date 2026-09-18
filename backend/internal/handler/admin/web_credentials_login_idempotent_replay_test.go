package admin

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 登录会话去重 × 幂等重放回归：去重检查位于幂等执行闭包内（不在闭包之外），
// 相同 Idempotency-Key 重试已成功的建号请求必须走 coordinator 重放路径直接
// 重放第一次成功响应，而不是被去重误判 409；无 key / 不同凭证的重复建号
// 仍按登录会话去重返回 409（reason=web_credential_duplicate）。

type replayAdminServiceStub struct {
	dedupAdminServiceStub
	createCalls int
}

// CreateAccount 把新建账号写回 stub.accounts（保持 Credentials 与请求凭证一致、
// access_mode=web）：若 CreateAccount 不回写 accounts，首次创建的账号不会进入
// ListAccountsForSchedulerScoreFilter 的去重列表，把去重移回闭包之外的测试也会
// 假通过——回写是本测试能真正约束"去重位于幂等闭包内"的前提。
func (s *replayAdminServiceStub) CreateAccount(_ context.Context, input *service.CreateAccountInput) (*service.Account, error) {
	s.createCalls++
	account := service.Account{
		ID:          int64(s.createCalls),
		Name:        fmt.Sprintf("新账号 %d", s.createCalls),
		Platform:    "zhipu",
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Credentials: input.Credentials,
	}
	s.accounts = append(s.accounts, account)
	return &account, nil
}

// ForceAntigravityPrivacy / ForceOpenAIPrivacy 供 Create 成功路径调用：本测试
// 关注点为幂等重放语义，直接放行。
func (s *replayAdminServiceStub) ForceAntigravityPrivacy(_ context.Context, _ *service.Account) string {
	return ""
}
func (s *replayAdminServiceStub) ForceOpenAIPrivacy(_ context.Context, _ *service.Account) string {
	return ""
}

func TestCreate_IdempotentReplayNotBlockedByLoginSessionDedup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newMemoryIdempotencyRepoStub()
	cfg := service.DefaultIdempotencyConfig()
	cfg.ProcessingTimeout = 2 * time.Second
	service.SetDefaultIdempotencyCoordinator(service.NewIdempotencyCoordinator(repo, cfg))
	t.Cleanup(func() {
		service.SetDefaultIdempotencyCoordinator(nil)
	})

	stub := &replayAdminServiceStub{}
	stub.accounts = []service.Account{
		{
			Name: "既有账号",
			Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "web",
				"cookie":      "chatglm_token=existing-session",
			},
		},
	}
	h := &AccountHandler{adminService: stub}
	router := gin.New()
	router.POST("/api/v1/admin/accounts", h.Create)

	call := func(idemKey string) (*httptest.ResponseRecorder, *AccountHandler) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts",
			bytes.NewBufferString(`{"name":"新账号","platform":"zhipu","type":"apikey","credentials":{"access_mode":"web","cookie":"chatglm_token=fresh-session"}}`))
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec, h
	}

	// 1. 首次请求（携带 key）→ 200，CreateAccount 执行一次。
	rec1, h1 := call("replay-key-1")
	require.Equal(t, http.StatusOK, rec1.Code, "首次创建应成功")
	require.Equal(t, 1, h1.adminService.(*replayAdminServiceStub).createCalls)

	// 补强断言：首次创建后 stub.accounts 必须含新账号（Credentials 与请求一致、
	// access_mode=web），否则去重列表缺失新账号、测试约束失效。
	require.Len(t, stub.accounts, 2, "新建账号必须写回 stub.accounts 进入去重列表")
	require.Equal(t, "新账号 1", stub.accounts[1].Name)
	require.Equal(t, "web", stub.accounts[1].Credentials["access_mode"])

	// 2. 相同 Idempotency-Key + 相同 web 凭证重试 → 重放第一次成功响应（非 409），
	//    去重闭包不执行，CreateAccount 不再次执行。
	rec2, h2 := call("replay-key-1")
	require.Equal(t, http.StatusOK, rec2.Code, "相同 key 重试必须重放成功响应而非 409 去重")
	require.NotContains(t, rec2.Body.String(), "web_credential_duplicate")
	require.Equal(t, "true", rec2.Header().Get("X-Idempotency-Replayed"))
	require.Equal(t, 1, h2.adminService.(*replayAdminServiceStub).createCalls, "重放不得再次执行 CreateAccount")

	// 3. 无 key + 与既有账号相同登录态 → 仍 409（登录会话去重生效）。
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts",
		bytes.NewBufferString(`{"name":"新账号","platform":"zhipu","type":"apikey","credentials":{"access_mode":"web","cookie":"chatglm_token=existing-session"}}`))
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	require.Equal(t, http.StatusConflict, rec3.Code, "无 key 的重复登录态建号仍须 409")
	require.Contains(t, rec3.Body.String(), "web_credential_duplicate")
	require.Equal(t, 1, stub.createCalls, "409 去重命中不得落库")
}
