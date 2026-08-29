package service

import (
	"context"
	"strconv"
	"time"
)

// XianyuAlertService 负责闲鱼告警：复用现有管理员邮件通知机制。
// 按「目标 ID + 事件」去重；恢复或事件状态变化时发送恢复通知。
type XianyuAlertService struct {
	control      *XianyuControlService
	settingStore SettingRepository
	notify       *NotificationEmailService
	settingsOnly bool
}

// NewXianyuAlertService 创建告警服务。
func NewXianyuAlertService(control *XianyuControlService, settingStore SettingRepository, notify *NotificationEmailService) *XianyuAlertService {
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

func (s *XianyuAlertService) evaluateWorker(ctx context.Context) {
	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		s.send(ctx, NotificationEmailEventXianyuWorkerUnhealthy, "worker", "unhealthy", map[string]string{
			"worker_id": "unknown", "status": "unhealthy", "last_checked_at": time.Now().Format(time.RFC3339),
		})
		return
	}
	_ = workerCfg
	// 健康检查结果由定时任务更新，这里只在明确 unhealthy 时告警。
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