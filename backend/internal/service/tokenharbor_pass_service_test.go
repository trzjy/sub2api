package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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
	// 官方 /api/me/free-tier 完整响应样例（D-QLM-006 冻结键名）：reset_at 微秒+
	// 时区时间戳，plan.window_days=7，plan_used_pct=100，plan_exhausted=true，
	// used_pct=12.5，exhausted=false。
	testTHFreeTierJSON = `{"reset_at":"2026-10-12T16:06:10.343487+00:00",` +
		`"plan":{"window_days":7},"plan_used_pct":100,"plan_exhausted":true,` +
		`"used_pct":12.5,"exhausted":false}`

	// C6 钱包余额采集 fixture：登录态 /dashboard/billing 脱敏 RSC 片段。顶栏组件
	// 含 userId + initialBalance，hero 组件含 balance + lockedBonus；RSC 转义形态
	// \"balance\": 必须按页面原文匹配（与订阅链 hasPass 同款转义纪律）。
	// 0 值（Pass 订阅号未充值稳态）与正值各一组。
	testTHWalletBillingRSCZero = `1:{\"userId\":\"usr_desensitized_id\",\"initialBalance\":0}` +
		`2:{\"balance\":0,\"lockedBonus\":0,\"balance_source\":\"paid\"}`
	testTHWalletBillingRSCPositive = `1:{\"userId\":\"usr_desensitized_id\",\"initialBalance\":123.45}` +
		`2:{\"balance\":123.45,\"lockedBonus\":10.5,\"balance_source\":\"paid\"}`
	// 非 RSC 的纯 HTML 页（如形态变化/错误页），无任何 balance 字段。
	testTHWalletBillingHTML = `<!doctype html><html><body><h1>Dashboard</h1><p>billing not rendered</p></body></html>`
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
	// usage CSV（th_usage_snapshot 探测用）：body 为空 = 默认空表头；status 非 0 = 覆盖状态码。
	usageCSVHits   int
	usageCSVBody   string
	usageCSVStatus int
	// billing 状态码覆盖（C6 钱包采集失败关闭用例）：非 0 = 直接回该状态码。
	billingStatus int
	// free-tier（D-QLM-006）：body 为空 = 默认官方样例 JSON；status 非 0 = 覆盖状态码。
	freeTierHits   int
	freeTierBody   string
	freeTierStatus int
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
	mux.HandleFunc("/api/me/free-tier", fake.handleFreeTier)
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
	if f.billingStatus != 0 {
		w.WriteHeader(f.billingStatus)
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

func (f *tokenHarborFakeTH) handleFreeTier(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freeTierHits++
	if r.Header.Get("Cookie") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.freeTierStatus != 0 {
		w.WriteHeader(f.freeTierStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	body := f.freeTierBody
	if body == "" {
		body = testTHFreeTierJSON
	}
	_, _ = w.Write([]byte(body))
}

func (f *tokenHarborFakeTH) setFreeTierStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freeTierStatus = status
}

func (f *tokenHarborFakeTH) setFreeTierBody(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freeTierBody = body
}

// setBillingStatus 覆盖 /dashboard/billing 响应状态码（C6 钱包采集 404 失败关闭用例）。
func (f *tokenHarborFakeTH) setBillingStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.billingStatus = status
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

	account *Account
	mu      sync.Mutex
	// updates 保留每账号最后一次 UpdateExtra（JSONB 合并语义，与生产对齐）。
	updates map[int64]map[string]any
	// allWrites 记录全部写入（TH 双快照落库断言用，HEAD 用例语义）。
	allWrites []map[string]any
	// updateCalls 记录每次 UpdateExtra 的完整 updates map（dql-5 用例语义）。
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

func (r *tokenHarborRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account != nil {
		return r.account, nil
	}
	return &Account{ID: id}, nil
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
	// 与生产 UpdateExtra 的 JSONB 合并语义对齐：按账号合并键，不整替。
	if r.updates[id] == nil {
		r.updates[id] = map[string]any{}
	}
	for k, v := range updates {
		r.updates[id][k] = v
	}
	r.allWrites = append(r.allWrites, updates)
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
		Type:    AccountTypeAPIKey,
		Status:  StatusActive,
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

// testTHUsageCSV 构造按 now 锚定的 usage CSV：三行分别落 today / 7d-only /
// 7d 窗外，窗口归属与 tokenharbor.ai 线上形态对齐。D-QLM-006 起 windows 只留
// today/7d 两键（30d 本地聚合删除）。相对锚定避免日期翻转炸弹。
func testTHUsageCSV(now time.Time) string {
	todayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	return fmt.Sprintf(`timestamp,model,input_tokens,output_tokens,status,cost
%s,glm-5.3-flash,100,10,200,0
%s,glm-5.3-flash,200,20,200,0
%s,qwen3.8-flash,400,40,200,0
`,
		todayStart.Format(time.RFC3339),                                   // today 窗口
		todayStart.Add(-1*time.Hour).Format(time.RFC3339),                 // 昨日 23:00，today+7d
		todayStart.Add(-8*24*time.Hour+13*time.Hour).Format(time.RFC3339), // 8 天前 13:00，7d 窗外
	)
}

func TestParseTokenHarborUsageCSV_WindowAggregation(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	snapshot, err := parseTokenHarborUsageCSV(testTHUsageCSV(now), now)

	require.NoError(t, err)
	// §4.3 契约：windows today/7d 两键，跨窗边界各归其位。
	require.Equal(t, []string{"7d", "today"}, sortedStringKeys(snapshot.Windows))
	require.Equal(t, TokenHarborUsageWindowTotals{Requests: 1, TokensIn: 100, TokensOut: 10}, snapshot.Windows["today"])
	require.Equal(t, TokenHarborUsageWindowTotals{Requests: 2, TokensIn: 300, TokensOut: 30}, snapshot.Windows["7d"])
	require.Equal(t, now, snapshot.FetchedAt)
}

// TestParseTokenHarborUsageCSV_NewOfficialHeader D-TH-02：新官方表头
// `timestamp (iso utc),...,tokens in,tokens out,...`（空格分隔、带括号）必须命中
// 三组新追加别名并解析成功，窗口归属与 testTHUsageCSV 同口径。旧表头用例不受影响
// （向后兼容，别名不删）。
func TestParseTokenHarborUsageCSV_NewOfficialHeader(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	todayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)

	body := fmt.Sprintf(`timestamp (iso utc),source,api key,model,status,error code,tokens in,tokens out,output visible tokens,output thinking tokens,cache layer,cache read tokens,cache savings usd,cost usd
%s,,k1,glm-5.3-flash,200,,100,10,,,,,,
%s,,k2,qwen3.8-flash,200,,200,20,,,,,,
%s,,k3,glm-5.3-flash,200,,400,40,,,,,,
%s,,k4,qwen3.8-flash,200,,8000,800,,,,,,
`,
		todayStart.Format(time.RFC3339),                                   // today 窗口
		todayStart.Add(-1*time.Hour).Format(time.RFC3339),                 // 昨日 23:00，today+7d+30d
		todayStart.Add(-8*24*time.Hour+13*time.Hour).Format(time.RFC3339), // 8 天前 13:00，仅 30d
		todayStart.Add(-35*24*time.Hour).Format(time.RFC3339),             // 35 天前，30d 窗外
	)

	snapshot, err := parseTokenHarborUsageCSV(body, now)

	require.NoError(t, err, "新官方表头必须命中别名并解析成功（D-TH-02）")
	require.Equal(t, []string{"7d", "today"}, sortedStringKeys(snapshot.Windows))
	require.Equal(t, TokenHarborUsageWindowTotals{Requests: 1, TokensIn: 100, TokensOut: 10}, snapshot.Windows["today"])
	require.Equal(t, TokenHarborUsageWindowTotals{Requests: 2, TokensIn: 300, TokensOut: 30}, snapshot.Windows["7d"])
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

// TestParseTokenHarborUsageCSV_EmptyTokenCountRows D-TH-03：新官方 CSV 含
// `error code` 列，status 为错误行的 tokens in/out 为**空串**（上游"该请求失败
// 无 token 计数"的确定性表达，非数据损坏）。空串应如实解析为 0：请求计数 +1、
// tokens 计 0，解析成功不失败关闭；非数字、负数仍失败关闭。
func TestParseTokenHarborUsageCSV_EmptyTokenCountRows(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	todayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)

	// 新官方 14 列表头（含 error code 列）。用 strings.Join 构造数据行以杜绝
	// 逗号计数错位导致的 field 数不符。
	header := []string{
		"timestamp (iso utc)", "source", "api key", "model", "status", "error code",
		"tokens in", "tokens out", "output visible tokens", "output thinking tokens",
		"cache layer", "cache read tokens", "cache savings usd", "cost usd",
	}
	// 构造一行：ts / status / tokens_in / tokens_out 之后其余列均为空。
	row := func(ts, status, tokensIn, tokensOut string) string {
		r := make([]string, len(header))
		r[0] = ts
		r[4] = status
		r[6] = tokensIn
		r[7] = tokensOut
		return strings.Join(r, ",")
	}
	body := func(lines ...string) string {
		return strings.Join(append([]string{strings.Join(header, ",")}, lines...), "\n") + "\n"
	}

	t.Run("error row with empty token counts parses as 0", func(t *testing.T) {
		// 错误行：status=429、tokens in/out 为空串 → 如实解析为 requests +1、tokens 0。
		b := body(row(todayStart.Format(time.RFC3339), "429", "", ""))

		snapshot, err := parseTokenHarborUsageCSV(b, now)

		require.NoError(t, err, "错误行空 token 列必须按官方语义解析为 0，不失败关闭（D-TH-03）")
		require.Equal(t, TokenHarborUsageWindowTotals{Requests: 1, TokensIn: 0, TokensOut: 0}, snapshot.Windows["today"],
			"错误行：请求计数 +1、tokens 计 0")
	})

	t.Run("non-numeric token count still fails closed", func(t *testing.T) {
		b := body(row(todayStart.Format(time.RFC3339), "200", "abc", "10"))

		_, err := parseTokenHarborUsageCSV(b, now)

		require.Error(t, err, "tokens in 为非数字仍须失败关闭")
	})

	t.Run("negative token count still fails closed", func(t *testing.T) {
		b := body(row(todayStart.Format(time.RFC3339), "200", "-1", "10"))

		_, err := parseTokenHarborUsageCSV(b, now)

		require.Error(t, err, "tokens in 为负仍须失败关闭")
	})
}

func TestTokenHarborUsageSnapshotJSONContract(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	snapshot, err := parseTokenHarborUsageCSV(testTHUsageCSV(now), now)
	require.NoError(t, err)

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(encoded, &raw))
	require.Equal(t, []string{"fetched_at", "windows"}, sortedStringKeys(raw))
	windows := raw["windows"].(map[string]any)
	require.Equal(t, []string{"7d", "today"}, sortedStringKeys(windows))
	for _, name := range []string{"today", "7d"} {
		keys := sortedStringKeys(windows[name].(map[string]any))
		require.Equal(t, []string{"requests", "tokens_in", "tokens_out"}, keys, "窗口 %s 键名必须与 §4.3 契约一致", name)
	}
}

// D-QLM-022：plan_used_pct / used_pct 为 0 时（窗口刚重置、req_used 小）是官方
// 合法值，omitempty 不得吞键——否则 PersistSnapshot 落 extra 整键丢失，前端津贴行隐藏。
func TestTokenHarborPassSnapshotZeroPctStored(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	snap := TokenHarborPassSnapshot{
		Provider:      "tokenharbor",
		HasPass:       true,
		FetchedAt:     now,
		ResetAt:       &now,
		WindowDays:    30,
		PlanUsedPct:   0,
		PlanExhausted: false,
		UsedPct:       0,
		Exhausted:     false,
	}

	encoded, err := json.Marshal(snap)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(encoded, &raw))

	// 0 值不得被 omitempty 吞掉：键必须存在且值为数值 0。
	planVal, ok := raw["plan_used_pct"]
	require.True(t, ok, "plan_used_pct 键必须存在（0 是合法值，不得省略），编码结果：%s", string(encoded))
	require.EqualValues(t, 0, planVal, "plan_used_pct 应为 0")

	usedVal, ok := raw["used_pct"]
	require.True(t, ok, "used_pct 键必须存在（0 是合法值，不得省略），编码结果：%s", string(encoded))
	require.EqualValues(t, 0, usedVal, "used_pct 应为 0")
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
	// D-QL-009 F2：服务存在时钟注入面（now func() time.Time，tokenharbor_pass_service.go
	// 构造默认 time.Now，ProbeUsageSnapshot 链路经 s.now() 取时）——注入固定锚点，
	// 夹具与窗口断言同一次取时，消除夹具 testTHUsageCSV(time.Now()) 与服务内部
	// time.Now() 两次独立取时跨 UTC 00:00 的 today 窗竞态。
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()

	snapshot, err := svc.ProbeUsageSnapshot(context.Background(), tokenHarborTestAccount(11))

	require.NoError(t, err)
	require.Equal(t, 1, fakeTH.loginPosts, "首次探测必须走一次登录链")
	require.Equal(t, 1, fakeTH.usageCSVHits)
	require.InDelta(t, 1, snapshot.Windows["today"].Requests, 0.001)
	require.InDelta(t, 100, snapshot.Windows["today"].TokensIn, 0.001)
	require.InDelta(t, 2, snapshot.Windows["7d"].Requests, 0.001)

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
	// D-QL-009 F2：同锚注入（注释详见 TestTokenHarborPassProbeUsageSnapshot）——
	// QueryUsage → ProbeUsageSnapshot 链路的窗口取时全部经 thSvc.now()。
	now := time.Now().UTC()
	thSvc.now = func() time.Time { return now }
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
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
	// D-QLM-006：free-tier 字段组随快照透传。
	require.NotEmpty(t, result.Snapshot.ResetAt, "free-tier reset_at 必须输出（RFC3339）")
	require.Equal(t, "2026-10-12T16:06:10Z", result.Snapshot.ResetAt, "reset_at 微秒时间戳截断为 RFC3339 秒级 Z")
	_, err = time.Parse(time.RFC3339, result.Snapshot.ResetAt)
	require.NoError(t, err)
	require.Equal(t, 7, result.Snapshot.WindowDays)
	require.InDelta(t, 100, result.Snapshot.PlanUsedPct, 0.001)
	require.True(t, result.Snapshot.PlanExhausted)
	require.InDelta(t, 12.5, result.Snapshot.UsedPct, 0.001)
	require.False(t, result.Snapshot.Exhausted)
	require.Len(t, result.Snapshot.Windows, 2)
	require.InDelta(t, 1, result.Snapshot.Windows["today"].Requests, 0.001)

	// 从 Extra 读回快照（管理端面板无探测读取路径）。
	stored := &Account{Extra: repo.updates[21]}
	passSnap, ok := TokenHarborPassSnapshotFromExtra(stored)
	require.True(t, ok)
	require.True(t, passSnap.HasPass)
	require.NotNil(t, passSnap.ResetAt)
	require.Equal(t, 7, passSnap.WindowDays)
	require.InDelta(t, 100, passSnap.PlanUsedPct, 0.001)
	require.True(t, passSnap.PlanExhausted)
	usageSnap, ok := TokenHarborUsageSnapshotFromExtra(stored)
	require.True(t, ok)
	require.Len(t, usageSnap.Windows, 2)
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

// ============================================================================
// D-QLM-006：free-tier 官方字段接入（GET /api/me/free-tier）
// ============================================================================

// 官方样例 JSON（含微秒+时区时间戳）解析出 reset_at=2026-10-12T16:06:10Z、
// plan_used_pct=100、plan_exhausted=true、window_days=7。
func TestParseTokenHarborFreeTier_OfficialSample(t *testing.T) {
	fields, err := parseTokenHarborFreeTier(testTHFreeTierJSON)
	require.NoError(t, err)
	require.NotNil(t, fields.ResetAt)
	require.Equal(t, "2026-10-12T16:06:10Z", fields.ResetAt.UTC().Format(time.RFC3339),
		"reset_at 微秒+时区时间戳解析为正确瞬间，RFC3339 截断为 2026-10-12T16:06:10Z")
	require.Equal(t, 7, fields.WindowDays)
	require.InDelta(t, 100, fields.PlanUsedPct, 0.001)
	require.True(t, fields.PlanExhausted)
	require.InDelta(t, 12.5, fields.UsedPct, 0.001)
	require.False(t, fields.Exhausted)
}

// reset_at 缺失 = 结构漂移，失败关闭（D-QLM-013：官方端点恒返回 reset_at；
// `{}` 这类结构不完整响应不得解码成功落全零快照覆盖 Extra 旧值）。
// TestParseTokenHarborFreeTier_EmptyResetAt 原断言"缺 reset_at 放行"，与本卡
// 新语义冲突，按 D-QLM-013 翻转为失败关闭断言。
func TestParseTokenHarborFreeTier_EmptyResetAt(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing reset_at with other fields", `{"plan":{"window_days":7},"plan_used_pct":0,"plan_exhausted":false,"used_pct":0,"exhausted":false}`},
		{"empty object", `{}`},
		{"only plan_used_pct", `{"plan_used_pct":50}`},
		{"blank reset_at", `{"reset_at":"  ","plan":{"window_days":7}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTokenHarborFreeTier(tc.body)
			require.Error(t, err, "缺 reset_at 必须失败关闭返回明确错误（D-QLM-013）")
			require.Contains(t, err.Error(), "missing reset_at")
		})
	}
}

// 坏 JSON → 失败关闭。
func TestParseTokenHarborFreeTier_BadJSONFailsClosed(t *testing.T) {
	_, err := parseTokenHarborFreeTier("not-json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "free-tier")
}

// D-QLM-006 失败语义：free-tier 拉取 500 → 整个 Probe 失败关闭（明确错误、不落
// 半截快照），与 CSV 链同口径；Probe 自身不落 Extra，allWrites 必为空。
func TestTokenHarborPassProbeFreeTierFailClosed_500(t *testing.T) {
	repo := &tokenHarborRepoStub{}
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	svc := NewTokenHarborPassService(repo, nil, upstream)
	svc.baseURL = fakeTH.server.URL
	fakeTH.setFreeTierStatus(http.StatusInternalServerError)

	_, err := svc.Probe(context.Background(), tokenHarborTestAccount(31))
	require.Error(t, err, "free-tier 500 必须失败关闭")
	require.Contains(t, err.Error(), "free-tier")
	require.Empty(t, repo.allWrites, "Probe 失败不得落任何 Extra 快照")
}

// D-QLM-006 失败语义：free-tier 返回坏 JSON → 整个 Probe 失败关闭。
func TestTokenHarborPassProbeFreeTierFailClosed_BadJSON(t *testing.T) {
	repo := &tokenHarborRepoStub{}
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	svc := NewTokenHarborPassService(repo, nil, upstream)
	svc.baseURL = fakeTH.server.URL
	fakeTH.setFreeTierBody("not-json")

	_, err := svc.Probe(context.Background(), tokenHarborTestAccount(32))
	require.Error(t, err, "free-tier 坏 JSON 必须失败关闭")
	require.Contains(t, err.Error(), "free-tier")
	require.Empty(t, repo.allWrites, "Probe 失败不得落任何 Extra 快照")
}

// D-QLM-013 失败语义：free-tier 返回结构不完整响应（缺 reset_at，如 `{}`）→
// 整个 Probe 失败关闭（明确错误、不落快照），不落 plan_exhausted=false 的全零
// 快照覆盖 Extra 旧值。
func TestTokenHarborPassProbeFreeTierFailClosed_MissingResetAt(t *testing.T) {
	repo := &tokenHarborRepoStub{}
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	svc := NewTokenHarborPassService(repo, nil, upstream)
	svc.baseURL = fakeTH.server.URL
	fakeTH.setFreeTierBody(`{}`)

	_, err := svc.Probe(context.Background(), tokenHarborTestAccount(34))
	require.Error(t, err, "free-tier 缺 reset_at 必须失败关闭（D-QLM-013）")
	require.Contains(t, err.Error(), "missing reset_at")
	require.Empty(t, repo.allWrites, "Probe 失败不得落任何 Extra 快照")
}

// D-QLM-006 集成失败关闭：free-tier 500 → 生产入口 QueryUsage 返回失败、不产出
// 快照输出、th_pass_snapshot 不落库（明确错误，不落半截快照）。
func TestTokenHarborQuotaProductionEntryIntegration_FailClosedOnFreeTier500(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	repo := &tokenHarborRepoStub{}
	quotaSvc := NewCNProviderQuotaService(repo, nil, upstream, nil)
	thSvc := NewTokenHarborPassService(repo, nil, upstream)
	thSvc.baseURL = fakeTH.server.URL
	quotaSvc.SetTokenHarborPassService(thSvc)
	fakeTH.setFreeTierStatus(http.StatusInternalServerError)
	account := thProductionAccount(33)
	repo.account = account

	result, err := quotaSvc.QueryUsage(context.Background(), 33)

	require.NoError(t, err)
	require.False(t, result.Success, "free-tier 500 必须失败关闭")
	require.Contains(t, result.Error, "free-tier")
	require.Nil(t, result.Snapshot, "失败关闭路径不产出快照输出")
	require.Empty(t, repo.allWrites, "free-tier 失败不得落 th_pass_snapshot")
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

// ============================================================================
// C6：TH 钱包余额采集（登录态 /dashboard/billing RSC 页解析）
// ============================================================================

// 解析成功：0 值 fixture（Pass 订阅号未充值稳态），Value/LockedBonus 均 0。
func TestParseTokenHarborWalletPage_ZeroValue(t *testing.T) {
	now := time.Now().UTC()
	snapshot, err := parseTokenHarborWalletPage(testTHWalletBillingRSCZero, now)
	require.NoError(t, err)
	require.Equal(t, TokenHarborPassProviderName, snapshot.Provider)
	require.Equal(t, 0.0, snapshot.Value, "0 是未充值账号的合法稳态，不得失败关闭")
	require.Equal(t, 0.0, snapshot.LockedBonus)
	require.Equal(t, now, snapshot.ObservedAt)
	require.Empty(t, snapshot.Error)
}

// 解析成功：正值 fixture，Value/LockedBonus 如实落。
func TestParseTokenHarborWalletPage_PositiveValue(t *testing.T) {
	now := time.Now().UTC()
	snapshot, err := parseTokenHarborWalletPage(testTHWalletBillingRSCPositive, now)
	require.NoError(t, err)
	require.InDelta(t, 123.45, snapshot.Value, 0.001, "hero balance 必须如实解析为正值")
	require.InDelta(t, 10.5, snapshot.LockedBonus, 0.001, "lockedBonus 必须如实解析")
	require.Equal(t, now, snapshot.ObservedAt)
}

// 失败关闭（解析层）：无 balance 字段 / 无 initialBalance 字段（页面形态变化）/
// 顶栏与 hero 不一致 / 纯 HTML 非 RSC → 一律返回明确错误，不落任何键。
func TestParseTokenHarborWalletPage_FailClosed(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		body string
	}{
		{"no balance field (only hasPass page)", testTHBillingRSC},
		{"no initialBalance field (hero only)", `2:{\"balance\":5,\"lockedBonus\":0}`},
		{"balance vs initialBalance mismatch", `1:{\"userId\":\"x\",\"initialBalance\":1}` + `2:{\"balance\":2,\"lockedBonus\":0}`},
		{"plain HTML, not RSC", testTHWalletBillingHTML},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTokenHarborWalletPage(tc.body, now)
			require.Error(t, err, "解析失败/页面形态变化/不一致必须失败关闭返回明确错误")
		})
	}
}

// 端到端成功：登录链 → 登录态 GET /dashboard/billing → 解析 → 落 SSOT 键组
// （th_balance / th_balance_updated_at）+ 钱包快照（th_wallet_snapshot）。
func TestTokenHarborProbeWalletBalance_SuccessAndPersist(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	repo := &tokenHarborRepoStub{}
	svc := NewTokenHarborPassService(repo, nil, upstream)
	svc.baseURL = fakeTH.server.URL
	fakeTH.mu.Lock()
	fakeTH.billingPage = testTHWalletBillingRSCPositive
	fakeTH.mu.Unlock()
	account := tokenHarborTestAccount(41)
	ctx := context.Background()

	snapshot, err := svc.ProbeWalletBalance(ctx, account)
	require.NoError(t, err)
	require.InDelta(t, 123.45, snapshot.Value, 0.001)
	require.InDelta(t, 10.5, snapshot.LockedBonus, 0.001)
	require.False(t, snapshot.ObservedAt.IsZero())

	require.NoError(t, svc.PersistWalletSnapshot(ctx, account.ID, snapshot))
	require.Contains(t, repo.updates, int64(41))
	last := repo.lastUpdate(41)
	require.Equal(t, TokenHarborWalletBalanceExtraKey, "th_balance")
	require.Equal(t, 123.45, last[TokenHarborWalletBalanceExtraKey])
	require.NotEmpty(t, last[TokenHarborWalletBalanceUpdatedAtExtraKey], "th_balance_updated_at 必须落时间戳")
	require.Contains(t, last, TokenHarborWalletSnapshotExtraKey)

	// 读回快照结构体。
	stored := &Account{Extra: repo.updates[41]}
	roundTripped, ok := TokenHarborWalletSnapshotFromExtra(stored)
	require.True(t, ok)
	require.InDelta(t, 123.45, roundTripped.Value, 0.001)
	require.InDelta(t, 10.5, roundTripped.LockedBonus, 0.001)
	require.Equal(t, snapshot.ObservedAt.UTC().Format(time.RFC3339), roundTripped.ObservedAt.UTC().Format(time.RFC3339))

	require.Equal(t, 1, fakeTH.loginPosts, "首次探测必须走一次登录链")
	require.Equal(t, 1, fakeTH.billingHits)
}

// 失败关闭（端到端）：billing 404 / 纯 HTML 非 RSC / 顶栏与 hero 不一致 → 返回
// 明确错误且不得落任何 SSOT 键与快照（repo.allWrites 必须为空）。
func TestTokenHarborProbeWalletBalance_FailClosed(t *testing.T) {
	t.Run("billing 404", func(t *testing.T) {
		fakeTH := newTokenHarborFakeTH(t, false)
		upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
		repo := &tokenHarborRepoStub{}
		svc := NewTokenHarborPassService(repo, nil, upstream)
		svc.baseURL = fakeTH.server.URL
		fakeTH.setBillingStatus(http.StatusNotFound)

		_, err := svc.ProbeWalletBalance(context.Background(), tokenHarborTestAccount(42))
		require.Error(t, err)
		require.Contains(t, err.Error(), "status 404")
		require.Empty(t, repo.allWrites, "失败关闭不得落任何 Extra 键")
	})

	t.Run("plain HTML not RSC", func(t *testing.T) {
		fakeTH := newTokenHarborFakeTH(t, false)
		upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
		repo := &tokenHarborRepoStub{}
		svc := NewTokenHarborPassService(repo, nil, upstream)
		svc.baseURL = fakeTH.server.URL
		fakeTH.mu.Lock()
		fakeTH.billingPage = testTHWalletBillingHTML
		fakeTH.mu.Unlock()

		_, err := svc.ProbeWalletBalance(context.Background(), tokenHarborTestAccount(43))
		require.Error(t, err)
		require.Empty(t, repo.allWrites, "失败关闭不得落任何 Extra 键")
	})

	t.Run("balance vs initialBalance mismatch", func(t *testing.T) {
		fakeTH := newTokenHarborFakeTH(t, false)
		upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
		repo := &tokenHarborRepoStub{}
		svc := NewTokenHarborPassService(repo, nil, upstream)
		svc.baseURL = fakeTH.server.URL
		fakeTH.mu.Lock()
		fakeTH.billingPage = `1:{\"userId\":\"x\",\"initialBalance\":1}` + `2:{\"balance\":2,\"lockedBonus\":0}`
		fakeTH.mu.Unlock()

		_, err := svc.ProbeWalletBalance(context.Background(), tokenHarborTestAccount(44))
		require.Error(t, err)
		require.Contains(t, err.Error(), "mismatch")
		require.Empty(t, repo.allWrites, "不一致失败关闭不得落任何 Extra 键")
	})
}

// 会话复用：已有未过期会话不重复登录（沿用文件内既有会话测试模式）。
func TestTokenHarborProbeWalletBalance_SessionReuseNoRelogin(t *testing.T) {
	svc, fakeTH, _ := newTokenHarborTestService(t, false)
	fakeTH.mu.Lock()
	fakeTH.billingPage = testTHWalletBillingRSCZero
	fakeTH.mu.Unlock()
	account := tokenHarborTestAccount(45)
	ctx := context.Background()

	_, err := svc.ProbeWalletBalance(ctx, account)
	require.NoError(t, err)
	loginPosts, billingHits := fakeTH.stats()
	require.Equal(t, 1, loginPosts, "首次探测必须登录一次")
	require.Equal(t, 1, billingHits)

	_, err = svc.ProbeWalletBalance(ctx, account)
	require.NoError(t, err)
	loginPosts, billingHits = fakeTH.stats()
	require.Equal(t, 1, loginPosts, "已有未过期会话必须复用，不得重复登录")
	require.Equal(t, 2, billingHits, "复用时仍会重新拉 billing 页")
	require.Contains(t, fakeTH.lastBillingCookie, "sess-1", "billing 必须使用首次登录的会话 cookie")
}
