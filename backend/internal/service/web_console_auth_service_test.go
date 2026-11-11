//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 假 helper（/web-auth/*）
// ---------------------------------------------------------------------------

// wcAuthFakeHelper 是 local_captcha_helper /web-auth/* 的 httptest 替身。
type wcAuthFakeHelper struct {
	server *httptest.Server

	mu            sync.Mutex
	startCode     int
	startBody     string
	statusCode    int
	statusBody    string
	resultCode    int
	resultBody    string
	startRequests []map[string]any
	startHeaders  []http.Header
	statusTargets []string
	resultReqs    []map[string]any
	resultHeaders []http.Header
}

// wcAuthNewFakeHelper 构造默认全成功的假 helper。
func wcAuthNewFakeHelper(t *testing.T) *wcAuthFakeHelper {
	t.Helper()
	h := &wcAuthFakeHelper{
		startCode:  http.StatusOK,
		startBody:  `{"success":true,"session_id":"sess-abc","status":"running"}`,
		statusCode: http.StatusOK,
		statusBody: `{"success":true,"status":"running"}`,
		resultCode: http.StatusOK,
		resultBody: `{"success":true,"data":{"storage_state":"{\"cookies\":[]}","account_label":"alice@example.com"}}`,
	}
	h.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)

		h.mu.Lock()
		defer h.mu.Unlock()

		rw.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/web-auth/start":
			h.startRequests = append(h.startRequests, decoded)
			h.startHeaders = append(h.startHeaders, r.Header.Clone())
			rw.WriteHeader(h.startCode)
			_, _ = rw.Write([]byte(h.startBody))
		case strings.HasSuffix(r.URL.Path, "/status"):
			h.statusTargets = append(h.statusTargets, r.URL.Path+"?"+r.URL.RawQuery)
			rw.WriteHeader(h.statusCode)
			_, _ = rw.Write([]byte(h.statusBody))
		case strings.HasSuffix(r.URL.Path, "/result"):
			h.resultReqs = append(h.resultReqs, decoded)
			h.resultHeaders = append(h.resultHeaders, r.Header.Clone())
			rw.WriteHeader(h.resultCode)
			_, _ = rw.Write([]byte(h.resultBody))
		default:
			rw.WriteHeader(http.StatusNotFound)
			_, _ = rw.Write([]byte(`{"success":false}`))
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *wcAuthFakeHelper) lastStart() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.startRequests) == 0 {
		return nil
	}
	return h.startRequests[len(h.startRequests)-1]
}

func (h *wcAuthFakeHelper) startCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.startRequests)
}

func (h *wcAuthFakeHelper) lastStatusTarget() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.statusTargets) == 0 {
		return ""
	}
	return h.statusTargets[len(h.statusTargets)-1]
}

// wcAuthClientFor 构造指向假 helper 的真实 HTTP 客户端。
func wcAuthClientFor(t *testing.T, helper *wcAuthFakeHelper) LocalCaptchaHelperWebAuth {
	t.Helper()
	client := NewLocalCaptchaHelperWebAuthClient(LocalCaptchaHelperConfig{
		BaseURL: helper.server.URL,
		APIKey:  "helper-key",
		Timeout: 5,
	})
	require.NotNil(t, client)
	return client
}

// wcAuthService 构造带可用 worker 与假 helper 的编排服务。
func wcAuthService(t *testing.T, repo *wcqRepoStub, helper LocalCaptchaHelperWebAuth) (*WebConsoleAuthService, *wcqFakeWorker) {
	t.Helper()
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(data))
	quota := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))
	return NewWebConsoleAuthService(quota, helper, repo), worker
}

// ---------------------------------------------------------------------------
// 用例：StartInteractiveLogin
// ---------------------------------------------------------------------------

func TestWebConsoleAuthService_Start_NotApplicable(t *testing.T) {
	repo := &wcqRepoStub{account: &Account{
		ID:          1,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://api.deepseek.com/v1"},
	}}
	helper := wcAuthNewFakeHelper(t)
	svc, _ := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	session, err := svc.StartInteractiveLogin(context.Background(), 1)
	require.ErrorIs(t, err, ErrWebConsoleAuthNotApplicable)
	require.Nil(t, session)
	require.Equal(t, 0, helper.startCount(), "不适用时不得调用 helper")
}

func TestWebConsoleAuthService_Start_Success(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(7)}
	helper := wcAuthNewFakeHelper(t)
	fixedNow := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	svc, _ := wcAuthService(t, repo, wcAuthClientFor(t, helper))
	svc.now = func() time.Time { return fixedNow }

	session, err := svc.StartInteractiveLogin(context.Background(), 7)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.Equal(t, "sess-abc", session.ID)
	require.Equal(t, int64(7), session.AccountID)
	require.Equal(t, providerCompshare, session.Provider)
	require.Equal(t, "Alice@Example.com", session.ExpectedUsername)
	require.Equal(t, fixedNow.Add(webConsoleAuthSessionTTL), session.ExpiresAt)
	require.True(t, strings.HasPrefix(session.Binding, "acct-7-"), "binding 必须为 acct-<id>-<nano>：%s", session.Binding)

	req := helper.lastStart()
	require.NotNil(t, req)
	endpoints := webConsoleQuotaProviderEndpointTable[providerCompshare]
	require.Equal(t, providerCompshare, req["provider"])
	require.Equal(t, session.Binding, req["session_binding_id"])
	require.Equal(t, endpoints.loginURL, req["login_url"])
	require.Equal(t, endpoints.successURLPrefix, req["success_url_prefix"])
	require.Equal(t, "Alice@Example.com", req["username"])
	require.Equal(t, "pw-should-never-leak", req["password"])
	require.EqualValues(t, webConsoleAuthHelperDefaultTimeout, req["timeout"])

	// helper 只认 X-API-Key 头。
	require.Equal(t, "helper-key", helper.startHeaders[0].Get("X-API-Key"))
}

func TestWebConsoleAuthService_Start_BusyConflict(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(8)}
	helper := wcAuthNewFakeHelper(t)
	helper.startCode = http.StatusConflict
	helper.startBody = `{"success":false,"message":"已有 web 认证会话占用单槽位"}`
	svc, _ := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	session, err := svc.StartInteractiveLogin(context.Background(), 8)
	require.Error(t, err)
	require.ErrorIs(t, err, errWebConsoleAuthHelperBusy)
	require.Nil(t, session)
}

func TestWebConsoleAuthService_Start_BusyTooManyRequests(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(9)}
	helper := wcAuthNewFakeHelper(t)
	helper.startCode = http.StatusTooManyRequests
	helper.startBody = `{"success":false,"message":"rate limited"}`
	svc, _ := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	_, err := svc.StartInteractiveLogin(context.Background(), 9)
	require.ErrorIs(t, err, errWebConsoleAuthHelperBusy)
}

func TestWebConsoleAuthService_Start_PropagatesLoadError(t *testing.T) {
	repo := &wcqRepoStub{getByIDErr: ErrAccountNotFound}
	helper := wcAuthNewFakeHelper(t)
	svc, _ := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	_, err := svc.StartInteractiveLogin(context.Background(), 404)
	require.ErrorIs(t, err, ErrAccountNotFound)
}

func TestWebConsoleAuthService_Start_PrunesExpiredSessions(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(61)}
	helper := wcAuthNewFakeHelper(t)
	svc, _ := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }
	first, err := svc.StartInteractiveLogin(context.Background(), 61)
	require.NoError(t, err)

	// 13 分钟后再发一次：旧会话必须被清理。
	svc.now = func() time.Time { return base.Add(13 * time.Minute) }
	helper.startBody = `{"success":true,"session_id":"sess-new"}`
	second, err := svc.StartInteractiveLogin(context.Background(), 61)
	require.NoError(t, err)
	require.Equal(t, "sess-new", second.ID)

	svc.mu.Lock()
	_, oldPresent := svc.sessions[first.ID]
	total := len(svc.sessions)
	svc.mu.Unlock()
	require.False(t, oldPresent, "过期会话必须在 Start 时被清理")
	require.Equal(t, 1, total)
}

// ---------------------------------------------------------------------------
// 用例：GetInteractiveLoginStatus 三态
// ---------------------------------------------------------------------------

func TestWebConsoleAuthService_Status_Pending(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(10)}
	helper := wcAuthNewFakeHelper(t)
	svc, worker := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	session, err := svc.StartInteractiveLogin(context.Background(), 10)
	require.NoError(t, err)

	status, err := svc.GetInteractiveLoginStatus(context.Background(), 10, session.ID)
	require.NoError(t, err)
	require.NotNil(t, status)
	require.Equal(t, webConsoleAuthStatusPending, status.Status)
	require.Empty(t, status.Message)
	require.Nil(t, status.Probe)
	require.Equal(t, 0, worker.callCount(), "pending 时不得探测")
	require.Equal(t, 0, repo.patchCount())

	// helper 的 status 请求必须回带 provider 与 session_binding_id。
	target := helper.lastStatusTarget()
	require.Contains(t, target, "/web-auth/sess-abc/status")
	require.Contains(t, target, "provider=compshare")
	require.Contains(t, target, "session_binding_id="+session.Binding)
}

func TestWebConsoleAuthService_Status_FailedContextGap(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(11)}
	helper := wcAuthNewFakeHelper(t)
	helper.statusBody = `{"success":true,"status":"context_gap","message":"未在限定时间内完成登录"}`
	svc, worker := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	session, err := svc.StartInteractiveLogin(context.Background(), 11)
	require.NoError(t, err)

	status, err := svc.GetInteractiveLoginStatus(context.Background(), 11, session.ID)
	require.NoError(t, err)
	require.Equal(t, webConsoleAuthStatusFailed, status.Status)
	require.Equal(t, "未在限定时间内完成登录", status.Message)
	require.Nil(t, status.Probe)
	require.Equal(t, 0, worker.callCount())
	require.Equal(t, 0, repo.patchCount())
}

func TestWebConsoleAuthService_Status_FailedUnknownHelperStatus(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(12)}
	helper := wcAuthNewFakeHelper(t)
	helper.statusBody = `{"success":true,"status":"weird"}`
	svc, _ := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	session, err := svc.StartInteractiveLogin(context.Background(), 12)
	require.NoError(t, err)

	status, err := svc.GetInteractiveLoginStatus(context.Background(), 12, session.ID)
	require.NoError(t, err)
	require.Equal(t, webConsoleAuthStatusFailed, status.Status)
	require.Contains(t, status.Message, "weird")
}

func TestWebConsoleAuthService_Status_SucceededPersistsThenProbes(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(13)}
	helper := wcAuthNewFakeHelper(t)
	helper.statusBody = `{"success":true,"status":"ok","message":"登录成功，已捕获 storage_state"}`
	helper.resultBody = `{"success":true,"data":{"storage_state":"{\"cookies\":[{\"name\":\"U_JWT_TOKEN\"}]}","account_label":"alice@example.com"}}`

	// 自定义 worker：记录探测发起瞬间的凭据落库次数，用于断言「先落库后探测」。
	var patchesAtProbe int
	var workerCalls int
	probeServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		repo.mu.Lock()
		patchesAtProbe = len(repo.patchCalls)
		repo.mu.Unlock()
		workerCalls++
		data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(wcqProbeBody(data)))
	}))
	t.Cleanup(probeServer.Close)

	quota := NewWebConsoleQuotaService(repo, wcqConfig(probeServer.URL))
	svc := NewWebConsoleAuthService(quota, wcAuthClientFor(t, helper), repo)

	session, err := svc.StartInteractiveLogin(context.Background(), 13)
	require.NoError(t, err)

	status, err := svc.GetInteractiveLoginStatus(context.Background(), 13, session.ID)
	require.NoError(t, err)
	require.Equal(t, webConsoleAuthStatusSucceeded, status.Status)
	require.NotNil(t, status.Probe)
	require.True(t, status.Probe.Success)
	require.Equal(t, 1, workerCalls, "成功后必须立即触发一次探测")

	require.Equal(t, 1, repo.patchCount())
	call := repo.patchCalls[0]
	require.Equal(t, int64(13), call.accountID)
	state, ok := call.patch[webConsoleQuotaCredentialKeyStorageState].(string)
	require.True(t, ok)
	require.Contains(t, state, "U_JWT_TOKEN")
	// 本地已有非空 console_username：不得覆盖。
	require.NotContains(t, call.patch, webConsoleQuotaCredentialKeyUsername)
	require.Equal(t, "Alice@Example.com", call.expected)

	require.Equal(t, 1, patchesAtProbe, "探测必须在 storageState 落库之后发起")

	// helper result 请求体带 provider + session_binding_id。
	require.Len(t, helper.resultReqs, 1)
	require.Equal(t, providerCompshare, helper.resultReqs[0]["provider"])
	require.Equal(t, session.Binding, helper.resultReqs[0]["session_binding_id"])
	require.Equal(t, "helper-key", helper.resultHeaders[0].Get("X-API-Key"))

	// 结果一次性消费：会话绑定已移除，二次轮询返回 failed。
	second, err := svc.GetInteractiveLoginStatus(context.Background(), 13, session.ID)
	require.NoError(t, err)
	require.Equal(t, webConsoleAuthStatusFailed, second.Status)
	require.Equal(t, 1, repo.patchCount(), "不可重复落库")
}

func TestWebConsoleAuthService_Status_SucceededFillsEmptyUsername(t *testing.T) {
	account := wcqCompshareAccount(14)
	delete(account.Credentials, "console_username")
	repo := &wcqRepoStub{account: account}
	helper := wcAuthNewFakeHelper(t)
	helper.statusBody = `{"success":true,"status":"ok"}`
	helper.resultBody = `{"success":true,"data":{"storage_state":"{\"cookies\":[]}","account_label":"bob@example.com"}}`
	svc, _ := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	session, err := svc.StartInteractiveLogin(context.Background(), 14)
	require.NoError(t, err)
	_, err = svc.GetInteractiveLoginStatus(context.Background(), 14, session.ID)
	require.NoError(t, err)

	require.Equal(t, 1, repo.patchCount())
	require.Equal(t, "bob@example.com", repo.patchCalls[0].patch[webConsoleQuotaCredentialKeyUsername])
	require.Equal(t, "", repo.patchCalls[0].expected)
}

func TestWebConsoleAuthService_Status_SucceededResultContextGap(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(15)}
	helper := wcAuthNewFakeHelper(t)
	helper.statusBody = `{"success":true,"status":"ok"}`
	helper.resultCode = http.StatusUnprocessableEntity
	helper.resultBody = `{"success":false,"status":"context_gap","message":"认证会话已过期"}`
	svc, worker := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	session, err := svc.StartInteractiveLogin(context.Background(), 15)
	require.NoError(t, err)
	status, err := svc.GetInteractiveLoginStatus(context.Background(), 15, session.ID)
	require.NoError(t, err)
	require.Equal(t, webConsoleAuthStatusFailed, status.Status)
	require.Contains(t, status.Message, "认证会话已过期")
	require.Equal(t, 0, repo.patchCount())
	require.Equal(t, 0, worker.callCount())
}

func TestWebConsoleAuthService_Status_SucceededMissingStorageState(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(16)}
	helper := wcAuthNewFakeHelper(t)
	helper.statusBody = `{"success":true,"status":"ok"}`
	helper.resultBody = `{"success":true,"data":{}}`
	svc, worker := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	session, err := svc.StartInteractiveLogin(context.Background(), 16)
	require.NoError(t, err)
	status, err := svc.GetInteractiveLoginStatus(context.Background(), 16, session.ID)
	require.NoError(t, err)
	require.Equal(t, webConsoleAuthStatusFailed, status.Status)
	require.Equal(t, 0, repo.patchCount())
	require.Equal(t, 0, worker.callCount())
}

// ---------------------------------------------------------------------------
// 用例：会话校验
// ---------------------------------------------------------------------------

func TestWebConsoleAuthService_Status_SessionAccountMismatch(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(17)}
	helper := wcAuthNewFakeHelper(t)
	svc, worker := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	session, err := svc.StartInteractiveLogin(context.Background(), 17)
	require.NoError(t, err)

	status, err := svc.GetInteractiveLoginStatus(context.Background(), 18, session.ID)
	require.NoError(t, err)
	require.Equal(t, webConsoleAuthStatusFailed, status.Status)
	require.Contains(t, status.Message, "does not belong")
	require.Empty(t, helper.statusTargets, "归属不匹配时不得调用 helper")
	require.Equal(t, 0, worker.callCount())
}

func TestWebConsoleAuthService_Status_UnknownSession(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(19)}
	helper := wcAuthNewFakeHelper(t)
	svc, _ := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	status, err := svc.GetInteractiveLoginStatus(context.Background(), 19, "nope")
	require.NoError(t, err)
	require.Equal(t, webConsoleAuthStatusFailed, status.Status)
	require.Contains(t, status.Message, "not found")
}

func TestWebConsoleAuthService_Status_ExpiredSession(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(20)}
	helper := wcAuthNewFakeHelper(t)
	svc, worker := wcAuthService(t, repo, wcAuthClientFor(t, helper))

	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }
	session, err := svc.StartInteractiveLogin(context.Background(), 20)
	require.NoError(t, err)

	// 恰好在 12 分钟边界：必须判过期（now 不早于 ExpiresAt）。
	svc.now = func() time.Time { return base.Add(webConsoleAuthSessionTTL) }
	status, err := svc.GetInteractiveLoginStatus(context.Background(), 20, session.ID)
	require.NoError(t, err)
	require.Equal(t, webConsoleAuthStatusFailed, status.Status)
	require.Contains(t, status.Message, "expired")
	require.Equal(t, 0, worker.callCount())

	svc.mu.Lock()
	_, present := svc.sessions[session.ID]
	svc.mu.Unlock()
	require.False(t, present, "过期会话必须自动清理")
}

// ---------------------------------------------------------------------------
// 用例：HTTP 客户端契约
// ---------------------------------------------------------------------------

func TestWebConsoleAuthService_Client_StatusUnknownBinding(t *testing.T) {
	helper := wcAuthNewFakeHelper(t)
	client := wcAuthClientFor(t, helper)

	_, _, err := client.WebAuthStatus(context.Background(), "unbound")
	require.ErrorIs(t, err, errWebConsoleAuthSessionBindingUnknown)

	_, _, err = client.WebAuthResult(context.Background(), "unbound", providerCompshare, "b")
	require.NoError(t, err, "result 由调用方显式传 provider/binding，无需本地绑定")
	_ = helper
}

func TestWebConsoleAuthService_Client_StartRejectsEmptySessionID(t *testing.T) {
	helper := wcAuthNewFakeHelper(t)
	helper.startBody = `{"success":true,"session_id":""}`
	client := wcAuthClientFor(t, helper)

	_, err := client.WebAuthStart(context.Background(), WebAuthStartRequest{
		Provider: providerCompshare, SessionBindingID: "acct-1-2",
		LoginURL: "https://passport.compshare.cn/login", SuccessURLPrefix: "https://console.compshare.cn/",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "session_id")
}

func TestWebConsoleAuthService_Client_Result404ForgetsBinding(t *testing.T) {
	helper := wcAuthNewFakeHelper(t)
	helper.statusBody = `{"success":true,"status":"ok"}`
	helper.resultCode = http.StatusNotFound
	helper.resultBody = `{"success":false,"message":"认证会话不存在或已消费"}`
	client := wcAuthClientFor(t, helper)

	sessionID, err := client.WebAuthStart(context.Background(), WebAuthStartRequest{
		Provider: providerCompshare, SessionBindingID: "acct-1-2",
		LoginURL: "https://passport.compshare.cn/login", SuccessURLPrefix: "https://console.compshare.cn/",
	})
	require.NoError(t, err)

	_, _, err = client.WebAuthResult(context.Background(), sessionID, providerCompshare, "acct-1-2")
	require.Error(t, err)

	// 绑定已消费：后续 status 直接拒绝，不再打 helper。
	_, _, err = client.WebAuthStatus(context.Background(), sessionID)
	require.ErrorIs(t, err, errWebConsoleAuthSessionBindingUnknown)
}

func TestWebConsoleAuthService_NilHelper(t *testing.T) {
	repo := &wcqRepoStub{account: wcqCompshareAccount(21)}
	svc := NewWebConsoleAuthService(NewWebConsoleQuotaService(repo, &configStub), nil, repo)

	_, err := svc.StartInteractiveLogin(context.Background(), 21)
	require.ErrorIs(t, err, errWebConsoleAuthHelperNotConfigured)
}

// configStub 是空配置，仅用于构造不接 worker 的服务。
var configStub = *wcqConfig("http://unused.invalid")
