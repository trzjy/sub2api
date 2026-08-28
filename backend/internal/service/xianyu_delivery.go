package service

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
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
	ErrXianyuPoolNotMapped         = infraerrors.BadRequest("XIANYU_ITEM_POOL_NOT_MAPPED", "item is not mapped to a redeem-code pool")
	ErrXianyuInventoryEmpty        = infraerrors.Conflict("XIANYU_INVENTORY_EMPTY", "no redeem code is available for this item")
	ErrXianyuInvalidAmount         = infraerrors.BadRequest("XIANYU_AMOUNT_INVALID", "order_amount must be a valid decimal")
	ErrXianyuOrderTooLong          = infraerrors.BadRequest("XIANYU_ORDER_ID_TOO_LONG", "order_id is too long")
	ErrXianyuItemTooLong           = infraerrors.BadRequest("XIANYU_ITEM_ID_TOO_LONG", "item_id is too long")
	ErrXianyuAccountTooLong        = infraerrors.BadRequest("XIANYU_ACCOUNT_ID_TOO_LONG", "cookie_id is too long")
	ErrXianyuBuyerTooLong          = infraerrors.BadRequest("XIANYU_BUYER_ID_TOO_LONG", "buyer_id is too long")
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
}

type XianyuDeliveryClaim struct {
	OrderID   string
	ItemID    string
	AccountID string
	BuyerID   string
	Amount    *string
	Pool      string
}

type XianyuDeliveryRepository interface {
	Claim(ctx context.Context, claim XianyuDeliveryClaim, systemUserID int64) (string, error)
}

type XianyuDeliveryService struct {
	repo XianyuDeliveryRepository
	cfg  *config.Config
}

func NewXianyuDeliveryService(repo XianyuDeliveryRepository, cfg *config.Config) *XianyuDeliveryService {
	return &XianyuDeliveryService{repo: repo, cfg: cfg}
}

func (s *XianyuDeliveryService) Claim(ctx context.Context, req XianyuDeliveryClaimRequest) (string, error) {
	if s.cfg == nil || s.repo == nil || s.cfg.XianyuDelivery.SystemUserID <= 0 {
		return "", ErrXianyuDeliveryNotConfigured
	}
	claim, err := normalizeXianyuClaim(req, s.cfg.XianyuDelivery.ItemPools)
	if err != nil {
		return "", err
	}
	return s.repo.Claim(ctx, claim, s.cfg.XianyuDelivery.SystemUserID)
}

func normalizeXianyuClaim(req XianyuDeliveryClaimRequest, pools map[string]string) (XianyuDeliveryClaim, error) {
	claim := XianyuDeliveryClaim{
		OrderID:   strings.TrimSpace(req.OrderID),
		ItemID:    strings.TrimSpace(req.ItemID),
		AccountID: strings.TrimSpace(req.CookieID),
		BuyerID:   strings.TrimSpace(req.BuyerID),
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
	if strings.TrimSpace(req.OrderQuantity) != "1" {
		return XianyuDeliveryClaim{}, ErrXianyuQuantityUnsupported
	}
	pool := strings.TrimSpace(pools[claim.AccountID+":"+claim.ItemID])
	if pool == "" {
		return XianyuDeliveryClaim{}, ErrXianyuPoolNotMapped
	}
	claim.Pool = pool
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
	if s == nil || s.cfg == nil || !s.cfg.XianyuDelivery.Enabled {
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

type SystemUserReader interface {
	GetByIDIncludeDeleted(ctx context.Context, id int64) (*User, error)
}
