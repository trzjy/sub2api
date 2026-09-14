package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// APIKeyLister 是 promo intel(self 模式) 需要的最小 API Key 读取面：
// 列出管理端可见 key、按 ID 取回明文（key 明文仅存于服务端，绝不回读给前端）。
type APIKeyLister interface {
	ListAdminAPIKeys(ctx context.Context) ([]AdminAPIKeyRef, error)
	GetAPIKeyByID(ctx context.Context, id int64) (string, error) // 返回明文 key
}

// AdminAPIKeyRef 是管理端 API Key 的选择器条目（不含明文）。
// 带分组与该分组的模型白名单，让管理员选 key 时就知道能用哪些模型——
// 否则模型框自由填写只能撞运气（key 的分组不认这个模型时网关会 404）。
type AdminAPIKeyRef struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	OwnerEmail string   `json:"owner_email"`
	GroupID    *int64   `json:"group_id"`
	GroupName  string   `json:"group_name"`
	Platform   string   `json:"platform"`
	Models     []string `json:"models"` // 分组模型白名单；未启用白名单为空（不限）
}

// PromoIntelRepository 优惠情报仓储契约（service 层定义，repository 层实现）。
type PromoIntelRepository interface {
	// 资讯源
	CreateSource(ctx context.Context, src *PromoIntelSource) error
	GetSourceByID(ctx context.Context, id int64) (*PromoIntelSource, error)
	GetSourceByName(ctx context.Context, name string) (*PromoIntelSource, error)
	UpdateSource(ctx context.Context, src *PromoIntelSource) error
	DeleteSource(ctx context.Context, id int64) error
	ListSources(ctx context.Context, params PromoIntelSourceListParams) ([]*PromoIntelSource, int64, error)
	// ListDueSources 列出到期源：enabled 且 (last_fetched_at IS NULL 或已过各自间隔)。
	ListDueSources(ctx context.Context, now time.Time, limit int) ([]*PromoIntelSource, error)
	// MarkSourceFetched 记录最近一次抓取结果；extractedHash 为 nil 表示保持不变
	//（降级路径不推进已整理指纹，LLM 可用后自动补跑）。
	MarkSourceFetched(ctx context.Context, id int64, status, lastErr string, extractedHash *string, at time.Time) error
	// 情报条目
	// UpsertItem 按 fingerprint 去重写入；返回是否新建。冲突时只刷新内容字段，
	// 绝不覆盖管理员分诊状态（status）与首次发现日期（digest_date）。
	UpsertItem(ctx context.Context, item *PromoIntelItem) (bool, error)
	GetItemByID(ctx context.Context, id int64) (*PromoIntelItem, error)
	UpdateItemStatus(ctx context.Context, id int64, status string) error
	DeletePendingItemsBySource(ctx context.Context, sourceID int64) (int64, error)
	ListItems(ctx context.Context, params PromoIntelItemListParams) ([]*PromoIntelItem, int64, error)
}

// infraerrorsBadRequest 构造参数类业务错误。
func infraerrorsBadRequest(msg string) error {
	return infraerrors.BadRequest("PROMO_INTEL_VALIDATION_ERROR", msg)
}

// PromoIntelService 优惠情报服务：资讯源 CRUD + 每日轮询循环 + LLM 整理编排 + 简报。
type PromoIntelService struct {
	repo     PromoIntelRepository
	settings SettingRepository
	cfg      *config.PromoIntelConfig

	// apiKeyLister 仅用于 self 模式：列出/读取本系统管理员 API Key（明文在服务端）。
	apiKeyLister APIKeyLister
	// serverBaseURL 是本系统中转网关的对内地址（如 http://127.0.0.1:3300），
	// 未配置时回退 http://localhost:<server_port>。
	serverBaseURL string

	// 多实例防重复（nil 时单实例直接运行）。
	lockCache  LeaderLockCache
	db         *sql.DB
	instanceID string

	nowFn func() time.Time

	// 测试注入。
	testFetchClient *http.Client
	testLLMClient   *http.Client

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	startMu sync.Mutex
	started bool

	inFlight   map[int64]struct{}
	inFlightMu sync.Mutex
}

// NewPromoIntelService 构造服务；Start 前不启动任何后台行为。
func NewPromoIntelService(
	repo PromoIntelRepository,
	settings SettingRepository,
	cfg *config.PromoIntelConfig,
	apiKeyLister APIKeyLister,
	serverBaseURL string,
) *PromoIntelService {
	return &PromoIntelService{
		repo:          repo,
		settings:      settings,
		cfg:           cfg,
		apiKeyLister:  apiKeyLister,
		serverBaseURL: serverBaseURL,
		instanceID:    uuid.NewString(),
		nowFn:         time.Now,
		inFlight:      make(map[int64]struct{}),
		stopCh:        make(chan struct{}),
	}
}

// SetLeaderLock 注入多实例协同（dashboard_aggregation 同款 setter 注入）。
func (s *PromoIntelService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	s.lockCache = lockCache
	s.db = db
}

// SetTestClients 注入测试客户端（生产为 nil）。
func (s *PromoIntelService) SetTestClients(fetch, llm *http.Client) {
	s.testFetchClient = fetch
	s.testLLMClient = llm
}

func (s *PromoIntelService) scanInterval() time.Duration {
	if s.cfg != nil && s.cfg.ScanIntervalSeconds > 0 {
		return time.Duration(s.cfg.ScanIntervalSeconds) * time.Second
	}
	return PromoIntelDefaultScanIntervalSec * time.Second
}

func (s *PromoIntelService) workerConcurrency() int {
	if s.cfg != nil && s.cfg.WorkerConcurrency > 0 {
		return s.cfg.WorkerConcurrency
	}
	return PromoIntelDefaultWorkerConcurrency
}

func (s *PromoIntelService) dueBatchSize() int {
	if s.cfg != nil && s.cfg.DueBatchSize > 0 {
		return s.cfg.DueBatchSize
	}
	return 20
}

const promoIntelLeaderLockKey = "promo_intel:scan:leader"

// Start 启动轮询循环：补种默认源 → 立即扫一轮 → 按周期扫描。
func (s *PromoIntelService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return
	}
	if s.cfg != nil && !s.cfg.Enabled {
		slog.Info("promo_intel: disabled by config, loop not started")
		return
	}
	s.started = true

	if s.cfg == nil || s.cfg.SeedDefaults {
		if err := s.ensureDefaultSources(context.Background()); err != nil {
			slog.Warn("promo_intel: seed default sources failed", "err", err)
		}
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// 启动即先扫一轮（错开抖动，避免与其它启动任务挤在一起）。
		select {
		case <-s.stopCh:
			return
		case <-time.After(time.Duration(rand.Intn(10)) * time.Second):
		}
		s.scanDueOnce()
		ticker := time.NewTicker(s.scanInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.scanDueOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
	slog.Info("promo_intel: loop started", "scan_interval", s.scanInterval().String())
}

// Stop 停止循环并等待在途抓取退出。
func (s *PromoIntelService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

// scanDueOnce 扫描一轮到期源；多实例经 leader lock 只有一个实例执行。
func (s *PromoIntelService) scanDueOnce() {
	if s.isDisabledBySetting() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	release, ok := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, promoIntelLeaderLockKey, s.instanceID, s.scanInterval()*4)
	if !ok {
		return
	}
	defer release()

	due, err := s.repo.ListDueSources(ctx, s.nowFn(), s.dueBatchSize())
	if err != nil {
		slog.Error("promo_intel: list due sources failed", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}
	sem := make(chan struct{}, s.workerConcurrency())
	var wg sync.WaitGroup
	for _, src := range due {
		if !s.tryAcquireInFlight(src.ID) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(src *PromoIntelSource) {
			defer wg.Done()
			defer func() { <-sem; s.releaseInFlight(src.ID) }()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("promo_intel: panicked processing source", "source_id", src.ID, "panic", r)
				}
			}()
			res, err := s.ProcessSource(ctx, src)
			if err != nil {
				slog.Warn("promo_intel: source processed with error",
					"source", src.Name, "err", err)
				return
			}
			if res.ContentChanged {
				slog.Info("promo_intel: source content changed",
					"source", src.Name, "items_created", res.ItemsCreated, "items_updated", res.ItemsUpdated,
					"skipped", res.SkippedReason)
			}
		}(src)
	}
	wg.Wait()
}

func (s *PromoIntelService) isDisabledBySetting() bool {
	rt := s.GetPromoIntelRuntime(context.Background())
	return !rt.Enabled
}

func (s *PromoIntelService) tryAcquireInFlight(id int64) bool {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	if s.inFlight == nil {
		s.inFlight = make(map[int64]struct{})
	}
	if _, busy := s.inFlight[id]; busy {
		return false
	}
	s.inFlight[id] = struct{}{}
	return true
}

func (s *PromoIntelService) releaseInFlight(id int64) {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	delete(s.inFlight, id)
}

// ProcessSource 抓取单个源并整理（循环与「立即抓取」共用）。
func (s *PromoIntelService) ProcessSource(ctx context.Context, src *PromoIntelSource) (*PromoIntelFetchResult, error) {
	if src == nil {
		return nil, errors.New("nil source")
	}
	timeout := PromoIntelDefaultFetchTimeoutSec + PromoIntelDefaultLLMTimeoutSeconds
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+30*time.Second)
	defer cancel()

	fetched, err := s.fetchPromoIntelPage(ctx, src.URL)
	if err != nil {
		_ = s.markSourceFetched(ctx, src, PromoIntelFetchStatusError, err.Error(), nil)
		return nil, fmt.Errorf("fetch source %q: %w", src.Name, err)
	}
	text, err := extractPromoIntelText(fetched.Body)
	if err != nil {
		_ = s.markSourceFetched(ctx, src, PromoIntelFetchStatusError, "extract text: "+err.Error(), nil)
		return nil, fmt.Errorf("extract text for %q: %w", src.Name, err)
	}
	contentHash := promoIntelContentHash(text)

	res := &PromoIntelFetchResult{Source: src}
	if contentHash == src.LastExtractedHash {
		_ = s.markSourceFetched(ctx, src, PromoIntelFetchStatusOK, "", &src.LastExtractedHash)
		res.SkippedReason = "unchanged"
		return res, nil
	}
	res.ContentChanged = true

	rt := s.GetPromoIntelRuntime(ctx)
	useLLM := src.LLMExtract && rt.HasLLM()
	if !useLLM {
		if err := s.upsertRawPendingItem(ctx, src, text, contentHash, fetched.FinalURL); err != nil {
			_ = s.markSourceFetched(ctx, src, PromoIntelFetchStatusError, "upsert raw item: "+err.Error(), nil)
			return nil, err
		}
		// 不更新 last_extracted_hash：配置整理模型后凭「指纹 ≠ 已整理」自动补跑。
		_ = s.markSourceFetched(ctx, src, PromoIntelFetchStatusOK, "", nil)
		if !src.LLMExtract {
			res.SkippedReason = "llm_extract_disabled"
		} else {
			res.SkippedReason = "llm_not_configured"
		}
		return res, nil
	}

	offers, err := s.extractOffersWithLLM(ctx, src, text)
	if err != nil {
		if errors.Is(err, ErrPromoIntelLLMNotConfigured) {
			// 竞态：运行时开关刚关闭。降级与上面一致。
			if upErr := s.upsertRawPendingItem(ctx, src, text, contentHash, fetched.FinalURL); upErr != nil {
				return nil, upErr
			}
			_ = s.markSourceFetched(ctx, src, PromoIntelFetchStatusOK, "", nil)
			res.SkippedReason = "llm_not_configured"
			return res, nil
		}
		// 抓取成功但整理失败：降级原文待整理，不推进已整理指纹（恢复后自动补跑）。
		if upErr := s.upsertRawPendingItem(ctx, src, text, contentHash, fetched.FinalURL); upErr != nil {
			slog.Warn("promo_intel: degraded raw item upsert failed", "source", src.Name, "err", upErr)
		}
		_ = s.markSourceFetched(ctx, src, PromoIntelFetchStatusError, "llm: "+err.Error(), nil)
		return nil, fmt.Errorf("extract offers for %q: %w", src.Name, err)
	}

	now := s.nowFn()
	today := now.UTC()
	digestDate := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	created, updated := 0, 0
	for _, o := range offers {
		if o.Vendor == "" {
			// LLM 未给出厂商时回填源厂商，保证指纹与展示稳定。
			o.Vendor = src.Vendor
		}
		item := &PromoIntelItem{
			SourceID:        src.ID,
			SourceName:      src.Name,
			Vendor:          o.Vendor,
			Category:        o.Category,
			Title:           o.Title,
			Summary:         o.Summary,
			Details:         o.Details,
			DiscountInfo:    o.Discount,
			ValidUntil:      o.ValidUntil,
			URL:             firstNonEmptyPromoIntel(o.URL, fetched.FinalURL),
			Relevance:       o.Relevance,
			Status:          PromoIntelItemStatusPending,
			Fingerprint:     promoIntelOfferFingerprint(o.Vendor, o.Title),
			RawExcerpt:      truncatePromoIntelString(text, promoIntelRawExcerptMaxLen),
			ExtractStatus:   PromoIntelExtractLLM,
			DigestDate:      &digestDate,
			SourceFetchedAt: now,
		}
		itemCreated, err := s.repo.UpsertItem(ctx, item)
		if err != nil {
			slog.Warn("promo_intel: upsert item failed", "source", src.Name, "title", o.Title, "err", err)
			continue
		}
		if itemCreated {
			created++
		} else {
			updated++
		}
	}
	// LLM 成功整理了当前内容版本：清除该源遗留的降级原文条目。
	if _, err := s.repo.DeletePendingItemsBySource(ctx, src.ID); err != nil {
		slog.Warn("promo_intel: clear pending raw items failed", "source", src.Name, "err", err)
	}
	_ = s.markSourceFetched(ctx, src, PromoIntelFetchStatusOK, "", &contentHash)
	res.ItemsCreated = created
	res.ItemsUpdated = updated
	return res, nil
}

// upsertRawPendingItem 降级路径：LLM 未配置/失败时把原文存成「待整理」条目。
func (s *PromoIntelService) upsertRawPendingItem(ctx context.Context, src *PromoIntelSource, text, contentHash, finalURL string) error {
	now := s.nowFn()
	item := &PromoIntelItem{
		SourceID:      src.ID,
		SourceName:    src.Name,
		Vendor:        src.Vendor,
		Category:      PromoIntelCategoryOther,
		Title:         src.Name + "：内容更新（待整理）",
		Summary:       "该源内容发生变化，但整理模型不可用，暂存原文；配置或恢复整理模型后将自动结构化。",
		URL:           firstNonEmptyPromoIntel(finalURL, src.URL),
		Relevance:     PromoIntelRelevanceMedium,
		Status:        PromoIntelItemStatusPending,
		Fingerprint:   promoIntelRawFingerprint(src.Name, contentHash),
		RawExcerpt:    truncatePromoIntelString(text, promoIntelRawExcerptMaxLen),
		ExtractStatus: PromoIntelExtractPending,
	}
	day := now.UTC()
	dd := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	item.DigestDate = &dd
	item.SourceFetchedAt = now
	_, err := s.repo.UpsertItem(ctx, item)
	return err
}

func (s *PromoIntelService) markSourceFetched(ctx context.Context, src *PromoIntelSource, status, lastErr string, extractedHash *string) error {
	now := s.nowFn()
	return s.repo.MarkSourceFetched(ctx, src.ID, status, lastErr, extractedHash, now)
}

func firstNonEmptyPromoIntel(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 资讯源 CRUD
// ---------------------------------------------------------------------------

// ListSources 分页列出资讯源。
func (s *PromoIntelService) ListSources(ctx context.Context, params PromoIntelSourceListParams) ([]*PromoIntelSource, int64, error) {
	s.clampPromoIntelPage(&params.Page, &params.PageSize)
	out, total, err := s.repo.ListSources(ctx, params)
	if err != nil {
		return nil, 0, fmt.Errorf("list promo intel sources: %w", err)
	}
	return out, total, nil
}

// GetSource 单个资讯源。
func (s *PromoIntelService) GetSource(ctx context.Context, id int64) (*PromoIntelSource, error) {
	src, err := s.repo.GetSourceByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get promo intel source: %w", err)
	}
	return src, nil
}

// CreateSource 创建资讯源（参数校验含 URL 形态与枚举白名单）。
func (s *PromoIntelService) CreateSource(ctx context.Context, params PromoIntelSourceCreateParams) (*PromoIntelSource, error) {
	name := strings.TrimSpace(params.Name)
	if name == "" || len(name) > promoIntelSourceNameMaxLen {
		return nil, infraerrorsBadRequest("promo intel source name is required (<=100 chars)")
	}
	if !ValidatePromoIntelVendor(strings.TrimSpace(params.Vendor)) {
		return nil, infraerrorsBadRequest("promo intel vendor must be lowercase letters/digits/-/_ (<=50 chars)")
	}
	category := normalizePromoIntelEnum(strings.TrimSpace(params.Category), PromoIntelSourceCategories, PromoIntelSourceCategoryAnnouncement)
	srcURL := strings.TrimSpace(params.URL)
	if err := validatePromoIntelURL(srcURL); err != nil {
		return nil, err
	}
	interval := params.FetchIntervalMinutes
	if interval <= 0 {
		interval = promoIntelDefaultInterval
	}
	if interval < promoIntelMinIntervalMin || interval > promoIntelMaxIntervalMin {
		return nil, infraerrorsBadRequest("fetch interval must be within 30 minutes .. 30 days")
	}
	enabled := true
	if params.Enabled != nil {
		enabled = *params.Enabled
	}
	llmExtract := true
	if params.LLMExtract != nil {
		llmExtract = *params.LLMExtract
	}
	src := &PromoIntelSource{
		Name:                 name,
		Vendor:               strings.TrimSpace(params.Vendor),
		Category:             category,
		URL:                  srcURL,
		FetchIntervalMinutes: interval,
		Enabled:              enabled,
		LLMExtract:           llmExtract,
		Notes:                strings.TrimSpace(params.Notes),
		CreatedBy:            params.CreatedBy,
	}
	if err := s.repo.CreateSource(ctx, src); err != nil {
		return nil, fmt.Errorf("create promo intel source: %w", err)
	}
	return src, nil
}

// UpdateSource 更新资讯源（nil 字段不动）。
func (s *PromoIntelService) UpdateSource(ctx context.Context, id int64, params PromoIntelSourceUpdateParams) (*PromoIntelSource, error) {
	existing, err := s.repo.GetSourceByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get promo intel source: %w", err)
	}
	if params.Name != nil {
		name := strings.TrimSpace(*params.Name)
		if name == "" || len(name) > promoIntelSourceNameMaxLen {
			return nil, infraerrorsBadRequest("promo intel source name is required (<=100 chars)")
		}
		existing.Name = name
	}
	if params.Vendor != nil {
		vendor := strings.TrimSpace(*params.Vendor)
		if !ValidatePromoIntelVendor(vendor) {
			return nil, infraerrorsBadRequest("promo intel vendor must be lowercase letters/digits/-/_ (<=50 chars)")
		}
		existing.Vendor = vendor
	}
	if params.Category != nil {
		existing.Category = normalizePromoIntelEnum(strings.TrimSpace(*params.Category), PromoIntelSourceCategories, existing.Category)
	}
	if params.URL != nil {
		srcURL := strings.TrimSpace(*params.URL)
		if err := validatePromoIntelURL(srcURL); err != nil {
			return nil, err
		}
		existing.URL = srcURL
		// URL 变化后旧指纹作废，下次抓取强制进入整理层。
		existing.LastExtractedHash = ""
	}
	if params.FetchIntervalMinutes != nil {
		interval := *params.FetchIntervalMinutes
		if interval < promoIntelMinIntervalMin || interval > promoIntelMaxIntervalMin {
			return nil, infraerrorsBadRequest("fetch interval must be within 30 minutes .. 30 days")
		}
		existing.FetchIntervalMinutes = interval
	}
	if params.Enabled != nil {
		existing.Enabled = *params.Enabled
	}
	if params.LLMExtract != nil {
		existing.LLMExtract = *params.LLMExtract
	}
	if params.Notes != nil {
		existing.Notes = strings.TrimSpace(*params.Notes)
	}
	if err := s.repo.UpdateSource(ctx, existing); err != nil {
		return nil, fmt.Errorf("update promo intel source: %w", err)
	}
	return existing, nil
}

// DeleteSource 删除资讯源（情报条目随 FK 级联删除）。
func (s *PromoIntelService) DeleteSource(ctx context.Context, id int64) error {
	if _, err := s.repo.GetSourceByID(ctx, id); err != nil {
		return fmt.Errorf("get promo intel source: %w", err)
	}
	return s.repo.DeleteSource(ctx, id)
}

// FetchSourceNow 立即抓取并整理单个源（管理台按钮）。
func (s *PromoIntelService) FetchSourceNow(ctx context.Context, id int64) (*PromoIntelFetchResult, error) {
	src, err := s.repo.GetSourceByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get promo intel source: %w", err)
	}
	return s.ProcessSource(ctx, src)
}

func (s *PromoIntelService) clampPromoIntelPage(page, pageSize *int) {
	if *page < 1 {
		*page = 1
	}
	if *pageSize < 1 || *pageSize > 200 {
		*pageSize = 20
	}
}

// validatePromoIntelURL 校验源 URL 为绝对 http(s) 地址。
func validatePromoIntelURL(raw string) error {
	if raw == "" {
		return ErrPromoIntelInvalidURL
	}
	if len(raw) > promoIntelSourceURLMaxLen {
		return ErrPromoIntelInvalidURL
	}
	u := strings.ToLower(raw)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return ErrPromoIntelInvalidURL
	}
	return nil
}

// ---------------------------------------------------------------------------
// 情报条目与简报
// ---------------------------------------------------------------------------

// ListItems 分页列出情报（含筛选）。
func (s *PromoIntelService) ListItems(ctx context.Context, params PromoIntelItemListParams) ([]*PromoIntelItem, int64, error) {
	s.clampPromoIntelPage(&params.Page, &params.PageSize)
	params.Vendor = strings.TrimSpace(params.Vendor)
	params.Category = strings.TrimSpace(params.Category)
	params.Relevance = normalizePromoIntelEnum(strings.TrimSpace(params.Relevance),
		[]string{PromoIntelRelevanceHigh, PromoIntelRelevanceMedium, PromoIntelRelevanceLow}, "")
	params.Status = normalizePromoIntelEnum(strings.TrimSpace(params.Status),
		[]string{PromoIntelItemStatusPending, PromoIntelItemStatusUseful, PromoIntelItemStatusIgnored}, "")
	params.Search = strings.TrimSpace(params.Search)
	out, total, err := s.repo.ListItems(ctx, params)
	if err != nil {
		return nil, 0, fmt.Errorf("list promo intel items: %w", err)
	}
	return out, total, nil
}

// UpdateItemStatus 情报分诊：待阅/有用/忽略。
func (s *PromoIntelService) UpdateItemStatus(ctx context.Context, id int64, status string) (*PromoIntelItem, error) {
	status = normalizePromoIntelEnum(strings.TrimSpace(status),
		[]string{PromoIntelItemStatusPending, PromoIntelItemStatusUseful, PromoIntelItemStatusIgnored}, "")
	if status == "" {
		return nil, infraerrorsBadRequest("invalid promo intel item status")
	}
	if _, err := s.repo.GetItemByID(ctx, id); err != nil {
		return nil, fmt.Errorf("get promo intel item: %w", err)
	}
	if err := s.repo.UpdateItemStatus(ctx, id, status); err != nil {
		return nil, fmt.Errorf("update promo intel item status: %w", err)
	}
	return s.repo.GetItemByID(ctx, id)
}

// GetBriefing 每日简报：按发现日期聚合当日全部情报（默认今天，UTC）。
func (s *PromoIntelService) GetBriefing(ctx context.Context, date string) (*PromoIntelBriefing, error) {
	day, err := parsePromoIntelDate(date)
	if err != nil {
		return nil, err
	}
	if day.IsZero() {
		t := s.nowFn().UTC()
		day = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	items, total, err := s.repo.ListItems(ctx, PromoIntelItemListParams{
		Page: 1, PageSize: 200, DigestDate: day.Format("2006-01-02"),
	})
	if err != nil {
		return nil, fmt.Errorf("list promo intel items for briefing: %w", err)
	}
	briefing := &PromoIntelBriefing{
		Date:       day.Format("2006-01-02"),
		Total:      total,
		ByVendor:   map[string]int64{},
		ByCategory: map[string]int64{},
		Items:      items,
	}
	for _, it := range items {
		briefing.ByVendor[it.Vendor]++
		briefing.ByCategory[it.Category]++
		if it.Relevance == PromoIntelRelevanceHigh {
			briefing.HighCount++
		}
		if it.Status == PromoIntelItemStatusPending {
			briefing.Pending++
		}
	}
	return briefing, nil
}

// parsePromoIntelDate 解析 YYYY-MM-DD；空串返回零值（表示默认今天）。
func parsePromoIntelDate(date string) (time.Time, error) {
	date = strings.TrimSpace(date)
	if date == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation("2006-01-02", date, time.UTC)
	if err != nil {
		return time.Time{}, infraerrorsBadRequest("digest date must be YYYY-MM-DD")
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// 整理模型设置（settings 表）
// ---------------------------------------------------------------------------

// promoIntelLLMSettingsInternal 是内部使用的完整 LLM 配置（含明文 key）。
type promoIntelLLMSettingsInternal struct {
	// self 模式
	SelfAPIKeyID int64
	SelfAPIKey   string // 明文（仅服务端）
	SelfModel    string
	// external 模式
	BaseURL string
	APIKey  string
	Model   string
	// 协议：openai / anthropic
	Protocol string
	// 组合判定
	Configured bool
}

// GetPromoIntelRuntime 读取运行时开关与 LLM 配置（公开视图，密钥脱敏）。
// fail-open：读不到 settings 时默认启用（与渠道监控一致），LLM 视为未配置。
func (s *PromoIntelService) GetPromoIntelRuntime(ctx context.Context) PromoIntelRuntime {
	rt := PromoIntelRuntime{Enabled: true, Source: PromoIntelLLMSourceSelf, Protocol: PromoIntelLLMProtocolOpenAI}
	if s == nil || s.settings == nil {
		return rt
	}
	vals, err := s.settings.GetMultiple(ctx, []string{
		SettingKeyPromoIntelEnabled,
		SettingKeyPromoIntelLLMSource,
		SettingKeyPromoIntelLLMProtocol,
		SettingKeyPromoIntelSelfAPIKeyID,
		SettingKeyPromoIntelSelfModel,
		SettingKeyPromoIntelLLMBaseURL,
		SettingKeyPromoIntelLLMAPIKey,
		SettingKeyPromoIntelLLMModel,
	})
	if err != nil {
		return rt
	}
	if isFalsePromoIntelSetting(vals[SettingKeyPromoIntelEnabled]) {
		rt.Enabled = false
	}
	rt.Source = normalizePromoIntelEnum(strings.TrimSpace(vals[SettingKeyPromoIntelLLMSource]),
		PromoIntelLLMSources, PromoIntelLLMSourceSelf)
	rt.Protocol = normalizePromoIntelEnum(strings.TrimSpace(vals[SettingKeyPromoIntelLLMProtocol]),
		PromoIntelLLMProtocols, PromoIntelLLMProtocolOpenAI)
	rt.SelfAPIKeyID = promoIntelParseInt64(vals[SettingKeyPromoIntelSelfAPIKeyID])
	rt.SelfModel = strings.TrimSpace(vals[SettingKeyPromoIntelSelfModel])
	if rt.SelfAPIKeyID > 0 {
		// 展示层补 key 名称（不取明文）。
		if name := s.adminAPIKeyName(ctx, rt.SelfAPIKeyID); name != "" {
			rt.SelfAPIKeyName = name
		}
	}
	rt.LLMBaseURL = strings.TrimSpace(vals[SettingKeyPromoIntelLLMBaseURL])
	rt.LLMModel = strings.TrimSpace(vals[SettingKeyPromoIntelLLMModel])
	key := strings.TrimSpace(vals[SettingKeyPromoIntelLLMAPIKey])
	rt.LLMAPIKeySet = key != ""
	rt.LLMAPIKeyMasked = maskPromoIntelSecret(key)
	rt.LLMConfigured = rt.LLMBaseURL != "" && rt.LLMModel != ""
	return rt
}

// promoIntelLLMSettings 内部视图（明文 key，仅服务端调用 LLM 时使用）。
func (s *PromoIntelService) promoIntelLLMSettings(ctx context.Context) promoIntelLLMSettingsInternal {
	out := promoIntelLLMSettingsInternal{}
	if s == nil || s.settings == nil {
		return out
	}
	vals, err := s.settings.GetMultiple(ctx, []string{
		SettingKeyPromoIntelLLMSource,
		SettingKeyPromoIntelLLMProtocol,
		SettingKeyPromoIntelSelfAPIKeyID,
		SettingKeyPromoIntelSelfModel,
		SettingKeyPromoIntelLLMBaseURL,
		SettingKeyPromoIntelLLMAPIKey,
		SettingKeyPromoIntelLLMModel,
	})
	if err != nil {
		return out
	}
	out.Protocol = normalizePromoIntelEnum(strings.TrimSpace(vals[SettingKeyPromoIntelLLMProtocol]),
		PromoIntelLLMProtocols, PromoIntelLLMProtocolOpenAI)
	source := normalizePromoIntelEnum(strings.TrimSpace(vals[SettingKeyPromoIntelLLMSource]),
		PromoIntelLLMSources, PromoIntelLLMSourceSelf)
	out.SelfModel = strings.TrimSpace(vals[SettingKeyPromoIntelSelfModel])
	if source == PromoIntelLLMSourceSelf {
		out.SelfAPIKeyID = promoIntelParseInt64(vals[SettingKeyPromoIntelSelfAPIKeyID])
		if out.SelfAPIKeyID > 0 && s.apiKeyLister != nil {
			if key, err := s.apiKeyLister.GetAPIKeyByID(ctx, out.SelfAPIKeyID); err == nil {
				out.SelfAPIKey = key
			}
		}
		// Model 统一填充 self 模型，供两种协议共用（anthropic 走 /v1/messages 同样用 model 字段）。
		out.Model = out.SelfModel
		out.Configured = out.SelfAPIKeyID > 0 && out.SelfAPIKey != "" && out.SelfModel != ""
		return out
	}
	out.BaseURL = strings.TrimSpace(vals[SettingKeyPromoIntelLLMBaseURL])
	out.APIKey = strings.TrimSpace(vals[SettingKeyPromoIntelLLMAPIKey])
	out.Model = strings.TrimSpace(vals[SettingKeyPromoIntelLLMModel])
	out.Configured = out.BaseURL != "" && out.Model != ""
	return out
}

// ListAPIKeyRefs 返回 self 模式可选的管理员 API Key（ID + 名称，无明文）。
func (s *PromoIntelService) ListAPIKeyRefs(ctx context.Context) ([]AdminAPIKeyRef, error) {
	if s == nil || s.apiKeyLister == nil {
		return []AdminAPIKeyRef{}, nil
	}
	keys, err := s.apiKeyLister.ListAdminAPIKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list admin api keys for promo intel: %w", err)
	}
	return keys, nil
}

// adminAPIKeyName 补 key 显示名（self 模式列表回读）。
func (s *PromoIntelService) adminAPIKeyName(ctx context.Context, id int64) string {
	if s.apiKeyLister == nil {
		return ""
	}
	keys, err := s.apiKeyLister.ListAdminAPIKeys(ctx)
	if err != nil {
		return ""
	}
	for _, k := range keys {
		if k.ID == id {
			return k.Name
		}
	}
	return ""
}

// promoIntelParseInt64 宽容解析设置值。
func promoIntelParseInt64(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// PromoIntelSettingsUpdate 管理台设置更新（nil = 不修改）。
type PromoIntelSettingsUpdate struct {
	Enabled      *bool
	Source       *string // self / external
	Protocol     *string // openai / anthropic
	SelfAPIKeyID *int64
	SelfModel    *string
	LLMBaseURL   *string
	LLMAPIKey    *string
	LLMModel     *string
}

// UpdateLLMSettings 写入整理模型设置；切换来源时校验新来源字段完备性，
// 外部密钥空/掩码表示保持原值。
func (s *PromoIntelService) UpdateLLMSettings(ctx context.Context, update PromoIntelSettingsUpdate) (PromoIntelRuntime, error) {
	if s == nil || s.settings == nil {
		return PromoIntelRuntime{}, errors.New("settings store unavailable")
	}
	if update.Enabled != nil {
		if err := s.settings.Set(ctx, SettingKeyPromoIntelEnabled, boolPromoIntelSetting(*update.Enabled)); err != nil {
			return PromoIntelRuntime{}, fmt.Errorf("save promo intel enabled: %w", err)
		}
	}
	if update.Source != nil {
		src := normalizePromoIntelEnum(strings.TrimSpace(*update.Source), PromoIntelLLMSources, "")
		if src == "" {
			return PromoIntelRuntime{}, infraerrorsBadRequest("llm source must be self or external")
		}
		if err := s.settings.Set(ctx, SettingKeyPromoIntelLLMSource, src); err != nil {
			return PromoIntelRuntime{}, fmt.Errorf("save promo intel llm source: %w", err)
		}
	}
	if update.Protocol != nil {
		proto := normalizePromoIntelEnum(strings.TrimSpace(*update.Protocol), PromoIntelLLMProtocols, "")
		if proto == "" {
			return PromoIntelRuntime{}, infraerrorsBadRequest("llm protocol must be openai or anthropic")
		}
		if err := s.settings.Set(ctx, SettingKeyPromoIntelLLMProtocol, proto); err != nil {
			return PromoIntelRuntime{}, fmt.Errorf("save promo intel llm protocol: %w", err)
		}
	}
	if update.SelfAPIKeyID != nil {
		if *update.SelfAPIKeyID < 0 {
			return PromoIntelRuntime{}, infraerrorsBadRequest("invalid self api key id")
		}
		if *update.SelfAPIKeyID > 0 && s.apiKeyLister != nil {
			// 校验该 key 存在且可取到明文。
			if key, err := s.apiKeyLister.GetAPIKeyByID(ctx, *update.SelfAPIKeyID); err != nil || key == "" {
				return PromoIntelRuntime{}, infraerrorsBadRequest("self api key not found or unreadable")
			}
		}
		if err := s.settings.Set(ctx, SettingKeyPromoIntelSelfAPIKeyID, strconv.FormatInt(*update.SelfAPIKeyID, 10)); err != nil {
			return PromoIntelRuntime{}, fmt.Errorf("save promo intel self api key: %w", err)
		}
	}
	if update.SelfModel != nil {
		model := strings.TrimSpace(*update.SelfModel)
		if model == "" {
			return PromoIntelRuntime{}, infraerrorsBadRequest("self model is required")
		}
		if err := s.settings.Set(ctx, SettingKeyPromoIntelSelfModel, model); err != nil {
			return PromoIntelRuntime{}, fmt.Errorf("save promo intel self model: %w", err)
		}
	}
	if update.LLMBaseURL != nil {
		base := strings.TrimSpace(*update.LLMBaseURL)
		if base != "" && !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			return PromoIntelRuntime{}, infraerrorsBadRequest("llm base url must start with http(s)://")
		}
		if err := s.settings.Set(ctx, SettingKeyPromoIntelLLMBaseURL, base); err != nil {
			return PromoIntelRuntime{}, fmt.Errorf("save promo intel llm base url: %w", err)
		}
	}
	if update.LLMAPIKey != nil {
		key := strings.TrimSpace(*update.LLMAPIKey)
		// 语义：空串或前端回显的掩码 = 保持不变；"-" = 显式清空；其余 = 新密钥。
		switch {
		case key == "":
		case strings.Contains(key, "*"):
		default:
			if key == "-" {
				key = ""
			}
			if err := s.settings.Set(ctx, SettingKeyPromoIntelLLMAPIKey, key); err != nil {
				return PromoIntelRuntime{}, fmt.Errorf("save promo intel llm api key: %w", err)
			}
		}
	}
	if update.LLMModel != nil {
		if err := s.settings.Set(ctx, SettingKeyPromoIntelLLMModel, strings.TrimSpace(*update.LLMModel)); err != nil {
			return PromoIntelRuntime{}, fmt.Errorf("save promo intel llm model: %w", err)
		}
	}
	return s.GetPromoIntelRuntime(ctx), nil
}

// TestLLMSettings 用当前保存的配置发一次最小补全，验证连通性。
func (s *PromoIntelService) TestLLMSettings(ctx context.Context) (string, error) {
	cfgInt := s.promoIntelLLMSettings(ctx)
	if !cfgInt.Configured {
		return "", ErrPromoIntelLLMNotConfigured
	}
	// 测试请求用短超时临时客户端，避免把整轮 LLM 超时耗在坏端点上；
	// 已注入的测试客户端原样保留。
	origClient := s.testLLMClient
	if origClient == nil {
		s.testLLMClient = &http.Client{Timeout: 30 * time.Second}
		defer func() { s.testLLMClient = origClient }()
	}
	testSrc := &PromoIntelSource{Name: "连通性测试", Vendor: "test", URL: "https://example.com"}
	// 正文带一条迷你优惠：parsed_offers 应为 1，顺带验证端到端解析链而不只是连通。
	offers, err := s.extractOffersWithLLM(ctx, testSrc, "示例厂商发布公告：新用户注册即送 100 万 tokens 体验金，限时活动。")
	if err != nil {
		// 必须返回带明细的业务错误（400 族），否则错误映射层会把普通 error
		// 兜底成笼统的 "internal error"，管理员在页面上看不到真实原因。
		return "", s.promoIntelTestError(ctx, err)
	}
	return fmt.Sprintf("ok: endpoint reachable, protocol=%s, model=%s, parsed_offers=%d", cfgInt.Protocol, cfgInt.Model, len(offers)), nil
}

// promoIntelTestError 把整理端点测试失败分类成带明细的 400 业务错误，
// 让前端 toast 直接显示真实原因（模型不认 / 端点不可达 / 上游报错），
// 而不是被错误映射兜底成 "internal error"。
func (s *PromoIntelService) promoIntelTestError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// 最常见误配：key 的分组不认填写的模型（网关 404）。附上可用模型清单。
	// 网关错误是 JSON 原文，引号带反斜杠转义（\"kimi-k3\"），先还原再匹配/展示。
	msg = strings.ReplaceAll(msg, `\"`, `"`)
	if strings.Contains(msg, "is not available for this group") {
		hint := s.selfKeyModelsHint(ctx, s.promoIntelSelfKeyID(ctx))
		if hint != "" {
			return infraerrors.BadRequest("PROMO_INTEL_LLM_MODEL_REJECTED",
				"该 Key 所在分组不允许模型 "+extractPromoIntelQuotedModel(msg)+"；该分组可用模型："+hint)
		}
		return infraerrors.BadRequest("PROMO_INTEL_LLM_MODEL_REJECTED", msg)
	}
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "Client.Timeout"):
		return infraerrors.BadRequest("PROMO_INTEL_LLM_UNREACHABLE", "无法连接整理端点："+msg)
	}
	return infraerrors.BadRequest("PROMO_INTEL_LLM_TEST_FAILED", msg)
}

// promoIntelSelfKeyID 读当前配置的 self key id（提示用）。
func (s *PromoIntelService) promoIntelSelfKeyID(ctx context.Context) int64 {
	return s.promoIntelLLMSettings(ctx).SelfAPIKeyID
}

// extractPromoIntelQuotedModel 从网关错误消息里抠出被拒的模型名（Model "x" is ...）。
func extractPromoIntelQuotedModel(msg string) string {
	i := strings.Index(msg, `Model "`)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(`Model "`):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// selfKeyModelsHint 返回指定 key 分组白名单模型的逗号串（无白名单返回空）。
func (s *PromoIntelService) selfKeyModelsHint(ctx context.Context, keyID int64) string {
	if s == nil || s.apiKeyLister == nil || keyID <= 0 {
		return ""
	}
	keys, err := s.apiKeyLister.ListAdminAPIKeys(ctx)
	if err != nil {
		return ""
	}
	for _, k := range keys {
		if k.ID == keyID && len(k.Models) > 0 {
			return strings.Join(k.Models, ", ")
		}
	}
	return ""
}

func isFalsePromoIntelSetting(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "off", "no":
		return true
	}
	return false
}

func boolPromoIntelSetting(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// maskPromoIntelSecret 掩码展示：保留前 3 后 4（过短只给尾部）。
func maskPromoIntelSecret(key string) string {
	if key == "" {
		return ""
	}
	runes := []rune(key)
	if len(runes) <= 8 {
		return strings.Repeat("*", len(runes))
	}
	return string(runes[:3]) + "****" + string(runes[len(runes)-4:])
}
