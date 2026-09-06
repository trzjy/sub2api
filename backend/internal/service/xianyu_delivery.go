package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var xianyuAmountPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)(?:\.[0-9]{1,2})?$`)

var (
	ErrXianyuDeliveryNotConfigured = infraerrors.ServiceUnavailable("XIANYU_DELIVERY_NOT_CONFIGURED", "xianyu delivery is not configured")
	ErrXianyuOrderRequired         = infraerrors.BadRequest("XIANYU_ORDER_ID_REQUIRED", "order_id is required")
	ErrXianyuQuantityUnsupported   = infraerrors.BadRequest("XIANYU_QUANTITY_UNSUPPORTED", "order_quantity must be 1")
	ErrXianyuItemRequired          = infraerrors.BadRequest("XIANYU_ITEM_ID_REQUIRED", "item_id is required")
	ErrXianyuAccountRequired       = infraerrors.BadRequest("XIANYU_ACCOUNT_ID_REQUIRED", "cookie_id is required")
	ErrXianyuBuyerRequired         = infraerrors.BadRequest("XIANYU_BUYER_ID_REQUIRED", "buyer_id is required")
	ErrXianyuChatRequired          = infraerrors.BadRequest("XIANYU_CHAT_ID_REQUIRED", "chat_id is required")
	ErrXianyuPoolNotMapped         = infraerrors.BadRequest("XIANYU_ITEM_POOL_NOT_MAPPED", "item is not mapped to a redeem-code pool")
	ErrXianyuInventoryEmpty        = infraerrors.Conflict("XIANYU_INVENTORY_EMPTY", "no redeem code is available for this item")
	ErrXianyuInvalidAmount         = infraerrors.BadRequest("XIANYU_AMOUNT_INVALID", "order_amount must be a valid decimal")
	ErrXianyuOrderTooLong          = infraerrors.BadRequest("XIANYU_ORDER_ID_TOO_LONG", "order_id is too long")
	ErrXianyuItemTooLong           = infraerrors.BadRequest("XIANYU_ITEM_ID_TOO_LONG", "item_id is too long")
	ErrXianyuAccountTooLong        = infraerrors.BadRequest("XIANYU_ACCOUNT_ID_TOO_LONG", "cookie_id is too long")
	ErrXianyuBuyerTooLong          = infraerrors.BadRequest("XIANYU_BUYER_ID_TOO_LONG", "buyer_id is too long")
	ErrXianyuChatTooLong           = infraerrors.BadRequest("XIANYU_CHAT_ID_TOO_LONG", "chat_id is too long")
	ErrXianyuDeliveryQuantitySentOutOfRange = infraerrors.BadRequest("XIANYU_DELIVERY_QUANTITY_SENT_OUT_OF_RANGE", "quantity_sent must be between 0 and order quantity")
	// ErrXianyuResendUndispatched 标记人工补发"确定未向 Worker 发出发送请求"的错误
	// （无 active Worker、账号不可用、请求构建前失败等）。这类错误必须回滚 pending→failed，
	// 保持人工可重试；只有"可能已 dispatch / 发送结果不确定"时才保留 pending。
	ErrXianyuResendUndispatched = errors.New("xianyu resend not dispatched")
	// ErrXianyuResendRejected 标记补发"消息已 dispatch 但被平台明确拒绝"（如 CSI_FORBID 拦截）。
	// 与 ErrXianyuResendUndispatched 一样属于"确定未送达"，必须回滚 pending→failed，
	// 与 Worker 侧异步兜底回传（REJECTED → success=false）收敛一致，避免同步/异步终态分歧。
	ErrXianyuResendRejected = errors.New("xianyu resend rejected")
)

type XianyuDeliveryClaimRequest struct {
	OrderID       string `json:"order_id"`
	ItemID        string `json:"item_id"`
	ItemDetail    string `json:"item_detail"`
	OrderAmount   string `json:"order_amount"`
	OrderQuantity string `json:"order_quantity"`
	SpecName      string `json:"spec_name"`
	SpecValue     string `json:"spec_value"`
	CookieID      string `json:"cookie_id"`
	BuyerID       string `json:"buyer_id"`
	ChatID        string `json:"chat_id"`
}

type XianyuDeliveryClaim struct {
	OrderID   string
	ItemID    string
	AccountID string
	BuyerID   string
	ChatID    string
	Amount    *string
	SpecName  string
	SpecValue string

	AccountPK     int64
	ProductID     int64
	PoolID        int64
	BindingSource string
}

// XianyuDeliveryRepository 负责幂等领取与发货状态持久化。
type XianyuDeliveryRepository interface {
	Claim(ctx context.Context, claim XianyuDeliveryClaim, systemUserID int64) (string, error)
}

type XianyuDeliveryService struct {
	repo           XianyuDeliveryRepository
	control        XianyuControlRepository
	cfg            *config.Config
	setting        XianyuDeliverySettingReader
	delivery       XianyuDeliveryStateUpdater
	workerDelivery XianyuWorkerDeliveryRepository
	workerSvc      *XianyuWorkerService
	resender       func(ctx context.Context, claim *XianyuOrderClaim) error
}

type XianyuDeliverySettingReader interface {
	GetXianyuDeliveryRuntime(ctx context.Context) XianyuDeliveryRuntime
}

// XianyuDeliveryStateUpdater 更新发货状态（适配端点回传 + 人工补发）。
// 所有状态写（回执 / 补发成功 / 补发回滚）统一收敛到 RecordDeliveryResult，
// 由它按 attempt 代次做 CAS 隔离；ResendOriginalCode 仅负责 failed→pending 发起补发。
type XianyuDeliveryStateUpdater interface {
	RecordDeliveryResult(ctx context.Context, result XianyuDeliveryStatusResult) error
	GetDeliveryClaim(ctx context.Context, orderNo string) (*XianyuOrderClaim, error)
	// ResendOriginalCode 把 failed→pending（attempt_count+1），返回新 attempt 用于回执关联。
	ResendOriginalCode(ctx context.Context, orderNo string) (string, int, error)
}

func NewXianyuDeliveryService(
	repo XianyuDeliveryRepository,
	control XianyuControlRepository,
	stateUpdater XianyuDeliveryStateUpdater,
	workerDelivery XianyuWorkerDeliveryRepository,
	cfg *config.Config,
	setting XianyuDeliverySettingReader,
	workerSvc *XianyuWorkerService,
) *XianyuDeliveryService {
	return &XianyuDeliveryService{
		repo: repo, control: control, delivery: stateUpdater,
		workerDelivery: workerDelivery, cfg: cfg, setting: setting, workerSvc: workerSvc,
	}
}

// Claim 处理闲鱼订单领取请求。
// 链路：校验账号 → 校验商品绑定 → 按订单加锁领取库存码。
func (s *XianyuDeliveryService) Claim(ctx context.Context, req XianyuDeliveryClaimRequest) (string, error) {
	if s.cfg == nil || s.repo == nil || s.control == nil || s.cfg.XianyuDelivery.SystemUserID <= 0 {
		return "", ErrXianyuDeliveryNotConfigured
	}
	if s.setting == nil || !s.setting.GetXianyuDeliveryRuntime(ctx).Enabled {
		return "", ErrXianyuDeliveryNotConfigured
	}
	claim, err := s.normalizeAndResolveClaim(ctx, req)
	if err != nil {
		return "", err
	}
	return s.repo.Claim(ctx, claim, s.cfg.XianyuDelivery.SystemUserID)
}

// normalizeAndResolveClaim 校验请求并把 cookie_id / item_id 解析为主程序内部身份与库存池。
func (s *XianyuDeliveryService) normalizeAndResolveClaim(ctx context.Context, req XianyuDeliveryClaimRequest) (XianyuDeliveryClaim, error) {
	claim, err := normalizeXianyuClaim(req)
	if err != nil {
		return XianyuDeliveryClaim{}, err
	}

	workerCfg, err := s.control.GetActiveWorkerConfig(ctx)
	if err != nil {
		if err == ErrXianyuWorkerConfigNotFound {
			return XianyuDeliveryClaim{}, ErrXianyuDeliveryNotConfigured
		}
		return XianyuDeliveryClaim{}, fmt.Errorf("load active xianyu worker: %w", err)
	}

	account, err := s.control.GetAccountByWorkerAndAccountID(ctx, workerCfg.ID, claim.AccountID)
	if err != nil {
		if err == ErrXianyuAccountNotFound {
			return XianyuDeliveryClaim{}, ErrXianyuAccountRequired
		}
		return XianyuDeliveryClaim{}, fmt.Errorf("load xianyu account: %w", err)
	}
	if account.Status != XianyuAccountStatusEnabled {
		return XianyuDeliveryClaim{}, ErrXianyuAccountDisabled
	}

	product, err := s.control.GetProductByIdentity(ctx, account.ID, claim.ItemID, claim.SpecName, claim.SpecValue)
	if err != nil {
		if err == ErrXianyuProductNotFound {
			return XianyuDeliveryClaim{}, ErrXianyuPoolNotMapped
		}
		return XianyuDeliveryClaim{}, fmt.Errorf("load xianyu product: %w", err)
	}
	if product.Status != XianyuProductStatusActive || product.BindingStatus != XianyuBindingStatusMapped || product.PoolID == nil {
		return XianyuDeliveryClaim{}, ErrXianyuProductUnmapped
	}

	claim.AccountPK = account.ID
	claim.ProductID = product.ID
	claim.PoolID = *product.PoolID
	claim.BindingSource = product.BindingSource
	return claim, nil
}

func normalizeXianyuClaim(req XianyuDeliveryClaimRequest) (XianyuDeliveryClaim, error) {
	claim := XianyuDeliveryClaim{
		OrderID:   strings.TrimSpace(req.OrderID),
		ItemID:    strings.TrimSpace(req.ItemID),
		AccountID: strings.TrimSpace(req.CookieID),
		BuyerID:   strings.TrimSpace(req.BuyerID),
		ChatID:    strings.TrimSpace(req.ChatID),
		SpecName:  strings.TrimSpace(req.SpecName),
		SpecValue: strings.TrimSpace(req.SpecValue),
	}
	if claim.OrderID == "" {
		return XianyuDeliveryClaim{}, ErrXianyuOrderRequired
	}
	if utf8.RuneCountInString(claim.OrderID) > 64 {
		return XianyuDeliveryClaim{}, ErrXianyuOrderTooLong
	}
	if claim.ItemID == "" {
		return XianyuDeliveryClaim{}, ErrXianyuItemRequired
	}
	if utf8.RuneCountInString(claim.ItemID) > 64 {
		return XianyuDeliveryClaim{}, ErrXianyuItemTooLong
	}
	if claim.AccountID == "" {
		return XianyuDeliveryClaim{}, ErrXianyuAccountRequired
	}
	if utf8.RuneCountInString(claim.AccountID) > 80 {
		return XianyuDeliveryClaim{}, ErrXianyuAccountTooLong
	}
	if claim.BuyerID == "" {
		return XianyuDeliveryClaim{}, ErrXianyuBuyerRequired
	}
	if utf8.RuneCountInString(claim.BuyerID) > 80 {
		return XianyuDeliveryClaim{}, ErrXianyuBuyerTooLong
	}
	if claim.ChatID == "" {
		return XianyuDeliveryClaim{}, ErrXianyuChatRequired
	}
	if utf8.RuneCountInString(claim.ChatID) > 120 {
		return XianyuDeliveryClaim{}, ErrXianyuChatTooLong
	}
	if strings.TrimSpace(req.OrderQuantity) != "1" {
		return XianyuDeliveryClaim{}, ErrXianyuQuantityUnsupported
	}
	if amount := strings.TrimSpace(req.OrderAmount); amount != "" {
		if !xianyuAmountPattern.MatchString(amount) || integerDigits(amount) > 18 {
			return XianyuDeliveryClaim{}, ErrXianyuInvalidAmount
		}
		value, err := strconv.ParseFloat(amount, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return XianyuDeliveryClaim{}, ErrXianyuInvalidAmount
		}
		claim.Amount = &amount
	}
	return claim, nil
}

func integerDigits(amount string) int {
	integerPart := amount
	if idx := strings.IndexByte(amount, '.'); idx >= 0 {
		integerPart = amount[:idx]
	}
	return len(integerPart)
}

func xianyuPoolNote(pool string) string {
	return "xianyu_pool=" + pool
}

// XianyuPoolNote is the immutable pool marker format used by inventory rows.
func XianyuPoolNote(pool string) string {
	return xianyuPoolNote(strings.TrimSpace(pool))
}

func (s *XianyuDeliveryService) ValidateConfiguration() error {
	if s == nil || s.cfg == nil || strings.TrimSpace(s.cfg.XianyuDelivery.InternalToken) == "" || s.cfg.XianyuDelivery.SystemUserID <= 0 {
		return ErrXianyuDeliveryNotConfigured
	}
	return nil
}

// ValidateStartup enforces the protected audit user only when delivery is on.
func (s *XianyuDeliveryService) ValidateStartup(ctx context.Context, users SystemUserReader) error {
	if s == nil || s.cfg == nil || s.setting == nil || !s.setting.GetXianyuDeliveryRuntime(ctx).Enabled {
		return nil
	}
	if err := s.ValidateConfiguration(); err != nil {
		return err
	}
	user, err := users.GetByIDIncludeDeleted(ctx, s.cfg.XianyuDelivery.SystemUserID)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("xianyu delivery system user %d not found", s.cfg.XianyuDelivery.SystemUserID)
		}
		return fmt.Errorf("load xianyu delivery system user: %w", err)
	}
	if user == nil || user.DeletedAt != nil || strings.TrimSpace(user.Status) != StatusActive {
		return fmt.Errorf("xianyu delivery system user %d is unavailable", s.cfg.XianyuDelivery.SystemUserID)
	}
	return nil
}

// RecordDeliveryResult 转发给状态更新器（适配端点回传）。
func (s *XianyuDeliveryService) RecordDeliveryResult(ctx context.Context, result XianyuDeliveryStatusResult) error {
	if s.delivery == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	return s.delivery.RecordDeliveryResult(ctx, result)
}

// EnsureWorkerDeliveryRecord 幂等创建 Worker 发货记录（订单级）。
func (s *XianyuDeliveryService) EnsureWorkerDeliveryRecord(ctx context.Context, d XianyuWorkerDelivery) error {
	if s == nil || s.workerDelivery == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	return s.workerDelivery.EnsureWorkerDeliveryRecord(ctx, d)
}

// RecordWorkerDeliveryResult 更新 Worker 发货记录；记录不存在返回 ErrXianyuDeliveryClaimNotFound，
// 供 delivery-results 端点按 order_no 在 Worker 记录与主程序库存记录之间路由。
// 未注入 Worker 记录仓库（未启用 Worker 发货记录适配）时同样返回 not found，交回主程序记录路径。
func (s *XianyuDeliveryService) RecordWorkerDeliveryResult(ctx context.Context, result XianyuDeliveryStatusResult) error {
	if s == nil || s.workerDelivery == nil {
		return ErrXianyuDeliveryClaimNotFound
	}
	return s.workerDelivery.RecordWorkerDeliveryResult(ctx, result.OrderNo, result)
}

// ListWorkerDeliveries 分页列出 Worker 发货记录。
func (s *XianyuDeliveryService) ListWorkerDeliveries(ctx context.Context, filter XianyuDeliveryFilter) ([]XianyuWorkerDelivery, int, error) {
	if s == nil || s.workerDelivery == nil {
		return nil, 0, ErrXianyuDeliveryNotConfigured
	}
	return s.workerDelivery.ListWorkerDeliveries(ctx, filter)
}

// ResendOriginalCode 人工补发原码。
func (s *XianyuDeliveryService) ResendOriginalCode(ctx context.Context, orderNo string) (string, error) {
	if s == nil || s.delivery == nil || s.setting == nil || !s.setting.GetXianyuDeliveryRuntime(ctx).Enabled {
		return "", ErrXianyuDeliveryNotConfigured
	}
	claim, err := s.delivery.GetDeliveryClaim(ctx, orderNo)
	if err != nil {
		return "", err
	}
	resender := s.resender
	if resender == nil {
		if s.workerSvc == nil {
			return "", ErrXianyuDeliveryNotConfigured
		}
		resender = s.workerSvc.ResendDelivery
	}
	// 先把 failed → pending（事务内 advisory lock + 状态校验 + attempt CAS），
	// 再触发 Worker 发送：即使发送成功但响应丢失/DB 提交失败，状态已不在 failed，
	// 管理员重试不会触发双重发货。
	code, attempt, err := s.delivery.ResendOriginalCode(ctx, orderNo)
	if err != nil {
		return "", err
	}
	// 让补发发送链路携带本次 attempt 代次（用于回执关联，旧 attempt 回执不得改变新状态）。
	claim.AttemptCount = attempt
	if err := resender(ctx, claim); err != nil {
		// 确定未送达（未 dispatch：无 active Worker / 账号不可用 / 请求构建前失败；
		// 或已 dispatch 但被平台明确拒绝：rejected）→ 回滚 pending→failed
		// （RecordDeliveryResult 失败回执），保持可人工重试；
		// 可能已 dispatch / 结果不确定的错误保留 pending（等待回执或转人工）。
		if errors.Is(err, ErrXianyuResendUndispatched) || errors.Is(err, ErrXianyuResendRejected) {
			reason := err.Error()
			if failErr := s.delivery.RecordDeliveryResult(ctx, XianyuDeliveryStatusResult{
				OrderNo: orderNo, Success: false, Error: &reason, Attempt: attempt,
			}); failErr != nil {
				return "", fmt.Errorf("xianyu resend rollback failed: %w (send error: %v)", failErr, err)
			}
		}
		return "", err
	}
	// 明确成功回执（resender 返回 nil = Worker 确认发送成功）：
	// 按 attempt 条件原子 pending/failed → sent（RecordDeliveryResult 成功回执），避免订单永久滞留 pending。
	if err := s.delivery.RecordDeliveryResult(ctx, XianyuDeliveryStatusResult{
		OrderNo: orderNo, Success: true, Confirmed: true, Attempt: attempt,
	}); err != nil {
		return "", fmt.Errorf("mark xianyu resend sent: %w", err)
	}
	return code, nil
}

type SystemUserReader interface {
	GetByIDIncludeDeleted(ctx context.Context, id int64) (*User, error)
}

// XianyuWorkerDelivery 是 Worker 自动发货的订单级记录。
// 只记录订单级汇总（数量/状态/错误），不复制 Worker 卡券内容或卡券模型；
// Worker 保持本地库存与逐份发货实现，两边只通过 order_no、数量和结果回传做幂等关联。
type XianyuWorkerDelivery struct {
	OrderNo        string     `json:"order_no"`
	DeliveryKind   string     `json:"delivery_kind"` // auto / manual / redelivery
	Quantity       int        `json:"quantity"`
	QuantitySent   int        `json:"quantity_sent"`
	DeliveryStatus string     `json:"delivery_status"`
	DeliveryError  *string    `json:"delivery_error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// XianyuWorkerDeliveryRepository 维护 Worker 发货记录。
type XianyuWorkerDeliveryRepository interface {
	// EnsureWorkerDeliveryRecord 按 order_no 幂等创建/合并记录：不存在则插入（pending），
	// 已存在不覆盖状态；仅当传入 quantity 更大时更新（请求重试不会重复重置状态）。
	EnsureWorkerDeliveryRecord(ctx context.Context, d XianyuWorkerDelivery) error
	// RecordWorkerDeliveryResult 按 order_no 更新 Worker 发货记录状态：
	// success+confirmed→sent、!success→failed、其余（unknown）保持 pending；终态不降级。
	RecordWorkerDeliveryResult(ctx context.Context, orderNo string, result XianyuDeliveryStatusResult) error
	ListWorkerDeliveries(ctx context.Context, filter XianyuDeliveryFilter) ([]XianyuWorkerDelivery, int, error)
}
