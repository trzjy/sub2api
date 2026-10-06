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
	"github.com/shopspring/decimal"
	"github.com/tidwall/gjson"
)

// CodeBuddy 计费/积分与模型列表端点路径（§2.1）。域名/Origin/Referer 由站点表
// （codebuddy_site.go）按账号 site 选取；路径与站点无关。
const (
	codeBuddyBillingMeterPath = "/v2/billing/meter/get-user-resource"
	codeBuddyDailyCheckinPath = "/v2/billing/meter/daily-checkin"
	codeBuddyModelsPath       = "/console/enterprises/personal/models"

	// Extra 快照键（CodeBuddyQuotaService 周期写入，调度阈值评估与 UI 消费）。
	// 旧总量键 codebuddy_credit_total/used/updated_at 已随 §4.1 分包化停写并
	// 删除（Card C，旧键归零；存量数据由 264 同批不动，UI 不再消费）。
	codebuddyCreditUsedPercentKey = "codebuddy_credit_used_percent"
	codebuddyCreditResetAtKey     = "codebuddy_credit_reset_at"
	// codebuddyCreditErrorKey 是错误标记键（§4.1 SSOT = codebuddy_credit_error；
	// 旧名 codebuddy_quota_error 废弃）。存量数据迁移与读写端切换由 Card E 执行，
	// 本卡只做常量层改名（零新增 DB 调用）。
	codebuddyCreditErrorKey = "codebuddy_credit_error"
	// codebuddyCreditPackagesKey 是 §4.1 分包快照数组键（存储契约）。
	codebuddyCreditPackagesKey = "codebuddy_credit_packages"

	// codebuddyCreditPackagesUpdatedAtKey 是分包快照的唯一 freshness 依据
	// （§1 / §4.1：codebuddy_credit_packages_updated_at，失败绝不更新）。
	// 与 codebuddy_urgency.go 同名常量一致，集中定义避免双源。
	codebuddyCreditPackagesUpdatedAtKey = "codebuddy_credit_packages_updated_at"
	// codebuddyCreditLastAttemptAtKey 是失败/成功尝试时间键（§1 统一排序键
	// 失败路径比较时间持久化于此；成功事务同事务写为 success_time）。
	codebuddyCreditLastAttemptAtKey = "codebuddy_credit_last_attempt_at"
	// codebuddyCreditVersionKey 是最近一次被接受事件的 attempt_version
	// （§1 R6 回修 #1：仅保存被接受事件版本，成功/失败提交均不得再自增）。
	codebuddyCreditVersionKey = "codebuddy_credit_version"

	// 导出别名：供 repository 包条件更新实现引用（单一来源，不自创键名）。
	// 条件更新 SQL 内只合并 B4 负责的键、保留其余 extra 内容。
	CodeBuddyCreditPackagesKey          = codebuddyCreditPackagesKey
	CodeBuddyCreditPackagesUpdatedAtKey = codebuddyCreditPackagesUpdatedAtKey
	CodeBuddyCreditResetAtKey           = codebuddyCreditResetAtKey
	CodeBuddyCreditUsedPercentKey       = codebuddyCreditUsedPercentKey
	CodeBuddyCreditLastAttemptAtKey     = codebuddyCreditLastAttemptAtKey
	CodeBuddyCreditVersionKey           = codebuddyCreditVersionKey
	CodeBuddyCreditErrorKey             = codebuddyCreditErrorKey

	// codebuddyCreditCanonicalUnit 是 §4.1 addendum（2026-09-29 探针）钉死的规范单位
	// （cn 站真实账号 11/11 分包唯一值）；异单位分包失败关闭。
	codebuddyCreditCanonicalUnit = "credits"
	// Status 合法枚举（§4.1 addendum 实测）：0 = 在用、3 = 耗尽/过期；其余未知值失败关闭。
	codebuddyCreditPackageStatusActive    = 0
	codebuddyCreditPackageStatusExhausted = 3

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

	// creditWriter 是 Card B 唯一条件更新写口（accountRepository 实现）。
	// 通过构造时类型断言注入；nil 时写路径失败关闭（不静默降级为 UpdateExtra）。
	creditWriter CodeBuddyConditionalExtraWriter

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
	// Card B：快照写路径必须走条件更新入口（§0.2 UpdateExtra 不足）。
	// 真实 accountRepository 实现 CodeBuddyConditionalExtraWriter；断言失败
	// 时 creditWriter 保持 nil，queryUsageForAccount 检测到即失败关闭。
	if writer, ok := accountRepo.(CodeBuddyConditionalExtraWriter); ok {
		s.creditWriter = writer
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
	// Card B：成功/失败写路径必须走唯一条件更新入口。creditWriter 未注入时
	// 失败关闭（不得静默降级回 UpdateExtra）。
	if s.creditWriter == nil {
		result.Error = "codebuddy credit writer not configured"
		return result, nil
	}
	// 抓取开始：attempt_time 采样 + attempt_version 取号（同一采样点）。
	// attempt_version 由 DB sequence 分配；取号失败 = 本次抓取失败关闭
	// （禁应用本地计数器）。
	attemptTime := time.Now().UTC().Truncate(time.Microsecond)
	attemptVersion, err := s.creditWriter.NextCodeBuddyCreditAttemptVersion(ctx)
	if err != nil {
		result.Error = "codebuddy credit attempt version acquisition failed: " + err.Error()
		return result, nil
	}

	body, status, err := s.doBillingRequest(ctx, account, http.MethodPost, codeBuddyBillingMeterPath, []byte("{}"))
	result.StatusCode = status
	if err != nil {
		result.Error = err.Error()
		s.persistAttemptError(ctx, account.ID, attemptTime, attemptVersion, err.Error())
		return result, nil
	}
	// Card A 分包解析（§1 契约，校验失败关闭）：返回分包集合 + 本次解析错误
	// 条目（保留原始值供排查）。envelope 结构缺失 → 失败关闭。
	packages, parseErrors, envelopeErr := parseCodeBuddyCreditPackages(body, account.GetCredential("uid"))
	// 上线观测（§0.1 #8）：无效分包错误计数（结构化 slog，复用既有日志路径）。
	if len(parseErrors) > 0 {
		slog.Warn("codebuddy_credit_package_parse_errors", "account_id", account.ID, "count", len(parseErrors))
	}
	if envelopeErr {
		errMsg := "codebuddy quota: 无法从 get-user-resource 响应解析账号容量计数器"
		result.Error = errMsg
		s.persistAttemptError(ctx, account.ID, attemptTime, attemptVersion, errMsg)
		return result, nil
	}
	// Σtotal=0（无有效分包可参与基础快照有效性谓词）→ 不写假百分比，失败关闭。
	usedPercent, usable := DeriveCodeBuddyCreditUsedPercent(packages)
	if !usable {
		errMsg := "codebuddy quota: 无有效分包可参与 used_percent 推导（Σtotal=0）"
		result.Error = errMsg
		s.persistAttemptError(ctx, account.ID, attemptTime, attemptVersion, errMsg)
		return result, nil
	}
	resetAt := deriveResetAtFromPackages(packages)
	if resetAt.IsZero() {
		resetAt = time.Now().Add(codebuddyCreditDefaultResetWindow)
	}
	// 成功时刻 = 快照完成时刻（与 version 同实例，应用侧截断微秒）。
	successTime := time.Now().UTC().Truncate(time.Microsecond)
	// 本次解析错误结果：无错误(errEntries 为空)才清除旧错误；有错误随成功
	// 快照同事务写入。
	errEntriesJSON := marshalCodeBuddyPackageErrors(parseErrors)
	accepted, err := s.creditWriter.WriteCodeBuddyCreditSnapshot(ctx, account.ID, CodeBuddyCreditSnapshotWrite{
		SuccessTime: successTime,
		Version:     attemptVersion,
		Packages:    packages,
		UsedPercent: usedPercent,
		ResetAt:     resetAt,
		ErrorMsg:    errEntriesJSON,
	})
	if err != nil {
		slog.Warn("codebuddy_quota_snapshot_failed", "account_id", account.ID, "error", err)
		result.Error = "snapshot write failed: " + err.Error()
		return result, nil
	}
	// RowsAffected=0 = 条件更新被拒绝（排序键不通过）。调用方按失败路径处理，
	// 不得重试改写比较条件；不产生成功状态变更。
	if !accepted {
		// 上线观测（§0.1 #8）：条件更新拒绝计数（结构化 slog）。调度侧调用方
		// 丢弃 result.Error，此处不落日志则拒绝完全静默。
		slog.Warn("codebuddy_credit_snapshot_rejected_by_ordering_tuple", "account_id", account.ID, "version", attemptVersion)
		result.Error = "codebuddy credit snapshot rejected by ordering tuple"
		return result, nil
	}
	result.Success = true
	result.Persisted = true
	result.UsedPercent = usedPercent
	result.TotalCredit, result.UsedCredit = deriveTotalsFromPackages(packages)
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
	// 管理员可配置静态模型 ID 列表以绕过该端点；空配置保持原有错误降级行为。
	if account.CodeBuddySite() == CodeBuddySiteIntl {
		if s.cfg != nil {
			if models := parseCodeBuddyStaticModels(s.cfg.Gateway.CodeBuddy.StaticModelsIntl); len(models) > 0 {
				s.modelCacheMu.Lock()
				if s.modelCache == nil {
					s.modelCache = make(map[int64]codeBuddyModelCacheEntry)
				}
				s.modelCache[accountID] = codeBuddyModelCacheEntry{models: models, fetchedAt: time.Now()}
				s.modelCacheMu.Unlock()
				return models, nil
			}
		}
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
	if s.modelCache == nil {
		s.modelCache = make(map[int64]codeBuddyModelCacheEntry)
	}
	s.modelCache[accountID] = codeBuddyModelCacheEntry{models: models, fetchedAt: time.Now()}
	s.modelCacheMu.Unlock()
	return models, nil
}

func parseCodeBuddyStaticModels(raw string) []CodeBuddyModel {
	seen := make(map[string]struct{})
	models := make([]CodeBuddyModel, 0)
	for _, item := range strings.Split(raw, ",") {
		id := strings.TrimSpace(item)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, CodeBuddyModel{ID: id})
	}
	return models
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
	if accessToken == "" && account.Type == AccountTypeAPIKey {
		// APIKey 型（ck_ 定制 key）：token 存于 credentials.api_key。
		accessToken = account.GetCredential("api_key")
	}
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
		// §4.1 唯一具名例外：credentials.domain 为空时，chat/billing 出站不发送
		// X-Domain，改发占位头 X-No-Authorization: 1（上游实测契约即此头名，
		// 不按 §0 通用规则推导出 X-No-Domain）。
		req.Header.Set("X-No-Authorization", "1")
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

	selected := codeBuddySelectResourceAccounts(accounts, uid)

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

// codeBuddySelectResourceAccounts 选取本次快照参与聚合的资源账号：优先按 uid 命中，
// 命不中时退回响应中的全部账号（请求以 X-User-Id=uid 认证，返回的 Accounts[] 本就
// 属于当前用户）。抽取为共享助手，供新旧两条解析路径复用同一匹配语义。
func codeBuddySelectResourceAccounts(accounts []gjson.Result, uid string) []gjson.Result {
	if strings.TrimSpace(uid) == "" {
		return accounts
	}
	selected := make([]gjson.Result, 0, len(accounts))
	for _, acc := range accounts {
		if codeBuddyAccountMatchesUID(acc, uid) {
			selected = append(selected, acc)
		}
	}
	if len(selected) == 0 {
		return accounts
	}
	return selected
}

// CodeBuddyCreditPackage 是 §4.1 存储契约定义的单条分包快照
// （Extra 键 codebuddy_credit_packages 数组的元素）。
//
// 字段名以权威 §4.1 为准，不得自创：id/name/unit/remaining/total/expires_at/status。
// remaining/total 用 decimal 承载 NUMERIC(20,8) 口径，序列化为 JSON 字符串以避免
// float64 在传输/前端解析阶段的精度损失。
type CodeBuddyCreditPackage struct {
	ID        string
	Name      string
	Unit      string
	Remaining decimal.Decimal
	Total     decimal.Decimal
	ExpiresAt string // UTC RFC3339 字面量
	Status    int64
}

func (p CodeBuddyCreditPackage) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Unit      string `json:"unit"`
		Remaining string `json:"remaining"`
		Total     string `json:"total"`
		ExpiresAt string `json:"expires_at"`
		Status    int64  `json:"status"`
	}{
		ID:        p.ID,
		Name:      p.Name,
		Unit:      p.Unit,
		Remaining: p.Remaining.String(),
		Total:     p.Total.String(),
		ExpiresAt: p.ExpiresAt,
		Status:    p.Status,
	})
}

// codeBuddyCreditPackageError 记录一个被整包剔除的分包（保留原始值供排查）。
// 错误结构仅用于组装返回，本卡不落库（写路径由 Card B 单点替换）。
type codeBuddyCreditPackageError struct {
	ID        string
	Reason    string
	RawValues map[string]string
}

// parseCodeBuddyCreditPackages 按 §4.1 契约解析 get-user-resource 响应中的分包条目。
//
// 若 data.Response.Data 结构缺失/Accounts 为空，返回 errorFlag=true 且空集合
// （调用方据此写错误标记）。uid 非空时先按 uid 选取账号；命不中退回全部账号。
//
// 校验失败关闭（禁兜底/钳位）：unit≠credits、负值、remaining>total、未知 Status
// 一律整包剔除并计入 errorEntries（保留原始值）。TotalDosage/ExpiredTime 不消费。
func parseCodeBuddyCreditPackages(body []byte, uid string) ([]CodeBuddyCreditPackage, []codeBuddyCreditPackageError, bool) {
	root := gjson.GetBytes(body, "data.Response.Data")
	if !root.Exists() {
		return nil, nil, true
	}
	accounts := root.Get("Accounts").Array()
	if len(accounts) == 0 {
		return nil, nil, true
	}
	selected := codeBuddySelectResourceAccounts(accounts, uid)

	packages := make([]CodeBuddyCreditPackage, 0, len(selected))
	errEntries := make([]codeBuddyCreditPackageError, 0)
	for _, acc := range selected {
		pkg, errEntry, ok := parseCodeBuddyCreditPackage(acc)
		if !ok {
			errEntries = append(errEntries, errEntry)
			continue
		}
		packages = append(packages, pkg)
	}
	return packages, errEntries, false
}

// parseCodeBuddyCreditPackage 解析单个分包条目。ok=false 时 errorEntry 记录剔除原因
// 与原始值（不落库，仅组装）。
func parseCodeBuddyCreditPackage(acc gjson.Result) (CodeBuddyCreditPackage, codeBuddyCreditPackageError, bool) {
	id := acc.Get("AccountId").String()
	name := acc.Get("PackageName").String()
	unit := acc.Get("CapacityUnit").String()
	status := acc.Get("Status").Int()
	rawValues := map[string]string{
		"unit":      unit,
		"status":    strconv.FormatInt(status, 10),
		"remaining": acc.Get("CapacityRemainPrecise").String(),
		"total":     acc.Get("CapacitySizePrecise").String(),
	}
	if status != codebuddyCreditPackageStatusActive && status != codebuddyCreditPackageStatusExhausted {
		return CodeBuddyCreditPackage{}, codeBuddyCreditPackageError{
			ID:        id,
			Reason:    "unknown_status",
			RawValues: rawValues,
		}, false
	}
	if unit != codebuddyCreditCanonicalUnit {
		return CodeBuddyCreditPackage{}, codeBuddyCreditPackageError{
			ID:        id,
			Reason:    "unit_mismatch",
			RawValues: rawValues,
		}, false
	}
	remaining, err := parseCodeBuddyCreditDecimal(acc.Get("CapacityRemainPrecise").String())
	if err != nil {
		return CodeBuddyCreditPackage{}, codeBuddyCreditPackageError{
			ID:        id,
			Reason:    "invalid_remaining",
			RawValues: rawValues,
		}, false
	}
	total, err := parseCodeBuddyCreditDecimal(acc.Get("CapacitySizePrecise").String())
	if err != nil {
		return CodeBuddyCreditPackage{}, codeBuddyCreditPackageError{
			ID:        id,
			Reason:    "invalid_total",
			RawValues: rawValues,
		}, false
	}
	if remaining.IsNegative() || total.IsNegative() {
		return CodeBuddyCreditPackage{}, codeBuddyCreditPackageError{
			ID:        id,
			Reason:    "negative_value",
			RawValues: rawValues,
		}, false
	}
	if remaining.GreaterThan(total) {
		return CodeBuddyCreditPackage{}, codeBuddyCreditPackageError{
			ID:        id,
			Reason:    "remaining_gt_total",
			RawValues: rawValues,
		}, false
	}
	// expires_at 非法/缺失**不整包剔除**：§1 的失败关闭清单仅含 unit/数值/Status，
	// 不含到期时间；§4.2 明确"expires_at 缺失/非法的分包不计入（紧迫度参与集合）"，
	// 即该分包仍进入存储（供展示与 used_percent 求和），仅不参与紧迫度窗口。
	expiresAt := ""
	if t, ok := parseCodeBuddyCycleEnd(acc.Get("CycleEndTime").String()); ok {
		expiresAt = t.UTC().Format(time.RFC3339)
	}
	return CodeBuddyCreditPackage{
		ID:        id,
		Name:      name,
		Unit:      unit,
		Remaining: remaining,
		Total:     total,
		ExpiresAt: expiresAt,
		Status:    status,
	}, codeBuddyCreditPackageError{}, true
}

// parseCodeBuddyCreditDecimal 把容量字段解析为 decimal 并按 §4.1 addendum 存储口径
// 量化到 NUMERIC(20,8)。源字段为整数 + 同名 *Precise 定点字符串变体，解析源 =
// *Precise。仅接受 decimal 字符串单一口径（与存储契约 MarshalJSON 对称）；
// 解析失败失败关闭，不做 float64/量化兜底。
func parseCodeBuddyCreditDecimal(raw string) (decimal.Decimal, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return decimal.Zero, fmt.Errorf("codebuddy credit: empty amount")
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("codebuddy credit: invalid amount %q: %w", s, err)
	}
	return d.Round(int32(UsageBillingMonetaryScale)), nil
}

// IsValidCodeBuddyCreditPackage 是**基础快照有效性谓词**（唯一实现，Card D 复用）。
// 条件 = 字段有效（单位/数值/Status 校验通过）+ 非 Status=3（耗尽/过期）。
// 不含时间窗口/新鲜度条件（紧迫度参与性谓词由 Card D 在此基础之上叠加）。
func IsValidCodeBuddyCreditPackage(pkg CodeBuddyCreditPackage) bool {
	if pkg.Unit != codebuddyCreditCanonicalUnit {
		return false
	}
	// 字段有效：Status 必须为已知枚举；合法枚举仅 0（在用）/3（耗尽/过期），
	// 故基础谓词只接纳 Status=0——Status=3 与未知值（非法字段）均不参与。
	if pkg.Status != codebuddyCreditPackageStatusActive {
		return false
	}
	if pkg.Remaining.IsNegative() || pkg.Total.IsNegative() {
		return false
	}
	if pkg.Remaining.GreaterThan(pkg.Total) {
		return false
	}
	return true
}

// DeriveCodeBuddyCreditUsedPercent 从参与分包（基础快照有效性谓词）推导已用比例。
// 公式钉死 = (Σtotal−Σremaining)/Σtotal×100。Σtotal=0（无有效分包）→ ok=false，
// 表示"不写入 + 错误标记"信号（禁静默写 0 伪装满额/空额）。
func DeriveCodeBuddyCreditUsedPercent(packages []CodeBuddyCreditPackage) (float64, bool) {
	var sumTotal, sumRemaining decimal.Decimal
	for _, pkg := range packages {
		if !IsValidCodeBuddyCreditPackage(pkg) {
			continue
		}
		sumTotal = sumTotal.Add(pkg.Total)
		sumRemaining = sumRemaining.Add(pkg.Remaining)
	}
	if sumTotal.LessThanOrEqual(decimal.Zero) {
		return 0, false
	}
	used := sumTotal.Sub(sumRemaining)
	res, _ := used.Div(sumTotal).Mul(decimal.NewFromInt(100)).Float64()
	return res, true
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

// persistAttemptError 走失败事务条件更新：仅 last_attempt_at + 错误标记 +
// version 持久化（不得修改 freshness/reset_at/成功快照）。接受条件 = 同一
// 字典序 tuple (AttemptTime, Version)；accepted=false（RowsAffected=0）= 被拒，
// 按失败路径处理（不重试改写比较条件）。creditWriter 为 nil 时无法写失败
// 记录，仅记日志（此时 process 启动即失败关闭，正常不会到达）。
func (s *CodeBuddyQuotaService) persistAttemptError(ctx context.Context, accountID int64, attemptTime time.Time, version int64, errMsg string) {
	if s.creditWriter == nil {
		slog.Warn("codebuddy_credit_attempt_error_skipped_unconfigured", "account_id", accountID, "error", errMsg)
		return
	}
	if _, err := s.creditWriter.WriteCodeBuddyCreditAttemptError(ctx, accountID, CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: attemptTime,
		Version:     version,
		ErrorMsg:    errMsg,
	}); err != nil {
		slog.Warn("codebuddy_credit_error_persist_failed", "account_id", accountID, "error", err)
	}
}

// deriveResetAtFromPackages 从分包快照推导 reset_at（§1：源 = CycleEndTime，
// 已由 Card A 转 UTC RFC3339 存于 ExpiresAt）。取可解析且最晚的未来值；
// 全部缺失/非法 → 返回零值（调用方回退默认窗口，与缺失探针 fail-open 一致）。
func deriveResetAtFromPackages(packages []CodeBuddyCreditPackage) time.Time {
	var latest time.Time
	now := time.Now().UTC()
	for _, pkg := range packages {
		if pkg.ExpiresAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, pkg.ExpiresAt)
		if err != nil {
			continue
		}
		t = t.UTC()
		if !t.After(now) {
			continue
		}
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

// deriveTotalsFromPackages 从参与基础快照有效性谓词的分包推导合计
// （旧 total/used 展示退役后仍为 probe result 提供兼容聚合值；异单位/
// 无效分包不计入，与 used_percent 分子分母同一集合）。
func deriveTotalsFromPackages(packages []CodeBuddyCreditPackage) (total, used float64) {
	for _, pkg := range packages {
		if !IsValidCodeBuddyCreditPackage(pkg) {
			continue
		}
		total += pkg.Total.InexactFloat64()
		used += pkg.Total.Sub(pkg.Remaining).InexactFloat64()
	}
	return total, used
}

// marshalCodeBuddyPackageErrors 把本次解析错误条目序列化为错误标记内容
// （保留原始值供排查）；无错误返回空串（调用方据此清除旧错误）。
func marshalCodeBuddyPackageErrors(entries []codeBuddyCreditPackageError) string {
	if len(entries) == 0 {
		return ""
	}
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		items = append(items, map[string]any{
			"id":     e.ID,
			"reason": e.Reason,
			"raw":    e.RawValues,
		})
	}
	b, err := json.Marshal(items)
	if err != nil {
		slog.Warn("codebuddy_credit_package_errors_marshal_failed", "entries", len(entries), "error", err)
		return ""
	}
	return string(b)
}

// CodeBuddyConditionalExtraWriter 是 CodeBuddy 快照专用条件更新入口（Card B
// 唯一新写口；§0.2 已证 UpdateExtra 无条件谓词不足，本窄接口不做通用扩展）。
// 由 accountRepository 实现；CodeBuddyQuotaService 通过构造时类型断言持有。
// 所有时间值由应用进程生成（time.Now().UTC().Truncate(time.Microsecond)），
// 比较值与写入值同一实例；RowsAffected=0 即条件更新被拒绝（调用方按失败
// 路径处理，不得重试改写比较条件）。
type CodeBuddyConditionalExtraWriter interface {
	// NextCodeBuddyCreditAttemptVersion 在抓取开始时取得 DB 分配的单调号
	// （sequence seq_codebuddy_credit_attempt_version）。调用失败 = 本次抓取
	// 失败关闭（禁应用本地计数器）。
	NextCodeBuddyCreditAttemptVersion(ctx context.Context) (int64, error)
	// WriteCodeBuddyCreditSnapshot 提交成功事务：分包快照 + packages_updated_at
	// + reset_at + used_percent + last_attempt_at(=SuccessTime) + version +
	// 解析错误结果（ErrorMsg 为空则清除旧错误）。接受条件 = tuple 语义
	// (event_time, version)，成功路径 event_time = SuccessTime。原子：条件判断、
	// 写入、version 写入在同一 UPDATE。
	WriteCodeBuddyCreditSnapshot(ctx context.Context, accountID int64, write CodeBuddyCreditSnapshotWrite) (accepted bool, err error)
	// WriteCodeBuddyCreditAttemptError 提交失败事务：仅 last_attempt_at +
	// 错误标记 + version 持久化，不得修改 freshness/reset_at/成功快照。
	// 接受条件 tuple 语义，失败路径 event_time = AttemptTime。
	WriteCodeBuddyCreditAttemptError(ctx context.Context, accountID int64, write CodeBuddyCreditAttemptErrorWrite) (accepted bool, err error)
}

// CodeBuddyCreditSnapshotWrite 是成功事务条件更新写入参数（§1 统一排序键）。
type CodeBuddyCreditSnapshotWrite struct {
	// SuccessTime 是该次成功快照完成时刻（应用侧截断微秒）。
	SuccessTime time.Time
	// Version 是抓取开始时取得的 attempt_version；成功提交不得另行自增。
	Version int64
	// Packages 是 §4.1 分包快照数组（存储契约，decimal 字符串承载）。
	Packages []CodeBuddyCreditPackage
	// UsedPercent 从参与分包（基础快照有效性谓词）推导。
	UsedPercent float64
	// ResetAt 语义保留（阈值消费依赖）；成功事务继续写。
	ResetAt time.Time
	// ErrorMsg 非空 = 本次解析错误结果随快照同事务写入；空 = 清除旧错误。
	ErrorMsg string
}

// CodeBuddyCreditAttemptErrorWrite 是失败事务条件更新写入参数。
type CodeBuddyCreditAttemptErrorWrite struct {
	// AttemptTime 是本次抓取请求开始时刻（与 attempt_version 同点采样）。
	AttemptTime time.Time
	// Version 是抓取开始时取得的 attempt_version；失败提交不得另行自增。
	Version int64
	// ErrorMsg 是失败标记内容（写入 codebuddy_credit_error）。
	ErrorMsg string
}
