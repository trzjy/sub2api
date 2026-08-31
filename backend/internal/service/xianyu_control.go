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
	// 传输错误细分：Unreachable = 请求未到达 Worker（仅连接建立前的 dial 失败），可判定"确定未 dispatch"；
	// Timeout = 请求已发出、结果不确定；Uncertain = 请求可能已写出、结果不确定（连接重置/TLS 等）；
	// Malformed = 响应解码失败。
	ErrXianyuWorkerUnreachable = infraerrors.ServiceUnavailable("XIANYU_WORKER_UNREACHABLE", "xianyu worker is unreachable")
	ErrXianyuWorkerTimeout     = infraerrors.ServiceUnavailable("XIANYU_WORKER_TIMEOUT", "xianyu worker request timed out")
	ErrXianyuWorkerUncertain   = infraerrors.ServiceUnavailable("XIANYU_WORKER_UNCERTAIN", "xianyu worker request outcome uncertain")
	ErrXianyuWorkerMalformed   = infraerrors.BadRequest("XIANYU_WORKER_MALFORMED", "xianyu worker response malformed")
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
	ID                int64     `json:"id"`
	BaseURL           string    `json:"base_url"`
	APITokenEncrypted string    `json:"api_token_encrypted"`
	Status            string    `json:"status"`
	HealthStatus      string    `json:"health_status"`
	LastCheckedAt     *time.Time `json:"last_checked_at,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
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
	ID             int64      `json:"id"`
	WorkerConfigID int64      `json:"worker_config_id"`
	AccountID      string     `json:"account_id"`
	Nickname       string     `json:"nickname"`
	Status         string     `json:"status"`
	CookieStatus   string     `json:"cookie_status"`
	TaskStatus     string     `json:"task_status"`
	LastLoginAt    *time.Time `json:"last_login_at,omitempty"`
	LastSeenAt     *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// XianyuItemPoolStatus 表示商品池状态。
const (
	XianyuItemPoolStatusActive   = "active"
	XianyuItemPoolStatusDisabled = "disabled"
)

// XianyuItemPool 是库存池配置。
type XianyuItemPool struct {
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	Slug              string    `json:"slug"`
	Description       string    `json:"description"`
	LowStockThreshold int       `json:"low_stock_threshold"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
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
	ID            int64      `json:"id"`
	AccountPK     int64      `json:"account_pk"`
	AccountID     string     `json:"account_id"`
	ItemID        string     `json:"item_id"`
	Title         string     `json:"title"`
	SpecName      string     `json:"spec_name"`
	SpecValue     string     `json:"spec_value"`
	PoolID        *int64     `json:"pool_id"` // 未绑定为 null（前端类型必填 number|null）
	BindingStatus string     `json:"binding_status"`
	BindingSource string     `json:"binding_source"`
	Status        string     `json:"status"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// XianyuBindingRuleMatchType 表示绑定规则匹配类型。
const (
	XianyuBindingRuleAccountDefault = "account_default"
	XianyuBindingRuleKeyword        = "keyword"
)

// XianyuBindingRule 是商品自动绑定规则。
type XianyuBindingRule struct {
	ID        int64     `json:"id"`
	Priority  int       `json:"priority"`
	AccountPK int64     `json:"account_pk"`
	MatchType string    `json:"match_type"`
	Keyword   string    `json:"keyword"`
	PoolID    int64     `json:"pool_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
	OrderNo        string     `json:"order_no"`
	RedeemCodeID   int64      `json:"redeem_code_id"`
	Code           string     `json:"code"`
	AccountID      string     `json:"account_id"`
	ItemID         string     `json:"item_id"`
	BuyerID        string     `json:"buyer_id"`
	ChatID         string     `json:"chat_id"`
	Amount         *string    `json:"amount,omitempty"`
	ProductID      *int64     `json:"product_id,omitempty"`
	PoolID         *int64     `json:"pool_id,omitempty"`
	BindingSource  *string    `json:"binding_source,omitempty"`
	DeliveryStatus string     `json:"delivery_status"`
	DeliveryError  *string    `json:"delivery_error,omitempty"`
	AttemptCount   int        `json:"attempt_count"`
	LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// XianyuDeliveryStatusResult 是 Worker 回传的发货结果。
type XianyuDeliveryStatusResult struct {
	OrderNo   string
	Success   bool
	Error     *string
	Confirmed bool // 是否有最终发送回执；true 时才可标记 sent
	// Attempt 回执所属发送尝试代次（补发递增后由调用方携带）；0 表示未关联（兼容旧回执）。
	// 条件更新时仅当与记录当前 attempt_count 匹配才生效，防止旧 attempt 回执改变新状态。
	Attempt int
	// QuantitySent 实际成功获取/发送的卡券份数；仅在 sent（Success=true 且 Confirmed=true）
	// 路径下写入主程序 quantity_sent 字段。其它状态（pending/failed）保留默认 0，
	// 由调用方决定是否传值；值 < 0 视为未提供。
	QuantitySent int
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
	// UpdateWorkerConfigUserFields 仅更新 base_url 与 api_token_encrypted（token 留空时原地保留），
	// 不写 status / health_status / last_checked_at：status 由激活端点专管，健康字段由健康检查专管。
	UpdateWorkerConfigUserFields(ctx context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error)
	// UpdateWorkerHealth 仅写入健康检查结果字段（health_status / last_checked_at），
	// 不触碰 base_url / api_token_encrypted / status（admin 保存并发时避免健康检查回滚）。
	UpdateWorkerHealth(ctx context.Context, id int64, healthStatus string, lastCheckedAt time.Time) error
	// ActivateWorkerConfig 仅置 active 并写入新 token（status / api_token_encrypted / updated_at），
	// 不写 base_url 与健康字段：避免用激活读取的旧快照回滚并发 admin 保存的 base_url。
	ActivateWorkerConfig(ctx context.Context, id int64, encryptedToken string) (*XianyuWorkerConfig, error)
	GetActiveWorkerConfig(ctx context.Context) (*XianyuWorkerConfig, error)
	GetWorkerConfigByID(ctx context.Context, id int64) (*XianyuWorkerConfig, error)

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
