package service

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/ent/paymentproviderinstance"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// validateProviderCurrencyInFXRates 校验 stripe/airwallex 实例的币种必须存在于全局 FX_RATES。
// 保存（创建/更新启用）实例时调用；校验通过返回 nil。
func (s *PaymentConfigService) validateProviderCurrencyInFXRates(ctx context.Context, providerKey string, config map[string]string) error {
	if providerKey != payment.TypeStripe && providerKey != payment.TypeAirwallex {
		return nil
	}
	currency := paymentProviderConfigCurrency(providerKey, config)
	normalized, nerr := payment.NormalizePaymentCurrency(currency)
	if nerr != nil {
		// 缺省/非法币种回落 DefaultPaymentCurrency，不拦截（与既有 paymentProviderConfigCurrency 语义一致）。
		normalized = payment.DefaultPaymentCurrency
	}

	cfg, err := s.getPaymentConfigForValidations(ctx)
	if err != nil {
		return infraerrors.BadRequest(payment.FXRateMissingCode, "failed to load FX_RATES for currency validation")
	}
	if _, err := cfg.FXRates.Rate(normalized); err != nil {
		return infraerrors.BadRequest(payment.FXRateMissingCode,
			fmt.Sprintf("%s instance currency %s is not in FX_RATES — configure the FX rate first", providerKey, normalized)).
			WithMetadata(map[string]string{"provider": providerKey, "currency": normalized})
	}
	return nil
}

// getPaymentConfigForValidations 读取 FX_RATES 配置（供校验等只读场景）。
// settingRepo 不可用/未配置时按默认汇率表处理（与 parsePaymentConfig 的空值语义一致：
// 真正的 fail-closed 保护在 checkout 换算处，保存期校验只是前置 UX）。
func (s *PaymentConfigService) getPaymentConfigForValidations(ctx context.Context) (*PaymentConfig, error) {
	if s == nil || s.settingRepo == nil {
		return &PaymentConfig{FXRates: payment.DefaultFXRates()}, nil
	}
	vals, err := s.settingRepo.GetMultiple(ctx, []string{SettingFXRates})
	if err != nil {
		return nil, fmt.Errorf("get fx rates: %w", err)
	}
	cfg := &PaymentConfig{}
	if raw := vals[SettingFXRates]; raw != "" {
		parsed, perr := payment.ParseFXRates(raw)
		if perr != nil {
			cfg.FXRates = payment.DefaultFXRates()
		} else {
			cfg.FXRates = parsed
		}
	} else {
		cfg.FXRates = payment.DefaultFXRates()
	}
	return cfg, nil
}

// validateFxRatesForEnabledInstances 反向校验：保存 FX_RATES 时，所有已启用
// stripe/airwallex 实例的币种必须仍在表中。不满足返回错误，调用方拒绝保存。
func (s *PaymentConfigService) validateFxRatesForEnabledInstances(ctx context.Context, fxRates payment.FXRates) error {
	if s.entClient == nil {
		return nil
	}
	instances, err := s.entClient.PaymentProviderInstance.Query().
		Where(paymentproviderinstance.EnabledEQ(true)).All(ctx)
	if err != nil {
		return fmt.Errorf("query enabled provider instances: %w", err)
	}
	for _, inst := range instances {
		if inst.ProviderKey != payment.TypeStripe && inst.ProviderKey != payment.TypeAirwallex {
			continue
		}
		cfg, cerr := s.decryptConfig(inst.Config)
		if cerr != nil || cfg == nil {
			cfg = map[string]string{}
		}
		currency := paymentProviderConfigCurrency(inst.ProviderKey, cfg)
		normalized, nerr := payment.NormalizePaymentCurrency(currency)
		if nerr != nil {
			normalized = payment.DefaultPaymentCurrency
		}
		if _, err := fxRates.Rate(normalized); err != nil {
			return infraerrors.BadRequest(payment.FXRateMissingCode,
				fmt.Sprintf("enabled %s instance %q uses currency %s, which is not in FX_RATES", inst.ProviderKey, inst.Name, normalized)).
				WithMetadata(map[string]string{"provider": inst.ProviderKey, "currency": normalized})
		}
	}
	return nil
}
