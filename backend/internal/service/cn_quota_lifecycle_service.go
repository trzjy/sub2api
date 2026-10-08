package service

// CNQuotaLifecycleService：TH（TokenHarbor）/ Kira 账号额度耗尽状态机（方案
// th-kira-quota-lifecycle §4.1/§4.4，派发单 D-QL-001）。
//
// 状态流（L2/L3/L4 裁定）：
//
//	用户调用 → 上游 402/429（额度耗尽统一口径，账号级）
//	  → 确认探针 ×1（本服务唯一探测面，不走周期扫描）
//	      ├─ 确认耗尽 → 停调（temp_unschedulable 到期=官方恢复时间，L4 优先级取值；
//	     │              无官方值登记未知→远期占位）+ fire OpsAlertEvent（kind=quota_exhausted）
//	      ├─ 探针成功（瞬时信号）→ 不停调不告警，保持现状
//	      └─ 探测不确定（网络/5xx/鉴权/解析失败）→ 失败关闭：不动现状态（绝不把
//	                 探测失败当已恢复）+ fire 告警，进入 5 分钟确认循环
//	恢复时间到期 → 确认探针 ×1（RunRecoverySweep，5 分钟节奏由 Start() ticker 驱动）
//	      ├─ 成功 → 清停调 + resolve 告警 + 刷新用量快照（可选注入）→ 闭环
//	      ├─ 仍耗尽 → 保持停调（到期已过则按 L4 重取恢复时间续停）+ 告警保持 firing
//	      └─ 不确定 → 失败关闭 + 保持停调 + 告警保持 firing
//
// 两条链的差异只有恢复时间来源（L1/L7）：
//   - TH 订阅链：extra th_pass_snapshot.renews_at 只是订阅续期日（≈28 天），与
//     额度周期无关，不参与恢复判定；真实额度周期 = free-tier reset_at（7 天窗口，
//     重置时刻在 /api/me/free-tier，见 resolveRecoveryTime）。
//   - TH 免费链：7 天滚动无精确时刻 → 恢复时间未知，靠 5 分钟确认循环兜底
//   - Kira 免费链：每日重置（站点按越南时区，由当日窗口推导）
//   - 上游响应内重置时间永远优先（L4）
//
// 告警只走 OpsAlertEvent（QuotaAlertStore 窄面，生产由 OpsService 实现），
// 不新增邮件/通知管道。停调载体沿用 temp_unschedulable 字段语义，但到期时间
// =官方恢复时间，不再是 2×interval 滚动冷却（滚动冷却在 D-QL-002 退役）。
//
// 状态持久化：账号 extra 键 cn_quota_lifecycle 记录恢复时间/来源/最近探测结果，
// 供跨重启的 sweep 候选发现（ListTempUnschedulableAccounts + 本键过滤）；进程内
// 另维护不确定路径（未停调账号）的 5 分钟循环候选集。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

const (
	// cnQuotaLifecycleExtraKey 账号 extra 中状态机状态记录的键。
	cnQuotaLifecycleExtraKey = "cn_quota_lifecycle"

	// cnQuotaExhaustedReasonPrefix 额度耗尽停调 reason 的稳定前缀（sweep 候选识别）。
	cnQuotaExhaustedReasonPrefix = "cn_quota_exhausted"

	// quotaExhaustedAlertKind OpsAlertEvent.Dimensions["kind"] 取值（方案 §4.1）。
	quotaExhaustedAlertKind = "quota_exhausted"

	// quotaLifecycleSweepInterval 恢复确认循环节奏（L3：每 5 分钟一次探针）。
	quotaLifecycleSweepInterval = 5 * time.Minute

	// quotaLifecycleUnknownRecoveryPlaceholder 恢复时间未知的远期占位停调时长。
	// 循环确认探针成功即提前清除，占位本身不是恢复依据。
	quotaLifecycleUnknownRecoveryPlaceholder = 7 * 24 * time.Hour

	// quotaAlertSeverity 额度耗尽告警严重级别（与新鲜度告警同档）。
	quotaAlertSeverity = "warning"

	// 恢复时间来源（L4 优先级：upstream > snapshot > unknown 兜底循环）。
	cnQuotaRecoverySourceUpstream = "upstream"
	cnQuotaRecoverySourceSnapshot = "snapshot"
	cnQuotaRecoverySourceUnknown  = "unknown"

	// 状态机状态取值。
	cnQuotaLifecycleStateExhausted  = "exhausted"
	cnQuotaLifecycleStateUncertain  = "uncertain"
	cnQuotaLifecycleStateRecovered  = "recovered"
	cnQuotaLifecycleProbeExhausted  = "exhausted"
	cnQuotaLifecycleProbeUncertain  = "uncertain"
	cnQuotaLifecycleProbeRecovered  = "recovered"

	cnQuotaLifecycleProviderKira        = "kira"
	cnQuotaLifecycleProviderTokenHarbor = "tokenharbor"
)

// QuotaAlertStore 额度耗尽告警持久化抽象（照 FreshnessAlertStore 模式，生产由
// OpsService 复用 OpsAlertEvent 实现）。仅取子集能力，便于测试用 fake。
type QuotaAlertStore interface {
	CreateAlertEvent(ctx context.Context, event *OpsAlertEvent) (*OpsAlertEvent, error)
	// GetActiveQuotaAlert 按维度返回当前 firing 的额度耗尽告警（无则 nil）。
	GetActiveQuotaAlert(ctx context.Context, dims map[string]any) (*OpsAlertEvent, error)
	// ResolveQuotaAlertOnRecovery 不受监控开关门禁约束的恢复关闭窄面（与
	// ResolveFreshnessAlertOnRecovery 同模式）：无活动告警幂等 no-op。
	ResolveQuotaAlertOnRecovery(ctx context.Context, dims map[string]any) error
}

// QuotaSnapshotRefresher 恢复成功后的用量快照刷新窄面（可选注入；快照探测本体
// 属 CNProviderQuotaService/TH/Kira 探测链，由后续派发单接线）。nil 时跳过刷新。
type QuotaSnapshotRefresher interface {
	RefreshCNQuotaSnapshot(ctx context.Context, account *Account) error
}

// quotaProbeOutcome 确认探针三态结果。
type quotaProbeOutcome int

const (
	quotaProbeUncertain quotaProbeOutcome = iota // 网络/5xx/鉴权/解析失败 → 失败关闭
	quotaProbeExhausted                          // 确认仍耗尽
	quotaProbeRecovered                          // 上游已恢复（2xx / 免费池余量>0）
)

// cnQuotaLifecycleState extra 持久化的状态机状态记录。
type cnQuotaLifecycleState struct {
	State            string `json:"state"`
	RecoveryAt       string `json:"recovery_at,omitempty"`       // 空 = 恢复时间未知
	RecoverySource   string `json:"recovery_source,omitempty"`   // upstream / snapshot / unknown
	LastProbeAt      string `json:"last_probe_at,omitempty"`     // RFC3339
	LastProbeOutcome string `json:"last_probe_outcome,omitempty"` // exhausted / uncertain / recovered
	UpdatedAt        string `json:"updated_at"`
}

// probeDue 报告本账号是否到期需要确认探针：恢复时间已知且已到 → 探；
// 恢复时间未知（或 recovery_at 损坏）→ 按 5 分钟节距兜底循环（L3/L4）。
func (st *cnQuotaLifecycleState) probeDue(now time.Time, interval time.Duration) bool {
	if st == nil {
		return false
	}
	if st.RecoveryAt != "" {
		if t, err := time.Parse(time.RFC3339, st.RecoveryAt); err == nil {
			return !t.After(now)
		}
		// recovery_at 损坏按未知处理，落入 5 分钟兜底循环。
	}
	if st.LastProbeAt == "" {
		return true
	}
	if t, err := time.Parse(time.RFC3339, st.LastProbeAt); err == nil {
		return now.Sub(t) >= interval
	}
	return true
}

// CNQuotaLifecycleService 额度耗尽状态机（TH/Kira 两条链共用，仅恢复时间来源不同）。
type CNQuotaLifecycleService struct {
	accountRepo  AccountRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config
	alerts       QuotaAlertStore
	// snapshotRefresher 可选：恢复成功后刷新用量快照。
	snapshotRefresher QuotaSnapshotRefresher

	// nowFunc 可注入时钟（确定性测试用），默认 time.Now。
	nowFunc func() time.Time
	// probeOverride 替换真实确认探针（测试用）。nil 时走真实上游探测。
	probeOverride func(ctx context.Context, account *Account) (quotaProbeOutcome, error)

	mu      sync.Mutex
	stopCh  chan struct{}
	stopOnce sync.Once
	wg      sync.WaitGroup
	// tracked 不确定路径（未停调账号）的 5 分钟循环候选集（进程内）；已停调账号
	// 由 ListTempUnschedulableAccounts + extra 状态键做跨重启的持久候选发现。
	tracked map[int64]struct{}
}

// NewCNQuotaLifecycleService 构造额度耗尽状态机服务。
func NewCNQuotaLifecycleService(
	accountRepo AccountRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	alerts QuotaAlertStore,
) *CNQuotaLifecycleService {
	return &CNQuotaLifecycleService{
		accountRepo:  accountRepo,
		httpUpstream: httpUpstream,
		cfg:          cfg,
		alerts:       alerts,
		nowFunc:      time.Now,
		stopCh:       make(chan struct{}),
		tracked:      make(map[int64]struct{}),
	}
}

// now 当前时钟（可注入，测试确定性）。
func (s *CNQuotaLifecycleService) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// SetQuotaSnapshotRefresher 注入恢复后用量快照刷新窄面（可选）。未注入时不刷新。
func (s *CNQuotaLifecycleService) SetQuotaSnapshotRefresher(r QuotaSnapshotRefresher) {
	if s != nil {
		s.snapshotRefresher = r
	}
}

// SetQuotaLifecycleClock 注入确定性时钟（测试用）。
func (s *CNQuotaLifecycleService) SetQuotaLifecycleClock(nowFunc func() time.Time) {
	if s != nil && nowFunc != nil {
		s.nowFunc = nowFunc
	}
}

// SetQuotaProbeOverride 注入确认探针覆盖（测试用）。
func (s *CNQuotaLifecycleService) SetQuotaProbeOverride(fn func(ctx context.Context, account *Account) (quotaProbeOutcome, error)) {
	if s != nil {
		s.probeOverride = fn
	}
}

// Start 启动 5 分钟恢复确认循环（复用仓库既有 ticker 后台任务模式，参见
// account_expiry_service.go）。调度只按固定节距触发 RunRecoverySweep，是否
// 探针由每账号的恢复时间/上次探针时间判定，启动不立即探测。
func (s *CNQuotaLifecycleService) Start() {
	if s == nil || s.accountRepo == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(quotaLifecycleSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.sweepOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

// Stop 停止后台循环。
func (s *CNQuotaLifecycleService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

func (s *CNQuotaLifecycleService) sweepOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), quotaLifecycleSweepInterval)
	defer cancel()
	if err := s.RunRecoverySweep(ctx); err != nil {
		// 单账号错误已在 sweep 内聚合告警日志，这里不阻断循环。
		fmt.Println("[CNQuotaLifecycle] recovery sweep failed:", err)
	}
}

// --- 响应式入口（D-QL-002 接线） ---

// OnUpstreamQuotaExhausted 处理上游 402/429 额度耗尽信号：先做一次确认探针，
// 属实则停调至官方恢复时间并 fire 告警；探测不确定则失败关闭（不动现状态）、
// fire 告警并进入 5 分钟确认循环；探针成功视为瞬时信号，保持现状。
// 非 TH/Kira 账号不在本状态机管辖内，直接返回 nil。
func (s *CNQuotaLifecycleService) OnUpstreamQuotaExhausted(ctx context.Context, account *Account, upstreamMsg string) error {
	if s == nil || s.accountRepo == nil {
		return errors.New("cn quota lifecycle service is not configured")
	}
	if account == nil {
		return errors.New("cn quota lifecycle requires an account")
	}
	if cnQuotaLifecycleProviderOf(account) == "" {
		return nil
	}
	now := s.now()
	outcome, perr := s.runConfirmationProbe(ctx, account)
	switch outcome {
	case quotaProbeRecovered:
		// 确认探针成功：瞬时信号（上游当前可用），不停调不告警，保持现状。
		fmt.Printf("[CNQuotaLifecycle] account=%d quota-exhausted signal not confirmed (probe ok), no state change\n", account.ID)
		return nil
	case quotaProbeExhausted:
		return s.confirmExhausted(ctx, account, upstreamMsg, now)
	default:
		return s.enterUncertain(ctx, account, upstreamMsg, now, perr)
	}
}

// confirmExhausted 确认耗尽：停调至官方恢复时间（L4 优先级取值；无官方值登记
// 未知→远期占位）+ 持久化状态 + fire 告警。停调失败返回错误（信号入口据此感知）。
func (s *CNQuotaLifecycleService) confirmExhausted(ctx context.Context, account *Account, upstreamMsg string, now time.Time) error {
	recoveryAt, source, known := s.resolveRecoveryTime(account, upstreamMsg, now)
	until := recoveryAt
	if !known || !until.After(now) {
		// 官方恢复时间缺失或已过期（上游数据与实际不符）：登记未知，远期占位 +
		// 5 分钟确认循环推进（循环成功即清除，占位不是恢复依据）。
		until = now.Add(quotaLifecycleUnknownRecoveryPlaceholder)
		source = cnQuotaRecoverySourceUnknown
		known = false
	}
	reason := cnQuotaExhaustedReason(upstreamMsg, known, recoveryAt, source)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		return fmt.Errorf("cn quota lifecycle park account %d: %w", account.ID, err)
	}
	s.persistLifecycleState(ctx, account.ID, &cnQuotaLifecycleState{
		State:            cnQuotaLifecycleStateExhausted,
		RecoveryAt:       cnQuotaRFC3339OrEmpty(recoveryAt, known),
		RecoverySource:   source,
		LastProbeAt:      now.UTC().Format(time.RFC3339),
		LastProbeOutcome: cnQuotaLifecycleProbeExhausted,
		UpdatedAt:        now.UTC().Format(time.RFC3339),
	})
	// 存量收敛（D-QLM-007 §2）：把既有 recovery_at（可能按旧 renewsAt 口径写入）
	// 收敛到最新 reset_at；幂等，正常路径下 recovery_at 已为 reset_at 时 no-op。
	s.convergeTHStockRecovery(ctx, account)
	// 已知恢复时间时持久候选发现（ListTempUnschedulableAccounts + extra 状态键）
	// 覆盖本账号，无需进程内跟踪；未知恢复时间同样停调、同样持久可见。
	s.untrack(account.ID)
	s.ensureQuotaAlertFiring(ctx, account, upstreamMsg, reason)
	fmt.Printf("[CNQuotaLifecycle] account=%d quota exhausted confirmed, parked until=%s source=%s\n",
		account.ID, until.UTC().Format(time.RFC3339), source)
	return nil
}

// enterUncertain 探测不确定（网络/5xx/鉴权/解析失败）：失败关闭——不动现状态
// （绝不把探测失败当已恢复，也绝不额外停调扩大影响面），fire/保持告警，进入
// 5 分钟确认循环。未停调账号由进程内候选集跟踪，已停调账号由持久候选发现兜底。
func (s *CNQuotaLifecycleService) enterUncertain(ctx context.Context, account *Account, upstreamMsg string, now time.Time, probeErr error) error {
	s.persistLifecycleState(ctx, account.ID, &cnQuotaLifecycleState{
		State:            cnQuotaLifecycleStateUncertain,
		RecoverySource:   cnQuotaRecoverySourceUnknown,
		LastProbeAt:      now.UTC().Format(time.RFC3339),
		LastProbeOutcome: cnQuotaLifecycleProbeUncertain,
		UpdatedAt:        now.UTC().Format(time.RFC3339),
	})
	s.track(account.ID)
	s.ensureQuotaAlertFiring(ctx, account, upstreamMsg, cnQuotaUncertainReason(upstreamMsg, probeErr))
	fmt.Printf("[CNQuotaLifecycle] account=%d probe uncertain (fail-closed, state unchanged), err=%v\n", account.ID, probeErr)
	return nil
}

// --- 恢复 sweep ---

// RunRecoverySweep 恢复确认循环单轮：对到期（恢复时间已到，或恢复时间未知且距
// 上次探针 ≥5 分钟）的受管账号做一次确认探针。成功=清停调+resolve 告警+刷新
// 快照；仍耗尽=保持停调（到期已过则按 L4 重取恢复时间续停）；不确定=失败关闭
// 保持停调。告警在非恢复分支保持 firing。
// 返回聚合错误（首个），供后台循环 Warn；单账号失败不阻断其余账号。
func (s *CNQuotaLifecycleService) RunRecoverySweep(ctx context.Context) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	now := s.now()
	candidates, err := s.sweepCandidates(ctx, now)
	if err != nil {
		return err
	}
	var firstErr error
	for _, account := range candidates {
		if account == nil || ctx.Err() != nil {
			continue
		}
		if cnQuotaLifecycleProviderOf(account) == "" {
			continue
		}
		state, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
		if !ok || state.State == cnQuotaLifecycleStateRecovered {
			// 已恢复/无状态记录：退出循环跟踪（恢复路径负责清停调，这里只兜孤儿跟踪项）。
			s.untrack(account.ID)
			continue
		}
		if !state.probeDue(now, quotaLifecycleSweepInterval) {
			continue
		}
		if err := s.sweepProbeAccount(ctx, account, state, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// sweepCandidates 汇合两类候选：停调中且带 extra 状态键的持久候选（跨重启）+
// 进程内跟踪的不确定候选（未停调账号）。统一经 GetByID 取最新账号快照
// （代理/凭据可能已变更）。
func (s *CNQuotaLifecycleService) sweepCandidates(ctx context.Context, now time.Time) ([]*Account, error) {
	seen := make(map[int64]struct{})
	var out []*Account
	parked, err := s.accountRepo.ListTempUnschedulableAccounts(ctx, now, 200)
	if err != nil {
		return nil, fmt.Errorf("cn quota lifecycle list candidates: %w", err)
	}
	for _, account := range parked {
		if account == nil {
			continue
		}
		if _, ok := cnQuotaLifecycleStateFromExtra(account.Extra); !ok {
			continue
		}
		seen[account.ID] = struct{}{}
		out = append(out, account)
	}
	for _, id := range s.trackedIDs() {
		if _, ok := seen[id]; ok {
			continue
		}
		account, err := s.accountRepo.GetByID(ctx, id)
		if err != nil || account == nil {
			// 账号已删除/读取失败：退出跟踪（下轮不再候选）。
			s.untrack(id)
			continue
		}
		out = append(out, account)
	}
	return out, nil
}

// sweepProbeAccount 对单个到期账号执行恢复确认探针并按三态迁移。
func (s *CNQuotaLifecycleService) sweepProbeAccount(ctx context.Context, account *Account, state *cnQuotaLifecycleState, now time.Time) error {
	outcome, perr := s.runConfirmationProbe(ctx, account)
	probeAt := s.now().UTC().Format(time.RFC3339)
	switch outcome {
	case quotaProbeRecovered:
		// 恢复闭环：清停调 + resolve 告警 + 刷新快照（可选）+ 状态落墓碑。
		if err := s.accountRepo.ClearTempUnschedulable(ctx, account.ID); err != nil {
			return fmt.Errorf("cn quota lifecycle clear account %d: %w", account.ID, err)
		}
		s.resolveQuotaAlert(ctx, account.ID)
		s.persistLifecycleState(ctx, account.ID, &cnQuotaLifecycleState{
			State:            cnQuotaLifecycleStateRecovered,
			LastProbeAt:      probeAt,
			LastProbeOutcome: cnQuotaLifecycleProbeRecovered,
			UpdatedAt:        probeAt,
		})
		s.untrack(account.ID)
		if s.snapshotRefresher != nil {
			if err := s.snapshotRefresher.RefreshCNQuotaSnapshot(ctx, account); err != nil {
				// 快照刷新失败不影响恢复闭环（Warn 暴露，下轮 402 会重新进入状态机）。
				fmt.Printf("[CNQuotaLifecycle] account=%d snapshot refresh failed: %v\n", account.ID, err)
			}
		}
		fmt.Printf("[CNQuotaLifecycle] account=%d recovered, scheduling restored\n", account.ID)
		return nil
	case quotaProbeExhausted:
		// 仍耗尽：保持停调 + 告警保持 firing。停调到期已过（官方恢复时间失准）
		// 时按 L4 重取恢复时间续停；无官方值则远期占位续停，等下一轮循环确认。
		recoveryAt, source, known := s.resolveRecoveryTime(account, "", now)
		if !known || !recoveryAt.After(now) {
			recoveryAt = now.Add(quotaLifecycleUnknownRecoveryPlaceholder)
			source = cnQuotaRecoverySourceUnknown
			known = false
		}
		until := state.parkedUntil()
		if until == nil || until.Before(now) || recoveryAt.After(*until) {
			if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, recoveryAt,
				cnQuotaExhaustedReason("", known, recoveryAt, source)); err != nil {
				return fmt.Errorf("cn quota lifecycle re-park account %d: %w", account.ID, err)
			}
		}
		s.persistLifecycleState(ctx, account.ID, &cnQuotaLifecycleState{
			State:            cnQuotaLifecycleStateExhausted,
			RecoveryAt:       cnQuotaRFC3339OrEmpty(recoveryAt, known),
			RecoverySource:   source,
			LastProbeAt:      probeAt,
			LastProbeOutcome: cnQuotaLifecycleProbeExhausted,
			UpdatedAt:        probeAt,
		})
		s.ensureQuotaAlertFiring(ctx, account, "", cnQuotaExhaustedReason("", known, recoveryAt, source))
		fmt.Printf("[CNQuotaLifecycle] account=%d still exhausted after recovery deadline, re-parked until=%s\n",
			account.ID, recoveryAt.UTC().Format(time.RFC3339))
		return nil
	default:
		// 不确定：失败关闭——保持停调（绝不因探测失败恢复调度），告警保持 firing，
		// 5 分钟后再次探针。
		s.persistLifecycleState(ctx, account.ID, &cnQuotaLifecycleState{
			State:            state.State,
			RecoveryAt:       state.RecoveryAt,
			RecoverySource:   state.RecoverySource,
			LastProbeAt:      probeAt,
			LastProbeOutcome: cnQuotaLifecycleProbeUncertain,
			UpdatedAt:        probeAt,
		})
		s.ensureQuotaAlertFiring(ctx, account, "", cnQuotaUncertainReason("", perr))
		fmt.Printf("[CNQuotaLifecycle] account=%d recovery probe uncertain (fail-closed, keep parked), err=%v\n", account.ID, perr)
		return nil
	}
}

// parkedUntil 从持久化恢复时间近似还原当前停调到期点（用于「仍耗尽」续停的
// 只延长判定）；恢复时间未知返回 nil（视为已过期，允许续停）。
func (st *cnQuotaLifecycleState) parkedUntil() *time.Time {
	if st == nil || st.RecoveryAt == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, st.RecoveryAt); err == nil {
		return &t
	}
	return nil
}

// --- 确认探针（§4.4） ---

// runConfirmationProbe 派发单次确认探针：优先用注入覆盖（测试），否则按账号
// 上游类型分派（Kira=GET /api/user/usage 拉当日窗口，不烧 VND；TH=raw 模型
// max_tokens=1 ping）。
func (s *CNQuotaLifecycleService) runConfirmationProbe(ctx context.Context, account *Account) (quotaProbeOutcome, error) {
	if s == nil {
		return quotaProbeUncertain, errors.New("cn quota lifecycle service is nil")
	}
	if s.probeOverride != nil {
		return s.probeOverride(ctx, account)
	}
	switch cnQuotaLifecycleProviderOf(account) {
	case cnQuotaLifecycleProviderKira:
		return s.probeKiraExhaustion(ctx, account)
	case cnQuotaLifecycleProviderTokenHarbor:
		return s.probeTokenHarborExhaustion(ctx, account)
	default:
		return quotaProbeUncertain, errors.New("unsupported provider for quota confirmation probe")
	}
}

// probeKiraExhaustion Kira 确认探针（§4.4）：GET /api/user/usage 拉当日窗口，
// 免费池余量>0 即恢复，不向上游发推理请求（避免烧 VND）。非 2xx/解析失败/
// 分母缺失一律不确定（失败关闭）。
func (s *CNQuotaLifecycleService) probeKiraExhaustion(ctx context.Context, account *Account) (quotaProbeOutcome, error) {
	if s.httpUpstream == nil || s.cfg == nil {
		return quotaProbeUncertain, errors.New("probe transport not configured")
	}
	usageURL, loginURL, err := kiraProbeURLs(s.cfg, account)
	if err != nil {
		return quotaProbeUncertain, fmt.Errorf("validate kira probe urls: %w", err)
	}
	proxyURL := s.resolveAccountProxyURL(account)
	client := &kiraProbeClient{
		upstream:    s.httpUpstream,
		proxyURL:    proxyURL,
		accountID:   account.ID,
		concurrency: maxInt(account.Concurrency, 1),
	}
	callCtx, cancel := context.WithTimeout(ctx, cnQuotaUpstreamTimeout*2)
	defer cancel()
	body, status, err := fetchKiraUsageWithReauth(callCtx, client, account, s.accountRepo, usageURL, loginURL)
	if err != nil {
		return quotaProbeUncertain, err
	}
	if status < 200 || status >= 300 {
		// 鉴权失败（已尝试重登）/5xx/其他：数据不明，失败关闭。
		return quotaProbeUncertain, fmt.Errorf("kira usage probe status %d", status)
	}
	_, used, limit, ok := parseKiraUsageTier(body)
	if !ok {
		return quotaProbeUncertain, errors.New("kira usage probe: missing summary")
	}
	if limit <= 0 {
		return quotaProbeUncertain, errors.New("kira usage probe: no daily limit denominator")
	}
	if used < limit {
		return quotaProbeRecovered, nil
	}
	return quotaProbeExhausted, nil
}

// probeTokenHarborExhaustion TH 确认探针（§4.4，请求形状收编自
// probeTokenHarborModelUpstream，仅作单次确认/恢复确认用，不走 60s 扫描）：
// 对账号 raw 模型发 max_tokens=1 ping。2xx=恢复；402/免费档 429=确认耗尽；
// 鉴权失败/5xx/传输失败/其他一律不确定（失败关闭，绝不误恢复）。
func (s *CNQuotaLifecycleService) probeTokenHarborExhaustion(ctx context.Context, account *Account) (quotaProbeOutcome, error) {
	if s.httpUpstream == nil || s.cfg == nil {
		return quotaProbeUncertain, errors.New("probe transport not configured")
	}
	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return quotaProbeUncertain, errors.New("no api key for quota confirmation probe")
	}
	modelKey := cnQuotaLifecycleProbeModel(account)
	if modelKey == "" {
		return quotaProbeUncertain, errors.New("no probe model (model_mapping/主模型缺失)")
	}
	baseURL := account.GetOpenAIFormatBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.openai.com"
	}
	normalized, err := cnValidateProbeURL(s.cfg, baseURL)
	if err != nil {
		return quotaProbeUncertain, fmt.Errorf("validate base url: %w", err)
	}
	body, err := json.Marshal(map[string]any{
		"model":      modelKey,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		return quotaProbeUncertain, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(normalized, "/")+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return quotaProbeUncertain, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := s.resolveAccountProxyURL(account)
	// TLS 指纹解析属探测链可选增强，本状态机固定 nil profile（行为等同 Do）。
	var tlsProfile *tlsfingerprint.Profile
	callCtx, cancel := context.WithTimeout(ctx, probeRequestHardTimeout)
	defer cancel()
	resp, err := s.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		return quotaProbeUncertain, fmt.Errorf("transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return quotaProbeRecovered, nil
	}
	if resp.StatusCode == http.StatusPaymentRequired {
		// Pass 周期额度硬上限（spendAfterAllowance=false），402 即确认耗尽。
		return quotaProbeExhausted, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests && isTokenHarborFreeTierExhausted(raw) {
		return quotaProbeExhausted, nil
	}
	return quotaProbeUncertain, fmt.Errorf("th probe status %d", resp.StatusCode)
}

// cnQuotaLifecycleProbeModel 解析 TH 确认探针模型名（§4.4：model_mapping 反解
// 或账号主模型）。model_mapping 存在时取一个 raw 目标模型（键序确定性）；无
// 映射时回退 credentials.model；都没有返回空（调用方按不确定失败关闭）。
func cnQuotaLifecycleProbeModel(account *Account) string {
	if account == nil {
		return ""
	}
	if mapping := account.GetModelMapping(); len(mapping) > 0 {
		keys := make([]string, 0, len(mapping))
		for k := range mapping {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if raw := strings.TrimSpace(mapping[keys[0]]); raw != "" {
			return raw
		}
	}
	return strings.TrimSpace(account.GetCredential("model"))
}

// resolveAccountProxyURL 解析账号出站代理（与探测链同口径：绑定代理时经代理出站）。
func (s *CNQuotaLifecycleService) resolveAccountProxyURL(account *Account) string {
	if account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

// --- 恢复时间解析（L4） ---

// resolveRecoveryTime 按优先级解析官方恢复时间（L4：上游响应内重置时间 > 面板
// 快照（TH free-tier reset_at / Kira 每日重置时刻）> 兜底周期确认探针）。返回
// (时间, 来源, 是否有官方值)；无官方值时 known=false，由调用方登记未知并走
// 5 分钟确认循环。注意：TH 的 renews_at 是订阅续期日，与额度周期无关，不采用。
func (s *CNQuotaLifecycleService) resolveRecoveryTime(account *Account, upstreamMsg string, now time.Time) (time.Time, string, bool) {
	// 1) 上游响应内重置时间（最高优先级）。
	if t := cnQuotaUpstreamMsgResetTime(upstreamMsg, now); t != nil {
		return *t, cnQuotaRecoverySourceUpstream, true
	}
	// 2) 面板快照。
	if accountIsKiraBaseURL(account) {
		// Kira 免费池每日重置（站点按越南时区），上游无响应字段，由当日窗口推导。
		return kiraNextDailyReset(now), cnQuotaRecoverySourceSnapshot, true
	}
	// TH 真实额度周期 = free-tier reset_at（7 天窗口，重置时刻在 /api/me/free-tier
	// 的 reset_at，见 TokenHarborPassSnapshot.ResetAt）；renews_at 是订阅续期日
	//（≈28 天，与额度无关）不参与恢复判定。reset_at 在未来时返回它；过期或缺失
	// → 落 unknown（免费链 7 天滚动无精确时刻，靠 5 分钟确认循环兜底）。
	if snap, ok := TokenHarborPassSnapshotFromExtra(account); ok && snap.ResetAt != nil && snap.ResetAt.After(now) {
		return *snap.ResetAt, cnQuotaRecoverySourceSnapshot, true
	}
	// 3) 兜底：恢复时间未知，5 分钟确认循环推进。
	return time.Time{}, cnQuotaRecoverySourceUnknown, false
}

// --- TH 免费档快照读取（D-QLM-010 平替 D-QLM-007 的 raw-key 帮助函数）---
//
// TH 恢复/收敛判定所需的 reset_at / plan_exhausted 直接经
// TokenHarborPassSnapshotFromExtra 反序列化为 TokenHarborPassSnapshot 后读取
// struct 字段（D-QLM-006 冻结字段）。反序列化失败按既有 unknown 路径失败关闭，
// 不新增兜底分支。

// convergeTHStockRecovery 存量收敛（D-QLM-007 §2）：TH 账号周期快照刷新观察到
// plan_exhausted=true 且 reset_at 有效时，把额度状态机里的 recovery_at 收敛到
// 最新 reset_at，覆盖历史上按 renewsAt 写入的旧值（含线上存量）。仅在已有
// lifecycle 状态（recovery_at 已知）时改写；无状态/未知则不编造。reset_at 过期
// 或缺失且仍 plan_exhausted → 不收敛（交由 L3/L4 的 unknown 兜底循环处理）。
// 调用方（快照刷新链 / 响应式确认路径）负责在刷新后触发；本方法幂等且只改
// cn_quota_lifecycle 窄面。
func (s *CNQuotaLifecycleService) convergeTHStockRecovery(ctx context.Context, account *Account) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}
	if cnQuotaLifecycleProviderOf(account) != cnQuotaLifecycleProviderTokenHarbor {
		return
	}
	// 仅在免费档确已耗尽且缓存了有效重置时刻时收敛。
	snap, snapOK := TokenHarborPassSnapshotFromExtra(account)
	if !snapOK || !snap.PlanExhausted {
		return
	}
	if snap.ResetAt == nil || !snap.ResetAt.After(s.now()) {
		return // 过期/缺失 → 不收敛（unknown 兜底循环负责）
	}
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	if !ok || st.RecoveryAt == "" {
		return // 无既有 recovery_at 不编造（首次耗尽由确认路径写入）
	}
	// 上游响应内重置时间（L4 最高优先级）已登记的 recovery_at 不回退到 snapshot
	// 口径：上游 > reset_at，避免覆盖更权威的官方值。
	if st.RecoverySource == cnQuotaRecoverySourceUpstream {
		return
	}
	// 已有 recovery_at：与最新 reset_at 相同则无需改写。
	if cur, err := time.Parse(time.RFC3339, st.RecoveryAt); err == nil && cur.Equal(*snap.ResetAt) {
		return
	}
	st.RecoveryAt = cnQuotaRFC3339OrEmpty(*snap.ResetAt, true)
	st.RecoverySource = cnQuotaRecoverySourceSnapshot
	st.UpdatedAt = s.now().UTC().Format(time.RFC3339)
	s.persistLifecycleState(ctx, account.ID, st)
}

// cnQuotaUpstreamMsgResetTime 从上游响应文案中提取未来的 RFC3339 重置时间；
// 无匹配或时间已过返回 nil（不编时间）。
var cnQuotaUpstreamMsgResetTimeRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})`)

func cnQuotaUpstreamMsgResetTime(upstreamMsg string, now time.Time) *time.Time {
	msg := strings.TrimSpace(upstreamMsg)
	if msg == "" {
		return nil
	}
	for _, m := range cnQuotaUpstreamMsgResetTimeRe.FindAllString(msg, 8) {
		t, err := time.Parse(time.RFC3339, m)
		if err != nil {
			continue
		}
		if t.After(now) {
			return &t
		}
	}
	return nil
}

// kiraQuotaVNTimeZone Kira 站点时区（越南 UTC+7，固定偏移免 IANA 依赖）。
func kiraQuotaVNTimeZone() *time.Location {
	return time.FixedZone("ICT", 7*3600)
}

// kiraNextDailyReset 由当日窗口推导 Kira 免费池下次每日重置时刻（越南时区
// 零点；站点无响应字段，这是面板快照档的口径）。
func kiraNextDailyReset(now time.Time) time.Time {
	tz := kiraQuotaVNTimeZone()
	local := now.In(tz)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, tz)
	if !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}

// --- 告警（只走 OpsAlertEvent，不新增通知管道） ---

// quotaAlertDims 额度耗尽告警维度（账号级，kind=quota_exhausted）。维度键复用
// 新鲜度告警的 kind/account_id 常量（同载体 OpsAlertEvent.Dimensions）。
func quotaAlertDims(accountID int64) map[string]any {
	return map[string]any{freshnessDimKind: quotaExhaustedAlertKind, freshnessDimAccountID: accountID}
}

// ensureQuotaAlertFiring 幂等 fire：同维度无 firing 告警时创建。告警失败仅
// 记录不阻断状态迁移（停调/恢复不依赖告警落盘）。
func (s *CNQuotaLifecycleService) ensureQuotaAlertFiring(ctx context.Context, account *Account, upstreamMsg, desc string) {
	if s == nil || s.alerts == nil || account == nil {
		return
	}
	dims := quotaAlertDims(account.ID)
	active, err := s.alerts.GetActiveQuotaAlert(ctx, dims)
	if err != nil {
		fmt.Printf("[CNQuotaLifecycle] account=%d alert lookup failed: %v\n", account.ID, err)
		return
	}
	if active != nil {
		return
	}
	if msg := strings.TrimSpace(upstreamMsg); msg != "" && desc != "" {
		desc = desc + "；上游：" + truncate(msg, 240)
	}
	if _, err := s.alerts.CreateAlertEvent(ctx, &OpsAlertEvent{
		Status:      OpsAlertStatusFiring,
		Severity:    quotaAlertSeverity,
		Title:       fmt.Sprintf("账号 %d 上游额度耗尽", account.ID),
		Description: desc,
		Dimensions:  dims,
		FiredAt:     s.now(),
	}); err != nil {
		fmt.Printf("[CNQuotaLifecycle] account=%d alert fire failed: %v\n", account.ID, err)
	}
}

// resolveQuotaAlert 恢复路径幂等关闭（未门禁窄面，无活动告警 no-op）。
func (s *CNQuotaLifecycleService) resolveQuotaAlert(ctx context.Context, accountID int64) {
	if s == nil || s.alerts == nil {
		return
	}
	if err := s.alerts.ResolveQuotaAlertOnRecovery(ctx, quotaAlertDims(accountID)); err != nil {
		fmt.Printf("[CNQuotaLifecycle] account=%d alert resolve failed: %v\n", accountID, err)
	}
}

// --- 状态持久化（extra cn_quota_lifecycle） ---

func (s *CNQuotaLifecycleService) persistLifecycleState(ctx context.Context, accountID int64, st *cnQuotaLifecycleState) {
	if s == nil || s.accountRepo == nil || st == nil {
		return
	}
	if st.UpdatedAt == "" {
		st.UpdatedAt = s.now().UTC().Format(time.RFC3339)
	}
	updates := map[string]any{cnQuotaLifecycleExtraKey: map[string]any{
		"state":              st.State,
		"recovery_at":        st.RecoveryAt,
		"recovery_source":    st.RecoverySource,
		"last_probe_at":      st.LastProbeAt,
		"last_probe_outcome": st.LastProbeOutcome,
		"updated_at":         st.UpdatedAt,
	}}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, updates); err != nil {
		// 状态记录失败不阻断状态迁移主路径（停调/清除已落库）；持久候选发现对
		// 已停调账号会因缺状态键跳过，下一轮 402 重进状态机自愈。
		fmt.Printf("[CNQuotaLifecycle] account=%d persist lifecycle state failed: %v\n", accountID, err)
	}
}

// cnQuotaLifecycleStateFromExtra 解析 extra 状态记录（兼容 JSON 反序列化后的
// map[string]any 与内存写入形态）。
func cnQuotaLifecycleStateFromExtra(extra map[string]any) (*cnQuotaLifecycleState, bool) {
	raw, ok := extra[cnQuotaLifecycleExtraKey]
	if !ok || raw == nil {
		return nil, false
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var st cnQuotaLifecycleState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, false
	}
	if st.State == "" {
		return nil, false
	}
	return &st, true
}

// --- 候选跟踪（不确定路径的进程内 5 分钟循环） ---

func (s *CNQuotaLifecycleService) track(accountID int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tracked == nil {
		s.tracked = make(map[int64]struct{})
	}
	s.tracked[accountID] = struct{}{}
}

func (s *CNQuotaLifecycleService) untrack(accountID int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tracked, accountID)
}

func (s *CNQuotaLifecycleService) trackedIDs() []int64 {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.tracked))
	for id := range s.tracked {
		ids = append(ids, id)
	}
	return ids
}

// --- 账号分类 / 文案 ---

// cnQuotaLifecycleProviderOf 判定账号归属的上游链（Kira 先于 TH；两者都不是
// 返回空 = 不在状态机管辖内）。判定口径与 CN 探测链一致：base_url 是唯一事实源。
func cnQuotaLifecycleProviderOf(account *Account) string {
	if account == nil {
		return ""
	}
	if accountIsKiraBaseURL(account) {
		return cnQuotaLifecycleProviderKira
	}
	if isTokenHarborBaseURL(account.GetOpenAIBaseURL()) || isTokenHarborBaseURL(account.GetBaseURL()) {
		return cnQuotaLifecycleProviderTokenHarbor
	}
	return ""
}

// isTokenHarborBaseURL 报告 base_url 是否指向 tokenharbor.ai 上游（大小写不敏感
// 包含判定，与 isKiraBaseURL 同构）。
func isTokenHarborBaseURL(baseURL string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(baseURL)), "tokenharbor.ai")
}

// cnQuotaExhaustedReason 构造确认耗尽停调 reason（稳定前缀 + 恢复时间来源）。
func cnQuotaExhaustedReason(upstreamMsg string, known bool, recoveryAt time.Time, source string) string {
	var b strings.Builder
	b.WriteString(cnQuotaExhaustedReasonPrefix)
	if msg := strings.TrimSpace(upstreamMsg); msg != "" {
		b.WriteString(": ")
		b.WriteString(truncate(msg, 200))
	}
	if known {
		b.WriteString("；停调至官方恢复时间 " + recoveryAt.UTC().Format(time.RFC3339) + "（来源 " + source + "）")
	} else {
		b.WriteString("；官方恢复时间未知，5 分钟确认循环推进")
	}
	return b.String()
}

// cnQuotaUncertainReason 构造探测不确定（失败关闭）的告警描述。
func cnQuotaUncertainReason(upstreamMsg string, probeErr error) string {
	msg := strings.TrimSpace(upstreamMsg)
	if msg == "" {
		return "额度耗尽确认探针结果不确定（失败关闭：状态不变，5 分钟后再次确认）"
	}
	detail := msg
	if probeErr != nil {
		detail = msg + "；探针错误：" + probeErr.Error()
	}
	return "额度耗尽确认探针结果不确定（失败关闭：状态不变，5 分钟后再次确认）；上游：" + truncate(detail, 240)
}

// cnQuotaRFC3339OrEmpty 已知时间转 RFC3339（UTC）；未知返回空串（登记未知）。
func cnQuotaRFC3339OrEmpty(t time.Time, known bool) string {
	if !known || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
