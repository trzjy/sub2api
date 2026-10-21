//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// R1-1：手动刷新错误原文必须到达页面 + 未知结果不得显示成功。
// 验证：手动链路（RefreshAccounts）返回的错误携带上游中文 msg 原文（Cooldown/Expired/Unknown）；
// 自动链路（PullAccountsForModel）对未知结果落失败标记（exhausted 保持 false：未知失败不得标
// 耗尽，CUR-P1-FIX-KEY-EXHAUSTED），且不再静默成功。
// ---------------------------------------------------------------------------

func TestCURF4_RefreshAccounts_CooldownCarriesOriginalMessage(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		err: NewUpstreamCooldownError("每5分钟内仅可刷新1次，请3分钟后再试"),
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.RefreshAccounts(ctx, qk.ID)
	require.Error(t, err, "manual refresh must surface the upstream error (not success)")
	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe), "manual refresh must return *UpstreamPullError")
	require.Equal(t, UpstreamCooldown, upe.Kind)
	require.Contains(t, upe.Message(), "每5分钟内仅可刷新1次，请3分钟后再试",
		"cooldown response message must carry the original Chinese msg verbatim")

	// 落库：冷却豁免 exhausted（B2），但标记已写入。
	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, got.Exhausted, "cooldown must NOT set exhausted")
	require.NotNil(t, got.LastRefreshError)
}

func TestCURF4_RefreshAccounts_ExpiredCarriesOriginalMessage(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		err: NewUpstreamExpiredError("密钥额度耗尽或已过期"),
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.RefreshAccounts(ctx, qk.ID)
	require.Error(t, err, "manual refresh must surface the upstream error (not success)")
	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe), "manual refresh must return *UpstreamPullError")
	require.Equal(t, UpstreamExpired, upe.Kind)
	require.Contains(t, upe.Message(), "密钥额度耗尽或已过期",
		"expired response message must carry the original Chinese msg verbatim")
}

func TestCURF4_RefreshAccounts_UnknownCarriesOriginalMessage(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		err: NewUpstreamUnknownError("some unknown text"),
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.RefreshAccounts(ctx, qk.ID)
	require.Error(t, err, "manual refresh must NOT report success on unknown upstream result")
	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe), "manual refresh must return *UpstreamPullError")
	require.Equal(t, UpstreamUnknown, upe.Kind)
	require.Contains(t, upe.Message(), "some unknown text",
		"unknown response message must carry the original msg verbatim")
}

// TestCURF4_PullAccountsForModel_UnknownSurfacesBlockedSentinel：自动链路对未知上游结果
// 走 writeUpstreamFailureMarker（落库只写标记、exhausted 保持 false），但不再裸 return nil（R13-2）——改为透传
// 含 ErrUpstreamKeyBlocked 哨兵的包裹错误，使 reconciler 如实计入失败（不再被记成成功）。
// 手动链路仍返回 *UpstreamPullError（F4 语义不变，见 TestCURF4_RefreshAccounts_UnknownCarriesOriginalMessage）。
func TestCURF4_PullAccountsForModel_UnknownSurfacesBlockedSentinel(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		err: NewUpstreamUnknownError("boom unknown"),
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	// 自动链路（manual=false）：返回含 ErrUpstreamKeyBlocked 哨兵的包裹错误（不再静默 nil）。
	err := svc.PullAccountsForModel(ctx, qk.ID, "cursor-f4-unknown")
	require.Error(t, err, "automatic chain must surface the blocked sentinel (not silent success)")
	require.True(t, errors.Is(err, ErrUpstreamKeyBlocked),
		"automatic unknown path must wrap ErrUpstreamKeyBlocked, got=%v", err)
	require.Contains(t, err.Error(), "boom unknown",
		"underlying reason summary must be carried in the wrapped error")

	// 落库：失败标记保留原文，exhausted 保持 false（未知失败不是已确认终态，不得停供）。
	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, got.Exhausted, "automatic unknown path must keep exhausted=false (no inferred exhaustion)")
	require.NotNil(t, got.LastRefreshError)
	require.Contains(t, *got.LastRefreshError, "boom unknown",
		"last_refresh_error must preserve the original msg text")
}

// ---------------------------------------------------------------------------
// R1-2：挂组门槛与枚举/水位对齐（active + 未软删）。
// ---------------------------------------------------------------------------

// TestCURF4_FindCursorPlatformGroups_ExcludesNonActiveOrSoftDeleted：挂载解析器
// （findCursorPlatformGroups，U7 唯一判定出口）只取 active + 未软删分组；disabled/软删
// 分组不得被当成目标组导致新号误挂（§20.2-A 后该语义由 cursorGroupResolver 承担）。
func TestCURF4_FindCursorPlatformGroups_ExcludesNonActiveOrSoftDeleted(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// U7（§20.2-A）：挂载按账号模型能力（静态实供清单）判定，用实供模型构造覆盖组。
	modelM := "claude-opus-5-5"

	// 分组 A：active + 白名单含实供模型（应入选）。
	grpA := mustCreateCursorGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{modelM},
	})
	// 分组 B：disabled + 白名单含实供模型（不应入选）。
	grpB := mustCreateCursorGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{modelM},
	})
	require.NoError(t, client.Group.UpdateOneID(grpB.ID).SetStatus(domain.StatusDisabled).Exec(ctx))
	// 分组 C：软删 + 白名单含实供模型（不应入选）。
	grpC := mustCreateCursorGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{modelM},
	})
	require.NoError(t, client.Group.UpdateOneID(grpC.ID).SetDeletedAt(time.Now()).Exec(ctx))

	// 分组 D：active 但白名单不含实供模型（不应入选，白名单判定不变）。
	grpD := mustCreateCursorGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"other-model"},
	})
	// R10-1（CUR-F13）：按 ID 清理本用例自建分组，避免残留共享库污染他用例
	//（覆盖实供模型的启用组会使零覆盖/全等断言用例失败）。
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range []int64{grpA.ID, grpB.ID, grpC.ID, grpD.ID} {
			_, _ = client.Group.Delete().Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
	})

	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	svc, _ := qkNewTestService(t, &fakeUpstreamClient{})
	ids, err := svc.findCursorPlatformGroups(ctx, tx)
	require.NoError(t, err)

	require.Contains(t, ids, grpA.ID, "active+allowlist group must be in the mount set")
	require.NotContains(t, ids, grpB.ID, "disabled group must NOT be in the mount set")
	require.NotContains(t, ids, grpC.ID, "soft-deleted group must NOT be in the mount set")
	require.NotContains(t, ids, grpD.ID, "allowlist-mismatched group must NOT be in the mount set")
}

// ---------------------------------------------------------------------------
// R1-3：关键词分类消灭否定句误判。
// ---------------------------------------------------------------------------

func TestCURF4_ClassifyUpstreamError_NoFalsePositiveOnNegatives(t *testing.T) {
	// 反例（必须覆盖——否定句/普通文案不得误判）。
	negExpired := classifyUpstreamError("密钥未过期")
	require.False(t, errors.Is(negExpired, ErrUpstreamKeyExpired), "否定句'未过期' 不得判过期")
	require.False(t, errors.Is(negExpired, ErrUpstreamCooldown), "否定句'未过期' 不得判冷却")

	negRefresh := classifyUpstreamError("刷新失败")
	require.False(t, errors.Is(negRefresh, ErrUpstreamCooldown), "普通'刷新失败' 不含'仅可刷新'，不得判冷却")
	require.False(t, errors.Is(negRefresh, ErrUpstreamKeyExpired), "普通'刷新失败' 不得判过期")

	// 正例（生产真实文案）。
	cooldown := classifyUpstreamError("每5分钟内仅可刷新1次，请3分钟后再试")
	require.True(t, errors.Is(cooldown, ErrUpstreamCooldown), "生产冷却样本必须判冷却（仅可刷新）")
	require.False(t, errors.Is(cooldown, ErrUpstreamKeyExpired), "冷却样本不得判过期")

	expired1 := classifyUpstreamError("密钥额度耗尽或已过期")
	require.True(t, errors.Is(expired1, ErrUpstreamKeyExpired), "生产过期样本必须判 expired")

	expired2 := classifyUpstreamError("密钥已过期")
	require.True(t, errors.Is(expired2, ErrUpstreamKeyExpired), "'密钥已过期' 必须判 expired")

	// 过期/耗尽优先于限频：含"仅可刷新"且含"已过期"的 msg 应归过期。
	both := classifyUpstreamError("密钥已过期，每5分钟内仅可刷新1次")
	require.True(t, errors.Is(both, ErrUpstreamKeyExpired), "同时命中的 msg 应优先归过期")

	// 未知结果 failure-closed：原文保留。
	unknown := classifyUpstreamError("系统当前繁忙，请稍后再试")
	require.False(t, errors.Is(unknown, ErrUpstreamCooldown))
	require.False(t, errors.Is(unknown, ErrUpstreamKeyExpired))
	var upe *UpstreamPullError
	require.True(t, errors.As(unknown, &upe))
	require.Contains(t, upe.Message(), "系统当前繁忙，请稍后再试")
}
