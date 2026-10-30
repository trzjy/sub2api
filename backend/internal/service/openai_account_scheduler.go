package service

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"golang.org/x/sync/singleflight"
)

const (
	openAIAccountScheduleLayerPreviousResponse = "previous_response_id"
	openAIAccountScheduleLayerGuardianParent   = "guardian_parent"
	openAIAccountScheduleLayerSessionSticky    = "session_hash"
	openAIAccountScheduleLayerLoadBalance      = "load_balance"
	openAIAdvancedSchedulerSettingKey          = "openai_advanced_scheduler_enabled"
)

const (
	openAIAdvancedSchedulerSettingCacheTTL  = 5 * time.Second
	openAIAdvancedSchedulerSettingDBTimeout = 2 * time.Second
	// ponytail: cap probes added when cost ordering expands configured Top-K;
	// use bulk acquisition if a measured workload needs a higher ceiling.
	openAIAccountSelectionProbeLimit = 64
)

const (
	openAIQuotaHeadroomNeutralFactor           = 0.5
	openAIQuotaHeadroomSecondaryLowRemain      = 0.10
	openAIQuotaHeadroomSnapshotStaleAfter      = 8 * time.Hour
	openAIUpstreamCostNeutralFactor            = 0.5
	defaultOpenAIOAuthSchedulingRateMultiplier = 1.0
)

type cachedOpenAIAdvancedSchedulerSetting struct {
	lowUpstreamRatePriorityEnabled bool
	oauthSchedulingRateMultiplier  *float64
	enabled                        bool
	stickyWeightedEnabled          bool
	subscriptionPriorityEnabled    bool
	lbTopKOverride                 int
	weightOverrides                map[string]float64
	expiresAt                      int64
}

type openAIAdvancedSchedulerRuntimeSettings struct {
	lowUpstreamRatePriorityEnabled bool
	oauthSchedulingRateMultiplier  *float64
	enabled                        bool
	stickyWeightedEnabled          bool
	subscriptionPriorityEnabled    bool
	lbTopKOverride                 int
	weightOverrides                map[string]float64
}

var openAIAdvancedSchedulerSettingCache atomic.Value // *cachedOpenAIAdvancedSchedulerSetting
var openAIAdvancedSchedulerSettingSF singleflight.Group

type OpenAIAccountScheduleRequest struct {
	GroupID                 *int64
	Platform                string
	SessionHash             string
	StickyAccountID         int64
	GuardianParentAccountID int64
	StickyPreviousAccountID int64
	StickyWeighted          bool
	SubscriptionPriority    bool
	PreserveStickyBinding   bool
	// DisableStickyEscape keeps task-owner lookups on their account even when
	// generic sticky health or concurrency heuristics would prefer another.
	DisableStickyEscape     bool
	RequirePrivacySet       bool
	PreviousResponseID      string
	PreviousResponseCanMove bool
	UseUpstreamTokenCost    bool
	RequestedModel          string
	RequiredTransport       OpenAIUpstreamTransport
	RequiredCapability      OpenAIEndpointCapability
	RequiredImageCapability OpenAIImagesCapability
	// RequireCompact is only for legacy /responses/compact capability filtering
	// and compact_model_mapping; native remote compaction v2 leaves it false.
	RequireCompact bool
	// RequireVision 表示请求含图（有图片输入），选号时排除已知不支持视觉的账号
	// （docs-local/capability-routing-plan.md §3.4）；未标记的账号按 unknown 放行。
	RequireVision bool
	// VisionRoutingTargets 是带图请求命中 groups.vision_routing 配置时的目标账号
	// ID 集合（docs-local/capability-routing-plan.md §3.7）。非空时候选池正向收窄为该
	// 集合，且仍叠加 §3.4 的 vision_not_supported 能力过滤（只收窄、不豁免）。
	// 由选号入口每次请求最多读取一次后透传，避免候选循环内重复查库。
	VisionRoutingTargets []int64
	// TierRoutingPool 是分层路由（派发单 OC-T1 泛化，docs-local/model-source-tier-routing-plan.md
	// §2）的分档候选约束："masked" 仅伪装线池（qwen 伪装线，extra.mask_upstream_identity=true），
	// "upstream" 仅非伪装池（星思）。空值不约束——handler 未对请求装分档门时零行为变化，
	// 装门后平台无关（不限于 kimi）。由 selectAccountWithSchedulerOnce 按请求 ctx 中的分档状态装门。
	TierRoutingPool string
	ExcludedIDs     map[int64]struct{}
}

type OpenAIAccountScheduleDecision struct {
	Layer               string
	StickyPreviousHit   bool
	StickySessionHit    bool
	CandidateCount      int
	TopK                int
	LatencyMs           int64
	LoadSkew            float64
	SelectedAccountID   int64
	SelectedAccountType string
}

type OpenAIAccountSchedulerMetricsSnapshot struct {
	SelectTotal              int64
	StickyPreviousHitTotal   int64
	StickySessionHitTotal    int64
	LoadBalanceSelectTotal   int64
	AccountSwitchTotal       int64
	SchedulerLatencyMsTotal  int64
	SchedulerLatencyMsAvg    float64
	StickyHitRatio           float64
	AccountSwitchRate        float64
	LoadSkewAvg              float64
	RuntimeStatsAccountCount int
}

type OpenAIAccountScheduler interface {
	Select(ctx context.Context, req OpenAIAccountScheduleRequest) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error)
	ReportResult(accountID int64, success bool, firstTokenMs *int)
	ReportSwitch()
	SnapshotMetrics() OpenAIAccountSchedulerMetricsSnapshot
}

type openAIAccountSchedulerMetrics struct {
	selectTotal            atomic.Int64
	stickyPreviousHitTotal atomic.Int64
	stickySessionHitTotal  atomic.Int64
	stickyHitTotal         atomic.Int64
	loadBalanceSelectTotal atomic.Int64
	accountSwitchTotal     atomic.Int64
	latencyMsTotal         atomic.Int64
	loadSkewMilliTotal     atomic.Int64
}

type openAIAccountLoadPlan struct {
	allCandidates             []openAIAccountCandidateScore
	candidates                []openAIAccountCandidateScore
	staleSnapshotCompactRetry []openAIAccountCandidateScore
	selectionOrder            []openAIAccountCandidateScore
	candidateCount            int
	topK                      int
	loadSkew                  float64
	includeOverflowFallback   bool
}

type openAIAccountLoadSelectionAttempt struct {
	result              *AccountSelectionResult
	selectionOrder      []openAIAccountCandidateScore
	candidateCount      int
	topK                int
	loadSkew            float64
	compactBlocked      bool
	noCompactCandidates bool
	err                 error
}

func (m *openAIAccountSchedulerMetrics) recordSelect(decision OpenAIAccountScheduleDecision) {
	if m == nil {
		return
	}
	m.selectTotal.Add(1)
	m.latencyMsTotal.Add(decision.LatencyMs)
	m.loadSkewMilliTotal.Add(int64(math.Round(decision.LoadSkew * 1000)))
	if decision.StickyPreviousHit {
		m.stickyPreviousHitTotal.Add(1)
	}
	if decision.StickySessionHit {
		m.stickySessionHitTotal.Add(1)
	}
	if decision.StickyPreviousHit || decision.StickySessionHit {
		m.stickyHitTotal.Add(1)
	}
	if decision.Layer == openAIAccountScheduleLayerLoadBalance {
		m.loadBalanceSelectTotal.Add(1)
	}
}

func (m *openAIAccountSchedulerMetrics) recordSwitch() {
	if m == nil {
		return
	}
	m.accountSwitchTotal.Add(1)
}

type openAIAccountRuntimeStats struct {
	accounts     sync.Map
	accountCount atomic.Int64
}

type openAIAccountRuntimeStat struct {
	errorRateEWMABits atomic.Uint64
	ttftEWMABits      atomic.Uint64
}

func newOpenAIAccountRuntimeStats() *openAIAccountRuntimeStats {
	return &openAIAccountRuntimeStats{}
}

func (s *openAIAccountRuntimeStats) loadOrCreate(accountID int64) *openAIAccountRuntimeStat {
	if value, ok := s.accounts.Load(accountID); ok {
		stat, _ := value.(*openAIAccountRuntimeStat)
		if stat != nil {
			return stat
		}
	}

	stat := &openAIAccountRuntimeStat{}
	stat.ttftEWMABits.Store(math.Float64bits(math.NaN()))
	actual, loaded := s.accounts.LoadOrStore(accountID, stat)
	if !loaded {
		s.accountCount.Add(1)
		return stat
	}
	existing, _ := actual.(*openAIAccountRuntimeStat)
	if existing != nil {
		return existing
	}
	return stat
}

func updateEWMAAtomic(target *atomic.Uint64, sample float64, alpha float64) {
	for {
		oldBits := target.Load()
		oldValue := math.Float64frombits(oldBits)
		newValue := alpha*sample + (1-alpha)*oldValue
		if target.CompareAndSwap(oldBits, math.Float64bits(newValue)) {
			return
		}
	}
}

func (s *openAIAccountRuntimeStats) report(accountID int64, success bool, firstTokenMs *int) {
	if s == nil || accountID <= 0 {
		return
	}
	const alpha = 0.2
	stat := s.loadOrCreate(accountID)

	errorSample := 1.0
	if success {
		errorSample = 0.0
	}
	updateEWMAAtomic(&stat.errorRateEWMABits, errorSample, alpha)

	if firstTokenMs != nil && *firstTokenMs > 0 {
		ttft := float64(*firstTokenMs)
		ttftBits := math.Float64bits(ttft)
		for {
			oldBits := stat.ttftEWMABits.Load()
			oldValue := math.Float64frombits(oldBits)
			if math.IsNaN(oldValue) {
				if stat.ttftEWMABits.CompareAndSwap(oldBits, ttftBits) {
					break
				}
				continue
			}
			newValue := alpha*ttft + (1-alpha)*oldValue
			if stat.ttftEWMABits.CompareAndSwap(oldBits, math.Float64bits(newValue)) {
				break
			}
		}
	}
}

func (s *openAIAccountRuntimeStats) snapshot(accountID int64) (errorRate float64, ttft float64, hasTTFT bool) {
	if s == nil || accountID <= 0 {
		return 0, 0, false
	}
	value, ok := s.accounts.Load(accountID)
	if !ok {
		return 0, 0, false
	}
	stat, _ := value.(*openAIAccountRuntimeStat)
	if stat == nil {
		return 0, 0, false
	}
	errorRate = clamp01(math.Float64frombits(stat.errorRateEWMABits.Load()))
	ttftValue := math.Float64frombits(stat.ttftEWMABits.Load())
	if math.IsNaN(ttftValue) {
		return errorRate, 0, false
	}
	return errorRate, ttftValue, true
}

func (s *openAIAccountRuntimeStats) size() int {
	if s == nil {
		return 0
	}
	return int(s.accountCount.Load())
}

type defaultOpenAIAccountScheduler struct {
	service                *OpenAIGatewayService
	metrics                openAIAccountSchedulerMetrics
	stats                  *openAIAccountRuntimeStats
	grokFreeQuotaGateCache sync.Map // key: int64(accountID), value: grokFreeQuotaGateCacheEntry
	// codeBuddyCatalogProvider 提供账号级上游模型目录查询，用于聚合分组逐模型
	// 闸门（§2.1-1）。默认实现从账号既有 UpstreamModelMetadataSnapshot 读取；
	// 测试可注入 fake。nil 时回落到默认快照实现。
	codeBuddyCatalogProvider codeBuddyCatalogProvider
}

// codeBuddyCatalogProvider 查询单个 codebuddy 账号的实时上游模型目录。
// 实现必须遵循失败关闭：不可达/超时/空集合/非集合结构一律返回 error，
// 由闸门计为 upstream_catalog_unavailable，绝不兜底放行（合同 §5）。
type codeBuddyCatalogProvider interface {
	UpstreamModelIDs(ctx context.Context, account *Account) ([]string, error)
}

// codeBuddyCatalogVerdict 是聚合分组逐模型目录闸门的判定结果。
type codeBuddyCatalogVerdict int

const (
	// codeBuddyCatalogAllowed 映射后的上游模型在账号目录中，账号可进入候选。
	codeBuddyCatalogAllowed codeBuddyCatalogVerdict = iota
	// codeBuddyCatalogModelMissing 映射后的上游模型不在账号目录，本请求不可选。
	codeBuddyCatalogModelMissing
	// codeBuddyCatalogUnavailable 目录不可达/超时/空集合/非集合结构，本请求不可选。
	codeBuddyCatalogUnavailable
	// codeBuddyCatalogNotProbed 超出本轮查询预算（>4 候选），本轮不探测、不选，非错误态。
	codeBuddyCatalogNotProbed
)

const (
	// codeBuddyCatalogProbeConcurrency 单次选号决策内并发探测上游目录的上限（§2.1-1）。
	codeBuddyCatalogProbeConcurrency = 4
	// codeBuddyCatalogProbeTimeout 单次选号决策内目录查询总时限（§2.1-1：≤300ms）。
	codeBuddyCatalogProbeTimeout = 300 * time.Millisecond
)

// errCodeBuddyCatalogUnavailable 表示账号级上游模型目录不可用（失败关闭，非兜底）。
var errCodeBuddyCatalogUnavailable = errors.New("codebuddy upstream catalog unavailable")

// codeBuddySnapshotCatalogProvider 是 codeBuddyCatalogProvider 的默认实现：
// 直接读取账号既有 UpstreamModelMetadataSnapshot（站点既有目录缓存机制）。
// 快照为空/不可解析即视为不可用，失败关闭。
type codeBuddySnapshotCatalogProvider struct{}

func (codeBuddySnapshotCatalogProvider) UpstreamModelIDs(ctx context.Context, account *Account) ([]string, error) {
	if account == nil {
		return nil, errCodeBuddyCatalogUnavailable
	}
	snapshot := account.GetUpstreamModelMetadataSnapshot()
	if snapshot == nil || len(snapshot.Models) == 0 {
		return nil, errCodeBuddyCatalogUnavailable
	}
	ids := make([]string, 0, len(snapshot.Models))
	for id := range snapshot.Models {
		ids = append(ids, id)
	}
	return ids, nil
}

// catalogProvider 返回注入的目录提供者，未注入时回落默认快照实现。
func (s *defaultOpenAIAccountScheduler) catalogProvider() codeBuddyCatalogProvider {
	if s != nil && s.codeBuddyCatalogProvider != nil {
		return s.codeBuddyCatalogProvider
	}
	return codeBuddySnapshotCatalogProvider{}
}

// codeBuddyPlatformMismatchAllowed 判断 codebuddy 账号在聚合分组请求下是否能越过
// platform_mismatch：聚合平台（deepseek/zhipu/kimi/minimax/other/codebuddy）请求下的
// codebuddy OpenAI 兼容账号可纳入候选（§2.1-2）。非聚合平台（openai/grok 等）不放行。
func codeBuddyPlatformMismatchAllowed(account *Account, req OpenAIAccountScheduleRequest) bool {
	if account == nil || account.Platform != PlatformCodeBuddy || !account.IsOpenAICompatible() {
		return false
	}
	return isCodeBuddyAggregatedPlatform(req.Platform)
}

// codeBuddyAggregatedCatalogGated 判断该账号是否由聚合逐模型目录闸门接管模型校验：
// 聚合分组请求下的 codebuddy 账号跳过客户端名白名单（IsModelSupported），
// 改由目录闸门（先 GetMappedModel 映射为上游名再精确比较）判定（§2.1-1）。
func codeBuddyAggregatedCatalogGated(account *Account, req OpenAIAccountScheduleRequest) bool {
	return account != nil && account.Platform == PlatformCodeBuddy && isCodeBuddyAggregatedPlatform(req.Platform)
}

// evaluateCodeBuddyCatalogGate 计算聚合分组请求下 codebuddy 候选账号的目录闸门判定。
// 仅在 isCodeBuddyAggregatedPlatform(req.Platform) 且 RequestedModel 非空时生效；
// 其余情况返回 nil（不参与排除）。返回 map[accountID]verdict。
//
// 预算（§2.1-1，防热路径放大）：候选 codebuddy 账号按 Priority 降序、并列 ID 升序
// 确定性排序；固定查询集合 = 前 codeBuddyCatalogProbeConcurrency(4) 个，并发 ≤4、
// 总时限 ≤300ms、零重试；超出预算的候选显式计 upstream_catalog_not_probed（非静默跳过）。
//
// 快照后强制重执行：每次选号都按当前账号目录实时判定（不缓存目录结论），
// 快照命中不豁免（§2.1-3）。
func (s *defaultOpenAIAccountScheduler) evaluateCodeBuddyCatalogGate(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	accounts []Account,
) map[int64]codeBuddyCatalogVerdict {
	if !isCodeBuddyAggregatedPlatform(req.Platform) || req.RequestedModel == "" {
		return nil
	}
	candidates := make([]*Account, 0, len(accounts))
	for i := range accounts {
		if accounts[i].Platform == PlatformCodeBuddy {
			candidates = append(candidates, &accounts[i])
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// 确定性排序：Priority 降序，并列 ID 升序。
	order := make([]*Account, len(candidates))
	copy(order, candidates)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Priority != order[j].Priority {
			return order[i].Priority > order[j].Priority
		}
		return order[i].ID < order[j].ID
	})

	verdicts := make(map[int64]codeBuddyCatalogVerdict, len(order))
	provider := s.catalogProvider()

	probeCtx, cancel := context.WithTimeout(ctx, codeBuddyCatalogProbeTimeout)
	defer cancel()

	// 超出预算的候选在启动任何探测协程之前由主协程一次性预写 not_probed
	// （显式计数，绝不静默跳过）；此后仅探测协程在 mu 保护下写 verdicts，
	// 避免主协程与探测协程对同一 map 的 concurrent writes（可崩进程）。
	probeCount := len(order)
	if probeCount > codeBuddyCatalogProbeConcurrency {
		probeCount = codeBuddyCatalogProbeConcurrency
	}
	for _, acc := range order[probeCount:] {
		verdicts[acc.ID] = codeBuddyCatalogNotProbed
	}

	sem := make(chan struct{}, codeBuddyCatalogProbeConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, acc := range order[:probeCount] {
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			v := probeCodeBuddyCatalog(probeCtx, provider, acc, req.RequestedModel)
			mu.Lock()
			verdicts[acc.ID] = v
			mu.Unlock()
		}(acc)
	}
	wg.Wait()
	return verdicts
}

// probeCodeBuddyCatalog 探测单个 codebuddy 账号的目录闸门：
// 先按账号级 GetMappedModel 把客户端请求模型映射为上游名，再以
// 上游名与账号目录精确比较（§2.1-1 判定顺序：禁止客户端名直比、禁止映射后跳过比较）。
func probeCodeBuddyCatalog(
	ctx context.Context,
	provider codeBuddyCatalogProvider,
	account *Account,
	requestedModel string,
) codeBuddyCatalogVerdict {
	upstreamModel := account.GetMappedModel(requestedModel)
	ids, err := provider.UpstreamModelIDs(ctx, account)
	if err != nil || len(ids) == 0 {
		// 目录不可达/超时/空集合/非集合结构 → 不可用，失败关闭，禁兜底。
		return codeBuddyCatalogUnavailable
	}
	for _, id := range ids {
		if id == upstreamModel {
			return codeBuddyCatalogAllowed
		}
	}
	return codeBuddyCatalogModelMissing
}

type openAISelectionProbeBudget struct {
	acquires  int
	rechecks  int
	attempted map[int64]struct{}
	limited   bool
}

func newOpenAISelectionProbeBudget() *openAISelectionProbeBudget {
	return &openAISelectionProbeBudget{attempted: make(map[int64]struct{})}
}

func (b *openAISelectionProbeBudget) enableLimit() {
	if b != nil {
		b.limited = true
	}
}

func (b *openAISelectionProbeBudget) recordAcquire(accountID int64) bool {
	if b == nil {
		return false
	}
	if !b.limited {
		return true
	}
	if b.acquires >= openAIAccountSelectionProbeLimit {
		return false
	}
	if b.attempted == nil {
		b.attempted = make(map[int64]struct{})
	}
	b.acquires++
	b.attempted[accountID] = struct{}{}
	return true
}

func (b *openAISelectionProbeBudget) recordRecheck() bool {
	if b == nil {
		return false
	}
	if !b.limited {
		return true
	}
	if b.rechecks >= openAIAccountSelectionProbeLimit {
		return false
	}
	b.rechecks++
	return true
}

func (b *openAISelectionProbeBudget) acquireExhausted() bool {
	return b != nil && b.limited && b.acquires >= openAIAccountSelectionProbeLimit
}

func (b *openAISelectionProbeBudget) wasAttempted(accountID int64) bool {
	if b == nil {
		return false
	}
	_, ok := b.attempted[accountID]
	return ok
}

type openAIStickyEscapeConfig struct {
	enabled   bool
	ttftMs    float64
	errorRate float64
}

func newDefaultOpenAIAccountScheduler(service *OpenAIGatewayService, stats *openAIAccountRuntimeStats) OpenAIAccountScheduler {
	if stats == nil {
		stats = newOpenAIAccountRuntimeStats()
	}
	return &defaultOpenAIAccountScheduler{
		service: service,
		stats:   stats,
	}
}

func (s *defaultOpenAIAccountScheduler) Select(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
) (selection *AccountSelectionResult, decision OpenAIAccountScheduleDecision, err error) {
	if s != nil && s.service != nil && s.service.openAIGroupRequiresPrivacySet(ctx, req.GroupID) {
		req.RequirePrivacySet = true
	}
	start := time.Now()
	// 命名返回值保证 defer 写入的耗时同时返回给调用方。
	defer func() {
		decision.LatencyMs = time.Since(start).Milliseconds()
		s.metrics.recordSelect(decision)
	}()

	previousResponseID := strings.TrimSpace(req.PreviousResponseID)
	if previousResponseID != "" && NormalizeOpenAICompatiblePlatform(req.Platform) == PlatformOpenAI &&
		(!req.StickyWeighted || !req.PreviousResponseCanMove) {
		selection, err := s.service.selectAccountByPreviousResponseIDForCapability(
			ctx,
			req.GroupID,
			previousResponseID,
			req.RequestedModel,
			req.ExcludedIDs,
			req.RequiredCapability,
			req.RequireCompact,
		)
		if err != nil {
			return nil, decision, err
		}
		if selection != nil && selection.Account != nil {
			compatible, _ := s.isAccountRequestCompatibleReason(ctx, selection.Account, req)
			hasGroupMetadata := len(selection.Account.GroupIDs) > 0 || len(selection.Account.AccountGroups) > 0
			groupCompatible := !hasGroupMetadata || openAIStickyAccountMatchesGroup(selection.Account, req.GroupID)
			if hasGroupMetadata && s.service != nil {
				groupCompatible = s.service.openAIAccountMatchesSchedulingGroup(selection.Account, req.GroupID)
			}
			if !groupCompatible ||
				!compatible || !s.isAccountTransportCompatible(selection.Account, req.RequiredTransport) {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				selection = nil
			}
		}
		if selection != nil && selection.Account != nil {
			decision.Layer = openAIAccountScheduleLayerPreviousResponse
			decision.StickyPreviousHit = true
			decision.SelectedAccountID = selection.Account.ID
			decision.SelectedAccountType = selection.Account.Type
			if req.SessionHash != "" {
				_ = s.service.bindOpenAIStickySessionDuringSelection(ctx, req.GroupID, req.SessionHash, selection.Account.ID)
			}
			return selection, decision, nil
		}
	}

	if req.GuardianParentAccountID > 0 {
		parentReq := req
		parentReq.StickyAccountID = req.GuardianParentAccountID
		parentReq.PreserveStickyBinding = true
		selection, _, err := s.selectBySessionHash(ctx, parentReq)
		if err != nil {
			return nil, decision, err
		}
		if selection != nil && selection.Account != nil {
			decision.Layer = openAIAccountScheduleLayerGuardianParent
			decision.StickySessionHit = true
			decision.SelectedAccountID = selection.Account.ID
			decision.SelectedAccountType = selection.Account.Type
			return selection, decision, nil
		}
	}

	if !req.StickyWeighted {
		selection, escapedSticky, err := s.selectBySessionHash(ctx, req)
		if err != nil {
			return nil, decision, err
		}
		if selection != nil && selection.Account != nil {
			decision.Layer = openAIAccountScheduleLayerSessionSticky
			decision.StickySessionHit = true
			decision.SelectedAccountID = selection.Account.ID
			decision.SelectedAccountType = selection.Account.Type
			return selection, decision, nil
		}
		if escapedSticky {
			req.PreserveStickyBinding = true
		}
	}

	selection, candidateCount, topK, loadSkew, err := s.selectByLoadBalance(ctx, req)
	decision.Layer = openAIAccountScheduleLayerLoadBalance
	decision.CandidateCount = candidateCount
	decision.TopK = topK
	decision.LoadSkew = loadSkew
	if err != nil {
		return nil, decision, err
	}
	if selection != nil && selection.Account != nil {
		decision.SelectedAccountID = selection.Account.ID
		decision.SelectedAccountType = selection.Account.Type
		if req.StickyWeighted {
			if req.StickyPreviousAccountID > 0 && selection.Account.ID == req.StickyPreviousAccountID {
				decision.StickyPreviousHit = true
			}
			if req.StickyAccountID > 0 && selection.Account.ID == req.StickyAccountID {
				decision.StickySessionHit = true
			}
		}
	}
	return selection, decision, nil
}

func (s *defaultOpenAIAccountScheduler) selectBySessionHash(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
) (*AccountSelectionResult, bool, error) {
	sessionHash := strings.TrimSpace(req.SessionHash)
	if sessionHash == "" || s == nil || s.service == nil || s.service.cache == nil {
		return nil, false, nil
	}

	accountID := req.StickyAccountID
	clearBinding := func() {
		if !req.PreserveStickyBinding {
			_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, sessionHash)
		}
	}
	if accountID <= 0 {
		var err error
		accountID, err = s.service.getStickySessionAccountID(ctx, req.GroupID, sessionHash)
		if err != nil || accountID <= 0 {
			return nil, false, nil
		}
	}
	if accountID <= 0 {
		return nil, false, nil
	}
	if req.ExcludedIDs != nil {
		if _, excluded := req.ExcludedIDs[accountID]; excluded {
			return nil, false, nil
		}
	}

	account, err := s.service.getSchedulableAccount(ctx, accountID)
	if err != nil || account == nil {
		clearBinding()
		return nil, false, nil
	}
	// B3 sticky 不入 Web 池：Web 接入模式账号不得经粘性会话路由，清理绑定后回落两级池。
	if account.IsWebAccessMode() {
		clearBinding()
		return nil, false, nil
	}
	// 显式非法 access_mode fail-closed：粘性命中也判为不可选（§5.1 规则 2）。
	if openAIAccessModeExcludeReason(account) != "" {
		clearBinding()
		return nil, false, nil
	}
	if shouldClearStickySession(account, req.RequestedModel) || account.Platform != NormalizeOpenAICompatiblePlatform(req.Platform) || !account.IsOpenAICompatible() || !account.IsSchedulable() {
		clearBinding()
		return nil, false, nil
	}
	if !s.isAccountRequestCompatible(ctx, account, req) {
		return nil, false, nil
	}
	if !s.isAccountTransportCompatible(account, req.RequiredTransport) {
		clearBinding()
		return nil, false, nil
	}
	account = s.service.recheckSelectedOpenAIAccountFromDB(ctx, account, req.GroupID, req.Platform, req.RequestedModel, req.RequireCompact, req.RequiredCapability)
	if account == nil || !s.service.openAIAccountMatchesSchedulingGroup(account, req.GroupID) || !s.isAccountRequestCompatible(ctx, account, req) || !s.isAccountTransportCompatible(account, req.RequiredTransport) {
		clearBinding()
		return nil, false, nil
	}
	// CodeBuddy 平台默认 RPM：粘性允许黄区（StickyOnly），红区才清绑定换号。
	if !s.codeBuddyRPMAllowsSticky(ctx, account) {
		clearBinding()
		return nil, false, nil
	}
	// Free-tier soft gate: sticky session must not pin an over-quota free OAuth account.
	// Admin QueryQuota / import probes do not use this path.
	if account != nil && len(s.filterGrokFreeQuotaAccounts(ctx, []Account{*account})) == 0 {
		clearBinding()
		return nil, false, nil
	}
	// Team+model cool: sticky must not pin a sibling under the same team 429 window.
	now := time.Now()
	upstreamModel := canonicalOpenAIAccountSchedulingModel(account, req.RequestedModel)
	if account != nil && isGrokTeamModelRateLimited(account, upstreamModel, now) {
		clearBinding()
		return nil, false, nil
	}
	if account != nil && isGrokModelQuotaBlocked(account.ID, upstreamModel, now) {
		clearBinding()
		return nil, false, nil
	}
	escapeCfg := s.service.openAIStickyEscapeConfig()
	if reason, errorRate, ttft, shouldEscape := s.shouldEscapeStickyAccount(accountID, escapeCfg); shouldEscape && !req.DisableStickyEscape {
		slog.Info("sticky_escape_triggered",
			"account_id", accountID,
			"reason", reason,
			"error_rate", errorRate,
			"ttft", ttft,
		)
		return nil, true, nil
	}
	result, acquireErr := s.service.tryAcquireAccountSlot(ctx, accountID, account.Concurrency)
	if acquireErr != nil && req.DisableStickyEscape {
		return nil, false, acquireErr
	}
	if acquireErr == nil && result != nil && result.Acquired {
		if !req.PreserveStickyBinding {
			_ = s.service.refreshStickySessionTTL(ctx, req.GroupID, sessionHash, s.service.openAIWSSessionStickyTTL())
		}
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{
			Account:     account,
			Acquired:    true,
			ReleaseFunc: result.ReleaseFunc,
		}), false, nil
	}

	cfg := s.service.schedulingConfig()
	// WaitPlan.MaxConcurrency 使用 Concurrency（非 EffectiveLoadFactor），因为 WaitPlan 控制的是 Redis 实际并发槽位等待。
	if s.service.concurrencyService != nil {
		if escapeCfg.enabled && !req.DisableStickyEscape && acquireErr == nil && result != nil && !result.Acquired {
			errorRate, ttft, _ := s.stats.snapshot(accountID)
			slog.Info("sticky_escape_triggered",
				"account_id", accountID,
				"reason", "concurrency_full",
				"error_rate", errorRate,
				"ttft", ttft,
			)
			return nil, true, nil
		}
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{
			Account: account,
			WaitPlan: &AccountWaitPlan{
				AccountID:      accountID,
				MaxConcurrency: account.Concurrency,
				Timeout:        cfg.StickySessionWaitTimeout,
				MaxWaiting:     cfg.StickySessionMaxWaiting,
			},
		}), false, nil
	}
	return nil, false, nil
}

func openAIStickyAccountMatchesGroup(account *Account, groupID *int64) bool {
	if account == nil {
		return false
	}
	if groupID == nil {
		return len(account.AccountGroups) == 0 && len(account.GroupIDs) == 0
	}
	for _, accountGroupID := range account.GroupIDs {
		if accountGroupID == *groupID {
			return true
		}
	}
	for _, accountGroup := range account.AccountGroups {
		if accountGroup.GroupID == *groupID {
			return true
		}
	}
	return false
}

func openAIAccountSchedulingPriority(account *Account) int {
	if account == nil {
		return 0
	}
	return account.Priority
}

func (s *defaultOpenAIAccountScheduler) shouldEscapeStickyAccount(accountID int64, cfg openAIStickyEscapeConfig) (reason string, errorRate float64, ttft float64, shouldEscape bool) {
	if !cfg.enabled || s == nil || s.stats == nil || accountID <= 0 {
		return "", 0, 0, false
	}
	errorRate, ttft, hasTTFT := s.stats.snapshot(accountID)
	if hasTTFT && ttft > cfg.ttftMs {
		return "ttft", errorRate, ttft, true
	}
	if errorRate > cfg.errorRate {
		return "error_rate", errorRate, ttft, true
	}
	return "", errorRate, ttft, false
}

// openAISchedulingStandbyPromotedBonus 是备用开启时给 codebuddy 备用账号的固定顶分：
// 远高于任何权重组合（权重组上限为个位数量级），保证其进入 top-K 并在加权序列中显著领先。
// 仅参与排序计算，不写回 account.Priority，也不影响候选池构成（白名单与档位判定另行负责）。
const openAISchedulingStandbyPromotedBonus = 1e6

type openAIAccountCandidateScore struct {
	account   *Account
	loadInfo  *AccountLoadInfo
	loadKnown bool
	score     float64
	priority  int
	errorRate float64
	ttft      float64
	hasTTFT   bool
}

type openAIAccountCandidateHeap []openAIAccountCandidateScore

func (h openAIAccountCandidateHeap) Len() int {
	return len(h)
}

func (h openAIAccountCandidateHeap) Less(i, j int) bool {
	// 最小堆根节点保存“最差”候选，便于 O(log k) 维护 topK。
	return isOpenAIAccountCandidateBetter(h[j], h[i])
}

func (h openAIAccountCandidateHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *openAIAccountCandidateHeap) Push(x any) {
	candidate, ok := x.(openAIAccountCandidateScore)
	if !ok {
		panic("openAIAccountCandidateHeap: invalid element type")
	}
	*h = append(*h, candidate)
}

func (h *openAIAccountCandidateHeap) Pop() any {
	old := *h
	n := len(old)
	last := old[n-1]
	*h = old[:n-1]
	return last
}

func isOpenAIAccountCandidateBetter(left openAIAccountCandidateScore, right openAIAccountCandidateScore) bool {
	if left.score != right.score {
		return left.score > right.score
	}
	if left.account.Priority != right.account.Priority {
		return left.account.Priority < right.account.Priority
	}
	if left.loadInfo.LoadRate != right.loadInfo.LoadRate {
		return left.loadInfo.LoadRate < right.loadInfo.LoadRate
	}
	if left.loadInfo.WaitingCount != right.loadInfo.WaitingCount {
		return left.loadInfo.WaitingCount < right.loadInfo.WaitingCount
	}
	return left.account.ID < right.account.ID
}

func selectTopKOpenAICandidates(candidates []openAIAccountCandidateScore, topK int) []openAIAccountCandidateScore {
	if len(candidates) == 0 {
		return nil
	}
	if topK <= 0 {
		topK = 1
	}
	if topK >= len(candidates) {
		ranked := append([]openAIAccountCandidateScore(nil), candidates...)
		sort.Slice(ranked, func(i, j int) bool {
			return isOpenAIAccountCandidateBetter(ranked[i], ranked[j])
		})
		return ranked
	}

	best := make(openAIAccountCandidateHeap, 0, topK)
	for _, candidate := range candidates {
		if len(best) < topK {
			heap.Push(&best, candidate)
			continue
		}
		if isOpenAIAccountCandidateBetter(candidate, best[0]) {
			best[0] = candidate
			heap.Fix(&best, 0)
		}
	}

	ranked := make([]openAIAccountCandidateScore, len(best))
	copy(ranked, best)
	sort.Slice(ranked, func(i, j int) bool {
		return isOpenAIAccountCandidateBetter(ranked[i], ranked[j])
	})
	return ranked
}

type openAISelectionRNG struct {
	state uint64
}

func newOpenAISelectionRNG(seed uint64) openAISelectionRNG {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return openAISelectionRNG{state: seed}
}

func (r *openAISelectionRNG) nextUint64() uint64 {
	// xorshift64*
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 2685821657736338717
}

func (r *openAISelectionRNG) nextFloat64() float64 {
	// [0,1)
	return float64(r.nextUint64()>>11) / (1 << 53)
}

func deriveOpenAISelectionSeed(req OpenAIAccountScheduleRequest) uint64 {
	hasher := fnv.New64a()
	writeValue := func(value string) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return
		}
		_, _ = hasher.Write([]byte(trimmed))
		_, _ = hasher.Write([]byte{0})
	}

	writeValue(req.SessionHash)
	writeValue(req.PreviousResponseID)
	writeValue(req.RequestedModel)
	if req.GroupID != nil {
		_, _ = hasher.Write([]byte(strconv.FormatInt(*req.GroupID, 10)))
	}

	seed := hasher.Sum64()
	// 对“无会话锚点”的纯负载均衡请求引入时间熵，避免固定命中同一账号。
	if strings.TrimSpace(req.SessionHash) == "" && strings.TrimSpace(req.PreviousResponseID) == "" {
		seed ^= uint64(time.Now().UnixNano())
	}
	if seed == 0 {
		seed = uint64(time.Now().UnixNano()) ^ 0x9e3779b97f4a7c15
	}
	return seed
}

func buildOpenAIWeightedSelectionOrder(
	candidates []openAIAccountCandidateScore,
	req OpenAIAccountScheduleRequest,
) []openAIAccountCandidateScore {
	if len(candidates) <= 1 {
		return append([]openAIAccountCandidateScore(nil), candidates...)
	}

	pool := append([]openAIAccountCandidateScore(nil), candidates...)
	weights := make([]float64, len(pool))
	minScore := pool[0].score
	for i := 1; i < len(pool); i++ {
		if pool[i].score < minScore {
			minScore = pool[i].score
		}
	}
	for i := range pool {
		// 将 top-K 分值平移到正区间，避免“单一最高分账号”长期垄断。
		weight := (pool[i].score - minScore) + 1.0
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight <= 0 {
			weight = 1.0
		}
		weights[i] = weight
	}

	order := make([]openAIAccountCandidateScore, 0, len(pool))
	rng := newOpenAISelectionRNG(deriveOpenAISelectionSeed(req))
	for len(pool) > 0 {
		total := 0.0
		for _, w := range weights {
			total += w
		}

		selectedIdx := 0
		if total > 0 {
			r := rng.nextFloat64() * total
			acc := 0.0
			for i, w := range weights {
				acc += w
				if r <= acc {
					selectedIdx = i
					break
				}
			}
		} else {
			selectedIdx = int(rng.nextUint64() % uint64(len(pool)))
		}

		order = append(order, pool[selectedIdx])
		pool = append(pool[:selectedIdx], pool[selectedIdx+1:]...)
		weights = append(weights[:selectedIdx], weights[selectedIdx+1:]...)
	}
	return order
}

func (s *defaultOpenAIAccountScheduler) buildOpenAIAccountLoadPlan(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	filtered []*Account,
	loadMap map[int64]*AccountLoadInfo,
) openAIAccountLoadPlan {
	allCandidates := make([]openAIAccountCandidateScore, 0, len(filtered))
	for _, account := range filtered {
		loadInfo, loadKnown := loadMap[account.ID]
		if !loadKnown || loadInfo == nil {
			loadInfo = &AccountLoadInfo{AccountID: account.ID}
			loadKnown = false
		}
		errorRate, ttft, hasTTFT := 0.0, 0.0, false
		if s.stats != nil {
			errorRate, ttft, hasTTFT = s.stats.snapshot(account.ID)
		}
		allCandidates = append(allCandidates, openAIAccountCandidateScore{
			account:   account,
			loadInfo:  loadInfo,
			loadKnown: loadKnown,
			errorRate: errorRate,
			ttft:      ttft,
			hasTTFT:   hasTTFT,
		})
	}

	candidates := allCandidates
	staleSnapshotCompactRetry := make([]openAIAccountCandidateScore, 0, len(allCandidates))
	if req.RequireCompact {
		candidates = make([]openAIAccountCandidateScore, 0, len(allCandidates))
		for _, candidate := range allCandidates {
			if openAICompactSupportTier(candidate.account) == 0 {
				staleSnapshotCompactRetry = append(staleSnapshotCompactRetry, candidate)
				continue
			}
			candidates = append(candidates, candidate)
		}
	}

	plan := openAIAccountLoadPlan{
		allCandidates:             allCandidates,
		candidates:                candidates,
		staleSnapshotCompactRetry: staleSnapshotCompactRetry,
		candidateCount:            len(candidates),
	}
	if len(candidates) == 0 {
		plan.selectionOrder = s.buildOpenAISelectionOrder(req, plan)
		return plan
	}

	minPriority, maxPriority := openAIAccountSchedulingPriority(candidates[0].account), openAIAccountSchedulingPriority(candidates[0].account)
	maxWaiting := 1
	loadRateSum := 0.0
	loadRateSumSquares := 0.0
	minTTFT, maxTTFT := 0.0, 0.0
	hasTTFTSample := false
	for i := range candidates {
		candidate := &candidates[i]
		candidate.priority = openAIAccountSchedulingPriority(candidate.account)
		if candidate.priority < minPriority {
			minPriority = candidate.priority
		}
		if candidate.priority > maxPriority {
			maxPriority = candidate.priority
		}
		if candidate.loadInfo.WaitingCount > maxWaiting {
			maxWaiting = candidate.loadInfo.WaitingCount
		}
		if candidate.hasTTFT && candidate.ttft > 0 {
			if !hasTTFTSample {
				minTTFT, maxTTFT = candidate.ttft, candidate.ttft
				hasTTFTSample = true
			} else {
				if candidate.ttft < minTTFT {
					minTTFT = candidate.ttft
				}
				if candidate.ttft > maxTTFT {
					maxTTFT = candidate.ttft
				}
			}
		}
		loadRate := float64(candidate.loadInfo.LoadRate)
		loadRateSum += loadRate
		loadRateSumSquares += loadRate * loadRate
	}
	plan.loadSkew = calcLoadSkewByMoments(loadRateSum, loadRateSumSquares, len(candidates))

	weights := s.service.openAIWSSchedulerWeightsForRequest(ctx)
	now := time.Now()
	upstreamCostFactors := map[int64]float64(nil)
	if req.UseUpstreamTokenCost && weights.UpstreamCost > 0 {
		accounts := make([]*Account, 0, len(candidates))
		for _, candidate := range candidates {
			accounts = append(accounts, candidate.account)
		}
		upstreamCostFactors = openAIUpstreamCostFactors(accounts, now, s.service.openAIOAuthSchedulingRateMultiplier(ctx))
		for _, factor := range upstreamCostFactors {
			if factor != openAIUpstreamCostNeutralFactor {
				plan.includeOverflowFallback = true
				break
			}
		}
	}

	// Reset 因子（use-it-or-lose-it）：优先读取 Codex 5h 重置时间，回退到会话窗口。
	// 剩余时间越短 → 因子越接近 1（越早重置越优先用尽）。无活跃窗口的账号因子为 0。
	// 仅在 weights.Reset > 0 时计算，默认关闭不影响原有行为。
	minResetRemaining, maxResetRemaining := 0.0, 0.0
	hasResetSample := false
	if weights.Reset > 0 {
		for _, candidate := range candidates {
			end, ok := openAISchedulingResetWindowEnd(candidate.account, now)
			if !ok {
				continue
			}
			remaining := end.Sub(now).Seconds()
			if !hasResetSample {
				minResetRemaining, maxResetRemaining = remaining, remaining
				hasResetSample = true
				continue
			}
			if remaining < minResetRemaining {
				minResetRemaining = remaining
			}
			if remaining > maxResetRemaining {
				maxResetRemaining = remaining
			}
		}
	}

	// §4.2 到期紧迫度加权：仅本请求 platform=codebuddy 时生效。快照按候选集一次
	// 批量读取（每请求一次，不逐候选重复解析）。k=0 / 未启用 / 非 codebuddy 平台
	// 都不进乘法路径（urgencySnapshots 为 nil）→ score 计算逐位不变。
	urgencyBoost := codeBuddyUrgencyBoost{}
	var urgencySnapshots map[int64]codeBuddyUrgencySnapshot
	if NormalizeOpenAICompatiblePlatform(req.Platform) == PlatformCodeBuddy {
		urgencyBoost = s.service.codeBuddyUrgencyBoostForRequest()
		if urgencyBoost.Enabled && urgencyBoost.K > 0 {
			accountPtrs := make([]*Account, len(candidates))
			for i := range candidates {
				accountPtrs[i] = candidates[i].account
			}
			urgencySnapshots = codeBuddyUrgencySnapshotsFor(accountPtrs, now)
		}
	}

	for i := range candidates {
		item := &candidates[i]
		priorityFactor := 1.0
		if maxPriority > minPriority {
			priorityFactor = 1 - float64(item.priority-minPriority)/float64(maxPriority-minPriority)
		}
		loadFactor := 1 - clamp01(float64(item.loadInfo.LoadRate)/100.0)
		queueFactor := 1 - clamp01(float64(item.loadInfo.WaitingCount)/float64(maxWaiting))
		errorFactor := 1 - clamp01(item.errorRate)
		ttftFactor := 0.5
		if item.hasTTFT && hasTTFTSample && maxTTFT > minTTFT {
			ttftFactor = 1 - clamp01((item.ttft-minTTFT)/(maxTTFT-minTTFT))
		}
		resetFactor := 0.0
		if weights.Reset > 0 && hasResetSample {
			if end, ok := openAISchedulingResetWindowEnd(item.account, now); ok {
				if maxResetRemaining > minResetRemaining {
					resetFactor = 1 - clamp01((end.Sub(now).Seconds()-minResetRemaining)/(maxResetRemaining-minResetRemaining))
				} else {
					// 所有有窗口的账号剩余时间相同：一律给满分，让其优于无窗口账号。
					resetFactor = 1
				}
			}
		}
		quotaHeadroomFactor := 0.0
		if weights.QuotaHeadroom > 0 {
			quotaHeadroomFactor = openAIQuotaHeadroomFactor(item.account, now)
		}
		upstreamCostFactor := openAIUpstreamCostNeutralFactor
		if factor, ok := upstreamCostFactors[item.account.ID]; ok {
			upstreamCostFactor = factor
		}

		item.score = weights.Priority*priorityFactor +
			weights.Load*loadFactor +
			weights.Queue*queueFactor +
			weights.ErrorRate*errorFactor +
			weights.TTFT*ttftFactor +
			weights.Reset*resetFactor +
			weights.QuotaHeadroom*quotaHeadroomFactor +
			weights.UpstreamCost*(upstreamCostFactor-openAIUpstreamCostNeutralFactor)
		if req.StickyWeighted {
			if req.PreviousResponseCanMove && req.StickyPreviousAccountID > 0 && item.account.ID == req.StickyPreviousAccountID {
				item.score += weights.Previous
			}
			if req.StickyAccountID > 0 && item.account.ID == req.StickyAccountID {
				item.score += weights.SessionSticky
			}
		}
		// L15：备用开启时该模型的 codebuddy 备用账号加固定顶分，与 legacy 排序闭包的
		// "置于首位"同语义（不改 Priority 字段、不写库）。集合与 L14 同源；未命中集合的
		// 账号 score 逐位不变。
		if standbyRoutingAccountPromoted(item.account, req.RequestedModel) {
			item.score += openAISchedulingStandbyPromotedBonus
		}
		// §4.2：codebuddy 平台 + 启用 + K>0 时才走乘法路径。
		// 乘数恒 ≥1（norm∈[0,1]），无快照/快照陈旧/无参与分包 → norm=0 → 乘 1。
		if urgencySnapshots != nil {
			item.score *= ApplyCodeBuddyUrgencyBoost(item.account, urgencySnapshots[item.account.ID], urgencyBoost, now)
		}
	}
	plan.candidates = candidates

	plan.topK = s.service.openAIWSLBTopKForRequest(ctx)
	if plan.topK > len(candidates) {
		plan.topK = len(candidates)
	}
	if plan.topK <= 0 {
		plan.topK = 1
	}

	plan.selectionOrder = s.buildOpenAISelectionOrder(req, plan)
	return plan
}

func (s *defaultOpenAIAccountScheduler) buildOpenAISelectionOrder(
	req OpenAIAccountScheduleRequest,
	plan openAIAccountLoadPlan,
) []openAIAccountCandidateScore {
	buildSelectionOrder := func(pool []openAIAccountCandidateScore) []openAIAccountCandidateScore {
		if len(pool) == 0 || plan.topK <= 0 {
			return nil
		}
		groupTopK := plan.topK
		if groupTopK > len(pool) {
			groupTopK = len(pool)
		}
		ranked := selectTopKOpenAICandidates(pool, groupTopK)
		var primary []openAIAccountCandidateScore
		if req.StickyWeighted {
			for _, stickyID := range []int64{req.StickyPreviousAccountID, req.StickyAccountID} {
				if stickyID <= 0 {
					continue
				}
				for i, candidate := range ranked {
					if candidate.account != nil && candidate.account.ID == stickyID {
						primary = append([]openAIAccountCandidateScore{candidate}, ranked[:i]...)
						primary = append(primary, ranked[i+1:]...)
						break
					}
				}
				if len(primary) > 0 {
					break
				}
			}
		}
		if len(primary) == 0 {
			primary = buildOpenAIWeightedSelectionOrder(ranked, req)
		}
		if !plan.includeOverflowFallback || groupTopK >= len(pool) {
			return primary
		}

		selected := make(map[int64]struct{}, len(primary))
		for _, candidate := range primary {
			selected[candidate.account.ID] = struct{}{}
		}
		overflow := make([]openAIAccountCandidateScore, 0, len(pool)-len(primary))
		for _, candidate := range pool {
			if _, ok := selected[candidate.account.ID]; !ok {
				overflow = append(overflow, candidate)
			}
		}
		sort.Slice(overflow, func(i, j int) bool {
			return isOpenAIAccountCandidateBetter(overflow[i], overflow[j])
		})
		return append(primary, overflow...)
	}

	if req.RequireCompact {
		supported := make([]openAIAccountCandidateScore, 0, len(plan.candidates))
		unknown := make([]openAIAccountCandidateScore, 0, len(plan.candidates))
		for _, candidate := range plan.candidates {
			switch openAICompactSupportTier(candidate.account) {
			case 2:
				supported = append(supported, candidate)
			case 1:
				unknown = append(unknown, candidate)
			}
		}
		selectionOrder := make([]openAIAccountCandidateScore, 0, len(plan.allCandidates))
		selectionOrder = append(selectionOrder, buildSelectionOrder(supported)...)
		selectionOrder = append(selectionOrder, buildSelectionOrder(unknown)...)
		if len(plan.staleSnapshotCompactRetry) > 0 && s.service.schedulerSnapshot != nil {
			selectionOrder = append(selectionOrder, sortOpenAICompactRetryCandidates(plan.staleSnapshotCompactRetry)...)
		}
		return selectionOrder
	}

	return buildSelectionOrder(plan.candidates)
}

func sortOpenAICompactRetryCandidates(pool []openAIAccountCandidateScore) []openAIAccountCandidateScore {
	if len(pool) == 0 {
		return nil
	}
	ordered := append([]openAIAccountCandidateScore(nil), pool...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.account.Priority != b.account.Priority {
			return a.account.Priority < b.account.Priority
		}
		if a.loadInfo.LoadRate != b.loadInfo.LoadRate {
			return a.loadInfo.LoadRate < b.loadInfo.LoadRate
		}
		if a.loadInfo.WaitingCount != b.loadInfo.WaitingCount {
			return a.loadInfo.WaitingCount < b.loadInfo.WaitingCount
		}
		switch {
		case a.account.LastUsedAt == nil && b.account.LastUsedAt != nil:
			return true
		case a.account.LastUsedAt != nil && b.account.LastUsedAt == nil:
			return false
		case a.account.LastUsedAt == nil && b.account.LastUsedAt == nil:
			return false
		default:
			return a.account.LastUsedAt.Before(*b.account.LastUsedAt)
		}
	})
	return ordered
}

func (s *defaultOpenAIAccountScheduler) tryAcquireOpenAISelectionOrder(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	selectionOrder []openAIAccountCandidateScore,
) (*AccountSelectionResult, bool, error) {
	budget := newOpenAISelectionProbeBudget()
	budget.enableLimit()
	return s.tryAcquireOpenAISelectionOrderWithBudget(ctx, req, selectionOrder, budget)
}

func (s *defaultOpenAIAccountScheduler) tryAcquireOpenAISelectionOrderWithBudget(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	selectionOrder []openAIAccountCandidateScore,
	budget *openAISelectionProbeBudget,
) (*AccountSelectionResult, bool, error) {
	compactBlocked := false
	release := func(result *AcquireResult) {
		if result != nil && result.ReleaseFunc != nil {
			result.ReleaseFunc()
		}
	}
	for i := 0; i < len(selectionOrder); i++ {
		candidate := selectionOrder[i]
		if candidate.account == nil {
			continue
		}
		if candidate.loadKnown && candidate.account.Concurrency > 0 &&
			candidate.loadInfo.CurrentConcurrency >= candidate.account.Concurrency {
			continue
		}

		result, attempted, acquireErr := s.tryAcquireOpenAIAccountSlot(ctx, candidate.account.ID, candidate.account.Concurrency, budget)
		if !attempted {
			break
		}
		if acquireErr != nil {
			return nil, compactBlocked, acquireErr
		}
		if result == nil || !result.Acquired {
			continue
		}

		fresh := s.service.resolveFreshSchedulableOpenAIAccount(ctx, candidate.account, req.Platform, req.RequestedModel, false, req.RequiredCapability)
		if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
			release(result)
			continue
		}
		if !s.consumeOpenAISelectionDBRecheck(budget) {
			release(result)
			break
		}
		fresh = s.service.recheckSelectedOpenAIAccountFromDB(ctx, fresh, req.GroupID, req.Platform, req.RequestedModel, false, req.RequiredCapability)
		if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
			release(result)
			continue
		}
		if req.RequireCompact && openAICompactSupportTier(fresh) == 0 {
			compactBlocked = true
			release(result)
			continue
		}

		if fresh.Concurrency != candidate.account.Concurrency {
			release(result)
			result, attempted, acquireErr = s.tryAcquireOpenAIAccountSlot(ctx, fresh.ID, fresh.Concurrency, budget)
			if !attempted {
				continue
			}
			if acquireErr != nil {
				return nil, compactBlocked, acquireErr
			}
			if result == nil || !result.Acquired {
				continue
			}
		}
		if req.SessionHash != "" && !req.PreserveStickyBinding {
			_ = s.service.bindOpenAIStickySessionDuringSelection(ctx, req.GroupID, req.SessionHash, fresh.ID)
		}
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{
			Account:     fresh,
			Acquired:    true,
			ReleaseFunc: result.ReleaseFunc,
		}), compactBlocked, nil
	}
	return nil, compactBlocked, nil
}

func (s *defaultOpenAIAccountScheduler) tryAcquireOpenAIAccountSlot(
	ctx context.Context,
	accountID int64,
	maxConcurrency int,
	budget *openAISelectionProbeBudget,
) (*AcquireResult, bool, error) {
	if s.service.concurrencyService != nil && maxConcurrency > 0 && !budget.recordAcquire(accountID) {
		return nil, false, nil
	}
	result, err := s.service.tryAcquireAccountSlot(ctx, accountID, maxConcurrency)
	return result, true, err
}

func (s *defaultOpenAIAccountScheduler) consumeOpenAISelectionDBRecheck(budget *openAISelectionProbeBudget) bool {
	if s.service.schedulerSnapshot == nil || s.service.accountRepo == nil {
		return true
	}
	return budget.recordRecheck()
}

func (s *defaultOpenAIAccountScheduler) tryFallbackToWeightedSticky(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
) (*AccountSelectionResult, error) {
	if !req.StickyWeighted {
		return nil, nil
	}
	for _, accountID := range []int64{req.StickyPreviousAccountID, req.StickyAccountID} {
		if accountID <= 0 {
			continue
		}
		if req.ExcludedIDs != nil {
			if _, excluded := req.ExcludedIDs[accountID]; excluded {
				continue
			}
		}
		account, err := s.service.getSchedulableAccount(ctx, accountID)
		if err != nil || account == nil {
			continue
		}
		if !s.isAccountRequestCompatible(ctx, account, req) || !s.isAccountTransportCompatible(account, req.RequiredTransport) {
			continue
		}
		account = s.service.recheckSelectedOpenAIAccountFromDB(ctx, account, req.GroupID, req.Platform, req.RequestedModel, req.RequireCompact, req.RequiredCapability)
		if account == nil {
			if accountID == req.StickyAccountID && strings.TrimSpace(req.SessionHash) != "" {
				_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, req.SessionHash)
			}
			continue
		}
		if !s.service.openAIAccountMatchesSchedulingGroup(account, req.GroupID) {
			if accountID == req.StickyAccountID && strings.TrimSpace(req.SessionHash) != "" {
				_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, req.SessionHash)
			}
			continue
		}
		if !s.isAccountRequestCompatible(ctx, account, req) || !s.isAccountTransportCompatible(account, req.RequiredTransport) {
			continue
		}
		if req.RequireCompact && openAICompactSupportTier(account) == 0 {
			continue
		}
		// Keep weighted sticky fallback subject to the same free-tier gate as the
		// normal and sticky selection paths. Otherwise an over-quota free account
		// could be reintroduced after the primary candidate pass.
		if len(s.filterGrokFreeQuotaAccounts(ctx, []Account{*account})) == 0 {
			continue
		}
		upstreamModel := canonicalOpenAIAccountSchedulingModel(account, req.RequestedModel)
		now := time.Now()
		if isGrokTeamModelRateLimited(account, upstreamModel, now) ||
			isGrokModelQuotaBlocked(account.ID, upstreamModel, now) {
			continue
		}
		result, acquireErr := s.service.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
		if acquireErr != nil {
			return nil, acquireErr
		}
		if result != nil && result.Acquired {
			if req.SessionHash != "" && !req.PreserveStickyBinding {
				_ = s.service.bindOpenAIStickySessionDuringSelection(ctx, req.GroupID, req.SessionHash, account.ID)
			}
			return attachSelectionProfitGate(ctx, &AccountSelectionResult{
				Account:     account,
				Acquired:    true,
				ReleaseFunc: result.ReleaseFunc,
			}), nil
		}
		if s.service.concurrencyService != nil {
			cfg := s.service.schedulingConfig()
			return attachSelectionProfitGate(ctx, &AccountSelectionResult{
				Account: account,
				WaitPlan: &AccountWaitPlan{
					AccountID:      account.ID,
					MaxConcurrency: account.Concurrency,
					Timeout:        cfg.StickySessionWaitTimeout,
					MaxWaiting:     cfg.StickySessionMaxWaiting,
				},
			}), nil
		}
	}
	return nil, nil
}

// openAISelectionFilterStats counts why candidates were dropped by the
// selectByLoadBalance initial filter. Historically these exclusions were
// silent (debug logs at best), so a "no available accounts" failure with
// excluded_account_count=0 was undiagnosable from the error alone (#4599).
// The reasons map is lazily allocated: on the happy path (nothing filtered
// out, or an account is eventually selected) no extra allocation happens.
type openAISelectionFilterStats struct {
	pool    int
	reasons map[string]int
}

func (s *openAISelectionFilterStats) exclude(reason string) {
	if s.reasons == nil {
		s.reasons = make(map[string]int, 4)
	}
	s.reasons[reason]++
}

// summary renders deterministic exclusion statistics for scheduling error
// messages, e.g. "pool=3, filtered: model_not_supported=2 quota_auto_pause_7d=1".
// Reasons are sorted lexicographically so the output is stable for tests and
// log aggregation. extra, when non-empty, is appended as a trailing marker.
func (s openAISelectionFilterStats) summary(extra string) string {
	var b strings.Builder
	_, _ = b.WriteString("pool=")
	_, _ = b.WriteString(strconv.Itoa(s.pool))
	if len(s.reasons) > 0 {
		reasons := make([]string, 0, len(s.reasons))
		for reason := range s.reasons {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		_, _ = b.WriteString(", filtered:")
		for _, reason := range reasons {
			_, _ = b.WriteString(" ")
			_, _ = b.WriteString(reason)
			_, _ = b.WriteString("=")
			_, _ = b.WriteString(strconv.Itoa(s.reasons[reason]))
		}
	}
	if extra != "" {
		_, _ = b.WriteString(", ")
		_, _ = b.WriteString(extra)
	}
	return b.String()
}

func (s *defaultOpenAIAccountScheduler) selectByLoadBalance(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
) (*AccountSelectionResult, int, int, float64, error) {
	budget := newOpenAISelectionProbeBudget()
	accounts, err := s.service.listSchedulableAccounts(ctx, req.GroupID, req.Platform)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	if len(accounts) == 0 {
		return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false, openAISelectionFilterStats{}.summary(""))
	}
	// CodeBuddy 平台默认 RPM：一次预取候选计数，供候选过滤读取，避免逐账号查 Redis（N+1）。
	ctx = withCodeBuddyRPMCounts(ctx, s.service.prefetchCodeBuddyRPMCounts(ctx, accounts))
	// Local free-tier soft gate on the Grok scheduling path only (not admin probe).
	accounts = s.filterGrokFreeQuotaAccounts(ctx, accounts)
	if len(accounts) == 0 {
		return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false, openAISelectionFilterStats{}.summary("grok_free_quota_soft_gate"))
	}
	// Team+model rate-limit cool: siblings of a 429'd team skip the hot model.
	if req.Platform == PlatformGrok {
		now := time.Now()
		filtered := filterGrokTeamModelRateLimitedAccounts(accounts, req.RequestedModel, now)
		if len(filtered) == 0 && len(accounts) > 0 {
			return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false, openAISelectionFilterStats{}.summary("grok_team_model_rate_limit"))
		}
		if filtered != nil {
			accounts = filtered
		}
		// Per-account model free-usage soft-block (other models stay eligible).
		modelFiltered := filterGrokModelQuotaBlockedAccounts(accounts, req.RequestedModel, now)
		if len(modelFiltered) == 0 && len(accounts) > 0 {
			return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false, openAISelectionFilterStats{}.summary("grok_model_quota_block"))
		}
		accounts = modelFiltered
	}

	// require_privacy_set: 获取分组配置。GetByID 会聚合账号计数，选号不能走它。
	var schedGroup *Group
	if req.GroupID != nil && s.service.schedulerSnapshot != nil {
		schedGroup, _ = s.service.schedulerSnapshot.GetGroupByIDLite(ctx, *req.GroupID)
	}

	filterStats := openAISelectionFilterStats{pool: len(accounts)}
	// 聚合分组逐模型目录闸门（§2.1-1）：选号前按当前请求模型 + 当前目录实时判定，
	// 快照命中不豁免。返回 codebuddy 候选账号的 verdict，循环内据此排除。
	codeBuddyVerdicts := s.evaluateCodeBuddyCatalogGate(ctx, req, accounts)
	filtered := make([]*Account, 0, len(accounts))
	loadReq := make([]AccountWithConcurrency, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if req.ExcludedIDs != nil {
			if _, excluded := req.ExcludedIDs[account.ID]; excluded {
				filterStats.exclude("excluded")
				continue
			}
		}
		if !account.IsSchedulable() {
			filterStats.exclude("not_schedulable")
			continue
		}
		// platform_mismatch：聚合分组请求下的 codebuddy OpenAI 兼容账号越过
		// 平台名校验进入候选（§2.1-2）；其余仍按既有规则排除。
		if account.Platform != NormalizeOpenAICompatiblePlatform(req.Platform) || !account.IsOpenAICompatible() {
			if !codeBuddyPlatformMismatchAllowed(account, req) {
				filterStats.exclude("platform_mismatch")
				continue
			}
		}
		// P1-S §16.1 + §25.6 同一来源门：归属激活码过期或真实来源剩余不足的 cursor 账号
		// 不进入高级候选池。与其余三条链共用 cursorSupplySourceAvailabilityVetoReasonString
		// 同一口径（key 终态 + 真实来源剩余，来源可用 AND 账号可用）；此处只做候选剔除，
		// 账号状态一律不动。
		//
		// error 路径显式失败关闭：读取来源状态/额度快照出错时，来源门只能给出 error 而拿不到
		// 判定结论（cursorSupplySourceAvailabilityVetoReasonString 会把它折叠成
		// quota_key_state_unavailable）。此处显式接住该 error 并按同一原因串剔除候选——
		// 既不静默当"无否决"放行（可能已停供的号不得进池），也不让单个账号的读取故障
		// 整轮选号失败。排除原因串与其余三条链保持一致，账号状态依旧一律不动。
		sourceReason, sourceErr := cursorSupplySourceAvailabilityVetoReason(ctx, account)
		if sourceErr != nil {
			filterStats.exclude("quota_key_state_unavailable")
			continue
		}
		if sourceReason != "" {
			filterStats.exclude(sourceReason)
			continue
		}
		if s.service.isOpenAIAccountRequestRuntimeBlocked(account, req.RequestedModel) {
			filterStats.exclude("runtime_blocked")
			continue
		}
		// require_privacy_set is a group-scoped eligibility gate. Do not mutate the
		// shared account: another group may intentionally allow accounts whose
		// upstream privacy setting has not been confirmed.
		if schedGroup != nil && schedGroup.RequirePrivacySet && !account.IsPrivacySet() {
			filterStats.exclude("privacy_not_set")
			continue
		}
		if compatible, reason := s.isAccountRequestCompatibleReason(ctx, account, req); !compatible {
			filterStats.exclude(reason)
			continue
		}
		// 聚合分组逐模型目录闸门（§2.1-1）：codebuddy 候选按 verdict 排除。
		// 未命中映射后上游模型 → model_not_in_upstream_catalog；
		// 目录不可达/超时/空集合/非集合结构 → upstream_catalog_unavailable（禁兜底）；
		// 超出本轮探测预算 → upstream_catalog_not_probed（非静默跳过）。
		if verdict, ok := codeBuddyVerdicts[account.ID]; ok {
			switch verdict {
			case codeBuddyCatalogModelMissing:
				filterStats.exclude("model_not_in_upstream_catalog")
				continue
			case codeBuddyCatalogUnavailable:
				filterStats.exclude("upstream_catalog_unavailable")
				continue
			case codeBuddyCatalogNotProbed:
				filterStats.exclude("upstream_catalog_not_probed")
				continue
			}
		}
		// CodeBuddy 平台默认 RPM：非粘性候选仅绿区可选（计数来自本次选号预取）。
		if !s.codeBuddyRPMAllowsCandidate(ctx, account) {
			filterStats.exclude("rpm_limited")
			continue
		}
		if !s.isAccountTransportCompatible(account, req.RequiredTransport) {
			filterStats.exclude("transport_incompatible")
			continue
		}
		// B2: /v1/messages 候选过滤 —— Web 接入模式账号永不入选。
		if isOpenAIMessagesDispatchContext(ctx) && account.IsWebAccessMode() {
			filterStats.exclude("web_excluded_for_messages")
			continue
		}
		// 显式非法 access_mode fail-closed：调度判为不可选（B3/§5.1 规则 2）。
		if reason := openAIAccessModeExcludeReason(account); reason != "" {
			filterStats.exclude(reason)
			continue
		}
		filtered = append(filtered, account)
		loadReq = append(loadReq, AccountWithConcurrency{
			ID:             account.ID,
			MaxConcurrency: account.EffectiveLoadFactor(),
		})
	}
	if len(filtered) == 0 {
		return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false, filterStats.summary(""))
	}

	loadMap := map[int64]*AccountLoadInfo{}
	if s.service.concurrencyService != nil {
		if batchLoad, loadErr := s.service.concurrencyService.GetAccountsLoadBatch(ctx, loadReq); loadErr == nil {
			loadMap = batchLoad
		}
	}

	// B3 两级池：将合格候选按接入模式拆成 API 池 / Web 池，先完整尝试 API 池，
	// 仅当 API 池未产出任何可选账号（含订阅/常规/等待计划兜底）时才回落 Web 池。
	// 池内保留原有评分/负载/订阅优先等规则（runPoolSelection 复用既有选择管线）。
	runPoolSelection := func(pool []*Account) (*AccountSelectionResult, int, int, float64, error) {
		if len(pool) == 0 {
			return nil, 0, 0, 0, nil
		}
		if req.SubscriptionPriority {
			subscriptionAccounts, regularAccounts := partitionOpenAIChatGPTSubscriptionAccounts(pool)
			if len(subscriptionAccounts) > 0 {
				attempt := s.trySelectByLoadBalancePool(ctx, req, subscriptionAccounts, loadMap, budget)
				if attempt.err != nil && (!attempt.noCompactCandidates || len(regularAccounts) <= 0) {
					return nil, attempt.candidateCount, attempt.topK, attempt.loadSkew, attempt.err
				}
				if attempt.result != nil {
					return attempt.result, attempt.candidateCount, attempt.topK, attempt.loadSkew, nil
				}
				if len(regularAccounts) > 0 {
					regularAttempt := s.trySelectByLoadBalancePool(ctx, req, regularAccounts, loadMap, budget)
					if regularAttempt.err != nil && !regularAttempt.noCompactCandidates {
						return nil, regularAttempt.candidateCount, regularAttempt.topK, regularAttempt.loadSkew, regularAttempt.err
					}
					if regularAttempt.result != nil {
						return regularAttempt.result, regularAttempt.candidateCount, regularAttempt.topK, regularAttempt.loadSkew, nil
					}
					var result *AccountSelectionResult
					candidateCount, topK, loadSkew := regularAttempt.candidateCount, regularAttempt.topK, regularAttempt.loadSkew
					fallbackErr := regularAttempt.err
					if regularAttempt.err == nil {
						result, candidateCount, topK, loadSkew, fallbackErr = s.finishLoadBalanceSelectionFallback(ctx, req, regularAttempt, budget, filterStats)
						if fallbackErr == nil && result != nil {
							return result, candidateCount, topK, loadSkew, nil
						}
					}
					// 常规池既无法获取也无法排队（含仅剩不支持 compact 的候选）时，
					// 回退到订阅池的等待计划：busy-but-waitable 的订阅账号不应因常规池存在
					// 而被丢弃，否则开启订阅优先反而让本可排队成功的请求硬失败。
					subResult, subCandidateCount, subTopK, subLoadSkew, subErr := s.finishLoadBalanceSelectionFallback(ctx, req, attempt, budget, filterStats)
					if subErr == nil && subResult != nil {
						return subResult, subCandidateCount, subTopK, subLoadSkew, nil
					}
					return result, candidateCount, topK, loadSkew, fallbackErr
				}
				return s.finishLoadBalanceSelectionFallback(ctx, req, attempt, budget, filterStats)
			}
		}

		attempt := s.trySelectByLoadBalancePool(ctx, req, pool, loadMap, budget)
		if attempt.err != nil {
			return nil, attempt.candidateCount, attempt.topK, attempt.loadSkew, attempt.err
		}
		if attempt.result != nil {
			return attempt.result, attempt.candidateCount, attempt.topK, attempt.loadSkew, nil
		}
		return s.finishLoadBalanceSelectionFallback(ctx, req, attempt, budget, filterStats)
	}

	apiPool, webPool := partitionOpenAIAccessModePools(filtered)
	if res, candidateCount, topK, loadSkew, selErr := runPoolSelection(apiPool); res != nil {
		return res, candidateCount, topK, loadSkew, nil
	} else if selErr != nil && len(webPool) == 0 {
		return nil, candidateCount, topK, loadSkew, selErr
	}
	if res, candidateCount, topK, loadSkew, selErr := runPoolSelection(webPool); res != nil {
		return res, candidateCount, topK, loadSkew, nil
	} else {
		return nil, candidateCount, topK, loadSkew, selErr
	}
}

func partitionOpenAIChatGPTSubscriptionAccounts(accounts []*Account) ([]*Account, []*Account) {
	subscriptionAccounts := make([]*Account, 0, len(accounts))
	regularAccounts := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		if account != nil && account.IsOpenAIChatGPTSubscription() {
			subscriptionAccounts = append(subscriptionAccounts, account)
			continue
		}
		regularAccounts = append(regularAccounts, account)
	}
	return subscriptionAccounts, regularAccounts
}

func (s *defaultOpenAIAccountScheduler) trySelectByLoadBalancePool(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	filtered []*Account,
	loadMap map[int64]*AccountLoadInfo,
	budget *openAISelectionProbeBudget,
) openAIAccountLoadSelectionAttempt {
	plan := s.buildOpenAIAccountLoadPlan(ctx, req, filtered, loadMap)
	if openAICostOverflowExpanded(req, plan) {
		budget.enableLimit()
	}
	attempt := openAIAccountLoadSelectionAttempt{
		selectionOrder: plan.selectionOrder,
		candidateCount: plan.candidateCount,
		topK:           plan.topK,
		loadSkew:       plan.loadSkew,
	}
	if req.RequireCompact && len(plan.candidates) == 0 && len(plan.staleSnapshotCompactRetry) == 0 {
		attempt.noCompactCandidates = true
		attempt.err = ErrNoAvailableCompactAccounts
		return attempt
	}
	if req.RequireCompact && len(attempt.selectionOrder) == 0 && s.service.schedulerSnapshot == nil {
		attempt.noCompactCandidates = true
		attempt.err = ErrNoAvailableCompactAccounts
		return attempt
	}
	if len(attempt.selectionOrder) == 0 {
		attempt.compactBlocked = req.RequireCompact && len(plan.allCandidates) > 0
		return attempt
	}

	result, compactBlocked, acquireErr := s.tryAcquireOpenAISelectionOrderWithBudget(ctx, req, attempt.selectionOrder, budget)
	attempt.compactBlocked = compactBlocked
	if acquireErr != nil {
		attempt.err = acquireErr
		return attempt
	}
	if result != nil {
		attempt.result = result
		return attempt
	}

	if s.service.concurrencyService != nil && !budget.acquireExhausted() {
		loadReq := buildOpenAIAccountLoadRequest(filtered)
		if freshLoadMap, loadErr := s.service.concurrencyService.GetAccountsLoadBatchFresh(ctx, loadReq); loadErr == nil {
			freshPlan := s.buildOpenAIAccountLoadPlan(ctx, req, filtered, freshLoadMap)
			if openAICostOverflowExpanded(req, freshPlan) {
				budget.enableLimit()
			}
			if len(freshPlan.selectionOrder) > 0 {
				freshResult, freshCompactBlocked, freshAcquireErr := s.tryAcquireOpenAISelectionOrderWithBudget(ctx, req, freshPlan.selectionOrder, budget)
				if freshAcquireErr != nil {
					attempt.err = freshAcquireErr
					return attempt
				}
				if freshResult != nil {
					attempt.result = freshResult
					attempt.selectionOrder = freshPlan.selectionOrder
					attempt.candidateCount = freshPlan.candidateCount
					attempt.topK = freshPlan.topK
					attempt.loadSkew = freshPlan.loadSkew
					return attempt
				}
				attempt.compactBlocked = attempt.compactBlocked || freshCompactBlocked
				attempt.selectionOrder = freshPlan.selectionOrder
				attempt.candidateCount = freshPlan.candidateCount
				attempt.topK = freshPlan.topK
				attempt.loadSkew = freshPlan.loadSkew
			}
		}
	}

	return attempt
}

func openAICostOverflowExpanded(req OpenAIAccountScheduleRequest, plan openAIAccountLoadPlan) bool {
	if !plan.includeOverflowFallback || plan.topK <= 0 {
		return false
	}
	if !req.RequireCompact {
		return len(plan.candidates) > plan.topK
	}
	supported, unknown := 0, 0
	for _, candidate := range plan.candidates {
		switch openAICompactSupportTier(candidate.account) {
		case 2:
			supported++
		case 1:
			unknown++
		}
	}
	return supported > plan.topK || unknown > plan.topK
}

func buildOpenAIAccountLoadRequest(accounts []*Account) []AccountWithConcurrency {
	loadReq := make([]AccountWithConcurrency, 0, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		loadReq = append(loadReq, AccountWithConcurrency{
			ID:             account.ID,
			MaxConcurrency: account.EffectiveLoadFactor(),
		})
	}
	return loadReq
}

func (s *defaultOpenAIAccountScheduler) finishLoadBalanceSelectionFallback(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	attempt openAIAccountLoadSelectionAttempt,
	budget *openAISelectionProbeBudget,
	filterStats openAISelectionFilterStats,
) (*AccountSelectionResult, int, int, float64, error) {
	candidateCount := attempt.candidateCount
	topK := attempt.topK
	loadSkew := attempt.loadSkew

	if len(attempt.selectionOrder) == 0 {
		return nil, candidateCount, topK, loadSkew, noAvailableOpenAISelectionError(req.RequestedModel, attempt.compactBlocked, filterStats.summary("selection_order_empty"))
	}

	if stickyFallback, stickyErr := s.tryFallbackToWeightedSticky(ctx, req); stickyErr != nil {
		return nil, candidateCount, topK, loadSkew, stickyErr
	} else if stickyFallback != nil {
		return stickyFallback, candidateCount, topK, loadSkew, nil
	}

	cfg := s.service.schedulingConfig()
	compactBlocked := attempt.compactBlocked
	// WaitPlan.MaxConcurrency 使用 Concurrency（非 EffectiveLoadFactor），因为 WaitPlan 控制的是 Redis 实际并发槽位等待。
	passes := 1
	if budget != nil && budget.limited {
		passes = 4
	}
	for pass := 0; pass < passes; pass++ {
		wantAttempted := pass == 1 || pass == 3
		wantKnownFull := pass >= 2
		for _, candidate := range attempt.selectionOrder {
			if candidate.account == nil {
				continue
			}
			if budget != nil && budget.limited {
				knownFull := candidate.loadKnown && candidate.account.Concurrency > 0 &&
					candidate.loadInfo.CurrentConcurrency >= candidate.account.Concurrency
				if budget.wasAttempted(candidate.account.ID) != wantAttempted || knownFull != wantKnownFull {
					continue
				}
			}
			fresh := s.service.resolveFreshSchedulableOpenAIAccount(ctx, candidate.account, req.Platform, req.RequestedModel, false, req.RequiredCapability)
			if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
				continue
			}
			if !s.consumeOpenAISelectionDBRecheck(budget) {
				return nil, candidateCount, topK, loadSkew, noAvailableOpenAISelectionError(req.RequestedModel, compactBlocked, filterStats.summary("selection_order_exhausted"))
			}
			fresh = s.service.recheckSelectedOpenAIAccountFromDB(ctx, fresh, req.GroupID, req.Platform, req.RequestedModel, false, req.RequiredCapability)
			if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
				continue
			}
			if req.RequireCompact && openAICompactSupportTier(fresh) == 0 {
				compactBlocked = true
				continue
			}
			return attachSelectionProfitGate(ctx, &AccountSelectionResult{
				Account: fresh,
				WaitPlan: &AccountWaitPlan{
					AccountID:      fresh.ID,
					MaxConcurrency: fresh.Concurrency,
					Timeout:        cfg.FallbackWaitTimeout,
					MaxWaiting:     cfg.FallbackMaxWaiting,
				},
			}), candidateCount, topK, loadSkew, nil
		}
	}

	return nil, candidateCount, topK, loadSkew, noAvailableOpenAISelectionError(req.RequestedModel, compactBlocked, filterStats.summary("selection_order_exhausted"))
}

func (s *defaultOpenAIAccountScheduler) isAccountTransportCompatible(account *Account, requiredTransport OpenAIUpstreamTransport) bool {
	if requiredTransport == OpenAIUpstreamTransportAny || requiredTransport == OpenAIUpstreamTransportHTTPSSE {
		return true
	}
	if s == nil || s.service == nil {
		return false
	}
	return s.service.isOpenAIAccountTransportCompatible(account, requiredTransport)
}

func (s *defaultOpenAIAccountScheduler) lookupShadowParentAccount(ctx context.Context, id int64) *Account {
	if s == nil || s.service == nil {
		return nil
	}
	if s.service.schedulerSnapshot != nil {
		if account, err := s.service.schedulerSnapshot.GetAccount(ctx, id); err == nil && account != nil {
			return account
		}
	}
	if s.service.accountRepo == nil {
		return nil
	}
	account, _ := s.service.accountRepo.GetByID(ctx, id)
	return account
}

func (s *defaultOpenAIAccountScheduler) isAccountRequestCompatible(ctx context.Context, account *Account, req OpenAIAccountScheduleRequest) bool {
	compatible, _ := s.isAccountRequestCompatibleReason(ctx, account, req)
	return compatible
}

// openAIAccountVisionCapabilityLookup 按 (账号, 上游模型, 协议) 查询视觉能力标记。
// 默认返回 (false, false, nil)=未知，调用方按"未标记账号放行"保守处理
// （docs-local/capability-routing-plan.md §3.4）。wiring 装配能力表服务后通过
// SetOpenAIAccountVisionCapabilityLookup 注入（复用账号维度二级缓存），
// 未装配时行为退化为 unknown 放行，与方案语义一致。读取失败返回非 nil err，
// 由调用方失败关闭（合同 §5 禁兜底）。
var openAIAccountVisionCapabilityLookup = func(ctx context.Context, accountID int64, upstreamModel, protocol string) (supported, known bool, err error) {
	return false, false, nil
}

// SetOpenAIAccountVisionCapabilityLookup 注入视觉能力查询实现（测试与 wiring 用）。
// 注入函数需返回读取错误，读取失败时由调度侧失败关闭。
func SetOpenAIAccountVisionCapabilityLookup(lookup func(ctx context.Context, accountID int64, upstreamModel, protocol string) (supported, known bool, err error)) {
	if lookup == nil {
		openAIAccountVisionCapabilityLookup = func(ctx context.Context, accountID int64, upstreamModel, protocol string) (supported, known bool, err error) {
			return false, false, nil
		}
		return
	}
	openAIAccountVisionCapabilityLookup = lookup
}

// codeBuddyRPMAllowsCandidate 判断非粘性候选是否通过 CodeBuddy 平台默认 RPM 筛选。
// 计数取自本次选号预取（ctx 缺失即失败开放）。
func (s *defaultOpenAIAccountScheduler) codeBuddyRPMAllowsCandidate(ctx context.Context, account *Account) bool {
	if s == nil || s.service == nil || !codeBuddyRPMGated(account) {
		return true
	}
	count, ok := codeBuddyRPMCountFromContext(ctx, codeBuddyRPMKeyAccountID(account))
	if !ok {
		count = -1
	}
	return s.service.codeBuddyRPMSchedulable(ctx, account, count, false)
}

// codeBuddyRPMAllowsSticky 判断粘性账号是否通过 CodeBuddy 平台默认 RPM 筛选
// （粘性允许黄区，红区才清绑定换号）。
func (s *defaultOpenAIAccountScheduler) codeBuddyRPMAllowsSticky(ctx context.Context, account *Account) bool {
	if s == nil || s.service == nil || !codeBuddyRPMGated(account) {
		return true
	}
	currentRPM := -1
	if s.service.rpmCache != nil {
		if count, err := s.service.rpmCache.GetRPM(ctx, codeBuddyRPMKeyAccountID(account)); err == nil {
			currentRPM = count
		}
	}
	return s.service.codeBuddyRPMSchedulable(ctx, account, currentRPM, true)
}

// isAccountRequestCompatibleReason reports whether the account can serve the
// request, and when it cannot, names the veto point. The reason feeds
// openAISelectionFilterStats so that "no available accounts" errors state why
// each candidate was dropped instead of failing silently (#4599).
func (s *defaultOpenAIAccountScheduler) isAccountRequestCompatibleReason(ctx context.Context, account *Account, req OpenAIAccountScheduleRequest) (bool, string) {
	if account == nil {
		return false, "account_nil"
	}
	if source, ok := CompositeRouteSourceFromContext(ctx); ok && source == CompositeRouteSourceAccount {
		if publicModel, modelOK := RequestedPublicModelFromContext(ctx); modelOK && !explicitModelMappingClaims(*account, publicModel) {
			return false, "account_model_not_owned"
		}
	}
	if req.RequirePrivacySet && !account.IsPrivacySet() {
		return false, "privacy_not_set"
	}
	if s != nil && s.service != nil && s.service.isOpenAIAccountRequestRuntimeBlocked(account, req.RequestedModel) {
		return false, "runtime_blocked"
	}
	if s != nil && s.service != nil && s.service.isOpenAIProxyStreamQuarantined(ctx, account) {
		return false, "proxy_stream_quarantined"
	}
	// Quota auto-pause must be evaluated during the initial filter too. Without it the
	// TopK candidate pool can be filled with paused accounts and the later fresh/DB
	// rechecks won't reach healthy accounts that fell outside TopK — manifesting as
	// "no available accounts" even though healthy ones exist.
	if paused, decision := shouldAutoPauseOpenAIAccountByQuota(ctx, account); paused {
		if decision.reason != "" {
			return false, decision.reason
		}
		reason := "quota_auto_pause"
		if decision.window != "" {
			reason += "_" + decision.window
		}
		return false, reason
	}
	// 母账号健康联动：影子账号的凭据来自母账号，母账号不可调度时影子也不应被选中。
	// Parent-health gate: shadow borrows the parent's credentials; an unschedulable
	// parent must block the shadow across all scheduler paths.
	if !parentHealthyForShadow(account, func(id int64) *Account {
		return s.lookupShadowParentAccount(ctx, id)
	}) {
		return false, "shadow_parent_unhealthy"
	}
	// 聚合分组请求下的 codebuddy 账号由逐模型目录闸门接管模型校验
	// （先 GetMappedModel 映射为上游名再与目录精确比较），跳过客户端名白名单检查，
	// 避免别名场景被误杀（§2.1-1）。其余账号走上游 v0.2.18 的统一判定
	// supportsOpenAIRequestModel（含退役模型与 compact 映射解析）。
	if req.RequestedModel != "" && !codeBuddyAggregatedCatalogGated(account, req) &&
		!supportsOpenAIRequestModel(account, req.RequestedModel, req.RequireCompact) {
		return false, "model_not_supported"
	}
	if req.GroupID != nil && s != nil && s.service != nil &&
		s.service.needsUpstreamChannelRestrictionCheck(ctx, req.GroupID) &&
		s.service.isUpstreamModelRestrictedByChannel(ctx, *req.GroupID, account, req.RequestedModel, req.RequireCompact) {
		return false, "channel_upstream_restricted"
	}
	if !accountSupportsOpenAICapabilities(account, req.RequiredCapability, req.RequiredImageCapability) {
		return false, "capability_mismatch"
	}
	// 请求含图时排除已知不支持视觉的账号（docs-local/capability-routing-plan.md §3.4）。
	// 上游模型名按请求模型经账号 model_mapping 解析（与 forward 阶段一致），
	// 协议取 req.RequiredCapability 的字符串值（chat_completions / responses），
	// 与能力标记表的协议维度对齐。未知（未检测）账号放行，由管理端检测收敛。
	if req.RequireVision {
		// §3.7 视觉分流正向收窄：命中 groups.vision_routing 配置时，候选账号必须
		// 属于目标账号集合（纯收窄）。此处位于 §3.4 能力过滤之前，两者叠加：
		// 集合之外排除（vision_routing_excluded），集合之内仍执行下面的
		// vision_not_supported 能力过滤，绝不豁免（方案明文"规则只收窄候选池"）。
		if len(req.VisionRoutingTargets) > 0 && !visionRoutingTargetContains(req.VisionRoutingTargets, account.ID) {
			return false, "vision_routing_excluded"
		}
		upstreamModel := resolveOpenAIAccountUpstreamModelForRequest(account, req.RequestedModel, req.RequireCompact)
		protocol := string(req.RequiredCapability)
		if protocol == "" {
			// 兜底：未声明协议时按 chat_completions 判定，避免空协议永远查不到记录。
			protocol = string(OpenAIEndpointCapabilityChatCompletions)
		}
		supported, known, err := openAIAccountVisionCapabilityLookup(ctx, account.ID, upstreamModel, protocol)
		if err != nil {
			// 能力源读取失败：禁止静默降级为 unknown 放行，失败关闭（合同 §5 禁兜底）。
			return false, "vision_capability_unavailable"
		}
		if known && !supported {
			return false, "vision_not_supported"
		}
		// unknown：放行（保守），等待管理端检测收敛为 known。
	}
	// B2 kimi 分层路由候选约束：小档仅 qwen 伪装线池（extra.mask_upstream_identity=true），
	// 大档仅非伪装池（星思）；空值不约束（非 kimi 请求零行为变化）。换号不出档——
	// 同档内换号走既有熔断/重试，跨档只发生在 selectAccountWithScheduler 的
	// qwen 整池无可用重试（仅小档、告警）。
	if req.TierRoutingPool != "" {
		masked := IsIdentityMaskedAccount(account)
		switch req.TierRoutingPool {
		case tierRoutingPoolMasked:
			// 小档仍只收伪装线，备用开启不改变 masked 池成员（L14'/F3）。
			if !masked {
				return false, "kimi_tier_pool_excluded"
			}
		case tierRoutingPoolUpstream:
			// L14'：备用开启时，本模型下的 codebuddy 账号跳过池归属判定，按 upstream
			// 候选参与；其余准入条件与关闭态完全一致。
			if masked && !standbyRoutingAccountPromoted(account, req.RequestedModel) {
				return false, "kimi_tier_pool_excluded"
			}
		}
	}
	// 分组利润控制：不合格账号在候选过滤与抢槽后终检阶段即被排除，
	// 排序/评分/粘性/熔断只在合格账号之间工作；named reason 进入 filter stats。
	if vetoed, reason := openAIProfitControlVetoReason(ctx, account); vetoed {
		return false, reason
	}
	return true, ""
}

func (s *defaultOpenAIAccountScheduler) ReportResult(accountID int64, success bool, firstTokenMs *int) {
	if s == nil || s.stats == nil {
		return
	}
	s.stats.report(accountID, success, firstTokenMs)
}

func (s *defaultOpenAIAccountScheduler) ReportSwitch() {
	if s == nil {
		return
	}
	s.metrics.recordSwitch()
}

func (s *defaultOpenAIAccountScheduler) SnapshotMetrics() OpenAIAccountSchedulerMetricsSnapshot {
	if s == nil {
		return OpenAIAccountSchedulerMetricsSnapshot{}
	}

	selectTotal := s.metrics.selectTotal.Load()
	prevHit := s.metrics.stickyPreviousHitTotal.Load()
	sessionHit := s.metrics.stickySessionHitTotal.Load()
	stickyHit := s.metrics.stickyHitTotal.Load()
	switchTotal := s.metrics.accountSwitchTotal.Load()
	latencyTotal := s.metrics.latencyMsTotal.Load()
	loadSkewTotal := s.metrics.loadSkewMilliTotal.Load()

	snapshot := OpenAIAccountSchedulerMetricsSnapshot{
		SelectTotal:              selectTotal,
		StickyPreviousHitTotal:   prevHit,
		StickySessionHitTotal:    sessionHit,
		LoadBalanceSelectTotal:   s.metrics.loadBalanceSelectTotal.Load(),
		AccountSwitchTotal:       switchTotal,
		SchedulerLatencyMsTotal:  latencyTotal,
		RuntimeStatsAccountCount: s.stats.size(),
	}
	if selectTotal > 0 {
		snapshot.SchedulerLatencyMsAvg = float64(latencyTotal) / float64(selectTotal)
		snapshot.StickyHitRatio = float64(stickyHit) / float64(selectTotal)
		snapshot.AccountSwitchRate = float64(switchTotal) / float64(selectTotal)
		snapshot.LoadSkewAvg = float64(loadSkewTotal) / 1000 / float64(selectTotal)
	}
	return snapshot
}

func (s *OpenAIGatewayService) openAIAdvancedSchedulerSettingRepo() SettingRepository {
	if s == nil || s.rateLimitService == nil || s.rateLimitService.settingService == nil {
		return nil
	}
	return s.rateLimitService.settingService.settingRepo
}

func (s *OpenAIGatewayService) openAIAdvancedSchedulerRuntimeSettings(ctx context.Context) openAIAdvancedSchedulerRuntimeSettings {
	if cached, ok := openAIAdvancedSchedulerSettingCache.Load().(*cachedOpenAIAdvancedSchedulerSetting); ok && cached != nil {
		if time.Now().UnixNano() < cached.expiresAt {
			return openAIAdvancedSchedulerRuntimeSettings{
				lowUpstreamRatePriorityEnabled: cached.lowUpstreamRatePriorityEnabled,
				oauthSchedulingRateMultiplier:  cached.oauthSchedulingRateMultiplier,
				enabled:                        cached.enabled,
				stickyWeightedEnabled:          cached.stickyWeightedEnabled,
				subscriptionPriorityEnabled:    cached.subscriptionPriorityEnabled,
				lbTopKOverride:                 cached.lbTopKOverride,
				weightOverrides:                cloneOpenAIAdvancedSchedulerWeightOverrides(cached.weightOverrides),
			}
		}
	}

	result, _, _ := openAIAdvancedSchedulerSettingSF.Do(openAIAdvancedSchedulerSettingKey, func() (any, error) {
		if cached, ok := openAIAdvancedSchedulerSettingCache.Load().(*cachedOpenAIAdvancedSchedulerSetting); ok && cached != nil {
			if time.Now().UnixNano() < cached.expiresAt {
				return openAIAdvancedSchedulerRuntimeSettings{
					lowUpstreamRatePriorityEnabled: cached.lowUpstreamRatePriorityEnabled,
					oauthSchedulingRateMultiplier:  cached.oauthSchedulingRateMultiplier,
					enabled:                        cached.enabled,
					stickyWeightedEnabled:          cached.stickyWeightedEnabled,
					subscriptionPriorityEnabled:    cached.subscriptionPriorityEnabled,
					lbTopKOverride:                 cached.lbTopKOverride,
					weightOverrides:                cloneOpenAIAdvancedSchedulerWeightOverrides(cached.weightOverrides),
				}, nil
			}
		}

		lowUpstreamRatePriorityEnabled := false
		oauthSchedulingRateMultiplier := parseOpenAIOAuthSchedulingRateMultiplier(nil)
		enabled := false
		stickyWeightedEnabled := false
		subscriptionPriorityEnabled := false
		lbTopKOverride := 0
		weightOverrides := map[string]float64{}
		if repo := s.openAIAdvancedSchedulerSettingRepo(); repo != nil {
			dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIAdvancedSchedulerSettingDBTimeout)
			defer cancel()

			if values, err := repo.GetMultiple(dbCtx, openAIAdvancedSchedulerRuntimeSettingKeys()); err == nil {
				lowUpstreamRatePriorityEnabled = strings.EqualFold(strings.TrimSpace(values[SettingKeyOpenAILowUpstreamRatePriorityEnabled]), "true")
				oauthSchedulingRateMultiplier = parseOpenAIOAuthSchedulingRateMultiplier(values)
				enabled = strings.EqualFold(strings.TrimSpace(values[openAIAdvancedSchedulerSettingKey]), "true")
				stickyWeightedEnabled = strings.EqualFold(strings.TrimSpace(values[SettingKeyOpenAIAdvancedSchedulerStickyWeightedEnabled]), "true")
				subscriptionPriorityEnabled = strings.EqualFold(strings.TrimSpace(values[SettingKeyOpenAIAdvancedSchedulerSubscriptionPriorityEnabled]), "true")
				lbTopKOverride = parsePositiveIntOverride(values[SettingKeyOpenAIAdvancedSchedulerLBTopK])
				weightOverrides = parseOpenAIAdvancedSchedulerWeightOverrides(values)
			} else {
				// 批量读取失败时逐键降级，覆盖全部键（含 TopK/权重），避免只加载布尔开关
				// 而静默丢弃管理员配置的覆盖值；降级状态会被缓存一个 TTL，必须留痕。
				slog.Warn("openai_advanced_scheduler_settings_batch_load_failed", "error", err)
				fallbackValues := make(map[string]string)
				for _, key := range openAIAdvancedSchedulerRuntimeSettingKeys() {
					if value, valueErr := repo.GetValue(dbCtx, key); valueErr == nil {
						fallbackValues[key] = value
					}
				}
				lowUpstreamRatePriorityEnabled = strings.EqualFold(strings.TrimSpace(fallbackValues[SettingKeyOpenAILowUpstreamRatePriorityEnabled]), "true")
				oauthSchedulingRateMultiplier = parseOpenAIOAuthSchedulingRateMultiplier(fallbackValues)
				enabled = strings.EqualFold(strings.TrimSpace(fallbackValues[openAIAdvancedSchedulerSettingKey]), "true")
				stickyWeightedEnabled = strings.EqualFold(strings.TrimSpace(fallbackValues[SettingKeyOpenAIAdvancedSchedulerStickyWeightedEnabled]), "true")
				subscriptionPriorityEnabled = strings.EqualFold(strings.TrimSpace(fallbackValues[SettingKeyOpenAIAdvancedSchedulerSubscriptionPriorityEnabled]), "true")
				lbTopKOverride = parsePositiveIntOverride(fallbackValues[SettingKeyOpenAIAdvancedSchedulerLBTopK])
				weightOverrides = parseOpenAIAdvancedSchedulerWeightOverrides(fallbackValues)
			}
		}

		openAIAdvancedSchedulerSettingCache.Store(&cachedOpenAIAdvancedSchedulerSetting{
			lowUpstreamRatePriorityEnabled: lowUpstreamRatePriorityEnabled,
			oauthSchedulingRateMultiplier:  oauthSchedulingRateMultiplier,
			enabled:                        enabled,
			stickyWeightedEnabled:          stickyWeightedEnabled,
			subscriptionPriorityEnabled:    subscriptionPriorityEnabled,
			lbTopKOverride:                 lbTopKOverride,
			weightOverrides:                cloneOpenAIAdvancedSchedulerWeightOverrides(weightOverrides),
			expiresAt:                      time.Now().Add(openAIAdvancedSchedulerSettingCacheTTL).UnixNano(),
		})
		return openAIAdvancedSchedulerRuntimeSettings{
			lowUpstreamRatePriorityEnabled: lowUpstreamRatePriorityEnabled,
			oauthSchedulingRateMultiplier:  oauthSchedulingRateMultiplier,
			enabled:                        enabled,
			stickyWeightedEnabled:          stickyWeightedEnabled,
			subscriptionPriorityEnabled:    subscriptionPriorityEnabled,
			lbTopKOverride:                 lbTopKOverride,
			weightOverrides:                weightOverrides,
		}, nil
	})

	settings, _ := result.(openAIAdvancedSchedulerRuntimeSettings)
	return settings
}

func (s *OpenAIGatewayService) isOpenAIAdvancedSchedulerEnabled(ctx context.Context) bool {
	return s.openAIAdvancedSchedulerRuntimeSettings(ctx).enabled
}

func (s *OpenAIGatewayService) isOpenAILowUpstreamRatePriorityEnabled(ctx context.Context) bool {
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	return !settings.enabled && settings.lowUpstreamRatePriorityEnabled
}

func (s *OpenAIGatewayService) openAIOAuthSchedulingRateMultiplier(ctx context.Context) *float64 {
	return s.openAIAdvancedSchedulerRuntimeSettings(ctx).oauthSchedulingRateMultiplier
}

func (s *OpenAIGatewayService) isOpenAIAdvancedSchedulerStickyWeightedEnabled(ctx context.Context) bool {
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	return settings.enabled && settings.stickyWeightedEnabled
}

func (s *OpenAIGatewayService) isOpenAIAdvancedSchedulerSubscriptionPriorityEnabled(ctx context.Context) bool {
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	return settings.enabled && settings.subscriptionPriorityEnabled
}

func openAIAdvancedSchedulerRuntimeSettingKeys() []string {
	keys := []string{
		SettingKeyOpenAILowUpstreamRatePriorityEnabled,
		SettingKeyOpenAIOAuthSchedulingRateMultiplier,
		openAIAdvancedSchedulerSettingKey,
		SettingKeyOpenAIAdvancedSchedulerStickyWeightedEnabled,
		SettingKeyOpenAIAdvancedSchedulerSubscriptionPriorityEnabled,
		SettingKeyOpenAIAdvancedSchedulerLBTopK,
	}
	for _, spec := range openAIAdvancedSchedulerWeightOverrideSpecs() {
		keys = append(keys, spec.key)
	}
	return keys
}

type openAIAdvancedSchedulerWeightOverrideSpec struct {
	key  string
	name string
}

func openAIAdvancedSchedulerWeightOverrideSpecs() []openAIAdvancedSchedulerWeightOverrideSpec {
	return []openAIAdvancedSchedulerWeightOverrideSpec{
		{key: SettingKeyOpenAIAdvancedSchedulerWeightPriority, name: "priority"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightLoad, name: "load"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightQueue, name: "queue"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightErrorRate, name: "error_rate"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightTTFT, name: "ttft"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightReset, name: "reset"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightQuotaHeadroom, name: "quota_headroom"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightUpstreamCost, name: "upstream_cost"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightPreviousResponse, name: "previous_response"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightSessionSticky, name: "session_sticky"},
	}
}

func parsePositiveIntOverride(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func parseOpenAIAdvancedSchedulerWeightOverrides(values map[string]string) map[string]float64 {
	overrides := map[string]float64{}
	for _, spec := range openAIAdvancedSchedulerWeightOverrideSpecs() {
		raw := strings.TrimSpace(values[spec.key])
		if raw == "" {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		overrides[spec.name] = value
	}
	return overrides
}

func cloneOpenAIAdvancedSchedulerWeightOverrides(in map[string]float64) map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]float64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (s *OpenAIGatewayService) getOpenAIAccountScheduler(ctx context.Context) OpenAIAccountScheduler {
	if s == nil {
		return nil
	}
	if !s.isOpenAIAdvancedSchedulerEnabled(ctx) {
		return nil
	}
	s.openaiSchedulerOnce.Do(func() {
		if s.openaiAccountStats == nil {
			s.openaiAccountStats = newOpenAIAccountRuntimeStats()
		}
		if s.openaiScheduler == nil {
			s.openaiScheduler = newDefaultOpenAIAccountScheduler(s, s.openaiAccountStats)
		}
	})
	return s.openaiScheduler
}

func resetOpenAIAdvancedSchedulerSettingCacheForTest() {
	openAIAdvancedSchedulerSettingCache = atomic.Value{}
	openAIAdvancedSchedulerSettingSF = singleflight.Group{}
}

// TierRoutingClass 是 kimi 分层路由（派发单 B2）的分档结果：估算 input ≤ 阈值判小
// （small → qwen 伪装线池），> 阈值判大（big → 星思池）。
type TierRoutingClass string

const (
	TierRoutingSmall TierRoutingClass = "small"
	TierRoutingBig   TierRoutingClass = "big"
)

// TierRoutingInputThreshold 是分层路由的 input-only 估算阈值（含边界：估算值
// 恰好等于阈值判小），统一采用 3000 input tokens。input-only 口径沿用
// 2026-10-07 用户裁定（docs-local/model-source-tier-routing-plan.md §2）；阈值按
// 2026-10-19 用户裁定统一为 3000。
const TierRoutingInputThreshold = 3000

// TierRoutingClassForEstimate 按估算 input 值分档；=阈值判小。
func TierRoutingClassForEstimate(estimatedInputTokens int) TierRoutingClass {
	if estimatedInputTokens <= TierRoutingInputThreshold {
		return TierRoutingSmall
	}
	return TierRoutingBig
}

// kimi 分档候选池取值（OpenAIAccountScheduleRequest.TierRoutingPool）。
const (
	tierRoutingPoolMasked   = "masked"   // qwen 伪装线池（extra.mask_upstream_identity=true）
	tierRoutingPoolUpstream = "upstream" // 非伪装池（星思）
)

type tierRoutingContextKey struct{}

// tierRoutingState 是请求级分档状态。crossPool 仅在 qwen 伪装线整池无可用后
// 由跨档重试置位（仅 small 允许跨档；big 永不进伪装线）。
type tierRoutingState struct {
	class     TierRoutingClass
	crossPool bool
}

// WithTierRouting 把 kimi 分层路由的分档结果装入请求 ctx；handler 在选号前、
// 估算判定后调用。空 class 不装门。
func WithTierRouting(ctx context.Context, class TierRoutingClass) context.Context {
	if ctx == nil || class == "" {
		return ctx
	}
	return context.WithValue(ctx, tierRoutingContextKey{}, tierRoutingState{class: class})
}

// tierRoutingPoolFromContext 解析请求 ctx 中的分档状态为候选池约束；未装门返回 ""。
func tierRoutingPoolFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	st, ok := ctx.Value(tierRoutingContextKey{}).(tierRoutingState)
	if !ok {
		return ""
	}
	switch st.class {
	case TierRoutingSmall:
		if st.crossPool {
			return tierRoutingPoolUpstream
		}
		return tierRoutingPoolMasked
	case TierRoutingBig:
		return tierRoutingPoolUpstream
	default:
		return ""
	}
}

// TierRoutingGateOpen 是派发单 OC-T1 的动态门控判定：对目标模型 model，门开启当且仅当
// 存在账号 A 满足「管理态 schedulable=true（以调度快照的管理态为准，忽略运行态熔断/
// 配额暂停/临时停调——它们不改管理态 schedulable）且 extra.mask_upstream_identity=true
// 且 A.model_mapping 含键 model」。未开启（无满足条件的账号）→ 返回 false，调用方不装
// 分档门（原路径零行为变化）；运行态不可用不关门。
//
// 账号候选经调度快照（含 DB fallback）取出：schedulerSnapshot 非空时走与选号路径同源的
// ListSchedulableAccounts（snapshot 已带 mask/mapping，选号器的 IsIdentityMaskedAccount /
// IsModelSupported 过滤同源消费，生产已证；hasForcePlatform 取选号同款 false）；snapshot
// 为 nil 时回退既有 repo 查询 ListModelAvailabilityCandidates（与选号器 nil-snapshot 回退
// 同构）。两类候选最终都由纯函数 tierRoutingGateOpenForAccounts 按上述条件判定，便于单测。
func (s *OpenAIGatewayService) TierRoutingGateOpen(ctx context.Context, groupID *int64, platform, model string) (bool, error) {
	if s == nil {
		return false, nil
	}
	if s.schedulerSnapshot != nil {
		// 与选号路径同源：listSchedulableAccounts 对 OpenAI 兼容入口固定 hasForcePlatform=false。
		accounts, _, err := s.schedulerSnapshot.ListSchedulableAccounts(ctx, groupID, platform, false)
		if err != nil {
			return false, err
		}
		return tierRoutingGateOpenForAccounts(accounts, model), nil
	}
	if s.accountRepo == nil {
		return false, nil
	}
	// nil-snapshot 回退：与选号器 nil-snapshot 回退同构（仅管理态候选，忽略运行态瞬态）。
	accounts, err := s.accountRepo.ListModelAvailabilityCandidates(ctx, groupID, []string{platform}, true)
	if err != nil {
		return false, err
	}
	return tierRoutingGateOpenForAccounts(accounts, model), nil
}

// tierRoutingGateOpenForAccounts 是动态门控的判定纯函数（派发单 OC-T1）：任一账号满足
// 「管理态 schedulable=true 且掩码（mask_upstream_identity=true）且 model_mapping 含键
// model」即开门。运行态不可用（熔断/配额暂停/临时停调）一律不关门——这里只看管理态
// Schedulable 字段，绝不调用运行态 IsSchedulable()。
func tierRoutingGateOpenForAccounts(accounts []Account, model string) bool {
	for i := range accounts {
		a := &accounts[i]
		if !a.IsActive() || !a.Schedulable {
			continue
		}
		if !IsIdentityMaskedAccount(a) {
			continue
		}
		if _, ok := a.GetModelMapping()[model]; ok {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) SelectAccountWithScheduler(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requireCompact bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.selectAccountWithScheduler(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, "", "", requireCompact, PlatformOpenAI, false, true, false)
}

// SelectAccountWithSchedulerForCapability 按能力要求调度账号。
// previousResponseCanMove 表示首包 input 可自行重建工具续链，previous_response_id 允许跨账号迁移
// （粘性加权模式下改为加权偏好而非硬粘连）。
// requireVision 表示请求含图，选号时排除已知不支持视觉的账号
// （docs-local/capability-routing-plan.md §3.4）。
func (s *OpenAIGatewayService) SelectAccountWithSchedulerForCapability(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
	previousResponseCanMove bool,
	useUpstreamTokenCost bool,
	requireVision bool,
	platformOverride ...string,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	platform := PlatformOpenAI
	if len(platformOverride) > 0 {
		platform = platformOverride[0]
	}
	return s.selectAccountWithScheduler(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, "", requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost, requireVision)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForImages(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredCapability OpenAIImagesCapability,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	selection, decision, err := s.selectAccountWithScheduler(ctx, groupID, "", sessionHash, requestedModel, excludedIDs, OpenAIUpstreamTransportHTTPSSE, "", requiredCapability, false, PlatformOpenAI, false, false, false)
	if err == nil && selection != nil && selection.Account != nil {
		return selection, decision, nil
	}
	// 如果要求 native 能力（如指定了模型）但没有可用的 APIKey 账号，回退到 basic（OAuth 账号）
	if requiredCapability == OpenAIImagesCapabilityNative {
		return s.selectAccountWithScheduler(ctx, groupID, "", sessionHash, requestedModel, excludedIDs, OpenAIUpstreamTransportHTTPSSE, "", OpenAIImagesCapabilityBasic, false, PlatformOpenAI, false, false, false)
	}
	return selection, decision, err
}

// selectAccountWithScheduler wraps selectAccountWithSchedulerOnce with a
// fail-open second pass for the proxy stream circuit (#5056): when the only
// reason no account is available is that every candidate sits behind a
// quarantined proxy, the quarantine must degrade to a preference instead of
// zeroing out capacity. The retry re-runs the exact same selection with the
// quarantine checks bypassed, so healthy proxies always win the first pass
// and quarantined ones only serve when nothing else can.
func (s *OpenAIGatewayService) selectAccountWithScheduler(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requireCompact bool,
	platform string,
	previousResponseCanMove bool,
	useUpstreamTokenCost bool,
	requireVision bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	selection, decision, err := s.selectAccountWithSchedulerOnce(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost, requireVision)
	// 分层路由跨档（派发单 OC-T1 泛化）：小档请求在伪装线整池无可用时，按既有熔断语义跨档
	// 走星思池一次并告警；反向（大档落伪装线）不允许。跨档依据 ctx 中的分档状态
	// （small + 未 crossPool）判定，不再以平台为门。双侧均无可用时 err 原样返回，
	// 沿 noAvailableOpenAISelectionError 路径 fail-closed——不隐式转其他档，无第三回退源。
	// 跨档故障态优先于 session 粘性（方案 §2 职责隔离）。
	if err != nil &&
		(errors.Is(err, ErrNoAvailableAccounts) || errors.Is(err, ErrNoAvailableCompactAccounts)) {
		if st, ok := ctx.Value(tierRoutingContextKey{}).(tierRoutingState); ok && st.class == TierRoutingSmall && !st.crossPool {
			slog.Warn("kimi_tier_cross_pool_failover",
				"model", requestedModel,
				"from_pool", tierRoutingPoolMasked,
				"to_pool", tierRoutingPoolUpstream,
				"reason", "masked_pool_exhausted",
			)
			st.crossPool = true
			ctx = context.WithValue(ctx, tierRoutingContextKey{}, st)
			selection, decision, err = s.selectAccountWithSchedulerOnce(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost, requireVision)
		}
	}
	if err == nil || openAIProxyStreamQuarantineBypassed(ctx) {
		return selection, decision, err
	}
	if !errors.Is(err, ErrNoAvailableAccounts) && !errors.Is(err, ErrNoAvailableCompactAccounts) {
		return selection, decision, err
	}
	// The circuit only ever quarantines PlatformOpenAI accounts.
	if NormalizeOpenAICompatiblePlatform(platform) != PlatformOpenAI {
		return selection, decision, err
	}
	blocked := s.getOpenAIProxyStreamCircuit().activeBlockCount(time.Now())
	if blocked == 0 {
		return selection, decision, err
	}
	s.logOpenAIProxyStreamQuarantineFailOpen(requestedModel, blocked)
	return s.selectAccountWithSchedulerOnce(withOpenAIProxyStreamQuarantineBypass(ctx), groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost, requireVision)
}

type openAIGroupPrivacyRequirementContextKey struct{}

type openAIGroupPrivacyRequirement struct {
	groupID  int64
	required bool
	group    *Group
}

func (s *OpenAIGatewayService) withOpenAIGroupPrivacyRequirement(ctx context.Context, groupID *int64) context.Context {
	return context.WithValue(ctx, openAIGroupPrivacyRequirementContextKey{}, s.loadOpenAIGroupPrivacyRequirement(ctx, groupID))
}

func (s *OpenAIGatewayService) openAIGroupRequiresPrivacySet(ctx context.Context, groupID *int64) bool {
	if cached, ok := ctx.Value(openAIGroupPrivacyRequirementContextKey{}).(openAIGroupPrivacyRequirement); ok && cached.groupID == derefGroupID(groupID) {
		return cached.required
	}
	return s.loadOpenAIGroupPrivacyRequirement(ctx, groupID).required
}

func (s *OpenAIGatewayService) loadOpenAIGroupPrivacyRequirement(ctx context.Context, groupID *int64) openAIGroupPrivacyRequirement {
	requirement := openAIGroupPrivacyRequirement{groupID: derefGroupID(groupID)}
	if s == nil || groupID == nil || s.schedulerSnapshot == nil {
		return requirement
	}
	// GetByID 会聚合账号计数；隐私开关只在分组行上。
	group, err := s.schedulerSnapshot.GetGroupByIDLite(ctx, *groupID)
	if err != nil {
		requirement.required = true
		return requirement
	}
	requirement.group = group
	requirement.required = group != nil && group.RequirePrivacySet
	return requirement
}

// applyLegacySelectionDecision 把非高级调度路径的选号结果回填到决策：记录选中账号，
// 命中会话粘性时标记 session_hash 层，否则保持 load_balance。
func applyLegacySelectionDecision(decision *OpenAIAccountScheduleDecision, selection *AccountSelectionResult) {
	if decision == nil || selection == nil || selection.Account == nil {
		return
	}
	decision.SelectedAccountID = selection.Account.ID
	decision.SelectedAccountType = selection.Account.Type
	if selection.stickySessionHit {
		decision.Layer = openAIAccountScheduleLayerSessionSticky
		decision.StickySessionHit = true
	}
}

// selectLegacyAccountByPreviousResponse 在非高级调度路径按 previous_response_id 命中持有该响应的账号，
// 先按请求模型做渠道限制检查，再复用高级调度器 previous_response 层的账号兼容校验
// （分组、隐私、运行期封禁、代理隔离、上游模型渠道限制、传输与能力）；未命中返回 (nil, false, nil)。
func (s *OpenAIGatewayService) selectLegacyAccountByPreviousResponse(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requireCompact bool,
	platform string,
) (*AccountSelectionResult, bool, error) {
	if strings.TrimSpace(previousResponseID) == "" || platform != PlatformOpenAI {
		return nil, false, nil
	}
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		return nil, false, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}
	selection, err := s.selectAccountByPreviousResponseIDForCapability(ctx, groupID, previousResponseID, requestedModel, excludedIDs, requiredCapability, requireCompact)
	if err != nil {
		return nil, false, err
	}
	if selection == nil || selection.Account == nil {
		return nil, false, nil
	}
	account := selection.Account
	scheduler := &defaultOpenAIAccountScheduler{service: s, stats: newOpenAIAccountRuntimeStats()}
	compatible, _ := scheduler.isAccountRequestCompatibleReason(ctx, account, OpenAIAccountScheduleRequest{
		GroupID:                 groupID,
		Platform:                platform,
		RequestedModel:          requestedModel,
		RequiredTransport:       requiredTransport,
		RequiredCapability:      requiredCapability,
		RequiredImageCapability: requiredImageCapability,
		RequireCompact:          requireCompact,
		ExcludedIDs:             excludedIDs,
		RequirePrivacySet:       s.openAIGroupRequiresPrivacySet(ctx, groupID),
	})
	if !s.openAIAccountMatchesSchedulingGroup(account, groupID) || !compatible || !scheduler.isAccountTransportCompatible(account, requiredTransport) {
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return nil, false, nil
	}
	if sessionHash != "" {
		_ = s.bindOpenAIStickySessionDuringSelection(ctx, groupID, sessionHash, account.ID)
	}
	return selection, true, nil
}

func (s *OpenAIGatewayService) selectAccountWithSchedulerOnce(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requireCompact bool,
	platform string,
	previousResponseCanMove bool,
	useUpstreamTokenCost bool,
	requireVision bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	ctx = s.withOpenAIGroupPrivacyRequirement(ctx, groupID)
	// 分组利润控制：唯一文本调度入口的防御性装门。handler 文本
	// 入口已在请求开始经 WithOpenAIRequestPricingContext 装门并固定 pricingAt，
	// 此处对同分组门直接复用（failover 重入阈值稳定），仅为不经 handler 装配的
	// 内部调用兜底。图片/视频调度不在利润门范围：requiredImageCapability 非空的
	// Images 调度不装门；其他使用 Responses 能力的文本请求（包括原生远程压缩）
	// 仍须装门。其余媒体路径通过 WithOpenAIProfitControlSuppressed 显式跳过。
	if requiredImageCapability == "" {
		ctx = s.withOpenAIProfitControlGate(ctx, groupID)
	}
	platform = NormalizeOpenAICompatiblePlatform(platform)
	// 分层路由泛化（派发单 OC-T1）：消费请求 ctx 中由 handler 按目标模型 + 动态门装好的
	// 分档状态（平台无关）；只要 ctx 中分档状态非空即透传候选池约束进本请求全部选号路径
	// （粘性/负载均衡/failover 重入共用同一 req），调度器侧不再以平台为门。
	tierRoutingPool := tierRoutingPoolFromContext(ctx)
	// §3.8 全局视觉路由 kill-switch（vision_routing_enabled，默认开启）：关闭后
	// RequireVision 恒为 false，行为回到 v1 现状（普通路由）。单点收口于此，
	// 同时覆盖 §3.4 能力过滤与 §3.7 候选池收窄：置 false 后
	// resolveVisionRoutingTargets 零读取、候选过滤不再执行 vision 判定。
	if requireVision && !s.isVisionRoutingEnabled(ctx) {
		requireVision = false
	}
	decision := OpenAIAccountScheduleDecision{}
	// §3.7 视觉分流：带图请求命中 groups.vision_routing 时，本次选号先读取一次
	// 目标账号集合，透传给候选过滤做正向收窄。读取错误失败关闭（合同 §5），
	// 禁止静默当作未配置——路由不可知时把带图请求发往任意账号会复现静默丢图。
	visionRoutingTargets, err := s.resolveVisionRoutingTargets(ctx, groupID, requestedModel, requireVision)
	if err != nil {
		return nil, decision, err
	}
	if len(visionRoutingTargets) > 0 {
		slog.Info("vision_routed",
			"group_id", derefGroupID(groupID),
			"model", requestedModel,
			"target_account_count", len(visionRoutingTargets),
		)
	}
	preserveGuardianParentBinding := preserveOpenAIGuardianParentBinding(ctx, sessionHash)
	guardianParentAccountID := int64(0)
	if strings.TrimSpace(previousResponseID) == "" {
		guardianParentAccountID = s.resolveOpenAIGuardianParentAccountID(ctx, groupID)
	}
	scheduler := s.getOpenAIAccountScheduler(ctx)
	if scheduler == nil {

		decision.Layer = openAIAccountScheduleLayerLoadBalance
		if selection, hit, err := s.selectLegacyAccountByPreviousResponse(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform); err != nil {
			return nil, decision, err
		} else if hit {
			decision.Layer = openAIAccountScheduleLayerPreviousResponse
			decision.StickyPreviousHit = true
			decision.SelectedAccountID = selection.Account.ID
			decision.SelectedAccountType = selection.Account.Type
			return selection, decision, nil
		}
		if guardianParentAccountID > 0 {
			if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
				return nil, decision, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
			}
			fallbackScheduler := &defaultOpenAIAccountScheduler{service: s, stats: newOpenAIAccountRuntimeStats()}
			selection, _, err := fallbackScheduler.selectBySessionHash(ctx, OpenAIAccountScheduleRequest{
				GroupID:                 groupID,
				Platform:                platform,
				SessionHash:             sessionHash,
				StickyAccountID:         guardianParentAccountID,
				PreserveStickyBinding:   true,
				RequestedModel:          requestedModel,
				RequiredTransport:       requiredTransport,
				RequiredCapability:      requiredCapability,
				RequiredImageCapability: requiredImageCapability,
				RequireCompact:          requireCompact,
				RequireVision:           requireVision,
				VisionRoutingTargets:    visionRoutingTargets,
				TierRoutingPool:         tierRoutingPool,
				ExcludedIDs:             excludedIDs,
				RequirePrivacySet:       s.openAIGroupRequiresPrivacySet(ctx, groupID),
			})
			if err != nil {
				return nil, decision, err
			}
			if selection != nil && selection.Account != nil {
				decision.Layer = openAIAccountScheduleLayerGuardianParent
				decision.StickySessionHit = true
				decision.SelectedAccountID = selection.Account.ID
				decision.SelectedAccountType = selection.Account.Type
				return selection, decision, nil
			}
		}
		legacySessionHash := sessionHash
		if preserveGuardianParentBinding {
			legacySessionHash = ""
		}
		if requiredTransport == OpenAIUpstreamTransportAny || requiredTransport == OpenAIUpstreamTransportHTTPSSE {
			effectiveExcludedIDs := cloneExcludedAccountIDs(excludedIDs)
			for {
				selection, err := s.selectAccountWithLoadAwareness(ctx, groupID, platform, legacySessionHash, requestedModel, effectiveExcludedIDs, requireCompact, requiredCapability, useUpstreamTokenCost)
				if err != nil {
					return nil, decision, err
				}
				if selection == nil || selection.Account == nil {
					return selection, decision, nil
				}
				if accountSupportsOpenAICapabilities(selection.Account, requiredCapability, requiredImageCapability) {
					applyLegacySelectionDecision(&decision, selection)
					return selection, decision, nil
				}
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				if effectiveExcludedIDs == nil {
					effectiveExcludedIDs = make(map[int64]struct{})
				}
				if _, exists := effectiveExcludedIDs[selection.Account.ID]; exists {
					return nil, decision, ErrNoAvailableAccounts
				}
				effectiveExcludedIDs[selection.Account.ID] = struct{}{}
			}
		}

		effectiveExcludedIDs := cloneExcludedAccountIDs(excludedIDs)
		for {
			selection, err := s.selectAccountWithLoadAwareness(ctx, groupID, platform, legacySessionHash, requestedModel, effectiveExcludedIDs, requireCompact, requiredCapability, useUpstreamTokenCost)
			if err != nil {
				return nil, decision, err
			}
			if selection == nil || selection.Account == nil {
				return selection, decision, nil
			}
			if s.isOpenAIAccountTransportCompatible(selection.Account, requiredTransport) &&
				accountSupportsOpenAICapabilities(selection.Account, requiredCapability, requiredImageCapability) {
				applyLegacySelectionDecision(&decision, selection)
				return selection, decision, nil
			}
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			if effectiveExcludedIDs == nil {
				effectiveExcludedIDs = make(map[int64]struct{})
			}
			if _, exists := effectiveExcludedIDs[selection.Account.ID]; exists {
				return nil, decision, ErrNoAvailableAccounts
			}
			effectiveExcludedIDs[selection.Account.ID] = struct{}{}
		}
	}

	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		slog.Warn("channel pricing restriction blocked request",
			"group_id", derefGroupID(groupID),
			"model", requestedModel)
		return nil, decision, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}

	var stickyAccountID int64
	if sessionHash != "" && s.cache != nil {
		if accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash); err == nil && accountID > 0 {
			stickyAccountID = accountID
		}
	}
	stickyWeighted := s.isOpenAIAdvancedSchedulerStickyWeightedEnabled(ctx)
	subscriptionPriority := s.isOpenAIAdvancedSchedulerSubscriptionPriorityEnabled(ctx)
	stickyPreviousAccountID := int64(0)
	if stickyWeighted && previousResponseCanMove && strings.TrimSpace(previousResponseID) != "" && platform == PlatformOpenAI {
		stickyPreviousAccountID = s.ResolveAccountIDByPreviousResponseIDForScheduler(ctx, groupID, previousResponseID, requestedModel, excludedIDs, requiredCapability, requireCompact)
	}

	return scheduler.Select(ctx, OpenAIAccountScheduleRequest{
		GroupID:                 groupID,
		Platform:                platform,
		SessionHash:             sessionHash,
		StickyAccountID:         stickyAccountID,
		GuardianParentAccountID: guardianParentAccountID,
		StickyPreviousAccountID: stickyPreviousAccountID,
		StickyWeighted:          stickyWeighted,
		SubscriptionPriority:    subscriptionPriority,
		PreserveStickyBinding:   preserveGuardianParentBinding,
		RequirePrivacySet:       s.openAIGroupRequiresPrivacySet(ctx, groupID),
		PreviousResponseID:      previousResponseID,
		PreviousResponseCanMove: previousResponseCanMove,
		UseUpstreamTokenCost:    useUpstreamTokenCost,
		RequestedModel:          requestedModel,
		RequiredTransport:       requiredTransport,
		RequiredCapability:      requiredCapability,
		RequiredImageCapability: requiredImageCapability,
		RequireCompact:          requireCompact,
		RequireVision:           requireVision,
		VisionRoutingTargets:    visionRoutingTargets,
		TierRoutingPool:         tierRoutingPool,

		ExcludedIDs: excludedIDs,
	})
}

// isVisionRoutingEnabled 读取全局视觉路由 kill-switch（vision_routing_enabled，
// docs-local/capability-routing-plan.md §3.8）。默认开启：未装配设置服务或未配置时返回
// true，与「未配置 = 开启」的开关定位一致。读取走 SettingService 的进程内缓存链路。
// 设置源取构造函数注入的 s.settingService（权威实例）；rateLimitService 上另挂的
// settingService 非同一注入链，读到错实例会导致配置的 false 被误当默认开启。
func (s *OpenAIGatewayService) isVisionRoutingEnabled(ctx context.Context) bool {
	if s == nil || s.settingService == nil {
		return true
	}
	return s.settingService.IsVisionRoutingEnabled(ctx)
}

// resolveVisionRoutingTargets 在带图请求命中分组视觉分流配置时返回目标账号 ID
// 集合（docs-local/capability-routing-plan.md §3.7）。requireVision=false 或 groupID 为空
// 时零读取、返回 nil。读取错误失败关闭：禁止静默当作未配置（合同 §5），带图请求
// 在路由不可知时发往任意账号会复现静默丢图。
//
// visionRouting 未装配（nil，如部分测试）时按未配置处理返回 nil，保证
// requireVision 既有行为零回归。
func (s *OpenAIGatewayService) resolveVisionRoutingTargets(ctx context.Context, groupID *int64, requestedModel string, requireVision bool) ([]int64, error) {
	if !requireVision || groupID == nil || requestedModel == "" {
		return nil, nil
	}
	if s.visionRouting == nil {
		return nil, nil
	}
	targets, err := s.visionRouting.GetVisionRoutingAccountIDs(ctx, *groupID, requestedModel)
	if err != nil {
		return nil, fmt.Errorf("load vision routing targets for group %d model %s: %w", *groupID, requestedModel, err)
	}
	return targets, nil
}

// visionRoutingTargetContains 判断账号是否属于视觉分流目标集合。
func visionRoutingTargetContains(targets []int64, accountID int64) bool {
	for _, targetID := range targets {
		if targetID == accountID {
			return true
		}
	}
	return false
}

func accountSupportsOpenAICapabilities(account *Account, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability) bool {
	if account == nil {
		return false
	}
	return account.SupportsOpenAIEndpointCapability(requiredCapability) &&
		account.SupportsOpenAIImageCapability(requiredImageCapability)
}

func cloneExcludedAccountIDs(excludedIDs map[int64]struct{}) map[int64]struct{} {
	if len(excludedIDs) == 0 {
		return nil
	}
	cloned := make(map[int64]struct{}, len(excludedIDs))
	for id := range excludedIDs {
		cloned[id] = struct{}{}
	}
	return cloned
}

func (s *OpenAIGatewayService) isOpenAIAccountTransportCompatible(account *Account, requiredTransport OpenAIUpstreamTransport) bool {
	if requiredTransport == OpenAIUpstreamTransportAny || requiredTransport == OpenAIUpstreamTransportHTTPSSE {
		return true
	}
	if s == nil || account == nil {
		return false
	}
	if requiredTransport == OpenAIUpstreamTransportResponsesWebsocketV2Ingress {
		if s.cfg == nil || !s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled {
			return s.getOpenAIWSProtocolResolver().Resolve(account).Transport == OpenAIUpstreamTransportResponsesWebsocketV2
		}
		mode := account.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault)
		switch mode {
		case OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough, OpenAIWSIngressModeHTTPBridge, OpenAIWSIngressModeShared, OpenAIWSIngressModeDedicated:
			return true
		default:
			return false
		}
	}
	return s.getOpenAIWSProtocolResolver().Resolve(account).Transport == requiredTransport
}

func (s *OpenAIGatewayService) ReportOpenAIAccountScheduleResult(account *Account, model string, success bool, firstTokenMs *int, observedErr ...error) bool {
	if account == nil {
		return false
	}
	accountID := account.ID
	healthTripped := false
	if s != nil && s.rateLimitService != nil {
		if success {
			s.rateLimitService.ObserveOpenAIAPIKeyHealthSuccess(context.Background(), account)
			// T5（R4-F3）：调度成功 = 池模式账号实际可用，重置 401 升级窗口状态机；
			// 否则恢复后同一 epoch 内再次 401 不再跨越边沿，账号失去保护。
			resetPool401State(accountID)
		} else if len(observedErr) > 0 && observedErr[0] != nil {
			// 把本次失败的模型贯通到熔断状态：恢复探针据此探测真实故障面，
			// 而不是笼统探平台默认模型（空模型时探针自行回落默认值）。
			healthTripped = s.rateLimitService.ObserveOpenAIAPIKeyHealthFailure(context.Background(), account, observedErr[0], model)
		}
	}
	if success {
		s.openaiOAuth429RetryStartedAt.Delete(accountID)
		// r6 阻塞2（AC-A1）：401 记账在 resolveCredentialAccount 解析后的母账号 ID 上，
		// 而此处原本只清被调度账号 ID，导致影子场景母子键错位、连击永不清零。
		// 对称清键：原账号 ID 与解析后的母账号 ID 两键都清（幂等，非影子场景两键相同）。
		clearModel := normalizeOpenAIAccountModelTransientModel(model)
		s.clearOpenAIAccountModelTransientState(accountID, clearModel)
		if s.accountRepo != nil {
			if resolved, rerr := resolveCredentialAccount(context.Background(), s.accountRepo, account); rerr == nil && resolved != nil && resolved.ID != accountID {
				s.clearOpenAIAccountModelTransientState(resolved.ID, clearModel)
			}
		}
	}
	scheduler := s.getOpenAIAccountScheduler(context.Background())
	if scheduler == nil {
		return healthTripped
	}
	scheduler.ReportResult(accountID, success, firstTokenMs)
	return healthTripped
}

// ObserveOpenAIAccountHealthFailure records failures that cannot reach the
// scheduler-result path, for example after semantic response bytes were sent.
// model 可选：提供时贯通到熔断状态，供恢复探针探测真实故障面。
func (s *OpenAIGatewayService) ObserveOpenAIAccountHealthFailure(ctx context.Context, account *Account, observedErr error, model ...string) bool {
	if s == nil || s.rateLimitService == nil || account == nil || observedErr == nil {
		return false
	}
	return s.rateLimitService.ObserveOpenAIAPIKeyHealthFailure(ctx, account, observedErr, model...)
}

func (s *OpenAIGatewayService) RecordOpenAIAccountSwitch() {
	scheduler := s.getOpenAIAccountScheduler(context.Background())
	if scheduler == nil {
		return
	}
	scheduler.ReportSwitch()
}

func (s *OpenAIGatewayService) SnapshotOpenAIAccountSchedulerMetrics() OpenAIAccountSchedulerMetricsSnapshot {
	scheduler := s.getOpenAIAccountScheduler(context.Background())
	if scheduler == nil {
		return OpenAIAccountSchedulerMetricsSnapshot{}
	}
	return scheduler.SnapshotMetrics()
}

func (s *OpenAIGatewayService) openAIWSSessionStickyTTL() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds > 0 {
		return time.Duration(s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds) * time.Second
	}
	return openaiStickySessionTTL
}

func (s *OpenAIGatewayService) openAIWSLBTopK() int {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.LBTopK > 0 {
		return s.cfg.Gateway.OpenAIWS.LBTopK
	}
	return 7
}

func (s *OpenAIGatewayService) openAIWSLBTopKForRequest(ctx context.Context) int {
	base := s.openAIWSLBTopK()
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	// DB 覆盖值与 stickyWeighted/subscriptionPriority 一样受总开关门控：
	// 关闭高级调度器后所有调用方（含管理页分数快照）都应回到配置/默认行为。
	if !settings.enabled {
		return base
	}
	if settings.lbTopKOverride > 0 {
		return settings.lbTopKOverride
	}
	return base
}

func (s *OpenAIGatewayService) openAIStickyEscapeConfig() openAIStickyEscapeConfig {
	if s != nil && s.cfg != nil {
		cfg := s.cfg.Gateway.OpenAIScheduler
		enabled := cfg.StickyEscapeEnabled
		if !enabled && cfg.StickyEscapeTTFTMs == 0 && cfg.StickyEscapeErrorRate == 0 {
			enabled = true
		}
		ttftMs := float64(cfg.StickyEscapeTTFTMs)
		if ttftMs <= 0 {
			ttftMs = 15000
		}
		errorRate := cfg.StickyEscapeErrorRate
		if errorRate < 0 || errorRate > 1 {
			errorRate = 0.5
		}
		if errorRate == 0 && cfg.StickyEscapeTTFTMs == 0 && cfg.StickyEscapeErrorRate == 0 {
			errorRate = 0.5
		}
		return openAIStickyEscapeConfig{
			enabled:   enabled,
			ttftMs:    ttftMs,
			errorRate: errorRate,
		}
	}
	return openAIStickyEscapeConfig{
		enabled:   true,
		ttftMs:    15000,
		errorRate: 0.5,
	}
}

func (s *OpenAIGatewayService) openAIWSSchedulerWeights() GatewayOpenAIWSSchedulerScoreWeightsView {
	if s != nil && s.cfg != nil {
		return GatewayOpenAIWSSchedulerScoreWeightsView{
			Priority:      s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority,
			Load:          s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load,
			Queue:         s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue,
			ErrorRate:     s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate,
			TTFT:          s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT,
			Reset:         s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Reset,
			QuotaHeadroom: s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.QuotaHeadroom,
			UpstreamCost:  s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.UpstreamCost,
			Previous:      s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.PreviousResponse,
			SessionSticky: s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.SessionSticky,
		}
	}
	return GatewayOpenAIWSSchedulerScoreWeightsView{
		Priority:      1.0,
		Load:          1.0,
		Queue:         0.7,
		ErrorRate:     0.8,
		TTFT:          0.5,
		Reset:         0.0,
		QuotaHeadroom: 0.0,
		UpstreamCost:  0.0,
		Previous:      5.0,
		SessionSticky: 3.0,
	}
}

func (s *OpenAIGatewayService) openAIWSSchedulerWeightsForRequest(ctx context.Context) GatewayOpenAIWSSchedulerScoreWeightsView {
	weights := s.openAIWSSchedulerWeights()
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	// 同 openAIWSLBTopKForRequest：总开关关闭时不应用 DB 覆盖值。
	if !settings.enabled {
		return weights
	}
	overridden := applyOpenAIAdvancedSchedulerWeightOverrides(weights, settings.weightOverrides)
	if !overridden.configWeights().IsValid() {
		return weights
	}
	return overridden
}

// codeBuddyUrgencyBoostForRequest 读取当前生效的 §4.2 紧迫度加权配置。
// 配置值由 internal/config 层校验（启动与热加载同一路径失败关闭），此处直接
// 透传给纯函数 normalize（未启用/非法按 0 处理，恒等语义，无第二份钳位）。
func (s *OpenAIGatewayService) codeBuddyUrgencyBoostForRequest() codeBuddyUrgencyBoost {
	if s == nil || s.cfg == nil {
		return codeBuddyUrgencyBoost{Enabled: false, K: 0}
	}
	cb := s.cfg.Gateway.CodeBuddy
	return codeBuddyUrgencyBoost{Enabled: cb.UrgencyBoostEnabled, K: cb.UrgencyBoostK}
}

func applyOpenAIAdvancedSchedulerWeightOverrides(
	weights GatewayOpenAIWSSchedulerScoreWeightsView,
	overrides map[string]float64,
) GatewayOpenAIWSSchedulerScoreWeightsView {
	for key, value := range overrides {
		switch key {
		case "priority":
			weights.Priority = value
		case "load":
			weights.Load = value
		case "queue":
			weights.Queue = value
		case "error_rate":
			weights.ErrorRate = value
		case "ttft":
			weights.TTFT = value
		case "reset":
			weights.Reset = value
		case "quota_headroom":
			weights.QuotaHeadroom = value
		case "upstream_cost":
			weights.UpstreamCost = value
		case "previous_response":
			weights.Previous = value
		case "session_sticky":
			weights.SessionSticky = value
		}
	}
	return weights
}

type GatewayOpenAIWSSchedulerScoreWeightsView struct {
	Priority  float64
	Load      float64
	Queue     float64
	ErrorRate float64
	TTFT      float64
	// Reset 倾向「会话窗口最早重置」的账号；0 表示关闭（默认）。
	Reset         float64
	QuotaHeadroom float64
	UpstreamCost  float64
	Previous      float64
	SessionSticky float64
}

func (w GatewayOpenAIWSSchedulerScoreWeightsView) configWeights() config.GatewayOpenAIWSSchedulerScoreWeights {
	return config.GatewayOpenAIWSSchedulerScoreWeights{
		Priority:         w.Priority,
		Load:             w.Load,
		Queue:            w.Queue,
		ErrorRate:        w.ErrorRate,
		TTFT:             w.TTFT,
		Reset:            w.Reset,
		QuotaHeadroom:    w.QuotaHeadroom,
		UpstreamCost:     w.UpstreamCost,
		PreviousResponse: w.Previous,
		SessionSticky:    w.SessionSticky,
	}
}

type OpenAIAccountSchedulerScoreSnapshot struct {
	BaseScore             float64
	StickyScore           float64
	StickyScoreInfinity   bool
	StickyWeightedEnabled bool
}

func (s *RateLimitService) BuildOpenAIAccountSchedulerScoreSnapshot(
	ctx context.Context,
	accounts []*Account,
	loadMap map[int64]*AccountLoadInfo,
) map[int64]OpenAIAccountSchedulerScoreSnapshot {
	gateway := &OpenAIGatewayService{cfg: nil, rateLimitService: s}
	if s != nil {
		gateway.cfg = s.cfg
	}
	return buildOpenAIAccountSchedulerScoreSnapshot(
		accounts,
		loadMap,
		gateway.openAIWSSchedulerWeightsForRequest(ctx),
		gateway.isOpenAIAdvancedSchedulerStickyWeightedEnabled(ctx),
		gateway.openAIOAuthSchedulingRateMultiplier(ctx),
	)
}

func BuildOpenAIAccountSchedulerScoreSnapshot(
	accounts []*Account,
	loadMap map[int64]*AccountLoadInfo,
) map[int64]OpenAIAccountSchedulerScoreSnapshot {
	gateway := &OpenAIGatewayService{}
	return buildOpenAIAccountSchedulerScoreSnapshot(accounts, loadMap, gateway.openAIWSSchedulerWeights(), false, parseOpenAIOAuthSchedulingRateMultiplier(nil))
}

func buildOpenAIAccountSchedulerScoreSnapshot(
	accounts []*Account,
	loadMap map[int64]*AccountLoadInfo,
	weights GatewayOpenAIWSSchedulerScoreWeightsView,
	stickyWeightedEnabled bool,
	oauthSchedulingRateMultiplier *float64,
) map[int64]OpenAIAccountSchedulerScoreSnapshot {
	if len(accounts) == 0 {
		return nil
	}
	candidates := make([]openAIAccountCandidateScore, 0, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		loadInfo := loadMap[account.ID]
		if loadInfo == nil {
			loadInfo = &AccountLoadInfo{AccountID: account.ID}
		}
		candidates = append(candidates, openAIAccountCandidateScore{
			account:   account,
			loadInfo:  loadInfo,
			errorRate: 0,
			ttft:      0,
			hasTTFT:   false,
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	minPriority, maxPriority := openAIAccountSchedulingPriority(candidates[0].account), openAIAccountSchedulingPriority(candidates[0].account)
	maxWaiting := 1
	for i := range candidates {
		candidate := &candidates[i]
		candidate.priority = openAIAccountSchedulingPriority(candidate.account)
		if candidate.priority < minPriority {
			minPriority = candidate.priority
		}
		if candidate.priority > maxPriority {
			maxPriority = candidate.priority
		}
		if candidate.loadInfo.WaitingCount > maxWaiting {
			maxWaiting = candidate.loadInfo.WaitingCount
		}
	}

	minResetRemaining, maxResetRemaining := 0.0, 0.0
	hasResetSample := false
	now := time.Now()
	upstreamCostFactors := map[int64]float64(nil)
	if weights.UpstreamCost > 0 {
		accounts := make([]*Account, 0, len(candidates))
		for _, candidate := range candidates {
			accounts = append(accounts, candidate.account)
		}
		upstreamCostFactors = openAIUpstreamCostFactors(accounts, now, oauthSchedulingRateMultiplier)
	}
	if weights.Reset > 0 {
		for _, candidate := range candidates {
			end, ok := openAISchedulingResetWindowEnd(candidate.account, now)
			if !ok {
				continue
			}
			remaining := end.Sub(now).Seconds()
			if !hasResetSample {
				minResetRemaining, maxResetRemaining = remaining, remaining
				hasResetSample = true
				continue
			}
			if remaining < minResetRemaining {
				minResetRemaining = remaining
			}
			if remaining > maxResetRemaining {
				maxResetRemaining = remaining
			}
		}
	}

	result := make(map[int64]OpenAIAccountSchedulerScoreSnapshot, len(candidates))
	for _, candidate := range candidates {
		priorityFactor := 1.0
		if maxPriority > minPriority {
			priorityFactor = 1 - float64(candidate.priority-minPriority)/float64(maxPriority-minPriority)
		}
		loadFactor := 1 - clamp01(float64(candidate.loadInfo.LoadRate)/100.0)
		queueFactor := 1 - clamp01(float64(candidate.loadInfo.WaitingCount)/float64(maxWaiting))
		errorFactor := 1.0
		ttftFactor := 0.5
		resetFactor := 0.0
		if weights.Reset > 0 && hasResetSample {
			if end, ok := openAISchedulingResetWindowEnd(candidate.account, now); ok {
				if maxResetRemaining > minResetRemaining {
					resetFactor = 1 - clamp01((end.Sub(now).Seconds()-minResetRemaining)/(maxResetRemaining-minResetRemaining))
				} else {
					resetFactor = 1
				}
			}
		}
		quotaHeadroomFactor := 0.0
		if weights.QuotaHeadroom > 0 {
			quotaHeadroomFactor = openAIQuotaHeadroomFactor(candidate.account, now)
		}
		upstreamCostFactor := openAIUpstreamCostNeutralFactor
		if factor, ok := upstreamCostFactors[candidate.account.ID]; ok {
			upstreamCostFactor = factor
		}
		baseScore := weights.Priority*priorityFactor +
			weights.Load*loadFactor +
			weights.Queue*queueFactor +
			weights.ErrorRate*errorFactor +
			weights.TTFT*ttftFactor +
			weights.Reset*resetFactor +
			weights.QuotaHeadroom*quotaHeadroomFactor +
			weights.UpstreamCost*(upstreamCostFactor-openAIUpstreamCostNeutralFactor)
		score := OpenAIAccountSchedulerScoreSnapshot{
			BaseScore:             baseScore,
			StickyWeightedEnabled: stickyWeightedEnabled,
			StickyScoreInfinity:   !stickyWeightedEnabled,
		}
		if stickyWeightedEnabled {
			score.StickyScore = baseScore + weights.Previous + weights.SessionSticky
		}
		result[candidate.account.ID] = score
	}
	return result
}

func openAIUpstreamCostFactors(accounts []*Account, now time.Time, oauthSchedulingRateMultiplier *float64) map[int64]float64 {
	type rateSample struct {
		accountID int64
		rate      float64
	}

	factors := make(map[int64]float64, len(accounts))
	samples := make([]rateSample, 0, len(accounts))
	eligibleCount := 0
	for _, account := range accounts {
		if account == nil {
			continue
		}
		factors[account.ID] = openAIUpstreamCostNeutralFactor
		if !account.IsOpenAIApiKey() && !account.IsOpenAIOAuthLike() {
			continue
		}
		eligibleCount++
		if rate, ok := openAISchedulingRate(account, now, oauthSchedulingRateMultiplier); ok {
			samples = append(samples, rateSample{accountID: account.ID, rate: rate})
		}
	}
	if len(samples) < 2 || eligibleCount == 0 {
		return factors
	}

	allEqual := true
	positiveLogs := make([]float64, 0, len(samples))
	for i, sample := range samples {
		if i > 0 && sample.rate != samples[0].rate {
			allEqual = false
		}
		if sample.rate > 0 {
			positiveLogs = append(positiveLogs, math.Log(sample.rate))
		}
	}
	if allEqual || len(positiveLogs) == 0 {
		return factors
	}

	sort.Float64s(positiveLogs)
	middle := len(positiveLogs) / 2
	medianLog := positiveLogs[middle]
	if len(positiveLogs)%2 == 0 {
		medianLog = (positiveLogs[middle-1] + positiveLogs[middle]) / 2
	}
	center := math.Exp(medianLog)
	if center <= 0 || math.IsNaN(center) || math.IsInf(center, 0) {
		return factors
	}

	coverage := float64(len(samples)) / float64(eligibleCount)
	for _, sample := range samples {
		rawFactor := 1.0
		if sample.rate > 0 {
			rawFactor = 1 / (1 + sample.rate/center)
		}
		factors[sample.accountID] = clamp01(openAIUpstreamCostNeutralFactor + coverage*(rawFactor-openAIUpstreamCostNeutralFactor))
	}
	return factors
}

type openAILegacyUpstreamRateOrder struct {
	enabled bool
	rates   map[int64]float64
}

func newOpenAILegacyUpstreamRateOrder(accounts []*Account, now time.Time, oauthSchedulingRateMultiplier *float64) openAILegacyUpstreamRateOrder {
	rates := make(map[int64]float64, len(accounts))
	var first float64
	distinct := false
	for _, account := range accounts {
		if account == nil {
			continue
		}
		// 与 openAIUpstreamCostFactors 使用同一道平台门控：只有 OpenAI 平台账号
		// 的倍率参与 legacy 低倍率优先排序。上游自报倍率来自中转方，不能让它对
		// 其他平台的调度产生影响——否则自报低价即可吸走流量，而实际结算走本地倍率。
		if !account.IsOpenAIApiKey() && !account.IsOpenAIOAuthLike() {
			continue
		}
		rate, ok := openAISchedulingRate(account, now, oauthSchedulingRateMultiplier)
		if !ok {
			continue
		}
		if len(rates) == 0 {
			first = rate
		} else if rate != first {
			distinct = true
		}
		rates[account.ID] = rate
	}
	return openAILegacyUpstreamRateOrder{enabled: len(rates) >= 2 && distinct, rates: rates}
}

func openAISchedulingRate(account *Account, now time.Time, oauthSchedulingRateMultiplier *float64) (float64, bool) {
	if account == nil || (!account.IsOpenAIApiKey() && !account.IsOpenAIOAuthLike()) {
		return 0, false
	}
	if account.IsOpenAIOAuthLike() {
		if rate := oauthSchedulingRateMultiplier; rate != nil && *rate >= 0 && !math.IsNaN(*rate) && !math.IsInf(*rate, 0) {
			return *rate, true
		}
	} else if rate, ok := openAIFreshUpstreamBillingRate(account, now); ok {
		return rate, true
	}
	rate := account.BillingRateMultiplier()
	return rate, !math.IsNaN(rate) && !math.IsInf(rate, 0)
}

// compare returns -1 when a should be selected before b, 1 when b should be
// selected first, and 0 when the rate signal does not distinguish them.
func (o openAILegacyUpstreamRateOrder) compare(a, b *Account) int {
	if !o.enabled || a == nil || b == nil {
		return 0
	}
	aRate, aKnown := o.rates[a.ID]
	bRate, bKnown := o.rates[b.ID]
	if aKnown != bKnown {
		if aKnown {
			return -1
		}
		return 1
	}
	if !aKnown || aRate == bRate {
		return 0
	}
	if aRate < bRate {
		return -1
	}
	return 1
}

func openAIFreshUpstreamBillingRate(account *Account, now time.Time) (float64, bool) {
	accountsnap := account.SnapshotExtra()
	if !isUpstreamBillingProbeAccount(account) {
		return 0, false
	}
	snapshot := decodeUpstreamBillingProbeSnapshot(accountsnap)
	if snapshot == nil || (snapshot.Status != UpstreamBillingProbeStatusOK && snapshot.Status != UpstreamBillingProbeStatusFailed) ||
		snapshot.ReceivedAt == nil || snapshot.ReceivedAt.IsZero() {
		return 0, false
	}
	receivedAt := *snapshot.ReceivedAt
	freshUntil := snapshot.FreshUntil
	if freshUntil == nil && snapshot.Status == UpstreamBillingProbeStatusOK {
		interval := snapshot.NextProbeAt.Sub(receivedAt)
		if interval > 0 {
			freshUntil = probeTimePtr(receivedAt.Add(2 * interval))
		}
	}
	if freshUntil == nil || !freshUntil.After(receivedAt) || now.Before(receivedAt) || now.After(*freshUntil) {
		return 0, false
	}
	return upstreamBillingRateAt(snapshot.Data, now)
}

func openAIQuotaHeadroomFactor(account *Account, now time.Time) float64 {
	accountsnap := account.SnapshotExtra()
	if account == nil || len(accountsnap) == 0 || openAIQuotaHeadroomSnapshotStale(accountsnap, now) {
		return openAIQuotaHeadroomNeutralFactor
	}
	window5h, window7d := openAICanonicalQuotaWindows(accountsnap, now)
	if !window7d.hasUsed || window7d.reset {
		return openAIQuotaHeadroomNeutralFactor
	}

	factor := 1 - clamp01(window7d.usedPercent/100)
	if window5h.hasUsed && !window5h.reset {
		remaining := 1 - clamp01(window5h.usedPercent/100)
		if remaining < openAIQuotaHeadroomSecondaryLowRemain {
			factor *= openAIQuotaHeadroomNeutralFactor
		}
	}
	return factor
}

type openAICanonicalQuotaWindow struct {
	usedPercent float64
	hasUsed     bool
	reset       bool
}

// 规范字段优先；历史原始字段复用写入端 Normalize 的窗口分类，不能固定把 primary 当作 7d。
func openAICanonicalQuotaWindows(extra map[string]any, now time.Time) (window5h, window7d openAICanonicalQuotaWindow) {
	if used, ok := resolveAccountExtraNumber(extra, "codex_5h_used_percent"); ok {
		window5h = openAICanonicalQuotaWindow{usedPercent: used, hasUsed: true, reset: openAIQuotaWindowReset(extra, "5h", now)}
	}
	if used, ok := resolveAccountExtraNumber(extra, "codex_7d_used_percent"); ok {
		window7d = openAICanonicalQuotaWindow{usedPercent: used, hasUsed: true, reset: openAIQuotaWindowReset(extra, "7d", now)}
	}
	if window5h.hasUsed && window7d.hasUsed {
		return window5h, window7d
	}

	snapshot := &OpenAICodexUsageSnapshot{}
	if used, ok := resolveAccountExtraNumber(extra, "codex_primary_used_percent"); ok {
		snapshot.PrimaryUsedPercent = &used
	}
	if used, ok := resolveAccountExtraNumber(extra, "codex_secondary_used_percent"); ok {
		snapshot.SecondaryUsedPercent = &used
	}
	if minutes := parseExtraInt(extra["codex_primary_window_minutes"]); minutes > 0 {
		snapshot.PrimaryWindowMinutes = &minutes
	}
	if minutes := parseExtraInt(extra["codex_secondary_window_minutes"]); minutes > 0 {
		snapshot.SecondaryWindowMinutes = &minutes
	}
	normalized := snapshot.Normalize()
	if normalized == nil {
		return window5h, window7d
	}
	fromRaw := func(used *float64) openAICanonicalQuotaWindow {
		if used == nil {
			return openAICanonicalQuotaWindow{}
		}
		// Normalize 保留原始字段指针，据此配对同一个窗口的用量和重置时间。
		window := "secondary"
		if used == snapshot.PrimaryUsedPercent {
			window = "primary"
		}
		return openAICanonicalQuotaWindow{usedPercent: *used, hasUsed: true, reset: openAIQuotaWindowReset(extra, window, now)}
	}
	if !window5h.hasUsed {
		window5h = fromRaw(normalized.Used5hPercent)
	}
	if !window7d.hasUsed {
		window7d = fromRaw(normalized.Used7dPercent)
	}
	return window5h, window7d
}

func openAISchedulingResetWindowEnd(account *Account, now time.Time) (time.Time, bool) {
	accountsnap := account.SnapshotExtra()
	if account == nil {
		return time.Time{}, false
	}
	if end, ok := openAICodexWindowResetAt(accountsnap, "5h"); ok && now.Before(end) {
		return end, true
	}
	if end := account.SessionWindowEnd; end != nil && now.Before(*end) {
		return *end, true
	}
	return time.Time{}, false
}

func openAIQuotaHeadroomSnapshotStale(extra map[string]any, now time.Time) bool {
	updatedRaw, ok := extra["codex_usage_updated_at"]
	if !ok {
		return true
	}
	updatedAt, err := parseTime(fmt.Sprint(updatedRaw))
	if err != nil {
		return true
	}
	return now.Sub(updatedAt) >= openAIQuotaHeadroomSnapshotStaleAfter
}

func clamp01(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

func calcLoadSkewByMoments(sum float64, sumSquares float64, count int) float64 {
	if count <= 1 {
		return 0
	}
	mean := sum / float64(count)
	variance := sumSquares/float64(count) - mean*mean
	if variance < 0 {
		variance = 0
	}
	return math.Sqrt(variance)
}
