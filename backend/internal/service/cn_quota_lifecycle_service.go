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
// 402 来源冻结（freeze_source=402_account）的到期探针是唯一例外：Kira 的确认探针是
// usage GET（只读用量查询），VND=0 时也能成功，永不产生完成级 2xx——而 402 冻结的唯一
// 解冻事件就是完成级轻请求 2xx（§0.2(1) e）。故 Kira 402 冻结到期时改发完成级
// max_tokens=1 ping（TH 链 probeTokenHarborExhaustion 既有形状，无新机制），否则该
// 冻结永不自动恢复，违反锁定项 5。维度来源冻结仍走 usage GET 9 格判定表。
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
	"log/slog"
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
	cnQuotaLifecycleStateExhausted = "exhausted"
	cnQuotaLifecycleStateUncertain = "uncertain"
	cnQuotaLifecycleStateRecovered = "recovered"
	cnQuotaLifecycleProbeExhausted = "exhausted"
	cnQuotaLifecycleProbeUncertain = "uncertain"
	cnQuotaLifecycleProbeRecovered = "recovered"

	cnQuotaLifecycleProviderKira        = "kira"
	cnQuotaLifecycleProviderTokenHarbor = "tokenharbor"

	// cnQuotaExhaustedReason402Account 是 402 权威冻结 reason 的标记后缀：标识该
	// 停调由账号级 402 权威冻结产生（方案 §3.2 / §3.3 R19-F4）。该标记用于：
	//   - 恢复门控：余额观察 fresh>0 不得直接恢复 402 权威冻结（防振荡，R17-F1/R18-F1）；
	//   - F4 受控缩短：仅 lifecycle 域内（本标记前缀）拥有的 until 允许被缩短。
	//   - 来源交叉（用户 2026-10-10 §0.2(1)）：确认探针结论落地前据此判定冻结来源。
	cnQuotaExhaustedReason402Account = "402-account-authoritative"

	// 冻结来源标记（cn_quota_lifecycle.freeze_source；来源交叉判定的持久化事实源，
	// 重启不丢，不依赖账号级 reason 文本）。
	cnQuotaFreezeSource402Account = "402_account"
	cnQuotaFreezeSourceDimension  = "dimension"
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

// QuotaExhaustionScope 上游额度耗尽信号的结构化作用域（方案 §4 F2 / R4-F2/R5-F2/
// R6-F1/R10-F3；C1 结构化信号透传闭包）。禁止从自由报文文本猜测作用域。
type QuotaExhaustionScope string

const (
	// QuotaScopeAccount 账号级耗尽（上游以 402 拒绝 = 账号当前无可服务维度，如 Kira）。
	QuotaScopeAccount QuotaExhaustionScope = "account"
	// QuotaScopeModel 模型级耗尽（如 TH free-tier 模型级、非 Kira 平台 402）。
	QuotaScopeModel QuotaExhaustionScope = "model"
	// QuotaScopePath 路径级耗尽。
	QuotaScopePath QuotaExhaustionScope = "path"
	// QuotaScopeUnknown 作用域不清：不泛化、不做整号转换（维持维度级/响应式语义）。
	QuotaScopeUnknown QuotaExhaustionScope = "unknown"
)

// QuotaExhaustionSignal 上游额度耗尽信号的结构化载体（方案 §4 F2）。
//
// 字段：
//   - Status：上游 HTTP 状态码（402/429/502/504...），结构化可得，不得靠文本猜测。
//   - Scope：作用域（account/model/path/unknown），由 ClassifyQuotaExhaustionScope 判定。
//   - Platform：账号归属平台（同程序标识），用于三态解析来源绑定。
//   - Msg：既有自由文本 upstreamMsg，照传兼容（不用于作用域判定）。
type QuotaExhaustionSignal struct {
	Status   int
	Scope    QuotaExhaustionScope
	Platform string
	Msg      string
}

// ClassifyQuotaExhaustionScope 结构化作用域判定矩阵（方案 §4 F2）：
//   - kira + 402 = account（到达证据；Kira 实测 VND=0 时含免费池请求在内全拒）。
//   - 非 Kira 平台 402 = model/path（按已确认维度上下文，不得泛化账号冻结）。
//   - kira + 502/504 = 仅当新鲜 VND≤0 三态佐证成立（ResolveKiraVNDBalanceState）
//     才 account，否则 unknown（禁止把通用 5xx 泛化为账号冻结）。
//   - 429 / 其他 = model/path（429 不触发账号级权威冻结）；判不清 = unknown。
//
// 禁止从自由报文文本猜测作用域。
func ClassifyQuotaExhaustionScope(account *Account, status int, now time.Time) QuotaExhaustionScope {
	switch status {
	case 402:
		if accountIsKiraBaseURL(account) {
			return QuotaScopeAccount // kira+402=account（到达证据）
		}
		return QuotaScopeModel // 非 Kira 平台 402 = model/path
	case 502, 504:
		if accountIsKiraBaseURL(account) {
			// kira+502/504：仅当新鲜 VND≤0 三态佐证成立才 account，否则 unknown。
			if ResolveKiraVNDBalanceState(account, now).IsExhausted() {
				return QuotaScopeAccount
			}
			return QuotaScopeUnknown
		}
		return QuotaScopeUnknown
	case 429:
		return QuotaScopeModel // 429 不触发账号级权威冻结
	default:
		return QuotaScopeUnknown
	}
}

// cnQuotaLifecycleState extra 持久化的状态机状态记录。
type cnQuotaLifecycleState struct {
	State            string `json:"state"`
	RecoveryAt       string `json:"recovery_at,omitempty"`        // 空 = 恢复时间未知
	RecoverySource   string `json:"recovery_source,omitempty"`    // upstream / snapshot / unknown
	ProbeDueAt       string `json:"probe_due_at,omitempty"`       // 恢复探针资格到期（§3.3 R19-F4 迁移表；持久化，重启不丢）
	LastProbeAt      string `json:"last_probe_at,omitempty"`      // RFC3339
	LastProbeOutcome string `json:"last_probe_outcome,omitempty"` // exhausted / uncertain / recovered
	UpdatedAt        string `json:"updated_at"`
	// FreezeSource 冻结来源（402_account / dimension）：来源交叉判定的持久化事实源，
	// 重启不丢（用户 2026-10-10 §0.2(1) 单一状态迁移表）。缺值（存量记录）时回退
	// 到账号级 temp_unschedulable_reason 文本判定。
	FreezeSource string `json:"freeze_source,omitempty"`
	// FreezeGeneration 冻结代际序号（单调递增 int64）：每次 402 权威冻结写入开启新
	// 代际（+1）；代际内 fresh>0 的 immediate due 提前至多消费一次。
	FreezeGeneration int64 `json:"freeze_generation,omitempty"`
	// ImmediateDueConsumed 本冻结代际内 fresh>0 的 immediate due 提前是否已消费
	// （边沿触发，至多一次/代际；持久化，重启/并发采集不得重复提前）。
	ImmediateDueConsumed bool `json:"immediate_due_consumed,omitempty"`
}

// probeDue 报告本账号是否到期需要确认探针：优先看持久化的 probe_due_at 资格字段
// （§3.3 R19-F4 迁移表；未知/损坏按 5 分钟兜底循环）；其次看恢复时间（已知且已到 →
// 探；未知或损坏 → 按 5 分钟节距兜底，L3/L4）。
func (st *cnQuotaLifecycleState) probeDue(now time.Time, interval time.Duration) bool {
	if st == nil {
		return false
	}
	if st.ProbeDueAt != "" {
		if t, err := time.Parse(time.RFC3339, st.ProbeDueAt); err == nil {
			return !t.After(now)
		}
		// probe_due_at 损坏按未知处理，落入 5 分钟兜底循环。
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

	// f3Repo 是 F3 403 恢复链存储原语窄面（对 accountRepo 一次性类型断言，承
	// tempUnschedulableShortener 同构）：nil 表示 repo 不支持 F3，F3 分支静默跳过
	// （能力缺失降级，非错误吞没）。全部 F3 原语经此消费，不扩大 AccountRepository 大接口。
	f3Repo http403RecoveryRepo
	// http403Counter 可选注入：2xx 恢复时清零三振计数的窄面依赖。nil 时跳过清零。
	http403Counter OpenAI403CounterCache
	// f3ProbeOverride 替换真实恢复探针（测试用）；nil 时走真实上游探测（复用
	// httpUpstream.DoWithTLS + resolveAccountProxyURL，禁止新造探针基建）。回传响应体
	// 供仍 403 分支从 body 解析 paused-until。
	f3ProbeOverride func(ctx context.Context, account *Account) (f3ProbeResponseClass, int, []byte, error)
	// completionProbeOverride 替换 402 来源冻结的到期完成级探针（测试用）；nil 时走
	// 真实上游探测（runCompletionPing，与 F3 恢复探针同一请求形状）。
	completionProbeOverride func(ctx context.Context, account *Account) (f3ProbeResponseClass, int, []byte, error)

	mu sync.Mutex
	// dueMu 串行化 402 probe_due 迁移推进（边沿触发的至多一次消费登记；与 mu 分离，
	// 避免与候选集跟踪互斥）。
	dueMu    sync.Mutex
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
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
	// F3 窄面一次性断言（承 tempUnschedulableShortener 同构）：断言失败静默降级，
	// F3 分支在 sweep 中按 s.f3Repo == nil 跳过并 Warn 一次，不阻断既有 lifecycle 链。
	f3Repo, ok := accountRepo.(http403RecoveryRepo)
	if !ok {
		f3CapabilityWarn(accountRepo, "NewCNQuotaLifecycleService")
	}
	return &CNQuotaLifecycleService{
		accountRepo:  accountRepo,
		httpUpstream: httpUpstream,
		cfg:          cfg,
		alerts:       alerts,
		nowFunc:      time.Now,
		stopCh:       make(chan struct{}),
		tracked:      make(map[int64]struct{}),
		f3Repo:       f3Repo,
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

// SetQuota403CounterCache 注入三振计数清除窄面依赖（可选）。2xx 恢复时清零三振计数；
// 未注入（nil）时跳过清零（不影响恢复闭环）。
func (s *CNQuotaLifecycleService) SetQuota403CounterCache(cache OpenAI403CounterCache) {
	if s != nil {
		s.http403Counter = cache
	}
}

// SetQuotaF3ProbeOverride 注入 F3 恢复探针覆盖（测试用）。nil 走真实上游探测。
func (s *CNQuotaLifecycleService) SetQuotaF3ProbeOverride(fn func(ctx context.Context, account *Account) (f3ProbeResponseClass, int, []byte, error)) {
	if s != nil && fn != nil {
		s.f3ProbeOverride = fn
	}
}

// SetQuotaCompletionProbeOverride 注入 402 来源冻结的到期完成级探针覆盖（测试用）。
// nil 走真实上游探测（runCompletionPing）。
func (s *CNQuotaLifecycleService) SetQuotaCompletionProbeOverride(fn func(ctx context.Context, account *Account) (f3ProbeResponseClass, int, []byte, error)) {
	if s != nil && fn != nil {
		s.completionProbeOverride = fn
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

// OnUpstreamQuotaExhausted 处理上游 402/429 额度耗尽信号（既有兼容签名，派生默认
// 作用域后转交结构化入口 OnUpstreamQuotaExhaustedScoped）。非 TH/Kira 账号不在本
// 状态机管辖内，直接返回 nil。
//
// 默认作用域派生：Kira 账号 → account（kira+402 到达证据），其余 → model/path
// （探针确认语义，既有行为）。结构化信号应由响应式入口（ratelimit_cn_providers.go）
// 经 ClassifyQuotaExhaustionScope 显式生成并通过 Scoped 入口透传。
func (s *CNQuotaLifecycleService) OnUpstreamQuotaExhausted(ctx context.Context, account *Account, upstreamMsg string) error {
	scope := QuotaScopeModel
	if accountIsKiraBaseURL(account) {
		scope = QuotaScopeAccount
	}
	signal := QuotaExhaustionSignal{Status: 0, Scope: scope, Platform: account.Platform, Msg: upstreamMsg}
	return s.OnUpstreamQuotaExhaustedScoped(ctx, account, upstreamMsg, signal)
}

// OnUpstreamQuotaExhaustedScoped 结构化作用域信号入口（方案 §4 F2 / §3.2 / §3.3
// R19-F4）：消费结构化 {status, scope, platform} 信号，按 Kira 402×三态裁决表执行。
//
// 裁决：
//   - scope=unknown → 不做整号转换（不冻结、不改账号级状态，维持维度级/响应式语义）。
//   - scope=account（如 kira+402 到达证据）→ 402 权威冻结：独立成立冻结（三态裁决表
//     三行全冻结），余额三态只管辖恢复时机（probe_due_at 迁移表）；不占用 403 链的
//     http_403_recovery 键（本卡不实现）。
//   - scope=model/path → 维持既有探针确认语义（TH 等非 Kira 平台既有行为）：仅当确认
//     探针 Exhausted 才冻结，Recovered/Uncertain 维持现状（失败关闭/瞬时信号）。
//   - 不变式（§3.1）：unknown 永不单独导致冻结或跳过。
func (s *CNQuotaLifecycleService) OnUpstreamQuotaExhaustedScoped(ctx context.Context, account *Account, upstreamMsg string, signal QuotaExhaustionSignal) error {
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
	// unknown 作用域：不做整号转换（不冻结、不改账号级状态）。
	if signal.Scope == QuotaScopeUnknown {
		fmt.Printf("[CNQuotaLifecycle] account=%d quota-exhausted signal scope=unknown, no whole-account conversion\n", account.ID)
		return nil
	}
	outcome, _, perr := s.runConfirmationProbe(ctx, account)
	if signal.Scope == QuotaScopeAccount {
		// 402 权威冻结（三态裁决表三行全冻结，独立于确认探针结果）。
		return s.confirmExhaustedAuthoritative(ctx, account, upstreamMsg, signal, now, outcome, perr)
	}
	// model/path：既有探针确认语义。
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
		// 维度来源冻结（非 402 权威冻结）：可由确认探针的 Recovered 清除
		// （§0.2(1) 来源交叉：仅 402 来源冻结不得由 Recovered 解冻）。
		FreezeSource: cnQuotaFreezeSourceDimension,
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

// confirmExhaustedAuthoritative 402 权威冻结（方案 §3.2 / §3.3 R19-F4）：账号级 402
// 独立成立冻结——三态裁决表三行（fresh≤0 / fresh>0 / unknown）全冻结，余额三态只管辖
// 恢复时机。除既有 exhausted 落库外，额外：
//  1. reason 标记账号级 402 来源（cnQuotaExhaustedReason402Account 后缀），与维度耗尽
//     冻结区分；该标记是 F4 受控缩短的归属依据，也是恢复门控的识别依据。
//  2. 持久化 probe_due_at 资格字段（重启不丢），按 §3.3 R19-F4 迁移表取值：
//     fresh>0 → 立即 due（下一轮 sweep 探测——余额事实变化提示可能充值）；
//     unknown / 仍耗尽 → 账号每日 reset_at 兜底 due（Kira 每日重置 / TH free-tier
//     重置）；该字段仅由 lifecycle 域内写，sweep 候选条件覆盖（账号已 park，天然在
//     ListTempUnschedulableAccounts 候选内，C0 ② 结论，无需扩展查询）。
//
// 不占用 403 链的 http_403_recovery 键（本卡根本不实现）。
func (s *CNQuotaLifecycleService) confirmExhaustedAuthoritative(ctx context.Context, account *Account, upstreamMsg string, signal QuotaExhaustionSignal, now time.Time, outcome quotaProbeOutcome, probeErr error) error {
	recoveryAt, source, known := s.resolveRecoveryTime(account, upstreamMsg, now)
	until := recoveryAt
	if !known || !until.After(now) {
		until = now.Add(quotaLifecycleUnknownRecoveryPlaceholder)
		source = cnQuotaRecoverySourceUnknown
		known = false
	}
	reason := cnQuotaExhaustedReason(upstreamMsg, known, recoveryAt, source) + " [" + cnQuotaExhaustedReason402Account + "]"
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		return fmt.Errorf("cn quota lifecycle park account %d: %w", account.ID, err)
	}
	// probe_due_at 迁移表（§3.3 R19-F4）+ 边沿触发（§0.2(1) 单一状态迁移表）：
	// 本次 402 权威冻结写入开启新冻结代际（freeze_generation），代际内 fresh>0 的
	// immediate due 至多消费一次——冻结即 fresh>0 时当场登记已消费（due=now），
	// 后续同值重观察不再提前；fresh≤0 → due = 账号每日 reset_at。
	//
	// probe_due / consumed / generation 三者的读-改-写统一在 dueMu 互斥域内完成，
	// 且全部基于锁内 GetByID 的最新持久账号对象（检查单 #3 c + R2-#2）：
	//   - generation 不再各自从旧 generation 算出重复值（单实例内单调递增，多实例
	//     CAS 仍是 Phase 2 残余，本卡不建存储层 CAS）；
	//   - 余额解析不再用锁外传入的旧账号快照：杜绝「余额先落库 → fresh 回调因未
	//     冻结 no-op → 冻结入口持旧余额」的交错把本应 immediate 的冻结写成每日 due。
	s.dueMu.Lock()
	defer s.dueMu.Unlock()
	latest := account
	if s.accountRepo != nil {
		if fresh, err := s.accountRepo.GetByID(ctx, account.ID); err == nil && fresh != nil {
			latest = fresh
		}
	}
	generation := int64(1)
	if prev, ok := cnQuotaLifecycleStateFromExtra(latest.Extra); ok && prev != nil && prev.FreezeGeneration > 0 {
		generation = prev.FreezeGeneration + 1
	}
	probeDueAt, immediateConsumed := s.resolveFreezeTimeProbeDue(latest, now)
	freezeState := &cnQuotaLifecycleState{
		State:                cnQuotaLifecycleStateExhausted,
		RecoveryAt:           cnQuotaRFC3339OrEmpty(recoveryAt, known),
		RecoverySource:       source,
		ProbeDueAt:           cnQuotaRFC3339OrEmpty(probeDueAt, true),
		LastProbeAt:          now.UTC().Format(time.RFC3339),
		LastProbeOutcome:     probeOutcomeName(outcome),
		UpdatedAt:            now.UTC().Format(time.RFC3339),
		FreezeSource:         cnQuotaFreezeSource402Account,
		FreezeGeneration:     generation,
		ImmediateDueConsumed: immediateConsumed,
	}
	if immediateConsumed {
		// 含 immediate 消费登记的冻结写入走 strict 持久化并上抛失败（R2-#1）：
		// UpdateExtra 失败 ⇒ 本次冻结迁移不成立（失败关闭，下轮 sweep/响应式
		// 重试），不得出现「内存 consumed=true 但落库失败」的假成功——否则重启后
		// 同一边沿可重复消费。不含消费登记的普通冻结写入保持既有吞错路径。
		if err := s.persistLifecycleStateStrict(ctx, account.ID, freezeState); err != nil {
			return fmt.Errorf("cn quota lifecycle persist 402 authoritative freeze for account %d: %w", account.ID, err)
		}
	} else {
		s.persistLifecycleState(ctx, account.ID, freezeState)
	}
	s.convergeTHStockRecovery(ctx, account)
	s.untrack(account.ID)
	s.ensureQuotaAlertFiring(ctx, account, upstreamMsg, reason)
	fmt.Printf("[CNQuotaLifecycle] account=%d 402-account-authoritative freeze, parked until=%s source=%s probe_due=%s\n",
		account.ID, until.UTC().Format(time.RFC3339), source, cnQuotaRFC3339OrEmpty(probeDueAt, true))
	return nil
}

// resolveProbeDueAt 按 §3.3 R19-F4 迁移表计算恢复探针资格到期：
//   - 余额 fresh>0 → 立即 due（now，下一轮 sweep 即探测——充值提示）；
//   - 余额 unknown / fresh≤0（仍耗尽）→ 账号每日 reset_at 兜底 due（Kira 每日重置
//     / TH free-tier 重置），避免对未充值账号无限周期探测。
//
// 纯计算，不做消费登记：边沿触发的「至多一次/冻结代际」由 resolveFreezeTimeProbeDue
// （冻结时）与 advance402ProbeDue（冻结中）在写侧登记（§0.2(1)）。
func (s *CNQuotaLifecycleService) resolveProbeDueAt(account *Account, signal QuotaExhaustionSignal, now time.Time) time.Time {
	if vnd := ResolveKiraVNDBalanceState(account, now); vnd.IsFresh() && vnd.Value > 0 {
		return now // fresh>0 → 立即 due
	}
	return s.resolveDailyResetDue(account, now)
}

// resolveDailyResetDue 迁移表的「每日 reset_at 兜底」档（无 immediate 资格时的 due）：
// Kira → 每日重置时刻；TH → 快照 reset_at；都无权威重置点 → now（本轮即探，由既有
// 5 分钟循环推进）。
func (s *CNQuotaLifecycleService) resolveDailyResetDue(account *Account, now time.Time) time.Time {
	if accountIsKiraBaseURL(account) {
		return kiraNextDailyReset(now)
	}
	if snap, ok := TokenHarborPassSnapshotFromExtra(account); ok && snap.ResetAt != nil && snap.ResetAt.After(now) {
		return *snap.ResetAt
	}
	return now
}

// resolveFreezeTimeProbeDue 冻结时刻的迁移表取值（§0.2(1) 单一状态迁移表）：
//   - fresh>0 → due = immediate（now），且当场登记已消费（返回 consumed=true）；
//   - fresh≤0 / unknown → due = 账号每日 reset_at，不消费（代际内后续观察到
//     fresh>0 仍可提前一次）。
func (s *CNQuotaLifecycleService) resolveFreezeTimeProbeDue(account *Account, now time.Time) (time.Time, bool) {
	if vnd := ResolveKiraVNDBalanceState(account, now); vnd.IsFresh() && vnd.Value > 0 {
		return now, true
	}
	return s.resolveDailyResetDue(account, now), false
}

// is402AuthoritativeFrozen 来源交叉判定（§0.2(1)）：报告该账号当前的冻结是否由
// 账号级 402 权威冻结产生。判定事实源按优先级：
//  1. 持久化的 cn_quota_lifecycle.freeze_source = 402_account（重启不丢）；
//  2. 账号级 temp_unschedulable_reason 含 [402-account-authoritative] 标记
//     （存量记录无 freeze_source 字段时的回退口径）。
//
// 仅维度来源冻结（freeze_source=dimension 或 reason 无该标记）返回 false。
func is402AuthoritativeFrozen(account *Account, st *cnQuotaLifecycleState) bool {
	if st != nil && st.FreezeSource == cnQuotaFreezeSource402Account {
		return true
	}
	if st != nil && st.FreezeSource == cnQuotaFreezeSourceDimension {
		return false
	}
	if account != nil && strings.Contains(account.TempUnschedulableReason, cnQuotaExhaustedReason402Account) {
		return true
	}
	return false
}

// advance402ProbeDue 402 权威冻结账号的 probe_due 迁移推进（§0.2(1) 单一状态迁移表，
// 检查单 #1 边沿触发）：
//   - fresh>0 且本冻结代际尚未消费过 immediate due → due = now（立即），并在
//     lifecycle 状态内持久化登记已消费证据（与 probe_due_at 同一次写入，原子）；
//   - 同值重观察 / 仅余额时间戳更新 / 进程重启（消费证据已持久化）/ 并发采集
//     （锁内重读最新状态）→ 均不得再次提前，due = 账号每日 reset_at；
//   - 新冻结代际（confirmExhaustedAuthoritative 重写 freeze_generation 并清零消费
//     标记）方可再提前一次。
//
// 本方法只推进 probe_due，**永不解冻**（解冻唯一事件 = 完成级 2xx，见
// sweepProbeAccount）。返回推进后的 due；持久化失败返回错误（见下方 invariant）。
//
// 检查单 #3 invariant：持久化失败必须显式上抛，且失败时回滚本次内存置位的消费标记
// ——不得出现「内存 consumed=true 但落库失败」的假成功（否则重启后重复 immediate，
// 违反边沿至多一次）。调用方按失败关闭处理：本次不视为已消费，下轮重试推进。
// 保证级别收窄为**单实例**（dueMu 内互斥 + 写入失败不假成功）；多实例 CAS
// （generation 并发递增丢失更新）登记为 Phase 2 残余。
func (s *CNQuotaLifecycleService) advance402ProbeDue(ctx context.Context, account *Account, st *cnQuotaLifecycleState, outcome quotaProbeOutcome, now time.Time) (time.Time, error) {
	if s == nil || account == nil {
		return time.Time{}, nil
	}
	s.dueMu.Lock()
	defer s.dueMu.Unlock()
	// 锁内重读最新持久状态（并发采集/重启一致性）：优先 repo 最新快照，其次 extra。
	if s.accountRepo != nil {
		if fresh, err := s.accountRepo.GetByID(ctx, account.ID); err == nil && fresh != nil {
			account = fresh
		}
	}
	if fresh, ok := cnQuotaLifecycleStateFromExtra(account.Extra); ok && fresh != nil {
		st = fresh
	}
	if st == nil {
		st = &cnQuotaLifecycleState{}
	}
	if st.FreezeGeneration <= 0 {
		// 存量记录（无代际序号）：以 1 建立代际，代际内仍至多一次。
		st.FreezeGeneration = 1
	}
	vnd := ResolveKiraVNDBalanceState(account, now)
	due := s.resolveDailyResetDue(account, now)
	consumedBefore := st.ImmediateDueConsumed
	if vnd.IsFresh() && vnd.Value > 0 && !st.ImmediateDueConsumed {
		due = now // 边沿：本代际唯一一次 immediate due
		st.ImmediateDueConsumed = true
	}
	// 恢复时间按权威口径重算（与 sweep 续停判定同源），保证持久化状态一致。
	recoveryAt, source, known := s.resolveRecoveryTime(account, "", now)
	st.State = cnQuotaLifecycleStateExhausted
	st.FreezeSource = cnQuotaFreezeSource402Account
	st.RecoveryAt = cnQuotaRFC3339OrEmpty(recoveryAt, known)
	st.RecoverySource = source
	st.ProbeDueAt = cnQuotaRFC3339OrEmpty(due, true)
	st.LastProbeAt = now.UTC().Format(time.RFC3339)
	st.LastProbeOutcome = probeOutcomeName(outcome)
	st.UpdatedAt = now.UTC().Format(time.RFC3339)
	if err := s.persistLifecycleStateStrict(ctx, account.ID, st); err != nil {
		// 落库失败 ⇒ 本次推进不成立：回滚内存消费标记并上抛（调用方失败关闭）。
		st.ImmediateDueConsumed = consumedBefore
		return time.Time{}, fmt.Errorf("cn quota lifecycle persist 402 probe due for account %d: %w", account.ID, err)
	}
	fmt.Printf("[CNQuotaLifecycle] account=%d 402-source freeze held (no unfreeze); probe_due=%s consumed=%v\n",
		account.ID, cnQuotaRFC3339OrEmpty(due, true), st.ImmediateDueConsumed)
	return due, nil
}

// OnKiraFreshBalanceObserved Kira 付费 VND 余额采集侧的 immediate due 边沿
// （检查单 #1 / §0.2(1) 单一状态迁移表）。周期余额链（CNProviderBalanceCheckService.
// refreshKiraAccount）把新鲜 VND 快照成功落库后调用本方法，使「充值后不必再等每日
// due」的锁定语义真正接通——推进走现有 lifecycle 权威路径 advance402ProbeDue
// （复用其代际消费判据），不新增 ticker/状态机。
//
// 条件推进的守门（不满足即零写入返回，绝不扩大影响面）：
//  1. 账号当前必须处于 402 权威冻结（is402AuthoritativeFrozen 来源交叉判定）；
//  2. 采到的 VND 必须 fresh>0（其余余额态不推进）；
//  3. 本冻结代际尚未消费过 immediate due（已消费则本代际不再提前）。
//
// 推进的意义：probe_due 被落到「下一次 sweep 即可探」（due=now），且发生在完成级
// 到期探针之前——下一次 sweep 立即把该账号纳入探测候选。返回值：推进持久化失败时
// 上抛（调用方按失败关闭记录，下轮周期链重试推进；检查单 #3）。
func (s *CNQuotaLifecycleService) OnKiraFreshBalanceObserved(ctx context.Context, account *Account) error {
	if s == nil || account == nil {
		return nil
	}
	// 用最新持久状态判定（周期链采集到的新余额只在库里，内存账号可能是旧快照）。
	latest := account
	if s.accountRepo != nil {
		if fresh, err := s.accountRepo.GetByID(ctx, account.ID); err == nil && fresh != nil {
			latest = fresh
		}
	}
	st, _ := cnQuotaLifecycleStateFromExtra(latest.Extra)
	if !is402AuthoritativeFrozen(latest, st) {
		return nil // 非 402 冻结账号：不进入 402 状态机，零写入。
	}
	now := s.now()
	vnd := ResolveKiraVNDBalanceState(latest, now)
	if !vnd.IsFresh() || vnd.Value <= 0 {
		return nil // 只有 fresh>0 才构成 immediate due 边沿。
	}
	if st != nil && st.ImmediateDueConsumed {
		return nil // 本冻结代际的 immediate 额度已消费：不再提前。
	}
	due, err := s.advance402ProbeDue(ctx, latest, st, quotaProbeUncertain, now)
	if err != nil {
		return fmt.Errorf("cn quota lifecycle advance probe due on fresh balance for account %d: %w", account.ID, err)
	}
	fmt.Printf("[CNQuotaLifecycle] account=%d fresh VND observed on balance collection; probe_due advanced to=%s\n",
		account.ID, cnQuotaRFC3339OrEmpty(due, true))
	return nil
}

// probeOutcomeName 把 quotaProbeOutcome 映射为可持久化文本。
func probeOutcomeName(outcome quotaProbeOutcome) string {
	switch outcome {
	case quotaProbeExhausted:
		return cnQuotaLifecycleProbeExhausted
	case quotaProbeRecovered:
		return cnQuotaLifecycleProbeRecovered
	default:
		return cnQuotaLifecycleProbeUncertain
	}
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
		// F3 恢复链候选：独立处理分支，不受 cnQuotaLifecycleProviderOf 过滤影响，
		// 不与 lifecycle 状态机混用（F3 键与 cnQuota_lifecycle 状态键互斥消费）。
		if rec, ok := http403RecoveryFromExtra(account.Extra); ok {
			if s.f3Repo != nil {
				if err := s.sweepF3RecoveryAccount(ctx, account, rec, now); err != nil && firstErr == nil {
					firstErr = err
				}
			}
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
		if !state.probeDue(now, quotaLifecycleSweepInterval) && !s.sweepTHResidualRecoveryDue(account, state, now) {
			continue
		}
		if err := s.sweepProbeAccount(ctx, account, state, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// 存量初始化（R8-F1）：一次性、幂等的存量 403 error 解析收编（每轮都跑该查询，
	// 查不到即空转；候选查询排除已持有 http_403_recovery 键的账号，天然幂等）。
	if s.f3Repo != nil {
		if err := s.runLegacyHTTP403Init(ctx, now); err != nil && firstErr == nil {
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
	// 第三类候选：F3 恢复链（持有 http_403_recovery 且 until<=now）。不受 schedulable
	// / HasError / 快照新鲜度过滤影响（D4 证据基线：冻结/error 账号会被既有链跳过）。
	// F3 键与 cnQuota_lifecycle 状态键互斥消费，故不加 lifecycle 状态过滤。
	if s.f3Repo != nil {
		due, derr := s.f3Repo.ListHTTP403RecoveryDueAccounts(ctx, now, 200)
		if derr != nil {
			// 候选查询失败不阻断既有 lifecycle 候选（Warn 暴露，下轮重试）。
			fmt.Printf("[CNQuotaLifecycle] F3 recovery due list failed: %v\n", derr)
		} else {
			for _, account := range due {
				if account == nil {
					continue
				}
				if _, ok := seen[account.ID]; ok {
					continue
				}
				seen[account.ID] = struct{}{}
				out = append(out, account)
			}
		}
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
//
// 分流（§0.2(1) e / 本卡）：402 来源冻结（freeze_source=402_account 或账号级 reason
// 带 402 标记）+ Kira 账号 → 到期探针走完成级轻请求（sweepProbe402FrozenAccount），
// 因为 Kira 的 usage GET 不是完成级请求，永不能解冻 402 冻结；维度来源冻结与 TH 链
// 照旧走各自确认探针（Kira 维度冻结 = usage GET 9 格表，TH = 完成级 ping）。
func (s *CNQuotaLifecycleService) sweepProbeAccount(ctx context.Context, account *Account, state *cnQuotaLifecycleState, now time.Time) error {
	frozen402 := is402AuthoritativeFrozen(account, state)
	if frozen402 && cnQuotaLifecycleProviderOf(account) == cnQuotaLifecycleProviderKira {
		return s.sweepProbe402FrozenAccount(ctx, account, state, now)
	}
	outcome, completed2xx, perr := s.runConfirmationProbe(ctx, account)
	probeAt := s.now().UTC().Format(time.RFC3339)
	switch outcome {
	case quotaProbeRecovered:
		if frozen402 && !completed2xx {
			// 来源交叉（§0.2(1)）：402 权威冻结账号的事实层 Recovered（含 9 格表
			// Recovered）不得解冻，只按迁移表推进 probe_due；完成级 2xx 是唯一解冻事件。
			// 推进落库失败 ⇒ 失败关闭：本次不视为已消费，下轮重试（检查单 #3）。
			if _, err := s.advance402ProbeDue(ctx, account, state, outcome, now); err != nil {
				return err
			}
			return nil
		}
		// 恢复闭环：清停调 + resolve 告警 + 刷新快照（可选）+ 状态落墓碑。
		return s.clearQuotaExhaustedPark(ctx, account)
	case quotaProbeExhausted:
		// 仍耗尽：保持停调 + 告警保持 firing。停调到期已过（官方恢复时间失准）
		// 时按 L4 重取恢复时间续停；无官方值则远期占位续停，等下一轮循环确认。
		recoveryAt, source, known := s.resolveRecoveryTime(account, "", now)
		if !known || !recoveryAt.After(now) {
			recoveryAt = now.Add(quotaLifecycleUnknownRecoveryPlaceholder)
			source = cnQuotaRecoverySourceUnknown
			known = false
		}
		if err := s.reParkUntilRecovery(ctx, account, state, recoveryAt, known, source, now); err != nil {
			return err
		}
		if frozen402 {
			// 402 来源：保持停调（上方续停/re-park 已落地），probe_due 按迁移表推进
			// （仍 402 → 每日 reset_at；本代际首次 fresh>0 → immediate 一次）。
			if _, err := s.advance402ProbeDue(ctx, account, state, outcome, now); err != nil {
				return err
			}
		} else {
			s.persistLifecycleState(ctx, account.ID, &cnQuotaLifecycleState{
				State:            cnQuotaLifecycleStateExhausted,
				RecoveryAt:       cnQuotaRFC3339OrEmpty(recoveryAt, known),
				RecoverySource:   source,
				LastProbeAt:      probeAt,
				LastProbeOutcome: cnQuotaLifecycleProbeExhausted,
				UpdatedAt:        probeAt,
			})
		}
		s.ensureQuotaAlertFiring(ctx, account, "", cnQuotaExhaustedReason("", known, recoveryAt, source))
		fmt.Printf("[CNQuotaLifecycle] account=%d still exhausted after recovery deadline, re-parked until=%s\n",
			account.ID, recoveryAt.UTC().Format(time.RFC3339))
		return nil
	default:
		if frozen402 {
			// 402 来源：不确定同样只推进 probe_due（失败关闭，绝不恢复）。
			if _, err := s.advance402ProbeDue(ctx, account, state, quotaProbeUncertain, now); err != nil {
				return err
			}
			s.ensureQuotaAlertFiring(ctx, account, "", cnQuotaUncertainReason("", perr))
			fmt.Printf("[CNQuotaLifecycle] account=%d 402-source freeze: probe uncertain (fail-closed, keep parked), err=%v\n", account.ID, perr)
			return nil
		}
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

// sweepProbe402FrozenAccount 402 来源冻结（Kira）账号的到期探针（§0.2(1) e /
// §3.3 probe_due 迁移表）：到期时发**完成级轻请求**（max_tokens=1 ping，与 TH 链
// probeTokenHarborExhaustion 同形状），而不是 Kira 的 usage GET——usage GET 是只读
// 用量查询，VND=0 时也能 2xx，永远不产生完成级证据，沿用它会让 402 冻结无解冻事件
// （锁定项 5：一切账号级暂停必须能自动恢复）。分流判据沿用 G1 的
// freeze_source/reason 交叉判定，不新造状态。
//
// 响应分派：
//   - 2xx → 解冻（唯一解冻事件，走既有清除路径 clearQuotaExhaustedPark）；
//   - 402 → 保持冻结 + 按迁移表重排 probe_due（每日 reset_at / 本代际首次
//     fresh>0 的 immediate，语义同 G1 advance402ProbeDue）；
//   - 401 → 转交既有认证失败链（SetError），退出 402 恢复循环；
//   - 5xx/传输失败/其他未列举 → 失败关闭保持冻结，受控快速重探（既有
//     f3FastRetryMax/f3FastRetryBackoff 上限）后按每日 reset_at 重排。
func (s *CNQuotaLifecycleService) sweepProbe402FrozenAccount(ctx context.Context, account *Account, state *cnQuotaLifecycleState, now time.Time) error {
	res := s.runCompletionProbe(ctx, account)
	// 受控快速重探（瞬时传输错误/5xx）：秒级退避、上限 f3FastRetryMax 次（既有上限）。
	if res.Class == f3ProbeTransient {
		transient := true
		for i := 0; i < f3FastRetryMax; i++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(f3FastRetryBackoff)
			res = s.runCompletionProbe(ctx, account)
			if res.Class != f3ProbeTransient {
				transient = false
				break
			}
		}
		if transient {
			// 达上限：失败关闭——保持冻结（不清停调），按迁移表重排每日 due，告警
			// 保持 firing；不终止自动恢复，下轮 sweep 继续到期探测。
			if _, err := s.advance402ProbeDue(ctx, account, state, quotaProbeUncertain, now); err != nil {
				return err
			}
			s.ensureQuotaAlertFiring(ctx, account, "", cnQuotaUncertainReason("", res.Err))
			fmt.Printf("[CNQuotaLifecycle] account=%d 402 freeze completion probe transient (fail-closed, keep frozen), err=%v\n",
				account.ID, res.Err)
			return nil
		}
	}
	switch res.Class {
	case f3ProbeRecovered2xx:
		// 完成级 2xx = 唯一解冻事件。
		if err := s.clearQuotaExhaustedPark(ctx, account); err != nil {
			return err
		}
		fmt.Printf("[CNQuotaLifecycle] account=%d 402 freeze cleared by completion-level 2xx (only unfreeze event)\n", account.ID)
		return nil
	case f3ProbeQuota402:
		// 仍 402：保持冻结 + 续停至官方恢复时间（与 G1「仍耗尽」分支同口径）+ 按迁移表
		// 重排 probe_due（每日 reset_at）。
		recoveryAt, source, known := s.resolveRecoveryTime(account, "", now)
		if !known || !recoveryAt.After(now) {
			recoveryAt = now.Add(quotaLifecycleUnknownRecoveryPlaceholder)
			source = cnQuotaRecoverySourceUnknown
			known = false
		}
		if err := s.reParkUntilRecovery(ctx, account, state, recoveryAt, known, source, now); err != nil {
			return err
		}
		if _, err := s.advance402ProbeDue(ctx, account, state, quotaProbeExhausted, now); err != nil {
			return err
		}
		s.ensureQuotaAlertFiring(ctx, account, "", cnQuotaExhaustedReason("", known, recoveryAt, source))
		fmt.Printf("[CNQuotaLifecycle] account=%d 402 freeze completion probe still 402, frozen; probe_due re-scheduled\n", account.ID)
		return nil
	case f3ProbeAuth401:
		// 401 = 凭据失效，不是额度问题：转交既有认证失败链，退出恢复循环。
		return s.handoff402FreezeToAuthFailure(ctx, account)
	default:
		// 其他未列举响应（R18-F3 同构）：失败关闭——保持冻结 + 每日重排 + 告警。
		if _, err := s.advance402ProbeDue(ctx, account, state, quotaProbeUncertain, now); err != nil {
			return err
		}
		s.ensureQuotaAlertFiring(ctx, account, "", cnQuotaUncertainReason("", res.Err))
		fmt.Printf("[CNQuotaLifecycle] account=%d 402 freeze completion probe unclassified (fail-closed, keep frozen), status=%d\n",
			account.ID, res.Status)
		return nil
	}
}

// clearQuotaExhaustedPark 恢复闭环的清除路径（G1 既有，维度冻结与 402 冻结共用）：
// 清停调 + resolve 告警 + 状态落墓碑 + 退出进程内跟踪 + 刷新快照（可选注入）。
func (s *CNQuotaLifecycleService) clearQuotaExhaustedPark(ctx context.Context, account *Account) error {
	probeAt := s.now().UTC().Format(time.RFC3339)
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
}

// reParkUntilRecovery 「仍耗尽/仍 402」的续停逻辑（G1 既有，两条链共用）：把账号级
// until 收敛到官方恢复时间。受控缩短前提（方案 F4）：仅 lifecycle 域内 reason 拥有的
// until 允许被本状态机续停/改写；他因暂停（余额阈值、muse 用量等）的 until 不动。空
// reason 表示当前无外因暂停（本账号由生命周期状态机接管/续停），lifecycle 可建立/维持
// 暂停。探测资格 floor（state.RecoveryAt）始终按 resolveRecoveryTime 收敛至 reset_at。
func (s *CNQuotaLifecycleService) reParkUntilRecovery(ctx context.Context, account *Account, state *cnQuotaLifecycleState, recoveryAt time.Time, known bool, source string, now time.Time) error {
	if account.TempUnschedulableReason != "" && !isLifecycleOwnedReason(account.TempUnschedulableReason) {
		return nil
	}
	until := state.parkedUntil()
	// F4 受控缩短（派发单 C1-a-r3 / 方案 §4 F4）：lifecycle 拥有该 until 且账号级
	// until 晚于官方 reset_at（矛盾第三形态）时，缩短（而非延长）至 reset_at，使过度
	// 暂停收敛；非 lifecycle 拥有的 until 与「until<=reset_at 已收敛」两种情形走下方
	// 只延长/新建 re-park 守卫。
	if isLifecycleOwnedReason(account.TempUnschedulableReason) &&
		account.TempUnschedulableUntil != nil &&
		account.TempUnschedulableUntil.After(recoveryAt) {
		if sh, ok := s.accountRepo.(tempUnschedulableShortener); ok {
			if _, err := sh.ShortenTempUnschedulableIfOwned(ctx, account.ID, recoveryAt,
				cnQuotaExhaustedReasonPrefix); err != nil {
				return fmt.Errorf("cn quota lifecycle shorten account %d: %w", account.ID, err)
			}
		}
		return nil
	}
	if until == nil || until.Before(now) || recoveryAt.After(*until) {
		if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, recoveryAt,
			cnQuotaExhaustedReason("", known, recoveryAt, source)); err != nil {
			return fmt.Errorf("cn quota lifecycle re-park account %d: %w", account.ID, err)
		}
	}
	return nil
}

// handoff402FreezeToAuthFailure 402 冻结的完成级探针返回 401：凭据失效属认证问题，
// 不属于额度恢复循环管辖——转交既有认证失败链（SetError 既有原语：status=error +
// error_message + schedulable=false，无新机制），并让本账号退出 402 恢复循环
// （状态落墓碑 + 退出进程内跟踪）。账号级 until 由 lifecycle 拥有时一并清除，避免
// 认证失败后再无人解冻的僵死 park；他因拥有的 until 不动。告警交由认证失败链承载，
// 本链不再 fire 额度告警（避免误导）。
func (s *CNQuotaLifecycleService) handoff402FreezeToAuthFailure(ctx context.Context, account *Account) error {
	msg := fmt.Sprintf("402 冻结恢复探针返回 401（转交认证失败链）；%s",
		cnQuotaUncertainReason("", errors.New("completion probe status 401")))
	if err := s.accountRepo.SetError(ctx, account.ID, msg); err != nil {
		return fmt.Errorf("cn quota lifecycle handoff 401 account %d: %w", account.ID, err)
	}
	if account.TempUnschedulableReason == "" || isLifecycleOwnedReason(account.TempUnschedulableReason) {
		if err := s.accountRepo.ClearTempUnschedulable(ctx, account.ID); err != nil {
			return fmt.Errorf("cn quota lifecycle clear 401 account %d: %w", account.ID, err)
		}
	}
	probeAt := s.now().UTC().Format(time.RFC3339)
	s.persistLifecycleState(ctx, account.ID, &cnQuotaLifecycleState{
		State:            cnQuotaLifecycleStateRecovered,
		LastProbeAt:      probeAt,
		LastProbeOutcome: cnQuotaLifecycleProbeUncertain,
		UpdatedAt:        probeAt,
	})
	s.untrack(account.ID)
	fmt.Printf("[CNQuotaLifecycle] account=%d 402 freeze completion probe 401, handed off to auth-failure chain\n", account.ID)
	return nil
}

// runCompletionProbe 402 来源冻结的到期探针：完成级轻请求（照 probeTokenHarborExhaustion
// 请求形状——账号 openai 协议路径 chat/completions + max_tokens=1 ping；模型取
// cnQuotaLifecycleProbeModel 同口径；URL 过既有出站校验 cnValidateProbeURL）。
// 响应类别复用既有 classifyF3ProbeStatus（2xx/401/402/403/429/5xx/未列举）。
func (s *CNQuotaLifecycleService) runCompletionProbe(ctx context.Context, account *Account) f3ProbeResult {
	if s == nil || account == nil {
		return f3ProbeResult{Class: f3ProbeUnclassified, Err: errors.New("completion probe: no account")}
	}
	if s.completionProbeOverride != nil {
		cls, status, body, err := s.completionProbeOverride(ctx, account)
		return f3ProbeResult{Class: cls, Status: status, Body: body, Err: err}
	}
	return s.runCompletionPing(ctx, account)
}

// sweepTHResidualRecoveryDue TH 残留矛盾读侧判定（D-QLM-016，D-QLM-018 补第三
// 形态，D-QLM-021 第三形态去掉 pe=true 前提）：lifecycle 里挂着未来的
// recovery_at（旧口径 renewsAt 存量），且 reset_at 呈三形态矛盾之一——缺失、
// 已过期、停调时刻晚于官方 reset_at（停调到期点不是 reset_at 口径；生产实证
// 2026-10-08：9 个存量账号 recovery_at 挂 renewsAt 旧口径、刷新链新快照
// reset_at 仍在未来）。快照与恢复时间自相矛盾，视为 probe 到期进入确认循环
// （unknown 路径可达）。仅在 sweep 读路径判定，无写入，竞态类整体消失。来源按
// 显式允许清单判定：只认 snapshot——旧 renewsAt 存量写入时来源即 snapshot
// （confirmExhausted/sweepProbeAccount/convergeTHStockRecovery 三个写入点口径）；
// upstream（L4 最权威，交由其自身到期推进）与其他任何未知/未来新增来源不触发
// （fail-closed，不扩语义）。第一/二形态（reset_at 缺失/过期，语义=「仍耗尽无
// 恢复时刻」）保持 plan_exhausted=true 前提：pe=false 不触发（无矛盾证据，保守
// 不扩语义）；第三形态（停调晚于官方 reset_at）以时间矛盾独立触发，pe 值不作
// 前提（生产实证 2026-10-08 第二轮：4 账号官方已恢复 pe=false 但停调挂旧口径
// 11-06，探针 recovered 即清停调）；快照缺失不触发（无矛盾证据）。account 为
// sweepCandidates 取到的 repo 新鲜副本，无检查-写入窗口。
func (s *CNQuotaLifecycleService) sweepTHResidualRecoveryDue(account *Account, st *cnQuotaLifecycleState, now time.Time) bool {
	if s == nil || account == nil || st == nil {
		return false
	}
	if cnQuotaLifecycleProviderOf(account) != cnQuotaLifecycleProviderTokenHarbor {
		return false
	}
	if st.RecoverySource != cnQuotaRecoverySourceSnapshot {
		return false
	}
	// F4 受控缩短前提（派发单 C1-a-r3）：仅 lifecycle 域拥有的 until 才纳入矛盾判定
	//（缩短/续停）；他因（余额阈值、muse 用量窗口等）暂停的 until 不被 lifecycle
	// 动，保持既有负例语义不变（矛盾门直接返回 false，不触发探针/缩短）。
	if !isLifecycleOwnedReason(account.TempUnschedulableReason) {
		return false
	}
	// 读侧矛盾判定改比较账号级 temp_unschedulable_until vs 快照 reset_at（方案 F4）：
	// 账号级 until 是调度实际执行的暂停到期点（source of truth），替代 lifecycle
	// 内部 recovery_at 的镜像——两者在 confirmExhausted 写入时同源，但账号级 until
	// 是权威暂停判定；若二者因任何时序偏差，应以账号级为准，避免 stale 镜像误判。
	if account.TempUnschedulableUntil == nil || !account.TempUnschedulableUntil.After(now) {
		return false
	}
	snap, ok := TokenHarborPassSnapshotFromExtra(account)
	// 快照缺失：无矛盾证据，保守不扩语义（fail-closed）。
	if !ok {
		return false
	}
	// 三形态矛盾：
	//   - 第一/二形态（pe=true，reset_at 缺失/已过期）：仍耗尽但无未来恢复点；
	//   - 第三形态（账号级 until 晚于官方 reset_at，无论官方 reset_at 在未来还是
	//     已过期——过期时矛盾更直接）：过度暂停（旧 renewsAt 口径存量），以时间
	//     矛盾独立触发（pe 值不作前提，生产实证 2026-10-08 第二轮：4 账号官方已
	//     恢复 pe=false 但停调挂旧口径 11-06，探针 recovered 即清停调）。
	return snap.PlanExhausted && (snap.ResetAt == nil || !snap.ResetAt.After(now)) ||
		(snap.ResetAt != nil && account.TempUnschedulableUntil.After(*snap.ResetAt))
}

// isLifecycleOwnedReason 判定账号级 temp_unschedulable_reason 是否由本生命周期状态机
// 域拥有（方案 F4 受控缩短前提）：仅 lifecycle 写入的 reason（前缀
// cnQuotaExhaustedReasonPrefix，含 402 权威冻结后缀变体）允许被 lifecycle 改写/续停/
// 缩短；他因（余额阈值、muse 用量窗口等）暂停的 until 不被 lifecycle 动。
func isLifecycleOwnedReason(reason string) bool {
	return strings.HasPrefix(reason, cnQuotaExhaustedReasonPrefix)
}

// tempUnschedulableShortener 是 F4 受控缩短存储原语的窄面（方案 §4 F4 / 派发单
// C1-a-r3）。不扩大 AccountRepository 通用接口（避免波及其它消费方桩），由本服务
// 在调用点经 accountRepo 类型断言消费；生产 accountRepository 与测试 fake 均实现之。
type tempUnschedulableShortener interface {
	// ShortenTempUnschedulableIfOwned 当且仅当 until>newUntil 且 reason 以
	// ownedReasonPrefix 前缀开头时，把 until 缩短至 newUntil；返回是否实际缩短。
	ShortenTempUnschedulableIfOwned(ctx context.Context, accountID int64, newUntil time.Time, ownedReasonPrefix string) (bool, error)
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

// --- F3 403 content-policy 恢复链（派发单 C1-b② / 方案 §3.3） ---

// f3FastRetryMax / f3FastRetryBackoff 是瞬时传输错误/5xx 受控快速重探参数（派发单
// 1d「瞬时传输错误/5xx → 受控快速重探，有上限，建议 2 次、短退避秒级」）：最多 2 次
// 快速重探、每次 2 秒退避。达上限后不终止自动恢复（R2-F4）——until 仍是权威恢复时间，
// 账号保持「恢复到期待确认」状态，由既有 lifecycle 周期 sweep 在后续每轮继续到期探测。
const (
	f3FastRetryMax     = 2
	f3FastRetryBackoff = 2 * time.Second
)

// sweepF3RecoveryAccount 对单个 F3 候选执行恢复探测并按 §3.3 逐条分派（派发单 1b/1c/1d）。
func (s *CNQuotaLifecycleService) sweepF3RecoveryAccount(ctx context.Context, account *Account, rec *http403RecoveryRecord, now time.Time) error {
	if s.f3Repo == nil {
		return nil
	}
	// stale 核验（R17-F2）：键内 state_revision 与全局 sched_state_revision 失配 = 已易主
	// → 原子清除 F3 键（只删键，不动 status/error/temp_unschedulable，状态归他链管），本轮不探测。
	if rec.StateRevision != http403GlobalStateRevision(account.Extra) {
		if _, err := s.f3Repo.RemoveHTTP403RecoveryRecord(ctx, account.ID, rec.Generation); err != nil {
			return err
		}
		slog.Warn("cn_quota_403_recovery_stale_removed",
			"account_id", account.ID, "generation", rec.Generation,
			"reason", "owner changed; F3 key removed atomically, state owned by other chain")
		return nil
	}

	slog.Info(eventCNQuota403RecoveryProbeStarted,
		"account_id", account.ID, "generation", rec.Generation, "until", rec.Until)

	res := s.runHTTP403RecoveryProbe(ctx, account)

	// 受控快速重探（瞬时传输错误/5xx）：秒级退避、上限 f3FastRetryMax 次。
	if res.Class == f3ProbeTransient {
		exhausted := true
		for i := 0; i < f3FastRetryMax; i++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(f3FastRetryBackoff)
			res = s.runHTTP403RecoveryProbe(ctx, account)
			if res.Class != f3ProbeTransient {
				exhausted = false
				break
			}
		}
		if exhausted {
			// 达上限：保留 F3 记录（until 不变 = 权威恢复时间仍有效），转既有 sweep 周期
			// 继续探测（非终止）。下轮 sweep 仍会把本账号纳入候选。
			slog.Info(eventCNQuota403RecoveryFastRetryExhausted,
				"account_id", account.ID, "generation", rec.Generation, "until", rec.Until)
			slog.Info(eventCNQuota403RecoveryProbeResult,
				"account_id", account.ID, "generation", rec.Generation, "until", rec.Until,
				"response_class", f3EventResponseClassFastRetryExhausted, "result", "kept_for_sweep")
			return nil
		}
	}

	s.dispatchF3ProbeResult(ctx, account, rec, res, now)
	return nil
}

// dispatchF3ProbeResult 按恢复探针响应类别分派（派发单 1d / §3.3）。
func (s *CNQuotaLifecycleService) dispatchF3ProbeResult(ctx context.Context, account *Account, rec *http403RecoveryRecord, res f3ProbeResult, now time.Time) {
	switch res.Class {
	case f3ProbeRecovered2xx:
		// 2xx → CAS 清 error + 恢复调度 + 清 F3 拥有 until（ClearHTTP403RecoveryIfOwned 同语句）。
		cleared, err := s.f3Repo.ClearHTTP403RecoveryIfOwned(ctx, account.ID, rec.Generation)
		if err != nil {
			slog.Warn("cn_quota_403_recovery_clear_failed", "account_id", account.ID, "error", err)
			return
		}
		if cleared {
			// 清零三振计数（经注入的窄面计数器清除依赖；未注入则跳过）。
			if s.http403Counter != nil {
				if rerr := s.http403Counter.ResetOpenAI403Count(ctx, account.ID); rerr != nil {
					slog.Warn("cn_quota_403_recovery_counter_reset_failed", "account_id", account.ID, "error", rerr)
				}
			}
			s.resolveQuotaAlert(ctx, account.ID)
			slog.Info(eventCNQuota403RecoveryProbeResult,
				"account_id", account.ID, "generation", rec.Generation, "until", rec.Until,
				"response_class", f3EventResponseClassRecovered2xx, "result", "recovered")
		} else {
			// CAS 失配（在途新状态写入 / 他链已夺权）→ 丢弃本次结果（R11-F3 ①）。
			slog.Info(eventCNQuota403RecoveryCASConflict,
				"account_id", account.ID, "generation", rec.Generation, "until", rec.Until,
				"reason", "in-flight state write (generation/owner/revision mismatch) discarded")
		}
	case f3ProbeStill403:
		s.handleF3Still403(ctx, account, rec, res, now)
	case f3ProbeAuth401, f3ProbeQuota402, f3ProbeRate429:
		s.handleF3Handoff(ctx, account, rec, res, now)
	default:
		// 未列举响应（R18-F3）：失败关闭——保持冻结、不清 error、保留下轮 sweep 探测资格、
		// ensureQuotaAlertFiring 告警；不做无界重试、不绕道既有错误链。
		s.ensureQuotaAlertFiring(ctx, account, rec.Reason, "http_403_recovery unclassified probe response (fail-closed, kept for next sweep)")
		slog.Info(eventCNQuota403RecoveryProbeResult,
			"account_id", account.ID, "generation", rec.Generation, "until", rec.Until,
			"response_class", string(res.Class), "result", "fail_closed_kept")
	}
}

// handleF3Still403 仍 403：带新 until → 解析落库续等；无 until → 冷却循环（同键重写
// until = now + 既有 403 冷却常量，reason 标记 no-until 冷却轮）。
func (s *CNQuotaLifecycleService) handleF3Still403(ctx context.Context, account *Account, rec *http403RecoveryRecord, res f3ProbeResult, now time.Time) {
	newUntil, ok := ParseHTTP403PausedUntil("", res.Body)
	reason := "http_403_recovery until refreshed"
	if !ok {
		newUntil = now.Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
		reason = "http_403_recovery no-until cooldown round"
	}
	if _, err := s.f3Repo.UpdateHTTP403RecoveryUntil(ctx, account.ID, rec.Generation, newUntil, reason); err != nil {
		slog.Warn("cn_quota_403_recovery_update_until_failed", "account_id", account.ID, "error", err)
		return
	}
	result := "continue_wait"
	if !ok {
		result = "no_until_cooldown_round"
	}
	slog.Info(eventCNQuota403RecoveryProbeResult,
		"account_id", account.ID, "generation", rec.Generation,
		"until", newUntil.UTC().Format(time.RFC3339),
		"response_class", f3EventResponseClassStill403Cooldown, "result", result)
}

// handleF3Handoff 401/402/429 转交：同一单条 UPDATE 内移除 F3 元数据 + 写入新链状态
// （旧 403 状态不得残留阻塞后续恢复，R7-F3）。402 额外转交既有额度响应式链
// （OnUpstreamQuotaExhaustedScoped 既有路径，仅 CN 平台生效；非 CN 平台 no-op）。
func (s *CNQuotaLifecycleService) handleF3Handoff(ctx context.Context, account *Account, rec *http403RecoveryRecord, res f3ProbeResult, now time.Time) {
	var handoffTarget string
	var target HTTP403TransitionTarget
	switch res.Class {
	case f3ProbeAuth401:
		handoffTarget = "401"
		// 401 → 认证失败链：SetError 等价（error_message = 探针错误文本）。该链的停止
		// 语义本就是 status=error，维持现状正确，不改（R11-F3 验收基线）。
		target = HTTP403TransitionTarget{
			Status:       StatusError,
			ErrorMessage: f3HandoffErrorText(res, "401 authentication failed"),
			Schedulable:  false,
		}
	case f3ProbeQuota402:
		handoffTarget = "402"
		// 402 → 转交既有额度响应式链（下文 OnUpstreamQuotaExhaustedScoped）；本步复刻
		// 既有额度链 park 终态 = status active + temp park（生产实证：exhausted 号
		// status=active + cn_quota_exhausted reason），故 Status=active、ErrorMessage 置空
		// （清旧 403 残留、不写转交文本进 error 字段；ops 可见性由 §7.1 probe_result 的
		// handoff_target 字段承担）。新增过渡 park：TempUnschedulableUntil = now + 冷却、
		// reason 标记 402 转交；CN 号随后由 OnUpstreamQuotaExhaustedScoped 按自身语义
		// 接管 park（仅延长守卫），非 CN 号 / 额度链调用失败时该 park 保证冷却后自然回归
		// 调度而非无 until 僵死（机制 = 既有 SetTempUnschedulable 语义，无新机制）。
		until := now.Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
		reason := "http_403_recovery handed off to 402 quota chain"
		target = HTTP403TransitionTarget{
			Status:                  StatusActive,
			ErrorMessage:            "",
			Schedulable:             false,
			TempUnschedulableUntil:  &until,
			TempUnschedulableReason: &reason,
		}
	case f3ProbeRate429:
		handoffTarget = "429"
		// 429 → 复刻既有限流停调效果（SetTempUnschedulable 既有语义）：账号本为 active，
		// 故 Status=active、ErrorMessage 置空（清旧 403 残留、不写转交文本进 error 字段；
		// ops 可见性由 §7.1 probe_result 的 handoff_target 字段承担）。temp park 保持现状值
		// （F3 拥有的 temp_unschedulable_until，CAS 清理/转交据此同语句清除旧 F3 拥有的
		// until）。转交写 error 后 until 过期残留「error 信息 + 可调度」不一致态（生产
		// tier-b5 实证为缺陷态），故 Status/error 一律复刻 active 语义。
		until := now.Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
		reason := "http_403_recovery handed off to 429 rate-limit chain"
		target = HTTP403TransitionTarget{
			Status:                  StatusActive,
			ErrorMessage:            "",
			Schedulable:             false,
			TempUnschedulableUntil:  &until,
			TempUnschedulableReason: &reason,
		}
	}
	applied, err := s.f3Repo.TransitionHTTP403RecoveryTo(ctx, account.ID, rec.Generation, target)
	if err != nil {
		slog.Warn("cn_quota_403_recovery_transition_failed", "account_id", account.ID, "handoff_target", handoffTarget, "error", err)
		return
	}
	if !applied {
		// CAS 失配（在途新状态写入 / 他链已夺权）→ 丢弃本次转交结果。
		slog.Info(eventCNQuota403RecoveryCASConflict,
			"account_id", account.ID, "generation", rec.Generation, "until", rec.Until,
			"handoff_target", handoffTarget, "reason", "in-flight state write (generation/owner/revision mismatch) discarded")
		return
	}
	// 402 转交既有额度响应式链（OnUpstreamQuotaExhaustedScoped 既有路径）；非 CN 平台 no-op。
	if res.Class == f3ProbeQuota402 && cnQuotaLifecycleProviderOf(account) != "" {
		signal := QuotaExhaustionSignal{Status: 402, Scope: QuotaScopeAccount, Platform: account.Platform, Msg: target.ErrorMessage}
		if herr := s.OnUpstreamQuotaExhaustedScoped(ctx, account, target.ErrorMessage, signal); herr != nil {
			slog.Warn("cn_quota_403_recovery_402_handoff_chain_failed", "account_id", account.ID, "error", herr)
		}
	}
	slog.Info(eventCNQuota403RecoveryProbeResult,
		"account_id", account.ID, "generation", rec.Generation, "until", rec.Until,
		"response_class", f3EventResponseClassHandedOff401402429, "handoff_target", handoffTarget, "result", "handed_off")
}

// f3HandoffErrorText 构造转交错误文本（含探针状态码，便于 ops 排查）。
func f3HandoffErrorText(res f3ProbeResult, prefix string) string {
	return fmt.Sprintf("Access forbidden (403) recovery probe -> %s (status %d)", prefix, res.Status)
}

// runHTTP403RecoveryProbe 派发单次 F3 恢复探针：优先用注入覆盖（测试），否则复用既有
// 完成级轻请求（runCompletionPing，同一轻请求路径/客户端/代理/鉴权，禁止新造探针基建）。
func (s *CNQuotaLifecycleService) runHTTP403RecoveryProbe(ctx context.Context, account *Account) f3ProbeResult {
	if s.f3ProbeOverride != nil {
		cls, status, body, err := s.f3ProbeOverride(ctx, account)
		return f3ProbeResult{Class: cls, Status: status, Body: body, Err: err}
	}
	return s.runCompletionPing(ctx, account)
}

// runCompletionPing 完成级轻请求（TH 确认探针 probeTokenHarborExhaustion 的既有请求
// 形状，F3 恢复探针与 402 冻结到期探针共用）：经 httpUpstream.DoWithTLS +
// resolveAccountProxyURL 对账号 openai-format 基址（Kira 走其 base_url，过
// cnValidateProbeURL 出站校验）发 chat/completions + max_tokens=1 ping，派生响应类别
// 与状态码/响应体（仍 403 时需从响应体解析 paused until）。传输错误统一归类为瞬时重探。
func (s *CNQuotaLifecycleService) runCompletionPing(ctx context.Context, account *Account) f3ProbeResult {
	if s.httpUpstream == nil || s.cfg == nil {
		return f3ProbeResult{Class: f3ProbeTransient, Err: errF3NoProbeTransport}
	}
	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return f3ProbeResult{Class: f3ProbeTransient, Err: errors.New("f3 recovery probe: no api key")}
	}
	modelKey := cnQuotaLifecycleProbeModel(account)
	if modelKey == "" {
		return f3ProbeResult{Class: f3ProbeTransient, Err: errors.New("f3 recovery probe: no probe model")}
	}
	baseURL := account.GetOpenAIFormatBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.openai.com"
	}
	normalized, err := cnValidateProbeURL(s.cfg, baseURL)
	if err != nil {
		return f3ProbeResult{Class: f3ProbeTransient, Err: fmt.Errorf("f3 recovery probe validate url: %w", err)}
	}
	body, err := json.Marshal(map[string]any{
		"model":      modelKey,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		return f3ProbeResult{Class: f3ProbeTransient, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(normalized, "/")+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return f3ProbeResult{Class: f3ProbeTransient, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := s.resolveAccountProxyURL(account)
	callCtx, cancel := context.WithTimeout(ctx, probeRequestHardTimeout)
	defer cancel()
	resp, err := s.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, account.ID, account.Concurrency, nil)
	if err != nil {
		return f3ProbeResult{Class: f3ProbeTransient, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return f3ProbeResult{Class: classifyF3ProbeStatus(resp.StatusCode), Status: resp.StatusCode, Body: raw}
}

// runLegacyHTTP403Init 存量初始化（R8-F1）：每轮 sweep 跑该查询（查不到即空转，成本一次
// 索引扫描，无需进程内开关）；幂等 = 候选查询排除已持有 http_403_recovery 键的账号。
func (s *CNQuotaLifecycleService) runLegacyHTTP403Init(ctx context.Context, now time.Time) error {
	if s.f3Repo == nil {
		return nil
	}
	legacy, err := s.f3Repo.ListLegacyHTTP403ErrorAccounts(ctx, 200)
	if err != nil {
		return err
	}
	for _, acc := range legacy {
		if acc == nil {
			continue
		}
		if err := s.initLegacyHTTP403Recovery(ctx, acc, now); err != nil {
			return err
		}
	}
	return nil
}

// initLegacyHTTP403Recovery 收编单条存量 403 error：解析现存 error_message 的 paused until，
// 成功 → AllocHTTP403Generation + Mark403PausedWithRecovery（until=解析值）；无法解析 →
// 同原语 + until = now + 10min 冷却循环 + parse_failed 事件 + 告警。解析成功/失败均发事件。
func (s *CNQuotaLifecycleService) initLegacyHTTP403Recovery(ctx context.Context, account *Account, now time.Time) error {
	if s.f3Repo == nil {
		return nil
	}
	generation, err := s.f3Repo.AllocHTTP403Generation(ctx, account.ID)
	if err != nil {
		return err
	}
	until, ok := ParseHTTP403PausedUntil(account.ErrorMessage, nil)
	reason := "http_403_recovery legacy init"
	if ok {
		slog.Info(eventCNQuota403PausedUntilParsed,
			"account_id", account.ID, "generation", generation, "until", until.UTC().Format(time.RFC3339))
	} else {
		until = now.Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
		reason = "http_403_recovery legacy init no-until cooldown"
		slog.Info(eventCNQuota403PausedUntilParseFailed,
			"account_id", account.ID, "generation", generation, "until", until.UTC().Format(time.RFC3339),
			"reason", "legacy 403 error_message had no parseable paused-until; entered cooldown loop")
	}
	// Mark403PausedWithRecovery 复刻 SetError 字段效果 + 合并 F3 恢复键（字段效果与现状一致）。
	if err := s.f3Repo.Mark403PausedWithRecovery(ctx, account.ID, until, generation, reason, account.ErrorMessage, nil); err != nil {
		return err
	}
	if !ok {
		// 无法解析：冷却循环 + 告警（保持 ops 可见性）。
		s.ensureQuotaAlertFiring(ctx, account, account.ErrorMessage, "legacy 403 without parseable paused-until (cooldown loop)")
	}
	return nil
}

// --- 确认探针（§4.4） ---

// runConfirmationProbe 派发单次确认探针：优先用注入覆盖（测试），否则按账号
// 上游类型分派（Kira=GET /api/user/usage 拉当日窗口，不烧 VND；TH=raw 模型
// max_tokens=1 ping）。
//
// 第二返回值 completed2xx 报告本探针是否收到**完成级 2xx**（上游对请求本身的完成
// 响应，如 TH 的 max_tokens=1 ping 返回 2xx）。这是 §0.2(1) 单一状态迁移表中
// 402 权威冻结的**唯一解冻事件**；Kira 的 usage GET 是只读用量查询而非完成级请求，
// 其 2xx 不构成解冻事件（completed2xx=false），故 402 来源冻结账号只能由
// completed2xx 清除。测试注入覆盖（probeOverride）不产生完成级证据，视为 false。
func (s *CNQuotaLifecycleService) runConfirmationProbe(ctx context.Context, account *Account) (quotaProbeOutcome, bool, error) {
	if s == nil {
		return quotaProbeUncertain, false, errors.New("cn quota lifecycle service is nil")
	}
	if s.probeOverride != nil {
		outcome, err := s.probeOverride(ctx, account)
		return outcome, false, err
	}
	switch cnQuotaLifecycleProviderOf(account) {
	case cnQuotaLifecycleProviderKira:
		outcome, err := s.probeKiraExhaustion(ctx, account)
		return outcome, false, err
	case cnQuotaLifecycleProviderTokenHarbor:
		outcome, err := s.probeTokenHarborExhaustion(ctx, account)
		// TH 探针只对 chat/completions 发完成级请求：2xx = 完成级解冻证据。
		return outcome, err == nil && outcome == quotaProbeRecovered, err
	default:
		return quotaProbeUncertain, false, errors.New("unsupported provider for quota confirmation probe")
	}
}

// kiraFreePoolState 免费池三态（Kira 确认探针 usage GET 的解析产物）。
type kiraFreePoolState int

const (
	// kiraFreePoolUnknown 免费池状态不可知：传输失败/非 2xx/解析失败/缺分母。
	kiraFreePoolUnknown kiraFreePoolState = iota
	// kiraFreePoolRemaining 免费池确认有余量（used < limit）。
	kiraFreePoolRemaining
	// kiraFreePoolExhausted 免费池确认耗尽（used >= limit）。
	kiraFreePoolExhausted
)

// kiraVNDGridState 付费 VND 三态（由 C1 余额三态契约派生，不重新解释余额键）。
type kiraVNDGridState int

const (
	// kiraVNDGridUnknown 余额 unknown（stale / error / 缺键）：不产生任何耗尽结论。
	kiraVNDGridUnknown kiraVNDGridState = iota
	// kiraVNDGridPositive 新鲜且 >0。
	kiraVNDGridPositive
	// kiraVNDGridNonPositive 新鲜且 ≤0。
	kiraVNDGridNonPositive
)

// kiraVNDGridStateFrom 把余额三态映射为判定表的 VND 三态（R5-F1：只有 fresh 才产生
// confirmed 结论；stale/error 一律 unknown）。
func kiraVNDGridStateFrom(st BalanceState) kiraVNDGridState {
	if !st.IsFresh() {
		return kiraVNDGridUnknown
	}
	if st.Value > 0 {
		return kiraVNDGridPositive
	}
	return kiraVNDGridNonPositive
}

// evaluateKiraConfirmGrid Kira 确认探针 9 格判定表（方案 §3.2；用户 2026-10-10
// §0.2(1) 再裁定，逐格唯一）：free 三态 × VND 三态 → 探针结论。
//
//	| free \ VND        | fresh>0    | fresh≤0    | unknown    |
//	|-------------------|------------|------------|------------|
//	| free remaining    | Recovered  | Uncertain  | Uncertain  |
//	| free exhausted    | Recovered  | Exhausted  | Uncertain  |
//	| free unknown      | Recovered  | Exhausted  | Uncertain  |
//
// 语义要点：
//   - VND fresh>0 是唯一的 Recovered 依据（付费池可服务 ⇒ 账号可服务，无论免费池
//     呈现何种形态；修复现状 used>=limit 无视 VND>0 直接 Exhausted 的违规）；
//   - free remaining + VND fresh≤0 → Uncertain（不冻结也不恢复：免费池有量但付费
//     耗尽是事实冲突态，冻结/解冻都无权威依据，交响应式 402 链与下轮探针）；
//   - VND unknown 一律 Uncertain（unknown 不产生耗尽结论，也不得反证恢复）；
//   - free unknown（usage GET 失败/缺分母）仍参与判定：VND fresh>0 ⇒ Recovered。
func evaluateKiraConfirmGrid(free kiraFreePoolState, vnd kiraVNDGridState) quotaProbeOutcome {
	switch vnd {
	case kiraVNDGridPositive:
		return quotaProbeRecovered
	case kiraVNDGridNonPositive:
		if free == kiraFreePoolRemaining {
			// 免费池有余量但付费新鲜耗尽：事实冲突，既不停调也不解冻。
			return quotaProbeUncertain
		}
		// free exhausted / free unknown + 新鲜 VND≤0 ⇒ Exhausted。
		return quotaProbeExhausted
	default:
		return quotaProbeUncertain
	}
}

// probeKiraExhaustion Kira 确认探针（§4.4 + 9 格判定表）：GET /api/user/usage 拉
// 当日窗口，不向上游发推理请求（避免烧 VND）。usage GET 结果解析失败/缺分母/非 2xx/
// 传输失败一律归 free unknown 格，与 VND 三态共同查表得结论。
func (s *CNQuotaLifecycleService) probeKiraExhaustion(ctx context.Context, account *Account) (quotaProbeOutcome, error) {
	vnd := kiraVNDGridStateFrom(ResolveKiraVNDBalanceState(account, s.now()))
	free := kiraFreePoolUnknown
	var freeErr error
	switch {
	case s.httpUpstream == nil || s.cfg == nil:
		freeErr = errors.New("probe transport not configured")
	default:
		usageURL, loginURL, err := kiraProbeURLs(s.cfg, account)
		if err != nil {
			freeErr = fmt.Errorf("validate kira probe urls: %w", err)
			break
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
		switch {
		case err != nil:
			freeErr = err
		case status < 200 || status >= 300:
			// 鉴权失败（已尝试重登）/5xx/其他：免费池状态不可知 → unknown 格。
			freeErr = fmt.Errorf("kira usage probe status %d", status)
		default:
			_, used, limit, ok := parseKiraUsageTier(body)
			switch {
			case !ok:
				freeErr = errors.New("kira usage probe: missing summary")
			case limit <= 0:
				freeErr = errors.New("kira usage probe: no daily limit denominator")
			case used >= limit:
				free = kiraFreePoolExhausted
			default:
				free = kiraFreePoolRemaining
			}
		}
	}
	outcome := evaluateKiraConfirmGrid(free, vnd)
	if outcome == quotaProbeUncertain && freeErr == nil {
		freeErr = errors.New("kira confirm probe: grid concluded uncertain (no fresh VND>0 evidence)")
	}
	return outcome, freeErr
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

// convergeTHStockRecovery 存量收敛（D-QLM-007 §2，守卫补强 D-QLM-012）：TH 账号
// 周期快照刷新观察到 plan_exhausted=true 且 reset_at 有效时，把额度状态机里的
// recovery_at 收敛到最新 reset_at，覆盖历史上按 renewsAt 写入的旧值（含线上存量）。
// 守卫三件：① recovery_at 来源为 upstream 不动（L4 上游响应 > 快照）；② cur 已
// 等于/晚于 reset_at 不回退（reset_at 随窗口单调递增，晚于只可能是并发旧快照后写）；
// ③ reset_at 过期/缺失时不收敛、零写入（D-QLM-016：写侧清理的检查-写入窗口在无
// CAS 前提下不可闭合，残留的未来 recovery_at 由 RunRecoverySweep 读侧矛盾判定
// 推进，见 sweepTHResidualRecoveryDue）。仅在已有 lifecycle 状态时动作；无状态/
// 未知不编造。本方法幂等且只改 cn_quota_lifecycle 窄面。
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
		// reset_at 过期/缺失：不收敛到过期值，零写入（回归 007 原样 plain
		// return）。残留的未来 recovery_at 由 sweep 读侧判定推进确认循环。
		return
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
	// 已有 recovery_at：与最新 reset_at 相同则无需改写。cur 晚于 reset_at 时也不
	// 回退：官方 reset_at 随窗口单调递增，cur 更晚只可能是并发双入口（确认路径 /
	// 刷新链）下持旧快照的后写者，回退会把 recovery_at 拖回旧到期点（D-QLM-012
	// §2）。cur 晚于 reset_at 的 renewsAt 存量形态不在此收敛（不回退守卫保留），
	// 由 sweep 读侧 sweepTHResidualRecoveryDue 第三形态（停调晚于官方 reset_at）
	// 推进确认探针收敛（D-QLM-018）。
	if cur, err := time.Parse(time.RFC3339, st.RecoveryAt); err == nil {
		if cur.Equal(*snap.ResetAt) || cur.After(*snap.ResetAt) {
			return
		}
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
	if err := s.persistLifecycleStateStrict(ctx, accountID, st); err != nil {
		// 状态记录失败不阻断状态迁移主路径（停调/清除已落库）；持久候选发现对
		// 已停调账号会因缺状态键跳过，下一轮 402 重进状态机自愈。
		fmt.Printf("[CNQuotaLifecycle] account=%d persist lifecycle state failed: %v\n", accountID, err)
	}
}

// persistLifecycleStateStrict 写 lifecycle 状态到 extra，并**显式上抛持久化失败**
// （检查单 #3）。仅用于「写入成功与否决定本次迁移是否成立」的路径
// （advance402ProbeDue 的代际消费登记）：内存置位 + 落库失败的组合必须被看见，
// 否则会出现「假成功」（重启后重复 immediate）。其余路径继续用吞错版
// persistLifecycleState（状态记录失败可由下一轮自愈）。
func (s *CNQuotaLifecycleService) persistLifecycleStateStrict(ctx context.Context, accountID int64, st *cnQuotaLifecycleState) error {
	if s == nil || s.accountRepo == nil || st == nil {
		return nil
	}
	if st.UpdatedAt == "" {
		st.UpdatedAt = s.now().UTC().Format(time.RFC3339)
	}
	updates := map[string]any{cnQuotaLifecycleExtraKey: map[string]any{
		"state":                  st.State,
		"recovery_at":            st.RecoveryAt,
		"recovery_source":        st.RecoverySource,
		"probe_due_at":           st.ProbeDueAt,
		"last_probe_at":          st.LastProbeAt,
		"last_probe_outcome":     st.LastProbeOutcome,
		"updated_at":             st.UpdatedAt,
		"freeze_source":          st.FreezeSource,
		"freeze_generation":      st.FreezeGeneration,
		"immediate_due_consumed": st.ImmediateDueConsumed,
	}}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, updates); err != nil {
		return fmt.Errorf("cn quota lifecycle persist state for account %d: %w", accountID, err)
	}
	return nil
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
