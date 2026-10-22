package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

const gatewayStreamHeartbeatBytesKey = "gateway_stream_heartbeat_bytes"

func recordGatewayStreamHeartbeat(c *gin.Context, written int) {
	if c == nil || written <= 0 {
		return
	}
	total, _ := c.Get(gatewayStreamHeartbeatBytesKey)
	bytes, _ := total.(int)
	c.Set(gatewayStreamHeartbeatBytesKey, bytes+written)
}

func gatewayStreamHasOnlyHeartbeats(c *gin.Context) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	value, ok := c.Get(gatewayStreamHeartbeatBytesKey)
	if !ok {
		return false
	}
	heartbeatBytes, _ := value.(int)
	return heartbeatBytes > 0 && c.Writer.Size() == heartbeatBytes
}

// claudeCodeValidator is a singleton validator for Claude Code client detection
var claudeCodeValidator = service.NewClaudeCodeValidator()

// SetClaudeCodeClientContext 检查请求是否来自 Claude Code 客户端，并设置到 context 中
// 返回更新后的 context
func SetClaudeCodeClientContext(c *gin.Context, body []byte, parsedReq *service.ParsedRequest) {
	if c == nil || c.Request == nil {
		return
	}
	ua := c.GetHeader("User-Agent")
	// Fast path：非 Claude CLI UA 直接判定 false，避免热路径二次 JSON 反序列化。
	if !claudeCodeValidator.ValidateUserAgent(ua) {
		ctx := service.SetClaudeCodeClient(c.Request.Context(), false)
		c.Request = c.Request.WithContext(ctx)
		return
	}

	isClaudeCode := false
	if !strings.Contains(c.Request.URL.Path, "messages") {
		// 与 Validate 行为一致：非 messages 路径 UA 命中即可视为 Claude Code 客户端。
		isClaudeCode = true
	} else {
		// 仅在确认为 Claude CLI 且 messages 路径时再做 body 解析。
		bodyMap := claudeCodeBodyMapFromParsedRequest(parsedReq)
		if bodyMap == nil && len(body) > 0 {
			_ = json.Unmarshal(body, &bodyMap)
		}
		isClaudeCode = claudeCodeValidator.Validate(c.Request, bodyMap)
	}

	// 更新 request context
	ctx := service.SetClaudeCodeClient(c.Request.Context(), isClaudeCode)

	// 仅在确认为 Claude Code 客户端时提取版本号写入 context
	if isClaudeCode {
		if version := claudeCodeValidator.ExtractVersion(ua); version != "" {
			ctx = service.SetClaudeCodeVersion(ctx, version)
		}
	}

	c.Request = c.Request.WithContext(ctx)
}

func claudeCodeBodyMapFromParsedRequest(parsedReq *service.ParsedRequest) map[string]any {
	if parsedReq == nil {
		return nil
	}
	bodyMap := map[string]any{
		"model": parsedReq.Model,
	}
	// 探测识别（max_tokens=1）需要看到该字段，复用已解析请求时一并带上。
	if parsedReq.MaxTokens > 0 {
		bodyMap["max_tokens"] = parsedReq.MaxTokens
	}
	if parsedReq.HasSystem {
		if system, ok := parsedReq.SystemValue(); ok {
			bodyMap["system"] = system
		} else {
			bodyMap["system"] = nil
		}
	}
	if parsedReq.MetadataUserID != "" {
		bodyMap["metadata"] = map[string]any{"user_id": parsedReq.MetadataUserID}
	}
	return bodyMap
}

// 并发槽位等待相关常量
//
// 性能优化说明：
// 原实现使用固定间隔（100ms）轮询并发槽位，存在以下问题：
// 1. 高并发时频繁轮询增加 Redis 压力
// 2. 固定间隔可能导致多个请求同时重试（惊群效应）
//
// 新实现使用指数退避 + 抖动算法：
// 1. 初始退避 100ms，每次乘以 1.5，最大 2s
// 2. 添加 ±20% 的随机抖动，分散重试时间点
// 3. 减少 Redis 压力，避免惊群效应
const (
	// maxConcurrencyWait 等待并发槽位的最大时间
	maxConcurrencyWait = 30 * time.Second
	// defaultPingInterval 流式响应等待时发送 ping 的默认间隔
	defaultPingInterval = 10 * time.Second
	// initialBackoff 初始退避时间
	initialBackoff = 100 * time.Millisecond
	// backoffMultiplier 退避时间乘数（指数退避）
	backoffMultiplier = 1.5
	// maxBackoff 最大退避时间
	maxBackoff = 2 * time.Second
)

// SSEPingFormat defines the format of SSE ping events for different platforms
type SSEPingFormat string

const (
	// SSEPingFormatClaude is the Claude/Anthropic SSE ping format
	SSEPingFormatClaude SSEPingFormat = "data: {\"type\": \"ping\"}\n\n"
	// SSEPingFormatNone indicates no ping should be sent (e.g., OpenAI has no ping spec)
	SSEPingFormatNone SSEPingFormat = ""
	// SSEPingFormatComment is an SSE comment ping for OpenAI/Codex CLI clients
	SSEPingFormatComment SSEPingFormat = ":\n\n"
)

// ConcurrencyError represents a concurrency limit error with context
type ConcurrencyError struct {
	SlotType  string
	IsTimeout bool
	// Limit 触发本次超限的并发上限值；用于可配文案的 {limit} 占位符。
	Limit int
}

func (e *ConcurrencyError) Error() string {
	if e.IsTimeout {
		return fmt.Sprintf("timeout waiting for %s concurrency slot", e.SlotType)
	}
	return fmt.Sprintf("%s concurrency limit reached", e.SlotType)
}

// concurrencyScope 描述一次请求应占用的并发维度。
// 订阅模式请求走 group 维度（用户+分组独立计数），计量模式走 user 维度。
type concurrencyScope struct {
	Kind      string // "user"（计量）| "group"（订阅）
	Max       int    // 对应上限；0 = 不限
	GroupID   int64  // 订阅时有效
	ScopeWord string // 文案 {scope} 维度词："用户" | "订阅"
}

// resolveConcurrencyScope 判定本次请求走"订阅分组并发"还是"用户全局并发"。
//
// 判定口径必须与 service.CheckBillingEligibility 逐字一致：
//
//	group.IsSubscriptionType() && subscription != nil
//
// 分组取自认证中间件写入 ctxkey.Group 的"认证时刻分组"——取槽位发生在调度覆盖之前，
// 必须从这里取，不能从可能被 fallback/composite 覆盖的位置取。分组缺失或无效一律回落
// 用户全局并发（沿用 service.IsGroupContextValid 防御），且订阅与计量两条路之间不得有任何
// 取小/叠加/回落逻辑。
func resolveConcurrencyScope(c *gin.Context, subject middleware2.AuthSubject, subscription *service.UserSubscription) concurrencyScope {
	userScope := concurrencyScope{Kind: "user", Max: subject.Concurrency, ScopeWord: "用户"}

	group, ok := c.Request.Context().Value(ctxkey.Group).(*service.Group)
	if !ok || !service.IsGroupContextValid(group) {
		return userScope
	}
	if !group.IsSubscriptionType() || subscription == nil {
		return userScope
	}
	return concurrencyScope{
		Kind:      "group",
		Max:       group.Concurrency,
		GroupID:   group.ID,
		ScopeWord: "订阅",
	}
}

// AcquireScopedUserSlotWithWait 按并发作用域分流占槽：订阅走 group 维度，计量走 user 维度。
// 调用方负责用 scope.Kind 作为并发超限出口的 slotType。
func (h *ConcurrencyHelper) AcquireScopedUserSlotWithWait(c *gin.Context, scope concurrencyScope, userID int64, isStream bool, streamStarted *bool) (func(), error) {
	if scope.Kind == "group" {
		return h.AcquireUserGroupSlotWithWait(c, scope.GroupID, userID, scope.Max, isStream, streamStarted)
	}
	return h.AcquireUserSlotWithWait(c, userID, scope.Max, isStream, streamStarted)
}

type WaitQueueFullError struct {
	SlotType string
}

func (e *WaitQueueFullError) Error() string {
	// 内部错误标识文本，与下游可见文案共用同一常量（errors.As 匹配不依赖文本），
	// 保证全仓不再出现裸英文串。
	return infraerrors.GatewayQueueFull
}

// ConcurrencyHelper provides common concurrency slot management for gateway handlers
type ConcurrencyHelper struct {
	concurrencyService *service.ConcurrencyService
	pingFormat         SSEPingFormat
	pingInterval       time.Duration
}

// NewConcurrencyHelper creates a new ConcurrencyHelper
func NewConcurrencyHelper(concurrencyService *service.ConcurrencyService, pingFormat SSEPingFormat, pingInterval time.Duration) *ConcurrencyHelper {
	if pingInterval <= 0 {
		pingInterval = defaultPingInterval
	}
	return &ConcurrencyHelper{
		concurrencyService: concurrencyService,
		pingFormat:         pingFormat,
		pingInterval:       pingInterval,
	}
}

// wrapReleaseOnDone ensures release runs at most once and still triggers on context cancellation.
// 用于避免客户端断开或上游超时导致的并发槽位泄漏。
// 优化：基于 context.AfterFunc 注册回调，避免每请求额外守护 goroutine。
func wrapReleaseOnDone(ctx context.Context, releaseFunc func()) func() {
	if releaseFunc == nil {
		return nil
	}
	var once sync.Once
	releaseOnce := func() {
		once.Do(releaseFunc)
	}
	stop := context.AfterFunc(ctx, releaseOnce)

	return func() {
		_ = stop()
		releaseOnce()
	}
}

// IncrementWaitCount increments the wait count for a user
func (h *ConcurrencyHelper) IncrementWaitCount(ctx context.Context, userID int64, maxWait int) (bool, error) {
	return h.concurrencyService.IncrementWaitCount(ctx, userID, maxWait)
}

// DecrementWaitCount decrements the wait count for a user
func (h *ConcurrencyHelper) DecrementWaitCount(ctx context.Context, userID int64) {
	h.concurrencyService.DecrementWaitCount(ctx, userID)
}

// IncrementAccountWaitCount increments the wait count for an account
func (h *ConcurrencyHelper) IncrementAccountWaitCount(ctx context.Context, accountID int64, maxWait int) (bool, error) {
	return h.concurrencyService.IncrementAccountWaitCount(ctx, accountID, maxWait)
}

// DecrementAccountWaitCount decrements the wait count for an account
func (h *ConcurrencyHelper) DecrementAccountWaitCount(ctx context.Context, accountID int64) {
	h.concurrencyService.DecrementAccountWaitCount(ctx, accountID)
}

// IncrementUserGroupWaitCount increments the wait count for a (groupID, userID) pair
func (h *ConcurrencyHelper) IncrementUserGroupWaitCount(ctx context.Context, groupID, userID int64, maxWait int) (bool, error) {
	return h.concurrencyService.IncrementUserGroupWaitCount(ctx, groupID, userID, maxWait)
}

// DecrementUserGroupWaitCount decrements the wait count for a (groupID, userID) pair
func (h *ConcurrencyHelper) DecrementUserGroupWaitCount(ctx context.Context, groupID, userID int64) {
	h.concurrencyService.DecrementUserGroupWaitCount(ctx, groupID, userID)
}

// TryAcquireUserSlot 尝试立即获取用户并发槽位。
// 返回值: (releaseFunc, acquired, error)
func (h *ConcurrencyHelper) TryAcquireUserSlot(ctx context.Context, userID int64, maxConcurrency int) (func(), bool, error) {
	result, err := h.concurrencyService.AcquireUserSlot(ctx, userID, maxConcurrency)
	if err != nil {
		return nil, false, err
	}
	if !result.Acquired {
		return nil, false, nil
	}
	return result.ReleaseFunc, true, nil
}

func (h *ConcurrencyHelper) TryAcquireUserSlotForAPIKey(ctx context.Context, userID int64, maxConcurrency int, apiKeyID int64) (func(), bool, error) {
	releaseFunc, acquired, err := h.TryAcquireUserSlot(ctx, userID, maxConcurrency)
	if err != nil || !acquired {
		return releaseFunc, acquired, err
	}
	return h.withAPIKeySlot(ctx, apiKeyID, releaseFunc), true, nil
}

// TryAcquireUserGroupSlot 尝试立即获取"用户+分组"并发槽位（订阅分组维度，独立于用户全局并发）。
// 返回值: (releaseFunc, acquired, error)
func (h *ConcurrencyHelper) TryAcquireUserGroupSlot(ctx context.Context, groupID, userID int64, maxConcurrency int) (func(), bool, error) {
	result, err := h.concurrencyService.AcquireUserGroupSlot(ctx, groupID, userID, maxConcurrency)
	if err != nil {
		return nil, false, err
	}
	if !result.Acquired {
		return nil, false, nil
	}
	return result.ReleaseFunc, true, nil
}

func (h *ConcurrencyHelper) TryAcquireUserGroupSlotForAPIKey(ctx context.Context, groupID, userID int64, maxConcurrency int, apiKeyID int64) (func(), bool, error) {
	releaseFunc, acquired, err := h.TryAcquireUserGroupSlot(ctx, groupID, userID, maxConcurrency)
	if err != nil || !acquired {
		return releaseFunc, acquired, err
	}
	return h.withAPIKeySlot(ctx, apiKeyID, releaseFunc), true, nil
}

// AcquireOpenAIWSIngressLease bounds the whole client WebSocket lifecycle,
// independently from per-turn user and account slots.
func (h *ConcurrencyHelper) AcquireOpenAIWSIngressLease(ctx context.Context, apiKeyID int64, maxConnections int) (*service.OpenAIWSIngressLease, bool, error) {
	if h == nil || h.concurrencyService == nil {
		return nil, false, fmt.Errorf("concurrency service is unavailable")
	}
	return h.concurrencyService.AcquireOpenAIWSIngressLease(ctx, apiKeyID, maxConnections)
}

// TryAcquireAccountSlot 尝试立即获取账号并发槽位。
// 返回值: (releaseFunc, acquired, error)
func (h *ConcurrencyHelper) TryAcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int) (func(), bool, error) {
	result, err := h.concurrencyService.AcquireAccountSlot(ctx, accountID, maxConcurrency)
	if err != nil {
		return nil, false, err
	}
	if !result.Acquired {
		return nil, false, nil
	}
	return result.ReleaseFunc, true, nil
}

// AcquireUserSlotWithWait acquires a user concurrency slot, waiting if necessary.
// For streaming requests, sends ping events during the wait.
// streamStarted is updated if streaming response has begun.
func (h *ConcurrencyHelper) AcquireUserSlotWithWait(c *gin.Context, userID int64, maxConcurrency int, isStream bool, streamStarted *bool) (func(), error) {
	return h.acquireUserSlotWithWaitTimeout(c, userID, maxConcurrency, maxConcurrencyWait, isStream, streamStarted)
}

func (h *ConcurrencyHelper) acquireUserSlotWithWaitTimeout(c *gin.Context, userID int64, maxConcurrency int, timeout time.Duration, isStream bool, streamStarted *bool) (func(), error) {
	ctx := c.Request.Context()

	// Try to acquire immediately
	releaseFunc, acquired, err := h.TryAcquireUserSlot(ctx, userID, maxConcurrency)
	if err != nil {
		return nil, err
	}

	if acquired {
		return h.withAPIKeySlotFromGin(c, releaseFunc), nil
	}

	queueLimit := service.CalculateMaxWait(maxConcurrency) - maxConcurrency
	if queueLimit < 1 {
		queueLimit = 1
	}
	canWait, err := h.IncrementWaitCount(ctx, userID, queueLimit)
	if err != nil {
		return nil, err
	}
	if !canWait {
		return nil, &WaitQueueFullError{SlotType: "user"}
	}
	defer h.DecrementWaitCount(ctx, userID)

	// Need to wait - handle streaming ping if needed
	releaseFunc, err = h.waitForSlotWithPingTimeout(c, "user", userID, maxConcurrency, timeout, isStream, streamStarted, false)
	if err != nil {
		return nil, err
	}
	return h.withAPIKeySlotFromGin(c, releaseFunc), nil
}

// AcquireUserGroupSlotWithWait 获取"用户+分组"并发槽位，必要时等待（订阅分组维度）。
// slotType 固定为 "group"，与用户全局并发互不干扰。
func (h *ConcurrencyHelper) AcquireUserGroupSlotWithWait(c *gin.Context, groupID, userID int64, maxConcurrency int, isStream bool, streamStarted *bool) (func(), error) {
	return h.acquireUserGroupSlotWithWaitTimeout(c, groupID, userID, maxConcurrency, maxConcurrencyWait, isStream, streamStarted)
}

func (h *ConcurrencyHelper) acquireUserGroupSlotWithWaitTimeout(c *gin.Context, groupID, userID int64, maxConcurrency int, timeout time.Duration, isStream bool, streamStarted *bool) (func(), error) {
	ctx := c.Request.Context()

	// Try to acquire immediately
	releaseFunc, acquired, err := h.TryAcquireUserGroupSlot(ctx, groupID, userID, maxConcurrency)
	if err != nil {
		return nil, err
	}

	if acquired {
		return h.withAPIKeySlotFromGin(c, releaseFunc), nil
	}

	queueLimit := service.CalculateMaxWait(maxConcurrency) - maxConcurrency
	if queueLimit < 1 {
		queueLimit = 1
	}
	canWait, err := h.IncrementUserGroupWaitCount(ctx, groupID, userID, queueLimit)
	if err != nil {
		return nil, err
	}
	if !canWait {
		return nil, &WaitQueueFullError{SlotType: "group"}
	}
	defer h.DecrementUserGroupWaitCount(ctx, groupID, userID)

	// Need to wait - handle streaming ping if needed
	releaseFunc, err = h.waitForUserGroupSlotWithPingTimeout(c, "group", groupID, userID, maxConcurrency, timeout, isStream, streamStarted)
	if err != nil {
		return nil, err
	}
	return h.withAPIKeySlotFromGin(c, releaseFunc), nil
}

func (h *ConcurrencyHelper) withAPIKeySlotFromGin(c *gin.Context, releaseFunc func()) func() {
	if c == nil {
		return releaseFunc
	}
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil {
		return releaseFunc
	}
	return h.withAPIKeySlot(c.Request.Context(), apiKey.ID, releaseFunc)
}

func (h *ConcurrencyHelper) withAPIKeySlot(ctx context.Context, apiKeyID int64, releaseFunc func()) func() {
	if h == nil || h.concurrencyService == nil || apiKeyID <= 0 {
		return releaseFunc
	}
	apiKeyReleaseFunc := h.concurrencyService.TrackAPIKeySlot(ctx, apiKeyID)
	return func() {
		if releaseFunc != nil {
			releaseFunc()
		}
		if apiKeyReleaseFunc != nil {
			apiKeyReleaseFunc()
		}
	}
}

// AcquireAccountSlotWithWait acquires an account concurrency slot, waiting if necessary.
// For streaming requests, sends ping events during the wait.
// streamStarted is updated if streaming response has begun.
func (h *ConcurrencyHelper) AcquireAccountSlotWithWait(c *gin.Context, accountID int64, maxConcurrency int, isStream bool, streamStarted *bool) (func(), error) {
	ctx := c.Request.Context()

	// Try to acquire immediately
	releaseFunc, acquired, err := h.TryAcquireAccountSlot(ctx, accountID, maxConcurrency)
	if err != nil {
		return nil, err
	}

	if acquired {
		return releaseFunc, nil
	}

	// Need to wait - handle streaming ping if needed
	return h.waitForSlotWithPing(c, "account", accountID, maxConcurrency, isStream, streamStarted)
}

// waitForSlotWithPing waits for a concurrency slot, sending ping events for streaming requests.
// streamStarted pointer is updated when streaming begins (for proper error handling by caller).
func (h *ConcurrencyHelper) waitForSlotWithPing(c *gin.Context, slotType string, id int64, maxConcurrency int, isStream bool, streamStarted *bool) (func(), error) {
	return h.waitForSlotWithPingTimeout(c, slotType, id, maxConcurrency, maxConcurrencyWait, isStream, streamStarted, false)
}

// waitForSlotWithPingTimeout waits for a concurrency slot with a custom timeout.
func (h *ConcurrencyHelper) waitForSlotWithPingTimeout(c *gin.Context, slotType string, id int64, maxConcurrency int, timeout time.Duration, isStream bool, streamStarted *bool, tryImmediate bool) (func(), error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	acquireSlot := func() (*service.AcquireResult, error) {
		if slotType == "user" {
			return h.concurrencyService.AcquireUserSlot(ctx, id, maxConcurrency)
		}
		return h.concurrencyService.AcquireAccountSlot(ctx, id, maxConcurrency)
	}

	if tryImmediate {
		result, err := acquireSlot()
		if err != nil {
			return nil, err
		}
		if result.Acquired {
			return result.ReleaseFunc, nil
		}
	}

	if result, err := h.waitForSlotWithRetry(ctx, c, slotType, acquireSlot, maxConcurrency, isStream, streamStarted); err != nil || result != nil {
		return result, err
	}
	return nil, nil
}

// waitForUserGroupSlotWithPingTimeout 等待"用户+分组"并发槽位（独立维度，逻辑同 waitForSlotWithPingTimeout）。
func (h *ConcurrencyHelper) waitForUserGroupSlotWithPingTimeout(c *gin.Context, slotType string, groupID, userID int64, maxConcurrency int, timeout time.Duration, isStream bool, streamStarted *bool) (func(), error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	acquireSlot := func() (*service.AcquireResult, error) {
		return h.concurrencyService.AcquireUserGroupSlot(ctx, groupID, userID, maxConcurrency)
	}

	return h.waitForSlotWithRetry(ctx, c, slotType, acquireSlot, maxConcurrency, isStream, streamStarted)
}

// waitForSlotWithRetry 是 user/account/group 三种维度共用的退避+ping 等待核心实现。
// acquireSlot 闭包负责按具体维度占槽；slotType 仅用于错误上报与可配文案的 {scope} 占位符。
func (h *ConcurrencyHelper) waitForSlotWithRetry(ctx context.Context, c *gin.Context, slotType string, acquireSlot func() (*service.AcquireResult, error), maxConcurrency int, isStream bool, streamStarted *bool) (func(), error) {
	// Determine if ping is needed (streaming + ping format defined)
	needPing := isStream && h.pingFormat != ""

	var flusher http.Flusher
	if needPing {
		var ok bool
		flusher, ok = c.Writer.(http.Flusher)
		if !ok {
			return nil, fmt.Errorf("streaming not supported")
		}
	}

	// Only create ping ticker if ping is needed
	var pingCh <-chan time.Time
	if needPing {
		pingTicker := time.NewTicker(h.pingInterval)
		defer pingTicker.Stop()
		pingCh = pingTicker.C
	}

	backoff := initialBackoff
	timer := time.NewTimer(backoff)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			if parentErr := c.Request.Context().Err(); parentErr != nil {
				return nil, parentErr
			}
			return nil, &ConcurrencyError{
				SlotType:  slotType,
				IsTimeout: true,
				Limit:     maxConcurrency,
			}

		case <-pingCh:
			// Send ping to keep connection alive
			if !*streamStarted {
				c.Header("Content-Type", "text/event-stream")
				c.Header("Cache-Control", "no-cache")
				c.Header("Connection", "keep-alive")
				c.Header("X-Accel-Buffering", "no")
				*streamStarted = true
			}
			written, err := fmt.Fprint(c.Writer, string(h.pingFormat))
			if err != nil {
				return nil, err
			}
			recordGatewayStreamHeartbeat(c, written)
			flusher.Flush()

		case <-timer.C:
			// Try to acquire slot
			result, err := acquireSlot()
			if err != nil {
				return nil, err
			}

			if result.Acquired {
				return result.ReleaseFunc, nil
			}
			backoff = nextBackoff(backoff)
			timer.Reset(backoff)
		}
	}
}

// AcquireAccountSlotWithWaitTimeout acquires an account slot with a custom timeout (keeps SSE ping).
func (h *ConcurrencyHelper) AcquireAccountSlotWithWaitTimeout(c *gin.Context, accountID int64, maxConcurrency int, timeout time.Duration, isStream bool, streamStarted *bool) (func(), error) {
	return h.waitForSlotWithPingTimeout(c, "account", accountID, maxConcurrency, timeout, isStream, streamStarted, true)
}

// nextBackoff 计算下一次退避时间
// 性能优化：使用指数退避 + 随机抖动，避免惊群效应
// current: 当前退避时间
// 返回值：下一次退避时间（100ms ~ 2s 之间）
func nextBackoff(current time.Duration) time.Duration {
	// 指数退避：当前时间 * 1.5
	next := time.Duration(float64(current) * backoffMultiplier)
	if next > maxBackoff {
		next = maxBackoff
	}
	// 添加 ±20% 的随机抖动（jitter 范围 0.8 ~ 1.2）
	// 抖动可以分散多个请求的重试时间点，避免同时冲击 Redis
	jitter := 0.8 + rand.Float64()*0.4
	jittered := time.Duration(float64(next) * jitter)
	if jittered < initialBackoff {
		return initialBackoff
	}
	if jittered > maxBackoff {
		return maxBackoff
	}
	return jittered
}

// ---------------------------------------------------------------------------
// §25.4 跨 HTTP 资源转交：可转交 context 的取消注册 + 会话持有
// ---------------------------------------------------------------------------

// 槽位句柄错误（交接失败一律失败关闭，不交失效句柄给会话）。
var (
	// errCursorHandleReleased 表示句柄已取消/已释放：转交必须失败。
	errCursorHandleReleased = errors.New("handler: cursor slot handle already released (handover failed)")
	// errCursorHandleHanded 表示同一句柄被重复转交。
	errCursorHandleHanded = errors.New("handler: cursor slot handle already handed over")
)

// transferableRelease 是可**转交 context** 的槽位释放句柄（§25.4）。
//
// 与 wrapReleaseOnDone 的差别只有一个：释放所有权可以整体交给会话——
// 转交在同一把锁内停掉原 HTTP 取消回调并检查是否已释放；已取消/已释放即交接失败，
// 不能把失效槽位交 session。转交成功后取消回调绑定 session context；原始 release
// 不变，整个生命周期仍然只 release 一次。
type transferableRelease struct {
	name      string
	releaseFn func()
	once      sync.Once

	mu     sync.Mutex
	stop   func() bool
	handed bool
	done   bool
}

// newTransferableRelease 在 ctx 上登记释放回调（等价于 wrapReleaseOnDone 的语义）。
func newTransferableRelease(ctx context.Context, name string, releaseFunc func()) *transferableRelease {
	if releaseFunc == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h := &transferableRelease{name: name, releaseFn: releaseFunc}
	h.stop = context.AfterFunc(ctx, h.releaseNow)
	return h
}

// Name 实现 service.CursorTransferableResource。
func (h *transferableRelease) Name() string {
	if h == nil {
		return ""
	}
	return h.name
}

// releaseNow 是唯一的真正释放点（once 保证只 release 一次）。
func (h *transferableRelease) releaseNow() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.done {
		h.mu.Unlock()
		return
	}
	h.done = true
	h.mu.Unlock()
	h.once.Do(h.releaseFn)
}

// Release 幂等释放；已转交给会话时是空操作（释放归会话 finalizer）。
func (h *transferableRelease) Release() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.handed {
		h.mu.Unlock()
		return
	}
	stop := h.stop
	h.stop = nil
	h.mu.Unlock()
	if stop != nil {
		_ = stop()
	}
	h.releaseNow()
}

// TransferTo 把释放所有权转交给会话上下文（§25.4）。
//
// 同一锁内：停掉 HTTP 取消回调 → 复查是否已释放 → 绑定 session context。
// 已取消/已释放/重复转交一律返回错误，绝不把失效槽位交给会话。
func (h *transferableRelease) TransferTo(sessionCtx context.Context) error {
	if h == nil {
		return nil
	}
	if sessionCtx == nil {
		return errCursorHandleReleased
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handed {
		return errCursorHandleHanded
	}
	if h.done {
		return errCursorHandleReleased
	}
	if h.stop != nil {
		if stopped := h.stop(); !stopped {
			// 取消回调已在运行/已触发：本槽位不再可信，交接必须失败。
			return errCursorHandleReleased
		}
		h.stop = nil
	}
	if h.done {
		return errCursorHandleReleased
	}
	h.stop = context.AfterFunc(sessionCtx, h.releaseNow)
	h.handed = true
	return nil
}

// HandedOver 报告句柄是否已交给会话（诊断/测试用）。
func (h *transferableRelease) HandedOver() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.handed
}

// transferableInflight 把余额预留的归还时机延后到会话终结（§25.4/§25.5）。
//
// 只延后**现有** HandlerDone；转交前必须先 HoldPastHTTP 成功——已在 HTTP 侧归还的
// 预留不得再 Acquire 假装保住续期。
type transferableInflight struct {
	name string
	res  *service.InflightReservation

	mu     sync.Mutex
	stop   func() bool
	handed bool
}

// newTransferableInflight 用 ctx 绑定 HTTP 生命周期归还。
func newTransferableInflight(ctx context.Context, res *service.InflightReservation) *transferableInflight {
	if res == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h := &transferableInflight{name: "inflight", res: res}
	h.stop = context.AfterFunc(ctx, func() { res.HandlerDone() })
	return h
}

// Name 实现 service.CursorTransferableResource。
func (h *transferableInflight) Name() string {
	if h == nil {
		return ""
	}
	if h.name == "" {
		return "inflight"
	}
	return h.name
}

// TransferTo 延后 HandlerDone 至会话终结。
func (h *transferableInflight) TransferTo(sessionCtx context.Context) error {
	if h == nil || h.res == nil {
		return nil
	}
	if !h.res.HoldPastHTTP() {
		return service.ErrCursorSessionResourceReleased
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handed {
		return service.ErrCursorSessionResourceHanded
	}
	if h.stop != nil {
		if stopped := h.stop(); !stopped {
			return service.ErrCursorSessionResourceReleased
		}
		h.stop = nil
	}
	if sessionCtx != nil {
		h.stop = context.AfterFunc(sessionCtx, func() { h.res.SessionDone() })
	}
	h.handed = true
	return nil
}

// Release 按原 HTTP 生命周期归还（已转交则空操作，归会话 finalizer）。
func (h *transferableInflight) Release() {
	if h == nil || h.res == nil {
		return
	}
	h.mu.Lock()
	if h.handed {
		h.mu.Unlock()
		return
	}
	stop := h.stop
	h.stop = nil
	h.mu.Unlock()
	if stop != nil {
		_ = stop()
	}
	h.res.HandlerDone()
}

// CursorInflightReservation 暴露预留句柄给会话（结算提交后才 SessionDone）。
func (h *transferableInflight) CursorInflightReservation() *service.InflightReservation {
	if h == nil {
		return nil
	}
	return h.res
}

// cursorResourceHandover 是 Cursor 分支把本 HTTP 资源与**唯一结算**交给会话的载体。
//
// 各 handler 用它识别"交接中/已由 session 结算"：不再为每个工具 HTTP 排一次
// UsageRecordTask，而是把该任务登记给会话唯一结算（至多执行一次）。
type cursorResourceHandover struct {
	resources  []service.CursorTransferableResource
	submitTask func(context.Context, service.UsageRecordTask)

	mu        sync.Mutex
	finalized bool
	complete  bool
	eventID   string
	task      service.UsageRecordTask
	settled   bool
	// settleCtx / releaseInflight 由会话终结时接纳：结算提交之后才归还在途预留引用，
	// 保证"不得在缓存扣减前先释放预留"（§25.5）。
	settleCtx       context.Context
	releaseInflight func()
}

// Resources 实现 service.CursorSessionHandover。
func (h *cursorResourceHandover) Resources() []service.CursorTransferableResource {
	if h == nil {
		return nil
	}
	return h.resources
}

// Settlement 实现 service.CursorSessionHandover。
func (h *cursorResourceHandover) Settlement() service.CursorSessionSettlement {
	if h == nil {
		return nil
	}
	return h.settle
}

// AdoptSettlementTask 实现 service.CursorSettlementOwner。
func (h *cursorResourceHandover) AdoptSettlementTask(task service.UsageRecordTask) {
	if h == nil || task == nil {
		return
	}
	h.mu.Lock()
	if !h.finalized {
		h.task = task
		h.mu.Unlock()
		return
	}
	complete, eventID := h.complete, h.eventID
	if complete && eventID != "" {
		// 会话已终结且接纳了完整真实 usage：登记后立即结算（至多一次）。
		h.task = task
		h.mu.Unlock()
		h.runSettlement()
		return
	}
	h.mu.Unlock()
	// 会话未结算：归还预留引用（幂等），避免留下无人归还的预留。
	h.releaseOnNoSettlement()
}

// releaseOnNoSettlement 在会话未结算时归还预留引用（幂等）。
func (h *cursorResourceHandover) releaseOnNoSettlement() {
	if h == nil {
		return
	}
	h.mu.Lock()
	release := h.releaseInflight
	h.releaseInflight = nil
	h.mu.Unlock()
	if release != nil {
		release()
	}
}

// Settled 报告本 HTTP 的计费是否已由会话结算（handler 据此跳过自己的计费任务）。
func (h *cursorResourceHandover) Settled() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settled
}

// Finalized 报告会话是否已终结（诊断/测试用）。
func (h *cursorResourceHandover) Finalized() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.finalized
}

// settle 是会话唯一结算回调：只记录被接纳的终态，实际提交至多一次。
func (h *cursorResourceHandover) settle(ctx context.Context, ev service.CursorSettlementEvent) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.finalized {
		h.mu.Unlock()
		return
	}
	h.finalized = true
	h.complete = ev.UsageComplete
	h.eventID = ev.BillingEventID
	// 结算 ctx 必须携带**会话级不可变账单事件 ID**：即使调用方忘了绑定，这里也强制绑定，
	// 保证 durable 键永远是会话级而不是某个 HTTP 的 request_id（§25.5）。
	h.settleCtx = service.WithCursorSessionBillingEventID(ctx, ev.BillingEventID)
	h.releaseInflight = ev.InflightRelease
	pending := h.task
	h.mu.Unlock()
	if !ev.UsageComplete {
		// 未取得完整真实 usage：不结算、不写零 usage 假成功；预留引用在这里归还。
		h.releaseOnNoSettlement()
		return
	}
	if pending == nil {
		// 本 HTTP 的计费任务尚未登记（handler 计费块在 Execute 返回之后才执行）：
		// 等 AdoptSettlementTask 到达时再提交，至多一次。
		return
	}
	h.runSettlement()
}

// runSettlement 至多提交一次计费任务。
//
// 结算 ctx 由会话终结时交给本载体：它带**会话级不可变账单事件 ID**与在途预留句柄，
// 所以提交（Acquire 交接）发生在归还预留之前，扣减语义与既有计费链一致。
func (h *cursorResourceHandover) runSettlement() {
	h.mu.Lock()
	if h.settled || h.task == nil || h.submitTask == nil {
		h.mu.Unlock()
		return
	}
	h.settled = true
	task := h.task
	submit := h.submitTask
	ctx := h.settleCtx
	release := h.releaseInflight
	h.releaseInflight = nil
	h.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	// 结算 ctx 继承会话级 durable 键与预留句柄，但不继承 HTTP 取消（会话已终结）。
	submit(context.WithoutCancel(ctx), task)
	if release != nil {
		release()
	}
}

// cursorSettlementSubmitter 是计费任务提交函数签名（复用既有 worker 池）。
type cursorSettlementSubmitter func(context.Context, service.UsageRecordTask)

// newCursorResourceHandover 构造 Cursor 分支的转交物。
func newCursorResourceHandover(
	resources []service.CursorTransferableResource,
	submit cursorSettlementSubmitter,
) *cursorResourceHandover {
	return &cursorResourceHandover{resources: resources, submitTask: submit}
}

// bindCursorHandoverForAttempt 为**本次账号尝试**绑定 Cursor 转交物。
//
// 只有 Cursor 账号才会登记；未选中 Cursor 的其它供应商显式清空转交物并保持原 HTTP
// 生命周期释放（**不能**全站改成会话持有，也不能让上一次尝试的转交物串到下一次）。
func bindCursorHandoverForAttempt(ctx context.Context, account *service.Account, handover *cursorResourceHandover) context.Context {
	if ctx == nil {
		return ctx
	}
	if !isCursorPlatformAccount(account) || handover == nil {
		return service.WithCursorSessionHandoverCleared(ctx)
	}
	return service.WithCursorSessionHandover(ctx, handover)
}

// isCursorPlatformAccount 判定账号是否为 Cursor 平台（非 Cursor 路径语义不变）。
func isCursorPlatformAccount(account *service.Account) bool {
	return account != nil && account.Platform == service.PlatformCursor
}

// cursorHandoverResources 收集本次真正获取到的 Cursor 可转交资源。
//
// nil 句柄表示该资源本次未获取（开关关闭、订阅模式、无限并发等），不登记空壳，
// 会话 finalizer 只释放真正转交过的句柄。
func cursorHandoverResources(
	userSlot *transferableRelease,
	accountSlot *transferableRelease,
	inflight *transferableInflight,
) []service.CursorTransferableResource {
	resources := make([]service.CursorTransferableResource, 0, 3)
	if userSlot != nil {
		resources = append(resources, userSlot)
	}
	if accountSlot != nil {
		resources = append(resources, accountSlot)
	}
	if inflight != nil {
		resources = append(resources, inflight)
	}
	return resources
}

// Cursor 可转交槽位句柄的请求级登记键（gin context）。
//
// 用途：OpenAI 出口的槽位获取在 helper 内部完成并自带 wrapReleaseOnDone，调用方拿不到
// 停止回调。改为 helper 内直接用**可转交**句柄并登记到请求上，调用方（Cursor 分支）
// 按名字取出即可转交，且不需要改任何 helper 签名。
const (
	cursorSlotHandleUserKey    = "cursor.slot.user"
	cursorSlotHandleAccountKey = "cursor.slot.account"
)

// trackTransferableSlot 把本次请求的槽位句柄登记到请求 context 上。
func trackTransferableSlot(c *gin.Context, key string, handle *transferableRelease) {
	if c == nil || handle == nil || key == "" {
		return
	}
	c.Set(key, handle)
}

// transferableSlot 取出本次请求登记的槽位句柄（可能为 nil）。
func transferableSlot(c *gin.Context, key string) *transferableRelease {
	if c == nil {
		return nil
	}
	value, ok := c.Get(key)
	if !ok {
		return nil
	}
	handle, _ := value.(*transferableRelease)
	return handle
}

// ---------------------------------------------------------------------------
// §25.2 续轮识别区（各协议 handler 在**资源获取之前**调用）
// ---------------------------------------------------------------------------

// cursorResumeIdentity 用已鉴权上下文构造会话身份快照（只含内部 ID，不含凭据）。
func cursorResumeIdentity(apiKey *service.APIKey, userID int64) service.CursorSessionIdentity {
	identity := service.CursorSessionIdentity{UserID: userID}
	if apiKey != nil {
		identity.APIKeyID = apiKey.ID
		if apiKey.GroupID != nil {
			identity.GroupID = *apiKey.GroupID
		}
	}
	return identity
}

// serveCursorResumeIfRequested 在**用户槽/账号槽/预留/选号之前**识别续轮并整批接管。
//
// 顺序固定（§25.14-D「整批接管顺序」）：
//
//	正常鉴权（已由 handler 完成） → 解析最后待完成批及结果 ID → 统一索引定位 →
//	身份/来源/账号校验（ServeCursorResume 内） → 配置/前缀摘要/本批结果完整校验 →
//	服务锁内接管（唯一胜者，后者 409） → 锁外串行写真实结果。
//
// 返回值语义：
//   - true：已写出响应（续轮被接纳，或明确了 400/403/409/503 拒绝），调用方必须 return；
//   - false：本次不是可识别的续轮，按普通新请求继续（会话身份已挂到 ctx）。
//
// 校验失败零上游写入；不换号、不重选、不猜最近会话。
func serveCursorResumeIfRequested(
	c *gin.Context,
	gw service.CursorResumeGateway,
	protocol service.CursorResumeProtocol,
	body []byte,
	identity service.CursorSessionIdentity,
	stream bool,
	reject func(status int, message string),
) bool {
	if c == nil || c.Request == nil || reject == nil {
		return false
	}
	ctx := service.WithCursorSessionIdentity(c.Request.Context(), identity)
	c.Request = c.Request.WithContext(ctx)
	if gw == nil || len(body) == 0 {
		return false
	}
	probe, probeErr := service.CursorProbeResumeRequest(protocol, body)
	if probeErr != nil {
		if status, reason := service.CursorResumeHTTPStatus(probeErr); status != 0 {
			reject(status, reason)
			return true
		}
		// 探针解码失败不改既有解析错误链：交给普通路径按原语义处理。
		return false
	}
	if probe == nil || !probe.ClaimsContinuation() {
		return false
	}
	ticket, identifyErr := gw.IdentifyCursorResume(probe)
	if identifyErr != nil {
		status, reason := service.CursorResumeHTTPStatus(identifyErr)
		if status == 0 {
			// 非续轮错误（例如执行部件未装配）：不在资源获取前判死，交回普通路径按其
			// 原契约失败关闭，避免把无关请求改成续轮错误。
			return false
		}
		reject(status, reason)
		return true
	}
	if ticket == nil {
		// 没有登记任何关联（含重启后孤儿不命中）：按普通新请求继续。
		return false
	}
	if _, serveErr := gw.ServeCursorResume(ctx, c, ticket, service.CursorResumeServeInput{
		Body:     body,
		Stream:   stream,
		Identity: identity,
	}); serveErr != nil {
		status, reason := service.CursorResumeHTTPStatus(serveErr)
		if status == 0 {
			status, reason = http.StatusBadGateway, "resume_failed"
		}
		reject(status, reason)
		return true
	}
	return true
}
