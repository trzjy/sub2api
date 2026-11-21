package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// 停调恢复整改 批4：六源探测 tick 收编。
//
// 全仓「探测域」只保留一处周期 ticker —— AccountHealthRecoveryProbeService.Start
// 的主循环。它每 tick 调一次本注册表的 DispatchDue；六源（CN 额度生命周期、余额快照、
// CN provider、CodeBuddy、Ollama 周期 RunDue、OpenCode Go、Upstream Billing）在各自
// Start() 里向注册表登记，不再自持 ticker。
//
// 设计约束（对应 proposal 外审 6 条 must_fix）：
//   - DispatchDue 非阻塞：只做到期判定并为每个到期任务起独立 goroutine，不阻塞健康恢复主循环。
//   - 不引入硬 deadline：只提供可取消的 roundCtx（仅进程关停/Stop 取消），各任务沿用
//     自身既有内部预算；绝不把 leader 租约当业务 timeout。
//   - Register 按 name 幂等；Unregister 供各服务 Stop() 关闭派发窗口。
//   - nextDue 用固定 cadence 锚点（不按完成时间重算）；运行中到期记为 pending，
//     完成后下一 tick 立即补跑。
//   - 跨副本：每个任务由持有该任务 leader 锁的机器执行；锁后端不可用时不执行（失败关闭）。
//   - 逐任务独立执行（互不阻塞），避免长周期任务饿死短周期任务。
const (
	// backgroundTickLeaderKeyPrefix 是逐任务跨副本 leader 键前缀（键 = 前缀 + 任务名）。
	//
	// 采用逐任务 leader 而非「整轮单 leader + 等待全部任务完成」：后者会让一个 5 分钟
	// 长任务（CN 生命周期/CodeBuddy）把 1 分钟短任务（Ollama/OpenCode/Upstream）饿死
	// 数分钟。逐任务 leader 使每个任务按自身 cadence 独立执行、互不阻塞，同时仍满足
	// 「跨副本加锁去重 + 失败关闭」。
	backgroundTickLeaderKeyPrefix = "background:tick:leader:"
	// backgroundTickLeaseMargin 是逐任务 leader 租约在任务 maxRun 之上的余量。
	backgroundTickLeaseMargin = 60 * time.Second
)

type backgroundTickFirstRun int

const (
	// tickFirstRunImmediate 注册后首个 tick 即跑第一轮。
	tickFirstRunImmediate backgroundTickFirstRun = iota
	// tickFirstRunAfterCadence 等满一个 cadence 再跑第一轮。
	tickFirstRunAfterCadence
	// tickFirstRunAfterDelay 等固定 delay 后跑第一轮。
	tickFirstRunAfterDelay
)

// backgroundTickTask 是一个被注册的周期任务定义。
//
// maxRun 是该任务单次运行的最长墙钟（用于逐任务 leader 租约 = maxRun + 余量，以及
// 文档），**不作为硬 deadline**：任务的真实取消边界由任务自身既有预算或进程关停决定。
// cadence 是该任务的调度周期；唯一 tick 的驱动分辨率必须不慢于最短 cadence
// （见 backgroundTickBaseInterval）。
type backgroundTickTask struct {
	name     string
	cadence  time.Duration
	maxRun   time.Duration
	firstRun backgroundTickFirstRun
	delay    time.Duration
	run      func(ctx context.Context) error
}

type registeredTickTask struct {
	task         backgroundTickTask
	nextDue      time.Time
	running      bool
	enabled      bool
	registeredAt time.Time

	// ctx/cancel 是该任务的生命周期：Unregister 时 cancel，使仍持有该 ctx 的在途
	// run 能尽快返回；wg 统计在途 run，Unregister 等待其归零，保证 service.Stop()
	// 返回后不再遗留该服务的探测任务（外审 #2）。
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// BackgroundTickRegistry 是统一 tick 注册表。
type BackgroundTickRegistry struct {
	mu    sync.Mutex
	tasks map[string]*registeredTickTask
	order []string
	now   func() time.Time

	lock  LeaderLockCache
	owner string
}

// NewBackgroundTickRegistry 构造注册表。
func NewBackgroundTickRegistry() *BackgroundTickRegistry {
	return &BackgroundTickRegistry{
		tasks: make(map[string]*registeredTickTask),
		now:   time.Now,
	}
}

var defaultBackgroundTicks = NewBackgroundTickRegistry()

// DefaultBackgroundTickRegistry 返回进程级默认注册表。
func DefaultBackgroundTickRegistry() *BackgroundTickRegistry { return defaultBackgroundTicks }

// SetDefaultBackgroundTickRegistry 替换进程级默认注册表并返回旧值（测试用；
// 生产仅在装配期经 SetRecoveryProbeDependencies 设置 leader，不替换实例）。
func SetDefaultBackgroundTickRegistry(r *BackgroundTickRegistry) *BackgroundTickRegistry {
	prev := defaultBackgroundTicks
	if r == nil {
		r = NewBackgroundTickRegistry()
	}
	defaultBackgroundTicks = r
	return prev
}

// SetNow 注入时钟（测试用）。
func (r *BackgroundTickRegistry) SetNow(now func() time.Time) {
	if r == nil || now == nil {
		return
	}
	r.mu.Lock()
	r.now = now
	r.mu.Unlock()
}

// SetLeader 注入跨副本 leader 锁（复用批 1 的 Redis-only 锁）。
func (r *BackgroundTickRegistry) SetLeader(lock LeaderLockCache, owner string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.lock = lock
	r.owner = owner
	r.mu.Unlock()
}

// Register 登记（或更新）一个周期任务。按 name 幂等：重复 Start() 不产生第二条，
// 也不重置既有 nextDue。
func (r *BackgroundTickRegistry) Register(task backgroundTickTask) {
	if r == nil || task.name == "" || task.run == nil || task.cadence <= 0 {
		return
	}
	now := r.nowLocked()
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.tasks[task.name]; ok {
		// 更新定义但保留调度状态，避免重复 Start 重置节拍。
		existing.task = task
		return
	}
	next := now
	switch task.firstRun {
	case tickFirstRunAfterCadence:
		next = now.Add(task.cadence)
	case tickFirstRunAfterDelay:
		next = now.Add(task.delay)
	}
	taskCtx, cancel := context.WithCancel(context.Background())
	r.tasks[task.name] = &registeredTickTask{
		task:         task,
		nextDue:      next,
		enabled:      true,
		registeredAt: now,
		ctx:          taskCtx,
		cancel:       cancel,
	}
	r.order = append(r.order, task.name)
}

// Unregister 移除一个任务（幂等）并与其在途执行建立关闭同步；供各服务 Stop()
// 调用。返回前会 cancel 该任务 ctx 并等待在途 run 退出，保证 Stop() 返回后不再
// 遗留该服务的探测任务。
func (r *BackgroundTickRegistry) Unregister(name string) {
	if r == nil || name == "" {
		return
	}
	r.mu.Lock()
	t := r.tasks[name]
	if t == nil {
		r.mu.Unlock()
		return
	}
	t.enabled = false
	delete(r.tasks, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
	// 等待在途 run 退出（可能受自身内部预算约束，最多一个任务周期）。此后该任务
	// 不会再被派发。
	t.wg.Wait()
}

// MinCadence 返回当前已登记任务的最短 cadence（无任务返回 0）。唯一 tick 以其为
// 分辨率上界，保证 1 分钟任务不会被更大的健康探测间隔（最长 600s）粗化。
func (r *BackgroundTickRegistry) MinCadence() time.Duration {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var min time.Duration
	for _, t := range r.tasks {
		if t.task.cadence > 0 && (min == 0 || t.task.cadence < min) {
			min = t.task.cadence
		}
	}
	return min
}

// backgroundTickBaseInterval 计算唯一 tick 的驱动间隔：在不慢于最短注册 cadence 的
// 前提下，尽量沿用健康探测周期；无注册任务时沿用探测周期。
func backgroundTickBaseInterval(probeInterval, minCadence time.Duration) time.Duration {
	base := probeInterval
	if base <= 0 {
		base = 15 * time.Second
	}
	if minCadence > 0 && minCadence < base {
		base = minCadence
	}
	return base
}

// RegisteredCount 返回当前已登记任务数（测试/观测用）。
func (r *BackgroundTickRegistry) RegisteredCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tasks)
}

func (r *BackgroundTickRegistry) nowLocked() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

// DispatchDue 由唯一 tick 每 tick 调用。立即返回，绝不阻塞调用方：只做到期判定并为
// 每个到期任务起一个独立 goroutine（逐任务独立 leader + 独立生命周期），不同任务之间
// 互不阻塞（避免长任务饿死短任务）。
func (r *BackgroundTickRegistry) DispatchDue(ctx context.Context) {
	if r == nil {
		return
	}
	now := r.nowLocked()
	r.mu.Lock()
	lock := r.lock
	owner := r.owner
	var batch []*registeredTickTask
	for _, name := range r.order {
		t := r.tasks[name]
		if t == nil || !t.enabled || t.running {
			continue
		}
		if now.Before(t.nextDue) {
			continue
		}
		t.running = true
		// wg.Add 必须在持锁期间完成：Unregister 持同一把锁删除任务后才 wg.Wait，
		// 二者串行，避免 Add 与 Wait 并发误用。
		t.wg.Add(1)
		// 固定 cadence 锚点前推：一次跨过当前时刻，不按完成时间重算。
		cad := t.task.cadence
		if cad > 0 {
			for !t.nextDue.After(now) {
				t.nextDue = t.nextDue.Add(cad)
			}
		} else {
			t.nextDue = now.Add(24 * time.Hour)
		}
		batch = append(batch, t)
	}
	r.mu.Unlock()

	for _, t := range batch {
		t := t
		go r.runTask(t, lock, owner)
	}
}

// runTask 执行单个任务：先取该任务的跨副本 leader（TTL = maxRun + 余量），失败关闭；
// 任务结束后无论成败都清 running（允许下一个 cadence 再次派发）。
func (r *BackgroundTickRegistry) runTask(t *registeredTickTask, lock LeaderLockCache, owner string) {
	defer t.wg.Done()
	defer func() {
		r.mu.Lock()
		t.running = false
		r.mu.Unlock()
	}()
	ctx := t.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if lock == nil {
		// 失败关闭：无协调后端不执行，避免多副本重复探测。
		return
	}
	key := backgroundTickLeaderKeyPrefix + t.task.name
	ttl := t.task.maxRun + backgroundTickLeaseMargin
	ok, err := tryAcquireRedisLeaderLock(ctx, lock, key, owner, ttl)
	if err != nil || !ok {
		if err != nil {
			logger.L().Warn("background_tick_leader_unavailable",
				zap.String("task", t.task.name), zap.Error(err))
		}
		return
	}
	defer releaseRecoveryLock(lock, key, owner)

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				logger.L().Error("background_tick_task_panic",
					zap.String("task", t.task.name), zap.Any("panic", rec))
			}
		}()
		if runErr := t.task.run(ctx); runErr != nil {
			logger.L().Warn("background_tick_task_failed",
				zap.String("task", t.task.name), zap.Error(runErr))
		}
	}()
}
