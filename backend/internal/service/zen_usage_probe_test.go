package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件锚定 ZB-T1（usage 服务三窗口接线放开，凭证级 opt-in）：
// getUsageForAccount 的 muse 分支条件由 `Platform == PlatformMuse` 放开为
// `Platform == PlatformMuse || GetCredential("usage_probe") == "opencode_zen"`。
// 其余逻辑（getMuseUsage / MuseQuotaFetcher / repo 守卫）零改动、不碰。
// 约定：无 //go:build tag，与 muse_pause_test.go 一致。

// TestGetUsageForAccount_DeepseekOptInZenProbe_RoutesToMuseUsage 锚定 ①：
// 带 usage_probe=opencode_zen 的 deepseek(apikey) 账号，base_url 指向
// opencode.ai/zen/go 网关，进入 getMuseUsage 分支（而非原“不支持 usage 查询”错误），
// 且经 ZB-T1b 放开 CanFetch 后真实发起 /usage 拉取并填充三窗口。
//
// ZB-T1b：MuseQuotaFetcher.CanFetch 已与 PlatformMuse 同条件放开 opt-in，故此处
// 断言由「hits==0 早返回」升级为「hits>0 真实拉取 + MuseUsage 被填充」。
func TestGetUsageForAccount_DeepseekOptInZenProbe_RoutesToMuseUsage(t *testing.T) {
	t.Parallel()

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":0},"weekly":{"status":"ok","percent":0},"monthly":{"status":"ok","percent":0}}}`))
	}))
	defer srv.Close()

	s := &AccountUsageService{
		museQuotaFetcher: NewMuseQuotaFetcher(nil, nil),
		cache:            NewUsageCache(),
	}

	acct := &Account{
		ID:       201,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "k",
			"base_url":     srv.URL, // https://opencode.ai/zen/go/v1 的生产等价（本用例用桩）
			"usage_probe":  "opencode_zen",
		},
	}

	u, err := s.getUsageForAccount(context.Background(), acct, false)
	// 路由到 getMuseUsage 后，对 apikey 类 deepseek 账号这是唯一的非错误返回路径。
	require.NoError(t, err, "opt-in deepseek 必须进入 muse 分支而非返回不支持错误")
	require.NotNil(t, u)
	require.NotNil(t, u.UpdatedAt, "getMuseUsage 返回形态须带 UpdatedAt")
	// ZB-T1b：CanFetch 已放开 opt-in → 真实拉取并填充三窗口。
	require.NotNil(t, u.MuseUsage, "opt-in deepseek 经 ZB-T1b 放开后须填充 MuseUsage")
	require.Equal(t, 1, hits, "opt-in deepseek 须真实发起一次 /usage 拉取")
}

// TestGetUsageForAccount_DeepseekNonOptIn_Unchanged 锚定 ②：
// 未 opt-in 的 deepseek(apikey) 账号行为零变化——仍走原余额分支并回报
// “account type ... does not support usage query” 错误（与放开前完全一致）。
// 这是「其他账号行为零变化」的回归锚点。
func TestGetUsageForAccount_DeepseekNonOptIn_Unchanged(t *testing.T) {
	t.Parallel()

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
	}))
	defer srv.Close()

	s := &AccountUsageService{
		museQuotaFetcher: NewMuseQuotaFetcher(nil, nil),
		cache:            NewUsageCache(),
	}

	acct := &Account{
		ID:       202,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "k",
			"base_url": srv.URL,
			// 注意：无 usage_probe，未 opt-in。
		},
	}

	u, err := s.getUsageForAccount(context.Background(), acct, false)
	require.Error(t, err, "未 opt-in 的 deepseek 必须保持原余额分支错误")
	require.Nil(t, u)
	require.Contains(t, err.Error(), "does not support usage query")
	require.Equal(t, 0, hits, "未 opt-in 不得触碰 muse 拉取端点")
}

// TestGetUsageForAccount_MusePlatform_UnchangedByZenOptIn 锚定 ③：
// muse 平台账号（无论是否带 usage_probe）行为不变——仍走 getMuseUsage 并实际拉取
// 三窗口。既有 muse_pause_test.go 四用例亦不得回归；此处额外显式锁定 muse 路由完好。
func TestGetUsageForAccount_MusePlatform_UnchangedByZenOptIn(t *testing.T) {
	t.Parallel()

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":10},"weekly":{"status":"exhausted","percent":100,"resetsAt":"2030-06-01T00:00:00Z"},"monthly":{"status":"ok","percent":0}}}`))
	}))
	defer srv.Close()

	s := &AccountUsageService{
		museQuotaFetcher: NewMuseQuotaFetcher(nil, nil),
		cache:            NewUsageCache(),
	}

	acct := &Account{
		ID:       203,
		Platform: PlatformMuse,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":     "k",
			"base_url":    srv.URL,
			"usage_probe": "opencode_zen", // 即使带 opt-in，muse 平台仍走既有 muse 全链
		},
	}

	u, err := s.getUsageForAccount(context.Background(), acct, false)
	require.NoError(t, err)
	require.NotNil(t, u)
	require.NotNil(t, u.MuseUsage, "muse 平台须实际拉取并填充三窗口")
	require.True(t, u.MuseUsage.Paused, "weekly 打满须判定暂停")
	require.Equal(t, 1, hits, "muse 平台须恰好拉取一次 /usage")
}
