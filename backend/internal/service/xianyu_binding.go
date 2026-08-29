package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// autoBindProduct 对新商品按固定顺序尝试自动绑定。
// 顺序：已存在精确映射（不覆盖手工映射）→ 关键词规则 → 账号默认池规则。
// 只有 unmapped 商品才会被自动绑定；mapped 商品不受自动绑定影响。
func autoBindProduct(ctx context.Context, control XianyuControlRepository, product XianyuProduct, rules []XianyuBindingRule) error {
	if control == nil {
		return fmt.Errorf("auto bind: control repository is nil")
	}
	if product.BindingStatus == XianyuBindingStatusMapped {
		// 已绑定商品不被自动覆盖。
		return nil
	}

	active := make([]XianyuBindingRule, 0, len(rules))
	for _, rule := range rules {
		if strings.TrimSpace(rule.Status) != "active" || rule.AccountPK != product.AccountPK {
			continue
		}
		active = append(active, rule)
	}
	// priority 升序，同优先级按 id 升序。
	sort.SliceStable(active, func(i, j int) bool {
		if active[i].Priority != active[j].Priority {
			return active[i].Priority < active[j].Priority
		}
		return active[i].ID < active[j].ID
	})

	var matchedPoolID *int64
	var matchedSource string

	title := normalizeKeyword(product.Title)
	specName := normalizeKeyword(product.SpecName)
	specValue := normalizeKeyword(product.SpecValue)

	for _, rule := range active {
		switch rule.MatchType {
		case XianyuBindingRuleKeyword:
			kw := normalizeKeyword(rule.Keyword)
			if kw == "" {
				continue
			}
			if strings.Contains(title, kw) || strings.Contains(specName, kw) || strings.Contains(specValue, kw) {
				matchedPoolID = &rule.PoolID
				matchedSource = XianyuBindingSourceKeyword
				break
			}
		case XianyuBindingRuleAccountDefault:
			matchedPoolID = &rule.PoolID
			matchedSource = XianyuBindingSourceAccountDefault
			break
		}
		if matchedPoolID != nil {
			break
		}
	}

	if matchedPoolID != nil {
		return control.UpdateProductBinding(ctx, product.ID, XianyuBindingStatusMapped, matchedSource, matchedPoolID)
	}
	return nil
}