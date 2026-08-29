package service

import (
	"context"
	"strconv"
	"time"
)

// XianyuNotificationSender 是告警发送端口（由 NotificationEmailService 实现）。
type XianyuNotificationSender interface {
	Send(ctx context.Context, input NotificationEmailSendInput) error
}

// XianyuAlertService 负责闲鱼告警：复用现有管理员邮件通知机制。
// 按「目标 ID + 事件」去重；恢复或事件状态变化时发送恢复通知。
type XianyuAlertService struct {
	control      *XianyuControlService
	settingStore XianyuSettingStore
	notify       XianyuNotificationSender
	settingsOnly bool

	// workerWasUnhealthy 记录上次巡检的 Worker 健康状态，用于只在
	// unhealthy -> healthy 迁移时发送恢复通知（避免每次巡检重复发送）。
	workerWasUnhealthy bool
	workerKnown        bool
}

// NewXianyuAlertService 创建告警服务。
func NewXianyuAlertService(control *XianyuControlService, settingStore XianyuSettingStore, notify XianyuNotificationSender) *XianyuAlertService {
	return &XianyuAlertService{control: control, settingStore: settingStore, notify: notify}
}

// alertRecipients 读取管理员通知邮箱并过滤未验证/禁用项。
func (s *XianyuAlertService) alertRecipients(ctx context.Context) []string {
	if s.settingStore == nil {
		return nil
	}
	raw, err := s.settingStore.GetValue(ctx, SettingKeyAccountQuotaNotifyEmails)
	if err != nil {
		return nil
	}
	return filterVerifiedEmails(ParseNotifyEmails(raw))
}

// send 发送一封告警邮件（失败不阻断巡检）。
func (s *XianyuAlertService) send(ctx context.Context, event, sourceID, reminder string, variables map[string]string) {
	if s.notify == nil {
		return
	}
	recipients := s.alertRecipients(ctx)
	if len(recipients) == 0 {
		return
	}
	for _, to := range recipients {
		_ = s.notify.Send(ctx, NotificationEmailSendInput{
			Event:          event,
			RecipientEmail: to,
			SourceType:     "xianyu",
			SourceID:       sourceID,
			ReminderKey:    reminder, // 状态变化时恢复发送；同状态未恢复去重。
			Variables:      variables,
		})
	}
}

// Evaluate 每次巡检评估全部告警条件。
func (s *XianyuAlertService) Evaluate(ctx context.Context) {
	if s == nil || s.control == nil || !s.control.Enabled(ctx) {
		return
	}
	s.evaluateWorker(ctx)
	s.evaluateAccounts(ctx)
	s.evaluatePools(ctx)
	s.evaluatePendingTimeouts(ctx)
}

// xianyuWorkerAlertDecision 描述一次 worker 告警巡检的输出。
type xianyuWorkerAlertDecision struct {
	// Alerted 表示本次是否需要发送告警邮件。
	Alerted bool
	// Recovery 表示本次是否是恢复通知（unhealthy -> healthy）。
	Recovery bool
	// Reminder 是通知去重键（unhealthy / recovered / ""）。
	Reminder string
	// WorkerID 用于组装 source ID 与变量。
	WorkerID string
	// HealthStatus 透传给通知变量。
	HealthStatus string
	// LastCheckedAt 透传给通知变量。
	LastCheckedAt time.Time
}

// workerHealthTransition 计算基于上次状态的告警决策（纯函数，便于单测）。
func workerHealthTransition(known, wasUnhealthy bool, healthStatus string, workerID string, lastChecked time.Time) xianyuWorkerAlertDecision {
	switch healthStatus {
	case XianyuWorkerHealthUnknown, XianyuWorkerHealthUnhealthy:
		if known && wasUnhealthy {
			// 同事件未恢复不重复发送。
			return xianyuWorkerAlertDecision{WorkerID: workerID, HealthStatus: healthStatus, LastCheckedAt: lastChecked}
		}
		return xianyuWorkerAlertDecision{
			Alerted: true, Reminder: "unhealthy",
			WorkerID: workerID, HealthStatus: healthStatus, LastCheckedAt: lastChecked,
		}
	default: // healthy / unknown
		if known && wasUnhealthy {
			// 恢复通知。
			return xianyuWorkerAlertDecision{
				Alerted: true, Recovery: true, Reminder: "recovered",
				WorkerID: workerID, HealthStatus: healthStatus, LastCheckedAt: lastChecked,
			}
		}
		return xianyuWorkerAlertDecision{WorkerID: workerID, HealthStatus: healthStatus, LastCheckedAt: lastChecked}
	}
}

func (s *XianyuAlertService) evaluateWorker(ctx context.Context) {
	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		// 无 active Worker 配置：视为不可用并告警；同一状态去重。
		if !s.workerKnown || !s.workerWasUnhealthy {
			s.sendAlert(NotificationEmailEventXianyuWorkerUnhealthy, "worker", "unhealthy", map[string]string{
				"worker_id":       "unknown",
				"status":          "unhealthy",
				"last_checked_at": time.Now().Format(time.RFC3339),
			})
		}
		s.workerKnown = true
		s.workerWasUnhealthy = true
		return
	}
	// 健康检查结果由定时任务写入 xianyu_worker_configs.health_status；
	// unknown 是没有成功完成健康检查的事实状态，也必须告警。
	lastChecked := time.Now()
	if workerCfg.LastCheckedAt != nil {
		lastChecked = *workerCfg.LastCheckedAt
	}
	decision := workerHealthTransition(s.workerKnown, s.workerWasUnhealthy, workerCfg.HealthStatus,
		strconv.FormatInt(workerCfg.ID, 10), lastChecked)
	if decision.Alerted {
		s.sendAlert(NotificationEmailEventXianyuWorkerUnhealthy,
			"worker:"+decision.WorkerID, decision.Reminder, map[string]string{
				"worker_id":       decision.WorkerID,
				"status":          decision.HealthStatus,
				"last_checked_at": lastChecked.Format(time.RFC3339),
			})
	}
	s.workerKnown = true
	s.workerWasUnhealthy = workerCfg.HealthStatus != XianyuWorkerHealthHealthy
}

// sendAlert 发送一封告警邮件（失败不阻断巡检）。
func (s *XianyuAlertService) sendAlert(event, sourceID, reminder string, variables map[string]string) {
	s.send(context.Background(), event, sourceID, reminder, variables)
}

func (s *XianyuAlertService) evaluateAccounts(ctx context.Context) {
	accounts, err := s.control.ListAccounts(ctx)
	if err != nil {
		return
	}
	for _, account := range accounts {
		if account.CookieStatus == XianyuCookieStatusInvalid || account.CookieStatus == XianyuCookieStatusExpiring {
			s.send(ctx, NotificationEmailEventXianyuCookieInvalid,
				"account:"+account.AccountID, account.CookieStatus, map[string]string{
					"account_id":    account.AccountID,
					"nickname":      account.Nickname,
					"cookie_status": account.CookieStatus,
				})
		}
		if account.TaskStatus == XianyuTaskStatusStopped && account.Status == XianyuAccountStatusEnabled {
			s.send(ctx, NotificationEmailEventXianyuTaskStopped,
				"account:"+account.AccountID, account.TaskStatus, map[string]string{
					"account_id":  account.AccountID,
					"nickname":    account.Nickname,
					"task_status": account.TaskStatus,
				})
		}
	}
}

func (s *XianyuAlertService) evaluatePools(ctx context.Context) {
	pools, err := s.control.ListItemPools(ctx)
	if err != nil {
		return
	}
	for _, pool := range pools {
		if pool.Status != XianyuItemPoolStatusActive {
			continue
		}
		remaining, _, _, err := s.control.PoolStockCounts(ctx, pool.Slug)
		if err != nil {
			continue
		}
		if pool.LowStockThreshold > 0 && remaining <= pool.LowStockThreshold {
			s.send(ctx, NotificationEmailEventXianyuPoolLowStock,
				"pool:"+pool.Slug, "low:"+pool.Slug, map[string]string{
					"pool_id":   strconv.FormatInt(pool.ID, 10),
					"pool_name": pool.Name,
					"remaining": strconv.Itoa(remaining),
					"threshold": strconv.Itoa(pool.LowStockThreshold),
				})
		}
	}
}

func (s *XianyuAlertService) evaluatePendingTimeouts(ctx context.Context) {
	pending, err := s.control.PendingDeliveryCountRaw(ctx)
	if err != nil || pending == 0 {
		return
	}
	claims, _, err := s.control.ListDeliveryClaims(ctx, XianyuDeliveryFilter{Status: XianyuDeliveryStatusPending, Limit: 20})
	if err != nil {
		return
	}
	for _, claim := range claims {
		if claim.LastAttemptAt != nil && time.Since(*claim.LastAttemptAt) > 10*time.Minute {
			s.send(ctx, NotificationEmailEventXianyuDeliveryPendingTimeout,
				"order:"+claim.OrderNo, "pending", map[string]string{
					"order_no":        claim.OrderNo,
					"created_at":      claim.CreatedAt.Format(time.RFC3339),
					"last_attempt_at": claim.LastAttemptAt.Format(time.RFC3339),
				})
		}
	}
}
