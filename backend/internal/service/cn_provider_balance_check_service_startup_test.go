package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 2026-11-03 生产实证回归：余额巡检 Start() 必须在首个 interval 之前做一轮补采。
//
// 背景：快照新鲜度门 15 分钟 < 巡检间隔 10 分钟之外没有余量——若首轮探测推迟整整
// 一个 interval，进程重启（每次部署都会重启）后到首轮到来之前，全部账号快照都越过
// 新鲜度门，管理端余额单元格按单轨口径隐藏金额、统一显示「未知」。
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

func TestCNProviderBalanceCheckStartRunsCatchUpBeforeFirstInterval(t *testing.T) {
	repo := &startupCountingRepo{}
	// interval 取 1 小时：任何在 200ms 内发生的采集只可能来自启动补采。
	svc := newStartupTestService(repo, time.Hour)
	svc.startupFirstRunDelay = time.Millisecond

	svc.Start()
	defer svc.Stop()

	require.Eventually(t, func() bool { return repo.listCalls.Load() > 0 }, 2*time.Second, 5*time.Millisecond,
		"启动补采未在首个 interval 之前执行：重启窗口内余额快照会集体过期并显示「未知」")
}

func TestCNProviderBalanceCheckStartHonorsStartupDelay(t *testing.T) {
	repo := &startupCountingRepo{}
	svc := newStartupTestService(repo, time.Hour)
	// 默认 30s 延迟：启动瞬间不得探测（不与启动峰重叠）。
	svc.startupFirstRunDelay = 300 * time.Millisecond

	svc.Start()
	defer svc.Stop()

	time.Sleep(50 * time.Millisecond)
	require.EqualValues(t, 0, repo.listCalls.Load(), "启动补采必须等延迟到点，不得在进程启动峰立即探测")
}

func TestCNProviderBalanceCheckStopDuringStartupDelaySkipsCollection(t *testing.T) {
	repo := &startupCountingRepo{}
	svc := newStartupTestService(repo, time.Hour)
	svc.startupFirstRunDelay = 60 * time.Millisecond

	svc.Start()
	svc.Stop()

	time.Sleep(150 * time.Millisecond)
	require.EqualValues(t, 0, repo.listCalls.Load(), "停止后不得再跑启动补采")
}
