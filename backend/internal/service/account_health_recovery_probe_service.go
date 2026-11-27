package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// accountHealthProbeCandidateLimit caps how many temp-unschedulable accounts a single
// probe sweep evaluates. The breaker cooldown is short (minutes), so the active set
// is small; the cap protects against pathological backlog after a long outage.
const accountHealthProbeCandidateLimit = 200

// devinProbeMaxCandidates 是单次 devin 账号探测尝试的候选模型上限。GF-20：候选从旧上限
// 5 扩到全部 concrete 键（可用模型可能排在很后面，如第 11 位），但为避免异常账号拖长
// 探测仍设硬上限 20。
const devinProbeMaxCandidates = 20

// openAIOAuthProbeMaxCandidates 是单次 OpenAI OAuth 账号探测尝试的候选模型上限。
//
// 与 devinProbeMaxCandidates 同型（探测侧硬上限，名单更长时只试字典序前 N 个），但
// OAuth/Codex 上游更贵：每个候选探测都是一次真实的 ChatGPT 订阅出站请求（还带 Codex
// 鉴权头与 SSE 流式响应）。故取 5：足以覆盖「管理员维护的名单里靠前的模型被上游禁掉、
// 后面还有可用模型」这一主要形态，又不至于把单账号探测拖成分钟级。
const openAIOAuthProbeMaxCandidates = 5

// accountRecoveryDevinMarkerBudget 是 marker sweep 在单账号锁临界区内跑 Devin 探针的
// 串行预算：最多约 20 个模型 × 每个约 20s ≈ 400s。账号锁必须覆盖完整单账号临界区，
// 故 TTL 至少取本预算 + 30s 余量，避免探针仍在跑时锁提前过期导致临界区重叠。
const accountRecoveryDevinMarkerBudget = 400 * time.Second

// accountHealthProbeBurstConcurrencyLimit 是补探 burst 的在途探测并发上限（进程本地，
// 单实例生效；多实例部署时不约束合计探测量，见方案 §5 风险 6）。cursor 探针是真实
// AgentRun 出站（预算 ≥256），单轮补探若一次入队数十账号（候选上限 200）会瞬间打出
// 数十个真实上游请求 ⇒ 限额有明确必要性（方案 §2.1 / §5 风险 1）。上限作用在补探 burst
// 的并发探测 goroutine 数上（runProbeRetryBurst 内），首探串行 for 循环不受此限（天然=1）。
const accountHealthProbeBurstConcurrencyLimit = 8

// 补探（retry burst）默认节奏：首探失败后不再空等一个完整轮次（默认 15s）才再试，
// 而是在本轮 sweep 内按短间隔连补，把恢复时长从「冷却到期」（cooldown 分钟级）拉回
// 秒级。补探自 AC-C1 起异步执行（独立 goroutine，纳入 Start 的 wg），同账号补探由
// retryInFlight（in-flight）去重，防止重叠。
//
// 补探不设轮数上限（C1 删除探测次数上限）：每个账号探测成功并完成恢复后从 pending
// 移除、失败则保留继续探，循环天然收敛；退出条件只有 pending 空、ctx 取消或 Stop 信号。
const (
	// accountHealthProbeRetryInterval 首探失败后的补探间隔（秒级，AC-C1 不动）。
	accountHealthProbeRetryInterval = 5 * time.Second
	// accountHealthProbeQuotaRetryInterval 是 429/额度耗尽类停调的补探间隔：额度以小时/自然月
	// 为周期，5 秒补探只会无意义轰炸上游与 DB 写锁；拉长到 5 分钟，恢复仍由探针自动拉活
	// （首成功后加速见 runProbeRetryBurst）。
	accountHealthProbeQuotaRetryInterval = 5 * time.Minute
	// accountHealthProbeDataPlaneTimeout 单次数据面探测超时。取 45s：上游正常时
	// 极简探测请求在数秒内返回；故障时快速失败，不必等满客户端链路时长。
	accountHealthProbeDataPlaneTimeout = 45 * time.Second
	// accountHealthProbeDataPlaneMaxTokens 数据面探测的输出上限。
	//
	// 为什么不能取极小值（1、16、32）：思考型模型把输出预算先花在 reasoning_content
	// 上，给多少都能吃满；正文一个字都轮不到时，桥接会判「上游没有产出助手文本」
	// 并回 502 upstream_empty_result（生产实证：qoder-bridge）。
	//
	// 生产实测（账号 411，上游 119.28.137.187:18081，deepseek-v4-pro，prompt="hi"）：
	//   max_tokens=16/24/32 → 502 upstream_empty_result
	//   max_tokens=48/64/96/128/256 → 200，正文正常，reasoning_content 127~287 字符
	// 即阈值落在 32~48 之间，且思考长度随采样波动。取 256 留约 3 倍余量，单次探测
	// 实际只产出百余 token，成本可忽略；同时远低于 45s 探测超时（实测 2~3s 返回）。
	//
	// 历史：曾从 1 提到 16，方向对但仍不够——账号 406 因此连续探测 1.6 万余次全失败，
	// 熔断后永远恢复不了，而同地址 /v1/models 秒回 200、同 key 的业务请求完全正常。
	accountHealthProbeDataPlaneMaxTokens = 256
	// accountHealthProbeDataPlaneMaxBodyBytes 探测响应读取上限，防止异常大响应。
	accountHealthProbeDataPlaneMaxBodyBytes = 256 << 10
	// probeRecoverySuccessThreshold 连续多少次真实探测成功才解除熔断。
	// 取 2：生产数据（14 天/8 账号/7177 次失败）显示熔断后连续 1 次成功的中位耗时
	// 仅 0.8 分钟，无法区分"真恢复"与"偶然一次成功"；连续 2 次中位 1.9 分钟，
	// 既能挡住抖动又不过度拖延恢复。
	probeRecoverySuccessThreshold = 2
)

// probeRecoverableMarkers 列出可被探测恢复的停调标记词（TempUnschedState.MatchedKeyword）：
//   - openai_apikey_health_breaker：APIKey 健康熔断 L3 停调（CodeBuddy 影子准入后
//     同样携带该标记，方案 T1）；
//   - pool_mode_401_escalation：池模式 401 窗口升级停调（方案 T5，常量单一定义于
//     pool_401_escalation.go，此处禁止字面量重复）。
var probeRecoverableMarkers = []string{
	openAIAPIKeyHealthBreakerReason,
	PoolMode401EscalationMarker,
	oauth401RecoverableMarker,
	// cursor Connect 会话型 / OpenAI 兼容型账号的探针恢复 marker（CURSOR-PROBE-001-A1）。
	// 单加一项，全平台共用的 parseHealthBreakerState / isHealthBreakerTrip 一行不改。
	cursorAccountHealthBreakerMarker,
	// Devin 上游额度耗尽停调的探针恢复标记，额度恢复后探测成功可提前解除停调。
	devinUpstreamQuotaExhaustedMarker,
}

// accountHealthProbeRepository is the narrow repository surface the recovery probe
// needs. It is intentionally a subset of AccountRepository so the real repository
// satisfies it without forcing every AccountRepository test double to implement the
// probe-specific methods.
type accountHealthProbeRepository interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	ListTempUnschedulableAccounts(ctx context.Context, now time.Time, limit int) ([]*Account, error)
	// ClearTempUnschedulableIfGeneration 是仓储层条件清除原语（AC-G2R5 must_fix / AC-G2R6 子秒
	// 精度）：用「截止时间」做不可变代际令牌。仅当该账号当前 temp_unschedulable_until 仍等于
	// expectedUntil（探测开始/入队时读到的代际令牌，timestamptz 全精度等值）时才清除停调并返回
	// true；若 until 已被他人改写（新停调接管，必写更晚 until）则零写入、返回 false，探测方据此
	// 保留新停调、不清除。仓储层单语句原子保证「读代际→清停调」不被并发覆盖
	// （见 account_repo.go ClearTempUnschedulableIfGeneration）。
	ClearTempUnschedulableIfGeneration(ctx context.Context, accountID int64, expectedUntil time.Time) (bool, error)
	// BumpTempUnschedulableReasonIfGeneration 是 attempt 回写的代际守卫原语（AC-G2R6 must_fix）：
	// 仅当当前 until 仍等于 expectedUntil 且仍处有效停调窗口时才改写 reason 并返回 true；否则
	// 返回 false（已被接管），调用方据此终止本轮补探、保留接管者 reason。
	BumpTempUnschedulableReasonIfGeneration(ctx context.Context, accountID int64, expectedUntil time.Time, newReason string) (bool, error)

	// —— 批1 通用恢复通道（账号级停调恢复闭环）——
	ListProbeDueAccounts(ctx context.Context, now time.Time, limit int) ([]*Account, error)
	ClearRecoveryProbeComponent(ctx context.Context, accountID int64, component string, expectedGen int64) (bool, error)
	RescheduleRecoveryProbeComponent(ctx context.Context, accountID int64, component string, expectedGen int64, newDue time.Time) (bool, error)
}

// AccountHealthRecoveryProbeService implements the optional Phase C probe-based
// recovery. While an account is inside the health-breaker cooldown, it periodically
// sends a minimal real data-plane request (the same protocol/model/proxy/TLS path the
// forwarding chain uses) and requires probeRecoverySuccessThreshold consecutive
// successes before clearing the temp-unschedulable state early. A failing probe keeps
// the account parked until the cooldown expires; probing itself has no attempt ceiling.
//
// The probe is opt-in: Start() is a no-op unless the admin explicitly enables
// settings.probe.enabled. SSRF protection is enforced via cnValidateProbeURL before
// any request is built, reusing the same gate as the gateway's own outbound calls.
//
// Multi-instance note: each backend replica runs its own independent sweep, so the
// same temp-unschedulable account may be probed by several instances at once. The
// probe is read-only against upstream and its effects (ClearTempUnschedulable on
// success, or an incremented probe_attempts on failure) are idempotent, so the
// duplication is harmless beyond a little extra upstream traffic.
type AccountHealthRecoveryProbeService struct {
	accountRepo         accountHealthProbeRepository
	httpUpstream        HTTPUpstream
	cfg                 *config.Config
	rateLimit           *RateLimitService
	settingService      *SettingService
	tlsFPProfileService *TLSFingerprintProfileService

	startMu sync.Mutex
	stopCh  chan struct{}
	// stopping 是单停权标志（受 startMu 保护）：Stop 在开始关闭路径前于锁内置 true，
	// 声明独占关闭权并独占 wg.Wait；期间任何 Start 在锁内被 stopping 挡住，绝不执行
	// wg.Add，从而彻底消除「Stop 的 wg.Wait 与 Start 的 wg.Add 交错」（sync.WaitGroup
	// 禁止在 Wait 仍挂起时并发 Add）。Stop 完成 wg.Wait 后在锁内清 stopping，恢复 Start 可用。
	stopping bool
	wg       sync.WaitGroup

	// 批 2：模型级恢复通道的进程内单轮保护（分布式去重由 leader/账号锁负责）。
	modelProbeRunning atomic.Bool

	// probeOverride, when set, replaces the real upstream probe (used by tests).
	probeOverride func(ctx context.Context, account *Account) (ok bool, err error)

	// retryInterval 覆盖补探间隔（仅测试注入，避免单测真实等待
	// accountHealthProbeRetryInterval）；零值经 probeRetryInterval 回落到默认常量。
	retryInterval time.Duration

	// frozenMu / frozenBounds 保存每个 (account,scope) 进入调度时冻结的候选总数
	//（C，上限公式）。冻结值经 RateLimitService.freshnessBoundsProvider 供三维新鲜度
	// 阈值公式消费（account_freshness_threshold.go / account_freshness_alert.go），
	// 写入语义归 freshness 链所有，不随 D2 探针相位退役（方案 th-kira-quota-lifecycle §7）。
	frozenMu     sync.Mutex
	frozenBounds map[string]int

	// freshnessAlerts 是 D4 陈旧告警服务（可选注入）。
	freshnessAlerts *FreshnessAlertService
	// channelFreshness 是 E39 渠道维陈旧收敛窄面（可选注入）。账号恢复成功后触发关联
	// 渠道陈旧收敛；未注入时不收敛。
	channelFreshness ChannelFreshnessRefresher

	// —— 批1 通用恢复通道依赖（可选注入；未注入时对应探测记 uncertain，失败关闭）——
	// recoveryLock 是 Redis-only 的跨副本锁（leader + 账号级）。为 nil 时新通道整轮
	// 不探测；既有 marker 路径保持原行为（仅在其非 nil 时加锁）。
	recoveryLock      LeaderLockCache
	balanceProbe      *AccountBalanceProbeService
	antigravityQuota  *AntigravityQuotaFetcher
	antigravityTokens *AntigravityTokenProvider
	cursorEntry       *CursorAutoEntryService

	// retryInFlightMu / retryInFlight 是补探账号级 in-flight 去重集合（AC-C1 异步化后
	// 必需）：同一账号的补探正在异步执行时，新一轮 sweep 跳过该账号的本轮补探，避免
	// 同账号补探重叠。复用既有 wg 模式，不引入新 goroutine 管理机制。
	retryInFlightMu sync.Mutex
	retryInFlight   map[int64]struct{}

	// cursorExec 是 cursor Connect 会话型账号的原生 AgentRun 探针执行部件（CURSOR-PROBE-001-A1
	// 在 cursor_account_health_probe.go 提供 ProbeCursorAccountHealth 封装，本卡的 A5 卡在
	// wire 装配注入 *CursorExecutionService）。未注入时 Connect 分支安全降级返回 error（不伪造失败）。
	cursorExec *CursorExecutionService

	// cursorAgentRunProbe 是 cursor Connect 会话型探针的实际执行函数，默认走 A1 的
	// ProbeCursorAccountHealth（原生 AgentRun）。可被子测试替换以断言分派并跳过真实出站请求。
	cursorAgentRunProbe func(ctx context.Context, exec *CursorExecutionService, account *Account, upstreamModel string, budget int) (int, string, string, error)
}

// SetCursorExecutionService 注入 cursor Connect 会话型探针执行部件（A5 卡在 wire 装配；
// 本卡只提供字段与注入入口，未注入时 Connect 分支安全降级）。未注入时不改变其它平台行为。
func (p *AccountHealthRecoveryProbeService) SetCursorExecutionService(exec *CursorExecutionService) {
	if p != nil {
		p.cursorExec = exec
	}
}

// SetCursorAgentRunProbe 注入 cursor Connect 会话型探针执行函数（仅供子测试替换，断言分派）。
func (p *AccountHealthRecoveryProbeService) SetCursorAgentRunProbe(fn func(ctx context.Context, exec *CursorExecutionService, account *Account, upstreamModel string, budget int) (int, string, string, error)) {
	if p != nil {
		p.cursorAgentRunProbe = fn
	}
}

// SetChannelFreshnessRefresher 注入 E39 渠道维陈旧收敛窄面（可选）。未注入时不收敛。
func (p *AccountHealthRecoveryProbeService) SetChannelFreshnessRefresher(r ChannelFreshnessRefresher) {
	if p != nil {
		p.channelFreshness = r
	}
}

// SetFreshnessAlertService 注入 D4 陈旧告警服务（可选）。未注入时不做陈旧评估。
// SetRecoveryProbeDependencies 注入批1 通用恢复通道所需的锁与额度探测依赖。
// 全部可选：未注入时对应探测返回 uncertain（保持停调），不会误判恢复。
func (p *AccountHealthRecoveryProbeService) SetRecoveryProbeDependencies(
	lock LeaderLockCache,
	balance *AccountBalanceProbeService,
	antigravityQuota *AntigravityQuotaFetcher,
	antigravityTokens *AntigravityTokenProvider,
	cursor *CursorAutoEntryService,
) {
	if p == nil {
		return
	}
	p.recoveryLock = lock
	p.balanceProbe = balance
	p.antigravityQuota = antigravityQuota
	p.antigravityTokens = antigravityTokens
	p.cursorEntry = cursor

	// 批4：统一 tick 注册表复用同一把 Redis 锁（跨副本 leader），六源单账号探测点
	// 复用同一把账号锁（recovery:probe:<id>）。lock 为 nil 时注册表失败关闭、账号锁
	// 退回无锁执行（与批 1 单实例语义一致）。
	SetRecoveryAccountLockCache(lock)

	// 账号锁 TTL 按本轮装配的实际预算推导：默认 330s 覆盖 cursor 额度查询 225s；
	// Web Console 探测预算为 TimeoutSeconds + 10s（HTTP grace），再加 30s 余量；
	// Devin marker sweep 串行约 20 模型 × ~20s ≈ 400s，同样加 30s 余量。
	// 取三者较大值，避免配置放大预算时锁提前过期导致同账号临界区重叠。
	if p.cfg != nil {
		webBudget := time.Duration(p.cfg.WebQuotaWorker.TimeoutSeconds)*time.Second + 10*time.Second
		ttl := 330 * time.Second
		if margin := webBudget + 30*time.Second; margin > ttl {
			ttl = margin
		}
		if margin := accountRecoveryDevinMarkerBudget + 30*time.Second; margin > ttl {
			ttl = margin
		}
		SetRecoveryAccountLockTTL(ttl)
	}
}

func (p *AccountHealthRecoveryProbeService) SetFreshnessAlertService(svc *FreshnessAlertService) {
	if p != nil {
		p.freshnessAlerts = svc
	}
}

// NewAccountHealthRecoveryProbeService constructs the recovery probe service.
func NewAccountHealthRecoveryProbeService(
	accountRepo accountHealthProbeRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	rateLimit *RateLimitService,
	settingService *SettingService,
	tlsFPProfileService *TLSFingerprintProfileService,
) *AccountHealthRecoveryProbeService {
	return &AccountHealthRecoveryProbeService{
		accountRepo:         accountRepo,
		httpUpstream:        httpUpstream,
		cfg:                 cfg,
		rateLimit:           rateLimit,
		settingService:      settingService,
		tlsFPProfileService: tlsFPProfileService,
		frozenBounds:        make(map[string]int),
		retryInFlight:       make(map[int64]struct{}),
	}
}

// Start 把健康探测登记为统一 tick 注册表的两个周期任务（marker sweep 与通用恢复通道），
// 由注册表唯一的自持 ticker 按配置的 interval 驱动；本服务不再自持 ticker。RunOnce 每轮
// 重读探测开关（见其 per-class enable 检查），故后台切换开关在一个 interval 内生效，无需
// 重启进程。重复调用安全（注册表按任务名幂等、仅首次有效启动），通过 Stop 终止。
func (p *AccountHealthRecoveryProbeService) Start(ctx context.Context) {
	// settingService 为 nil 时失败关闭安全返回（不启动探测循环），与旧 probeEnabled
	// 的 `p == nil || p.settingService == nil → return` 同款：重构使能门禁时该保护被
	// 移除，若不在此短路，下方解引用 nil 会 panic（第三轮终审 #7 回归）。
	if p == nil || p.settingService == nil {
		return
	}
	p.startMu.Lock()
	defer p.startMu.Unlock()
	// 停止期间（stopping 独占关闭权）或已在运行（stopCh 非空）一律拒绝重启：保证
	// wg.Add 只在锁内 stopCh==nil 且 !stopping 时发生，永不与 Stop 的 wg.Wait 交错。
	if p.stopCh != nil || p.stopping {
		return
	}

	// 探测 sweep 间隔：默认 15s（AC-C1：旧 60s），settings.Probe.IntervalSeconds
	// 经 normalize 已 clamp 到 [15,600]；>= 15 时采用配置值（外审实证：clamp 与
	// 默认值都改、此处不改仍跑 60s）。读取失败或 Probe 未配置时回落默认 15s。
	settings, serr := p.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	interval := p.probeIntervalFromSettings(settings, serr)

	p.stopCh = make(chan struct{})

	// 批4 B 卡：健康探测不再自持 ticker，改为向统一 tick 注册表登记两个周期任务，
	// 由注册表唯一的自持 ticker 驱动。两个任务自身已持有内部租约/预算。
	reg := DefaultBackgroundTickRegistry()
	reg.Register(backgroundTickTask{
		name:     "health_marker_sweep",
		cadence:  interval,
		maxRun:   accountHealthProbeDataPlaneTimeout,
		firstRun: tickFirstRunAfterCadence,
		run: func(c context.Context) error {
			p.RunOnce(c)
			return nil
		},
	})
	reg.Register(backgroundTickTask{
		name:     "health_recovery_channel",
		cadence:  interval,
		maxRun:   accountRecoveryRoundBudget,
		firstRun: tickFirstRunAfterCadence,
		run: func(c context.Context) error {
			p.RunRecoveryChannel(c)
			return nil
		},
	})
	reg.Start(ctx)
	logger.L().Info("openai.apikey_health_probe_started",
		zap.Duration("interval", interval))

	// 批2：模型级限流恢复通道登记到同一注册表（独立 cadence 15s）；进程内 atomic
	// 防轮次叠加，跨副本去重由通道内 leader/账号锁负责。
	reg.Register(backgroundTickTask{
		name:     "model_rate_limit_recovery_channel",
		cadence:  15 * time.Second,
		maxRun:   modelRateLimitProbeRoundBudget,
		firstRun: tickFirstRunAfterCadence,
		run: func(c context.Context) error {
			if !p.modelProbeRunning.CompareAndSwap(false, true) {
				return nil
			}
			defer p.modelProbeRunning.Store(false)
			p.RunModelRateLimitRecoveryChannel(c)
			return nil
		},
	})

}

// Stop terminates the probe loop if running. It must be called from the process
// graceful-shutdown path so the goroutine does not leak.
//
// 重入竞态修复（AC-G2R4 第 5 轮 must_fix）：Stop 在开始关闭路径前于锁内置 stopping=true
// 并取走 stopCh（置 nil 声明关闭权），锁外 close + wg.Wait；期间任何并发 Start 在锁内被
// stopping 挡住，绝不执行 wg.Add，从而彻底消除「Stop 的 wg.Wait 与 Start 的 wg.Wait 交错
// 并发 Add」这一 WaitGroup 误用。第二个并发 Stop 在锁内读到 stopping 已置位即直接返回，不再
// 重复关闭。wg.Wait 完成后在锁内清 stopping，恢复 Start 可用。
func (p *AccountHealthRecoveryProbeService) Stop() {
	if p == nil {
		return
	}
	p.startMu.Lock()
	if p.stopping {
		// 第二个并发 Stop：首个 Stop 已独占关闭权，本调用直接返回（不再重复 close/等待）。
		p.startMu.Unlock()
		return
	}
	p.stopping = true
	stopCh := p.stopCh
	// 取走 stopCh：声明关闭权（与 AC-G2R-3 同型），并发的第二个 Stop 因 stopping 已置位
	// 不会再进入本分支。主循环与补探 goroutine 均持有各自启动时捕获的 stopCh 引用，
	// 不受此处置 nil 影响，仍能由下方 close 唤醒。
	p.stopCh = nil
	p.startMu.Unlock()
	// 批4 B 卡 / B4V2-N R3 must_fix：先停掉统一 tick 注册表的自持 ticker（Stop 立即
	// 取消驱动 ctx 并等待 tick goroutine 退出，返回后不再有新的自动派发），再摘除本
	// 服务的两个任务（Unregister 取消任务 ctx 并有界排空在途 registry run）。三者均
	// 幂等。必须在 close(stopCh)/wg.Wait 之前完成：否则 stopCh 关闭与 wg.Wait 期间
	// registry 仍按 15s cadence 派发已到期健康任务（其 ctx 与 stopCh 无关），借
	// p.RunOnce/RunRecoveryChannel 触发新补探；补探信号须待派发窗口关闭、在途 run
	// 排空后才可安全关闭。
	reg := DefaultBackgroundTickRegistry()
	reg.Stop()
	reg.Unregister("health_marker_sweep")
	reg.Unregister("health_recovery_channel")
	// 批2：模型级恢复通道同样关闭派发窗口。
	reg.Unregister("model_rate_limit_recovery_channel")
	if stopCh != nil {
		// 拿到关闭权的调用在锁外 close（不持锁关闭），随后等待 goroutine 退出。
		close(stopCh)
		p.wg.Wait()
	}
	// 优雅关闭完成：清独占标志，恢复 Start 可用（stopCh 已为 nil，故 Start 可在下次调用重建）。
	p.startMu.Lock()
	p.stopping = false
	p.startMu.Unlock()
}

// RunOnce performs a single probe sweep over the currently cooldowned accounts.
//
// 旧「按候选类拆分使能门禁」中 TokenHarbor 免费档模型级相位（D2）已随
// th-kira-quota-lifecycle 方案 §7 旧链退役：该相位以裸模型名 ping 上游烧毁订阅额度，
// 其语义由 cn_quota_lifecycle_service.go 的额度耗尽状态机确认探针收编（429 分类
// isTokenHarborFreeTierExhausted 归状态机所有）。剩余唯一相位为账号级
// （temp-unschedulable/熔断）探测，维持双开关 ProbeEnabledForCandidate(
// ProbeCandidateCircuitBreaker) 门禁。settings 读取失败维持现状 err != nil → return。
func (p *AccountHealthRecoveryProbeService) RunOnce(ctx context.Context) {
	// settingService 为 nil 时与 p == nil / accountRepo == nil 同款失败关闭：重构使能门禁时
	// 移除了旧 probeEnabled 对 nil settingService 的保护，此处必须显式短路，否则下一行
	// 解引用 nil 会 panic（第三轮终审 #7 回归）。
	//
	// 其余依赖字段核查：httpUpstream/cfg/rateLimit/tlsFPProfileService/freshnessAlerts/
	// accountRepo 均已在各自解引用点做 nil 保护；frozenBounds 由
	// NewAccountHealthRecoveryProbeService 构造时初始化，生产构造点唯一（ProvideAccount...
	// 经构造器），既有测试引用处亦均显式赋值，故不再重复加守卫。
	if p == nil || p.accountRepo == nil || p.settingService == nil {
		return
	}
	settings, err := p.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	if err != nil || settings == nil || settings.Probe == nil {
		return
	}

	now := time.Now()

	// 账号级（temp-unschedulable/熔断）相位：维持双开关门禁。
	// 方案 §7 旧链退役后相位列表仅此一项；D2 TokenHarbor 模型级相位已删除，
	// 未来新增相位须在此追加独立分支并保持各自使能门禁（显式空处理：无相位时
	// 循环体不执行、孤儿告警清扫照常进行，见下方 freshnessAlerts 分支）。
	if ProbeEnabledForCandidate(settings, ProbeCandidateCircuitBreaker) {
		candidates, lerr := p.accountRepo.ListTempUnschedulableAccounts(ctx, now, accountHealthProbeCandidateLimit)
		if lerr != nil {
			logger.L().Warn("openai.apikey_health_probe_list_failed", zap.Error(lerr))
			return
		}
		// 首探失败的账号：主循环结束后按短间隔补探（不再空等一个完整轮次）。
		pendingRetry := make([]*Account, 0, len(candidates))
		// pendingUntils 记录每个入队补探账号在「入队时」读到的 temp_unschedulable_until
		// （完整 time.Time，全精度），作为后续条件清除/attempt 回写的不可变代际令牌
		// （AC-G2R5 must_fix / AC-G2R6 子秒精度）：用 until 作令牌后，同一次停调内
		// 状态回写只改 reason 不改 until，补探成功时令牌恒命中；他因接管（写更晚
		// until）必令令牌失配、保留新停调。补探 goroutine 每次回读的是最新 until，若探测期间
		// until 被他人改写，用最新 until 作令牌会误清新停调；故条件清除/attempt 回写一律用
		// 入队时读到的令牌。
		pendingUntils := make(map[int64]time.Time)
		retryInterval := p.probeRetryInterval()
		owner := newRecoveryLockOwner()
		for _, acc := range candidates {
			if !p.isHealthBreakerTrip(acc, now) {
				continue
			}
			// 补探互斥（AC-G2R-2）：同账号在途（含补探中）时本轮 sweep 完全跳过该账号：
			// 不首探、不入队补探。先查 in-flight（补探 burst 仍在异步执行中），命中即
			// 跳过（含冻结上界也不写入，避免误缩短尾部候选阈值）。该检查须在首探之前，
			// 否则上一轮补探仍在进行时本轮又对该账号做首探，造成同账号探测重叠。
			if p.isRetryInFlight(acc.ID) {
				logger.L().Info("openai.apikey_health_probe_retry_inflight_skip_sweep",
					zap.Int64("account_id", acc.ID),
				)
				continue
			}
			// 冻结账号级候选总数（freshness 链既有机制，D-QL-007 F1 恢复：删除 D2
			// 相位时该写入者被一并删除，但 EvaluateAccountLevelFreshness 仍经
			// AccountFreshnessUpperBound(accountID, "") 读 frozenBounds——缺写入者则
			// 账号级上界恒 0、阈值退化为基线）。候选总数=1：账号级相位每账号每轮
			// 至多一个候选，与预算管理器删除后的现状语义一致；旧 D2 按 scope 数+
			// 账号级计数冻结的公平旋转口径已随相位退役。冻结值生命周期由
			// clearCandidateBound（probeAccount 恢复路径 :365）负责清除，"首冻结
			// 不覆盖"语义保持。
			unlock, lockOK, lockErr := p.acquireAccountProbeLock(ctx, acc.ID, owner)
			if lockErr != nil {
				logger.L().Warn("account_probe_lock_unavailable", zap.Int64("account_id", acc.ID), zap.Error(lockErr))
			}
			if !lockOK {
				continue
			}
			p.freezeCandidateBound(acc.ID, "", 1)
			stillParked := p.probeAccount(ctx, acc)
			unlock()
			if stillParked {
				// 账号级 in-flight 去重（AC-C1 异步化后必需）：首探失败后才标记该账号
				// 补探在途并入队；markRetryInFlight 返回 false（理论上方已先查
				// isRetryInFlight，此处不会发生）仅作防御性跳过，避免同账号补探重叠。
				if p.markRetryInFlight(acc.ID) {
					pendingRetry = append(pendingRetry, acc)
					pendingUntils[acc.ID] = tempUnschedUntil(acc)
					logger.L().Info("openai.apikey_health_probe_retry_scheduled",
						zap.Int64("account_id", acc.ID),
						zap.Duration("retry_interval", retryInterval),
					)
				} else {
					logger.L().Info("openai.apikey_health_probe_retry_inflight_skip",
						zap.Int64("account_id", acc.ID),
					)
				}
			}
		}
		// 补探相位：先让所有候选都拿到一次首探（公平），再对失败者异步连补
		//（AC-C1 异步化：独立 goroutine，纳入 wg；Stop 等待语义不变；不阻塞本 sweep）。
		if len(pendingRetry) > 0 {
			// 补探 spawn 收进锁段（AC-G2R5 must_fix2）：读 stopCh、判 stopping、wg.Add、
			// 与「清理本轮已标 in-flight 标记」全部在 startMu 锁内完成——彻底消除「锁外
			// wg.Add 与 Stop 的 wg.Wait 交错」（sync.WaitGroup 禁止 Wait 仍挂起时并发 Add）。
			// 锁内若发现 stopCh 已取走（Stop 已声明关闭权）或 stopping=true（Stop 正在关闭），
			// 则绝不入队补探，并清掉本轮已 mark 的 in-flight 标记后直接跳过（runProbeRetryBurst
			// 的 defer 清理未启动故不会执行，遗漏则后续 sweep 凭 in-flight 标记永久跳过该账号）；
			// 否则捕获 stopCh 引用、锁内 wg.Add，再解锁启动 goroutine——goroutine 持有锁外独立
			// 引用，不受本锁/Stop 影响（AC-G2R-3 同型：避免 Stop 置 nil 后 close 失灵、wg.Wait
			// 挂死）。
			p.startMu.Lock()
			stopCh := p.stopCh
			if p.stopping {
				// Stop 进行中（stopping=true，Stop 已在锁内置独占关闭权并即将/正在
				// wg.Wait）：不入队补探、清掉本轮已 mark 的 in-flight 标记后直接跳过，
				// 杜绝「锁外 wg.Add 与 Stop 的 wg.Wait 交错」（sync.WaitGroup 禁止 Wait
				// 仍挂起时并发 Add）。stopCh 是否为 nil 不影响本判定——guard 只看 stopping，
				// 故未调用 Start（stopCh 为 nil）的直驱 RunOnce 路径仍可正常补探。
				p.startMu.Unlock()
				for _, acc := range pendingRetry {
					p.clearRetryInFlight(acc.ID)
				}
			} else {
				p.wg.Add(1)
				p.startMu.Unlock()
				go func(stopCh chan struct{}) {
					defer p.wg.Done()
					p.runProbeRetryBurst(ctx, pendingRetry, pendingUntils, stopCh)
				}(stopCh)
			}
		}
	}

	// E45：孤儿告警清扫（候选过期转非活动的账号+模型维）。以 firing 新鲜度告警为界反向核对
	// 限流条目解析态，关闭再无评估/关闭路径的孤儿告警。错误只记日志，不阻断探测主路径
	// （与 evaluateAccountFreshness 调用方同口径）。
	if p.freshnessAlerts != nil {
		if sweepErr := p.freshnessAlerts.CloseOrphanedFreshnessAlerts(ctx); sweepErr != nil {
			logger.L().Warn("freshness_orphan_sweep_failed", zap.Error(sweepErr))
		}
	}
}

// isHealthBreakerTrip reports whether the account's active temp-unschedulable block
// is probe-recoverable (opened by the health breaker or by the pool-mode 401
// escalation, 方案 T5 marker 放行) and has not yet expired.
func (p *AccountHealthRecoveryProbeService) isHealthBreakerTrip(acc *Account, now time.Time) bool {
	if acc == nil || acc.TempUnschedulableReason == "" {
		return false
	}
	if acc.TempUnschedulableUntil == nil || !acc.TempUnschedulableUntil.After(now) {
		// Devin 额度耗尽属「探针驱动恢复」的持续停调：until 到期不解除扫描资格，
		// 唯一解除路径是探针连续成功清除停调（marker 随之消失）。
		if !acc.IsParkedPendingDevinQuotaRecovery() {
			return false
		}
	}
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return false
	}
	for _, marker := range probeRecoverableMarkers {
		if state.MatchedKeyword == marker {
			return true
		}
	}
	return false
}

// parseHealthBreakerState parses the JSON reason payload written by the breaker.
func parseHealthBreakerState(reason string) (*TempUnschedState, bool) {
	if strings.TrimSpace(reason) == "" {
		return nil, false
	}
	var state TempUnschedState
	if err := json.Unmarshal([]byte(reason), &state); err != nil {
		return nil, false
	}
	return &state, true
}

// probeAccount 探测一次并尝试计入恢复；代际令牌取探测开始时读到的 until
// （acc.TempUnschedulableUntil，全精度 time.Time）。补探异步路径需传入其入队时读到的
// until，故走 probeAccountWithReason。
//
// 返回值 stillParked 报告本次尝试后账号是否仍处于停调、值得短间隔继续补探：真实探测
// 成功但连续成功次数未达 probeRecoverySuccessThreshold（计数已回写、账号保持停调）、
// 探测失败或降级时返回 true；完成恢复提交或代际被接管时返回 false。
func (p *AccountHealthRecoveryProbeService) probeAccount(ctx context.Context, acc *Account) (stillParked bool) {
	return p.probeAccountWithReason(ctx, acc, tempUnschedUntil(acc))
}

// tempUnschedUntil 返回账号当前 temp_unschedulable_until 的完整 time.Time（代际令牌，
// 全精度）。until 为 nil（不应出现在已入队候选上）时回落零值 time.Time——零值与「until IS
// NOT NULL」守卫组合，保证失配、不误清（见 ClearTempUnschedulableIfGeneration）。
func tempUnschedUntil(acc *Account) time.Time {
	if acc == nil || acc.TempUnschedulableUntil == nil {
		return time.Time{}
	}
	return *acc.TempUnschedulableUntil
}

// probeAccountWithReason 与 probeAccount 同语义，但代际令牌 expectedUntil 由调用方显式
// 传入（完整 time.Time，全精度）。补探异步路径（runProbeRetryBurst）传账号入队时读到的 until，
// 而非每次回读的最新 until——若探测期间 until 被他人改写（新停调接管，必写更晚 until），回读的
// 最新 until 已不同，用其作令牌会误清新停调；用入队令牌则条件清除命中失配、rows==0、保留新停调
// （见 ClearTempUnschedulableIfGeneration）。
//
// 连续成功判定（C1）：单次真实探测成功不足以解除熔断——生产数据里熔断后「连续 1 次成功」的
// 中位耗时仅 0.8 分钟，无法区分真恢复与偶然抖动。成功一次即把 ConsecutiveSuccesses 计数回写
// （账号保持停调），达到 probeRecoverySuccessThreshold 才走条件清除恢复；任一失败/降级把计数
// 归零。计数落 TempUnschedState.ConsecutiveSuccesses，随停调状态一起在重启后存活。
func (p *AccountHealthRecoveryProbeService) probeAccountWithReason(ctx context.Context, acc *Account, expectedUntil time.Time) (stillParked bool) {
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return false
	}

	ok, err := p.runProbe(ctx, acc)
	// F：账号级候选观察时间（两维度，同一状态入口）。既有的 ClearTempUnschedulable
	// 恢复路径保持不变，这里仅追加记录账号级 observed_at / attempted_at。
	// #2：观测提交失败透传（与 E19 口径一致：提交确认成功才允许清理状态）。
	obsErr := p.recordAccountLevelObservation(ctx, acc.ID, ok, err)
	if err != nil {
		// 降级（无可构造的安全探测面）：不做恢复判定，只簿记后保持停调，等冷却到期。
		logger.L().Warn("openai.apikey_health_probe_degraded",
			zap.Int64("account_id", acc.ID),
			zap.Error(err),
		)
		// E49：探测错误分支下，账号级权威观测未写入同样需结构化暴露
		//（与 err==nil 路径的 openai.apikey_health_probe_commit_failed 同口径），
		// 否则该观测提交失败不可观测。
		if obsErr != nil {
			logger.L().Warn("openai.apikey_health_probe_commit_failed",
				zap.Int64("account_id", acc.ID),
				zap.Error(obsErr),
			)
		}
		// 降级不等于恢复：连续成功计数归零后回写，账号保持停调等冷却到期。
		state.ProbeAttempts++
		state.ConsecutiveSuccesses = 0
		// 回写加代际守卫（AC-G2R6 must_fix）：用入队令牌回写，若代际已被接管
		//（rows==0）则终止本轮补探（它在清的停调已不属于本代），记 Info。
		if !p.persistProbeState(ctx, acc.ID, expectedUntil, state) {
			logger.L().Info("probe attempt bump skipped: temp-unsched generation taken over (degraded)",
				zap.Int64("account_id", acc.ID),
				zap.Time("expected_until", expectedUntil),
			)
			return false
		}
		return true
	}

	// #2：非 success outcome（探测失败/降级）且观测写错误 → Warn 后继续既有失败簿记
	//（attempted_at 丢失可容忍，状态无迁移，账号仍 parked）。
	if !ok && obsErr != nil {
		logger.L().Warn("openai.apikey_health_probe_commit_failed",
			zap.Int64("account_id", acc.ID),
			zap.Error(obsErr))
	}

	if ok {
		// #2：成功 outcome 但观测提交失败 → 失败关闭（E19 同口径）：保持 parked，
		// 不 ClearTempUnschedulable、不 clearCandidateBound、不计成功，账号仍
		// temp-unschedulable 直到下次探测成功提交——否则状态分裂（观测未提交而恢复已发生）。
		if obsErr != nil {
			logger.L().Warn("openai.apikey_health_probe_commit_failed",
				zap.Int64("account_id", acc.ID),
				zap.Error(obsErr))
			return true
		}
		state.ConsecutiveSuccesses++
		if state.ConsecutiveSuccesses < probeRecoverySuccessThreshold {
			// 未达阈值：回写计数、账号保持停调，交由补探继续累计连续成功次数。
			state.ProbeAttempts++
			if !p.persistProbeState(ctx, acc.ID, expectedUntil, state) {
				logger.L().Info("probe success count skipped: temp-unsched generation taken over",
					zap.Int64("account_id", acc.ID),
					zap.Time("expected_until", expectedUntil),
				)
				return false
			}
			logger.L().Info("openai.apikey_health_probe_success_pending",
				zap.Int64("account_id", acc.ID),
				zap.Int("consecutive_successes", state.ConsecutiveSuccesses),
				zap.Int("threshold", probeRecoverySuccessThreshold),
			)
			return true
		}
		// AC-G2R4 第 5 轮 must_fix / AC-G2R6 子秒精度：清除所有权——探测成功提交恢复时
		// 改条件清除。用探测开始/入队时读到的 until（全精度 time.Time）作为代际令牌，调用
		// 仓储层原子条件清除：仅当当前 until 仍等于令牌时才清停调（rows>0）；若 until 已被
		// 他人改写（新停调接管，rows==0）则不清除、记 Info 日志，账号保持新停调，避免错清
		// 他链刚写入的状态。
		if p.accountRepo != nil {
			cleared, clearErr := p.accountRepo.ClearTempUnschedulableIfGeneration(ctx, acc.ID, expectedUntil)
			if clearErr != nil {
				logger.L().Warn("openai.apikey_health_probe_clear_failed",
					zap.Int64("account_id", acc.ID),
					zap.Error(clearErr),
				)
				return
			}
			if !cleared {
				logger.L().Info("probe skipped clear: temp-unsched generation taken over",
					zap.Int64("account_id", acc.ID),
					zap.Time("expected_until", expectedUntil),
				)
				return true
			}
			// 池模式 401 升级停调的探测恢复出口（方案 T5 条目 4，R4-F3）：清停调
			// 成功后重置该账号的 401 窗口状态机——否则同一 epoch 内再次 401 不再
			// 跨越边沿，账号失去升级保护。健康熔断 marker 不触碰池窗口。
			if state.MatchedKeyword == PoolMode401EscalationMarker {
				resetPool401State(acc.ID)
			}
			// E39：账号恢复成功后触发关联渠道维陈旧收敛（最佳努力，失败只记日志，
			// 不回滚/不阻断恢复结论；恢复链提交在先，与本调用无关）。
			if p.channelFreshness != nil {
				if ferr := p.channelFreshness.RefreshChannelFreshnessForAccount(ctx, acc.ID); ferr != nil {
					logger.L().Warn("openai.apikey_health_probe_channel_freshness_refresh_failed",
						zap.Int64("account_id", acc.ID),
						zap.Error(ferr),
					)
				}
			}
		}
		// 账号级恢复成功：清除账号级（无模型键）冻结上界，下一轮进入调度重新冻结。
		p.clearCandidateBound(acc.ID, "")
		logger.L().Info("openai.apikey_health_probe_recovered",
			zap.Int64("account_id", acc.ID),
			zap.Int("attempt", state.ProbeAttempts+1),
			zap.Int("consecutive_successes", state.ConsecutiveSuccesses),
		)
		return false
	}

	// Probe failed: 连续成功计数归零 + attempt 计数递增后回写（加代际守卫，AC-G2R6
	// must_fix）。代际失配（已被接管，rows==0）→ 终止本轮补探（它在清的停调已不属于本代），
	// 记 Info 并不再补探该账号。
	state.ProbeAttempts++
	state.ConsecutiveSuccesses = 0
	if !p.persistProbeState(ctx, acc.ID, expectedUntil, state) {
		logger.L().Info("probe attempt bump skipped: temp-unsched generation taken over",
			zap.Int64("account_id", acc.ID),
			zap.Time("expected_until", expectedUntil),
		)
		return false
	}
	logger.L().Info("openai.apikey_health_probe_failed",
		zap.Int64("account_id", acc.ID),
		zap.Int("attempt", state.ProbeAttempts),
	)
	return true
}

// persistProbeState 把更新后的探针状态序列化回写 temp_unschedulable_reason，并保持
// 代际守卫生效（AC-G2R6 must_fix）：仅当 until 仍等于入队令牌且仍处有效停调窗口时才改写
// reason。返回 true 表示回写成功（停调仍属本代），false 表示代际已被他因接管（rows==0，
// 必写更晚 until），调用方据此终止本轮补探、保留接管者 reason。
//
// 它是探针唯一的 reason 写入口：ProbeAttempts（排障用的调用次数记录）与
// ConsecutiveSuccesses（连续成功计数）都经它落库，不再有独立的 attempt 专用写函数。
func (p *AccountHealthRecoveryProbeService) persistProbeState(ctx context.Context, accountID int64, expectedUntil time.Time, state *TempUnschedState) bool {
	if p == nil || p.accountRepo == nil || state == nil {
		return false
	}
	updated, mErr := json.Marshal(state)
	if mErr != nil {
		return false
	}
	persisted, bErr := p.accountRepo.BumpTempUnschedulableReasonIfGeneration(ctx, accountID, expectedUntil, string(updated))
	if bErr != nil {
		// DB 写入失败按 best-effort 处理：不因瞬时 DB 错误误杀补探。
		logger.L().Debug("openai.apikey_health_probe_attempt_update_failed",
			zap.Int64("account_id", accountID), zap.Error(bErr))
		return true
	}
	return persisted
}

// probeRetryInterval 返回补探间隔：未显式注入（零值）时回落到默认常量。
func (p *AccountHealthRecoveryProbeService) probeRetryInterval() time.Duration {
	if p != nil && p.retryInterval > 0 {
		return p.retryInterval
	}
	return accountHealthProbeRetryInterval
}

// isQuotaExhaustedTrip 判定一次可探测停调是否属 429/额度耗尽类（慢类，长间隔补探）。
// 健康熔断仅把 429/500/503/504 计入（openai_apikey_health_breaker.go），401 冷却与
// oauth401/pool401 的 StatusCode=401 均不属慢类，保持秒级现状。
// 除 429 外，Devin 额度耗尽停调（HTTP 200 + trailer error，MatchedKeyword 为
// devinUpstreamQuotaExhaustedMarker）同样归入慢类：其本质是上游额度耗尽，必须走
// 5 分钟额度补探节奏，避免对真实对话探针做 5 秒级高频重试而持续消耗上游额度。
func (p *AccountHealthRecoveryProbeService) isQuotaExhaustedTrip(acc *Account) bool {
	if acc == nil {
		return false
	}
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return false
	}
	if state.StatusCode == http.StatusTooManyRequests {
		return true
	}
	// Devin 用 HTTP 200 + trailer error 表达额度耗尽；该停调同样属慢类，
	// 必须走 5 分钟额度补探节奏，避免对真实对话探针做 5 秒级高频重试。
	return state.MatchedKeyword == devinUpstreamQuotaExhaustedMarker
}

// probeIntervalFromSettings 解析探测 sweep 间隔（AC-C1：旧默认 60s → 15s）。
// 规则：settings.Probe 未配置或读取失败时回落默认 15s；IntervalSeconds >= 15
// （经 normalize 已 clamp 到 [15,600]）时采用配置值，否则回落默认 15s。外审实证
// 三道门禁（clamp 下限、本函数默认、Start 内 >=15 判断）须同步改，只改其一仍跑 60s。
func (p *AccountHealthRecoveryProbeService) probeIntervalFromSettings(settings *OpenAIAPIKeyHealthBreakerSettings, err error) time.Duration {
	const defaultInterval = 15 * time.Second
	if err != nil || settings == nil || settings.Probe == nil {
		return defaultInterval
	}
	if settings.Probe.IntervalSeconds >= 15 {
		return time.Duration(settings.Probe.IntervalSeconds) * time.Second
	}
	return defaultInterval
}

// markRetryInFlight 标记某账号补探 in-flight（AC-C1 异步化后同账号去重）。返回 true
// 表示本次成功入队（该账号此前无在途补探）；返回 false 表示该账号已有补探在途，
// 调用方应跳过本轮补探以免重叠。并发安全（retryInFlightMu 保护）。
func (p *AccountHealthRecoveryProbeService) markRetryInFlight(id int64) bool {
	if p == nil {
		return false
	}
	p.retryInFlightMu.Lock()
	defer p.retryInFlightMu.Unlock()
	if p.retryInFlight == nil {
		p.retryInFlight = make(map[int64]struct{})
	}
	if _, ok := p.retryInFlight[id]; ok {
		return false
	}
	p.retryInFlight[id] = struct{}{}
	return true
}

// isRetryInFlight 只读报告某账号是否已有补探在途（AC-G2R-2：同账号去重查询）。与
// markRetryInFlight 配对：sweep 首探前先查本函数，命中则整账号跳过（不首探、不入队
// 补探）；markRetryInFlight 在首探失败后才设置 in-flight 标记。并发安全
// （retryInFlightMu 保护）。
func (p *AccountHealthRecoveryProbeService) isRetryInFlight(id int64) bool {
	if p == nil || p.retryInFlight == nil {
		return false
	}
	p.retryInFlightMu.Lock()
	defer p.retryInFlightMu.Unlock()
	_, ok := p.retryInFlight[id]
	return ok
}

// clearRetryInFlight 清除某账号的补探 in-flight 标记（补探完成/放弃后调用，允许后续
// sweep 重新纳入补探）。并发安全。
func (p *AccountHealthRecoveryProbeService) clearRetryInFlight(id int64) {
	if p == nil {
		return
	}
	p.retryInFlightMu.Lock()
	defer p.retryInFlightMu.Unlock()
	delete(p.retryInFlight, id)
}

// runProbeRetryBurst 对本轮首探失败的账号按短间隔持续补探（AC-C1 起由 RunOnce 在独立
// goroutine 异步调用，纳入 wg；不阻塞主 sweep），直到 pending 清空：某账号完成恢复提交
// （由 probeAccount 内部清除停调）、账号不再处于可探测停调态（已被他实例恢复或冷却到期）、
// 或代际被接管（探测链已不属于本代）。探测次数没有上限（C1 删除 settings.probe.max_attempts
// 上限）——成功靠连续 probeRecoverySuccessThreshold 次真实探测累计，账号恢复后自然退出队列，
// 循环据此收敛；唯一的强退条件为 ctx 取消或 Stop 信号。
//
// 恢复前提不因去掉上限而放松：补探与首探共用同一 probeAccount 路径，仍须真实数据面探测
// 成功且连续成功计数达标才清除停调。退出时清除各账号的补探 in-flight 标记（retryInFlight
// 去重），允许后续 sweep 重新纳入补探。untils 为各账号「入队时」读到的
// temp_unschedulable_until（完整 time.Time，全精度，不可变代际令牌），供条件清除/状态
// 回写使用（见 probeAccountWithReason / ClearTempUnschedulableIfGeneration /
// BumpTempUnschedulableReasonIfGeneration）。
//
// 在途探测并发上限（CURSOR-PROBE-001-A2 §2.5）：每账号一个 goroutine 探测，但同时在途探测数
// 受 accountHealthProbeBurstConcurrencyLimit 约束（进程本地，单实例生效，不约束多实例合计；
// 方案 §5 风险 6）。cursor 探针是真实 AgentRun 出站（预算 ≥256），单轮补探若一次入队数十账号
// （候选上限 200）会瞬间打出数十个真实上游请求 ⇒ 限额有明确必要性（方案 §2.1 / §5 风险 1）。
// 复用既有 wg 模式：每轮用局部 roundWG 等待本轮在途探测，并用计数信号量（buffered channel）
// 限流；不触碰服务级 p.wg（避免与 Stop 的 p.wg.Wait 交错，AC-G2R-3），各账号补探结果经
// results 切片收集、本轮 roundWG.Wait 后由主循环统一回写 nextAt / next，避免 map 并发写竞争。
//
// burstAccountResult 收集单个账号本轮补探的结果，供主循环在 roundWG.Wait 后统一回写。
type burstAccountResult struct {
	stillParked bool
	chosen      time.Duration
}

func (p *AccountHealthRecoveryProbeService) runProbeRetryBurst(ctx context.Context, pending []*Account, untils map[int64]time.Time, stopCh chan struct{}) {
	if p == nil || p.accountRepo == nil || len(pending) == 0 {
		return
	}
	owner := newRecoveryLockOwner()
	// 入队时固定原始账号集合快照，供 defer 清理使用：pending 每轮被重赋值为仅含仍需补探
	// 账号的 next，恢复/回读后不再停调/回读失败的账号退出队列后若仍按 pending 清理则标记
	// 永不清，后续 sweep 凭 in-flight 标记永久跳过该账号、永不再对其探测。改为遍历入队时
	// 固定的原始集合，确保所有被标记 in-flight 的账号最终都能被清理。
	enqueued := make([]*Account, len(pending))
	copy(enqueued, pending)
	// 退出时清除本轮补探账号的 in-flight 标记，避免同账号补探去重误伤后续 sweep。
	defer func() {
		for _, acc := range enqueued {
			p.clearRetryInFlight(acc.ID)
		}
	}()
	// nextAt 维护每个账号的下次探测时间（零值=立即）：429/额度耗尽类失败写入
	// accountHealthProbeQuotaRetryInterval（5min），其余失败与「首成功后加速」写入
	// accountHealthProbeRetryInterval（5s，可被测试注入 retryInterval 覆盖）。
	nextAt := make(map[int64]time.Time, len(pending))
	stay := make([]*Account, 0, len(pending))
	stay = append(stay, pending...)
	// 首轮补探先等一个补探间隔：入队发生在首探刚失败之后，立即重探等于对同一上游
	// 连打两次（生产上多数是同一故障面）。后续轮次按探测结果再细分 5s / 5min。
	if first := time.Now().Add(p.probeRetryInterval()); !first.IsZero() {
		for _, acc := range pending {
			nextAt[acc.ID] = first
		}
	}
	round := 0
	for len(stay) > 0 {
		round++
		// 取仍停留账号中最早的非零 nextAt；全为零值（立即）则不睡，直接探测本轮。
		earliest := time.Time{}
		hasScheduled := false
		for _, acc := range stay {
			if t, ok := nextAt[acc.ID]; ok && !t.IsZero() {
				hasScheduled = true
				if earliest.IsZero() || t.Before(earliest) {
					earliest = t
				}
			}
		}
		if hasScheduled {
			// 睡到最早到期时间（sleep 时长取 time.Until(最早到期)）；ctx 取消或 Stop
			// 触发时 sleepBeforeProbeRetry 立即返回 false，本函数随之强退（不把补探
			// 等待拖进优雅关闭路径）。
			if !p.sleepBeforeProbeRetry(ctx, stopCh, time.Until(earliest)) {
				return
			}
		} else {
			// 本轮全部账号立即探测：仍须尊重 ctx/stop 强退（非阻塞检查，不 sleep）。
			if ctx.Err() != nil {
				return
			}
			select {
			case <-stopCh:
				return
			default:
			}
		}

		now := time.Now()
		next := make([]*Account, 0, len(stay))
		// 在途探测并发上限：每账号一个 goroutine，但同时在途数受
		// accountHealthProbeBurstConcurrencyLimit 约束（进程本地）。复用既有 wg 模式——
		// 用局部 roundWG 等待本轮在途探测、计数信号量 sem 限流；不触碰服务级 p.wg
		//（避免与 Stop 的 p.wg.Wait 交错，AC-G2R-3）。results 收集各账号本轮结果，主循环
		// 在 roundWG.Wait 后统一回写 nextAt / next，避免 map 并发写竞争。
		results := make([]burstAccountResult, len(stay))
		sem := make(chan struct{}, accountHealthProbeBurstConcurrencyLimit)
		var roundWG sync.WaitGroup
		for i, acc := range stay {
			if ctx.Err() != nil {
				roundWG.Wait()
				return
			}
			// 本轮仅探测已到期账号；未到期者保留到后续轮（其 nextAt 不变）。
			if t, ok := nextAt[acc.ID]; ok && !t.IsZero() && t.After(now) {
				next = append(next, acc)
				continue
			}
			// 回读最新账号：补探前必须确认账号仍处于可探测停调态（他实例可能已恢复，
			// 冷却也可能已到期），且要拿到最新的连续成功计数。
			fresh, err := p.accountRepo.GetByID(ctx, acc.ID)
			if err != nil || fresh == nil {
				continue
			}
			if !p.isHealthBreakerTrip(fresh, time.Now()) {
				continue
			}
			// 补探序号与 probeAccount 内的 attempt 同口径（本次尝试后的累计次数），
			// 便于按 account_id+attempt 串联「首探→补探→恢复」日志链（事故复盘时
			// 恢复时长抖动需逐次定位）；解析失败退回 round+1，仅影响日志不阻塞补探。
			attempt := round + 1
			if st, ok := parseHealthBreakerState(fresh.TempUnschedulableReason); ok && st != nil {
				attempt = st.ProbeAttempts + 1
			}
			// 代际令牌取入队时读到的 until（untils，全精度 time.Time），而非回读的最新
			// until：若探测期间 until 被他人改写（新停调接管，必写更晚 until），用最新
			// until 作令牌会误清新停调；用入队令牌则条件清除命中失配、rows==0、保留新停调
			//（AC-G2R5 must_fix / AC-G2R6 子秒精度）。
			expectedUntil := tempUnschedUntil(fresh)
			if u, ok := untils[acc.ID]; ok {
				expectedUntil = u
			}
			// 受并发上限约束地发起在途探测。
			roundWG.Add(1)
			sem <- struct{}{}
			go func(idx int, acc *Account, fresh *Account, attempt int, expectedUntil time.Time) {
				defer roundWG.Done()
				defer func() { <-sem }()
				stillParked := true
				if unlock, lockOK, lockErr := p.acquireAccountProbeLock(ctx, acc.ID, owner); lockOK {
					stillParked = p.probeAccountWithReason(ctx, fresh, expectedUntil)
					unlock()
				} else if lockErr != nil {
					logger.L().Warn("account_probe_lock_unavailable", zap.Int64("account_id", acc.ID), zap.Error(lockErr))
				}

				// 探测后重读最新 reason，依据本次结果决定该账号下次补探间隔（首成功后加速 /
				// 429 慢类 5min）：
				//   - 探测成功且未达恢复阈值：probeAccountWithReason 已把 ConsecutiveSuccesses
				//     递增回写，重读应见 >0 → 下次 accountHealthProbeRetryInterval（5s，首成功后
				//     加速确认第二次）；
				//   - 探测失败：ConsecutiveSuccesses==0，按 isQuotaExhaustedTrip 分类——慢类
				//     （429）accountHealthProbeQuotaRetryInterval（5min），其余 accountHealthProbeRetryInterval。
				chosen := p.probeRetryInterval()
				if stillParked {
					if reread, rerr := p.accountRepo.GetByID(ctx, acc.ID); rerr == nil && reread != nil {
						if st, ok := parseHealthBreakerState(reread.TempUnschedulableReason); ok && st != nil {
							if st.ConsecutiveSuccesses > 0 {
								chosen = p.probeRetryInterval()
							} else if p.isQuotaExhaustedTrip(reread) {
								chosen = accountHealthProbeQuotaRetryInterval
							} else {
								chosen = p.probeRetryInterval()
							}
						}
					}
				}
				// 日志 interval 字段改为该账号本次实际采用间隔（chosen）。
				logger.L().Info("openai.apikey_health_probe_retry",
					zap.Int64("account_id", acc.ID),
					zap.Int("round", round),
					zap.Int("attempt", attempt),
					zap.Duration("interval", chosen),
				)
				results[idx] = burstAccountResult{stillParked: stillParked, chosen: chosen}
			}(i, acc, fresh, attempt, expectedUntil)
		}
		roundWG.Wait()
		// 本轮在途探测全部结束后，统一回写 nextAt / next（主循环串行，无 map 并发写竞争）。
		for i, acc := range stay {
			res := results[i]
			if !res.stillParked {
				// 恢复/被接管：退出队列（不探测，不安排下次）。
				continue
			}
			nextAt[acc.ID] = time.Now().Add(res.chosen)
			next = append(next, acc)
		}
		stay = next
	}
}

// sleepBeforeProbeRetry 等一个补探间隔；ctx 取消或 Stop() 触发时立即返回 false
// （不把补探等待拖进优雅关闭路径）。stopCh 由补探 goroutine 启动时捕获并全程持有，
// 不在此重新读取 p.stopCh（否则 Stop 先置 nil 后本函数取到 nil channel、close 失灵、
// 睡眠不可唤醒、wg.Wait 挂死，AC-G2R-3）。stopCh 为 nil 时（RunOnce 未经 Start 直接
// 调用，无关闭路径）由 timer 正常驱动，不影响补探节奏。
func (p *AccountHealthRecoveryProbeService) sleepBeforeProbeRetry(ctx context.Context, stopCh chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-stopCh:
		return false
	case <-timer.C:
		return true
	}
}

// runProbe executes the upstream probe, preferring a real request but allowing an
// override for tests.
func (p *AccountHealthRecoveryProbeService) runProbe(ctx context.Context, acc *Account) (bool, error) {
	if p.probeOverride != nil {
		return p.probeOverride(ctx, acc)
	}
	return p.probeUpstream(ctx, acc)
}

// probeUpstream 按账号归属分派探测：cursor 平台账号按形态二分（Connect 会话型走原生
// AgentRun 探针、OpenAI 兼容型走既有数据面探测）；CodeBuddy（影子或直绑）走专用探测分发器；
// 其余 OpenAI 协议族账号走真实数据面探测（probeUpstreamDataPlane）。
func (p *AccountHealthRecoveryProbeService) probeUpstream(ctx context.Context, acc *Account) (bool, error) {
	if p.httpUpstream == nil || p.cfg == nil {
		return false, errors.New("probe transport not configured")
	}
	// CURSOR-PROBE-001-A2：cursor 平台账号优先按形态二分，精确限定在 PlatformCursor，
	// 不抢其它平台账号（非 cursor 账号根本不进本分支）。
	if acc != nil && acc.Platform == PlatformCursor {
		return p.probeCursorUpstream(ctx, acc)
	}
	// CodeBuddy 平台账号（影子或直绑）统一走按账号形态选模型的实际 chat 探测。
	// 判定用
	// isCodeBuddyPlatformAccount（直绑 platform=codebuddy）|| isCodeBuddyShadowAccount
	//（存量影子），二者均复用既有函数，不得新写判定（方案 §三 卡 B）。
	if isCodeBuddyPlatformAccount(acc) || isCodeBuddyShadowAccount(acc) {
		return p.probeCodeBuddyUpstream(ctx, acc)
	}
	// DEVIN-PROBE：devin（Cascade）账号的 api_key 是 session token，不在
	// OpenAI 协议凭据解析中 ⇒ 落通用数据面会报 "probe account has no api key" 而
	// 永远解封不了。改派 devin 专用探针（GetUserJwt + GetChatMessage connect 帧）。
	if acc != nil && acc.Platform == PlatformDevin {
		return p.probeDevinUpstream(ctx, acc)
	}
	// 其余平台统一走 probeUpstreamDataPlane → probeUpstreamDataPlaneWithProtocolForModel，
	// 由 buildRecoveryProbeRequestSpec 按 probeProtocolFamily 分派协议；专用链路（cursor /
	// codebuddy / devin）已在上方收窄，不会被通用分派抢走。
	return p.probeUpstreamDataPlane(ctx, acc)
}

// probeDevinUpstream 对 devin（Cascade）平台账号做真实数据面探活：session token 经
// GetUserJwt 换取本请求 JWT，再走 GetChatMessage connect 帧发最小非流式对话
// （max_tokens=128）。与账号测试 testDevinAccountConnection 共用同一底层部件
// （NewDevinUpstreamClient / BuildDevinChatParams / NewDevinChatFrame / DevinStreamReader /
// decodeGetChatMessageResponse），成功判定 = 解析出非空正文。
//
// 403 是模型级拒绝而非账号失效：按 model_mapping 排序后的 concrete 键逐个试（最多
// devinProbeMaxCandidates 个），复用同一次 GetUserJwt 结果，只换模型重发 chat 帧。
// 遇到 403（forbidden / "Request not
// allowed"）或其它任何失败（非 200 / 读取错误 / trailer error / 空正文 / 构造错误）都
// 继续下一个候选，逐个试完；全部失败才返回错误（含尝试数与最后一个错误原因），避免
// 某个模型被禁就误判整账号 status=error。
func (p *AccountHealthRecoveryProbeService) probeDevinUpstream(ctx context.Context, acc *Account) (bool, error) {
	return p.probeDevinUpstreamForModel(ctx, acc, "")
}

func (p *AccountHealthRecoveryProbeService) probeDevinUpstreamForModel(ctx context.Context, acc *Account, requestedModel string) (bool, error) {
	token := strings.TrimSpace(acc.GetDevinToken())
	if token == "" {
		return false, errors.New("devin account has no api key")
	}

	// 候选：与可测试模型列表同源（model_mapping 请求侧键，排除通配），排序后取前
	// devinProbeMaxCandidates 个（GF-20：不再固定取前 5 个，避免可用模型排在后面轮不到）。
	mapping := acc.GetModelMapping()
	keys := make([]string, 0, len(mapping))
	for requestedModel := range mapping {
		trimmed := strings.TrimSpace(requestedModel)
		if trimmed == "" || strings.Contains(trimmed, "*") {
			continue
		}
		keys = append(keys, trimmed)
	}
	sort.Strings(keys)
	if len(keys) == 0 && strings.TrimSpace(requestedModel) == "" {
		return false, errors.New("devin account has no testable model")
	}
	if strings.TrimSpace(requestedModel) == "" && len(keys) > devinProbeMaxCandidates {
		keys = keys[:devinProbeMaxCandidates]
	}

	// 每个候选经 ResolveMappedModel 解析为上游 uid；解析不到或为空则跳过。
	// 全部解析不到则在鉴权前失败关闭（保持既有失败关闭语义）。
	upstreamUIDs := make([]string, 0, len(keys))
	if strings.TrimSpace(requestedModel) != "" {
		upstreamUIDs = append(upstreamUIDs, strings.TrimSpace(requestedModel))
	} else {
		for _, key := range keys {
			uid, matched := acc.ResolveMappedModel(key)
			if !matched || strings.TrimSpace(uid) == "" {
				continue
			}
			upstreamUIDs = append(upstreamUIDs, uid)
		}
	}
	if len(upstreamUIDs) == 0 {
		return false, errors.New("devin account has no mapped upstream model")
	}

	// 鉴权：GetUserJwt 只取一次，所有候选复用其 JWT/base（只换模型重发 chat 帧）。
	// 独立 30s 超时（devinAuthTimeout）；base 取 CustomAPIServerURL，缺省 DevinAPIURL。
	authCtx, cancelAuth := context.WithTimeout(ctx, devinAuthTimeout)
	defer cancelAuth()
	authResp, err := NewDevinUpstreamClient().GetUserJwt(authCtx, token)
	if err != nil {
		return false, fmt.Errorf("devin probe get user jwt: %w", err)
	}
	base := strings.TrimSpace(authResp.CustomAPIServerURL)
	if base == "" {
		base = DevinAPIURL
	}

	// tryCandidate 用给定上游 uid 发一次最小非流式对话并按原判定规则给结论。
	tryCandidate := func(upstreamUID string) (bool, error) {
		// 最小非流式对话：max_tokens=128 + 固定 system/user 提示。复用 C3 转换把 OpenAI
		// chat body 归一为 Devin 参数（system 进 SystemPrompt、user 进 Prompts）。
		probeBody, _ := json.Marshal(map[string]any{
			"model": upstreamUID,
			"messages": []map[string]string{
				{"role": "system", "content": "You are a helpful assistant."},
				{"role": "user", "content": "Reply with: ok"},
			},
			"max_tokens": 128,
			"stream":     false,
		})
		params, err := BuildDevinChatParams(probeBody, upstreamUID, "")
		if err != nil {
			return false, fmt.Errorf("devin probe build params: %w", err)
		}
		params.APIKey = token

		// 请求：复用 C2 帧构造，头族与 forwardDevin / 账号测试完全一致。
		frame, chatBaseURL, err := NewDevinChatFrame(params, authResp.UserJWT, base)
		if err != nil {
			return false, fmt.Errorf("devin probe build chat frame: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(chatBaseURL, "/")+DevinChatMessagePath, bytes.NewReader(frame))
		if err != nil {
			return false, fmt.Errorf("devin probe create request: %w", err)
		}
		req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
		req.Header.Set("Content-Type", "application/connect+proto")
		req.Header.Set("Connect-Protocol-Version", "1")
		req.Header.Set("Connect-Content-Encoding", "gzip")
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Connect-Accept-Encoding", "gzip")
		req.Header.Set("User-Agent", devinClientUA)

		proxyURL := ""
		if acc.ProxyID != nil && acc.Proxy != nil {
			proxyURL = acc.Proxy.URL()
		}
		resp, err := p.httpUpstream.DoWithTLS(req, proxyURL, acc.ID, acc.Concurrency,
			p.tlsFPProfileService.ResolveTLSProfile(acc))
		if err != nil {
			return false, fmt.Errorf("devin probe request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			// 403（forbidden / "Request not allowed"）是模型级拒绝，由调用方继续下一个候选。
			return false, fmt.Errorf("devin probe upstream status %d", resp.StatusCode)
		}

		// 聚合所有正文增量；END_STREAM trailer 含 error 视为失败。空流/空正文按失败关闭。
		reader := NewDevinStreamReader(resp.Body)
		var text strings.Builder
		for {
			payload, endStream, rerr := reader.Next()
			if rerr != nil {
				if rerr == io.EOF {
					break
				}
				return false, fmt.Errorf("devin probe stream read: %w", rerr)
			}
			if endStream {
				if code, msg := devinTrailerError(payload); code != "" {
					detail := code
					if msg != "" {
						detail += ": " + msg
					}
					return false, fmt.Errorf("devin probe upstream trailer error: %s", detail)
				}
				break
			}
			msg, derr := decodeGetChatMessageResponse(payload)
			if derr != nil {
				return false, fmt.Errorf("devin probe decode frame: %w", derr)
			}
			if msg.DeltaText != "" {
				text.WriteString(msg.DeltaText)
			}
		}
		if strings.TrimSpace(text.String()) == "" {
			return false, errors.New("devin probe upstream returned no text")
		}
		return true, nil
	}

	// 逐个候选探活：任一成功即返回；403 及其它失败都继续下一个（保守试完）。
	var lastErr error
	for _, uid := range upstreamUIDs {
		ok, err := tryCandidate(uid)
		if ok {
			return true, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	return false, fmt.Errorf("devin probe tried %d candidate(s), all failed: last error: %v",
		len(upstreamUIDs), lastErr)
}

// probeCursorUpstream 是 cursor 平台账号的探测分派器（CURSOR-PROBE-001-A2）：
//   - Connect 会话型（IsCursorConnectSessionAccount，即 type≠apikey 的 ShareAI 号池
//     OAuth 账号）：走 A1 提供的原生 AgentRun 探针封装 ProbeCursorAccountHealth（它只取
//     账号自身静态 access_token，不落到 Auto 动态池的另一个身份）。
//   - OpenAI 兼容型（IsCursorOpenAICompatibleAccount，即 type=apikey 的反代账号）：落回
//     既有 probeUpstreamDataPlane，并强制 Chat Completions 协议（复用转发侧既有的
//     shouldForwardOpenAIResponsesViaRawChatCompletions 判据，与真实转发形状一致），
//     不另写一份 CC 形状探测。
//
// 调用方 probeUpstream 已用 acc.Platform == PlatformCursor 收窄，此处只需按形态二分。
func (p *AccountHealthRecoveryProbeService) probeCursorUpstream(ctx context.Context, acc *Account) (bool, error) {
	if acc.IsCursorConnectSessionAccount() {
		return p.runCursorConnectProbe(ctx, acc)
	}
	// OpenAI 兼容型（apikey 反代）：落回既有数据面探测；协议判据复用转发侧
	// shouldForwardOpenAIResponsesViaRawChatCompletions（cursor+apikey ⇒ true），强制 CC。
	return p.probeUpstreamDataPlaneWithProtocol(ctx, acc, shouldForwardOpenAIResponsesViaRawChatCompletions(acc))
}

// runCursorConnectProbe 对 cursor Connect 会话型账号发一次最小原生 AgentRun 探测，复用 A1 的
// ProbeCursorAccountHealth 封装（它内部只取账号自身静态 access_token，不落到 Auto 动态池
// 的另一个身份——这正是 R2 发现 1 的三个条件之一）。cursorExec 未注入（A5 卡装配前）时安全
// 降级返回 error（不伪造失败，等装配后生效）。
func (p *AccountHealthRecoveryProbeService) runCursorConnectProbe(ctx context.Context, acc *Account) (bool, error) {
	if p.cursorExec == nil {
		return false, errors.New("cursor execution service not configured for connect-session probe")
	}
	upstreamModel := p.cursorProbeOutboundModel(acc)
	if strings.TrimSpace(upstreamModel) == "" {
		// 无可用出站模型属配置错误，降级到到期恢复（既有 degraded 语义），不伪造失败。
		return false, errors.New("cursor connect probe has no model to probe")
	}
	return p.runCursorConnectProbeForModel(ctx, acc, upstreamModel)
}

func (p *AccountHealthRecoveryProbeService) runCursorConnectProbeForModel(ctx context.Context, acc *Account, upstreamModel string) (bool, error) {
	if p.cursorExec == nil {
		return false, errors.New("cursor execution service not configured for connect-session probe")
	}
	fn := p.cursorAgentRunProbe
	if fn == nil {
		fn = ProbeCursorAccountHealth
	}
	// 预算传 cursorAccountHealthProbeMinBudget（≥256）：思考型模型把预算先花在 reasoning
	// 上，16 token 会探不出正文 ⇒ 探针恒失败（生产账号 406 实证）。A3 卡改造 ProbeAgentRun
	// 接纳预算参数后此处即生效；当前 A1 的 ProbeCursorAccountHealth 暂忽略该入参（A3 接线）。
	status, _, text, err := fn(ctx, p.cursorExec, acc, upstreamModel, cursorAccountHealthProbeMinBudget)
	if err != nil {
		// 降级（无可构造的安全探测面）：保持停调，等冷却到期。
		return false, err
	}
	// 成功判定：HTTP 200 且解析出非空文本帧（与账号测试同口径）。
	return status == http.StatusOK && strings.TrimSpace(text) != "", nil
}

// probeUpstreamDataPlane 按账号真实协议发一次最小真实请求（协议判据走既有
// ShouldUseResponsesAPI，不强制 Chat Completions）。它是 probeUpstreamDataPlaneWithProtocol
// 的薄封装（forceChatCompletions=false），既有调用方与既有测试一行不改。
//
// 为什么不再探 /v1/models：中转站的 /models 由轻量前端直接返回模型清单，不与
// chat/responses 数据面共用链路；上游数据面全挂时 /models 仍可 2xx，会误判健康。
// 本函数复刻真实转发链的协议选择、模型映射、代理、TLS 指纹与会话头，探的就是故障面本身。
func (p *AccountHealthRecoveryProbeService) probeUpstreamDataPlane(ctx context.Context, acc *Account) (bool, error) {
	return p.probeUpstreamDataPlaneWithProtocol(ctx, acc, false)
}

// probeUpstreamDataPlaneWithProtocol 是 probeUpstreamDataPlane 的内部实现，新增可选协议覆盖
// 参数 forceChatCompletions：为 true 时跳过 ShouldUseResponsesAPI 判据、强制按 Chat
// Completions 探测（端点 /v1/chat/completions），用于 cursor APIKey 反代账号——其真实转发被
// 显式固定为 CC（shouldForwardOpenAIResponsesViaRawChatCompletions 对 cursor+apikey 返回
// true），若沿用 ShouldUseResponsesAPI（标记缺失 ⇒ true）会打向上游不存在的 /v1/responses
// ⇒ 探针恒失败 ⇒ 账号停到 9999 哨兵永不解封（R4 发现 1）。传入该判据本身（而非字面 true）
// 即 SSOT 对齐，不另造协议判定。其它平台调用方一律传 false，行为逐字不变。
//
// 批 2：模型参数化后本函数仅作薄委托；传空 model 表示沿用既有取模型逻辑（行为逐字不变）。
func (p *AccountHealthRecoveryProbeService) probeUpstreamDataPlaneWithProtocol(ctx context.Context, acc *Account, forceChatCompletions bool) (bool, error) {
	return p.probeUpstreamDataPlaneWithProtocolForModel(ctx, acc, "", forceChatCompletions)
}

// probeUpstreamDataPlaneWithProtocolForModel 是三个恢复分量（账号 transport / 账号
// quota / 模型 rate-limit）共用的数据面探测实际实现：协议判定统一收敛到
// probeProtocolFamily + buildRecoveryProbeRequestSpec，本函数只负责发送细节与响应判定，
// 不再自写平台判断。model 非空时直接使用传入模型（跳过 probeModelForAccount 与 cursor
// 分支回落）；为空时沿用既有取值逻辑。
//
// 传输（DoWithTLS + proxy + TLS 指纹 + 45s 超时 + 读上限 + 头覆写 + OpenCode 会话头 +
// muse UA）与 CAS/generation/probe_due_at/lock 节奏逐字保留。
func (p *AccountHealthRecoveryProbeService) probeUpstreamDataPlaneWithProtocolForModel(ctx context.Context, acc *Account, model string, forceChatCompletions bool) (bool, error) {
	if acc == nil {
		return false, errors.New("probe account is nil")
	}
	// OpenAI OAuth/SetupToken 订阅号的 OpenAI APIKey 凭据恒为空（type!=apikey），
	// 落原 APIKey 分支会报 "probe account has no api key" 走 degraded 到期恢复，账号
	// 永远探不通。保留 OAuth 专用凭据路径（access_token + Codex 端点）。
	if isOpenAIOAuthProbeAccount(acc) {
		return p.probeOpenAIOAuthDataPlane(ctx, acc, model)
	}
	// 显式 model 和 probeModelForAccount 都是已解析的上游模型名。Cursor 账号级探针
	// 单独从其 model_mapping 取一个入站键，再解析为相同的上游名。
	if model == "" {
		model = p.probeModelForAccount(acc)
		if acc.Platform == PlatformCursor {
			model = p.cursorProbeOutboundModel(acc)
		}
	}
	if model == "" {
		return false, errors.New("probe account has no model to probe")
	}
	upstreamModel := normalizeOpenAIModelForUpstream(acc, model)

	// 具体端点优先于能力标记：base 已写死 .../chat/completions 或 .../responses 时必须
	// 照用，否则会拼出 /v1/chat/completions/v1/responses 这种不存在的路径（生产实证）。
	// base 获取走共享 helper（含 Grok 回退），恢复链不再自行取 OpenAI base。
	useResponses := !forceChatCompletions && openai_compat.ShouldUseResponsesAPI(acc.SnapshotExtra())
	if concrete, concreteIsResponses := probeConcreteEndpoint(openAIProbeBaseURL(acc)); concrete {
		useResponses = concreteIsResponses
	}
	// 恢复链唯一协议分派入口：family 为空（如 Antigravity transport / Gemini OAuth）时
	// 返回明确 error，调用方失败关闭、保持停调，绝不 OpenAI fallback。
	spec, family, err := buildRecoveryProbeRequestSpec(acc, upstreamModel, useResponses)
	if err != nil {
		return false, err
	}
	// URL 安全校验放在拿到 spec.url 之后：Anthropic/Gemini/Grok 不能依赖 OpenAI base，
	// base 获取与端点派生已全部交给共享 builder。
	validated, err := cnValidateProbeURL(p.cfg, spec.url)
	if err != nil {
		return false, fmt.Errorf("invalid probe url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, spec.method, validated, bytes.NewReader(spec.body))
	if err != nil {
		return false, fmt.Errorf("build probe request: %w", err)
	}
	for k, v := range spec.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	// 账号级 header 覆写：与真实转发最终头同源
	acc.ApplyHeaderOverrides(req.Header)
	// 伪装账号（OpenCode Go 端点）必需会话头。探测无入站 gin.Context，
	// resolveOpenCodeSessionID 走 generate 分支生成 UUID；body 参与解析以复用
	// prompt_cache_key 等稳定会话键。
	applyOpenCodeSessionHeader(nil, acc, validated, req.Header, spec.body)
	// muse 系 / OpenCode 官方主机需要网关中继 UA（与转发链同源）
	if acc.Platform == PlatformMuse || isOpenCodeHostURL(strings.TrimSpace(acc.GetCredential("base_url"))) {
		req.Header.Set("User-Agent", museRelayUserAgent)
	}
	if !useResponses {
		req.Header.Set("Accept", "application/json")
	}

	proxyURL := ""
	if acc.ProxyID != nil && acc.Proxy != nil {
		proxyURL = acc.Proxy.URL()
	}
	var tlsProfile *tlsfingerprint.Profile
	if p.tlsFPProfileService != nil {
		tlsProfile = p.tlsFPProfileService.ResolveTLSProfile(acc)
	}

	callCtx, cancel := context.WithTimeout(ctx, accountHealthProbeDataPlaneTimeout)
	defer cancel()
	resp, err := p.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, acc.ID, acc.Concurrency, tlsProfile)
	if err != nil {
		// 传输失败与既有数据面探测同语义：上游不可达即不健康，保持停调（非降级）。
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, accountHealthProbeDataPlaneMaxBodyBytes))
	if err != nil {
		// 读取失败（如流出文本增量后连接中断）与传输失败同语义：不健康，保持停调。
		// 不得拿已读到的部分字节判恢复成功，那是伪恢复证明。
		return false, nil
	}
	// 成功判定按 family：anthropic/gemini 为 2xx + SSE 文本增量；openai 沿用
	// evaluateProbeDataPlaneResponse（2xx + 合法 JSON + 无 error 信封 + 协议成功体）。
	// 2xx 无文本增量/无成功体一律失败关闭。
	if family == "anthropic" || family == "gemini" {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return false, nil
		}
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if probeSSEDataHasText(family, string(line)) {
				return true, nil
			}
		}
		return false, nil
	}
	return evaluateProbeDataPlaneResponse(resp.StatusCode, raw, useResponses), nil
}

// probeOpenAIOAuthDataPlane 对 OpenAI OAuth/SetupToken 订阅号做真实数据面探测。
//
// 与 APIKey 路径的差异只在凭据与端点：用 access_token（影子账号取母账号凭据）+
// 转发侧 chatgptCodexURL 的 Codex /responses 流式端点。发送路径（DoWithTLS +
// acc.Proxy + tlsProfile + accountHealthProbeDataPlaneTimeout）与响应读取上限均沿用
// 本服务既有数据面探测，逐字同源，不新造传输逻辑。
//
// 候选回退（OAUTH-PROBE-CANDIDATE-FALLBACK）：管理员随时维护账号模型名单
// （credentials.model_mapping），名单里靠前的模型随时可能被上游禁掉（400
// model_not_found / 403 forbidden）。只押一个模型 ⇒ 探针恒失败 ⇒ 账号永远回不了池。
// 故按候选列表（openAIOAuthProbeCandidates，字典序可复现）逐个试：
//   - **模型级失败**（上游只拒绝这个模型：HTTP 400/403/404，或响应体含模型级拒绝
//     文案）⇒ 换下一个候选继续试（与 devin 恢复探针同思路）；
//   - **账号级失败**（HTTP 401/429/5xx、传输错误、配置类错误）⇒ 换模型也没用 ⇒
//     立即结束，不再试其它候选。
//
// 配置类错误（无 model、凭据/影子解析失败、请求构造失败）按既有 degraded 语义返回 error
// （到期恢复，不伪造失败）；传输失败与上游拒绝按「不可达即不健康」返回 (false, nil)，
// 保持停调（非降级）。
func (p *AccountHealthRecoveryProbeService) probeOpenAIOAuthDataPlane(ctx context.Context, acc *Account, model string) (bool, error) {
	// 候选列表：调用方显式传入的 model 优先（模型级限流恢复通道据此探活具体 model，避免被
	// "另一个模型"的探活误清）；其次熔断 state 记录的触发模型；再否则取管理员维护的账号
	// model_mapping 解析出的全部候选（见 openAIOAuthProbeCandidates）。
	//
	// 这里不再回落 openai.DefaultTestModel（= "gpt-5.4"）：Codex 上游不支持该模型，用它探测
	// 恒得 400 model-not-found ⇒ 账号恒判不健康 ⇒ 永远卡在"探测中，无法自动回池"。
	candidates := openAIOAuthProbeCandidates(acc, model)
	if len(candidates) == 0 {
		return false, errors.New("probe account has no model to probe")
	}
	// 候选上限：OAuth/Codex 上游每个候选都是一次真实订阅出站请求（比 devin 贵），按字典序
	// 取前 openAIOAuthProbeMaxCandidates 个试（名单里更靠后的模型本轮轮不到）。
	if len(candidates) > openAIOAuthProbeMaxCandidates {
		candidates = candidates[:openAIOAuthProbeMaxCandidates]
	}

	proxyURL := ""
	if acc.ProxyID != nil && acc.Proxy != nil {
		proxyURL = acc.Proxy.URL()
	}
	var tlsProfile *tlsfingerprint.Profile
	if p.tlsFPProfileService != nil {
		tlsProfile = p.tlsFPProfileService.ResolveTLSProfile(acc)
	}

	var lastReason string
	attempted := 0
	for _, candidate := range candidates {
		attempted++
		// 请求构造失败属于配置类错误（凭据缺失 / 影子解析失败 / 构造失败）：换模型也没用 ⇒
		// 不试下一个候选，走既有 degraded 语义（返回 error ⇒ 到期恢复，不伪造失败）。
		req, err := buildOpenAIOAuthProbeRequest(ctx, p.accountRepo, acc, candidate)
		if err != nil {
			return false, err
		}

		callCtx, cancel := context.WithTimeout(ctx, accountHealthProbeDataPlaneTimeout)
		resp, err := p.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, acc.ID, acc.Concurrency, tlsProfile)
		if err != nil {
			cancel()
			// 传输失败与既有数据面探测同语义：上游不可达即不健康，保持停调（非降级）。
			// 传输错误是账号级失败（换模型也没用）⇒ 不再试其它候选。
			return false, nil
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, accountHealthProbeDataPlaneMaxBodyBytes))
		_ = resp.Body.Close()
		cancel()

		if probeOpenAIOAuthResponseOK(resp.StatusCode, raw) {
			return true, nil
		}

		lastReason = fmt.Sprintf("model %q: status %d", candidate, resp.StatusCode)
		if classifyOpenAIOAuthProbeFailure(resp.StatusCode, raw) != openAIOAuthProbeFailureModelLevel {
			// 账号级失败（401/429/5xx 等）⇒ 换模型也没用 ⇒ 立即结束，保持停调。
			return false, nil
		}
		// 模型级失败（上游禁掉了这个模型）⇒ 换下一个候选继续试。
	}

	// 全部候选都失败 ⇒ 保持停调（不伪造成功），并带上尝试过的候选数与最后一个失败原因
	// （与 devin 恢复探针同口径）。
	logger.L().Warn("openai.apikey_health_probe_oauth_candidates_failed",
		zap.Int64("account_id", acc.ID),
		zap.Int("candidates", attempted),
		zap.String("last_failure", lastReason),
	)
	return false, nil
}

// probeModelForAccount 取探测模型：熔断 state 记录的 Model 优先，其次账号自己维护的
// 模型名单（credentials.model_mapping）解析出的上游真名。
//
// 这里不再回落 openai.DefaultTestModel（= "gpt-5.4"）：那个模型只在 OpenAI 官方上游存在，
// 第三方上游（商汤/智谱/讯飞…）压根没有 ⇒ 探测恒得 404 model-not-found ⇒ 账号恒判不健康
// ⇒ 永远卡在"探测中，无法自动回池"（生产实证：商汤账号 1002 卡死 12:04→12:27 反复重排）。
//
// 账号没维护模型名单时返回空串，调用方按既有 degraded 语义处理（不伪造失败、不乱猜模型）。
func (p *AccountHealthRecoveryProbeService) probeModelForAccount(acc *Account) string {
	if acc == nil {
		return ""
	}
	if state, ok := parseHealthBreakerState(acc.TempUnschedulableReason); ok && state != nil {
		if m := strings.TrimSpace(state.Model); m != "" {
			return m
		}
	}
	return pickProbeModelUpstream(probeModelCandidates(acc))
}

// cursorProbeModelKey 取 cursor 账号探针的入站模型键（model_mapping 的键）。
//
// 优先级：① 熔断 state 记录的触发模型（若该键在 model_mapping 中存在）；
// ② 账号自己维护的模型名单（credentials.model_mapping）解析出的候选键。
//
// 这里不再用硬编码键名（gpt-6.1-sol / claude-fable-5-1）挑模型：那是"猜账号有哪些模型"，
// 猜错了该账号就探不到可用模型 ⇒ 探针失败关闭 ⇒ 账号永远不解封。改为从唯一事实源
// （probeModelCandidates）取候选键，管理员配了什么就探什么。
// 仍然要求键命中 mapping（cursorProbeOutboundModel 随后把键解析成出站值）。
func (p *AccountHealthRecoveryProbeService) cursorProbeModelKey(acc *Account) string {
	mapping := acc.GetModelMapping()
	has := func(k string) bool {
		_, ok := mapping[k]
		return ok
	}
	if state, ok := parseHealthBreakerState(acc.TempUnschedulableReason); ok && state != nil {
		if m := strings.TrimSpace(state.Model); m != "" && has(m) {
			return m
		}
	}
	for _, candidate := range probeModelCandidates(acc) {
		if has(candidate.InboundKey) {
			return candidate.InboundKey
		}
	}
	return ""
}

// cursorProbeOutboundModel 取 cursor Connect 会话型探针的出站模型名（ProbeAgentRun 入参）。
// 先用 cursorProbeModelKey 取入站键，再经 account.ResolveMappedModel 解析为出站值；解析不到
// 真实上游名时回落该键本身（调用方按降级处理）。出站模型必须是 ResolveMappedModel 的值而非
// 键名（方案 §2.1 探针模型口径）。
func (p *AccountHealthRecoveryProbeService) cursorProbeOutboundModel(acc *Account) string {
	key := p.cursorProbeModelKey(acc)
	if v, matched := acc.ResolveMappedModel(key); matched && strings.TrimSpace(v) != "" {
		return v
	}
	return key
}

// probeConcreteEndpoint 判断 base_url 是否已经写到具体端点（而非主机或 /v1 前缀），
// 并返回该端点是否为 Responses 协议。
//
// 为什么必须单独判：base_url 写成 http://host/v1/chat/completions 时，端点派生函数
// （buildVersionedEndpointURL）看到 path 既不以 /v1/responses 结尾、末段也不是版本号，
// 会直接追加一段 /v1/responses，拼出 /v1/chat/completions/v1/responses 这种根本不存在
// 的路径。这里的后果是致命的：探测永远 404/秒失败（生产实证：账号 451 探针 50ms 失败、
// 熔断后停到 9999 年永远恢复不了），而同一 base_url 的业务链路完全正常（线上 419/416 号
// 各 2~3 万次请求均如此配置）。
//
// 更糟的是它构成死循环：extra 里的 openai_responses_supported 由自动探测写回，而探测本身
// 要先能把 Responses 端点探通才能写回。URL 拼错 → 标记永远缺失 → 按"未探测=保留旧行为=
// 走 Responses"处理 → 继续拼错。管理员既然写死了完整端点，就照用，不再派生。
func probeConcreteEndpoint(base string) (isConcrete bool, isResponses bool) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return false, false
	}
	path := strings.ToLower(strings.TrimRight(parsed.Path, "/"))
	switch {
	case strings.HasSuffix(path, "/responses"):
		return true, true
	case strings.HasSuffix(path, "/chat/completions"):
		return true, false
	default:
		return false, false
	}
}

// evaluateProbeDataPlaneResponse 判定一次真实数据面探测是否成功。
// 成功需同时满足：HTTP 2xx、响应体是合法 JSON、且不携带 error 信封。
// 只有 2xx 不够——部分中转站会用 200 + {"error":{...}} 表达业务失败。
func evaluateProbeDataPlaneResponse(statusCode int, body []byte, useResponses bool) bool {
	if statusCode < 200 || statusCode >= 300 {
		return false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		// 空体无法证明数据面可用，失败关闭。
		return false
	}
	if !gjson.ValidBytes(trimmed) {
		return false
	}
	if gjson.GetBytes(trimmed, "error").Exists() {
		return false
	}
	if useResponses {
		// Responses 成功体至少含 id 或 object 之一。
		return gjson.GetBytes(trimmed, "id").Exists() || gjson.GetBytes(trimmed, "object").Exists()
	}
	// Chat Completions 成功体含 choices 数组。
	return gjson.GetBytes(trimmed, "choices").IsArray()
}

// ---------------------------------------------------------------------------
// CodeBuddy 探测分发器（方案 §三 卡 B，AC-B1）
// ---------------------------------------------------------------------------

// probeCodeBuddyUpstream 是 CodeBuddy 平台账号（影子或直绑）的探测分发器：
//   - 影子账号使用其 shadow_model 与母账号凭证；
//   - 直绑账号使用自身配置模型与凭证。
//
// 两者都以最小流式 chat 完成证据确认数据面恢复。
//
// 调用方 probeUpstream 已在进入本函数前用 isCodeBuddyPlatformAccount ||
// isCodeBuddyShadowAccount 收窄，故此处只需按影子/直绑二分。
func (p *AccountHealthRecoveryProbeService) probeCodeBuddyUpstream(ctx context.Context, acc *Account) (bool, error) {
	model := ""
	if isCodeBuddyShadowAccount(acc) {
		model = strings.TrimSpace(acc.GetExtraString(ShadowModelExtraKey))
	} else {
		model = p.probeModelForAccount(acc)
	}
	return p.probeCodeBuddyChatUpstream(ctx, acc, model)
}

// ---------------------------------------------------------------------------
// CodeBuddy 流式 chat 探测（方案 T4，R2-F1/R3-F2）
// ---------------------------------------------------------------------------

// codeBuddyShadowProbeTimeout 单次探测超时，与既有 /models 探测同款（15s）。
const codeBuddyShadowProbeTimeout = 15 * time.Second

// codeBuddyShadowProbeMaxBodyBytes 探测响应读取上限。max_tokens=1 的最小流远小于该值；
// 达到上限即视为截断流（缺 data: [DONE]）→ 探测失败（fail-closed）。
const codeBuddyShadowProbeMaxBodyBytes = 256 << 10

// codeBuddyShadowProbeSystemMessage 是探测请求的 system 消息（PrepareCodeBuddyBody
// 规则 3b 要求首条消息为 system）；内容保持极短且不含任何框架指纹。
const codeBuddyShadowProbeSystemMessage = "health probe"

// probeCodeBuddyChatUpstream verifies the model data plane for direct and shadow accounts.
// CodeBuddy requires stream=true. The shared probe also checks the aggregated
// completion, [DONE] terminator, and absence of upstream error frames.
func (p *AccountHealthRecoveryProbeService) probeCodeBuddyChatUpstream(ctx context.Context, acc *Account, model string) (bool, error) {
	if p.accountRepo == nil {
		return false, errors.New("probe account repo not configured")
	}

	// 影子→母账号凭证解析：母账号的 access_token / site / uid / enterprise_id / domain
	// 才是出站的真正身份。校验语义与 resolveCredentialAccount 一致（fail-closed）。
	credAccount := acc
	if acc.ParentAccountID != nil {
		parent, err := p.accountRepo.GetByID(ctx, *acc.ParentAccountID)
		if err != nil {
			return false, fmt.Errorf("resolve codebuddy shadow parent: %w", err)
		}
		if parent == nil || parent.IsShadow() {
			return false, fmt.Errorf("codebuddy shadow parent %d missing or itself a shadow", *acc.ParentAccountID)
		}
		if !(parent.IsCodeBuddy() && (parent.Type == AccountTypeOAuth || parent.Type == AccountTypeAPIKey)) {
			return false, fmt.Errorf("codebuddy shadow parent %d is not a codebuddy OAuth/APIKey account", parent.ID)
		}
		credAccount = parent
	}

	// 模型来自影子 extra.shadow_model，或直绑/模型级调用方指定的真实 scope。
	model = strings.TrimSpace(model)
	if model == "" {
		return false, errors.New("codebuddy account has no model to probe")
	}

	token := codeBuddyTestAccessToken(credAccount)
	if token == "" {
		return false, errors.New("codebuddy credential account is missing access_token credential")
	}

	// 最小探测体：system-first、极短 prompt、max_tokens=1、stream:true；再过既有
	// PrepareCodeBuddyBody 归一，保证全部协议规则（强制 stream、tool_choice 归一、
	// DeepSeek thinking 注入等）与转发链一致。探测体不设 reasoning_effort，
	// SupportedEfforts 留空（规则 5 对无该字段的请求是 no-op）。
	probeBody, err := PrepareCodeBuddyBody(codeBuddyShadowProbeBody(model), CodeBuddyRewriteOptions{
		Sanitize: p.codeBuddyProbeSanitizeEnabled(),
		Model:    model,
	})
	if err != nil {
		return false, fmt.Errorf("prepare codebuddy probe body: %w", err)
	}

	req, err := buildCodeBuddyShadowProbeRequest(ctx, credAccount, probeBody, token, p.cfg)
	if err != nil {
		return false, fmt.Errorf("build codebuddy probe request: %w", err)
	}

	proxyURL := ""
	if acc.ProxyID != nil && acc.Proxy != nil {
		proxyURL = acc.Proxy.URL()
	}
	var tlsProfile *tlsfingerprint.Profile
	if p.tlsFPProfileService != nil {
		tlsProfile = p.tlsFPProfileService.ResolveTLSProfile(acc)
	}

	callCtx, cancel := context.WithTimeout(ctx, codeBuddyShadowProbeTimeout)
	defer cancel()
	resp, err := p.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, acc.ID, acc.Concurrency, tlsProfile)
	if err != nil {
		// 传输失败与 /models 探测同语义：上游不可达即不健康，保持停调（非降级）。
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, codeBuddyShadowProbeMaxBodyBytes))
	return evaluateCodeBuddyShadowProbeResponse(resp.StatusCode, raw), nil
}

// codeBuddyShadowProbeBody 构造最小探测请求体（构造失败返回 nil，由
// PrepareCodeBuddyBody 的 json.Valid 校验失败关闭）。
func codeBuddyShadowProbeBody(model string) []byte {
	encoded, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": codeBuddyShadowProbeSystemMessage},
			{"role": "user", "content": "ping"},
		},
		"max_tokens": 1,
		"stream":     true,
	})
	if err != nil {
		return nil
	}
	return encoded
}

// codeBuddyProbeSanitizeEnabled 返回探测出站的 system 指纹脱敏开关，与转发链
// （OpenAIGatewayService.codeBuddySanitizeEnabled）同默认：开启。
func (p *AccountHealthRecoveryProbeService) codeBuddyProbeSanitizeEnabled() bool {
	if p != nil && p.cfg != nil {
		return p.cfg.Gateway.CodeBuddy.SanitizeEnabled
	}
	return true
}

// buildCodeBuddyShadowProbeRequest 构造 CodeBuddy 影子探测请求，请求形状与转发链
// buildCodeBuddyChatRequest 同源（§2.4 指纹头：Content-Type/Accept/X-Requested-With/
// Origin/Referer/UA/Authorization/X-Product + 身份头空值 X-No-*: 1 占位；身份头取自
// 母账号，影子自身凭证为空；母账号 ApplyHeaderOverrides 最后应用）。
//
// 独立成函数的原因：buildCodeBuddyChatRequest 挂在 OpenAIGatewayService 上（探测服务
// 不依赖该服务），且 CARD-C 文件边界不含 codebuddy_gateway_forward.go，无法无损提取
// 纯构造。两处形状必须同步演进：改动转发请求形状时须同步此处。
//
// 安全红线同源：chat 请求绝不携带 X-Refresh-Token（该头仅用于 refresh 端点）。
func buildCodeBuddyShadowProbeRequest(ctx context.Context, credAccount *Account, body []byte, token string, cfg *config.Config) (*http.Request, error) {
	ep := codeBuddyEndpointsFor(credAccount.CodeBuddySite())
	targetURL := strings.TrimRight(ep.UpstreamBase, "/") + codeBuddyChatCompletionsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	ua := CodeBuddyClientUA
	if cfg != nil {
		if candidate := strings.TrimSpace(cfg.Gateway.CodeBuddy.ChatUserAgent); candidate != "" {
			ua = candidate
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", ep.OriginReferer)
	req.Header.Set("Referer", ep.OriginReferer+"/")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Product", "SaaS")

	uid := credAccount.GetCredential("uid")
	enterpriseID := credAccount.GetCredential("enterprise_id")
	domainCred := credAccount.GetCredential("domain")
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if domainCred != "" {
		req.Header.Set("X-Domain", domainCred)
	} else {
		req.Header.Set("X-No-Domain", "1")
	}

	credAccount.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// evaluateCodeBuddyShadowProbeResponse 实现探测成功四条件（方案 T4，R3-F2）：
//  1. HTTP 2xx（仅凭状态码判定被 R1-F2 明确禁止）；
//  2. 聚合结果为有效 chat completion：aggregateOpenAIChatCompletionsSSE 聚合成功
//     （非 SSE / 无 chunk / 纯错误信封在此返回 false）、choices 非空、无业务错误信封；
//  3. 原始 SSE 含 data: [DONE] 正常终止帧；
//  4. 全程无错误帧（event: error 帧 + data 帧业务错误信封，见扫描器注释）。
//
// 条件 3/4 必不可少：聚合器见首 chunk 即置位、缺 finish_reason 会合成 stop
// （openai_chat_completions_sse_aggregate.go），「部分内容+错误帧」与截断流的聚合体
// 仍可能有非空 choices，仅验聚合体会误清熔断。
func evaluateCodeBuddyShadowProbeResponse(statusCode int, rawSSE []byte) bool {
	if statusCode < 200 || statusCode >= 300 {
		return false
	}
	aggregated, ok := aggregateOpenAIChatCompletionsSSE(rawSSE)
	if !ok {
		return false
	}
	var completion struct {
		Choices []json.RawMessage `json:"choices"`
		Code    json.RawMessage   `json:"code"`
	}
	if err := json.Unmarshal(aggregated, &completion); err != nil {
		return false
	}
	if len(completion.Choices) == 0 {
		return false
	}
	if codeBuddySSEPayloadCarriesError(completion.Code) {
		return false
	}
	return scanCodeBuddyShadowProbeSSE(rawSSE)
}

// scanCodeBuddyShadowProbeSSE 扫描原始 SSE：返回是否「正常终止且全程无错误帧」。
// 终止判定：出现 data: [DONE] 帧。错误帧判定复用 gateway_upstream_response.go
// processSSEEvent 的 sseStreamErrorEventError 语义（event 名为 error 的帧），并按方案
// 「无业务错误信封」口径把 data 帧中携带 code!=0 / error 对象的帧一并视为错误帧
// ——CodeBuddy 业务错误正是以 JSON 信封随 HTTP 200 返回（account_test_service.go
// evaluateCodeBuddyProbeBody 同款判定口径）。保守方向正确：可疑流判失败只多停一轮
// 冷却，误判成功才会错清熔断。
func scanCodeBuddyShadowProbeSSE(raw []byte) bool {
	sawDone := false
	for _, rawLine := range strings.Split(string(raw), "\n") {
		line := strings.TrimSpace(rawLine)
		switch {
		case strings.HasPrefix(line, "event:"):
			if strings.TrimSpace(strings.TrimPrefix(line, "event:")) == "error" {
				return false
			}
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				sawDone = true
				continue
			}
			if payload != "" && codeBuddySSEPayloadCarriesError([]byte(payload)) {
				return false
			}
		}
	}
	return sawDone
}

// codeBuddySSEPayloadCarriesError 判定一个 data 帧 JSON 是否为业务错误信封：
//   - 顶层 error 字段非空（OpenAI 兼容流式错误形状 {"error":{...}}；null 不算）；
//   - 顶层 code 语义与 codeBuddyProbeEnvelopeCode 同口径：数字 != 0，或字符串非空
//     且 != "0"。
//
// 正常 chat.completion.chunk 不含这两个顶层字段，无误伤面；非 JSON / 非 object
// payload 一律按非错误处理（交给聚合器与终止帧判定兜底）。
func codeBuddySSEPayloadCarriesError(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var envelope struct {
		Code  json.RawMessage `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return false
	}
	if errorField := bytes.TrimSpace(envelope.Error); len(errorField) > 0 && string(errorField) != "null" {
		return true
	}
	if len(envelope.Code) == 0 {
		return false
	}
	var codeValue any
	if err := json.Unmarshal(envelope.Code, &codeValue); err != nil {
		return false
	}
	switch value := codeValue.(type) {
	case float64:
		return value != 0
	case string:
		trimmedCode := strings.TrimSpace(value)
		return trimmedCode != "" && trimmedCode != "0"
	}
	return false
}

// SetProbeOverride installs a probe function override (tests only).
func (p *AccountHealthRecoveryProbeService) SetProbeOverride(fn func(ctx context.Context, account *Account) (bool, error)) {
	if p != nil {
		p.probeOverride = fn
	}
}

// ===========================================================================
// 探测观测状态面（D2 探针相位已退役，方案 th-kira-quota-lifecycle §7）。
// 以下符号被禁区文件消费，不随相位退役：
//   - tokenHarborAccountLevelProbeScope / entryObservedAtKey / entryAttemptedAtKey /
//     ProbeOutcome：ratelimit_service.go 写入口与 account_freshness_*.go 告警/视图链；
//   - probeBudgetPerMinute / probeStartupCooldown / probeRoundPeriod /
//     probeRequestHardTimeout / ComputeProbeUpperBound：三维新鲜度阈值公式；
//   - 冻结上界族（freezeCandidateBound 等）：freshness 阈值输入，写入语义归 freshness 链。
// ===========================================================================

// tokenHarborAccountLevelProbeScope 是账号级（无模型键）维度在 model_rate_limits
// 桶中的占位 scope，与模型级 scope 互不掩盖（F：两个维度不得互相刷新）。
const tokenHarborAccountLevelProbeScope = "tokenharbor_account_level_probe"

// 探测链写入条目内的时间字段键（与 ratelimit_service.go 的写入入口共用）。
const (
	entryObservedAtKey  = "observed_at"
	entryAttemptedAtKey = "attempted_at"
)

// 上限公式常量（C/E，单一事实源）。probeBudgetWindow / probeBudgetManager 等预算
// 执行体随 D2 相位退役；probeBudgetPerMinute 仍为上限公式的分母输入。
const (
	probeStartupCooldown    = 60 * time.Second
	probeRoundPeriod        = 60 * time.Second
	probeRequestHardTimeout = 30 * time.Second
)

// ProbeOutcome 是探测观测写入口对单次上游响应的分类（D）。
type ProbeOutcome int

const (
	// ProbeOutcomeSuccess 目标模型最小请求 2xx（含慢 2xx）→ 幂等清除该 scope 限流。
	ProbeOutcomeSuccess ProbeOutcome = iota
	// ProbeOutcomeFreeTier429 识别出免费档额度用光 429 → 确认仍受限，置 observed_at（不清）。
	ProbeOutcomeFreeTier429
	// ProbeOutcomeUnclassified 5xx/超时/传输错误/其他 429/401/403/非免费档 429 → 仅置 attempted_at。
	ProbeOutcomeUnclassified
)

// ComputeProbeUpperBound 是上限公式的单一事实源（C）。两口径统一于此：
//   - 普通场景（候选总数 ≤60 且窗口无先前占用）：无额外预算等待项（0 分钟），仅
//     1×轮次周期 + 请求硬超时 + 启动冷却 60s；
//   - 最坏场景（冻结值 >60）：预算等待项 = ceil(候选总数/60) 分钟（例如 100 → 2 分钟）
//   - 1×轮次周期 + 请求硬超时 + 启动冷却。
//
// 两类共用同一计算，展示/告警/验收读同一结果，不与「普通场景无预算等待项」矛盾：
// 普通场景由候选总数 ≤60 触发 0 预算分钟分支。公式语义 = 下一次探测尝试的完成界，
// 不是有限恢复时间保证；不可分类结果只更新 attempt 时间，超出单次尝试界后由 stale
// 告警暴露，不静默丢弃。
func ComputeProbeUpperBound(candidateTotal int, roundPeriod, hardTimeout, startupCooldown time.Duration) time.Duration {
	budgetMinutes := 0
	if candidateTotal > probeBudgetPerMinute {
		budgetMinutes = (candidateTotal + probeBudgetPerMinute - 1) / probeBudgetPerMinute
	}
	return time.Duration(budgetMinutes)*time.Minute + roundPeriod + hardTimeout + startupCooldown
}

const probeBudgetPerMinute = 60

func accountIDKey(accountID int64, modelKey string) string {
	return strconv.FormatInt(accountID, 10) + ":" + modelKey
}

// freezeCandidateBound 在候选首次进入调度时冻结其候选总数（C：冻结，不得因早期候选
// 恢复离开而缩短尾部候选的阈值）。后续进入不再覆盖。
func (p *AccountHealthRecoveryProbeService) freezeCandidateBound(accountID int64, modelKey string, total int) {
	p.frozenMu.Lock()
	defer p.frozenMu.Unlock()
	key := accountIDKey(accountID, modelKey)
	if _, ok := p.frozenBounds[key]; !ok {
		p.frozenBounds[key] = total
	}
}

// frozenCandidateBound 读取冻结值（未冻结返回 0）。
func (p *AccountHealthRecoveryProbeService) frozenCandidateBound(accountID int64, modelKey string) int {
	v, _ := p.frozenCandidateBoundOK(accountID, modelKey)
	return v
}

// frozenCandidateBoundOK 读取冻结值并报告该 key 是否确实冻结过（区别于冻结值 0）。
func (p *AccountHealthRecoveryProbeService) frozenCandidateBoundOK(accountID int64, modelKey string) (int, bool) {
	p.frozenMu.Lock()
	defer p.frozenMu.Unlock()
	v, ok := p.frozenBounds[accountIDKey(accountID, modelKey)]
	return v, ok
}

// clearCandidateBound 清除某 (account, scope) 的冻结值。冻结值生命周期绑定当前异常候选
// 周期：权威恢复写入口（模型级 Success 幂等清除 / 账号级恢复）成功后清除对应 key，下一轮
// 进入调度时按当时候选总数重新冻结，不复用上一轮（可能已过期的）候选总数。modelKey==""
// 为账号级维度。
func (p *AccountHealthRecoveryProbeService) clearCandidateBound(accountID int64, modelKey string) {
	if p == nil {
		return
	}
	p.frozenMu.Lock()
	defer p.frozenMu.Unlock()
	delete(p.frozenBounds, accountIDKey(accountID, modelKey))
}

// AccountFreshnessUpperBound 返回某 (account, modelKey) 的有效上界（D4 三维阈值公式的
// 账号侧输入）：复用冻结候选总数经工作项 1 上界公式导出。modelKey=="" 表示账号级维度。
// 未冻结（key 不存在，该候选尚未进入调度）直接返回 0 —— 调用方以 0 视为「空集合」走
// 基线；只有确实冻结过的候选才计算探测上界，避免未进入调度的 scope 得到与唯一公式
// 不一致的阈值。
func (p *AccountHealthRecoveryProbeService) AccountFreshnessUpperBound(accountID int64, modelKey string) time.Duration {
	if p == nil {
		return 0
	}
	total, frozen := p.frozenCandidateBoundOK(accountID, modelKey)
	if !frozen {
		return 0
	}
	return ComputeProbeUpperBound(total, probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown)
}

// recordAccountLevelObservation 记录账号级候选的观察/尝试时间（F，两维度共用入口）。
// 成功→observed_at；失败/降级/传输错误→attempted_at；绝不清除任何模型级状态。
// 返回 ApplyModelRateLimitObservation 的提交错误（#2：透传以便调用点按提交失败做失败关闭）；
// nil 守卫分支返回 nil。函数内 freshness 评估错误维持 Warn-only（评估不影响状态迁移），
// 不并入返回错误。
func (p *AccountHealthRecoveryProbeService) recordAccountLevelObservation(ctx context.Context, accountID int64, ok bool, probeErr error) error {
	if p == nil || p.rateLimit == nil {
		return nil
	}
	outcome := ProbeOutcomeUnclassified
	if probeErr == nil && ok {
		outcome = ProbeOutcomeSuccess
	}
	err := p.rateLimit.ApplyModelRateLimitObservation(ctx, accountID, tokenHarborAccountLevelProbeScope, ModelRateLimitObservation{
		EventTime:    time.Now(),
		Outcome:      outcome,
		AccountLevel: true,
	})
	// D4：账号级候选（temp-unschedulable/熔断）在既有恢复探测路径上同样评估陈旧——
	// 同一探测链、同一状态入口，账号级维度不被漏评估。评估失败不影响状态迁移，Warn-only。
	if p.freshnessAlerts != nil {
		if ferr := p.freshnessAlerts.EvaluateAccountLevelFreshness(ctx, accountID); ferr != nil {
			logger.L().Warn("freshness_alert_evaluate_failed",
				zap.Int64("account_id", accountID),
				zap.Bool("account_level", true),
				zap.Error(ferr))
		}
	}
	return err
}

// isAccountLevelStale 报告账号级探测是否已超出阈值未观察到权威响应（F：stale 告警）。
//
// observed_at 的类型判别复用 service 包单一判别口径 observedAtFromEntry（视图/告警同一
// 事实源，E21），解析复用 parseObservedAt（E16）。键不存在 / nil / 空串 / 仅空白 = 无观测，
// 既有「回退 attempted_at 判定」语义保持不变；存在但非法（类型非 string 或格式不可解析）=
// 权威数据损坏 → 返回明确错误令调用方失败关闭（跳过本轮状态变更），绝不静默降级为
// 「有新鲜观测」而漏告警（与 E16/E21 同口径）。
func (p *AccountHealthRecoveryProbeService) isAccountLevelStale(ctx context.Context, accountID int64, threshold time.Duration, now time.Time) (bool, error) {
	if p == nil || p.rateLimit == nil {
		return false, nil
	}
	entry, err := p.rateLimit.GetModelRateLimitObservation(ctx, accountID, tokenHarborAccountLevelProbeScope)
	if err != nil || entry == nil {
		return false, err
	}
	// observed_at：类型判别经 observedAtFromEntry（单一判别口径），解析经 parseObservedAt
	// （单一解析口径）。存在但非法 → 明确错误失败关闭，不再静默忽略后回退 attempted_at。
	rawObserved, oerr := observedAtFromEntry(entry)
	if oerr != nil {
		return false, oerr
	}
	observedAt, hasObserved, perr := parseObservedAt(rawObserved)
	if perr != nil {
		return false, perr
	}
	var last time.Time
	if hasObserved {
		last = observedAt
	}
	// attempted_at：同类类型断言按同口径失败关闭（键不存在 / nil = 无值；存在但类型非
	// string = 损坏 → 明确错误）。attempted_at 仅作回退判定，不解除告警（既有语义不变）。
	if raw, exists := entry[entryAttemptedAtKey]; exists && raw != nil {
		v, ok := raw.(string)
		if !ok {
			return false, fmt.Errorf("invalid attempted_at: wrong type %T (want string), authoritative data corrupted", raw)
		}
		if strings.TrimSpace(v) != "" {
			t, e := time.Parse(time.RFC3339, v)
			if e != nil {
				return false, fmt.Errorf("invalid attempted_at %q: %w", v, e)
			}
			if t.After(last) {
				last = t
			}
		}
	}
	if last.IsZero() {
		return false, nil
	}
	return now.Sub(last) > threshold, nil
}
