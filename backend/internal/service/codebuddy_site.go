package service

import (
	"strings"
)

// CodeBuddy 站点标识。站点是账号级属性（credentials["site"]），缺省 = cn：
// 存量账号无 site 键，必须与改造前行为完全一致（零迁移、零行为变化）。
const (
	CodeBuddySiteCN   = "cn"
	CodeBuddySiteIntl = "intl"
)

// CodeBuddySiteEndpoints 是单个站点的出站 URL 表（本仓库唯一新增的站点常量结构）。
//
// 所有 CodeBuddy 出站代码（OAuth/poll/refresh/chat/models/billing）都必须经由
// codeBuddyEndpointsFor(site) 选表，禁止在调用点散落默认域名。
type CodeBuddySiteEndpoints struct {
	// UpstreamBase 承载 auth/state、auth/token、login/account、chat/completions。
	UpstreamBase string
	// OriginReferer 是出站 Origin/Referer 的站点域（Referer 追加 "/"）。
	OriginReferer string
	// BillingBase 承载 /v2/billing/meter/*。
	BillingBase string
	// ModelsBase 承载 /console/enterprises/personal/models。
	//
	// 注意 intl：Phase 0 实测该端点认证后返回 HTTP 500（动态模型列表不可用），
	// 见 docs/evidence/codebuddy-intl/intl-calibration-report.md D4。
	ModelsBase string
}

// codeBuddySiteEndpoints CN 上游 base 复用既有导出常量（保留兼容），其余站点值集中于此。
var codeBuddySiteEndpoints = map[string]CodeBuddySiteEndpoints{
	CodeBuddySiteCN: {
		UpstreamBase:  DefaultCodeBuddyBaseURL,
		OriginReferer: "https://www.codebuddy.cn",
		BillingBase:   "https://www.codebuddy.cn",
		ModelsBase:    DefaultCodeBuddyBaseURL,
	},
	CodeBuddySiteIntl: {
		UpstreamBase:  "https://www.codebuddy.ai",
		OriginReferer: "https://www.codebuddy.ai",
		BillingBase:   "https://www.codebuddy.ai",
		ModelsBase:    "https://www.codebuddy.ai",
	},
}

// NormalizeCodeBuddySite 归一化站点标识：仅显式 intl 视为国际版，其余（含空/未知）
// 一律回落 cn。所有读取站点的地方都必须经过此函数，不得各自判空。
func NormalizeCodeBuddySite(site string) string {
	if strings.EqualFold(strings.TrimSpace(site), CodeBuddySiteIntl) {
		return CodeBuddySiteIntl
	}
	return CodeBuddySiteCN
}

// codeBuddyEndpointsFor 返回站点对应的 URL 表（未知站点回落 cn）。
func codeBuddyEndpointsFor(site string) CodeBuddySiteEndpoints {
	return codeBuddySiteEndpoints[NormalizeCodeBuddySite(site)]
}

// CodeBuddySite 返回账号的 CodeBuddy 站点（credentials["site"]），缺省 cn。
// 存量账号无 site 键时恒返回 cn，行为与改造前一致。
func (a *Account) CodeBuddySite() string {
	if a == nil {
		return CodeBuddySiteCN
	}
	return NormalizeCodeBuddySite(a.GetCredential("site"))
}
