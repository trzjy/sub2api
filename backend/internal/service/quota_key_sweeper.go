package service

import (
	"context"
	"errors"
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

// QuotaKeySweeper 周期将过期的激活码（expires_at < now）标记为 expired，并对
// 到期激活码名下的 cursor 账号执行软删。
//
// 严格约束（方案 §3.1 / §4.3，C7；EXP-B 按 2026-10-24 用户裁定修订边界）：
//   - 只做两件事：改 quota_keys.status（标 expired）+ 到期激活码名下的 cursor 账号软删；
//   - 仍然不做拉号、不做补池、不碰非 cursor 平台、不物理删除、不直接操作 Redis
//     （outbox 由既有消费者处理）；exhausted 维度独立保留，绝不重置；
//   - 已是 expired 的跳过标 expired（幂等），但继续补删账号；
//   - 失败只记日志、不 panic、不影响下一轮（照 AccountExpiryService.runOnce 容错基调）。
//
// 生命周期严格复用 AccountExpiryService 形态：
// goroutine + time.NewTicker(interval) + 立即 runOnce() 后再按 tick 循环 +
// Stop() 用 stopCh/stopOnce/wg 优雅退出；runOnce() 内用 context.WithTimeout。
//
// 因既有 UpdateStatusFields 需要 *ent.Tx，故额外持有 entClient 用于开启每轮事务；
// opsRepo 用于写 job 心跳（照 ops_cleanup_service.recordHeartbeatSuccess 同型）。
// 这二者是满足"标 expired + 账号软删同事务 + 写心跳"所必须的协作者，非范围外新增机制。
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
// repo 为 QuotaKeyRepository（契约含到期账号软删与 outbox 入口，由 sweepOnce 在同一
// 事务内编排）；opsRepo 用于写 job 心跳；entClient 用于开启每轮事务（标 expired 与
// 账号软删共用同一事务）。
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

// SweepNow 同步执行一次到期清扫，返回本轮标记为 expired 的条数（§4.4 sweep_expired
// 的 admin 手动触发口）。
//
// 复用既有 sweepOnce，不重造任何清扫逻辑；与 runOnce 唯一差别是同步返回改写条数与
// 错误，而非只写日志/心跳。因复用 sweepOnce，SweepNow 同样具备「到期激活码名下
// cursor 账号软删 + outbox」语义（EXP-B 预期行为）。事务在本方法内开启、提交/回滚，
// 契约与 runOnce 保持一致（entClient 为 nil 时直接报错，不做静默成功）。
func (s *QuotaKeySweeper) SweepNow(ctx context.Context) (int, error) {
	if s == nil || s.entClient == nil {
		return 0, errors.New("quota_key_sweeper: ent client not configured")
	}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return 0, fmt.Errorf("quota_key_sweeper: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	n, err := s.sweepOnce(ctx, tx)
	if err != nil {
		return n, err
	}
	if err := tx.Commit(); err != nil {
		return n, fmt.Errorf("quota_key_sweeper: commit sweep: %w", err)
	}
	committed = true
	return n, nil
}

// sweepOnce 对当前过期集合逐条处理：active 的按 DB 当前条件标 expired；随后统一
// 尝试「到期激活码名下 cursor 账号」的软删补删，账号软删与 outbox 写同一事务。
//
// 分派逻辑（EXP-B）：
//   - active：先条件化标 expired（MarkExpiredIfStillExpired），命中计入 count；
//   - expired：历史已 expired、此前未做账号补删的 key，继续补删账号（不再标，count 不增）；
//   - 其它状态：跳过，不触发账号删除。
//   - 账号级补删对所有 active/expired 分支统一执行：DB 当前条件在仓储方法内用行锁
//     复核（status='expired' AND expires_at IS NOT NULL AND expires_at < now），不满足
//     返回空集合 → 不产生 outbox；满足则软删 platform='cursor' 未删账号，并返回实际
//     删除的账号 ID。
//
// 幂等：已是 expired 的跳过标 expired；软删条件含 deleted_at IS NULL，第二次 sweep
// 不再改写。两个维度独立：只标 expired，保留 exhausted / last_error / last_refresh_at
// （不重置 exhausted）。
//
// 关键约束（D-CURSOR-032）：状态写入按数据库当前 expires_at 条件限定。ListExpired 取回
// 的是旧值，管理员可能在两步间用 UpdateMeta 清空 expires_at；因此写入走条件化方法
// MarkExpiredIfStillExpired（UPDATE ... WHERE id AND status='active' AND expires_at
// IS NOT NULL AND expires_at < now），由 DB 当前行重新判定，不受旧值影响。条件不满足时
// 影响 0 行，count 不增，天然幂等。
//
// tx 由调用方提供：生产路径传真实事务，单测路径传占位事务（fake repo 忽略 tx）。
// 返回本轮实际改写条数（标 expired 的 key 数，不含纯账号补删）。
func (s *QuotaKeySweeper) sweepOnce(ctx context.Context, tx *ent.Tx) (int, error) {
	now := time.Now()
	expired, err := s.repo.ListExpired(ctx, now)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, qk := range expired {
		switch qk.Status {
		case domain.QuotaKeyStatusActive:
			// 先条件化标 expired（DB 当前行重新判定，旧快照不生效）。
			affected, err := s.repo.MarkExpiredIfStillExpired(ctx, tx, qk.ID, now)
			if err != nil {
				return count, err
			}
			count += affected
		case domain.QuotaKeyStatusExpired:
			// 历史已 expired、此前未做账号补删的 key，继续补删。
		default:
			continue // 其它状态不触发账号删除
		}
		// 账号级补删：DB 当前条件在仓储方法内用行锁复核，不满足则返回空集合。
		ids, err := s.repo.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, qk.ID, now)
		if err != nil {
			return count, fmt.Errorf("sweep quota key %d cursor accounts: %w", qk.ID, err)
		}
		if len(ids) == 0 {
			continue
		}
		// outbox 与软删同事务；失败返回错误 → 整个 sweeper 事务回滚。
		if err := s.repo.EnqueueAccountChangedOutbox(ctx, tx, ids); err != nil {
			return count, fmt.Errorf("sweep quota key %d enqueue outbox: %w", qk.ID, err)
		}
		// 脱敏日志：只记 key ID 与删除数量，禁止记录 code/token/鉴权头。
		log.Printf("[QuotaKeySweep] quota key %d expired: soft-deleted %d cursor accounts", qk.ID, len(ids))
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
