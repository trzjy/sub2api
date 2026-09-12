package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type reconcileControlStub struct{ enabled bool }

func (s reconcileControlStub) Enabled(context.Context) bool { return s.enabled }

type reconcileWorkerStub struct {
	orders []XianyuWorkerAutoDelivery
	err    error
	calls  int
}

func (s *reconcileWorkerStub) ListAutoDeliveries(context.Context, time.Time, int) ([]XianyuWorkerAutoDelivery, error) {
	s.calls++
	return s.orders, s.err
}

type reconcileStateStub struct {
	claim *XianyuOrderClaim
	err   error
	// results 记录每次 RecordDeliveryResult 的输入；claimByOrder 决定 GetDeliveryClaim 行为。
	results     []XianyuDeliveryStatusResult
	claimByOrder map[string]*XianyuOrderClaim
	insertErr   error
	inserts     []XianyuDeliveryClaim
}

func (s *reconcileStateStub) GetDeliveryClaim(_ context.Context, orderNo string) (*XianyuOrderClaim, error) {
	if s.claimByOrder != nil {
		if c, ok := s.claimByOrder[orderNo]; ok {
			if c == nil {
				return nil, ErrXianyuDeliveryClaimNotFound
			}
			return c, nil
		}
		return nil, ErrXianyuDeliveryClaimNotFound
	}
	if s.claim == nil {
		return nil, ErrXianyuDeliveryClaimNotFound
	}
	return s.claim, s.err
}

func (s *reconcileStateStub) RecordDeliveryResult(_ context.Context, result XianyuDeliveryStatusResult) error {
	s.results = append(s.results, result)
	return s.err
}

func (s *reconcileStateStub) ResendOriginalCode(context.Context, string) (string, int, error) {
	panic("unexpected ResendOriginalCode call")
}

func (s *reconcileStateStub) InsertReconciledClaim(_ context.Context, claim XianyuDeliveryClaim, _ int64) error {
	s.inserts = append(s.inserts, claim)
	return s.insertErr
}

type reconcileMirrorStub struct {
	mirrors []XianyuWorkerDelivery
	ensured []XianyuWorkerDelivery
	updated []string
	err     error
}

func (s *reconcileMirrorStub) ListWorkerDeliveriesUpdatedSince(context.Context, time.Time, int) ([]XianyuWorkerDelivery, error) {
	return s.mirrors, s.err
}

func (s *reconcileMirrorStub) EnsureWorkerDeliveryRecord(_ context.Context, d XianyuWorkerDelivery) error {
	s.ensured = append(s.ensured, d)
	return s.err
}

func (s *reconcileMirrorStub) RecordWorkerDeliveryResult(_ context.Context, orderNo string, _ XianyuDeliveryStatusResult) error {
	s.updated = append(s.updated, orderNo)
	return s.err
}

type reconcilePoolsStub struct{ id int64 }

func (s reconcilePoolsStub) GetItemPoolBySlug(context.Context, string) (*XianyuItemPool, error) {
	return &XianyuItemPool{ID: s.id, Slug: "pool-test"}, nil
}

type reconcileRedeemStub struct {
	code *RedeemCode
	err  error
}

func (s reconcileRedeemStub) GetByCode(context.Context, string) (*RedeemCode, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.code == nil {
		return nil, ErrRedeemCodeNotFound
	}
	return s.code, nil
}

type reconcileSettingStub struct{ values map[string]string }

func (s *reconcileSettingStub) GetValue(_ context.Context, key string) (string, error) {
	if v, ok := s.values[key]; ok {
		return v, nil
	}
	return "", ErrSettingNotFound
}

func (s *reconcileSettingStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *reconcileSettingStub) SetMultiple(_ context.Context, values map[string]string) error {
	for k, v := range values {
		s.values[k] = v
	}
	return nil
}

type reconcileAlertStub struct{ calls []string }

func (s *reconcileAlertStub) SendReconcileAlert(_ context.Context, sourceID, reminder string, _ map[string]string) {
	s.calls = append(s.calls, sourceID+"/"+reminder)
}

func newReconcileTestService(
	worker *reconcileWorkerStub,
	state *reconcileStateStub,
	mirror *reconcileMirrorStub,
	redeem *reconcileRedeemStub,
	settings map[string]string,
) (*XianyuReconcileService, *reconcileAlertStub) {
	alert := &reconcileAlertStub{}
	svc := NewXianyuReconcileService(
		reconcileControlStub{enabled: true},
		worker, state, state, mirror,
		reconcilePoolsStub{id: 9}, redeem,
		&reconcileSettingStub{values: settings}, alert,
	)
	return svc, alert
}

func autoOrder(orderNo, content string) XianyuWorkerAutoDelivery {
	return XianyuWorkerAutoDelivery{
		OrderNo: orderNo, Status: "shipped", AccountID: "a1", ItemID: "i1",
		BuyerID: "b1", ChatID: "c1", Quantity: 1,
		DeliveryContent: content,
		UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
}

func TestReconcile_FirstRunEstablishesBaseline(t *testing.T) {
	worker := &reconcileWorkerStub{orders: []XianyuWorkerAutoDelivery{autoOrder("o1", "卡密：abcd")}}
	svc, alert := newReconcileTestService(worker, &reconcileStateStub{}, &reconcileMirrorStub{},
		&reconcileRedeemStub{}, map[string]string{})

	svc.runOnce()

	require.Equal(t, 0, worker.calls, "首轮只建水位线，不拉取 Worker")
	require.Empty(t, alert.calls)
	raw, err := svc.setting.GetValue(context.Background(), SettingKeyXianyuReconcileWatermark)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
}

func TestReconcile_DisabledSkips(t *testing.T) {
	worker := &reconcileWorkerStub{}
	alert := &reconcileAlertStub{}
	svc := NewXianyuReconcileService(
		reconcileControlStub{enabled: false},
		worker, &reconcileStateStub{}, &reconcileStateStub{}, &reconcileMirrorStub{},
		reconcilePoolsStub{id: 9}, &reconcileRedeemStub{},
		&reconcileSettingStub{values: map[string]string{SettingKeyXianyuReconcileWatermark: time.Now().UTC().Format(time.RFC3339)}},
		alert,
	)
	svc.runOnce()
	require.Equal(t, 0, worker.calls, "开关关闭时不拉取")
}

func TestReconcile_ClaimPendingClosedAsSent(t *testing.T) {
	worker := &reconcileWorkerStub{orders: []XianyuWorkerAutoDelivery{autoOrder("o1", "卡密：abcd")}}
	state := &reconcileStateStub{claimByOrder: map[string]*XianyuOrderClaim{
		"o1": {OrderNo: "o1", DeliveryStatus: XianyuDeliveryStatusPending, AttemptCount: 0},
	}}
	svc, alert := newReconcileTestService(worker, state, &reconcileMirrorStub{},
		&reconcileRedeemStub{}, watermarkSettings())

	svc.runOnce()

	require.Len(t, state.results, 1)
	require.Equal(t, "o1", state.results[0].OrderNo)
	require.True(t, state.results[0].Success)
	require.True(t, state.results[0].Confirmed)
	require.Contains(t, strings.Join(alert.calls, ","), "o1")
}

func TestReconcile_MissingClaimHealedWhenCodeDelivered(t *testing.T) {
	worker := &reconcileWorkerStub{orders: []XianyuWorkerAutoDelivery{autoOrder("o1", "卡密：f962e8b2cb8e06b655c78e6a6d564c14")}}
	state := &reconcileStateStub{claimByOrder: map[string]*XianyuOrderClaim{}}
	redeem := &reconcileRedeemStub{code: &RedeemCode{ID: 7, Code: "f962e8b2cb8e06b655c78e6a6d564c14", Status: StatusDelivered, Notes: "xianyu_pool=pool-test"}}
	svc, alert := newReconcileTestService(worker, state, &reconcileMirrorStub{}, redeem, watermarkSettings())

	svc.runOnce()

	require.Len(t, state.inserts, 1, "应补登 claim")
	require.Equal(t, "o1", state.inserts[0].OrderID)
	require.Equal(t, int64(9), state.inserts[0].PoolID, "应从码的库存标记反推池")
	require.Equal(t, XianyuReconcileBindingSource, state.inserts[0].BindingSource)
	require.Contains(t, strings.Join(alert.calls, ","), "healed-claim-registered")
}

func TestReconcile_MissingClaimAlertsWhenCodeUnknown(t *testing.T) {
	worker := &reconcileWorkerStub{orders: []XianyuWorkerAutoDelivery{autoOrder("o1", "sub2api test-pool claim 72281153c8e02f9671b3bcd9e5457e06")}}
	state := &reconcileStateStub{claimByOrder: map[string]*XianyuOrderClaim{}}
	// 历史测试串：码不在主程序。
	redeem := &reconcileRedeemStub{}
	svc, alert := newReconcileTestService(worker, state, &reconcileMirrorStub{}, redeem, watermarkSettings())

	svc.runOnce()

	require.Empty(t, state.inserts, "未知码不得补登")
	require.Contains(t, strings.Join(alert.calls, ","), "drift-no-claim-code-unknown")
}

func TestReconcile_MissingClaimMultiCodeAlerts(t *testing.T) {
	content := "卡密1：f962e8b2cb8e06b655c78e6a6d564c14\n---\n卡密2：0245d77ea2bb2422e734615b38c259ef"
	worker := &reconcileWorkerStub{orders: []XianyuWorkerAutoDelivery{autoOrder("o1", content)}}
	state := &reconcileStateStub{claimByOrder: map[string]*XianyuOrderClaim{}}
	svc, alert := newReconcileTestService(worker, state, &reconcileMirrorStub{},
		&reconcileRedeemStub{}, watermarkSettings())

	svc.runOnce()

	require.Empty(t, state.inserts, "多码不得自动补登")
	require.Contains(t, strings.Join(alert.calls, ","), "drift-no-claim-code-unresolved")
}

func TestReconcile_WorkerFailedClosesPendingClaim(t *testing.T) {
	order := autoOrder("o1", "")
	order.DeliveryFailReason = "自动确认发货开关未开启"
	worker := &reconcileWorkerStub{orders: []XianyuWorkerAutoDelivery{order}}
	state := &reconcileStateStub{claimByOrder: map[string]*XianyuOrderClaim{
		"o1": {OrderNo: "o1", DeliveryStatus: XianyuDeliveryStatusPending, AttemptCount: 0},
	}}
	svc, alert := newReconcileTestService(worker, state, &reconcileMirrorStub{},
		&reconcileRedeemStub{}, watermarkSettings())

	svc.runOnce()

	require.Len(t, state.results, 1)
	require.False(t, state.results[0].Success, "应收口为失败")
	require.NotNil(t, state.results[0].Error)
	require.Contains(t, strings.Join(alert.calls, ","), "healed-claim-failed")
}

func TestReconcile_ConflictAlertsOnly(t *testing.T) {
	worker := &reconcileWorkerStub{orders: []XianyuWorkerAutoDelivery{autoOrder("o1", "卡密：abcd")}}
	state := &reconcileStateStub{claimByOrder: map[string]*XianyuOrderClaim{
		"o1": {OrderNo: "o1", DeliveryStatus: XianyuDeliveryStatusFailed, AttemptCount: 0},
	}}
	svc, alert := newReconcileTestService(worker, state, &reconcileMirrorStub{},
		&reconcileRedeemStub{}, watermarkSettings())

	svc.runOnce()

	require.Empty(t, state.results, "冲突不得自动改状态")
	require.Empty(t, state.inserts)
	require.Contains(t, strings.Join(alert.calls, ","), "conflict-claim-failed-worker-sent")
}

func watermarkSettings() map[string]string {
	return map[string]string{
		SettingKeyXianyuReconcileWatermark: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
	}
}
