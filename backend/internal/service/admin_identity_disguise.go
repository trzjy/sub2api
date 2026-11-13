package service

import (
	"context"
	"fmt"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 伪装配置（identity disguise）的管理接口实现。
//
// 写入契约（唯一口径，见 docs-local 方案 §2.2.1）：
//   - **只写新键 extra.identity_disguise**，不写、不删任何老键；
//   - 走 UpdateAccountExtra（JSONB key 级合并），天然不碰其它 extra 键；
//   - 老键（identity_prompt / mask_upstream_identity / response_identity_mask /
//     strip_reasoning_content）保持原值，读侧归一时回退使用 → 分层路由档位归属零变化。

// IdentityDisguiseUpdate 是伪装配置的写入载荷。
type IdentityDisguiseUpdate struct {
	// Profile 是目标身份（none = 不伪装）。
	Profile IdentityDisguiseProfileID
	// ForbiddenNames 是禁词列表；nil 表示「按账号 model_mapping 自动生成」。
	ForbiddenNames []string
	// AutoDeriveForbidden 为真时按 model_mapping 自动推导禁词（覆盖 ForbiddenNames）。
	AutoDeriveForbidden bool
	// RewriteModel 表示是否把上游回显的模型名改写回客户端请求名（L1）。
	RewriteModel bool
	// StripReasoning 表示是否剥离客户端可见的思考内容（L3）。
	StripReasoning bool
}

// IdentityDisguiseState 是伪装配置的读取结果（供管理接口回显）。
type IdentityDisguiseState struct {
	// Profile 是生效的目标身份。
	Profile IdentityDisguiseProfileID
	// ForbiddenNames 是当前生效的禁词列表。
	ForbiddenNames []string
	// RewriteModel / StripReasoning 是两个伪装面的开关。
	RewriteModel   bool
	StripReasoning bool
	// AutoDeriveForbidden 报告禁词列表是否由账号 model_mapping 自动推导。
	// 该开关随配置落库并原样回显 —— 列表本身无法反推它。
	AutoDeriveForbidden bool
	// Prompt 是当前生效的人设正文（供运营核对）。
	Prompt string
	// UsesLegacyKeys 报告该账号是否仍在用老键（迁移未完成 / 无权威正文可迁）。
	UsesLegacyKeys bool
}

// SetAccountIdentityDisguise 设置账号的伪装配置。
//
// 只写 extra.identity_disguise 一个键（走 UpdateAccountExtra 的 JSONB key 级合并）。
func (s *adminServiceImpl) SetAccountIdentityDisguise(ctx context.Context, id int64, update IdentityDisguiseUpdate) (*IdentityDisguiseState, error) {
	if err := ValidateIdentityDisguiseProfile(update.Profile); err != nil {
		return nil, infraerrors.BadRequest("IDENTITY_DISGUISE_INVALID_PROFILE", err.Error())
	}

	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	names := update.ForbiddenNames
	if update.AutoDeriveForbidden {
		names = DeriveIdentityDisguiseForbiddenNames(account.GetModelMapping())
	}
	names = sanitizeForbiddenNames(names)

	payload := map[string]any{
		"profile":                  string(update.Profile),
		"forbidden_names":          toAnySlice(names),
		"auto_derive_forbidden_names": update.AutoDeriveForbidden,
		"rewrite_model":            update.RewriteModel,
		"strip_reasoning":          update.StripReasoning,
	}
	if err := s.accountRepo.UpdateExtra(ctx, id, map[string]any{
		IdentityDisguiseExtraKey: payload,
	}); err != nil {
		return nil, err
	}

	updated, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return buildIdentityDisguiseState(updated), nil
}

// GetAccountIdentityDisguise 读取账号的伪装配置（供管理接口回显）。
func (s *adminServiceImpl) GetAccountIdentityDisguise(ctx context.Context, id int64) (*IdentityDisguiseState, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return buildIdentityDisguiseState(account), nil
}

// buildIdentityDisguiseState 组装伪装配置的读取结果。
func buildIdentityDisguiseState(account *Account) *IdentityDisguiseState {
	state := &IdentityDisguiseState{Profile: IdentityDisguiseProfileNone}
	if account == nil {
		return state
	}
	extra := account.SnapshotExtra()
	d, isNew := parseIdentityDisguiseExtra(extra)
	if !isNew {
		// 老键分支：先用既有的 legacyIdentityDisguiseOf 复用老键解析（不另立键名清单）。
		// 存在性由 legacyIdentityDisguiseOf 单一来源给出（d.HasLegacyKeys）：
		// 只要账号 extra 中确实带着某个老键（哪怕值为空串 / false），就报「使用旧配置」，
		// 否则既无新键、也无老键的普通账号会被误报为「正在使用旧配置」。
		d = legacyIdentityDisguiseOf(account)
		state.UsesLegacyKeys = d.HasLegacyKeys
		if !d.HasLegacyKeys {
			// 一个老键都没有 → 视为未配置：Profile 保持 none，其余字段取零值。
			d = IdentityDisguise{Profile: IdentityDisguiseProfileNone}
		}
	}
	state.Profile = d.Profile
	state.ForbiddenNames = d.ForbiddenNames
	state.RewriteModel = d.RewriteModel
	state.StripReasoning = d.StripReasoning
	state.AutoDeriveForbidden = d.AutoDeriveForbidden
	prompt, _, _ := AccountIdentityDisguiseEffective(account)
	state.Prompt = prompt
	return state
}

// toAnySlice 把字符串切片转成 []any（供 JSONB 序列化）。
func toAnySlice(items []string) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, it)
	}
	return out
}

// ValidateIdentityDisguiseUpdate 校验写入载荷（handler 层用，给出可读错误）。
func ValidateIdentityDisguiseUpdate(update IdentityDisguiseUpdate) error {
	if err := ValidateIdentityDisguiseProfile(update.Profile); err != nil {
		return infraerrors.BadRequest("IDENTITY_DISGUISE_INVALID_PROFILE", err.Error())
	}
	for _, n := range update.ForbiddenNames {
		trimmed := strings.TrimSpace(n)
		if trimmed == "" {
			continue
		}
		if len(trimmed) > IdentityDisguiseForbiddenNameMaxLen {
			return infraerrors.BadRequest("IDENTITY_DISGUISE_INVALID_FORBIDDEN_NAME",
				fmt.Sprintf("forbidden name too long (max %d bytes): %s", IdentityDisguiseForbiddenNameMaxLen, trimmed))
		}
		if strings.ContainsAny(trimmed, "\n\r\t") {
			return infraerrors.BadRequest("IDENTITY_DISGUISE_INVALID_FORBIDDEN_NAME",
				fmt.Sprintf("forbidden name must not contain control characters: %s", trimmed))
		}
	}
	return nil
}
