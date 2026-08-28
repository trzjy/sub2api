package service

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

const (
	BalanceProbeSnapshotExtraKey       = "balance_probe_snapshot"
	balanceProbeDefaultIntervalMinutes = 10
	balanceProbeCycleInterval          = time.Minute
	balanceProbeMaxPerCycle            = 20
	balanceProbeConcurrency            = 4
	balanceProbeLeaderLockKey          = "account:balance:probe:leader"
	balanceProbeLeaderLockTTL          = 2 * time.Minute
)

type balanceProbeSnapshotWriter interface {
	UpdateExtra(ctx context.Context, id int64, updates map[string]any) error
}

type balanceProbeQuery func(ctx context.Context, accountID int64) (*BalanceProbeResult, error)

type balanceProbeCandidateLister interface {
	ListDueBalanceProbeAccounts(ctx context.Context, now time.Time, limit int) ([]Account, error)
}

type AccountBalanceProbeCheckService struct {
	accountRepo AccountRepository
	probe       *AccountBalanceProbeService
	query       balanceProbeQuery
	interval    time.Duration
	lockCache   LeaderLockCache
	db          *sql.DB

	parentCtx    context.Context
	parentCancel context.CancelFunc
	stopCh       chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
	instanceID   string
}

func NewAccountBalanceProbeCheckService(accountRepo AccountRepository, probe *AccountBalanceProbeService, interval time.Duration) *AccountBalanceProbeCheckService {
	ctx, cancel := context.WithCancel(context.Background())
	service := &AccountBalanceProbeCheckService{
		accountRepo:  accountRepo,
		probe:        probe,
		interval:     interval,
		parentCtx:    ctx,
		parentCancel: cancel,
		stopCh:       make(chan struct{}),
		instanceID:   uuid.NewString(),
	}
	service.query = func(queryCtx context.Context, queryAccountID int64) (*BalanceProbeResult, error) {
		return probe.Query(queryCtx, queryAccountID)
	}
	return service
}

func newTestAccountBalanceProbeCheckService(accountRepo AccountRepository, query balanceProbeQuery, interval time.Duration) *AccountBalanceProbeCheckService {
	ctx, cancel := context.WithCancel(context.Background())
	svc := &AccountBalanceProbeCheckService{
		accountRepo:  accountRepo,
		query:        query,
		interval:     interval,
		parentCtx:    ctx,
		parentCancel: cancel,
		stopCh:       make(chan struct{}),
		instanceID:   uuid.NewString(),
	}
	return svc
}

func ProvideAccountBalanceProbeCheckService(accountRepo AccountRepository, probe *AccountBalanceProbeService, lockCache LeaderLockCache, db *sql.DB, cfg *config.Config) *AccountBalanceProbeCheckService {
	minutes := balanceProbeDefaultIntervalMinutes
	if cfg != nil && cfg.Gateway.APIKeyBalanceProbe.IntervalMinutes > 0 {
		minutes = cfg.Gateway.APIKeyBalanceProbe.IntervalMinutes
	}
	service := NewAccountBalanceProbeCheckService(accountRepo, probe, time.Duration(minutes)*time.Minute)
	service.SetLeaderLock(lockCache, db)
	service.Start()
	return service
}

func (s *AccountBalanceProbeCheckService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

func (s *AccountBalanceProbeCheckService) Start() {
	if s == nil || s.accountRepo == nil || s.probe == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.parentCtx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				if err := s.RunDue(s.parentCtx); err != nil {
					log.Printf("[APIKeyBalanceProbe] run due failed: %v", err)
				}
			}
		}
	}()
}

func (s *AccountBalanceProbeCheckService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.parentCancel()
	})
	s.wg.Wait()
}

func (s *AccountBalanceProbeCheckService) RunDue(ctx context.Context) error {
	if s == nil || s.accountRepo == nil || s.query == nil {
		return nil
	}
	release, acquired := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, balanceProbeLeaderLockKey, s.instanceID, balanceProbeLeaderLockTTL)
	if !acquired {
		return nil
	}
	defer release()

	accounts, err := s.listDueAccounts(ctx)
	if err != nil {
		return err
	}
	var group errgroup.Group
	for i := range accounts {
		accountID := accounts[i].ID
		group.Go(func() error {
			if probeErr := s.probeAndPersist(ctx, accountID); probeErr != nil {
				log.Printf("[APIKeyBalanceProbe] probe failed: account_id=%d err=%v", accountID, probeErr)
			}
			return nil
		})
	}
	return group.Wait()
}

func (s *AccountBalanceProbeCheckService) listDueAccounts(ctx context.Context) ([]Account, error) {
	if lister, ok := s.accountRepo.(balanceProbeCandidateLister); ok {
		return lister.ListDueBalanceProbeAccounts(ctx, time.Now(), balanceProbeMaxPerCycle)
	}
	return s.accountRepo.ListActive(ctx)
}

func (s *AccountBalanceProbeCheckService) probeAndPersist(ctx context.Context, accountID int64) error {
	result, err := s.query(ctx, accountID)
	if err != nil {
		result = &BalanceProbeResult{Success: false, Valid: false, FetchedAt: time.Now().UTC(), Error: err.Error()}
	}
	if result == nil {
		return nil
	}
	writer, ok := s.accountRepo.(balanceProbeSnapshotWriter)
	if !ok {
		return nil
	}
	return writer.UpdateExtra(ctx, accountID, map[string]any{BalanceProbeSnapshotExtraKey: result})
}
