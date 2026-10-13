package service

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	// quotaKeyReconcileJobName 是周期 reconciler 在 ops_job_heartbeats 表中的 job 名
	// （照 quota_key_sweep 同型命名，§3.1 / C9）。
	quotaKeyReconcileJobName = "quota_key_reconcile"
	// quotaKeyReconcileInterval 周期 recon 间隔（方案 §6 C9）。
	quotaKeyReconcileInterval = 5 * time.Minute
	// quotaKeyReconcileTimeout 单轮超时（含 ListEligibleForRefresh + 全量串行拉号）。
	quotaKeyReconcileTimeout = 30 * time.Second
	// quotaKeyReconcileHeartbeatTimeout 写心跳的超时（照 quota_key_sweep 同型）。
	quotaKeyReconcileHeartbeatTimeout = 5 * time.Second
)

// quotaKeyPuller 是 reconciler 拉号依赖的窄面（仅为测试替身而存在，接口最小化）。
// 直接落到 QuotaKeyService.PullAccounts：其内部持 advisory xact lock + FOR UPDATE +
// 计数停限 + 拉号，working 满 15 即早退；reconciler 不新造计数查询/不新造锁。
type quotaKeyPuller interface {
	PullAccounts(ctx context.Context, quotaKeyID int64) error
}

// QuotaKeyReconciler 是 C9 周期 reconciler（补池持久源）。
//
// 用途（方案 §3.1 慢路径 / §6 C9，F4 持久源）：进程崩溃 / 漏触发后，周期重新发现
// working_count < 15 的激活码并补池；与 §3.3 互斥域兼容（同一 key 不并发拉号，
// 因为 PullAccounts 内部 advisory 锁串行化）。
//
// 严格约束：
//   - 扫描走 QuotaKeyRepository.ListEligibleForRefresh（已存在，status=active &&
//     !exhausted && 未过期 && 未软删），不新造聚合计数 SQL；
//   - 拉号直接调 quotaKeyPuller.PullAccounts（= QuotaKeyService.PullAccounts），
//     不新造锁、不新造计数查询、不直接写 accounts 表；
//   - 失败只记日志、不 panic、不影响下一轮（照 AccountExpiryService / QuotaKeySweeper
//     容错基调）；单 key 拉号失败仅记日志/计数，不中断其余 key。
//
// 生命周期严格复用 QuotaKeySweeper / AccountExpiryService 形态：
// goroutine + time.NewTicker(interval) + 立即 runOnce() 后再按 tick 循环 +
// Stop() 用 stopCh/stopOnce/wg 优雅退出；runOnce() 内用 context.WithTimeout。
//
// 为什么串行（而非每 key 起一个 worker）：每 key 的拉号由 PullAccounts 内部独立
// advisory 锁保护，彼此不共享计数；且 PullAccounts 单批即返回（满 15 早退），不会
// 占死 worker。串行即可保证不漏 key，又避免 goroutine 风暴（可刷新 key 可能很多）。
type QuotaKeyReconciler struct {
	repo     QuotaKeyRepository
	puller   quotaKeyPuller
	opsRepo  OpsRepository
	interval time.Duration

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewQuotaKeyReconciler 构造周期 reconciler。
// repo 为 QuotaKeyRepository（仅 quota_keys 操作，天然不触碰 accounts）；
// puller 为拉号窄面（生产传 *QuotaKeyService，测试传替身）；opsRepo 用于写 job 心跳。
func NewQuotaKeyReconciler(repo QuotaKeyRepository, puller quotaKeyPuller, opsRepo OpsRepository) *QuotaKeyReconciler {
	return &QuotaKeyReconciler{
		repo:     repo,
		puller:   puller,
		opsRepo:  opsRepo,
		interval: quotaKeyReconcileInterval,
		stopCh:   make(chan struct{}),
	}
}

// Start 启动 reconciler 循环（goroutine + ticker，立即跑一轮）。照 QuotaKeySweeper.Start。
func (r *QuotaKeyReconciler) Start() {
	if r == nil || r.repo == nil || r.puller == nil || r.interval <= 0 {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()

		r.runOnce()
		for {
			select {
			case <-ticker.C:
				r.runOnce()
			case <-r.stopCh:
				return
			}
		}
	}()
}

// Stop 停止 reconciler 循环并等待已启动的轮次退出。照 QuotaKeySweeper.Stop。
func (r *QuotaKeyReconciler) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
	r.wg.Wait()
}

// runOnce 在一轮超时上下文内：扫描可刷新激活码 → 串行逐 key 调 PullAccounts 补池
// → 最后写一次 job 心跳。失败只记日志、不 panic；单 key 拉号失败不中断其余 key。
func (r *QuotaKeyReconciler) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), quotaKeyReconcileTimeout)
	defer cancel()
	runAt := time.Now().UTC()

	keys, err := r.repo.ListEligibleForRefresh(ctx)
	if err != nil {
		log.Printf("[QuotaKeyReconcile] list eligible keys failed: %v", err)
		r.recordHeartbeat(ctx, runAt, false, err, 0)
		return
	}

	pulled := 0
	for _, key := range keys {
		// 单 key 拉号失败仅记日志/计数，继续下一个 key（绝不中断其余 key）。
		if err := r.puller.PullAccounts(ctx, key.ID); err != nil {
			log.Printf("[QuotaKeyReconcile] pull accounts for quota key %d failed: %v", key.ID, err)
			continue
		}
		pulled++
	}
	if pulled > 0 {
		log.Printf("[QuotaKeyReconcile] reconciled %d quota keys", pulled)
	}
	// 每轮写且仅写一次心跳（无论成功或扫描失败路径都已在上方各分支写一次）。
	r.recordHeartbeat(ctx, runAt, true, nil, pulled)
}

// recordHeartbeat 写一次 job 心跳（照 quota_key_sweeper.recordHeartbeat 同型）。
func (r *QuotaKeyReconciler) recordHeartbeat(ctx context.Context, runAt time.Time, success bool, reconcileErr error, pulled int) {
	if r == nil || r.opsRepo == nil {
		return
	}
	hbCtx, hbCancel := context.WithTimeout(context.Background(), quotaKeyReconcileHeartbeatTimeout)
	defer hbCancel()

	durMs := time.Since(runAt).Milliseconds()
	input := &OpsUpsertJobHeartbeatInput{
		JobName:        quotaKeyReconcileJobName,
		LastRunAt:      &runAt,
		LastDurationMs: &durMs,
	}
	if success {
		now := time.Now().UTC()
		input.LastSuccessAt = &now
		result := truncateString(reconcileResult(pulled), 2048)
		input.LastResult = &result
	} else if reconcileErr != nil {
		msg := truncateString(reconcileErr.Error(), 2048)
		errAt := time.Now().UTC()
		input.LastErrorAt = &errAt
		input.LastError = &msg
	}
	_ = r.opsRepo.UpsertJobHeartbeat(hbCtx, input)
}

// reconcileResult 生成心跳 LastResult 的可读摘要。
func reconcileResult(pulled int) string {
	if pulled == 0 {
		return "no quota keys reconciled"
	}
	return fmt.Sprintf("reconciled %d quota keys", pulled)
}

// compile-time guard：*QuotaKeyService 满足 quotaKeyPuller（生产拉号窄面）。
var _ quotaKeyPuller = (*QuotaKeyService)(nil)
