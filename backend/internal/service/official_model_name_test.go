//go:build unit

package service

// 官方模型名对齐的定向测试（2026-11-05 用户裁定：A 档）。
//
// 口径：
//  1. 老名字（deepseek-v4.1-flash）继续可调，老用户不掉线；
//  2. 新用户从 /v1/models 只能看到官方现行调用名 deepseek-flash；
//  3. 上游真名 -1017 依然对下游封闭（前置裁定不倒退）；
//  4. 回显按客户端请求名返回——老用户看到老名、新用户看到官方名，都不露上游真名。

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func gjsonGet(raw []byte, path string) string {
	return gjson.GetBytes(raw, path).String()
}

func TestPublicModelNameMapping(t *testing.T) {
	// 族内所有写法，对外展示名统一为官方现行调用名。
	for _, name := range []string{
		"deepseek-v4.1-flash",
		"deepseek-v4.1-flash-1017",
		"deepseek-v4-1-flash",
		"deepseek-flash",
		"DeepSeek-Flash",
	} {
		require.Equal(t, "deepseek-flash", PublicModelName(name), "name=%q", name)
	}

	// 族外名字不受影响（含 deepseek-v4-flash：官方 legacy 名，不是同一模型）。
	require.Equal(t, "deepseek-v4-pro", PublicModelName("deepseek-v4-pro"))
	require.Equal(t, "deepseek-v4-flash", PublicModelName("deepseek-v4-flash"))
	require.Equal(t, "", PublicModelName(""))
}

func TestOfficialNameAcceptedByAllowlist(t *testing.T) {
	// 分组白名单里照旧写着旧名：官方名要能进得来，老名字也不能被挤掉。
	enabled := GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-v4.1-flash"}}
	require.True(t, enabled.Allows("deepseek-flash"))
	require.True(t, enabled.Allows("deepseek-v4.1-flash"))
	// 上游真名仍然拒绝。
	require.False(t, enabled.Allows("deepseek-v4.1-flash-1017"))

	// 未开启白名单的分组同样放行官方名。
	disabled := GroupModelAllowlist{}
	require.True(t, disabled.Allows("deepseek-flash"))
}

func TestOfficialNameResolvesToExistingMapping(t *testing.T) {
	// 账号里配的映射键是站内写法，客户端写官方名要能命中它，
	// 这样"上游一行配置都不用改"才成立。
	account := &Account{
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"deepseek-v4.1-flash": "deepseek-v4.1-flash-1017",
			},
		},
	}
	mapped, matched := account.ResolveMappedModel("deepseek-flash")
	require.True(t, matched)
	require.Equal(t, "deepseek-v4.1-flash-1017", mapped)
}

func TestListingOnlyExposesOfficialName(t *testing.T) {
	source := []string{"deepseek-v4.1-flash", "deepseek-v4.1-flash-1017", "deepseek-v4-pro"}

	enabled := GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-v4.1-flash", "deepseek-v4-pro"}}
	require.Equal(t, []string{"deepseek-flash", "deepseek-v4-pro"}, enabled.FilterForListing(source))

	disabled := GroupModelAllowlist{}
	require.Equal(t, []string{"deepseek-flash", "deepseek-v4-pro"}, disabled.FilterForListing(source))

	// 白名单里同时写了旧名和官方名时，列表只出现一条（去重发生在换名之后）。
	both := GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-v4.1-flash", "deepseek-flash"}}
	require.Equal(t, []string{"deepseek-flash"}, both.FilterForListing(source))
}

func TestCatalogEntryIdentityRewrittenToOfficialName(t *testing.T) {
	raw := json.RawMessage(`{"id":"deepseek-v4.1-flash","object":"model","owned_by":"deepseek"}`)
	byID := map[string]json.RawMessage{"deepseek-v4.1-flash": raw}

	// 列表传进来的是对外名，目录键是站内写法：要能回查并改写 id。
	selected := selectModelCatalogEntries(byID, []string{"deepseek-flash"})
	require.Len(t, selected, 1)
	require.Equal(t, "deepseek-flash", gjsonGet(selected[0], "id"))
	require.Equal(t, "deepseek", gjsonGet(selected[0], "owned_by"))

	// slug 字段（Codex 目录）同样改写。
	slugRaw := json.RawMessage(`{"slug":"deepseek-v4.1-flash","display_name":"DeepSeek"}`)
	slugByID := map[string]json.RawMessage{"deepseek-v4.1-flash": slugRaw}
	selected = selectModelCatalogEntries(slugByID, []string{"deepseek-flash"})
	require.Len(t, selected, 1)
	require.Equal(t, "deepseek-flash", gjsonGet(selected[0], "slug"))

	// 目录里没有对应条目时保持原样丢弃，不凭空造项。
	require.Empty(t, selectModelCatalogEntries(map[string]json.RawMessage{}, []string{"deepseek-flash"}))
}

func TestResponseEchoFollowsRequestedName(t *testing.T) {
	svc := &OpenAIGatewayService{}

	// 新用户写官方名：上游回显 -1017，要给回官方名。
	body := []byte(`{"id":"x","model":"deepseek-v4.1-flash-1017","choices":[]}`)
	got := svc.normalizeResponseModelFamily(body, "deepseek-flash")
	require.Equal(t, "deepseek-flash", gjsonGet(got, "model"))

	// 老用户写旧名：回显仍是旧名（与改动前行为一致，无缝）。
	got = svc.normalizeResponseModelFamily(body, "deepseek-v4.1-flash")
	require.Equal(t, "deepseek-v4.1-flash", gjsonGet(got, "model"))

	// 客户端直接用了上游真名（入口会被白名单拦，这里只保证回显不炸）。
	got = svc.normalizeResponseModelFamily(body, "deepseek-v4.1-flash-1017")
	require.Equal(t, "deepseek-v4.1-flash-1017", gjsonGet(got, "model"))

	// 族外回显（跨渠道映射）保持上游真实模型，不冒充 flash。
	cross := []byte(`{"id":"y","model":"sensenova-6.8-flash-lite"}`)
	got = svc.normalizeResponseModelFamily(cross, "deepseek-flash")
	require.Equal(t, "sensenova-6.8-flash-lite", gjsonGet(got, "model"))
}
