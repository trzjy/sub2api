package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 内部供给平台（codebuddy/muse/cursor）在用户侧 API Key 响应边界必须剥离供给方
// 身份；管理端共用的 APIKeyFromService 必须保留真实值。判定源 =
// domain.IsInternalSupplyPlatform（internal-platform-key-visibility-repair-plan §3.1）。
// 聚合直绑开关为管理端专属字段（AdminGroup.AggregateDirectEnabled，2026-10-18 用户
// 裁定迁出用户侧 Group DTO），用户响应 JSON 不得含任何 aggregate_* 键。

func apiKeyFixture(group *service.Group) *service.APIKey {
	gid := group.ID
	return &service.APIKey{
		ID:      101,
		UserID:  1,
		Key:     "sk-test",
		Name:    "k",
		GroupID: &gid,
		Group:   group,
		Status:  "active",
	}
}

// requireNoAggregateKey 断言用户侧 DTO 序列化后不含任何 aggregate_* 键
// （覆盖迁移前的旧 tag aggregate_codebuddy_enabled 与新 tag aggregate_direct_enabled）。
func requireNoAggregateKey(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "aggregate_", "user DTO must not expose the admin-only aggregate flag under any name")
}

func TestAPIKeyFromServiceForUserStripsInternalSupplyPlatform(t *testing.T) {
	for _, platform := range []string{"codebuddy", "muse", "cursor"} {
		group := &service.Group{ID: 10, Name: "supply-g", Platform: platform, AggregateCodeBuddyEnabled: platform == "codebuddy"}
		out := APIKeyFromServiceForUser(apiKeyFixture(group))
		require.NotNil(t, out)
		require.NotNil(t, out.Group, "group object must be kept: key stays bound, not 'ungrouped'")
		require.Equal(t, int64(10), out.Group.ID)
		require.Equal(t, "supply-g", out.Group.Name)
		require.NotNil(t, out.GroupID)
		require.Equal(t, int64(10), *out.GroupID)
		require.Empty(t, out.Group.Platform, "supply platform %s must not appear in user response", platform)
		requireNoAggregateKey(t, out)
	}
}

func TestAPIKeyFromServiceForUserKeepsVisibleGroups(t *testing.T) {
	for _, platform := range []string{"composite", "anthropic", "openai"} {
		group := &service.Group{ID: 20, Name: "visible-g", Platform: platform, AggregateCodeBuddyEnabled: platform == "composite"}
		out := APIKeyFromServiceForUser(apiKeyFixture(group))
		require.NotNil(t, out)
		require.NotNil(t, out.Group)
		require.Equal(t, platform, out.Group.Platform, "non-internal platform must pass through unchanged")
		// composite 组即使聚合直绑开启，用户响应也不得携带该字段（2026-10-18 用户裁定）。
		requireNoAggregateKey(t, out)
	}
}

func TestAPIKeyFromServiceForUserNilGroupAndNilKey(t *testing.T) {
	require.Nil(t, APIKeyFromServiceForUser(nil))

	key := apiKeyFixture(&service.Group{ID: 10, Name: "g", Platform: "codebuddy"})
	key.Group = nil
	out := APIKeyFromServiceForUser(key)
	require.NotNil(t, out)
	require.Nil(t, out.Group)
}

func TestAPIKeyFromServiceAdminKeepsRealPlatform(t *testing.T) {
	// 管理端共用 mapper 回归锚：管理端必须继续看到真实上游平台。
	for _, platform := range []string{"codebuddy", "muse", "cursor"} {
		out := APIKeyFromService(apiKeyFixture(&service.Group{ID: 10, Name: "supply-g", Platform: platform}))
		require.NotNil(t, out)
		require.NotNil(t, out.Group)
		require.Equal(t, platform, out.Group.Platform)
	}

	adminOut := GroupFromServiceAdmin(&service.Group{ID: 10, Name: "supply-g", Platform: "codebuddy", AggregateCodeBuddyEnabled: true})
	require.True(t, adminOut.AggregateDirectEnabled, "admin mapper must keep the real aggregate flag")
	raw, err := json.Marshal(adminOut)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"aggregate_direct_enabled":true`, "admin response carries the renamed neutral tag")
	require.NotContains(t, string(raw), "aggregate_codebuddy_enabled", "old supplier-named tag must be gone")
}

func TestAPIKeyFromServiceForUserDoesNotMutateServiceObjects(t *testing.T) {
	// 副本独立性锚（R3 外审残余风险闭合）：ForUser 只变异 DTO 副本，绝不回写
	// service 层对象——否则同一对象复用会给管理端响应带来串味风险。
	group := &service.Group{ID: 10, Name: "supply-g", Platform: "codebuddy", AggregateCodeBuddyEnabled: true}
	key := apiKeyFixture(group)

	out := APIKeyFromServiceForUser(key)
	require.NotNil(t, out.Group)
	require.Empty(t, out.Group.Platform)

	require.Equal(t, "codebuddy", group.Platform, "service.Group must be untouched by user mapper")
	require.True(t, group.AggregateCodeBuddyEnabled, "service.Group aggregate flag must be untouched")
	require.Equal(t, "codebuddy", key.Group.Platform, "source key's nested group must be untouched")
}

func TestAPIKeyFromServiceOmitsAggregateKeys(t *testing.T) {
	// R3 外审 must_fix 消化：钉死管理端 API Key 出口新契约——聚合直绑开关已整体
	// 迁出共享 dto.Group（仅存于 AdminGroup），管理端 API Key 响应（经共享
	// APIKeyFromService）不含任何 aggregate_* 键；开关的管理功能由分组管理端点
	// （GroupFromServiceAdmin → aggregate_direct_enabled）完整承担，读写语义零损失。
	// 这是 2026-10-18 字段迁移裁定的有意结果，不是字段丢失缺陷。
	group := &service.Group{ID: 10, Name: "supply-g", Platform: "codebuddy", AggregateCodeBuddyEnabled: true}
	out := APIKeyFromService(apiKeyFixture(group))
	requireNoAggregateKey(t, out)
}
