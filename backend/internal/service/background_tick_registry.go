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
// 全仓「探测域」只保留一处周期 ticker —— 本注册表自持的唯一 ticker，由
// BackgroundTickRegistry.Start 启动并按 backgroundTickDispatchInterval 间隔调用
// DispatchDue；健康恢复服务不再自持 ticker 主循环。六源（CN 额度生命周期、余额快照、
// CN provider、CodeBuddy、Ollama 周期 RunDue、OpenCode Go、Upstream Billing）在各自
// Start() 里向注册表登记，由该唯一 ticker 统一驱动，不再自持 ticker。Start/Stop 幂等，
// Stop 后可再次 Start 重建 ticker 并恢复派发。
//
// 设计约束（对应 proposal 外审 6 条 must_fix）：
//   - DispatchDue 非阻塞：只做到期判定并为每个到期任务起独立 goroutine，不阻塞注册表 ticker。
//   - 不引入硬 deadline：只提供可取消的 roundCtx（仅进程关停/Stop 取消），各任务沿用
//     自身既有内部预算；不设置统一的业务 timeout。
//   - Register 按 name 幂等；Unregister 供各服务 Stop() 关闭派发窗口。
//   - nextDue 用固定 cadence 锚点（不按完成时间重算）；运行中到期记为 pending，
//     完成后下一 tick 立即补跑。
//   - 去多副本：每个到期任务直接执行 run，不再获取跨副本 leader 租约。
//   - 逐任务独立执行（互不阻塞），避免长周期任务饿死短周期任务。
const (
	// backgroundTickDispatchInterval 是注册表自持唯一 ticker 的派发间隔。注册表在无外部
	// 探测主循环驱动时按此间隔自行调用 DispatchDue（1 秒，不慢于最短 1 分钟 cadence）。
	backgroundTickDispatchInterval = time.Second
	// backgroundTickDrainMargin 是 Unregister 有界排空时在任务 maxRun 之上的等待余量，
	// 避免忽略 ctx 的在途任务阻塞关停。
	backgroundTickDrainMargin = 60 * time.Second
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
// maxRun 是该任务单次运行的最长墙钟估计，用于 Unregister 有界排空的等待上限
// （maxRun + backgroundTickDrainMargin），**不作为硬 deadline**：任务的真实取消
// 边界由任务自身既有预算或进程关停决定。
// cadence 是该任务的调度周期；唯一 tick 的驱动分辨率固定为 backgroundTickDispatchInterval。
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
	// run 能尽快返回；wg 统计在途 run，Unregister 有界等待其归零（超时告警），
	// 避免忽略 ctx 的任务让 service.Stop() 无限等待（外审 #2）。
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

	// startMu 保护自持 ticker 的启停生命周期；started 使 Start/Stop 幂等，
	// Stop 清零 started 以支持再次 Start 重建 ticker（不保留永久 stopped 门闩）。
	startMu    sync.Mutex
	started    bool
	tickCancel context.CancelFunc
	tickWG     sync.WaitGroup
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
// 生产不替换实例）。
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

// Start 启动注册表自持的唯一 ticker（幂等）。首次调用起一个 goroutine：先立即
// DispatchDue 一次，随后每 backgroundTickDispatchInterval 派发一次，直到 Stop 或
// 传入 ctx 取消。重复调用不产生额外 goroutine；Stop 之后可再次 Start 重建 ticker
// 并恢复派发。
func (r *BackgroundTickRegistry) Start(ctx context.Context) {
	if r == nil {
		return
	}
	r.startMu.Lock()
	defer r.startMu.Unlock()
	if r.started {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.started = true
	tickCtx, cancel := context.WithCancel(ctx)
	r.tickCancel = cancel
	r.tickWG.Add(1)
	go func() {
		defer r.tickWG.Done()
		ticker := time.NewTicker(backgroundTickDispatchInterval)
		defer ticker.Stop()
		// 启动先派发一次，随后按固定间隔派发。
		r.DispatchDue(tickCtx)
		for {
			select {
			case <-tickCtx.Done():
				return
			case <-ticker.C:
				r.DispatchDue(tickCtx)
			}
		}
	}()
}

// Stop 停止自持 ticker（幂等）：取消驱动 ctx 并等待 tick goroutine 退出。返回后
// 不再有新的自动派发；重复调用安全。Stop 清零 started，使随后再次 Start 能重建
// ticker 并恢复派发。
func (r *BackgroundTickRegistry) Stop() {
	if r == nil {
		return
	}
	r.startMu.Lock()
	defer r.startMu.Unlock()
	if !r.started {
		return
	}
	r.started = false
	if r.tickCancel != nil {
		r.tickCancel()
		r.tickCancel = nil
	}
	// 持 startMu 等待旧 tick goroutine 完全退出：确保并发 Start 不会在旧 Wait 仍
	// 进行时 Add 到同一 tickWG（WaitGroup 计数归零后不可再并发 Add），也保证新
	// ticker 不会被上一次 Stop 的 Wait 顺带等待。tick goroutine 不取 startMu，无死锁。
	r.tickWG.Wait()
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
// 调用。返回前会 cancel 该任务 ctx 并有界等待在途 run 退出：最多等待
// maxRun + backgroundTickDrainMargin，超时则告警返回，不无限阻塞关停。
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
	// 有界排空：在途 run 已无法再被派发（任务已移出表），只等其自然退出。
	// DispatchDue 的 wg.Add 均在持锁期间完成，删除完成后不再有新的 Add，
	// 故此处在锁外起 goroutine 等待不会与 Add 并发误用。
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	wait := t.task.maxRun + backgroundTickDrainMargin
	if wait <= 0 {
		wait = backgroundTickDrainMargin
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		// 忽略 ctx 的在途任务（如先枚举再建预算 ctx 的 provider）可能不响应取消，
		// 超时后不再等待，避免 service.Stop() 无限挂起。
		logger.L().Warn("background_tick_stop_drain_timeout", zap.String("task", name))
	}
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
// 每个到期任务起一个独立 goroutine（独立生命周期），不同任务之间互不阻塞（避免长任务
// 饿死短任务）。
func (r *BackgroundTickRegistry) DispatchDue(ctx context.Context) {
	if r == nil {
		return
	}
	now := r.nowLocked()
	r.mu.Lock()
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
		go r.runTask(t)
	}
}

// runTask 直接执行单个任务（去多副本：不再获取跨副本 leader 租约）。任务结束后
// 无论成败都清 running（允许下一个 cadence 再次派发）。
func (r *BackgroundTickRegistry) runTask(t *registeredTickTask) {
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
	r.executeTask(t, ctx)
}

// executeTask 执行单个 run，隔离 panic 并记录失败。
func (r *BackgroundTickRegistry) executeTask(t *registeredTickTask, ctx context.Context) {
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
