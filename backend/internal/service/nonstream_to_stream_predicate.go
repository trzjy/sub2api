package service

import (
	"net/url"
	"strings"
)

// nonstreamToStreamOfficialCNHosts 是「国产供应商官方直连」判定用的官方 host 集合。
// 仅由 domain_constants.go 既有默认 base_url 常量（init 时解析取 host）派生，
// 外加 MiniMax 国际站 api.minimax.io（官方 FAQ 指定国际站推理域名，cn_provider_quota_service.go:710）。
// 键统一为小写：下方常量字面量的 host 均为小写，isOfficialCNUpstreamBaseURL 比对前也
// 将 host 小写化（strings.ToLower），两侧同态避免大小写变体漏判。
// 严禁在此硬编码第二张官方域字面量表：任何新增官方域都必须先落入 domain_constants.go
// 的默认 base_url 常量，再经本 init 派生。
var nonstreamToStreamOfficialCNHosts map[string]struct{}

func init() {
	official := []string{
		DefaultKimiPayGBaseURL,
		DefaultKimiCodingBaseURL,
		DefaultZhipuPayGBaseURL,
		DefaultZhipuCodingBaseURL,
		DefaultDeepseekBaseURL,
		DefaultMiniMaxBaseURL,
		"https://api.minimax.io", // MiniMax 国际站官方推理域名
	}
	nonstreamToStreamOfficialCNHosts = make(map[string]struct{}, len(official))
	for _, raw := range official {
		u, err := url.Parse(raw)
		if err != nil {
			// 常量均为编译期已知合法 URL，解析失败属编程错误，直接跳过该条。
			continue
		}
		if host := u.Hostname(); host != "" {
			nonstreamToStreamOfficialCNHosts[host] = struct{}{}
		}
	}
}

// shouldConvertNonstreamToStream 报告该账号的非流式上游请求是否转换流式。
// 两类适用对象：
//   - CodeBuddy：上游硬性只接受流式（出站改写 §2.5 规则 1 无条件置 stream=true，
//     codebuddy_upstream.go:73），非流式必须在本地聚合。
//   - 非官方上游（池类/第三方中转，生产挂死集中地）：官方直连保持原行为。
func (s *OpenAIGatewayService) shouldConvertNonstreamToStream(account *Account) bool {
	if s.cfg == nil || s.cfg.Gateway.NonstreamToStreamDisabled {
		return false
	}
	// CodeBuddy 分支（上游强制流式），必须位于下方 AccountTypeAPIKey 类型门槛之前：
	// codebuddy 账号以 OAuth 为主（生产 group 109 的直绑账号即 OAuth），若放在门槛
	// 之后则永远进不来——非流式请求会以非流式预期去解析上游的 SSE 流，在
	// bufferChatCompletionsAsAnthropic/readCCUpstreamJSONResponse 里对 "data: {...}"
	// 跑 json.Unmarshal，报 "invalid character 'd' looking for beginning of value"
	// 并回 502（生产实证 2026-10-16，request_id d8f59d51-407e-4b74-b71b-dc173fc1b67e）。
	//
	// 边界说明：分支只按 platform 收敛（account.IsCodeBuddy()），不放宽类型门槛，
	// 因此非 codebuddy 的 OAuth/upstream 类型账号仍被下方门槛原样挡住——CN/other
	// OAuth 账号行为与本分支存在与否逐字节一致。判定不看 base_url：codebuddy 上游
	// 由站点表决定（account.go:1430 CodeBuddySite），不存在「官方直连可放行非流式」
	// 的概念，故不适用下游的 isOfficialCNUpstreamBaseURL 排除。
	if account.IsCodeBuddy() {
		return true
	}
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	// 网页逆向账号显式排除：IsWebAccessMode 是独立判定源
	//（account.go:1481，GetAccessMode()==web）。
	if account.IsWebAccessMode() {
		return false
	}
	// 官方直连排除：经 GetOpenAIBaseURL（account.go:1377，forwarder 构建上游
	// 请求用的同一解析函数，含 adaptive api_base_urls 与凭证 base_url 覆盖链）
	// 解析生效上游，比对官方默认域（domain_constants.go:84-91 常量 + minimax
	// 国际站 api.minimax.io，cn_provider_quota_service.go:710），命中即不转换。
	if account.IsCNProvider() && isOfficialCNUpstreamBaseURL(account) {
		return false
	}
	return account.IsCNProvider() || account.Platform == PlatformOther
}

// isOfficialCNUpstreamBaseURL 报告账号解析后的生效上游 base_url 是否落在国产供应商
// 官方默认域集合内（见 nonstreamToStreamOfficialCNHosts）。解析失败按非官方处理返回 false。
// platform=other 无官方默认端点（account.go:1414 GetOpenAIBaseURL 返回空），恒返回 false。
func isOfficialCNUpstreamBaseURL(account *Account) bool {
	if account == nil {
		return false
	}
	// 仅国产供应商有「官方默认端点」概念；other/openai/grok/codebuddy 等无官方
	// CN 端点，恒返回 false（即便其解析后的 base_url 恰好落在官方 host 集合内）。
	if !account.IsCNProvider() {
		return false
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		return false
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		// 解析失败按非官方处理：宁可转换（规避挂死）也不误判为官方直连放行。
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	_, ok := nonstreamToStreamOfficialCNHosts[host]
	return ok
}
