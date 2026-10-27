//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// P1-S：调度资格门 + 来源停供的配对单测。
//
// 覆盖三条 Done when：
//  1. OpenAI 系四链（普通 / 高级 / 粘性 / DB 复核）cursor 资格同口径；
//  2. 原生 Claude/Gemini 链 cursor 可选 + 未挂组不外溢；
//  3. key expired → 关联 cursor 账号全链不可选，且账号 status 未被改写。
//
// 全部断言走包内既有夹具（stub repo / gateway cache），不连 DB、不引入 mock 框架。

const (
	cursorSchedTestGroupID  = int64(7701)
	cursorSchedTestModel    = "cursor-sonnet"
	cursorSchedTestModelAlt = "claude-sonnet-4-5"
)

// cursorQuotaKeyStateStub 是激活码状态读取器的测试替身（合同 §5：替身仅存于测试）。
type cursorQuotaKeyStateStub struct {
	statuses map[int64]string
	err      error
}

func (s cursorQuotaKeyStateStub) CursorQuotaKeyStatus(_ context.Context, quotaKeyID int64) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	status, ok := s.statuses[quotaKeyID]
	if !ok {
		return "", errors.New("quota key not found")
	}
	return status, nil
}

// installCursorQuotaKeyStateForTest 安装替身并在测试结束时还原包级实例，
// 避免污染同包内其它用例（包级单例是共享状态）。
func installCursorQuotaKeyStateForTest(t *testing.T, provider CursorQuotaKeyStateProvider) {
	t.Helper()
	cursorQuotaKeyStateMu.Lock()
	prev := cursorQuotaKeyStateInstance
	cursorQuotaKeyStateInstance = provider
	cursorQuotaKeyStateMu.Unlock()
	t.Cleanup(func() {
		cursorQuotaKeyStateMu.Lock()
		cursorQuotaKeyStateInstance = prev
		cursorQuotaKeyStateMu.Unlock()
	})
}

// cursorSchedFreshUsageSnapshot 返回 A04-G 来源门认可的合法额度快照：真实剩余 > 0，
// 且 checked_at 新鲜。来源门只接受 cursorSupplySourceUsageFreshnessWindow（5 分钟）内的
// 成功值，超窗一律判 quota_key_usage_stale 否决，故这里贴着实时钟取"一分钟前"。
func cursorSchedFreshUsageSnapshot() []byte {
	return []byte(fmt.Sprintf(
		`{"total":400,"used":86,"status_code":200,"checked_at":%q,"error_code":""}`,
		time.Now().Add(-time.Minute).Format(time.RFC3339),
	))
}

// cursorSchedTestAccount 构造一个可参与 cursor 调度的账号（显式挂组 + 声明模型映射）。
func cursorSchedTestAccount(id int64, quotaKeyID *int64, model string) Account {
	return Account{
		ID:          id,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		QuotaKeyID:  quotaKeyID,
		GroupIDs:    []int64{cursorSchedTestGroupID},
		Credentials: map[string]any{"model_mapping": map[string]any{model: "upstream-" + model}},
	}
}

// cursorSchedTestAdvancedFixture 装配高级/粘性链所需的 service + scheduler。
func cursorSchedTestAdvancedFixture(accounts []Account) (*OpenAIGatewayService, *defaultOpenAIAccountScheduler) {
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                &config.Config{},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	return svc, &defaultOpenAIAccountScheduler{service: svc, stats: newOpenAIAccountRuntimeStats()}
}

// TestOpenAIAccountEligibilityCursorKeyExpiredSameAcrossChains 钉死「四链同口径」：
// 普通链（openAICompatibleAccountEligibilityFailureReasonBeforeProfit）、高级链
// （selectByLoadBalance）、粘性链（selectBySessionHash）、DB 复核链
// （recheckSelectedOpenAIAccountFromDB）对同一个 expired key 的 cursor 账号给出同一结论。
func TestOpenAIAccountEligibilityCursorKeyExpiredSameAcrossChains(t *testing.T) {
	ctx := context.Background()
	groupID := cursorSchedTestGroupID
	activeKey := int64(9101)
	expiredKey := int64(9102)

	// A04-G 后的生产装配成对安装「状态 + 用量快照」两个窄读取器（quota_key_service.go:387-390），
	// 任一缺装即被来源门判为装配错误 → 折叠成 quota_key_state_unavailable 失败关闭。
	// 因此 active key 必须同时具备合法新鲜快照（expired key 在状态门即短路，快照不会被读取）。
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{
			activeKey:  domain.QuotaKeyStatusActive,
			expiredKey: domain.QuotaKeyStatusExpired,
		},
		snapshots: map[int64][]byte{activeKey: cursorSchedFreshUsageSnapshot()},
	})

	active := cursorSchedTestAccount(8101, &activeKey, cursorSchedTestModel)
	expired := cursorSchedTestAccount(8102, &expiredKey, cursorSchedTestModel)

	// (a) 谓词层：四链共用的唯一出口
	require.Equal(t, "quota_key_expired", cursorAccountQuotaKeyVetoReason(ctx, &expired))
	require.Empty(t, cursorAccountQuotaKeyVetoReason(ctx, &active))
	// 非 cursor 平台 / 未绑码的账号恒不受本门影响
	unbound := cursorSchedTestAccount(8103, nil, cursorSchedTestModel)
	require.Empty(t, cursorAccountQuotaKeyVetoReason(ctx, &unbound))
	openaiAcc := Account{ID: 8104, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, QuotaKeyID: &expiredKey}
	require.Empty(t, cursorAccountQuotaKeyVetoReason(ctx, &openaiAcc))

	// (b) 普通链
	require.Empty(t, openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		ctx, &active, PlatformCursor, cursorSchedTestModel, false, ""))
	require.Equal(t, "quota_key_expired", openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		ctx, &expired, PlatformCursor, cursorSchedTestModel, false, ""))

	// (c) DB 复核链
	svc, _ := cursorSchedTestAdvancedFixture([]Account{active, expired})
	require.NotNil(t, svc.recheckSelectedOpenAIAccountFromDB(
		ctx, &active, &groupID, PlatformCursor, cursorSchedTestModel, false, ""))
	require.Nil(t, svc.recheckSelectedOpenAIAccountFromDB(
		ctx, &expired, &groupID, PlatformCursor, cursorSchedTestModel, false, ""))
}

// TestCursorSchedulingAdvancedPoolExcludesExpiredKey 钉死高级候选链：expired key 的
// cursor 账号被剔出候选池（并留下可诊断的 filter reason），healthy 的则正常入选。
func TestCursorSchedulingAdvancedPoolExcludesExpiredKey(t *testing.T) {
	ctx := context.Background()
	groupID := cursorSchedTestGroupID
	activeKey := int64(9201)
	expiredKey := int64(9202)

	// 同上：active key 需装齐用量快照依赖，否则来源门先因装配错误否决，
	// 候选池里的账号根本走不到"是否过期"的判定。
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{
			activeKey:  domain.QuotaKeyStatusActive,
			expiredKey: domain.QuotaKeyStatusExpired,
		},
		snapshots: map[int64][]byte{activeKey: cursorSchedFreshUsageSnapshot()},
	})

	req := OpenAIAccountScheduleRequest{
		GroupID:        &groupID,
		Platform:       PlatformCursor,
		RequestedModel: cursorSchedTestModel,
	}

	expiredOnly := cursorSchedTestAccount(8201, &expiredKey, cursorSchedTestModel)
	_, schedulerExpired := cursorSchedTestAdvancedFixture([]Account{expiredOnly})
	_, _, _, _, err := schedulerExpired.selectByLoadBalance(ctx, req)
	require.ErrorIs(t, err, ErrNoAvailableAccounts, "expired key 的 cursor 账号不得进入高级候选池")
	require.Contains(t, err.Error(), "quota_key_expired")

	activeOnly := cursorSchedTestAccount(8202, &activeKey, cursorSchedTestModel)
	_, schedulerActive := cursorSchedTestAdvancedFixture([]Account{activeOnly})
	selection, _, _, _, err := schedulerActive.selectByLoadBalance(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, activeOnly.ID, selection.Account.ID)
}

// TestCursorSchedulingStickyChainExcludesExpiredKey 钉死粘性链：粘性命中的 cursor 账号若
// 归属码已过期，绑定被清理、本轮不选（回落其它链），账号本体不动。
func TestCursorSchedulingStickyChainExcludesExpiredKey(t *testing.T) {
	ctx := context.Background()
	groupID := cursorSchedTestGroupID
	activeKey := int64(9301)
	expiredKey := int64(9302)

	// 同上：粘性命中后仍要过来源门，active key 必须装齐用量快照依赖。
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{
			activeKey:  domain.QuotaKeyStatusActive,
			expiredKey: domain.QuotaKeyStatusExpired,
		},
		snapshots: map[int64][]byte{activeKey: cursorSchedFreshUsageSnapshot()},
	})

	active := cursorSchedTestAccount(8301, &activeKey, cursorSchedTestModel)
	expired := cursorSchedTestAccount(8302, &expiredKey, cursorSchedTestModel)
	_, scheduler := cursorSchedTestAdvancedFixture([]Account{active, expired})

	stickyReq := func(accountID int64) OpenAIAccountScheduleRequest {
		return OpenAIAccountScheduleRequest{
			GroupID:         &groupID,
			Platform:        PlatformCursor,
			SessionHash:     "sess-cursor-p1s",
			StickyAccountID: accountID,
			RequestedModel:  cursorSchedTestModel,
		}
	}

	selection, _, err := scheduler.selectBySessionHash(ctx, stickyReq(expired.ID))
	require.NoError(t, err)
	require.Nil(t, selection, "expired key 的 cursor 账号不得经粘性会话被选中")

	selection, _, err = scheduler.selectBySessionHash(ctx, stickyReq(active.ID))
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, active.ID, selection.Account.ID)
}

// TestCursorSchedulingUnavailableKeyStateFailsClosed 钉死失败关闭：读取激活码状态出错时，
// cursor 账号本次不予入选（宁可不选，也不把可能已停供的号放进候选）。
func TestCursorSchedulingUnavailableKeyStateFailsClosed(t *testing.T) {
	ctx := context.Background()
	installCursorQuotaKeyStateForTest(t, cursorQuotaKeyStateStub{err: errors.New("quota key store unreachable")})

	keyID := int64(9401)
	account := cursorSchedTestAccount(8401, &keyID, cursorSchedTestModel)
	require.Equal(t, "quota_key_state_unavailable", cursorAccountQuotaKeyVetoReason(ctx, &account))
	require.Equal(t, "quota_key_state_unavailable", openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		ctx, &account, PlatformCursor, cursorSchedTestModel, false, ""))
}

// TestCursorSchedulingNoProviderInstalledDoesNotTighten 的用例名沿用历史（原断言
// 「未装配不收紧」已被 A04-G 严格门废弃），本用例现钉死新契约的失败关闭行为。
//
// 依据：commit c50b0edf8 把 cursorSupplySourceAvailabilityVetoReason 改为「状态 provider
// 或用量快照 provider 任一未装配 → 返回 error」，各调度链再经
// cursorSupplySourceAvailabilityVetoReasonString 把该 error 折叠成
// quota_key_state_unavailable（失败关闭）。生产装配总是成对安装两个 provider
// （quota_key_service.go:387-390），所以"未装配"只可能是装配错误，不得当作"无否决"放行。
func TestCursorSchedulingNoProviderInstalledDoesNotTighten(t *testing.T) {
	ctx := context.Background()
	clearCursorQuotaKeySourceProvidersForTest(t)

	keyID := int64(9501)
	account := cursorSchedTestAccount(8501, &keyID, cursorSchedTestModel)

	// 共同来源门本体：依赖未装配是装配错误，必须如实上抛而不是静默放行。
	reason, err := cursorSupplySourceAvailabilityVetoReason(ctx, &account)
	require.Error(t, err, "两个窄读取器都未装配时必须失败关闭")
	require.Contains(t, err.Error(), "state provider is not wired")
	require.Empty(t, reason)

	// 既有调度路径的兼容投影：error 折叠成 quota_key_state_unavailable。
	require.Equal(t, "quota_key_state_unavailable",
		cursorSupplySourceAvailabilityVetoReasonString(ctx, &account))
	require.Equal(t, "quota_key_state_unavailable", openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		ctx, &account, PlatformCursor, cursorSchedTestModel, false, ""))
}

// cursorSchedTestNativeGroup 构造注入 ctx 的原生（Claude/Gemini）分组，供准入谓词读取。
func cursorSchedTestNativeGroup(t *testing.T, platform string, allowlist []string) context.Context {
	t.Helper()
	group := &Group{
		ID:       cursorSchedTestGroupID,
		Platform: platform,
		Status:   StatusActive,
		Hydrated: true,
	}
	if allowlist != nil {
		group.ModelAllowlist = GroupModelAllowlist{Enabled: true, Models: allowlist}
	}
	return context.WithValue(context.Background(), ctxkey.Group, group)
}

// TestCursorSchedulableNativePlatformAdmission 钉死原生 Claude/Gemini 链的准入三条：
// 显式挂组 + 分组白名单允许 + 账号 model_mapping 声明该模型；未挂组一律不外溢。
func TestCursorSchedulableNativePlatformAdmission(t *testing.T) {
	groupID := cursorSchedTestGroupID
	svc := &GatewayService{}
	activeKey := int64(9601)
	// 跨平台准入在挂组/白名单判定后仍消费来源门，故必须装齐状态 + 用量快照两个窄读取器。
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses:  map[int64]string{activeKey: domain.QuotaKeyStatusActive},
		snapshots: map[int64][]byte{activeKey: cursorSchedFreshUsageSnapshot()},
	})

	allowAll := cursorSchedTestNativeGroup(t, PlatformAnthropic, []string{cursorSchedTestModelAlt})
	admitted := cursorSchedTestAccount(8601, &activeKey, cursorSchedTestModelAlt)

	// ① 组内挂载 + 白名单允许 + 映射声明 → 可选
	require.True(t, svc.isAccountAdmittedForPlatform(allowAll, &admitted, PlatformAnthropic, false, cursorSchedTestModelAlt, &groupID))
	// Gemini 同理
	geminiCtx := cursorSchedTestNativeGroup(t, PlatformGemini, []string{cursorSchedTestModelAlt})
	require.True(t, svc.isAccountAdmittedForPlatform(geminiCtx, &admitted, PlatformGemini, false, cursorSchedTestModelAlt, &groupID))

	// ② 未挂组（groupID 为 nil）→ 不外溢到别的平台候选池
	require.False(t, svc.isAccountAdmittedForPlatform(allowAll, &admitted, PlatformAnthropic, false, cursorSchedTestModelAlt, nil))

	// ③ 白名单不含该模型 → 不可选
	otherModelCtx := cursorSchedTestNativeGroup(t, PlatformAnthropic, []string{"gpt-5.1"})
	require.False(t, svc.isAccountAdmittedForPlatform(otherModelCtx, &admitted, PlatformAnthropic, false, cursorSchedTestModelAlt, &groupID))

	// ④ 模型映射是唯一出口：未声明该模型 → 不可选
	require.False(t, svc.isAccountAdmittedForPlatform(allowAll, &admitted, PlatformAnthropic, false, "gpt-5.1", &groupID))

	// ⑤ 桥接平台扩展（O57-A §1）：U7 已挂组的 openai 兼容族（grok/openai）现接纳
	//    cursor 跨平台供给——分组挂组 + 模型白名单允许 + 账号映射声明 → 可选；
	//    未挂组不外溢，多协议族（composite）仍不接纳。
	require.True(t, svc.isAccountAdmittedForPlatform(allowAll, &admitted, PlatformOpenAI, false, cursorSchedTestModelAlt, &groupID),
		"openai 兼容族已挂组时应接纳 cursor 供给")
	require.True(t, svc.isAccountAdmittedForPlatform(allowAll, &admitted, PlatformGrok, false, cursorSchedTestModelAlt, &groupID),
		"grok 已挂组时应接纳 cursor 供给")
	require.False(t, svc.isAccountAdmittedForPlatform(allowAll, &admitted, PlatformComposite, false, cursorSchedTestModelAlt, &groupID),
		"多协议族（composite）不接纳 cursor 跨平台供给")
	// 未挂组 → 即便平台可桥接也不外溢
	require.False(t, svc.isAccountAdmittedForPlatform(allowAll, &admitted, PlatformOpenAI, false, cursorSchedTestModelAlt, nil))
	// 模型不在白名单 → 不可选（即便平台可桥接）
	require.False(t, svc.isAccountAdmittedForPlatform(otherModelCtx, &admitted, PlatformOpenAI, false, cursorSchedTestModelAlt, &groupID))

	// ⑥ 同平台原生账号不受本门影响
	native := Account{ID: 8602, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true}
	require.True(t, svc.isAccountAdmittedForPlatform(allowAll, &native, PlatformAnthropic, false, cursorSchedTestModelAlt, &groupID))
}

// TestCursorSchedulableNativePlatformQuerySet 钉死候选查询集合：cursor 只在「原生平台 +
// 显式挂组」时被并入多平台查询，未挂组或非原生平台时不外溢。
func TestCursorSchedulableNativePlatformQuerySet(t *testing.T) {
	groupID := cursorSchedTestGroupID

	require.Equal(t, []string{PlatformAnthropic, PlatformCursor},
		cursorSupplyPlatformsFor(&groupID, []string{PlatformAnthropic}))
	require.Equal(t, []string{PlatformAnthropic, PlatformAntigravity, PlatformCursor},
		cursorSupplyPlatformsFor(&groupID, []string{PlatformAnthropic, PlatformAntigravity}))
	// 未挂组：不并入 cursor
	require.Equal(t, []string{PlatformAnthropic},
		cursorSupplyPlatformsFor(nil, []string{PlatformAnthropic}))
	// OpenAI 兼容族（O57-A §1）：已挂组的 openai 兼容族（grok/openai）现并入 cursor
	// 跨平台供给候选（与 anthropic/gemini 同源），多协议族（composite）仍不并入。
	require.Equal(t, []string{PlatformOpenAI, PlatformCursor},
		cursorSupplyPlatformsFor(&groupID, []string{PlatformOpenAI}))
	require.Equal(t, []string{PlatformGrok, PlatformCursor},
		cursorSupplyPlatformsFor(&groupID, []string{PlatformGrok}))
	// 入参不被就地改写
	base := []string{PlatformGemini}
	_ = cursorSupplyPlatformsFor(&groupID, base)
	require.Equal(t, []string{PlatformGemini}, base)
}

// TestQuotaKeyExpiredBlocksCursorSchedulingWithoutMutatingAccount 钉死 I2 的核心不变量：
// expired 的码只剥夺调度资格，**账号本体不动**——status 未被伪标耗尽、schedulable 未被改写。
func TestQuotaKeyExpiredBlocksCursorSchedulingWithoutMutatingAccount(t *testing.T) {
	ctx := context.Background()
	groupID := cursorSchedTestGroupID
	expiredKey := int64(9701)

	installCursorQuotaKeyStateForTest(t, cursorQuotaKeyStateStub{statuses: map[int64]string{
		expiredKey: domain.QuotaKeyStatusExpired,
	}})

	account := cursorSchedTestAccount(8701, &expiredKey, cursorSchedTestModel)
	statusBefore := account.Status
	schedulableBefore := account.Schedulable

	// 1) 普通链
	require.Equal(t, "quota_key_expired", openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		ctx, &account, PlatformCursor, cursorSchedTestModel, false, ""))
	// 2) DB 复核链
	svc, scheduler := cursorSchedTestAdvancedFixture([]Account{account})
	require.Nil(t, svc.recheckSelectedOpenAIAccountFromDB(
		ctx, &account, &groupID, PlatformCursor, cursorSchedTestModel, false, ""))
	// 3) 高级链
	_, _, _, _, err := scheduler.selectByLoadBalance(ctx, OpenAIAccountScheduleRequest{
		GroupID: &groupID, Platform: PlatformCursor, RequestedModel: cursorSchedTestModel,
	})
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	// 4) 粘性链
	selection, _, err := scheduler.selectBySessionHash(ctx, OpenAIAccountScheduleRequest{
		GroupID: &groupID, Platform: PlatformCursor, SessionHash: "sess-p1s-expired",
		StickyAccountID: account.ID, RequestedModel: cursorSchedTestModel,
	})
	require.NoError(t, err)
	require.Nil(t, selection)
	// 5) 原生链（显式挂组也因 key 终态被拒）
	nativeCtx := cursorSchedTestNativeGroup(t, PlatformAnthropic, []string{cursorSchedTestModel})
	require.False(t, (&GatewayService{}).isAccountAdmittedForPlatform(
		nativeCtx, &account, PlatformAnthropic, false, cursorSchedTestModel, &groupID))

	// 账号本体未被改写（I2：不把账号伪标耗尽、不软删）
	require.Equal(t, statusBefore, account.Status)
	require.Equal(t, schedulableBefore, account.Schedulable)
	require.Equal(t, domain.StatusActive, account.Status)
	require.True(t, account.Schedulable)
}

// TestQuotaKeyExpiredStatusGateSemantics 钉死停供判定与状态归一化：
// 只有 expired（含 expires_at 已到期的等价态）停供；exhausted/active 不停供。
func TestQuotaKeyExpiredStatusGateSemantics(t *testing.T) {
	require.True(t, quotaKeyStatusBlocksCursorScheduling(domain.QuotaKeyStatusExpired))
	require.False(t, quotaKeyStatusBlocksCursorScheduling(domain.QuotaKeyStatusExhausted))
	require.False(t, quotaKeyStatusBlocksCursorScheduling(domain.QuotaKeyStatusActive))

	installCursorQuotaKeyStateForTest(t, nil) // NewQuotaKeyService 会安装包级实例，结束时还原

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	repo := newFakeQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: domain.QuotaKeyStatusExpired}
	repo.byID[2] = &ent.QuotaKey{ID: 2, Status: domain.QuotaKeyStatusActive, ExpiresAt: &past}
	repo.byID[3] = &ent.QuotaKey{ID: 3, Status: domain.QuotaKeyStatusActive, ExpiresAt: &future}
	repo.byID[4] = &ent.QuotaKey{ID: 4, Status: domain.QuotaKeyStatusExhausted}

	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)
	ctx := context.Background()

	// 已落盘 expired
	status, err := svc.CursorQuotaKeyStatus(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusExpired, status)
	// expires_at 已到期但 sweeper 尚未落盘 → 归一为 expired（同一事实的两个时序切片）
	status, err = svc.CursorQuotaKeyStatus(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusExpired, status)
	// 未到期 → 原样返回，不停供
	status, err = svc.CursorQuotaKeyStatus(ctx, 3)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusActive, status)
	// exhausted 是拉号侧占位，不是供给终态
	status, err = svc.CursorQuotaKeyStatus(ctx, 4)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusExhausted, status)
	require.False(t, quotaKeyStatusBlocksCursorScheduling(status))
	// 码不存在 → error，由资格门失败关闭
	_, err = svc.CursorQuotaKeyStatus(ctx, 99)
	require.Error(t, err)
}
