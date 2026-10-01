package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// muse 平台上游模型目录同步（muse-4）：照 minimax/SyncVolcanoPlanModels 模式，从
// GET <base_url>/models 拉取上游目录 → 过滤（前缀 muse-spark + 必须 -contributor 结尾 +
// 显式排除 -free）→ 派生干净名 → 落库系统托管快照并自动建立 clean→contributor 的
// model_mapping。下游对外唯一契约 = 干净名。
const (
	// museManagedModelExtraKey 是账号 extra 中 muse 系统托管模型快照的 key。
	museManagedModelExtraKey = "muse_managed_models"
	// museModelPrefix 是 muse 家模型 ID 的前缀（仅该前缀进入候选）。
	museModelPrefix = "muse-spark"
	// museContributorSuffix 是 muse 付费 contributor 模型的强制后缀。
	museContributorSuffix = "-contributor"
	// museFreeSuffix 是免费变体的后缀（用户裁定不接免费模型，显式排除）。
	museFreeSuffix = "-free"
)

// museLogger 是 muse 同步的观测日志出口（默认走 slog.Default，测试可指向捕获 handler）。
var museLogger = slog.Default()

// museManagedModel 记录一个由 muse 同步器建立的干净名→contributor ID 映射。
type museManagedModel struct {
	CleanName     string `json:"clean_name"`
	ContributorID string `json:"contributor_id"`
}

// MuseManagedModels 是系统托管的 muse 上游模型集合快照（随账号 extra 持久化）。
// Managed 为系统托管集（clean→contributor）；LastSuccessAt / ConsecutiveFailures 为
// 运行观测状态（外审-8）。
type MuseManagedModels struct {
	Managed             []museManagedModel `json:"managed"`
	SyncedAt            time.Time          `json:"synced_at"`
	LastSuccessAt       *time.Time         `json:"last_success_at,omitempty"`
	ConsecutiveFailures int                `json:"consecutive_failures"`
}

// MuseModelFilterReason 是被过滤规则丢弃的单个上游模型及其原因。
// Reason 取值：free_variant（显式 -free 后缀被排除）、prefix（非 muse-spark 前缀）、
// suffix_not_contributor（不以 -contributor 结尾）。
type MuseModelFilterReason struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// isMuseAccount 报告账号是否指向 muse 平台。
func isMuseAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformMuse
}

// filterMuseUpstreamModels 应用 muse 目录过滤规则（外审-2 收敛，落代码）：
//   - 显式排除 -free 后缀（免费裁定，防未来目录混入 free 变体；优先级最高）；
//   - 其余必须以 "muse-spark" 前缀开头 且 以 "-contributor" 结尾，否则丢弃
//     （防同模型双货源打架：非 muse 前缀 或 非 contributor 版一律不入库）；
//   - 通过者派生干净名 = 去掉 "-contributor" 后缀。
//
// 返回通过集合（clean→contributor）与被丢弃集合（含原因分类，已排序以便测试断言）。
func filterMuseUpstreamModels(rawIDs []string) ([]museManagedModel, []MuseModelFilterReason) {
	accepted := make([]museManagedModel, 0, len(rawIDs))
	discarded := make([]MuseModelFilterReason, 0, len(rawIDs))
	for _, id := range rawIDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		// 免费变体显式排除（优先级最高，防未来目录混入 free 变体）。
		if strings.HasSuffix(trimmed, museFreeSuffix) {
			discarded = append(discarded, MuseModelFilterReason{Model: trimmed, Reason: "free_variant"})
			continue
		}
		// 前缀 + contributor 后缀双约束：非 muse 前缀 或 非 contributor 版一律不入库。
		if !strings.HasPrefix(trimmed, museModelPrefix) {
			discarded = append(discarded, MuseModelFilterReason{Model: trimmed, Reason: "prefix"})
			continue
		}
		if !strings.HasSuffix(trimmed, museContributorSuffix) {
			discarded = append(discarded, MuseModelFilterReason{Model: trimmed, Reason: "suffix_not_contributor"})
			continue
		}
		clean := strings.TrimSuffix(trimmed, museContributorSuffix)
		accepted = append(accepted, museManagedModel{CleanName: clean, ContributorID: trimmed})
	}
	sort.Slice(accepted, func(i, j int) bool {
		return accepted[i].CleanName < accepted[j].CleanName
	})
	sort.Slice(discarded, func(i, j int) bool {
		if discarded[i].Reason != discarded[j].Reason {
			return discarded[i].Reason < discarded[j].Reason
		}
		return discarded[i].Model < discarded[j].Model
	})
	return accepted, discarded
}

// fetchMuseUpstreamCatalog 拉取 muse 上游模型目录 GET <base_url>/models（base_url 默认
// https://opencode.ai/zen/go/v1）。返回原始模型 ID、上游 HTTP 状态码、错误。
func (s *AccountTestService) fetchMuseUpstreamCatalog(ctx context.Context, account *Account) ([]string, int, error) {
	apiKey := strings.TrimSpace(account.GetCredential("api_key"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	}
	if apiKey == "" {
		return nil, 0, newUpstreamModelSyncConfigError("No muse API key is available", nil)
	}
	baseURL := strings.TrimSpace(account.GetCredential("base_url"))
	if baseURL == "" {
		baseURL = "https://opencode.ai/zen/go/v1"
	}
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, 0, newUpstreamModelSyncConfigError("Invalid muse base URL", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, buildOpenAIModelsURL(normalizedBaseURL), nil)
	if err != nil {
		return nil, 0, newUpstreamModelSyncConfigError("Invalid muse model list URL", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := upstreamModelsProxyURL(account)
	resp, err := s.doUpstreamModelsRequest(req, proxyURL, account)
	if err != nil {
		return nil, 0, newUpstreamModelSyncUpstreamError("Failed to request muse model list", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.StatusCode, &UpstreamModelSyncError{
			Kind:       UpstreamModelSyncErrorUpstream,
			Message:    fmt.Sprintf("Muse model list request failed with HTTP %d", resp.StatusCode),
			StatusCode: resp.StatusCode,
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, upstreamModelsBodyLimit))
	if err != nil {
		return nil, resp.StatusCode, newUpstreamModelSyncUpstreamError("Failed to read muse model list", err)
	}
	models, err := extractUpstreamModelIDs(body)
	if err != nil {
		return nil, resp.StatusCode, newUpstreamModelSyncUpstreamError("Muse model list response was not valid JSON", err)
	}
	if len(models) == 0 {
		return nil, resp.StatusCode, newUpstreamModelSyncUpstreamError("Muse upstream returned no models", nil)
	}
	return models, resp.StatusCode, nil
}

// syncMuseModelCatalog 是 muse 平台上游模型目录同步的实现入口（照 SyncVolcanoPlanModels
// 模式）：拉取 → 过滤 → 派生干净名 → 落库系统托管快照 + 自动建立 clean→contributor 的
// model_mapping。返回下游契约（干净名）组成的 UpstreamModelCatalog。
//
// 落库仅当账号已落库（account.ID>0 且 repo 可用）时发生；预览（无 ID）只返回分类结果，
// 不动库（与火山 preview 一致）。
func (s *AccountTestService) syncMuseModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error) {
	rawModels, status, fetchErr := s.fetchMuseUpstreamCatalog(ctx, account)

	prevFailures := 0
	if snap := loadMuseManagedModels(account); snap != nil {
		prevFailures = snap.ConsecutiveFailures
	}

	if fetchErr != nil {
		// 运行观测：连续失败次数 + 上游状态码（绝不泄露密钥）。
		newFailures := prevFailures + 1
		s.persistMuseSyncState(ctx, account, newFailures, nil)
		s.logMuseSyncFailure(account, status, fetchErr, newFailures)
		return nil, fetchErr
	}

	accepted, discarded := filterMuseUpstreamModels(rawModels)
	cleanNames := make([]string, 0, len(accepted))
	for _, m := range accepted {
		cleanNames = append(cleanNames, m.CleanName)
	}

	now := time.Now().UTC()
	// 运行观测：同步成功日志（上游状态码、过滤计数按原因分类、最后成功时间、连续失败清零）。
	s.logMuseSyncSuccess(account, status, cleanNames, discarded, &now)

	if account != nil && account.ID > 0 && s.accountRepo != nil {
		s.applyMuseModelMapping(ctx, account, accepted)
		// 系统托管快照入库（含连续失败清零与最后成功时间）。
		s.persistMuseManagedSnapshot(ctx, account, &MuseManagedModels{
			Managed:             accepted,
			SyncedAt:            now,
			LastSuccessAt:       &now,
			ConsecutiveFailures: 0,
		})
	}

	return &UpstreamModelCatalog{
		Models:   cleanNames,
		Metadata: make(map[string]UpstreamModelMetadata),
	}, nil
}

// applyMuseModelMapping 计算并落库 muse 干净名→contributor 映射。移除仅限本同步器曾
// 写入的身份键（clean==旧 contributor），人工 alias 永不删除（R3-2）。
func (s *AccountTestService) applyMuseModelMapping(ctx context.Context, account *Account, accepted []museManagedModel) {
	current := account.GetModelMapping()
	raw := make(map[string]any, len(current))
	for k, v := range current {
		raw[k] = v
	}

	newCleanSet := make(map[string]struct{}, len(accepted))
	for _, m := range accepted {
		newCleanSet[m.CleanName] = struct{}{}
	}

	oldManaged := loadMuseManagedModels(account)
	oldCleanToContributor := map[string]string{}
	if oldManaged != nil {
		for _, m := range oldManaged.Managed {
			oldCleanToContributor[m.CleanName] = m.ContributorID
		}
	}

	for _, m := range accepted {
		if existing, ok := raw[m.CleanName]; ok && existing == m.ContributorID {
			continue // 已是本同步器的身份键，无需重写
		}
		raw[m.CleanName] = m.ContributorID
	}

	// 仅移除本同步器曾写入、且上游已下线的干净名身份键，保护人工 alias。
	for clean, oldContributor := range oldCleanToContributor {
		if _, keep := newCleanSet[clean]; keep {
			continue
		}
		if cur, ok := raw[clean]; ok && cur == oldContributor {
			delete(raw, clean)
		}
	}

	account.Credentials = shallowCopyMap(account.Credentials)
	account.Credentials["model_mapping"] = raw
	if err := persistAccountCredentials(ctx, s.accountRepo, account, account.Credentials); err != nil {
		slog.Warn("muse_model_mapping_persist_failed", "account_id", museAccountID(account), "error", err)
	}
}

// persistMuseSyncState 在同步失败时只更新连续失败次数（保留既有托管集与最后成功时间）。
func (s *AccountTestService) persistMuseSyncState(ctx context.Context, account *Account, failures int, lastSuccess *time.Time) {
	if s.accountRepo == nil || account == nil || account.ID == 0 {
		return
	}
	snap := loadMuseManagedModels(account)
	if snap == nil {
		snap = &MuseManagedModels{}
	}
	snap.ConsecutiveFailures = failures
	if lastSuccess != nil {
		snap.LastSuccessAt = lastSuccess
	}
	s.persistMuseManagedSnapshot(ctx, account, snap)
}

// persistMuseManagedSnapshot 写入账号 extra 的 muse 系统托管模型快照。
func (s *AccountTestService) persistMuseManagedSnapshot(ctx context.Context, account *Account, snap *MuseManagedModels) {
	if s.accountRepo == nil || account == nil || account.ID == 0 {
		return
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra[museManagedModelExtraKey] = snap
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{museManagedModelExtraKey: snap}); err != nil {
		slog.Warn("muse_managed_snapshot_persist_failed", "account_id", museAccountID(account), "error", err)
	}
}

// loadMuseManagedModels 读取账号 extra 中的 muse 系统托管模型快照。
func loadMuseManagedModels(account *Account) *MuseManagedModels {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[museManagedModelExtraKey]
	if !ok || raw == nil {
		return nil
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var snap MuseManagedModels
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil
	}
	return &snap
}

// museAccountID 返回账号 ID（供日志，不泄露密钥）。
func museAccountID(account *Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID
}

// logMuseSyncSuccess 输出 muse 同步成功的结构化观测日志（不泄露密钥）。
func (s *AccountTestService) logMuseSyncSuccess(account *Account, status int, cleanNames []string, discarded []MuseModelFilterReason, lastSuccess *time.Time) {
	discardedByReason := map[string]int{}
	for _, d := range discarded {
		discardedByReason[d.Reason]++
	}
	museLogger.Info("muse_model_sync_success",
		"platform", PlatformMuse,
		"account_id", museAccountID(account),
		"sync_result", "success",
		"upstream_status", status,
		"accepted_count", len(cleanNames),
		"discarded_count", len(discarded),
		"discarded_free", discardedByReason["free_variant"],
		"discarded_prefix", discardedByReason["prefix"],
		"discarded_suffix", discardedByReason["suffix_not_contributor"],
		"last_success_at", lastSuccess,
		"consecutive_failures", 0,
	)
}

// logMuseSyncFailure 输出 muse 同步失败的结构化观测日志（不泄露密钥）。
func (s *AccountTestService) logMuseSyncFailure(account *Account, status int, syncErr error, consecutiveFailures int) {
	attrs := []any{
		"platform", PlatformMuse,
		"account_id", museAccountID(account),
		"sync_result", "failed",
		"upstream_status", status,
		"consecutive_failures", consecutiveFailures,
	}
	if syncErr != nil {
		attrs = append(attrs, "error", syncErr.Error())
	}
	museLogger.Warn("muse_model_sync_failed", attrs...)
}
