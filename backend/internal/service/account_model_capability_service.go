package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// ErrAccountModelCapabilityInvalid 能力标记记录校验失败。
var ErrAccountModelCapabilityInvalid = errors.New("invalid account model capability record")

// AccountModelCapabilityRepository 定义能力标记的数据访问接口。
//
// 能力是 (账号, 上游模型, 协议) 三元组属性（docs/capability-routing-plan.md §3.3）。
type AccountModelCapabilityRepository interface {
	// Get 按 (accountID, upstreamModel, protocol) 三元组读取一条标记；
	// 未命中返回 (nil, nil)。
	Get(ctx context.Context, accountID int64, upstreamModel, protocol string) (*model.AccountModelCapability, error)
	// Upsert 按唯一键 (account_id, upstream_model, protocol) 插入或更新标记。
	Upsert(ctx context.Context, cap *model.AccountModelCapability) (*model.AccountModelCapability, error)
	// ListByAccount 返回某账号下的全部能力标记。
	ListByAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error)
	// DeleteByAccount 删除某账号下的全部能力标记。
	DeleteByAccount(ctx context.Context, accountID int64) error
}

// AccountModelCapabilityCache 定义能力标记的账号维度二级缓存接口（local + Redis）。
type AccountModelCapabilityCache interface {
	// GetAccount 从缓存获取某账号的标记列表；未命中返回 (nil, false)。
	GetAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, bool)
	// SetAccount 设置某账号的标记列表。
	SetAccount(ctx context.Context, accountID int64, caps []*model.AccountModelCapability) error
	// InvalidateAccount 使某账号的缓存失效（清 local + Redis）。
	InvalidateAccount(ctx context.Context, accountID int64) error
	// NotifyUpdate 广播某账号的缓存失效（跨实例通知）。
	// suspectRedis=true 表示发布方 Redis 失效失败，订阅方须绕开 Redis 直读 DB 权威源
	// （方案 §3.3 失败关闭；合同 §5 禁兜底）。
	NotifyUpdate(ctx context.Context, accountID int64, suspectRedis bool) error
	// SubscribeUpdates 订阅缓存失效广播；回调携带失效账号 ID 与发布方的 Redis 失效是否成功。
	// 订阅方据此推进代际、清本地缓存，并按 suspectRedis 标记/解除 Redis 可疑。
	SubscribeUpdates(ctx context.Context, handler func(accountID int64, suspectRedis bool))
}

// AccountModelCapabilityService 账号模型能力标记服务。
//
// 读路径（ModelSupportsVisionInput）走账号维度 local+Redis 二级缓存；检测/覆盖落库
// （UpsertCapability）后触发该账号缓存失效与跨实例广播，保证多实例即时生效。
type AccountModelCapabilityService struct {
	repo  AccountModelCapabilityRepository
	cache AccountModelCapabilityCache

	// localCache 账号维度本地内存缓存：accountID -> 标记列表。
	localCache   map[int64][]*model.AccountModelCapability
	localCacheMu sync.RWMutex

	// suspectRedisAccounts 记录因失效/广播失败而"二级缓存可能陈旧"的账号。
	// 这些账号后续读取绕过 Redis 直读 DB 权威源、仅回填本地缓存，
	// 禁止把可能陈旧的 Redis 值回填（方案 §3.3；合同 §5 禁兜底）。
	suspectRedisAccounts map[int64]struct{}
	suspectRedisMu       sync.Mutex

	// accountGen 账号级代际计数：每次失效/写操作递增，用于并发回源竞态检测。
	// 回源读取发起前记录代际，安装本地缓存前复核代际未变，变了则说明期间发生了
	// 失效/写（新值已写入），本次旧回源结果丢弃不安装，交由下一请求重读拿到新值，
	// 避免"读请求回源读到旧值→写请求完成失效+刷新→读请求把旧值装回本地缓存"的毒化。
	accountGen map[int64]int64
	genMu      sync.Mutex
}

// NewAccountModelCapabilityService 创建能力标记服务。
func NewAccountModelCapabilityService(
	repo AccountModelCapabilityRepository,
	cache AccountModelCapabilityCache,
) *AccountModelCapabilityService {
	svc := &AccountModelCapabilityService{
		repo:       repo,
		cache:      cache,
		localCache: make(map[int64][]*model.AccountModelCapability),
	}

	// 订阅缓存失效广播：收到通知即推进代际（使本实例并发中的旧回源快照全部作废）、
	// 清空本地缓存，并按发布方 Redis 失效是否成功标记/解除 Redis 可疑。
	// suspect 标记持续到本实例下一次收到 success 事件或自身写路径失效成功——
	// 这是失败关闭的设计意图，不引入 TTL/自动过期兜底（合同 §5 禁兜底）。
	if cache != nil {
		cache.SubscribeUpdates(context.Background(), func(accountID int64, suspectRedis bool) {
			svc.bumpGeneration(accountID) // 推进代际：作废本实例并发中的旧回源快照
			svc.clearLocalAccount(accountID)
			if suspectRedis {
				svc.markSuspectRedis(accountID) // 发布者失效失败：Redis 可能陈旧，本实例绕开
			} else {
				svc.unmarkSuspectRedis(accountID) // 发布者失效成功：Redis 已干净，解除可疑
			}
		})
	}

	return svc
}

// ModelSupportsVisionInput 判断某账号在某模型某协议下是否支持视觉（读图）。
//
// 返回 (supported, known, err)：
//   - 命中能力标记记录 → known=true，supported 取标记的 supports_vision。
//   - 未记录（未检测）→ 返回 (false, false, nil)，调用方按"未知"保守处理（默认放行）。
//   - 缓存刷新/DB 读取失败 → 返回 (false, false, err)：读取错误显式传播，
//     调用方据此失败关闭，禁止静默降级为 unknown 放行（合同 §5 禁兜底；方案 §3.4）。
func (s *AccountModelCapabilityService) ModelSupportsVisionInput(
	ctx context.Context,
	accountID int64,
	upstreamModel, protocol string,
) (supported, known bool, err error) {
	caps, err := s.getAccountCapabilities(ctx, accountID)
	if err != nil {
		// 读取失败：显式传播，调用方失败关闭，不得当作 unknown 静默放行。
		return false, false, err
	}
	for _, cap := range caps {
		if cap.UpstreamModel == upstreamModel && cap.Protocol == protocol {
			return cap.SupportsVision, true, nil
		}
	}
	// 未记录（未检测）→ known=false，调用方按"未知"保守处理（默认放行）。
	return false, false, nil
}

// UpsertCapability 落库（检测/覆盖）一条能力标记，并触发缓存失效广播。
// 供能力检测（方案 §3.5）与手动覆盖（§3.8）使用。
func (s *AccountModelCapabilityService) UpsertCapability(
	ctx context.Context,
	cap *model.AccountModelCapability,
) (*model.AccountModelCapability, error) {
	if err := validateAccountModelCapability(cap); err != nil {
		return nil, err
	}

	upserted, err := s.repo.Upsert(ctx, cap)
	if err != nil {
		return nil, err
	}

	// 该账号缓存失效 + 本地刷新 + 跨实例广播。
	refreshCtx, cancel := s.newCacheRefreshContext()
	defer cancel()
	if err := s.invalidateAndNotifyAccount(refreshCtx, upserted.AccountID); err != nil {
		return nil, fmt.Errorf("invalidate vision capability cache: %w", err)
	}

	return upserted, nil
}

// ListByAccount 透传：按账号读取全部能力标记（管理端展示用）。
func (s *AccountModelCapabilityService) ListByAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error) {
	return s.repo.ListByAccount(ctx, accountID)
}

// DeleteByAccount 透传：删除某账号的全部能力标记。
func (s *AccountModelCapabilityService) DeleteByAccount(ctx context.Context, accountID int64) error {
	if err := s.repo.DeleteByAccount(ctx, accountID); err != nil {
		return err
	}

	refreshCtx, cancel := s.newCacheRefreshContext()
	defer cancel()
	if err := s.invalidateAndNotifyAccount(refreshCtx, accountID); err != nil {
		return fmt.Errorf("invalidate vision capability cache: %w", err)
	}

	return nil
}

// validateAccountModelCapability 校验落库前的记录合法性。
func validateAccountModelCapability(cap *model.AccountModelCapability) error {
	if cap == nil {
		return ErrAccountModelCapabilityInvalid
	}
	if cap.AccountID <= 0 {
		return ErrAccountModelCapabilityInvalid
	}
	if cap.UpstreamModel == "" {
		return ErrAccountModelCapabilityInvalid
	}
	if cap.Protocol != model.CapabilityProtocolChatCompletions &&
		cap.Protocol != model.CapabilityProtocolResponses &&
		cap.Protocol != model.CapabilityProtocolAnthropic {
		return ErrAccountModelCapabilityInvalid
	}
	if cap.Source != model.CapabilitySourceDetect && cap.Source != model.CapabilitySourceManual {
		return ErrAccountModelCapabilityInvalid
	}
	return nil
}

// getAccountCapabilities 获取某账号的标记列表；本地未命中时尝试从缓存/DB 加载。
// 缓存刷新/DB 读取失败显式返回错误，由上层（ModelSupportsVisionInput）传播为失败关闭。
func (s *AccountModelCapabilityService) getAccountCapabilities(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error) {
	s.localCacheMu.RLock()
	caps, ok := s.localCache[accountID]
	s.localCacheMu.RUnlock()
	if ok {
		return caps, nil
	}

	// 尝试刷新该账号缓存（Redis 优先，未命中则回源 DB）。
	// refreshAccountCache 已原子化代际复核：代际失配则重读，耗尽返回显式错误
	// （上层 ModelSupportsVisionInput 据此失败关闭，不得返回陈旧 caps）。
	caps, err := s.refreshAccountCache(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("service.account_model_capability", "[AccountModelCapabilityService] Failed to refresh account %d cache: %v", accountID, err)
		return nil, err
	}

	return caps, nil
}

// refreshAccountCache 刷新某账号的本地缓存：优先 Redis，未命中则回源 DB。
//
// 代际复核原子化（P1-F）：check（genMu）与 install（localCacheMu）在同一把 genMu 持有期
// 内完成，杜绝 bumpGeneration 在二者之间插入导致旧值毒化缓存。安装前代际已变（并发失效/
// 写）则本结果陈旧，丢弃并重读，最多 maxRefreshAttempts 次；耗尽仍失配返回显式错误，
// 不得返回失配的陈旧 caps（上层 ModelSupportsVisionInput 据此失败关闭，方案 §3.4）。
func (s *AccountModelCapabilityService) refreshAccountCache(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error) {
	const maxRefreshAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxRefreshAttempts; attempt++ {
		// 回源前记录代际快照：安装本地缓存前复核代际未变，变了则丢弃本次结果重读，
		// 保证并发下"读旧值 + 并发写新值"时旧值不可能胜出。
		gen := s.getGeneration(accountID)

		// 失效/广播失败窗口内：二级缓存可能陈旧，禁止读取，直接回源 DB。
		var caps []*model.AccountModelCapability
		fromDB := false
		if s.cache != nil && !s.isSuspectRedis(accountID) {
			if got, ok := s.cache.GetAccount(ctx, accountID); ok {
				caps = got
			}
		}

		if caps == nil {
			// Redis 未命中（或可疑绕开）→ 回源 DB。
			fetched, err := s.repo.ListByAccount(ctx, accountID)
			if err != nil {
				// 读取失败：保留底层错误供耗尽后如实返回，短暂重试；
				// 不得与"代际翻转"混同（错误保真，禁止吞错重标记）。
				lastErr = err
				continue
			}
			caps = fetched
			fromDB = true
		}

		// 原子安装：代际未变则安装并返回；已变则丢弃重读（不污染二级缓存）。
		if s.installLocalIfGenerationMatches(accountID, caps, gen) {
			// 仅 DB 回源且二级缓存可信时回写 Redis（回填缺失项）。
			// Redis 命中路径不回写——回写刚读到的同一个值既无意义，也会在
			// "失效失败但本实例未知"的窗口把陈旧值重新盖章（P1-E 方向）；
			// 且仅在代际复核通过后回写，避免把竞态值扩散回 Redis（P1-F）。
			if fromDB && s.cache != nil && !s.isSuspectRedis(accountID) {
				if setErr := s.cache.SetAccount(ctx, accountID, caps); setErr != nil {
					logger.LegacyPrintf("service.account_model_capability", "[AccountModelCapabilityService] Failed to set account %d cache: %v", accountID, setErr)
				}
			}
			return caps, nil
		}
		// 代际已变：本次回源结果陈旧，重读（重记快照重读），不安装也不回写 Redis。
	}
	if lastErr != nil {
		return nil, fmt.Errorf("refresh account %d capability cache: %w", accountID, lastErr)
	}
	return nil, fmt.Errorf("capability cache generation churned during refresh for account %d", accountID)
}

// installLocalIfGenerationMatches 仅在代际仍等于回源前快照时安装本地缓存。
// check（genMu）与 install（localCacheMu）在同一把 genMu 持有期内完成（锁序 genMu →
// localCacheMu，与 clearLocalAccount/bumpGeneration 不构成环），杜绝 bumpGeneration 在
// 二者之间插入导致旧值毒化缓存。返回 false 表示代际已变，本次回源结果陈旧，调用方须重读。
func (s *AccountModelCapabilityService) installLocalIfGenerationMatches(accountID int64, caps []*model.AccountModelCapability, gen int64) bool {
	s.genMu.Lock()
	defer s.genMu.Unlock()
	if s.accountGen[accountID] != gen {
		// 代际已变：本次回源结果陈旧，不写入本地缓存。
		return false
	}
	s.localCacheMu.Lock()
	s.localCache[accountID] = caps
	s.localCacheMu.Unlock()
	return true
}

// clearLocalAccount 清空某账号的本地缓存，避免刷新失败时命中陈旧数据。
func (s *AccountModelCapabilityService) clearLocalAccount(accountID int64) {
	s.localCacheMu.Lock()
	delete(s.localCache, accountID)
	s.localCacheMu.Unlock()
}

// markSuspectRedis 标记某账号二级缓存可能陈旧，后续读取须绕开 Redis 直读 DB。
func (s *AccountModelCapabilityService) markSuspectRedis(accountID int64) {
	s.suspectRedisMu.Lock()
	if s.suspectRedisAccounts == nil {
		s.suspectRedisAccounts = make(map[int64]struct{})
	}
	s.suspectRedisAccounts[accountID] = struct{}{}
	s.suspectRedisMu.Unlock()
}

// unmarkSuspectRedis 在某账号失效成功后清除其可疑标记，恢复正常缓存回源。
func (s *AccountModelCapabilityService) unmarkSuspectRedis(accountID int64) {
	s.suspectRedisMu.Lock()
	delete(s.suspectRedisAccounts, accountID)
	s.suspectRedisMu.Unlock()
}

// isSuspectRedis 判定某账号是否处于"二级缓存可疑"窗口。
func (s *AccountModelCapabilityService) isSuspectRedis(accountID int64) bool {
	s.suspectRedisMu.Lock()
	_, ok := s.suspectRedisAccounts[accountID]
	s.suspectRedisMu.Unlock()
	return ok
}

// getGeneration 读取某账号当前代际计数（并发安全）。
func (s *AccountModelCapabilityService) getGeneration(accountID int64) int64 {
	s.genMu.Lock()
	defer s.genMu.Unlock()
	return s.accountGen[accountID]
}

// bumpGeneration 递增某账号代际计数（失效/写操作调用），并发安全。
func (s *AccountModelCapabilityService) bumpGeneration(accountID int64) {
	s.genMu.Lock()
	defer s.genMu.Unlock()
	if s.accountGen == nil {
		s.accountGen = make(map[int64]int64)
	}
	s.accountGen[accountID]++
}

// newCacheRefreshContext 为写路径缓存同步创建独立上下文，避免受请求取消影响。
func (s *AccountModelCapabilityService) newCacheRefreshContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

// invalidateAndNotifyAccount 使某账号缓存失效并通知其他实例。
//
// 失效或广播任一失败：清除本地缓存、标记该账号二级缓存"可疑"（禁止后续从可能陈旧的
// Redis 回填），并把错误传播到写路径，避免把"未即时生效"的更新报成功（合同 §5 禁兜底；
// 方案 §3.3）。仅当失效成功才回源刷新本地缓存，避免读到陈旧 Redis 值。
func (s *AccountModelCapabilityService) invalidateAndNotifyAccount(ctx context.Context, accountID int64) error {
	// 代际递增：标记该账号进入失效窗口。任何回源读取若在我之前发起（记录的是旧代际），
	// 其安装前复核代际必不相等 → 丢弃旧回源结果，下一请求重读拿到新值，杜绝旧值毒化缓存。
	// 放置于失效/回源之前、且本账号的 DB 写（Upsert/Delete）已由调用方提交之后，
	// 故代际递增后发生的回源读取必然能看到已提交的新值。
	s.bumpGeneration(accountID)

	var invalidateErr, notifyErr error

	// 1) 失效缓存。
	if s.cache != nil {
		if err := s.cache.InvalidateAccount(ctx, accountID); err != nil {
			invalidateErr = err
			logger.LegacyPrintf("service.account_model_capability", "[AccountModelCapabilityService] Failed to invalidate account %d cache: %v", accountID, err)
		} else {
			s.unmarkSuspectRedis(accountID)
		}
	}

	// 2) 仅当失效成功才回源刷新本地缓存；失效失败则清本地并标记可疑，禁止回填。
	if invalidateErr == nil {
		if _, err := s.refreshAccountCache(ctx, accountID); err != nil {
			invalidateErr = fmt.Errorf("refresh local cache: %w", err)
			logger.LegacyPrintf("service.account_model_capability", "[AccountModelCapabilityService] Failed to refresh local cache for account %d: %v", accountID, err)
		}
	}
	if invalidateErr != nil {
		s.clearLocalAccount(accountID)
		s.markSuspectRedis(accountID)
	}

	// 3) 跨实例广播：失效失败也要广播（suspectRedis=true），让其他实例知道须绕开
	// Redis 直读 DB；失效成功则广播 suspectRedis=false，订阅方解除可疑恢复 Redis 回源。
	if s.cache != nil {
		if err := s.cache.NotifyUpdate(ctx, accountID, invalidateErr != nil); err != nil {
			notifyErr = err
			logger.LegacyPrintf("service.account_model_capability", "[AccountModelCapabilityService] Failed to notify cache update for account %d: %v", accountID, err)
			// 广播失败：其他实例可能仍命中旧值，本实例同样清本地并标记可疑。
			s.clearLocalAccount(accountID)
			s.markSuspectRedis(accountID)
		}
	}

	if invalidateErr != nil {
		return invalidateErr
	}
	return notifyErr
}
