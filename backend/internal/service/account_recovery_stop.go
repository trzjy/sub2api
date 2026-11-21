package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// 账号级停调恢复探测（批1）的服务侧写入口。
//
// 分两类：
//   - quota（429 有/无回显、过载）：只写 probe_due_at 排程，不写 temp_unschedulable_until
//     —— 写 until 会让母账号 global 429 经 IsCredentialUsableForShadow 连坐影子账号；
//   - transport（503 账号级、5xx/502/非 JSON）：写 until + 结构化 reason + 排程，
//     影子账号按既有语义连坐。

const (
	recoveryModeEcho        = "echo"
	recoveryModeNoEcho      = "noecho"
	recoveryModeOverload    = "overload"
	recoveryModeTransient   = "transient"
	recoveryModeUnavailable = "unavailable"

	recoveryKindQuota  = "quota"
	recoveryKindActual = "actual"
	recoveryKindBoth   = "both"

	recoveryQuotaProbeCadence    = 5 * time.Minute
	recoveryTransientCadence     = 30 * time.Second
	recoveryUnavailableCadence   = 5 * time.Minute
	recoveryProbeReasonBodyLimit = 512
)

// accountRecoveryRepository 是批1 恢复探测需要的窄面仓储接口。
// 真实实现见 repository.accountRepository；测试桩只需实现本接口即可。
type accountRecoveryRepository interface {
	SetQuotaProbeSchedule(ctx context.Context, accountID int64, due time.Time, mode, kind string) (bool, error)
	SetTransportRecoveryStop(ctx context.Context, accountID int64, until, due time.Time, mode, reasonJSON string) (bool, error)
}

// registerAccountScopedStop 是 5xx 的唯一登记入口（HandleUpstreamError 最前）。
//
// 判据与状态写入都收在这里：503 账号级 → transport 5 分钟；其余 5xx/非 JSON →
// transport 30 秒；请求级容量降载不登记。不以 RetryableOnSameAccount 为前置。
func (s *RateLimitService) registerAccountScopedStop(ctx context.Context, account *Account, statusCode int, responseBody []byte) {
	if s == nil || account == nil || statusCode < http.StatusInternalServerError {
		return
	}
	repo, ok := s.accountRepo.(accountRecoveryRepository)
	if !ok {
		slog.Warn("account_recovery_probe_repo_unavailable", "account_id", account.ID, "status_code", statusCode)
		return
	}
	now := time.Now()
	var mode string
	var cadence time.Duration
	switch {
	case IsAccountScopedUpstreamUnavailable(statusCode, responseBody, ""):
		mode, cadence = recoveryModeUnavailable, recoveryUnavailableCadence
	case IsAccountScopedTransientUpstreamError(statusCode, responseBody, ""):
		mode, cadence = recoveryModeTransient, recoveryTransientCadence
	default:
		return
	}
	until := now.Add(cadence)
	reason := buildAccountRecoveryReasonJSON(mode, statusCode, responseBody, now)
	applied, err := repo.SetTransportRecoveryStop(ctx, account.ID, until, until, mode, reason)
	if err != nil {
		slog.Warn("account_recovery_probe_set_failed", "account_id", account.ID, "status_code", statusCode, "error", err)
		return
	}
	if !applied {
		slog.Info("account_recovery_probe_set_skipped", "account_id", account.ID, "status_code", statusCode)
	}
}

// scheduleQuotaRecoveryProbe 为额度类停调（429/过载）排一次探测。
// 不写 until/reason：额度停调由 rate_limit_reset_at / overload_until 表达。
func (s *RateLimitService) scheduleQuotaRecoveryProbe(ctx context.Context, account *Account, dueAt time.Time, mode string) {
	if s == nil || account == nil || dueAt.IsZero() {
		return
	}
	repo, ok := s.accountRepo.(accountRecoveryRepository)
	if !ok {
		slog.Warn("account_recovery_probe_repo_unavailable", "account_id", account.ID, "mode", mode)
		return
	}
	kind := s.quotaProbeKindFor(account)
	applied, err := repo.SetQuotaProbeSchedule(ctx, account.ID, dueAt, mode, kind)
	if err != nil {
		slog.Warn("account_recovery_probe_schedule_failed", "account_id", account.ID, "mode", mode, "error", err)
		return
	}
	if !applied {
		slog.Info("account_recovery_probe_schedule_skipped", "account_id", account.ID, "mode", mode)
	}
}

// quotaProbeKindFor 决定额度类停调用哪种探测：有底层真实额度入口走 quota，
// 否则退化为实际调用（用户裁定 5）。
func (s *RateLimitService) quotaProbeKindFor(account *Account) string {
	if account == nil {
		return recoveryKindActual
	}
	if account.BalanceProbeConfig().Enabled {
		return recoveryKindQuota
	}
	if account.Platform == PlatformAntigravity {
		return recoveryKindQuota
	}
	if account.Platform == PlatformCursor && account.QuotaKeyID != nil && *account.QuotaKeyID > 0 {
		return recoveryKindQuota
	}
	return recoveryKindActual
}

// buildAccountRecoveryReasonJSON 构造 transport 类停调的结构化 reason：
// source=recovery_probe 与 ClearTempUnschedulableIfOwned 的来源约束一一对应；
// 错误原文按 512 字截断后写入（json.Marshal 会转义引号，不会伪造 source 字面量）。
func buildAccountRecoveryReasonJSON(mode string, statusCode int, responseBody []byte, now time.Time) string {
	message := string(responseBody)
	if len(message) > recoveryProbeReasonBodyLimit {
		message = message[:recoveryProbeReasonBodyLimit]
	}
	state := TempUnschedState{
		TriggeredAtUnix: now.Unix(),
		StatusCode:      statusCode,
		MatchedKeyword:  "account_recovery_generic",
		ErrorMessage:    message,
		Source:          accountRecoveryProbeSource,
		RecoveryMode:    mode,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	return string(raw)
}
