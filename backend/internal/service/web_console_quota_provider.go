package service

import (
	"net/url"
	"strings"
)

// Web 控制台额度采集的上游标识与停调来源常量。
//
// 唯一事实源：按账号 base_url 主机名识别 3 家无公开用量 API 的上游，与
// platform / api_key 完全解耦（与 accountIsKiraBaseURL 同构）。本文件只负责
// 「识别」，不持有任何采集逻辑；采集写入的 source 与快照键一并在此声明，供
// 停调所属权校验与展示复用。
const (
	// providerCompshare 优云智算（cp.compshare.cn 推理网关 + console.compshare.cn 控制台）
	providerCompshare = "compshare"
	// providerSensenova 商汤日日新（token.sensenova.cn 推理 + platform.sensenova.cn 控制台）
	providerSensenova = "sensenova"
	// providerLongcat 美团 LongCat（api.longcat.chat 推理 + longcat.chat 控制台）
	providerLongcat = "longcat"
	// providerIflytek 讯飞星辰 MaaS（推理 maas-api.cn-huabei-1.xf-yun.com、
	// maas-coding-api.cn-huabei-1.xf-yun.com；控制台 maas.xfyun.cn）
	providerIflytek = "iflytek"

	// webConsoleQuotaOwnedSource 是本链路写入 temp_unschedulable_reason 的 source 标识，
	// 用于停调所属权校验（仅该来源可被本链路覆盖或清除）。
	webConsoleQuotaOwnedSource = "web_console_quota"
)

// webConsoleQuotaSnapshotExtraKey 账号 extra 中存放统一用量快照的键。
const webConsoleQuotaSnapshotExtraKey = "web_console_quota_snapshot"

// webConsoleQuotaProviderRoot 是「根域 → 上游 provider」的映射项。
type webConsoleQuotaProviderRoot struct {
	root     string // 小写根域，匹配时不区分大小写
	provider string // 命中返回的上游标识
}

// webConsoleQuotaProviderRoots 是四家的根域映射，按 base_url 主机名后缀匹配。
//   - compshare：cp.compshare.cn / compshare.cn 同属优云智算（推理网关 + 控制台）
//   - sensenova：sensenova.cn 含 token.sensenova.cn、platform.sensenova.cn
//   - longcat：longcat.chat 含 api.longcat.chat
//   - iflytek：xf-yun.com 覆盖讯飞星辰 MaaS 推理网关（maas-api.*.xf-yun.com）
//     与编码网关（maas-coding-api.*.xf-yun.com）；控制台 maas.xfyun.cn 同属此根域。
//     注意 xfyun.cn（开放平台域）与 xf-yun.com 不同根，不会被误命中。
var webConsoleQuotaProviderRoots = []webConsoleQuotaProviderRoot{
	{"cp.compshare.cn", providerCompshare},
	{"compshare.cn", providerCompshare},
	{"sensenova.cn", providerSensenova},
	{"longcat.chat", providerLongcat},
	{"xf-yun.com", providerIflytek},
}

// webConsoleHostMatches 报告 host 是否精确等于根域 root，或以 ".root" 结尾。
// 大小写不敏感；采用后缀匹配而非子串包含，防 xxx.compshare.cn.evil.com 误判。
func webConsoleHostMatches(host, root string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == root || strings.HasSuffix(h, "."+root)
}

// ResolveWebConsoleQuotaProvider 按账号 base_url 主机名判定是否属 Web 控制台额度上游。
// 取 GetOpenAIBaseURL() 优先、GetBaseURL() 兜底（与 accountIsKiraBaseURL 同构）；
// 主机名大小写不敏感，采用「等于根域 或 以 .根域 结尾」的后缀匹配，防 xxx.compshare.cn.evil.com 误判。
// 返回 "" 表示不属本链路。
func ResolveWebConsoleQuotaProvider(account *Account) string {
	if account == nil {
		return ""
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = account.GetBaseURL()
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return ""
	}
	host := parsed.Hostname()
	for _, r := range webConsoleQuotaProviderRoots {
		if webConsoleHostMatches(host, r.root) {
			return r.provider
		}
	}
	return ""
}

// ResolveWebConsoleQuotaLoginModes 返回该账号控制台上游实际支持的登录方式
// （按优先推荐顺序）。返回空切片表示非本链路或不支持任何交互登录。
//
// 唯一事实源：webConsoleQuotaProviderEndpointTable[provider].loginModes。
// 适配器与管理端从此处取，禁止各自按 provider 名字再写一份判断。
func ResolveWebConsoleQuotaLoginModes(account *Account) []string {
	provider := ResolveWebConsoleQuotaProvider(account)
	if provider == "" {
		return nil
	}
	endpoints, ok := webConsoleQuotaProviderEndpointTable[provider]
	if !ok || len(endpoints.loginModes) == 0 {
		return nil
	}
	out := make([]string, len(endpoints.loginModes))
	copy(out, endpoints.loginModes)
	return out
}
