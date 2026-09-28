// cockpit_platform_map.go —— Cockpit Tools 备份导出 slug → 站点平台归属三态表。
//
// 唯一权威来源：docs/codebuddy-cockpit-fusion-plan.md §1.2（v15）。
// 这是导入引擎的平台归属唯一映射源（domain 层常量表）；新增平台只改这张表。
//
// 三态：
//   enabled      已有真实导出 fixture，字段映射已核对，可导入。当前为空（codebuddy 系三平台待含账号 fixture 落盘后启用，§1.2 取证现状）。
//   pending      站点有对应平台，但尚无真实导出样本，字段映射未核对（预览显示"已识别 N 个账号，待字段映射核对，未导入"，不报错不计入失败）。
//   unsupported  站点无对应平台（含 Cockpit 未来新增的未知 slug，按 unsupported 处理并单独计数 unknown_platform_accounts）。
//
// 注意：本文件只录入三态表与判定函数，不实现任何字段映射器（codebuddy 系 pending，fixture 落盘前不合入映射器）。

package service

// CockpitPlatformState 是 slug 归属的三态。
type CockpitPlatformState int

const (
	// CockpitPlatformEnabled 已有真实 fixture，可导入。
	CockpitPlatformEnabled CockpitPlatformState = iota
	// CockpitPlatformPending 站点有对应平台但字段映射未核对，预览识别不导入。
	CockpitPlatformPending
	// CockpitPlatformUnsupported 站点无对应平台（含未知 slug）。
	CockpitPlatformUnsupported
)

// CockpitPlatformMapping 描述一个 slug 的归属。
type CockpitPlatformMapping struct {
	State        CockpitPlatformState
	SitePlatform string // enabled/pending 时对应的站点平台名；unsupported 为空
}

// cockpitSlugPlatformMap 是 §1.2 三态表的唯一录入点。
// 键为小写 slug（匹配时按小写精确匹配）。
//
// §1.2 原文摘录（逐行录入）：
//   pending:  codebuddy_cn / codebuddy / workbuddy → codebuddy(cn/intl/cn)
//             codex→openai、codex_api_service→openai、claude_manager→anthropic
//             antigravity(_ide)→antigravity、grok→grok
//   unsupported: kiro/cursor/windsurf/trae×4/qoder/zcode/zed/github-copilot
//   enabled: （当前为空）
var cockpitSlugPlatformMap = map[string]CockpitPlatformMapping{
	// —— pending（站点有对应平台，字段映射待核对）——
	"codebuddy_cn":      {CockpitPlatformPending, "codebuddy"},
	"codebuddy":         {CockpitPlatformPending, "codebuddy"},
	"workbuddy":         {CockpitPlatformPending, "codebuddy"},
	"codex":             {CockpitPlatformPending, "openai"},
	"codex_api_service": {CockpitPlatformPending, "openai"},
	"claude_manager":    {CockpitPlatformPending, "anthropic"},
	"antigravity":       {CockpitPlatformPending, "antigravity"},
	"antigravity_ide":   {CockpitPlatformPending, "antigravity"},
	"grok":              {CockpitPlatformPending, "grok"},

	// —— unsupported（站点无对应平台）——
	"zed":            {CockpitPlatformUnsupported, ""},
	"github-copilot": {CockpitPlatformUnsupported, ""},
	"windsurf":       {CockpitPlatformUnsupported, ""},
	"kiro":           {CockpitPlatformUnsupported, ""},
	"cursor":         {CockpitPlatformUnsupported, ""},
	"qoder":          {CockpitPlatformUnsupported, ""},
	"zcode":          {CockpitPlatformUnsupported, ""},
	"trae":           {CockpitPlatformUnsupported, ""},
	"trae_solo":      {CockpitPlatformUnsupported, ""},
	"trae_cn":        {CockpitPlatformUnsupported, ""},
	"trae_solo_cn":   {CockpitPlatformUnsupported, ""},
}

// cockpitAllSlugs 是 §1.1 取证的全量 slug 集合（20 个），用于校验三态表覆盖完整性。
var cockpitAllSlugs = []string{
	"claude_manager", "codex", "codex_api_service", "antigravity", "antigravity_ide",
	"zed", "github-copilot", "windsurf", "kiro", "cursor",
	"grok", "codebuddy", "codebuddy_cn", "qoder", "zcode",
	"trae", "trae_solo", "trae_cn", "trae_solo_cn", "workbuddy",
}

// ClassifyCockpitSlug 按小写精确匹配返回 slug 的归属。
// 返回 known=false 表示 Cockpit 新增的未知 slug（按 unsupported 处理，由调用方计入 unknown_platform_accounts）。
func ClassifyCockpitSlug(slug string) (CockpitPlatformMapping, bool) {
	norm := normalizeCockpitSlug(slug)
	m, ok := cockpitSlugPlatformMap[norm]
	if !ok {
		return CockpitPlatformMapping{State: CockpitPlatformUnsupported, SitePlatform: ""}, false
	}
	return m, true
}

// KnownCockpitSlugs 返回 §1.1 取证的全量 slug（小写），用于测试与完整性校验。
func KnownCockpitSlugs() []string {
	out := make([]string, len(cockpitAllSlugs))
	copy(out, cockpitAllSlugs)
	return out
}
