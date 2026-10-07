package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// 假 TH 站点：按 2026-09/10 实抓形态造页（登录页现抓 action 字段 + JS chunk 里的
// createServerReference signIn id；登录成功 303 + 分片 Set-Cookie；billing 页是
// RSC 转义形态 \"hasPass\":true）。
const (
	testTHActionID   = "abc123def456abc123def456abc123def456abc1"
	testTHNextAction = "0123456789abcdef0123456789abcdef01234567"

	// 与 /tmp/th_pass_scan.log 里真实输出同形的 RSC 转义页面。
	testTHBillingRSC = `1:{\"hasPass\":true,\"passName\":\"Agent Pass\",` +
		`\"renewsAt\":\"2026-11-04T19:24:16.346489+00:00\",` +
		`\"spendAfterAllowance\":true,\"autoReloadEnabled\":false}`
	testTHLoginPage = `<!doctype html><html><body><form>` +
		`<input name="next" value=""/>` +
		`<input name="$ACTION_1:0" value='{"id":"` + testTHActionID + `","bound":"$@1"}'/>` +
		`<input name="$ACTION_KEY" value="action-key-xyz"/>` +
		`<script src="/_next/static/chunks/login-page.js"></script>` +
		`</form></body></html>`
	testTHLoginChunk = `globalThis.createServerReference("` + testTHNextAction +
		`", callServer, findSourceMapURL, "signIn");`
)

type tokenHarborFakeTH struct {
	server *httptest.Server
	mux    *http.ServeMux

	mu                sync.Mutex
	loginPageHits     int
	loginPosts        int
	billingHits       int
	billing401Once    bool
	billingPage       string // 空 = 默认 RSC 页；测试可覆盖为无订阅数据的页面
	loginRejected     bool   // true = POST /login 回 200 + RSC error（模拟风控拒登）
	seenNextAction    string
	lastBillingCookie string
	loginBodies       []string
}

func newTokenHarborFakeTH(t *testing.T, billing401Once bool) *tokenHarborFakeTH {
	fake := &tokenHarborFakeTH{billing401Once: billing401Once}
	mux := http.NewServeMux()
	mux.HandleFunc("/login", fake.handleLogin)
	mux.HandleFunc("/_next/static/chunks/login-page.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(testTHLoginChunk))
	})
	mux.HandleFunc("/dashboard/billing", fake.handleBilling)
	fake.mux = mux
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *tokenHarborFakeTH) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		f.loginPageHits++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(testTHLoginPage))
	case http.MethodPost:
		f.mu.Lock()
		defer f.mu.Unlock()
		f.loginPosts++
		f.seenNextAction = r.Header.Get("Next-Action")
		body, _ := io.ReadAll(r.Body)
		f.loginBodies = append(f.loginBodies, string(body))
		if f.seenNextAction != testTHNextAction {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<html>missing or wrong Next-Action</html>"))
			return
		}
		// 成功判据形态：303 + 分片 Set-Cookie（Supabase sb-auth-auth-token.0/.1）。
		if f.loginRejected {
			// 风控拒登形态：200 + RSC 流 error 行。
			_, _ = w.Write([]byte(`1:{"error":"Invalid login credentials"}`))
			return
		}
		w.Header().Add("Set-Cookie", fmt.Sprintf("sb-auth-auth-token.0=sess-%d; Path=/; HttpOnly; Secure", f.loginPosts))
		w.Header().Add("Set-Cookie", fmt.Sprintf("sb-auth-auth-token.1=part-%d; Path=/; HttpOnly; Secure", f.loginPosts))
		w.Header().Set("Location", "/dashboard")
		w.WriteHeader(http.StatusSeeOther)
	}
}

func (f *tokenHarborFakeTH) handleBilling(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.billingHits++
	f.lastBillingCookie = r.Header.Get("Cookie")
	if f.billing401Once && f.billingHits == 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	page := f.billingPage
	if page == "" {
		page = testTHBillingRSC
	}
	_, _ = w.Write([]byte(page))
}

func (f *tokenHarborFakeTH) stats() (loginPosts, billingHits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginPosts, f.billingHits
}

func (f *tokenHarborFakeTH) setLoginRejected(rejected bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginRejected = rejected
}

// tokenHarborFakeUpstream 测试桩：共享上游语义的替身——记录请求数，重定向一律
// 不跟随（与生产 WithHTTPUpstreamRedirectsDisabled 下的行为一致）。
type tokenHarborFakeUpstream struct {
	server *httptest.Server

	mu  sync.Mutex
	n   int
	err error
}

func (u *tokenHarborFakeUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.n++
	if u.err != nil {
		err := u.err
		u.mu.Unlock()
		return nil, err
	}
	u.mu.Unlock()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client.Do(req)
}

func (u *tokenHarborFakeUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func (u *tokenHarborFakeUpstream) requests() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

type tokenHarborRepoStub struct {
	AccountRepository

	mu sync.Mutex
	// updates 记录每次 UpdateExtra 的完整 updates map（多次调用按序追加）。
	updates     map[int64]map[string]any
	updateCalls []map[string]any
	// sessions 是持久化会话的内存替身，供 Store/Load/Clear 三方法闭环。
	sessions map[int64]tokenHarborSession
	// updateErr 注入落库失败（UpdateExtra/Store/Clear 统一走它），验证 WARN 不阻断。
	updateErr error
	// loadErr 注入读取失败。
	loadErr error
	// clearCalls 记录 ClearTokenHarborSession 调用次数（rejected 双清断言）。
	clearCalls int
}

func (r *tokenHarborRepoStub) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return r.updateErr
	}
	if r.updates == nil {
		r.updates = map[int64]map[string]any{}
	}
	r.updates[id] = updates
	r.updateCalls = append(r.updateCalls, updates)
	return nil
}

func (r *tokenHarborRepoStub) StoreTokenHarborSession(_ context.Context, id int64, cookie string, loginAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return r.updateErr
	}
	if r.sessions == nil {
		r.sessions = map[int64]tokenHarborSession{}
	}
	r.sessions[id] = tokenHarborSession{cookie: cookie, loginAt: loginAt}
	return nil
}

func (r *tokenHarborRepoStub) LoadTokenHarborSession(_ context.Context, id int64) (string, time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loadErr != nil {
		return "", time.Time{}, r.loadErr
	}
	session, ok := r.sessions[id]
	if !ok {
		return "", time.Time{}, nil
	}
	return session.cookie, session.loginAt, nil
}

func (r *tokenHarborRepoStub) ClearTokenHarborSession(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearCalls++
	if r.updateErr != nil {
		return r.updateErr
	}
	delete(r.sessions, id)
	return nil
}

// lastUpdate 返回第 id 账号最近一次 UpdateExtra 的 updates。
func (r *tokenHarborRepoStub) lastUpdate(id int64) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updates[id]
}

// updateCallCount 返回 UpdateExtra 调用次数。
func (r *tokenHarborRepoStub) updateCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.updateCalls)
}

func newTokenHarborTestService(t *testing.T, billing401Once bool) (*TokenHarborPassService, *tokenHarborFakeTH, *tokenHarborFakeUpstream) {
	fakeTH := newTokenHarborFakeTH(t, billing401Once)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	svc := NewTokenHarborPassService(&tokenHarborRepoStub{}, nil, upstream)
	svc.baseURL = fakeTH.server.URL
	return svc, fakeTH, upstream
}

func tokenHarborTestAccount(id int64) *Account {
	proxyID := int64(7)
	return &Account{
		ID:      id,
		ProxyID: &proxyID,
		Proxy:   &Proxy{ID: proxyID, Protocol: "socks5", Host: "127.0.0.1", Port: 7890},
		Credentials: map[string]any{
			"th_email":         "thk-user@example.com",
			"th_password":      "secret-password",
			"th_probe_enabled": true,
		},
	}
}

func TestTokenHarborPassProbeLoginAndParse(t *testing.T) {
	svc, fakeTH, upstream := newTokenHarborTestService(t, false)

	snapshot, err := svc.Probe(context.Background(), tokenHarborTestAccount(1))

	require.NoError(t, err)
	require.Equal(t, TokenHarborPassProviderName, snapshot.Provider)
	require.True(t, snapshot.HasPass)
	require.Equal(t, "Agent Pass", snapshot.PassName)
	require.NotNil(t, snapshot.RenewsAt)
	expected, parseErr := time.Parse(time.RFC3339, "2026-11-04T19:24:16.346489+00:00")
	require.NoError(t, parseErr)
	require.True(t, snapshot.RenewsAt.Equal(expected), "renewsAt=%v", snapshot.RenewsAt)
	require.True(t, snapshot.SpendAfterAllowance)
	require.False(t, snapshot.AutoReloadEnabled)
	require.False(t, snapshot.FetchedAt.IsZero())

	// 登录流程次序：先 GET /login，再带 Next-Action 头 POST。
	require.Equal(t, 1, fakeTH.loginPageHits)
	require.Equal(t, 1, fakeTH.loginPosts)
	require.Equal(t, testTHNextAction, fakeTH.seenNextAction)
	require.Len(t, fakeTH.loginBodies, 1)
	require.Contains(t, fakeTH.loginBodies[0], `name="1_$ACTION_KEY"`)
	require.Contains(t, fakeTH.loginBodies[0], `name="1_email"`)
	require.NotContains(t, fakeTH.loginBodies[0], "secret-password=X")

	// 分片 Set-Cookie 全部拼接进会话 cookie，billing 请求带全。
	require.Contains(t, fakeTH.lastBillingCookie, "sb-auth-auth-token.0=sess-1")
	require.Contains(t, fakeTH.lastBillingCookie, "sb-auth-auth-token.1=part-1")
	require.Contains(t, fakeTH.lastBillingCookie, "; ")
	require.Positive(t, upstream.requests())
}

func TestTokenHarborPassParsePlainForm(t *testing.T) {
	page := `{"hasPass":false,"passName":"","renewsAt":"",` +
		`"spendAfterAllowance":false,"autoReloadEnabled":true}`
	now := time.Now().UTC()

	snapshot, err := parseTokenHarborPassPage(page, now)

	require.NoError(t, err)
	require.False(t, snapshot.HasPass)
	require.Empty(t, snapshot.PassName)
	require.Nil(t, snapshot.RenewsAt)
	require.False(t, snapshot.SpendAfterAllowance)
	require.True(t, snapshot.AutoReloadEnabled)
	require.Equal(t, now, snapshot.FetchedAt)
}

func TestTokenHarborPassParseMissingHasPassFails(t *testing.T) {
	_, err := parseTokenHarborPassPage("<html>no subscription data here</html>", time.Now().UTC())

	require.Error(t, err)
	require.Contains(t, err.Error(), "hasPass")
}

func TestTokenHarborPassProbeMissingHasPassFails(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	svc := NewTokenHarborPassService(&tokenHarborRepoStub{}, nil, upstream)
	svc.baseURL = fakeTH.server.URL
	// 覆盖 billing 响应为无 hasPass 的页面。
	fakeTH.mu.Lock()
	fakeTH.billingPage = "<html>billing page without subscription payload</html>"
	fakeTH.mu.Unlock()

	_, err := svc.Probe(context.Background(), tokenHarborTestAccount(2))

	require.Error(t, err)
	require.Contains(t, err.Error(), "hasPass")
}

func TestTokenHarborPassCookieCacheHitAnd401Relogin(t *testing.T) {
	// billing 第一次回 401：清缓存 → 重登一次 → 再打成功。
	svc, fakeTH, _ := newTokenHarborTestService(t, true)
	account := tokenHarborTestAccount(3)
	ctx := context.Background()

	_, err := svc.Probe(ctx, account)
	require.NoError(t, err)
	loginPosts, billingHits := fakeTH.stats()
	require.Equal(t, 2, loginPosts, "initial login + relogin after 401")
	require.Equal(t, 2, billingHits)

	// cookie 缓存命中：第二次 Probe 不再重登。
	_, err = svc.Probe(ctx, account)
	require.NoError(t, err)
	loginPosts, billingHits = fakeTH.stats()
	require.Equal(t, 2, loginPosts, "session cache must avoid re-login")
	require.Equal(t, 3, billingHits)
	require.Contains(t, fakeTH.lastBillingCookie, "sess-2", "billing must use the relogin cookie")
}

func TestTokenHarborPassProbeThrottledWithin10Minutes(t *testing.T) {
	svc, _, upstream := newTokenHarborTestService(t, false)
	account := tokenHarborTestAccount(4)
	ctx := context.Background()

	snapshot, err := svc.Probe(ctx, account)
	require.NoError(t, err)
	require.NoError(t, svc.PersistSnapshot(ctx, account.ID, snapshot))

	account.Extra = map[string]any{TokenHarborPassSnapshotExtraKey: snapshot}
	requestsBefore := upstream.requests()

	again, err := svc.Probe(ctx, account)
	require.NoError(t, err)
	require.Equal(t, requestsBefore, upstream.requests(), "throttled probe must not send any request")
	require.Equal(t, snapshot.FetchedAt, again.FetchedAt, "throttled probe returns the cached snapshot")
}

func TestTokenHarborPassProbeRequiresProxy(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	svc := NewTokenHarborPassService(&tokenHarborRepoStub{}, nil, upstream)
	svc.baseURL = fakeTH.server.URL
	account := tokenHarborTestAccount(5)
	account.ProxyID = nil
	account.Proxy = nil

	_, err := svc.Probe(context.Background(), account)

	require.Error(t, err)
	require.Contains(t, err.Error(), "proxy")
	require.Zero(t, upstream.requests())
}

func TestTokenHarborPassProbeRequiresEnabledAndCredentials(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	svc := NewTokenHarborPassService(&tokenHarborRepoStub{}, nil, upstream)
	svc.baseURL = fakeTH.server.URL

	account := tokenHarborTestAccount(6)
	account.Credentials["th_probe_enabled"] = false
	_, err := svc.Probe(context.Background(), account)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not enabled")

	account = tokenHarborTestAccount(6)
	delete(account.Credentials, "th_password")
	_, err = svc.Probe(context.Background(), account)
	require.Error(t, err)
	require.Contains(t, err.Error(), "credentials")
	require.Zero(t, upstream.requests())
}

func TestTokenHarborPassPersistAndReadSnapshotExtra(t *testing.T) {
	repo := &tokenHarborRepoStub{}
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	svc := NewTokenHarborPassService(repo, nil, upstream)
	svc.baseURL = fakeTH.server.URL
	account := tokenHarborTestAccount(8)

	snapshot, err := svc.Probe(context.Background(), account)
	require.NoError(t, err)
	require.NoError(t, svc.PersistSnapshot(context.Background(), account.ID, snapshot))
	require.Contains(t, repo.updates, int64(8))
	require.Equal(t, TokenHarborPassSnapshotExtraKey, "th_pass_snapshot")

	stored := &Account{Extra: repo.updates[8]}
	roundTripped, ok := TokenHarborPassSnapshotFromExtra(stored)
	require.True(t, ok)
	require.Equal(t, snapshot.Provider, roundTripped.Provider)
	require.Equal(t, snapshot.HasPass, roundTripped.HasPass)
	require.Equal(t, snapshot.PassName, roundTripped.PassName)
	require.True(t, roundTripped.FetchedAt.Equal(snapshot.FetchedAt))

	_, ok = TokenHarborPassSnapshotFromExtra(&Account{Extra: map[string]any{}})
	require.False(t, ok)
}

// ============================================================================
// D-TH-05A：会话持久化（三级读取）+ 风控退避状态机
// ============================================================================

func newTokenHarborTestServiceWithRepo(t *testing.T, repo *tokenHarborRepoStub, billing401Once bool) (*TokenHarborPassService, *tokenHarborFakeTH) {
	fakeTH := newTokenHarborFakeTH(t, billing401Once)
	return newTokenHarborServiceOn(repo, fakeTH), fakeTH
}

// newTokenHarborServiceOn 在同一假 TH 站点上建新 service 实例（模拟重启：
// 内存态清零，持久层 repo 共享）。
func newTokenHarborServiceOn(repo *tokenHarborRepoStub, fakeTH *tokenHarborFakeTH) *TokenHarborPassService {
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	svc := NewTokenHarborPassService(repo, nil, upstream)
	svc.baseURL = fakeTH.server.URL
	return svc
}

// 三级读取：内存未命中 → 持久化会话命中（重启恢复语义）→ 免登；并回填内存。
func TestTokenHarborPassSessionPersistedHitSkipsLogin(t *testing.T) {
	repo := &tokenHarborRepoStub{sessions: map[int64]tokenHarborSession{
		11: {cookie: "persisted-cookie", loginAt: time.Now()},
	}}
	// 新实例内存为空，模拟重启后从 extra 读会话。
	svc, fakeTH := newTokenHarborTestServiceWithRepo(t, repo, false)
	ctx := context.Background()

	snapshot, err := svc.Probe(ctx, tokenHarborTestAccount(11))
	require.NoError(t, err)
	require.True(t, snapshot.HasPass)
	loginPosts, billingHits := fakeTH.stats()
	require.Zero(t, loginPosts, "persisted session hit must skip login")
	require.Equal(t, 1, billingHits)
	require.Contains(t, fakeTH.lastBillingCookie, "persisted-cookie")

	// 回填内存：第二次 Probe 命中内存缓存，依旧零登录。
	_, err = svc.Probe(ctx, tokenHarborTestAccount(11))
	require.NoError(t, err)
	loginPosts, _ = fakeTH.stats()
	require.Zero(t, loginPosts, "refilled in-memory cache must skip login")
}

// 三级读取：持久化会话 TTL 过期（≥6h）→ 现场登录并回写持久层。
func TestTokenHarborPassSessionPersistedExpiredRelogins(t *testing.T) {
	repo := &tokenHarborRepoStub{sessions: map[int64]tokenHarborSession{
		12: {cookie: "stale-cookie", loginAt: time.Now().Add(-tokenHarborSessionTTL - time.Hour)},
	}}
	svc, fakeTH := newTokenHarborTestServiceWithRepo(t, repo, false)

	_, err := svc.Probe(context.Background(), tokenHarborTestAccount(12))
	require.NoError(t, err)
	loginPosts, _ := fakeTH.stats()
	require.Equal(t, 1, loginPosts, "expired persisted session must relogin")
	require.Contains(t, fakeTH.lastBillingCookie, "sess-1")
	require.Contains(t, repo.sessions[12].cookie, "sess-1", "fresh session must be persisted")
}

// 落库失败只 WARN：读失败与写失败都不阻断探针。
func TestTokenHarborPassSessionPersistFailureWarnOnly(t *testing.T) {
	// 写失败（StoreTokenHarborSession 走 updateErr）。
	repo := &tokenHarborRepoStub{updateErr: errors.New("db down")}
	svc, _ := newTokenHarborTestServiceWithRepo(t, repo, false)
	snapshot, err := svc.Probe(context.Background(), tokenHarborTestAccount(13))
	require.NoError(t, err, "store failure must not block probe")
	require.True(t, snapshot.HasPass)

	// 读失败（LoadTokenHarborSession 走 loadErr）→ 按无会话现场登录。
	repo = &tokenHarborRepoStub{loadErr: errors.New("db down")}
	svc, fakeTH := newTokenHarborTestServiceWithRepo(t, repo, false)
	_, err = svc.Probe(context.Background(), tokenHarborTestAccount(14))
	require.NoError(t, err, "load failure must not block probe")
	loginPosts, _ := fakeTH.stats()
	require.Equal(t, 1, loginPosts)
}

// rejected 双清：billing 401 → 内存 + extra 同步清除，重登后新会话落库。
func TestTokenHarborPassSessionRejectedDoubleClear(t *testing.T) {
	repo := &tokenHarborRepoStub{sessions: map[int64]tokenHarborSession{
		15: {cookie: "persisted-cookie", loginAt: time.Now()},
	}}
	svc, fakeTH := newTokenHarborTestServiceWithRepo(t, repo, true)

	_, err := svc.Probe(context.Background(), tokenHarborTestAccount(15))
	require.NoError(t, err)
	require.Equal(t, 1, repo.clearCalls, "rejected session must clear persisted keys")
	require.Contains(t, repo.sessions[15].cookie, "sess-1", "relogin session must replace the cleared one")
	loginPosts, billingHits := fakeTH.stats()
	require.Equal(t, 1, loginPosts)
	require.Equal(t, 2, billingHits)
}

// V1 退避闭包：触发→level/until/reason 正确；连续触发翻倍到上限 4h；
// 成功清除置 null 一次；无退避时成功不写。
func TestTokenHarborProbeBackoffTriggerEscalateAndClear(t *testing.T) {
	repo := &tokenHarborRepoStub{}
	svc, fakeTH := newTokenHarborTestServiceWithRepo(t, repo, false)
	ctx := context.Background()
	account := tokenHarborTestAccount(21)
	fakeTH.setLoginRejected(true)

	wantLevels := []int{0, 1, 2, 3, 3}
	wantDurations := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour}
	for i := range wantLevels {
		before := time.Now()
		_, err := svc.Probe(ctx, account)
		require.Error(t, err)
		require.Contains(t, err.Error(), "tokenharbor login rejected")

		update := repo.lastUpdate(account.ID)
		require.Equal(t, wantLevels[i], update[TokenHarborProbeBackoffLevelExtraKey], "round %d level", i)
		untilUnix, ok := update[TokenHarborProbeBackoffUntilExtraKey].(int64)
		require.True(t, ok, "round %d until must be int64 unix seconds", i)
		require.InDelta(t, before.Add(wantDurations[i]).Unix(), untilUnix, 5, "round %d until", i)
		reason, ok := update[TokenHarborProbeBackoffReasonExtraKey].(string)
		require.True(t, ok)
		require.Contains(t, reason, "tokenharbor login rejected")
		require.LessOrEqual(t, len([]rune(reason)), tokenHarborProbeBackoffReasonMaxLen)
		require.Equal(t, wantLevels[i], svc.backoffs[account.ID].level, "round %d memory level", i)
	}

	// 成功 → 三键置 null 一次。
	fakeTH.setLoginRejected(false)
	callsBefore := repo.updateCallCount()
	snapshot, err := svc.Probe(ctx, account)
	require.NoError(t, err)
	require.True(t, snapshot.HasPass)
	require.Equal(t, callsBefore+1, repo.updateCallCount(), "clear must write exactly once")
	update := repo.lastUpdate(account.ID)
	for _, key := range []string{TokenHarborProbeBackoffUntilExtraKey, TokenHarborProbeBackoffLevelExtraKey, TokenHarborProbeBackoffReasonExtraKey} {
		value, ok := update[key]
		require.True(t, ok, "clear update must contain key %s", key)
		require.Nil(t, value, "clear update must set %s to null", key)
	}
	require.Empty(t, svc.backoffs)

	// 已无退避：再次成功不写（避免每轮空写）。
	callsBefore = repo.updateCallCount()
	_, err = svc.Probe(ctx, account)
	require.NoError(t, err)
	require.Equal(t, callsBefore, repo.updateCallCount(), "success without backoff must not write")
}

// V1 重启恢复：新实例内存为空，以 extra 三键为"已有"基准继续升级。
func TestTokenHarborProbeBackoffRestartRecovery(t *testing.T) {
	repo := &tokenHarborRepoStub{}
	svc1, fakeTH := newTokenHarborTestServiceWithRepo(t, repo, false)
	ctx := context.Background()
	fakeTH.setLoginRejected(true)

	_, err := svc1.Probe(ctx, tokenHarborTestAccount(22))
	require.Error(t, err)
	first := repo.lastUpdate(22)
	require.Equal(t, 0, first[TokenHarborProbeBackoffLevelExtraKey])

	// 模拟重启：新实例（同一假站点）内存为空 + 账号 extra 带上轮落库退避
	// （DB JSON 反解为 float64）。
	restarted := newTokenHarborServiceOn(repo, fakeTH)
	require.Empty(t, restarted.backoffs)
	account := tokenHarborTestAccount(22)
	account.Extra = map[string]any{
		TokenHarborProbeBackoffUntilExtraKey:  float64(first[TokenHarborProbeBackoffUntilExtraKey].(int64)),
		TokenHarborProbeBackoffLevelExtraKey:  float64(0),
		TokenHarborProbeBackoffReasonExtraKey: first[TokenHarborProbeBackoffReasonExtraKey],
	}

	_, err = restarted.Probe(ctx, account)
	require.Error(t, err)
	second := repo.lastUpdate(22)
	require.Equal(t, 1, second[TokenHarborProbeBackoffLevelExtraKey], "restart must escalate from persisted level")

	// 重启后成功路径也能读到 extra 退避并清除。
	fakeTH.setLoginRejected(false)
	_, err = restarted.Probe(ctx, account)
	require.NoError(t, err)
	cleared := repo.lastUpdate(22)
	require.Nil(t, cleared[TokenHarborProbeBackoffUntilExtraKey])
	require.Empty(t, restarted.backoffs)
}

// 退避触发落库失败只 WARN：内存状态照常转移。
func TestTokenHarborProbeBackoffPersistFailureWarnOnly(t *testing.T) {
	repo := &tokenHarborRepoStub{updateErr: errors.New("db down")}
	svc, fakeTH := newTokenHarborTestServiceWithRepo(t, repo, false)
	fakeTH.setLoginRejected(true)

	_, err := svc.Probe(context.Background(), tokenHarborTestAccount(23))
	require.Error(t, err)
	require.Contains(t, err.Error(), "tokenharbor login rejected")
	require.Equal(t, 0, svc.backoffs[23].level, "memory transition must survive persist failure")
}

// V4 交错：手动成功/失败 × 自动成功/失败交错序列，断言最终状态=最后完成者。
func TestTokenHarborProbeBackoffInterleavedOutcomes(t *testing.T) {
	repo := &tokenHarborRepoStub{}
	svc, fakeTH := newTokenHarborTestServiceWithRepo(t, repo, false)
	ctx := context.Background()
	account := tokenHarborTestAccount(24)

	// 败、败
	fakeTH.setLoginRejected(true)
	_, err := svc.Probe(ctx, account)
	require.Error(t, err)
	_, err = svc.Probe(ctx, account)
	require.Error(t, err)
	require.Equal(t, 1, repo.lastUpdate(account.ID)[TokenHarborProbeBackoffLevelExtraKey])

	// 胜（最后完成者清退避）
	fakeTH.setLoginRejected(false)
	_, err = svc.Probe(ctx, account)
	require.NoError(t, err)
	require.Nil(t, repo.lastUpdate(account.ID)[TokenHarborProbeBackoffUntilExtraKey])
	require.Empty(t, svc.backoffs)

	// 败（最后完成者重新计退避，level 从 0 起）。先失效会话强制重登——
	// 拒登只发生在登录路径，缓存会话命中时探测不经登录。
	svc.invalidateSession(ctx, account.ID)
	fakeTH.setLoginRejected(true)
	_, err = svc.Probe(ctx, account)
	require.Error(t, err)
	require.Equal(t, 0, repo.lastUpdate(account.ID)[TokenHarborProbeBackoffLevelExtraKey])
	require.NotNil(t, repo.lastUpdate(account.ID)[TokenHarborProbeBackoffUntilExtraKey])

	// 胜（最后完成者：无退避）
	fakeTH.setLoginRejected(false)
	_, err = svc.Probe(ctx, account)
	require.NoError(t, err)
	final := repo.lastUpdate(account.ID)
	require.Nil(t, final[TokenHarborProbeBackoffUntilExtraKey])
	require.Nil(t, final[TokenHarborProbeBackoffLevelExtraKey])
	require.Nil(t, final[TokenHarborProbeBackoffReasonExtraKey])
	require.Empty(t, svc.backoffs, "final state must be the last finisher's (success, no backoff)")
}

// V2 解析器边界：输出只含 error 文案，不含响应体全文/堆栈/cookie/Authorization/
// token 片段。
func TestTokenHarborRSCErrorParseBoundary(t *testing.T) {
	body := strings.Join([]string{
		`0:{"cookie":"sb-auth-auth-token.0=secret-cookie-value","authorization":"Bearer secret-token-value"}`,
		`1:{"error":"Invalid login credentials"}`,
		`2:{"stack":"goroutine 1 [running]: main.handleLogin()"}`,
		`3:{"token":"tok_abc123secret"}`,
	}, "\n")

	parsed := parseTokenHarborRSCError(body)

	require.Equal(t, "Invalid login credentials", parsed)
	require.NotContains(t, parsed, body, "output must not contain the full response body")
	for _, fragment := range []string{"secret-cookie-value", "Bearer", "secret-token-value", "goroutine", "tok_abc123secret", "sb-auth-auth-token"} {
		require.NotContains(t, parsed, fragment)
	}
}

// V2 reason 200 字符截断（按字符计，不切断 UTF-8）。
func TestTokenHarborBackoffReasonTruncatedTo200(t *testing.T) {
	longErr := "tokenharbor login rejected: " + strings.Repeat("x", 500)
	require.Len(t, []rune(truncateTokenHarborBackoffReason(longErr)), tokenHarborProbeBackoffReasonMaxLen)

	cnErr := "tokenharbor login rejected: " + strings.Repeat("拒", 500)
	truncated := truncateTokenHarborBackoffReason(cnErr)
	require.Len(t, []rune(truncated), tokenHarborProbeBackoffReasonMaxLen)
	require.True(t, utf8.ValidString(truncated), "truncation must not cut a UTF-8 sequence")

	short := "tokenharbor login rejected: Invalid login credentials"
	require.Equal(t, short, truncateTokenHarborBackoffReason(short))
}
