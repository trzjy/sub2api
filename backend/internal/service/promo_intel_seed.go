package service

import (
	"context"
	"errors"
	"log/slog"
)

// 默认资讯源清单：全部经外部逐个实测（HTTP 可达 + 正文可提取）后才列入。
// 验证日期 2026-09-13；清单以数据形式维护，管理台可增删改，`seed_defaults`
// 配置只在缺失名字时补种，绝不覆盖管理员的修改。
//
// 已知无法直抓、未列入的渠道（管理员可自行评估）：
//   - 火山引擎官网活动页/公众号（官网纯 JS 渲染；模型价格走文档结构化接口已覆盖）
//   - Windsurf（Vercel 盾 429）、Trae（壳页 SPA）、CodeBuddy 国际版（无公开 changelog）
//   - 讯飞星火活动页（内容长期未更新）、商汤（重度依赖公众号）
//   - 零一万物（已公告停服，无监控价值）
type promoIntelDefaultSource struct {
	Name     string
	Vendor   string
	Category string
	URL      string
	Notes    string
	Interval int // 分钟；0 = 默认 1440（每日）
}

func defaultPromoIntelSources() []promoIntelDefaultSource {
	return []promoIntelDefaultSource{
		// ---- 国内：云厂商大模型平台 ----
		{Name: "阿里云百炼·模型发布记录", Vendor: "alibaba", Category: PromoIntelSourceCategoryChangelog,
			URL:   "https://help.aliyun.com/zh/model-studio/model-release-notes",
			Notes: "模型上下架与发布记录，情报价值最高"},
		{Name: "百度千帆·模型更新记录", Vendor: "baidu", Category: PromoIntelSourceCategoryChangelog,
			URL:   "https://cloud.baidu.com/doc/qianfan/s/Kmh4stnjp",
			Notes: "Token Plan 上线、批量推理促销等公告"},
		{Name: "百度千帆·免费额度说明", Vendor: "baidu", Category: PromoIntelSourceCategoryPricing,
			URL: "https://cloud.baidu.com/doc/qianfan/s/Imi2rpirg", Notes: "新用户免费额度规则"},
		{Name: "腾讯云混元·计费概述", Vendor: "tencent", Category: PromoIntelSourceCategoryPricing,
			URL: "https://cloud.tencent.com/document/product/1729/97731", Notes: "含免费额度章节"},
		{Name: "火山方舟·模型价格（文档接口）", Vendor: "volcano", Category: PromoIntelSourceCategoryPricing,
			URL:   "https://docs.volcengine.com/api/doc/getDocDetail?DocumentID=1544106",
			Notes: "官方文档结构化接口直连（官网页面纯 JS 渲染无法直抓）；各模型单价与计费说明"},

		// ---- 国内：模型厂 ----
		{Name: "DeepSeek·定价", Vendor: "deepseek", Category: PromoIntelSourceCategoryPricing,
			URL: "https://api-docs.deepseek.com/zh-cn/quick_start/pricing", Notes: "峰谷定价结构"},
		{Name: "DeepSeek·更新日志", Vendor: "deepseek", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://api-docs.deepseek.com/zh-cn/updates", Notes: "含调价记录"},
		{Name: "Kimi 开放平台·定价说明", Vendor: "kimi", Category: PromoIntelSourceCategoryPricing,
			URL: "https://platform.kimi.com/docs/pricing/chat", Notes: "platform.moonshot.cn 旧域重定向至此"},
		{Name: "智谱 BigModel·发布记录", Vendor: "zhipu", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://docs.bigmodel.cn/cn/update/new-releases"},
		{Name: "智谱 BigModel·上新活动", Vendor: "zhipu", Category: PromoIntelSourceCategoryActivity,
			URL: "https://docs.bigmodel.cn/cn/update/promotion", Notes: "官方促销专属页", Interval: 720},
		{Name: "MiniMax·按量计费", Vendor: "minimax", Category: PromoIntelSourceCategoryPricing,
			URL: "https://platform.minimax.cn/docs/guides/pricing-paygo"},
		{Name: "阶跃星辰·定价", Vendor: "stepfun", Category: PromoIntelSourceCategoryPricing,
			URL: "https://platform.stepfun.com/docs/zh/guides/pricing/details", Notes: "含 Step Plan 订阅"},

		// ---- 国内：API 聚合/中转商（竞对动态同样有情报价值） ----
		{Name: "硅基流动·价格总览", Vendor: "siliconflow", Category: PromoIntelSourceCategoryPricing,
			URL: "https://siliconflow.cn/pricing"},
		{Name: "硅基流动·更新公告", Vendor: "siliconflow", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://docs.siliconflow.cn/docs/release-notes/overview"},
		{Name: "PPIO 派欧云·官方公告", Vendor: "ppio", Category: PromoIntelSourceCategoryAnnouncement,
			URL: "https://ppio.com/docs/announcement/announcement"},
		{Name: "PPIO 派欧云·大模型发版记录", Vendor: "ppio", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://ppio.com/docs/announcement/changelog-llm"},
		{Name: "DMXAPI·价格承诺", Vendor: "dmxapi", Category: PromoIntelSourceCategoryPricing,
			URL: "https://dmxapi.cn/chengnuo.html", Notes: "主站为纯 SPA，此页为唯一可抓页面"},
		{Name: "AiHubMix·更新日志", Vendor: "aihubmix", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://docs.aihubmix.com/cn/update/News", Notes: "第三方聚合商，可信度中低，供竞对参考"},

		// ---- 海外：模型厂 ----
		{Name: "OpenAI·定价", Vendor: "openai", Category: PromoIntelSourceCategoryPricing,
			URL: "https://platform.openai.com/docs/pricing"},
		{Name: "Anthropic·定价", Vendor: "anthropic", Category: PromoIntelSourceCategoryPricing,
			URL: "https://docs.claude.com/en/docs/about-claude/pricing", Notes: "docs.anthropic.com 已迁移至此"},
		{Name: "Anthropic·新闻", Vendor: "anthropic", Category: PromoIntelSourceCategoryAnnouncement,
			URL: "https://www.anthropic.com/news"},
		{Name: "Google Gemini·定价", Vendor: "google", Category: PromoIntelSourceCategoryPricing,
			URL: "https://ai.google.dev/gemini-api/docs/pricing", Notes: "含免费层/付费层明细"},
		{Name: "Google Gemini·更新日志", Vendor: "google", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://ai.google.dev/gemini-api/docs/changelog"},
		{Name: "xAI·模型与定价", Vendor: "xai", Category: PromoIntelSourceCategoryPricing,
			URL: "https://docs.x.ai/docs/models"},
		{Name: "Mistral·定价", Vendor: "mistral", Category: PromoIntelSourceCategoryPricing,
			URL: "https://docs.mistral.ai/deployment/laplateforme/pricing"},
		{Name: "Mistral·新闻 RSS", Vendor: "mistral", Category: PromoIntelSourceCategoryBlog,
			URL: "https://mistral.ai/rss.xml", Notes: "RSS，抓取最稳"},

		// ---- 海外：聚合商与编码工具（订阅羊毛重点） ----
		{Name: "OpenRouter·公告", Vendor: "openrouter", Category: PromoIntelSourceCategoryAnnouncement,
			URL: "https://openrouter.ai/announcements", Notes: "免费模型/促销多发于此"},
		{Name: "Together AI·定价", Vendor: "together", Category: PromoIntelSourceCategoryPricing,
			URL: "https://www.together.ai/pricing"},
		{Name: "Fireworks AI·定价", Vendor: "fireworks", Category: PromoIntelSourceCategoryPricing,
			URL: "https://fireworks.ai/pricing"},
		{Name: "Groq·新闻", Vendor: "groq", Category: PromoIntelSourceCategoryAnnouncement,
			URL: "https://groq.com/newsroom"},
		{Name: "DeepInfra·定价", Vendor: "deepinfra", Category: PromoIntelSourceCategoryPricing,
			URL: "https://deepinfra.com/pricing"},
		{Name: "GitHub Changelog RSS", Vendor: "github", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://github.blog/changelog/feed/", Notes: "RSS；Copilot 订阅变更高频"},
		{Name: "GitHub Copilot·订阅计划", Vendor: "github", Category: PromoIntelSourceCategoryPricing,
			URL: "https://github.com/features/copilot/plans", Notes: "订阅计划与免费额度"},
		{Name: "Cursor·更新日志 RSS", Vendor: "cursor", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://cursor.com/changelog/rss.xml", Notes: "RSS；订阅/额度变动首选"},
		{Name: "Qoder·更新日志", Vendor: "qoder", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://qoder.com/changelog"},
		{Name: "Augment·更新日志", Vendor: "augment", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://www.augmentcode.com/changelog"},
		{Name: "Zed·发布记录", Vendor: "zed", Category: PromoIntelSourceCategoryChangelog,
			URL: "https://zed.dev/releases"},
	}
}

// ensureDefaultSources 幂等补种：只按名字补缺，绝不覆盖既有行（含被管理员停用的）。
func (s *PromoIntelService) ensureDefaultSources(ctx context.Context) error {
	if s == nil || s.repo == nil {
		return nil
	}
	seeded := 0
	for _, def := range defaultPromoIntelSources() {
		existing, err := s.repo.GetSourceByName(ctx, def.Name)
		if err != nil && !errors.Is(err, ErrPromoIntelSourceNotFound) {
			return err
		}
		if existing != nil {
			continue
		}
		interval := def.Interval
		if interval <= 0 {
			interval = promoIntelDefaultInterval
		}
		src := &PromoIntelSource{
			Name:                 def.Name,
			Vendor:               def.Vendor,
			Category:             def.Category,
			URL:                  def.URL,
			FetchIntervalMinutes: interval,
			Enabled:              true,
			LLMExtract:           true,
			Notes:                def.Notes,
			CreatedBy:            0,
		}
		if err := s.repo.CreateSource(ctx, src); err != nil {
			// 唯一索引冲突等并发补种场景静默跳过。
			slog.Warn("promo_intel: seed source skipped", "name", def.Name, "err", err)
			continue
		}
		seeded++
	}
	if seeded > 0 {
		slog.Info("promo_intel: default sources seeded", "count", seeded)
	}
	return nil
}
