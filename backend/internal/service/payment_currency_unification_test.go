//go:build unit

package service

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// infraErrorReason 提取业务错误码（FX_RATE_MISSING 等）。
func infraErrorReason(t *testing.T, err error) string {
	t.Helper()
	return infraerrors.FromError(err).Reason
}

// === M11 新增用例：快照守卫与三家 CN 渠道 currency 落快照 ===

// 三家 CN 渠道（alipay/easypay/xunhupay）实例**无任何商户字段**时，
// buildPaymentOrderProviderSnapshot 仍返回含 schema_version+currency 的快照
// （len==1 守卫不吞，M5/M11）。
func TestBuildPaymentOrderProviderSnapshot_CNProvidersWithoutMerchantFieldsKeepSnapshot(t *testing.T) {
	t.Parallel()

	for _, providerKey := range []string{payment.TypeAlipay, payment.TypeEasyPay, payment.TypeXunhupay} {
		snapshot := buildPaymentOrderProviderSnapshot(&payment.InstanceSelection{
			InstanceID:  "1",
			ProviderKey: providerKey,
			Config:      map[string]string{"secretKey": "secret"},
		}, CreateOrderRequest{})

		require.NotNil(t, snapshot, providerKey)
		require.Equal(t, 2, snapshot["schema_version"], providerKey)
		require.Equal(t, "CNY", snapshot["currency"], providerKey)
	}
}

// 退化选择（providerKey 为空、仅含 instance_id）命中 len==1 守卫返回 nil（兜底语义保持）。
// M5：三家补 currency 后快照最少两项（{schema_version, currency}），守卫对它们不再触发；
// 守卫本身保留，仅对"无 providerKey 的退化选择"继续生效。`sel==nil` 亦返回 nil。
func TestBuildPaymentOrderProviderSnapshot_EmptyProviderKeyGuardStillApplies(t *testing.T) {
	t.Parallel()

	snapshot := buildPaymentOrderProviderSnapshot(&payment.InstanceSelection{
		InstanceID: "",
		Config:     map[string]string{"x": "y"},
	}, CreateOrderRequest{})

	require.Nil(t, snapshot)
	require.Nil(t, buildPaymentOrderProviderSnapshot(nil, CreateOrderRequest{}))
}

// === M11 新增用例：三家 CN 渠道回调币种校验（对齐 wxpay/stripe 模式） ===

// 快照 currency=CNY、回调 metadata 带 currency=CNY → 通过；
// 带不同币种 → 拒绝；不带 currency（CN 渠道不回传币种）→ 跳过校验不拒绝。
// 覆盖"快照存在但 merchant 字段缺省"的存量单：币种校验仍执行，不因字段缺失拒绝回调。
func TestValidateProviderSnapshotMetadata_CNProvidersCurrency(t *testing.T) {
	t.Parallel()

	for _, providerKey := range []string{payment.TypeAlipay, payment.TypeEasyPay, payment.TypeXunhupay} {
		order := &dbent.PaymentOrder{
			ProviderSnapshot: map[string]any{
				"schema_version": 2,
				"provider_key":   providerKey,
				"currency":       "CNY",
				// 无 merchant_app_id / merchant_id：模拟存量单快照存在但商户字段缺省
			},
		}

		// 同币种 → 通过
		require.NoError(t, validateProviderSnapshotMetadata(order, providerKey, map[string]string{"currency": "cny"}), providerKey)
		// 不回传币种 → 跳过（CN 渠道 metadata 可能无 currency）
		require.NoError(t, validateProviderSnapshotMetadata(order, providerKey, map[string]string{}), providerKey)
		// 异币种 → 拒绝
		err := validateProviderSnapshotMetadata(order, providerKey, map[string]string{"currency": "HKD"})
		require.Error(t, err, providerKey)
		require.Contains(t, err.Error(), "currency mismatch", providerKey)
	}
}

// === M5/PaymentOrderCurrency：先列后快照 ===

// 测试辅助：构造带 provider_snapshot（含 currency）的订单。
func newPaymentOrderForCurrencyTest(snapshotCurrency string) *dbent.PaymentOrder {
	return &dbent.PaymentOrder{
		ProviderSnapshot: map[string]any{
			"schema_version": 2,
			"provider_key":   "stripe",
			"currency":       snapshotCurrency,
		},
	}
}

// 列值优先：currency 列为 HKD 时返回 HKD，即便快照是 CNY。
func TestPaymentOrderCurrency_PrefersColumnOverSnapshot(t *testing.T) {
	t.Parallel()

	order := newPaymentOrderForCurrencyTest("CNY")
	order.Currency = "HKD"
	require.Equal(t, "HKD", PaymentOrderCurrency(order))
}

// 列空时回退快照。
func TestPaymentOrderCurrency_FallsBackToSnapshotWhenColumnEmpty(t *testing.T) {
	t.Parallel()

	order := newPaymentOrderForCurrencyTest("HKD")
	order.Currency = ""
	require.Equal(t, "HKD", PaymentOrderCurrency(order))
}

// 列与快照都空 → 默认 CNY。
func TestPaymentOrderCurrency_DefaultsToCNY(t *testing.T) {
	t.Parallel()

	require.Equal(t, "CNY", PaymentOrderCurrency(nil))
}

// === M2/FX 校验：stripe/airwallex 币种 ∈ FX_RATES ===

// stripe 实例币种不在 FX 表 → 创建被拒，错误码 FX_RATE_MISSING。
func TestCreateProviderInstance_RejectsCurrencyMissingFromFXRates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	svc := &PaymentConfigService{
		entClient:     client,
		settingRepo:   &paymentConfigSettingRepoStub{values: map[string]string{SettingFXRates: `{"CNY": 7.15}`}},
		encryptionKey: []byte("0123456789abcdef0123456789abcdef"),
	}

	_, err := svc.CreateProviderInstance(ctx, CreateProviderInstanceRequest{
		ProviderKey:    payment.TypeStripe,
		Name:           "stripe-jpy",
		Config:         map[string]string{"currency": "JPY", "secretKey": "sk_test_123", "publishableKey": "pk_test_123", "webhookSecret": "whsec-test"},
		SupportedTypes: []string{payment.TypeStripe},
		Enabled:        true,
	})
	require.Error(t, err)
	require.Equal(t, payment.FXRateMissingCode, infraErrorReason(t, err))
}

// FX 表包含该币种 → 创建通过。
func TestCreateProviderInstance_AcceptsCurrencyPresentInFXRates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	svc := &PaymentConfigService{
		entClient:     client,
		settingRepo:   &paymentConfigSettingRepoStub{values: map[string]string{SettingFXRates: `{"CNY": 7.15, "JPY": 150}`}},
		encryptionKey: []byte("0123456789abcdef0123456789abcdef"),
	}

	_, err := svc.CreateProviderInstance(ctx, CreateProviderInstanceRequest{
		ProviderKey:    payment.TypeStripe,
		Name:           "stripe-jpy",
		Config:         map[string]string{"currency": "JPY", "secretKey": "sk_test_123", "publishableKey": "pk_test_123", "webhookSecret": "whsec-test"},
		SupportedTypes: []string{payment.TypeStripe},
		Enabled:        true,
	})
	require.NoError(t, err)
}

// === M2 反向校验：保存 FX_RATES 时，已启用 stripe/airwallex 实例币种必须仍在表内 ===

func TestUpdatePaymentConfig_RejectsFXRatesDroppingEnabledInstanceCurrency(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	settingRepo := &paymentConfigSettingRepoStub{values: map[string]string{SettingFXRates: `{"CNY": 7.15, "HKD": 7.8}`}}
	svc := &PaymentConfigService{
		entClient:     client,
		settingRepo:   settingRepo,
		encryptionKey: []byte("0123456789abcdef0123456789abcdef"),
	}

	_, err := svc.CreateProviderInstance(ctx, CreateProviderInstanceRequest{
		ProviderKey:    payment.TypeStripe,
		Name:           "stripe-hkd",
		Config:         map[string]string{"currency": "HKD", "secretKey": "sk_test_123", "publishableKey": "pk_test_123", "webhookSecret": "whsec-test"},
		SupportedTypes: []string{payment.TypeStripe},
		Enabled:        true,
	})
	require.NoError(t, err)

	// 删除 HKD → 拒绝保存
	err = svc.UpdatePaymentConfig(ctx, UpdatePaymentConfigRequest{
		FXRates: strPtr(`{"CNY": 7.15}`),
	})
	require.Error(t, err)
	require.Equal(t, payment.FXRateMissingCode, infraErrorReason(t, err))
	// 旧值未被覆盖
	require.Contains(t, settingRepo.values[SettingFXRates], "HKD")

	// 保留 HKD → 通过
	err = svc.UpdatePaymentConfig(ctx, UpdatePaymentConfigRequest{
		FXRates: strPtr(`{"CNY": 7.2, "HKD": 7.8}`),
	})
	require.NoError(t, err)
	require.Contains(t, settingRepo.values[SettingFXRates], "7.2")
}

// === M8/pricing cost basis：FX 空值回落全局 FX_RATES["CNY"] ===

func TestGetPricingCostBasis_FXFallsBackToGlobalFXRates(t *testing.T) {
	t.Parallel()

	repo := &paymentConfigSettingRepoStub{values: map[string]string{
		SettingFXRates: `{"CNY": 7.3}`,
		SettingKeyPricingCostBasis: `{"plans":[
			{"provider":"p1","monthly_fee_cny":100,"fx":0},
			{"provider":"p2","monthly_fee_cny":100,"fx":7.9}
		]}`,
	}}

	basis, err := getPricingCostBasis(context.Background(), repo)
	require.NoError(t, err)
	require.Len(t, basis.Plans, 2)
	// 空值回落全局
	require.Equal(t, 7.3, basis.Plans[0].FX)
	// 已配置值保留可覆盖
	require.Equal(t, 7.9, basis.Plans[1].FX)
}

func TestGetPricingCostBasis_FXFallsBackToDefaultWhenFXRatesMissing(t *testing.T) {
	t.Parallel()

	repo := &paymentConfigSettingRepoStub{values: map[string]string{
		SettingKeyPricingCostBasis: `{"plans":[{"provider":"p1","monthly_fee_cny":100}]}`,
	}}

	basis, err := getPricingCostBasis(context.Background(), repo)
	require.NoError(t, err)
	require.Len(t, basis.Plans, 1)
	// 默认 {"CNY": 7.15}
	require.Equal(t, 7.15, basis.Plans[0].FX)
}
