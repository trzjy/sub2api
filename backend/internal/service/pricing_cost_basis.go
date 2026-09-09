package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PricingCostBasisPlan 单个订阅计划的成本核算基础数据。
// 数值为管理端人工维护（实测/官方文档），用于模型定价调整时的成本基准：
//   - 满月容量(tokens) = QuotaUnits / Weight_m
//   - 成本(¥/M tokens) = MonthlyFeeCNY × Weight_m / QuotaUnits × 1e6
type PricingCostBasisPlan struct {
	Provider         string             `json:"provider"`                      // 订阅来源，如 "火山引擎 Agent Plan (medium)"
	Plans            []string           `json:"plans"`                         // 覆盖的订阅（主体/账号），仅备注用
	MonthlyFeeCNY    float64            `json:"monthly_fee_cny"`               // 稳态月费（次月起）
	FirstMonthCNY    float64            `json:"first_month_cny"`               // 首月促销价（0 表示无）
	QuotaUnits       float64            `json:"quota_units"`                   // 满月配额（扣减单位）
	Window           string             `json:"window"`                        // 配额窗口说明（如 "monthly 锚定订阅日"）
	FX               float64            `json:"fx"`                            // 人民币兑美元汇率（保本倍率换算用）
	Weights          map[string]float64 `json:"weights"`                       // 每模型扣减权重（units / M tokens），绝对用量型订阅实测
	MeasuredCostPerM map[string]float64 `json:"measured_cost_per_m,omitempty"` // 每模型实测成本（¥/M tokens），百分比型订阅直接测得
	Accounts         []int64            `json:"accounts"`                      // 绑定账号（实测探针/突发用其凭据）
	Note             string             `json:"note,omitempty"`                // 实测方法/口径备注
	UpdatedAt        string             `json:"updated_at"`
}

// PricingCostBasis 全部订阅成本核算（按来源扩展）。
type PricingCostBasis struct {
	Plans []PricingCostBasisPlan `json:"plans"`
}

// getPricingCostBasis 解析 settings 中的成本核算数据（损坏时返回空并交由调用方兜底）。
func getPricingCostBasis(ctx context.Context, settingRepo SettingRepository) (*PricingCostBasis, error) {
	vals, err := settingRepo.GetMultiple(ctx, []string{SettingKeyPricingCostBasis})
	if err != nil {
		return nil, fmt.Errorf("get pricing cost basis: %w", err)
	}
	raw := vals[SettingKeyPricingCostBasis]
	out := &PricingCostBasis{Plans: []PricingCostBasisPlan{}}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return nil, fmt.Errorf("parse pricing cost basis: %w", err)
	}
	return out, nil
}

// GetPricingCostBasis 读取订阅成本核算数据。
func (s *SettingService) GetPricingCostBasis(ctx context.Context) (*PricingCostBasis, error) {
	return getPricingCostBasis(ctx, s.settingRepo)
}

// SavePricingCostBasis 保存订阅成本核算数据。
func (s *SettingService) SavePricingCostBasis(ctx context.Context, basis *PricingCostBasis) error {
	if basis == nil {
		basis = &PricingCostBasis{Plans: []PricingCostBasisPlan{}}
	}
	data, err := json.Marshal(basis)
	if err != nil {
		return fmt.Errorf("marshal pricing cost basis: %w", err)
	}
	return s.settingRepo.SetMultiple(ctx, map[string]string{
		SettingKeyPricingCostBasis: string(data),
	})
}
