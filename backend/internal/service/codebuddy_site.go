package service

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
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

// CodeBuddySiteEgressHosts 返回某站点所有出站请求的域名集合（去重）。
// 直连覆盖据此判断"目标 host 是否属于 CodeBuddy 站点"，从而决定是否重定向 TCP。
func CodeBuddySiteEgressHosts(site string) []string {
	ep := codeBuddyEndpointsFor(site)
	bases := []string{ep.UpstreamBase, ep.BillingBase, ep.ModelsBase, ep.OriginReferer}
	seen := make(map[string]struct{}, len(bases))
	out := make([]string, 0, len(bases))
	for _, b := range bases {
		u, err := url.Parse(b)
		if err != nil || u.Host == "" {
			continue
		}
		host := u.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

// CodeBuddyDirectOriginActive 返回配置中是否存在任一启用且字段齐全的站点覆盖。
func CodeBuddyDirectOriginActive(direct map[string]config.CodeBuddyDirectOriginConfig) bool {
	for _, oc := range direct {
		if oc.Enabled && strings.TrimSpace(oc.IP) != "" && strings.TrimSpace(oc.Host) != "" {
			return true
		}
	}
	return false
}

// CodeBuddyDirectOriginResolve 判断 host 是否命中某启用站点的直连覆盖，命中返回
// "ip:port" 地址。命中条件：host 属于该站点出站域名集合，或命中该站点配置的 Host。
func CodeBuddyDirectOriginResolve(host string, direct map[string]config.CodeBuddyDirectOriginConfig) (string, bool) {
	for site, oc := range direct {
		if !oc.Enabled || strings.TrimSpace(oc.IP) == "" || strings.TrimSpace(oc.Host) == "" {
			continue
		}
		port := oc.Port
		if port == 0 {
			port = 443
		}
		addr := net.JoinHostPort(strings.TrimSpace(oc.IP), strconv.Itoa(port))
		for _, h := range CodeBuddySiteEgressHosts(site) {
			if strings.EqualFold(h, host) {
				return addr, true
			}
		}
		if strings.EqualFold(strings.TrimSpace(oc.Host), host) {
			return addr, true
		}
	}
	return "", false
}

// WrapDirectOriginDialContext 包一层直连重定向拨号器：仅当目标 host 命中 CodeBuddy
// 直连覆盖时，把 TCP 连接重定向到钉点 "ip:port"；其余 host 原样交给 base 拨号，
// 行为逐字节不变。SNI 与 Host 头由调用方（Go 默认 TLS / 请求）按原域名完成，
// 故重定向只改变 TCP 目的地址，不改变任何 TLS/HTTP 字节——这是"默认关闭=零行为变化"
// 与"覆盖所有 CodeBuddy 出站"能在 Transport 层一处实现的关键。
func WrapDirectOriginDialContext(
	base func(context.Context, string, string) (net.Conn, error),
	direct map[string]config.CodeBuddyDirectOriginConfig,
) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host := addr
		if h, _, err := net.SplitHostPort(addr); err == nil {
			host = h
		}
		if target, ok := CodeBuddyDirectOriginResolve(host, direct); ok {
			return base(ctx, network, target)
		}
		return base(ctx, network, addr)
	}
}
