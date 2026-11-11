//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// wcqPatchCall 记录一次 PatchAccountCredentials 调用。
type wcqPatchCall struct {
	accountID int64
	patch     map[string]any
	expected  string
}

// wcqSetCall 记录一次 SetTempUnschedulableIfOwnedOrUnpaused 调用。
type wcqSetCall struct {
	accountID int64
	until     time.Time
	reason    string
	source    string
}

// wcqClearCall 记录一次 ClearTempUnschedulableIfOwned 调用。
type wcqClearCall struct {
	accountID int64
	until     time.Time
	source    string
}

// wcqRepoStub 只实现本服务消费的仓储方法，其余方法由内嵌接口兜底（调用即 panic）。
type wcqRepoStub struct {
	AccountRepository

	mu sync.Mutex

	account    *Account
	getByIDErr error

	patchCalls    []wcqPatchCall
	patchErr      error
	updateExtras  []map[string]any
	updateExtraID []int64
	updateErr     error

	setCalls   []wcqSetCall
	setApplied bool
	setErr     error

	clearCalls   []wcqClearCall
	clearCleared bool
	clearErr     error
}

func (r *wcqRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
	if r.getByIDErr != nil {
		return nil, r.getByIDErr
	}
	return r.account, nil
}

func (r *wcqRepoStub) PatchAccountCredentials(_ context.Context, id int64, patch map[string]any, expectedUsername string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.patchCalls = append(r.patchCalls, wcqPatchCall{accountID: id, patch: patch, expected: expectedUsername})
	return r.patchErr
}

func (r *wcqRepoStub) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateExtras = append(r.updateExtras, updates)
	r.updateExtraID = append(r.updateExtraID, id)
	return r.updateErr
}

func (r *wcqRepoStub) SetTempUnschedulableIfOwnedOrUnpaused(_ context.Context, id int64, until time.Time, reason, ownedSource string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setCalls = append(r.setCalls, wcqSetCall{accountID: id, until: until, reason: reason, source: ownedSource})
	return r.setApplied, r.setErr
}

func (r *wcqRepoStub) ClearTempUnschedulableIfOwned(_ context.Context, id int64, expectedUntil time.Time, ownedSource string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearCalls = append(r.clearCalls, wcqClearCall{accountID: id, until: expectedUntil, source: ownedSource})
	return r.clearCleared, r.clearErr
}

func (r *wcqRepoStub) patchCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.patchCalls)
}

func (r *wcqRepoStub) setCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.setCalls)
}

func (r *wcqRepoStub) clearCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.clearCalls)
}

func (r *wcqRepoStub) lastUpdateExtra() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.updateExtras) == 0 {
		return nil
	}
	return r.updateExtras[len(r.updateExtras)-1]
}

func (r *wcqRepoStub) lastSetCall() (wcqSetCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.setCalls) == 0 {
		return wcqSetCall{}, false
	}
	return r.setCalls[len(r.setCalls)-1], true
}

// wcqCacheStub 记录缓存失效调用；本链只允许 Delete。
type wcqCacheStub struct {
	TempUnschedCache

	mu      sync.Mutex
	deletes []int64
	sets    []int64
}

func (c *wcqCacheStub) DeleteTempUnsched(_ context.Context, accountID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes = append(c.deletes, accountID)
	return nil
}

func (c *wcqCacheStub) SetTempUnsched(_ context.Context, accountID int64, _ *TempUnschedState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets = append(c.sets, accountID)
	return nil
}

func (c *wcqCacheStub) deleteCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.deletes)
}

func (c *wcqCacheStub) setCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sets)
}

// ---------------------------------------------------------------------------
// 假 worker
// ---------------------------------------------------------------------------

// wcqFakeWorker 是 worker 的 httptest 替身：记录收到的请求并回固定响应。
type wcqFakeWorker struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
	calls    int
}

func wcqNewFakeWorker(t *testing.T, statusCode int, body string) *wcqFakeWorker {
	t.Helper()
	w := &wcqFakeWorker{}
	w.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)

		w.mu.Lock()
		w.calls++
		w.requests = append(w.requests, decoded)
		w.headers = append(w.headers, r.Header.Clone())
		w.mu.Unlock()

		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(statusCode)
		_, _ = rw.Write([]byte(body))
	}))
	t.Cleanup(w.server.Close)
	return w
}

func (w *wcqFakeWorker) lastRequest() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.requests) == 0 {
		return nil
	}
	return w.requests[len(w.requests)-1]
}

func (w *wcqFakeWorker) lastHeader() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.headers) == 0 {
		return nil
	}
	return w.headers[len(w.headers)-1]
}

func (w *wcqFakeWorker) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// ---------------------------------------------------------------------------
// 构造辅助
// ---------------------------------------------------------------------------

const wcqTestAPIKey = "sk-console-secret-value"

// wcqCompshareAccount 构造一个命中 compshare 上游的 API Key 账号。
func wcqCompshareAccount(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url":              "https://cp.compshare.cn/api/v1",
			"api_key":               wcqTestAPIKey,
			"console_username":      "Alice@Example.com",
			"console_password":      "pw-should-never-leak",
			"console_storage_state": `{"cookies":[]}`,
			"model_mapping":         map[string]any{"claude-3-5-sonnet": "deepseek-v3"},
		},
	}
}

// wcqConfig 构造指向假 worker 的配置。
func wcqConfig(baseURL string) *config.Config {
	return &config.Config{
		WebQuotaWorker: config.WebQuotaWorkerConfig{
			BaseURL:        baseURL,
			APIKey:         "worker-token",
			TimeoutSeconds: 5,
		},
	}
}

// wcqCompsharePlanData 构造 compshare 单套餐 data。
// used5h/limit5h 用于控制耗尽判定；reset5h 为 5h 窗口重置时间。
func wcqCompsharePlanData(limit5h, used5h float64, reset5h string) string {
	expire := time.Now().Add(30 * 24 * time.Hour).Unix()
	data := map[string]any{
		"auth_state":    "ok",
		"account_label": "alice",
		"plans": []map[string]any{{
			"plan_name":   "Basic",
			"expire_at":   expire,
			"plan_status": 1,
			"limit_5h":    limit5h,
			"used_5h":     used5h,
			"reset_5h":    reset5h,
			"limit_week":  0,
			"used_week":   0,
			"limit_month": 0,
			"used_month":  0,
		}},
	}
	raw, _ := json.Marshal(data)
	return string(raw)
}

// wcqProbeBody 构造 worker 正常成功响应。
func wcqProbeBody(data string) string {
	raw, _ := json.Marshal(map[string]any{
		"status":   "ok",
		"provider": providerCompshare,
		"data":     json.RawMessage(data),
	})
	return string(raw)
}

// ---------------------------------------------------------------------------
// 用例：不适用 / 未配置
// ---------------------------------------------------------------------------

func TestWebConsoleQuotaService_ProbeLoadedAccount_NotApplicable(t *testing.T) {
	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig("http://unused.invalid"))

	account := &Account{
		ID:          1,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://api.deepseek.com/v1"},
	}

	res, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)
	require.Nil(t, res, "provider 不适用时必须返回 (nil, nil)，不是错误")
	require.Equal(t, 0, repo.patchCount())
	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
}

func TestWebConsoleQuotaService_ProbeLoadedAccount_NilAccount(t *testing.T) {
	svc := NewWebConsoleQuotaService(&wcqRepoStub{}, wcqConfig("http://unused.invalid"))

	res, err := svc.ProbeLoadedAccount(context.Background(), nil)
	require.NoError(t, err)
	require.Nil(t, res)
}

func TestWebConsoleQuotaService_ProbeLoadedAccount_WorkerNotConfigured(t *testing.T) {
	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, &config.Config{})

	account := wcqCompshareAccount(10)
	res, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.Success)
	require.Equal(t, webConsoleQuotaAuthStateError, res.AuthState)
	require.Equal(t, webConsoleQuotaNotConfiguredError, res.Error)
	require.False(t, res.Persisted)

	// 未配置：不写 extra、不动停调、不回写凭据。
	require.Empty(t, repo.updateExtras)
	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
	require.Equal(t, 0, repo.patchCount())
}

// ---------------------------------------------------------------------------
// 用例：ok 正常链路
// ---------------------------------------------------------------------------

func TestWebConsoleQuotaService_ProbeOK_PersistsSnapshotAndKeepsScheduling(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(data))
	repo := &wcqRepoStub{}
	cache := &wcqCacheStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))
	svc.SetTempUnschedCache(cache)

	account := wcqCompshareAccount(11)
	res, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.Success)
	require.Equal(t, webConsoleQuotaAuthStateOK, res.AuthState)
	require.False(t, res.Exhausted)
	require.True(t, res.Persisted)
	require.NotNil(t, res.SkillSnapshot)
	require.Equal(t, providerCompshare, res.SkillSnapshot.Provider)
	require.Equal(t, webConsoleQuotaAuthStateOK, res.SkillSnapshot.AuthState)
	require.Len(t, res.SkillSnapshot.Tiers, 3)

	// 快照写进 extra 的固定键。
	update := repo.lastUpdateExtra()
	require.NotNil(t, update)
	require.Contains(t, update, webConsoleQuotaSnapshotExtraKey)

	// 未耗尽且无停调：既不写停调也不清除。
	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
	require.Equal(t, 0, cache.deleteCount())
	require.Equal(t, 0, cache.setCount())
}

func TestWebConsoleQuotaService_ProbeOK_RequestShapeNeverLeaksAPIKey(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(data))
	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	account := wcqCompshareAccount(12)
	_, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)

	req := worker.lastRequest()
	require.NotNil(t, req)
	require.Equal(t, providerCompshare, req["provider"])
	require.Equal(t, "https://passport.compshare.cn/login", req["console_login_url"])
	require.Equal(t, "https://console.compshare.cn/", req["success_url_prefix"])
	require.EqualValues(t, 5, req["timeout_seconds"])
	require.Equal(t, sha256Hex([]byte(wcqTestAPIKey)), req["api_key_sha256"])

	// 请求体绝不携带明文 api_key / 密码明文以外的推理密钥。
	encoded, err := json.Marshal(req)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), wcqTestAPIKey)
	require.NotContains(t, string(encoded), `"api_key"`)

	// worker token 走头部。
	require.Equal(t, "worker-token", worker.lastHeader().Get("X-Worker-Token"))
}

func TestWebConsoleQuotaService_ProbeOK_StorageStateWriteBack(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	body := `{"status":"ok","provider":"compshare","account_label":"alice",` +
		`"updated_storage_state":{"cookies":[{"name":"U_JWT_TOKEN","value":"new"}],"origins":[]},` +
		`"data":` + data + `}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)
	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	account := wcqCompshareAccount(13)
	_, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)

	require.Equal(t, 1, repo.patchCount())
	call := repo.patchCalls[0]
	require.Equal(t, int64(13), call.accountID)
	state, ok := call.patch[webConsoleQuotaCredentialKeyStorageState].(string)
	require.True(t, ok, "storage_state 必须以字符串落库")
	require.Contains(t, state, "U_JWT_TOKEN")

	// 本地已有非空 console_username：不得补写、更不得覆盖。
	require.NotContains(t, call.patch, webConsoleQuotaCredentialKeyUsername)
	// expectedUsername 传本地当前值，供仓储做并发改写守卫。
	require.Equal(t, "Alice@Example.com", call.expected)
}

func TestWebConsoleQuotaService_ProbeOK_StorageStateFillsEmptyUsername(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	body := `{"status":"ok","provider":"compshare","account_label":"bob@example.com",` +
		`"updated_storage_state":{"cookies":[]},"data":` + data + `}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)
	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	account := wcqCompshareAccount(14)
	delete(account.Credentials, "console_username")
	_, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)

	require.Equal(t, 1, repo.patchCount())
	call := repo.patchCalls[0]
	require.Equal(t, "bob@example.com", call.patch[webConsoleQuotaCredentialKeyUsername])
	require.Equal(t, "", call.expected)
}

func TestWebConsoleQuotaService_ProbeOK_StorageStateStringForm(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	body := `{"status":"ok","provider":"compshare","updated_storage_state":"{\"cookies\":[]}","data":` + data + `}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)
	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	_, err := svc.ProbeLoadedAccount(context.Background(), wcqCompshareAccount(15))
	require.NoError(t, err)

	require.Equal(t, 1, repo.patchCount())
	require.Equal(t, `{"cookies":[]}`, repo.patchCalls[0].patch[webConsoleQuotaCredentialKeyStorageState])
}

func TestWebConsoleQuotaService_ProbeOK_StorageStatePatchFailureDoesNotAbortProbe(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	body := `{"status":"ok","provider":"compshare","updated_storage_state":{"cookies":[]},"data":` + data + `}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)
	repo := &wcqRepoStub{patchErr: errors.New("boom")}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	res, err := svc.ProbeLoadedAccount(context.Background(), wcqCompshareAccount(16))
	require.NoError(t, err)
	require.True(t, res.Success, "凭据回写失败只记日志，不得中断探测")
	require.True(t, res.Persisted)
}

func TestWebConsoleQuotaService_ProbeOK_SnapshotBuildFailureIsFailClosed(t *testing.T) {
	// data 不是合法 JSON → BuildWebConsoleQuotaSnapshot 报错。
	body := `{"status":"ok","provider":"compshare","data":"not-an-object"}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)
	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	res, err := svc.ProbeLoadedAccount(context.Background(), wcqCompshareAccount(17))
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Equal(t, webConsoleQuotaAuthStateError, res.AuthState)
	require.NotEmpty(t, res.Error)

	// 归一失败：不写 extra、不动停调。
	require.Empty(t, repo.updateExtras)
	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
}

func TestWebConsoleQuotaService_ProbeOK_UpdateExtraErrorSkipsScheduling(t *testing.T) {
	data := wcqCompsharePlanData(100, 100, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(data))
	repo := &wcqRepoStub{updateErr: errors.New("db down"), setApplied: true}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	res, err := svc.ProbeLoadedAccount(context.Background(), wcqCompshareAccount(18))
	require.NoError(t, err)
	require.False(t, res.Persisted)
	// 方案 §3.2-C-3 + 外审 must_fix：本次判定无法留痕时不得应用调度结果（失败关闭），
	// 避免「有停调、无快照」的不可观测状态。上一轮语义（UpdateExtra 失败仍调度）已被取代。
	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
}

// ---------------------------------------------------------------------------
// 用例：耗尽写入 / 恢复清除 / 他因不动
// ---------------------------------------------------------------------------

func TestWebConsoleQuotaService_ExhaustedWritesTempUnsched(t *testing.T) {
	resetAt := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	data := wcqCompsharePlanData(100, 100, resetAt.Format(time.RFC3339))
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(data))
	repo := &wcqRepoStub{setApplied: true}
	cache := &wcqCacheStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))
	svc.SetTempUnschedCache(cache)

	res, err := svc.ProbeLoadedAccount(context.Background(), wcqCompshareAccount(20))
	require.NoError(t, err)
	require.True(t, res.Exhausted)
	require.NotEmpty(t, res.Until)

	require.Equal(t, 1, repo.setCount())
	call, ok := repo.lastSetCall()
	require.True(t, ok)
	require.Equal(t, int64(20), call.accountID)
	require.Equal(t, webConsoleQuotaOwnedSource, call.source)
	require.True(t, call.until.Equal(resetAt), "停调截止时间必须取官方 reset_at")
	require.True(t, IsWebConsoleQuotaTempUnschedReason(call.reason))

	// 写入生效后只允许 Delete，绝不 Set。
	require.Equal(t, 1, cache.deleteCount())
	require.Equal(t, int64(20), cache.deletes[0])
	require.Equal(t, 0, cache.setCount())
}

func TestWebConsoleQuotaService_ExhaustedIndefiniteUsesSentinel(t *testing.T) {
	// longcat：无周期重置，耗尽即无限期哨兵。
	body := `{"status":"ok","provider":"longcat","data":{"auth_state":"ok","lots":[],` +
		`"paygo_balance_cent":0,"paygo_status":"inactive"}}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)
	repo := &wcqRepoStub{setApplied: true}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	account := &Account{
		ID:          21,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://api.longcat.chat/v1", "api_key": "sk-lc"},
	}
	res, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, providerLongcat, res.Provider)
	require.True(t, res.Exhausted)

	call, ok := repo.lastSetCall()
	require.True(t, ok)
	require.True(t, call.until.Equal(tempUnschedIndefiniteUntil), "无限期必须复用 tempUnschedIndefiniteUntil")
}

func TestWebConsoleQuotaService_ExhaustedNotAppliedLeavesCacheAlone(t *testing.T) {
	data := wcqCompsharePlanData(100, 100, time.Now().Add(time.Hour).Format(time.RFC3339))
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(data))
	repo := &wcqRepoStub{setApplied: false} // 他因活跃停调，仓储零写入
	cache := &wcqCacheStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))
	svc.SetTempUnschedCache(cache)

	_, err := svc.ProbeLoadedAccount(context.Background(), wcqCompshareAccount(22))
	require.NoError(t, err)

	require.Equal(t, 1, repo.setCount())
	require.Equal(t, 0, cache.deleteCount(), "applied=false 时不得动缓存")
}

func TestWebConsoleQuotaService_RecoveredClearsOwnSource(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(data))
	repo := &wcqRepoStub{clearCleared: true}
	cache := &wcqCacheStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))
	svc.SetTempUnschedCache(cache)

	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	account := wcqCompshareAccount(30)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = BuildTempUnschedReasonPayload(webConsoleQuotaOwnedSource, "compshare 5h 窗口已用满")

	res, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)
	require.False(t, res.Exhausted)

	require.Equal(t, 1, repo.clearCount())
	call := repo.clearCalls[0]
	require.Equal(t, int64(30), call.accountID)
	require.Equal(t, webConsoleQuotaOwnedSource, call.source)
	require.True(t, call.until.Equal(until))

	require.Equal(t, 1, cache.deleteCount())
	require.Equal(t, 0, cache.setCount())
}

func TestWebConsoleQuotaService_RecoveredDoesNotClearForeignReason(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(data))
	repo := &wcqRepoStub{clearCleared: true}
	cache := &wcqCacheStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))
	svc.SetTempUnschedCache(cache)

	until := time.Now().Add(time.Hour)
	account := wcqCompshareAccount(31)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = BuildTempUnschedReasonPayload("openai_apikey_health_breaker", "trip")

	_, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)

	require.Equal(t, 0, repo.clearCount(), "他因停调绝不清除")
	require.Equal(t, 0, cache.deleteCount())
}

func TestWebConsoleQuotaService_RecoveredClearedFalseLeavesCacheAlone(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(data))
	repo := &wcqRepoStub{clearCleared: false} // 代际已变，仓储零写入
	cache := &wcqCacheStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))
	svc.SetTempUnschedCache(cache)

	until := time.Now().Add(time.Hour)
	account := wcqCompshareAccount(32)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = BuildTempUnschedReasonPayload(webConsoleQuotaOwnedSource, "x")

	_, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)

	require.Equal(t, 1, repo.clearCount())
	require.Equal(t, 0, cache.deleteCount())
}

// ---------------------------------------------------------------------------
// 用例：unmatched 失败关闭
// ---------------------------------------------------------------------------

func TestWebConsoleQuotaService_UnmatchedDoesNothing(t *testing.T) {
	expire := time.Now().Add(30 * 24 * time.Hour).Unix()
	data := map[string]any{
		"auth_state": "ok",
		"plans": []map[string]any{
			{"plan_name": "A", "expire_at": expire, "plan_status": 1,
				"limit_5h": 100, "used_5h": 100, "key_hashes": []string{"deadbeef"}},
			{"plan_name": "B", "expire_at": expire, "plan_status": 1,
				"limit_5h": 100, "used_5h": 100, "key_hashes": []string{"cafebabe"}},
		},
	}
	raw, _ := json.Marshal(data)
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(string(raw)))
	repo := &wcqRepoStub{setApplied: true, clearCleared: true}
	cache := &wcqCacheStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))
	svc.SetTempUnschedCache(cache)

	until := time.Now().Add(time.Hour)
	account := wcqCompshareAccount(40)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = BuildTempUnschedReasonPayload(webConsoleQuotaOwnedSource, "old")

	res, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, webConsoleQuotaPlanMatchStatusUnmatched, res.SkillSnapshot.PlanMatchStatus)

	// unmatched：不写停调、不清停调、不动缓存。
	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
	require.Equal(t, 0, cache.deleteCount())
	require.Equal(t, 0, cache.setCount())
}

// ---------------------------------------------------------------------------
// 用例：非 ok 状态（失败关闭 + 保留历史数值）
// ---------------------------------------------------------------------------

// wcqPriorSnapshot 是历史有效快照（含数值字段）。
func wcqPriorSnapshot() *WebConsoleQuotaSnapshot {
	return &WebConsoleQuotaSnapshot{
		Provider:        providerCompshare,
		FetchedAt:       time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		AuthState:       webConsoleQuotaAuthStateOK,
		PlanName:        "Basic",
		PlanMatchStatus: "matched",
		Tiers: []WebConsoleQuotaTier{
			{Window: "5h", Limit: 1200, Used: 300, ResetAt: time.Now().Add(time.Hour).Format(time.RFC3339)},
		},
		Exhausted: false,
	}
}

func TestWebConsoleQuotaService_NeedsInteractiveAuthKeepsHistoricalValues(t *testing.T) {
	body := `{"status":"needs_interactive_auth","provider":"compshare",` +
		`"auth_reason":"captcha_required","error":"geetest challenge hit"}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)
	repo := &wcqRepoStub{setApplied: true, clearCleared: true}
	cache := &wcqCacheStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))
	svc.SetTempUnschedCache(cache)

	until := time.Now().Add(time.Hour)
	account := wcqCompshareAccount(50)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = BuildTempUnschedReasonPayload(webConsoleQuotaOwnedSource, "old")
	account.Extra = map[string]any{webConsoleQuotaSnapshotExtraKey: wcqPriorSnapshot()}

	res, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Equal(t, webConsoleQuotaAuthStateNeedsInteractive, res.AuthState)
	require.Equal(t, "captcha_required", res.AuthReason)

	// 历史数值字段必须保留。
	require.NotNil(t, res.SkillSnapshot)
	require.Equal(t, "Basic", res.SkillSnapshot.PlanName)
	require.Len(t, res.SkillSnapshot.Tiers, 1)
	require.EqualValues(t, 1200, res.SkillSnapshot.Tiers[0].Limit)
	require.EqualValues(t, 300, res.SkillSnapshot.Tiers[0].Used)
	require.Equal(t, webConsoleQuotaAuthStateNeedsInteractive, res.SkillSnapshot.AuthState)
	require.Equal(t, "geetest challenge hit", res.SkillSnapshot.AuthError)

	// 失败关闭：不写停调也不清除。
	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
	require.Equal(t, 0, cache.deleteCount())
	require.Equal(t, 0, cache.setCount())

	// 快照仍然落库（供前端提示需要人工登录）。
	require.True(t, res.Persisted)
	require.Contains(t, repo.lastUpdateExtra(), webConsoleQuotaSnapshotExtraKey)
}

func TestWebConsoleQuotaService_AuthFailedKeepsHistoricalValues(t *testing.T) {
	body := `{"status":"auth_failed","provider":"sensenova","error":"bad password"}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)
	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	account := &Account{
		ID:       51,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://token.sensenova.cn/v1",
			"api_key":  "sk-sn",
		},
		Extra: map[string]any{webConsoleQuotaSnapshotExtraKey: &WebConsoleQuotaSnapshot{
			Provider:  providerSensenova,
			AuthState: webConsoleQuotaAuthStateOK,
			Pools: []WebConsoleQuotaPool{{
				ID: "p1", PoolType: "general",
				GrantBalance: fptr(12.5),
			}},
			Tiers: []WebConsoleQuotaTier{{Window: "5h", Limit: 100, Used: 40}},
		}},
	}

	res, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, webConsoleQuotaAuthStateAuthFailed, res.AuthState)
	require.Equal(t, "bad password", res.AuthError)
	require.Len(t, res.SkillSnapshot.Pools, 1)
	require.NotNil(t, res.SkillSnapshot.Pools[0].GrantBalance)
	require.EqualValues(t, 12.5, *res.SkillSnapshot.Pools[0].GrantBalance)
	require.Len(t, res.SkillSnapshot.Tiers, 1)

	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
}

func TestWebConsoleQuotaService_UnknownWorkerStatusBecomesError(t *testing.T) {
	body := `{"status":"weird_status","provider":"compshare"}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)
	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	res, err := svc.ProbeLoadedAccount(context.Background(), wcqCompshareAccount(52))
	require.NoError(t, err)
	require.Equal(t, webConsoleQuotaAuthStateError, res.AuthState)
	require.NotEmpty(t, res.Error)
	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
}

func TestWebConsoleQuotaService_WorkerHTTPErrorIsFailClosed(t *testing.T) {
	worker := wcqNewFakeWorker(t, http.StatusInternalServerError, `{"error":"boom"}`)
	repo := &wcqRepoStub{setApplied: true, clearCleared: true}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	until := time.Now().Add(time.Hour)
	account := wcqCompshareAccount(53)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = BuildTempUnschedReasonPayload(webConsoleQuotaOwnedSource, "old")

	res, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Equal(t, webConsoleQuotaAuthStateError, res.AuthState)

	// 传输失败：不写 extra、不动停调。
	require.Empty(t, repo.updateExtras)
	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
}

func TestWebConsoleQuotaService_WorkerUnreachableIsFailClosed(t *testing.T) {
	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig("http://127.0.0.1:1"))

	res, err := svc.ProbeLoadedAccount(context.Background(), wcqCompshareAccount(54))
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Equal(t, webConsoleQuotaAuthStateError, res.AuthState)
	require.Empty(t, repo.updateExtras)
}

// ---------------------------------------------------------------------------
// 用例：ProbeAccount / 单飞键 / detail
// ---------------------------------------------------------------------------

func TestWebConsoleQuotaService_ProbeAccount_LoadsByID(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	worker := wcqNewFakeWorker(t, http.StatusOK, wcqProbeBody(data))
	repo := &wcqRepoStub{account: wcqCompshareAccount(60)}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	res, err := svc.ProbeAccount(context.Background(), 60)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.Success)
	require.Equal(t, int64(60), repo.updateExtraID[0])
}

func TestWebConsoleQuotaService_ProbeAccount_PropagatesLoadError(t *testing.T) {
	repo := &wcqRepoStub{getByIDErr: ErrAccountNotFound}
	svc := NewWebConsoleQuotaService(repo, wcqConfig("http://unused.invalid"))

	res, err := svc.ProbeAccount(context.Background(), 404)
	require.ErrorIs(t, err, ErrAccountNotFound)
	require.Nil(t, res)
}

func TestWebConsoleQuotaService_SingleflightKey(t *testing.T) {
	svc := NewWebConsoleQuotaService(&wcqRepoStub{}, wcqConfig("http://unused.invalid"))

	withUsername := wcqCompshareAccount(70)
	require.Equal(t, "wcq:compshare:alice@example.com", svc.probeSingleflightKey(providerCompshare, withUsername))

	// 大小写不敏感：同控制台账号共用一个单飞键。
	other := wcqCompshareAccount(71)
	other.Credentials["console_username"] = "ALICE@example.COM"
	require.Equal(t, svc.probeSingleflightKey(providerCompshare, withUsername),
		svc.probeSingleflightKey(providerCompshare, other))

	// username 为空：按账号隔离。
	anon := wcqCompshareAccount(72)
	delete(anon.Credentials, "console_username")
	require.Equal(t, "wcq:acct:72", svc.probeSingleflightKey(providerCompshare, anon))
}

func TestWebConsoleQuotaService_BuildDetail(t *testing.T) {
	snap := &WebConsoleQuotaSnapshot{
		Provider:        providerCompshare,
		PlanMatchStatus: "matched",
		Exhausted:       true,
		Until:           "2026-11-11T18:00:00Z",
		Tiers:           []WebConsoleQuotaTier{{Window: "5h", Limit: 100, Used: 100}},
	}

	detail := webConsoleQuotaBuildDetail(snap)
	require.Contains(t, detail, "compshare")
	require.Contains(t, detail, "5h")
	require.Contains(t, detail, "2026-11-11T18:00:00Z")
	require.LessOrEqual(t, len([]rune(detail)), webConsoleQuotaDetailMaxRunes)

	// 不含凭据。
	require.NotContains(t, detail, wcqTestAPIKey)

	indefinite := &WebConsoleQuotaSnapshot{Provider: providerLongcat, Exhausted: true, UntilIndefinite: true}
	require.Contains(t, webConsoleQuotaBuildDetail(indefinite), "longcat")

	noPlan := &WebConsoleQuotaSnapshot{Provider: providerCompshare, PlanMatchStatus: "no_active_plan", Exhausted: true, UntilIndefinite: true}
	require.Contains(t, webConsoleQuotaBuildDetail(noPlan), "无有效订阅套餐")

	require.Equal(t, "", webConsoleQuotaBuildDetail(nil))
}

func TestWebConsoleQuotaService_BuildDetailTruncates(t *testing.T) {
	snap := &WebConsoleQuotaSnapshot{
		Provider:  strings.Repeat("p", 500),
		Exhausted: true,
		Until:     "2026-11-11T18:00:00Z",
	}
	detail := webConsoleQuotaBuildDetail(snap)
	require.Len(t, []rune(detail), webConsoleQuotaDetailMaxRunes)
}

func TestWebConsoleQuotaService_ResolveUntil(t *testing.T) {
	require.True(t, webConsoleQuotaResolveUntil(&WebConsoleQuotaSnapshot{UntilIndefinite: true}).Equal(tempUnschedIndefiniteUntil))
	// Until 为空/不可解析：同样落到无限期哨兵（官方未给出可用 reset_at）。
	require.True(t, webConsoleQuotaResolveUntil(&WebConsoleQuotaSnapshot{}).Equal(tempUnschedIndefiniteUntil))
	require.True(t, webConsoleQuotaResolveUntil(&WebConsoleQuotaSnapshot{Until: "not-a-time"}).Equal(tempUnschedIndefiniteUntil))

	ts := "2026-11-11T18:00:00Z"
	got := webConsoleQuotaResolveUntil(&WebConsoleQuotaSnapshot{Until: ts})
	want, err := time.Parse(time.RFC3339, ts)
	require.NoError(t, err)
	require.True(t, got.Equal(want))
}
