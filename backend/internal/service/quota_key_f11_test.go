//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CUR-F11 R8 回修验证。
//
//	R8-1（CUR-F17 外审 R14 回修后）：旧配置失效的可见性。仅"清单外勾选"判配置错误——
//	      勾选非空但经实供清单门控后为空（如残留官方模型名 gpt-4.1）→ ListAllowedModels
//	      返回可识别错误（CURSOR_CONFIG_NO_REAL_SUPPLY_INTERSECTION），使 reconciler 走失败心跳。
//	      "清单内勾选但无分组覆盖"不再判配置错误（CUR-F17 语义反转）：分组覆盖是运营态，
//	      模型/分组随时可能调整，reconciler 正常空跑停轮（返回空集 + nil 错误，v3.5 §13.2）。
//	      勾选为空（用户全关 / 配置无勾选）→ 返回空集 nil 错误（现状语义不回归）。
//	R8-2：交集测试隔离（改 quota_key_service_test.go 的
//	      TestCURF1_AllowedModels_IntersectionExcludesOutside，此处不重复）。
// ---------------------------------------------------------------------------

// TestCURF11_ListAllowedModels_StaleOfficialModelNoLongerErrors 验证 P1-B2 收敛：
// 补池左项恒为静态实供清单，不再取"人工勾选"，故旧配置残留官方模型名（gpt-4.1，不在实供清单）
// 不再产生 CURSOR_CONFIG_NO_REAL_SUPPLY_INTERSECTION 配置错误——残留勾选不再是补池输入。
func TestCURF11_ListAllowedModels_StaleOfficialModelNoLongerErrors(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 仅勾选官方模型名 gpt-4.1（不在实供清单）。
	cfg := &CursorAutoEntryConfig{
		Vendors:       map[string]bool{CursorVendorOpenAI: true},
		Models:        []CursorModelRef{{ID: "gpt-4.1", Vendor: CursorVendorOpenAI}},
		EntryDefaults: CursorEntryDefaults{RateMultiplier: 1.0, Priority: 1},
	}
	w := NewQuotaKeyModelWater(client, &curF1ConfigSource{cfg: cfg})

	models, err := w.ListAllowedModels(ctx)
	require.NoError(t, err, "stale official-only selection no longer drives 补池 → must not surface a config error")
	for _, m := range models {
		require.NotEqual(t, "gpt-4.1", m, "official-catalog-only name must never be enumerated")
	}
}

// TestCURF11_ListAllowedModels_ConfigSelectionIrrelevantAfterP1B2 验证 R12-2（CUR-F15）回归
// 在 P1-B2 下的形态：无论配置残留官方名还是已清理，ListAllowedModels 都不报错，
// 且产出同一份"静态实供清单 ∩ 目标分组覆盖"集合（配置不再是补池输入）。
//
// 形式说明：本测试直接读配置（curF1ConfigSource），不依赖 settings 落库；"保存清理后的落库态"
// 以"对配置做与 save() 同口径的实供清单过滤"模拟——与前端 save() 的 availIds 过滤同集
// （CursorRealSupplyModels）。
func TestCURF11_ListAllowedModels_ConfigSelectionIrrelevantAfterP1B2(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 原始配置（残留官方模型名形态）：仅勾选 gpt-4.1（不在实供清单）。
	rawCfg := &CursorAutoEntryConfig{
		Vendors:       map[string]bool{CursorVendorOpenAI: true},
		Models:        []CursorModelRef{{ID: "gpt-4.1", Vendor: CursorVendorOpenAI}},
		EntryDefaults: CursorEntryDefaults{RateMultiplier: 1.0, Priority: 1},
	}
	wRaw := NewQuotaKeyModelWater(client, &curF1ConfigSource{cfg: rawCfg})
	modelsRaw, errRaw := wRaw.ListAllowedModels(ctx)
	require.NoError(t, errRaw, "residual official-name config must not surface a config error (P1-B2)")

	// 模拟 save() 清理——仅保留实供清单（CursorRealSupplyModels）内 ID，丢弃 gpt-4.1。
	realSupplyIDs := make(map[string]bool, len(CursorRealSupplyModels))
	for _, sm := range CursorRealSupplyModels {
		realSupplyIDs[sm.ID] = true
	}
	cleanedModels := make([]CursorModelRef, 0, len(rawCfg.Models))
	for _, m := range rawCfg.Models {
		if realSupplyIDs[m.ID] {
			cleanedModels = append(cleanedModels, m)
		}
	}
	cleanedCfg := &CursorAutoEntryConfig{
		Vendors:       rawCfg.Vendors,
		Models:        cleanedModels,
		EntryDefaults: rawCfg.EntryDefaults,
	}
	wClean := NewQuotaKeyModelWater(client, &curF1ConfigSource{cfg: cleanedCfg})
	modelsClean, err := wClean.ListAllowedModels(ctx)
	require.NoError(t, err, "cleaned config must not surface a config error")
	require.Equal(t, modelsRaw, modelsClean,
		"config selection no longer affects 补池 enumeration (static supply list drives it)")
}

// TestCURF11_ListAllowedModels_EmptySelectionIsNormal 验证 R8-1 边界：勾选为空（用户全关）
// 必须仍为正常空跑（nil 错误），现状语义不回归。
func TestCURF11_ListAllowedModels_EmptySelectionIsNormal(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	cfg := &CursorAutoEntryConfig{
		Vendors:       map[string]bool{CursorVendorOpenAI: true},
		Models:        []CursorModelRef{}, // 用户全关
		EntryDefaults: CursorEntryDefaults{RateMultiplier: 1.0, Priority: 1},
	}
	w := NewQuotaKeyModelWater(client, &curF1ConfigSource{cfg: cfg})

	models, err := w.ListAllowedModels(ctx)
	require.NoError(t, err, "user turning everything off must remain a normal empty run (no regression)")
	require.Empty(t, models)
}

// TestCURF11_ListAllowedModels_RealSupplyWithCoverage 验证 R8-1 回归：勾选实供模型
// grok-4.7 且存在活跃分组白名单覆盖 → 正常返回，不报配置错误。
func TestCURF11_ListAllowedModels_RealSupplyWithCoverage(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	w := NewQuotaKeyModelWater(client, curF1ConfigSelecting("grok-4.7"))

	// P1-B2 U7：分组必须是模型所属平台（cursor）的目标分组——composite 聚合组为生产形态。
	grp, err := client.Group.Create().
		SetName(fmt.Sprintf("curf11-cov-%d", time.Now().UnixNano())).
		SetPlatform(domain.PlatformComposite).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{"grok-4.7"}}).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().
			Where(accountgroup.GroupIDEQ(grp.ID)).Exec(context.Background())
		_, _ = client.Group.Delete().
			Where(entgroup.IDEQ(grp.ID)).Exec(context.Background())
	})

	models, err := w.ListAllowedModels(ctx)
	require.NoError(t, err, "real-supply model covered by a group must not raise a config error")
	require.Contains(t, models, "grok-4.7", "covered real-supply model must be enumerated")
}

// TestCURF11_ListAllowedModels_RealSupplyNoCoverageTriggersConfigError 验证 CUR-F17 语义反转
// （原 R8-1 第二态，外审 R14 回修后撤销）：勾选实供模型（gpt-6.1-sol）但共享库无任何活跃分组
// 白名单覆盖它 → selected 非空但交集为空 → 不再返回配置错误，而是返回空集 + nil 错误，
// reconciler 正常空跑停轮（v3.5 §13.2「无可选 key 即停」；分组覆盖是运营态不是配置错误）。
func TestCURF11_ListAllowedModels_RealSupplyNoCoverageTriggersConfigError(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 勾选 gpt-6.1-sol（实供模型），但不创建任何覆盖它的分组。
	w := NewQuotaKeyModelWater(client, curF1ConfigSelecting("gpt-6.1-sol"))

	models, err := w.ListAllowedModels(ctx)
	require.NoError(t, err, "real-supply model with no group coverage is an operational empty state, NOT a config error (CUR-F17)")
	require.Empty(t, models, "no coverage means no models enumerated; reconciler run is a normal empty run")
}
