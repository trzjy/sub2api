//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// CR-E（方案 §25.14-B 上半：读取与投影）定向测试。
//
// 覆盖两件事：
//  1. cursorSupplyPublicModelIDsFromMapping 是 Cursor 映射键的**唯一纯投影**
//     （排除空键/通配键、不取 mapping 值、不补静态清单），且公开 /v1/models 路径与
//     窄只读入口共用同一份结果；
//  2. GetCursorSupplyModelIDs 的错误契约：明确空集合（无组/非桥接平台/无合格账号）
//     与读取失败（账号/来源/分组白名单）严格区分，来源被拒账号不贡献模型。
//
// 全部断言走包内既有夹具（stub repo + 已安装的来源状态替身），不连 DB、不引 mock 框架。

const (
	cursorSupplyProjGroupID = int64(8801)
	// 两个具体公开名保持字典序（A < B），使"排序稳定"的断言可读。
	cursorSupplyProjModelA   = "cursor-haiku"
	cursorSupplyProjModelB   = "cursor-sonnet"
	cursorSupplyProjWildcard = "gpt-*"
)

// cursorSupplyProjAccount 构造一个显式挂组、绑定 ShareAI 归属激活码的 cursor 号池账号
// （mapping 键＝公开名）。
//
// Type 固定为 OAuth：本文件的用例全部围绕"绑定 QuotaKeyID 的号池账号"的来源门语义
// （快照新鲜度 / key 终态 / 依赖装配）。第三方 OpenAI 兼容反代账号（cursor + api_key）
// 按 CARD-KC-10 已整体豁免该来源门，若夹具延用 api_key，来源相关断言会全部失去对象。
func cursorSupplyProjAccount(id int64, quotaKeyID *int64, mapping map[string]any) Account {
	return Account{
		ID:          id,
		Platform:    PlatformCursor,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		QuotaKeyID:  quotaKeyID,
		GroupIDs:    []int64{cursorSupplyProjGroupID},
		Credentials: map[string]any{"model_mapping": mapping},
	}
}

// cursorSupplyProjCtx 注入已装载的分组（白名单 nil 表示未启用分组白名单）。
func cursorSupplyProjCtx(platform string, allowlist []string) context.Context {
	group := &Group{
		ID:       cursorSupplyProjGroupID,
		Platform: platform,
		Status:   StatusActive,
		Hydrated: true,
	}
	if allowlist != nil {
		group.ModelAllowlist = GroupModelAllowlist{Enabled: true, Models: allowlist}
	}
	return context.WithValue(context.Background(), ctxkey.Group, group)
}

// cursorSupplyProjGroupRepoStub 只实现窄只读入口会回源的那一个方法。
type cursorSupplyProjGroupRepoStub struct {
	GroupRepository

	err error
}

func (s *cursorSupplyProjGroupRepoStub) GetByIDLite(_ context.Context, _ int64) (*Group, error) {
	if s.err != nil {
		return nil, s.err
	}
	return nil, nil
}

// cursorSupplyProjFreezeNow 冻结来源门的新鲜度时钟（快照 checked_at 判定用），
// 与主区已合入来源门用例（gateway_scheduling_admission_test.go a04nSchedFreezeNow）
// 同一安装模式：合法快照 + 新鲜 checked_at + 剩余>0 + t.Cleanup 还原，避免污染同包
// 其它用例（包级单例是共享状态）。
func cursorSupplyProjFreezeNow(t *testing.T) time.Time {
	t.Helper()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prev := cursorSupplySourceUsageNow
	cursorSupplySourceUsageNow = func() time.Time { return now }
	t.Cleanup(func() { cursorSupplySourceUsageNow = prev })
	return now
}

// TestCursorSupplyPublicModelIDsFromMapping 钉死纯投影的四条边界：空键与通配键不出现、
// mapping 值不当公开名、结果排序稳定、空映射返回空。
func TestCursorSupplyPublicModelIDsFromMapping(t *testing.T) {
	// 空键 + 通配键（"*" / "gpt-*"）+ 具体键混合：只保留具体键。
	got := cursorSupplyPublicModelIDsFromMapping(map[string]string{
		"":                       "upstream-empty",
		"*":                      "upstream-star",
		cursorSupplyProjWildcard: "upstream-wildcard",
		cursorSupplyProjModelB:   "upstream-b",
		cursorSupplyProjModelA:   "upstream-a",
	})
	require.Equal(t, []string{cursorSupplyProjModelA, cursorSupplyProjModelB}, got,
		"纯投影只输出具体请求侧键：排除空键与通配键，且不取 mapping 值")

	// 别名不取值：公开名是键，不是上游模型名。
	require.Equal(t, []string{"public-alias"}, cursorSupplyPublicModelIDsFromMapping(map[string]string{
		"public-alias": "upstream-real-model",
	}), "mapping 值（上游模型名）不得作为公开名暴露")

	// 空映射 / 全通配映射都返回空：不补静态清单、不猜模型名。
	require.Empty(t, cursorSupplyPublicModelIDsFromMapping(nil))
	require.Empty(t, cursorSupplyPublicModelIDsFromMapping(map[string]string{}))
	require.Empty(t, cursorSupplyPublicModelIDsFromMapping(map[string]string{
		"":  "x",
		"*": "y",
	}))
}

// TestCursorSupplyProjectionSharedByListModelsAndNarrowEntry 钉死「唯一纯投影」：
// 公开目录路径（GetAvailableModels）与窄只读入口对同一账号给出同一份 Cursor 名单，
// 都排除空键/通配键，说明两处调用点共用同一个投影函数而非各写一份算法。
//
// 注意：本用例的模型全部属于 gemini 家族。家族过滤（QKG-C1）只让"属于本组平台家族"
// 的 Cursor 模型进入 /v1/models；窄只读入口（GetCursorSupplyModelIDs）不做家族过滤，
// 故只有在两者家族一致的分组（此处 gemini 组 + gemini 家族模型）下，listed 与 supplied
// 才会相等。改用 gemini 家族模型而非无法识别家族的 cursor-* 名称，以在新语义下仍验证
// "同一份纯投影"的不变量。
func TestCursorSupplyProjectionSharedByListModelsAndNarrowEntry(t *testing.T) {
	keyID := int64(9701)
	now := cursorSupplyProjFreezeNow(t)
	freshCheckedAt := now.Add(-time.Minute).Format(time.RFC3339)
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{
			keyID: domain.QuotaKeyStatusActive,
		},
		snapshots: map[int64][]byte{keyID: []byte(
			`{"total":400,"used":86,"status_code":200,"checked_at":"` + freshCheckedAt + `","error_code":""}`,
		)},
	})

	// 两个模型都属于 gemini 家族：在 gemini 分组下 /v1/models（已按家族过滤）与窄只读
	// 入口给出同一份 Cursor 名单。
	const geminiModelA = "gemini-2.5-flash"
	const geminiModelB = "gemini-2.5-pro"
	account := cursorSupplyProjAccount(8101, &keyID, map[string]any{
		"":                       "upstream-empty",
		"*":                      "upstream-star",
		cursorSupplyProjWildcard: "upstream-wildcard-" + geminiModelA,
		geminiModelB:             "upstream-b",
		geminiModelA:             "upstream-a",
	})
	groupID := cursorSupplyProjGroupID
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {account}}}
	svc := &GatewayService{accountRepo: repo}
	ctx := cursorSupplyProjCtx(PlatformGemini, nil)

	listed := svc.GetAvailableModels(ctx, &groupID, PlatformGemini)
	supplied, err := svc.GetCursorSupplyModelIDs(ctx, &groupID, PlatformGemini)
	require.NoError(t, err)
	require.Equal(t, []string{geminiModelA, geminiModelB}, supplied)
	require.Equal(t, listed, supplied,
		"/v1/models 与窄只读入口必须共用同一个纯投影（空键/通配键都不外露）")
}

// TestGetCursorSupplyModelIDsCursorOnlyGroup 钉死 Cursor-only 组：返回精确键集合
// （排序稳定），无错误。
func TestCursorSupplyNarrowEntryCursorOnlyGroup(t *testing.T) {
	activeKey := int64(9711)
	now := cursorSupplyProjFreezeNow(t)
	freshCheckedAt := now.Add(-time.Minute).Format(time.RFC3339)
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{
			activeKey: domain.QuotaKeyStatusActive,
		},
		snapshots: map[int64][]byte{activeKey: []byte(
			`{"total":400,"used":86,"status_code":200,"checked_at":"` + freshCheckedAt + `","error_code":""}`,
		)},
	})

	groupID := cursorSupplyProjGroupID
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{
		groupID: {
			cursorSupplyProjAccount(8201, &activeKey, map[string]any{
				cursorSupplyProjModelA: "upstream-a",
				cursorSupplyProjModelB: "upstream-b",
			}),
		},
	}}
	svc := &GatewayService{accountRepo: repo}

	models, err := svc.GetCursorSupplyModelIDs(cursorSupplyProjCtx(PlatformGemini, nil), &groupID, PlatformGemini)
	require.NoError(t, err)
	require.Equal(t, []string{cursorSupplyProjModelA, cursorSupplyProjModelB}, models)
	require.Equal(t, int64(1), repo.listByGroupCalls.Load(), "窄入口只走已绑定该组的可调度查询")
	require.Equal(t, int64(0), repo.listAllCalls.Load(), "不访问全局 AvailableModels / 全站账号扫描")
}

// TestGetCursorSupplyModelIDsMixedGroupContributesSubset 钉死混合组：窄入口只贡献
// Cursor 账号的映射键子集（其它平台账号的模型不混入），并继续受组白名单约束。
func TestCursorSupplyNarrowEntryMixedGroupSubset(t *testing.T) {
	activeKey := int64(9721)
	now := cursorSupplyProjFreezeNow(t)
	freshCheckedAt := now.Add(-time.Minute).Format(time.RFC3339)
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{
			activeKey: domain.QuotaKeyStatusActive,
		},
		snapshots: map[int64][]byte{activeKey: []byte(
			`{"total":400,"used":86,"status_code":200,"checked_at":"` + freshCheckedAt + `","error_code":""}`,
		)},
	})

	groupID := cursorSupplyProjGroupID
	anthropicAccount := Account{
		ID:       8301,
		Platform: PlatformAnthropic,
		Type:     AccountTypeSetupToken,
		Status:   StatusActive,
		Credentials: map[string]any{"model_mapping": map[string]any{
			"claude-3-7-sonnet": "claude-3-7-sonnet",
		}},
	}
	cursorAccount := cursorSupplyProjAccount(8302, &activeKey, map[string]any{
		cursorSupplyProjModelA: "upstream-a",
		cursorSupplyProjModelB: "upstream-b",
	})
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{
		groupID: {anthropicAccount, cursorAccount},
	}}
	svc := &GatewayService{accountRepo: repo}

	// 组白名单只放行 m1：Cursor 贡献子集 = {m1}，且 claude-* 不得混入。
	models, err := svc.GetCursorSupplyModelIDs(
		cursorSupplyProjCtx(PlatformGemini, []string{cursorSupplyProjModelA}), &groupID, PlatformGemini)
	require.NoError(t, err)
	require.Equal(t, []string{cursorSupplyProjModelA}, models)

	// 关闭白名单后同一混合组贡献 Cursor 的两个键子集，仍不含其它平台模型。
	all, err := svc.GetCursorSupplyModelIDs(cursorSupplyProjCtx(PlatformGemini, nil), &groupID, PlatformGemini)
	require.NoError(t, err)
	require.Equal(t, []string{cursorSupplyProjModelA, cursorSupplyProjModelB}, all)
	require.NotContains(t, all, "claude-3-7-sonnet")
}

// TestGetCursorSupplyModelIDsRejectedSourceContributesNothing 钉死「来源明确被拒绝」
// 与「读取失败」的区别：激活码过期的账号不贡献模型且不报错；绑定来源却读不到来源
// 状态（provider 未接线 / 读取报错）必须报错而不是静默返回空表。
func TestCursorSupplyNarrowEntryRejectedSource(t *testing.T) {
	groupID := cursorSupplyProjGroupID
	expiredKey := int64(9731)
	installCursorQuotaKeyStateForTest(t, cursorQuotaKeyStateStub{statuses: map[int64]string{
		expiredKey: domain.QuotaKeyStatusExpired,
	}})
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{
		groupID: {
			cursorSupplyProjAccount(8401, &expiredKey, map[string]any{cursorSupplyProjModelA: "upstream-a"}),
		},
	}}
	svc := &GatewayService{accountRepo: repo}
	ctx := cursorSupplyProjCtx(PlatformGemini, nil)

	models, err := svc.GetCursorSupplyModelIDs(ctx, &groupID, PlatformGemini)
	require.NoError(t, err, "来源明确被拒绝是确定结论，不是读取失败")
	require.Empty(t, models, "已过期来源的账号不贡献模型")

	// provider 未接线：无法判定来源可用性 → 失败关闭（§25.6 要求真实 DI 依赖）。
	installCursorQuotaKeyStateForTest(t, nil)
	models, err = svc.GetCursorSupplyModelIDs(ctx, &groupID, PlatformGemini)
	require.Error(t, err, "绑定来源但来源读取器未接线必须报错，不能当空目录")
	require.Nil(t, models)

	// provider 读取报错：同样是「读不到」，必须报错。
	installCursorQuotaKeyStateForTest(t, cursorQuotaKeyStateStub{err: errors.New("quota key store unreachable")})
	models, err = svc.GetCursorSupplyModelIDs(ctx, &groupID, PlatformGemini)
	require.Error(t, err)
	require.Nil(t, models)
}

// TestGetCursorSupplyModelIDsReadFailureReturnsErrorNotEmpty 钉死「读取失败 ≠ 空目录」：
// 账号列表读取失败、分组白名单读取失败都必须返回错误且不返回"成功空表"。
func TestCursorSupplyNarrowEntryReadFailureReturnsError(t *testing.T) {
	activeKey := int64(9741)
	installCursorQuotaKeyStateForTest(t, cursorQuotaKeyStateStub{statuses: map[int64]string{
		activeKey: domain.QuotaKeyStatusActive,
	}})
	groupID := cursorSupplyProjGroupID
	account := cursorSupplyProjAccount(8501, &activeKey, map[string]any{cursorSupplyProjModelA: "upstream-a"})

	// ① 账号（可调度查询）读取失败。
	failingRepo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{groupID: {account}},
		err:     errors.New("accounts table unreachable"),
	}
	svc := &GatewayService{accountRepo: failingRepo}
	models, err := svc.GetCursorSupplyModelIDs(cursorSupplyProjCtx(PlatformGemini, nil), &groupID, PlatformGemini)
	require.Error(t, err)
	require.Nil(t, models, "读取失败不得伪装成成功空表")

	// ② 分组白名单读取失败（ctx 未装载分组，回源报错）。
	okRepo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {account}}}
	svc = &GatewayService{
		accountRepo: okRepo,
		groupRepo:   &cursorSupplyProjGroupRepoStub{err: errors.New("groups table unreachable")},
	}
	models, err = svc.GetCursorSupplyModelIDs(context.Background(), &groupID, PlatformGemini)
	require.Error(t, err)
	require.Nil(t, models, "组读取失败不得当成「组里没有这个模型」")
}

// TestGetCursorSupplyModelIDsNoQualifyingAccountReturnsEmptyNotError 钉死确定的空集合：
// 无组 ID、非已接受桥接平台、组内无合格 Cursor 账号都返回成功空表而非错误。
func TestCursorSupplyNarrowEntryNoQualifyingAccount(t *testing.T) {
	activeKey := int64(9751)
	installCursorQuotaKeyStateForTest(t, cursorQuotaKeyStateStub{statuses: map[int64]string{
		activeKey: domain.QuotaKeyStatusActive,
	}})
	groupID := cursorSupplyProjGroupID
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{
		groupID: {
			{ID: 8601, Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Status: StatusActive,
				Credentials: map[string]any{"model_mapping": map[string]any{"claude-3-7-sonnet": "claude-3-7-sonnet"}}},
		},
	}}
	svc := &GatewayService{accountRepo: repo}
	ctx := cursorSupplyProjCtx(PlatformGemini, nil)

	// ① 无组 ID（未挂组 / undefined 分组 API Key）。
	models, err := svc.GetCursorSupplyModelIDs(ctx, nil, PlatformGemini)
	require.NoError(t, err)
	require.Empty(t, models)

	// ② 非已接受桥接平台。
	models, err = svc.GetCursorSupplyModelIDs(ctx, &groupID, PlatformCodeBuddy)
	require.NoError(t, err)
	require.Empty(t, models)
	require.Equal(t, int64(0), repo.listByGroupCalls.Load(), "非桥接平台不发起账号查询")

	// ③ 组内只有非 Cursor 账号。
	models, err = svc.GetCursorSupplyModelIDs(ctx, &groupID, PlatformGemini)
	require.NoError(t, err)
	require.Empty(t, models)
}

// TestCursorSupplyGetAvailableModelsFamilyFilter 钉死 QKG-C1 的修复：Cursor 跨平台供给
// 账号在原生平台分组的 /v1/models 中必须按模型家族过滤，绝不能把其它平台模型泄漏进本组。
//
// 映射覆盖四个家族 + 一个自定义别名（my-claude → claude-sonnet-5-5）+ 一个两端都判定不出的
// 未知别名：
//   - anthropic 组仅返回 claude-* 家族（含别名 my-claude：源 my-claude 判定不出，回落到
//     映射目标 claude-sonnet-5-5 属 anthropic）；
//   - openai 组仅返回 gpt-*；gemini 组仅返回 gemini-*；grok 组仅返回 grok-*；
//   - 自定义别名 my-claude 在 openai 组不可见（映射目标属 anthropic，与 openai 不符）；
//   - 未知别名（源与目标都判定不出）在任何平台组都不可见（失败关闭）。
func TestCursorSupplyGetAvailableModelsFamilyFilter(t *testing.T) {
	activeKey := int64(9761)
	now := cursorSupplyProjFreezeNow(t)
	freshCheckedAt := now.Add(-time.Minute).Format(time.RFC3339)
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{
			activeKey: domain.QuotaKeyStatusActive,
		},
		snapshots: map[int64][]byte{activeKey: []byte(
			`{"total":400,"used":86,"status_code":200,"checked_at":"` + freshCheckedAt + `","error_code":""}`,
		)},
	})

	account := cursorSupplyProjAccount(8701, &activeKey, map[string]any{
		"claude-fable-5-1": "anthropic.claude-fable-5-1",
		"gpt-6.1-sol":      "gpt-6.1-sol",
		"gemini-3.8-flash": "gemini-3.8-flash",
		"grok-4.7":         "grok-4.7",
		"my-claude":        "claude-sonnet-5-5", // 自定义别名：源 my-claude 判定不出，回落到映射目标
		"zzz-unknown":      "qqq-unknown",       // 源与目标都判定不出 → 任何平台组都不可见
	})
	groupID := cursorSupplyProjGroupID
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {account}}}
	svc := &GatewayService{accountRepo: repo}

	cases := []struct {
		platform string
		want     []string
	}{
		{PlatformAnthropic, []string{"claude-fable-5-1", "my-claude"}},
		{PlatformOpenAI, []string{"gpt-6.1-sol"}},
		{PlatformGemini, []string{"gemini-3.8-flash"}},
		{PlatformGrok, []string{"grok-4.7"}},
	}
	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			ctx := cursorSupplyProjCtx(tc.platform, nil)
			got := svc.GetAvailableModels(ctx, &groupID, tc.platform)
			require.Equal(t, tc.want, got,
				"平台分组 %s 的 /v1/models 必须只含其模型家族", tc.platform)
			require.NotContains(t, got, "zzz-unknown",
				"未知别名在任何平台组都必须失败关闭")
		})
	}

	// 自定义别名 my-claude 在 openai 组不可见：映射目标 claude-sonnet-5-5 属 anthropic，
	// 与 openai 不符 → 被过滤。
	ctxOpenAI := cursorSupplyProjCtx(PlatformOpenAI, nil)
	openAIListed := svc.GetAvailableModels(ctxOpenAI, &groupID, PlatformOpenAI)
	require.NotContains(t, openAIListed, "my-claude",
		"自定义别名 my-claude 的映射目标属 anthropic，不得泄漏进 openai 组")
}
