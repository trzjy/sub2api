package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrXianyuWorkerConfigNotFound   = infraerrors.NotFound("XIANYU_WORKER_CONFIG_NOT_FOUND", "xianyu worker config not found")
	ErrXianyuWorkerConfigExists     = infraerrors.Conflict("XIANYU_WORKER_CONFIG_EXISTS", "xianyu worker config already exists")
	ErrXianyuActiveWorkerExists     = infraerrors.Conflict("XIANYU_ACTIVE_WORKER_EXISTS", "only one active xianyu worker config is allowed")
	ErrXianyuAccountNotFound        = infraerrors.NotFound("XIANYU_ACCOUNT_NOT_FOUND", "xianyu account not found")
	ErrXianyuAccountDisabled        = infraerrors.Conflict("XIANYU_ACCOUNT_DISABLED", "xianyu account is disabled")
	ErrXianyuItemPoolNotFound       = infraerrors.NotFound("XIANYU_ITEM_POOL_NOT_FOUND", "xianyu item pool not found")
	ErrXianyuItemPoolSlugExists     = infraerrors.Conflict("XIANYU_ITEM_POOL_SLUG_EXISTS", "xianyu item pool slug already exists")
	ErrXianyuProductNotFound        = infraerrors.NotFound("XIANYU_PRODUCT_NOT_FOUND", "xianyu product not found")
	ErrXianyuBindingRuleNotFound    = infraerrors.NotFound("XIANYU_BINDING_RULE_NOT_FOUND", "xianyu binding rule not found")
	ErrXianyuProductUnmapped        = infraerrors.BadRequest("XIANYU_PRODUCT_UNMAPPED", "item is not bound to a redeem-code pool")
	ErrXianyuDeliveryClaimNotFound  = infraerrors.NotFound("XIANYU_DELIVERY_CLAIM_NOT_FOUND", "xianyu delivery claim not found")
	ErrXianyuDeliveryAlreadySent    = infraerrors.Conflict("XIANYU_DELIVERY_ALREADY_SENT", "xianyu delivery already sent")
	ErrXianyuWorkerUnhealthy        = infraerrors.ServiceUnavailable("XIANYU_WORKER_UNHEALTHY", "xianyu worker is unhealthy")
	ErrXianyuBaseURLInvalid         = infraerrors.BadRequest("XIANYU_BASE_URL_INVALID", "xianyu worker base_url must be http://<docker-hostname>:<port> or http://<private-ip>:<port>")
	ErrXianyuBaseURLLoopbackInvalid = infraerrors.BadRequest("XIANYU_BASE_URL_LOOPBACK_INVALID", "xianyu worker base_url must not use 127.0.0.1 inside the container deployment")
	ErrXianyuDeliveryUnavailable    = infraerrors.ServiceUnavailable("XIANYU_DELIVERY_UNAVAILABLE", "xianyu delivery is unavailable")
	ErrXianyuResendNotPending       = infraerrors.Conflict("XIANYU_RESEND_NOT_PENDING", "only failed deliveries can be resent")
	ErrXianyuSyncBusy               = infraerrors.Conflict("XIANYU_SYNC_BUSY", "product sync is already running")
	ErrXianyuForbidden              = infraerrors.New(403, "XIANYU_MANAGE_FORBIDDEN", "you are not authorized to manage xianyu delivery")
)

// Xianyu 控制面领域模型与仓库接口。
// 主程序保存业务主数据；Worker 只作为平台通道。

// XianyuWorkerStatus 表示 Worker 连接配置的启用状态。
const (
	XianyuWorkerStatusActive   = "active"
	XianyuWorkerStatusDisabled = "disabled"
)

// XianyuWorkerHealth 表示最近一次健康检查结果。
const (
	XianyuWorkerHealthUnknown   = "unknown"
	XianyuWorkerHealthHealthy   = "healthy"
	XianyuWorkerHealthUnhealthy = "unhealthy"
)

// XianyuWorkerConfig 是主程序对单条 Worker 内网连接配置的视图。
type XianyuWorkerConfig struct {
	ID                int64
	BaseURL           string
	APITokenEncrypted string
	Status            string
	HealthStatus      string
	LastCheckedAt     *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// XianyuAccountStatus 表示闲鱼账号状态。
const (
	XianyuAccountStatusEnabled  = "enabled"
	XianyuAccountStatusDisabled = "disabled"
	XianyuAccountStatusExpired  = "expired"
	XianyuAccountStatusSyncing  = "syncing"
)

// XianyuCookieStatus 表示账号 Cookie 状态。
const (
	XianyuCookieStatusValid    = "valid"
	XianyuCookieStatusInvalid  = "invalid"
	XianyuCookieStatusExpiring = "expiring"
	XianyuCookieStatusUnknown  = "unknown"
)

// XianyuTaskStatus 表示 Worker 内账号收消息任务状态。
const (
	XianyuTaskStatusRunning  = "running"
	XianyuTaskStatusStopped  = "stopped"
	XianyuTaskStatusStarting = "starting"
	XianyuTaskStatusStopping = "stopping"
	XianyuTaskStatusUnknown  = "unknown"
)

// XianyuAccount 是主程序保存的闲鱼账号状态视图。
type XianyuAccount struct {
	ID             int64
	WorkerConfigID int64
	AccountID      string
	Nickname       string
	Status         string
	CookieStatus   string
	TaskStatus     string
	LastLoginAt    *time.Time
	LastSeenAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// XianyuItemPoolStatus 表示商品池状态。
const (
	XianyuItemPoolStatusActive   = "active"
	XianyuItemPoolStatusDisabled = "disabled"
)

// XianyuItemPool 是库存池配置。
type XianyuItemPool struct {
	ID                int64
	Name              string
	Slug              string
	Description       string
	LowStockThreshold int
	Status            string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// XianyuBindingStatus 表示商品绑定状态。
const (
	XianyuBindingStatusMapped   = "mapped"
	XianyuBindingStatusUnmapped = "unmapped"
)

// XianyuBindingSource 表示商品绑定来源。
const (
	XianyuBindingSourceManual           = "manual"
	XianyuBindingSourceAccountDefault   = "account_default"
	XianyuBindingSourceKeyword          = "keyword"
	XianyuBindingSourceAutoNew          = "auto_new"
	XianyuBindingSourceLegacyUnresolved = "legacy_unresolved"
)

// XianyuProductStatus 表示商品记录状态。
const (
	XianyuProductStatusActive   = "active"
	XianyuProductStatusDisabled = "disabled"
	XianyuProductStatusRemoved  = "removed"
)

// XianyuProduct 是商品映射。
type XianyuProduct struct {
	ID            int64
	AccountPK     int64
	AccountID     string
	ItemID        string
	Title         string
	SpecName      string
	SpecValue     string
	PoolID        *int64
	BindingStatus string
	BindingSource string
	Status        string
	LastSeenAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// XianyuBindingRuleMatchType 表示绑定规则匹配类型。
const (
	XianyuBindingRuleAccountDefault = "account_default"
	XianyuBindingRuleKeyword        = "keyword"
)

// XianyuBindingRule 是商品自动绑定规则。
type XianyuBindingRule struct {
	ID        int64
	Priority  int
	AccountPK int64
	MatchType string
	Keyword   string
	PoolID    int64
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// XianyuDeliveryStatus 表示发货状态。
const (
	XianyuDeliveryStatusPending          = "pending"
	XianyuDeliveryStatusSent             = "sent"
	XianyuDeliveryStatusFailed           = "failed"
	XianyuDeliveryStatusLegacyUnverified = "legacy_unverified"
)

// XianyuOrderClaim 是发货记录（幂等事实表视图）。
type XianyuOrderClaim struct {
	OrderNo        string
	RedeemCodeID   int64
	Code           string
	AccountID      string
	ItemID         string
	BuyerID        string
	ChatID         string
	Amount         *string
	ProductID      *int64
	PoolID         *int64
	BindingSource  *string
	DeliveryStatus string
	DeliveryError  *string
	AttemptCount   int
	LastAttemptAt  *time.Time
	CreatedAt      time.Time
}

// XianyuDeliveryStatusResult 是 Worker 回传的发货结果。
type XianyuDeliveryStatusResult struct {
	OrderNo   string
	Success   bool
	Error     *string
	Confirmed bool // 是否有最终发送回执；true 时才可标记 sent
}

// XianyuDeliveryFilter 是发货记录列表筛选条件。
type XianyuDeliveryFilter struct {
	Status string
	Search string
	Offset int
	Limit  int
}

// XianyuControlRepository 聚合控制面所有数据访问。
type XianyuControlRepository interface {
	// Worker 连接配置
	ListWorkerConfigs(ctx context.Context) ([]XianyuWorkerConfig, error)
	CreateWorkerConfig(ctx context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error)
	UpdateWorkerConfig(ctx context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error)
	GetActiveWorkerConfig(ctx context.Context) (*XianyuWorkerConfig, error)

	// 闲鱼账号
	ListAccounts(ctx context.Context, workerConfigID int64) ([]XianyuAccount, error)
	GetAccountByWorkerAndAccountID(ctx context.Context, workerConfigID int64, accountID string) (*XianyuAccount, error)
	UpsertAccount(ctx context.Context, account XianyuAccount) (*XianyuAccount, error)
	UpdateAccount(ctx context.Context, account XianyuAccount) (*XianyuAccount, error)

	// 商品池
	ListItemPools(ctx context.Context) ([]XianyuItemPool, error)
	GetItemPoolByID(ctx context.Context, id int64) (*XianyuItemPool, error)
	GetItemPoolBySlug(ctx context.Context, slug string) (*XianyuItemPool, error)
	CreateItemPool(ctx context.Context, pool XianyuItemPool) (*XianyuItemPool, error)
	UpdateItemPool(ctx context.Context, pool XianyuItemPool) (*XianyuItemPool, error)
	PoolStockCounts(ctx context.Context, poolSlug string) (remaining, used, disabled int, err error)
	DeliveryStats(ctx context.Context, since time.Time) (sent, failed int, err error)
	PendingDeliveryCount(ctx context.Context) (int, error)

	// 商品映射
	ListProducts(ctx context.Context) ([]XianyuProduct, error)
	ListProductsByAccount(ctx context.Context, accountPK int64) ([]XianyuProduct, error)
	GetProductByIdentity(ctx context.Context, accountPK int64, itemID, specName, specValue string) (*XianyuProduct, error)
	UpsertProduct(ctx context.Context, product XianyuProduct) (*XianyuProduct, error)
	UpdateProduct(ctx context.Context, product XianyuProduct) (*XianyuProduct, error)
	UpdateProductBinding(ctx context.Context, productID int64, bindingStatus, bindingSource string, poolID *int64) error

	// 绑定规则
	ListBindingRules(ctx context.Context) ([]XianyuBindingRule, error)
	CreateBindingRule(ctx context.Context, rule XianyuBindingRule) (*XianyuBindingRule, error)
	UpdateBindingRule(ctx context.Context, rule XianyuBindingRule) (*XianyuBindingRule, error)
}

// XianyuLegacyMigrationMarker 提供旧 item_pools 迁移完成标记。
type XianyuLegacyMigrationMarker interface {
	MarkLegacyMigrated(ctx context.Context) error
	IsLegacyMigrated(ctx context.Context) (bool, error)
}

// XianyuDeliveryListRepository 提供发货记录查询（admin 面板）。
type XianyuDeliveryListRepository interface {
	ListDeliveryClaims(ctx context.Context, filter XianyuDeliveryFilter) ([]XianyuOrderClaim, int, error)
}
