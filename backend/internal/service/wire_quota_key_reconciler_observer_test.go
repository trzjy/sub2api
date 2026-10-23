//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProvideQuotaKeyReconcilerBindsProductionUsageObserver 断言生产 provider
// ProvideQuotaKeyReconciler 在 Start() 之前把唯一的 *CursorAutoEntryService 实例装配为
// reconciler 的来源额度观测窄面（§25.19 第2项）。装配断言必须走真实生产 provider，
// 不得用替身 observer 顶替生产 AutoEntry "证明"装配。
func TestProvideQuotaKeyReconcilerBindsProductionUsageObserver(t *testing.T) {
	// 用生产 provider 构造真实零值 AutoEntry（client==nil，provider 内不注入水位提供方、不触 DB）。
	autoEntry := ProvideCursorAutoEntryService(nil, nil, nil, nil)
	require.NotNil(t, autoEntry, "生产构造的 AutoEntry 不应为 nil")

	ops := newReconOpsRepo()

	// 调用真实生产 provider；repo==nil 让 Start() 立即返回（既有门禁），不起后台轮次。
	// puller 用真实零值 *QuotaKeyService（client==nil）。
	puller := &QuotaKeyService{}
	svc := ProvideQuotaKeyReconciler(nil, puller, autoEntry, ops, nil, nil, nil)
	require.NotNil(t, svc, "生产构造的 reconciler 不应为 nil")

	// 1) usageObserver 非 nil，且与传入 autoEntry 是同一指针（真实生产装配，非替身）。
	require.NotNil(t, svc.usageObserver, "reconciler.usageObserver 应已由生产装配为非 nil")

	obs, ok := svc.usageObserver.(*CursorAutoEntryService)
	require.True(t, ok, "usageObserver 的动态类型应为 *CursorAutoEntryService（生产真实实现，非替身）")
	require.Same(t, autoEntry, obs, "usageObserver 应与传入的 autoEntry 是同一指针")

	// 接口等值比较：动态类型与动态值（指针）均相同。
	require.Equal(t, svc.usageObserver, quotaKeyUsageObserver(autoEntry), "usageObserver 接口值应与 (quotaKeyUsageObserver)autoEntry 相等")

	// 2) 既有 SetReconciler 反向注入未被破坏：puller.reconciler 仍指向 svc。
	rn, ok := puller.reconciler.(*QuotaKeyReconciler)
	require.True(t, ok, "puller.reconciler 应为 *QuotaKeyReconciler")
	require.Same(t, rn, svc, "puller.reconciler 应指向同一个 reconciler 实例")

	// 3) nil repo 下未启动后台轮次：静置 50ms 后心跳写入次数应为 0；随后 Stop 立即返回。
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, 0, ops.hbCalls(), "nil repo 下 Start() 不应启动任何后台轮次（心跳写入应为 0）")
	svc.Stop() // repo==nil 时 Start 未起 goroutine，应无阻塞直接返回。
}
