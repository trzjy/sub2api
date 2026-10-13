package service

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
)

const (
	// quotaKeySweepJobName 是过期激活码清扫任务在 ops_job_heartbeats 表中的 job 名
	// （照 ops_cleanup_service 的 opsCleanupJobName = "ops_cleanup" 同型命名，§3.1 / C7）。
	quotaKeySweepJobName = "quota_key_sweep"
	// quotaKeySweepInterval 周期清扫间隔（方案 §3.1）。
	quotaKeySweepInterval = 5 * time.Minute
	// quotaKeySweepTimeout 单轮超时（含 ListExpired + 整批事务提交）。
	quotaKeySweepTimeout = 30 * time.Second
	// quotaKeySweepHeartbeatTimeout 写心跳的超时（照 ops_cleanup 的 recordHeartbeat 同型）。
	quotaKeySweepHeartbeatTimeout = 5 * time.Second
)

// QuotaKeySweeper 周期将过期的激活码（expires_at < now）标记为 expired。
//
// 严格约束（方案 §3.1 / §4.3，C7）：
//   - 只改 status 维度（标 expired）；exhausted 维度独立保留，绝不重置；
//   - 已是 expired 的跳过（幂等）；
//   - 不触碰 accounts 表、不做拉号、不做删除、不做补池（C3.2/C9 范围，禁区）；
//   - 失败只记日志、不 panic、不影响下一轮（照 AccountExpiryService.runOnce 容错基调）。
//
// 生命周期严格复用 AccountExpiryService 形态：
// goroutine + time.NewTicker(interval) + 立即 runOnce() 后再按 tick 循环 +
// Stop() 用 stopCh/stopOnce/wg 优雅退出；runOnce() 内用 context.WithTimeout。
//
// 因既有 UpdateStatusFields 需要 *ent.Tx，故额外持有 entClient 用于开启每轮事务；
// opsRepo 用于写 job 心跳（照 ops_cleanup_service.recordHeartbeatSuccess 同型）。
// 这二者是满足“标 expired + 写心跳”所必须的协作者，非范围外新增机制。
type QuotaKeySweeper struct {
	repo      QuotaKeyRepository
	opsRepo   OpsRepository
	entClient *ent.Client

	interval time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewQuotaKeySweeper 构造过期激活码清扫器。
// repo 为 QuotaKeyRepository（契约仅含 quota_keys 操作，天然不触碰 accounts）；
// opsRepo 用于写 job 心跳；entClient 用于开启每轮事务（UpdateStatusFields 需要 *ent.Tx）。
func NewQuotaKeySweeper(repo QuotaKeyRepository, opsRepo OpsRepository, entClient *ent.Client) *QuotaKeySweeper {
	return &QuotaKeySweeper{
		repo:      repo,
		opsRepo:   opsRepo,
		entClient: entClient,
		interval:  quotaKeySweepInterval,
		stopCh:    make(chan struct{}),
	}
}

// Start 启动清扫循环（goroutine + ticker，立即跑一轮）。照 AccountExpiryService.Start。
func (s *QuotaKeySweeper) Start() {
	if s == nil || s.repo == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		s.runOnce()
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

// Stop 停止清扫循环并等待已启动的轮次退出。照 AccountExpiryService.Stop。
func (s *QuotaKeySweeper) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

// runOnce 在一轮超时上下文内执行清扫并写一次 job 心跳。
// 失败只记日志、不 panic；entClient 为 nil 时跳过扫库但仍报活（便于无 DB 单测）。
func (s *QuotaKeySweeper) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), quotaKeySweepTimeout)
	defer cancel()
	runAt := time.Now().UTC()

	if s.entClient == nil {
		s.recordHeartbeat(ctx, runAt, true, nil, 0)
		return
	}

	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		log.Printf("[QuotaKeySweep] open tx failed: %v", err)
		s.recordHeartbeat(ctx, runAt, false, err, 0)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	n, err := s.sweepOnce(ctx, tx)
	if err != nil {
		log.Printf("[QuotaKeySweep] sweep failed: %v", err)
		s.recordHeartbeat(ctx, runAt, false, err, n)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[QuotaKeySweep] commit failed: %v", err)
		s.recordHeartbeat(ctx, runAt, false, err, n)
		return
	}
	committed = true
	if n > 0 {
		log.Printf("[QuotaKeySweep] marked %d expired quota keys", n)
	}
	s.recordHeartbeat(ctx, runAt, true, nil, n)
}

// sweepOnce 对当前过期集合逐条标 expired。
//
// 幂等：已是 expired 的跳过；两个维度独立：只标 expired，保留 exhausted /
// last_error / last_refresh_at（不重置 exhausted）。
//
// tx 由调用方提供：生产路径传真实事务，单测路径传占位事务（fake repo 忽略 tx）。
// 返回本轮实际改写条数。
func (s *QuotaKeySweeper) sweepOnce(ctx context.Context, tx *ent.Tx) (int, error) {
	now := time.Now()
	expired, err := s.repo.ListExpired(ctx, now)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, qk := range expired {
		if qk.Status == domain.QuotaKeyStatusExpired {
			continue // 幂等：已是 expired 的跳过
		}
		// 只标 expired，保留 exhausted 与其它字段（两个维度独立）。
		if err := s.repo.UpdateStatusFields(ctx, tx, qk.ID, domain.QuotaKeyStatusExpired, qk.Exhausted, qk.LastRefreshError, qk.LastRefreshAt); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// recordHeartbeat 写一次 job 心跳（照 ops_cleanup_service.recordHeartbeatSuccess/Error 同型）。
func (s *QuotaKeySweeper) recordHeartbeat(ctx context.Context, runAt time.Time, success bool, sweepErr error, count int) {
	if s == nil || s.opsRepo == nil {
		return
	}
	hbCtx, hbCancel := context.WithTimeout(context.Background(), quotaKeySweepHeartbeatTimeout)
	defer hbCancel()

	durMs := time.Since(runAt).Milliseconds()
	input := &OpsUpsertJobHeartbeatInput{
		JobName:        quotaKeySweepJobName,
		LastRunAt:      &runAt,
		LastDurationMs: &durMs,
	}
	if success {
		now := time.Now().UTC()
		input.LastSuccessAt = &now
		result := truncateString(heartbeatResult(count), 2048)
		input.LastResult = &result
	} else if sweepErr != nil {
		msg := truncateString(sweepErr.Error(), 2048)
		errAt := time.Now().UTC()
		input.LastErrorAt = &errAt
		input.LastError = &msg
	}
	_ = s.opsRepo.UpsertJobHeartbeat(hbCtx, input)
}

func heartbeatResult(count int) string {
	if count == 0 {
		return "no expired quota keys"
	}
	return fmt.Sprintf("marked %d expired quota keys", count)
}
