package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 闲鱼发货对账任务：以 Worker 自动发货订单（xy_orders delivery_method='auto'）为基准，
// 与主程序两本账（xianyu_order_claims 卡密领取记录、xianyu_worker_deliveries 订单镜像）
// 逐单核对。能确定结论的漂移自动补平，不能确定的告警人工介入。
//
// 漂移类型与处理：
//
//	A) Worker 已发 + claim 缺失：发货内容能提取唯一卡密且该码在主程序是 delivered
//	   → 自动补登 claim（sent，binding_source=reconcile）；码不在主程序/多码 → 告警。
//	B) Worker 已发 + claim=pending：回执回传丢失 → RecordDeliveryResult 收口 sent。
//	C) Worker 发货失败 + claim=pending：→ RecordDeliveryResult 收口 failed（可人工补发）。
//	D) Worker 已发 + claim=sent + 镜像缺失/非 sent：→ 补登/更新镜像为 sent。
//	E) Worker 已发 + claim=failed，或 Worker 失败 + claim=sent：→ 冲突，只告警。
//	F) Worker 订单存在但镜像缺失且 claim 也缺失（注册回传与 claim 双丢）：
//	   按 A 处理（A 内会同时补镜像）。
//
// 历史豁免：首轮运行只建立水位线（baseline=部署时刻），之前的订单一律不核对，
// 避免测试期遗留单（无真实卡密）触发误报。
const (
	xianyuReconcileInterval    = 5 * time.Minute
	xianyuReconcileOverlap     = 10 * time.Minute
	xianyuReconcileWorkerLimit = 500
	xianyuReconcileMirrorLimit = 2000

	// XianyuReconcileBindingSource 是对账补登记录的绑定来源标记。
	XianyuReconcileBindingSource = "reconcile"
)

var xianyuReconcileCodePattern = regexp.MustCompile(`[0-9a-f]{32}`)

// XianyuReconcileWorker 是对账任务需要的 Worker 调用面（*XianyuWorkerService 实现）。
type XianyuReconcileWorker interface {
	ListAutoDeliveries(ctx context.Context, since time.Time, limit int) ([]XianyuWorkerAutoDelivery, error)
}

// XianyuReconcileClaimInserter 是对账补登的持久化面（XianyuDeliveryRepository 实现）。
type XianyuReconcileClaimInserter interface {
	InsertReconciledClaim(ctx context.Context, claim XianyuDeliveryClaim, codeID int64) error
}

// XianyuReconcileMirrorRepository 是对账任务需要的订单镜像持久化面。
type XianyuReconcileMirrorRepository interface {
	ListWorkerDeliveriesUpdatedSince(ctx context.Context, since time.Time, limit int) ([]XianyuWorkerDelivery, error)
	EnsureWorkerDeliveryRecord(ctx context.Context, d XianyuWorkerDelivery) error
	RecordWorkerDeliveryResult(ctx context.Context, orderNo string, result XianyuDeliveryStatusResult) error
}

// XianyuReconcilePoolLookup 按库存池 slug 解析池（*xianyuControlRepository 实现）。
type XianyuReconcilePoolLookup interface {
	GetItemPoolBySlug(ctx context.Context, slug string) (*XianyuItemPool, error)
}

// XianyuReconcileAlerter 是对账漂移告警出口（*XianyuAlertService 实现）。
type XianyuReconcileAlerter interface {
	SendReconcileAlert(ctx context.Context, sourceID, reminder string, variables map[string]string)
}

// XianyuReconcileRedeemReader 按码文本查兑换码（RedeemCodeRepository 实现）。
type XianyuReconcileRedeemReader interface {
	GetByCode(ctx context.Context, code string) (*RedeemCode, error)
}

// XianyuReconcileControl 是对账任务需要的开关面（*XianyuControlService 实现）。
type XianyuReconcileControl interface {
	Enabled(ctx context.Context) bool
}

// XianyuReconcileService 是闲鱼发货对账后台任务。
type XianyuReconcileService struct {
	control  XianyuReconcileControl
	worker   XianyuReconcileWorker
	state    XianyuDeliveryStateUpdater
	inserter XianyuReconcileClaimInserter
	mirror   XianyuReconcileMirrorRepository
	pools    XianyuReconcilePoolLookup
	redeem   XianyuReconcileRedeemReader
	setting  XianyuSettingStore
	alert    XianyuReconcileAlerter

	now func() time.Time

	parentCtx    context.Context
	parentCancel context.CancelFunc
	stopCh       chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
}

// NewXianyuReconcileService 创建对账任务（尚未启动）。
func NewXianyuReconcileService(
	control XianyuReconcileControl,
	worker XianyuReconcileWorker,
	state XianyuDeliveryStateUpdater,
	inserter XianyuReconcileClaimInserter,
	mirror XianyuReconcileMirrorRepository,
	pools XianyuReconcilePoolLookup,
	redeem XianyuReconcileRedeemReader,
	setting XianyuSettingStore,
	alert XianyuReconcileAlerter,
) *XianyuReconcileService {
	ctx, cancel := context.WithCancel(context.Background())
	return &XianyuReconcileService{
		control: control, worker: worker, state: state, inserter: inserter, mirror: mirror,
		pools: pools, redeem: redeem, setting: setting, alert: alert,
		now:          time.Now,
		parentCtx:    ctx,
		parentCancel: cancel,
		stopCh:       make(chan struct{}),
	}
}

// Start 启动周期对账（首轮仅建立水位线）。
func (s *XianyuReconcileService) Start() {
	s.wg.Add(1)
	go s.loop()
}

// Stop 停止对账任务并等待退出。
func (s *XianyuReconcileService) Stop() {
	s.stopOnce.Do(func() {
		s.parentCancel()
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *XianyuReconcileService) loop() {
	defer s.wg.Done()
	s.runOnce()
	ticker := time.NewTicker(xianyuReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-s.parentCtx.Done():
			return
		case <-ticker.C:
			s.runOnce()
		}
	}
}

func (s *XianyuReconcileService) runOnce() {
	ctx, cancel := context.WithTimeout(s.parentCtx, 2*time.Minute)
	defer cancel()

	if s.control == nil || !s.control.Enabled(ctx) {
		return
	}
	now := s.now().UTC()
	wm, err := s.loadWatermark(ctx)
	if err != nil {
		slog.Warn("xianyu reconcile: load watermark failed", "error", err)
		return
	}
	if wm.IsZero() {
		// 首轮：只建立基线，历史订单（含测试期遗留）一律豁免。
		if err := s.saveWatermark(ctx, now); err != nil {
			slog.Warn("xianyu reconcile: save baseline watermark failed", "error", err)
			return
		}
		slog.Info("xianyu reconcile: baseline established, history exempt", "watermark", now.Format(time.RFC3339))
		return
	}

	since := wm.Add(-xianyuReconcileOverlap)
	orders, err := s.worker.ListAutoDeliveries(ctx, since, xianyuReconcileWorkerLimit)
	if err != nil {
		// Worker 不可达有独立健康告警覆盖，对账本轮跳过即可。
		slog.Warn("xianyu reconcile: list worker auto deliveries failed, skip round", "error", err)
		return
	}
	mirrors, err := s.mirror.ListWorkerDeliveriesUpdatedSince(ctx, since, xianyuReconcileMirrorLimit)
	if err != nil {
		slog.Warn("xianyu reconcile: list mirror deliveries failed, skip round", "error", err)
		return
	}
	mirrorByOrder := make(map[string]*XianyuWorkerDelivery, len(mirrors))
	for i := range mirrors {
		mirrorByOrder[mirrors[i].OrderNo] = &mirrors[i]
	}

	for _, order := range orders {
		s.reconcileOrder(ctx, order, mirrorByOrder[order.OrderNo])
	}

	if err := s.saveWatermark(ctx, now); err != nil {
		slog.Warn("xianyu reconcile: save watermark failed", "error", err)
	}
}

// reconcileOrder 核对单个 Worker 自动发货订单。所有补平操作幂等，重复执行安全。
func (s *XianyuReconcileService) reconcileOrder(ctx context.Context, order XianyuWorkerAutoDelivery, mirror *XianyuWorkerDelivery) {
	orderNo := strings.TrimSpace(order.OrderNo)
	if orderNo == "" {
		return
	}
	// Worker 语义：delivery_content 在消息发送失败时也会落库（伴随 delivery_fail_reason），
	// 因此"已发"必须同时满足内容非空且无失败原因。
	workerSent := strings.TrimSpace(order.DeliveryContent) != "" && strings.TrimSpace(order.DeliveryFailReason) == ""
	workerFailed := strings.TrimSpace(order.DeliveryFailReason) != ""

	claim, err := s.state.GetDeliveryClaim(ctx, orderNo)
	claimExists := err == nil
	if err != nil && !errors.Is(err, ErrXianyuDeliveryClaimNotFound) {
		slog.Warn("xianyu reconcile: load claim failed", "order_no", orderNo, "error", err)
		return
	}

	switch {
	case !claimExists && workerSent:
		s.healMissingClaim(ctx, order, mirror)
	case !claimExists:
		// Worker 未发货成功且没有 claim：claim 阶段失败（库存不足等）属正常态。
		// 仅当镜像声称 sent 时才是矛盾，告警人工核对。
		if mirror != nil && mirror.DeliveryStatus == XianyuDeliveryStatusSent {
			s.alertDrift(ctx, orderNo, "conflict-mirror-sent-no-claim", order, mirror, nil,
				"镜像记录为已发送，但无卡密领取记录且 Worker 未成功发货，请人工核对")
		}
	case claimExists && workerSent && claim.DeliveryStatus == XianyuDeliveryStatusPending:
		// B) 回执回传丢失：Worker 已证明发货成功，收口为 sent。
		if err := s.state.RecordDeliveryResult(ctx, XianyuDeliveryStatusResult{
			OrderNo: orderNo, Success: true, Confirmed: true,
			Attempt: claim.AttemptCount, QuantitySent: order.Quantity,
		}); err != nil {
			slog.Warn("xianyu reconcile: close claim as sent failed", "order_no", orderNo, "error", err)
			return
		}
		s.alertDrift(ctx, orderNo, "healed-claim-sent", order, mirror, claim,
			"发货回执丢失，对账已自动补平：领取记录收口为已发送")
		s.healMirrorIfStale(ctx, order, mirror, XianyuDeliveryStatusSent)
	case claimExists && workerFailed && claim.DeliveryStatus == XianyuDeliveryStatusPending:
		// C) Worker 明确失败但 claim 挂起：收口 failed，恢复人工补发入口。
		failReason := strings.TrimSpace(order.DeliveryFailReason)
		if err := s.state.RecordDeliveryResult(ctx, XianyuDeliveryStatusResult{
			OrderNo: orderNo, Success: false, Error: &failReason, Attempt: claim.AttemptCount,
		}); err != nil {
			slog.Warn("xianyu reconcile: close claim as failed failed", "order_no", orderNo, "error", err)
			return
		}
		s.alertDrift(ctx, orderNo, "healed-claim-failed", order, mirror, claim,
			"领取记录长期挂起，对账已按 Worker 失败原因收口为失败，可在发货记录页补发")
	case claimExists && workerSent && claim.DeliveryStatus == XianyuDeliveryStatusFailed:
		// E) Worker 已发但 claim 记为失败：可能重复发货或回执错乱，只告警。
		s.alertDrift(ctx, orderNo, "conflict-claim-failed-worker-sent", order, mirror, claim,
			"领取记录为失败但 Worker 已成功发货，存在重复发货风险，请人工核对")
	case claimExists && workerFailed && claim.DeliveryStatus == XianyuDeliveryStatusSent:
		// E) claim 已发但 Worker 报失败：结论冲突，只告警。
		s.alertDrift(ctx, orderNo, "conflict-claim-sent-worker-failed", order, mirror, claim,
			"领取记录为已发送但 Worker 报告发货失败，请人工核对买家是否收到卡密")
	case claimExists && workerSent && claim.DeliveryStatus == XianyuDeliveryStatusSent:
		// D) 双边一致已发：仅镜像可能缺失/滞后，补平镜像。
		s.healMirrorIfStale(ctx, order, mirror, XianyuDeliveryStatusSent)
	}
}

// healMissingClaim 处理漂移 A：Worker 已发货但主程序没有卡密领取记录。
// 仅当发货内容能提取唯一 32 位十六进制卡密、且该码在主程序处于 delivered 时自动补登；
// 否则告警人工（码不在主程序 = 非主程序库存发出，补登会伪造数据）。
func (s *XianyuReconcileService) healMissingClaim(ctx context.Context, order XianyuWorkerAutoDelivery, mirror *XianyuWorkerDelivery) {
	orderNo := order.OrderNo
	codes := xianyuReconcileCodePattern.FindAllString(order.DeliveryContent, -1)
	if len(codes) != 1 {
		s.alertDrift(ctx, orderNo, "drift-no-claim-code-unresolved", order, mirror, nil,
			fmt.Sprintf("Worker 已发货但无领取记录，且发货内容含 %d 个候选卡密（≠1），无法自动补登", len(codes)))
		return
	}
	code, err := s.redeem.GetByCode(ctx, codes[0])
	if err != nil {
		if errors.Is(err, ErrRedeemCodeNotFound) {
			s.alertDrift(ctx, orderNo, "drift-no-claim-code-unknown", order, mirror, nil,
				"Worker 已发货但无领取记录，且发出的卡密不在主程序库存中（非主程序渠道发货），请人工核对")
			return
		}
		slog.Warn("xianyu reconcile: lookup redeem code failed", "order_no", orderNo, "error", err)
		return
	}

	// 从码的库存标记反推归属池（库存码 notes 约定为 xianyu_pool=<slug>）。
	var poolID, productID int64
	if slug, ok := strings.CutPrefix(code.Notes, "xianyu_pool="); ok && slug != "" {
		if pool, err := s.pools.GetItemPoolBySlug(ctx, slug); err == nil && pool != nil {
			poolID = pool.ID
		}
	}

	insertClaim := XianyuDeliveryClaim{
		OrderID:   orderNo,
		ItemID:    order.ItemID,
		AccountID: order.AccountID,
		BuyerID:   order.BuyerID,
		ChatID:    order.ChatID,
		Amount:    order.Amount,
		ProductID: productID,
		PoolID:    poolID,
		// BuyerID 列 NOT NULL：老订单 buyer_id 可能缺失，用 chat_id 兜底保持可补发语义。
		BindingSource: XianyuReconcileBindingSource,
	}
	if insertClaim.BuyerID == "" {
		insertClaim.BuyerID = order.ChatID
	}
	if insertClaim.BuyerID == "" {
		insertClaim.BuyerID = "unknown"
	}

	if err := s.inserter.InsertReconciledClaim(ctx, insertClaim, code.ID); err != nil {
		if errors.Is(err, ErrXianyuReconcileCodeUnmatched) {
			s.alertDrift(ctx, orderNo, "drift-no-claim-code-not-delivered", order, mirror, nil,
				"Worker 已发货但无领取记录，发出的卡密在主程序中不是已发货状态，请人工核对")
			return
		}
		slog.Warn("xianyu reconcile: insert reconciled claim failed", "order_no", orderNo, "error", err)
		return
	}

	// claim 补登成功后，镜像缺失则一并补登为 sent（漂移 F 同步愈合）。
	s.healMirrorIfStale(ctx, order, mirror, XianyuDeliveryStatusSent)
	s.alertDrift(ctx, orderNo, "healed-claim-registered", order, mirror, nil,
		"Worker 已发货但缺少卡密领取记录，对账已自动补登为已发送")
}

// healMirrorIfStale 镜像缺失则补登、非终态则按目标状态收口（幂等；已是终态会被仓储层忽略）。
func (s *XianyuReconcileService) healMirrorIfStale(ctx context.Context, order XianyuWorkerAutoDelivery, mirror *XianyuWorkerDelivery, targetStatus string) {
	orderNo := order.OrderNo
	if mirror != nil && mirror.DeliveryStatus == targetStatus {
		return
	}
	if mirror == nil {
		quantity := order.Quantity
		if quantity <= 0 {
			quantity = 1
		}
		if err := s.mirror.EnsureWorkerDeliveryRecord(ctx, XianyuWorkerDelivery{
			OrderNo: orderNo, DeliveryKind: "auto", Quantity: quantity,
		}); err != nil {
			slog.Warn("xianyu reconcile: ensure mirror record failed", "order_no", orderNo, "error", err)
			return
		}
	}
	result := XianyuDeliveryStatusResult{OrderNo: orderNo, QuantitySent: order.Quantity}
	if targetStatus == XianyuDeliveryStatusSent {
		result.Success = true
		result.Confirmed = true
	} else {
		reason := "reconciled by delivery audit"
		result.Error = &reason
	}
	if err := s.mirror.RecordWorkerDeliveryResult(ctx, orderNo, result); err != nil {
		slog.Warn("xianyu reconcile: update mirror record failed", "order_no", orderNo, "error", err)
		return
	}
	s.alertDrift(ctx, orderNo, "healed-mirror-"+targetStatus, order, mirror, nil,
		"订单发货镜像缺失/滞后，对账已自动补平为"+targetStatus)
}

// alertDrift 发送对账告警（healed-* 为已自动补平通知，drift-*/conflict-* 需人工介入）。
// reminder 以漂移类型为键：同一订单同一类型去重，类型变化（如补平后再漂移）会再次通知。
func (s *XianyuReconcileService) alertDrift(ctx context.Context, orderNo, driftType string, order XianyuWorkerAutoDelivery, mirror *XianyuWorkerDelivery, claim *XianyuOrderClaim, detail string) {
	if s.alert == nil {
		return
	}
	vars := map[string]string{
		"order_no":      orderNo,
		"drift_type":    driftType,
		"worker_status": order.Status,
		"detail":        detail,
	}
	if mirror != nil {
		vars["mirror_status"] = mirror.DeliveryStatus
	} else {
		vars["mirror_status"] = "missing"
	}
	if claim != nil {
		vars["claim_status"] = claim.DeliveryStatus
	} else {
		vars["claim_status"] = "missing"
	}
	s.alert.SendReconcileAlert(ctx, "order:"+orderNo, "reconcile:"+driftType, vars)
}

func (s *XianyuReconcileService) loadWatermark(ctx context.Context) (time.Time, error) {
	raw, err := s.setting.GetValue(ctx, SettingKeyXianyuReconcileWatermark)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	wm, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse reconcile watermark: %w", err)
	}
	return wm.UTC(), nil
}

func (s *XianyuReconcileService) saveWatermark(ctx context.Context, ts time.Time) error {
	return s.setting.SetMultiple(ctx, map[string]string{
		SettingKeyXianyuReconcileWatermark: ts.UTC().Format(time.RFC3339),
	})
}
