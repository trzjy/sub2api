package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// PoolMode401EscalationMarker 池模式账号 401 窗口升级停调的标记词，写入
// TempUnschedState.MatchedKeyword 随停调原因持久化；探测恢复（CARD-C）据此把该类
// 停调纳入可探测恢复范围。全仓单一定义（R1-F4：marker 字面量双 SSOT 禁止）。
const PoolMode401EscalationMarker = "pool_mode_401_escalation"

// 阈值/窗口/冷却硬编码（方案 T5：不加设置项，YAGNI，运行观察后再议）：
// 5 分钟窗口内累计 6 次 401（≈2 个请求的同号重试全灭）→ 停调 30 分钟。
const (
	pool401EscalationWindow    = 5 * time.Minute
	pool401EscalationThreshold = 6
	pool401EscalationCooldown  = 30 * time.Minute
	// 持久化预算与熔断 L3 trip 路径同款（tripAccountForHealth 的 3s 超时先例）。
	pool401EscalationPersistBudget = 3 * time.Second
)

// pool401WindowEntry 是单账号的 401 窗口状态（进程内内存态；单实例部署为前提，
// 方案 §7 部署前置核验项）。
type pool401WindowEntry struct {
	windowStart time.Time
	count       int
	// tripped 标记本 epoch 已成功持久化停调（边沿已消费）；同 epoch 内后续 401
	// 仅计数，不重复 SetTempUnschedulable、不重复告警（R2-F4 边沿幂等）。
	tripped bool
	// escalating 标记本 epoch 有一个升级动作在途：锁内置位，保证并发 401 只有一个
	// 升级在途；持久化成功锁存 tripped，失败清回 false（R6-F2 失败不锁存边沿）。
	escalating bool
}

// pool401WindowState 进程内 per-account 401 窗口计数器（先例：openAIAccountModelTransientState）。
type pool401WindowState struct {
	mu      sync.Mutex
	entries map[int64]*pool401WindowEntry
}

var pool401Windows = &pool401WindowState{entries: make(map[int64]*pool401WindowEntry)}

// record 记一次 401 并返回 (窗口内计数, 是否应由本调用执行升级动作)。
// 窗口自旋（R4-F3）：now 超出窗口（或时钟回拨）开新 epoch（计数=1、tripped=false）
// ——到期恢复（无探测）路径靠窗口自然轮转在 ≤5min 内自愈，无需额外清理。
func (w *pool401WindowState) record(accountID int64, now time.Time) (int, bool) {
	if w == nil || accountID <= 0 {
		return 0, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.entries == nil {
		w.entries = make(map[int64]*pool401WindowEntry)
	}
	entry := w.entries[accountID]
	if entry == nil || now.Sub(entry.windowStart) > pool401EscalationWindow || now.Before(entry.windowStart) {
		entry = &pool401WindowEntry{windowStart: now}
		w.entries[accountID] = entry
	}
	entry.count++
	if entry.count >= pool401EscalationThreshold && !entry.tripped && !entry.escalating {
		entry.escalating = true
		return entry.count, true
	}
	return entry.count, false
}

// commitEscalation 持久化成功后锁存边沿：同 epoch 内不再重复升级/告警。
func (w *pool401WindowState) commitEscalation(accountID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry := w.entries[accountID]; entry != nil {
		entry.tripped = true
		entry.escalating = false
	}
}

// abortEscalation 持久化失败后回滚在途标记（不置 tripped）——同窗口下一个 401
// 会重试升级动作（R6-F2）。
func (w *pool401WindowState) abortEscalation(accountID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry := w.entries[accountID]; entry != nil {
		entry.escalating = false
	}
}

// resetPool401State 唯一重置入口（R4-F3）：清计数与 tripped epoch。调度器成功
// 观察点与 probe 成功恢复出口调用——否则恢复后同一 epoch 内再次 401 不再跨越
// 边沿，账号失去保护。
func resetPool401State(accountID int64) {
	if accountID <= 0 {
		return
	}
	pool401Windows.mu.Lock()
	delete(pool401Windows.entries, accountID)
	pool401Windows.mu.Unlock()
}

// notePool401 在池模式 401 豁免点记账并按阈值边沿触发临时停调。单次 401 仍不罚
// 账号（既有认证语义不变，方案 T5 不变量）。
func (s *RateLimitService) notePool401(ctx context.Context, account *Account, now time.Time) {
	if s == nil || s.accountRepo == nil || account == nil || account.ID <= 0 {
		return
	}
	count, shouldEscalate := pool401Windows.record(account.ID, now)
	if !shouldEscalate {
		return
	}

	until := now.Add(pool401EscalationCooldown)
	state := pool401EscalationState(now, count, until)
	reasonBytes, _ := json.Marshal(state)
	reason := string(reasonBytes)
	if reason == "" {
		reason = PoolMode401EscalationMarker
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pool401EscalationPersistBudget)
	defer cancel()
	if err := s.accountRepo.SetTempUnschedulable(persistCtx, account.ID, until, reason); err != nil {
		// R6-F2：持久化失败不锁存边沿——不置 tripped、不告警，仅 WARN 记账。
		pool401Windows.abortEscalation(account.ID)
		slog.Warn("pool_mode_401_escalation_persist_failed", "account_id", account.ID, "count", count, "error", err)
		return
	}
	pool401Windows.commitEscalation(account.ID)

	// 复用熔断 L3 trip 的既有生效管线：进程内调度守卫 + 临时停调缓存与 DB 同步。
	if account.TempUnschedulableUntil == nil || account.TempUnschedulableUntil.Before(until) {
		account.TempUnschedulableUntil = &until
		account.TempUnschedulableReason = reason
	}
	s.notifyAccountSchedulingBlocked(account, until, PoolMode401EscalationMarker)
	if s.tempUnschedCache != nil {
		if err := s.tempUnschedCache.SetTempUnsched(persistCtx, account.ID, state); err != nil {
			slog.Warn("pool_mode_401_escalation_cache_failed", "account_id", account.ID, "error", err)
		}
	}
	logger.L().Warn("pool_mode_401_escalation_tripped",
		zap.Int64("account_id", account.ID),
		zap.Int("count", count),
		zap.Int("threshold", pool401EscalationThreshold),
		zap.Duration("window", pool401EscalationWindow),
		zap.Duration("cooldown", pool401EscalationCooldown),
		zap.Time("until", until),
	)
	s.recordPool401EscalationAlert(ctx, account, count)
}

// pool401EscalationState 组装停调状态：TempUnschedState（MatchedKeyword=
// PoolMode401EscalationMarker、Tier=3），与熔断 L3 trip 的可解析格式同源，
// 供探测恢复（CARD-C）按 marker 识别。同一 state 序列化后作停调原因持久化。
func pool401EscalationState(now time.Time, count int, until time.Time) *TempUnschedState {
	return &TempUnschedState{
		UntilUnix:            until.Unix(),
		TriggeredAtUnix:      now.Unix(),
		StatusCode:           http.StatusUnauthorized,
		MatchedKeyword:       PoolMode401EscalationMarker,
		RuleIndex:            -1,
		ErrorMessage:         fmt.Sprintf("pool mode accumulated %d upstream 401s within %d minute(s)", count, int(pool401EscalationWindow.Minutes())),
		TriggerCount:         int64(count),
		TriggerThreshold:     pool401EscalationThreshold,
		TriggerWindowMinutes: int(pool401EscalationWindow.Minutes()),
		Tier:                 HealthBreakerTierTrip,
	}
}

// recordPool401EscalationAlert 写一条可查询的 P1 告警（既有 OpsRepository.
// CreateAlertEvent 管线，形状与熔断 recordHealthWarningAlert 同源）。best-effort：
// 缺 ops 仓库或写入失败不影响停调本身。
func (s *RateLimitService) recordPool401EscalationAlert(ctx context.Context, account *Account, count int) {
	if s == nil || s.opsRepo == nil {
		return
	}
	metric := float64(count)
	threshold := float64(pool401EscalationThreshold)
	event := &OpsAlertEvent{
		Severity:       "P1",
		Status:         OpsAlertStatusFiring,
		Title:          "池模式 401 升级停调",
		Description:    fmt.Sprintf("account %d (pool mode) accumulated %d upstream 401s within %d min; temporarily unschedulable for %d min", account.ID, count, int(pool401EscalationWindow.Minutes()), int(pool401EscalationCooldown.Minutes())),
		MetricValue:    &metric,
		ThresholdValue: &threshold,
		Dimensions:     map[string]any{"account_id": account.ID, "platform": account.Platform, "reason": PoolMode401EscalationMarker},
		FiredAt:        time.Now(),
	}
	if _, err := s.opsRepo.CreateAlertEvent(ctx, event); err != nil {
		slog.Warn("pool_mode_401_escalation_alert_failed", "account_id", account.ID, "error", err)
	}
}
