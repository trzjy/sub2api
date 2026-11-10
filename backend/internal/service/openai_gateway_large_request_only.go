package service

import "context"

// LargeRequestOnlyExtraKey 是「仅接大请求」账号级开关（extra 键名）。
// 开启该开关的账号从小请求候选池中排除，只参与估算 input 超过阈值的
// 大请求候选。
const LargeRequestOnlyExtraKey = "large_request_only"

// LargeRequestOnlyInputThreshold 是「小请求」与「大请求」的估算 input 分界阈值
// （单位 tokens）。估算 input <= 本阈值判为「小请求」（含边界）；> 本阈值为「大请求」。
const LargeRequestOnlyInputThreshold = 256000

// IsLargeRequestOnlyAccount reports whether the account is configured to accept
// only large requests (estimated input above LargeRequestOnlyInputThreshold).
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

// LargeRequestOnlyExcludedIDs 列举该分组可调度候选中开启「仅接大请求」开关的账号 id 集合（派发单 LRO-01）。
//
// 语义：复用 OpenAIGatewayService.listSchedulableAccounts 的候选列举，返回
// IsLargeRequestOnlyAccount 命中的账号 id。仅供 handler 侧选号前小请求动态排除使用，
// 不改任何既有选号/调度函数签名与行为；列举候选失败返回 error，由调用方失败开放处理。
func (s *OpenAIGatewayService) LargeRequestOnlyExcludedIDs(ctx context.Context, groupID *int64, platform string) (map[int64]struct{}, error) {
	accounts, err := s.listSchedulableAccounts(ctx, groupID, platform)
	if err != nil {
		return nil, err
	}
	return largeRequestOnlyAccountIDs(accounts), nil
}
