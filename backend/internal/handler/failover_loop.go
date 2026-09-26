package handler

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

// TempUnscheduler 用于 HandleFailoverError 中同账号重试耗尽后的临时封禁。
// GatewayService 隐式实现此接口。
type TempUnscheduler interface {
	TempUnscheduleRetryableError(ctx context.Context, accountID int64, failoverErr *service.UpstreamFailoverError)
}

// FailoverAction 表示 failover 错误处理后的下一步动作
type FailoverAction int

const (
	// FailoverContinue 继续循环（同账号重试或切换账号，调用方统一 continue）
	FailoverContinue FailoverAction = iota
	// FailoverExhausted 切换次数耗尽（调用方应返回错误响应）
	FailoverExhausted
	// FailoverCanceled context 已取消（调用方应直接 return）
	FailoverCanceled
)

const (
	// maxSameAccountRetries 同账号重试次数默认上限（针对 RetryableOnSameAccount 错误）。
	// 生产调用方通常传入账号级配置 account.GetPoolModeRetryCount()，该常量仅作兜底/测试默认值。
	// 用户裁定（2026-09-26）：同账号重试默认 3→1，账号不行赶紧切下一个。
	maxSameAccountRetries = 1
	// sameAccountRetryDelay 同账号重试间隔
	sameAccountRetryDelay = 500 * time.Millisecond
	// maxRequestScopedRetryDelay 限制请求级瞬时错误的指数退避上限，避免高重试配置
	// 将单次请求拖入分钟级等待。
	maxRequestScopedRetryDelay = 8 * time.Second
	// maxProfitVetoAttempts 单次请求内允许的分组利润门终检否决次数上限。
	// 利润否决不产生上游请求，因此不会推进 SwitchCount；没有独立上限的话，
	// 「选号 → 终检否决 → 重选」在候选池与账号快照短暂不一致时可以空转很久。
	// 取值与请求级利润否决上限一致：混合定价的大分组仍有充分重选机会，
	// 同时把整池越线时的无谓选号开销限制在常数级。
	maxProfitVetoAttempts = 10
)

// profitVetoExhaustedMessage 是利润否决次数耗尽时返回给客户端的文案。
// 语义上等同于「无可用账号」：候选账号都不满足分组的利润约束。
const profitVetoExhaustedMessage = infraerrors.NoAvailableAccountsProfitControl

func sameAccountRetryDelayFor(failoverErr *service.UpstreamFailoverError, retryCount int) time.Duration {
	if failoverErr == nil {
		return sameAccountRetryDelay
	}
	if failoverErr.SameAccountRetryDelay > 0 {
		return failoverErr.SameAccountRetryDelay
	}
	if !failoverErr.RequestScopedTransient || retryCount <= 1 {
		return sameAccountRetryDelay
	}

	delay := sameAccountRetryDelay
	for i := 1; i < retryCount; i++ {
		if delay >= maxRequestScopedRetryDelay/2 {
			return maxRequestScopedRetryDelay
		}
		delay *= 2
	}
	return delay
}

func sameAccountRetryAllowed(failoverErr *service.UpstreamFailoverError, retryCount, retryLimit int) bool {
	if failoverErr == nil || !failoverErr.RetryableOnSameAccount {
		return false
	}
	if !sameAccountRetryDeadlineAllows(failoverErr) {
		return false
	}
	// Error-specific caps (Grok capacity/stream-idle) remain hard limits even
	// when the error also carries a freshly reconstructed deadline.
	if failoverErr.SameAccountRetryMax > 0 {
		if retryLimit <= 0 {
			return false
		}
		if failoverErr.SameAccountRetryMax < retryLimit {
			retryLimit = failoverErr.SameAccountRetryMax
		}
		return retryCount < retryLimit
	}
	// OAuth 429 explicitly opts into a deadline window. It is intentionally not
	// bounded by the ordinary/default pool retry count.
	if !failoverErr.SameAccountRetryDeadline.IsZero() {
		return true
	}
	return retryLimit > 0 && retryCount < retryLimit
}

// sameAccountRetryDeadlineAllows prevents a retry from starting after the
// service-provided same-account retry window has elapsed.
func sameAccountRetryDeadlineAllows(failoverErr *service.UpstreamFailoverError) bool {
	return failoverErr == nil || failoverErr.SameAccountRetryDeadline.IsZero() || time.Now().Before(failoverErr.SameAccountRetryDeadline)
}

// effectiveSameAccountRetryLimit applies an error-specific cap without
// overriding an explicit account setting of zero (which disables retries).
func effectiveSameAccountRetryLimit(failoverErr *service.UpstreamFailoverError, account *service.Account) int {
	if account == nil {
		return 0
	}
	limit := account.GetPoolModeRetryCount()
	if limit > 0 && failoverErr != nil && failoverErr.SameAccountRetryMax > 0 && failoverErr.SameAccountRetryMax < limit {
		return failoverErr.SameAccountRetryMax
	}
	return limit
}

// FailoverState 跨循环迭代共享的 failover 状态
type FailoverState struct {
	SwitchCount           int
	MaxSwitches           int
	FailedAccountIDs      map[int64]struct{}
	SameAccountRetryCount map[int64]int
	LastFailoverErr       *service.UpstreamFailoverError
	ForceCacheBilling     bool
	hasBoundSession       bool

	// profitVetoCount 本次请求累计的利润否决次数，用于 maxProfitVetoAttempts 上限。
	profitVetoCount int
}

// NewFailoverState 创建无固定切换上限的 failover 状态（OpenAI 家族链专用）。
//
// 旧链（maxAccountSwitches / MaxAccountSwitches / max_account_switches）已删除：
// 不再有固定全局切换上限。新语义——每个失败账号在本请求内至多尝试一次（被加入
// 排除列表后不再回池），候选集合不冻结：请求期间恢复/新增的账号仍可被尝试（需求
// ①"所有有效账号都要轮询到"）；账号耗尽由动态调度在排除集下返回无候选判定，
// 而非固定计数上限。
func NewFailoverState(hasBoundSession bool) *FailoverState {
	return &FailoverState{
		FailedAccountIDs:      make(map[int64]struct{}),
		SameAccountRetryCount: make(map[int64]int),
		hasBoundSession:       hasBoundSession,
	}
}

// NewFailoverStateCapped 创建带固定切换上限的 failover 状态（仅 gemini 链使用）。
//
// gemini 链不在 OpenAI 家族的无上限方案范围内，其"默认 3 次、配置
// max_account_switches_gemini 覆盖"的切换上限语义必须原样保留。MaxSwitches
// 上限判定（HandleFailoverError 内 SwitchCount >= MaxSwitches 时返回
// FailoverExhausted）与旧版完全一致；503 退避分支属于已验收删除项，不在此恢复。
func NewFailoverStateCapped(maxSwitches int, hasBoundSession bool) *FailoverState {
	fs := NewFailoverState(hasBoundSession)
	fs.MaxSwitches = maxSwitches
	return fs
}

// RecordProfitVeto 记录一次分组利润门终检否决：把账号加入排除列表并递增否决
// 计数。无退避语义下排除列表在整个请求内只增不减，被否决账号不会重新回池。
//
// 返回 FailoverContinue 表示调用方可以继续重选下一个账号；返回 FailoverExhausted
// 表示本次请求的利润否决次数已达上限，调用方应按「无可用账号」终止，
// 不得继续 continue。
func (s *FailoverState) RecordProfitVeto(accountID int64) FailoverAction {
	s.FailedAccountIDs[accountID] = struct{}{}
	s.profitVetoCount++
	if s.profitVetoCount >= maxProfitVetoAttempts {
		return FailoverExhausted
	}
	return FailoverContinue
}

// ProfitVetoCount 返回本次请求累计的利润否决次数（供日志使用）。
func (s *FailoverState) ProfitVetoCount() int { return s.profitVetoCount }

// HandleFailoverError 处理 UpstreamFailoverError，返回下一步动作。
// 包含：缓存计费判断、同账号重试、临时封禁、切换计数、Antigravity 延时。
func (s *FailoverState) HandleFailoverError(
	ctx context.Context,
	gatewayService TempUnscheduler,
	accountID int64,
	platform string,
	retryLimit int,
	failoverErr *service.UpstreamFailoverError,
) FailoverAction {
	// 客户端已断开：failover 只会用已取消的 context 重新选号并必然失败，
	// 不应再被当成账号耗尽处理（误报 502）。
	if ctx != nil && ctx.Err() != nil {
		return FailoverCanceled
	}
	s.LastFailoverErr = failoverErr
	if failoverErr == nil || !failoverErr.ShouldRetryNextAccount() {
		return FailoverExhausted
	}

	// 同账号重试不算切换账号，粘性会话仅在实际切换时强制缓存计费。
	retryCount := s.SameAccountRetryCount[accountID]
	sameAccountRetry := sameAccountRetryAllowed(failoverErr, retryCount, retryLimit)
	if needForceCacheBilling(s.hasBoundSession, failoverErr, sameAccountRetry) {
		s.ForceCacheBilling = true
	}

	// 同账号重试：对 RetryableOnSameAccount 的临时性错误，先在同一账号上重试。
	// 重试次数上限 retryLimit 由调用方传入（账号级 pool_mode_retry_count 配置）。
	if sameAccountRetry {
		s.SameAccountRetryCount[accountID]++
		retryDelay := sameAccountRetryDelayFor(failoverErr, s.SameAccountRetryCount[accountID])
		logger.FromContext(ctx).Warn("gateway.failover_same_account_retry",
			zap.Int64("account_id", accountID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("same_account_retry_count", s.SameAccountRetryCount[accountID]),
			zap.Int("same_account_retry_max", retryLimit),
			zap.Duration("retry_delay", retryDelay),
		)
		if !sleepWithContext(ctx, retryDelay) {
			return FailoverCanceled
		}
		return FailoverContinue
	}

	// 同账号重试用尽，执行临时封禁
	if failoverErr.RetryableOnSameAccount {
		gatewayService.TempUnscheduleRetryableError(ctx, accountID, failoverErr)
	}

	// 加入失败列表
	s.FailedAccountIDs[accountID] = struct{}{}

	// OpenAI 家族链（NewFailoverState 构造，MaxSwitches==0）的固定切换上限已删除：
	// 账号耗尽由"所有候选账号均在排除列表、选号返回无可用账号"自然判定，
	// 从而支持"账号轮询不限制个数、所有有效账号都轮询到"。
	// gemini 链经 NewFailoverStateCapped 构造（MaxSwitches>0），保留旧版固定上限
	// 判定：切换计数达到上限即耗尽，语义与旧版完全一致。
	if s.MaxSwitches > 0 && s.SwitchCount >= s.MaxSwitches {
		return FailoverExhausted
	}

	// 递增切换计数（仅用于日志与上游上下文标记，不再作为 OpenAI 家族链的耗尽判据）
	s.SwitchCount++
	logger.FromContext(ctx).Warn("gateway.failover_switch_account",
		zap.Int64("account_id", accountID),
		zap.Int("upstream_status", failoverErr.StatusCode),
		zap.Int("switch_count", s.SwitchCount),
	)

	// Antigravity 平台换号线性递增延时
	if platform == service.PlatformAntigravity {
		delay := time.Duration(s.SwitchCount-1) * time.Second
		if !sleepWithContext(ctx, delay) {
			return FailoverCanceled
		}
	}

	return FailoverContinue
}

// HandleSelectionExhausted 处理选号失败（所有候选账号都在排除列表中）时的退避重试决策。
// 针对 Antigravity 单账号分组的 503 (MODEL_CAPACITY_EXHAUSTED) 场景：
// 清除排除列表、等待退避后重新选号。
//
// 返回 FailoverContinue 时，调用方应设置 SingleAccountRetry context 并 continue。
// 返回 FailoverExhausted 时，调用方应返回错误响应。
// 返回 FailoverCanceled 时，调用方应直接 return。
func (s *FailoverState) HandleSelectionExhausted(ctx context.Context) FailoverAction {
	// 客户端已断开时选号失败是 context canceled 的必然结果，
	// 不代表账号耗尽，直接按取消终止。
	if ctx.Err() != nil {
		return FailoverCanceled
	}

	// 旧链（maxAccountSwitches 上限 + 单账号 503 退避重试）已删除：
	// 每个失败账号在本请求内至多尝试一次（已进排除列表），候选集合不冻结；
	// 选号失败（所有候选均在排除列表）即表示账号耗尽，直接终止。
	// 利润否决导致的活锁由“排除列表下动态选号无可用候选”自然终止（清空后退避
	// 重试拿不到任何新候选），无需单独的 503 退避重试分支（禁区禁止新增替代上限）。
	return FailoverExhausted
}

// needForceCacheBilling 判断 failover 时是否需要强制缓存计费。
// 粘性会话实际切换账号、或上游明确标记时，将 input_tokens 转为 cache_read 计费。
func needForceCacheBilling(hasBoundSession bool, failoverErr *service.UpstreamFailoverError, sameAccountRetry bool) bool {
	return (hasBoundSession && !sameAccountRetry) || (failoverErr != nil && failoverErr.ForceCacheBilling)
}

// failoverClientGone 判断下游客户端是否已断开（请求 context 已取消）。
// 客户端断开后 failover 必须静默终止：用已取消的 context 重新选号只会得到
// context.Canceled，并被误报成账号耗尽（通用 502）；上游 detach 的在途请求
// 照常完成计费，但不再为无人接收的响应启动新的上游尝试。
// 响应尚未提交时把状态码标记为 499（client closed request），供访问日志归类。
func failoverClientGone(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.Context().Err() == nil {
		return false
	}
	// 先停 compact 心跳（接管 ResponseWriter，建立 happens-before），与
	// handleStreamingAwareError/errorResponse 等终结路径对齐，避免心跳
	// goroutine 与下面的状态标记并发触碰同一 writer。心跳已提交 200 时
	// 状态码已固化，不再标 499。
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		return true
	}
	if !c.Writer.Written() {
		c.Status(statusClientClosedRequest)
	}
	return true
}

// sleepWithContext 等待指定时长，返回 false 表示 context 已取消。
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
