package service

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// XianyuSettingStore 读写闲鱼控制面数据库设置。
type XianyuSettingStore interface {
	GetValue(ctx context.Context, key string) (string, error)
	GetMultiple(ctx context.Context, keys []string) (map[string]string, error)
	SetMultiple(ctx context.Context, values map[string]string) error
}

// XianyuControlService 提供控制面管理操作（admin 路由 + 自动绑定/同步任务）。
type XianyuControlService struct {
	control      XianyuControlRepository
	claimRepo    XianyuDeliveryRepository
	stateRepo    XianyuDeliveryStateUpdater
	listRepo     XianyuDeliveryListRepository
	encryptor    SecretEncryptor
	delivery     *XianyuDeliveryService
	worker       *XianyuWorkerService
	sync         *XianyuSyncService
	setting      XianyuDeliverySettingReader
	settingStore XianyuSettingStore
}

// NewXianyuControlService 创建控制面服务。
func NewXianyuControlService(
	control XianyuControlRepository,
	claimRepo XianyuDeliveryRepository,
	stateRepo XianyuDeliveryStateUpdater,
	listRepo XianyuDeliveryListRepository,
	encryptor SecretEncryptor,
	delivery *XianyuDeliveryService,
	worker *XianyuWorkerService,
	setting XianyuDeliverySettingReader,
	settingStore XianyuSettingStore,
	sync *XianyuSyncService,
) *XianyuControlService {
	return &XianyuControlService{
		control:      control,
		claimRepo:    claimRepo,
		stateRepo:    stateRepo,
		listRepo:     listRepo,
		encryptor:    encryptor,
		delivery:     delivery,
		worker:       worker,
		sync:         sync,
		setting:      setting,
		settingStore: settingStore,
	}
}

// XianyuSettings 是控制面设置视图。
type XianyuSettings struct {
	DeliveryEnabled     bool `json:"delivery_enabled"`
	AccountAutoRefresh  bool `json:"account_auto_refresh"`
	ProductAutoBind     bool `json:"product_auto_bind"`
	SyncIntervalMinutes int  `json:"sync_interval_minutes"`
}

// GetSettings 读取控制面设置。
func (s *XianyuControlService) GetSettings(ctx context.Context) (XianyuSettings, error) {
	if s.settingStore == nil {
		return XianyuSettings{DeliveryEnabled: s.Enabled(ctx), AccountAutoRefresh: true, ProductAutoBind: true, SyncIntervalMinutes: 5}, nil
	}
	vals, err := s.settingStore.GetMultiple(ctx, []string{
		SettingKeyXianyuDeliveryEnabled,
		SettingKeyXianyuAccountAutoRefresh,
		SettingKeyXianyuProductAutoBind,
		SettingKeyXianyuSyncIntervalMinutes,
	})
	if err != nil {
		return XianyuSettings{}, err
	}
	out := XianyuSettings{
		DeliveryEnabled:     vals[SettingKeyXianyuDeliveryEnabled] == "true",
		AccountAutoRefresh:  isFalseSettingValue(vals[SettingKeyXianyuAccountAutoRefresh]) == false,
		ProductAutoBind:     isFalseSettingValue(vals[SettingKeyXianyuProductAutoBind]) == false,
		SyncIntervalMinutes: 5,
	}
	if v := vals[SettingKeyXianyuSyncIntervalMinutes]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			out.SyncIntervalMinutes = n
		}
	}
	return out, nil
}

// SaveSettings 保存控制面设置。
func (s *XianyuControlService) SaveSettings(ctx context.Context, settings XianyuSettings) error {
	if s.settingStore == nil {
		return infraerrors.ServiceUnavailable("XIANYU_SETTINGS_UNAVAILABLE", "xianyu settings store is unavailable")
	}
	if settings.SyncIntervalMinutes < 1 {
		settings.SyncIntervalMinutes = 5
	}
	if settings.DeliveryEnabled {
		if s.worker == nil {
			return ErrXianyuDeliveryNotConfigured
		}
		if err := s.worker.CheckHealth(ctx); err != nil {
			return err
		}
	}
	values := map[string]string{
		SettingKeyXianyuDeliveryEnabled:     strconv.FormatBool(settings.DeliveryEnabled),
		SettingKeyXianyuAccountAutoRefresh:  strconv.FormatBool(settings.AccountAutoRefresh),
		SettingKeyXianyuProductAutoBind:     strconv.FormatBool(settings.ProductAutoBind),
		SettingKeyXianyuSyncIntervalMinutes: strconv.Itoa(settings.SyncIntervalMinutes),
	}
	return s.settingStore.SetMultiple(ctx, values)
}

// Enabled 返回闲鱼发货总开关。
func (s *XianyuControlService) Enabled(ctx context.Context) bool {
	return s.setting != nil && s.setting.GetXianyuDeliveryRuntime(ctx).Enabled
} // GetActiveWorkerConfig 返回当前 active Worker 配置（告警巡检用）。
func (s *XianyuControlService) GetActiveWorkerConfig(ctx context.Context) (*XianyuWorkerConfig, error) {
	return s.control.GetActiveWorkerConfig(ctx)
}

// PoolStockCounts 返回池库存计数（剩余/已发货/已兑换/禁用，告警巡检用）。
func (s *XianyuControlService) PoolStockCounts(ctx context.Context, slug string) (int, int, int, int, error) {
	return s.control.PoolStockCounts(ctx, slug)
}

// PendingDeliveryCountRaw 返回待处理发货数量（告警巡检用）。
func (s *XianyuControlService) PendingDeliveryCountRaw(ctx context.Context) (int, error) {
	return s.control.PendingDeliveryCount(ctx)
}

// SettingsEnabled 返回闲鱼发货总开关（告警巡检用）。
func (s *XianyuControlService) SettingsEnabled(ctx context.Context) bool {
	return s.Enabled(ctx)
}

// ---------------------------------------------------------------------------
// Worker 连接配置
// ---------------------------------------------------------------------------

// ListWorkerConfigs 列出全部 Worker 配置（token 不回显明文）。
func (s *XianyuControlService) ListWorkerConfigs(ctx context.Context) ([]XianyuWorkerConfig, error) {
	cfgs, err := s.control.ListWorkerConfigs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]XianyuWorkerConfig, 0, len(cfgs))
	for _, c := range cfgs {
		c.APITokenEncrypted = ""
		out = append(out, c)
	}
	return out, nil
}

// SaveWorkerConfig 创建或更新 Worker 配置。active 记录数 > 1 时失败。
func (s *XianyuControlService) SaveWorkerConfig(ctx context.Context, input XianyuWorkerConfig) (*XianyuWorkerConfig, error) {
	if input.ID == 0 {
		existing, err := s.control.ListWorkerConfigs(ctx)
		if err != nil {
			return nil, err
		}
		if len(existing) > 0 {
			return nil, ErrXianyuWorkerConfigExists
		}
	}
	baseURL := strings.TrimSpace(input.BaseURL)
	if err := validateWorkerBaseURL(baseURL, s.worker != nil && s.worker.forbidLoop); err != nil {
		return nil, err
	}
	token := strings.TrimSpace(input.APITokenEncrypted)

	// 更新已有配置时允许留空 token（仅修改地址/状态），保留原加密 token。
	if input.ID != 0 && token == "" {
		existing, err := s.control.GetWorkerConfigByID(ctx, input.ID)
		if err != nil {
			return nil, err
		}
		input.APITokenEncrypted = existing.APITokenEncrypted
		input.BaseURL = baseURL
		if input.Status == "" {
			input.Status = existing.Status
		}
		updated, err := s.control.UpdateWorkerConfig(ctx, input)
		if err != nil {
			if isUniqueViolation(err) {
				return nil, ErrXianyuActiveWorkerExists
			}
			return nil, err
		}
		return updated, nil
	}

	if token == "" {
		return nil, infraerrors.BadRequest("XIANYU_WORKER_TOKEN_REQUIRED", "worker token is required")
	}
	encrypted, err := s.encryptor.Encrypt(token)
	if err != nil {
		return nil, fmt.Errorf("encrypt xianyu worker token: %w", err)
	}
	input.BaseURL = baseURL
	input.APITokenEncrypted = encrypted
	if input.Status == "" {
		input.Status = XianyuWorkerStatusDisabled
	}

	// 创建时先以 disabled 落库，再在应用层校验唯一 active（DB 部分唯一索引兜底）。
	if input.ID == 0 {
		created, err := s.control.CreateWorkerConfig(ctx, input)
		if err != nil {
			if isUniqueViolation(err) {
				return nil, ErrXianyuWorkerConfigExists
			}
			return nil, err
		}
		return created, nil
	}
	updated, err := s.control.UpdateWorkerConfig(ctx, input)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrXianyuActiveWorkerExists
		}
		return nil, err
	}
	return updated, nil
}

// SetWorkerActive 仅允许将当前唯一配置置为 active；已存在其他 active 则失败。
func (s *XianyuControlService) SetWorkerActive(ctx context.Context, id int64) (*XianyuWorkerConfig, error) {
	all, err := s.control.ListWorkerConfigs(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range all {
		if c.ID != id && c.Status == XianyuWorkerStatusActive {
			return nil, ErrXianyuActiveWorkerExists
		}
	}
	var target *XianyuWorkerConfig
	for i := range all {
		if all[i].ID == id {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return nil, ErrXianyuWorkerConfigNotFound
	}
	if target.Status == XianyuWorkerStatusActive {
		return target, nil
	}
	target.Status = XianyuWorkerStatusActive
	// 保留加密 token。
	return s.control.UpdateWorkerConfig(ctx, *target)
}

// ---------------------------------------------------------------------------
// 商品池
// ---------------------------------------------------------------------------

func (s *XianyuControlService) ListItemPools(ctx context.Context) ([]XianyuItemPool, error) {
	return s.control.ListItemPools(ctx)
}

func (s *XianyuControlService) SaveItemPool(ctx context.Context, pool XianyuItemPool) (*XianyuItemPool, error) {
	pool.Slug = strings.TrimSpace(pool.Slug)
	pool.Name = strings.TrimSpace(pool.Name)
	if pool.Name == "" {
		return nil, infraerrors.BadRequest("XIANYU_POOL_REQUIRED", "pool name is required")
	}
	// slug 是纯内部标识（烙进库存码 notes），创建时未提供则自动生成，避免手填出错。
	autoSlug := pool.ID == 0 && pool.Slug == ""
	// 编辑时 slug 不可变且前端不回传：空 slug 在更新路径直接视为"保持原值"。
	if !autoSlug && pool.Slug != "" && !validPoolSlug(pool.Slug) {
		return nil, infraerrors.BadRequest("XIANYU_POOL_SLUG_INVALID", "pool slug must be [a-z0-9_-]")
	}
	if pool.LowStockThreshold < 0 {
		pool.LowStockThreshold = 0
	}
	if pool.Status == "" {
		pool.Status = XianyuItemPoolStatusActive
	}
	if err := s.validatePoolCardSpec(ctx, &pool); err != nil {
		return nil, err
	}
	if pool.ID == 0 {
		for attempt := 0; ; attempt++ {
			if autoSlug {
				pool.Slug = generatePoolSlug()
			}
			created, err := s.control.CreateItemPool(ctx, pool)
			if err != nil {
				if isUniqueViolation(err) {
					if autoSlug && attempt < 5 {
						continue // 自动生成的 slug 撞名，换一个重试
					}
					return nil, ErrXianyuItemPoolSlugExists
				}
				return nil, err
			}
			return created, nil
		}
	}
	updated, err := s.control.UpdateItemPool(ctx, pool)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrXianyuItemPoolSlugExists
		}
		return nil, err
	}
	return updated, nil
}

// generatePoolSlug 生成池的内部标识：pool- + 8 位小写字母数字随机串。
func generatePoolSlug() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		n, err := crand.Int(crand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			// crypto/rand 失败极罕见；退化为时间戳保证可用性
			return fmt.Sprintf("pool-%x", time.Now().UnixNano())
		}
		b[i] = alphabet[n.Int64()]
	}
	return "pool-" + string(b)
}

// XianyuPoolCodeTypeSubscription 当前库存池唯一支持的发码类型：真实可兑换的订阅码。
const XianyuPoolCodeTypeSubscription = "subscription"

// validatePoolCardSpec 校验池发码规格：订阅码池必须绑定订阅型分组且天数有效。
func (s *XianyuControlService) validatePoolCardSpec(ctx context.Context, pool *XianyuItemPool) error {
	if pool.CodeType == "" {
		pool.CodeType = XianyuPoolCodeTypeSubscription
	}
	if pool.CodeType != XianyuPoolCodeTypeSubscription {
		return infraerrors.BadRequest("XIANYU_POOL_CODE_TYPE_INVALID", "库存池当前仅支持订阅码（subscription）")
	}
	if pool.GroupID == nil {
		return infraerrors.BadRequest("XIANYU_POOL_GROUP_REQUIRED", "请选择库存池的发码分组")
	}
	if pool.ValidityDays < 1 || pool.ValidityDays > 365 {
		return infraerrors.BadRequest("XIANYU_POOL_VALIDITY_INVALID", "发码有效天数须在 1-365 之间")
	}
	subType, err := s.control.GroupSubscriptionType(ctx, *pool.GroupID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return infraerrors.BadRequest("XIANYU_POOL_GROUP_NOT_FOUND", "发码分组不存在")
		}
		return fmt.Errorf("query group subscription type: %w", err)
	}
	if subType != SubscriptionTypeSubscription {
		return infraerrors.BadRequest("XIANYU_POOL_GROUP_NOT_SUBSCRIPTION", "发码分组必须是订阅模式分组")
	}
	return nil
}

// XianyuPoolStockInput 是池补货请求。
type XianyuPoolStockInput struct {
	Count         int `json:"count"`
	ExpiresInDays int `json:"expires_in_days"` // 0 = 码本身永不过期
}

// StockItemPool 按池的发码规格批量生成真实可兑换的订阅码，返回生成数量与补货后剩余库存。
func (s *XianyuControlService) StockItemPool(ctx context.Context, poolID int64, input XianyuPoolStockInput) (int, int, error) {
	if input.Count < 1 || input.Count > 1000 {
		return 0, 0, infraerrors.BadRequest("XIANYU_POOL_STOCK_COUNT_INVALID", "单次补货数量须在 1-1000 之间")
	}
	if input.ExpiresInDays < 0 || input.ExpiresInDays > 3650 {
		return 0, 0, infraerrors.BadRequest("XIANYU_POOL_STOCK_EXPIRY_INVALID", "码过期天数须在 0-3650 之间（0 表示永不过期）")
	}
	pools, err := s.control.ListItemPools(ctx)
	if err != nil {
		return 0, 0, err
	}
	var pool *XianyuItemPool
	for i := range pools {
		if pools[i].ID == poolID {
			pool = &pools[i]
			break
		}
	}
	if pool == nil {
		return 0, 0, ErrXianyuItemPoolNotFound
	}
	spec := *pool
	if err := s.validatePoolCardSpec(ctx, &spec); err != nil {
		return 0, 0, err
	}
	var expiresAt *time.Time
	if input.ExpiresInDays > 0 {
		t := time.Now().AddDate(0, 0, input.ExpiresInDays)
		expiresAt = &t
	}
	codes := make([]string, 0, input.Count)
	for i := 0; i < input.Count; i++ {
		code, err := GenerateRedeemCode()
		if err != nil {
			return 0, 0, fmt.Errorf("generate pool stock code: %w", err)
		}
		codes = append(codes, code)
	}
	if err := s.control.InsertPoolStock(ctx, pool.Slug, *pool.GroupID, pool.ValidityDays, expiresAt, codes); err != nil {
		return 0, 0, err
	}
	remaining, _, _, _, err := s.control.PoolStockCounts(ctx, pool.Slug)
	if err != nil {
		return input.Count, 0, err
	}
	return input.Count, remaining, nil
}

func validPoolSlug(slug string) bool {
	if slug == "" {
		return false
	}
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 商品映射与绑定
// ---------------------------------------------------------------------------

func (s *XianyuControlService) ListProducts(ctx context.Context) ([]XianyuProduct, error) {
	return s.control.ListProducts(ctx)
}

// BindProduct 手工映射商品到池；解绑传 nil pool。
func (s *XianyuControlService) BindProduct(ctx context.Context, productID int64, poolID *int64, source string) error {
	product, err := s.control.GetProductByID(ctx, productID)
	if err != nil {
		return err
	}
	var boundPool *XianyuItemPool
	if poolID != nil {
		pool, err := s.control.GetItemPoolByID(ctx, *poolID)
		if err != nil {
			return err
		}
		if pool.Status != XianyuItemPoolStatusActive {
			return infraerrors.Conflict("XIANYU_ITEM_POOL_DISABLED", "cannot bind to a disabled item pool")
		}
		boundPool = pool
		if err := s.control.UpdateProductBinding(ctx, productID, XianyuBindingStatusMapped, source, poolID); err != nil {
			return err
		}
	} else if err := s.control.UpdateProductBinding(ctx, productID, XianyuBindingStatusUnmapped, source, nil); err != nil {
		return err
	}
	// 主程序绑定即唯一绑定动作：Worker 侧"商品→卡券"关系自动跟上。
	if err := s.syncProductCardBinding(ctx, product.ItemID, boundPool, poolID != nil); err != nil {
		// 用 infra 错误携带详情到 UI（普通 error 会被响应层吞成裸 internal error）。
		return infraerrors.Conflict("XIANYU_WORKER_CARD_SYNC_FAILED",
			fmt.Sprintf("绑定已保存，但同步 Worker 卡券关联失败：%v。Worker 恢复后重新保存绑定即可", err))
	}
	return nil
}

// syncProductCardBinding 绑定/解绑后同步 Worker 卡券关联：
// 绑定 → 关系覆盖为该池的专属发货卡券（缺失则自动供给）；解绑 → 清空关系。
func (s *XianyuControlService) syncProductCardBinding(ctx context.Context, itemID string, pool *XianyuItemPool, bound bool) error {
	if s.worker == nil {
		return nil
	}
	if !bound {
		return s.worker.SyncItemCard(ctx, itemID, 0)
	}
	if pool == nil {
		return nil
	}
	cardID, err := s.ensurePoolWorkerCard(ctx, pool)
	if err != nil {
		return err
	}
	return s.worker.SyncItemCard(ctx, itemID, cardID)
}

// ensurePoolWorkerCard 返回池的 Worker 发货卡券 ID；缺失时按池名幂等供给并持久化。
func (s *XianyuControlService) ensurePoolWorkerCard(ctx context.Context, pool *XianyuItemPool) (int64, error) {
	if pool.WorkerCardID != nil {
		return *pool.WorkerCardID, nil
	}
	cardID, err := s.worker.ProvisionPoolCard(ctx, "pool-"+pool.Slug)
	if err != nil {
		return 0, err
	}
	if err := s.control.UpdatePoolWorkerCardID(ctx, pool.ID, &cardID); err != nil {
		return 0, err
	}
	pool.WorkerCardID = &cardID
	return cardID, nil
}

// CreateProductSpecBinding 同一商品多档次：手动添加规格绑定。
// 规格名/规格值必须与闲鱼商品规格文案一致，买家拍下该规格时按此路由到对应池；
// 若商品行已存在则强制改为该池（幂等）。
func (s *XianyuControlService) CreateProductSpecBinding(ctx context.Context, productID int64, specName, specValue string, poolID int64) error {
	parent, err := s.control.GetProductByID(ctx, productID)
	if err != nil {
		return err
	}
	specName = strings.TrimSpace(specName)
	specValue = strings.TrimSpace(specValue)
	if specName == "" || specValue == "" {
		return infraerrors.BadRequest("XIANYU_SPEC_REQUIRED", "规格名与规格值不能为空")
	}
	pool, err := s.control.GetItemPoolByID(ctx, poolID)
	if err != nil {
		return err
	}
	if pool.Status != XianyuItemPoolStatusActive {
		return infraerrors.Conflict("XIANYU_ITEM_POOL_DISABLED", "cannot bind to a disabled item pool")
	}
	created, err := s.control.UpsertProduct(ctx, XianyuProduct{
		AccountPK:     parent.AccountPK,
		AccountID:     parent.AccountID,
		ItemID:        parent.ItemID,
		Title:         parent.Title,
		SpecName:      specName,
		SpecValue:     specValue,
		PoolID:        &poolID,
		BindingStatus: XianyuBindingStatusMapped,
		BindingSource: XianyuBindingSourceManual,
		Status:        XianyuProductStatusActive,
	})
	if err != nil {
		return err
	}
	// 已存在的行 UpsertProduct 不改池，显式强制绑定为该池（幂等）。
	if err := s.control.UpdateProductBinding(ctx, created.ID, XianyuBindingStatusMapped, XianyuBindingSourceManual, &poolID); err != nil {
		return err
	}
	if err := s.syncProductCardBinding(ctx, parent.ItemID, pool, true); err != nil {
		return infraerrors.Conflict("XIANYU_WORKER_CARD_SYNC_FAILED",
			fmt.Sprintf("绑定已保存，但同步 Worker 卡券关联失败：%v。Worker 恢复后重新保存绑定即可", err))
	}
	return nil
}

// AutoBindProducts 对所有 unmapped 商品执行自动绑定。
func (s *XianyuControlService) AutoBindProducts(ctx context.Context) error {
	products, err := s.control.ListProducts(ctx)
	if err != nil {
		return err
	}
	rules, err := s.control.ListBindingRules(ctx)
	if err != nil {
		return err
	}
	for _, p := range products {
		if p.BindingStatus != XianyuBindingStatusUnmapped {
			continue
		}
		if err := autoBindProduct(ctx, s.control, p, rules); err != nil {
			return err
		}
		// 自动绑定同样要保持 Worker 卡券关联同步。
		// 同步失败时回滚为 unmapped，保证下轮自动绑定会重试；
		// 否则 mapped 商品被跳过，会出现"已绑定但永不同步"的卡死。
		fresh, err := s.control.GetProductByID(ctx, p.ID)
		if err != nil {
			return err
		}
		if fresh.BindingStatus != XianyuBindingStatusMapped {
			continue
		}
		var boundPool *XianyuItemPool
		if fresh.PoolID != nil {
			if pool, poolErr := s.control.GetItemPoolByID(ctx, *fresh.PoolID); poolErr == nil {
				boundPool = pool
			}
		}
		if err := s.syncProductCardBinding(ctx, fresh.ItemID, boundPool, true); err != nil {
			if revertErr := s.control.UpdateProductBinding(ctx, fresh.ID, XianyuBindingStatusUnmapped, XianyuBindingSourceAutoNew, nil); revertErr != nil {
				return fmt.Errorf("sync worker card failed: %v; revert binding also failed: %w", err, revertErr)
			}
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 绑定规则
// ---------------------------------------------------------------------------

func (s *XianyuControlService) ListBindingRules(ctx context.Context) ([]XianyuBindingRule, error) {
	return s.control.ListBindingRules(ctx)
}

func (s *XianyuControlService) SaveBindingRule(ctx context.Context, rule XianyuBindingRule) (*XianyuBindingRule, error) {
	if rule.AccountPK <= 0 {
		return nil, infraerrors.BadRequest("XIANYU_RULE_ACCOUNT_REQUIRED", "binding rule account is required")
	}
	if rule.MatchType == XianyuBindingRuleKeyword {
		if strings.TrimSpace(rule.Keyword) == "" {
			return nil, infraerrors.BadRequest("XIANYU_RULE_KEYWORD_REQUIRED", "keyword is required for keyword rules")
		}
		rule.Keyword = strings.TrimSpace(rule.Keyword)
	} else {
		rule.MatchType = XianyuBindingRuleAccountDefault
		rule.Keyword = ""
	}
	if rule.PoolID <= 0 {
		return nil, infraerrors.BadRequest("XIANYU_RULE_POOL_REQUIRED", "binding rule pool is required")
	}
	if rule.Status == "" {
		rule.Status = "active"
	}
	if rule.ID == 0 {
		return s.control.CreateBindingRule(ctx, rule)
	}
	return s.control.UpdateBindingRule(ctx, rule)
}

// ---------------------------------------------------------------------------
// 账号操作（委托 Worker）
// ---------------------------------------------------------------------------

// ListAccounts 列出 Worker 账号。
func (s *XianyuControlService) ListAccounts(ctx context.Context) ([]XianyuAccount, error) {
	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		if err == ErrXianyuWorkerConfigNotFound {
			return nil, nil
		}
		return nil, err
	}
	return s.control.ListAccounts(ctx, workerCfg.ID)
}

// GetDeliveryTemplate 读取 Worker 全局发货模板（对所有卡券统一生效）。
func (s *XianyuControlService) GetDeliveryTemplate(ctx context.Context) (string, error) {
	if s.worker == nil {
		return "", ErrXianyuDeliveryNotConfigured
	}
	return s.worker.GetDeliveryTemplate(ctx)
}

// UpdateDeliveryTemplate 更新 Worker 全局发货模板。
func (s *XianyuControlService) UpdateDeliveryTemplate(ctx context.Context, template string) error {
	if s.worker == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	return s.worker.UpdateDeliveryTemplate(ctx, template)
}

// DeleteItemPool 删除库存池（售罄/下架清理；绑定商品、剩余库存码、引用规则任一存在时拒绝）。
func (s *XianyuControlService) DeleteItemPool(ctx context.Context, poolID int64) error {
	pool, poolErr := s.control.GetItemPoolByID(ctx, poolID)
	if poolErr != nil {
		return poolErr
	}
	if err := s.control.DeleteItemPool(ctx, poolID); err != nil {
		return err
	}
	// 顺手清理池的专属发货卡券（best-effort：残留卡券无商品关联，无实际危害）。
	if s.worker != nil && pool.WorkerCardID != nil {
		_ = s.worker.DeletePoolCard(ctx, *pool.WorkerCardID)
	}
	return nil
}

// DeleteBindingRule 删除绑定规则。
func (s *XianyuControlService) DeleteBindingRule(ctx context.Context, ruleID int64) error {
	return s.control.DeleteBindingRule(ctx, ruleID)
}

// MarkDeliveryClaimSent 管理员确认待处理发货记录的卡密已线下送达。
func (s *XianyuControlService) MarkDeliveryClaimSent(ctx context.Context, orderNo string) error {
	if s.delivery == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	return s.delivery.MarkClaimSent(ctx, orderNo)
}

// UpdateAccountRemark 更新账号运营备注。
func (s *XianyuControlService) UpdateAccountRemark(ctx context.Context, accountPK int64, remark string) error {
	return s.control.UpdateAccountRemark(ctx, accountPK, remark)
}

// EnableAccount 启用账号。
func (s *XianyuControlService) EnableAccount(ctx context.Context, accountID string) error {
	if s.worker == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	return s.worker.EnableAccount(ctx, accountID)
}

// DisableAccount 停用账号。
func (s *XianyuControlService) DisableAccount(ctx context.Context, accountID string) error {
	if s.worker == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	return s.worker.DisableAccount(ctx, accountID)
}

// RefreshCookie 刷新账号 Cookie。
func (s *XianyuControlService) RefreshCookie(ctx context.Context, accountID string) (*XianyuAccount, error) {
	if s.worker == nil {
		return nil, ErrXianyuDeliveryNotConfigured
	}
	return s.worker.RefreshCookie(ctx, accountID)
}

// ClearCredentials 退出/清除凭证：停止任务并删除 Worker 侧凭证，主程序投影标记 disabled。
func (s *XianyuControlService) ClearCredentials(ctx context.Context, accountID string) error {
	if s.worker == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	return s.worker.ClearCredentials(ctx, accountID)
}

// CheckHealth 立即执行健康检查。
func (s *XianyuControlService) CheckHealth(ctx context.Context) error {
	if s.worker == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	return s.worker.CheckHealth(ctx)
}

// SyncAccounts 立即同步账号。
func (s *XianyuControlService) SyncAccounts(ctx context.Context) error {
	if s.worker == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	return s.worker.SyncAccounts(ctx)
}

// SyncProducts 立即刷新商品。
func (s *XianyuControlService) SyncProducts(ctx context.Context) error {
	if s.worker == nil || s.sync == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	return s.sync.RunProductSync(ctx)
}

// CreateLoginSession 创建扫码会话。
func (s *XianyuControlService) CreateLoginSession(ctx context.Context, accountID string) (*XianyuWorkerLoginSessionStatus, error) {
	if s.worker == nil {
		return nil, ErrXianyuDeliveryNotConfigured
	}
	client, _, err := s.worker.clientForActiveWorker(ctx)
	if err != nil {
		return nil, err
	}
	return client.CreateLoginSession(ctx, accountID)
}

// QueryLoginSession 按 Worker session_id 查询扫码会话状态。
func (s *XianyuControlService) QueryLoginSession(ctx context.Context, sessionID string) (*XianyuWorkerLoginSessionStatus, error) {
	if s.worker == nil {
		return nil, ErrXianyuDeliveryNotConfigured
	}
	client, _, err := s.worker.clientForActiveWorker(ctx)
	if err != nil {
		return nil, err
	}
	return client.QueryLoginSession(ctx, sessionID)
}

// ---------------------------------------------------------------------------
// 发货记录
// ---------------------------------------------------------------------------

// ListDeliveryClaims 列出发货记录。
func (s *XianyuControlService) ListDeliveryClaims(ctx context.Context, filter XianyuDeliveryFilter) ([]XianyuOrderClaim, int, error) {
	if s.listRepo == nil {
		return nil, 0, ErrXianyuDeliveryNotConfigured
	}
	return s.listRepo.ListDeliveryClaims(ctx, filter)
}

// ListWorkerDeliveries 列出 Worker 自动发货记录（订单级）。
func (s *XianyuControlService) ListWorkerDeliveries(ctx context.Context, filter XianyuDeliveryFilter) ([]XianyuWorkerDelivery, int, error) {
	if s.delivery == nil {
		return nil, 0, ErrXianyuDeliveryNotConfigured
	}
	return s.delivery.ListWorkerDeliveries(ctx, filter)
}

// ResendOriginalCode 人工补发原码。
func (s *XianyuControlService) ResendOriginalCode(ctx context.Context, orderNo string) (string, error) {
	if s.delivery == nil {
		return "", ErrXianyuDeliveryNotConfigured
	}
	return s.delivery.ResendOriginalCode(ctx, orderNo)
}

// ---------------------------------------------------------------------------
// 概览
// ---------------------------------------------------------------------------// XianyuOverview 是概览页数据。
type XianyuOverview struct {
	WorkerHealthy       bool                 `json:"worker_healthy"`
	WorkerHealthStatus  string               `json:"worker_health_status"` // unknown / healthy / unhealthy
	WorkerLastCheckedAt *time.Time           `json:"worker_last_checked_at,omitempty"`
	EnabledAccounts     int                  `json:"enabled_accounts"`
	RunningTasks        int                  `json:"running_tasks"`
	UnmappedProducts    int                  `json:"unmapped_products"`
	Pools               []XianyuPoolOverview `json:"pools"`
	TodayDelivered      int                  `json:"today_delivered"`
	TodayFailed         int                  `json:"today_failed"`
	PendingDeliveries   int                  `json:"pending_deliveries"`
}

// XianyuPoolOverview 是池库存概览。
type XianyuPoolOverview struct {
	Pool      XianyuItemPool `json:"pool"`
	Remaining int            `json:"remaining"`
	Delivered int            `json:"delivered"`
	Used      int            `json:"used"`
	Disabled  int            `json:"disabled"`
	LowStock  bool           `json:"low_stock"`
}

func (s *XianyuControlService) GetOverview(ctx context.Context) (*XianyuOverview, error) {
	out := &XianyuOverview{Pools: make([]XianyuPoolOverview, 0)}

	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		if err == ErrXianyuWorkerConfigNotFound {
			// 未配置 Worker：返回空概览（前端据此展示未配置态）。
			out.WorkerHealthy = false
			return out, nil
		}
		return nil, err
	}
	out.WorkerHealthy = workerCfg.HealthStatus == XianyuWorkerHealthHealthy
	out.WorkerHealthStatus = workerCfg.HealthStatus
	out.WorkerLastCheckedAt = workerCfg.LastCheckedAt

	accounts, err := s.control.ListAccounts(ctx, workerCfg.ID)
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		if a.Status == XianyuAccountStatusEnabled {
			out.EnabledAccounts++
		}
		if a.TaskStatus == XianyuTaskStatusRunning {
			out.RunningTasks++
		}
	}

	products, err := s.control.ListProducts(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range products {
		if p.Status == XianyuProductStatusActive && p.BindingStatus == XianyuBindingStatusUnmapped {
			out.UnmappedProducts++
		}
	}

	pools, err := s.control.ListItemPools(ctx)
	if err != nil {
		return nil, err
	}
	for _, pool := range pools {
		po := XianyuPoolOverview{Pool: pool}
		remaining, delivered, used, disabled, err := s.control.PoolStockCounts(ctx, pool.Slug)
		if err != nil {
			return nil, err
		}
		po.Remaining = remaining
		po.Delivered = delivered
		po.Used = used
		po.Disabled = disabled
		po.LowStock = pool.Status == XianyuItemPoolStatusActive && pool.LowStockThreshold > 0 && po.Remaining <= pool.LowStockThreshold
		out.Pools = append(out.Pools, po)
	}

	if err := s.countTodayDeliveries(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *XianyuControlService) countTodayDeliveries(ctx context.Context, out *XianyuOverview) error {
	since := time.Now().UTC().Truncate(24 * time.Hour)
	sent, failed, err := s.control.DeliveryStats(ctx, since)
	if err != nil {
		return err
	}
	out.TodayDelivered = sent
	out.TodayFailed = failed
	pending, err := s.control.PendingDeliveryCount(ctx)
	if err != nil {
		return err
	}
	out.PendingDeliveries = pending
	return nil
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var msg string
	if ae := new(infraerrors.ApplicationError); errors.As(err, &ae) {
		msg = ae.Message
	} else {
		msg = err.Error()
	}
	return strings.Contains(strings.ToLower(msg), "unique")
}
