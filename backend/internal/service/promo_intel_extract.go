package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLM 整理层：把资讯源正文交给**管理员可配置的独立 OpenAI 兼容端点**做结构化提取。
//
// 设计要点：
//   - 端点与系统已支持平台完全解耦：任意厂商任意新模型均可（填 base_url + api_key + model）；
//   - 未配置或调用/解析失败时降级为「原文待整理」条目，绝不丢内容；恢复后凭内容指纹自动补跑；
//   - prompt 面向 API 聚合转售业务定制，无关营销内容判 none 直接丢弃。

// promoIntelOffer 是 LLM 单条提取结果（宽容 JSON，字段均可缺失）。
type promoIntelOffer struct {
	Title      string `json:"title"`
	Vendor     string `json:"vendor"`
	Category   string `json:"category"`
	Summary    string `json:"summary"`
	Details    string `json:"details"`
	Discount   string `json:"discount"`
	ValidUntil string `json:"valid_until"`
	URL        string `json:"url"`
	Relevance  string `json:"relevance"`
}

const promoIntelSystemPrompt = `你是面向 API 聚合转售业务的优惠情报分析员。输入是一个厂商官方页面的正文。请提取其中全部与「优惠/商机」相关的信息：免费额度、赠送金、直充/首充折扣、包月或订阅优惠（尤其首月优惠）、价格调整（降价/涨价）、限时活动、新模型上线、新产品或新功能上线、政策变化（限速/风控/结算）。

输出严格的 JSON 数组，每个元素形如：
{"title":"简短标题(不超过40字)","vendor":"厂商英文名(小写)","category":"free_quota|discount|subscription|price_change|new_model|new_product|event|policy|other","summary":"对我们业务的价值要点(不超过120字)","details":"参与方式/条件/入口","discount":"优惠力度，如 首月49元/5折/每天免费100万tokens，无则空串","valid_until":"有效期，未知则空串","url":"信息所在的原文链接，未知则空串","relevance":"high|medium|low|none，判定对API聚合转售业务的价值"}

规则：
1. 页面没有可提取的优惠信息时输出 []。
2. 纯营销/品牌宣传、与优惠或产品变化无关的内容判 relevance=none。
3. 只输出 JSON 数组，不要输出任何其他文字、注释或代码围栏。`

// extractOffersWithLLM 调用可配置端点提取优惠情报；返回归一化后的条目列表。
func (s *PromoIntelService) extractOffersWithLLM(ctx context.Context, source *PromoIntelSource, text string) ([]*promoIntelOffer, error) {
	llmCfg := s.promoIntelLLMSettings(ctx)
	if !llmCfg.Configured {
		return nil, ErrPromoIntelLLMNotConfigured
	}

	capped := capPromoIntelRunes(text, s.llmMaxTextChars())
	userMsg := fmt.Sprintf("厂商: %s\n页面: %s\n正文:\n<<<\n%s\n>>>", source.Vendor, source.URL, capped)

	payload := map[string]any{
		"model": llmCfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": promoIntelSystemPrompt},
			{"role": "user", "content": userMsg},
		},
		"temperature": 0.2,
		"stream":      false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal llm request: %w", err)
	}

	endpoint := normalizePromoIntelLLMEndpoint(llmCfg.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build llm request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if llmCfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+llmCfg.APIKey)
	}

	client := s.promoIntelLLMClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, promoIntelLLMResponseMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("read llm response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("llm endpoint returned HTTP %d: %s", resp.StatusCode, truncatePromoIntelString(string(respBody), 300))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse llm response envelope: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("llm response has no choices")
	}
	return parsePromoIntelOffersJSON(parsed.Choices[0].Message.Content)
}

// promoIntelLLMResponseMaxBytes 限制 LLM 响应体读取上限（8MB，防御异常端点）。
const promoIntelLLMResponseMaxBytes = 8 << 20

// ErrPromoIntelLLMNotConfigured 表示整理模型未配置（降级为原文待整理）。
var ErrPromoIntelLLMNotConfigured = fmt.Errorf("promo intel llm endpoint not configured")

// normalizePromoIntelLLMEndpoint 归一化端点：去尾部斜杠；缺 /chat/completions 时补全。
// base_url 允许带或不带 /v1（如 https://api.deepseek.com 或 https://api.openai.com/v1）。
func normalizePromoIntelLLMEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

// parsePromoIntelOffersJSON 宽容解析 LLM 输出：剥代码围栏 → 定位首个 '[' 到末个 ']'
// → JSON 反序列化 → 白名单归一化 → 丢弃空标题与 relevance=none。
func parsePromoIntelOffersJSON(content string) ([]*promoIntelOffer, error) {
	trimmed := strings.TrimSpace(content)
	// 常见代码围栏 ```json ... ```
	if idx := strings.Index(trimmed, "```"); idx >= 0 {
		trimmed = strings.TrimPrefix(trimmed, "```json")
		trimmed = strings.TrimPrefix(trimmed, "```")
		if end := strings.LastIndex(trimmed, "```"); end >= 0 {
			trimmed = trimmed[:end]
		}
		trimmed = strings.TrimSpace(trimmed)
	}
	start := strings.Index(trimmed, "[")
	end := strings.LastIndex(trimmed, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("llm output contains no JSON array")
	}
	var raw []*promoIntelOffer
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("parse llm offers json: %w", err)
	}

	out := make([]*promoIntelOffer, 0, len(raw))
	for _, o := range raw {
		if o == nil {
			continue
		}
		o.Title = strings.TrimSpace(o.Title)
		if o.Title == "" {
			continue
		}
		o.Vendor = strings.ToLower(strings.TrimSpace(o.Vendor))
		if o.Vendor == "" || !ValidatePromoIntelVendor(o.Vendor) {
			o.Vendor = "" // 留空由调用方回填源厂商
		}
		o.Category = normalizePromoIntelEnum(strings.TrimSpace(o.Category), PromoIntelItemCategories, PromoIntelCategoryOther)
		o.Relevance = normalizePromoIntelEnum(strings.TrimSpace(o.Relevance),
			[]string{PromoIntelRelevanceHigh, PromoIntelRelevanceMedium, PromoIntelRelevanceLow, PromoIntelRelevanceNone},
			PromoIntelRelevanceMedium)
		if o.Relevance == PromoIntelRelevanceNone {
			continue // 无关营销内容直接丢弃
		}
		o.Summary = strings.TrimSpace(o.Summary)
		o.Details = strings.TrimSpace(o.Details)
		o.Discount = strings.TrimSpace(o.Discount)
		o.ValidUntil = strings.TrimSpace(o.ValidUntil)
		o.URL = strings.TrimSpace(o.URL)
		out = append(out, o)
		if len(out) >= promoIntelMaxItemsPerFetch {
			break
		}
	}
	return out, nil
}

// promoIntelLLMClient 返回 LLM 调用客户端（测试可注入）。
func (s *PromoIntelService) promoIntelLLMClient() *http.Client {
	if s.testLLMClient != nil {
		return s.testLLMClient
	}
	timeout := PromoIntelDefaultLLMTimeoutSeconds * time.Second
	if s.cfg != nil && s.cfg.LLMTimeoutSeconds > 0 {
		timeout = time.Duration(s.cfg.LLMTimeoutSeconds) * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// llmMaxTextChars 送入 LLM 的正文上限（rune）。
func (s *PromoIntelService) llmMaxTextChars() int {
	if s.cfg != nil && s.cfg.LLMMaxTextChars > 0 {
		return s.cfg.LLMMaxTextChars
	}
	return promoIntelLLMTextCap
}

// truncatePromoIntelString 按 rune 截断。必须以 rune 为界：按字节切会把
// 多字节汉字切成非法 UTF-8，入库时被 PG 以 invalid byte sequence 拒绝
// （生产实测：百度千帆源 0xe8 0x8d 0x2e、DeepSeek 0x00）。
func truncatePromoIntelString(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
