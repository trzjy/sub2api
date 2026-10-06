package payment

import (
	"encoding/json"
	"fmt"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
)

// FXRateMissingCode 是换算遇缺币种汇率时的错误码。checkout 换算遇缺币种失败关闭，
// 不得静默按 1:1 兜底。
const FXRateMissingCode = "FX_RATE_MISSING"

// FXRates 是全局汇率表（新 SSOT），语义为 1 USD = X <币种>。
// USD 隐含 1.0 不写入；CNY 必填。与 settings 中 FX_RATES key 的 JSON 一一对应。
type FXRates map[string]decimal.Decimal

// DefaultFXRates 返回默认汇率表（与既有前端硬编码 7.15 一致，避免存量语义跳变）。
func DefaultFXRates() FXRates {
	rates, err := ParseFXRates(`{"CNY": 7.15}`)
	if err != nil {
		// 字面量默认值必然可解析；此处仅防回归。
		panic(fmt.Sprintf("default fx rates unparseable: %v", err))
	}
	return rates
}

// ParseFXRates 从 settings 的 JSON 字符串解析汇率表，全程 decimal，禁止 float 参与运算链路。
// 空串按默认值处理；非法 JSON、非正数汇率、缺少 CNY 均报错。
// 兼容数值（{"CNY": 7.15}）与字符串（{"CNY": "7.15"}）两种写入形式。
func ParseFXRates(raw string) (FXRates, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultFXRates(), nil
	}
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &rawMap); err != nil {
		return nil, infraerrors.BadRequest("INVALID_FX_RATES", "FX_RATES must be a JSON object of currency → rate")
	}
	rates := make(FXRates, len(rawMap))
	for ccy, rawValue := range rawMap {
		normalized, err := NormalizePaymentCurrency(ccy)
		if err != nil {
			return nil, infraerrors.BadRequest("INVALID_FX_RATES", fmt.Sprintf("invalid currency %q in FX_RATES", ccy))
		}
		if normalized == "USD" {
			return nil, infraerrors.BadRequest("INVALID_FX_RATES", "USD is the accounting currency and must not be listed in FX_RATES")
		}
		value, err := fxRawMessageToDecimal(rawValue)
		if err != nil || !value.IsPositive() {
			return nil, infraerrors.BadRequest("INVALID_FX_RATES", fmt.Sprintf("rate for %s must be a positive number", normalized))
		}
		rates[normalized] = value
	}
	if _, ok := rates["CNY"]; !ok {
		return nil, infraerrors.BadRequest("INVALID_FX_RATES", "FX_RATES must include CNY")
	}
	return rates, nil
}

// fxRawMessageToDecimal 将汇率 JSON 值（数值或字符串）解析为 decimal。
func fxRawMessageToDecimal(raw json.RawMessage) (decimal.Decimal, error) {
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil {
		return decimal.NewFromFloat(num), nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return decimal.NewFromString(strings.TrimSpace(str))
	}
	return decimal.Zero, fmt.Errorf("rate value must be a number or a numeric string")
}

// MarshalJSON 序列化为 settings 存储/展示用的 JSON 对象（数值形式，保留 decimal 精度）。
func (r FXRates) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("{}"), nil
	}
	out := make(map[string]json.Number, len(r))
	for ccy, rate := range r {
		out[ccy] = json.Number(rate.String())
	}
	return json.Marshal(out)
}

// ToString 返回 settings 存储用的 JSON 字符串。
func (r FXRates) ToString() string {
	data, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(data)
}

// ToFloatMap 导出为 float64 map（仅用于 DTO 展示；换算链路不经过它）。
func (r FXRates) ToFloatMap() map[string]float64 {
	if len(r) == 0 {
		return nil
	}
	out := make(map[string]float64, len(r))
	for ccy, rate := range r {
		out[ccy], _ = rate.Float64()
	}
	return out
}

// Rate 返回指定币种的汇率。USD 隐含 1.0。
func (r FXRates) Rate(ccy string) (decimal.Decimal, error) {
	normalized, err := NormalizePaymentCurrency(ccy)
	if err != nil {
		return decimal.Zero, err
	}
	if normalized == "USD" {
		return decimal.NewFromInt(1), nil
	}
	rate, ok := r[normalized]
	if !ok {
		return decimal.Zero, infraerrors.BadRequest(FXRateMissingCode,
			fmt.Sprintf("no FX rate configured for %s", normalized)).
			WithMetadata(map[string]string{"currency": normalized})
	}
	return rate, nil
}

// ToUSD 将支付币种金额换算为 USD 账本金额：amount / rates[ccy]（decimal 运算）。
func (r FXRates) ToUSD(amount decimal.Decimal, ccy string) (decimal.Decimal, error) {
	rate, err := r.Rate(ccy)
	if err != nil {
		return decimal.Zero, err
	}
	return amount.Div(rate), nil
}

// FromUSD 将 USD 账本金额换算为指定支付币种金额：amountUSD × rates[ccy]（decimal 运算）。
func (r FXRates) FromUSD(amountUSD decimal.Decimal, ccy string) (decimal.Decimal, error) {
	rate, err := r.Rate(ccy)
	if err != nil {
		return decimal.Zero, err
	}
	return amountUSD.Mul(rate), nil
}

// ToUSDFloat 是 ToUSD 的 float64 便捷入口；运算仍全程 decimal，仅在边界转换。
func (r FXRates) ToUSDFloat(amount float64, ccy string) (float64, error) {
	converted, err := r.ToUSD(decimal.NewFromFloat(amount), ccy)
	if err != nil {
		return 0, err
	}
	return converted.InexactFloat64(), nil
}

// FromUSDFloat 是 FromUSD 的 float64 便捷入口；运算仍全程 decimal，仅在边界转换。
func (r FXRates) FromUSDFloat(amountUSD float64, ccy string) (float64, error) {
	converted, err := r.FromUSD(decimal.NewFromFloat(amountUSD), ccy)
	if err != nil {
		return 0, err
	}
	return converted.InexactFloat64(), nil
}
