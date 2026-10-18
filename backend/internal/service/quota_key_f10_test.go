//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CUR-F10 R7 回修验证。
//
//	R7-1：手动挂组口径收窄到「配置勾选模型 ∩ 分组白名单」（与 ListAllowedModels 同源）。
//	R7-3：上游 msg 原文逐字回显（分类用裁剪副本，message 存原文）。
//	R7-2：无到达证据，仅登记不修（见 /root/cursor-f10-report.md）。
// ---------------------------------------------------------------------------

// fakeAutoEntryConfigProvider 是测试用 AutoEntryConfigProvider 替身：固定返回构造的配置，
// ApplyEntryDefaults 直接透传（R7-1 测试不依赖入库属性写入）。
type fakeAutoEntryConfigProvider struct {
	cfg *CursorAutoEntryConfig
}

func (f *fakeAutoEntryConfigProvider) GetConfig(_ context.Context) (*CursorAutoEntryConfig, error) {
	return f.cfg, nil
}

func (f *fakeAutoEntryConfigProvider) ApplyEntryDefaults(_ context.Context, _ *ent.Tx, acc *ent.Account, _ *CursorAutoEntryConfig) (*ent.Account, error) {
	return acc, nil
}

// TestCURF10_R7_1_ManualMountIgnoresSelectionUnderP1B2：P1-B2 后手动挂组不再按"人工勾选"
// 收窄——挂的是【静态实供清单 ∩ 模型所属平台的目标分组 ∩ 白名单实际覆盖】。
// 配置只勾 grok-4.7 时：分组 A（覆盖另一实供模型 gpt-6-sol）与 B（覆盖 grok-4.7）都挂；
// 分组 C（未启用白名单，allow-all 无对应模型）不挂。
func TestCURF10_R7_1_ManualMountIgnoresSelectionUnderP1B2(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// R10-1（CUR-F13）：删除"清全表分组/挂载链接"的隔离段——该段越界清掉共享库里他用例
	// 的分组数据。改为"确定性构造 + 全等断言"：本用例只自建 A/B/C，断言 findCursorPlatformGroups
	// 返回集合 == 本用例覆盖模型应挂载的集合 {idA, idB}（P1-B2 U7：allow-all 组 C 无对应模型不挂）；
	// 若共享库被他用例遗留 allow-all 组（Enabled=false，Allows 恒真）或覆盖实供模型的启用组，
	// 会使断言失败并暴露污染，而非旧版清表掩盖。自建分组由 t.Cleanup 按 ID 清理
	// （先删挂载链接再删分组），不触碰他用例数据。
	createdGroupIDs := []int64{}
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range createdGroupIDs {
			_, _ = client.AccountGroup.Delete().
				Where(accountgroup.GroupIDEQ(gid)).Exec(cctx)
			_, _ = client.Group.Delete().
				Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
	})

	// 配置：仅勾选 grok-4.7（保留全部厂商开关 → grok-4.7 真实归属厂商仍开）。
	cfg := DefaultAutoEntryConfig()
	cfg.Models = []CursorModelRef{{ID: "grok-4.7"}}
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{})
	svc.SetAutoEntry(&fakeAutoEntryConfigProvider{cfg: cfg})

	// 分组 A：白名单启用，仅覆盖 gpt-6-sol（实供清单内但未被本配置勾选）→ 仍挂
	// （P1-B2：挂载不再受人工勾选收窄）。
	idA := qkF7MustCreateGroup(t, client, "f10-A-gpt6sol", domain.GroupModelAllowlist{
		Enabled: true, Models: []string{"gpt-6-sol"},
	})
	createdGroupIDs = append(createdGroupIDs, idA)
	// 分组 B：白名单启用，覆盖实供模型 grok-4.7 → 挂。
	idB := qkF7MustCreateGroup(t, client, "f10-B-grok47", domain.GroupModelAllowlist{
		Enabled: true, Models: []string{"grok-4.7"},
	})
	createdGroupIDs = append(createdGroupIDs, idB)
	// 分组 C：未启用白名单（allow-all，无对应模型）→ P1-B2 起不挂。
	idC := qkF7MustCreateGroup(t, client, "f10-C-disabled", domain.GroupModelAllowlist{
		Enabled: false, Models: nil,
	})
	createdGroupIDs = append(createdGroupIDs, idC)

	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	got, err := svc.findCursorPlatformGroups(ctx, tx)
	require.NoError(t, err)
	_ = tx.Rollback()

	// R10-1 全等断言：返回集合应精确等于本用例应挂载集合 {idA, idB}（C 无对应模型，不挂）。
	// 共享库若有遗留 allow-all 或覆盖实供模型的启用组，会混入结果使全等断言失败并暴露污染
	// （而非旧版清表掩盖的静默假绿）。
	require.ElementsMatch(t, []int64{idA, idB}, got,
		"mounted group set must equal exactly {idA, idB} (R10-1 exact assertion)")
}

// TestCURF10_R7_1_EmptySelection_MountsDisabledOnly：唯一候选分组为未启用白名单的
// allow-all 聚合组（无对应模型）→ P1-B2 U7 下不构成目标分组，手动拉号走零覆盖失败关闭
// （号不入库，不落裸号）。
//
// 语义演进：R9-3（CUR-F12，2026-10-17）判定"勾选为空不可挂 allow-all 组"；P1-B2 后勾选
// 不再是挂载输入，allow-all 组（enabled=false，Allows 恒真、无对应模型）恒不被挂载——
// 本用例以 allow-all 聚合组唯一候选制造零覆盖，失败关闭语义（ErrQuotaKeyModelNoCoverage）
// 与自动链路一致，且不落裸号。
func TestCURF10_R7_1_EmptySelection_MountsDisabledOnly(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// R10-1（CUR-F13）：删除"清全表分组"的隔离段，改为按 ID 清理本用例自建分组。
	createdGroupIDs := []int64{}
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range createdGroupIDs {
			_, _ = client.AccountGroup.Delete().
				Where(accountgroup.GroupIDEQ(gid)).Exec(cctx)
			_, _ = client.Group.Delete().
				Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
	})

	// 勾选为空：合法空集（用户全关）。P1-B2 下勾选不参与挂载判定——本用例零覆盖
	// 由"唯一 allow-all 组不构成目标分组"单独造成，而非空勾选。
	cfg := DefaultAutoEntryConfig()
	cfg.Models = []CursorModelRef{}
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "f10-empty@x.com", Platform: "cursor", Type: "oauth",
				Credentials: map[string]any{"access_token": "AT"}},
		}},
	})
	svc.SetAutoEntry(&fakeAutoEntryConfigProvider{cfg: cfg})

	// 仅一个未启用白名单的分组（allow-all，Allows 恒真、无对应模型）。R9-3 后勾选空不收录；
	// P1-B2 后 allow-all 恒不构成目标分组。
	idC := qkF7MustCreateGroup(t, client, "f10-E-disabled", domain.GroupModelAllowlist{
		Enabled: false, Models: nil,
	})
	createdGroupIDs = append(createdGroupIDs, idC)

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	err := svc.RefreshAccounts(ctx, qk.ID)
	require.Error(t, err, "allow-all-only surroundings must fail-closed (P1-B2 U7)")
	require.True(t, errors.Is(err, ErrQuotaKeyModelNoCoverage),
		"allow-all-only surroundings must surface ErrQuotaKeyModelNoCoverage; got=%v", err)
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"allow-all-only surroundings must also surface ErrQuotaKeyMountFailed; got=%v", err)

	accs, qerr := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, qerr)
	require.Len(t, accs, 0, "zero coverage must not persist any account (no bare account)")
	_ = idC
}

// TestCURF10_R7_1_NoCoverage_FailClosed：无任何目标分组覆盖静态实供清单
// （唯一分组 Enabled=true 但只覆盖非实供模型）→ 零覆盖，失败关闭（不落裸号）。
//
// P1-B2 注：旧用例以"勾选为空"制造零覆盖；勾选不再是挂载输入后，零覆盖改由"无分组覆盖
// 静态清单"制造（勾选态无关），失败关闭语义（ErrQuotaKeyModelNoCoverage + 不落裸号）不变。
func TestCURF10_R7_1_NoCoverage_FailClosed(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// R10-1（CUR-F13）：删除"清全表分组"的隔离段，改为按 ID 清理本用例自建分组。
	createdGroupIDs := []int64{}
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range createdGroupIDs {
			_, _ = client.AccountGroup.Delete().
				Where(accountgroup.GroupIDEQ(gid)).Exec(cctx)
			_, _ = client.Group.Delete().
				Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
	})

	cfg := DefaultAutoEntryConfig()
	cfg.Models = []CursorModelRef{}
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "f10-nocov@x.com", Platform: "cursor", Type: "oauth",
				Credentials: map[string]any{"access_token": "AT"}},
		}},
	})
	svc.SetAutoEntry(&fakeAutoEntryConfigProvider{cfg: cfg})

	// 唯一分组 Enabled=true 但只覆盖非实供模型 → 静态清单零覆盖（勾选态已不参与判定）。
	idNC := qkF7MustCreateGroup(t, client, "f10-NC-enabled", domain.GroupModelAllowlist{
		Enabled: true, Models: []string{"deepseek-v4-1-flash"},
	})
	createdGroupIDs = append(createdGroupIDs, idNC)

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	err := svc.RefreshAccounts(ctx, qk.ID)
	require.Error(t, err, "no group covering the static supply list must fail-closed")
	require.True(t, errors.Is(err, ErrQuotaKeyModelNoCoverage),
		"zero coverage must surface ErrQuotaKeyModelNoCoverage; got=%v", err)
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"zero coverage must also surface ErrQuotaKeyMountFailed; got=%v", err)

	accs, qerr := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, qerr)
	require.Len(t, accs, 0, "zero coverage must not persist any account (no bare account)")
}

// TestCURF10_R7_3_MessagePreservesWhitespace：classifyUpstreamError 必须将上游 msg 原文
// （含首尾空格）逐字存入 Message()，仅用 TrimSpace 副本做分类匹配；分类 Kind 不受空格影响。
func TestCURF10_R7_3_MessagePreservesWhitespace(t *testing.T) {
	// 过期类，含首尾空格。
	exp := classifyUpstreamError("  密钥已过期  ")
	var expUpe *UpstreamPullError
	require.True(t, errors.As(exp, &expUpe), "expired must be *UpstreamPullError")
	require.True(t, errors.Is(exp, ErrUpstreamKeyExpired), "whitespace must not affect expired classification")
	require.Equal(t, "  密钥已过期  ", expUpe.Message(),
		"Message must keep raw msg including surrounding whitespace")

	// 冷却类，含首尾空格。
	cd := classifyUpstreamError("  每5分钟内仅可刷新1次  ")
	var cdUpe *UpstreamPullError
	require.True(t, errors.As(cd, &cdUpe))
	require.True(t, errors.Is(cd, ErrUpstreamCooldown), "whitespace must not affect cooldown classification")
	require.Equal(t, "  每5分钟内仅可刷新1次  ", cdUpe.Message())

	// 未知类，含首尾空格。
	unk := classifyUpstreamError("  系统当前繁忙，请稍后再试  ")
	var unkUpe *UpstreamPullError
	require.True(t, errors.As(unk, &unkUpe))
	require.Equal(t, UpstreamUnknown, unkUpe.Kind, "whitespace must not affect unknown classification")
	require.Equal(t, "  系统当前繁忙，请稍后再试  ", unkUpe.Message())
}

// TestCURF10_R7_3_ActivatePreservesWhitespace：activate 业务码路径同样把含首尾空格的上游
// msg 原文逐字传入手动响应 message（经 classifyUpstreamError 产出 raw 后透传）。
func TestCURF10_R7_3_ActivatePreservesWhitespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":400,"msg":"  密钥已过期  "}`))
	}))
	defer srv.Close()

	client := &shareAIUpstreamClient{
		httpClient:   srv.Client(),
		primaryBase:  srv.URL,
		fallbackBase: srv.URL,
	}
	err := client.activate(context.Background(), "anycode", "DID12345")
	require.Error(t, err, "activate non-zero code must surface an error")

	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe), "activate non-zero code must return *UpstreamPullError")
	require.Equal(t, UpstreamExpired, upe.Kind)
	require.Equal(t, "  密钥已过期  ", upe.Message(),
		"activate message must keep raw upstream msg including surrounding whitespace")
	require.True(t, errors.Is(err, ErrUpstreamKeyExpired), "expired activate msg must be classified as expired")
}
