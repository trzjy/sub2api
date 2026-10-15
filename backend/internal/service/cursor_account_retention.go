package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// 本文件实现 Cursor 平台账号/流水定期清理（保留期默认 90 天，配置化）。
// 口径（方案 v3.4 §3.6）：
//   - 作用域仅 platform='cursor'；其它平台清理/删除路径一律不动。
//   - 清理=物理删除（非号池“删除”）；软删账号行与其 usage_logs 超期后清掉。
//   - 复用既有机制：循环模式照抄 AuditLogService.runRetentionLoop；
//     流水删除复用 UsageCleanupRepository.DeleteUsageLogsBatch（内含 lockGroupUsageRollupState
//     聚合失效处理）。严禁靠 accounts 的 FK ON DELETE CASCADE 直接删流水（会绕过聚合失效）。

const (
	cursorRetentionStartupDelay  = 5 * time.Minute
	cursorRetentionCheckInterval = 24 * time.Hour
	cursorRetentionBatchSize     = 1000 // 每轮批次上限（账号 ID 批）
	cursorRetentionUsageLogBatch = 1000 // 单次 DeleteUsageLogsBatch 的流水删除上限
	cursorRetentionMaxRounds     = 1000 // 单轮（runRetentionOnce）最大批次轮数，防长事务/死循环
	cursorRetentionRunTimeout    = 30 * time.Minute

	// cursorAccountRetentionJobName 是本清理任务在 ops_job_heartbeats 表中的 job 名
	// （照 quotaKeySweepJobName = "quota_key_sweep" 同型命名，§7.9 三定时任务报活）。
	cursorAccountRetentionJobName = "cursor_account_retention"
	// cursorAccountRetentionHeartbeatTimeout 写心跳的超时（照 quota_key_sweep 同型）。
	cursorAccountRetentionHeartbeatTimeout = 5 * time.Second

	// cursorRetentionFarPast / cursorRetentionFarFuture 作为 DeleteUsageLogsBatch 的
	// 时间范围边界，确保“先删某账号全部流水”时不漏删（流水产生时间可能远早于软删时间）。
)

// 远边界时间（非 const，time.Date 非编译期常量）：用于按账号删除全部流水时兜底时间范围。
var (
	cursorRetentionFarPast   = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	cursorRetentionFarFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
)

// CursorRetentionAccountRepository 是 cursor 账号物理清理所需的私有持久层端口。
// 它不属于任何公共契约（不进入 UsageCleanupFilters 等公共结构），仅服务于本清理服务，
// 因此可自由演进而不违反“不得自行扩展公共契约”的纪律。
type CursorRetentionAccountRepository interface {
	// ListExpiredSoftDeletedCursorAccountIDs 返回 platform='cursor' 且已软删且
	// deleted_at 早于 cutoff 的账号 ID 批（按 id 升序，限 limit）。
	ListExpiredSoftDeletedCursorAccountIDs(ctx context.Context, cutoff time.Time, limit int) ([]int64, error)
	// PhysicallyDeleteAccounts 物理删除给定 ID 的账号行（非软删，直接 DELETE）。
	PhysicallyDeleteAccounts(ctx context.Context, ids []int64) (int64, error)
}

// CursorAccountRetentionService 周期性物理清理超期软删的 cursor 账号及其用量流水。
type CursorAccountRetentionService struct {
	usageCleanupRepo UsageCleanupRepository
	dbRepo           CursorRetentionAccountRepository
	settingService   *SettingService
	// opsRepo 用于每轮写 ops_job_heartbeats 心跳（照 QuotaKeySweeper / QuotaKeyReconciler 同型）。
	opsRepo OpsRepository

	// nowFunc 注入可控时钟，便于单测时间边界；生产默认 time.Now。
	nowFunc func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewCursorAccountRetentionService 构造清理服务。db 用于物理删账号（私有 repo 实现）；
// opsRepo 用于写 job 心跳（照 NewQuotaKeySweeper 同型）。
func NewCursorAccountRetentionService(usageCleanupRepo UsageCleanupRepository, db *sql.DB, settingService *SettingService, opsRepo OpsRepository) *CursorAccountRetentionService {
	return &CursorAccountRetentionService{
		usageCleanupRepo: usageCleanupRepo,
		dbRepo:           &cursorRetentionAccountRepo{db: db},
		settingService:   settingService,
		opsRepo:          opsRepo,
		nowFunc:          time.Now,
	}
}

// Start 启动周期清理协程（照抄 AuditLogService.Start）。
func (s *CursorAccountRetentionService) Start() {
	if s == nil || s.usageCleanupRepo == nil || s.dbRepo == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	s.cancel = cancel
	s.wg.Add(1)
	go s.runRetentionLoop()
}

// Stop 停止服务并等待协程退出（照抄 AuditLogService.Stop）。
func (s *CursorAccountRetentionService) Stop() {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// runRetentionLoop 周期清理循环：启动延迟 5min + 每 24h 一轮；失败仅 WARN 不 panic。
func (s *CursorAccountRetentionService) runRetentionLoop() {
	defer s.wg.Done()

	startupTimer := time.NewTimer(cursorRetentionStartupDelay)
	defer startupTimer.Stop()
	select {
	case <-s.ctx.Done():
		return
	case <-startupTimer.C:
	}

	ticker := time.NewTicker(cursorRetentionCheckInterval)
	defer ticker.Stop()

	s.RunOnce(s.ctx)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.RunOnce(s.ctx)
		}
	}
}

// RunOnce 执行一轮清理（可独立调用，便于测试与手动触发）。
// 每轮结束写且仅写一次 ops_job_heartbeats 心跳（成功/失败），照 QuotaKeySweeper.runOnce 同型。
func (s *CursorAccountRetentionService) RunOnce(ctx context.Context) {
	if s == nil || s.usageCleanupRepo == nil || s.dbRepo == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runAt := time.Now().UTC()
	deleted, err := s.runRetentionOnce(ctx)
	if err != nil {
		retentionWarnf("retention round failed: %v", err)
		s.recordHeartbeat(ctx, runAt, false, err, deleted)
		return
	}
	s.recordHeartbeat(ctx, runAt, true, nil, deleted)
}

// runRetentionOnce 是清理本体：返回本轮物理删除的账号总数与首个错误（清理语义不变）。
func (s *CursorAccountRetentionService) runRetentionOnce(ctx context.Context) (int64, error) {
	days := 0
	if s.settingService != nil {
		days = s.settingService.GetCursorAccountRetentionDays(ctx)
	}
	if days <= 0 {
		return 0, nil // 0 或负 = 永久保留，仅支持手动清空
	}
	now := time.Now()
	if s.nowFunc != nil {
		now = s.nowFunc()
	}
	cutoff := now.UTC().AddDate(0, 0, -days)

	var deleted int64
	for round := 0; round < cursorRetentionMaxRounds; round++ {
		ids, err := s.dbRepo.ListExpiredSoftDeletedCursorAccountIDs(ctx, cutoff, cursorRetentionBatchSize)
		if err != nil {
			return deleted, fmt.Errorf("list expired cursor accounts: %w", err)
		}
		if len(ids) == 0 {
			return deleted, nil
		}

		// 1) 先删该批账号的 usage_logs（走既有 DeleteUsageLogsBatch 原语，内含聚合失效处理）。
		if err := s.deleteUsageLogsForAccounts(ctx, ids); err != nil {
			return deleted, fmt.Errorf("delete usage logs for cursor accounts: %w", err)
		}

		// 2) 再物理删账号行（此时其流水已清；不依赖 accounts FK 的 CASCADE 删流水）。
		n, err := s.dbRepo.PhysicallyDeleteAccounts(ctx, ids)
		if err != nil {
			return deleted, fmt.Errorf("physically delete cursor accounts: %w", err)
		}
		deleted += n

		// 单轮批量上限生效：当批不足上限即结束；达上限则继续下一轮。
		if len(ids) < cursorRetentionBatchSize {
			return deleted, nil
		}
	}
	return deleted, nil
}

// recordHeartbeat 写一次 job 心跳（照 quota_key_sweeper.recordHeartbeat 同型）。
func (s *CursorAccountRetentionService) recordHeartbeat(ctx context.Context, runAt time.Time, success bool, runErr error, deleted int64) {
	if s == nil || s.opsRepo == nil {
		return
	}
	hbCtx, hbCancel := context.WithTimeout(context.Background(), cursorAccountRetentionHeartbeatTimeout)
	defer hbCancel()

	durMs := time.Since(runAt).Milliseconds()
	input := &OpsUpsertJobHeartbeatInput{
		JobName:        cursorAccountRetentionJobName,
		LastRunAt:      &runAt,
		LastDurationMs: &durMs,
	}
	if success {
		now := time.Now().UTC()
		input.LastSuccessAt = &now
		result := truncateString(retentionResult(deleted), 2048)
		input.LastResult = &result
	} else if runErr != nil {
		msg := truncateString(runErr.Error(), 2048)
		errAt := time.Now().UTC()
		input.LastErrorAt = &errAt
		input.LastError = &msg
	}
	_ = s.opsRepo.UpsertJobHeartbeat(hbCtx, input)
}

// retentionResult 生成心跳 LastResult 的可读摘要。
func retentionResult(deleted int64) string {
	if deleted == 0 {
		return "no cursor accounts deleted"
	}
	return fmt.Sprintf("deleted %d cursor accounts", deleted)
}

// deleteUsageLogsForAccounts 对给定账号逐个走既有原语删除其全部 usage_logs。
// 每个账号的删除分批循环直到 0，保证不漏删（既不依赖 CASCADE，也不绕过聚合失效）。
func (s *CursorAccountRetentionService) deleteUsageLogsForAccounts(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		accountID := id
		filters := UsageCleanupFilters{
			AccountID: &accountID,
			StartTime: cursorRetentionFarPast,
			EndTime:   cursorRetentionFarFuture,
		}
		for {
			deleted, err := s.usageCleanupRepo.DeleteUsageLogsBatch(ctx, filters, cursorRetentionUsageLogBatch)
			if err != nil {
				return fmt.Errorf("delete usage logs for account %d: %w", id, err)
			}
			if deleted == 0 {
				break
			}
		}
	}
	return nil
}

// cursorRetentionAccountRepo 是 CursorRetentionAccountRepository 的 raw SQL 实现（私有）。
type cursorRetentionAccountRepo struct {
	db *sql.DB
}

func (r *cursorRetentionAccountRepo) ListExpiredSoftDeletedCursorAccountIDs(ctx context.Context, cutoff time.Time, limit int) ([]int64, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("cursor retention account repo not initialized")
	}
	query := `
		SELECT id FROM accounts
		WHERE platform = 'cursor'
		  AND deleted_at IS NOT NULL
		  AND deleted_at < $1
		ORDER BY id ASC
		LIMIT $2`
	rows, err := r.db.QueryContext(ctx, query, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired cursor accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired cursor account id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired cursor accounts: %w", err)
	}
	return ids, nil
}

func (r *cursorRetentionAccountRepo) PhysicallyDeleteAccounts(ctx context.Context, ids []int64) (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("cursor retention account repo not initialized")
	}
	if len(ids) == 0 {
		return 0, nil
	}
	clause, args := buildCursorInClause(ids)
	query := `DELETE FROM accounts WHERE id IN (` + clause + `)`
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("physically delete cursor accounts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("physically delete cursor accounts rows affected: %w", err)
	}
	return n, nil
}

// buildCursorInClause 生成 IN (...) 占位符与参数（驱动无关，避免依赖 pgx/pq 数组类型）。
func buildCursorInClause(ids []int64) (string, []any) {
	parts := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	return strings.Join(parts, ","), args
}

// retentionWarnf 失败仅 WARN 不 panic（与 AuditLogService 同型）。
func retentionWarnf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "time=%s level=WARN msg=\"cursor account retention: "+format+"\"\n",
		append([]any{time.Now().Format(time.RFC3339Nano)}, args...)...)
}
