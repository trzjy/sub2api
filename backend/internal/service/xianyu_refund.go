package service

import (
	"context"
	"errors"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	// ErrXianyuRefundOrderRequired 校验 Worker 上报必填字段。
	ErrXianyuRefundOrderRequired = infraerrors.BadRequest("XIANYU_REFUND_ORDER_NO_REQUIRED", "order_no is required")
	// ErrXianyuRefundClaimNotFound 订单无池卡领取记录（非池卡订单，无需处置）。
	ErrXianyuRefundClaimNotFound = infraerrors.NotFound("XIANYU_REFUND_CLAIM_NOT_FOUND", "no xianyu claim for this order")
	// ErrXianyuRefundAccountMismatch Worker 上报的账号与领取记录不一致（共享内网 token 下的绑定校验）。
	ErrXianyuRefundAccountMismatch = infraerrors.Conflict("XIANYU_REFUND_ACCOUNT_MISMATCH", "reported account does not match the claim")
	// ErrXianyuRefundClawbackUnavailable 追回能力未装配（RedeemService 缺失）。
	ErrXianyuRefundClawbackUnavailable = infraerrors.InternalServer("XIANYU_REFUND_CLAWBACK_UNAVAILABLE", "redeem clawback is not configured")
	// ErrXianyuRefundCodeStatusUnknown 兑换码处于未知状态，防御性报错等待人工。
	ErrXianyuRefundCodeStatusUnknown = infraerrors.Conflict("XIANYU_REFUND_CODE_STATUS_UNKNOWN", "unknown redeem code status for refund handling")
)

// XianyuRefundOutcome 是原子处置结果。
type XianyuRefundOutcome struct {
	// Action 处置动作（Worker 侧不解析语义，2xx 即视为已上报）。
	Action string
	// Detail 审计文案（写入 refund_detail）。
	Detail string
	// Code 仅 used 追回分支非 nil：提交成功后用于失效用户相关缓存。
	Code *RedeemCode
}

// XianyuRedeemClawback 追回已核销兑换码权益的能力（RedeemService 实现）。
type XianyuRedeemClawback interface {
	// ClawbackXianyuRedeemCodeTx 在调用方事务内（ctx 携带 ent 事务）扣回 used 码已发放的
	// 权益，返回动作描述（写入 refund_detail 审计）。扣减失败时调用方整体回滚，可重试。
	ClawbackXianyuRedeemCodeTx(ctx context.Context, code *RedeemCode) (string, error)
	// InvalidateAfterClawback 事务提交成功后的缓存失效（best-effort）。
	InvalidateAfterClawback(ctx context.Context, code *RedeemCode)
}

// XianyuRefundEventRepository 闲鱼退款处置（单事务原子）。
type XianyuRefundEventRepository interface {
	// ProcessRefundAtomically 在单个数据库事务内完成：CAS 认领（refund_handled_at IS NULL
	// + account_id 绑定）→ 码快照（FOR UPDATE，与买家兑换互斥）→ delivered/unused 原子作废
	// 或 used 码回调追回 → 写入处置终态。任一步失败整体回滚。
	// 返回 ErrXianyuRefundClaimNotFound 表示无领取记录（非池卡订单）。
	ProcessRefundAtomically(ctx context.Context, orderNo, accountID string, clawback XianyuRedeemClawback) (*XianyuRefundOutcome, error)
}

// 退款处置 action 取值（写入 refund_action；并发重复事件回放该值）。
const (
	XianyuRefundActionVoided      = "voided"         // delivered/unused → expired
	XianyuRefundActionClawedBack  = "clawed_back"    // used → 按类型追回权益
	XianyuRefundActionNoClaim     = "no_claim"       // 无领取记录（非池卡订单）
	XianyuRefundActionAlreadyVoid = "already_voided" // 码已是 expired 终态
	XianyuRefundActionIgnored     = "ignored"        // disabled 等无需处置的状态
)

// ProcessRefundEvent 处理 Worker 上报的闲鱼退款成功事件（幂等）。
// 仅处理 status=refunded；delivered/unused 码作废，used 码按类型追回权益。
// 返回处置 action 供响应体回显。
func (s *XianyuDeliveryService) ProcessRefundEvent(ctx context.Context, orderNo, accountID, status string) (string, error) {
	if s.refunds == nil {
		return "", ErrXianyuDeliveryNotConfigured
	}
	if s.clawback == nil {
		return "", ErrXianyuRefundClawbackUnavailable
	}
	orderNo = strings.TrimSpace(orderNo)
	accountID = strings.TrimSpace(accountID)
	if orderNo == "" {
		return "", ErrXianyuRefundOrderRequired
	}
	if len(orderNo) > 64 {
		return "", ErrXianyuOrderTooLong
	}
	if accountID == "" {
		return "", ErrXianyuAccountRequired
	}
	if len(accountID) > 80 {
		return "", ErrXianyuAccountTooLong
	}
	// Worker 当前只上报退款成功；防御非 refunded 状态被误标为已处置。
	if status != "refunded" {
		return "", infraerrors.BadRequest("XIANYU_REFUND_STATUS_UNSUPPORTED", "only refunded refund events are processed")
	}

	outcome, err := s.refunds.ProcessRefundAtomically(ctx, orderNo, accountID, s.clawback)
	if err != nil {
		if errors.Is(err, ErrXianyuRefundClaimNotFound) {
			return XianyuRefundActionNoClaim, nil
		}
		return "", err
	}
	if outcome.Code != nil && outcome.Code.UsedBy != nil {
		// 事务已提交，失效核销用户的余额/订阅/鉴权缓存（best-effort）。
		s.clawback.InvalidateAfterClawback(ctx, outcome.Code)
	}
	return outcome.Action, nil
}
