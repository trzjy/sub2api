package dto

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 用户侧用量 DTO（UsageLogFromService）在输出边界剥离内部供给平台身份
// （platform 置空串，唯一判定源 domain.IsInternalSupplyPlatform）；管理端
// UsageLogFromServiceAdmin 不经过该剥离，保留真实平台。记录本身与账务数据
// 原样保留（不按分组过滤行），group 对象保留（group_id/名称/绑定状态）。
// 到达链（2026-10-18 生产实证）：UsageHandler.List → UsageService.ListWithFilters
// → repo hydrateUsageLogAssociations(loadGroups) → UsageLogFromService →
// GroupFromServiceShallow 原样输出内部 platform。

func usageLogFixture(group *service.Group) *service.UsageLog {
	gid := group.ID
	return &service.UsageLog{
		ID:           9001,
		UserID:       1,
		APIKeyID:     101,
		AccountID:    7,
		Model:        "space-bunny",
		GroupID:      &gid,
		InputTokens:  100,
		OutputTokens: 50,
		Group:        group,
	}
}

func TestUsageLogFromServiceStripsInternalSupplyPlatform(t *testing.T) {
	for _, platform := range []string{"codebuddy", "muse", "cursor"} {
		out := UsageLogFromService(usageLogFixture(&service.Group{ID: 109, Name: "space-bunny-sub x2", Platform: platform}))
		require.NotNil(t, out)
		require.NotNil(t, out.Group, "group object must be kept: record and binding stay visible")
		require.Equal(t, int64(109), out.Group.ID, "group_id binding must be preserved")
		require.Equal(t, "space-bunny-sub x2", out.Group.Name)
		require.NotNil(t, out.GroupID)
		require.Equal(t, int64(109), *out.GroupID)
		require.Empty(t, out.Group.Platform, "supply platform %s must not appear in user usage response", platform)
		// 记录与账务字段原样保留：不按分组过滤行。
		require.Equal(t, int64(9001), out.ID)
		require.Equal(t, 100, out.InputTokens)
		require.Equal(t, 50, out.OutputTokens)
	}
}

func TestUsageLogFromServiceKeepsVisibleGroups(t *testing.T) {
	for _, platform := range []string{"composite", "anthropic", "openai", "deepseek"} {
		out := UsageLogFromService(usageLogFixture(&service.Group{ID: 57, Name: "Deepseek 订阅 x2", Platform: platform}))
		require.NotNil(t, out)
		require.NotNil(t, out.Group)
		require.Equal(t, platform, out.Group.Platform, "non-internal platform must pass through unchanged")
	}
}

func TestUsageLogFromServiceNilGroupAndNilLog(t *testing.T) {
	require.Nil(t, UsageLogFromService(nil))

	log := usageLogFixture(&service.Group{ID: 109, Name: "g", Platform: "codebuddy"})
	log.Group = nil
	out := UsageLogFromService(log)
	require.NotNil(t, out)
	require.Nil(t, out.Group, "log without a hydrated group must not grow one")
}

func TestUsageLogFromServiceAdminKeepsRealPlatform(t *testing.T) {
	// 管理端共用 usageLogFromServiceUser 基座，剥离只发生在用户入口
	// UsageLogFromService——管理端必须继续看到真实上游平台。
	for _, platform := range []string{"codebuddy", "muse", "cursor"} {
		adminOut := UsageLogFromServiceAdmin(usageLogFixture(&service.Group{ID: 109, Name: "space-bunny-sub x2", Platform: platform}))
		require.NotNil(t, adminOut)
		require.NotNil(t, adminOut.Group)
		require.Equal(t, platform, adminOut.Group.Platform, "admin usage response must keep the real supply platform %s", platform)
	}
}

func TestUsageLogFromServiceDoesNotMutateServiceObjects(t *testing.T) {
	// 副本独立性锚：用户入口只变异 DTO 副本（GroupFromServiceShallow 值拷贝），
	// 绝不回写 service 层对象——否则同一对象复用会给管理端响应带来串味风险。
	group := &service.Group{ID: 109, Name: "space-bunny-sub x2", Platform: "codebuddy"}
	log := usageLogFixture(group)

	out := UsageLogFromService(log)
	require.NotNil(t, out.Group)
	require.Empty(t, out.Group.Platform)

	require.Equal(t, "codebuddy", group.Platform, "service.Group must be untouched by user usage mapper")
	require.Equal(t, "codebuddy", log.Group.Platform, "source log's nested group must be untouched")
}
