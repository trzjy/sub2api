package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

const (
	forbiddenTypeValidation = "validation"
	forbiddenTypeViolation  = "violation"
	forbiddenTypeForbidden  = "forbidden"

	// 机器可读的错误码
	errorCodeForbidden       = "forbidden"
	errorCodeUnauthenticated = "unauthenticated"
	errorCodeRateLimited     = "rate_limited"
	errorCodeNetworkError    = "network_error"
)

// AntigravityQuotaFetcher 从 Antigravity API 获取额度
type AntigravityQuotaFetcher struct {
	proxyRepo ProxyRepository
	cfg       *config.Config
}

// NewAntigravityQuotaFetcher 创建 AntigravityQuotaFetcher
func NewAntigravityQuotaFetcher(proxyRepo ProxyRepository, cfg *config.Config) *AntigravityQuotaFetcher {
	return &AntigravityQuotaFetcher{proxyRepo: proxyRepo, cfg: cfg}
}

// CanFetch 检查是否可以获取此账户的额度
func (f *AntigravityQuotaFetcher) CanFetch(account *Account) bool {
	if account.Platform != PlatformAntigravity {
		return false
	}
	accessToken := account.GetCredential("access_token")
	return accessToken != ""
}

// FetchQuota 获取 Antigravity 账户额度信息
func (f *AntigravityQuotaFetcher) FetchQuota(ctx context.Context, account *Account, proxyURL string) (*QuotaResult, error) {
	accessToken := account.GetCredential("access_token")
	projectID := account.GetCredential("project_id")

	client, err := antigravity.NewClient(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create antigravity client failed: %w", err)
	}

	// 调用 API 获取配额
	modelsResp, modelsRaw, err := client.FetchAvailableModels(ctx, accessToken, projectID, resolveModelsListReadLimit(f.cfg))
	if err != nil {
		// 403 Forbidden: 不报错，返回 is_forbidden 标记
		var forbiddenErr *antigravity.ForbiddenError
		if errors.As(err, &forbiddenErr) {
			now := time.Now()
			fbType := classifyForbiddenType(forbiddenErr.Body)
			return &QuotaResult{
				UsageInfo: &UsageInfo{
					UpdatedAt:       &now,
					IsForbidden:     true,
					ForbiddenReason: forbiddenErr.Body,
					ForbiddenType:   fbType,
					ValidationURL:   extractValidationURL(forbiddenErr.Body),
					NeedsVerify:     fbType == forbiddenTypeValidation,
					IsBanned:        fbType == forbiddenTypeViolation,
					ErrorCode:       errorCodeForbidden,
				},
			}, nil
		}
		return nil, err
	}

	// 调用 LoadCodeAssist 获取订阅等级和 AI Credits 余额（非关键路径，失败不影响主流程）
	tierRaw, tierNormalized, loadResp := f.fetchSubscriptionTier(ctx, client, accessToken)

	// 转换为 UsageInfo
	usageInfo := f.buildUsageInfo(modelsResp, tierRaw, tierNormalized, loadResp)

	// 用真实的个人订阅配额覆盖模型目录附带的项目池水位。
	// 非关键路径：失败保留原值，不阻断额度刷新。
	f.applyUserQuota(ctx, client, accessToken, projectID, usageInfo)

	return &QuotaResult{
		UsageInfo: usageInfo,
		Raw:       modelsRaw,
	}, nil
}

// applyUserQuota 调 retrieveUserQuota 取该账号本人的订阅配额，覆盖 AntigravityQuota 里的使用率。
//
// 为什么必须这一步：fetchAvailableModels 的 quotaInfo 是模型目录顺带返回的项目池水位，
// 三个账号共用同一个 project 时它不跟随单账号消耗变化（实测单账号 5 小时 86 次调用后
// remainingFraction 仍为 1），面板据此画出的「已用 0%」是假数据。retrieveUserQuota 的
// buckets 才是按账号、按模型给出的真实剩余比例（与官方 gemini-cli 查额度同源）。
func (f *AntigravityQuotaFetcher) applyUserQuota(ctx context.Context, client *antigravity.Client, accessToken, projectID string, usageInfo *UsageInfo) {
	if client == nil || usageInfo == nil || usageInfo.AntigravityQuota == nil {
		return
	}

	quotaResp, _, err := client.RetrieveUserQuota(ctx, accessToken, projectID, resolveModelsListReadLimit(f.cfg))
	if err != nil {
		// 上游该接口不可用时保持旧行为（项目池水位），只记录不报错。
		slog.Debug("antigravity retrieveUserQuota failed, keep catalog quota", "error", err)
		return
	}
	if quotaResp == nil || len(quotaResp.Buckets) == 0 {
		return
	}

	// 同一 modelId 可能有多条（请求数 / token 数两个维度），聚合时取剩余最少的作为该模型水位。
	aggregated := antigravity.AggregateUserQuotaBuckets(quotaResp.Buckets)
	if len(aggregated) == 0 {
		return
	}

	modelIDs := make([]string, 0, len(aggregated))
	for modelID, bucket := range aggregated {
		entry, ok := usageInfo.AntigravityQuota[modelID]
		if !ok || entry == nil {
			entry = &AntigravityModelQuota{}
			usageInfo.AntigravityQuota[modelID] = entry
		}
		entry.Utilization = bucket.UsagePercent()
		if bucket.ResetTime != "" {
			entry.ResetTime = bucket.ResetTime
		}
		modelIDs = append(modelIDs, modelID)
	}

	slog.Debug("antigravity user quota applied", "models", modelIDs)
}

// fetchSubscriptionTier 获取账号订阅等级，失败返回空字符串。
// 同时返回 LoadCodeAssistResponse，以便提取 AI Credits 余额。
func (f *AntigravityQuotaFetcher) fetchSubscriptionTier(ctx context.Context, client *antigravity.Client, accessToken string) (raw, normalized string, loadResp *antigravity.LoadCodeAssistResponse) {
	loadResp, _, err := client.LoadCodeAssist(ctx, accessToken)
	if err != nil {
		slog.Warn("failed to fetch subscription tier", "error", err)
		return "", "", nil
	}
	if loadResp == nil {
		return "", "", nil
	}

	raw = loadResp.GetTier() // 已有方法：paidTier > currentTier
	normalized = normalizeTier(raw)
	return raw, normalized, loadResp
}

// normalizeTier 将原始 tier 字符串归一化为 FREE/PRO/ULTRA/UNKNOWN
func normalizeTier(raw string) string {
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "ultra"):
		return "ULTRA"
	case strings.Contains(lower, "pro"):
		return "PRO"
	case strings.Contains(lower, "free"):
		return "FREE"
	default:
		return "UNKNOWN"
	}
}

// buildUsageInfo 将 API 响应转换为 UsageInfo。
func (f *AntigravityQuotaFetcher) buildUsageInfo(modelsResp *antigravity.FetchAvailableModelsResponse, tierRaw, tierNormalized string, loadResp *antigravity.LoadCodeAssistResponse) *UsageInfo {
	now := time.Now()
	info := &UsageInfo{
		UpdatedAt:               &now,
		AntigravityQuota:        make(map[string]*AntigravityModelQuota),
		AntigravityQuotaDetails: make(map[string]*AntigravityModelDetail),
		SubscriptionTier:        tierNormalized,
		SubscriptionTierRaw:     tierRaw,
	}

	// 遍历所有模型，填充 AntigravityQuota 和 AntigravityQuotaDetails
	for modelName, modelInfo := range modelsResp.Models {
		if modelInfo.QuotaInfo == nil {
			continue
		}

		// remainingFraction 是剩余比例 (0.0-1.0)，转换为使用率百分比。
		// 保留两位小数，避免小比例消耗被取整成 0。
		utilization := math.Round((1.0-modelInfo.QuotaInfo.RemainingFraction)*100*100) / 100

		info.AntigravityQuota[modelName] = &AntigravityModelQuota{
			Utilization: utilization,
			ResetTime:   modelInfo.QuotaInfo.ResetTime,
		}

		// 填充模型详细能力信息
		detail := &AntigravityModelDetail{
			DisplayName:        modelInfo.DisplayName,
			SupportsImages:     modelInfo.SupportsImages,
			SupportsThinking:   modelInfo.SupportsThinking,
			ThinkingBudget:     modelInfo.ThinkingBudget,
			Recommended:        modelInfo.Recommended,
			MaxTokens:          modelInfo.MaxTokens,
			MaxOutputTokens:    modelInfo.MaxOutputTokens,
			SupportedMimeTypes: modelInfo.SupportedMimeTypes,
		}
		info.AntigravityQuotaDetails[modelName] = detail
	}

	// 废弃模型转发规则
	if len(modelsResp.DeprecatedModelIDs) > 0 {
		info.ModelForwardingRules = make(map[string]string, len(modelsResp.DeprecatedModelIDs))
		for oldID, deprecated := range modelsResp.DeprecatedModelIDs {
			info.ModelForwardingRules[oldID] = deprecated.NewModelID
		}
	}

	// 同时设置 FiveHour 用于兼容展示（取主要模型）
	priorityModels := []string{"claude-sonnet-4-20250514", "claude-sonnet-4", "gemini-2.5-pro"}
	for _, modelName := range priorityModels {
		if modelInfo, ok := modelsResp.Models[modelName]; ok && modelInfo.QuotaInfo != nil {
			utilization := (1.0 - modelInfo.QuotaInfo.RemainingFraction) * 100
			progress := &UsageProgress{
				Utilization: utilization,
			}
			if modelInfo.QuotaInfo.ResetTime != "" {
				if resetTime, err := time.Parse(time.RFC3339, modelInfo.QuotaInfo.ResetTime); err == nil {
					progress.ResetsAt = &resetTime
					progress.RemainingSeconds = int(time.Until(resetTime).Seconds())
				}
			}
			info.FiveHour = progress
			break
		}
	}

	if loadResp != nil {
		for _, credit := range loadResp.GetAvailableCredits() {
			info.AICredits = append(info.AICredits, AICredit{
				CreditType:     credit.CreditType,
				Amount:         credit.GetAmount(),
				MinimumBalance: credit.GetMinimumAmount(),
			})
		}
	}

	return info
}

// GetProxyURL 获取账户的代理 URL
func (f *AntigravityQuotaFetcher) GetProxyURL(ctx context.Context, account *Account) string {
	if account.ProxyID == nil || f.proxyRepo == nil {
		return ""
	}
	proxy, err := f.proxyRepo.GetByID(ctx, *account.ProxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}

// classifyForbiddenType 根据 403 响应体判断禁止类型
func classifyForbiddenType(body string) string {
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(lower, "validation_required") ||
		strings.Contains(lower, "verify your account") ||
		strings.Contains(lower, "validation_url"):
		return forbiddenTypeValidation
	case strings.Contains(lower, "terms of service") ||
		strings.Contains(lower, "violation"):
		return forbiddenTypeViolation
	default:
		return forbiddenTypeForbidden
	}
}

// urlPattern 用于从 403 响应体中提取 URL（降级方案）
var urlPattern = regexp.MustCompile(`https://[^\s"'\\]+`)

// extractValidationURL 从 403 响应 JSON 中提取验证/申诉链接
func extractValidationURL(body string) string {
	// 1. 尝试结构化 JSON 提取: /error/details[*]/metadata/validation_url 或 appeal_url
	var parsed struct {
		Error struct {
			Details []struct {
				Metadata map[string]string `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &parsed) == nil {
		for _, detail := range parsed.Error.Details {
			if u := detail.Metadata["validation_url"]; u != "" {
				return u
			}
			if u := detail.Metadata["appeal_url"]; u != "" {
				return u
			}
		}
	}

	// 2. 降级：正则匹配 URL
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "validation") &&
		!strings.Contains(lower, "verify") &&
		!strings.Contains(lower, "appeal") {
		return ""
	}
	// 先解码常见转义再匹配
	normalized := strings.ReplaceAll(body, `\u0026`, "&")
	if m := urlPattern.FindString(normalized); m != "" {
		return m
	}
	return ""
}
