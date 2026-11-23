package service

import "context"

// LargeRequestOnlyExtraKey 是「仅接大请求」账号级开关（extra 键名）。
// 开启该开关的账号从小请求候选池中排除，只参与估算 input 超过阈值的
// 大请求候选。
const LargeRequestOnlyExtraKey = "large_request_only"

// SmallRequestOnlyExtraKey 是「仅接小请求」账号级开关（extra 键名），与
// LargeRequestOnlyExtraKey 对称且互斥：开启的账号从大请求候选池中排除，
// 只参与被判为小请求的候选。大/小的分界阈值只有一个来源：设置键
// request_size_routing_threshold（存 settings 表）。系统不保留任何硬编码阈值常量：
// 兜底常量一旦与线上真实配置漂移，就会在配置读取失败的瞬间把分界悄悄改成另一个数，
// 让大请求被误判成小请求落进只接小请求的账号（11-12 事故形态）。
// 两个键同时为 true 属非法状态，由写入入口互斥保证；读取时按「大请求优先」
// 兜底，避免脏数据把账号从两个池子里同时排除而彻底失去候选资格。
const SmallRequestOnlyExtraKey = "small_request_only"

// RequestSizeScope 是三态请求规模归属。
type RequestSizeScope string

const (
	// RequestSizeScopeAny 不限制请求大小（两个开关键都不生效）。
	RequestSizeScopeAny RequestSizeScope = "any"
	// RequestSizeScopeLargeOnly 只接大请求，小请求排除该账号。
	RequestSizeScopeLargeOnly RequestSizeScope = "large_only"
	// RequestSizeScopeSmallOnly 只接小请求，大请求排除该账号。
	RequestSizeScopeSmallOnly RequestSizeScope = "small_only"
)

// ParseRequestSizeScope 解析外部传入的三态取值，未识别的取值返回 ok=false。
func ParseRequestSizeScope(raw string) (RequestSizeScope, bool) {
	switch RequestSizeScope(raw) {
	case RequestSizeScopeAny:
		return RequestSizeScopeAny, true
	case RequestSizeScopeLargeOnly:
		return RequestSizeScopeLargeOnly, true
	case RequestSizeScopeSmallOnly:
		return RequestSizeScopeSmallOnly, true
	default:
		return "", false
	}
}

// RequestSizeScopeOf 返回账号当前的三态归属。两个键同时为 true 时按「大请求优先」
// 兜底（见 SmallRequestOnlyExtraKey 注释）。
func RequestSizeScopeOf(a *Account) RequestSizeScope {
	if IsLargeRequestOnlyAccount(a) {
		return RequestSizeScopeLargeOnly
	}
	if IsSmallRequestOnlyAccount(a) {
		return RequestSizeScopeSmallOnly
	}
	return RequestSizeScopeAny
}

// IsSmallRequestOnlyAccount reports whether the account is configured to accept
// only small requests. 读取语义与 IsLargeRequestOnlyAccount 完全一致。
func IsSmallRequestOnlyAccount(a *Account) bool {
	asnap := a.SnapshotExtra()
	if a == nil || asnap == nil {
		return false
	}
	v, ok := asnap[SmallRequestOnlyExtraKey]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true" || b == "1"
	default:
		return false
	}
}

// RequestSizeRoutingThresholdUnknown 表示「当前没有可信的大/小请求分界阈值」。
//
// 阈值只有一个来源：设置键 request_size_routing_threshold（存 settings 表）。
// 系统不再保留任何硬编码阈值常量——兜底常量与线上真实配置一旦漂移，就会在配置
// 读取失败的瞬间把分界悄悄改成另一个数，让大请求被误判成小请求落进只接小请求的
// 账号（11-12 事故形态）。读不到阈值就是读不到，由调用方按「不做规模分流」处理。
const RequestSizeRoutingThresholdUnknown = 0

// IsLargeRequestOnlyAccount reports whether the account is configured to accept
// only large requests. 这里的大/小分界取运行时分流阈值（设置键
// request_size_routing_threshold），阈值不可用时不做规模分流。
//
// The flag is read from a.Extra[LargeRequestOnlyExtraKey] as a boolean. Missing,
// empty, non-bool, or false values — as well as a nil account or an account
// with a nil Extra map — return false. The read semantics are copied from
// IsIdentityMaskedAccount. This keeps the behavior a zero-impact no-op for
// every other platform account.
func IsLargeRequestOnlyAccount(a *Account) bool {
	asnap := a.SnapshotExtra()
	if a == nil || asnap == nil {
		return false
	}
	v, ok := asnap[LargeRequestOnlyExtraKey]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		// Tolerate JSON-ish string encodings of the flag.
		return b == "true" || b == "1"
	default:
		return false
	}
}

// largeRequestOnlyAccountIDs 返回 accounts 中开启「仅接大请求」开关的账号 id 集合。
func largeRequestOnlyAccountIDs(accounts []Account) map[int64]struct{} {
	excluded := make(map[int64]struct{})
	for i := range accounts {
		if IsLargeRequestOnlyAccount(&accounts[i]) {
			excluded[accounts[i].ID] = struct{}{}
		}
	}
	return excluded
}

// RequestSizeScopeExcludedIDs 一次性列举该分组可调度候选，按请求规模返回应当排除的账号 id 集合。
//
// 语义：estimated 判为大请求时返回「仅接小请求」账号；判为小请求时返回「仅接大请求」账号。
// 复用 listSchedulableAccounts 的候选列举，不改任何既有选号/调度函数签名与行为；
// 列举候选失败返回 error，由调用方失败开放处理。
func (s *OpenAIGatewayService) RequestSizeScopeExcludedIDs(ctx context.Context, groupID *int64, platform string, isLargeRequest bool) (map[int64]struct{}, error) {
	accounts, err := s.listSchedulableAccountsForSizeExclusion(ctx, groupID, platform)
	if err != nil {
		return nil, err
	}
	// 必须按 RequestSizeScopeOf 归一化后的取值判断，而不是直接读原始键：
	// 两个键同时为 true 的脏数据会被归一化为 large_only，若这里仍按 small 键排除，
	// 该账号会在大请求侧被排除、小请求侧又因 large 键被排除，两侧都选不到。
	want := RequestSizeScopeLargeOnly
	if isLargeRequest {
		want = RequestSizeScopeSmallOnly
	}
	excluded := make(map[int64]struct{})
	for i := range accounts {
		if RequestSizeScopeOf(&accounts[i]) == want {
			excluded[accounts[i].ID] = struct{}{}
		}
	}
	return excluded, nil
}
