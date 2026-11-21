package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"go.uber.org/zap"
)

// errUnsupportedModelProbe 表示当前平台无法按 scope 精确指定模型发出最小实际调用。
// 调用方据此保留 due 重排，不误清除该 scope 的限流（方案 §2.6 / 残余风险 2）。
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

// accountHasQuotaEntrance 判定账号是否具备额度探测入口（方案 §2.4 / §2.6）。
func accountHasQuotaEntrance(acc *Account) bool {
	return acc.BalanceProbeConfig().Enabled ||
		acc.Platform == PlatformAntigravity ||
		(acc.Platform == PlatformCursor && acc.QuotaKeyID != nil && *acc.QuotaKeyID > 0)
}

// RunModelRateLimitRecoveryChannel 是批 2 模型级限流恢复通道：按 probe_due_at 取候选，
// 逐 (accountID, scope) 加锁，按「有/无额度入口 × 有/无回显」四格分支探测，成功经
// 事件版本化入口清除该 scope，失败按代际条件重排。
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
		// 归属过滤在 Go 侧只做一次（写入侧已有同一判据）。
		if !isModelProbeOwned(task.Scope, task.Entry, acc) {
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

// probeModelScope 对单条 (accountID, scope) 候选执行四格分支探测。
func (p *AccountHealthRecoveryProbeService) probeModelScope(ctx context.Context, acc *Account, task ModelProbeTask, owner string) {
	if p == nil || acc == nil {
		return
	}
	expectedGen, ok := modelProbeExpectedGen(task.Entry)
	if !ok {
		return
	}
	precise, _ := task.Entry["precise_reset"].(bool)

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

	if !accountHasQuotaEntrance(acc) {
		// 无额度入口：到点实际调用，2xx 才清除；失败/不确定 30s 续探。
		upOK, upErr := p.probeModelScopeUpstream(ctx, acc, task.Scope)
		if upErr != nil || !upOK {
			observeAttempt()
			reschedule(modelRateLimitProbeUnavailableCadence)
			return
		}
		clearScope()
		return
	}

	// 有额度入口：先走额度信号；无回显（precise=false）额度通过即清除。
	quotaOK, quotaErr := p.probeQuotaSignal(ctx, acc)
	if quotaErr != nil || !quotaOK {
		observeAttempt()
		reschedule(modelRateLimitProbeQuotaCadence)
		return
	}
	if !precise {
		clearScope()
		return
	}
	// 有回显：额度 + 实际调用两项都过才清除，任一项失败 5min 重排。
	upOK, upErr := p.probeModelScopeUpstream(ctx, acc, task.Scope)
	if upErr != nil || !upOK {
		observeAttempt()
		reschedule(modelRateLimitProbeQuotaCadence)
		return
	}
	clearScope()
}

// probeModelScopeUpstream 对能精确指定 scope 模型的平台发出最小实际调用。
// 不能精确指定的平台返回 errUnsupportedModelProbe，由调用方保留 due 重排、不误清除。
func (p *AccountHealthRecoveryProbeService) probeModelScopeUpstream(ctx context.Context, acc *Account, scope string) (bool, error) {
	if p == nil || acc == nil {
		return false, errUnsupportedModelProbe
	}

	if isCodeBuddyShadowAccount(acc) {
		body, err := PrepareCodeBuddyBody(codeBuddyShadowProbeBody(scope), CodeBuddyRewriteOptions{
			Sanitize: p.codeBuddyProbeSanitizeEnabled(),
			Model:    scope,
		})
		if err != nil {
			return false, fmt.Errorf("prepare model scope codebuddy probe body: %w", err)
		}
		// 影子→母账号凭证解析：取法与 probeCodeBuddyShadowUpstream 一致（fail-closed）。
		credAccount := acc
		if acc.ParentAccountID != nil {
			parent, err := p.accountRepo.GetByID(ctx, *acc.ParentAccountID)
			if err != nil {
				return false, fmt.Errorf("resolve codebuddy shadow parent: %w", err)
			}
			if parent == nil || parent.IsShadow() {
				return false, fmt.Errorf("codebuddy shadow parent %d missing or itself a shadow", *acc.ParentAccountID)
			}
			if !(parent.IsCodeBuddy() && (parent.Type == AccountTypeOAuth || parent.Type == AccountTypeAPIKey)) {
				return false, fmt.Errorf("codebuddy shadow parent %d is not a codebuddy OAuth/APIKey account", parent.ID)
			}
			credAccount = parent
		}
		token := codeBuddyTestAccessToken(credAccount)
		if token == "" {
			return false, errors.New("codebuddy credential account is missing access_token credential")
		}
		req, err := buildCodeBuddyShadowProbeRequest(ctx, credAccount, body, token, p.cfg)
		if err != nil {
			return false, fmt.Errorf("build model scope codebuddy probe request: %w", err)
		}
		proxyURL := ""
		if acc.ProxyID != nil && acc.Proxy != nil {
			proxyURL = acc.Proxy.URL()
		}
		var tlsProfile *tlsfingerprint.Profile
		if p.tlsFPProfileService != nil {
			tlsProfile = p.tlsFPProfileService.ResolveTLSProfile(acc)
		}
		callCtx, cancel := context.WithTimeout(ctx, codeBuddyShadowProbeTimeout)
		defer cancel()
		resp, err := p.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, acc.ID, acc.Concurrency, tlsProfile)
		if err != nil {
			return false, nil
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, codeBuddyShadowProbeMaxBodyBytes))
		return evaluateCodeBuddyShadowProbeResponse(resp.StatusCode, raw), nil
	}

	// antigravity 无可用实际调用构造；cursor/devin 多模型遍历、codebuddy 直绑只有
	// billing 面——均无法按 scope 精确指定模型，保留 due 重排，不伪造桩实现。
	switch acc.Platform {
	case PlatformAntigravity, PlatformCursor, PlatformDevin, PlatformCodeBuddy:
		return false, errUnsupportedModelProbe
	}

	// 其它 OpenAI 协议族：按 scope 解析实际上游模型，空则直接用 scope。
	model := resolveOpenAIAccountUpstreamModelForRequest(acc, scope, false)
	if strings.TrimSpace(model) == "" {
		model = scope
	}
	return p.probeUpstreamDataPlaneWithProtocolForModel(ctx, acc, model, false)
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
