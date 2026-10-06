//go:build unit

// E14 定向测试：RunOnce 空 settingService 保护（第三轮终审 #7 回归）。
// 重构使能门禁时移除了旧 probeEnabled 对 nil settingService 的保护，RunOnce 现直接
// 解引用 p.settingService 会 panic。本测试锁定：nil settingService 下 RunOnce/Start
// 均失败关闭安全返回，不 panic、不产生任何探测副作用。
package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// RunOnce 在 settingService 为 nil 时必须安全返回（不 panic），且因短路发生在读取设置
// 之前，不得进入候选列举（listCalls==0）——证明失败关闭而非「继续执行」。
func TestE14RunOnce_NilSettingServiceDoesNotPanic(t *testing.T) {
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: parkedBreakerAccount(7, 0)},
		candidates: []*Account{parkedBreakerAccount(7, 0)},
	}
	// settingService 显式传 nil（其余依赖齐备，确保 panic 只可能来自 settingService）。
	p := NewAccountHealthRecoveryProbeService(repo, nil, &config.Config{}, nil, nil, nil)

	require.NotPanics(t, func() {
		p.RunOnce(context.Background())
	}, "nil settingService 下 RunOnce 不得 panic")

	require.Zero(t, repo.listCalls, "短路须发生在读取设置之前，不得进入候选列举")
}

// Start 在 settingService 为 nil 时同样失败关闭安全返回：不 panic、不启动探测循环
// （stopCh 保持 nil），随后 Stop 亦不 panic。
func TestE14Start_NilSettingServiceDoesNotPanic(t *testing.T) {
	p := NewAccountHealthRecoveryProbeService(&probeRepoMock{accounts: map[int64]*Account{}}, nil, &config.Config{}, nil, nil, nil)

	require.NotPanics(t, func() {
		p.Start(context.Background())
	}, "nil settingService 下 Start 不得 panic")

	require.Nil(t, p.stopCh, "nil settingService 下 Start 应失败关闭，不启动探测循环")

	require.NotPanics(t, func() {
		p.Stop()
	}, "未启动的 Stop 不得 panic")
}
