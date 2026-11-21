package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 2026-11-03 生产实证回归：余额巡检首轮补采必须在首个 interval 之前发生。
//
// 批4 起，该服务的周期调度改由统一 tick 注册表驱动（Start() 只登记，不再自持
// ticker）。故测试改为：把进程级注册表换成可驱动的实例，模拟健康服务唯一 tick
// 反复调用 DispatchDue。
type startupCountingRepo struct {
	AccountRepository
	listCalls atomic.Int64
}

func (r *startupCountingRepo) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
	r.listCalls.Add(1)
	return nil, nil
}

func (r *startupCountingRepo) ListActive(_ context.Context) ([]Account, error) {
	return nil, nil
}

func newStartupTestService(repo AccountRepository, interval time.Duration) *CNProviderBalanceCheckService {
	cfg := &config.Config{}
	cfg.Gateway.CNProviders.BalanceCheckEnabled = true
	return NewCNProviderBalanceCheckService(
		repo,
		&CNProviderBalanceService{},
		nil,
		nil,
		nil,
		nil,
		cfg,
		interval,
	)
}

// installStartupTestRegistry 用可驱动的注册表替换默认实例，并在测试结束后还原。
func installStartupTestRegistry(t *testing.T) *BackgroundTickRegistry {
	t.Helper()
	r := NewBackgroundTickRegistry()
	prev := SetDefaultBackgroundTickRegistry(r)
	t.Cleanup(func() { SetDefaultBackgroundTickRegistry(prev) })
	return r
}

// driveTicks 模拟健康服务唯一 tick 反复派发。
func driveTicks(ctx context.Context, r *BackgroundTickRegistry, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		r.DispatchDue(ctx)
		time.Sleep(2 * time.Millisecond)
	}
}

func TestCNProviderBalanceCheckStartRunsCatchUpBeforeFirstInterval(t *testing.T) {
	repo := &startupCountingRepo{}
	// interval 取 1 小时：任何在 300ms 内发生的采集只可能来自启动补采。
	svc := newStartupTestService(repo, time.Hour)
	svc.startupFirstRunDelay = time.Millisecond
	r := installStartupTestRegistry(t)

	svc.Start()
	t.Cleanup(svc.Stop)

	driveTicks(context.Background(), r, 300*time.Millisecond)
	require.Greater(t, repo.listCalls.Load(), int64(0),
		"启动补采未在首个 interval 之前执行：重启窗口内余额快照会集体过期并显示「未知」")
}

func TestCNProviderBalanceCheckStartHonorsStartupDelay(t *testing.T) {
	repo := &startupCountingRepo{}
	svc := newStartupTestService(repo, time.Hour)
	// 默认 30s 延迟：启动瞬间不得探测（不与启动峰重叠）。
	svc.startupFirstRunDelay = 300 * time.Millisecond
	r := installStartupTestRegistry(t)

	svc.Start()
	t.Cleanup(svc.Stop)

	driveTicks(context.Background(), r, 50*time.Millisecond)
	require.EqualValues(t, 0, repo.listCalls.Load(), "启动补采必须等延迟到点，不得在进程启动峰立即探测")
}

func TestCNProviderBalanceCheckStopDuringStartupDelaySkipsCollection(t *testing.T) {
	repo := &startupCountingRepo{}
	svc := newStartupTestService(repo, time.Hour)
	svc.startupFirstRunDelay = 60 * time.Millisecond
	r := installStartupTestRegistry(t)

	svc.Start()
	svc.Stop()

	driveTicks(context.Background(), r, 150*time.Millisecond)
	require.EqualValues(t, 0, repo.listCalls.Load(), "停止后不得再跑启动补采")
}
