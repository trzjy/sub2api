package service

import (
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 优惠情报（Promo Intel）：厂商官方优惠/公告的每日轮询 + LLM 结构化整理 + 管理台简报。
//
// 边界：只轮询管理员配置的官方资讯源，不做开放式全网爬虫；情报条目是给人看的，
// 不自动创建账号/渠道。

// 情报条目业务类型：对 API 聚合转售业务可能的机会分类。
const (
	PromoIntelCategoryFreeQuota    = "free_quota"   // 免费额度/赠送金
	PromoIntelCategoryDiscount     = "discount"     // 直充/首充折扣
	PromoIntelCategorySubscription = "subscription" // 包月/订阅优惠（首月优惠等）
	PromoIntelCategoryPriceChange  = "price_change" // 价格调整
	PromoIntelCategoryNewModel     = "new_model"    // 新模型上线
	PromoIntelCategoryNewProduct   = "new_product"  // 新产品/新功能上线
	PromoIntelCategoryEvent        = "event"        // 限时活动
	PromoIntelCategoryPolicy       = "policy"       // 政策/限速/风控/结算变化
	PromoIntelCategoryOther        = "other"
)

// PromoIntelItemCategories 是 LLM 输出与筛选允许的全部条目类型。
var PromoIntelItemCategories = []string{
	PromoIntelCategoryFreeQuota,
	PromoIntelCategoryDiscount,
	PromoIntelCategorySubscription,
	PromoIntelCategoryPriceChange,
	PromoIntelCategoryNewModel,
	PromoIntelCategoryNewProduct,
	PromoIntelCategoryEvent,
	PromoIntelCategoryPolicy,
	PromoIntelCategoryOther,
}

// 资讯源页面性质。
const (
	PromoIntelSourceCategoryAnnouncement = "announcement" // 公告/新闻
	PromoIntelSourceCategoryChangelog    = "changelog"    // 更新日志
	PromoIntelSourceCategoryPricing      = "pricing"      // 价格页
	PromoIntelSourceCategoryActivity     = "activity"     // 活动/优惠页
	PromoIntelSourceCategoryBlog         = "blog"         // 官方博客
)

// PromoIntelSourceCategories 是资讯源允许的全部页面性质。
var PromoIntelSourceCategories = []string{
	PromoIntelSourceCategoryAnnouncement,
	PromoIntelSourceCategoryChangelog,
	PromoIntelSourceCategoryPricing,
	PromoIntelSourceCategoryActivity,
	PromoIntelSourceCategoryBlog,
}

// 情报相关性（LLM 判定，对 API 聚合转售业务）。
const (
	PromoIntelRelevanceHigh   = "high"
	PromoIntelRelevanceMedium = "medium"
	PromoIntelRelevanceLow    = "low"
	PromoIntelRelevanceNone   = "none"
)

// 情报条目分诊状态。
const (
	PromoIntelItemStatusPending = "pending" // 待阅
	PromoIntelItemStatusUseful  = "useful"  // 有用
	PromoIntelItemStatusIgnored = "ignored" // 忽略
)

// 整理状态。
const (
	PromoIntelExtractLLM     = "llm"     // 已结构化整理
	PromoIntelExtractPending = "pending" // 原文待整理（LLM 未配置/失败，恢复后自动补跑）
)

// 抓取状态（源维度最近一次结果）。
const (
	PromoIntelFetchStatusOK    = "ok"
	PromoIntelFetchStatusError = "error"
)

// 常量约束：与 ent schema 的 MaxLen/Range 保持同步。
const (
	promoIntelSourceNameMaxLen = 100
	promoIntelSourceURLMaxLen  = 2048
	promoIntelVendorMaxLen     = 50
	promoIntelMinIntervalMin   = 30           // 最短 30 分钟，礼貌抓取
	promoIntelMaxIntervalMin   = 60 * 24 * 30 // 最长 30 天
	promoIntelDefaultInterval  = 1440         // 默认每天一次
	promoIntelMaxItemsPerFetch = 20           // 单次 LLM 提取最多条目数
	promoIntelRawExcerptMaxLen = 4000         // 降级原文摘录上限（字节）
	promoIntelLLMTextCap       = 12000        // 送入 LLM 的正文上限（字符，rune）
)

// PromoIntelSource 优惠情报资讯源（service 层模型，不直接暴露 ent 类型）。
type PromoIntelSource struct {
	ID                   int64
	Name                 string
	Vendor               string
	Category             string
	URL                  string
	FetchIntervalMinutes int
	Enabled              bool
	LLMExtract           bool
	Notes                string
	LastFetchedAt        *time.Time
	LastExtractedHash    string
	LastStatus           string
	LastError            string
	CreatedBy            int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// PromoIntelItem 一条优惠情报（LLM 结构化结果或降级原文）。
type PromoIntelItem struct {
	ID              int64
	SourceID        int64
	SourceName      string // 列表展示用，由仓储层补充
	Vendor          string
	Category        string
	Title           string
	Summary         string
	Details         string
	DiscountInfo    string
	ValidUntil      string
	URL             string
	Relevance       string
	Status          string
	Fingerprint     string
	RawExcerpt      string
	ExtractStatus   string
	DigestDate      *time.Time
	SourceFetchedAt time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// PromoIntelSourceListParams 源列表查询参数。
type PromoIntelSourceListParams struct {
	Page     int
	PageSize int
	Vendor   string
	Enabled  *bool
	Search   string
}

// PromoIntelItemListParams 情报列表查询参数。
type PromoIntelItemListParams struct {
	Page      int
	PageSize  int
	Vendor    string
	Category  string
	Relevance string
	Status    string
	// DigestDate 为空表示不过滤；格式 YYYY-MM-DD（按 UTC 日期）。
	DigestDate string
	Search     string
}

// PromoIntelSourceCreateParams 创建资讯源参数。
type PromoIntelSourceCreateParams struct {
	Name                 string
	Vendor               string
	Category             string
	URL                  string
	FetchIntervalMinutes int
	Enabled              *bool
	LLMExtract           *bool
	Notes                string
	CreatedBy            int64
}

// PromoIntelSourceUpdateParams 更新资讯源参数（nil = 不修改）。
type PromoIntelSourceUpdateParams struct {
	Name                 *string
	Vendor               *string
	Category             *string
	URL                  *string
	FetchIntervalMinutes *int
	Enabled              *bool
	LLMExtract           *bool
	Notes                *string
}

// PromoIntelFetchResult 单次抓取+整理的结果（立即抓取接口返回给前端）。
type PromoIntelFetchResult struct {
	Source         *PromoIntelSource
	ContentChanged bool
	ItemsCreated   int
	ItemsUpdated   int
	SkippedReason  string // 非空表示未进入整理层的原因（如 llm_not_configured）
}

// PromoIntelBriefing 每日简报（按发现日期聚合）。
type PromoIntelBriefing struct {
	Date       string            `json:"date"`
	Total      int64             `json:"total"`
	HighCount  int64             `json:"high_count"`
	Pending    int64             `json:"pending"`
	ByVendor   map[string]int64  `json:"by_vendor"`
	ByCategory map[string]int64  `json:"by_category"`
	Items      []*PromoIntelItem `json:"items"`
}

// PromoIntelRuntime 优惠情报运行时开关与 LLM 配置（settings 表读取）。
type PromoIntelRuntime struct {
	Enabled bool
	// LLMConfigured 表示 base_url+model 已配置（api_key 可空——部分本地端点无鉴权）。
	LLMConfigured   bool
	LLMBaseURL      string
	LLMAPIKeySet    bool
	LLMAPIKeyMasked string
	LLMModel        string
}

// HasLLM 报告整理层是否可用。
func (r PromoIntelRuntime) HasLLM() bool { return r.LLMConfigured }

// Settings 键（domain_constants.go 有对应常量，此处集中默认值）。
const (
	PromoIntelDefaultLLMTimeoutSeconds = 120
	PromoIntelDefaultScanIntervalSec   = 60
	PromoIntelDefaultFetchTimeoutSec   = 30
	PromoIntelDefaultFetchMaxBytes     = 2 << 20
	PromoIntelDefaultWorkerConcurrency = 2
)

// Sentinel errors。
var (
	ErrPromoIntelSourceNotFound = infraerrors.NotFound(
		"PROMO_INTEL_SOURCE_NOT_FOUND", "promo intel source not found")
	ErrPromoIntelItemNotFound = infraerrors.NotFound(
		"PROMO_INTEL_ITEM_NOT_FOUND", "promo intel item not found")
	ErrPromoIntelDisabled = infraerrors.Forbidden(
		"PROMO_INTEL_DISABLED", "promo intel feature is disabled")
	ErrPromoIntelInvalidURL = infraerrors.BadRequest(
		"PROMO_INTEL_INVALID_URL", "promo intel source url must be a valid http(s) url")
)

// normalizePromoIntelEnum 宽容归一化 LLM/前端给的枚举值：不在白名单则回退默认。
func normalizePromoIntelEnum(value string, allowed []string, fallback string) string {
	for _, a := range allowed {
		if value == a {
			return value
		}
	}
	return fallback
}

// ValidatePromoIntelVendor 厂商标识：非空、≤50 字符、仅小写字母/数字/连字符，
// 允许自定义新厂商（情报边界不等于已接入平台）。
func ValidatePromoIntelVendor(vendor string) bool {
	if vendor == "" || len(vendor) > promoIntelVendorMaxLen {
		return false
	}
	for _, r := range vendor {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
