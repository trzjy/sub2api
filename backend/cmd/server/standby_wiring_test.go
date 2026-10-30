package main

import (
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 第 3 片装配层单测：控制器接入服务启动的开关、单例共享与启停幂等。
// 只跑装配，不跑调度；注册表空表即"全部 closed"是安全默认。

func TestProvideStandbyController_DisabledDoesNotStart(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	before := service.DefaultStandbyRegistry()
	cfg := &config.Config{Standby: config.StandbyConfig{Enabled: false}}

	controller := provideStandbyController(cfg, db)

	require.Nil(t, controller, "关闭开关时不得构造控制器")
	require.Same(t, before, service.DefaultStandbyRegistry(), "关闭开关时不得替换进程级注册表")
	require.False(t, service.DefaultStandbyRegistry().IsOpen("deepseek-v4-pro"))
}

// TestProvideStandbyController_AssemblyFailureFallsBackToDisabled 覆盖唯一失败开放点：
// 装配依赖不成立时返回 nil（禁用 standby）而不是报错阻断启动。
func TestProvideStandbyController_AssemblyFailureFallsBackToDisabled(t *testing.T) {
	before := service.DefaultStandbyRegistry()
	cfg := &config.Config{Standby: config.StandbyConfig{Enabled: true}}

	require.NotPanics(t, func() {
		require.Nil(t, provideStandbyController(cfg, nil), "无数据库时禁用 standby，不得阻断启动")
	})
	require.Same(t, before, service.DefaultStandbyRegistry())
}

func TestProvideStandbyController_EnabledSharesProcessRegistry(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cfg := &config.Config{
		Timezone: "Asia/Shanghai",
		Standby:  config.StandbyConfig{Enabled: true, PollIntervalSeconds: 30},
	}

	controller := provideStandbyController(cfg, db)

	require.NotNil(t, controller)
	// 调度层只读 DefaultStandbyRegistry；必须与控制器写入的是同一实例。
	require.Same(t, controller.Registry(), service.DefaultStandbyRegistry())
}

func TestStandbyController_StartStopIdempotent(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cfg := &config.Config{
		Timezone: "Asia/Shanghai",
		Standby:  config.StandbyConfig{Enabled: true, PollIntervalSeconds: 30},
	}
	controller := provideStandbyController(cfg, db)
	require.NotNil(t, controller)

	ctx := context.Background()
	require.NotPanics(t, func() {
		controller.Start(ctx)
		controller.Start(ctx) // 重复启动无副作用
	})
	require.NotPanics(t, func() {
		controller.Stop()
		controller.Stop() // 重复停止无副作用
	})
	// 停后可再次启停：生命周期可重入。
	require.NotPanics(t, func() {
		controller.Start(ctx)
		controller.Stop()
	})
	// 采集整轮失败（sqlmock 无期望调用）不得迁移状态：仍是空表 = 全部 closed。
	require.False(t, service.DefaultStandbyRegistry().IsOpen("deepseek-v4-pro"))
	require.Empty(t, service.DefaultStandbyRegistry().Snapshot())
}
