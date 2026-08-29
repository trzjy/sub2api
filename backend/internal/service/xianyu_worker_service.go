package service

import (
	"context"
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
		_, err = s.control.UpsertAccount(ctx, XianyuAccount{
			WorkerConfigID: workerCfg.ID,
			AccountID:      acc.AccountID,
			Nickname:       acc.Nickname,
			Status:         status,
			CookieStatus:   acc.CookieStatus,
			TaskStatus:     acc.TaskStatus,
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
	status, err := client.RefreshCookie(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if status != nil && status.CookieStatus != "" {
		account.CookieStatus = status.CookieStatus
	}
	now := time.Now()
	account.LastSeenAt = &now
	saved, err := s.control.UpdateAccount(ctx, *account)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// ResendDelivery resends an already-claimed code to the same buyer over the
// Worker's internal channel. It does not allocate or consume new inventory.
func (s *XianyuWorkerService) ResendDelivery(ctx context.Context, claim *XianyuOrderClaim) error {
	if s == nil || claim == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	client, workerCfg, err := s.clientForActiveWorker(ctx)
	if err != nil {
		return err
	}
	account, err := s.control.GetAccountByWorkerAndAccountID(ctx, workerCfg.ID, claim.AccountID)
	if err != nil {
		return err
	}
	if account.Status != XianyuAccountStatusEnabled {
		return ErrXianyuAccountDisabled
	}
	result, err := client.ResendDelivery(
		ctx, account.AccountID, claim.OrderNo, claim.ItemID, claim.BuyerID, claim.BuyerID, claim.Code,
	)
	if err != nil {
		return err
	}
	if result == nil || !result.Success || result.SendStatus != "success" {
		message := "worker did not confirm delivery"
		if result != nil && result.Message != "" {
			message = result.Message
		}
		return &XianyuWorkerError{StatusCode: 500, Reason: "DELIVERY_NOT_CONFIRMED", Message: message}
	}
	return nil
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
