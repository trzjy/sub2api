package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursorupstream"
	"go.uber.org/zap"
)

var (
	ErrNoAvailableCursorAutoToken = errors.New("cursor_auto: no available tokens in dynamic pool")
	ErrAllCardsExhausted          = errors.New("cursor_auto: all activation cards exhausted")
)

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

// DynamicCursorToken 封装单个动态试用号的内存运行态
type DynamicCursorToken struct {
	AccessToken string
	Email       string
	MachineIDs  cursorupstream.MachineIDs
	CardID      int64
	CreatedAt   time.Time
	FailCount   int
	InFlight    int
	CooldownTo  time.Time
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
	CooldownUntil time.Time
	Disabled      bool
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

// RegisterCard 注册/更新一张卡密到多卡聚合器
func (p *CursorDynamicPool) RegisterCard(id int64, cardKey, devCode, devMD5 string, overCount, sumCount int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cards[id] = &ManagedCard{
		ID:            id,
		CardKey:       cardKey,
		DeviceCode:    devCode,
		DeviceCodeMD5: devMD5,
		OverCount:     overCount,
		SumCount:      sumCount,
		IsUnlimited:   sumCount == 99999990,
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
	now := time.Now()

	// 1. 优先命中会话粘性且未进入冷却的 Token
	if sessionID != "" {
		if tok, ok := p.sessionMap[sessionID]; ok {
			if tok.CooldownTo.Before(now) && tok.FailCount == 0 {
				tok.InFlight++
				p.mu.Unlock()
				return tok, nil
			}
			// 绑定的 Token 已失效，解绑并准备漂移接续
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
		card.IsUnlimited = sumCount == 99999990
	}
}

// pullNewTokenFromAnyCard 原子预留卡密额度并在锁外调用网络请求，避免并发超扣并支持自动切至备用卡
//
// 有界性保证：tried 记录本轮已尝试过的卡，每张卡在一次调用里最多被打上游一次，
// 因此上游持续 429 时最多打 len(cards) 次即返回，绝不空转（活锁）。
//
// 冷却语义：只有上游明确回 force_reused（拉号过频）才给该卡加 60s 冷却；
// 429 / 网络错误属于瞬时限流，不给卡加冷却，只做「本轮换下一张卡重试」。
func (p *CursorDynamicPool) pullNewTokenFromAnyCard(ctx context.Context) (*DynamicCursorToken, error) {
	now := time.Now()
	var lastErr error
	// tried: 本轮已尝试的卡 ID，保证每卡至多一次
	tried := make(map[int64]bool)

	for {
		p.mu.Lock()
		var targetCard *ManagedCard
		for _, c := range p.cards {
			if tried[c.ID] {
				continue // 本轮已试过该卡，换下一张
			}
			if c.Disabled || !c.CooldownUntil.Before(now) {
				continue
			}
			if c.IsUnlimited || c.OverCount > 0 {
				targetCard = c
				if !c.IsUnlimited {
					c.OverCount-- // 锁内原子预留
				}
				tried[c.ID] = true // 预留成功即登记，保证每卡至多一次
				break
			}
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

		return &DynamicCursorToken{
			AccessToken: creds.Token,
			Email:       creds.Email,
			MachineIDs:  cursorupstream.GenerateMachineIDs(),
			CardID:      targetCard.ID,
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
	if sessionID != "" {
		delete(p.sessionMap, sessionID)
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
			p.mu.Lock()
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
