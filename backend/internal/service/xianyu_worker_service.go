package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// XianyuWorkerService 聚合 Worker 控制面操作：健康检查、账号同步、商品同步、
// 扫码登录、账号启停、Cookie 刷新、绑定规则与池管理。
type XianyuWorkerService struct {
	control    XianyuControlRepository
	encryptor  SecretEncryptor
	forbidLoop bool
	clientFor  func(baseURL, token string) *XianyuWorkerClient
}

// NewXianyuWorkerService 创建 Worker 控制面服务。
func NewXianyuWorkerService(control XianyuControlRepository, encryptor SecretEncryptor) *XianyuWorkerService {
	return &XianyuWorkerService{
		control:    control,
		encryptor:  encryptor,
		forbidLoop: true,
		clientFor:  newXianyuWorkerClientForService,
	}
}

// clientForActiveWorker 读取 active Worker 配置并构建客户端。
func (s *XianyuWorkerService) clientForActiveWorker(ctx context.Context) (*XianyuWorkerClient, *XianyuWorkerConfig, error) {
	cfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	token, err := s.encryptor.Decrypt(cfg.APITokenEncrypted)
	if err != nil {
		return nil, nil, ErrXianyuWorkerConfigNotFound
	}
	return s.clientFor(cfg.BaseURL, token), cfg, nil
}

// CheckHealth 检查 active Worker 健康状态并落库。
func (s *XianyuWorkerService) CheckHealth(ctx context.Context) error {
	client, cfg, err := s.clientForActiveWorker(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	health, err := client.Health(ctx)
	newStatus := XianyuWorkerHealthUnhealthy
	if err == nil && health != nil && health.Backend && health.WebSocket && health.Database {
		newStatus = XianyuWorkerHealthHealthy
	}
	cfg.HealthStatus = newStatus
	cfg.LastCheckedAt = &now
	if _, updateErr := s.control.UpdateWorkerConfig(ctx, *cfg); updateErr != nil {
		return updateErr
	}
	if err != nil {
		return err
	}
	if newStatus != XianyuWorkerHealthHealthy {
		return ErrXianyuWorkerUnhealthy
	}
	return nil
}

// SyncAccounts 拉取 Worker 账号列表并落库。
func (s *XianyuWorkerService) SyncAccounts(ctx context.Context) error {
	client, workerCfg, err := s.clientForActiveWorker(ctx)
	if err != nil {
		return err
	}
	accounts, err := client.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acc := range accounts {
		existing, err := s.control.GetAccountByWorkerAndAccountID(ctx, workerCfg.ID, acc.AccountID)
		status := XianyuAccountStatusDisabled
		if err == nil {
			status = existing.Status
		}
		cookieStatus := XianyuCookieStatusUnknown
		taskStatus := XianyuTaskStatusUnknown
		if acc.Enabled {
			if status == XianyuAccountStatusDisabled {
				status = XianyuAccountStatusEnabled
			}
			taskStatus = XianyuTaskStatusRunning
		}
		var lastLoginAt *time.Time
		if acc.LastLoginAt != "" {
			if ts, err := time.Parse(time.RFC3339, acc.LastLoginAt); err == nil {
				lastLoginAt = &ts
			}
		}
		_, err = s.control.UpsertAccount(ctx, XianyuAccount{
			WorkerConfigID: workerCfg.ID,
			AccountID:      acc.AccountID,
			Nickname:       acc.Nickname,
			Status:         status,
			CookieStatus:   cookieStatus,
			TaskStatus:     taskStatus,
			LastLoginAt:    lastLoginAt,
			LastSeenAt:     timePtr(time.Now()),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// EnableAccount 启用账号并启动 Worker 收消息任务。
func (s *XianyuWorkerService) EnableAccount(ctx context.Context, accountID string) error {
	client, workerCfg, err := s.clientForActiveWorker(ctx)
	if err != nil {
		return err
	}
	account, err := s.control.GetAccountByWorkerAndAccountID(ctx, workerCfg.ID, accountID)
	if err != nil {
		return err
	}
	if err := client.EnableAccount(ctx, accountID); err != nil {
		return err
	}
	account.Status = XianyuAccountStatusEnabled
	account.TaskStatus = XianyuTaskStatusRunning
	_, err = s.control.UpdateAccount(ctx, *account)
	return err
}

// DisableAccount 停用账号并停止 Worker 收消息任务。
func (s *XianyuWorkerService) DisableAccount(ctx context.Context, accountID string) error {
	client, workerCfg, err := s.clientForActiveWorker(ctx)
	if err != nil {
		return err
	}
	account, err := s.control.GetAccountByWorkerAndAccountID(ctx, workerCfg.ID, accountID)
	if err != nil {
		return err
	}
	if err := client.DisableAccount(ctx, accountID); err != nil {
		return err
	}
	account.Status = XianyuAccountStatusDisabled
	account.TaskStatus = XianyuTaskStatusStopped
	_, err = s.control.UpdateAccount(ctx, *account)
	return err
}

// RefreshCookie 刷新账号 Cookie。
func (s *XianyuWorkerService) RefreshCookie(ctx context.Context, accountID string) (*XianyuAccount, error) {
	client, workerCfg, err := s.clientForActiveWorker(ctx)
	if err != nil {
		return nil, err
	}
	account, err := s.control.GetAccountByWorkerAndAccountID(ctx, workerCfg.ID, accountID)
	if err != nil {
		return nil, err
	}
	if _, err := client.RefreshCookie(ctx, accountID); err != nil {
		return nil, err
	}
	// Worker 续期成功后自动启用账号；主程序账号状态仅按 Worker 启用结果更新为 enabled，
	// 不把 Worker 的 cookie/续期细节直接写入主程序启停状态字段。
	if account.Status != XianyuAccountStatusEnabled {
		account.Status = XianyuAccountStatusEnabled
		account.TaskStatus = XianyuTaskStatusRunning
	}
	now := time.Now()
	account.LastSeenAt = &now
	saved, err := s.control.UpdateAccount(ctx, *account)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// ClearCredentials 退出/清除凭证：停止 Worker 任务并删除 Worker 侧账号（含 Cookie），
// 主程序投影保留并标记为 disabled。
func (s *XianyuWorkerService) ClearCredentials(ctx context.Context, accountID string) error {
	client, workerCfg, err := s.clientForActiveWorker(ctx)
	if err != nil {
		return err
	}
	account, err := s.control.GetAccountByWorkerAndAccountID(ctx, workerCfg.ID, accountID)
	if err != nil {
		return err
	}
	if err := client.ClearCredentials(ctx, accountID); err != nil {
		// Worker 侧账号已不存在（曾被清除）时视为幂等成功：目标终态（禁用/停止）本已达成，
		// 无需再向用户报 internal error，仍把主程序投影收敛到 disabled/stopped。
		if !errors.Is(err, ErrXianyuWorkerAccountNotFound) {
			return err
		}
	}
	account.Status = XianyuAccountStatusDisabled
	account.CookieStatus = XianyuCookieStatusUnknown
	account.TaskStatus = XianyuTaskStatusStopped
	_, err = s.control.UpdateAccount(ctx, *account)
	return err
}

// ResendDelivery resends an already-claimed code to the same buyer over the
// Worker's internal channel. It does not allocate or consume new inventory.
func (s *XianyuWorkerService) ResendDelivery(ctx context.Context, claim *XianyuOrderClaim) error {
	if s == nil || claim == nil {
		return fmt.Errorf("%w: %v", ErrXianyuResendUndispatched, ErrXianyuDeliveryNotConfigured)
	}
	client, workerCfg, err := s.clientForActiveWorker(ctx)
	if err != nil {
		// 无 active Worker：确定未向任何 Worker 发出发送请求。
		return fmt.Errorf("%w: %v", ErrXianyuResendUndispatched, err)
	}
	account, err := s.control.GetAccountByWorkerAndAccountID(ctx, workerCfg.ID, claim.AccountID)
	if err != nil {
		// 账号在主程序侧不可解析：确定未 dispatch。
		return fmt.Errorf("%w: %v", ErrXianyuResendUndispatched, err)
	}
	if account.Status != XianyuAccountStatusEnabled {
		// 账号已停用：确定未 dispatch。
		return fmt.Errorf("%w: %v", ErrXianyuResendUndispatched, ErrXianyuAccountDisabled)
	}
	result, err := client.ResendDelivery(
		ctx, account.AccountID, claim.OrderNo, claim.ItemID, claim.BuyerID, claim.ChatID, claim.Code, claim.AttemptCount,
	)
	if err != nil {
		// 请求前失败（配置缺失）或请求未到达 Worker（Unreachable）→ 确定未 dispatch，可回滚 failed；
		// 超时（已发出）/ 解码失败 / 其他 → 结果不确定，保留 pending。
		if errors.Is(err, ErrXianyuDeliveryNotConfigured) || errors.Is(err, ErrXianyuWorkerUnreachable) {
			return fmt.Errorf("%w: %v", ErrXianyuResendUndispatched, err)
		}
		return err
	}
	receipt, reason := normalizeSendReceipt(result)
	switch receipt {
	case "sent_explicit_success":
		return nil
	case "dispatched_definite_failure":
		// 机器可判定的"明确未 dispatch"（Worker 端账号不存在/无权）：
		// 确定未发送，标记哨兵供主程序补发回滚 failed 后再次补发。
		return fmt.Errorf("%w: worker did not dispatch (reason=%s)", ErrXianyuResendUndispatched, reason)
	case "rejected":
		// 平台明确拒绝（如 CSI_FORBID 拦截）：消息已 dispatch 但确定未送达。
		// 与 dispatched_definite_failure 一样属于"确定失败"，标记哨兵供主程序
		// 回滚 failed，与 Worker 侧异步兜底回传（REJECTED → success=false）收敛一致，
		// 避免同步路径保留 pending、异步路径标 failed 的终态分歧。
		return fmt.Errorf("%w: worker delivery rejected (reason=%s)", ErrXianyuResendRejected, reason)
	default: // unknown_pending：已发出但未拿到最终回执，结果不确定，保留 pending（不重复发货）。
		message := "worker did not confirm delivery"
		if reason != "" {
			message = reason
		}
		return &XianyuWorkerError{StatusCode: 500, Reason: "DELIVERY_NOT_CONFIRMED", Message: message}
	}
}

// SyncProducts 拉取账号在售商品并落库；只在不覆盖手工绑定映射的前提下更新。
// 只有该流程完整成功后，才把本轮未出现的主程序记录标记为 removed。
func (s *XianyuWorkerService) SyncProducts(ctx context.Context) error {
	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		return err
	}
	accounts, err := s.control.ListAccounts(ctx, workerCfg.ID)
	if err != nil {
		return err
	}
	client := s.clientFor(workerCfg.BaseURL, mustDecrypt(s.encryptor, workerCfg.APITokenEncrypted))
	seenByAccount := map[int64]map[string]bool{}
	for _, account := range accounts {
		if account.Status != XianyuAccountStatusEnabled {
			continue
		}
		products, err := client.ListProducts(ctx, account.AccountID)
		if err != nil {
			// Worker 网络错误/认证失败:保留原状态并返回同步错误。
			return err
		}
		seen := map[string]bool{}
		for _, p := range products {
			key := normalizeProductIdentity(p.ItemID, p.SpecName, p.SpecValue)
			seen[key] = true
			existing, err := s.control.GetProductByIdentity(ctx, account.ID, p.ItemID, p.SpecName, p.SpecValue)
			var product XianyuProduct
			if err == nil && existing != nil {
				product = *existing
				product.Title = p.Title
				product.LastSeenAt = timePtr(time.Now())
				if product.Status == XianyuProductStatusRemoved {
					// 恢复上架的商品从 removed 恢复为 active。
					product.Status = XianyuProductStatusActive
				}
				_, err = s.control.UpdateProduct(ctx, product)
			} else {
				_, err = s.control.UpsertProduct(ctx, XianyuProduct{
					AccountPK:     account.ID,
					AccountID:     account.AccountID,
					ItemID:        p.ItemID,
					Title:         p.Title,
					SpecName:      p.SpecName,
					SpecValue:     p.SpecValue,
					BindingStatus: XianyuBindingStatusUnmapped,
					BindingSource: XianyuBindingSourceAutoNew,
					Status:        XianyuProductStatusActive,
					LastSeenAt:    timePtr(time.Now()),
				})
			}
			if err != nil {
				return err
			}
		}
		seenByAccount[account.ID] = seen
	}

	// 本轮响应成功后，标记未出现的 active 商品为 removed。
	for accountID, seen := range seenByAccount {
		products, err := s.control.ListProductsByAccount(ctx, accountID)
		if err != nil {
			return err
		}
		for _, p := range products {
			if p.Status != XianyuProductStatusActive {
				continue
			}
			key := normalizeProductIdentity(p.ItemID, p.SpecName, p.SpecValue)
			if !seen[key] {
				p.Status = XianyuProductStatusRemoved
				if _, err := s.control.UpdateProduct(ctx, p); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// normalizeProductIdentity 规范化商品唯一标识（规格空串统一）。
func normalizeProductIdentity(itemID, specName, specValue string) string {
	return strings.TrimSpace(itemID) + "\x00" + strings.TrimSpace(specName) + "\x00" + strings.TrimSpace(specValue)
}

// AutoBindProducts 对新商品按固定顺序尝试自动绑定。
// 顺序：关键词规则 → 账号默认池规则；不覆盖已手工映射的商品。
func (s *XianyuWorkerService) AutoBindProducts(ctx context.Context, product XianyuProduct, rules []XianyuBindingRule) error {
	return autoBindProduct(ctx, s.control, product, rules)
}

func mustDecrypt(e SecretEncryptor, ciphertext string) string {
	if e == nil {
		return ""
	}
	plain, err := e.Decrypt(ciphertext)
	if err != nil {
		slog.Warn("xianyu: failed to decrypt worker token", "error", err)
		return ""
	}
	return plain
}

func timePtr(t time.Time) *time.Time {
	return &t
}

func normalizeKeyword(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
