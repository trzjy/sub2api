package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const UpstreamJitterModelRateLimitReason = "upstream_jitter"
const UpstreamJitterProbeCadence = 15 * time.Second
const upstreamJitterEntryObservationWindow = 5 * time.Minute

// UpstreamJitterProbeInputKey 是停调条目 extra 中存放「原始选号输入」的 key。
// 探测时按当前账号映射重新解析该输入，避免条目绑定易变的上游真名。
const UpstreamJitterProbeInputKey = "probe_input"

// IsUpstreamJitterError 判定一条换号错误是否属于「上游整体抖动」——即故障因素
// 坐落于上游账号/主机而非客户端请求，值得按 (上游, 模型 scope) 维度做请求内守门。
// 本函数是唯一判定源：命中的错误才会进入 UpstreamJitterGuard.Observe。
func IsUpstreamJitterError(fe *UpstreamFailoverError) bool {
	if fe == nil {
		return false
	}
	if !fe.ShouldRetryNextAccount() {
		return false
	}
	if fe.RequestScopedTransient {
		return false
	}
	if fe.IsCredentialFailure() {
		return false
	}
	// 审核拒绝走既有的主机排除路径，这里不重复处理。
	if strings.TrimSpace(fe.ExcludeUpstreamHost) != "" {
		return false
	}
	if fe.Reason == CursorRateLimitedReason || fe.Reason == cursorKeyRejectedReason {
		return false
	}
	if IsCursorQuotaExhaustedReason(fe.Reason) {
		return false
	}
	if ModelProbeReasonExcluded("", string(fe.Reason)) {
		return false
	}
	if fe.Reason == CursorUpstreamAccountFailureReason {
		return true
	}
	if fe.StatusCode >= 500 && fe.StatusCode <= 599 {
		return true
	}
	return false
}

// CanProbeModelScope 判定账号是否纳入模型级抖动探测。
// Antigravity / Devin / CodeBuddy 有各自独立的模型状态机，不参与本探测链；
// 其余平台（含 cursor 的两种形态）纳入。
func CanProbeModelScope(acc *Account) bool {
	if acc == nil {
		return false
	}
	switch acc.Platform {
	case PlatformAntigravity, PlatformDevin, PlatformCodeBuddy:
		return false
	default:
		return true
	}
}

// UpstreamJitterRepo 是守卫所需仓库能力的窄接口；不修改 AccountRepository，
// 由调用点一次性类型断言得到。后续卡负责让生产仓库实现该接口。
type UpstreamJitterRepo interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	ListByGroup(ctx context.Context, groupID int64) ([]Account, error)
	SetModelRateLimitWithExtra(ctx context.Context, id int64, scope string, resetAt time.Time, reason string, extra map[string]any) error
	SiblingAccountIDsByCursorUpstreamKey(ctx context.Context, groupID, quotaKeyID int64) ([]int64, error)
}

// UpstreamJitterGuard 是请求内的抖动守门器：同一 (上游, 模型 scope) 下失败账号
// 达阈值后，把该上游尚未失败的兄弟账号一次性排除出后续候选集。
type UpstreamJitterGuard struct {
	repo               UpstreamJitterRepo
	groupID            int64
	accounts           map[int64]*Account
	siblingsByQuotaKey map[int64][]int64
	failedByUpstream   map[string]map[int64]struct{}
	skipped            map[string]bool
}

func newUpstreamJitterGuard(repo UpstreamJitterRepo, groupID int64) *UpstreamJitterGuard {
	return &UpstreamJitterGuard{
		repo:               repo,
		groupID:            groupID,
		accounts:           make(map[int64]*Account),
		siblingsByQuotaKey: make(map[int64][]int64),
		failedByUpstream:   make(map[string]map[int64]struct{}),
		skipped:            make(map[string]bool),
	}
}

// Observe 记录一次抖动失败：先写模型级停调，再按 (上游, scope) 计数；
// 达到阈值时返回应排除的兄弟账号 ID（只返回一次）。
//
// modelScope 是「传给选号器的模型（原始选号输入）」，不是上游真名：有 ctx 渠道映射时
// 停调 scope 直接取渠道映射名；否则按当前账号映射解析一次得到停调 scope，原始输入
// 保存在条目 extra 的 UpstreamJitterProbeInputKey 中供探测阶段重新解析。
func (g *UpstreamJitterGuard) Observe(ctx context.Context, accountID int64, modelScope string, fe *UpstreamFailoverError) (admitted bool, excludeIDs []int64) {
	if fe == nil || !IsUpstreamJitterError(fe) || strings.TrimSpace(modelScope) == "" {
		return false, nil
	}

	acc, ok := g.accounts[accountID]
	if !ok {
		var err error
		acc, err = g.repo.GetByID(ctx, accountID)
		if err != nil || acc == nil {
			return false, nil
		}
		g.accounts[accountID] = acc
	}
	if !CanProbeModelScope(acc) {
		return false, nil
	}

	var upstreamKey string
	var siblings []int64
	if acc.Platform == PlatformCursor {
		if acc.QuotaKeyID == nil || *acc.QuotaKeyID <= 0 {
			return false, nil
		}
		siblings, ok = g.siblingsByQuotaKey[*acc.QuotaKeyID]
		if !ok {
			var err error
			siblings, err = g.repo.SiblingAccountIDsByCursorUpstreamKey(ctx, g.groupID, *acc.QuotaKeyID)
			if err != nil {
				logger.L().Warn("upstream_jitter_siblings_lookup_failed",
					zap.Int64("account_id", acc.ID),
					zap.Error(err),
				)
				return false, nil
			}
			g.siblingsByQuotaKey[*acc.QuotaKeyID] = siblings
		}
		upstreamKey = fmt.Sprintf("cursor:qk:%d", minAccountID(siblings, *acc.QuotaKeyID))
	} else {
		host := upstreamHostOfAccount(acc)
		if host == "" {
			return false, nil
		}
		accounts, err := g.repo.ListByGroup(ctx, g.groupID)
		if err != nil {
			logger.L().Warn("upstream_jitter_siblings_lookup_failed",
				zap.Int64("account_id", acc.ID),
				zap.Error(err),
			)
			return false, nil
		}
		siblings = siblingAccountIDsByHost(accounts, host)
		upstreamKey = "host:" + strings.ToLower(strings.TrimSpace(host))
	}

	selectionModel := strings.TrimSpace(modelScope)
	alias := strings.TrimSpace(ChannelMappedModelFromContext(ctx))
	var stopScope, probeInput string
	if alias != "" {
		stopScope = alias
		probeInput = alias
	} else {
		stopScope = strings.TrimSpace(acc.GetMappedModel(selectionModel))
		probeInput = selectionModel
	}
	if stopScope == "" {
		return false, nil
	}
	if err := g.repo.SetModelRateLimitWithExtra(ctx, acc.ID, stopScope, time.Now().Add(upstreamJitterEntryObservationWindow), UpstreamJitterModelRateLimitReason, map[string]any{UpstreamJitterProbeInputKey: probeInput}); err != nil {
		logger.L().Warn("upstream_jitter_stop_write_failed",
			zap.Int64("account_id", acc.ID),
			zap.String("model_scope", stopScope),
			zap.Error(err),
		)
		return false, nil
	}

	key := upstreamKey + "|" + stopScope
	if g.failedByUpstream[key] == nil {
		g.failedByUpstream[key] = make(map[int64]struct{})
	}
	g.failedByUpstream[key][acc.ID] = struct{}{}
	logger.L().Warn("upstream_jitter_stop",
		zap.Int64("account_id", acc.ID),
		zap.String("upstream_key", upstreamKey),
		zap.String("model_scope", stopScope),
		zap.String("probe_input", probeInput),
		zap.Int("upstream_status", fe.StatusCode),
		zap.String("reason", string(fe.Reason)),
	)

	if len(g.failedByUpstream[key]) >= CursorUpstreamFlapThreshold && !g.skipped[key] {
		g.skipped[key] = true
		for _, id := range siblings {
			if id == acc.ID {
				continue
			}
			if _, failed := g.failedByUpstream[key][id]; !failed {
				excludeIDs = append(excludeIDs, id)
			}
		}
		logger.L().Warn("upstream_jitter_skip_upstream",
			zap.Int64("group_id", g.groupID),
			zap.String("upstream_key", upstreamKey),
			zap.String("model_scope", stopScope),
			zap.String("probe_input", probeInput),
			zap.Int("excluded_count", len(excludeIDs)),
		)
	}
	return true, excludeIDs
}

// minAccountID 取 ID 列表最小值；空列表回退到 quota key ID，
// 保证同一 quota key 下的账号共享同一个 upstreamKey。
func minAccountID(ids []int64, fallback int64) int64 {
	if len(ids) == 0 {
		return fallback
	}
	min := ids[0]
	for _, id := range ids[1:] {
		if id < min {
			min = id
		}
	}
	return min
}

func (s *GatewayService) NewUpstreamJitterGuard(groupID int64) *UpstreamJitterGuard {
	if s == nil {
		return nil
	}
	repo, ok := s.accountRepo.(UpstreamJitterRepo)
	if !ok {
		return nil
	}
	return newUpstreamJitterGuard(repo, groupID)
}

func (s *OpenAIGatewayService) NewUpstreamJitterGuard(groupID int64) *UpstreamJitterGuard {
	if s == nil {
		return nil
	}
	repo, ok := s.accountRepo.(UpstreamJitterRepo)
	if !ok {
		return nil
	}
	return newUpstreamJitterGuard(repo, groupID)
}
