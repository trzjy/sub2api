package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// quotaKeyReconcileJobName 是周期 reconciler 在 ops_job_heartbeats 表中的 job 名
	// （照 quota_key_sweep 同型命名，§3.1 / C9）。
	quotaKeyReconcileJobName = "quota_key_reconcile"
	// quotaKeyReconcileInterval 周期 recon 间隔（方案 §6 C9）。
	quotaKeyReconcileInterval = 5 * time.Minute
	// quotaKeyReconcileTimeout 单轮超时（含枚举模型 + 全量串行拉号）。
	quotaKeyReconcileTimeout = 30 * time.Second
	// quotaKeyReconcileHeartbeatTimeout 写心跳的超时（照 quota_key_sweep 同型）。
	quotaKeyReconcileHeartbeatTimeout = 5 * time.Second
	// quotaKeyWaterLevelThreshold 是统一水位阈值（C10 硬口径，不得改动）：
	// 任一允许模型 M 的候选计数低于此值即视为水位不足，按差额触发拉号。
	// 该阈值与单 key 通用语义（MaxAccountsPerQuotaKey=15）解耦——水位衡量的是
	// "可用候选 key 数"，不是单 key 的 working 计数。
	quotaKeyWaterLevelThreshold = 10
)

// quotaKeyPuller 是 reconciler 拉号依赖的窄面（仅为测试替身而存在，接口最小化）。
// 直接落到 QuotaKeyService：PullAccounts（2 参，无模型上下文）/ PullAccountsForModel
// （携带模型 M 上下文，触发 §13.3 分组挂载）。二者内部均持 advisory xact lock +
// FOR UPDATE + 计数停限 + 拉号，working 满 15 即早退；reconciler 不新造计数查询/不新造锁。
type quotaKeyPuller interface {
	PullAccounts(ctx context.Context, quotaKeyID int64) error
	// PullAccountsForModel 携带模型 M 上下文拉号（F2）：水位不足触发补池时由 reconciler
	// 调用，新号在落库事务提交前挂入白名单含 M 的全部分组。
	PullAccountsForModel(ctx context.Context, quotaKeyID int64, modelM string) error
}

// QuotaKeyModelWaterProvider 是 C10 水位 reconciler 需要的"模型 M 上下文"窄面
// （C10 仅定义契约；真实实现由 C11 接入，见禁区：C10 不实现拉号事务改造、
// 不碰 schema/migration、不碰 quota_key_service.go/wire）。
//
// 语义约定（窄接口只暴露编排所需的最小信息，所有 field/聚合 SQL 收敛在 C11 实现内）：
//   - 水位：属于指定模型 M 分组、cursor、schedulable、正常、未软删、且不在 temp 窗口的候选计数；
//   - 阈值：由 reconciler 侧常量 quotaKeyWaterLevelThreshold 统一固定为 10，不随模型变化；
//   - 低于阈值时，reconciler 按"差额 = threshold - current"取候选 key 触发已有
//     quotaKeyPuller.PullAccounts 拉号（不新造拉号接口、不改 key 通用语义）。
type QuotaKeyModelWaterProvider interface {
	// ListAllowedModels 枚举当前允许（在允许 M 分组内）的模型 M。
	ListAllowedModels(ctx context.Context) ([]string, error)
	// WaterLevel 返回指定模型 M 的当前水位（候选计数）与阈值；返回的阈值恒为统一阈值。
	WaterLevel(ctx context.Context, modelM string) (current int, threshold int, err error)
	// ListCandidateKeys 返回指定模型 M 下、需要补池的候选 quota_key ID；
	// limit>0 时由实现方按需截断（reconciler 已传入差额），limit<=0 表示不限制。
	// 返回的 key 已经过 cursor/schedulable/正常/未软删/temp窗口 过滤，reconciler 不再重复过滤。
	ListCandidateKeys(ctx context.Context, modelM string, limit int) ([]int64, error)
}

// QuotaKeyReconciler 是 C10 模型水位 reconciler（补池持久源）。
//
// 用途（方案 §13 慢路径 / F4 持久源）：进程崩溃 / 漏触发后，周期重新发现低于水位的
// 模型 M 分组并补池；与 §3.3 互斥域兼容（同一 key 的拉号由 PullAccounts 内部 advisory
// 锁串行化，reconciler 再叠加单 flight 防止同一 key 并发重复拉号）。
//
// 与旧版（C9 逐 key working<15 补满）的区别：旧版直接扫 quota_keys 逐 key 补满；
// C10 改为"枚举模型 M → 比对每组水位 → 低于阈值按差额触发已有 pull 接口"。
// 模型 M 的枚举/水位/候选 key 查询全部收敛到 QuotaKeyModelWaterProvider 窄面
// （C11 接入真实实现），reconciler 只负责编排与节流。
//
// 严格约束：
//   - 拉号直接调 quotaKeyPuller.PullAccounts（= QuotaKeyService.PullAccounts），
//     不新造锁、不新造计数查询、不直接写 accounts 表；
//   - 失败只记日志、不 panic、不影响下一轮；单 key 拉号失败仅记日志/计数，不中断其余 key；
//   - 单 flight：同一 key 在拉号中若再次被请求，合并为 pending wakeup，不重复发起；
//   - double-checked exit：停止信号置位后不再新发起任何拉号（含 pending wakeup）；
//     发起动作与停机置位共享 coalescer 同一互斥域（F3），Stop() 返回后零新发起；
//   - F4 哨兵消费：拉号返回 ErrQuotaKeyMountFailed（上游成功但本地挂载失败，非幂等边界）
//     时记告警级日志并停止本轮后续候选，写失败心跳记录原因（不重试、不 fallback）；
//   - 无候选 key（所有模型均达水位、或无允许模型、或提供方未接入）时停拉并记录原因。
//
// 生命周期严格复用 QuotaKeySweeper / AccountExpiryService 形态：
// goroutine + time.NewTicker(interval) + 立即 runOnce() 后再按 tick 循环 +
// Stop() 用 stopCh/stopOnce/wg 优雅退出；runOnce() 内用 context.WithTimeout。
type QuotaKeyReconciler struct {
	repo       QuotaKeyRepository
	puller     quotaKeyPuller
	opsRepo    OpsRepository
	modelWater QuotaKeyModelWaterProvider
	interval   time.Duration

	// coalescer 提供单 flight + pending wakeup：同一 key 在拉号中再次被请求时合并，
	// 拉号结束后若有 pending 则补拉一次。
	coalescer *pullCoalescer
	// stopping 由 Stop() 置位，供 double-checked exit 快速判停（无需锁）。
	stopping atomic.Bool
	// pulled 记录本轮成功发起的拉号数（仅用于心跳摘要）。轮内计数：runOnce 每轮开始时
	// 重置（R3-4），使心跳摘要只反映本轮，不把历史轮次的拉号数累计进本轮摘要。
	pulled atomic.Int64

	// accountRemovedPending 标记存在软删唤醒待处理（NotifyAccountRemoved 只置位，不直接拉号）。
	// loop 在每次 runOnce 轮出后判定：若置位则再跑一轮水位检查，确保忙碌期到达的唤醒不丢事件。
	accountRemovedPending atomic.Bool
	// wakeCh 用于即时唤醒 loop（缓冲 1，非阻塞发送），避免等待下一个 5 分钟 tick 才重判水位。
	// 缓冲 1：loop 正忙（在 runOnce 中未在 select 等待）时仍能保留至少一次唤醒，且不阻塞调用方。
	wakeCh chan struct{}

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewQuotaKeyReconciler 构造周期 reconciler。
// repo 为 QuotaKeyRepository（保留构造契约，实际扫描已委托给 modelWater，C11 接线）；
// puller 为拉号窄面（生产传 *QuotaKeyService，测试传替身）；opsRepo 用于写 job 心跳；
// 模型水位提供方通过 SetModelWaterProvider 注入（C11 接线前为 nil，reconciler 安全停拉）。
func NewQuotaKeyReconciler(repo QuotaKeyRepository, puller quotaKeyPuller, opsRepo OpsRepository) *QuotaKeyReconciler {
	r := &QuotaKeyReconciler{
		repo:     repo,
		puller:   puller,
		opsRepo:  opsRepo,
		interval: quotaKeyReconcileInterval,
		wakeCh:   make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
	}
	// F3：coalescer 持有 reconciler 的 stopping 标志，tryBegin 在同一互斥域内检查，
	// 消除"检查后发起前"间隙（Stop 置位后新 flight 直接被拒）。
	r.coalescer = newPullCoalescer(&r.stopping)
	return r
}

// SetModelWaterProvider 注入模型水位提供方（C11 负责接入真实实现）。
// 不传则 reconciler 视为"未接入"，每轮无候选、安全停拉并记录原因。
func (r *QuotaKeyReconciler) SetModelWaterProvider(p QuotaKeyModelWaterProvider) {
	if r == nil {
		return
	}
	r.modelWater = p
}

// NotifyAccountRemoved 是 a/b/c 软删提交后的水位快路径唤醒点（C12-v2，收编进 reconciler）。
// 调用方（QuotaKeyService.DeleteWithAccounts）在软删事务提交成功后调用本方法；本方法
// 只置 pending 唤醒标志，不发起任何拉号——不并行拉号、不新造拉号入口、不触碰 schema/handler。
//
// 与 §13.2 单 flight / double-checked exit 一致：
//   - 不直接拉号：仅唤醒 loop，由 loop 在"轮出前追加一次水位判定"时按缺口补池；
//   - double-checked exit：已 Stop 时直接返回，不置位、不唤醒；
//   - 单 flight：loop 内 runOnce 始终在单 goroutine 中执行，绝不并行拉号；
//   - 忙碌不丢事件：loop 正在 runOnce 期间到达的唤醒会落到 accountRemovedPending
//     （wakeCh 缓冲满时合并），loop 轮出后判定并再跑一轮，确保软删事件不丢。
//
// 本方法刻意"只置 pending"：是否补池、补哪些 key 完全由 reconciler 下一轮 runOnce 的水位
// 判定决定（有缺口才补），从而与 C10 已验收的水位口径 / 单 flight 语义叠加而非另起炉灶。
func (r *QuotaKeyReconciler) NotifyAccountRemoved() {
	if r == nil || r.isStopping() {
		return
	}
	// 仅置 pending 唤醒标志，不直接拉号。缓冲 1 的非阻塞发送用于即时唤醒 loop：
	// loop 忙碌时仍能保留至少一次唤醒且不阻塞调用方（调用方在软删提交路径上，须低延迟返回）。
	r.accountRemovedPending.Store(true)
	select {
	case r.wakeCh <- struct{}{}:
	default:
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
			case <-r.wakeCh:
				// 消费一次唤醒：wakeCh 令牌与 accountRemovedPending 同属【同一次通知】，
				// 故消费令牌即视为该次通知已消费 —— 先清 pending，再跑一轮水位判定
				//（有缺口才补），轮出后的 CAS 补跑因已清零而不再重复（R3-3）。
				r.accountRemovedPending.Store(false)
				r.runOnce()
			case <-r.stopCh:
				return
			}
			// 轮出前追加一次水位判定：若软删唤醒在 runOnce 执行期间（loop 忙碌）到达，
			// 已落到 accountRemovedPending（或 wakeCh 缓冲合并），此处再跑一轮确保不丢事件；
			// 有缺口才补（runOnce 内部水位判定）。若唤醒已被上方分支消费则 CAS 失败，不重复补跑。
			//
			// R3-3（唤醒单次）：wakeCh 令牌与 accountRemovedPending 是同一次通知的两个表征，
			// 消费其一即视为该次通知已消费，故 CAS 补跑分支必须同时非阻塞排空 wakeCh——
			// 否则忙碌期那次通知留下的缓冲令牌会在下一轮 select 中再触发一轮，
			// 使一次 NotifyAccountRemoved 触发两次追加轮次。
			if r.accountRemovedPending.CompareAndSwap(true, false) {
				select {
				case <-r.wakeCh:
				default:
				}
				r.runOnce()
			}
		}
	}()
}

// Stop 停止 reconciler 循环并等待已启动的轮次退出。照 QuotaKeySweeper.Stop。
// 置位 stopping（double-checked exit 生效）并关闭 stopCh 让 ticker goroutine 退出。
//
// F3 停机线性化：置位顺序固定为"先关 coalescer 发起门禁、再置 stopping 快速标志"——
// 二者都指向同一次停机，其中 coalescer.stop() 在与 tryBegin/confirmBegin 相同的 c.mu
// 临界区内完成。于是"发起拉号"与"停机置位"共享同一可线性化点：Stop() 返回后不可能再
// 有新真实拉号发起，已通过的（在途）拉号由 wg.Wait() 等其自然结束。
func (r *QuotaKeyReconciler) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		if r.coalescer != nil {
			r.coalescer.stop()
		}
		r.stopping.Store(true)
		close(r.stopCh)
	})
	r.wg.Wait()
}

// isStopping 返回是否已收到停止信号（供 double-checked exit 使用）。
func (r *QuotaKeyReconciler) isStopping() bool {
	if r == nil {
		return true
	}
	return r.stopping.Load()
}

// runOnce 在一轮超时上下文内：
// 枚举允许模型 M → 逐组比对水位 → 低于阈值按差额取候选 key 触发 PullAccounts 补池
// → 写一次 job 心跳。无候选 key 时停拉并记录原因；失败只记日志、不 panic。
func (r *QuotaKeyReconciler) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), quotaKeyReconcileTimeout)
	defer cancel()
	runAt := time.Now().UTC()
	// R3-4：轮内计数从 0 起算 —— 心跳摘要（reconcileResult）只反映本轮成功发起的拉号数，
	// 不跨轮累计（累计会让后续每轮的摘要都虚高为本轮 + 历史轮次之和）。
	r.pulled.Store(0)

	// 提供方未接入（C11 接线前）：本轮无候选，停拉并记录原因。
	if r.modelWater == nil {
		r.recordHeartbeat(ctx, runAt, true, nil, "model water provider not configured; no candidate keys")
		return
	}

	models, err := r.modelWater.ListAllowedModels(ctx)
	if err != nil {
		log.Printf("[QuotaKeyReconcile] list allowed models failed: %v", err)
		r.recordHeartbeat(ctx, runAt, false, err, "")
		return
	}
	if len(models) == 0 {
		// 无允许模型：无候选 key，停拉并记录原因。
		r.recordHeartbeat(ctx, runAt, true, nil, "no allowed models; no candidate keys")
		return
	}

	candidateCount := 0
	// mountStopReason 非空表示本轮命中 F4 哨兵（ErrQuotaKeyMountFailed）：立即停止本轮
	// 后续候选拉号，并改写失败心跳（不再走成功摘要）。
	mountStopReason := ""
	// R15-1：本轮 ErrUpstreamKeyBlocked（上游失败、key 已 exhausted 占位）计数。
	// 单 key 失败不停轮（既有语义），但收尾心跳必须如实记失败，不得记成功。
	blockedCount := 0
models:
	for _, m := range models {
		// double-checked exit：每处理一组前确认未停止。
		if r.isStopping() {
			break
		}
		current, threshold, werr := r.modelWater.WaterLevel(ctx, m)
		if werr != nil {
			log.Printf("[QuotaKeyReconcile] water level for model %q failed: %v", m, werr)
			continue
		}
		// 已达水位（候选计数 >= 阈值），无需补池。
		if current >= threshold {
			continue
		}
		// 低于阈值：按差额取候选 key。
		deficit := threshold - current
		candidates, cerr := r.modelWater.ListCandidateKeys(ctx, m, deficit)
		if cerr != nil {
			log.Printf("[QuotaKeyReconcile] list candidate keys for model %q failed: %v", m, cerr)
			continue
		}
		if len(candidates) == 0 {
			// 该模型水位不足但无可用候选 key 补池。
			continue
		}
		candidateCount += len(candidates)
		for _, keyID := range candidates {
			if r.isStopping() {
				break
			}
			// F2：持有模型 m 传入 pullKey，使补池拉号携带 M 上下文
			//（触发 §13.3 分组挂载，而非落成裸号）。
			// F4：命中 MountFailed 哨兵 → 停本轮后续候选拉号（含其余模型）。
			if mf, blk := r.pullKey(ctx, keyID, m); mf {
				mountStopReason = fmt.Sprintf("stopped: upstream may have been consumed (mount failed) for quota key %d", keyID)
				_ = blk
				break models
			} else if blk {
				blockedCount++
			}
		}
	}

	// F4：哨兵停轮 —— 写失败心跳并在 LastResult 记录原因（告警日志已在 pullKey 内打出）。
	if mountStopReason != "" {
		r.recordHeartbeat(ctx, runAt, false,
			fmt.Errorf("quota_key_reconcile: %w", ErrQuotaKeyMountFailed), mountStopReason)
		return
	}

	if blockedCount > 0 {
		// R15-1：本轮发生过上游拉号失败（key 已 exhausted 占位）——心跳如实记失败，
		// 不得记成功。不停轮语义保持：仅反映结果，不改变下一轮行为。
		r.recordHeartbeat(ctx, runAt, false,
			fmt.Errorf("quota_key_reconcile: %d upstream pull(s) blocked (keys moved to exhausted placeholder)", blockedCount),
			"")
		return
	}
	if candidateCount == 0 {
		// 无候选 key：停止补池并记录原因（可能是所有模型均达水位，或候选 key 不可补）。
		r.recordHeartbeat(ctx, runAt, true, nil, "no candidate keys for any allowed model")
		return
	}
	r.recordHeartbeat(ctx, runAt, true, nil, "")
}

// pullKey 对单个 quota key 触发拉号，叠加单 flight + pending wakeup + double-checked exit。
// modelM 非空时调用 quotaKeyPuller.PullAccountsForModel（携带 M 上下文、触发 §13.3 挂载）；
// modelM 为空时回退 2 参 PullAccounts（既有兼容路径，不执行分组挂载）。
//   - 单 flight：同 key 已在拉号中 → 合并为 pending，本次不重复发起；
//   - double-checked exit：确认未停止后再真正发起，停止信号置位则不发起；
//   - pending wakeup：拉号期间若再有同 key 请求，结束后自动补拉一次。
//
// 返回值 mountFailed 是 F4 哨兵消费的可识别信号：true 表示本次拉号命中
// ErrQuotaKeyMountFailed（上游已发号成功、本地挂载/落库失败 —— shareai 拉号非幂等，
// 上游该次配额可能已被消耗），调用方（runOnce）据此停止本轮后续候选拉号并写失败心跳。
// 其余错误返回 false，由调用方按既有语义（记日志、继续下一个候选）处理。
func (r *QuotaKeyReconciler) pullKey(ctx context.Context, keyID int64, modelM string) (mountFailed bool, blocked bool) {
	// 单 flight：若同 key 已在拉号，合并为 pending wakeup 后直接返回。
	if !r.coalescer.tryBegin(keyID) {
		return false, false
	}
	// double-checked exit / F3：发起真实拉号前在 coalescer 同一 c.mu 临界区内做最终确认，
	// 与 Stop() 的停机置位可线性化——Stop 置位后绝不新发起（已发起的在途者等待其结束）。
	if !r.coalescer.confirmBegin(keyID) {
		r.coalescer.end(keyID)
		return false, false
	}
	defer func() {
		// pending wakeup：若拉号期间又有同 key 请求，结束后补拉一次（补拉本身同样受门禁）。
		if r.coalescer.end(keyID) {
			if mf, blk := r.pullKey(ctx, keyID, modelM); mf {
				mountFailed = true
			} else if blk {
				blocked = true
			}
		}
	}()

	var err error
	if modelM != "" {
		// F2：水位触发补池携带模型上下文，新号挂入白名单含 M 的分组（非裸号）。
		err = r.puller.PullAccountsForModel(ctx, keyID, modelM)
	} else {
		err = r.puller.PullAccounts(ctx, keyID)
	}
	if err != nil {
		// F4 哨兵消费：上游成功但本地挂载失败（非幂等边界）——告警级日志 + 返回可识别信号。
		// 只带 quota_key_id 与语义原因，不落任何凭据/账号内容。
		if errors.Is(err, ErrQuotaKeyMountFailed) {
			log.Printf("[QuotaKeyReconcile] WARN: quota key %d mount failed after upstream success; upstream may have been consumed once (non-idempotent, no retry this round): %v", keyID, err)
			return true, false
		}
		// R13-2：自动链路未知上游失败（ErrUpstreamKeyBlocked）——记失败级日志（含 key id 与
		// 原因摘要，不落凭据）、不加 pulled 计数、返回 false（不停轮，下一候选继续；单 key 失败
		// 不中断既有语义保持不变）。落库（casSetExhausted 已置 exhausted=true）行为零变化，心跳
		// 摘要 reconcileResult(pulled) 只反映真实成功数，ops 侧据失败日志 + 低计数可辨。
		if errors.Is(err, ErrUpstreamKeyBlocked) {
			log.Printf("[QuotaKeyReconcile] WARN: quota key %d upstream pull blocked (failure counted, not success): %v", keyID, err)
			return false, true
		}
		log.Printf("[QuotaKeyReconcile] pull accounts for quota key %d failed: %v", keyID, err)
		return false, false
	}
	r.pulled.Add(1)
	return false, false
}

// recordHeartbeat 写一次 job 心跳（照 quota_key_sweeper.recordHeartbeat 同型）。
// reason 非空时作为 LastResult 记录（用于"无候选 key 停拉"、F4 哨兵停轮等可观测原因）；
// 否则（成功且无原因）用 reconcileResult(pulled) 做成功摘要。
// 失败心跳同样写入 LastResult（若有原因），使停轮原因在 ops 侧直接可见。
func (r *QuotaKeyReconciler) recordHeartbeat(ctx context.Context, runAt time.Time, success bool, reconcileErr error, reason string) {
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
		var result string
		if reason != "" {
			result = reason
		} else {
			result = reconcileResult(int(r.pulled.Load()))
		}
		rs := truncateString(result, 2048)
		input.LastResult = &rs
	} else {
		// F4：失败轮次也记录原因（哨兵停轮等），便于 ops 侧直接看到停轮原因。
		if reason != "" {
			rs := truncateString(reason, 2048)
			input.LastResult = &rs
		}
		if reconcileErr != nil {
			msg := truncateString(reconcileErr.Error(), 2048)
			errAt := time.Now().UTC()
			input.LastErrorAt = &errAt
			input.LastError = &msg
		}
	}
	_ = r.opsRepo.UpsertJobHeartbeat(hbCtx, input)
}

// reconcileResult 生成心跳 LastResult 的可读摘要（有候选 key 且成功补池时）。
func reconcileResult(pulled int) string {
	if pulled == 0 {
		return "no quota keys reconciled"
	}
	return fmt.Sprintf("reconciled %d quota keys", pulled)
}

// pullCoalescer 提供单 flight + pending wakeup 的 per-key 拉号合并。
//   - tryBegin：首次请求返回 true（应发起拉号）；同 key 在拉号中再次请求返回 false 并将
//     pending 计数 +1（合并，不重复发起）；
//   - confirmBegin：真正发起拉号前的最终门禁（F3），与 tryBegin/stop 同处 c.mu；
//   - end：结束一次拉号，返回是否有 pending 待补拉（true 则调用方应再拉一次）；
//   - stop：置位后 tryBegin/confirmBegin 一律返回 false，配合上层 double-checked exit 彻底停拉。
type pullCoalescer struct {
	mu       sync.Mutex
	inflight map[int64]int
	pending  map[int64]int
	stopped  bool
	// stopping 指向 reconciler 的 stopping 标志（F3）：tryBegin 在同一互斥域内检查，
	// 使 Stop 置位后（无论是否已走到 coalescer.stop()）新 flight 一概被拒，消除
	// "检查后发起前"间隙。nil 表示不关联停止标志（独立单 flight 测试用）。
	stopping *atomic.Bool
}

func newPullCoalescer(stopping *atomic.Bool) *pullCoalescer {
	return &pullCoalescer{
		inflight: make(map[int64]int),
		pending:  make(map[int64]int),
		stopping: stopping,
	}
}

// tryBegin 尝试开启 key 的拉号。返回 true 表示本次调用方应执行拉号；
// 若同 key 已在拉号中，或已停止（F3：stopping 置位，同一互斥域判定），则合并为
// pending wakeup（停止时不再合并，直接拒绝）并返回 false。
func (c *pullCoalescer) tryBegin(keyID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	// F3：停止标志置位即拒绝新 flight，消除"检查后发起前"间隙。
	if c.stopping != nil && c.stopping.Load() {
		return false
	}
	if c.inflight[keyID] > 0 {
		c.pending[keyID]++
		return false
	}
	c.inflight[keyID]++
	return true
}

// confirmBegin 是"发起真实拉号"前的最终门禁（F3 停机线性化）：与 tryBegin / stop 同处
// c.mu 临界区，因此"发起"与"停机置位"共享同一可线性化点，消除"检查后发起前"间隙。
//
// 语义：Stop() 已走完 coalescer.stop()（或与本次确认争用同一把锁并胜出）时一律拒绝；
// 反之本次确认先拿到锁，则视为"已发起/在途"，由 Stop() 的 wg.Wait() 等其自然结束。
// 返回 true 表示本次调用方可以真正发起拉号；false 表示必须放弃并发起 end 归还 flight。
func (c *pullCoalescer) confirmBegin(keyID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	if c.stopping != nil && c.stopping.Load() {
		return false
	}
	return c.inflight[keyID] > 0
}

// end 结束一次拉号；返回 true 表示有待处理的 pending wakeup，调用方应再拉一次。
func (c *pullCoalescer) end(keyID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight[keyID]--
	if c.inflight[keyID] <= 0 {
		delete(c.inflight, keyID)
	}
	n := c.pending[keyID]
	delete(c.pending, keyID)
	return n > 0
}

// stop 置位停止标志：此后 tryBegin 一律返回 false。
func (c *pullCoalescer) stop() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
}

// compile-time guard：*QuotaKeyService 满足 quotaKeyPuller（生产拉号窄面）。
var _ quotaKeyPuller = (*QuotaKeyService)(nil)
