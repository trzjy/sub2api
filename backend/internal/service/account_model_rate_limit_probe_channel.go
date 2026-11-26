package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// errUnsupportedModelProbe 表示当前平台无法按 scope 精确指定模型发出最小实际调用。
// 调用方据此保留 due 重排，不误清除该 scope 的限流。
var errUnsupportedModelProbe = errors.New("model rate limit probe: scoped upstream probe unsupported for this platform")

const (
	modelRateLimitProbeLeaderLockKey      = "recovery:model_probe:leader"
	modelRateLimitProbeLeaderTTL          = 360 * time.Second
	modelRateLimitProbeRoundBudget        = 240 * time.Second
	modelRateLimitProbeLockTTL            = 240 * time.Second
	modelRateLimitProbeContentionDelay    = 60 * time.Second
	modelRateLimitProbeCandidateLimit     = 200
	modelRateLimitProbeConcurrency        = 8
	modelRateLimitProbeCursorConcurrency  = 4
	modelRateLimitProbeQuotaCadence       = 5 * time.Minute
	modelRateLimitProbeUnavailableCadence = 30 * time.Second
)

// accountHasQuotaEntrance describes which accounts can enter the existing quota-aware
// model limit policy. Model recovery still requires a scope-specific signal.
func accountHasQuotaEntrance(acc *Account) bool {
	return acc.BalanceProbeConfig().Enabled ||
		acc.Platform == PlatformAntigravity ||
		(acc.Platform == PlatformCursor && acc.QuotaKeyID != nil && *acc.QuotaKeyID > 0)
}

// RunModelRateLimitRecoveryChannel 是批 2 模型级限流恢复通道：按 probe_due_at 取候选，
// 逐 (accountID, scope) 加锁，由该 scope 对应的上游数据面或专用额度来源验证恢复，
// 成功经事件版本化入口清除该 scope，失败按代际条件重排。已转由其它来源持有的旧候选
// 只删除通用探针排程字段，不改变其限流状态。
//
// 与批 1 账号级通道共用同一把账号锁（recovery:probe:<accountID>），但 leader 锁与
// 候选来源独立；两者互不影响。
func (p *AccountHealthRecoveryProbeService) RunModelRateLimitRecoveryChannel(ctx context.Context) {
	if p == nil || p.accountRepo == nil || p.recoveryLock == nil || p.httpUpstream == nil || p.cfg == nil {
		return
	}
	// 协调后端不可用/未抢到 leader → 失败关闭，本轮不探测。
	owner := newRecoveryLockOwner()
	leader, err := tryAcquireRedisLeaderLock(ctx, p.recoveryLock, modelRateLimitProbeLeaderLockKey, owner, modelRateLimitProbeLeaderTTL)
	if err != nil || !leader {
		if err != nil {
			logger.L().Warn("model_rate_limit_probe_leader_lock_unavailable", zap.Error(err))
		}
		return
	}
	defer releaseRecoveryLock(p.recoveryLock, modelRateLimitProbeLeaderLockKey, owner)

	roundCtx, cancel := context.WithTimeout(ctx, modelRateLimitProbeRoundBudget)
	defer cancel()

	lister, ok := p.accountRepo.(interface {
		ListModelRateLimitProbeDue(ctx context.Context, now time.Time, limit int) ([]ModelProbeTask, error)
	})
	if !ok {
		logger.L().Warn("model_rate_limit_probe_candidate_list_unsupported")
		return
	}
	tasks, err := lister.ListModelRateLimitProbeDue(roundCtx, time.Now(), modelRateLimitProbeCandidateLimit)
	if err != nil {
		logger.L().Warn("model_rate_limit_probe_candidate_list_failed", zap.Error(err))
		return
	}
	if len(tasks) == 0 {
		return
	}

	getter, ok := p.accountRepo.(interface {
		GetByIDs(ctx context.Context, ids []int64) ([]*Account, error)
	})
	if !ok {
		logger.L().Warn("model_rate_limit_probe_get_by_ids_unsupported")
		return
	}
	ids := make([]int64, 0, len(tasks))
	seen := make(map[int64]struct{}, len(tasks))
	for _, task := range tasks {
		if _, exists := seen[task.AccountID]; exists {
			continue
		}
		seen[task.AccountID] = struct{}{}
		ids = append(ids, task.AccountID)
	}
	accounts, err := getter.GetByIDs(roundCtx, ids)
	if err != nil {
		logger.L().Warn("model_rate_limit_probe_get_by_ids_failed", zap.Error(err))
		return
	}
	accByID := make(map[int64]*Account, len(accounts))
	for _, acc := range accounts {
		if acc != nil {
			accByID[acc.ID] = acc
		}
	}

	sem := make(chan struct{}, modelRateLimitProbeConcurrency)
	var wg sync.WaitGroup
dispatch:
	for _, task := range tasks {
		if roundCtx.Err() != nil {
			break
		}
		acc := accByID[task.AccountID]
		if acc == nil {
			continue
		}
		// 归属过滤在 Go 侧只做一次（写入侧已有同一判据）。历史候选若已转由专用来源
		// 持有，仅按原 probe_gen 移除通用排程字段，保留限流 scope 与 reset 状态。
		if !isModelProbeOwned(task.Scope, task.Entry, acc) {
			p.dropModelProbeSchedule(roundCtx, task)
			continue
		}
		limit := modelRateLimitProbeConcurrency
		if acc.Platform == PlatformCursor {
			// cursor 额度查询最长 180s，单独限并发 4，避免长查询叠加打满上游。
			limit = modelRateLimitProbeCursorConcurrency
		}
		if len(sem) >= limit {
			select {
			case <-roundCtx.Done():
				break dispatch
			case sem <- struct{}{}:
			}
		} else {
			sem <- struct{}{}
		}
		wg.Add(1)
		go func(t ModelProbeTask, a *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			p.probeModelScope(roundCtx, a, t, owner)
		}(task, acc)
	}
	wg.Wait()
}

func (p *AccountHealthRecoveryProbeService) dropModelProbeSchedule(ctx context.Context, task ModelProbeTask) {
	expectedGen, ok := modelProbeExpectedGen(task.Entry)
	if !ok {
		return
	}
	p.patchModelProbe(ctx, task.AccountID, task.Scope, expectedGen, ModelProbePatch{Op: ModelProbePatchDropSchedule})
}

// probeModelScope 对单条 (accountID, scope) 候选执行来源专属的恢复探测。
func (p *AccountHealthRecoveryProbeService) probeModelScope(ctx context.Context, acc *Account, task ModelProbeTask, owner string) {
	if p == nil || acc == nil {
		return
	}
	expectedGen, ok := modelProbeExpectedGen(task.Entry)
	if !ok {
		return
	}

	lockKey := fmt.Sprintf("recovery:probe:%d", acc.ID)
	locked, err := tryAcquireRedisLeaderLock(ctx, p.recoveryLock, lockKey, owner, modelRateLimitProbeLockTTL)
	if err != nil || !locked {
		if err != nil {
			logger.L().Warn("model_rate_limit_probe_lock_unavailable",
				zap.Int64("account_id", acc.ID), zap.String("scope", task.Scope), zap.Error(err))
		}
		p.patchModelProbe(ctx, acc.ID, task.Scope, expectedGen, ModelProbePatch{
			Op:  ModelProbePatchReschedule,
			Due: time.Now().Add(modelRateLimitProbeContentionDelay),
		})
		return
	}
	defer releaseRecoveryLock(p.recoveryLock, lockKey, owner)

	// 探测发起时刻：作为成功清除的权威 EventTime（防迟到清除覆盖更新的 429）。
	startedAt := time.Now()

	reschedule := func(d time.Duration) {
		p.patchModelProbe(ctx, acc.ID, task.Scope, expectedGen, ModelProbePatch{
			Op:  ModelProbePatchReschedule,
			Due: time.Now().Add(d),
		})
	}

	observeAttempt := func() {
		p.patchModelProbe(ctx, acc.ID, task.Scope, expectedGen, ModelProbePatch{Op: ModelProbePatchObserve, ObserveKey: entryAttemptedAtKey, Observe: time.Now().UTC().Format(time.RFC3339Nano)})
	}

	clearScope := func() {
		// TieBreaker 固定传 0：入口按当前 meta revision + 1 分配，无需读取 revision。
		applied, cerr := p.rateLimit.ApplyModelRateLimitObservationIfVersion(ctx, acc.ID, task.Scope, ModelRateLimitObservation{
			EventTime:  startedAt,
			Outcome:    ProbeOutcomeSuccess,
			TieBreaker: 0,
		}, task.MetaLastEventAt)
		if cerr != nil {
			// 本轮不重排：条目仍在，下一轮候选自然重试。
			logger.L().Warn("model_rate_limit_probe_clear_failed",
				zap.Int64("account_id", acc.ID), zap.String("scope", task.Scope), zap.Error(cerr))
			return
		}
		if !applied {
			logger.L().Info("model_rate_limit_probe_generation_takenover",
				zap.Int64("account_id", acc.ID), zap.String("scope", task.Scope))
			return
		}
		p.clearCandidateBound(acc.ID, task.Scope)
		if p.channelFreshness != nil {
			// 沿用 afterRecovered 的刷新代码，但不得调用 afterRecovered（账号级副作用）。
			freshCtx, cf := context.WithTimeout(context.Background(), 5*time.Second)
			defer cf()
			if err := p.channelFreshness.RefreshChannelFreshnessForAccount(freshCtx, acc.ID); err != nil {
				logger.L().Warn("account_recovery_channel_freshness_refresh_failed",
					zap.Int64("account_id", acc.ID), zap.Error(err))
			}
		}
		logger.L().Info("model_rate_limit_probe_recovered",
			zap.Int64("account_id", acc.ID), zap.String("scope", task.Scope))
	}

	// 上游抖动停调：只看真实调用探测，不查额度/余额（额度剩余 ≠ 该模型恢复），
	// 失败按 15 秒续探；成功走既有版本化清除。
	if reason, _ := task.Entry["reason"].(string); reason == UpstreamJitterModelRateLimitReason {
		probeInput := strings.TrimSpace(task.Scope)
		if raw, ok := task.Entry[UpstreamJitterProbeInputKey].(string); ok && strings.TrimSpace(raw) != "" {
			probeInput = strings.TrimSpace(raw)
		}
		model := ""
		if acc.IsCursorConnectSessionAccount() {
			model = strings.TrimSpace(acc.GetMappedModel(probeInput))
		} else {
			model = strings.TrimSpace(resolveOpenAIAccountUpstreamModelForRequest(acc, probeInput, false))
		}
		if model == "" {
			model = probeInput
		}
		upOK, upErr := p.probeModelScopeResolvedModel(ctx, acc, model)
		if upErr != nil || !upOK {
			observeAttempt()
			reschedule(UpstreamJitterProbeCadence)
			return
		}
		clearScope()
		return
	}

	// 模型 scope 必须由该模型自己的上游信号证明恢复。precise_reset 只描述
	// reset_at 是否精确，不改变观测证据的作用域；账号余额和 Cursor 激活码额度
	// 不能单独证明普通模型已恢复。
	upOK, upErr := p.probeModelScopeUpstream(ctx, acc, task.Scope)
	if upErr != nil || !upOK {
		observeAttempt()
		cadence := modelRateLimitProbeUnavailableCadence
		if acc.Platform == PlatformAntigravity {
			cadence = modelRateLimitProbeQuotaCadence
		}
		reschedule(cadence)
		return
	}
	clearScope()
}

// probeModelScopeUpstream 使用传入的模型 scope 选择该平台对应的数据面或用户额度观测。
func (p *AccountHealthRecoveryProbeService) probeModelScopeUpstream(ctx context.Context, acc *Account, scope string) (bool, error) {
	if p == nil || acc == nil {
		return false, errors.New("model rate limit probe: account is nil")
	}

	if acc.Platform == PlatformAntigravity {
		if p.antigravityQuota == nil || p.antigravityTokens == nil {
			return false, errors.New("antigravity model quota probe is not wired")
		}
		proxyURL := ""
		if acc.Proxy != nil {
			proxyURL = acc.Proxy.URL()
		}
		if scope == creditsExhaustedKey {
			return p.antigravityQuota.ProbeAICredits(ctx, acc, proxyURL, p.antigravityTokens)
		}
		return p.antigravityQuota.ProbeUserQuota(ctx, acc, proxyURL, scope, p.antigravityTokens)
	}
	if acc.Platform == PlatformOpenAI && scope == openAIImageGenerationRateLimitKey {
		if p.balanceProbe == nil {
			return false, errors.New("openai image balance probe is not wired")
		}
		result, err := p.balanceProbe.Query(ctx, acc.ID)
		if err != nil {
			return false, err
		}
		if result == nil {
			return false, errors.New("openai image balance probe returned nil result")
		}
		return result.Success && result.Valid && result.Remaining != nil && *result.Remaining > 0, nil
	}

	if isCodeBuddyPlatformAccount(acc) || isCodeBuddyShadowAccount(acc) {
		return p.probeCodeBuddyChatUpstream(ctx, acc, scope)
	}

	if acc.Platform == PlatformCursor {
		if acc.IsCursorConnectSessionAccount() {
			return p.runCursorConnectProbeForModel(ctx, acc, strings.TrimSpace(scope))
		}
		return p.probeUpstreamDataPlaneWithProtocolForModel(ctx, acc, scope,
			shouldForwardOpenAIResponsesViaRawChatCompletions(acc))
	}

	if acc.Platform == PlatformDevin {
		return p.probeDevinUpstreamForModel(ctx, acc, scope)
	}

	// scope 已是模型限流写入端保存的映射后名称；不得再按账号 model_mapping 映射一次。
	return p.probeUpstreamDataPlaneWithProtocolForModel(ctx, acc, scope, false)
}

// probeModelScopeResolvedModel 对已解析为实际上游模型的 model 发出最小真实调用。
// 调用方负责把 scope / probe_input 解析成 model（映射只做一次），本函数不再做二次映射。
func (p *AccountHealthRecoveryProbeService) probeModelScopeResolvedModel(ctx context.Context, acc *Account, model string) (bool, error) {
	if acc == nil {
		return false, errors.New("probe account is nil")
	}
	switch acc.Platform {
	case PlatformAntigravity, PlatformDevin, PlatformCodeBuddy:
		return false, errUnsupportedModelProbe
	case PlatformCursor:
		return p.probeCursorModelScopeUpstream(ctx, acc, model)
	}
	return p.probeUpstreamDataPlaneWithProtocolForModel(ctx, acc, model, false)
}

// probeCursorModelScopeUpstream 对 cursor 平台按指定上游模型发一次最小真实调用。
// Connect 会话号走既有原生 AgentRun 探针；apikey 反代走既有数据面探测（强制 CC，
// 与真实转发同判据）。cannot-probe 时返回 error，由调用方保留 due 重排、不误清。
func (p *AccountHealthRecoveryProbeService) probeCursorModelScopeUpstream(ctx context.Context, acc *Account, scope string) (bool, error) {
	if acc == nil {
		return false, errors.New("probe account is nil")
	}
	if acc.IsCursorConnectSessionAccount() {
		if p.cursorExec == nil {
			return false, errors.New("cursor execution service not configured for connect-session model scope probe")
		}
		fn := p.cursorAgentRunProbe
		if fn == nil {
			fn = ProbeCursorAccountHealth
		}
		status, _, text, err := fn(ctx, p.cursorExec, acc, scope, cursorAccountHealthProbeMinBudget)
		if err != nil {
			return false, err
		}
		return status == http.StatusOK && strings.TrimSpace(text) != "", nil
	}
	return p.probeUpstreamDataPlaneWithProtocolForModel(ctx, acc, scope, true)
}

// patchModelProbe 是 PatchModelRateLimitEntry 的窄面薄封装：未实现时 Warn 并返回，不 panic。
func (p *AccountHealthRecoveryProbeService) patchModelProbe(ctx context.Context, accountID int64, scope string, expectedGen int64, patch ModelProbePatch) {
	if p == nil || p.accountRepo == nil {
		return
	}
	patcher, ok := p.accountRepo.(interface {
		PatchModelRateLimitEntry(ctx context.Context, accountID int64, scope string, expectedGen int64, patch ModelProbePatch) (bool, error)
	})
	if !ok {
		logger.L().Warn("model_rate_limit_probe_patch_unsupported",
			zap.Int64("account_id", accountID), zap.String("scope", scope))
		return
	}
	if _, err := patcher.PatchModelRateLimitEntry(ctx, accountID, scope, expectedGen, patch); err != nil {
		logger.L().Warn("model_rate_limit_probe_patch_failed",
			zap.Int64("account_id", accountID), zap.String("scope", scope), zap.Error(err))
	}
}

// modelProbeExpectedGen 兼容 float64（JSON 解码默认）/int64/json.Number 三种 probe_gen 形态。
func modelProbeExpectedGen(entry map[string]any) (int64, bool) {
	raw, ok := entry["probe_gen"]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, true
		}
		if f, err := v.Float64(); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}
