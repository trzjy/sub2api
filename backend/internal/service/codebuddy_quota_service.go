package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
)

// CodeBuddy 计费/积分与模型列表端点（§2.1）。billing 走 www.codebuddy.cn，models 走
// copilot.tencent.com（与 chat 同源）。
const (
	CodeBuddyBillingBaseURL = "https://www.codebuddy.cn"
	codeBuddyBillingMeterPath   = "/v2/billing/meter/get-user-resource"
	codeBuddyDailyCheckinPath   = "/v2/billing/meter/daily-checkin"
	CodeBuddyModelsBaseURL      = "https://copilot.tencent.com"
	codeBuddyModelsPath         = "/console/enterprises/personal/models"

	// Extra 快照键（CodeBuddyQuotaService 周期写入，调度阈值评估与 UI 消费）。
	codebuddyCreditUsedPercentKey = "codebuddy_credit_used_percent"
	codebuddyCreditResetAtKey     = "codebuddy_credit_reset_at"
	codebuddyCreditTotalKey       = "codebuddy_credit_total"
	codebuddyCreditUsedKey        = "codebuddy_credit_used"
	codebuddyCreditUpdatedAtKey   = "codebuddy_credit_updated_at"
	codebuddyCreditErrorKey       = "codebuddy_quota_error"

	// 上游不返回积分重置时间时，快照重置锚点默认取「探测时刻 + 24h」，使阈值候选在
	// 窗口内有效、过期后重新评估（与缺失探针时的 fail-open 一致）。真实抓取后若上游
	// 提供重置时间则以真实值为准。
	codebuddyCreditDefaultResetWindow = 24 * time.Hour
	codebuddyModelCacheTTL            = time.Hour
)

// codeBuddyQuotaInstance 是进程内唯一配额服务实例，供转发热路径（reasoning_effort 降级）
// 读取动态模型列表，避免改动 OpenAIGatewayService 构造签名波及大量测试。仅作可选服务的
// 轻量可达性桥接：为 nil 时相关能力自动降级为空（不报错）。
var codeBuddyQuotaInstance *CodeBuddyQuotaService

// CodeBuddyModel 是动态模型列表中的单个模型（§2.3）。
type CodeBuddyModel struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	MaxInputTokens   int64    `json:"maxInputTokens"`
	MaxOutputTokens  int64    `json:"maxOutputTokens"`
	Disabled         bool     `json:"disabled"`
	SupportedEfforts []string `json:"supportedEfforts"`
}

// CodeBuddyQuotaProbeResult 是积分额度探测的返回结构（管理端 + UI 消费）。
type CodeBuddyQuotaProbeResult struct {
	AccountID     int64   `json:"account_id"`
	Success       bool    `json:"success"`
	UsedPercent   float64 `json:"used_percent"`
	ResetAt       string  `json:"reset_at,omitempty"`
	TotalCredit   float64 `json:"total_credit,omitempty"`
	UsedCredit    float64 `json:"used_credit,omitempty"`
	StatusCode    int     `json:"status_code,omitempty"`
	FetchedAt     int64   `json:"fetched_at"`
	Persisted     bool    `json:"persisted"`
	Error         string  `json:"error,omitempty"`
}

// CodeBuddyQuotaService 周期探测 CodeBuddy 积分额度、执行每日签到、拉取动态模型列表。
// 计费响应字段以真实抓包为准（PR3 计划 §7.1）：解析做容错——按候选字段名扫描数值百分比，
// 缺失字段不写假值。
type CodeBuddyQuotaService struct {
	accountRepo  AccountRepository
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config

	modelCacheMu sync.Mutex
	modelCache   map[int64]codeBuddyModelCacheEntry
}

type codeBuddyModelCacheEntry struct {
	models    []CodeBuddyModel
	fetchedAt time.Time
}

// NewCodeBuddyQuotaService 构造 CodeBuddy 配额/模型服务。
func NewCodeBuddyQuotaService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *CodeBuddyQuotaService {
	s := &CodeBuddyQuotaService{
		accountRepo:  accountRepo,
		proxyRepo:    proxyRepo,
		httpUpstream: httpUpstream,
		cfg:          cfg,
		modelCache:   make(map[int64]codeBuddyModelCacheEntry),
	}
	codeBuddyQuotaInstance = s
	return s
}

// QueryUsage 探测指定账号的积分额度并落 Extra 快照。同一账号并发探测由调用方去重。
func (s *CodeBuddyQuotaService) QueryUsage(ctx context.Context, accountID int64) (*CodeBuddyQuotaProbeResult, error) {
	if s == nil || s.accountRepo == nil {
		return nil, fmt.Errorf("codebuddy quota service is not configured")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account.Platform != PlatformCodeBuddy {
		return nil, fmt.Errorf("account %d is not a codebuddy account", accountID)
	}
	return s.queryUsageForAccount(ctx, account)
}

func (s *CodeBuddyQuotaService) queryUsageForAccount(ctx context.Context, account *Account) (*CodeBuddyQuotaProbeResult, error) {
	result := &CodeBuddyQuotaProbeResult{
		AccountID: account.ID,
		FetchedAt: time.Now().Unix(),
	}
	body, status, err := s.doBillingRequest(ctx, account, http.MethodPost, codeBuddyBillingMeterPath, []byte("{}"))
	result.StatusCode = status
	if err != nil {
		result.Error = err.Error()
		s.persistError(ctx, account.ID, err.Error())
		return result, nil
	}
	usedPercent, ok := s.extractCreditPercent(body)
	if !ok {
		// 报文结构异常：记录错误但不写假百分比（避免误触发阈值停调）。
		errMsg := "codebuddy quota: 无法从响应解析积分已用百分比"
		result.Error = errMsg
		s.persistError(ctx, account.ID, errMsg)
		return result, nil
	}
	resetAt := s.extractResetTime(body)
	if resetAt.IsZero() {
		resetAt = time.Now().Add(codebuddyCreditDefaultResetWindow)
	}
	now := time.Now().UTC()
	updates := map[string]any{
		codebuddyCreditUsedPercentKey: usedPercent,
		codebuddyCreditResetAtKey:     resetAt.UTC().Format(time.RFC3339),
		codebuddyCreditUpdatedAtKey:   now.Format(time.RFC3339),
		codebuddyCreditErrorKey:       nil,
	}
	if total, ok := s.extractCreditAmount(body, "totalCredit", "total_credit"); ok {
		updates[codebuddyCreditTotalKey] = total
	}
	if used, ok := s.extractCreditAmount(body, "usedCredit", "used_credit"); ok {
		updates[codebuddyCreditUsedKey] = used
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("codebuddy_quota_snapshot_failed", "account_id", account.ID, "error", err)
		result.Error = "snapshot write failed: " + err.Error()
		return result, nil
	}
	result.Success = true
	result.Persisted = true
	result.UsedPercent = usedPercent
	result.ResetAt = resetAt.UTC().Format(time.RFC3339)
	return result, nil
}

// DailyCheckin 执行每日签到（白嫖积分）。默认关闭（配置 gateway.codebuddy.daily_checkin_enabled）。
func (s *CodeBuddyQuotaService) DailyCheckin(ctx context.Context, accountID int64) error {
	if s == nil || s.accountRepo == nil {
		return fmt.Errorf("codebuddy quota service is not configured")
	}
	if s.cfg != nil && !s.cfg.Gateway.CodeBuddy.DailyCheckinEnabled {
		return fmt.Errorf("codebuddy daily checkin is disabled")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if account.Platform != PlatformCodeBuddy {
		return fmt.Errorf("account %d is not a codebuddy account", accountID)
	}
	_, _, err = s.doBillingRequest(ctx, account, http.MethodPost, codeBuddyDailyCheckinPath, []byte("{}"))
	if err != nil {
		return err
	}
	return nil
}

// FetchModels 拉取账号的动态模型列表并刷新进程内缓存。
func (s *CodeBuddyQuotaService) FetchModels(ctx context.Context, accountID int64) ([]CodeBuddyModel, error) {
	if s == nil || s.accountRepo == nil {
		return nil, fmt.Errorf("codebuddy quota service is not configured")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account.Platform != PlatformCodeBuddy {
		return nil, fmt.Errorf("account %d is not a codebuddy account", accountID)
	}
	body, _, err := s.doBillingRequest(ctx, account, http.MethodGet, codeBuddyModelsPath, nil)
	if err != nil {
		return nil, err
	}
	models, err := s.parseModels(body)
	if err != nil {
		return nil, err
	}
	s.modelCacheMu.Lock()
	s.modelCache[accountID] = codeBuddyModelCacheEntry{models: models, fetchedAt: time.Now()}
	s.modelCacheMu.Unlock()
	return models, nil
}

func (s *CodeBuddyQuotaService) parseModels(body []byte) ([]CodeBuddyModel, error) {
	// 信封 {code,msg,data}，data.{models:[...]}
	data := gjson.GetBytes(body, "data")
	if !data.Exists() {
		data = gjson.ParseBytes(body)
	}
	raw := data.Get("models").Raw
	if raw == "" {
		// 兼容 data 直接为模型数组的情况。
		raw = gjson.GetBytes(body, "models").Raw
	}
	if raw == "" {
		return nil, fmt.Errorf("codebuddy models: 响应缺少 models 字段")
	}
	var models []CodeBuddyModel
	if err := json.Unmarshal([]byte(raw), &models); err != nil {
		return nil, fmt.Errorf("codebuddy models: 解析失败: %w", err)
	}
	return models, nil
}

// SupportedEffortsForModel 返回指定模型的 supportedEfforts（供 reasoning_effort 降级）。
// 优先读缓存；缓存未命中时惰性拉取（失败则降级为空，不阻塞请求）。
func (s *CodeBuddyQuotaService) SupportedEffortsForModel(ctx context.Context, account *Account, model string) []string {
	if s == nil || account == nil || strings.TrimSpace(model) == "" {
		return nil
	}
	if models := s.cachedModels(account.ID); models != nil {
		if eff := matchModelEfforts(models, model); eff != nil {
			return eff
		}
	}
	models, err := s.FetchModels(ctx, account.ID)
	if err != nil {
		slog.Debug("codebuddy_fetch_models_failed", "account_id", account.ID, "error", err)
		return nil
	}
	return matchModelEfforts(models, model)
}

func (s *CodeBuddyQuotaService) cachedModels(accountID int64) []CodeBuddyModel {
	s.modelCacheMu.Lock()
	defer s.modelCacheMu.Unlock()
	entry, ok := s.modelCache[accountID]
	if !ok || time.Since(entry.fetchedAt) > codebuddyModelCacheTTL {
		return nil
	}
	return entry.models
}

func matchModelEfforts(models []CodeBuddyModel, model string) []string {
	target := strings.TrimSpace(strings.ToLower(model))
	for i := range models {
		m := models[i]
		if strings.EqualFold(m.ID, model) || strings.EqualFold(m.Name, model) ||
			strings.ToLower(m.ID) == target || strings.ToLower(m.Name) == target {
			return m.SupportedEfforts
		}
	}
	return nil
}

// codeBuddyResolveSupportedEfforts 是转发热路径的包级访问器：读取动态模型列表中的
// supportedEfforts。服务未初始化时降级为空（不降级为无操作，仅跳过 reasoning_effort 降级）。
func codeBuddyResolveSupportedEfforts(ctx context.Context, account *Account, model string) []string {
	if codeBuddyQuotaInstance == nil {
		return nil
	}
	return codeBuddyQuotaInstance.SupportedEffortsForModel(ctx, account, model)
}

func (s *CodeBuddyQuotaService) doBillingRequest(ctx context.Context, account *Account, method, path string, body []byte) ([]byte, int, error) {
	baseURL := CodeBuddyBillingBaseURL
	if strings.Contains(path, codeBuddyModelsPath) {
		baseURL = CodeBuddyModelsBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	s.setBillingHeaders(req, account)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	var resp *http.Response
	if s.httpUpstream != nil {
		resp, err = s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	} else {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err = client.Do(req)
	}
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return raw, resp.StatusCode, fmt.Errorf("codebuddy billing: http %d", resp.StatusCode)
	}
	// 业务信封：code != 0 视为业务错误。
	if code := gjson.GetBytes(raw, "code"); code.Exists() && code.Int() != 0 {
		return raw, resp.StatusCode, fmt.Errorf("codebuddy billing: code=%d msg=%s", code.Int(), gjson.GetBytes(raw, "msg").String())
	}
	return raw, resp.StatusCode, nil
}

func (s *CodeBuddyQuotaService) setBillingHeaders(req *http.Request, account *Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", CodeBuddyOriginReferer)
	req.Header.Set("Referer", CodeBuddyOriginReferer+"/")
	req.Header.Set("User-Agent", CodeBuddyClientUA)
	req.Header.Set("X-Product", "SaaS")

	accessToken := account.GetCredential("access_token")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	uid := account.GetCredential("uid")
	enterpriseID := account.GetCredential("enterprise_id")
	domain := account.GetCredential("domain")
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if domain != "" {
		req.Header.Set("X-Domain", domain)
	} else {
		req.Header.Set("X-No-Domain", "1")
	}
}

// extractCreditPercent 从计费响应的 data 中按候选字段名扫描积分已用百分比（0-100+）。
// 兼容 camelCase/snake_case 多种命名；仅取第一个命中且为数值的字段。
func (s *CodeBuddyQuotaService) extractCreditPercent(body []byte) (float64, bool) {
	data := gjson.GetBytes(body, "data")
	if !data.Exists() {
		data = gjson.ParseBytes(body)
	}
	candidates := []string{
		"creditUsedPercent", "credit_used_percent", "usedPercent", "used_percent",
		"creditPercent", "credit_percent", "percent", "usedCreditPercent", "used_credit_percent",
	}
	for _, key := range candidates {
		if v := data.Get(key); v.Exists() {
			if p, ok := toPercentValue(v); ok {
				return p, true
			}
		}
	}
	return 0, false
}

// extractCreditAmount 从 data 中按候选字段名提取积分金额（total/used）。
func (s *CodeBuddyQuotaService) extractCreditAmount(body []byte, keys ...string) (float64, bool) {
	data := gjson.GetBytes(body, "data")
	if !data.Exists() {
		data = gjson.ParseBytes(body)
	}
	for _, key := range keys {
		if v := data.Get(key); v.Exists() {
			if f, ok := toFloatValue(v); ok {
				return f, true
			}
		}
	}
	return 0, false
}

// extractResetTime 从 data 中按候选字段名提取积分重置时间（RFC3339 或 unix 秒/毫秒）。
func (s *CodeBuddyQuotaService) extractResetTime(body []byte) time.Time {
	data := gjson.GetBytes(body, "data")
	if !data.Exists() {
		data = gjson.ParseBytes(body)
	}
	candidates := []string{
		"resetTime", "reset_time", "resetAt", "reset_at",
		"expireTime", "expire_time", "expireAt", "expire_at",
	}
	for _, key := range candidates {
		if v := data.Get(key); v.Exists() {
			if t, ok := parseCreditTime(v); ok {
				return t
			}
		}
	}
	return time.Time{}
}

func (s *CodeBuddyQuotaService) persistError(ctx context.Context, accountID int64, errMsg string) {
	if s.accountRepo == nil {
		return
	}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
		codebuddyCreditErrorKey: errMsg,
	}); err != nil {
		slog.Warn("codebuddy_quota_error_persist_failed", "account_id", accountID, "error", err)
	}
}

// toPercentValue 兼容 0-1 比例与 0-100 百分比两种表达。
func toPercentValue(v gjson.Result) (float64, bool) {
	switch v.Type {
	case gjson.Number:
		f := v.Float()
		if f >= 0 && f <= 1 {
			return f * 100, true
		}
		if f >= 0 {
			return f, true
		}
	case gjson.String:
		if f, err := parseCreditTimeAsFloat(v.String()); err == nil {
			if f >= 0 && f <= 1 {
				return f * 100, true
			}
			if f >= 0 {
				return f, true
			}
		}
	}
	return 0, false
}

func toFloatValue(v gjson.Result) (float64, bool) {
	switch v.Type {
	case gjson.Number:
		return v.Float(), true
	case gjson.String:
		if f, err := parseCreditTimeAsFloat(v.String()); err == nil {
			return f, true
		}
	}
	return 0, false
}

func parseCreditTimeAsFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscan(strings.TrimSpace(s), &f)
	return f, err
}

func parseCreditTime(v gjson.Result) (time.Time, bool) {
	// 字符串：优先 RFC3339，其次 unix 秒/毫秒。
	if v.Type == gjson.String {
		s := strings.TrimSpace(v.String())
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, true
		}
		if f, err := parseCreditTimeAsFloat(s); err == nil {
			return unixToTime(f), true
		}
		return time.Time{}, false
	}
	if v.Type == gjson.Number {
		return unixToTime(v.Float()), true
	}
	return time.Time{}, false
}

func unixToTime(f float64) time.Time {
	if f > 1e12 {
		return time.UnixMilli(int64(f))
	}
	return time.Unix(int64(f), 0)
}
