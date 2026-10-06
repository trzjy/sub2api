package service

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// CodeBuddyQuotaCheckService 周期探测 CodeBuddy 账号的积分额度并应用调度阈值停调。
// 克隆 CNProviderBalanceCheckService 的 Start/Stop/runOnce + ticker 骨架：仅覆盖 codebuddy
// 平台（OAuth 账号），逐账号探测 billing/meter 写 Extra 快照，随后用现有阈值评估
// （ApplyAccountSchedulingThreshold）对达限账号施加临时停调。
//
// 配置 gate.codebuddy.quota_check_enabled 关闭时不启动（默认关闭——计费字段需真实 token
// 抓包确认后再生产启用，见 PR3 计划 §7.1）；interval<=0 同样不启动。
type CodeBuddyQuotaCheckService struct {
	accountRepo   AccountRepository
	quotaService  *CodeBuddyQuotaService
	rateLimitSvc  *RateLimitService
	cfg           *config.Config
	interval      time.Duration
	stopCh        chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup
	mu            sync.Mutex
	lastCheckinDay map[int64]string
}

// NewCodeBuddyQuotaCheckService 构造周期额度检测服务。
func NewCodeBuddyQuotaCheckService(
	accountRepo AccountRepository,
	quotaService *CodeBuddyQuotaService,
	rateLimitSvc *RateLimitService,
	cfg *config.Config,
	interval time.Duration,
) *CodeBuddyQuotaCheckService {
	return &CodeBuddyQuotaCheckService{
		accountRepo:    accountRepo,
		quotaService:   quotaService,
		rateLimitSvc:   rateLimitSvc,
		cfg:            cfg,
		interval:       interval,
		stopCh:         make(chan struct{}),
		lastCheckinDay: make(map[int64]string),
	}
}

// Start 启动周期探测。配置关闭或间隔非法时直接返回（不启动）。
func (s *CodeBuddyQuotaCheckService) Start() {
	if s == nil || s.accountRepo == nil || s.quotaService == nil || s.cfg == nil || s.rateLimitSvc == nil {
		return
	}
	if !s.cfg.Gateway.CodeBuddy.QuotaCheckEnabled {
		return
	}
	if s.interval <= 0 {
		return
	}
	log.Printf("[CodeBuddyQuota] started (interval=%s)", s.interval)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		// 启动后先等一个周期再首次探测，避免与进程启动峰重叠。
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

// Stop 停止周期探测并等待在途任务完成。
func (s *CodeBuddyQuotaCheckService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *CodeBuddyQuotaCheckService) runOnce() {
	accounts, err := s.accountRepo.ListByPlatform(context.Background(), PlatformCodeBuddy)
	if err != nil {
		log.Printf("[CodeBuddyQuota] list codebuddy accounts failed: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for i := range accounts {
		account := &accounts[i]
		if !account.IsActive() || account.Type != AccountTypeOAuth {
			continue
		}
		// 探测积分额度并落 Extra 快照（失败不阻断后续账号）。
		if _, qErr := s.quotaService.QueryUsage(ctx, account.ID); qErr != nil {
			log.Printf("[CodeBuddyQuota] probe account %d failed: %v", account.ID, qErr)
		}
		// 每日签到（默认关；避免重复签到按自然日去重）。
		if s.cfg.Gateway.CodeBuddy.DailyCheckinEnabled {
			s.maybeDailyCheckin(ctx, account)
		}
		// 重新载入（快照已落库）后应用阈值停调。
		fresh, getErr := s.accountRepo.GetByID(ctx, account.ID)
		if getErr != nil {
			log.Printf("[CodeBuddyQuota] reload account %d failed: %v", account.ID, getErr)
			continue
		}
		if s.rateLimitSvc.ApplyAccountSchedulingThreshold(ctx, fresh) {
			log.Printf("[CodeBuddyQuota] account %d paused by scheduling threshold", account.ID)
		}
	}
}

func (s *CodeBuddyQuotaCheckService) maybeDailyCheckin(ctx context.Context, account *Account) {
	today := time.Now().Format("2006-01-02")
	s.mu.Lock()
	last := s.lastCheckinDay[account.ID]
	s.lastCheckinDay[account.ID] = today
	s.mu.Unlock()
	if last == today {
		return
	}
	if err := s.quotaService.DailyCheckin(ctx, account.ID); err != nil {
		log.Printf("[CodeBuddyQuota] daily checkin account %d failed: %v", account.ID, err)
	}
}
