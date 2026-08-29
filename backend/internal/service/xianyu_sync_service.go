package service

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"time"
)

// XianyuSyncService 运行闲鱼控制面的后台任务：
//   - 健康检查：默认 60s
//   - 账号同步：默认 5 分钟
//   - 商品同步 + 自动绑定：默认 5 分钟，使用 PostgreSQL advisory lock 全局互斥
//   - 告警巡检：复用管理员邮件通知机制
type XianyuSyncService struct {
	control *XianyuControlService
	worker  *XianyuWorkerService
	alert   *XianyuAlertService
	db      *sql.DB
	setting XianyuDeliverySettingReader

	healthInterval time.Duration
	syncInterval   time.Duration
	parentCtx      context.Context
	parentCancel   context.CancelFunc
	stopCh         chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
}

// NewXianyuSyncService 创建同步任务服务。
func NewXianyuSyncService(control *XianyuControlService, worker *XianyuWorkerService, alert *XianyuAlertService, db *sql.DB, setting XianyuDeliverySettingReader) *XianyuSyncService {
	ctx, cancel := context.WithCancel(context.Background())
	return &XianyuSyncService{
		control:        control,
		worker:         worker,
		alert:          alert,
		db:             db,
		setting:        setting,
		healthInterval: 60 * time.Second,
		syncInterval:   5 * time.Minute,
		parentCtx:      ctx,
		parentCancel:   cancel,
		stopCh:         make(chan struct{}),
	}
}

// Start 启动后台任务（符合既有 Start/Stop 服务模式）。
func (s *XianyuSyncService) Start() {
	if s == nil || s.control == nil || s.worker == nil || s.db == nil {
		return
	}
	s.wg.Add(2)
	go s.healthLoop()
	go s.syncLoop()
}

func (s *XianyuSyncService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.parentCancel()
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *XianyuSyncService) healthLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			if !s.control.Enabled(s.parentCtx) {
				continue
			}
			if err := s.worker.CheckHealth(s.parentCtx); err != nil {
				log.Printf("[XianyuSync] health check failed: %v", err)
			}
			// 健康时同步账号（保证状态新鲜）。
			if s.accountAutoRefreshEnabled(s.parentCtx) {
				_ = s.worker.SyncAccounts(s.parentCtx)
			}
			// 每次巡检评估告警。
			if s.alert != nil {
				s.alert.Evaluate(s.parentCtx)
			}
		}
	}
}

// syncLoop 每周期同步商品并自动绑定；advisory lock 阻止并发与立即刷新重复执行。
func (s *XianyuSyncService) syncLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.currentSyncInterval(s.parentCtx))
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			if !s.control.Enabled(s.parentCtx) {
				continue
			}
			if err := s.RunProductSync(s.parentCtx); err != nil {
				log.Printf("[XianyuSync] product sync failed: %v", err)
			}
		}
	}
}

func (s *XianyuSyncService) settings(ctx context.Context) XianyuSettings {
	if s.control == nil {
		return XianyuSettings{}
	}
	settings, err := s.control.GetSettings(ctx)
	if err != nil {
		return XianyuSettings{}
	}
	return settings
}

func (s *XianyuSyncService) currentSyncInterval(ctx context.Context) time.Duration {
	minutes := s.settings(ctx).SyncIntervalMinutes
	if minutes < 1 {
		minutes = int(s.syncInterval / time.Minute)
	}
	if minutes < 1 {
		minutes = 1
	}
	return time.Duration(minutes) * time.Minute
}

func (s *XianyuSyncService) accountAutoRefreshEnabled(ctx context.Context) bool {
	return s.settings(ctx).AccountAutoRefresh
}

// advisoryLockKey 商品同步 advisory lock 键（与主程序内其他锁隔离）。
const xianyuProductSyncLockKey = 96812091411 // arbitrary stable int

// RunProductSync 获取全局互斥锁后同步商品并自动绑定。
// 拿不到锁直接返回忙（busy），不排队。
func (s *XianyuSyncService) RunProductSync(ctx context.Context) error {
	if s == nil || s.db == nil || s.worker == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, xianyuProductSyncLockKey).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return ErrXianyuSyncBusy
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, xianyuProductSyncLockKey)
	}()

	if !s.settings(ctx).ProductAutoBind {
		return s.worker.SyncProducts(ctx)
	}
	if err := s.worker.SyncProducts(ctx); err != nil {
		return err
	}
	if err := s.control.AutoBindProducts(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
