package xianguanjia

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// 卡密推仓（D4b）：把本侧卡密同步进闲管家仓库（storage/create），推仓成功后
// 在本侧把对应卡密标记为「闲鱼渠道/已推仓」，防止同一张卡在站内与闲鱼两侧
// 被重复售卖（防双卖）。
//
// 渠道标记实现方式（复用既有状态机，不改枚举语义）：
//   - 卡密池状态机（domain/constants.go:6-11）：unused/delivered/used/expired/disabled。
//   - delivered 的既有注释为「已发货待兑换（闲鱼库存池领取后、买家兑换前）」，
//     其不变量 = 出 unused 可发池 + 买家仍可兑换（redeem_code.go CanUse 放行
//     unused/delivered）+ 退款可作废（refund_void / xianyu_refund 覆盖
//     delivered/unused）。推仓成功后的卡处于完全相同的可观察状态
//     （闲鱼渠道占用、站内不可再发、买家拿到卡后仍可兑换），因此复用
//     delivered 作为「闲鱼渠道/已推仓」标记，未新增/修改任何枚举值。
//   - 池剩余口径 PoolStockCounts 只统计 status='unused'，delivered 自动出池；
//     池「已发货」统计取自 xianyu_order_claims（delivery_status='sent'），
//     推仓标记不会虚增发货数。
//
// 请求字段名（kind_id / cards[].card_no / cards[].card_pwd）按派发单给定契约，
// 与既有 ExternalCard（card_no/card_pwd，官方文档坐实）命名一致；body 为
// json.Marshal 压缩 JSON。响应明细字段 data.failed[].card_no 为本实现约定的
// 解析形状：官方若无失败明细字段则按整单处理（code==0 即全部成功），
// 该约定已在证据文件 d4b.md 中说明。

const pathKamiStorageCreate = "/api/open/kami/storage/create"

// CardPair 是推仓的一张卡密（卡号+卡密）。
type CardPair struct {
	CardNo  string `json:"card_no"`
	CardPwd string `json:"card_pwd"`
}

// StorageCreateRejectedError 表示官方整单拒绝（信封 code!=0）：全部失败、无部分成功。
// 与传输层错误区分：调用方据此把整批记入失败清单而非按临时故障重试。
type StorageCreateRejectedError struct {
	Code int
	Msg  string
}

func (e *StorageCreateRejectedError) Error() string {
	return fmt.Sprintf("xianguanjia storage/create rejected code %d: %s", e.Code, e.Msg)
}

// storageCreateData 是 storage/create 响应 data 的解析形状（见文件头约定说明）。
type storageCreateData struct {
	Failed []struct {
		CardNo string `json:"card_no"`
		Reason string `json:"reason"`
	} `json:"failed"`
}

// CreateStorageCards 推送卡密到闲管家仓库（POST /api/open/kami/storage/create）。
// 返回 (failed, err)：
//   - err 为 *StorageCreateRejectedError：官方整单拒绝（code!=0），整批失败；
//   - err 为其它非 nil：传输/解码层临时故障，推仓结果未知；
//   - err == nil：HTTP 与信封均成功，failed 为失败明细（card_no → reason）；
//     官方无明细字段时 failed 为空，按整单成功处理。
func (c *Client) CreateStorageCards(ctx context.Context, kindID int64, cards []CardPair) (map[string]string, error) {
	body, err := marshalBody(struct {
		KindID int64      `json:"kind_id"`
		Cards  []CardPair `json:"cards"`
	}{KindID: kindID, Cards: cards})
	if err != nil {
		return nil, fmt.Errorf("xianguanjia marshal storage/create: %w", err)
	}
	raw, err := c.do(ctx, pathKamiStorageCreate, body)
	if err != nil {
		return nil, err
	}
	var env responseEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("xianguanjia decode storage/create envelope: %w", err)
	}
	if env.Code != 0 {
		return nil, &StorageCreateRejectedError{Code: env.Code, Msg: env.Msg}
	}
	if len(env.Data) == 0 {
		return nil, nil
	}
	var data storageCreateData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		// data 形状不符：无法解析明细，按整单成功处理并留痕（不猜字段）。
		slog.Warn("xianguanjia storage/create: data not in expected shape, treat as whole-order success",
			"kind_id", kindID, "cards", len(cards), "error", err)
		return nil, nil
	}
	if len(data.Failed) == 0 {
		return nil, nil
	}
	failed := make(map[string]string, len(data.Failed))
	for _, f := range data.Failed {
		if no := strings.TrimSpace(f.CardNo); no != "" {
			failed[no] = f.Reason
		}
	}
	if len(failed) == 0 {
		// 明细存在但无法映射到 card_no：不猜映射，按整单成功处理并留痕。
		slog.Warn("xianguanjia storage/create: failure details without card_no, treat as whole-order success",
			"kind_id", kindID, "cards", len(cards), "detail_count", len(data.Failed))
		return nil, nil
	}
	return failed, nil
}

// PushResult 是一次推仓的结果报告。
type PushResult struct {
	// Succeeded 推仓成功的卡（官方判定）。
	Succeeded []CardPair
	// Failed 推仓失败的卡；重试时调用方只传本子集即可（不重复推已成功的卡）。
	Failed []CardPair
	// Message 官方 msg（整单拒绝时透传）。
	Message string
	// Mark 本侧渠道标记结果（card_no → 标记状态，见 MarkPushedToXianyu）。
	Mark map[string]string
}

// PushedCardMarker 推仓成功后的本侧渠道标记仓库（防双卖）。
type PushedCardMarker interface {
	// MarkPushedToXianyu 把 cardNos 对应的本侧卡密标记为闲鱼渠道已推仓
	// （复用既有状态机：unused → delivered，原子条件更新）。返回每张卡的标记结果：
	//   - "marked"            unused → delivered（本次标记成功）
	//   - "already_delivered" 已是 delivered（幂等，重复推仓/补标安全）
	//   - "missing"           池内无此卡
	//   - "skipped:<status>"  卡处于 used/expired/disabled 等状态，未动（留痕转人工）
	MarkPushedToXianyu(ctx context.Context, cardNos []string) (map[string]string, error)
}

// PoolSyncService 卡密推仓服务编排：调 storage/create 推仓，按官方判定拆分
// 成功/失败清单，推仓成功的卡在本侧做渠道标记（防双卖）。
type PoolSyncService struct {
	client *Client
	marker PushedCardMarker
}

// NewPoolSyncService 构造推仓服务。client/marker 均必填（fail-closed）：
// 无标记能力的推仓会留下双卖窗口，宁可拒绝服务。
func NewPoolSyncService(client *Client, marker PushedCardMarker) *PoolSyncService {
	return &PoolSyncService{client: client, marker: marker}
}

// PushCards 推送一批卡密到闲管家仓库并标记本侧渠道。
// 语义：
//   - cards 为空或 kindID 非法：拒绝（参数边界校验），不发请求；
//   - 官方整单拒绝（code!=0）：全部失败，返回 PushResult{Failed: cards}，err 为 nil
//     （这是业务结果而非临时故障；Message 透传官方 msg），不做渠道标记；
//   - 传输/解码失败：返回 err（推仓结果未知，调用方按临时故障重试全集）；
//   - 整单/部分成功：Succeeded 为成功子集，Failed 为失败子集（支持只重试失败子集）；
//     仅对成功子集做渠道标记，标记失败返回 err（卡已推到闲鱼但本侧未标记，
//     存在双卖窗口，必须显式上抛）。
func (s *PoolSyncService) PushCards(ctx context.Context, kindID int64, cards []CardPair) (PushResult, error) {
	if s == nil || s.client == nil || s.marker == nil {
		return PushResult{}, fmt.Errorf("xianguanjia pool sync service unavailable")
	}
	if kindID <= 0 {
		return PushResult{}, fmt.Errorf("xianguanjia storage/create: invalid kind_id %d", kindID)
	}
	if len(cards) == 0 {
		return PushResult{}, fmt.Errorf("xianguanjia storage/create: empty cards")
	}
	failedDetails, err := s.client.CreateStorageCards(ctx, kindID, cards)
	if err != nil {
		var rejected *StorageCreateRejectedError
		if errors.As(err, &rejected) {
			// 官方整单拒绝：全部失败，不标记，按业务结果返回。
			return PushResult{
				Failed:  append([]CardPair(nil), cards...),
				Message: rejected.Msg,
			}, nil
		}
		return PushResult{}, err
	}

	result := PushResult{Mark: map[string]string{}}
	succeededNos := make([]string, 0, len(cards))
	for _, c := range cards {
		if _, bad := failedDetails[c.CardNo]; bad {
			result.Failed = append(result.Failed, c)
			continue
		}
		result.Succeeded = append(result.Succeeded, c)
		if no := strings.TrimSpace(c.CardNo); no != "" {
			succeededNos = append(succeededNos, no)
		}
	}
	// 未知 card_no 出现在失败明细里（官方返回了本次请求之外的卡）：留痕忽略。
	for no := range failedDetails {
		found := false
		for _, c := range cards {
			if c.CardNo == no {
				found = true
				break
			}
		}
		if !found {
			slog.Warn("xianguanjia storage/create: failure detail for unknown card_no",
				"kind_id", kindID, "card_no", no)
		}
	}

	if len(succeededNos) > 0 {
		mark, err := s.marker.MarkPushedToXianyu(ctx, succeededNos)
		if err != nil {
			return result, fmt.Errorf("xianguanjia mark pushed cards (kind_id %d): %w", kindID, err)
		}
		result.Mark = mark
		abnormal := 0
		for _, st := range mark {
			if st != "marked" && st != "already_delivered" {
				abnormal++
			}
		}
		if abnormal > 0 {
			slog.Warn("xianguanjia pool sync: some pushed cards not cleanly marked",
				"kind_id", kindID, "abnormal", abnormal, "mark", mark)
		}
	}
	slog.Info("xianguanjia pool sync push completed",
		"kind_id", kindID, "pushed", len(cards),
		"succeeded", len(result.Succeeded), "failed", len(result.Failed))
	return result, nil
}

// ---- DB 实现：unused → delivered 渠道标记 ----

type pushedCardMarkerDB struct {
	db *sql.DB
}

// NewPushedCardMarker 返回基于 PostgreSQL 的推仓渠道标记仓库。
func NewPushedCardMarker(db *sql.DB) *pushedCardMarkerDB {
	return &pushedCardMarkerDB{db: db}
}

// MarkPushedToXianyu 按 card_no 精准匹配 redeem_codes.code，把 unused 卡置为
// delivered（复用既有状态机的「闲鱼渠道占用」值，见文件头说明）。
// 单条 UPDATE 原子完成状态判定+标记（code 唯一索引），无跨行事务需求。
func (r *pushedCardMarkerDB) MarkPushedToXianyu(ctx context.Context, cardNos []string) (map[string]string, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("xianguanjia pushed card marker unavailable")
	}
	out := make(map[string]string, len(cardNos))
	for _, no := range cardNos {
		res, err := r.db.ExecContext(ctx, `
			UPDATE redeem_codes SET status = 'delivered'
			WHERE code = $1 AND status = 'unused'`, no)
		if err != nil {
			return nil, fmt.Errorf("mark redeem code pushed by card_no: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("mark redeem code rows affected: %w", err)
		}
		if affected == 1 {
			out[no] = "marked"
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
		if status == "delivered" {
			out[no] = "already_delivered"
			continue
		}
		out[no] = "skipped:" + status
	}
	return out, nil
}
