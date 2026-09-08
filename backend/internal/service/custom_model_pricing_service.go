package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// CustomModelPricing 全局自定义模型定价条目（价格管理中心维护）。
//
// 定价解析链：分组价 → 渠道价 → 自定义价(本层) → 远程 LiteLLM 表 → 代码内置兜底。
// 语义与渠道定价一致（一条定价可绑定多个模型、支持 * 后缀通配、区间/按次分层），
// 但为全局生效、可启停，且远程同步永不触碰本层。
type CustomModelPricing struct {
	ID               int64             `json:"id"`
	Models           []string          `json:"models"`
	BillingMode      BillingMode       `json:"billing_mode"`
	InputPrice       *float64          `json:"input_price"`
	OutputPrice      *float64          `json:"output_price"`
	CacheWritePrice  *float64          `json:"cache_write_price"`
	CacheReadPrice   *float64          `json:"cache_read_price"`
	FastMultiplier   *float64          `json:"fast_multiplier"`
	FlexMultiplier   *float64          `json:"flex_multiplier"`
	ImageInputPrice  *float64          `json:"image_input_price"`
	ImageOutputPrice *float64          `json:"image_output_price"`
	PerRequestPrice  *float64          `json:"per_request_price"`
	Intervals        []PricingInterval `json:"intervals"`
	Enabled          bool              `json:"enabled"`
	Remark           string            `json:"remark"`
	CreatedBy        int64             `json:"created_by,omitempty"`
	CreatedAt        time.Time         `json:"created_at,omitempty"`
	UpdatedAt        time.Time         `json:"updated_at,omitempty"`
}

// ToChannelModelPricing 转换为渠道定价结构，复用既有的覆盖/分层计算逻辑。
// ChannelID/Platform/TimePricing 对 custom 层无意义，保持零值。
func (p *CustomModelPricing) ToChannelModelPricing() *ChannelModelPricing {
	if p == nil {
		return nil
	}
	return &ChannelModelPricing{
		ID:               p.ID,
		Models:           p.Models,
		BillingMode:      p.BillingMode,
		InputPrice:       p.InputPrice,
		OutputPrice:      p.OutputPrice,
		CacheWritePrice:  p.CacheWritePrice,
		CacheReadPrice:   p.CacheReadPrice,
		FastMultiplier:   p.FastMultiplier,
		FlexMultiplier:   p.FlexMultiplier,
		ImageInputPrice:  p.ImageInputPrice,
		ImageOutputPrice: p.ImageOutputPrice,
		PerRequestPrice:  p.PerRequestPrice,
		Intervals:        p.Intervals,
	}
}

// CustomModelPricingRepository 自定义定价仓储。
type CustomModelPricingRepository interface {
	List(ctx context.Context) ([]CustomModelPricing, error)
	GetByID(ctx context.Context, id int64) (*CustomModelPricing, error)
	Create(ctx context.Context, entry *CustomModelPricing) error
	Update(ctx context.Context, entry *CustomModelPricing) error
	Delete(ctx context.Context, id int64) error
}

const customModelPricingRefreshInterval = time.Minute

// CustomModelPricingService 自定义定价服务：进程内快照缓存 + 定时刷新。
//
// 热路径（每次网关请求的定价解析）只读内存快照；CRUD 同实例即时刷新，
// 多实例部署靠周期刷新达到最终一致。
type CustomModelPricingService struct {
	repo CustomModelPricingRepository

	mu      sync.RWMutex
	entries []CustomModelPricing

	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewCustomModelPricingService(repo CustomModelPricingRepository) *CustomModelPricingService {
	return &CustomModelPricingService{repo: repo, stopCh: make(chan struct{})}
}

// Start 加载快照并启动周期刷新。加载失败不阻断启动（记日志，快照为空等价于无 custom 层）。
func (s *CustomModelPricingService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	if err := s.refresh(); err != nil {
		slog.Warn("custom model pricing initial load failed", "error", err)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(customModelPricingRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := s.refresh(); err != nil {
					slog.Warn("custom model pricing refresh failed", "error", err)
				}
			case <-s.stopCh:
				return
			}
		}
	}()
}

// Stop 停止周期刷新。
func (s *CustomModelPricingService) Stop() {
	if s == nil {
		return
	}
	close(s.stopCh)
	s.wg.Wait()
}

func (s *CustomModelPricingService) refresh() error {
	entries, err := s.repo.List(context.Background())
	if err != nil {
		return fmt.Errorf("list custom model pricing: %w", err)
	}
	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
	return nil
}

func (s *CustomModelPricingService) refreshInBackground() {
	go func() {
		if err := s.refresh(); err != nil {
			slog.Warn("custom model pricing refresh after write failed", "error", err)
		}
	}()
}

// MatchCustomModelPricing 按模型名查找生效的自定义定价（热路径）。
// 精确匹配优先，其次 * 后缀通配（多条通配时取声明顺序的第一个）。
// 返回 Clone，调用方可安全改写。未命中或未配置返回 nil。
func (s *CustomModelPricingService) MatchCustomModelPricing(model string) *ChannelModelPricing {
	if s == nil {
		return nil
	}
	normalized := normalizeChannelPricingModelName(model)
	if normalized == "" {
		return nil
	}
	s.mu.RLock()
	entries := s.entries
	s.mu.RUnlock()

	var wildcard *ChannelModelPricing
	for i := range entries {
		entry := &entries[i]
		if !entry.Enabled {
			continue
		}
		for _, pattern := range entry.Models {
			if normalizeChannelPricingModelName(pattern) == normalized {
				return entry.ToChannelModelPricing()
			}
			if strings.HasSuffix(pattern, "*") &&
				strings.HasPrefix(normalized, strings.TrimSuffix(pattern, "*")) &&
				wildcard == nil {
				wildcard = entry.ToChannelModelPricing()
			}
		}
	}
	return wildcard
}

// Snapshot 返回当前缓存的全量条目（价格目录展示用）。
func (s *CustomModelPricingService) Snapshot() []CustomModelPricing {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CustomModelPricing, len(s.entries))
	copy(out, s.entries)
	return out
}

// List 读取仓储全量（绕过缓存，管理端编辑界面用）。
func (s *CustomModelPricingService) List(ctx context.Context) ([]CustomModelPricing, error) {
	return s.repo.List(ctx)
}

// GetByID 读取单条。
func (s *CustomModelPricingService) GetByID(ctx context.Context, id int64) (*CustomModelPricing, error) {
	return s.repo.GetByID(ctx, id)
}

// validate 校验条目：模型列表非空、无重复通配歧义由调用方容忍，价格字段允许部分配置
// （合并语义：未配置字段沿用更低价层）。
func validateCustomModelPricing(entry *CustomModelPricing) error {
	if entry == nil {
		return fmt.Errorf("entry is nil")
	}
	hasModel := false
	for _, m := range entry.Models {
		if strings.TrimSpace(m) != "" {
			hasModel = true
			break
		}
	}
	if !hasModel {
		return fmt.Errorf("at least one model name is required")
	}
	mode := entry.BillingMode
	if mode == "" {
		mode = BillingModeToken
	}
	switch mode {
	case BillingModeToken, BillingModePerRequest, BillingModeImage, BillingModeVideo:
	default:
		return fmt.Errorf("unsupported billing mode: %s", mode)
	}
	return nil
}

// Create 新增条目并刷新快照。
func (s *CustomModelPricingService) Create(ctx context.Context, entry *CustomModelPricing) error {
	if err := validateCustomModelPricing(entry); err != nil {
		return err
	}
	if entry.BillingMode == "" {
		entry.BillingMode = BillingModeToken
	}
	if err := s.repo.Create(ctx, entry); err != nil {
		return err
	}
	s.refreshInBackground()
	return nil
}

// Update 更新条目并刷新快照。
func (s *CustomModelPricingService) Update(ctx context.Context, entry *CustomModelPricing) error {
	if entry == nil || entry.ID <= 0 {
		return fmt.Errorf("invalid custom model pricing id")
	}
	if err := validateCustomModelPricing(entry); err != nil {
		return err
	}
	if err := s.repo.Update(ctx, entry); err != nil {
		return err
	}
	s.refreshInBackground()
	return nil
}

// Delete 删除条目并刷新快照。
func (s *CustomModelPricingService) Delete(ctx context.Context, id int64) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}
	s.refreshInBackground()
	return nil
}
