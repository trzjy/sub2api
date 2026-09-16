package payment

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

func mustParseFX(t *testing.T, raw string) FXRates {
	t.Helper()
	rates, err := ParseFXRates(raw)
	if err != nil {
		t.Fatalf("ParseFXRates(%q) error: %v", raw, err)
	}
	return rates
}

func TestParseFXRatesAcceptsNumberAndStringValues(t *testing.T) {
	t.Parallel()

	rates := mustParseFX(t, `{"CNY": 7.15, "HKD": "7.80"}`)
	if rates["CNY"].String() != "7.15" {
		t.Fatalf("CNY rate = %s, want 7.15", rates["CNY"])
	}
	if rates["HKD"].String() != "7.8" {
		t.Fatalf("HKD rate = %s, want 7.8", rates["HKD"])
	}
}

func TestParseFXRatesDefaultsOnEmpty(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "   "} {
		rates, err := ParseFXRates(raw)
		if err != nil {
			t.Fatalf("ParseFXRates(%q) error: %v", raw, err)
		}
		if rates["CNY"].String() != "7.15" {
			t.Fatalf("default CNY rate = %s, want 7.15", rates["CNY"])
		}
	}
}

func TestParseFXRatesRejectsInvalid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
	}{
		{"not json", "not-json"},
		{"missing CNY", `{"HKD": 7.8}`},
		{"non positive rate", `{"CNY": 0}`},
		{"negative rate", `{"CNY": -1}`},
		{"usd listed", `{"CNY": 7.15, "USD": 1}`},
		{"bad currency code", `{"CNY": 7.15, "renminbi": 7}`},
	}
	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseFXRates(tt.raw); err == nil {
				t.Fatalf("ParseFXRates(%q) expected error", tt.raw)
			}
		})
	}
}

func TestToUSDFromUSDRoundTrip(t *testing.T) {
	t.Parallel()

	fx := mustParseFX(t, `{"CNY": 7.15, "HKD": 7.8}`)

	// ToUSD: ¥100 / 7.15 = 13.9860... USD
	usd, err := fx.ToUSD(decimal.NewFromInt(100), "CNY")
	if err != nil {
		t.Fatalf("ToUSD error: %v", err)
	}
	if !usd.Round(4).Equal(decimal.RequireFromString("13.9860")) {
		t.Fatalf("ToUSD(100, CNY) = %s, want 13.9860", usd.Round(4))
	}

	// FromUSD: $10 × 7.15 = ¥71.50
	cny, err := fx.FromUSD(decimal.NewFromFloat(10), "CNY")
	if err != nil {
		t.Fatalf("FromUSD error: %v", err)
	}
	if cny.String() != "71.5" {
		t.Fatalf("FromUSD(10, CNY) = %s, want 71.5", cny)
	}

	// USD 隐含 1.0：直通
	usd, err = fx.ToUSD(decimal.NewFromFloat(9.99), "USD")
	if err != nil {
		t.Fatalf("ToUSD(USD) error: %v", err)
	}
	if usd.String() != "9.99" {
		t.Fatalf("ToUSD(9.99, USD) = %s, want 9.99", usd)
	}

	// 往返：FromUSD(ToUSD(x)) == x（同一汇率下）
	back, err := fx.FromUSD(usd, "USD")
	if err != nil {
		t.Fatalf("FromUSD(USD) error: %v", err)
	}
	if !back.Equal(decimal.NewFromFloat(9.99)) {
		t.Fatalf("round trip = %s, want 9.99", back)
	}
}

func TestFXRatesMissingCurrencyFailsClosed(t *testing.T) {
	t.Parallel()

	fx := mustParseFX(t, `{"CNY": 7.15}`)
	if _, err := fx.ToUSD(decimal.NewFromInt(100), "HKD"); err == nil {
		t.Fatal("expected missing-rate error on ToUSD")
	} else if err.Error() == "" {
		t.Fatal("error should carry message")
	}
	if _, err := fx.FromUSD(decimal.NewFromInt(1), "HKD"); err == nil {
		t.Fatal("expected missing-rate error on FromUSD")
	}
	if _, err := fx.Rate("JPY"); err == nil {
		t.Fatal("expected missing-rate error on Rate")
	}
}

func TestFXRatesPrecisionStaysDecimal(t *testing.T) {
	t.Parallel()

	// 7.15 无法被 float 精确表示；decimal 解析链路必须保真（0.1 + 0.2 类回归）。
	fx := mustParseFX(t, `{"CNY": 7.15}`)
	got, err := fx.FromUSD(decimal.NewFromFloat(3), "CNY")
	if err != nil {
		t.Fatalf("FromUSD error: %v", err)
	}
	// 3 × 7.15 = 21.45（float 链路可能产生 21.449999...）
	if got.String() != "21.45" {
		t.Fatalf("FromUSD(3, CNY) = %s, want exact 21.45", got)
	}
}

func TestFXRatesMarshalAndToString(t *testing.T) {
	t.Parallel()

	fx := mustParseFX(t, `{"CNY": 7.15}`)
	data, err := json.Marshal(fx)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var back map[string]json.Number
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if back["CNY"].String() != "7.15" {
		t.Fatalf("marshaled CNY = %s, want 7.15", back["CNY"])
	}

	if s := fx.ToString(); s != `{"CNY":7.15}` {
		t.Fatalf("ToString() = %q, want {\"CNY\":7.15}", s)
	}
}

func TestFXRatesToFloatMapIsDisplayOnly(t *testing.T) {
	t.Parallel()

	fx := mustParseFX(t, `{"CNY": 7.15, "HKD": 7.8}`)
	m := fx.ToFloatMap()
	if len(m) != 2 || m["CNY"] != 7.15 || m["HKD"] != 7.8 {
		t.Fatalf("ToFloatMap() = %v, want CNY=7.15 HKD=7.8", m)
	}
	if (FXRates{}).ToFloatMap() != nil {
		t.Fatal("empty rates ToFloatMap should be nil")
	}
}
