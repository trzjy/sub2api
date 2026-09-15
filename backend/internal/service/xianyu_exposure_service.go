package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
)

// ==================== 接口定义（测试可 mock） ====================

// XianyuExposureFactsStore 商品事实档案读写。
type XianyuExposureFactsStore interface {
	GetProductFacts(ctx context.Context, productID int64) (string, error)
	UpdateProductFacts(ctx context.Context, productID int64, facts string) error
}

// XianyuExposureOrderCounter 按商品聚合出单数。
type XianyuExposureOrderCounter interface {
	CountClaimsByItem(ctx context.Context) (map[string]int, error)
}

// XianyuExposureSettingsReader 设置读取。
type XianyuExposureSettingsReader interface {
	GetSettings(ctx context.Context) (XianyuSettings, error)
}

// XianyuExposureWorkerClient 即席搜索 + 商品详情。
type XianyuExposureWorkerClient interface {
	SearchKeyword(ctx context.Context, keyword string, rowsPerPage int) (*XianyuSearchOnceResult, error)
	GetItemDetail(ctx context.Context, accountID, itemID string) (*XianyuItemDetailInfo, error)
}

// XianyuExposureWorkerHealther Worker 健康探针。
type XianyuExposureWorkerHealther interface {
	GetItemDetail(ctx context.Context, accountID, itemID string) (*XianyuItemDetailInfo, error)
}

// XianyuExposureProductLister 商品列表（在售）。
type XianyuExposureProductLister interface {
	ListProducts(ctx context.Context) ([]XianyuProduct, error)
}

// ==================== 服务本体 ====================

// XianyuExposureService 闲鱼曝光助手：事实档案 + 市场采集 + AI 标题分析 + 企业微信推送。
type XianyuExposureService struct {
	facts      XianyuExposureFactsStore
	orders     XianyuExposureOrderCounter
	settings   XianyuExposureSettingsReader
	worker     XianyuExposureWorkerClient
	products   XianyuExposureProductLister
	redis      *redis.Client
	httpClient *http.Client
	cron       *cron.Cron
	pushedHash map[string]time.Time // 内存级防重（当天有效）
}

func NewXianyuExposureService(
	facts XianyuExposureFactsStore,
	orders XianyuExposureOrderCounter,
	settings XianyuExposureSettingsReader,
	worker XianyuExposureWorkerClient,
	products XianyuExposureProductLister,
	redisClient *redis.Client,
) *XianyuExposureService {
	return &XianyuExposureService{
		facts:      facts,
		orders:     orders,
		settings:   settings,
		worker:     worker,
		products:   products,
		redis:      redisClient,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		pushedHash: make(map[string]time.Time),
	}
}

// Start 启动 cron 调度（管理端保存设置后调用 Reload 即可切换）。
func (s *XianyuExposureService) Start() {
	s.cron = cron.New()
	s.cron.Start()
}

// Stop 停止 cron。
func (s *XianyuExposureService) Stop() {
	if s.cron != nil {
		s.cron.Stop()
	}
}

// Reload 用最新 cron 表达式重建调度。
func (s *XianyuExposureService) Reload(ctx context.Context) {
	s.stopCron()
	cfg, err := s.settings.GetSettings(ctx)
	if err != nil {
		return
	}
	if !cfg.ExposureEnabled || cfg.ExposureCron == "" {
		return
	}
	s.cron = cron.New()
	s.cron.AddFunc(cfg.ExposureCron, func() {
		s.runOnce(context.Background())
	})
	s.cron.Start()
}

func (s *XianyuExposureService) stopCron() {
	if s.cron != nil {
		s.cron.Stop()
		s.cron = nil
	}
}

// ==================== 主流程 ====================

func (s *XianyuExposureService) runOnce(ctx context.Context) {
	cfg, err := s.settings.GetSettings(ctx)
	if err != nil || !cfg.ExposureEnabled {
		return
	}

	wecomURL := strings.TrimSpace(cfg.ExposureWecomWebhook)
	aiBaseURL := strings.TrimSpace(cfg.ExposureAIBaseURL)
	aiKey := strings.TrimSpace(cfg.ExposureAIAPIKey)
	aiModel := strings.TrimSpace(cfg.ExposureAIModel)

	if wecomURL == "" || aiBaseURL == "" || aiKey == "" || aiModel == "" {
		log.Printf("[exposure] skip: incomplete config (webhook=%v, ai_base=%v)", wecomURL != "", aiBaseURL != "")
		return
	}

	// 1. 筛选待优化商品
	products, err := s.products.ListProducts(ctx)
	if err != nil || len(products) == 0 {
		return
	}
	orderCounts, _ := s.orders.CountClaimsByItem(ctx)
	if orderCounts == nil {
		orderCounts = map[string]int{}
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	staleDays := cfg.ExposureStaleDays
	if staleDays <= 0 {
		staleDays = 7
	}
	maxOrders := cfg.ExposureMaxOrders

	type candidate struct {
		product    XianyuProduct
		daysOnSale int
		orderCount int
		factsJSON  string
		freshness  float64 // 天数 × (1 + log2(1+orderCount))，越低越优先
	}
	var candidates []candidate

	for _, p := range products {
		if p.Status != "active" {
			continue
		}
		days := int(now.Sub(p.CreatedAt).Hours() / 24)
		if days < staleDays {
			continue
		}
		cnt := orderCounts[p.ItemID]
		if cnt >= maxOrders {
			continue
		}
		facts, _ := s.facts.GetProductFacts(ctx, p.ID)
		// 简单优先级：上架时长，零出单更优先
		freshness := float64(days) * (1.0 + float64(cnt))
		candidates = append(candidates, candidate{
			product: p, daysOnSale: days, orderCount: cnt, factsJSON: facts, freshness: freshness,
		})
	}
	if len(candidates) == 0 {
		return
	}
	// 按优先级升序（低 freshness 先处理）
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].freshness < candidates[j-1].freshness; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	maxSuggestions := 8
	var suggestions []struct {
		title    string
		original string
		facts    string
		evidence string
		itemID   string
	}

	for _, c := range candidates {
		if len(suggestions) >= maxSuggestions {
			break
		}

		// 2. 确保有事实档案
		facts := c.factsJSON
		if facts == "" {
			facts, err = s.ensureFacts(ctx, c.product, cfg)
			if err != nil || facts == "" {
				continue
			}
			_ = s.facts.UpdateProductFacts(ctx, c.product.ID, facts)
		}

		// 3. 关键词提取
		keywords := extractKeywords(c.product.Title)
		if len(keywords) == 0 {
			continue
		}

		// 4. 市场采集（带缓存）
		marketData := s.fetchMarketData(ctx, keywords, cfg.MarketCacheMinutes())

		// 5. AI 分析
		suggestion, evidence, _ := s.generateSuggestion(ctx, c.product.Title, facts, marketData, cfg)
		if suggestion == "" || suggestion == c.product.Title {
			continue
		}

		suggestions = append(suggestions, struct {
			title    string
			original string
			facts    string
			evidence string
			itemID   string
		}{title: suggestion, original: c.product.Title, facts: facts, evidence: evidence, itemID: c.product.ItemID})
	}

	if len(suggestions) == 0 {
		return
	}

	// 6. 防重（按建议标题 hash，当天有效；同一条建议不重复推送）
	if s.redis != nil {
		for i := range suggestions {
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(suggestions[i].title)))
			dupKey := fmt.Sprintf("xianyu:exposure:hash:%s:%s", today, hash[:16])
			exists, _ := s.redis.Exists(ctx, dupKey).Result()
			if exists > 0 {
				suggestions[i].title = "" // 标记跳过
			} else {
				_ = s.redis.Set(ctx, dupKey, "1", 24*time.Hour).Err()
			}
		}
	}

	// 7. 过滤空的（防重后）
	var filtered []struct {
		title    string
		original string
		facts    string
		evidence string
		itemID   string
	}
	for _, sg := range suggestions {
		if sg.title != "" {
			filtered = append(filtered, sg)
		}
	}
	if len(filtered) == 0 {
		return
	}

	// 8. 组装 Markdown
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**📊 闲鱼曝光助手 · %s**\n\n", today))

	for _, sg := range filtered {
		sb.WriteString(fmt.Sprintf("---\n**原标题：** %s\n\n", truncate(sg.original, 40)))
		sb.WriteString(fmt.Sprintf("✅ **建议标题：** %s\n\n", sg.title))
		if sg.evidence != "" {
			sb.WriteString(fmt.Sprintf("📊 %s\n\n", sg.evidence))
		}
	}
	sb.WriteString("---\n> 复制建议标题到闲鱼 App 替换原标题")

	// 9. 发送（失败则不返回，记日志）
	err = s.sendWecom(ctx, wecomURL, sb.String())
	if err != nil {
		log.Printf("[exposure] wecom push failed: %v", err)
		return
	}
}

// ==================== 事实档案 ====================

func (s *XianyuExposureService) ensureFacts(ctx context.Context, product XianyuProduct, cfg XianyuSettings) (string, error) {
	if s.worker == nil {
		return "", fmt.Errorf("worker not configured")
	}
	detail, err := s.worker.GetItemDetail(ctx, product.AccountID, product.ItemID)
	if err != nil {
		return "", err
	}
	if detail == nil {
		return "", fmt.Errorf("empty detail")
	}
	desc := strings.TrimSpace(detail.Desc)
	if desc == "" {
		return "", fmt.Errorf("empty description")
	}
	prompt := buildFactsPrompt(product.Title, desc, cfg.ExposureBlockedWords)
	return s.callAI(ctx, prompt, cfg)
}

// ==================== 关键词提取 ====================

func extractKeywords(title string) []string {
	// 简单拆分：空格/中英文标点分割，过滤停用词
	stopWords := map[string]bool{"的": true, "了": true, "是": true, "在": true, "和": true, "与": true,
		"a": true, "an": true, "the": true, "and": true, "or": true}
	raw := strings.FieldsFunc(title, func(r rune) bool {
		return r == ' ' || r == '/' || r == '|' || r == '｜' || r == '，' || r == ',' || r == '。' || r == '.' || r == '-' || r == '—' || r == '+' || r == '（' || r == '）' || r == '(' || r == ')'
	})
	var out []string
	for _, w := range raw {
		w = strings.TrimSpace(w)
		if w == "" || len(w) < 2 || stopWords[strings.ToLower(w)] {
			continue
		}
		out = append(out, w)
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

// ==================== 市场采集 ====================

func (s *XianyuExposureService) fetchMarketData(ctx context.Context, keywords []string, cacheMinutes int) map[string][]XianyuSearchItem {
	out := map[string][]XianyuSearchItem{}
	for _, kw := range keywords {
		data := s.fetchSingleKeyword(ctx, kw, cacheMinutes)
		out[kw] = data
	}
	return out
}

func (s *XianyuExposureService) fetchSingleKeyword(ctx context.Context, keyword string, cacheMinutes int) []XianyuSearchItem {
	// Redis 缓存
	cacheKey := fmt.Sprintf("xianyu:exposure:market:%s", keyword)
	if s.redis != nil && cacheMinutes > 0 {
		cached, err := s.redis.Get(ctx, cacheKey).Result()
		if err == nil && cached != "" {
			var items []XianyuSearchItem
			if json.Unmarshal([]byte(cached), &items) == nil {
				return items
			}
		}
	}

	// 实时采集
	if s.worker == nil {
		return nil
	}
	result, err := s.worker.SearchKeyword(ctx, keyword, 20)
	if err != nil || result == nil {
		return nil
	}

	// 写缓存
	if s.redis != nil && cacheMinutes > 0 && len(result.Items) > 0 {
		data, _ := json.Marshal(result.Items)
		_ = s.redis.Set(ctx, cacheKey, string(data), time.Duration(cacheMinutes)*time.Minute).Err()
	}

	return result.Items
}

// ==================== AI 分析 ====================

func (s *XianyuExposureService) generateSuggestion(ctx context.Context, currentTitle, facts string, marketData map[string][]XianyuSearchItem, cfg XianyuSettings) (string, string, error) {
	prompt := buildAnalysisPrompt(currentTitle, facts, marketData, cfg.ExposureBlockedWords)
	resp, err := s.callAI(ctx, prompt, cfg)
	if err != nil || resp == "" {
		return "", "", err
	}

	// AI 返回格式：建议标题\n---\n证据
	parts := strings.SplitN(resp, "---", 2)
	title := strings.TrimSpace(parts[0])
	evidence := ""
	if len(parts) > 1 {
		evidence = strings.TrimSpace(parts[1])
	}

	if title == "" || title == currentTitle {
		return "", "", nil
	}

	// 最终校验：标题不含黑名单词
	if blocked := cfg.ExposureBlockedWords; blocked != "" {
		for _, bw := range strings.Split(blocked, ",") {
			bw = strings.TrimSpace(bw)
			if bw != "" && strings.Contains(strings.ToLower(title), strings.ToLower(bw)) {
				return "", "", nil
			}
		}
	}

	return title, evidence, nil
}

// ==================== AI HTTP 调用 ====================

func (s *XianyuExposureService) callAI(ctx context.Context, prompt string, cfg XianyuSettings) (string, error) {
	body := map[string]any{
		"model": cfg.ExposureAIModel,
		"messages": []map[string]string{
			{"role": "system", "content": "你是闲鱼商品标题优化专家。严格基于给定事实生成标题，禁止编造卖点。"},
			{"role": "user", "content": prompt},
		},
		"temperature": 0.7,
		"max_tokens":  300,
	}
	data, _ := json.Marshal(body)

	url := strings.TrimRight(cfg.ExposureAIBaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.ExposureAIAPIKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	respData, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("AI API %d: %s", resp.StatusCode, truncate(string(respData), 200))
	}

	var env struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respData, &env); err != nil {
		return "", err
	}
	if len(env.Choices) == 0 {
		return "", fmt.Errorf("empty AI response")
	}
	return strings.TrimSpace(env.Choices[0].Message.Content), nil
}

// ==================== 企业微信推送 ====================

func (s *XianyuExposureService) sendWecom(ctx context.Context, webhookURL, markdown string) error {
	body := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"content": markdown,
		},
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respData, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("wecom %d: %s", resp.StatusCode, string(respData))
	}
	return nil
}

// ==================== Prompt 模板 ====================

func buildFactsPrompt(title, desc, blockedWords string) string {
	return fmt.Sprintf(`从以下商品标题和描述中，严格提取事实性信息，只提取描述中明确写了的内容。

【商品标题】
%s

【商品描述正文】
%s

【提取规则】
- 只提取描述中**明确写了**的事实
- 没有提及的维度不要编造，设为 null
- 绝对禁止添加描述中没有的卖点
- 输出纯 JSON，不要 markdown 代码块

【输出格式】
{"model": "模型名或null", "duration": "有效期描述或null", "quota": "额度规则描述或null", "features": ["功能1","功能2"], "scenes": ["适用场景1","适用场景2"], "delivery": "发货方式或null", "warnings": ["注意事项1"], "not_included": ["未包含内容1"]}`, title, truncate(desc, 1500))
}

func buildAnalysisPrompt(currentTitle, facts string, marketData map[string][]XianyuSearchItem, blockedWords string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("【当前标题】\n%s\n\n", currentTitle))
	sb.WriteString(fmt.Sprintf("【商品事实档案】\n%s\n\n", facts))
	sb.WriteString("【竞品市场数据】\n")
	for kw, items := range marketData {
		sb.WriteString(fmt.Sprintf("\n关键词「%s」top %d 商品：\n", kw, len(items)))
		for i, item := range items {
			if i >= 5 {
				break
			}
			sb.WriteString(fmt.Sprintf("  %d. [想要%d] %s\n", i+1, item.WantCount, truncate(item.Title, 50)))
		}
		sb.WriteString(fmt.Sprintf("  （共 %d 个商品，总想要 %d）\n", len(items), sumWants(items)))
	}

	// 读取高频词
	wordCounts := analyzeHighFrequency(marketData)
	sb.WriteString(fmt.Sprintf("\n高频词统计：%s\n", formatWordCounts(wordCounts)))

	sb.WriteString(fmt.Sprintf(`
【生成规则】
1. 只能使用事实档案中的内容，禁止编造任何卖点
2. 优先使用事实档案中有、竞品也高频出现的关键词
3. 禁止使用以下违规词：%s
4. 竞品中的"老号/换绑/拼车/共享/官方"类词一律不用
5. 标题控制在 40 字以内，信息密度优先
6. 必须包含核心产品名（如 Kimi K3、DeepSeek 等）

【输出格式】（严格按此格式，不要其他内容）
第一行：建议标题（只这一行）
---
第二行起：市场证据（用简短文字说明选择了哪些关键词、原因）`, blockedWords))

	return sb.String()
}

// ==================== 工具函数 ====================

func sumWants(items []XianyuSearchItem) int {
	total := 0
	for _, item := range items {
		total += item.WantCount
	}
	return total
}

func analyzeHighFrequency(marketData map[string][]XianyuSearchItem) map[string]int {
	words := []string{"kimi", "api", "k3", "k2", "会员", "deepseek", "月之暗面", "大模型", "额度", "官方", "编程", "豆包",
		"稳定", "体验", "秒发", "共享", "24小时", "写代码", "不限量", "独享", "claude", "cursor", "日卡", "周卡", "自动发货", "key", "拼车"}
	counter := map[string]int{}
	for _, items := range marketData {
		for _, item := range items {
			titleLower := strings.ToLower(item.Title)
			for _, w := range words {
				if strings.Contains(titleLower, w) {
					counter[w] += 1
				}
			}
		}
	}
	return counter
}

func formatWordCounts(counter map[string]int) string {
	type kv struct {
		Key string
		Val int
	}
	var sorted []kv
	for k, v := range counter {
		if v > 0 {
			sorted = append(sorted, kv{k, v})
		}
	}
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Val > sorted[j-1].Val; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	var parts []string
	for _, kv := range sorted {
		parts = append(parts, fmt.Sprintf("%s(%d)", kv.Key, kv.Val))
	}
	return strings.Join(parts, "、")
}

// ==================== 管理端接口用 ====================

// GenerateTitleSuggestion 管理端"AI 标题"按钮：实时采集，单个商品。
func (s *XianyuExposureService) GenerateTitleSuggestion(ctx context.Context, productID int64) (title, evidence string, err error) {
	cfg, err := s.settings.GetSettings(ctx)
	if err != nil {
		return "", "", err
	}

	// 取商品
	products, err := s.products.ListProducts(ctx)
	if err != nil {
		return "", "", err
	}
	var target *XianyuProduct
	for _, p := range products {
		if p.ID == productID {
			target = &p
			break
		}
	}
	if target == nil {
		return "", "", fmt.Errorf("product not found")
	}

	// 确保事实档案（实时重建）
	facts, _ := s.facts.GetProductFacts(ctx, productID)
	if facts == "" {
		facts, err = s.ensureFacts(ctx, *target, cfg)
		if err == nil && facts != "" {
			_ = s.facts.UpdateProductFacts(ctx, productID, facts)
		}
	}

	// 关键词 + 实时市场采集（无缓存）
	keywords := extractKeywords(target.Title)
	marketData := s.fetchMarketData(ctx, keywords, 0) // cacheMinutes=0 强制实时

	// AI 生成
	return s.generateSuggestion(ctx, target.Title, facts, marketData, cfg)
}

// TestPush 测试推送：用当前设置立即跑一次。
func (s *XianyuExposureService) TestPush(ctx context.Context) error {
	cfg, err := s.settings.GetSettings(ctx)
	if err != nil {
		return err
	}
	wecomURL := strings.TrimSpace(cfg.ExposureWecomWebhook)
	if wecomURL == "" {
		return fmt.Errorf("webhook not configured")
	}
	testMsg := fmt.Sprintf("**📊 闲鱼曝光助手 · 测试推送**\n\n当前配置：\n- AI 端点：%s\n- 模型：%s\n- 推送时间：%s\n- stale_days：%d\n\n✅ 推送成功，配置正常。", cfg.ExposureAIBaseURL, cfg.ExposureAIModel, cfg.ExposureCron, cfg.ExposureStaleDays)
	return s.sendWecom(ctx, wecomURL, testMsg)
}

// MarketCacheMinutes 返回市场数据缓存分钟数。
func (x XianyuSettings) MarketCacheMinutes() int {
	if x.ExposureMarketCacheMinutes <= 0 {
		return 240
	}
	return x.ExposureMarketCacheMinutes
}
