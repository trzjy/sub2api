package repository

import (
	"context"
	"fmt"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/redeembatch"
	"github.com/Wei-Shaw/sub2api/ent/redeemcode"
	"github.com/Wei-Shaw/sub2api/ent/redeemcodegroup"
	"github.com/Wei-Shaw/sub2api/ent/welfarebalance"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type welfareRepository struct {
	client *dbent.Client
}

// NewWelfareRepository 福利兑换卡仓储。
func NewWelfareRepository(client *dbent.Client) service.WelfareRepository {
	return &welfareRepository{client: client}
}

// CreateWelfareBatch 单事务写入批次行 + N 张码 + N×G 行分组勾选；失败整批回滚。
func (r *welfareRepository) CreateWelfareBatch(ctx context.Context, batch *service.RedeemBatch, codes []service.RedeemCode, grants []service.WelfareGroupGrant) error {
	if r == nil || r.client == nil {
		return fmt.Errorf("welfare repository client is nil")
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	createdBatch, err := tx.RedeemBatch.Create().
		SetName(batch.Name).
		SetConfig(batch.Config).
		SetCodeCount(batch.CodeCount).
		SetCreatedBy(batch.CreatedBy).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create batch: %w", err)
	}
	batch.ID = createdBatch.ID
	batch.CreatedAt = createdBatch.CreatedAt

	codeIDs := make([]int64, 0, len(codes))
	for i := range codes {
		c := &codes[i]
		created, err := tx.RedeemCode.Create().
			SetCode(c.Code).
			SetType(c.Type).
			SetValue(c.Value).
			SetStatus(c.Status).
			SetNillableExpiresAt(c.ExpiresAt).
			SetBatchID(createdBatch.ID).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("create code: %w", err)
		}
		codeIDs = append(codeIDs, created.ID)
	}

	for _, codeID := range codeIDs {
		builders := make([]*dbent.RedeemCodeGroupCreate, 0, len(grants))
		for i := range grants {
			builders = append(builders, tx.RedeemCodeGroup.Create().
				SetRedeemCodeID(codeID).
				SetGroupID(grants[i].GroupID).
				SetValidityDays(grants[i].ValidityDays))
		}
		if len(builders) > 0 {
			if err := tx.RedeemCodeGroup.CreateBulk(builders...).Exec(ctx); err != nil {
				return fmt.Errorf("attach code groups: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (r *welfareRepository) ListBatches(ctx context.Context, params pagination.PaginationParams) ([]service.RedeemBatch, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	query := client.RedeemBatch.Query().Order(dbent.Desc(redeembatch.FieldCreatedAt), dbent.Desc(redeembatch.FieldID))

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	models, err := query.
		Offset(params.Offset()).
		Limit(params.Limit()).
		All(ctx)
	if err != nil {
		return nil, nil, err
	}

	out := make([]service.RedeemBatch, 0, len(models))
	for _, m := range models {
		b := service.RedeemBatch{
			ID:        m.ID,
			Name:      m.Name,
			Config:    m.Config,
			CodeCount: m.CodeCount,
			CreatedBy: m.CreatedBy,
			CreatedAt: m.CreatedAt,
		}
		used, err := client.RedeemCode.Query().
			Where(redeemcode.BatchIDEQ(m.ID), redeemcode.StatusEQ(service.StatusUsed)).
			Count(ctx)
		if err != nil {
			return nil, nil, err
		}
		b.UsedCount = int64(used)
		b.RemainingCount = int64(m.CodeCount - used)

		var cleared []struct {
			Sum float64 `json:"sum"`
		}
		if err := client.WelfareBalance.Query().
			Where(
				welfarebalance.BatchIDEQ(m.ID),
				welfarebalance.StatusEQ(service.WelfareStatusExpired),
			).
			Aggregate(dbent.As(dbent.Sum(welfarebalance.FieldAmountInitial), "sum")).
			Scan(ctx, &cleared); err != nil {
			return nil, nil, err
		}
		if len(cleared) > 0 {
			b.ClearedAmount = cleared[0].Sum
		}
		out = append(out, b)
	}

	return out, paginationResultFromTotal(int64(total), params), nil
}

func (r *welfareRepository) AttachCodeGroups(ctx context.Context, redeemCodeID int64, grants []service.WelfareGroupGrant) error {
	if len(grants) == 0 {
		return nil
	}
	client := clientFromContext(ctx, r.client)
	builders := make([]*dbent.RedeemCodeGroupCreate, 0, len(grants))
	for i := range grants {
		builders = append(builders, client.RedeemCodeGroup.Create().
			SetRedeemCodeID(redeemCodeID).
			SetGroupID(grants[i].GroupID).
			SetValidityDays(grants[i].ValidityDays))
	}
	return client.RedeemCodeGroup.CreateBulk(builders...).Exec(ctx)
}

func (r *welfareRepository) ListCodeGroups(ctx context.Context, redeemCodeID int64) ([]service.WelfareGroupGrant, error) {
	client := clientFromContext(ctx, r.client)
	models, err := client.RedeemCodeGroup.Query().
		Where(redeemcodegroup.RedeemCodeIDEQ(redeemCodeID)).
		WithGroup().
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.WelfareGroupGrant, 0, len(models))
	for _, m := range models {
		g := service.WelfareGroupGrant{
			GroupID:      m.GroupID,
			ValidityDays: m.ValidityDays,
		}
		if m.Edges.Group != nil {
			g.GroupName = m.Edges.Group.Name
		}
		out = append(out, g)
	}
	return out, nil
}

func (r *welfareRepository) CreateWelfareBalance(ctx context.Context, wb *service.WelfareBalance) error {
	client := clientFromContext(ctx, r.client)
	created, err := client.WelfareBalance.Create().
		SetUserID(wb.UserID).
		SetRedeemCodeID(wb.RedeemCodeID).
		SetBatchID(wb.BatchID).
		SetAmountInitial(wb.AmountInitial).
		SetAmountRemaining(wb.AmountRemaining).
		SetStatus(wb.Status).
		SetExpiresAt(wb.ExpiresAt).
		Save(ctx)
	if err != nil {
		return err
	}
	wb.ID = created.ID
	wb.CreatedAt = created.CreatedAt
	return nil
}

func (r *welfareRepository) SumActiveRemainingByUser(ctx context.Context, userID int64) (float64, error) {
	client := clientFromContext(ctx, r.client)
	var result []struct {
		Sum float64 `json:"sum"`
	}
	err := client.WelfareBalance.Query().
		Where(
			welfarebalance.UserIDEQ(userID),
			welfarebalance.StatusEQ(service.WelfareStatusActive),
			welfarebalance.AmountRemainingGT(0),
			welfarebalance.ExpiresAtGT(time.Now()),
		).
		Aggregate(dbent.As(dbent.Sum(welfarebalance.FieldAmountRemaining), "sum")).
		Scan(ctx, &result)
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, nil
	}
	return result[0].Sum, nil
}

func (r *welfareRepository) NearestActiveExpiry(ctx context.Context, userID int64) (*time.Time, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.WelfareBalance.Query().
		Where(
			welfarebalance.UserIDEQ(userID),
			welfarebalance.StatusEQ(service.WelfareStatusActive),
			welfarebalance.AmountRemainingGT(0),
			welfarebalance.ExpiresAtGT(time.Now()),
		).
		Order(dbent.Asc(welfarebalance.FieldExpiresAt)).
		First(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &m.ExpiresAt, nil
}

func (r *welfareRepository) ListCodesByBatch(ctx context.Context, batchID int64, status string) ([]service.RedeemCode, error) {
	client := clientFromContext(ctx, r.client)
	query := client.RedeemCode.Query().
		Where(redeemcode.BatchIDEQ(batchID)).
		WithUser().
		Order(dbent.Asc(redeemcode.FieldID)).
		Limit(2000)
	if status != "" {
		query = query.Where(redeemcode.StatusEQ(status))
	}
	models, err := query.All(ctx)
	if err != nil {
		return nil, err
	}
	return redeemCodeEntitiesToService(models), nil
}

// ExpireDueBalances 清零到期福利余额。只动 welfare_balances，绝不触碰 users.balance。
func (r *welfareRepository) ExpireDueBalances(ctx context.Context) (int64, float64, error) {
	client := clientFromContext(ctx, r.client)
	var cleared []struct {
		Sum float64 `json:"sum"`
	}
	if err := client.WelfareBalance.Query().
		Where(
			welfarebalance.StatusEQ(service.WelfareStatusActive),
			welfarebalance.AmountRemainingGT(0),
			welfarebalance.ExpiresAtLTE(time.Now()),
		).
		Aggregate(dbent.As(dbent.Sum(welfarebalance.FieldAmountRemaining), "sum")).
		Scan(ctx, &cleared); err != nil {
		return 0, 0, err
	}
	var amount float64
	if len(cleared) > 0 {
		amount = cleared[0].Sum
	}

	affected, err := client.WelfareBalance.Update().
		Where(
			welfarebalance.StatusEQ(service.WelfareStatusActive),
			welfarebalance.AmountRemainingGT(0),
			welfarebalance.ExpiresAtLTE(time.Now()),
		).
		SetStatus(service.WelfareStatusExpired).
		SetAmountRemaining(0).
		SetUpdatedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return 0, 0, err
	}
	return int64(affected), amount, nil
}
