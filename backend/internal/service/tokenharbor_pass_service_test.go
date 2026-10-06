package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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
	// usage CSV（th_usage_snapshot 探测用）：body 为空 = 默认空表头；status 非 0 = 覆盖状态码。
	usageCSVHits   int
	usageCSVBody   string
	usageCSVStatus int
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
	mux.HandleFunc("/api/usage/export.csv", fake.handleUsageCSV)
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

func (f *tokenHarborFakeTH) handleUsageCSV(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usageCSVHits++
	if r.Header.Get("Cookie") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.usageCSVStatus != 0 {
		w.WriteHeader(f.usageCSVStatus)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	_, _ = w.Write([]byte(f.usageCSVBody))
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
	account *Account
	// updates 保留每账号最后一次 UpdateExtra（旧用例语义）；
	// allWrites 记录全部写入（TH 双快照落库断言用）。
	updates   map[int64]map[string]any
	allWrites []map[string]any
}

func (r *tokenHarborRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account != nil {
		return r.account, nil
	}
	return &Account{ID: id}, nil
}

func (r *tokenHarborRepoStub) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	if r.updates == nil {
		r.updates = map[int64]map[string]any{}
	}
	r.updates[id] = updates
	r.allWrites = append(r.allWrites, updates)
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

// ---- th_usage_snapshot（§4.3）：CSV 窗口聚合 + 失败关闭 + 契约 ----

// 固定 now 的窗口切分：
//
//	todayStart = 2026-10-06T00:00:00Z；start7d = 2026-09-29T12:00:00Z；start30d = 2026-09-06T12:00:00Z
//	r1（11:00Z）→ today+7d+30d；r2（10-05T23:00Z，昨日）→ 7d+30d；
//	r3（09-28T13:00Z，7d 界外 1h）→ 仅 30d；r4（09-01）→ 30d 界外，不计入任何窗口。
const testTHUsageCSV = `timestamp,model,input_tokens,output_tokens,status,cost
2026-10-06T11:00:00Z,glm-5.3-flash,100,10,200,0
2026-10-05T23:00:00Z,glm-5.3-flash,200,20,200,0
2026-09-28T13:00:00Z,qwen3.8-flash,400,40,200,0
2026-09-01T00:00:00Z,qwen3.8-flash,8000,800,200,0
`

func TestParseTokenHarborUsageCSV_WindowAggregation(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	snapshot, err := parseTokenHarborUsageCSV(testTHUsageCSV, now)

	require.NoError(t, err)
	// §4.3 契约：windows 三键齐全，跨窗边界各归其位。
	require.Equal(t, []string{"30d", "7d", "today"}, sortedStringKeys(snapshot.Windows))
	require.Equal(t, TokenHarborUsageWindowTotals{Requests: 1, TokensIn: 100, TokensOut: 10}, snapshot.Windows["today"])
	require.Equal(t, TokenHarborUsageWindowTotals{Requests: 2, TokensIn: 300, TokensOut: 30}, snapshot.Windows["7d"])
	require.Equal(t, TokenHarborUsageWindowTotals{Requests: 3, TokensIn: 700, TokensOut: 70}, snapshot.Windows["30d"])
	require.Equal(t, now, snapshot.FetchedAt)
}

func TestParseTokenHarborUsageCSV_FailClosed(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"missing time column", "model,input_tokens,output_tokens\nm,1,2\n"},
		{"missing tokens_in column", "timestamp,model,output_tokens\n2026-10-06T11:00:00Z,m,2\n"},
		{"bad timestamp", "timestamp,input_tokens,output_tokens\nnot-a-time,1,2\n"},
		{"future timestamp", "timestamp,input_tokens,output_tokens\n2026-10-06T12:00:01Z,1,2\n"},
		{"bad token count", "timestamp,input_tokens,output_tokens\n2026-10-06T11:00:00Z,abc,2\n"},
		{"negative token count", "timestamp,input_tokens,output_tokens\n2026-10-06T11:00:00Z,-1,2\n"},
		{"empty timestamp cell", "timestamp,input_tokens,output_tokens\n,1,2\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTokenHarborUsageCSV(tc.body, now)
			require.Error(t, err, "解析失败必须失败关闭返回明确错误")
		})
	}
}

// §4.3 契约逐键断言：windows 下三窗口各含 requests/tokens_in/tokens_out，顶层 fetched_at。
func TestTokenHarborUsageSnapshotJSONContract(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	snapshot, err := parseTokenHarborUsageCSV(testTHUsageCSV, now)
	require.NoError(t, err)

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(encoded, &raw))
	require.Equal(t, []string{"fetched_at", "windows"}, sortedStringKeys(raw))
	windows := raw["windows"].(map[string]any)
	require.Equal(t, []string{"30d", "7d", "today"}, sortedStringKeys(windows))
	for _, name := range []string{"today", "7d", "30d"} {
		keys := sortedStringKeys(windows[name].(map[string]any))
		require.Equal(t, []string{"requests", "tokens_in", "tokens_out"}, keys, "窗口 %s 键名必须与 §4.3 契约一致", name)
	}
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ProbeUsageSnapshot 端到端：登录链 → 登录态 GET /api/usage/export.csv → 聚合；
// 会话缓存复用（第二次调用不重登）；UA/浏览器语义由 do() 统一带上。
func TestTokenHarborPassProbeUsageSnapshot(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	repo := &tokenHarborRepoStub{}
	svc := NewTokenHarborPassService(repo, nil, upstream)
	svc.baseURL = fakeTH.server.URL
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV
	fakeTH.mu.Unlock()

	snapshot, err := svc.ProbeUsageSnapshot(context.Background(), tokenHarborTestAccount(11))

	require.NoError(t, err)
	require.Equal(t, 1, fakeTH.loginPosts, "首次探测必须走一次登录链")
	require.Equal(t, 1, fakeTH.usageCSVHits)
	require.InDelta(t, 1, snapshot.Windows["today"].Requests, 0.001)
	require.InDelta(t, 100, snapshot.Windows["today"].TokensIn, 0.001)
	require.InDelta(t, 3, snapshot.Windows["30d"].Requests, 0.001)

	// 第二次：会话缓存命中 → 不重登（usage 快照节流：Extra 未落库所以仍拉 CSV）。
	_, err = svc.ProbeUsageSnapshot(context.Background(), tokenHarborTestAccount(11))
	require.NoError(t, err)
	require.Equal(t, 1, fakeTH.loginPosts, "会话必须复用，不得重登")
	require.Equal(t, 2, fakeTH.usageCSVHits)
}

// ---- 外审 F1：生产入口 → Probe → 快照落库 → DTO 输出 集成验证 ----

// thProductionAccount 构造 platform=openai + base_url=tokenharbor.ai 的账号（线上 207 形态）。
func thProductionAccount(id int64) *Account {
	account := tokenHarborTestAccount(id)
	account.Platform = PlatformOpenAI
	account.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	return account
}

// TestTokenHarborQuotaProductionEntryIntegration 从生产唯一入口
// （CNProviderQuotaService.QueryUsage——管理端手动查询按钮链路）发起：TH 分支
// → Probe/ProbeUsageSnapshot → th_pass_snapshot/th_usage_snapshot 落库 →
// DTO 快照输出（§4.3）。httptest 假站点，全链路无真实外呼。
func TestTokenHarborQuotaProductionEntryIntegration(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	repo := &tokenHarborRepoStub{}
	quotaSvc := NewCNProviderQuotaService(repo, nil, upstream, nil)
	thSvc := NewTokenHarborPassService(repo, nil, upstream)
	thSvc.baseURL = fakeTH.server.URL
	quotaSvc.SetTokenHarborPassService(thSvc)
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV
	fakeTH.mu.Unlock()
	account := thProductionAccount(21)
	repo.account = account

	result, err := quotaSvc.QueryUsage(context.Background(), 21)

	// 生产入口 → Probe：登录链 + billing + CSV 全部发生。
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, TokenHarborPassProviderName, result.Provider)
	loginPosts, billingHits := fakeTH.stats()
	require.Equal(t, 1, loginPosts)
	require.Equal(t, 1, billingHits)
	require.Equal(t, 1, fakeTH.usageCSVHits)
	// 快照落库：th_pass_snapshot 与 th_usage_snapshot 双键写入。
	require.Len(t, repo.allWrites, 2)
	persistedKeys := map[string]bool{}
	for _, w := range repo.allWrites {
		for k := range w {
			persistedKeys[k] = true
		}
	}
	require.True(t, persistedKeys[TokenHarborPassSnapshotExtraKey], "th_pass_snapshot 必须落库")
	require.True(t, persistedKeys[TokenHarborUsageSnapshotExtraKey], "th_usage_snapshot 必须落库")
	// DTO 快照输出（§4.3 键名对齐）。
	require.NotNil(t, result.Snapshot)
	require.NotNil(t, result.Snapshot.HasPass)
	require.True(t, *result.Snapshot.HasPass)
	require.Equal(t, "Agent Pass", result.Snapshot.PassName)
	require.NotEmpty(t, result.Snapshot.RenewsAt, "renews_at 必须输出（RFC3339）")
	_, err = time.Parse(time.RFC3339, result.Snapshot.RenewsAt)
	require.NoError(t, err)
	require.Len(t, result.Snapshot.Windows, 3)
	require.InDelta(t, 1, result.Snapshot.Windows["today"].Requests, 0.001)
	require.InDelta(t, 3, result.Snapshot.Windows["30d"].Requests, 0.001)

	// 从 Extra 读回快照（管理端面板无探测读取路径）。
	stored := &Account{Extra: repo.updates[21]}
	passSnap, ok := TokenHarborPassSnapshotFromExtra(stored)
	require.True(t, ok)
	require.True(t, passSnap.HasPass)
	usageSnap, ok := TokenHarborUsageSnapshotFromExtra(stored)
	require.True(t, ok)
	require.Len(t, usageSnap.Windows, 3)
}

// 集成失败关闭：usage CSV 解析失败 → 明确错误、th_usage_snapshot 不落库
// （pass 快照独立成功落库），DTO 快照输出不产出。
func TestTokenHarborQuotaProductionEntryIntegration_FailClosedOnBadCSV(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	repo := &tokenHarborRepoStub{}
	quotaSvc := NewCNProviderQuotaService(repo, nil, upstream, nil)
	thSvc := NewTokenHarborPassService(repo, nil, upstream)
	thSvc.baseURL = fakeTH.server.URL
	quotaSvc.SetTokenHarborPassService(thSvc)
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = "timestamp,input_tokens,output_tokens\nnot-a-time,1,2\n"
	fakeTH.mu.Unlock()
	account := thProductionAccount(22)
	repo.account = account

	result, err := quotaSvc.QueryUsage(context.Background(), 22)

	require.NoError(t, err)
	require.False(t, result.Success, "usage CSV 解析失败必须失败关闭")
	require.Contains(t, result.Error, "timestamp")
	require.Nil(t, result.Snapshot, "失败关闭路径不产出快照输出")
	// 只有 th_pass_snapshot 落库；th_usage_snapshot 不落。
	require.Len(t, repo.allWrites, 1)
	require.Contains(t, repo.allWrites[0], TokenHarborPassSnapshotExtraKey)
}
