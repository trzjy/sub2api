package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// sortedMapKeys map 键名排序（快照 JSON 契约逐键断言用）。
func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- stubs ----

// kiraRecordingUpstream 按方法 + 路径路由响应，并记录请求供断言。
type kiraRecordingUpstream struct {
	requests []*http.Request
	bodies   []string
	handler  func(r *http.Request, body string) (int, string)
}

func (u *kiraRecordingUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	var body string
	if r.Body != nil {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		body = string(bodyBytes)
	}
	u.requests = append(u.requests, r)
	u.bodies = append(u.bodies, body)
	status, respBody := u.handler(r, body)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(respBody)),
		Header:     make(http.Header),
	}, nil
}

func (u *kiraRecordingUpstream) DoWithTLS(r *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, proxyURL, accountID, concurrency)
}

// kiraRepo 记录 UpdateExtra / UpdateCredentials 写入的测试仓库。
type kiraRepo struct {
	AccountRepository
	account       *Account
	extraWrites   []map[string]any
	credWrites    []map[string]any
	credPlatforms []int64
}

func (r *kiraRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return r.account, nil
}

func (r *kiraRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.extraWrites = append(r.extraWrites, updates)
	return nil
}

func (r *kiraRepo) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	r.credPlatforms = append(r.credPlatforms, id)
	r.credWrites = append(r.credWrites, credentials)
	return nil
}

// ---- fixtures ----

// kiraUsageSummaryFixture 按 /tmp/kira_usage_live.json 的真实 summary 形状构造。
const kiraUsageSummaryFixture = `{"summary":{
  "totalRequests":400,
  "totalTokens":36527264,
  "tokensUsedToday":36527264,
  "baseFreeLimit":5000000,
  "freeDailyLimit":6000000,
  "isCheckedInToday":true,
  "checkinBonus":1000000,
  "tokenBalance":50000,
  "vndBalance":0,
  "vndSpentToday":38406.941183,
  "hasActiveFreeModels":true
}}`

func newKiraTestAccount(platform string) *Account {
	return &Account{
		ID:       77,
		Platform: platform,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"account_mode":  AccountModePayG,
			"api_key":       "kira-stale-api-key",
			"base_url":      "https://kiraai.vn/api/v1",
			"kira_jwt":      "stale-jwt",
			"kira_email":    "user@example.com",
			"kira_password": "pw-secret",
		},
	}
}

// ---- tests ----

func TestIsKiraBaseURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"official v1", "https://kiraai.vn/api/v1", true},
		{"case insensitive", "https://KIRAAI.VN/api/v1", true},
		{"mixed case host", "https://KiraAI.Vn/api/v1", true},
		{"www subdomain not exact", "https://www.kiraai.vn/api/v1", false},
		{"empty", "", false},
		{"kimi official", "https://api.kimi.com/coding", false},
		{"deepseek official", "https://api.deepseek.com", false},
		{"moonshot", "https://api.moonshot.cn/v1", false},
		{"volcano", "https://ark.cn-beijing.volces.com/api/v3", false},
		{"similar substring different host", "https://kiraai.vn.evil.example.com/api/v1", false},
		// 外审 F2 补充：子串出现在非主机位置也须精确匹配 host 判定。
		{"suffix host not equal", "https://api.kiraai.vn.evil.example.com/api/v1", false},
		{"subdomain not equal", "https://evil.kiraai.vn.attacker.io/api/v1", false},
		{"bare host", "https://kiraai.vn", true},
		{"port suffix", "https://kiraai.vn:8443/api/v1", true},
		{"user info trick", "https://kiraai.vn@evil.example.com/api/v1", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isKiraBaseURL(tt.url))
		})
	}
}

func TestParseKiraUsageTier_LiveShape(t *testing.T) {
	tier, used, limit, ok := parseKiraUsageTier([]byte(kiraUsageSummaryFixture))
	require.True(t, ok)
	require.Equal(t, "daily", tier.Window)
	require.InDelta(t, 36527264, used, 0.001)
	require.InDelta(t, 6000000, limit, 0.001)
	// 36527264 / 6000000 * 100 —— 允许超 100（不裁剪，与现有 tier 语义一致）。
	require.InDelta(t, 36527264.0/6000000.0*100.0, tier.UsedPercent, 0.001)
	require.Empty(t, tier.ResetAt, "上游无可靠重置口径，必须置空不编时间")
}

func TestParseKiraUsageTier_FallsBackToBasePlusCheckin(t *testing.T) {
	body := `{"summary":{"tokensUsedToday":1200,"baseFreeLimit":5000000,"freeDailyLimit":0,"checkinBonus":1000000,"vndBalance":100}}`
	tier, used, limit, ok := parseKiraUsageTier([]byte(body))
	require.True(t, ok)
	require.Equal(t, "daily", tier.Window)
	require.InDelta(t, 1200, used, 0.001)
	require.InDelta(t, 6000000, limit, 0.001)
	require.InDelta(t, 1200.0/6000000.0*100.0, tier.UsedPercent, 0.001)
}

func TestParseKiraUsageTier_MissingSummary(t *testing.T) {
	_, _, _, ok := parseKiraUsageTier([]byte(`{"error":"unauthorized"}`))
	require.False(t, ok)
}

// 外审 F3：tokensUsedToday / 限额字段缺失、null、解析失败、负数 → 失败关闭。
func TestParseKiraUsageTier_FailClosedOnMissingOrInvalidFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing tokensUsedToday", `{"summary":{"baseFreeLimit":100,"checkinBonus":0}}`},
		{"null tokensUsedToday", `{"summary":{"tokensUsedToday":null,"baseFreeLimit":100,"checkinBonus":0}}`},
		{"string tokensUsedToday", `{"summary":{"tokensUsedToday":"12abc","baseFreeLimit":100,"checkinBonus":0}}`},
		{"negative tokensUsedToday", `{"summary":{"tokensUsedToday":-1,"baseFreeLimit":100,"checkinBonus":0}}`},
		{"missing limit fields", `{"summary":{"tokensUsedToday":10}}`},
		{"null limit fields", `{"summary":{"tokensUsedToday":10,"baseFreeLimit":null,"checkinBonus":null}}`},
		{"negative baseFreeLimit", `{"summary":{"tokensUsedToday":10,"baseFreeLimit":-5,"checkinBonus":0}}`},
		{"negative checkinBonus", `{"summary":{"tokensUsedToday":10,"baseFreeLimit":100,"checkinBonus":-1}}`},
		{"zero freeDailyLimit and missing fallback", `{"summary":{"tokensUsedToday":10,"freeDailyLimit":0}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, ok := parseKiraUsageTier([]byte(tc.body))
			require.False(t, ok, "字段缺失/无效必须失败关闭（ok=false）")
		})
	}
}

// TestFetchKiraUsageWithReauth_401LoginRetry 走真实 httptest：缓存 JWT 401 →
// 登录换新 JWT → 加密通道回写 → 重试成功。
func TestFetchKiraUsageWithReauth_401LoginRetry(t *testing.T) {
	var loginBody string
	usageCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/user/usage":
			usageCalls++
			if r.Header.Get("Authorization") == "Bearer stale-jwt" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"jwt expired"}`))
				return
			}
			if r.Header.Get("Authorization") == "Bearer fresh-jwt" {
				_, _ = w.Write([]byte(kiraUsageSummaryFixture))
				return
			}
			w.WriteHeader(http.StatusForbidden)
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			body, _ := io.ReadAll(r.Body)
			loginBody = string(body)
			var payload map[string]string
			require.NoError(t, json.Unmarshal(body, &payload))
			require.Equal(t, "user@example.com", payload["usernameOrEmail"])
			require.Equal(t, "pw-secret", payload["password"])
			_, _ = w.Write([]byte(`{"token":"fresh-jwt","user":{"id":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	account := newKiraTestAccount(PlatformKimi)
	repo := &kiraRepo{account: account}
	client := &kiraProbeClient{upstream: noopUpstreamForKira(), accountID: account.ID, concurrency: 1}

	body, status, err := fetchKiraUsageWithReauth(context.Background(), client, account, repo, srv.URL+"/api/user/usage", srv.URL+"/api/auth/login")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, string(body), "tokensUsedToday")
	require.Equal(t, 2, usageCalls, "首次 401 后必须恰好重试一次")
	require.JSONEq(t, `{"usernameOrEmail":"user@example.com","password":"pw-secret"}`, loginBody)
	// JWT 必须经 UpdateCredentials（加密写路径）回写。
	require.Len(t, repo.credWrites, 1)
	require.Equal(t, "fresh-jwt", repo.credWrites[0]["kira_jwt"])
	require.Equal(t, account.ID, repo.credPlatforms[0])
}

// TestFetchKiraUsageWithReauth_LoginStillFails 登录失败必须返回明确错误，不兜底。
func TestFetchKiraUsageWithReauth_LoginStillFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad credentials"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	account := newKiraTestAccount(PlatformKimi)
	repo := &kiraRepo{account: account}
	client := &kiraProbeClient{upstream: noopUpstreamForKira(), accountID: account.ID, concurrency: 1}

	_, _, err := fetchKiraUsageWithReauth(context.Background(), client, account, repo, srv.URL+"/api/user/usage", srv.URL+"/api/auth/login")
	require.Error(t, err)
	require.Contains(t, err.Error(), "kira 登录失败")
	require.Contains(t, err.Error(), "401")
	// 失败时不得回写凭据。
	require.Empty(t, repo.credWrites)
}

// TestCNProviderQuotaService_KiraDispatch platform=kimi + base_url=kiraai.vn 的账号
// 必须按 base_url 分发进 Kira 分支，产出 daily 窗口并落 kira_usage_snapshot。
func TestCNProviderQuotaService_KiraDispatch(t *testing.T) {
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" && r.Header.Get("Authorization") == "Bearer stale-jwt" {
			return http.StatusOK, kiraUsageSummaryFixture
		}
		return http.StatusNotFound, `{}`
	}}
	account := newKiraTestAccount(PlatformKimi)
	repo := &kiraRepo{account: account}
	svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

	require.Equal(t, providerKira, resolveCNQuotaProvider(account))

	result, err := svc.QueryUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, result.CredentialValid)
	require.True(t, result.Persisted)
	require.Equal(t, http.StatusOK, result.StatusCode)
	require.Len(t, result.Tiers, 1)
	require.Equal(t, "daily", result.Tiers[0].Window)
	require.InDelta(t, 36527264.0/6000000.0*100.0, result.Tiers[0].UsedPercent, 0.001)
	// 请求必须打到主机根 /api/user/usage（而非 base_url 路径下）。
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/api/user/usage", upstream.requests[0].URL.Path)
	require.Equal(t, "kiraai.vn", upstream.requests[0].URL.Hostname())
	// 快照落 kira_usage_snapshot 单键，§4.3 契约键名逐键断言。
	require.Len(t, repo.extraWrites, 1)
	snapshot, hasSnapshot := repo.extraWrites[0][kiraUsageSnapshotExtraKey]
	require.True(t, hasSnapshot, "必须写 kira_usage_snapshot 快照键")
	snapshotMap, ok := snapshot.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "daily", snapshotMap["window"])
	require.Equal(t, []string{"fetched_at", "limit_tokens", "reset_at", "used_percent", "used_tokens", "window"}, sortedMapKeys(snapshotMap),
		"kira_usage_snapshot 键必须与方案 §4.3 契约逐键一致")
	// reset_at：上游无字段，由"当日窗口"语义推导（越南时区每日零点），RFC3339。
	_, err = time.Parse(time.RFC3339, snapshotMap["reset_at"].(string))
	require.NoError(t, err, "reset_at 必须是可解析的 RFC3339")
	require.Equal(t, kiraNextDailyReset(time.Now().UTC()).UTC().Format(time.RFC3339), snapshotMap["reset_at"])
	// DTO 快照读取输出（§4.3 键名对齐）。
	require.NotNil(t, result.Snapshot)
	require.Equal(t, "daily", result.Snapshot.Window)
	require.NotNil(t, result.Snapshot.UsedTokens)
	require.InDelta(t, 36527264, *result.Snapshot.UsedTokens, 0.001)
	require.NotNil(t, result.Snapshot.LimitTokens)
	require.InDelta(t, 6000000, *result.Snapshot.LimitTokens, 0.001)
	require.NotEmpty(t, result.Snapshot.ResetAt)
	require.NotEmpty(t, result.Snapshot.FetchedAt)
}

// TestCNProviderQuotaService_Kira401TriggersReloginAndFails 端到端：401 → 重登 →
// 重试仍 401 → 失败结果带明确错误、不落快照。
func TestCNProviderQuotaService_Kira401TriggersReloginAndFails(t *testing.T) {
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/user/usage":
			return http.StatusForbidden, `{"error":"forbidden"}`
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			return http.StatusOK, `{"token":"fresh-jwt"}`
		default:
			return http.StatusNotFound, `{}`
		}
	}}
	account := newKiraTestAccount(PlatformDeepseek)
	repo := &kiraRepo{account: account}
	svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

	result, err := svc.QueryUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.False(t, result.CredentialValid)
	require.Contains(t, result.Error, "kira 鉴权失败")
	require.Contains(t, result.Error, "403")
	require.Empty(t, repo.extraWrites, "鉴权失败不得落快照")
	// 重登发生了（登录请求发出，凭据正确）且新 JWT 已回写。
	var loginPayload map[string]string
	require.Len(t, upstream.bodies, 3)
	require.NoError(t, json.Unmarshal([]byte(upstream.bodies[1]), &loginPayload))
	require.Equal(t, "user@example.com", loginPayload["usernameOrEmail"])
	require.Equal(t, "pw-secret", loginPayload["password"])
	require.Len(t, repo.credWrites, 1)
}

// TestCNProviderBalanceService_KiraBalance platform=kimi 的 kiraai.vn 账号余额
// 探测必须走 Kira 分支（VND），不被错打到 moonshot。
func TestCNProviderBalanceService_KiraBalance(t *testing.T) {
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" && r.Header.Get("Authorization") == "Bearer stale-jwt" {
			return http.StatusOK, kiraUsageSummaryFixture
		}
		return http.StatusNotFound, `{}`
	}}
	account := newKiraTestAccount(PlatformKimi)
	repo := &kiraRepo{account: account}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalanceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, result.Persisted)
	require.Equal(t, "VND", result.Currency)
	require.InDelta(t, 0, result.Balance, 0.0001)
	// vnd=0 是免费/token 包账号合法稳态：Unlimited 让周期检测跳过 VND 阈值停调。
	require.True(t, result.Unlimited)
	require.Len(t, result.Balances, 1)
	require.Equal(t, "VND", result.Balances[0].Currency)
	// 请求没打到 moonshot。
	require.Equal(t, "/api/user/usage", upstream.requests[0].URL.Path)
	require.Equal(t, "kiraai.vn", upstream.requests[0].URL.Hostname())
	// 快照按 platform 前缀落 extra（usage 快照 + 余额快照两次写入）。
	// 外审 F4：vnd=0 保留既有 balance_low 标记不动（不写 false）。
	require.Len(t, repo.extraWrites, 2)
	balanceUpdates := kiraBalanceWrite(repo)
	require.NotNil(t, balanceUpdates)
	require.Equal(t, 0.0, balanceUpdates[cnExtraKey(PlatformKimi, cnBalanceExtraSuffixBalance)])
	require.Equal(t, "VND", balanceUpdates[cnExtraKey(PlatformKimi, cnBalanceExtraSuffixCurrency)])
	require.NotContains(t, balanceUpdates, cnExtraKey(PlatformKimi, cnBalanceExtraSuffixLow),
		"vnd=0 必须保留既有 balance_low 标记（单向清除，不得写 false 打摆）")
}

// kiraBalanceWrite 取余额快照写入（含 balance 前缀键的那次 UpdateExtra）。
func kiraBalanceWrite(repo *kiraRepo) map[string]any {
	for _, w := range repo.extraWrites {
		if _, ok := w[cnExtraKey(repo.account.Platform, cnBalanceExtraSuffixBalance)]; ok {
			return w
		}
	}
	return nil
}

// 外审 F4（vnd>0 态）：钱包有余额 → 探测成功写 balance_low=false 清除标记。
func TestCNProviderBalanceService_KiraPositiveVNDClearsLowMarker(t *testing.T) {
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
			return http.StatusOK, `{"summary":{"tokensUsedToday":1,"baseFreeLimit":100,"freeDailyLimit":200,"checkinBonus":0,"vndBalance":58000}}`
		}
		return http.StatusNotFound, `{}`
	}}
	account := newKiraTestAccount(PlatformKimi)
	repo := &kiraRepo{account: account}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalanceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.InDelta(t, 58000, result.Balance, 0.0001)
	require.Equal(t, "VND", result.Currency)
	require.False(t, result.Unlimited, "VND>0 必须参与阈值停调/清除语义")
	balanceUpdates := kiraBalanceWrite(repo)
	require.NotNil(t, balanceUpdates)
	require.Equal(t, false, balanceUpdates[cnExtraKey(PlatformKimi, cnBalanceExtraSuffixLow)],
		"vnd>0 必须清除既有 balance_low 标记")
}

// 外审 F3（balance 链）：vndBalance 缺失/null/负数 → 失败关闭：明确错误、
// Success=false、不落任何快照、不清 balance_low 标记。
func TestCNProviderBalanceService_KiraFailClosedOnInvalidVNDBalance(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing vndBalance", `{"summary":{"tokensUsedToday":1,"baseFreeLimit":100,"checkinBonus":0}}`},
		{"null vndBalance", `{"summary":{"tokensUsedToday":1,"baseFreeLimit":100,"checkinBonus":0,"vndBalance":null}}`},
		{"string vndBalance", `{"summary":{"tokensUsedToday":1,"baseFreeLimit":100,"checkinBonus":0,"vndBalance":"12abc"}}`},
		{"negative vndBalance", `{"summary":{"tokensUsedToday":1,"baseFreeLimit":100,"checkinBonus":0,"vndBalance":-1}}`},
		{"missing summary", `{"error":"unauthorized"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
				if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
					return http.StatusOK, tc.body
				}
				return http.StatusNotFound, `{}`
			}}
			account := newKiraTestAccount(PlatformKimi)
			repo := &kiraRepo{account: account}
			svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

			result, err := svc.QueryBalanceForAccount(context.Background(), account)
			require.NoError(t, err)
			require.False(t, result.Success)
			require.NotEmpty(t, result.Error)
			require.Empty(t, repo.extraWrites, "失败关闭不得落任何快照（含 balance_low 标记）")
		})
	}
}

// 外审 F3（usage 链端到端）：字段无效 → 明确错误、不落快照。
func TestCNProviderQuotaService_KiraFailClosedOnInvalidUsageFields(t *testing.T) {
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
			return http.StatusOK, `{"summary":{"tokensUsedToday":null,"baseFreeLimit":100,"checkinBonus":0}}`
		}
		return http.StatusNotFound, `{}`
	}}
	account := newKiraTestAccount(PlatformKimi)
	repo := &kiraRepo{account: account}
	svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

	result, err := svc.QueryUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.NotEmpty(t, result.Error)
	require.Nil(t, result.Snapshot, "失败关闭路径不产出快照输出")
	require.Empty(t, repo.extraWrites, "失败关闭不得落任何快照")
}

// TestCNProviderBalanceService_KiraPositiveVNDBalance vnd>0 参与阈值语义（非 Unlimited）。
func TestCNProviderBalanceService_KiraPositiveVNDBalance(t *testing.T) {
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
			return http.StatusOK, `{"summary":{"tokensUsedToday":1,"baseFreeLimit":100,"freeDailyLimit":200,"checkinBonus":0,"vndBalance":58000}}`
		}
		return http.StatusNotFound, `{}`
	}}
	account := newKiraTestAccount(PlatformZhipu)
	repo := &kiraRepo{account: account}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalanceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.InDelta(t, 58000, result.Balance, 0.0001)
	require.Equal(t, "VND", result.Currency)
	require.False(t, result.Unlimited, "VND>0 必须参与阈值停调/清除语义")
}

// kiraRealUpstream 直连 httptest 用：真实 http.Client 转发。
type kiraRealUpstream struct{}

func (kiraRealUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return http.DefaultClient.Do(r)
}

func (kiraRealUpstream) DoWithTLS(r *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return kiraRealUpstream{}.Do(r, proxyURL, accountID, concurrency)
}

func noopUpstreamForKira() HTTPUpstream { return kiraRealUpstream{} }
