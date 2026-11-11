package antigravity

import (
	"math"
	"strings"
)

// UserQuotaUsage 单个模型的真实剩余配额（同一模型多维度桶已聚合为最差水位）。
type UserQuotaUsage struct {
	ModelID           string
	RemainingFraction float64
	ResetTime         string
}

// UsagePercent 返回已用百分比（0-100，保留两位小数）。
//
// 不取整：上游大配额池里少量请求可能只把 remainingFraction 降到 0.9995，
// 取整后变成 0，面板会把「用了但很少」显示成「完全没用」。保留小数交给前端
// 呈现「<1%」。
func (u UserQuotaUsage) UsagePercent() float64 {
	percent := (1 - u.RemainingFraction) * 100
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return math.Round(percent*100) / 100
}

// isNonModelQuotaKey 过滤 retrieveUserQuota 里不是模型配额的噪音条目
// （如 chat_20706、tab_flash_lite_preview），它们没有模型语义，混进面板只会干扰阅读。
func isNonModelQuotaKey(modelID string) bool {
	return strings.HasPrefix(modelID, "chat_") || strings.HasPrefix(modelID, "tab_")
}

// AggregateUserQuotaBuckets 把 retrieveUserQuota 的 buckets 聚合成 modelID → 最差水位。
//
// 同一 modelId 可能出现多条（tokenType 区分「请求数」与「token 数」两个维度），
// 只要任一维度接近耗尽，这个模型就不能再算可用，所以取剩余最少的作为该模型水位；
// 对应的 resetTime 也跟随这条最差记录。
func AggregateUserQuotaBuckets(buckets []UserQuotaBucket) map[string]UserQuotaUsage {
	usage := make(map[string]UserQuotaUsage, len(buckets))
	for _, bucket := range buckets {
		modelID := NormalizeUserQuotaModelID(bucket.ModelID)
		if modelID == "" || isNonModelQuotaKey(modelID) {
			continue
		}
		fraction := bucket.RemainingFraction
		if fraction < 0 {
			fraction = 0
		}
		if fraction > 1 {
			fraction = 1
		}

		current, ok := usage[modelID]
		if !ok {
			usage[modelID] = UserQuotaUsage{
				ModelID:           modelID,
				RemainingFraction: fraction,
				ResetTime:         bucket.ResetTime,
			}
			continue
		}
		if fraction < current.RemainingFraction {
			current.RemainingFraction = fraction
			if bucket.ResetTime != "" {
				current.ResetTime = bucket.ResetTime
			}
			usage[modelID] = current
		}
	}
	return usage
}

// NormalizeUserQuotaModelID 统一 modelId 形态：部分端点会带 models/ 前缀。
func NormalizeUserQuotaModelID(raw string) string {
	modelID := strings.TrimSpace(raw)
	modelID = strings.TrimPrefix(modelID, "models/")
	return modelID
}
