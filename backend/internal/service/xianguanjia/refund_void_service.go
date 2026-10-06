package xianguanjia

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
)

// 退款作废卡密匹配规则（派发单 D2 / 外审 F1，资金安全核心）：
//
// 退款推送（refund_status=5 / order_status=23）到达后，必须先调闲管家
// /api/open/order/kam/list 查询该订单**实际发出的卡密**（card_no），再在
// sub2api 池（redeem_codes.code）内按 card_no **精准匹配**作废。
// 禁止「匹配池内任意未售卡」——那会把无关买家/无关订单的卡作废。
//
// 查无此卡（kam/list 返回空、或池内无匹配）不是失败：重试无意义，响应
// result=success 停止重试，但必须留痕（结构化日志 warn）供人工追查。

// VoidOutcome 描述一次退款作废的处理结果，handler 据此决定响应体。
type VoidOutcome int

const (
	// VoidDone 至少一张匹配卡被作废/追回（或此前已作废）。
	VoidDone VoidOutcome = iota
	// VoidNoCard 退款成功但查无此卡：kam/list 为空或池内无匹配。留痕后按成功消费。
	VoidNoCard
	// VoidNoClaim 订单在本系统无领取记录（非池卡订单，如闲管家直发），无需处置。
	VoidNoClaim
)

// RefundCardVoider 退款作废执行器：由 handler 在退款推送路径调用。
type RefundCardVoider interface {
	// VoidRefundedCards 按订单实际所发卡密作废池内卡。
	// 返回 (outcome, error)：error != nil 表示**临时性失败**（网络/配置/DB），
	// 调用方必须返回 result=fail 让闲管家重试；error == nil 时按 outcome 分流。
	VoidRefundedCards(ctx context.Context, orderNo string) (VoidOutcome, error)
}

// ClaimClawbackOrderNoResolver 用订单号解析领取记录锚点（既有 claim 追回链路）。
// 复用 XianyuDeliveryService.GetClaimAccountID + ProcessRefundEvent 的
// refund_handled_at 幂等锚点处理 used 码（已被核销的卡要追回权益，不能只翻状态）。
type ClaimClawbackOrderNoResolver interface {
	GetClaimAccountID(ctx context.Context, orderNo string) (string, error)
	ProcessRefundEvent(ctx context.Context, orderNo, accountID, status string) (string, error)
}

// CodeCardVoidRepository 池卡作废仓库（按 card_no 精准匹配 redeem_codes.code）。
type CodeCardVoidRepository interface {
	// VoidCodesByCardNos 把与 cardNos 精确匹配的 delivered/unused 卡置为 expired，
	// 返回每个 card_no 的处理结果（命中数与状态）；used/expired 等状态不在此处理。
	VoidCodesByCardNos(ctx context.Context, cardNos []string) (map[string]string, error)
}

// RefundCardVoidService 退款作废服务编排：
//  1. kam/list 查询订单实际所发卡密（ClientFactory 动态出站）；
//  2. 池内按 card_no 精准作废 delivered/unused；
//  3. 命中的卡若存在领取记录且为 used（已被买家核销），走既有 claim 追回；
//  4. 查无卡 → VoidNoCard（handler 留痕后返回 success）。
type RefundCardVoidService struct {
	factory *ClientFactory
	codes   CodeCardVoidRepository
	claims  ClaimClawbackOrderNoResolver
}

// NewRefundCardVoidService 构造退款作废服务。
// claims 可为 nil（则 used 码追回路径降级为仅告警日志——人工处置）。
func NewRefundCardVoidService(factory *ClientFactory, codes CodeCardVoidRepository, claims ClaimClawbackOrderNoResolver) *RefundCardVoidService {
	return &RefundCardVoidService{factory: factory, codes: codes, claims: claims}
}

// VoidRefundedCards 实现 RefundCardVoider。
func (s *RefundCardVoidService) VoidRefundedCards(ctx context.Context, orderNo string) (VoidOutcome, error) {
	orderNo = strings.TrimSpace(orderNo)
	if s == nil || s.factory == nil || s.codes == nil {
		return VoidDone, fmt.Errorf("xianguanjia refund card voider unavailable")
	}
	// 1) 查询闲管家侧该订单实际发出的卡密。失败属临时性失败 → 返回 error 让闲管家重试。
	client, err := s.factory.NewClient(ctx)
	if err != nil {
		return VoidDone, fmt.Errorf("xianguanjia build client for kam list: %w", err)
	}
	cards, err := client.ListOrderCards(ctx, orderNo)
	if err != nil {
		return VoidDone, fmt.Errorf("xianguanjia kam list for order %s: %w", orderNo, err)
	}
	if len(cards) == 0 {
		// 查无此卡（官方 kam/list 对未发货订单返回空 list）：不是失败，重试无意义。
		slog.Warn("xianguanjia refund void: no cards delivered for refunded order",
			"order_no", orderNo)
		return VoidNoCard, nil
	}
	cardNos := make([]string, 0, len(cards))
	for _, c := range cards {
		if no := strings.TrimSpace(c.CardNo); no != "" {
			cardNos = append(cardNos, no)
		}
	}
	if len(cardNos) == 0 {
		slog.Warn("xianguanjia refund void: kam list returned cards without card_no",
			"order_no", orderNo, "card_count", len(cards))
		return VoidNoCard, nil
	}

	// 2) 池内按 card_no 精准作废 delivered/unused。
	results, err := s.codes.VoidCodesByCardNos(ctx, cardNos)
	if err != nil {
		return VoidDone, fmt.Errorf("void pool codes by card_no for order %s: %w", orderNo, err)
	}
	voided := 0
	unmatched := 0
	for _, no := range cardNos {
		switch results[no] {
		case "voided":
			voided++
		case "missing":
			unmatched++
		}
		// "already_expired" / "in_use" 等其它状态见 VoidCodesByCardNos。
	}
	if voided == 0 && unmatched == len(cardNos) {
		// 池内一张都没匹配上：可能是闲管家直发（非我方池卡），或卡号映射漂移。
		// 不失败、留痕，转人工核对。此时代码状态无法确认，不走 used 追回
		//（追回必须以领取记录为锚点，见下）。
		slog.Warn("xianguanjia refund void: no pool code matched delivered cards",
			"order_no", orderNo, "card_nos_count", len(cardNos))
		return VoidNoCard, nil
	}

	// 3) 有 used 码（已被买家核销）时走既有 claim 追回链路（refund_handled_at 幂等锚点，
	//    防止余额/订阅被双扣）。追回失败同样属临时性失败 → 返回 error 让闲管家重试。
	if needs := countStatus(results, "in_use"); needs > 0 && s.claims != nil {
		accountID, err := s.claims.GetClaimAccountID(ctx, orderNo)
		if err != nil {
			return VoidDone, fmt.Errorf("resolve claim account for refund clawback order %s: %w", orderNo, err)
		}
		if _, err := s.claims.ProcessRefundEvent(ctx, orderNo, accountID, "refunded"); err != nil {
			return VoidDone, fmt.Errorf("claim clawback for order %s: %w", orderNo, err)
		}
	} else if needs > 0 {
		slog.Warn("xianguanjia refund void: used codes need clawback but claim resolver unavailable",
			"order_no", orderNo, "used_count", needs)
	}

	slog.Info("xianguanjia refund void completed",
		"order_no", orderNo, "cards", len(cardNos), "voided", voided)
	return VoidDone, nil
}

func countStatus(results map[string]string, status string) int {
	n := 0
	for _, v := range results {
		if v == status {
			n++
		}
	}
	return n
}

// ---- DB 实现：按 card_no 精准作废 ----

type codeCardVoidRepositoryDB struct {
	db *sql.DB
}

// NewCodeCardVoidRepository 返回基于 PostgreSQL 的池卡作废仓库。
func NewCodeCardVoidRepository(db *sql.DB) *codeCardVoidRepositoryDB {
	return &codeCardVoidRepositoryDB{db: db}
}

// VoidCodesByCardNos 按 card_no 精准匹配 redeem_codes.code 作废。
// 每张卡独立判定，状态映射：
//   - "voided"          delivered/unused → expired（原子条件更新命中）
//   - "already_expired" 已是 expired
//   - "in_use"          used（已被核销，需走 claim 追回，本方法不动）
//   - "disabled"        disabled（管理员禁用，不处置）
//   - "missing"         池内无此卡
//
// 单条 UPDATE 原子完成状态判定+作废（code 唯一索引），无跨行事务需求。
func (r *codeCardVoidRepositoryDB) VoidCodesByCardNos(ctx context.Context, cardNos []string) (map[string]string, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("xianguanjia code card void repository unavailable")
	}
	out := make(map[string]string, len(cardNos))
	for _, no := range cardNos {
		res, err := r.db.ExecContext(ctx, `
			UPDATE redeem_codes SET status = 'expired'
			WHERE code = $1 AND status IN ('delivered', 'unused')`, no)
		if err != nil {
			return nil, fmt.Errorf("void redeem code by card_no: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("void redeem code rows affected: %w", err)
		}
		if affected == 1 {
			out[no] = "voided"
			continue
		}
		// 未命中条件更新：回读状态分类。
		var status string
		err = r.db.QueryRowContext(ctx,
			`SELECT status FROM redeem_codes WHERE code = $1`, no).Scan(&status)
		if err == sql.ErrNoRows {
			out[no] = "missing"
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("readback redeem code status: %w", err)
		}
		switch status {
		case "expired":
			out[no] = "already_expired"
		case "used":
			out[no] = "in_use"
		default:
			out[no] = "disabled"
		}
	}
	return out, nil
}
