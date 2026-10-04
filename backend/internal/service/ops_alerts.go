package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func (s *OpsService) ListAlertRules(ctx context.Context) ([]*OpsAlertRule, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return []*OpsAlertRule{}, nil
	}
	return s.opsRepo.ListAlertRules(ctx)
}

func (s *OpsService) CreateAlertRule(ctx context.Context, rule *OpsAlertRule) (*OpsAlertRule, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if rule == nil {
		return nil, infraerrors.BadRequest("INVALID_RULE", "invalid rule")
	}

	created, err := s.opsRepo.CreateAlertRule(ctx, rule)
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *OpsService) UpdateAlertRule(ctx context.Context, rule *OpsAlertRule) (*OpsAlertRule, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if rule == nil || rule.ID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_RULE", "invalid rule")
	}

	updated, err := s.opsRepo.UpdateAlertRule(ctx, rule)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, infraerrors.NotFound("OPS_ALERT_RULE_NOT_FOUND", "alert rule not found")
		}
		return nil, err
	}
	return updated, nil
}

func (s *OpsService) DeleteAlertRule(ctx context.Context, id int64) error {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return err
	}
	if s.opsRepo == nil {
		return infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if id <= 0 {
		return infraerrors.BadRequest("INVALID_RULE_ID", "invalid rule id")
	}
	if err := s.opsRepo.DeleteAlertRule(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return infraerrors.NotFound("OPS_ALERT_RULE_NOT_FOUND", "alert rule not found")
		}
		return err
	}
	return nil
}

func (s *OpsService) ListAlertEvents(ctx context.Context, filter *OpsAlertEventFilter) ([]*OpsAlertEvent, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return []*OpsAlertEvent{}, nil
	}
	return s.opsRepo.ListAlertEvents(ctx, filter)
}

func (s *OpsService) GetAlertEventByID(ctx context.Context, eventID int64) (*OpsAlertEvent, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if eventID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_EVENT_ID", "invalid event id")
	}
	ev, err := s.opsRepo.GetAlertEventByID(ctx, eventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, infraerrors.NotFound("OPS_ALERT_EVENT_NOT_FOUND", "alert event not found")
		}
		return nil, err
	}
	if ev == nil {
		return nil, infraerrors.NotFound("OPS_ALERT_EVENT_NOT_FOUND", "alert event not found")
	}
	return ev, nil
}

func (s *OpsService) GetActiveAlertEvent(ctx context.Context, ruleID int64) (*OpsAlertEvent, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if ruleID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_RULE_ID", "invalid rule id")
	}
	return s.opsRepo.GetActiveAlertEvent(ctx, ruleID)
}

// GetActiveFreshnessAlert 按维度返回当前 firing 的陈旧告警（无则 nil）。用于状态新鲜度
// 陈旧告警的「同维度查/关」。按完整 dims 走仓储维度精确过滤（DimensionExact），
// 不做固定上限的内存匹配——活跃事件数超过任何上限都不影响目标维度命中/穷尽判定。
// 受监控开关门禁（管理端语义）：开关关闭时返回 ErrOpsDisabled，由管理端决定是否启用监控。
func (s *OpsService) GetActiveFreshnessAlert(ctx context.Context, dims map[string]any) (*OpsAlertEvent, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if len(dims) == 0 {
		return nil, infraerrors.BadRequest("INVALID_DIMS", "empty dimensions")
	}
	return s.getActiveFreshnessAlertUnchecked(ctx, dims)
}

// getActiveFreshnessAlertUnchecked 按维度返回当前 firing 的陈旧告警（不受监控开关门禁约束）。
// 监控开关关闭时恢复关闭窄面 ResolveFreshnessAlertOnRecovery 仍须走此查询关闭既有 firing 告警；
// 管理端公共面 GetActiveFreshnessAlert 门禁后的实现体与恢复窄面共用同一查询逻辑，不得出现两份
// 同源查询正文。
func (s *OpsService) getActiveFreshnessAlertUnchecked(ctx context.Context, dims map[string]any) (*OpsAlertEvent, error) {
	events, err := s.opsRepo.ListAlertEvents(ctx, &OpsAlertEventFilter{
		Status:         OpsAlertStatusFiring,
		DimensionExact: dims,
	})
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		if ev == nil {
			continue
		}
		return ev, nil
	}
	return nil, nil
}

// ListActiveFreshnessAlerts 列出当前 firing 的账号+模型维新鲜度告警（孤儿清扫使用，E45/E46）。
// 与 getActiveFreshnessAlertUnchecked 同款查询模式（直接走 s.opsRepo.ListAlertEvents，不受监控开关
// 门禁约束，firing 告警存在与否与监控开关无关，关闭孤儿维度不应因开关回滚，与恢复关闭窄面同口径）；
// Go 侧按 dims[kind]==account_model 过滤（DimensionExact 仅支持完整维度精确匹配，无法按单键 kind
// 部分匹配，故在内存侧过滤）。不出现两份同源查询正文：SQL 查询本体只在 s.opsRepo.ListAlertEvents。
//
// E46 完整游标分页：repo 默认 limit=100、上限 500（ops_repo_alerts.go），firing 告警总数超限时按
// ORDER BY fired_at DESC, id DESC 占据前窗的渠道/其他告警会使后续的账号+模型维新鲜度告警永远不进
// 单页查询。故循环消费完整游标分页直到耗尽，每页
// OpsAlertEventFilter{Status: OpsAlertStatusFiring, Limit: 500, BeforeFiredAt, BeforeID}，cursor 语义
// = fired_at DESC, id DESC（见 ops_alert_models.go 注释与 ops_repo_alerts.go ORDER BY）。首页
// BeforeFiredAt/BeforeID 传 nil；每页在内存侧按 kind 过滤追加，完整分页后不再有窗口外遗漏。
// 退出条件（防死循环）：页事件数 0 → 耗尽退出；页事件数 < 本页 limit → 已到末页退出；若 cursor
// 指针未推进（末事件 fired_at 零值或 ID<=0 却仍满页）则退出并报错，避免无限循环。
func (s *OpsService) ListActiveFreshnessAlerts(ctx context.Context) ([]*OpsAlertEvent, error) {
	if s.opsRepo == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	const pageSize = 500
	out := make([]*OpsAlertEvent, 0, 16)
	var beforeFiredAt *time.Time
	var beforeID *int64
	for {
		events, err := s.opsRepo.ListAlertEvents(ctx, &OpsAlertEventFilter{
			Status:        OpsAlertStatusFiring,
			Limit:         pageSize,
			BeforeFiredAt: beforeFiredAt,
			BeforeID:      beforeID,
		})
		if err != nil {
			return nil, err
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev == nil {
				continue
			}
			if kind, _ := ev.Dimensions[freshnessDimKind].(string); kind == freshnessDimAccountModel {
				out = append(out, ev)
			}
		}
		if int64(len(events)) < int64(pageSize) {
			break
		}
		last := events[len(events)-1]
		// 防死循环：cursor 指针未推进（末事件 fired_at 零值或 ID<=0）却仍满页 → 退出并报错。
		if last.FiredAt.IsZero() || last.ID <= 0 {
			return nil, fmt.Errorf("ops: ListActiveFreshnessAlerts cursor did not advance (last event id=%d)", last.ID)
		}
		bf := last.FiredAt
		bi := last.ID
		beforeFiredAt = &bf
		beforeID = &bi
	}
	return out, nil
}

// ResolveFreshnessAlertOnRecovery 不受监控开关门禁约束的恢复关闭窄面：在状态回升（限流条目清除
// / 观测到成功）的同一原子状态变更内关闭对应维度的 firing 告警。监控开关关闭时恢复事务不应因无关
// 的功能开关回滚（方案 account-channel-freshness v20:235 已批准语义「恢复迁移按同一原子状态变更
// 关闭对应告警」——告警关闭失败只应是存储故障，不应是功能开关）。无活动告警返回 nil（正常空结果）；
// opsRepo 不可用返回 ServiceUnavailable；实际存储故障返回错误失败关闭。内部查询/关闭逻辑复用
// GetActiveFreshnessAlert / UpdateAlertEventStatus 的未门禁私有 helper，与公共面同源。
func (s *OpsService) ResolveFreshnessAlertOnRecovery(ctx context.Context, dims map[string]any) error {
	if s.opsRepo == nil {
		return infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if len(dims) == 0 {
		return infraerrors.BadRequest("INVALID_DIMS", "empty dimensions")
	}
	active, err := s.getActiveFreshnessAlertUnchecked(ctx, dims)
	if err != nil {
		return err
	}
	if active == nil {
		return nil
	}
	resolvedAt := time.Now()
	return s.updateAlertEventStatusResolvedUnchecked(ctx, active.ID, OpsAlertStatusResolved, &resolvedAt)
}

func (s *OpsService) CreateAlertSilence(ctx context.Context, input *OpsAlertSilence) (*OpsAlertSilence, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if input == nil {
		return nil, infraerrors.BadRequest("INVALID_SILENCE", "invalid silence")
	}
	if input.RuleID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_RULE_ID", "invalid rule id")
	}
	if strings.TrimSpace(input.Platform) == "" {
		return nil, infraerrors.BadRequest("INVALID_PLATFORM", "invalid platform")
	}
	if input.Until.IsZero() {
		return nil, infraerrors.BadRequest("INVALID_UNTIL", "invalid until")
	}

	created, err := s.opsRepo.CreateAlertSilence(ctx, input)
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *OpsService) IsAlertSilenced(ctx context.Context, ruleID int64, platform string, groupID *int64, region *string, now time.Time) (bool, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return false, err
	}
	if s.opsRepo == nil {
		return false, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if ruleID <= 0 {
		return false, infraerrors.BadRequest("INVALID_RULE_ID", "invalid rule id")
	}
	if strings.TrimSpace(platform) == "" {
		return false, nil
	}
	return s.opsRepo.IsAlertSilenced(ctx, ruleID, platform, groupID, region, now)
}

func (s *OpsService) GetLatestAlertEvent(ctx context.Context, ruleID int64) (*OpsAlertEvent, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if ruleID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_RULE_ID", "invalid rule id")
	}
	return s.opsRepo.GetLatestAlertEvent(ctx, ruleID)
}

func (s *OpsService) CreateAlertEvent(ctx context.Context, event *OpsAlertEvent) (*OpsAlertEvent, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if event == nil {
		return nil, infraerrors.BadRequest("INVALID_EVENT", "invalid event")
	}

	created, err := s.opsRepo.CreateAlertEvent(ctx, event)
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *OpsService) UpdateAlertEventStatus(ctx context.Context, eventID int64, status string, resolvedAt *time.Time) error {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return err
	}
	if s.opsRepo == nil {
		return infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	return s.updateAlertEventStatusResolvedUnchecked(ctx, eventID, status, resolvedAt)
}

// updateAlertEventStatusResolvedUnchecked 原子关闭（resolved / manual_resolved）指定告警事件
// （不受监控开关门禁约束）。公共面 UpdateAlertEventStatus 与恢复关闭窄面 ResolveFreshnessAlertOnRecovery
// 共用同一关闭逻辑，不得出现两份同源正文。
func (s *OpsService) updateAlertEventStatusResolvedUnchecked(ctx context.Context, eventID int64, status string, resolvedAt *time.Time) error {
	if eventID <= 0 {
		return infraerrors.BadRequest("INVALID_EVENT_ID", "invalid event id")
	}
	status = strings.TrimSpace(status)
	if status == "" {
		return infraerrors.BadRequest("INVALID_STATUS", "invalid status")
	}
	if status != OpsAlertStatusResolved && status != OpsAlertStatusManualResolved {
		return infraerrors.BadRequest("INVALID_STATUS", "invalid status")
	}
	return s.opsRepo.UpdateAlertEventStatus(ctx, eventID, status, resolvedAt)
}

func (s *OpsService) UpdateAlertEventEmailSent(ctx context.Context, eventID int64, emailSent bool) error {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return err
	}
	if s.opsRepo == nil {
		return infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if eventID <= 0 {
		return infraerrors.BadRequest("INVALID_EVENT_ID", "invalid event id")
	}
	return s.opsRepo.UpdateAlertEventEmailSent(ctx, eventID, emailSent)
}
