package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursorupstream"
	"go.uber.org/zap"
)

var (
	ErrNoAvailableCursorAutoToken = errors.New("cursor_auto: no available tokens in dynamic pool")
	ErrAllCardsExhausted          = errors.New("cursor_auto: all activation cards exhausted")
)

// EnvCursorAutoUpstreamBaseURL 是 Cursor Auto 上游卡密服务的基址环境变量名。
// 上游基址只来自部署环境配置；未配置即失败关闭，不回落代码内默认地址。
const EnvCursorAutoUpstreamBaseURL = "CURSOR_AUTO_UPSTREAM_BASE_URL"

var (
	globalCursorDynamicPool   *CursorDynamicPool
	globalCursorDynamicPoolMu sync.RWMutex
)

// SetGlobalCursorDynamicPool 设置全局动态池单例
func SetGlobalCursorDynamicPool(p *CursorDynamicPool) {
	globalCursorDynamicPoolMu.Lock()
	defer globalCursorDynamicPoolMu.Unlock()
	globalCursorDynamicPool = p
}

// GetGlobalCursorDynamicPool 获取全局动态池单例
func GetGlobalCursorDynamicPool() *CursorDynamicPool {
	globalCursorDynamicPoolMu.RLock()
	defer globalCursorDynamicPoolMu.RUnlock()
	return globalCursorDynamicPool
}

// ResolveCursorAutoUpstreamBaseURL 从部署环境解析 Cursor Auto 上游基址。
//
// 校验口径：非空、绝对 http(s) URL、带主机名、无内嵌凭据。
// 不限制明文 http：上游实际只提供 http，运维显式配置该值即视为授权使用该端点；
// 仍然禁止回落到代码内任何默认地址（未配置即失败关闭）。
// 返回值去掉尾部斜杠。
func ResolveCursorAutoUpstreamBaseURL() (string, error) {
	raw := strings.TrimSpace(os.Getenv(EnvCursorAutoUpstreamBaseURL))
	if raw == "" {
		return "", fmt.Errorf("%s is not configured", EnvCursorAutoUpstreamBaseURL)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid %s: %w", EnvCursorAutoUpstreamBaseURL, err)
	}

	if u.Host == "" {
		return "", fmt.Errorf("invalid %s: missing host", EnvCursorAutoUpstreamBaseURL)
	}

	if u.User != nil {
		return "", fmt.Errorf("invalid %s: embedded credentials are not allowed", EnvCursorAutoUpstreamBaseURL)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid %s: only http/https are allowed", EnvCursorAutoUpstreamBaseURL)
	}

	return strings.TrimRight(u.String(), "/"), nil
}

// DynamicCursorToken 封装单个动态试用号的内存运行态
type DynamicCursorToken struct {
	AccessToken string
	Email       string
	MachineIDs  cursorupstream.MachineIDs
	CardID      int64
	// Generation 记录取号时目标卡密的代际；锁外取号返回后重新加锁时，
	// 必须与 p.cards 中该卡当前的 Generation 严格相等，否则该 Token 被原子丢弃
	// （卡密在网络 I/O 期间被注销或重新注册）。
	Generation int64
	CreatedAt  time.Time
	FailCount  int
	InFlight   int
	CooldownTo time.Time
}

// ManagedCard 封装一张上游卡密及其独占虚拟设备
type ManagedCard struct {
	ID            int64
	CardKey       string
	DeviceCode    string
	DeviceCodeMD5 string
	OverCount     int
	SumCount      int
	IsUnlimited   bool
	// Priority 是管理员设定的选卡优先级：数值越小越优先（与 quota_keys.priority 同语义）。
	Priority      int
	CooldownUntil time.Time
	Disabled      bool
	// Generation 是卡密的代际计数器：每次注册/重新注册都会自增，用于取号时的
	// 原子代际校验（锁外取号返回后比对，识别注销或重新注册）。
	Generation int64
}

// isUnlimitedCard 通用判断某卡密是否为「无限卡」（总额度达到虚拟无限阈值）。
//
// 收紧为上游已验证的两个明确标记集合 {99999999, 99999990}，移除宽泛的
// `>= 90000000` 区间判定：旧口径会把 90000000~99999989 之间的普通大额卡
// 误判为无限卡，导致超额扣减无限卡的额度预留逻辑被错误套用。
func IsUnlimitedCard(sumCount int) bool {
	return sumCount == 99999999 || sumCount == 99999990
}

// CursorDynamicPool 管理 1 对多卡密聚合、Token 预热环、会话粘性及 429 内部自愈
type CursorDynamicPool struct {
	mu           sync.RWMutex
	client       *cursorupstream.Client
	cards        map[int64]*ManagedCard
	activeTokens []*DynamicCursorToken
	sessionMap   map[string]*DynamicCursorToken
	logger       *zap.Logger
	maxInFlight  int // 单 Token 最大并发 (默认 2)
	upstreamErr  error
	genCounter   int64 // 卡密代际单调计数器：每次 RegisterCard 自增并写入目标卡
}

func NewCursorDynamicPool(baseURL string, logger *zap.Logger) *CursorDynamicPool {
	return &CursorDynamicPool{
		client:       cursorupstream.NewClient(baseURL),
		cards:        make(map[int64]*ManagedCard),
		activeTokens: make([]*DynamicCursorToken, 0, 10),
		sessionMap:   make(map[string]*DynamicCursorToken),
		logger:       logger,
		maxInFlight:  2,
	}
}

// MarkUpstreamUnavailable 记录上游基址不可用原因。此后取号直接返回该错误，
// 不访问网络、不回落到 cursorupstream 包内默认地址。
func (p *CursorDynamicPool) MarkUpstreamUnavailable(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upstreamErr = err
}

// RegisterCard 注册/更新一张卡密到多卡聚合器。
//
// priority 为可选参数：不传（生产既有调用方）时默认 0；LoadFromRepository 回放时
// 传入 quota_keys.priority，使选卡排序遵循管理员设定的优先级（数值越小越优先）。
// 使用可变参数以保持既有调用方（handler / 其它测试）无需改动（禁区约束）。
func (p *CursorDynamicPool) RegisterCard(id int64, cardKey, devCode, devMD5 string, overCount, sumCount int, priority ...int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 重新注册（同一卡密再次注册）时，锁内原子清理该卡已存在的所有旧代
	// Token 与会话绑定，防止旧代 Token 被复用（外审必须项）。
	if _, exists := p.cards[id]; exists {
		p.purgeCardTokensLocked(id)
	}

	prio := 0
	if len(priority) > 0 {
		prio = priority[0]
	}

	// 代际自增：同一 ID 的重新注册会得到更大的 Generation，使锁外取号期间
	// 完成的重新注册能被 AcquireToken 的原子代际校验识别并丢弃旧代 Token。
	p.genCounter++
	gen := p.genCounter

	p.cards[id] = &ManagedCard{
		ID:            id,
		CardKey:       cardKey,
		DeviceCode:    devCode,
		DeviceCodeMD5: devMD5,
		OverCount:     overCount,
		SumCount:      sumCount,
		IsUnlimited:   IsUnlimitedCard(sumCount),
		Priority:      prio,
		Generation:    gen,
	}
}

// purgeCardTokensLocked 在调用方已持有 p.mu 的前提下，原子清理指定卡密已存在的
// 所有 Token 与会话绑定：从 activeTokens 中过滤剔除 CardID == cardID 的 Token，
// 并解绑 sessionMap 中关联此 cardID 的会话。供 RegisterCard（重注册清理旧代）
// 与 UnregisterCard（注销清理）复用，防止旧代 Token 被并发复用。
//
// 非线程安全：必须在调用方已加锁（p.mu）时调用。
func (p *CursorDynamicPool) purgeCardTokensLocked(cardID int64) {
	// 从 activeTokens 中过滤剔除 CardID == cardID 的 Token。
	remainingTokens := make([]*DynamicCursorToken, 0, len(p.activeTokens))
	for _, tok := range p.activeTokens {
		if tok.CardID != cardID {
			remainingTokens = append(remainingTokens, tok)
		}
	}
	p.activeTokens = remainingTokens

	// 解绑 sessionMap 中关联此 cardID 的会话。
	for sessID, tok := range p.sessionMap {
		if tok.CardID == cardID {
			delete(p.sessionMap, sessID)
		}
	}
}

// ListCards 返回已纳管卡密快照（供管理端查询）
func (p *CursorDynamicPool) ListCards() []*ManagedCard {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]*ManagedCard, 0, len(p.cards))
	for _, c := range p.cards {
		copied := *c
		out = append(out, &copied)
	}
	return out
}

// GetStats 返回动态池统计指标
func (p *CursorDynamicPool) GetStats() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()

	activeCount := 0
	now := time.Now()
	for _, tok := range p.activeTokens {
		if tok.CooldownTo.Before(now) && tok.FailCount == 0 {
			activeCount++
		}
	}

	return map[string]any{
		"total_cards":       len(p.cards),
		"active_tokens":     activeCount,
		"bound_sessions":    len(p.sessionMap),
		"max_in_flight_cap": p.maxInFlight,
	}
}

// AcquireToken 获取可用 Token：优先遵循会话粘性，其次选择 In-Flight 最小的可用 Token
func (p *CursorDynamicPool) AcquireToken(ctx context.Context, sessionID string) (*DynamicCursorToken, error) {
	p.mu.Lock()
	if p.upstreamErr != nil {
		err := p.upstreamErr
		p.mu.Unlock()
		return nil, err
	}
	now := time.Now()

	// 1. 优先命中会话粘性且未进入冷却的 Token
	//
	// 并发上限口径与普通选取分支一致：粘性命中同样必须满足 InFlight < maxInFlight。
	// 否则同一 sessionID 的第三次并发调用会把已满载的 Token 顶到 maxInFlight+1，
	// 绕过单 Token 并发上限（增量外审 P1）。达到上限时走下面的解绑路径，
	// 漂移到别的有余量的 Token 或拉新 Token。
	if sessionID != "" {
		if tok, ok := p.sessionMap[sessionID]; ok {
			if tok.CooldownTo.Before(now) && tok.FailCount == 0 && tok.InFlight < p.maxInFlight {
				tok.InFlight++
				p.mu.Unlock()
				return tok, nil
			}
			// 绑定的 Token 已失效或已达并发上限，解绑并准备漂移接续
			delete(p.sessionMap, sessionID)
		}
	}

	// 2. 选择负载最小 (InFlight 最小) 的可用 Token
	var best *DynamicCursorToken
	for _, tok := range p.activeTokens {
		if tok.CooldownTo.Before(now) && tok.FailCount == 0 && tok.InFlight < p.maxInFlight {
			if best == nil || tok.InFlight < best.InFlight {
				best = tok
			}
		}
	}

	if best != nil {
		best.InFlight++
		if sessionID != "" {
			p.sessionMap[sessionID] = best
		}
		p.mu.Unlock()
		return best, nil
	}

	// 3. 细粒度优化：锁内无法找到 Token，释放主锁后发起上游网络拉取，防止网络 I/O 阻塞其他请求
	p.mu.Unlock()

	newTok, err := p.pullNewTokenFromAnyCard(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoAvailableCursorAutoToken, err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 锁内原子代际校验：网络 I/O 期间目标卡可能已被注销或重新注册。
	// 不仅要求卡仍存在，还要求代际严格等于发起取号时的代际；否则丢弃旧代 Token，
	// 绝不加入 activeTokens 或 sessionMap（并发注销/重注册竞态防护）。
	card, exists := p.cards[newTok.CardID]
	if !exists || card.Generation != newTok.Generation {
		return nil, fmt.Errorf("%w: card %d was invalidated during token acquire", ErrNoAvailableCursorAutoToken, newTok.CardID)
	}

	p.activeTokens = append(p.activeTokens, newTok)
	newTok.InFlight++
	if sessionID != "" {
		p.sessionMap[sessionID] = newTok
	}
	return newTok, nil
}

// UpdateCardCounts 仅更新卡密的额度与总额，完整保留冷却与禁用状态（外审 P2 修复）
func (p *CursorDynamicPool) UpdateCardCounts(id int64, overCount, sumCount int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if card, ok := p.cards[id]; ok {
		card.OverCount = overCount
		card.SumCount = sumCount
		card.IsUnlimited = IsUnlimitedCard(sumCount)
	}
}

// EnsureCardRegistered 原子地「不存在才纳管」，供单卡刷新成功后的恢复路径使用。
//
// 与「先判存在、再 RegisterCard」两步组合的关键差别：判定与纳管在 p.mu 内一次完成，
// 不会出现两个并发刷新都判定卡不在池、先后 RegisterCard，由后者清理掉前者刚建立的
// Token 与会话绑定。卡已在池内时只刷新额度，保留 Token、会话粘性、冷却与禁用态
// （与 UpdateCardCounts 同语义）。返回 true 表示本次新纳管。
func (p *CursorDynamicPool) EnsureCardRegistered(id int64, cardKey, devCode, devMD5 string, overCount, sumCount int, priority ...int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if card, ok := p.cards[id]; ok {
		card.OverCount = overCount
		card.SumCount = sumCount
		card.IsUnlimited = IsUnlimitedCard(sumCount)
		return false
	}
	prio := 0
	if len(priority) > 0 {
		prio = priority[0]
	}
	p.genCounter++
	p.cards[id] = &ManagedCard{
		ID:            id,
		CardKey:       cardKey,
		DeviceCode:    devCode,
		DeviceCodeMD5: devMD5,
		OverCount:     overCount,
		SumCount:      sumCount,
		IsUnlimited:   IsUnlimitedCard(sumCount),
		Priority:      prio,
		Generation:    p.genCounter,
	}
	return true
}

// UnregisterCard 从多卡聚合器注销一张卡密（卡密被删除或过期时调用）。
//
// 加锁原子完成三步清理：
//  1. 从 p.cards 移除目标卡；
//  2. 遍历 p.activeTokens 剔除所有 CardID == id 的 Token（失效卡不再提供号）；
//  3. 遍历 p.sessionMap 解绑所有指向被剔除 Token 的会话粘性。
//
// 并发防重入：全程持 p.mu，与选卡取号（pullNewTokenFromAnyCard 的锁内候选扫描、
// 锁外取号后的 p.cards 存在性复核）互斥，保证注销与取号不会出现竞态残留。
func (p *CursorDynamicPool) UnregisterCard(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 1. 移除目标卡。
	delete(p.cards, id)

	// 2. + 3. 复用统一清理：剔除该卡的活跃 Token 并解绑关联会话。
	p.purgeCardTokensLocked(id)
}

// UpdateCardPriority 加锁更新目标卡的选卡优先级。
//
// 并发防重入：持 p.mu 保证与选卡排序（读 Priority）互斥；卡不存在时为无操作。
func (p *CursorDynamicPool) UpdateCardPriority(id int64, priority int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if card, ok := p.cards[id]; ok {
		card.Priority = priority
	}
}

// LoadFromRepository 从库内有效卡密回放重建号池（服务重启后恢复内存状态）。
//
// 查询库内有效卡密（status=active、软删 deleted_at 为空、未过期），过滤 notes
// 以 `auto_pool:` 前缀的行，解析虚拟设备凭据（dev/md5）、提取 priority 与 usage_snapshot
// 快照额度，逐张调用 RegisterCard 纳管。ent 的 SoftDeleteMixin 已在查询层自动排除
// 软删行，此处仍显式再次校验 deleted_at 以防御绕开拦截器的写入路径。
//
// 并发防重入：逐卡注册均进入 WithAutoCardSync(qk.ID) 串行边界，与运行期单卡刷新
// 的「DB 写入 → 池同步」串行区同形，避免启动回放与运行期刷新交错造成池成员分叉；
// RegisterCard 自身也加锁，逐卡注册互不冲突。
//
// 生命周期约束（外审 R7 残余风险整改）：本方法**只在进程启动、HTTP 开始接流量之前
// 调用一次**，装配点为 ProvideCursorExecutionService。它是启动期的批量快照回放，
// 不在运行期与 DB 持续同步。若将来需要在运行期触发回放，必须按卡重读**当前**持久化
// 状态（GetStatus）后再同步到运行池，绝不得沿用启动时抓取的这批批量快照 —— 否则
// 会把启动瞬间的旧额度/旧资格误当作运行期权威状态，再次造成运行池与 DB 分叉。
func (p *CursorDynamicPool) LoadFromRepository(ctx context.Context, repo QuotaKeyRepository) error {
	keys, err := repo.List(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	for _, qk := range keys {
		// 过滤：仅 status=active 且未过期且未软删的有效卡密。
		if qk.Status != "active" {
			continue
		}
		if qk.DeletedAt != nil {
			continue
		}
		if qk.ExpiresAt != nil && qk.ExpiresAt.Before(now) {
			continue
		}
		// 仅回放 Auto 渠道卡密（notes 以 auto_pool: 前缀标记）。
		if qk.Notes == nil || !strings.HasPrefix(*qk.Notes, autoPoolNotesPrefix) {
			continue
		}
		// 启动失败态硬拦截：绑定失败（`bind=failed`）的卡密绝不入池。
		if strings.Contains(*qk.Notes, autoPoolBindFailedMarker) {
			continue
		}
		// 启动失败态硬拦截：存在未清除的预热/查额错误（LastRefreshError 非空且非空白）
		// 的卡密尚未恢复，禁止装配入池。
		if qk.LastRefreshError != nil && strings.TrimSpace(*qk.LastRefreshError) != "" {
			continue
		}
		devCode, devMD5, ok := parseAutoPoolNotes(*qk.Notes)
		if !ok {
			if p.logger != nil {
				p.logger.Warn("cursor_auto: skip auto_pool card with unparseable notes",
					zap.Int64("quota_key_id", qk.ID),
				)
			}
			continue
		}
		overCount, sumCount := snapshotQuotaFromUsage(json.RawMessage(qk.UsageSnapshot))
		// cardKey 仅作标识（网络取号只用 dev/md5）；库内无明文激活码，取脱敏末 4 位。
		cardKey := qk.CodeLast4
		// 进入按卡串行边界（与运行期单卡刷新的纳管同形），防止启动回放与运行期
		// 刷新交错造成池成员分叉。
		_ = WithAutoCardSync(qk.ID, func() error {
			p.RegisterCard(qk.ID, cardKey, devCode, devMD5, overCount, sumCount, qk.Priority)
			return nil
		})
	}
	return nil
}

// autoPoolNotesPrefix 是 Auto 卡密在 quota_keys.notes 中的标记前缀（与
// handler/admin/quota_key_auto_handler.go 保持字面一致，避免重复定义漂移）。
const autoPoolNotesPrefix = "auto_pool:"

// autoPoolBindFailedMarker 是卡密 notes 中标记「绑定失败」的子串。
// 绑定失败的卡密绝不允许装配入动态池（启动失败态拦截）。
const autoPoolBindFailedMarker = "bind=failed"

// parseAutoPoolNotes 解析 `auto_pool:dev=...;md5=...` 格式的 notes，提取虚拟设备凭据。
// 返回 (devCode, devMD5, ok)；任一缺失即 ok=false。
func parseAutoPoolNotes(notes string) (devCode, devMD5 string, ok bool) {
	if !strings.HasPrefix(notes, autoPoolNotesPrefix) {
		return "", "", false
	}
	body := notes[len(autoPoolNotesPrefix):]
	parts := strings.Split(body, ";")
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "dev":
			devCode = kv[1]
		case "md5":
			devMD5 = kv[1]
		}
	}
	if devCode == "" || devMD5 == "" {
		return "", "", false
	}
	return devCode, devMD5, true
}

// snapshotQuotaFromUsage 从 quota_keys.usage_snapshot（JSONB）解析来源额度快照，
// 返回 (overCount 剩余, sumCount 总额)。快照口径：total - used = 剩余。
// 无快照 / 解析失败 / 负剩余均回落为 (0, 0)，不臆造额度事实（与 schema 注释一致）。
func snapshotQuotaFromUsage(snap json.RawMessage) (overCount, sumCount int) {
	if len(snap) == 0 {
		return 0, 0
	}
	var data struct {
		Total int `json:"total"`
		Used  int `json:"used"`
	}
	if err := json.Unmarshal(snap, &data); err != nil {
		return 0, 0
	}
	sumCount = data.Total
	overCount = data.Total - data.Used
	if overCount < 0 {
		overCount = 0
	}
	return overCount, sumCount
}

// pullNewTokenFromAnyCard 原子预留卡密额度并在锁外调用网络请求，避免并发超扣并支持自动切至备用卡
//
// 有界性保证：tried 记录本轮已尝试过的卡，每张卡在一次调用里最多被打上游一次，
// 因此上游持续 429 时最多打 len(cards) 次即返回，绝不空转（活锁）。
//
// 冷却语义：只有上游明确回 force_reused（拉号过频）才给该卡加 60s 冷却；
// 429 / 网络错误属于瞬时限流，不给卡加冷却，只做「本轮换下一张卡重试」。
func (p *CursorDynamicPool) pullNewTokenFromAnyCard(ctx context.Context) (*DynamicCursorToken, error) {
	var lastErr error
	// tried: 本轮已尝试的卡 ID，保证每卡至多一次
	tried := make(map[int64]bool)

	for {
		now := time.Now()

		p.mu.Lock()
		var targetCard *ManagedCard
		// candidates: 本轮尚未尝试、未禁用、已退出冷却、且有可用额度（无限卡或剩余 > 0）的候选卡。
		// 选卡排序：按 Priority 升序（数值越小越优先）；同优先级按 OverCount 剩余次数多者优先（更耐用）。
		candidates := make([]*ManagedCard, 0, len(p.cards))
		for _, c := range p.cards {
			if tried[c.ID] {
				continue // 本轮已试过该卡，换下一张
			}
			if c.Disabled || !c.CooldownUntil.Before(now) {
				continue
			}
			if c.IsUnlimited || c.OverCount > 0 {
				candidates = append(candidates, c)
			}
		}
		if len(candidates) > 0 {
			sort.Slice(candidates, func(i, j int) bool {
				if candidates[i].Priority != candidates[j].Priority {
					return candidates[i].Priority < candidates[j].Priority
				}
				return candidates[i].OverCount > candidates[j].OverCount
			})
			targetCard = candidates[0]
			if !targetCard.IsUnlimited {
				targetCard.OverCount-- // 锁内原子预留
			}
			tried[targetCard.ID] = true // 预留成功即登记，保证每卡至多一次
		}
		p.mu.Unlock()

		if targetCard == nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, ErrAllCardsExhausted
		}

		creds, err := p.client.GetCredentialsSecure(ctx, targetCard.DeviceCode, targetCard.DeviceCodeMD5)
		if err != nil {
			// 取号失败（429 / 网络错误 / ctx 取消）属于瞬时限流：不冷却该卡，
			// 只归还预留额度并记 lastErr，然后由 tried 集合保证下一轮换一张卡，
			// 避免反复选中同一张卡形成无限活锁（空转打上游）。
			p.mu.Lock()
			if !targetCard.IsUnlimited {
				targetCard.OverCount++
			}
			p.mu.Unlock()
			if p.logger != nil {
				p.logger.Warn("cursor_auto: upstream credentials fetch failed, trying next card",
					zap.Int64("card_id", targetCard.ID),
					zap.Error(err),
				)
			}
			lastErr = err
			continue
		}

		if creds.ForceReused {
			p.mu.Lock()
			targetCard.CooldownUntil = time.Now().Add(60 * time.Second)
			if !targetCard.IsUnlimited {
				targetCard.OverCount++ // 强制复用未扣额度，归还预留
			}
			p.mu.Unlock()
			lastErr = cursorupstream.ErrAccountCooling
			continue // 自动切至备用卡
		}

		// 锁外网络取号成功：再次持锁检查目标卡是否已被并发注销（UnregisterCard）。
		// 若卡已被移除，本次取到的 Token 直接丢弃、不加入活跃池，并继续选下一张卡，
		// 绝不让失效卡密选中的 Token 进入可用集合（并发注销竞态防护）。
		p.mu.Lock()
		if _, stillPresent := p.cards[targetCard.ID]; !stillPresent {
			delete(tried, targetCard.ID) // 放行：该卡已不存在，下一轮不会再被选中
			p.mu.Unlock()
			if p.logger != nil {
				p.logger.Warn("cursor_auto: upstream token discarded, target card concurrently unregistered",
					zap.Int64("card_id", targetCard.ID),
				)
			}
			continue
		}
		p.mu.Unlock()

		return &DynamicCursorToken{
			AccessToken: creds.Token,
			Email:       creds.Email,
			MachineIDs:  cursorupstream.GenerateMachineIDs(),
			CardID:      targetCard.ID,
			Generation:  targetCard.Generation,
			CreatedAt:   time.Now(),
		}, nil
	}
}

// ReleaseToken 请求结束释放并发占用
func (p *CursorDynamicPool) ReleaseToken(tok *DynamicCursorToken) {
	if tok == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok.InFlight > 0 {
		tok.InFlight--
	}
}

// MarkTokenError 收到上游 429/Too-Many 等状态时标记故障，触发换号（站点侧自愈硬线）
func (p *CursorDynamicPool) MarkTokenError(tok *DynamicCursorToken, sessionID string, cooldownDuration time.Duration) {
	if tok == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	tok.FailCount++
	tok.CooldownTo = time.Now().Add(cooldownDuration)
	// 解绑只针对「会话当前仍绑在出故障的这个 Token 上」的情形。
	//
	// 会话因并发上限/冷却漂移到新 Token 后（AcquireToken 里 p.sessionMap[sessionID]
	// 已指向新 Token），旧 Token 迟到的 429 仍会走到这里。若无条件 delete，
	// 会把**新绑定**误删：会话粘性被无谓打断，且该 Token 未必已故障。
	// 故只有映射仍指向传入的 tok 时才解绑；已迁移到别的 Token 则保留新绑定。
	if sessionID != "" {
		if cur, ok := p.sessionMap[sessionID]; ok && cur == tok {
			delete(p.sessionMap, sessionID)
		}
	}

	if p.logger != nil {
		p.logger.Warn("cursor_auto: token marked exhausted/cooling, evicted from active pool",
			zap.String("email", tok.Email),
			zap.Duration("cooldown", cooldownDuration),
			zap.String("session_id", sessionID),
		)
	}

	// 异步预热补充一个新 Token 并正确追加到活跃池中
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if newTok, err := p.pullNewTokenFromAnyCard(ctx); err == nil && newTok != nil {
			// 锁内原子代际校验：异步预热取号期间目标卡可能已被注销或重新注册
			// （Generation 不匹配）。此时原子丢弃该 Token，绝不加入 activeTokens，
			// 防止旧代 Token 进入可用集合（并发注销/重注册竞态防护）。
			p.mu.Lock()
			card, ok := p.cards[newTok.CardID]
			if !ok || card.Generation != newTok.Generation {
				p.mu.Unlock()
				return
			}
			p.activeTokens = append(p.activeTokens, newTok)
			p.mu.Unlock()
			if p.logger != nil {
				p.logger.Info("cursor_auto: successfully prewarmed new token into pool",
					zap.String("email", newTok.Email),
					zap.Int64("card_id", newTok.CardID),
				)
			}
		}
	}()
}
