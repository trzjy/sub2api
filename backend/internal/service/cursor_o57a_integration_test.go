//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

// O57-A §6：OpenAI 兼容链纳 Cursor 跨平台供给 + 公开列表同源 的配对单测。
//
// 覆盖六条 Done when：
//  1. cursorNativeBridgePlatforms 新增 grok/openai 的正负例；
//  2. 候选查询集合：grok/openai 已挂组桶并入 cursor（多协议族不扩）；
//  3. 调度门：grok/openai 分组请求可接纳 cursor 候选；映射未声明模型给 model_mismatch；
//  4. 转发分派：cursor 账号经契约 0.1 包级桥接打到 cursor 原生端点；
//  5. 公开列表同源：cursor 映射的具体键出现、通配/空键剔除；AI Studio 候选剔除 cursor；
//     空 muse 分组返回真实空（不回落 Claude 默认目录）。
//
// 全部走包内既有夹具（stub repo / 包级单例），不连 DB、不引入 mock 框架。

// ---------------------------------------------------------------------------
// §6.1 桥接白名单正负例
// ---------------------------------------------------------------------------

func TestCursorSupplyNativeBridgePlatformsO57A(t *testing.T) {
	// 正向：原生 Claude/Gemini + O57-A 新增的 openai 兼容族（grok/openai）
	for _, p := range []string{PlatformAnthropic, PlatformGemini, PlatformGrok, PlatformOpenAI} {
		require.True(t, cursorNativeBridgePlatforms(p), "桥接平台 %q 必须为 true", p)
	}
	// 负向：多协议族与无内置目录平台不得接纳 cursor 跨平台供给
	for _, p := range []string{
		PlatformComposite, PlatformMuse, PlatformCodeBuddy,
		PlatformOther, PlatformTypeSafe, PlatformCursor,
	} {
		require.False(t, cursorNativeBridgePlatforms(p), "平台 %q 不得为桥接平台", p)
	}
}

// ---------------------------------------------------------------------------
// §6.2 候选查询集合：grok/openai 已挂组桶并入 cursor
// ---------------------------------------------------------------------------

func TestCursorSupplyQuerySetGrokOpenAIO57A(t *testing.T) {
	groupID := cursorSchedTestGroupID

	// grok / openai 已挂组时，cursor 被并入多平台查询集合（§1 + §2 联动）
	require.Equal(t, []string{PlatformGrok, PlatformCursor},
		cursorSupplyPlatformsFor(&groupID, []string{PlatformGrok}))
	require.Equal(t, []string{PlatformOpenAI, PlatformCursor},
		cursorSupplyPlatformsFor(&groupID, []string{PlatformOpenAI}))
	// 未挂组：不并入 cursor（不外溢）
	require.Equal(t, []string{PlatformGrok},
		cursorSupplyPlatformsFor(nil, []string{PlatformGrok}))
	// 多协议族（composite）即便与可桥接平台同桶也不扩 cursor（防 multi-protocol 外溢）
	require.Equal(t, []string{PlatformComposite},
		cursorSupplyPlatformsFor(&groupID, []string{PlatformComposite}))
	// 入参不被就地改写
	base := []string{PlatformOpenAI}
	_ = cursorSupplyPlatformsFor(&groupID, base)
	require.Equal(t, []string{PlatformOpenAI}, base)
}

// ---------------------------------------------------------------------------
// §6.3 调度门：grok/openai 分组请求接纳 cursor 候选；映射未声明模型给 model_mismatch
// ---------------------------------------------------------------------------

func TestOpenAIAccountEligibilityCursorCrossPlatformSupplyO57A(t *testing.T) {
	ctx := context.WithValue(context.Background(), openAIGroupPrivacyRequirementContextKey{},
		openAIGroupPrivacyRequirement{groupID: cursorSchedTestGroupID, required: false})
	// 不收紧 key 终态门（provider 未安装视为不额外收紧），聚焦调度门本身。
	installCursorQuotaKeyStateForTest(t, nil)

	// (a) grok 分组请求 grok-4.6：映射声明了 grok-4.6 的 cursor 候选应被准入（空原因）
	grokCursor := cursorSchedTestAccount(9101, nil, "grok-4.6")
	require.Empty(t, openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		ctx, &grokCursor, PlatformGrok, "grok-4.6", false, ""),
		"grok 分组请求 grok-4.6 应接纳映射了该模型的 cursor 候选")

	// (b) openai 分组请求 gpt-6-sol：映射声明了 gpt-6-sol 的 cursor 候选应被准入
	openaiCursor := cursorSchedTestAccount(9102, nil, "gpt-6-sol")
	require.Empty(t, openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		ctx, &openaiCursor, PlatformOpenAI, "gpt-6-sol", false, ""),
		"openai 分组请求 gpt-6-sol 应接纳映射了该模型的 cursor 候选")

	// (c) 映射未声明请求模型：cursor 候选被拒，且原因是 model_mismatch 而非 platform_mismatch
	noModelCursor := cursorSchedTestAccount(9103, nil, "grok-4.6")
	require.Equal(t, "model_mismatch", openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		ctx, &noModelCursor, PlatformGrok, "gpt-6-sol", false, ""),
		"映射未声明请求模型的 cursor 候选应给 model_mismatch（与调度准入口径一致）")

	// (d) 非桥接平台（composite）上的 cursor 账号仍按 platform_mismatch 拒绝
	compositeCursor := cursorSchedTestAccount(9104, nil, "grok-4.6")
	require.Equal(t, "platform_mismatch", openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		ctx, &compositeCursor, PlatformComposite, "grok-4.6", false, ""),
		"多协议族（composite）不接受 cursor 跨平台供给，应给 platform_mismatch")
}

// ---------------------------------------------------------------------------
// §6.4 转发分派：cursor 账号经契约 0.1 包级桥接打到 cursor 原生端点
// ---------------------------------------------------------------------------

func TestCursorSupplyForwardDispatchRoutingO57A(t *testing.T) {
	// 模拟 CC 入口（cursorInboundOpenAIChat）把 cursor 账号交给 CursorForwardInbound：
	// 受理后必须由 CursorUpstreamForwarder 真正打到 cursor 原生端点（路径/请求形状）。
	prev := cursorBridgeInstalled()
	t.Cleanup(func() { InstallCursorBridge(prev) })

	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: cursorOKStreamBody(t)}
	svc, c := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))
	InstallCursorBridge(svc)

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	res, err := CursorForwardInbound(context.Background(), c, acc, cursorInboundOpenAIChat, body)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 1, up.count())
	require.Equal(t, cursorPathAgentRun, up.at(0).path,
		"CC 入口的 cursor 账号必须经由 cursor 桥接打到原生端点")
	require.Equal(t, "Bearer BEARER-ACCOUNT-A", up.at(0).header.Get("Authorization"))
}

// ---------------------------------------------------------------------------
// §6.5 公开列表同源
// ---------------------------------------------------------------------------

// TestAvailableModelsCursorSupplySameSourceO57A 钉死：openai 分组公开列表须与调度候选
// 同源——cursor 跨平台供给账号的具体映射键出现，而通配键（"*"）与空键不得暴露为具体模型名。
func TestAvailableModelsCursorSupplySameSourceO57A(t *testing.T) {
	groupID := int64(109)
	openaiAcc := Account{
		ID:          7001,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5.1": "gpt-5.1"}},
	}
	cursorAcc := Account{
		ID:          7002,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		// 无 QuotaKeyID：列表场景 key 终态门放行
		Credentials: map[string]any{"model_mapping": map[string]any{
			"gpt-6-sol": "upstream-6-sol",
			"*":         "upstream-wild",  // 通配键：不得作为具体模型名暴露
			"":          "upstream-empty", // 空键：不得暴露
		}},
	}
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {openaiAcc, cursorAcc}}}
	svc := newCodeBuddyModelsListService(repo)

	models := svc.GetAvailableModels(context.Background(), &groupID, PlatformOpenAI)
	require.Contains(t, models, "gpt-5.1", "原生 openai 账号的映射键应出现")
	require.Contains(t, models, "gpt-6-sol", "cursor 跨平台供给账号的具体映射键应出现在 openai 组列表（同源）")
	require.NotContains(t, models, "*", "通配键不得作为具体模型名暴露")
	require.NotContains(t, models, "", "空键不得暴露")
}

// TestAvailableModelsCursorExcludedForNonBridgeablePlatformO57A 钉死反向隔离：
// muse 等非桥接平台分组即便绑定了 cursor 账号，公开列表也不得暴露 cursor 供给。
func TestAvailableModelsCursorExcludedForNonBridgeablePlatformO57A(t *testing.T) {
	groupID := int64(109)
	cursorAcc := Account{
		ID:          7002,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-6-sol": "upstream-6-sol"}},
	}
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {cursorAcc}}}
	svc := newCodeBuddyModelsListService(repo)

	models := svc.GetAvailableModels(context.Background(), &groupID, PlatformMuse)
	require.NotContains(t, models, "gpt-6-sol", "muse 等非桥接平台分组不得暴露 cursor 供给")
}

// TestAvailableModelsMuseEmptyGroupReturnsNilO57A 钉死失败闭合：muse 分组无真实供给
// 账号时公开列表返回真实空（nil），不回落到 Claude 默认模型（handler 层据此返回空）。
func TestAvailableModelsMuseEmptyGroupReturnsNilO57A(t *testing.T) {
	groupID := int64(109)
	// 仅绑定了一个 cursor 账号的 muse 分组：cursor 不被桥接平台接纳，过滤后无候选。
	cursorAcc := Account{
		ID:          7002,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-6-sol": "upstream-6-sol"}},
	}
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {cursorAcc}}}
	svc := newCodeBuddyModelsListService(repo)

	require.Nil(t, svc.GetAvailableModels(context.Background(), &groupID, PlatformMuse),
		"空 muse 分组须返回真实空列表，不得回落默认目录")
}

// TestAvailableModelsClaudeGeminiPreciseIDsAppearO57A 钉死既有契约不被回归破坏：
// anthropic/gemini 分组公开列表须包含账号映射声明的精确模型 ID。
func TestAvailableModelsClaudeGeminiPreciseIDsAppearO57A(t *testing.T) {
	groupID := int64(109)
	claudeAcc := Account{
		ID:          7101,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{"claude-fable-5.1": "claude-fable-5.1"}},
	}
	geminiAcc := Account{
		ID:          7102,
		Platform:    PlatformGemini,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{"gemini-3.1-pro": "gemini-3.1-pro"}},
	}
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {claudeAcc, geminiAcc}}}
	svc := newCodeBuddyModelsListService(repo)

	claudeModels := svc.GetAvailableModels(context.Background(), &groupID, PlatformAnthropic)
	require.Contains(t, claudeModels, "claude-fable-5.1", "anthropic 分组须暴露精确模型 ID")

	geminiModels := svc.GetAvailableModels(context.Background(), &groupID, PlatformGemini)
	require.Contains(t, geminiModels, "gemini-3.1-pro", "gemini 分组须暴露精确模型 ID")
}

// aiStudioAccountRepoStub 模拟快照展开后的 gemini 桶候选超集：真实场景里 gemini 分组桶
// 经 cursorSupplyPlatformsFor 把 cursor 供给账号并入，故本次查询返回 gemini + cursor 账号。
// 本替身直接返回该超集，用以隔离验证 SelectAccountForAIStudioEndpoints 的 cursor 剔除。
type aiStudioAccountRepoStub struct {
	AccountRepository
	expanded map[int64][]Account
}

func (s *aiStudioAccountRepoStub) ListSchedulableByGroupIDAndPlatforms(_ context.Context, groupID int64, _ []string) ([]Account, error) {
	return s.expanded[groupID], nil
}

// TestSelectAccountForAIStudioEndpointsExcludesCursorO57A 钉死 Gemini /v1beta/models
// 候选必须剔除 cursor 供给账号：即便 cursor 账号被并入该 gemini 分组桶，也不能被误选去
// 请求 cursor 原生端点。
func TestSelectAccountForAIStudioEndpointsExcludesCursorO57A(t *testing.T) {
	groupID := int64(5501)
	geminiAcc := Account{
		ID:          5501,
		Platform:    PlatformGemini,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"api_key": "gem-key"},
	}
	cursorAcc := Account{
		ID:          5502,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{"cursor-sonnet": "x"}},
	}
	repo := &aiStudioAccountRepoStub{expanded: map[int64][]Account{groupID: {geminiAcc, cursorAcc}}}
	svc := &GeminiMessagesCompatService{schedulerSnapshot: nil, accountRepo: repo}

	selected, err := svc.SelectAccountForAIStudioEndpoints(context.Background(), &groupID)
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.Equal(t, PlatformGemini, selected.Platform,
		"AI Studio 模型列表端点候选必须排除 cursor 供给账号")
	require.Equal(t, geminiAcc.ID, selected.ID)
}
