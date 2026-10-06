package service

import (
	"context"
	"log"
	"sync"
	"time"
)

// WelfareBalanceExpiryService 定期清零到期福利余额。
// 只动 welfare_balances（置 status='expired' 且 amount_remaining=0），
// 绝不触碰 users.balance；重复执行天然幂等，失败下一轮重试。
type WelfareBalanceExpiryService struct {
	welfareRepo WelfareRepository
	interval    time.Duration
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

func NewWelfareBalanceExpiryService(welfareRepo WelfareRepository, interval time.Duration) *WelfareBalanceExpiryService {
	return &WelfareBalanceExpiryService{
		welfareRepo: welfareRepo,
		interval:    interval,
		stopCh:      make(chan struct{}),
	}
}

func (s *WelfareBalanceExpiryService) Start() {
	if s == nil || s.welfareRepo == nil || s.interval <= 0 {
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

func (s *WelfareBalanceExpiryService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *WelfareBalanceExpiryService) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cleared, amount, err := s.welfareRepo.ExpireDueBalances(ctx)
	if err != nil {
		log.Printf("[WelfareBalanceExpiry] Expire due welfare balances failed: %v", err)
		return
	}
	if cleared > 0 {
		log.Printf("[WelfareBalanceExpiry] Cleared %d expired welfare balances, total %.2f", cleared, amount)
	}
}
