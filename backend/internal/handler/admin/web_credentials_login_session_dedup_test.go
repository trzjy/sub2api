package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 登录会话去重回归（web-platform-account-pool-auto-login-plan.md §0：登录会话隔离）：
// 同平台既有 web 账号已持有相同登录态（Cookie / Kimi access_token）时，新建会话
// 复用同一网页登录会话不是新账号——ValidateWebCredentials 必须拒绝（409 +
// reason=web_credential_duplicate）而不是伪装成多账号成功。

type dedupAdminServiceStub struct {
	service.AdminService
	accounts []service.Account
}

func (s *dedupAdminServiceStub) ListAccountsForSchedulerScoreFilter(_ context.Context, _, _, _, _ string, _ int64, _ string) ([]service.Account, error) {
	return s.accounts, nil
}

// CreateAccount 供 Create 处理器幂等链路调用：去重未命中时到达此处即视为
// 通过本测试关注点（返回明确错误而非 409 登录会话重复）。
func (s *dedupAdminServiceStub) CreateAccount(_ context.Context, _ *service.CreateAccountInput) (*service.Account, error) {
	return nil, fmt.Errorf("stub: create not exercised in dedup test")
}

func newDedupTestHandler(accounts []service.Account) *AccountHandler {
	return &AccountHandler{adminService: &dedupAdminServiceStub{accounts: accounts}}
}

func dedupValidateRequest(platform, credentialsJSON string) (*httptest.ResponseRecorder, *AccountHandler) {
	gin.SetMode(gin.TestMode)
	h := newDedupTestHandler([]service.Account{
		{
			Name: "既有账号",
			Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "web",
				"cookie":      "chatglm_token=existing-session",
			},
		},
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
		strings.NewReader(`{"platform":"`+platform+`","credentials":`+credentialsJSON+`}`))
	h.ValidateWebCredentials(c)
	return rec, h
}

func TestValidateWebCredentials_DuplicateLoginSessionRejected(t *testing.T) {
	// 1. zhipu：候选 Cookie 与既有 web 账号完全相同 → 409 + reason。
	rec, _ := dedupValidateRequest("zhipu", `{"access_mode":"web","cookie":"chatglm_token=existing-session"}`)
	require.Equal(t, http.StatusConflict, rec.Code, "同一登录会话的 Cookie 不得伪装成新账号")
	require.Contains(t, rec.Body.String(), "web_credential_duplicate")
	require.Contains(t, rec.Body.String(), "既有账号")

	// 2. deepseek：不同 Cookie → 200。
	rec, _ = dedupValidateRequest("deepseek", `{"access_mode":"web","cookie":"ds_session_id=fresh-session"}`)
	require.Equal(t, http.StatusOK, rec.Code, "全新登录态应放行")

	// 3. kimi：候选 access_token 与既有 kimi web 账号相同 → 409。
	h := newDedupTestHandler([]service.Account{
		{
			Name: "kimi 既有",
			Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode":  "web",
				"access_token": "tok-existing",
			},
		},
	})
	rec = httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
		strings.NewReader(`{"platform":"kimi","credentials":{"access_mode":"web","access_token":"tok-existing"}}`))
	h.ValidateWebCredentials(c)
	require.Equal(t, http.StatusConflict, rec.Code, "kimi 同一登录会话 Token 不得伪装成新账号")
	require.Contains(t, rec.Body.String(), "web_credential_duplicate")

	// 4. 既有同 Cookie 账号但为 api 模式（非 web 登录态）→ 200（去重只比较 web 账号）。
	h = newDedupTestHandler([]service.Account{
		{
			Name: "api 账号",
			Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "api",
				"api_key":     "sk-x",
				"cookie":      "chatglm_token=existing-session",
			},
		},
	})
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
		strings.NewReader(`{"platform":"zhipu","credentials":{"access_mode":"web","cookie":"chatglm_token=existing-session"}}`))
	h.ValidateWebCredentials(c)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestValidateWebCredentials_CookieOrderInsensitiveDuplicate(t *testing.T) {
	// 同一会话的 Cookie 串字段顺序差异不影响去重判定（归一化比较）。
	h := newDedupTestHandler([]service.Account{
		{
			Name: "既有账号",
			Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "web",
				"cookie":      "chatglm_token=t1; chatglm_user_id=u1",
			},
		},
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
		strings.NewReader(`{"platform":"zhipu","credentials":{"access_mode":"web","cookie":"chatglm_user_id=u1; chatglm_token=t1"}}`))
	h.ValidateWebCredentials(c)
	require.Equal(t, http.StatusConflict, rec.Code, "字段顺序不同的同一登录会话 Cookie 仍应命中去重")
}

// TestValidateWebCredentials_DuplicateBeyondFirstPage 覆盖分页截断场景：重复
// 登录态账号位于第一页之外（>1000 个账号）时，去重检查仍必须命中——去重走
// 全量扫描（ListAccountsForSchedulerScoreFilter），不受分页上限截断影响。
func TestValidateWebCredentials_DuplicateBeyondFirstPage(t *testing.T) {
	accounts := make([]service.Account, 0, 1500)
	for i := 0; i < 1499; i++ {
		accounts = append(accounts, service.Account{
			Name: fmt.Sprintf("web 账号 %04d", i),
			Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "web",
				"cookie":      "chatglm_token=session-" + strconv.Itoa(i),
			},
		})
	}
	// 重复登录态账号排在"第一页"之后。
	accounts = append(accounts, service.Account{
		Name: "末页既有账号",
		Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_mode": "web",
			"cookie":      "chatglm_token=tail-duplicate-session",
		},
	})

	h := newDedupTestHandler(accounts)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
		strings.NewReader(`{"platform":"zhipu","credentials":{"access_mode":"web","cookie":"chatglm_token=tail-duplicate-session"}}`))
	h.ValidateWebCredentials(c)
	require.Equal(t, http.StatusConflict, rec.Code, "超过 1000 个账号时末页重复 Cookie 不得漏判")
	require.Contains(t, rec.Body.String(), "web_credential_duplicate")
	require.Contains(t, rec.Body.String(), "末页既有账号")
}

// TestCreate_DuplicateLoginSessionRejected 覆盖创建端点的纵深防御：绕过
// ValidateWebCredentials 预校验的创建路径（主表单直接粘贴、直连 API 调用）
// 携带既有账号登录态建号时，Create 必须同样 409（reason=web_credential_duplicate）
// 且不落库（幂等执行前的同步拒绝）。
func TestCreate_DuplicateLoginSessionRejected(t *testing.T) {
	h := newDedupTestHandler([]service.Account{
		{
			Name: "既有账号",
			Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "web",
				"cookie":      "chatglm_token=existing-session",
			},
		},
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts",
		strings.NewReader(`{"name":"新账号","platform":"zhipu","type":"apikey","credentials":{"access_mode":"web","cookie":"chatglm_token=existing-session"}}`))
	h.Create(c)
	require.Equal(t, http.StatusConflict, rec.Code, "创建端点不得复用既有登录会话伪装成新账号")
	require.Contains(t, rec.Body.String(), "web_credential_duplicate")
	require.Contains(t, rec.Body.String(), "既有账号")

	// 全新登录态：不在创建端点拦截（后续走幂等创建链路，本测试桩不覆盖）。
	h2 := newDedupTestHandler([]service.Account{
		{
			Name: "既有账号",
			Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "web",
				"cookie":      "chatglm_token=existing-session",
			},
		},
	})
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts",
		strings.NewReader(`{"name":"新账号","platform":"zhipu","type":"apikey","credentials":{"access_mode":"web","cookie":"chatglm_token=fresh-session"}}`))
	h2.Create(c2)
	require.NotEqual(t, http.StatusConflict, rec2.Code, "全新登录态不得被创建端点去重拦截")
}

// ===== 批量创建 / 复制 / 更新 / 导入路径的登录会话去重回归 =====

// dedupBatchAdminServiceStub 在 dedupAdminServiceStub 基础上补充 GetAccount /
// UpdateAccount / DuplicateAccount 桩与调用计数，供批量与更新路径断言
// 「去重命中时不得触达后续写入链路」。
type dedupBatchAdminServiceStub struct {
	dedupAdminServiceStub
	getAccount     *service.Account
	createCalls    int
	updateCalls    int
	duplicateCalls int
}

func (s *dedupBatchAdminServiceStub) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	if s.getAccount != nil {
		return s.getAccount, nil
	}
	return &service.Account{ID: id, Name: "account", Status: service.StatusActive}, nil
}

// ValidateAccountGroupBindings 供 BatchCreate 前置校验调用：默认放行。
func (s *dedupBatchAdminServiceStub) ValidateAccountGroupBindings(_ context.Context, _ []int64) error {
	return nil
}

func (s *dedupBatchAdminServiceStub) CreateAccount(_ context.Context, _ *service.CreateAccountInput) (*service.Account, error) {
	s.createCalls++
	return nil, fmt.Errorf("stub: create not exercised in dedup test")
}

func (s *dedupBatchAdminServiceStub) UpdateAccount(_ context.Context, id int64, _ *service.UpdateAccountInput) (*service.Account, error) {
	s.updateCalls++
	return &service.Account{ID: id, Name: "account", Status: service.StatusActive}, nil
}

func (s *dedupBatchAdminServiceStub) DuplicateAccount(_ context.Context, id int64, _, _ string) (*service.Account, error) {
	s.duplicateCalls++
	return &service.Account{ID: id + 1, Name: "copy", Status: service.StatusActive, Schedulable: false}, nil
}

// TestBatchCreate_DuplicateLoginSessionRejected 覆盖批量导入：携带既有账号登录态
// 的 web 项按批量语义计为该项 failed（附重复文案），不得触达 CreateAccount；
// 全新登录态与非 web 平台项不受影响。
func TestBatchCreate_DuplicateLoginSessionRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &dedupBatchAdminServiceStub{}
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
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/batch",
		strings.NewReader(`{"accounts":[
			{"name":"重复项","platform":"zhipu","type":"apikey","credentials":{"access_mode":"web","cookie":"chatglm_token=existing-session"}},
			{"name":"全新项","platform":"zhipu","type":"apikey","credentials":{"access_mode":"web","cookie":"chatglm_token=fresh-session"}},
			{"name":"非web项","platform":"anthropic","type":"apikey","credentials":{"api_key":"sk-ant","cookie":"chatglm_token=existing-session"}}
		]}`))
	h.BatchCreate(c)
	require.Equal(t, http.StatusOK, rec.Code, "批量接口按项报告结果而非整体 409")

	var body struct {
		Data struct {
			Success int `json:"success"`
			Failed  int `json:"failed"`
			Results []struct {
				Name    string `json:"name"`
				Success bool   `json:"success"`
				Error   string `json:"error"`
			} `json:"results"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	byName := map[string]struct {
		success bool
		err     string
	}{}
	for _, r := range body.Data.Results {
		byName[r.Name] = struct {
			success bool
			err     string
		}{r.Success, r.Error}
	}

	dup := byName["重复项"]
	require.False(t, dup.success, "同登录态批量项必须被拒")
	require.Contains(t, dup.err, "既有账号", "重复文案应指明既有账号")
	require.Contains(t, dup.err, "登录会话")

	fresh := byName["全新项"]
	require.False(t, fresh.success)
	require.NotContains(t, fresh.err, "登录会话", "全新登录态不得被去重拦截")

	nonWeb := byName["非web项"]
	require.False(t, nonWeb.success)
	require.NotContains(t, nonWeb.err, "登录会话", "非 web 平台不得被去重拦截")

	// 去重命中的项不得触达 CreateAccount；通过项会触达（桩返回错误计一次）。
	require.Equal(t, stub.createCalls, 2, "仅全新项与非 web 项触达 CreateAccount，重复项被门禁拦截")
}

// TestDuplicate_WebPlatformRejected 覆盖复制路径：复制 web 平台（apikey +
// access_mode=web）账号 = 同一登录态第二账号，必须 409 拒绝且不触达
// DuplicateAccount；非 web 平台复制不受影响。
func TestDuplicate_WebPlatformRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 1. web 平台 → 409。
	stub := &dedupBatchAdminServiceStub{}
	stub.getAccount = &service.Account{
		ID:       7,
		Name:     "web 源账号",
		Platform: "zhipu",
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_mode": "web",
			"cookie":      "chatglm_token=existing-session",
		},
	}
	h := &AccountHandler{adminService: stub}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "7"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/7/duplicate", nil)
	h.Duplicate(c)
	require.Equal(t, http.StatusConflict, rec.Code, "复制 web 平台账号不得产生共享登录态的第二账号")
	require.Contains(t, rec.Body.String(), "web_credential_duplicate")
	require.Zero(t, stub.duplicateCalls, "去重命中不得触达 DuplicateAccount")

	// 2. 非 web 平台 → 放行到 DuplicateAccount（桩返回成功）。
	stub2 := &dedupBatchAdminServiceStub{}
	stub2.getAccount = &service.Account{
		ID:          8,
		Name:        "api 源账号",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-ant"},
	}
	h2 := &AccountHandler{adminService: stub2}
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Params = gin.Params{{Key: "id", Value: "8"}}
	c2.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/8/duplicate", nil)
	h2.Duplicate(c2)
	require.NotEqual(t, http.StatusConflict, rec2.Code, "非 web 平台复制不受去重影响")
	require.Equal(t, 1, stub2.duplicateCalls)
}

// TestUpdate_LoginSessionDedupSelfExclusion 覆盖更新路径（排除自身语义）：
// 1) 把账号 Cookie 改成其他账号已持有的登录态 → 409；
// 2) 更新自身当前登录态（值不变）→ 放行（排除自身）；
// 3) 非 web 平台 → 不受影响。
func TestUpdate_LoginSessionDedupSelfExclusion(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newStub := func(list []service.Account, self *service.Account) *dedupBatchAdminServiceStub {
		stub := &dedupBatchAdminServiceStub{}
		stub.accounts = list
		stub.getAccount = self
		return stub
	}

	selfAccount := func(id int64, cookie string) *service.Account {
		return &service.Account{
			ID:       id,
			Name:     "自身账号",
			Platform: "zhipu",
			Type:     service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "web",
				"cookie":      cookie,
			},
		}
	}

	otherAccount := func(id int64, name, cookie string) service.Account {
		return service.Account{
			ID:   id,
			Name: name,
			Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "web",
				"cookie":      cookie,
			},
		}
	}

	// 1. 改成他人登录态 → 409，UpdateAccount 不得被调用。
	stub := newStub(
		[]service.Account{otherAccount(9, "他人账号", "chatglm_token=other-session")},
		selfAccount(7, "chatglm_token=self-session"),
	)
	h := &AccountHandler{adminService: stub}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "7"}}
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/7",
		strings.NewReader(`{"credentials":{"access_mode":"web","cookie":"chatglm_token=other-session"}}`))
	h.Update(c)
	require.Equal(t, http.StatusConflict, rec.Code, "更新不得把账号改成其他账号已持有的登录态")
	require.Contains(t, rec.Body.String(), "web_credential_duplicate")
	require.Zero(t, stub.updateCalls)

	// 2. 自身 Cookie 值不变 → 排除自身，放行（UpdateAccount 被调用）。
	stub2 := newStub(
		[]service.Account{otherAccount(9, "他人账号", "chatglm_token=other-session")},
		selfAccount(7, "chatglm_token=self-session"),
	)
	h2 := &AccountHandler{adminService: stub2}
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Params = gin.Params{{Key: "id", Value: "7"}}
	c2.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/7",
		strings.NewReader(`{"credentials":{"access_mode":"web","cookie":"chatglm_token=self-session"}}`))
	h2.Update(c2)
	require.NotEqual(t, http.StatusConflict, rec2.Code, "更新自身当前登录态不得被误拒")
	require.Equal(t, 1, stub2.updateCalls)

	// 3. 非 web 平台携带同串 Cookie → 不受 web 登录态去重影响。
	stub3 := newStub(
		[]service.Account{otherAccount(9, "web 既有", "chatglm_token=other-session")},
		&service.Account{
			ID:          7,
			Name:        "anthropic 账号",
			Platform:    service.PlatformAnthropic,
			Type:        service.AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-ant"},
		},
	)
	h3 := &AccountHandler{adminService: stub3}
	rec3 := httptest.NewRecorder()
	c3, _ := gin.CreateTestContext(rec3)
	c3.Params = gin.Params{{Key: "id", Value: "7"}}
	c3.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/7",
		strings.NewReader(`{"credentials":{"cookie":"chatglm_token=other-session"}}`))
	h3.Update(c3)
	require.NotEqual(t, http.StatusConflict, rec3.Code, "非 web 平台不得被 web 登录态去重拦截")
	require.Equal(t, 1, stub3.updateCalls)

	// 4. kimi access_token 更新成他人 Token → 409。
	kimiSelf := &service.Account{
		ID:       7,
		Name:     "kimi 自身",
		Platform: service.PlatformKimi,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_mode":  "web",
			"access_token": "tok-self",
		},
	}
	kimiOther := service.Account{
		ID:   9,
		Name: "kimi 他人",
		Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_mode":  "web",
			"access_token": "tok-other",
		},
	}
	stub4 := newStub([]service.Account{kimiOther}, kimiSelf)
	h4 := &AccountHandler{adminService: stub4}
	rec4 := httptest.NewRecorder()
	c4, _ := gin.CreateTestContext(rec4)
	c4.Params = gin.Params{{Key: "id", Value: "7"}}
	c4.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/7",
		strings.NewReader(`{"credentials":{"access_token":"tok-other"}}`))
	h4.Update(c4)
	require.Equal(t, http.StatusConflict, rec4.Code, "kimi 更新不得复用他人 access_token 登录态")
	require.Zero(t, stub4.updateCalls)
}

// TestUpdate_WebToAPISwitchNotBlockedByLoginSessionDedup 覆盖 access_mode 切换：
// 去重门仅对"合并后目标 access_mode 仍为 web"生效——web 账号切换为 API 模式时
// （请求仍携带被其他 Web 账号持有的旧 Cookie）不得被 409 web_credential_duplicate
// 拦截；请求未显式提供 access_mode 时沿用原账号模式（web→web 门生效）。
func TestUpdate_WebToAPISwitchNotBlockedByLoginSessionDedup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newStub := func(list []service.Account, self *service.Account) *dedupBatchAdminServiceStub {
		stub := &dedupBatchAdminServiceStub{}
		stub.accounts = list
		stub.getAccount = self
		return stub
	}

	selfWebAccount := func(id int64, cookie string) *service.Account {
		return &service.Account{
			ID:       id,
			Name:     "自身账号",
			Platform: "zhipu",
			Type:     service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "web",
				"cookie":      cookie,
			},
		}
	}

	otherWebAccount := func(id int64, name, cookie string) service.Account {
		return service.Account{
			ID:   id,
			Name: name,
			Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_mode": "web",
				"cookie":      cookie,
			},
		}
	}

	// 1. web→api 显式切换（携带被他人持有的旧 Cookie）→ 放行，UpdateAccount 被调用。
	stub := newStub(
		[]service.Account{otherWebAccount(9, "他人账号", "chatglm_token=other-session")},
		selfWebAccount(7, "chatglm_token=self-session"),
	)
	h := &AccountHandler{adminService: stub}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "7"}}
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/7",
		strings.NewReader(`{"credentials":{"access_mode":"api","api_key":"sk-x","cookie":"chatglm_token=other-session"}}`))
	h.Update(c)
	require.NotEqual(t, http.StatusConflict, rec.Code, "web→api 模式切换不得被 web 登录态去重拦截")
	require.Equal(t, 1, stub.updateCalls)

	// 2. 未显式提供 access_mode（沿用原账号 web 模式）+ 他人登录态 → 409 门仍生效。
	stub2 := newStub(
		[]service.Account{otherWebAccount(9, "他人账号", "chatglm_token=other-session")},
		selfWebAccount(7, "chatglm_token=self-session"),
	)
	h2 := &AccountHandler{adminService: stub2}
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Params = gin.Params{{Key: "id", Value: "7"}}
	c2.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/7",
		strings.NewReader(`{"credentials":{"cookie":"chatglm_token=other-session"}}`))
	h2.Update(c2)
	require.Equal(t, http.StatusConflict, rec2.Code, "未显式提供 access_mode 时沿用原 web 模式，去重门必须生效")
	require.Zero(t, stub2.updateCalls)
}
