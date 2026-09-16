package service

import (
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func paymentProviderConfigCurrency(providerKey string, cfg map[string]string) string {
	switch strings.TrimSpace(providerKey) {
	case payment.TypeStripe, payment.TypeAirwallex:
		currency, err := payment.NormalizePaymentCurrency(cfg["currency"])
		if err == nil {
			return currency
		}
	}
	return payment.DefaultPaymentCurrency
}

// psOrderProviderSnapshotCurrency 从 provider 快照 map 读取币种（新单创建落列用）。
func psOrderProviderSnapshotCurrency(snapshot map[string]any) string {
	if len(snapshot) == 0 {
		return ""
	}
	if raw, ok := snapshot["currency"]; ok {
		if s, ok := raw.(string); ok {
			if currency, err := payment.NormalizePaymentCurrency(s); err == nil {
				return currency
			}
		}
	}
	return ""
}

// PaymentOrderCurrency 返回订单的支付币种。优先读 payment_orders.currency 列
// （253 迁移新增并回填，列值 = COALESCE(NULLIF(snapshot->>'currency',''),'CNY')；
// 正常创建路径由 createOrderInTx 同步写入列）；
// 列空（理论存量未回填）时回退快照，再回退默认 CNY。
// 函数签名不变，全部消费者零改动自动受益。
func PaymentOrderCurrency(order *dbent.PaymentOrder) string {
	if order != nil && order.Currency != "" {
		if currency, err := payment.NormalizePaymentCurrency(order.Currency); err == nil {
			return currency
		}
	}
	if snapshot := psOrderProviderSnapshot(order); snapshot != nil {
		if currency, err := payment.NormalizePaymentCurrency(snapshot.Currency); err == nil {
			return currency
		}
	}
	return payment.DefaultPaymentCurrency
}
