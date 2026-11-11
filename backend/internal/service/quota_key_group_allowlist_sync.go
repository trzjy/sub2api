package service

import (
	"context"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/ent"
	group "github.com/Wei-Shaw/sub2api/ent/group"
)

// appendModelsToBoundGroupAllowlists 在调用方事务内，把 modelMapping 的**源模型名**
// 追加进「该码已绑定且 model_allowlist.enabled = true」的分组的模型清单。
//
// 规则（逐条硬口径）：
//  1. 码级 group_ids 为 nil（未显式绑定）→ 直接返回，不猜测目标分组。
//  2. 候选 = modelMapping 的键（TrimSpace 后非空）；剔除 IsRetiredModelName 命中项；
//     剔除 DetectModelPlatform 判定不出家族（无法归组）的项。
//  3. 逐分组：deleted_at IS NULL 且 model_allowlist.enabled = true；
//     分组平台须满足 g.Platform == 模型家族 || g.Platform == PlatformComposite || g.Platform == PlatformCursor。
//  4. 分组白名单已 Allows(m) 命中（含大小写/别名族）→ 跳过，不重复追加。
//  5. 合并后经 normalizeGroupModelAllowlist 归一化再落库。
//  6. 只增不删：从不清除任何既有条目。
//
// 返回被实际修改的分组 ID（供调用方提交后失效认证缓存）。
func (s *QuotaKeyService) appendModelsToBoundGroupAllowlists(
	ctx context.Context, tx *ent.Tx, quotaKeyID int64, modelMapping map[string]string,
) ([]int64, error) {
	qk, err := tx.QuotaKey.Get(ctx, quotaKeyID)
	if err != nil {
		return nil, err
	}
	// 规则 1：未显式绑定分组（nil 或空切片）→ 直接返回，不猜测目标分组。
	if len(qk.GroupIds) == 0 {
		return nil, nil
	}

	groups, err := tx.Group.Query().
		Where(group.IDIn(qk.GroupIds...), group.DeletedAtIsNil()).
		All(ctx)
	if err != nil {
		return nil, err
	}

	// 规则 2：候选源模型名 = modelMapping 的键。TrimSpace 非空、非退役、能判定出家族。
	type candidate struct {
		name   string
		family string
	}
	candidates := make([]candidate, 0, len(modelMapping))
	for raw := range modelMapping {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if IsRetiredModelName(name) {
			continue
		}
		family, ok := DetectModelPlatform(name)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{name: name, family: family})
	}

	// 逐分组处理。
	modified := make([]int64, 0, len(groups))
	for _, g := range groups {
		// 规则 3：只处理已开启白名单的分组。
		if !g.ModelAllowlist.Enabled {
			continue
		}
		toAdd := make([]string, 0, len(candidates))
		for _, c := range candidates {
			// 规则 3：平台须匹配模型家族，或为 composite / cursor 聚合组。
			if g.Platform != c.family &&
				g.Platform != PlatformComposite &&
				g.Platform != PlatformCursor {
				continue
			}
			// 规则 4：分组白名单已命中（含大小写/别名族）→ 不重复追加。
			// Allows 是 service 层 GroupModelAllowlist 的方法，需从 domain 类型转换。
			if GroupModelAllowlistFromDomain(g.ModelAllowlist).Allows(c.name) {
				continue
			}
			toAdd = append(toAdd, c.name)
		}
		if len(toAdd) == 0 {
			continue
		}

		// 规则 5：合并既有 + 待追加，再归一化。只增不删（规则 6）。
		merged := make([]string, 0, len(g.ModelAllowlist.Models)+len(toAdd))
		merged = append(merged, g.ModelAllowlist.Models...)
		merged = append(merged, toAdd...)

		normalized, err := normalizeGroupModelAllowlist(GroupModelAllowlist{
			Enabled: true,
			Models:  merged,
		})
		if err != nil {
			// 不应发生（merged 非空且只增不删）；原样上抛，不静默跳过。
			return nil, err
		}

		if _, err := tx.Group.UpdateOneID(g.ID).
			SetModelAllowlist(DomainGroupModelAllowlist(normalized)).
			Save(ctx); err != nil {
			return nil, err
		}
		modified = append(modified, g.ID)
	}

	sort.Slice(modified, func(i, j int) bool { return modified[i] < modified[j] })
	return modified, nil
}
