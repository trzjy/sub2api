package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type helperConcurrencyCacheStub struct {
	mu sync.Mutex

	accountSeq []bool
	userSeq    []bool
	groupSeq   []bool

	accountAcquireCalls int
	userAcquireCalls    int
	groupAcquireCalls   int
	accountReleaseCalls int
	userReleaseCalls    int
	apiKeyTrackCalls    int
	apiKeyReleaseCalls  int
	apiKeyTrackIDs      []int64
}

func (s *helperConcurrencyCacheStub) AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountAcquireCalls++
	if len(s.accountSeq) == 0 {
		return false, nil
	}
	v := s.accountSeq[0]
	s.accountSeq = s.accountSeq[1:]
	return v, nil
}

func (s *helperConcurrencyCacheStub) ReleaseAccountSlot(ctx context.Context, accountID int64, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountReleaseCalls++
	return nil
}

func (s *helperConcurrencyCacheStub) GetAccountConcurrency(ctx context.Context, accountID int64) (int, error) {
	return 0, nil
}

func (s *helperConcurrencyCacheStub) GetAccountConcurrencyBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(accountIDs))
	for _, accountID := range accountIDs {
		out[accountID] = 0
	}
	return out, nil
}

func (s *helperConcurrencyCacheStub) IncrementAccountWaitCount(ctx context.Context, accountID int64, maxWait int) (bool, error) {
	return true, nil
}

func (s *helperConcurrencyCacheStub) DecrementAccountWaitCount(ctx context.Context, accountID int64) error {
	return nil
}

func (s *helperConcurrencyCacheStub) GetAccountWaitingCount(ctx context.Context, accountID int64) (int, error) {
	return 0, nil
}

func (s *helperConcurrencyCacheStub) AcquireUserSlot(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userAcquireCalls++
	if len(s.userSeq) == 0 {
		return false, nil
	}
	v := s.userSeq[0]
	s.userSeq = s.userSeq[1:]
	return v, nil
}

func (s *helperConcurrencyCacheStub) ReleaseUserSlot(ctx context.Context, userID int64, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userReleaseCalls++
	return nil
}

func (s *helperConcurrencyCacheStub) GetUserConcurrency(ctx context.Context, userID int64) (int, error) {
	return 0, nil
}

func (s *helperConcurrencyCacheStub) TrackAPIKeySlot(ctx context.Context, apiKeyID int64, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apiKeyTrackCalls++
	s.apiKeyTrackIDs = append(s.apiKeyTrackIDs, apiKeyID)
	return nil
}

func (s *helperConcurrencyCacheStub) ReleaseAPIKeySlot(ctx context.Context, apiKeyID int64, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apiKeyReleaseCalls++
	return nil
}

func (s *helperConcurrencyCacheStub) GetAPIKeyConcurrencyBatch(ctx context.Context, apiKeyIDs []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(apiKeyIDs))
	for _, apiKeyID := range apiKeyIDs {
		out[apiKeyID] = 0
	}
	return out, nil
}



func (s *helperConcurrencyCacheStub) AcquireUserGroupSlot(ctx context.Context, groupID, userID int64, maxConcurrency int, requestID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groupAcquireCalls++
	if len(s.groupSeq) == 0 {
		return true, nil
	}
	v := s.groupSeq[0]
	s.groupSeq = s.groupSeq[1:]
	return v, nil
}

func (s *helperConcurrencyCacheStub) ReleaseUserGroupSlot(ctx context.Context, groupID, userID int64, requestID string) error {
	return nil
}

func (s *helperConcurrencyCacheStub) GetUserGroupConcurrency(ctx context.Context, groupID, userID int64) (int, error) {
	return 0, nil
}



func (s *helperConcurrencyCacheStub) GetAccountsLoadBatch(ctx context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	out := make(map[int64]*service.AccountLoadInfo, len(accounts))
	for _, acc := range accounts {
		out[acc.ID] = &service.AccountLoadInfo{AccountID: acc.ID}
	}
	return out, nil
}

func (s *helperConcurrencyCacheStub) GetUsersLoadBatch(ctx context.Context, users []service.UserWithConcurrency) (map[int64]*service.UserLoadInfo, error) {
	out := make(map[int64]*service.UserLoadInfo, len(users))
	for _, user := range users {
		out[user.ID] = &service.UserLoadInfo{UserID: user.ID}
	}
	return out, nil
}

func (s *helperConcurrencyCacheStub) CleanupExpiredAccountSlots(ctx context.Context, accountID int64) error {
	return nil
}

func (s *helperConcurrencyCacheStub) CleanupExpiredAccountSlotKeys(ctx context.Context) error {
	return nil
}

func (s *helperConcurrencyCacheStub) CleanupStaleProcessSlots(ctx context.Context, activeRequestPrefix string) error {
	return nil
}

func newHelperTestContext(method, path string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, path, nil)
	return c, rec
}

func validClaudeCodeBodyJSON() []byte {
	return []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"system":[{"text":"You are Claude Code, Anthropic's official CLI for Claude."}],
		"metadata":{"user_id":"user_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_account__session_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}
	}`)
}

func TestSetClaudeCodeClientContext_FastPathAndStrictPath(t *testing.T) {
	t.Run("non_cli_user_agent_sets_false", func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
		c.Request.Header.Set("User-Agent", "curl/8.6.0")

		SetClaudeCodeClientContext(c, validClaudeCodeBodyJSON(), nil)
		require.False(t, service.IsClaudeCodeClient(c.Request.Context()))
	})

	t.Run("cli_non_messages_path_sets_true", func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodGet, "/v1/models")
		c.Request.Header.Set("User-Agent", "claude-cli/1.0.1")

		SetClaudeCodeClientContext(c, nil, nil)
		require.True(t, service.IsClaudeCodeClient(c.Request.Context()))
	})

	t.Run("cli_messages_path_valid_body_sets_true", func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
		c.Request.Header.Set("User-Agent", "claude-cli/1.0.1")
		c.Request.Header.Set("X-App", "claude-code")
		c.Request.Header.Set("anthropic-beta", "message-batches-2024-09-24")
		c.Request.Header.Set("anthropic-version", "2023-06-01")

		SetClaudeCodeClientContext(c, validClaudeCodeBodyJSON(), nil)
		require.True(t, service.IsClaudeCodeClient(c.Request.Context()))
	})

	t.Run("cli_messages_path_invalid_body_sets_false", func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
		c.Request.Header.Set("User-Agent", "claude-cli/1.0.1")
		// 缺少严格校验所需 header + body 字段
		SetClaudeCodeClientContext(c, []byte(`{"model":"x"}`), nil)
		require.False(t, service.IsClaudeCodeClient(c.Request.Context()))
	})
}

func TestSetClaudeCodeClientContext_ReuseParsedRequest(t *testing.T) {
	t.Run("reuse parsed request without body unmarshal", func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
		c.Request.Header.Set("User-Agent", "claude-cli/1.0.1")
		c.Request.Header.Set("X-App", "claude-code")
		c.Request.Header.Set("anthropic-beta", "message-batches-2024-09-24")
		c.Request.Header.Set("anthropic-version", "2023-06-01")

		parsedReq, err := service.ParseGatewayRequest(service.NewRequestBodyRef(validClaudeCodeBodyJSON()), "")
		require.NoError(t, err)

		// body 非法 JSON，如果函数复用 parsedReq 成功则仍应判定为 Claude Code。
		SetClaudeCodeClientContext(c, []byte(`{invalid`), parsedReq)
		require.True(t, service.IsClaudeCodeClient(c.Request.Context()))
	})
}

// TestWaitForSlotWithPingTimeout_AccountAcquire 账号维度等待路径（保留等待：等站点资源释放）。
// 2026-11-11 起本函数只服务账号维度——客户端维度已改为并发满立即拒绝。
func TestWaitForSlotWithPingTimeout_AccountAcquire(t *testing.T) {
	cache := &helperConcurrencyCacheStub{
		accountSeq: []bool{false, true},
	}
	concurrency := service.NewConcurrencyService(cache)
	helper := NewConcurrencyHelper(concurrency, SSEPingFormatNone, 5*time.Millisecond)

	c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
	streamStarted := false
	release, err := helper.waitForSlotWithPingTimeout(c, "account", 101, 2, time.Second, false, &streamStarted, true)
	require.NoError(t, err)
	require.NotNil(t, release)
	require.False(t, streamStarted)
	release()
	require.GreaterOrEqual(t, cache.accountAcquireCalls, 2)
	require.GreaterOrEqual(t, cache.accountReleaseCalls, 1)
}



func TestTryAcquireUserSlotForAPIKey_TracksAPIKeySlot(t *testing.T) {
	cache := &helperConcurrencyCacheStub{
		userSeq: []bool{true},
	}
	concurrency := service.NewConcurrencyService(cache)
	helper := NewConcurrencyHelper(concurrency, SSEPingFormatNone, 5*time.Millisecond)

	release, acquired, err := helper.TryAcquireUserSlotForAPIKey(context.Background(), 202, 3, 77)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NotNil(t, release)
	require.Equal(t, 1, cache.apiKeyTrackCalls)
	require.Equal(t, []int64{77}, cache.apiKeyTrackIDs)

	release()

	require.Equal(t, 1, cache.userReleaseCalls)
	require.Equal(t, 1, cache.apiKeyReleaseCalls)
}




func TestWaitForSlotWithPingTimeout_TimeoutAndStreamPing(t *testing.T) {
	cache := &helperConcurrencyCacheStub{
		accountSeq: []bool{false, false, false},
	}
	concurrency := service.NewConcurrencyService(cache)

	t.Run("timeout_returns_concurrency_error", func(t *testing.T) {
		helper := NewConcurrencyHelper(concurrency, SSEPingFormatNone, 5*time.Millisecond)
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
		streamStarted := false
		release, err := helper.waitForSlotWithPingTimeout(c, "account", 101, 2, 130*time.Millisecond, false, &streamStarted, true)
		require.Nil(t, release)
		var cErr *ConcurrencyError
		require.ErrorAs(t, err, &cErr)
		require.True(t, cErr.IsTimeout)
	})

	// 2026-11-11 契约变更：准入等待阶段不再发 SSE 心跳。
	// 原实现会在等待期间把响应头提交为 200 并写 ping，导致排队超时后只能把 429/503
	// 塞进「已经开始的流」，客户端拿不到真状态码、运维按状态码统计也看不到拒绝。
	// 现在准入阶段保持静默：streamStarted 保持 false，响应体为空，调用方能如实返回状态码。
	t.Run("stream_mode_keeps_admission_silent_before_timeout", func(t *testing.T) {
		helper := NewConcurrencyHelper(concurrency, SSEPingFormatComment, 10*time.Millisecond)
		c, rec := newHelperTestContext(http.MethodPost, "/v1/messages")
		streamStarted := false
		release, err := helper.waitForSlotWithPingTimeout(c, "account", 101, 2, 70*time.Millisecond, true, &streamStarted, true)
		require.Nil(t, release)
		var cErr *ConcurrencyError
		require.ErrorAs(t, err, &cErr)
		require.True(t, cErr.IsTimeout)
		require.False(t, streamStarted, "准入阶段不得把 streamStarted 置真")
		require.Empty(t, rec.Body.String(), "准入阶段不得写任何响应字节")
		require.False(t, c.Writer.Written(), "准入阶段不得提交响应头")
	})
}

func TestWaitForSlotWithPingTimeout_ParentContextCanceled(t *testing.T) {
	cache := &helperConcurrencyCacheStub{
		accountSeq: []bool{false},
	}
	concurrency := service.NewConcurrencyService(cache)
	helper := NewConcurrencyHelper(concurrency, SSEPingFormatNone, 5*time.Millisecond)
	c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
	reqCtx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(reqCtx)
	cancel()

	streamStarted := false
	release, err := helper.waitForSlotWithPingTimeout(c, "account", 101, 2, time.Second, false, &streamStarted, true)
	require.Nil(t, release)
	require.ErrorIs(t, err, context.Canceled)
	var cErr *ConcurrencyError
	require.False(t, errors.As(err, &cErr))
}

func TestWaitForSlotWithPingTimeout_AcquireError(t *testing.T) {
	errCache := &helperConcurrencyCacheStubWithError{
		err: errors.New("redis unavailable"),
	}
	concurrency := service.NewConcurrencyService(errCache)
	helper := NewConcurrencyHelper(concurrency, SSEPingFormatNone, 5*time.Millisecond)
	c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
	streamStarted := false
	release, err := helper.waitForSlotWithPingTimeout(c, "account", 1, 1, 200*time.Millisecond, false, &streamStarted, true)
	require.Nil(t, release)
	require.Error(t, err)
	require.Contains(t, err.Error(), "redis unavailable")
}

func TestAcquireAccountSlotWithWaitTimeout_ImmediateAttemptBeforeBackoff(t *testing.T) {
	cache := &helperConcurrencyCacheStub{
		accountSeq: []bool{false},
	}
	concurrency := service.NewConcurrencyService(cache)
	helper := NewConcurrencyHelper(concurrency, SSEPingFormatNone, 5*time.Millisecond)
	c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
	streamStarted := false

	release, err := helper.AcquireAccountSlotWithWaitTimeout(c, 301, 1, 30*time.Millisecond, false, &streamStarted)
	require.Nil(t, release)
	var cErr *ConcurrencyError
	require.ErrorAs(t, err, &cErr)
	require.True(t, cErr.IsTimeout)
	require.GreaterOrEqual(t, cache.accountAcquireCalls, 1)
}

type helperConcurrencyCacheStubWithError struct {
	helperConcurrencyCacheStub
	err error
}

func (s *helperConcurrencyCacheStubWithError) AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
	return false, s.err
}

func TestSetClaudeCodeClientContext_ParsedRequestProbeWithoutSystemPrompt(t *testing.T) {
	c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
	c.Request.Header.Set("User-Agent", "claude-cli/2.1.260 (external, cli)")

	// The hot path reuses ParsedRequest instead of re-parsing the body; the probe
	// marker must survive that projection or the exemption only works cold.
	parsed := &service.ParsedRequest{Model: "claude-sonnet-4-5", MaxTokens: 1}
	SetClaudeCodeClientContext(c, nil, parsed)
	require.True(t, service.IsClaudeCodeClient(c.Request.Context()))

	c2, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
	c2.Request.Header.Set("User-Agent", "claude-cli/2.1.260 (external, cli)")
	SetClaudeCodeClientContext(c2, nil, &service.ParsedRequest{Model: "claude-sonnet-4-5", MaxTokens: 64})
	require.False(t, service.IsClaudeCodeClient(c2.Request.Context()))
}

// TestAcquireUserSlot_ImmediateAcquire 立即拿到槽位时正常返回并登记 API Key 槽位。
func TestAcquireUserSlot_ImmediateAcquire(t *testing.T) {
	cache := &helperConcurrencyCacheStub{
		userSeq: []bool{true},
	}
	concurrency := service.NewConcurrencyService(cache)
	helper := NewConcurrencyHelper(concurrency, SSEPingFormatNone, 5*time.Millisecond)
	c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 77})

	release, err := helper.AcquireUserSlot(c, 202, 3)
	require.NoError(t, err)
	require.NotNil(t, release)
	require.Equal(t, 1, cache.userAcquireCalls)
	require.Equal(t, 1, cache.apiKeyTrackCalls)
	require.Equal(t, []int64{77}, cache.apiKeyTrackIDs)

	release()
	require.Equal(t, 1, cache.userReleaseCalls)
	require.Equal(t, 1, cache.apiKeyReleaseCalls)
}

// TestAcquireUserSlot_RejectsImmediatelyWhenFull 契约锁定（2026-11-11 用户裁定）：
// 客户端维度并发已满时**立即拒绝**，不排队、不等待、不写任何响应字节。
// 排队 30 秒是错误设计——并发上限的定义就是"同时最多 N 个"，第 N+1 个进来时
// 前面请求的完成时间不可控，等的是一个无法承诺的时间。
func TestAcquireUserSlot_RejectsImmediatelyWhenFull(t *testing.T) {
	cache := &helperConcurrencyCacheStub{
		userSeq: []bool{false},
	}
	concurrency := service.NewConcurrencyService(cache)
	helper := NewConcurrencyHelper(concurrency, SSEPingFormatNone, 5*time.Millisecond)
	c, rec := newHelperTestContext(http.MethodPost, "/v1/messages")

	start := time.Now()
	release, err := helper.AcquireUserSlot(c, 202, 3)
	elapsed := time.Since(start)

	require.Nil(t, release)
	var cErr *ConcurrencyError
	require.ErrorAs(t, err, &cErr)
	require.Equal(t, "user", cErr.SlotType)
	require.Equal(t, 3, cErr.Limit)
	require.False(t, cErr.IsTimeout, "立即拒绝不是超时")
	require.Less(t, elapsed, 50*time.Millisecond, "必须立即返回，不得排队等待")
	require.Equal(t, 1, cache.userAcquireCalls, "只尝试占用一次")
	require.Empty(t, rec.Body.String(), "拒绝前不得写响应体")
	require.False(t, c.Writer.Written(), "拒绝前不得提交响应头")
}

// TestAcquireUserGroupSlot_RejectsImmediatelyWhenFull 订阅分组维度同上。
func TestAcquireUserGroupSlot_RejectsImmediatelyWhenFull(t *testing.T) {
	cache := &helperConcurrencyCacheStub{
		groupSeq: []bool{false},
	}
	concurrency := service.NewConcurrencyService(cache)
	helper := NewConcurrencyHelper(concurrency, SSEPingFormatNone, 5*time.Millisecond)
	c, rec := newHelperTestContext(http.MethodPost, "/v1/messages")

	start := time.Now()
	release, err := helper.AcquireUserGroupSlot(c, 57, 202, 2)
	elapsed := time.Since(start)

	require.Nil(t, release)
	var cErr *ConcurrencyError
	require.ErrorAs(t, err, &cErr)
	require.Equal(t, "group", cErr.SlotType)
	require.Equal(t, 2, cErr.Limit)
	require.False(t, cErr.IsTimeout)
	require.Less(t, elapsed, 50*time.Millisecond, "必须立即返回，不得排队等待")
	require.Empty(t, rec.Body.String())
	require.False(t, c.Writer.Written())
}
