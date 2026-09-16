package service

import (
	"math"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/shopspring/decimal"
)

const defaultRechargeMarkup = 1.0

func normalizeRechargeMarkup(markup float64) float64 {
	if math.IsNaN(markup) || math.IsInf(markup, 0) || markup <= 0 {
		return defaultRechargeMarkup
	}
	return markup
}

// calculateCreditedBalance 计算用户到账金额（USD 账本）。
// credited = ToUSD(实付金额, 支付币种) × markup × 1.0，精度取 Round(2)。
func calculateCreditedBalance(paymentAmount float64, markup float64, currency string, fxRates payment.FXRates) (float64, error) {
	payDecimal := decimal.NewFromFloat(paymentAmount)
	usd, err := fxRates.ToUSD(payDecimal, currency)
	if err != nil {
		return 0, err
	}
	return usd.Mul(decimal.NewFromFloat(markup)).Round(2).InexactFloat64(), nil
}

func calculateGatewayRefundAmount(orderAmount, payAmount, refundAmount float64, currency string) float64 {
	if orderAmount <= 0 || payAmount <= 0 || refundAmount <= 0 {
		return 0
	}
	fractionDigits := int32(payment.CurrencyMaxFractionDigits(currency))
	if math.Abs(refundAmount-orderAmount) <= paymentAmountToleranceForCurrency(currency) {
		return decimal.NewFromFloat(payAmount).Round(fractionDigits).InexactFloat64()
	}
	return decimal.NewFromFloat(payAmount).
		Mul(decimal.NewFromFloat(refundAmount)).
		Div(decimal.NewFromFloat(orderAmount)).
		Round(fractionDigits).
		InexactFloat64()
}
