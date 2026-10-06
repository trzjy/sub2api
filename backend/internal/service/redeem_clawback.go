package service

import (
	"context"
	"errors"
	"fmt"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// ClawbackXianyuRedeemCodeTx 退款追回：在调用方事务内（ctx 携带 ent 事务，见
// dbent.NewTxContext）扣回已核销（used）兑换码发放的权益。
// 订阅扣发放天数（剩余不足则取消订阅）、余额/并发扣面值（下限 0，与负数兑换码语义一致）。
// 失败时调用方整体回滚，可重试；订阅已不存在视为无可追回，返回成功与说明文案。
func (s *RedeemService) ClawbackXianyuRedeemCodeTx(ctx context.Context, code *RedeemCode) (string, error) {
	if s == nil || code == nil {
		return "", errors.New("redeem clawback unavailable")
	}
	if code.UsedBy == nil {
		return "", infraerrors.InternalServer("REDEEM_CLAWBACK_NO_USER", "redeem code has no redeeming user, cannot claw back")
	}
	userID := *code.UsedBy

	switch code.Type {
	case RedeemTypeBalance, RedeemTypeConcurrency, RedeemTypeSubscription:
	default:
		return "", unsupportedRedeemTypeError(code.Type)
	}

	var detail string
	var err error
	switch code.Type {
	case RedeemTypeBalance:
		detail, err = s.clawbackBalance(ctx, userID, code)
	case RedeemTypeConcurrency:
		detail, err = s.clawbackConcurrency(ctx, userID, code)
	case RedeemTypeSubscription:
		detail, err = s.clawbackSubscription(ctx, userID, code)
	}
	if err != nil {
		return "", err
	}
	logger.LegacyPrintf("service.redeem", "[Clawback] 退款追回兑换码 %s 用户 %d: %s", code.Code, userID, detail)
	return detail, nil
}

// InvalidateAfterClawback 事务提交成功后的缓存失效（best-effort，与兑换路径一致）。
func (s *RedeemService) InvalidateAfterClawback(ctx context.Context, code *RedeemCode) {
	if s == nil || code == nil || code.UsedBy == nil {
		return
	}
	s.invalidateRedeemCaches(ctx, *code.UsedBy, code)
}

func (s *RedeemService) clawbackBalance(ctx context.Context, userID int64, code *RedeemCode) (string, error) {
	if code.Value <= 0 {
		// 负数面值码在核销时是扣减而非发放，无可追回。
		return "非正数面值，无需追回", nil
	}
	if s.redeemUserRepo == nil {
		return "", errors.New("user repository does not support atomic redeem balance adjustments")
	}
	if err := s.redeemUserRepo.ApplyRedeemBalanceAdjustment(ctx, userID, -code.Value); err != nil {
		return "", fmt.Errorf("reduce user balance: %w", err)
	}
	return fmt.Sprintf("追回余额 %.2f", code.Value), nil
}

func (s *RedeemService) clawbackConcurrency(ctx context.Context, userID int64, code *RedeemCode) (string, error) {
	delta := int(code.Value)
	if delta <= 0 {
		return "非正数面值，无需追回", nil
	}
	if s.redeemUserRepo == nil {
		return "", errors.New("user repository does not support atomic redeem concurrency adjustments")
	}
	if err := s.redeemUserRepo.ApplyRedeemConcurrencyAdjustment(ctx, userID, -delta); err != nil {
		return "", fmt.Errorf("reduce user concurrency: %w", err)
	}
	return fmt.Sprintf("追回并发 %d", delta), nil
}

func (s *RedeemService) clawbackSubscription(ctx context.Context, userID int64, code *RedeemCode) (string, error) {
	if code.GroupID == nil {
		return "", infraerrors.BadRequest("REDEEM_CLAWBACK_INVALID_SUBSCRIPTION", "invalid subscription redeem code: missing group_id")
	}
	if code.ValidityDays < 0 {
		// 负数天数码在核销时是缩短订阅，无可追回。
		return "非正数天数，无需追回", nil
	}
	days := code.ValidityDays
	if days == 0 {
		// 与兑换路径一致：0 天按 30 天发放，追回同样按 30 天扣减。
		days = 30
	}
	if err := s.reduceOrCancelSubscription(ctx, userID, *code.GroupID, days, code.Code); err != nil {
		if errors.Is(err, ErrSubscriptionNotFound) {
			// 订阅已不存在（过期/删除），无可扣减，视为追回完成。
			return "订阅已不存在，无需追回", nil
		}
		return "", fmt.Errorf("reduce subscription: %w", err)
	}
	return fmt.Sprintf("追回订阅 %d 天", days), nil
}
