package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func museTime(t string) time.Time {
	tt, _ := time.Parse(time.RFC3339, t)
	return tt
}

// TestComputeMusePauseAndRecovery 锚定外审-4 收敛的字段映射：
// 暂停判定 = 任一窗口 status != "ok" 且未到期；恢复 = 所有已打满窗口 resetsAt 最大值。
func TestComputeMusePauseAndRecovery(t *testing.T) {
	now := museTime("2026-10-02T12:00:00Z")
	future := now.Add(time.Hour)
	f2 := now.Add(2 * time.Hour)
	past := now.Add(-time.Hour)

	// 单窗口打满 → 暂停，恢复 = 该窗口 resetsAt
	info := &MuseUsageInfo{
		Rolling: &MuseWindow{Status: "exhausted", Percent: 100, ResetsAt: &future},
		Weekly:  &MuseWindow{Status: "ok", Percent: 0},
		Monthly: &MuseWindow{Status: "ok", Percent: 0},
	}
	paused, until := computeMusePauseAndRecovery(info, now)
	if !paused {
		t.Fatal("expected paused for single filled window")
	}
	if until == nil || !until.Equal(future) {
		t.Fatalf("expected recovery=%v got %v", future, until)
	}

	// 三窗口同时打满 → 恢复 = 最大 resetsAt
	info = &MuseUsageInfo{
		Rolling: &MuseWindow{Status: "exhausted", Percent: 100, ResetsAt: &future},
		Weekly:  &MuseWindow{Status: "exhausted", Percent: 100, ResetsAt: &f2},
		Monthly: &MuseWindow{Status: "exhausted", Percent: 100, ResetsAt: &future},
	}
	paused, until = computeMusePauseAndRecovery(info, now)
	if !paused {
		t.Fatal("expected paused for three filled windows")
	}
	if until == nil || !until.Equal(f2) {
		t.Fatalf("expected max recovery=%v got %v", f2, until)
	}

	// 全 ok → 不暂停，恢复 nil
	info = &MuseUsageInfo{
		Rolling: &MuseWindow{Status: "ok", Percent: 0},
		Weekly:  &MuseWindow{Status: "ok", Percent: 0},
		Monthly: &MuseWindow{Status: "ok", Percent: 0},
	}
	paused, until = computeMusePauseAndRecovery(info, now)
	if paused {
		t.Fatal("expected not paused for all-ok")
	}
	if until != nil {
		t.Fatalf("expected nil recovery got %v", until)
	}

	// 时间恢复语义：打满但 resetsAt 已过期 → 视为已重置，不再暂停
	info = &MuseUsageInfo{
		Rolling: &MuseWindow{Status: "exhausted", Percent: 100, ResetsAt: &past},
		Weekly:  &MuseWindow{Status: "ok", Percent: 0},
		Monthly: &MuseWindow{Status: "ok", Percent: 0},
	}
	paused, until = computeMusePauseAndRecovery(info, now)
	if paused {
		t.Fatal("expired filled window must not pause (time recovery)")
	}
	if until != nil {
		t.Fatalf("expected nil recovery for expired window got %v", until)
	}

	// 恰满 percent=100 但 status=ok → 不暂停（percent 不是暂停判据，status 才是）
	info = &MuseUsageInfo{Rolling: &MuseWindow{Status: "ok", Percent: 100}}
	paused, _ = computeMusePauseAndRecovery(info, now)
	if paused {
		t.Fatal("percent=100 with status ok must not pause")
	}

	// 恰满 percent=100 且 status 非 ok → 暂停
	info = &MuseUsageInfo{Rolling: &MuseWindow{Status: "exhausted", Percent: 100, ResetsAt: &future}}
	paused, _ = computeMusePauseAndRecovery(info, now)
	if !paused {
		t.Fatal("percent=100 with non-ok status must pause")
	}
}

// TestParseMuseWindow_PercentMapping 锚定 percent = 已用比例的值映射（含恰满 100）。
func TestParseMuseWindow_PercentMapping(t *testing.T) {
	cases := []struct {
		raw    museUsageWindowRaw
		expect float64
	}{
		{museUsageWindowRaw{Status: "ok", Percent: 0}, 0},
		{museUsageWindowRaw{Status: "ok", Percent: 42}, 42},
		{museUsageWindowRaw{Status: "ok", Percent: 100}, 100},
	}
	for _, c := range cases {
		w := parseMuseWindow(c.raw)
		if w.Percent != c.expect {
			t.Fatalf("percent mapping wrong: got %v want %v", w.Percent, c.expect)
		}
	}
	// resetsAt 解析
	w := parseMuseWindow(museUsageWindowRaw{Status: "ok", Percent: 0, ResetsAt: "2026-10-02T13:00:00Z"})
	if w.ResetsAt == nil || !w.ResetsAt.Equal(museTime("2026-10-02T13:00:00Z")) {
		t.Fatalf("resetsAt parse wrong: %v", w.ResetsAt)
	}
	// 非法 resetsAt 留空不报错
	w2 := parseMuseWindow(museUsageWindowRaw{Status: "ok", Percent: 0, ResetsAt: "garbage"})
	if w2.ResetsAt != nil {
		t.Fatalf("invalid resetsAt should be nil, got %v", w2.ResetsAt)
	}
}

func TestMuseQuotaFetcher_CanFetch(t *testing.T) {
	f := NewMuseQuotaFetcher(nil, nil)
	if f.CanFetch(&Account{Platform: PlatformAntigravity, Credentials: map[string]any{"api_key": "x"}}) {
		t.Fatal("non-muse account must not fetch")
	}
	if f.CanFetch(&Account{Platform: PlatformMuse}) {
		t.Fatal("muse account without api_key must not fetch")
	}
	if !f.CanFetch(&Account{Platform: PlatformMuse, Credentials: map[string]any{"api_key": "x"}}) {
		t.Fatal("muse account with api_key must fetch")
	}
}

// TestMuseQuotaFetcher_CanFetch_OptInZenProbe 锚定 ZB-T1b：凭证级 opt-in
// （usage_probe == "opencode_zen"）放开 CanFetch 守卫，与 PlatformMuse 同条件。
// 仅覆盖 CanFetch 逻辑本身；既有三锚点（antigravity=false / muse 无 api_key=false /
// muse 有 api_key=true）在本用例之上零改动、不回归。
func TestMuseQuotaFetcher_CanFetch_OptInZenProbe(t *testing.T) {
	f := NewMuseQuotaFetcher(nil, nil)

	// ① deepseek 平台 + usage_probe=opencode_zen + api_key → 放行
	if !f.CanFetch(&Account{
		Platform:    PlatformDeepseek,
		Credentials: map[string]any{"usage_probe": "opencode_zen", "api_key": "x"},
	}) {
		t.Fatal("deepseek opt-in (usage_probe=opencode_zen) with api_key must fetch")
	}

	// ② deepseek 平台 + 未 opt-in（无 usage_probe）→ 不放行
	if f.CanFetch(&Account{
		Platform:    PlatformDeepseek,
		Credentials: map[string]any{"api_key": "x"},
	}) {
		t.Fatal("deepseek non opt-in must not fetch")
	}

	// ③ deepseek 平台 + opt-in 但无 api_key → 不放行（api_key 非空要求保留）
	if f.CanFetch(&Account{
		Platform:    PlatformDeepseek,
		Credentials: map[string]any{"usage_probe": "opencode_zen"},
	}) {
		t.Fatal("deepseek opt-in without api_key must not fetch")
	}
}

// TestMuseQuotaFetcher_FetchQuota_Success 锚定三窗口映射、暂停与 max(resetsAt) 恢复。
func TestMuseQuotaFetcher_FetchQuota_Success(t *testing.T) {
	body := `{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":"2026-10-02T13:00:00Z"},"weekly":{"status":"ok","percent":10,"resetsAt":"2026-10-03T00:00:00Z"},"monthly":{"status":"full","percent":100,"resetsAt":"2026-11-01T00:00:00Z"}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("missing/invalid Authorization header")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := NewMuseQuotaFetcher(nil, nil)
	acct := &Account{ID: 1, Platform: PlatformMuse, Credentials: map[string]any{"api_key": "secret", "base_url": srv.URL}}
	res, err := f.FetchQuota(context.Background(), acct, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.UsageInfo == nil || res.UsageInfo.MuseUsage == nil {
		t.Fatal("missing muse usage in result")
	}
	mu := res.UsageInfo.MuseUsage
	if mu.Rolling == nil || mu.Rolling.Percent != 0 {
		t.Fatalf("rolling percent wrong: %v", mu.Rolling)
	}
	if mu.Weekly == nil || mu.Weekly.Percent != 10 {
		t.Fatalf("weekly percent wrong: %v", mu.Weekly)
	}
	if mu.Monthly == nil || mu.Monthly.Percent != 100 {
		t.Fatalf("monthly percent wrong: %v", mu.Monthly)
	}
	if !mu.Paused {
		t.Fatal("expected paused (monthly full)")
	}
	if mu.UnschedulableUntil == nil || !mu.UnschedulableUntil.Equal(museTime("2026-11-01T00:00:00Z")) {
		t.Fatalf("expected recovery=2026-11-01T00:00:00Z got %v", mu.UnschedulableUntil)
	}
}

// TestMuseQuotaFetcher_FetchQuota_HTTPError 上游非 2xx → 失败关闭（不 fallback）。
func TestMuseQuotaFetcher_FetchQuota_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	f := NewMuseQuotaFetcher(nil, nil)
	acct := &Account{ID: 2, Platform: PlatformMuse, Credentials: map[string]any{"api_key": "x", "base_url": srv.URL}}
	if _, err := f.FetchQuota(context.Background(), acct, ""); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

// TestMuseQuotaFetcher_FetchQuota_Unreachable 上游不可达 → 失败关闭。
func TestMuseQuotaFetcher_FetchQuota_Unreachable(t *testing.T) {
	f := NewMuseQuotaFetcher(nil, nil)
	acct := &Account{ID: 3, Platform: PlatformMuse, Credentials: map[string]any{"api_key": "x", "base_url": "http://127.0.0.1:1"}}
	if _, err := f.FetchQuota(context.Background(), acct, ""); err == nil {
		t.Fatal("expected error on unreachable upstream")
	}
}

// TestGetMuseUsage_Wiring 锚定接线：缓存命中（singleflight）不重复打上游。
func TestGetMuseUsage_Wiring(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":"2026-10-02T13:00:00Z"},"weekly":{"status":"ok","percent":0},"monthly":{"status":"ok","percent":0}}}`))
	}))
	defer srv.Close()

	s := &AccountUsageService{
		museQuotaFetcher: NewMuseQuotaFetcher(nil, nil),
		cache:            NewUsageCache(),
	}
	acct := &Account{ID: 10, Platform: PlatformMuse, Credentials: map[string]any{"api_key": "s", "base_url": srv.URL}}

	u1, err := s.getMuseUsage(context.Background(), acct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u1.MuseUsage == nil || u1.MuseUsage.Paused {
		t.Fatal("muse usage not populated correctly")
	}
	if hits != 1 {
		t.Fatalf("expected 1 upstream hit, got %d", hits)
	}

	// 第二次调用应命中缓存，不再打上游
	if _, err := s.getMuseUsage(context.Background(), acct); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected cache hit (no extra upstream call), got %d", hits)
	}
}

// TestGetMuseUsage_NoAPIKey muse 账号无 api_key → CanFetch 失败，返回空 UsageInfo 不打上游。
func TestGetMuseUsage_NoAPIKey(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	s := &AccountUsageService{
		museQuotaFetcher: NewMuseQuotaFetcher(nil, nil),
		cache:            NewUsageCache(),
	}
	acct := &Account{ID: 11, Platform: PlatformMuse, Credentials: map[string]any{"base_url": srv.URL}}
	u, err := s.getMuseUsage(context.Background(), acct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.MuseUsage != nil {
		t.Fatal("no-api_key account must not populate muse usage")
	}
	if hits != 0 {
		t.Fatalf("expected no upstream call, got %d", hits)
	}
}

// TestGetUsageForAccount_MuseRouting 锚定 getUsageForAccount 的 PlatformMuse 分支路由到 MuseQuotaFetcher。
func TestGetUsageForAccount_MuseRouting(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":0},"weekly":{"status":"ok","percent":0},"monthly":{"status":"ok","percent":0}}}`))
	}))
	defer srv.Close()

	s := &AccountUsageService{
		museQuotaFetcher: NewMuseQuotaFetcher(nil, nil),
		cache:            NewUsageCache(),
	}
	acct := &Account{ID: 20, Platform: PlatformMuse, Credentials: map[string]any{"api_key": "s", "base_url": srv.URL}}
	u, err := s.getUsageForAccount(context.Background(), acct, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.MuseUsage == nil {
		t.Fatal("muse routing did not populate muse usage")
	}
	if hits != 1 {
		t.Fatalf("expected 1 upstream hit, got %d", hits)
	}
}
