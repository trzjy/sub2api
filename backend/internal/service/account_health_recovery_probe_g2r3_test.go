//go:build unit

// AC-G2R3 定向测试：Stop 生命周期两缺陷回归。
//   1. 并发调用 Stop 两次不得 panic（单一关闭者：锁内声明关闭权，锁外仅一次 close，
//      第二个并发 Stop 在锁内读到字段已空直接返回，杜绝锁外双 close）。
//   2. Start 后 Stop，主循环必须退出（带超时断言，防 wg 挂死）；主循环持有启动时捕获的
//      stopCh 引用，Stop 置 nil 后仍能收到关闭信号。
//
// 用例以 TestHealthProbe 前缀命名，确保落入白名单 -run 过滤。

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 并发两次 Stop 不得 panic：首调拿到关闭权 close 并 wg.Wait；第二调在锁内读到 stopCh
// 已空直接返回，不重复 close（否则双 close 引发 panic）。
func TestHealthProbeStop_ConcurrentTwiceNoPanic(t *testing.T) {
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	repo := &probeRepoMock{}
	p := newProbeService(t, true, 100, rl, repo)
	p.retryInterval = time.Millisecond

	p.Start(context.Background())
	require.NotNil(t, p.stopCh, "Start 应启动探测循环并创建 stopCh")

	var wg sync.WaitGroup
	panicked := make(chan any, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicked <- r
				}
			}()
			p.Stop()
		}()
	}
	wg.Wait()

	select {
	case r := <-panicked:
		t.Fatalf("并发 Stop 两次不应 panic，却捕获到: %v", r)
	default:
	}
	require.Nil(t, p.stopCh, "两次 Stop 后 stopCh 应已被置空（优雅关闭完成）")
}

// Start 后 Stop，主循环必须退出：Stop 内部 wg.Wait 在探测 goroutine 退出后才返回。
// 带超时断言——若 Stop 置 nil 后主循环收不到关闭信号、wg.Wait 挂死，本测试超时失败。
func TestHealthProbeStop_MainLoopExitsAfterStop(t *testing.T) {
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	repo := &probeRepoMock{}
	p := newProbeService(t, true, 100, rl, repo)
	p.retryInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	require.NotNil(t, p.stopCh, "Start 应启动探测循环并创建 stopCh")

	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop 在超时内返回：主循环已退出、wg.Wait 未挂死。
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 超时：主循环未退出（Stop 置 nil 后运行中循环未收到关闭信号），wg.Wait 挂死")
	}
	require.Nil(t, p.stopCh, "Stop 后 stopCh 应已被置空")
}
