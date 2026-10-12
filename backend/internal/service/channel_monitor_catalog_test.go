//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mkCatalogAccount 构造一个用于目录推导测试的账号。
// mapping 为 model_mapping（客户端请求模型名 → 上游模型名），groupIDs 为其所属分组。
func mkCatalogAccount(id int64, status, platform string, mapping map[string]string, groupIDs []int64) Account {
	creds := map[string]any{}
	if mapping != nil {
		mm := make(map[string]any, len(mapping))
		for k, v := range mapping {
			mm[k] = v
		}
		creds["model_mapping"] = mm
	}
	return Account{ID: id, Status: status, Platform: platform, Credentials: creds, GroupIDs: groupIDs}
}

// mkCatalogGroup 构造一个用于目录推导测试的分组。
func mkCatalogGroup(id int64, status string, allowEnabled bool, allowModels []string) *Group {
	return &Group{
		ID:             id,
		Status:         status,
		ModelAllowlist: GroupModelAllowlist{Enabled: allowEnabled, Models: allowModels},
	}
}

// TestDeriveChannelMonitorCatalogAllowlistDisabled 锁定：分组模型白名单未启用时，
// 账号支持的任何模型都进入目录（含任意客户端请求模型名，不被白名单裁剪）。
func TestDeriveChannelMonitorCatalogAllowlistDisabled(t *testing.T) {
	t.Parallel()

	g := mkCatalogGroup(10, StatusActive, false, nil)
	acc := mkCatalogAccount(1, StatusActive, PlatformOpenAI, map[string]string{
		"gpt-4o":      "gpt-4o",
		"gpt-4o-mini": "gpt-4o-mini",
	}, []int64{10})

	catalog := DeriveChannelMonitorCatalog([]Account{acc}, map[int64]*Group{10: g})

	require.Contains(t, catalog.Keys, ChannelMonitorCatalogKey{Platform: PlatformOpenAI, Model: "gpt-4o", GroupID: 10})
	require.Contains(t, catalog.Keys, ChannelMonitorCatalogKey{Platform: PlatformOpenAI, Model: "gpt-4o-mini", GroupID: 10})
	require.Len(t, catalog.Keys, 2, "白名单关闭时不应裁剪模型")
}

// TestDeriveChannelMonitorCatalogAllowlistEnabled 锁定：白名单启用后，
// 在列表内的模型进入目录，列表外的模型被排除。
func TestDeriveChannelMonitorCatalogAllowlistEnabled(t *testing.T) {
	t.Parallel()

	g := mkCatalogGroup(20, StatusActive, true, []string{"gpt-4o"})
	acc := mkCatalogAccount(2, StatusActive, PlatformOpenAI, map[string]string{
		"gpt-4o":      "gpt-4o",
		"gpt-4o-mini": "gpt-4o-mini",
	}, []int64{20})

	catalog := DeriveChannelMonitorCatalog([]Account{acc}, map[int64]*Group{20: g})

	require.Contains(t, catalog.Keys, ChannelMonitorCatalogKey{Platform: PlatformOpenAI, Model: "gpt-4o", GroupID: 20})
	require.NotContains(t, catalog.Keys, ChannelMonitorCatalogKey{Platform: PlatformOpenAI, Model: "gpt-4o-mini", GroupID: 20})
	require.Len(t, catalog.Keys, 1, "白名单启用时应仅保留列表内模型")
}

// TestDeriveChannelMonitorCatalogAlias 锁定：账号以别名（models/ 前缀）暴露模型，
// 分组白名单写规范名时，Allows 的候选归一化（去除 models/ 前缀）应命中，模型进入目录。
func TestDeriveChannelMonitorCatalogAlias(t *testing.T) {
	t.Parallel()

	g := mkCatalogGroup(30, StatusActive, true, []string{"gpt-4o"})
	// 客户端请求模型名带 models/ 前缀（别名），上游映射同值。
	acc := mkCatalogAccount(3, StatusActive, PlatformOpenAI, map[string]string{
		"models/gpt-4o": "models/gpt-4o",
	}, []int64{30})

	catalog := DeriveChannelMonitorCatalog([]Account{acc}, map[int64]*Group{30: g})

	require.Contains(t, catalog.Keys, ChannelMonitorCatalogKey{Platform: PlatformOpenAI, Model: "models/gpt-4o", GroupID: 30})
	require.Len(t, catalog.Keys, 1)
}

// TestDeriveChannelMonitorCatalogAccountDisabled 锁定：Status 非 active 的账号
// 整条排除（无论其模型是否在白名单内）。
func TestDeriveChannelMonitorCatalogAccountDisabled(t *testing.T) {
	t.Parallel()

	g := mkCatalogGroup(40, StatusActive, false, nil)
	// 注意：Status 非 active（paused），即便白名单关闭、模型受支持，也应被排除。
	acc := mkCatalogAccount(4, "paused", PlatformOpenAI, map[string]string{
		"gpt-4o": "gpt-4o",
	}, []int64{40})

	catalog := DeriveChannelMonitorCatalog([]Account{acc}, map[int64]*Group{40: g})

	require.NotContains(t, catalog.Keys, ChannelMonitorCatalogKey{Platform: PlatformOpenAI, Model: "gpt-4o", GroupID: 40})
	require.Empty(t, catalog.Keys, "禁用账号不应贡献任何目录项")
}

// TestDeriveChannelMonitorCatalogTransientRetained 锁定：temp_unschedulable 瞬态账号
// 的 Status 仍为 active，目录推导依据 IsActive 而非 Schedulable，故保留在目录中。
func TestDeriveChannelMonitorCatalogTransientRetained(t *testing.T) {
	t.Parallel()

	g := mkCatalogGroup(50, StatusActive, false, nil)
	future := time.Now().Add(time.Hour)
	acc := mkCatalogAccount(5, StatusActive, PlatformOpenAI, map[string]string{
		"gpt-4o": "gpt-4o",
	}, []int64{50})
	acc.TempUnschedulableUntil = &future
	acc.Schedulable = false // 瞬态：不可调度，但 Status 仍为 active

	catalog := DeriveChannelMonitorCatalog([]Account{acc}, map[int64]*Group{50: g})

	require.Contains(t, catalog.Keys, ChannelMonitorCatalogKey{Platform: PlatformOpenAI, Model: "gpt-4o", GroupID: 50})
	require.Len(t, catalog.Keys, 1, "瞬态账号应保留在目录（仅被调度层临时摘牌）")
}

// TestDeriveChannelMonitorCatalogGroupDisabled 锁定：分组非 active 时，其下账号的
// 三元组不进入目录（即便白名单关闭、账号 active）。
func TestDeriveChannelMonitorCatalogGroupDisabled(t *testing.T) {
	t.Parallel()

	g := mkCatalogGroup(60, "paused", false, nil)
	acc := mkCatalogAccount(6, StatusActive, PlatformOpenAI, map[string]string{
		"gpt-4o": "gpt-4o",
	}, []int64{60})

	catalog := DeriveChannelMonitorCatalog([]Account{acc}, map[int64]*Group{60: g})

	require.Empty(t, catalog.Keys, "禁用分组不应贡献任何目录项")
}
