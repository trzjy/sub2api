package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// 福利兑换卡口径（design.md 第二节，用户拍板）：
//   - 卡的兑换有效期 = 生成后 24 小时；
//   - 福利余额自兑换起 7 天有效，过期自动清零；
//   - 每张卡的福利余额独立一行（welfare_balances），批次间互不影响；
//   - 扣减优先于充值余额，且优先扣离清零时间最近的行；
//   - 同批次每人限兑一张（DB 部分唯一索引兜底）。
const (
	WelfareCodeValidity    = 24 * time.Hour
	WelfareBalanceValidity = 7 * 24 * time.Hour

	WelfareStatusActive    = "active"
	WelfareStatusExhausted = "exhausted"
	WelfareStatusExpired   = "expired"

	welfareMaxGroupGrants = 50
)

// ErrRedeemBatchOnePerUser 同批次福利卡每人限兑一张（唯一索引 idx_redeem_codes_batch_one_per_user 命中）。
var ErrRedeemBatchOnePerUser = infraerrors.Conflict("REDEEM_BATCH_ONE_PER_USER", "本批次福利卡每人限兑一张")

// WelfareGroupGrant 福利卡上勾选的一个订阅分组及其有效天数（天卡=1/周卡=7/月卡=30）。
type WelfareGroupGrant struct {
	GroupID      int64  `json:"group_id"`
	ValidityDays int    `json:"validity_days"`
	GroupName    string `json:"group_name,omitempty"`
}

// RedeemBatch 福利批次元数据 + 运营统计。
type RedeemBatch struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	Config    map[string]any `json:"config"`
	CodeCount int            `json:"code_count"`
	CreatedBy int64          `json:"created_by"`
	CreatedAt time.Time      `json:"created_at"`

	// 统计（ListBatches 时填充）
	UsedCount      int64   `json:"used_count"`
	ClearedAmount  float64 `json:"cleared_amount"`
	RemainingCount int64   `json:"remaining_count"`
}

// WelfareBalance 一行 = 一张卡产生的独立福利余额。
type WelfareBalance struct {
	ID              int64     `json:"id"`
	UserID          int64     `json:"user_id"`
	RedeemCodeID    int64     `json:"redeem_code_id"`
	BatchID         int64     `json:"batch_id"`
	AmountInitial   float64   `json:"amount_initial"`
	AmountRemaining float64   `json:"amount_remaining"`
	Status          string    `json:"status"`
	ExpiresAt       time.Time `json:"expires_at"`
	CreatedAt       time.Time `json:"created_at"`
}

// WelfareSummary 用户视角的福利余额汇总。
type WelfareSummary struct {
	TotalRemaining   float64    `json:"total_remaining"`
	NearestExpiresAt *time.Time `json:"nearest_expires_at,omitempty"`
}

// WelfareRepository 福利卡相关持久化。所有方法通过 clientFromContext 支持事务复用。
type WelfareRepository interface {
	// CreateWelfareBatch 单事务写入：批次行 + N 张码 + N×G 行分组勾选。
	CreateWelfareBatch(ctx context.Context, batch *RedeemBatch, codes []RedeemCode, grants []WelfareGroupGrant) error
	ListBatches(ctx context.Context, params pagination.PaginationParams) ([]RedeemBatch, *pagination.PaginationResult, error)
	AttachCodeGroups(ctx context.Context, redeemCodeID int64, grants []WelfareGroupGrant) error
	ListCodeGroups(ctx context.Context, redeemCodeID int64) ([]WelfareGroupGrant, error)
	CreateWelfareBalance(ctx context.Context, wb *WelfareBalance) error
	// SumActiveRemainingByUser 汇总用户当前可用（active 且未过期）福利余额。
	SumActiveRemainingByUser(ctx context.Context, userID int64) (float64, error)
	// NearestActiveExpiry 返回用户最近一笔可用福利余额的清零时间。
	NearestActiveExpiry(ctx context.Context, userID int64) (*time.Time, error)
	// ExpireDueBalances 清零到期福利余额，返回清零行数与总额。只动 welfare_balances。
	ExpireDueBalances(ctx context.Context) (int64, float64, error)
	// ListCodesByBatch 返回批次下的卡密（status 可选过滤，如 "unused"），按 id 升序。
	ListCodesByBatch(ctx context.Context, batchID int64, status string) ([]RedeemCode, error)
}

// GenerateWelfareBatchRequest 创建福利批次入参。
type GenerateWelfareBatchRequest struct {
	Name      string             `json:"name"`
	Groups    []WelfareGroupGrant `json:"groups"`
	AmountMin float64            `json:"amount_min"`
	AmountMax float64            `json:"amount_max"`
	Count     int                `json:"count"`
	CreatedBy int64              `json:"-"`
}

func (req *GenerateWelfareBatchRequest) hasAmount() bool {
	return req.AmountMax > 0
}

// GenerateWelfareBatch 生成一批福利兑换卡（纯订阅卡 / 纯余额卡 / 混合卡）。
func (s *RedeemService) GenerateWelfareBatch(ctx context.Context, req *GenerateWelfareBatchRequest) (*RedeemBatch, []RedeemCode, error) {
	if s.welfareRepo == nil {
		return nil, nil, errors.New("welfare repository is not configured")
	}
	if req == nil {
		return nil, nil, infraerrors.BadRequest("WELFARE_BATCH_INVALID", "request is required")
	}
	if req.Count <= 0 || req.Count > 1000 {
		return nil, nil, infraerrors.BadRequest("WELFARE_BATCH_COUNT_INVALID", "count must be between 1 and 1000")
	}
	if len(req.Groups) == 0 && !req.hasAmount() {
		return nil, nil, infraerrors.BadRequest("WELFARE_BATCH_EMPTY", "至少勾选一项权益（订阅分组或余额区间）")
	}
	if len(req.Groups) > welfareMaxGroupGrants {
		return nil, nil, infraerrors.BadRequest("WELFARE_BATCH_TOO_MANY_GROUPS", "too many groups in one batch")
	}
	if req.hasAmount() {
		if req.AmountMin < 0 || req.AmountMax < req.AmountMin {
			return nil, nil, infraerrors.BadRequest("WELFARE_BATCH_AMOUNT_RANGE_INVALID", "金额区间必须满足 0 <= min <= max")
		}
	} else if req.AmountMin > 0 {
		// max 为 0 视为不送余额，但 min>0 说明调用方本意送余额，静默丢弃会发错卡。
		return nil, nil, infraerrors.BadRequest("WELFARE_BATCH_AMOUNT_RANGE_INVALID", "amount_min 与 amount_max 须同时填写")
	}
	seen := make(map[int64]struct{}, len(req.Groups))
	for i := range req.Groups {
		g := &req.Groups[i]
		if g.GroupID <= 0 {
			return nil, nil, infraerrors.BadRequest("WELFARE_BATCH_GROUP_INVALID", "group_id must be positive")
		}
		if g.ValidityDays <= 0 || g.ValidityDays > 3650 {
			return nil, nil, infraerrors.BadRequest("WELFARE_BATCH_VALIDITY_INVALID", "validity_days must be between 1 and 3650")
		}
		if _, dup := seen[g.GroupID]; dup {
			return nil, nil, infraerrors.BadRequest("WELFARE_BATCH_GROUP_DUPLICATED", "duplicate group in batch")
		}
		seen[g.GroupID] = struct{}{}
	}

	// 福利卡订阅权益只对订阅类型分组有意义；标准分组在此拦截，避免发出兑换时必失败的卡。
	if len(req.Groups) > 0 {
		if err := s.validateWelfareGroups(ctx, req.Groups); err != nil {
			return nil, nil, err
		}
	}

	expiresAt := time.Now().Add(WelfareCodeValidity)
	codes := make([]RedeemCode, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		code, err := s.GenerateRandomCode()
		if err != nil {
			return nil, nil, fmt.Errorf("generate code: %w", err)
		}
		value := 0.0
		if req.hasAmount() {
			value, err = randomAmountInRange(req.AmountMin, req.AmountMax)
			if err != nil {
				return nil, nil, fmt.Errorf("sample welfare amount: %w", err)
			}
		}
		codes = append(codes, RedeemCode{
			Code:      code,
			Type:      RedeemTypeWelfare,
			Value:     value,
			Status:    StatusUnused,
			ExpiresAt: &expiresAt,
		})
	}

	batch := &RedeemBatch{
		Name: req.Name,
		Config: map[string]any{
			"groups":     req.Groups,
			"amount_min": req.AmountMin,
			"amount_max": req.AmountMax,
			"count":      req.Count,
		},
		CodeCount: req.Count,
		CreatedBy: req.CreatedBy,
	}
	if err := s.welfareRepo.CreateWelfareBatch(ctx, batch, codes, req.Groups); err != nil {
		return nil, nil, fmt.Errorf("create welfare batch: %w", err)
	}
	return batch, codes, nil
}

// validateWelfareGroups 校验勾选的分组全部存在且为订阅类型。
// entClient 为空（单测构造）时跳过，由兑换时的 AssignOrExtendSubscription 兜底。
func (s *RedeemService) validateWelfareGroups(ctx context.Context, grants []WelfareGroupGrant) error {
	if s.entClient == nil {
		return nil
	}
	ids := make([]int64, 0, len(grants))
	for i := range grants {
		ids = append(ids, grants[i].GroupID)
	}
	count, err := s.entClient.Group.Query().
		Where(
			entgroup.IDIn(ids...),
			entgroup.SubscriptionTypeEQ(SubscriptionTypeSubscription),
			entgroup.DeletedAtIsNil(),
		).
		Count(ctx)
	if err != nil {
		return fmt.Errorf("validate welfare groups: %w", err)
	}
	if count != len(ids) {
		return infraerrors.BadRequest("WELFARE_BATCH_GROUP_NOT_SUBSCRIPTION", "勾选的分组不存在或不是订阅类型分组")
	}
	return nil
}

// randomAmountInRange 在 [min, max] 区间均匀随机，保留两位小数（crypto/rand 派生）。
func randomAmountInRange(min, max float64) (float64, error) {
	minCents := int64(min*100 + 0.5)
	maxCents := int64(max*100 + 0.5)
	if maxCents <= minCents {
		return float64(minCents) / 100, nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(maxCents-minCents+1))
	if err != nil {
		return 0, err
	}
	return float64(minCents+n.Int64()) / 100, nil
}

// redeemWelfare 在兑换事务内发放福利卡权益：逐分组订阅（同组顺延）+ 福利余额入账。
// 调用前兑换码已通过乐观锁标记为已使用；任何错误都会回滚整个兑换。
func (s *RedeemService) redeemWelfare(ctx context.Context, userID int64, redeemCode *RedeemCode) error {
	if s.welfareRepo == nil {
		return errors.New("welfare repository is not configured")
	}
	if redeemCode.BatchID == nil {
		return infraerrors.BadRequest("REDEEM_CODE_INVALID", "invalid welfare redeem code: missing batch_id")
	}

	grants, err := s.welfareRepo.ListCodeGroups(ctx, redeemCode.ID)
	if err != nil {
		return fmt.Errorf("list welfare group grants: %w", err)
	}
	for i := range grants {
		_, _, err := s.subscriptionService.AssignOrExtendSubscription(ctx, &AssignSubscriptionInput{
			UserID:       userID,
			GroupID:      grants[i].GroupID,
			ValidityDays: grants[i].ValidityDays,
			AssignedBy:   0, // 系统分配
			Notes:        fmt.Sprintf("通过福利兑换卡 %s 兑换", redeemCode.Code),
		})
		if err != nil {
			return fmt.Errorf("assign or extend subscription (group %d): %w", grants[i].GroupID, err)
		}
	}

	if redeemCode.Value > 0 {
		if err := s.welfareRepo.CreateWelfareBalance(ctx, &WelfareBalance{
			UserID:          userID,
			RedeemCodeID:    redeemCode.ID,
			BatchID:         *redeemCode.BatchID,
			AmountInitial:   redeemCode.Value,
			AmountRemaining: redeemCode.Value,
			Status:          WelfareStatusActive,
			ExpiresAt:       time.Now().Add(WelfareBalanceValidity),
		}); err != nil {
			return fmt.Errorf("create welfare balance: %w", err)
		}
	}
	return nil
}

// ListWelfareBatches 批次列表（含已兑/剩余/已清零统计）。
func (s *RedeemService) ListWelfareBatches(ctx context.Context, params pagination.PaginationParams) ([]RedeemBatch, *pagination.PaginationResult, error) {
	if s.welfareRepo == nil {
		return nil, nil, errors.New("welfare repository is not configured")
	}
	return s.welfareRepo.ListBatches(ctx, params)
}

// ListWelfareBatchCodes 批次卡密列表（复制未兑换卡密 / 导出用）。
func (s *RedeemService) ListWelfareBatchCodes(ctx context.Context, batchID int64, status string) ([]RedeemCode, error) {
	if s.welfareRepo == nil {
		return nil, errors.New("welfare repository is not configured")
	}
	if batchID <= 0 {
		return nil, infraerrors.BadRequest("WELFARE_BATCH_INVALID", "batch_id must be positive")
	}
	switch status {
	case "", StatusUnused, StatusUsed, StatusDisabled, StatusExpired:
	default:
		return nil, infraerrors.BadRequest("WELFARE_BATCH_STATUS_INVALID", "invalid status filter")
	}
	return s.welfareRepo.ListCodesByBatch(ctx, batchID, status)
}

// GetWelfareSummary 用户福利余额汇总（余额页展示用）。
func (s *RedeemService) GetWelfareSummary(ctx context.Context, userID int64) (*WelfareSummary, error) {
	if s.welfareRepo == nil {
		return &WelfareSummary{}, nil
	}
	total, err := s.welfareRepo.SumActiveRemainingByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("sum welfare balance: %w", err)
	}
	summary := &WelfareSummary{TotalRemaining: total}
	if total > 0 {
		if nearest, err := s.welfareRepo.NearestActiveExpiry(ctx, userID); err == nil {
			summary.NearestExpiresAt = nearest
		}
	}
	return summary, nil
}
