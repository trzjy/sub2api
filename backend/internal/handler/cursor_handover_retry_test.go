//go:build unit

package handler

// 本文件锁定「换号重试不重复交票」（2026-11-03 生产实证）。
//
// 事故形状：用户槽与余额预留是每个 HTTP 请求只拿一次的（选号循环外获取），而 Cursor
// 分支在每次尝试都把本 HTTP 的资源转交给该次尝试的会话。首轮尝试（任意原因）失败后
// 换号重试，第二次转交同一句柄必然撞上「句柄已交出」：
//
//	handler: cursor slot handle already handed over → 502
//
// 生产 24h 10 次；一次请求内后续所有换号都死（20:45:41–20:46:36 连刷 7 个号全 502）。
//
// 修复口径：已交出过的资源不再登记（所有权归首轮会话，释放由它的 finalizer 负责，
// 绝不重复释放）；未交出的资源照旧登记。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCursorHandoverResourcesSkipsHandedSlots 覆盖：已交出的用户槽不会被二次登记，
// 未交出的资源仍然登记（不能因为修重复交票而整体不交）。
func TestCursorHandoverResourcesSkipsHandedSlots(t *testing.T) {
	var releases atomic.Int32
	userSlot := newTransferableRelease(context.Background(), "user_slot", func() { releases.Add(1) })
	require.NotNil(t, userSlot)

	sessionCtx, cancelSession := context.WithCancel(context.Background())
	require.NoError(t, userSlot.TransferTo(sessionCtx), "首轮尝试转交必须成功")
	require.True(t, userSlot.HandedOver())

	// 换号重试：账号槽是新取的（未交出），用户槽已被首轮会话持有 → 只登记账号槽。
	accountSlot := newTransferableRelease(context.Background(), "account_slot", func() {})
	resources := cursorHandoverResources(userSlot, accountSlot, nil)
	require.Len(t, resources, 1, "已交出的用户槽不得再次登记")
	require.Equal(t, "account_slot", resources[0].Name())

	// 未交出的用户槽必须照常登记。
	freshUserSlot := newTransferableRelease(context.Background(), "user_slot", func() {})
	resources = cursorHandoverResources(freshUserSlot, nil, nil)
	require.Len(t, resources, 1)
	require.Equal(t, "user_slot", resources[0].Name())

	// 全空时返回空集合（不登记空壳）。
	require.Empty(t, cursorHandoverResources(nil, nil, nil))

	// 已交出的槽位仍只由持有它的会话释放一次。
	cancelSession()
	require.Eventually(t, func() bool { return releases.Load() == 1 }, 2*time.Second, 5*time.Millisecond,
		"释放仍由首轮会话负责，且只释放一次")
}
