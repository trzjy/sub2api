package service

// AntigravityMixedSchedulingExtraKey 是账号 extra 里"混合调度"开关的键名。
// 与 account.go 的 IsMixedSchedulingEnabled 读取口径保持一致（只读 extra，不读
// credentials 里的历史字段 mixed_scheduling_enabled）。
const AntigravityMixedSchedulingExtraKey = "mixed_scheduling"

// applyAntigravityDefaultMixedScheduling 给新建的 Antigravity 账号补上混合调度默认值。
//
// 为什么必须默认开：mixed_scheduling 是聚合平台账号进入 gemini / claude 分组候选池
// 的硬门。gateway_scheduling 对 Antigravity 账号有
// `acc.Platform == PlatformAntigravity && !acc.IsMixedSchedulingEnabled() → continue`
// 的判定，开关关闭时账号即使 model_mapping 里配了模型也永远选不中，分组侧只表现为
// 「该分组暂无可用账号」，排查时极难定位。
//
// 调用方显式传 false 时保留其意图，不覆盖；非 Antigravity 平台原样返回。
func applyAntigravityDefaultMixedScheduling(platform string, extra map[string]any) map[string]any {
	if platform != PlatformAntigravity {
		return extra
	}
	normalized := shallowCopyMap(extra)
	if _, exists := normalized[AntigravityMixedSchedulingExtraKey]; exists {
		return normalized
	}
	normalized[AntigravityMixedSchedulingExtraKey] = true
	return normalized
}
