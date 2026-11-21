package service

import (
	"context"
	"fmt"
	"time"
)

// ApplyModelRateLimitObservationIfVersion 是探测成功的版本化清除入口：
// 只有当前 meta.last_event_at 仍等于探测发起时读到的原文时才清除该 scope；
// 返回 applied=false 表示探测期间已被新事件接管。
func (s *RateLimitService) ApplyModelRateLimitObservationIfVersion(
	ctx context.Context, accountID int64, scope string, obs ModelRateLimitObservation, expectedLastEventAt string,
) (bool, error) {
	repo := s.observationRepo()
	if repo == nil {
		return false, fmt.Errorf("model rate limit observation: repository does not implement narrow interface")
	}
	versioned, ok := s.accountRepo.(interface {
		CommitModelRateLimitObservationIfVersion(ctx context.Context, id int64, scope string, entry map[string]any, clear bool, lastEventAt time.Time, revision int64, expectedLastEventAt string) (bool, error)
	})
	if !ok {
		return false, fmt.Errorf("model rate limit observation: versioned commit not supported")
	}
	var applied bool
	err := repo.WithModelRateLimitAccountLock(ctx, accountID, func(ctx context.Context) error {
		lastEventAt, rev, has, err := repo.GetModelRateLimitMeta(ctx, accountID, scope)
		if err != nil {
			return err
		}
		if has && obs.EventTime.Before(lastEventAt) {
			return nil
		}
		effectiveTB := obs.TieBreaker
		if effectiveTB == 0 {
			effectiveTB = rev + 1
		}
		if has && obs.EventTime.Equal(lastEventAt) && effectiveTB <= rev {
			return nil
		}
		applied, err = versioned.CommitModelRateLimitObservationIfVersion(ctx, accountID, scope, nil, true, obs.EventTime, effectiveTB, expectedLastEventAt)
		return err
	})
	return applied, err
}
