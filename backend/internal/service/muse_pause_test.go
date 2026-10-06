package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAllowedSchedulingThresholdPlatformsContainsMuse 锚定 muse 已注册进
// AllowedSchedulingThresholdPlatforms：前端 settings.ts（muse-6）已注册 muse 阈值卡，
// 后端不注册则保存被契约拒绝（前后端不闭合）。muse-7 缺口一。
func TestAllowedSchedulingThresholdPlatformsContainsMuse(t *testing.T) {
	t.Parallel()
	require.Contains(t, AllowedSchedulingThresholdPlatforms, PlatformMuse,
		"muse 必须进入阈值白名单，否则前端阈值卡保存被契约拒绝")
	// 禁区：不得动 other/openai/minimax/antigravity/grok 既有成员。
	for _, p := range []string{PlatformOpenAI, PlatformAnthropic, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformMiniMax, PlatformCodeBuddy} {
		require.Contains(t, AllowedSchedulingThresholdPlatforms, p, "既有阈值白名单成员不得因 muse 接入而丢失：%s", p)
	}
}

// musePauseRepoStub 是仅实现 SetTempUnschedulable 的 AccountRepository 桩：
// 内嵌接口（其余方法运行期不被 getMuseUsage 触发），并以「只延长不缩短」语义
// 模拟 DB 守卫（temp_unschedulable_until < $1），用于断言他因暂停不被覆盖。
type musePauseRepoStub struct {
	AccountRepository
	mu           sync.Mutex
	calls        int
	storedUntil  *time.Time
	storedReason string
}

func (r *musePauseRepoStub) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	// 模拟 DB 守卫：NULL 或现有值更早才写（只延长不缩短）。
	if r.storedUntil == nil || until.After(*r.storedUntil) {
		u := until
		r.storedUntil = &u
		r.storedReason = reason
	}
	return nil
}

// TestGetMuseUsage_PersistTempUnschedulableWhenPaused 锚定 muse-7 缺口二桥接：
// 暂停判定成立 → 写 temp_unschedulable_until，值 = 所有已打满窗口 resetsAt 最大值。
func TestGetMuseUsage_PersistTempUnschedulableWhenPaused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2030-01-01T00:00:00Z"},"weekly":{"status":"exhausted","percent":100,"resetsAt":"2030-06-01T00:00:00Z"},"monthly":{"status":"ok","percent":0,"resetsAt":"2030-01-01T00:00:00Z"}}}`))
	}))
	defer srv.Close()

	repo := &musePauseRepoStub{}
	s := &AccountUsageService{
		museQuotaFetcher: NewMuseQuotaFetcher(nil, nil),
		cache:            NewUsageCache(),
		accountRepo:      repo,
	}
	acct := &Account{ID: 99, Platform: PlatformMuse, Credentials: map[string]any{"api_key": "k", "base_url": srv.URL}}

	u, err := s.getMuseUsage(context.Background(), acct)
	require.NoError(t, err)
	require.NotNil(t, u.MuseUsage)
	require.True(t, u.MuseUsage.Paused, "weekly 打满应判定暂停")

	require.Equal(t, 1, repo.calls, "暂停时应恰好写一次 temp_unschedulable")
	require.NotNil(t, repo.storedUntil)
	require.Equal(t, "2030-06-01T00:00:00Z", repo.storedUntil.UTC().Format(time.RFC3339),
		"写入的恢复时间必须是 max resetsAt")
	require.Equal(t, museUsageTempUnschedulableReason, repo.storedReason)
}

// TestGetMuseUsage_NoPersistWhenNotPaused 锚定：未打满（Paused=false）一律不写。
func TestGetMuseUsage_NoPersistWhenNotPaused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":0},"weekly":{"status":"ok","percent":0},"monthly":{"status":"ok","percent":0}}}`))
	}))
	defer srv.Close()

	repo := &musePauseRepoStub{}
	s := &AccountUsageService{
		museQuotaFetcher: NewMuseQuotaFetcher(nil, nil),
		cache:            NewUsageCache(),
		accountRepo:      repo,
	}
	acct := &Account{ID: 100, Platform: PlatformMuse, Credentials: map[string]any{"api_key": "k", "base_url": srv.URL}}

	u, err := s.getMuseUsage(context.Background(), acct)
	require.NoError(t, err)
	require.NotNil(t, u.MuseUsage)
	require.False(t, u.MuseUsage.Paused, "全 ok 不得判定暂停")
	require.Equal(t, 0, repo.calls, "未打满不得写 temp_unschedulable")
	require.Nil(t, repo.storedUntil)
}

// TestGetMuseUsage_DoesNotShortenExistingOtherCausePause 锚定「他因暂停不碰」：
// 账号已有他因暂停（更晚的 until），muse 用量侧只延长不缩短，不得覆盖/缩短他因暂停。
func TestGetMuseUsage_DoesNotShortenExistingOtherCausePause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// muse 算得恢复时间 2030-06-01（早于他因暂停 2035-01-01）
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":10},"weekly":{"status":"exhausted","percent":100,"resetsAt":"2030-06-01T00:00:00Z"},"monthly":{"status":"ok","percent":0}}}`))
	}))
	defer srv.Close()

	otherUntil := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	repo := &musePauseRepoStub{storedUntil: &otherUntil, storedReason: "other:cause"}
	s := &AccountUsageService{
		museQuotaFetcher: NewMuseQuotaFetcher(nil, nil),
		cache:            NewUsageCache(),
		accountRepo:      repo,
	}
	acct := &Account{ID: 101, Platform: PlatformMuse, Credentials: map[string]any{"api_key": "k", "base_url": srv.URL}}

	_, err := s.getMuseUsage(context.Background(), acct)
	require.NoError(t, err)
	require.Equal(t, 1, repo.calls, "muse 仍会尝试写（SetTempUnschedulable 被调用）")
	// 守卫生效：他因暂停（更晚）未被覆盖/缩短。
	require.NotNil(t, repo.storedUntil)
	require.Equal(t, "2035-01-01T00:00:00Z", repo.storedUntil.UTC().Format(time.RFC3339),
		"他因暂停的更晚 until 必须保持，muse 不得缩短")
	require.Equal(t, "other:cause", repo.storedReason, "他因暂停原因不得被 muse 覆盖")
}
