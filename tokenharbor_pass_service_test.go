package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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
	updates map[int64]map[string]any
}

func (r *tokenHarborRepoStub) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	if r.updates == nil {
		r.updates = map[int64]map[string]any{}
	}
	r.updates[id] = updates
	return nil
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
