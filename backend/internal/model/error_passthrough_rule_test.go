package model

// w25 conflict-resolution fusion tests (second wave, B9 — semantic_conflict)
// 双方在同一个断言列表里各加平台项：我方（b0-ours）加 muse，上游（b0-upstream）把
// 末项换成 opencode_go / typesafe / command_code / cline 四个新平台。裁决"断言必须与
// AllPlatforms() 实现返回值严格对齐"，故本断言列表取实现当前实际返回的集合。
//
// keep OURS — muse：error_passthrough_rule.go 的 AllPlatforms() 逐项 return 显式列表，
//   末位为 PlatformMuse（domain/constants.go:36 已定义，= "muse"），且 Muse 平台方案已
//   批准下游全协议支持（见 handler/openai_gateway_cn_dispatch_test.go）。删除会让实现与
//   断言失配，故保留。
//
// take UPSTREAM（暂缓，跨卡依赖）— opencode_go / typesafe / command_code / cline：上游把
//   AllPlatforms() 改为 return domain.ConcretePlatformIDs()，由新文件 domain/platforms.go
//   的 platformList 单一数据源派生（权威表第 5 条：platforms.go 系 = upstream 新符号，
//   直接引用）。该定义与四个常量（domain.PlatformOpenCodeGo / PlatformTypeSafe /
//   PlatformCommandCode / PlatformCline）当前均未入本工作树（backend/internal/domain/
//   下无 platforms.go，constants.go 亦无这四个常量），且实现文件 error_passthrough_rule.go
//   归 w24 卡，不在本卡文件清单内（现场纪律：只动派发单文件清单）。故此处不照抄上游断言
//   列表，否则测试必然失败。
//   与 user_platform_quota.go 的先例一致：该处同样刻意保留 ours 的显式 switch，不换成
//   domain.IsConcretePlatform 注册表校验，理由同为"逐消费者复刻 ours 行为"。
//
// 收敛条件（供 B11 集成批核对）：当 w24 落地 AllPlatforms() = domain.ConcretePlatformIDs()
//   且 domain/platforms.go 入树后，本断言应扩充为 14 项并集 —— 在现有 10 项基础上追加
//   "opencode_go"、"typesafe"、"command_code"、"cline"；届时 muse 是否仍属具体平台清单，
//   取决于上游 platformList 未登记 muse 这一事实如何与我方既有 muse 账号/分组数据共存。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAllPlatformsIncludesEveryConcretePlatform(t *testing.T) {
	require.ElementsMatch(t, []string{
		"anthropic",
		"openai",
		"gemini",
		"antigravity",
		"grok",
		"kimi",
		"zhipu",
		"deepseek",
		"minimax",
		"muse",
	}, AllPlatforms())
}
