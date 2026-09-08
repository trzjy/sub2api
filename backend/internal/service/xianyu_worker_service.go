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
		// Cookie 状态由 Worker 最近一次自动续期结果推导，不再写死 unknown；
		// 仅失效时保留原因供 UI 提示：优先续期失败原因，其次 Worker 侧禁用原因。
		cookieStatus := deriveCookieStatus(acc, time.Now())
		cookieDetail := ""
		if cookieStatus == XianyuCookieStatusInvalid {
			cookieDetail = truncateRunes(strings.TrimSpace(acc.LastRenewError), xianyuCookieDetailMaxLen)
			if cookieDetail == "" {
				cookieDetail = truncateRunes(strings.TrimSpace(acc.DisableReason), xianyuCookieDetailMaxLen)
			}
		}
		taskStatus := XianyuTaskStatusUnknown
		if acc.Enabled {
			// Worker 侧账号存在且启用（含重新扫码登录后的恢复）：投影回到 enabled。
			if status == XianyuAccountStatusDisabled || status == XianyuAccountStatusLoggedOut {
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
			CookieDetail:   cookieDetail,
			TaskStatus:     taskStatus,
			LastLoginAt:    lastLoginAt,
			LastSeenAt:     timePtr(time.Now()),
		})
		if err != nil {
			return err
		}
	}
	// 对账收敛：本地有、而本次成功的 Worker 列表里已不存在的账号，说明 Worker 侧
	// 账号已被删除（凭证随之消失）。同步时直接把投影收敛为已退出登录，不再依赖
	// 启用/刷新等动作触发 404 自愈，避免账号列表长期残留"已停用/可启用"的误导状态。
	localAccounts, err := s.control.ListAccounts(ctx, workerCfg.ID)
	if err != nil {
		return err
	}
	workerAccountIDs := make(map[string]struct{}, len(accounts))
	for _, acc := range accounts {
		workerAccountIDs[acc.AccountID] = struct{}{}
	}
	for _, local := range localAccounts {
		if _, ok := workerAccountIDs[local.AccountID]; ok {
			continue
		}
		if local.Status == XianyuAccountStatusLoggedOut {
			continue
		}
		slog.Info("xianyu: account missing from worker list, converging projection to logged_out",
			"account_id", local.AccountID, "previous_status", local.Status)
		s.convergeLoggedOut(ctx, &local)
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
	if account.Status == XianyuAccountStatusLoggedOut {
		// 已退出的账号凭证已删除，启用无从谈起，必须重新扫码登录。
		return ErrXianyuAccountLoggedOut
	}
	if err := client.EnableAccount(ctx, accountID); err != nil {
		if errors.Is(err, ErrXianyuWorkerAccountNotFound) {
			// Worker 侧账号已不存在（凭证随之消失）：投影收敛为已退出，
			// 避免残留"可启用"的停用态误导用户。
			if cerr := s.convergeLoggedOut(ctx, account); cerr != nil {
				return cerr
			}
			return err
		}
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
		if errors.Is(err, ErrXianyuWorkerAccountNotFound) {
			// Worker 侧账号已不存在：停用目标状态本已达成，幂等成功并收敛为已退出。
			// 收敛（DB 投影更新）失败必须透传，不得谎报成功。
			if cerr := s.convergeLoggedOut(ctx, account); cerr != nil {
				return cerr
			}
			return nil
		}
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
		if errors.Is(err, ErrXianyuWorkerAccountNotFound) {
			// Worker 侧账号已不存在：投影收敛为已退出，并把错误透传给前端引导重新扫码。
			if cerr := s.convergeLoggedOut(ctx, account); cerr != nil {
				return nil, cerr
			}
			return nil, err
		}
		if isCookieRenewFailure(err) {
			// 续期明确失败：Cookie 状态即时标为失效并保留原因，不等下一轮同步。
			account.CookieStatus = XianyuCookieStatusInvalid
			account.CookieDetail = truncateRunes(renewFailureDetail(err), xianyuCookieDetailMaxLen)
			if _, updateErr := s.control.UpdateAccount(ctx, *account); updateErr != nil {
				return nil, updateErr
			}
			return nil, err
		}
		return nil, err
	}
	// Worker 续期成功后自动启用账号；主程序启停状态仅按 Worker 启用结果更新为 enabled，
	// Cookie 状态同步收敛为有效（续期成功即凭证可用的直接证据）。
	if account.Status != XianyuAccountStatusEnabled {
		account.Status = XianyuAccountStatusEnabled
		account.TaskStatus = XianyuTaskStatusRunning
	}
	now := time.Now()
	account.CookieStatus = XianyuCookieStatusValid
	account.CookieDetail = ""
	account.LastSeenAt = &now
	saved, err := s.control.UpdateAccount(ctx, *account)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// xianyuCookieRenewFreshWindow 判定"最近续期成功"的新鲜窗口。
// Worker 自动续期调度默认 600s 一轮；窗口取保守的 24h，兼容把续期周期放宽到小时级的部署。
const xianyuCookieRenewFreshWindow = 24 * time.Hour

// xianyuCookieDetailMaxLen 与 xianyu_accounts.cookie_detail VARCHAR(500) 对齐，超长会写库失败。
const xianyuCookieDetailMaxLen = 500

// truncateRunes 按字符数截断。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// deriveCookieStatus 从 Worker /cookies/details 投影推导主程序 Cookie 状态。
// 数据优先级：Worker 侧账号状态（inactive/suspended/deleted 视为失效）→ 最近续期结果
// （failed/need_password_login 视为失效）→ 成功续期超过新鲜窗口视为即将过期 →
// 无续期日志时，最近一次扫码登录本身就是 Cookie 有效的直接证据（新登录账号尚未
// 被续期调度覆盖）→ 两者皆无才保持 unknown，不伪造健康度。
func deriveCookieStatus(acc XianyuWorkerAccountStatus, now time.Time) string {
	switch acc.Status {
	case "inactive", "suspended", "deleted":
		return XianyuCookieStatusInvalid
	}
	switch acc.LastRenewStatus {
	case "failed", "need_password_login":
		return XianyuCookieStatusInvalid
	case "success", "cookie_updated", "browser_renewed":
		if ts, ok := parseWorkerTime(acc.LastRenewAt); ok && now.Sub(ts) > xianyuCookieRenewFreshWindow {
			return XianyuCookieStatusExpiring
		}
		return XianyuCookieStatusValid
	}
	if ts, ok := parseWorkerTime(acc.LastLoginAt); ok && now.Sub(ts) <= xianyuCookieRenewFreshWindow {
		return XianyuCookieStatusValid
	}
	return XianyuCookieStatusUnknown
}

// xianyuWorkerLoc 是闲鱼 Worker 写入时间戳所用的时区（Asia/Shanghai / UTC+8）。
// Worker 的 MySQL DATETIME / 续期日志时间为无时区北京时间，解析 naive 格式须按此落地，
// 否则 time.Parse 默认 UTC 会使 24h 级的续期新鲜度判断偏差约 8 小时。
var xianyuWorkerLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// parseWorkerTime 解析 Worker 投影时间：优先 RFC3339（带时区），兼容无时区后缀的 isoformat。
// 无时区时按 Asia/Shanghai 落地（Worker 时区），避免 24h 级新鲜度判断偏差约 8 小时。
func parseWorkerTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts, true
	}
	if ts, err := time.ParseInLocation("2006-01-02T15:04:05", raw, xianyuWorkerLoc); err == nil {
		return ts, true
	}
	return time.Time{}, false
}

// isCookieRenewFailure 判断续期错误是否为 Worker 明确返回的续期失败（区别于网络错误与账号缺失）。
func isCookieRenewFailure(err error) bool {
	var we *XianyuWorkerError
	if !errors.As(err, &we) {
		return false
	}
	return we.Reason == "COOKIE_RENEW_FAILED" || we.Reason == "ACCOUNT_NOT_IN_RENEW_RESULT"
}

// renewFailureDetail 提取续期失败的可展示原因。
func renewFailureDetail(err error) string {
	var we *XianyuWorkerError
	if errors.As(err, &we) && we.Message != "" {
		return we.Message
	}
	return ""
}

// ClearCredentials 退出/清除凭证：停止 Worker 任务并删除 Worker 侧账号（含 Cookie），
// 主程序投影保留并标记为 logged_out（已退出登录）：启用按钮随之隐藏，仅可重新扫码登录。
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
		// Worker 侧账号已不存在（曾被清除）时视为幂等成功：目标终态（已退出）本已达成，
		// 无需再向用户报 internal error，仍把主程序投影收敛到 logged_out。
		if !errors.Is(err, ErrXianyuWorkerAccountNotFound) {
			return err
		}
	}
	if cerr := s.convergeLoggedOut(ctx, account); cerr != nil {
		return cerr
	}
	return nil
}

// convergeLoggedOut 把主程序投影收敛为"已退出登录"终态。
// 适用场景：退出清除凭证，或任一账号操作发现 Worker 侧账号已不存在（凭证随之消失）。
// 返回 UpdateAccount 的错误：收敛失败（DB 投影未更新）时必须向上传播，禁止以成功掩盖
// 主程序与 Worker 状态不一致（否则 ClearCredentials / DisableAccount 会向调用方谎报完成）。
func (s *XianyuWorkerService) convergeLoggedOut(ctx context.Context, account *XianyuAccount) error {
	if account == nil {
		return nil
	}
	account.Status = XianyuAccountStatusLoggedOut
	account.CookieStatus = XianyuCookieStatusUnknown
	account.CookieDetail = ""
	account.TaskStatus = XianyuTaskStatusStopped
	if _, err := s.control.UpdateAccount(ctx, *account); err != nil {
		slog.Warn("xianyu: converge account to logged_out failed", "account_id", account.AccountID, "error", err)
		return err
	}
	return nil
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

// GetDeliveryTemplate 读取 Worker 全局发货模板（对所有卡券统一生效）。
func (s *XianyuWorkerService) GetDeliveryTemplate(ctx context.Context) (string, error) {
	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		return "", err
	}
	client := s.clientFor(workerCfg.BaseURL, mustDecrypt(s.encryptor, workerCfg.APITokenEncrypted))
	return client.GetDeliveryTemplate(ctx)
}

// UpdateDeliveryTemplate 更新 Worker 全局发货模板。
func (s *XianyuWorkerService) UpdateDeliveryTemplate(ctx context.Context, template string) error {
	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		return err
	}
	client := s.clientFor(workerCfg.BaseURL, mustDecrypt(s.encryptor, workerCfg.APITokenEncrypted))
	return client.UpdateDeliveryTemplate(ctx, template)
}

// ProvisionPoolCard 为库存池创建专属 API 发货卡券，返回卡券 ID。
func (s *XianyuWorkerService) ProvisionPoolCard(ctx context.Context, name string) (int64, error) {
	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		return 0, err
	}
	client := s.clientFor(workerCfg.BaseURL, mustDecrypt(s.encryptor, workerCfg.APITokenEncrypted))
	return client.ProvisionPoolCard(ctx, name)
}

// DeletePoolCard 删除池对应的自动发货卡券（幂等）。
func (s *XianyuWorkerService) DeletePoolCard(ctx context.Context, cardID int64) error {
	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		return err
	}
	client := s.clientFor(workerCfg.BaseURL, mustDecrypt(s.encryptor, workerCfg.APITokenEncrypted))
	return client.DeletePoolCard(ctx, cardID)
}

// SyncItemCard 同步商品的 Worker 卡券关联（cardID<=0 清空关联）。
func (s *XianyuWorkerService) SyncItemCard(ctx context.Context, itemID string, cardID int64) error {
	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		return err
	}
	client := s.clientFor(workerCfg.BaseURL, mustDecrypt(s.encryptor, workerCfg.APITokenEncrypted))
	return client.SyncItemCard(ctx, itemID, cardID)
}

// SyncProducts 拉取账号在售商品并落库；只在不覆盖手工绑定映射的前提下更新。
// 完整成功后，投影中未出现的商品行直接删除（售罄/下架清理，发货记录保留）。
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

	// 本轮响应成功后，投影中未出现的商品视为已售罄/下架，直接删除商品行
	// （Worker 侧已同步清理投影，配合主程序删除保持面板干净）。
	for accountID, seen := range seenByAccount {
		products, err := s.control.ListProductsByAccount(ctx, accountID)
		if err != nil {
			return err
		}
		for _, p := range products {
			key := normalizeProductIdentity(p.ItemID, p.SpecName, p.SpecValue)
			if !seen[key] {
				if err := s.control.DeleteProduct(ctx, p.ID); err != nil {
					if err == ErrXianyuProductNotFound {
						continue
					}
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
