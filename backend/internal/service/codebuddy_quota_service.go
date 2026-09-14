package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
)

// CodeBuddy 计费/积分与模型列表端点路径（§2.1）。域名/Origin/Referer 由站点表
// （codebuddy_site.go）按账号 site 选取；路径与站点无关。
const (
	codeBuddyBillingMeterPath = "/v2/billing/meter/get-user-resource"
	codeBuddyDailyCheckinPath = "/v2/billing/meter/daily-checkin"
	codeBuddyModelsPath       = "/console/enterprises/personal/models"

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
	ID               string                `json:"id"`
	Name             string                `json:"name"`
	MaxInputTokens   int64                 `json:"maxInputTokens"`
	MaxOutputTokens  int64                 `json:"maxOutputTokens"`
	Disabled         bool                  `json:"disabled"`
	// SupportedEfforts 兼容顶层 supportedEfforts；真实报文亦可能把档位放在
	// reasoning.supportedEfforts（见 CodeBuddyReasoningInfo），读取统一走 EffortLevels()。
	SupportedEfforts []string                `json:"supportedEfforts"`
	Reasoning        *CodeBuddyReasoningInfo `json:"reasoning,omitempty"`
}

// CodeBuddyReasoningInfo 是模型 reasoning 元数据（§2.3 报文形态之一）。
type CodeBuddyReasoningInfo struct {
	Effort           string   `json:"effort"`
	SupportedEfforts []string `json:"supportedEfforts"`
}

// EffortLevels 返回模型的 reasoning 档位，兼容顶层与 reasoning 嵌套两种报文形态。
// 供 F5 目录快照（codeBuddyUpstreamCatalogBody）与热路径 reasoning_effort 降级统一读取。
func (m CodeBuddyModel) EffortLevels() []string {
	if len(m.SupportedEfforts) > 0 {
		return m.SupportedEfforts
	}
	if m.Reasoning != nil {
		return m.Reasoning.SupportedEfforts
	}
	return nil
}

// CodeBuddyQuotaProbeResult 是积分额度探测的返回结构（管理端 + UI 消费）。
type CodeBuddyQuotaProbeResult struct {
	AccountID   int64   `json:"account_id"`
	Success     bool    `json:"success"`
	UsedPercent float64 `json:"used_percent"`
	ResetAt     string  `json:"reset_at,omitempty"`
	TotalCredit float64 `json:"total_credit,omitempty"`
	UsedCredit  float64 `json:"used_credit,omitempty"`
	StatusCode  int     `json:"status_code,omitempty"`
	FetchedAt   int64   `json:"fetched_at"`
	Persisted   bool    `json:"persisted"`
	Error       string  `json:"error,omitempty"`
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
	usage, ok := parseCodeBuddyCreditUsage(body, account.GetCredential("uid"))
	if !ok {
		// 报文结构异常：记录错误但不写假百分比（避免误触发阈值停调）。
		errMsg := "codebuddy quota: 无法从 get-user-resource 响应解析账号容量计数器"
		result.Error = errMsg
		s.persistError(ctx, account.ID, errMsg)
		return result, nil
	}
	resetAt := usage.ResetAt
	if !usage.HasResetAt || resetAt.IsZero() {
		resetAt = time.Now().Add(codebuddyCreditDefaultResetWindow)
	}
	now := time.Now().UTC()
	updates := map[string]any{
		codebuddyCreditUsedPercentKey: usage.UsedPercent,
		codebuddyCreditResetAtKey:     resetAt.UTC().Format(time.RFC3339),
		codebuddyCreditUpdatedAtKey:   now.Format(time.RFC3339),
		codebuddyCreditErrorKey:       nil,
		codebuddyCreditTotalKey:       usage.TotalCredit,
		codebuddyCreditUsedKey:        usage.UsedCredit,
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("codebuddy_quota_snapshot_failed", "account_id", account.ID, "error", err)
		result.Error = "snapshot write failed: " + err.Error()
		return result, nil
	}
	result.Success = true
	result.Persisted = true
	result.UsedPercent = usage.UsedPercent
	result.TotalCredit = usage.TotalCredit
	result.UsedCredit = usage.UsedCredit
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
	// Phase 0 校准（D4）：intl 站点 models 端点认证后返回 HTTP 500，动态模型列表不可用。
	// 此处直接降级返回错误（调用方 SupportedEffortsForModel 会吞掉并返回空），避免每次
	// 热路径都向上游打一个必 500 的请求。intl 端点若恢复，删除本分支即可自动生效。
	if account.CodeBuddySite() == CodeBuddySiteIntl {
		return nil, fmt.Errorf("codebuddy models: 国际版站点 models 端点不可用（Phase 0 实测 HTTP 500）")
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
	ep := codeBuddyEndpointsFor(account.CodeBuddySite())
	baseURL := ep.BillingBase
	if strings.Contains(path, codeBuddyModelsPath) {
		baseURL = ep.ModelsBase
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
	ep := codeBuddyEndpointsFor(account.CodeBuddySite())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", ep.OriginReferer)
	req.Header.Set("Referer", ep.OriginReferer+"/")
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

// codeBuddyCreditUsage 是 get-user-resource 响应的解析结果。
type codeBuddyCreditUsage struct {
	TotalCredit float64
	UsedCredit  float64
	UsedPercent float64
	ResetAt     time.Time
	HasResetAt  bool
}

// parseCodeBuddyCreditUsage 解析计费端点 get-user-resource 的**真实**响应结构
// （2026-09-13 生产抓包核实，PR-C1）：
//
//	{"code":0,"data":{"Response":{"Data":{
//	    "TotalCount":N,"TotalDosage":M,
//	    "Accounts":[{CapacitySize,CapacityUsed,CapacityRemain,
//	                CycleCapacitySize,CycleCapacityRemain,*Precise,
//	                CycleEndTime,BindRecords[]...}]}}}}
//
// 旧实现按 data.<camelCase 百分比字段> 扫描，与该 schema 完全不符、永远命不中
// （额度快照必然失败、阈值停调失效）。
//
// uid 匹配：真实响应**不含 OAuth uid**（账号对象仅有 AccountId/Uin/ResourceId/
// BindRecords[].BindObjectId），因此按 uid 命不中任何账号；此时退回使用响应中的全部
// 账号——请求以 X-User-Id=uid 认证，返回的 Accounts[] 本就属于当前用户。若上游将来
// 在账号对象中补上 uid 字段，匹配分支即自动生效。
//
// 重置时间：仅采用 CycleEndTime（资源包当前计费周期结束 = 容量重置时刻），取所选账号
// 中最晚的未来值（聚合用量需全部资源包滚动后才归零）。响应中的 DeductionEndTime 是
// 扣费有效期、ExpiredTime 是过期时间，语义均非"重置"，不予采用；CycleEndTime 全部
// 缺失/不可解析时 HasResetAt=false（调用方回退默认窗口），不做猜测。
func parseCodeBuddyCreditUsage(body []byte, uid string) (codeBuddyCreditUsage, bool) {
	var out codeBuddyCreditUsage
	root := gjson.GetBytes(body, "data.Response.Data")
	if !root.Exists() {
		return out, false
	}
	accounts := root.Get("Accounts").Array()
	if len(accounts) == 0 {
		return out, false
	}

	selected := make([]gjson.Result, 0, len(accounts))
	if strings.TrimSpace(uid) != "" {
		for _, acc := range accounts {
			if codeBuddyAccountMatchesUID(acc, uid) {
				selected = append(selected, acc)
			}
		}
	}
	if len(selected) == 0 {
		selected = accounts
	}

	var totalSize, totalUsed float64
	var latestReset time.Time
	usable := false
	for _, acc := range selected {
		size, sizeOK := codeBuddyFirstNumber(acc, "CapacitySize", "CapacitySizePrecise", "CycleCapacitySize", "CycleCapacitySizePrecise")
		if !sizeOK || size <= 0 {
			continue
		}
		used, usedOK := codeBuddyFirstNumber(acc, "CapacityUsed", "CapacityUsedPrecise", "CycleCapacityUsedPrecise")
		if !usedOK {
			// used 缺失时退化为 size - remain（remain 存在才用）。
			if remain, remainOK := codeBuddyFirstNumber(acc, "CapacityRemain", "CapacityRemainPrecise", "CycleCapacityRemain", "CycleCapacityRemainPrecise"); remainOK {
				used, usedOK = size-remain, true
			}
		}
		if !usedOK {
			continue
		}
		totalSize += size
		totalUsed += used
		usable = true
		if t, ok := parseCodeBuddyCycleEnd(acc.Get("CycleEndTime").String()); ok && t.After(latestReset) {
			latestReset = t
		}
	}
	if !usable || totalSize <= 0 {
		return out, false
	}

	out.TotalCredit = totalSize
	out.UsedCredit = totalUsed
	out.UsedPercent = totalUsed / totalSize * 100
	if !latestReset.IsZero() {
		out.ResetAt = latestReset
		out.HasResetAt = true
	}
	return out, true
}

// codeBuddyAccountMatchesUID 判断资源账号对象是否属于给定 uid。真实抓包中响应不含
// uid，各候选身份字段均不可能命中；保留该匹配以便上游补字段后自动生效。
func codeBuddyAccountMatchesUID(acc gjson.Result, uid string) bool {
	for _, key := range []string{"uid", "Uid", "UID", "UserId", "user_id"} {
		if v := strings.TrimSpace(acc.Get(key).String()); v != "" && v == uid {
			return true
		}
	}
	for _, key := range []string{"AccountId", "Uin", "ResourceId"} {
		if v := strings.TrimSpace(acc.Get(key).String()); v != "" && v == uid {
			return true
		}
	}
	for _, rec := range acc.Get("BindRecords").Array() {
		if v := strings.TrimSpace(rec.Get("BindObjectId").String()); v != "" && v == uid {
			return true
		}
	}
	return false
}

// codeBuddyFirstNumber 返回首个存在且可解析为数值的字段（兼容 Number 与数值字符串）。
func codeBuddyFirstNumber(r gjson.Result, keys ...string) (float64, bool) {
	for _, key := range keys {
		v := r.Get(key)
		if !v.Exists() {
			continue
		}
		switch v.Type {
		case gjson.Number:
			return v.Float(), true
		case gjson.String:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v.String()), 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// parseCodeBuddyCycleEnd 解析资源包周期结束时间（上游格式 "2006-01-02 15:04:05"，
// 与 §2.6 一致按 UTC+8 解释；兼容 RFC3339）。
func parseCodeBuddyCycleEnd(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	loc := time.FixedZone("UTC+8", 8*3600)
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
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
