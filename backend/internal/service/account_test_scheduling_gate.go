package service

import (
	"context"
	"fmt"
)

// evaluateAccountTestSchedulingGate 在发送真实测试请求之前评估账号是否具备
// 在分组调度中被选中的资格。它复用了生产调度（gateway_scheduling.go /
// scheduler_snapshot_service.go）对候选账号的几条硬性过滤规则：
//   - 账号状态必须是 active；
//   - 账号必须允许调度（schedulable=true）；
//   - Antigravity 账号必须开启混合调度（否则分组调度直接跳过）；
//   - 请求的模型必须在账号模型映射表（或平台白名单）内可解析；
//
// 任何一条不过，都会给出明确的中文失败原因并阻断测试，避免"测试假装成功"
// 却在生产分组调度里被 503 该分组暂无可用账号 的假象。
//
// 另有一条非阻断的调度提示（warning）：当账号未加入任何分组，或账号所属分组
// 全部启用了模型白名单且都拒绝了请求模型时，真实调用若经由分组将无法调度到此
// 账号。warning 仅用于提示，不影响测试是否通过。
//
// 返回值：warnings 为提示信息列表（可空），blockReason 为非空时表示阻断并给出原因。
func evaluateAccountTestSchedulingGate(ctx context.Context, account *Account, requestedModel string) (warnings []string, blockReason string) {
	if account == nil {
		return nil, "账号为空，无法执行调度资格预检"
	}

	// 1. 账号状态必须是 active，否则在分组调度中会被直接跳过。
	if account.Status != StatusActive {
		return nil, fmt.Sprintf("账号状态非 active (当前状态: %s)，在分组调度中会被直接跳过", account.Status)
	}

	// 2. 账号必须允许调度。
	if !account.Schedulable {
		return nil, "账号调度已禁用 (schedulable=false)，在分组调度中会被直接跳过"
	}

	// 3. Antigravity 必须开启混合调度。
	if account.Platform == PlatformAntigravity && !account.IsMixedSchedulingEnabled() {
		return nil, "混合调度未开启：该账号在分组调度中会被直接跳过，真实调用会返回 503 该分组暂无可用账号"
	}

	// 4. 模型映射与支持性检查。
	if requestedModel != "" {
		if account.Platform == PlatformAntigravity {
			normalizedModel := normalizeClaudeCodeLongContextModel(requestedModel)
			mapped := mapAntigravityModel(account, normalizedModel)
			if mapped == "" {
				return nil, fmt.Sprintf("模型 %s 未在账号模型映射表中配置或不属于 Antigravity 白名单", requestedModel)
			}
		} else if len(account.GetModelMapping()) > 0 && !account.IsModelSupported(requestedModel) {
			return nil, fmt.Sprintf("模型 %s 未在账号模型映射表中配置", requestedModel)
		}
	}

	// 5. 分组模型白名单提示（warning，不阻断）。
	// 5a. 账号未加入任何分组：仅支持直接指定账号调用，无法参与分组调度。
	if len(account.Groups) == 0 && len(account.GroupIDs) == 0 {
		warnings = append(warnings, "[调度提示] 账号未加入任何分组，仅支持直接指定账号调用，无法参与分组调度")
	}

	// 5b. 有关联分组且指定了模型：若所有启用白名单的分组都拒绝该模型，给出提示。
	if len(account.Groups) > 0 && requestedModel != "" {
		hasEnabledAllowlist := false
		allReject := true
		for _, g := range account.Groups {
			if g == nil {
				continue
			}
			if !g.ModelAllowlist.Enabled {
				continue
			}
			hasEnabledAllowlist = true
			if g.ModelAllowlist.Allows(requestedModel) {
				allReject = false
				break
			}
		}
		if hasEnabledAllowlist && allReject {
			warnings = append(warnings, fmt.Sprintf("[调度提示] 模型 %s 不在账号所属任何分组的模型白名单内，真实调用若通过现有分组将无法调度到此账号", requestedModel))
		}
	}

	return warnings, ""
}
