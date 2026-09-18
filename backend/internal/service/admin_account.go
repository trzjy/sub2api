package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// Account management implementations
func (s *adminServiceImpl) ListAccounts(ctx context.Context, page, pageSize int, platform, accountType, status, search string, groupID int64, privacyMode string, sortBy, sortOrder string) ([]Account, int64, error) {
	if groupID > 0 {
		if err := s.ValidateAccountGroupBindings(ctx, []int64{groupID}); err != nil {
			return nil, 0, err
		}
	}
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	accounts, result, err := s.accountRepo.ListWithFilters(ctx, params, platform, accountType, status, search, groupID, privacyMode)
	if err != nil {
		return nil, 0, err
	}
	return accounts, result.Total, nil
}

func (s *adminServiceImpl) ListAccountsForSchedulerScoreFilter(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	return s.accountRepo.ListAllWithFilters(ctx, platform, accountType, status, search, groupID, privacyMode)
}

func (s *adminServiceImpl) ListOpenAISchedulableAccountsForSchedulerScore(ctx context.Context, groupID *int64) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	if groupID != nil {
		return s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformOpenAI)
	}
	return s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, PlatformOpenAI)
}

func (s *adminServiceImpl) GetAccount(ctx context.Context, id int64) (*Account, error) {
	return s.accountRepo.GetByID(ctx, id)
}

func (s *adminServiceImpl) GetAccountsByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	if len(ids) == 0 {
		return []*Account{}, nil
	}

	accounts, err := s.accountRepo.GetByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("failed to get accounts by IDs: %w", err)
	}

	return accounts, nil
}

const maxAccountNameRunes = 100
const duplicateAccountOperationIDExtraKey = "duplicate_operation_id"

func duplicateAccountName(sourceName string) string {
	const suffix = " (Copy)"
	nameRunes := []rune(strings.TrimSpace(sourceName))
	maxBaseRunes := maxAccountNameRunes - len([]rune(suffix))
	if len(nameRunes) > maxBaseRunes {
		nameRunes = nameRunes[:maxBaseRunes]
	}
	return string(nameRunes) + suffix
}

func cloneAccountJSONMap(value map[string]any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	cloned := make(map[string]any, len(value))
	if err := json.Unmarshal(payload, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

var duplicateAccountDiscardedExtraKeys = map[string]struct{}{
	// A retry identity belongs to the operation that created one copy, not to later copies.
	duplicateAccountOperationIDExtraKey: {},
	// External sync identity belongs to one local account only.
	"crs_account_id": {},
	"crs_kind":       {},
	"crs_synced_at":  {},
	// Local quota usage and derived window timestamps must start fresh.
	"quota_used":            {},
	"quota_daily_used":      {},
	"quota_weekly_used":     {},
	"quota_daily_start":     {},
	"quota_weekly_start":    {},
	"quota_daily_reset_at":  {},
	"quota_weekly_reset_at": {},
	// Provider observations, capability probes, and transient scheduling state.
	"model_rate_limits":                      {},
	"session_window_utilization":             {},
	"passive_usage_7d_utilization":           {},
	"passive_usage_7d_reset":                 {},
	"passive_usage_7d_oi_utilization":        {},
	"passive_usage_7d_oi_reset":              {},
	"passive_usage_sampled_at":               {},
	"grok_usage_snapshot":                    {},
	"grok_billing_snapshot":                  {},
	"openai_responses_supported":             {},
	"openai_compact_supported":               {},
	"openai_compact_checked_at":              {},
	"openai_compact_last_status":             {},
	"openai_compact_last_error":              {},
	"antigravity_credits_overages":           {},
	"antigravity_force_token_refresh":        {},
	"antigravity_force_token_refresh_at":     {},
	"antigravity_force_token_refresh_reason": {},
	"drive_storage_limit":                    {},
	"drive_storage_usage":                    {},
	"drive_tier_updated_at":                  {},
	// Codex fingerprint convergence uses a per-account random seed, never copied from another account.
	codexFingerprintSeedExtraKey:           {},
	"codex_primary_used_percent":           {},
	"codex_primary_reset_after_seconds":    {},
	"codex_primary_window_minutes":         {},
	"codex_secondary_used_percent":         {},
	"codex_secondary_reset_after_seconds":  {},
	"codex_secondary_window_minutes":       {},
	"codex_primary_over_secondary_percent": {},
	"codex_usage_updated_at":               {},
	"codex_5h_used_percent":                {},
	"codex_5h_reset_after_seconds":         {},
	"codex_5h_window_minutes":              {},
	"codex_5h_reset_at":                    {},
	"codex_7d_used_percent":                {},
	"codex_7d_reset_after_seconds":         {},
	"codex_7d_window_minutes":              {},
	"codex_7d_reset_at":                    {},
}

func duplicateAccountExtra(value map[string]any) (map[string]any, error) {
	cloned, err := cloneAccountJSONMap(value)
	if err != nil {
		return nil, err
	}
	for key := range duplicateAccountDiscardedExtraKeys {
		delete(cloned, key)
	}
	return cloned, nil
}

func canDuplicateAccountType(accountType string) bool {
	switch accountType {
	case AccountTypeAPIKey, AccountTypeUpstream, AccountTypeBedrock, AccountTypeServiceAccount:
		return true
	default:
		return false
	}
}

func duplicateAccountGroups(source *Account) ([]AccountGroup, []int64) {
	if len(source.AccountGroups) > 0 {
		groups := make([]AccountGroup, 0, len(source.AccountGroups))
		groupIDs := make([]int64, 0, len(source.AccountGroups))
		for _, sourceGroup := range source.AccountGroups {
			groups = append(groups, AccountGroup{GroupID: sourceGroup.GroupID, Priority: sourceGroup.Priority})
			groupIDs = append(groupIDs, sourceGroup.GroupID)
		}
		return groups, groupIDs
	}

	groups := make([]AccountGroup, 0, len(source.GroupIDs))
	groupIDs := append([]int64(nil), source.GroupIDs...)
	for i, groupID := range groupIDs {
		groups = append(groups, AccountGroup{GroupID: groupID, Priority: i + 1})
	}
	return groups, groupIDs
}

func duplicateAccountOperationID(sourceID int64, actorScope, operationKey string) string {
	operationKey = strings.TrimSpace(operationKey)
	if operationKey == "" {
		return ""
	}
	actorScope = strings.TrimSpace(actorScope)
	if actorScope == "" {
		actorScope = "admin:0"
	}
	payload := "admin.accounts.duplicate\x00" + actorScope + "\x00" + strconv.FormatInt(sourceID, 10) + "\x00" + operationKey
	digest := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", digest)
}

func (s *adminServiceImpl) findDuplicateByOperationID(ctx context.Context, operationID string) (*Account, error) {
	if operationID == "" {
		return nil, nil
	}
	accounts, err := s.accountRepo.FindByExtraField(ctx, duplicateAccountOperationIDExtraKey, operationID)
	if err != nil {
		return nil, fmt.Errorf("find duplicate account operation: %w", err)
	}
	if len(accounts) == 0 {
		return nil, nil
	}
	account := accounts[0]
	return &account, nil
}

// RecoverDuplicateAccount performs a read-only lookup for an already committed duplicate.
// It is used when the idempotency coordinator cannot confirm whether response persistence
// succeeded, and deliberately never repeats the create side effect.
func (s *adminServiceImpl) RecoverDuplicateAccount(ctx context.Context, id int64, actorScope, operationKey string) (*Account, error) {
	return s.findDuplicateByOperationID(ctx, duplicateAccountOperationID(id, actorScope, operationKey))
}

func cloneAccountValuePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// DuplicateAccount creates a paused account from source configuration without carrying first-class
// runtime state. Credentials and extra configuration are deep-copied so normalization of the new
// account cannot mutate the in-memory source. Linked credential shadows are excluded because they
// intentionally do not own credentials and must be created through CreateShadow.
func (s *adminServiceImpl) DuplicateAccount(ctx context.Context, id int64, actorScope, operationKey string) (*Account, error) {
	operationID := duplicateAccountOperationID(id, actorScope, operationKey)
	existing, err := s.RecoverDuplicateAccount(ctx, id, actorScope, operationKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	source, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if source.IsCredentialShadow() {
		return nil, infraerrors.BadRequest(
			"ACCOUNT_DUPLICATE_SHADOW_UNSUPPORTED",
			"linked credential shadow accounts cannot be duplicated; duplicate the parent account instead",
		)
	}
	if !canDuplicateAccountType(source.Type) {
		return nil, infraerrors.BadRequest(
			"ACCOUNT_DUPLICATE_CREDENTIAL_TYPE_UNSUPPORTED",
			"accounts with rotating or unsupported credential types cannot be duplicated",
		)
	}

	credentials, err := cloneAccountJSONMap(source.Credentials)
	if err != nil {
		return nil, fmt.Errorf("clone account credentials: %w", err)
	}
	extra, err := duplicateAccountExtra(source.Extra)
	if err != nil {
		return nil, fmt.Errorf("clone account extra configuration: %w", err)
	}
	if operationID != "" {
		if extra == nil {
			extra = make(map[string]any, 1)
		}
		extra[duplicateAccountOperationIDExtraKey] = operationID
	}

	var expiresAt *int64
	if source.ExpiresAt != nil {
		unix := source.ExpiresAt.Unix()
		expiresAt = &unix
	}
	autoPauseOnExpired := source.AutoPauseOnExpired
	groups, groupIDs := duplicateAccountGroups(source)
	if err := s.ValidateAccountGroupBindings(ctx, groupIDs); err != nil {
		return nil, err
	}
	proxyID := source.ProxyID
	if source.ProxyFallbackOriginID != nil {
		// Proxy fallback is transient runtime state; duplicate the configured origin.
		proxyID = source.ProxyFallbackOriginID
	}
	input := &CreateAccountInput{
		Name:                  duplicateAccountName(source.Name),
		Notes:                 cloneAccountValuePointer(source.Notes),
		Platform:              source.Platform,
		Type:                  source.Type,
		Credentials:           credentials,
		Extra:                 extra,
		ProxyID:               cloneAccountValuePointer(proxyID),
		Concurrency:           source.Concurrency,
		Priority:              source.Priority,
		RateMultiplier:        cloneAccountValuePointer(source.RateMultiplier),
		LoadFactor:            cloneAccountValuePointer(source.LoadFactor),
		GroupIDs:              groupIDs,
		ExpiresAt:             expiresAt,
		AutoPauseOnExpired:    &autoPauseOnExpired,
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
	}
	accountExtra, err := normalizeOpenAILongContextBillingExtra(input.Platform, input.Extra)
	if err != nil {
		return nil, fmt.Errorf("normalize duplicate account extra: %w", err)
	}
	if err := NormalizeHeaderOverrideCredentials(input.Credentials); err != nil {
		return nil, err
	}
	duplicate, err := buildAccountForCreate(input, accountExtra)
	if err != nil {
		return nil, err
	}
	// A copied credential must be reviewed before it can share live traffic with its source.
	duplicate.Schedulable = false
	if s.accountDuplicateRepo == nil {
		return nil, errors.New("account duplicate repository is not configured")
	}
	if err := s.accountDuplicateRepo.CreateWithAccountGroups(ctx, duplicate, groups); err != nil {
		return nil, fmt.Errorf("create duplicate account: %w", err)
	}
	for i := range groups {
		groups[i].AccountID = duplicate.ID
	}
	duplicate.AccountGroups = groups
	duplicate.GroupIDs = groupIDs
	return duplicate, nil
}

func normalizeAccountConcurrency(platform, accountType string, concurrency int) int {
	if platform == PlatformGrok && accountType == AccountTypeOAuth {
		if concurrency <= 0 {
			return 1
		}
	}
	return concurrency
}

// ValidateOpenAILongContextBillingExtra validates the OpenAI account billing flag when present.
func ValidateOpenAILongContextBillingExtra(platform string, extra map[string]any) error {
	if platform != PlatformOpenAI {
		return nil
	}
	raw, exists := extra[openAILongContextBillingEnabledKey]
	if !exists {
		return nil
	}
	if _, ok := raw.(bool); !ok {
		return infraerrors.BadRequest(
			"OPENAI_LONG_CONTEXT_BILLING_INVALID",
			"openai_long_context_billing_enabled must be a boolean",
		)
	}
	return nil
}

func normalizeOpenAILongContextBillingExtra(platform string, extra map[string]any) (map[string]any, error) {
	if platform != PlatformOpenAI {
		return extra, nil
	}
	if err := ValidateOpenAILongContextBillingExtra(platform, extra); err != nil {
		return nil, err
	}

	normalized := maps.Clone(extra)
	if normalized == nil {
		normalized = make(map[string]any, 1)
	}
	_, exists := normalized[openAILongContextBillingEnabledKey]
	if !exists {
		normalized[openAILongContextBillingEnabledKey] = false
	}
	return normalized, nil
}

func normalizeOpenAILongContextBillingUpdateExtra(account *Account, input *UpdateAccountInput) (map[string]any, error) {
	normalized, err := normalizeOpenAILongContextBillingExtra(account.Platform, input.Extra)
	if err != nil || account.Platform != PlatformOpenAI {
		return normalized, err
	}

	_, provided := input.Extra[openAILongContextBillingEnabledKey]
	current, hasCurrent := account.Extra[openAILongContextBillingEnabledKey].(bool)
	if !provided {
		if hasCurrent {
			normalized[openAILongContextBillingEnabledKey] = current
		}
	}
	return normalized, nil
}

// Grok media eligibility helpers live in account_grok_media_eligibility.go.

// validateOtherAccountCredential 校验通用 other 平台账号：仅接受 API-Key 类型，且必须提供
// 自定义 base_url（other 无内置上游默认值；空 base_url 不得落到任何官方 OpenAI 端点）。
func validateOtherAccountCredential(platform, accountType string, credentials map[string]any) error {
	if platform != PlatformOther {
		return nil
	}
	if accountType != AccountTypeAPIKey {
		return fmt.Errorf("platform %s only supports apikey accounts", platform)
	}
	raw, ok := credentials["base_url"]
	if !ok {
		return fmt.Errorf("platform %s requires a custom base_url", platform)
	}
	baseURL, _ := raw.(string)
	if strings.TrimSpace(baseURL) == "" {
		return fmt.Errorf("platform %s requires a custom base_url", platform)
	}
	return nil
}

// accessModeFromCredentials 从 credentials 映射提取并归一 access_mode 显式值
// （credentials["access_mode"]）。用于无 Account 对象、仅持 credentials 的上下文
// （凭证清洗 / 写入校验 / 模式切换重验），与 Account.GetAccessMode 协同。
func accessModeFromCredentials(creds map[string]any) string {
	if creds == nil {
		return ""
	}
	mode, _ := creds["access_mode"].(string)
	return strings.TrimSpace(mode)
}

// WebModelCatalogPlatform 报告平台值是否拥有网页接入（access_mode=web）模型目录：
// 平台归并后（PR-4）网页接入挂在官方平台 zhipu/deepseek/kimi 上，目录键即为官方
// 平台值本身（DefaultWebModelIDs / ValidateWebBaseURL 均按官方平台键组织）。
// 支持网页登录的官方平台返回平台值本身，其余返回空串。供 admin / 网关 GetModels
// 与 web 探活链复用。
func WebModelCatalogPlatform(platform string) string {
	if IsWebLoginPlatform(platform) {
		return platform
	}
	return ""
}

// ResolveWebPlatform 报告账号是否应走网页接入目录/探活链，命中时返回平台值本身：
// 官方平台（zhipu/deepseek/kimi）+ access_mode=web 的账号返回其官方平台值，
// 非 web 接入模式返回空串（调用方按各自语义回落）。
func ResolveWebPlatform(account *Account) string {
	if account == nil {
		return ""
	}
	if account.IsWebAccessMode() && IsWebLoginPlatform(account.Platform) {
		return account.Platform
	}
	return ""
}

// apiKeyOf 从 credentials 提取并归一 api_key 值。
func apiKeyOf(credentials map[string]any) string {
	if credentials == nil {
		return ""
	}
	v, _ := credentials["api_key"].(string)
	return strings.TrimSpace(v)
}

// validateAccessModeCredential 按账号目标接入模式（access_mode）重验凭证：
// web 模式要求 cookie（zhipu/deepseek）或 access_token（kimi）非空；api 模式要求
// api_key 非空。缺失即拒绝（防切换后账号不可用，方案 §5.1 混合凭证规则 / §8 风险）。
// 凭证形状隐式归属的兼容期已关闭（2026-09-19 用户裁定：不保留兼容）：web 登录平台的 apikey 账号
// 携带网页登录形状凭证（cookie/access_token）但缺显式 access_mode 时 fail-closed
// 拒绝——网页登录态必须显式声明 credentials.access_mode，不得按凭证形状隐式归属
// （否则 SanitizeStoredCredentials 会把 cookie 当 API 账号残留剥离，账号静默不可用）。
// api 形状凭证仍经 Account.GetAccessMode 默认 "api"，不强制显式（与既有官方平台
// API 账号语义一致）。
func validateAccessModeCredential(platform, accountType string, credentials map[string]any) error {
	mode := accessModeFromCredentials(credentials)
	switch mode {
	case AccountAccessModeWeb:
		return validateWebAccountCredential(platform, accountType, credentials)
	case AccountAccessModeAPI:
		if apiKeyOf(credentials) == "" {
			return fmt.Errorf("access_mode=api requires a non-empty api_key")
		}
	case "":
		if IsWebLoginPlatform(platform) && accountType == AccountTypeAPIKey && hasWebLoginShapeCredential(platform, credentials) {
			return fmt.Errorf(
				"platform %s web login credentials (cookie/access_token) require an explicit credentials.access_mode (%q or %q)",
				platform, AccountAccessModeWeb, AccountAccessModeAPI)
		}
	}
	return nil
}

// hasWebLoginShapeCredential 报告 credentials 是否携带网页登录形状凭证：
// zhipu/deepseek 的非空 cookie、kimi 的非空 access_token（与
// validateWebAccountCredential 的网页供应商家族判定同口径）。
func hasWebLoginShapeCredential(platform string, credentials map[string]any) bool {
	if credentials == nil {
		return false
	}
	key := "cookie"
	if platform == PlatformKimi {
		key = "access_token"
	}
	raw, _ := credentials[key].(string)
	return strings.TrimSpace(raw) != ""
}

// validateWebAccountCredential 校验网页接入账号（官方平台 zhipu/deepseek/kimi +
// credentials["access_mode"]="web"，平台归并后唯一形态）：
// 仅接受 apikey 类型（静态登录态凭证）；DeepSeek/Zhipu 须提供非空整串 Cookie，
// Kimi 须提供非空 access_token（refresh_token/user_id 可选）；base_url 为可选的
// 官方域名覆盖。字段口径见 docs/web-reverse-embedded-login-plan.md §3.2。
// 仅做准入校验，不含任何上游请求逻辑（适配器见 W2-W4）。
// access_mode 非法显式值（非 api/web/空）在此拒绝持久化。
func validateWebAccountCredential(platform, accountType string, credentials map[string]any) error {
	mode := accessModeFromCredentials(credentials)
	// 接入模式写入侧校验：非法显式值（非 api/web/空）拒绝持久化。
	if mode != "" && mode != AccountAccessModeAPI && mode != AccountAccessModeWeb {
		return fmt.Errorf("access_mode %q is invalid: must be %q or %q", mode, AccountAccessModeAPI, AccountAccessModeWeb)
	}
	// web 接入模式：仅按 credentials["access_mode"]="web" 判定（PR-4 旧链归零，
	// 方案 §5.5：平台归并后网页接入由账号级 access_mode 唯一承载）。
	isWeb := mode == AccountAccessModeWeb
	if !isWeb {
		return nil
	}
	if accountType != AccountTypeAPIKey {
		return fmt.Errorf("web access mode only supports apikey accounts")
	}
	// #4：base_url 为可选的官方域名覆盖。
	// 键存在但类型非 string（数字 / 数组 / 对象）→ fail-closed 拒绝，不再静默跳过保存脏数据；
	// 键不存在或空串 → 回落平台默认，保持现状。
	if rawBaseURL, ok := credentials["base_url"]; ok {
		baseURL, isString := rawBaseURL.(string)
		if !isString {
			return fmt.Errorf("platform %s base_url must be a string", platform)
		}
		if strings.TrimSpace(baseURL) != "" {
			// ValidateWebBaseURL 后缀表按官方平台键组织（PR-4 旧链归零），直接以平台值校验。
			if _, err := ValidateWebBaseURL(platform, strings.TrimSpace(baseURL)); err != nil {
				return fmt.Errorf("platform %s base_url invalid: %w", platform, err)
			}
		}
	}
	// 判定网页供应商家族：kimi 用 access_token，其余（zhipu/deepseek）用整串 cookie。
	isKimi := platform == PlatformKimi && mode == AccountAccessModeWeb
	if isKimi {
		raw, _ := credentials["access_token"].(string)
		if strings.TrimSpace(raw) == "" {
			return fmt.Errorf("platform %s requires a non-empty access_token", platform)
		}
		return nil
	}
	raw, _ := credentials["cookie"].(string)
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("platform %s requires a non-empty cookie", platform)
	}
	return nil
}

func buildAccountForCreate(input *CreateAccountInput, accountExtra map[string]any) (*Account, error) {
	if err := validateOtherAccountCredential(input.Platform, input.Type, input.Credentials); err != nil {
		return nil, err
	}
	if err := validateWebAccountCredential(input.Platform, input.Type, input.Credentials); err != nil {
		return nil, err
	}
	// 凭证形状隐式归属的兼容期已关闭（2026-09-19 用户裁定）：web 登录形状凭证缺显式 access_mode
	// 即拒绝建号（复制账号/导入等旁路统一收敛到本校验，与创建主路径同口径）。
	if err := validateAccessModeCredential(input.Platform, input.Type, input.Credentials); err != nil {
		return nil, err
	}

	// Probe/session state is system-managed. New accounts always start with automatic refresh disabled.
	delete(accountExtra, UpstreamBillingProbeEnabledExtraKey)
	delete(accountExtra, UpstreamBillingRateSyncEnabledExtraKey)
	delete(accountExtra, UpstreamBillingProbeExtraKey)
	delete(accountExtra, OllamaCloudUsageSessionExtraKey)
	delete(accountExtra, OllamaCloudUsageAutoRefreshExtraKey)
	delete(accountExtra, OllamaCloudUsageSnapshotExtraKey)
	accountExtra = prepareCodexFingerprintExtraForCreate(input.Platform, input.Type, accountExtra)
	account := &Account{
		Name:        input.Name,
		Notes:       normalizeAccountNotes(input.Notes),
		Platform:    input.Platform,
		Type:        input.Type,
		Credentials: input.Credentials,
		Extra:       accountExtra,
		ProxyID:     input.ProxyID,
		Concurrency: normalizeAccountConcurrency(input.Platform, input.Type, input.Concurrency),
		Priority:    input.Priority,
		Status:      StatusActive,
		Schedulable: true,
	}
	if input.ProbeEnabled != nil && *input.ProbeEnabled {
		if !isUpstreamBillingProbeAccount(account) {
			return nil, ErrUpstreamBillingProbeAccountInvalid
		}
		if account.Extra == nil {
			account.Extra = make(map[string]any)
		}
		account.Extra[UpstreamBillingProbeEnabledExtraKey] = true
	}
	// 预计算固定时间重置的下次重置时间
	if account.Extra != nil {
		if err := ValidateQuotaResetConfig(account.Extra); err != nil {
			return nil, err
		}
		ComputeQuotaResetAt(account.Extra)
		NormalizeFixedQuotaWindows(account.Extra)
	}
	if input.ExpiresAt != nil && *input.ExpiresAt > 0 {
		expiresAt := time.Unix(*input.ExpiresAt, 0)
		account.ExpiresAt = &expiresAt
	}
	if input.AutoPauseOnExpired != nil {
		account.AutoPauseOnExpired = *input.AutoPauseOnExpired
	} else {
		account.AutoPauseOnExpired = true
	}
	if input.RateMultiplier != nil {
		if *input.RateMultiplier < 0 {
			return nil, errors.New("rate_multiplier must be >= 0")
		}
		account.RateMultiplier = input.RateMultiplier
	}
	if input.LoadFactor != nil && *input.LoadFactor > 0 {
		if *input.LoadFactor > 10000 {
			return nil, errors.New("load_factor must be <= 10000")
		}
		account.LoadFactor = input.LoadFactor
	}
	return account, nil
}

func (s *adminServiceImpl) CreateAccount(ctx context.Context, input *CreateAccountInput) (*Account, error) {
	accountExtra, err := normalizeOpenAILongContextBillingExtra(input.Platform, input.Extra)
	if err != nil {
		return nil, err
	}
	accountExtra, err = normalizeGrokMediaEligibilityExtra(input.Platform, accountExtra)
	if err != nil {
		return nil, err
	}
	accountExtra, err = normalizeOpenAIAutoResetCreditExtra(input.Platform, input.Type, false, accountExtra)
	if err != nil {
		return nil, err
	}
	if err := ValidateUpstreamRequestIDHeaderExtra(accountExtra); err != nil {
		return nil, err
	}

	// 绑定分组
	groupIDs := input.GroupIDs
	// 如果没有指定分组,自动绑定对应平台的默认分组
	if len(groupIDs) == 0 && !input.SkipDefaultGroupBind {
		defaultGroupName := input.Platform + "-default"
		groups, err := s.groupRepo.ListActiveByPlatform(ctx, input.Platform)
		if err == nil {
			for _, g := range groups {
				if g.Name == defaultGroupName {
					groupIDs = []int64{g.ID}
					break
				}
			}
		}
	}

	// 检查混合渠道风险（除非用户已确认）
	if len(groupIDs) > 0 && !input.SkipMixedChannelCheck {
		if err := s.checkMixedChannelRisk(ctx, 0, input.Platform, groupIDs); err != nil {
			return nil, err
		}
	}

	// 校验并规范化请求头覆写配置（header 名小写化、格式检查）
	if err := NormalizeHeaderOverrideCredentials(input.Credentials); err != nil {
		return nil, err
	}
	// 凭证形状隐式归属的兼容期已关闭（2026-09-19 用户裁定）：必须在脱敏前完成 web 登录形状判定——
	// SanitizeStoredCredentials 会剥离非 web 账号的 cookie，脱敏后再校验将失去判定依据，
	// web 形状凭证会被静默剥离登录态后当作 API 账号落库。
	if err := validateAccessModeCredential(input.Platform, input.Type, input.Credentials); err != nil {
		return nil, err
	}
	// Never persist ephemeral SSO/password secrets after OAuth conversion.
	input.Credentials = SanitizeStoredCredentials(input.Platform, input.Credentials)

	account, err := buildAccountForCreate(input, accountExtra)
	if err != nil {
		return nil, err
	}
	if err := s.ValidateAccountGroupBindings(ctx, groupIDs); err != nil {
		return nil, err
	}
	if err := s.accountRepo.Create(ctx, account); err != nil {
		return nil, err
	}

	// 绑定分组
	if len(groupIDs) > 0 {
		if err := s.accountRepo.BindGroups(ctx, account.ID, groupIDs); err != nil {
			return nil, err
		}
	}

	// OAuth 账号：创建后异步设置隐私。
	// 使用 Ensure（幂等）而非 Force：新建账号 Extra 为空时效果相同，但更安全。
	if account.Type == AccountTypeOAuth {
		switch account.Platform {
		case PlatformOpenAI:
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("create_account_openai_privacy_panic", "account_id", account.ID, "recover", r)
					}
				}()
				s.EnsureOpenAIPrivacy(context.Background(), account)
			}()
		case PlatformAntigravity:
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("create_account_antigravity_privacy_panic", "account_id", account.ID, "recover", r)
					}
				}()
				s.EnsureAntigravityPrivacy(context.Background(), account)
			}()
		}
	}

	return account, nil
}

func (s *adminServiceImpl) UpdateAccount(ctx context.Context, id int64, input *UpdateAccountInput) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// 更新路径同样守住 other 不变量（创建校验可被 edit/导入/直写绕过）。
	if account.Platform == PlatformOther {
		effectiveType := account.Type
		if input.Type != "" {
			effectiveType = input.Type
		}
		if effectiveType != AccountTypeAPIKey {
			return nil, infraerrors.New(http.StatusBadRequest, "OTHER_ACCOUNT_TYPE_INVALID",
				"platform other only supports apikey accounts")
		}
		if input.Credentials != nil {
			raw, ok := input.Credentials["base_url"]
			if !ok {
				return nil, infraerrors.New(http.StatusBadRequest, "OTHER_ACCOUNT_REQUIRES_BASE_URL",
					"platform other requires a custom base_url")
			}
			baseURL, _ := raw.(string)
			if strings.TrimSpace(baseURL) == "" {
				return nil, infraerrors.New(http.StatusBadRequest, "OTHER_ACCOUNT_REQUIRES_BASE_URL",
					"platform other requires a custom base_url")
			}
		}
	}
	// 更新路径同样守住网页逆向平台 / web 接入模式不变量（创建校验可被 edit/导入/直写绕过）。
	// 平台归并后 web 账号 platform 已是官方值，按账号接入模式判定（§5.5）。
	isWebAccount := account.IsWebAccessMode()
	if isWebAccount {
		effectiveType := account.Type
		if input.Type != "" {
			effectiveType = input.Type
		}
		if effectiveType != AccountTypeAPIKey {
			return nil, infraerrors.New(http.StatusBadRequest, "WEB_ACCOUNT_TYPE_INVALID",
				fmt.Sprintf("platform %s only supports apikey accounts", account.Platform))
		}
		// input.Credentials == nil 表示本轮不修改凭证，不参与校验。
		if input.Credentials != nil {
			if err := validateWebAccountCredential(account.Platform, effectiveType, input.Credentials); err != nil {
				return nil, infraerrors.New(http.StatusBadRequest, "WEB_ACCOUNT_CREDENTIAL_INVALID", err.Error())
			}
		}
	}
	var normalizedExtra map[string]any
	if input.Extra != nil {
		normalizedExtra, err = normalizeOpenAILongContextBillingUpdateExtra(account, input)
		if err != nil {
			return nil, err
		}
		normalizedExtra, err = normalizeGrokMediaEligibilityUpdateExtra(account, input, normalizedExtra)
		if err != nil {
			return nil, err
		}
		effectiveType := account.Type
		if input.Type != "" {
			effectiveType = input.Type
		}
		normalizedExtra, err = normalizeOpenAIAutoResetCreditExtra(account.Platform, effectiveType, account.IsShadow(), normalizedExtra)
		if err != nil {
			return nil, err
		}
		if err := ValidateUpstreamRequestIDHeaderExtra(normalizedExtra); err != nil {
			return nil, err
		}
	}
	previousProbeIdentity := upstreamBillingProbeIdentity(account)
	previousOllamaUsageIdentity := ollamaCloudUsageIdentity(account)
	// 安全/身份不变量(影子账号):通用更新路径被 edit/re-auth/refresh/batch 共用,
	// 必须在此守住,否则仅在创建时的保证可被这些路径绕过。
	if account.IsCredentialShadow() {
		// 影子绝不持有凭据(凭据只在母账号)——外审 F5。
		if !isAllowedShadowCredentialsUpdate(input.Credentials) {
			return nil, infraerrors.Newf(http.StatusBadRequest, "SHADOW_NO_CREDENTIALS",
				"shadow accounts do not hold auth credentials; only model mapping can be configured on the shadow account")
		}
		// 影子 type 不可变——很多上游逻辑按 account.Type 分支(OAuth transform / ChatGPT
		// header 注入 / WS OAuth 决策),改成 apikey 会让影子被选中后按错误协议转发(外审 G7)。
		if input.Type != "" && input.Type != account.Type {
			return nil, infraerrors.Newf(http.StatusBadRequest, "SHADOW_IMMUTABLE_TYPE",
				"shadow account type cannot be changed; it must remain an OAuth shadow")
		}
	} else if input.Type != "" && input.Type != account.Type && input.Type != AccountTypeOAuth {
		// 母账号守卫(外审 D/P1):有影子的账号不能把 type 改出 OAuth——影子读透母
		// 凭据,母变成 apikey/setup_token 会让影子被调度后按错协议失败(resolveCredentialAccount
		// 必报错)。须先删影子再改 type。
		shadows, serr := s.accountRepo.ListShadowsByParent(ctx, id)
		if serr != nil {
			return nil, serr
		}
		if len(shadows) > 0 {
			return nil, infraerrors.New(http.StatusBadRequest, "SHADOW_PARENT_IMMUTABLE_TYPE",
				"cannot change account type while it has a shadow; delete the shadow first")
		}
	}
	wasOveragesEnabled := account.IsOveragesEnabled()

	if input.Name != "" {
		account.Name = input.Name
	}
	if input.Type != "" {
		account.Type = input.Type
	}
	if input.Notes != nil {
		account.Notes = normalizeAccountNotes(input.Notes)
	}
	if account.IsCredentialShadow() && input.Credentials != nil {
		account.Credentials = sanitizeShadowCredentials(input.Credentials)
	} else if len(input.Credentials) > 0 {
		// 敏感子键采用"incoming 没提供就保留"的合并语义：前端响应已脱敏，
		// 全对象 PUT 编辑时不会再带回 token，避免覆盖时清空已有凭证。
		account.Credentials = MergePreservingSensitiveCreds(account.Credentials, input.Credentials)
		// 校验并规范化请求头覆写配置（header 名小写化、格式检查）
		if err := NormalizeHeaderOverrideCredentials(account.Credentials); err != nil {
			return nil, err
		}
		// 接入模式切换重验（方案 §5.1 混合凭证规则 / §8 风险）：本轮携带凭证时，按目标
		// 接入模式重验凭证（web 要求 cookie/access_token 非空、api 要求 api_key 非空），
		// 缺失即拒绝，防切换后账号不可用。凭证形状隐式归属的兼容期已关闭（2026-09-19 用户裁定）：
		// web 登录形状凭证缺显式 access_mode 在此 fail-closed 拒绝；必须在脱敏前判定，
		// 否则非 web 账号的 cookie 被 SanitizeStoredCredentials 剥离后失去判定依据。
		if err := validateAccessModeCredential(account.Platform, account.Type, account.Credentials); err != nil {
			return nil, infraerrors.New(http.StatusBadRequest, "ACCESS_MODE_CREDENTIAL_INVALID", err.Error())
		}
		// Strip SSO/password residue that must never sit next to OAuth tokens.
		account.Credentials = SanitizeStoredCredentials(account.Platform, account.Credentials)
	}
	// Extra 使用 map：需要区分“未提供(nil)”与“显式清空({})”。
	// 关闭配额限制时前端会删除 quota_* 键并提交 extra:{}，此时也必须落库。
	requestedProbeEnabledUpdate := input.ProbeEnabled
	requestedRateSyncEnabledUpdate := input.RateSyncEnabled
	if input.Extra != nil {
		requestedProbeEnabled, hasRequestedProbeEnabled := normalizedExtra[UpstreamBillingProbeEnabledExtraKey]
		if hasRequestedProbeEnabled {
			enabled, ok := requestedProbeEnabled.(bool)
			if !ok {
				return nil, infraerrors.BadRequest("INVALID_UPSTREAM_BILLING_PROBE_ENABLED", "upstream_billing_probe_enabled must be a boolean")
			}
			if requestedProbeEnabledUpdate != nil && *requestedProbeEnabledUpdate != enabled {
				return nil, infraerrors.BadRequest("CONFLICTING_UPSTREAM_BILLING_PROBE_ENABLED", "conflicting upstream_billing_probe_enabled values")
			}
			requestedProbeEnabledUpdate = &enabled
		}
		delete(normalizedExtra, UpstreamBillingProbeEnabledExtraKey)
		delete(normalizedExtra, UpstreamBillingRateSyncEnabledExtraKey)
		delete(normalizedExtra, UpstreamBillingProbeExtraKey)
		delete(normalizedExtra, OllamaCloudUsageSessionExtraKey)
		delete(normalizedExtra, OllamaCloudUsageAutoRefreshExtraKey)
		delete(normalizedExtra, OllamaCloudUsageSnapshotExtraKey)
		// 保留配额用量和专用服务受管字段，防止普通账号编辑意外覆盖。
		for _, key := range []string{
			"quota_used",
			"quota_daily_used",
			"quota_daily_start",
			"quota_weekly_used",
			"quota_weekly_start",
			grokBillingExtraKey,
			UpstreamBillingProbeEnabledExtraKey,
			UpstreamBillingRateSyncEnabledExtraKey,
			UpstreamBillingProbeExtraKey,
			OllamaCloudUsageSessionExtraKey,
			OllamaCloudUsageAutoRefreshExtraKey,
			OllamaCloudUsageSnapshotExtraKey,
			OpenAIAutoResetCreditStateExtraKey,
		} {
			if v, ok := account.Extra[key]; ok {
				normalizedExtra[key] = v
			}
		}
		normalizedExtra = prepareCodexFingerprintExtraForUpdate(account, normalizedExtra)
		account.Extra = normalizedExtra
		if account.Platform == PlatformAntigravity && wasOveragesEnabled && !account.IsOveragesEnabled() {
			delete(account.Extra, "antigravity_credits_overages") // 清理旧版 overages 运行态
			// 清除 AICredits 限流 key
			if rawLimits, ok := account.Extra[modelRateLimitsKey].(map[string]any); ok {
				delete(rawLimits, creditsExhaustedKey)
			}
		}
		if account.Platform == PlatformAntigravity && !wasOveragesEnabled && account.IsOveragesEnabled() {
			delete(account.Extra, modelRateLimitsKey)
			delete(account.Extra, "antigravity_credits_overages") // 清理旧版 overages 运行态
		}
		// 校验并预计算固定时间重置的下次重置时间
		if err := ValidateQuotaResetConfig(account.Extra); err != nil {
			return nil, err
		}
		ComputeQuotaResetAt(account.Extra)
		NormalizeFixedQuotaWindows(account.Extra)
	}
	if input.Extra == nil {
		account.Extra = prepareCodexFingerprintExtraForUpdate(account, account.Extra)
	}
	if requestedRateSyncEnabledUpdate != nil && *requestedRateSyncEnabledUpdate {
		if requestedProbeEnabledUpdate != nil && !*requestedProbeEnabledUpdate {
			return nil, infraerrors.BadRequest(
				"UPSTREAM_BILLING_RATE_SYNC_REQUIRES_PROBE",
				"upstream billing rate sync requires upstream billing probe",
			)
		}
		enabled := true
		requestedProbeEnabledUpdate = &enabled
	}
	if requestedProbeEnabledUpdate != nil && !*requestedProbeEnabledUpdate {
		disabled := false
		requestedRateSyncEnabledUpdate = &disabled
	}
	if (requestedProbeEnabledUpdate != nil && *requestedProbeEnabledUpdate) ||
		(requestedRateSyncEnabledUpdate != nil && *requestedRateSyncEnabledUpdate) {
		if !isUpstreamBillingProbeAccount(account) {
			return nil, ErrUpstreamBillingProbeAccountInvalid
		}
	}
	if account.Extra == nil && (requestedProbeEnabledUpdate != nil || requestedRateSyncEnabledUpdate != nil) {
		account.Extra = make(map[string]any)
	}
	if requestedProbeEnabledUpdate != nil {
		account.Extra[UpstreamBillingProbeEnabledExtraKey] = *requestedProbeEnabledUpdate
	}
	if requestedRateSyncEnabledUpdate != nil {
		account.Extra[UpstreamBillingRateSyncEnabledExtraKey] = *requestedRateSyncEnabledUpdate
	}
	// 影子代理恒继承母账号(由 propagateProxyToShadows 同步),不接受独立编辑——外审 B/P1;
	// 否则要等母账号下次改 proxy 才被覆盖,期间影子会出现"有时继承、有时独立"的漂移。
	if input.ProxyID != nil && !account.IsCredentialShadow() {
		// 0 表示清除代理（前端发送 0 而不是 null 来表达清除意图）
		if *input.ProxyID == 0 {
			account.ProxyID = nil
		} else {
			account.ProxyID = input.ProxyID
		}
		account.Proxy = nil // 清除关联对象，防止 GORM Save 时根据 Proxy.ID 覆盖 ProxyID
	}
	if !reflect.DeepEqual(previousProbeIdentity, upstreamBillingProbeIdentity(account)) && account.Extra != nil {
		delete(account.Extra, UpstreamBillingProbeExtraKey)
		if !isUpstreamBillingProbeAccount(account) {
			delete(account.Extra, UpstreamBillingProbeEnabledExtraKey)
			delete(account.Extra, UpstreamBillingRateSyncEnabledExtraKey)
		}
	}
	if account.Extra != nil {
		if !IsOllamaCloudUsageAccount(account) {
			delete(account.Extra, OllamaCloudUsageSessionExtraKey)
			delete(account.Extra, OllamaCloudUsageAutoRefreshExtraKey)
			delete(account.Extra, OllamaCloudUsageSnapshotExtraKey)
		} else if !reflect.DeepEqual(previousOllamaUsageIdentity, ollamaCloudUsageIdentity(account)) {
			delete(account.Extra, OllamaCloudUsageSessionExtraKey)
			delete(account.Extra, OllamaCloudUsageAutoRefreshExtraKey)
			delete(account.Extra, OllamaCloudUsageSnapshotExtraKey)
		}
	}
	// 只在指针非 nil 时更新 Concurrency（支持设置为 0）
	if input.Concurrency != nil {
		account.Concurrency = normalizeAccountConcurrency(account.Platform, account.Type, *input.Concurrency)
	}
	// 只在指针非 nil 时更新 Priority（支持设置为 0）
	if input.Priority != nil {
		account.Priority = *input.Priority
	}
	if input.RateMultiplier != nil {
		if *input.RateMultiplier < 0 {
			return nil, errors.New("rate_multiplier must be >= 0")
		}
		// 同步开启时倍率归上游所有，手工值活不过下一次成功探测（表现为"改了又自己
		// 变回去"），与批量路径一样直接拒绝。判断的是本次请求生效后的状态：上面
		// 已把请求携带的两个开关落进 account.Extra，所以"同一请求关闭同步 + 改倍率"
		// （用户显式收回所有权）会走到这里时读到 false，正常放行。
		if upstreamBillingRateSyncEnabled(account) {
			return nil, ErrUpstreamBillingRateSyncConflict
		}
		account.RateMultiplier = input.RateMultiplier
	}
	if input.LoadFactor != nil {
		if *input.LoadFactor <= 0 {
			account.LoadFactor = nil // 0 或负数表示清除
		} else if *input.LoadFactor > 10000 {
			return nil, errors.New("load_factor must be <= 10000")
		} else {
			account.LoadFactor = input.LoadFactor
		}
	}
	if input.Status != "" {
		account.Status = input.Status
	}
	if input.ExpiresAt != nil {
		if *input.ExpiresAt <= 0 {
			account.ExpiresAt = nil
		} else {
			expiresAt := time.Unix(*input.ExpiresAt, 0)
			account.ExpiresAt = &expiresAt
		}
	}
	if input.AutoPauseOnExpired != nil {
		account.AutoPauseOnExpired = *input.AutoPauseOnExpired
	}

	// 先验证分组是否存在（在任何写操作之前）
	if input.GroupIDs != nil {
		if err := s.validateGroupIDsExist(ctx, *input.GroupIDs); err != nil {
			return nil, err
		}
		if err := s.ValidateAccountGroupBindings(ctx, *input.GroupIDs); err != nil {
			return nil, err
		}

		// 检查混合渠道风险（除非用户已确认）
		if !input.SkipMixedChannelCheck {
			if err := s.checkMixedChannelRisk(ctx, account.ID, account.Platform, *input.GroupIDs); err != nil {
				return nil, err
			}
		}
	}

	billingSettingsAppliedAtomically := false
	updater := s.accountBillingRepo
	if updater == nil {
		// Unit tests and narrow internal callers may construct adminServiceImpl
		// directly; production wiring requires this capability through
		// AdminAccountRepository.
		updater, _ = s.accountRepo.(AccountBillingSettingsRepository)
	}
	if updater != nil {
		if err := updater.UpdateWithAccountBillingSettings(
			ctx,
			account,
			requestedProbeEnabledUpdate,
			requestedRateSyncEnabledUpdate,
			input.RateMultiplier,
		); err != nil {
			return nil, err
		}
		billingSettingsAppliedAtomically = true
	}
	if !billingSettingsAppliedAtomically {
		if err := s.accountRepo.Update(ctx, account); err != nil {
			return nil, err
		}
		if (requestedProbeEnabledUpdate != nil || requestedRateSyncEnabledUpdate != nil) &&
			isUpstreamBillingProbeAccount(account) {
			settings := make(map[string]any, 2)
			if requestedProbeEnabledUpdate != nil {
				settings[UpstreamBillingProbeEnabledExtraKey] = *requestedProbeEnabledUpdate
			}
			if requestedRateSyncEnabledUpdate != nil {
				settings[UpstreamBillingRateSyncEnabledExtraKey] = *requestedRateSyncEnabledUpdate
			}
			if err := s.accountRepo.UpdateExtra(ctx, account.ID, settings); err != nil {
				return nil, err
			}
		}
	}

	// 将 proxy 变更传播到 spark 影子账号（同步；Update 内部已触发调度快照）。
	// 影子自身 proxy 不可独立编辑(见上),故对影子的更新不触发传播。
	if input.ProxyID != nil && !account.IsCredentialShadow() {
		if err := s.propagateProxyToShadows(ctx, id, account.ProxyID); err != nil {
			return nil, err
		}
	}

	// 绑定分组
	if input.GroupIDs != nil {
		if err := s.accountRepo.BindGroups(ctx, account.ID, *input.GroupIDs); err != nil {
			return nil, err
		}
	}

	// 重新查询以确保返回完整数据（包括正确的 Proxy 关联对象）
	updated, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// UpdateAccountExtra 仅对 Extra JSONB 做 key 级合并，避免覆盖其它运行态键
// （如 model_rate_limits / passive_usage_* 等）。
func (s *adminServiceImpl) UpdateAccountExtra(ctx context.Context, id int64, updates map[string]any) error {
	updates = sanitizedCodexFingerprintExtraUpdates(updates)
	updates = stripOpenAIAutoResetCreditManagedExtra(updates, true)
	delete(updates, UpstreamBillingProbeEnabledExtraKey)
	delete(updates, UpstreamBillingRateSyncEnabledExtraKey)
	delete(updates, UpstreamBillingProbeExtraKey)
	delete(updates, OllamaCloudUsageSessionExtraKey)
	delete(updates, OllamaCloudUsageAutoRefreshExtraKey)
	delete(updates, OllamaCloudUsageSnapshotExtraKey)
	if _, exists := updates[openAILongContextBillingEnabledKey]; exists {
		account, err := s.accountRepo.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if err := ValidateOpenAILongContextBillingExtra(account.Platform, updates); err != nil {
			return err
		}
	}
	if len(updates) == 0 {
		return nil
	}
	return s.accountRepo.UpdateExtra(ctx, id, updates)
}

// BulkUpdateAccounts updates multiple accounts in one request.
// It merges credentials/extra keys instead of overwriting the whole object.
func (s *adminServiceImpl) BulkUpdateAccounts(ctx context.Context, input *BulkUpdateAccountsInput) (*BulkUpdateAccountsResult, error) {
	// Managed probe/session state may only enter through dedicated typed endpoints.
	input.Extra = sanitizedCodexFingerprintExtraUpdates(input.Extra)
	input.Extra = stripOpenAIAutoResetCreditManagedExtra(input.Extra, true)
	delete(input.Extra, UpstreamBillingProbeEnabledExtraKey)
	delete(input.Extra, UpstreamBillingRateSyncEnabledExtraKey)
	delete(input.Extra, UpstreamBillingProbeExtraKey)
	delete(input.Extra, OllamaCloudUsageSessionExtraKey)
	delete(input.Extra, OllamaCloudUsageAutoRefreshExtraKey)
	delete(input.Extra, OllamaCloudUsageSnapshotExtraKey)

	if len(input.AccountIDs) == 0 && input.Filters != nil {
		accountIDs, err := s.resolveBulkUpdateTargetIDs(ctx, input.Filters)
		if err != nil {
			return nil, err
		}
		input.AccountIDs = accountIDs
	}

	result := &BulkUpdateAccountsResult{
		SuccessIDs: make([]int64, 0, len(input.AccountIDs)),
		FailedIDs:  make([]int64, 0, len(input.AccountIDs)),
		Results:    make([]BulkUpdateAccountResult, 0, len(input.AccountIDs)),
	}

	if len(input.AccountIDs) == 0 {
		return result, nil
	}
	if input.GroupIDs != nil {
		if err := s.validateGroupIDsExist(ctx, *input.GroupIDs); err != nil {
			return nil, err
		}
		if err := s.ValidateAccountGroupBindings(ctx, *input.GroupIDs); err != nil {
			return nil, err
		}
	}
	openAISettings, err := normalizeBulkOpenAISettings(input)
	if err != nil {
		return nil, err
	}

	needMixedChannelCheck := input.GroupIDs != nil && !input.SkipMixedChannelCheck

	// 预取所有目标账号，供凭据守卫/代理守卫/混合渠道检查共用，避免多次 DB 查询。
	var cachedTargets []*Account
	if len(input.Credentials) > 0 || input.ProxyID != nil || needMixedChannelCheck || openAISettings.any() || input.ProbeEnabled != nil || input.RateMultiplier != nil {
		loaded, err := s.accountRepo.GetByIDs(ctx, input.AccountIDs)
		if err != nil {
			return nil, err
		}
		cachedTargets = loaded
	}
	targetsByID := make(map[int64]*Account, len(cachedTargets))
	for _, account := range cachedTargets {
		if account != nil {
			targetsByID[account.ID] = account
		}
	}
	if openAISettings.any() {
		inheritedCount, err := validateBulkOpenAISettingsTargets(input, openAISettings, targetsByID)
		if err != nil {
			return nil, err
		}
		result.LongContextInheritedCount = inheritedCount
	}
	if input.ProbeEnabled != nil {
		for _, accountID := range input.AccountIDs {
			account, ok := targetsByID[accountID]
			if !ok {
				return nil, ErrAccountNotFound
			}
			if !isUpstreamBillingProbeAccount(account) {
				return nil, ErrUpstreamBillingProbeAccountInvalid
			}
		}
	}
	// 影子账号绝不持有凭据:批量更新携带凭据时,目标中不得含影子(外审 G5,与单账号
	// UpdateAccount 守卫对齐)。覆盖显式 IDs 与 filter 解析出的 IDs(此处 AccountIDs 已解析完成)。
	if len(input.Credentials) > 0 {
		for _, acc := range cachedTargets {
			if acc != nil && acc.IsCredentialShadow() {
				return nil, infraerrors.Newf(http.StatusBadRequest, "SPARK_SHADOW_NO_CREDENTIALS",
					"spark shadow account %d cannot hold credentials; manage credentials on the parent account", acc.ID)
			}
		}
		// 网页逆向平台账号不接受批量凭证更新：批量载荷无法按平台区分清洗语义
		// （空平台标签下 cookie 会被 SanitizeStoredCredentials 剥离，静默丢失登录态），
		// 且批量路径不做 web 凭证校验。逐个编辑才能保证 cookie/access_token 校验与落盘。
		for _, acc := range cachedTargets {
			if acc != nil && acc.IsWebAccessMode() {
				return nil, infraerrors.Newf(http.StatusBadRequest, "WEB_ACCOUNT_BULK_CREDENTIALS_UNSUPPORTED",
					"web provider account %d (%s) does not support bulk credential updates; edit credentials individually", acc.ID, acc.Platform)
			}
		}
	}

	// 影子账号 proxy 恒继承母账号(与单账号 UpdateAccount 守卫对齐——外审第4轮 P1):批量携带 proxy
	// 时目标不得含影子,否则影子会获得独立 proxy、破坏继承不变量(网关按所选影子自身 proxy 出站,
	// 要等母账号下次改 proxy 才覆盖→漂移)。含影子即整体拒绝,提示从选择中剔除影子。
	if input.ProxyID != nil {
		for _, acc := range cachedTargets {
			if acc != nil && acc.IsCredentialShadow() {
				return nil, infraerrors.Newf(http.StatusBadRequest, "SPARK_SHADOW_PROXY_INHERITED",
					"spark shadow account %d proxy is inherited from its parent and cannot be set in bulk; manage it on the parent account", acc.ID)
			}
		}
	}

	// 预加载账号平台信息（混合渠道检查需要）。
	platformByID := map[int64]string{}
	if needMixedChannelCheck {
		for _, account := range cachedTargets {
			if account != nil {
				platformByID[account.ID] = account.Platform
			}
		}
	}

	// 预检查混合渠道风险：在任何写操作之前，若发现风险立即返回错误。
	if needMixedChannelCheck {
		for _, accountID := range input.AccountIDs {
			platform := platformByID[accountID]
			if platform == "" {
				continue
			}
			if err := s.checkMixedChannelRisk(ctx, accountID, platform, *input.GroupIDs); err != nil {
				return nil, err
			}
		}
	}

	if input.RateMultiplier != nil {
		if *input.RateMultiplier < 0 {
			return nil, errors.New("rate_multiplier must be >= 0")
		}
		syncEnabledCount := 0
		for _, account := range cachedTargets {
			if account == nil || account.Extra == nil {
				continue
			}
			enabled, _ := account.Extra[UpstreamBillingRateSyncEnabledExtraKey].(bool)
			if enabled {
				syncEnabledCount++
			}
		}
		if syncEnabledCount > 0 {
			return nil, ErrUpstreamBillingRateSyncBulkConflict.WithMetadata(map[string]string{
				"count": strconv.Itoa(syncEnabledCount),
			})
		}
	}

	// 校验并规范化请求头覆写配置（批量路径为 JSONB 顶层 key 合并，直接校验增量即可）
	if err := NormalizeHeaderOverrideCredentials(input.Credentials); err != nil {
		return nil, err
	}
	// Bulk may mix platforms; always drop ephemeral SSO/password keys (cookie
	// only when platform is known Grok — empty platform still strips password/*).
	if input.Credentials != nil {
		input.Credentials = SanitizeStoredCredentials("", input.Credentials)
	}

	// Prepare bulk updates for columns and JSONB fields.
	repoUpdates := AccountBulkUpdate{
		Credentials:                input.Credentials,
		Extra:                      input.Extra,
		ProbeEnabled:               input.ProbeEnabled,
		EnsureCodexFingerprintSeed: ShouldEnsureCodexFingerprintSeedForExtraUpdates(input.Extra),
	}
	if input.ProbeEnabled != nil {
		if repoUpdates.Extra == nil {
			repoUpdates.Extra = make(map[string]any)
		}
		repoUpdates.Extra[UpstreamBillingProbeEnabledExtraKey] = *input.ProbeEnabled
		if !*input.ProbeEnabled {
			repoUpdates.Extra[UpstreamBillingRateSyncEnabledExtraKey] = false
		}
	}
	if updatesUpstreamBillingProbeIdentity(input.Credentials) || input.ProxyID != nil {
		if repoUpdates.Extra == nil {
			repoUpdates.Extra = make(map[string]any)
		}
		// JSON null makes every reader treat the old snapshot as absent and lets the
		// next enabled runner cycle probe the new upstream identity immediately.
		repoUpdates.Extra[UpstreamBillingProbeExtraKey] = nil
	}
	if input.Name != "" {
		repoUpdates.Name = &input.Name
	}
	if input.ProxyID != nil {
		repoUpdates.ProxyID = input.ProxyID
	}
	if input.Concurrency != nil {
		repoUpdates.Concurrency = input.Concurrency
	}
	if input.Priority != nil {
		repoUpdates.Priority = input.Priority
	}
	if input.RateMultiplier != nil {
		repoUpdates.RateMultiplier = input.RateMultiplier
	}
	if input.LoadFactor != nil {
		if *input.LoadFactor <= 0 {
			repoUpdates.LoadFactor = nil // 0 或负数表示清除
		} else if *input.LoadFactor > 10000 {
			return nil, errors.New("load_factor must be <= 10000")
		} else {
			repoUpdates.LoadFactor = input.LoadFactor
		}
	}
	if input.Status != "" {
		repoUpdates.Status = &input.Status
	}
	if input.Schedulable != nil {
		repoUpdates.Schedulable = input.Schedulable
	}

	// Run bulk update for column/jsonb fields first.
	if _, err := s.accountRepo.BulkUpdate(ctx, input.AccountIDs, repoUpdates); err != nil {
		return nil, err
	}

	// 将 proxy 变更传播到每个目标账号的 spark 影子账号
	if repoUpdates.ProxyID != nil {
		var effectiveProxyID *int64
		if *repoUpdates.ProxyID != 0 {
			effectiveProxyID = repoUpdates.ProxyID
		}
		for _, accountID := range input.AccountIDs {
			if err := s.propagateProxyToShadows(ctx, accountID, effectiveProxyID); err != nil {
				return nil, err
			}
		}
	}

	// Handle group bindings per account (requires individual operations).
	for _, accountID := range input.AccountIDs {
		entry := BulkUpdateAccountResult{AccountID: accountID}

		if input.GroupIDs != nil {
			if err := s.accountRepo.BindGroups(ctx, accountID, *input.GroupIDs); err != nil {
				entry.Success = false
				entry.Error = err.Error()
				result.Failed++
				result.FailedIDs = append(result.FailedIDs, accountID)
				result.Results = append(result.Results, entry)
				continue
			}
		}

		entry.Success = true
		result.Success++
		result.SuccessIDs = append(result.SuccessIDs, accountID)
		result.Results = append(result.Results, entry)
	}

	return result, nil
}

func updatesUpstreamBillingProbeIdentity(credentials map[string]any) bool {
	for _, key := range []string{"api_key", "base_url", credKeyHeaderOverrideEnabled, credKeyHeaderOverrides} {
		if _, ok := credentials[key]; ok {
			return true
		}
	}
	return false
}

func upstreamBillingProbeIdentity(account *Account) map[string]any {
	if account == nil {
		return nil
	}
	identity := map[string]any{"platform": account.Platform, "type": account.Type, "proxy_id": nil}
	if account.ProxyID != nil {
		identity["proxy_id"] = *account.ProxyID
	}
	for _, key := range []string{"api_key", "base_url", credKeyHeaderOverrideEnabled, credKeyHeaderOverrides} {
		if value, ok := account.Credentials[key]; ok {
			identity[key] = value
		}
	}
	return identity
}

func (s *adminServiceImpl) resolveBulkUpdateTargetIDs(ctx context.Context, filters *BulkUpdateAccountFilters) ([]int64, error) {
	if filters == nil {
		return nil, nil
	}

	groupID := int64(0)
	switch strings.TrimSpace(filters.Group) {
	case "":
	case "ungrouped":
		groupID = AccountListGroupUngrouped
	default:
		parsedGroupID, err := strconv.ParseInt(strings.TrimSpace(filters.Group), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid group filter: %w", err)
		}
		groupID = parsedGroupID
	}

	const pageSize = 500
	page := 1
	accountIDs := make([]int64, 0, pageSize)

	for {
		accounts, total, err := s.ListAccounts(
			ctx,
			page,
			pageSize,
			filters.Platform,
			filters.Type,
			filters.Status,
			filters.Search,
			groupID,
			filters.PrivacyMode,
			"",
			"",
		)
		if err != nil {
			return nil, err
		}
		for _, account := range accounts {
			accountIDs = append(accountIDs, account.ID)
		}
		if int64(len(accountIDs)) >= total || len(accounts) == 0 {
			return accountIDs, nil
		}
		page++
	}
}

func (s *adminServiceImpl) DeleteAccount(ctx context.Context, id int64) error {
	// 级联删除 spark 影子账号（先删影子，再删母账号）
	shadows, err := s.accountRepo.ListShadowsByParent(ctx, id)
	if err != nil {
		return fmt.Errorf("list spark shadows for cascade delete: %w", err)
	}
	for _, shadow := range shadows {
		if err := s.accountRepo.Delete(ctx, shadow.ID); err != nil {
			return fmt.Errorf("cascade delete spark shadow %d: %w", shadow.ID, err)
		}
	}
	if err := s.accountRepo.Delete(ctx, id); err != nil {
		return err
	}
	return nil
}

func (s *adminServiceImpl) RefreshAccountCredentials(ctx context.Context, id int64) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// TODO: Implement refresh logic
	return account, nil
}

func (s *adminServiceImpl) ClearAccountError(ctx context.Context, id int64) (*Account, error) {
	if err := s.accountRepo.ClearError(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearRateLimit(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearAntigravityQuotaScopes(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearModelRateLimits(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearTempUnschedulable(ctx, id); err != nil {
		return nil, err
	}
	if s.runtimeBlocker != nil {
		s.runtimeBlocker.ClearAccountSchedulingBlock(id)
	}
	return s.accountRepo.GetByID(ctx, id)
}

func (s *adminServiceImpl) SetAccountError(ctx context.Context, id int64, errorMsg string) error {
	return s.accountRepo.SetError(ctx, id, errorMsg)
}

func (s *adminServiceImpl) SetAccountSchedulable(ctx context.Context, id int64, schedulable bool) (*Account, error) {
	if err := s.accountRepo.SetSchedulable(ctx, id, schedulable); err != nil {
		return nil, err
	}
	updated, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *adminServiceImpl) RevertAccountProxyFallback(ctx context.Context, id int64) error {
	if err := s.accountRepo.RevertProxyFallback(ctx, id); err != nil {
		return err
	}
	// 加载回退后的账号以获取实际 ProxyID，再传播到影子账号
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("get account after proxy revert: %w", err)
	}
	return s.propagateProxyToShadows(ctx, id, account.ProxyID)
}

// CreateShadow 为受支持的 OAuth 母账号创建影子账号：
//   - OpenAI OAuth 母账号 → spark 维度、一母一影（platform=openai，仅 model_mapping 凭据）。
//   - CodeBuddy OAuth 母账号 → codebuddy 维度、一母多影（platform=目标分组平台，凭证空走母账号）。
//
// 安全不变量：影子账号 Credentials 恒不含 auth token（spark 仅 model_mapping；codebuddy 默认空=透传）。
func (s *adminServiceImpl) CreateShadow(ctx context.Context, parentID int64, opts ShadowOptions) (*Account, error) {
	// 1. 加载母账号并校验平台/类型
	parent, err := s.accountRepo.GetByID(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("get parent account: %w", err)
	}
	// CodeBuddy 影子支持 OAuth 与 APIKey 两类母账号：两者都持有上游凭证，
	// 影子通过 resolveCredentialAccount 透传母账号 credentials（access_token 或 api_key）。
	isCodeBuddyParent := parent.IsCodeBuddy() && (parent.Type == AccountTypeOAuth || parent.Type == AccountTypeAPIKey)
	if !parent.IsOpenAIOAuth() && !isCodeBuddyParent {
		return nil, infraerrors.New(http.StatusBadRequest, "SHADOW_INVALID_PARENT",
			"shadow requires an OpenAI OAuth or CodeBuddy (OAuth/APIKey) parent account")
	}
	// G6:母账号本身不能是影子,否则会建出二级影子——resolveCredentialAccount 只解一层,
	// 会解析到无凭据的一级影子,进入坏调度/上游失败。
	if parent.IsCredentialShadow() {
		return nil, infraerrors.New(http.StatusBadRequest, "SHADOW_PARENT_IS_SHADOW",
			"shadow parent must be a real account, not another shadow")
	}

	// 2. 影子维度与目标平台
	var quotaDimension, shadowPlatform string
	if isCodeBuddyParent {
		quotaDimension = QuotaDimensionCodeBuddy
		// codebuddy 影子 platform=目标分组平台（缺省按模型名推断，向导可改）；
		// 刻意排除 openai/codebuddy 平台，避免与既有 OAuth/上游语义混淆（方案 §2.2）。
		shadowPlatform = strings.TrimSpace(opts.Platform)
		if shadowPlatform == "" {
			shadowPlatform = inferCodeBuddyShadowPlatform(opts.Model)
		}
		if shadowPlatform == PlatformOpenAI || shadowPlatform == PlatformCodeBuddy {
			return nil, infraerrors.New(http.StatusBadRequest, "CODEBUDDY_SHADOW_PLATFORM_INVALID",
				"codebuddy shadow platform must be a target provider (deepseek/zhipu/kimi/minimax/other), not openai or codebuddy")
		}
	} else {
		quotaDimension = QuotaDimensionSpark
		shadowPlatform = PlatformOpenAI
	}

	// 3. 唯一性校验
	shadows, err := s.accountRepo.ListShadowsByParent(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("check existing shadows: %w", err)
	}
	if !isCodeBuddyParent {
		// spark：一母一影
		if len(shadows) > 0 {
			return nil, infraerrors.New(http.StatusConflict, "SPARK_SHADOW_ALREADY_EXISTS",
				"parent account already has a spark shadow account")
		}
	} else if opts.Model != "" {
		// codebuddy：一母多影，但同一上游模型不重复创建（向导按模型勾选）。
		for _, sh := range shadows {
			if shadowTargetsModel(sh, opts.Model) {
				return nil, infraerrors.New(http.StatusConflict, "CODEBUDDY_SHADOW_MODEL_EXISTS",
					"parent account already has a codebuddy shadow for model "+opts.Model)
			}
		}
	}

	// 4. 解析分组。
	// - spark：未指定 GroupIDs 时优先继承母账号分组，再回落 openai-default（与既有语义一致）。
	// - codebuddy：影子 platform 已变为目标平台，不能继承 codebuddy 母账号的分组，
	//   必须显式指定目标分组（向导按平台联动过滤后单选）。
	groupIDs := opts.GroupIDs
	if len(groupIDs) > 0 {
		if s.groupRepo != nil {
			if err := s.validateGroupIDsExist(ctx, groupIDs); err != nil {
				return nil, err
			}
		}
	} else if !isCodeBuddyParent {
		if len(parent.GroupIDs) > 0 {
			groupIDs = append([]int64(nil), parent.GroupIDs...)
		} else if s.groupRepo != nil {
			defaultGroupName := PlatformOpenAI + "-default"
			if groups, gerr := s.groupRepo.ListActiveByPlatform(ctx, PlatformOpenAI); gerr == nil {
				for _, g := range groups {
					if g.Name == defaultGroupName {
						groupIDs = []int64{g.ID}
						break
					}
				}
			}
		}
	} else {
		return nil, infraerrors.New(http.StatusBadRequest, "CODEBUDDY_SHADOW_REQUIRES_GROUP",
			"codebuddy shadow requires an explicit target group")
	}
	if err := s.ValidateAccountGroupBindings(ctx, groupIDs); err != nil {
		return nil, err
	}

	// 5. 构造影子账号（安全不变量：Credentials 恒不含 auth token）。
	// name 为空时默认：spark→"<母> (Spark)"；codebuddy→"<母>:<model>"（缺模型时 "<母> (CodeBuddy)"）。
	// 空 name 会在 ent(name NotEmpty)处变裸 500，故必须给默认；并 rune 截断到 ent MaxLen(100)。
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		if isCodeBuddyParent {
			if strings.TrimSpace(opts.Model) != "" {
				name = parent.Name + ":" + opts.Model
			} else {
				name = parent.Name + " (CodeBuddy)"
			}
		} else {
			name = parent.Name + " (Spark)"
		}
	}
	if runes := []rune(name); len(runes) > 100 {
		name = string(runes[:100])
	}
	// 并发未指定(<=0)时继承母账号，避免 0 被限流器解读为"无限并发"（外审 F3）。
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = parent.Concurrency
	}
	// 优先级未指定(<=0)时继承母账号——调度比较「数值越小越优先」，省略直写 0 会让影子
	// 意外抢到最高优先级（外审第5轮 P1）。影子的 proxy/分组/并发亦全部继承母账号。
	priority := opts.Priority
	if priority <= 0 {
		priority = parent.Priority
	}

	var credentials map[string]any
	if isCodeBuddyParent {
		// codebuddy 影子凭证空，运行时透传母账号；model_mapping 默认空=原样透传
		// （GetMappedModel 对空 mapping 返回原模型名，向导可后续按官方名微调）。
		credentials = map[string]any{}
	} else {
		credentials = map[string]any{"model_mapping": defaultSparkShadowModelMapping()}
	}

	shadowExtra := map[string]any{}
	if !isCodeBuddyParent {
		shadowExtra[openAILongContextBillingEnabledKey] = parent.IsOpenAILongContextBillingEnabled()
	} else if strings.TrimSpace(opts.Model) != "" {
		shadowExtra[ShadowModelExtraKey] = opts.Model
	}

	shadow := &Account{
		Name:            name,
		Platform:        shadowPlatform,
		Type:            AccountTypeOAuth,
		Status:          StatusActive,
		Credentials:     credentials,
		ParentAccountID: &parentID,
		QuotaDimension:  quotaDimension,
		ProxyID:         parent.ProxyID,
		Priority:        priority,
		Concurrency:     concurrency,
		Schedulable:     true,
		Extra:           shadowExtra,
	}

	// 6. 持久化（Create 填充 shadow.ID）。并发竞态:预查(步骤3)放行后另一请求抢先建成,本次会撞
	// 唯一约束。复查确认确为"已存在"竞态时返回结构化 409 而非裸 500——外审 A/P1。
	if err := s.accountRepo.Create(ctx, shadow); err != nil {
		if existing, qerr := s.accountRepo.ListShadowsByParent(ctx, parentID); qerr == nil {
			if !isCodeBuddyParent && len(existing) > 0 {
				return nil, infraerrors.New(http.StatusConflict, "SPARK_SHADOW_ALREADY_EXISTS",
					"parent account already has a spark shadow account")
			}
			if isCodeBuddyParent && opts.Model != "" {
				for _, sh := range existing {
					if shadowTargetsModel(sh, opts.Model) {
						return nil, infraerrors.New(http.StatusConflict, "CODEBUDDY_SHADOW_MODEL_EXISTS",
							"parent account already has a codebuddy shadow for model "+opts.Model)
					}
				}
			}
		}
		return nil, fmt.Errorf("create shadow: %w", err)
	}

	// 7. 绑定分组。create+bind 非单一 DB 事务(通用 Create 走 r.client、outbox 走 r.sql,
	// 无现成共享事务路径),故绑组失败时做 best-effort 补偿删除刚建的影子,避免半成品影子
	// (唯一约束会挡住重试)——外审 C/P1。补偿删除用 detached ctx,即便请求 ctx 已取消/超时
	// 仍能完成清理(外审第4轮);进程崩溃这种极端仍可能残留,属已知权衡。
	if len(groupIDs) > 0 {
		if err := s.accountRepo.BindGroups(ctx, shadow.ID, groupIDs); err != nil {
			if delErr := s.accountRepo.Delete(context.WithoutCancel(ctx), shadow.ID); delErr != nil {
				slog.Error("shadow_bind_groups_rollback_failed",
					"shadow_id", shadow.ID, "parent_id", parentID, "delete_err", delErr)
			}
			return nil, fmt.Errorf("bind groups for shadow: %w", err)
		}
		shadow.GroupIDs = groupIDs
	}

	return shadow, nil
}

// propagateProxyToShadows syncs proxyID to all spark shadow accounts of parentID.
// It is called synchronously so that proxy changes are immediately consistent;
// accountRepo.Update triggers the scheduler outbox + cache propagation internally.
// Calling this for a non-parent account is a harmless no-op.
func (s *adminServiceImpl) propagateProxyToShadows(ctx context.Context, parentID int64, proxyID *int64) error {
	return propagateAccountProxyToShadows(ctx, s.accountRepo, parentID, proxyID)
}

// propagateAccountProxyToShadows 把母账号的 proxy 同步到其所有 spark 影子(影子 proxy 恒继承母账号)。
// 供 AdminService 编辑路径与 CRS 同步路径共用——后者改动母账号 proxy 后必须同样传播,否则影子保留
// 旧 proxy 出现出站漂移(外审第8轮)。
func propagateAccountProxyToShadows(ctx context.Context, repo AccountRepository, parentID int64, proxyID *int64) error {
	shadows, err := repo.ListShadowsByParent(ctx, parentID)
	if err != nil {
		return fmt.Errorf("list spark shadows for proxy propagation: %w", err)
	}
	for _, shadow := range shadows {
		shadow.ProxyID = proxyID
		if err := repo.Update(ctx, shadow); err != nil {
			return fmt.Errorf("update spark shadow %d proxy: %w", shadow.ID, err)
		}
	}
	return nil
}

// checkMixedChannelRisk 检查分组中是否存在混合渠道（Antigravity + Anthropic）
// 如果存在混合，返回错误提示用户确认
func (s *adminServiceImpl) checkMixedChannelRisk(ctx context.Context, currentAccountID int64, currentAccountPlatform string, groupIDs []int64) error {
	// 判断当前账号的渠道类型（基于 platform 字段，而不是 type 字段）
	currentPlatform := getAccountPlatform(currentAccountPlatform)
	if currentPlatform == "" {
		// 不是 Antigravity 或 Anthropic，无需检查
		return nil
	}

	// 检查每个分组中的其他账号
	for _, groupID := range groupIDs {
		accounts, err := s.accountRepo.ListByGroup(ctx, groupID)
		if err != nil {
			return fmt.Errorf("get accounts in group %d: %w", groupID, err)
		}

		// 检查是否存在不同渠道的账号
		for _, account := range accounts {
			if currentAccountID > 0 && account.ID == currentAccountID {
				continue // 跳过当前账号
			}

			otherPlatform := getAccountPlatform(account.Platform)
			if otherPlatform == "" {
				continue // 不是 Antigravity 或 Anthropic，跳过
			}

			// 检测混合渠道
			if currentPlatform != otherPlatform {
				group, _ := s.groupRepo.GetByID(ctx, groupID)
				groupName := fmt.Sprintf("Group %d", groupID)
				if group != nil {
					groupName = group.Name
				}

				return &MixedChannelError{
					GroupID:         groupID,
					GroupName:       groupName,
					CurrentPlatform: currentPlatform,
					OtherPlatform:   otherPlatform,
				}
			}
		}
	}

	return nil
}

func (s *adminServiceImpl) validateGroupIDsExist(ctx context.Context, groupIDs []int64) error {
	if len(groupIDs) == 0 {
		return nil
	}
	if s.groupRepo == nil {
		return errors.New("group repository not configured")
	}

	if batchReader, ok := s.groupRepo.(groupExistenceBatchReader); ok {
		existsByID, err := batchReader.ExistsByIDs(ctx, groupIDs)
		if err != nil {
			return fmt.Errorf("check groups exists: %w", err)
		}
		for _, groupID := range groupIDs {
			if groupID <= 0 || !existsByID[groupID] {
				return fmt.Errorf("get group: %w", ErrGroupNotFound)
			}
		}
		return nil
	}

	for _, groupID := range groupIDs {
		if _, err := s.groupRepo.GetByID(ctx, groupID); err != nil {
			return fmt.Errorf("get group: %w", err)
		}
	}
	return nil
}

// ValidateAccountGroupBindings is the shared fail-closed policy boundary for
// every account path that accepts explicit group bindings.
func (s *adminServiceImpl) ValidateAccountGroupBindings(ctx context.Context, groupIDs []int64) error {
	if len(groupIDs) == 0 || s.cfg == nil || s.cfg.RunMode != config.RunModeSimple {
		return nil
	}
	if s.groupRepo == nil {
		return errors.New("group repository not configured")
	}
	seen := make(map[int64]struct{}, len(groupIDs))
	for _, groupID := range groupIDs {
		if groupID <= 0 {
			return fmt.Errorf("get group: %w", ErrGroupNotFound)
		}
		if _, ok := seen[groupID]; ok {
			continue
		}
		seen[groupID] = struct{}{}
		group, err := s.groupRepo.GetByIDLite(ctx, groupID)
		if err != nil {
			return fmt.Errorf("get group: %w", err)
		}
		if !IsGroupBindableInSimpleMode(group) {
			return infraerrors.BadRequest("SIMPLE_MODE_GROUP_NOT_BINDABLE", "composite groups cannot be bound in simple mode")
		}
	}
	return nil
}

// CheckMixedChannelRisk checks whether target groups contain mixed channels for the current account platform.
func (s *adminServiceImpl) CheckMixedChannelRisk(ctx context.Context, currentAccountID int64, currentAccountPlatform string, groupIDs []int64) error {
	return s.checkMixedChannelRisk(ctx, currentAccountID, currentAccountPlatform, groupIDs)
}

// getAccountPlatform 根据账号 platform 判断混合渠道检查用的平台标识
func getAccountPlatform(accountPlatform string) string {
	switch strings.ToLower(strings.TrimSpace(accountPlatform)) {
	case PlatformAntigravity:
		return "Antigravity"
	case PlatformAnthropic, "claude":
		return "Anthropic"
	default:
		return ""
	}
}

// MixedChannelError 混合渠道错误
type MixedChannelError struct {
	GroupID         int64
	GroupName       string
	CurrentPlatform string
	OtherPlatform   string
}

func (e *MixedChannelError) Error() string {
	return fmt.Sprintf("mixed_channel_warning: Group '%s' contains both %s and %s accounts. Using mixed channels in the same context may cause thinking block signature validation issues, which will fallback to non-thinking mode for historical messages.",
		e.GroupName, e.CurrentPlatform, e.OtherPlatform)
}

func (s *adminServiceImpl) ResetAccountQuota(ctx context.Context, id int64) error {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	// spark 影子账号不持自有配额(凭据透传母账号、spark 用量走独立 codex_* 维度由 QueryUsage 维护),
	// 通用 quota 重置对其无意义且语义不一致——明确 400 拒绝(与 OpenAI reset-credit 对影子一致)(外审第7轮 P2)。
	if account.IsCredentialShadow() {
		return infraerrors.New(http.StatusBadRequest, "SPARK_SHADOW_NO_QUOTA_RESET",
			"cannot reset quota for a spark shadow account; manage it on the parent account")
	}
	return s.accountRepo.ResetQuotaUsedAndClearRateLimitCooldown(ctx, id)
}

// EnsureOpenAIPrivacy 检查 OpenAI OAuth 账号是否已设置 privacy_mode，
// 未设置则调用 disableOpenAITraining 并持久化到 Extra，返回设置的 mode 值。
func (s *adminServiceImpl) EnsureOpenAIPrivacy(ctx context.Context, account *Account) string {
	// 影子账号不持凭据，隐私设置由母账号管理，直接跳过。
	if account.IsCredentialShadow() {
		return ""
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return ""
	}
	if s.privacyClientFactory == nil {
		return ""
	}
	if shouldSkipOpenAIPrivacyEnsure(account.Extra) {
		return ""
	}

	token, _ := account.Credentials["access_token"].(string)
	if token == "" {
		return ""
	}

	var proxyURL string
	if account.ProxyID != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && p != nil {
			proxyURL = p.URL()
		}
	}

	mode := disableOpenAITraining(ctx, s.privacyClientFactory, token, proxyURL)
	if mode == "" {
		return ""
	}

	_ = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"privacy_mode": mode})
	return mode
}

// ForceOpenAIPrivacy 强制重新设置 OpenAI OAuth 账号隐私，无论当前状态。
func (s *adminServiceImpl) ForceOpenAIPrivacy(ctx context.Context, account *Account) string {
	// 影子账号不持凭据,隐私由母账号管理,直接跳过(与 EnsureOpenAIPrivacy 一致——外审第4轮)。
	if account.IsCredentialShadow() {
		return ""
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return ""
	}
	if s.privacyClientFactory == nil {
		return ""
	}

	token, _ := account.Credentials["access_token"].(string)
	if token == "" {
		return ""
	}

	var proxyURL string
	if account.ProxyID != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && p != nil {
			proxyURL = p.URL()
		}
	}

	mode := disableOpenAITraining(ctx, s.privacyClientFactory, token, proxyURL)
	if mode == "" {
		return ""
	}

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"privacy_mode": mode}); err != nil {
		logger.LegacyPrintf("service.admin", "force_update_openai_privacy_mode_failed: account_id=%d err=%v", account.ID, err)
		return mode
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra["privacy_mode"] = mode
	return mode
}

// EnsureAntigravityPrivacy 检查 Antigravity OAuth 账号隐私状态。
// 仅当 privacy_mode 已成功设置（"privacy_set"）时跳过；
// 未设置或之前失败（"privacy_set_failed"）均会重试。
func (s *adminServiceImpl) EnsureAntigravityPrivacy(ctx context.Context, account *Account) string {
	if account.Platform != PlatformAntigravity || account.Type != AccountTypeOAuth {
		return ""
	}
	if account.Extra != nil {
		if existing, ok := account.Extra["privacy_mode"].(string); ok && existing == AntigravityPrivacySet {
			return existing
		}
	}

	token, _ := account.Credentials["access_token"].(string)
	if token == "" {
		return ""
	}

	projectID, _ := account.Credentials["project_id"].(string)

	var proxyURL string
	if account.ProxyID != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && p != nil {
			proxyURL = p.URL()
		}
	}

	mode := setAntigravityPrivacy(ctx, token, projectID, proxyURL)
	if mode == "" {
		return ""
	}

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"privacy_mode": mode}); err != nil {
		logger.LegacyPrintf("service.admin", "update_antigravity_privacy_mode_failed: account_id=%d err=%v", account.ID, err)
		return mode
	}
	applyAntigravityPrivacyMode(account, mode)
	return mode
}

// ForceAntigravityPrivacy 强制重新设置 Antigravity OAuth 账号隐私，无论当前状态。
func (s *adminServiceImpl) ForceAntigravityPrivacy(ctx context.Context, account *Account) string {
	if account.Platform != PlatformAntigravity || account.Type != AccountTypeOAuth {
		return ""
	}

	token, _ := account.Credentials["access_token"].(string)
	if token == "" {
		return ""
	}

	projectID, _ := account.Credentials["project_id"].(string)

	var proxyURL string
	if account.ProxyID != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && p != nil {
			proxyURL = p.URL()
		}
	}

	mode := setAntigravityPrivacy(ctx, token, projectID, proxyURL)
	if mode == "" {
		return ""
	}

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"privacy_mode": mode}); err != nil {
		logger.LegacyPrintf("service.admin", "force_update_antigravity_privacy_mode_failed: account_id=%d err=%v", account.ID, err)
		return mode
	}
	applyAntigravityPrivacyMode(account, mode)
	return mode
}
