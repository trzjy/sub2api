// Package model 定义服务层使用的数据模型。
package model

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// ErrorPassthroughRule 全局错误透传规则
// 用于控制上游错误如何返回给客户端
type ErrorPassthroughRule struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`             // 规则名称
	Enabled         bool      `json:"enabled"`          // 是否启用
	Priority        int       `json:"priority"`         // 优先级（数字越小优先级越高）
	ErrorCodes      []int     `json:"error_codes"`      // 匹配的错误码列表（OR关系）
	Keywords        []string  `json:"keywords"`         // 匹配的关键词列表（OR关系）
	MatchMode       string    `json:"match_mode"`       // "any"(任一条件) 或 "all"(所有条件)
	Platforms       []string  `json:"platforms"`        // 适用平台列表
	PassthroughCode bool      `json:"passthrough_code"` // 是否透传原始状态码
	ResponseCode    *int      `json:"response_code"`    // 自定义状态码（passthrough_code=false 时使用）
	PassthroughBody bool      `json:"passthrough_body"` // 是否透传原始错误信息
	CustomMessage   *string   `json:"custom_message"`   // 自定义错误信息（passthrough_body=false 时使用）
	SkipMonitoring  bool      `json:"skip_monitoring"`  // 是否跳过运维监控记录
	Description     *string   `json:"description"`      // 规则描述
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MatchModeAny 表示任一条件匹配即可
const MatchModeAny = "any"

// MatchModeAll 表示所有条件都必须匹配
const MatchModeAll = "all"

// w24 conflict-resolution fusion (second wave, B9 — semantic_conflict)
// 双方在 AllPlatforms() 上语义分叉：我方（b0-ours）在显式列表末位加 PlatformMuse；
// 上游（b0-upstream）把整个函数体换成 return domain.ConcretePlatformIDs()，由新文件
// domain/platforms.go 的 platformList 单一数据源派生，并在本文件的常量别名块追加
// PlatformOpenCodeGo / PlatformTypeSafe。
//
// keep OURS — 显式列表 + PlatformMuse 别名：
//   1. 权威表第 5 条的前提在本树不成立。domain/platforms.go 只存在于 b0-upstream
//      （git ls-tree b0-upstream -- backend/internal/domain/ 有 platforms.go、
//      platforms_test.go），本工作树 backend/internal/domain/ 下无该文件；
//      domain/constants.go（归 w04 卡）亦未登记 PlatformOpenCodeGo / PlatformTypeSafe /
//      PlatformCommandCode / PlatformCline。按权威表直接引用 ConcretePlatformIDs 与两个
//      新别名会编译失败（undefined: domain.ConcretePlatformIDs 等），故不取上游形态。
//   2. merge-plan §5 rule 1「我方有重写的符号 → 我方语义为准」：AllPlatforms() 被我方
//      重写并新增 muse，属自研能力，不得静默丢弃。muse 的落地已登记于
//      docs-local/muse-closure-registry.md（"错误透传平台集合：AllPlatforms() 均含
//      muse；测试全绿"），删除即破坏已验收结论。
//   3. 前端平台清单 frontend/src/constants/platforms.ts 的 CONCRETE_PLATFORM_OPTIONS
//      是本 fork 的同源 SSOT，含 other / codebuddy / muse 且不含上游那四个新平台；
//      若后端改走上游 platformList，错误透传平台集合会与前端选择器分叉
//      （丢 muse/codebuddy/other，多出四个本 fork 无账号、无分组、无 UI 项的平台），
//      违反方案 §1「不改自研功能语义、不新增用户未提功能」。
//   4. 消费者核对（merge-plan §5 rule 2）：全仓 grep AllPlatforms() 的非测试引用为 0，
//      本函数当前无生产消费者，仅 error_passthrough_rule_test.go 断言它。上游的
//      "单一数据源"重构对我方无既得收益，不值得为它引入编译不过的跨卡依赖。
//
// take UPSTREAM（暂缓，跨卡依赖）— ConcretePlatformIDs() 单一数据源 + opencode_go /
//   typesafe / command_code / cline：
//   上游把"所有平台"类列表统一到 domain/platforms.go 的 platformList（13 项，注意
//   platformList 未登记 muse，errata 在 user_platform_quota.go:47 已注明"其中 muse 是
//   fork 侧既有漂移"）。该重构属跨卡工程，本卡只动 error_passthrough_rule.go，
//   既不能凭空引入 platforms.go，也不该在此手写四个新平台的字符串字面量
//   （会绕过 platformList 的单一数据源，制造第二个真值）。
//
// 收敛条件（供 B11 集成批核对）：当 backend/internal/domain/platforms.go 与
//   domain/constants.go 的四个新平台常量入树，且 platformList 补登记 muse 后，
//   本函数可收敛为 return domain.ConcretePlatformIDs()，届时
//   error_passthrough_rule_test.go 的断言应同步扩为 14 项并集（现有 10 项追加
//   "opencode_go"、"typesafe"、"command_code"、"cline"）；若届时 platformList 仍不含
//   muse，则须先在 platforms.go 卡解决 muse 归属，不得让错误透传集合静默丢掉 muse。

// 支持的平台常量
const (
	PlatformAnthropic   = domain.PlatformAnthropic
	PlatformOpenAI      = domain.PlatformOpenAI
	PlatformGemini      = domain.PlatformGemini
	PlatformAntigravity = domain.PlatformAntigravity
	PlatformGrok        = domain.PlatformGrok
	PlatformKimi        = domain.PlatformKimi
	PlatformZhipu       = domain.PlatformZhipu
	PlatformDeepseek    = domain.PlatformDeepseek
	PlatformMiniMax     = domain.PlatformMiniMax
	PlatformMuse        = domain.PlatformMuse
)

// AllPlatforms 返回所有支持的平台列表。平台归并后（PR-4）网页接入挂在官方平台
// zhipu/deepseek/kimi 账号级 access_mode 上，web-* 平台值不再是错误透传规则目标。
// w24 注：本函数与前端 frontend/src/constants/platforms.ts 的
// CONCRETE_PLATFORM_OPTIONS 属同一份平台清单语义，改动需两侧同步。
func AllPlatforms() []string {
	return []string{
		PlatformAnthropic,
		PlatformOpenAI,
		PlatformGemini,
		PlatformAntigravity,
		PlatformGrok,
		PlatformKimi,
		PlatformZhipu,
		PlatformDeepseek,
		PlatformMiniMax,
		PlatformMuse,
	}
}

// Validate 验证规则配置的有效性
func (r *ErrorPassthroughRule) Validate() error {
	if r.Name == "" {
		return &ValidationError{Field: "name", Message: "name is required"}
	}
	if r.MatchMode != MatchModeAny && r.MatchMode != MatchModeAll {
		return &ValidationError{Field: "match_mode", Message: "match_mode must be 'any' or 'all'"}
	}
	// 至少需要配置一个匹配条件（错误码或关键词）
	if len(r.ErrorCodes) == 0 && len(r.Keywords) == 0 {
		return &ValidationError{Field: "conditions", Message: "at least one error_code or keyword is required"}
	}
	if !r.PassthroughCode && (r.ResponseCode == nil || *r.ResponseCode <= 0) {
		return &ValidationError{Field: "response_code", Message: "response_code is required when passthrough_code is false"}
	}
	if !r.PassthroughBody && (r.CustomMessage == nil || *r.CustomMessage == "") {
		return &ValidationError{Field: "custom_message", Message: "custom_message is required when passthrough_body is false"}
	}
	return nil
}

// ValidationError 表示验证错误
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Message
}
