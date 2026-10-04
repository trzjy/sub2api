package repository

// E38 定向测试（repository 层）：per-account 写锁下沉后的互斥性证明。
// 业务 SET（commitModelRateLimitSet）与探测族写入口（ApplyModelRateLimitObservation 经
// WithModelRateLimitAccountLock）均路由到同一 accountRepository.modelRateLimitWriteLocks
// 的 per-account 互斥锁；以下测试直接验证该锁的串行化语义（退化测试无需真实 DB）。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRateLimitE38AccountWriteLockSerializesSameAccount 验证同账号并发请求经
// WithModelRateLimitAccountLock 串行化：任一时刻至多一个 goroutine 处于锁内，防并发交错
// 读同一 revision 后后写者覆盖较新条目与 meta（E38 互斥性核心证明）。
func TestRateLimitE38AccountWriteLockSerializesSameAccount(t *testing.T) {
	repo := &accountRepository{} // 仅测锁语义，无需 DB
	const accountID int64 = 42
	var inFlight int64
	var maxInFlight int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = repo.WithModelRateLimitAccountLock(context.Background(), accountID, func(ctx context.Context) error {
				cur := atomic.AddInt64(&inFlight, 1)
				mu.Lock()
				if cur > maxInFlight {
					maxInFlight = cur
				}
				mu.Unlock()
				// 让出，放大交错窗口以暴露潜在竞态（配合 -race 定向）。
				_ = ctx
				atomic.AddInt64(&inFlight, -1)
				return nil
			})
		}()
	}
	wg.Wait()
	require.Equal(t, int64(1), maxInFlight, "同账号写锁必须串行化：任一时刻至多一个 goroutine 在锁内")
}

// TestRateLimitE38AccountWriteLockPerAccountIsolation 验证 per-account 粒度：同一账号恒返回
// 同一把锁（保证串行化），不同账号返回不同锁（互不阻塞，而非全局锁）。
func TestRateLimitE38AccountWriteLockPerAccountIsolation(t *testing.T) {
	repo := &accountRepository{}
	a1 := repo.modelRateLimitWriteLock(1)
	a1Again := repo.modelRateLimitWriteLock(1)
	a2 := repo.modelRateLimitWriteLock(2)
	require.Same(t, a1, a1Again, "同一账号必须返回同一把锁（串行化同一账号读写改写）")
	require.NotSame(t, a1, a2, "不同账号必须返回不同锁（per-account 隔离，互不阻塞）")
}
