package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

// 国产供应商 payg（按量付费）账号余额探测服务。
//
// 仅覆盖有公开余额端点的供应商：
//   - Kimi/Moonshot：GET https://api.moonshot.cn/v1/users/me/balance (Bearer) → data.available_balance
//   - DeepSeek：     GET https://api.deepseek.com/user/balance (Bearer) → balance_infos[].total_balance + is_available
//
// 智谱（zhipu）无公开余额端点（OpenAPI 规格验证），仅靠响应式 429/402（见
// ratelimit_cn_providers.go）。解析逻辑对齐 cc-switch services/balance.rs::query_deepseek。
const (
	cnBalanceUpstreamTimeout = 15 * time.Second
	cnBalanceMaxBodyBytes    = 256 * 1024

	// Extra 余额快照键后缀（加 provider 前缀）。
	cnBalanceExtraSuffixBalance   = "balance"
	cnBalanceExtraSuffixCurrency  = "balance_currency"
	cnBalanceExtraSuffixAvailable = "balance_available" // deepseek is_available 健康标记
	cnBalanceExtraSuffixUpdated   = "balance_updated_at"
	cnBalanceExtraSuffixBalances  = "balances" // 多币种明细（deepseek USD+CNY）
	// 同程序中转账号（余额探测地址被账号覆盖）：上游为订阅制不限量时置 true，
	// 周期检测据此跳过阈值停调；plan_name 存上游分组名供展示。
	cnBalanceExtraSuffixUnlimited = "balance_unlimited"
	cnBalanceExtraSuffixPlanName  = "balance_plan_name"
	// 上游订阅/额度的到期时间（RFC3339 原文透传，前端格式化展示）。
	cnBalanceExtraSuffixExpiresAt = "balance_expires_at"
	// 订阅用量（同程序中转 /v1/usage 的 subscription.daily/monthly_usage_usd），
	// 订阅制账号没有余额数字，用量是唯一可展示的数值。
	cnBalanceExtraSuffixDailyUsed   = "balance_daily_used"
	cnBalanceExtraSuffixMonthlyUsed = "balance_monthly_used"
	// 余额种类与次卡额度（前后端契约：前端消费 data.value.kind/quota_limit/
	// quota_used 与 extra[<platform>_balance_kind]）。
	cnBalanceExtraSuffixKind       = "balance_kind"
	cnBalanceExtraSuffixQuotaLimit = "balance_quota_limit"
	cnBalanceExtraSuffixQuotaUsed  = "balance_quota_used"
)

// 余额种类（CNProviderBalanceResult.Kind / extra <platform>_balance_kind）取值。
//
//   - subscription：订阅制不限量（/v1/usage remaining<0），无数字余额，展示用量/到期；
//   - count_pack：次卡（次数包），Balance/QuotaLimit/QuotaUsed 的单位是「次」；
//   - balance：数字余额（钱包/额度），Currency 为真实币种。
const (
	CNBalanceKindSubscription = "subscription"
	CNBalanceKindCountPack    = "count_pack"
	CNBalanceKindBalance      = "balance"
)

// CNProviderBalanceEntry 是单一币种的余额明细。
type CNProviderBalanceEntry struct {
	Currency string  `json:"currency"`
	Balance  float64 `json:"balance"`
}

// CNProviderBalanceResult 是余额探测的返回结构（管理端 + UI 消费）。
type CNProviderBalanceResult struct {
	Provider string `json:"provider"`
	Success  bool   `json:"success"`
	// Balance/Currency 为主币种（balance_infos 首条，兼容单币种消费方）；
	// 完整明细见 Balances（deepseek 双币种账号含 CNY + USD 两条）。
	Balance   float64                  `json:"balance"`
	Currency  string                   `json:"currency,omitempty"`
	Balances  []CNProviderBalanceEntry `json:"balances,omitempty"`
	Available bool                     `json:"available"` // 健康标记（deepseek is_available；kimi 无此概念恒 true）
	// Unlimited 标记上游为订阅制不限量（同程序中转 /v1/usage 返回 remaining<0）：
	// 无数字余额可比，周期检测必须跳过阈值停调；展示用 PlanName（上游分组名）。
	Unlimited bool   `json:"unlimited,omitempty"`
	PlanName  string `json:"plan_name,omitempty"`
	// ExpiresAt 为上游订阅/额度的到期时间（RFC3339，原文透传；无则空）。
	ExpiresAt string `json:"expires_at,omitempty"`
	// DailyUsage/MonthlyUsage 为订阅制账号的当日/当月用量（USD）；订阅没有
	// 余额数字，用量是唯一可展示的数值。非订阅账号为 nil。
	DailyUsage   *float64 `json:"daily_usage,omitempty"`
	MonthlyUsage *float64 `json:"monthly_usage,omitempty"`
	// Kind 余额种类（CNBalanceKindSubscription / CNBalanceKindCountPack /
	// CNBalanceKindBalance），前端据此切换展示口径（订阅/次卡/金额）。
	//
	// 次卡（count_pack）：Balance / QuotaLimit / QuotaUsed 的单位是「次」，不是金额。
	// 上游 unit 在次卡下误标 USD 不可信（czc.zh.kg 生产实证：quota.used=295 与
	// usage.today.requests=295 逐位相等，即计数而非美元），故次卡分支 Currency 置
	// 空串、Balances 置空，避免前端把「次」渲染成金额。
	Kind       string  `json:"kind,omitempty"`
	QuotaLimit float64 `json:"quota_limit,omitempty"`
	QuotaUsed  float64 `json:"quota_used,omitempty"`
	// Snapshot 快照读取输出（§4.3 契约键名；Kira/TH 分支填充，供前端一次
	// 拉取。失败关闭路径不落快照也不产出，保持 nil）。
	Snapshot   *CNProviderSnapshotOutput `json:"snapshot,omitempty"`
	StatusCode int                       `json:"status_code,omitempty"`
	FetchedAt  int64                     `json:"fetched_at"`
	Persisted  bool                      `json:"persisted"`
	// SubscriptionExpired 表示「上游订阅已到期/不可用」这一**账号状态**（不是探测故障）：
	// 当订阅型分组 + 上游用户无活跃订阅时，上游 /v1/usage 返回 HTTP 200 但省略
	// remaining/subscription 键，探测本身没坏，坏的是「这个账号在上游的额度没了」——
	// 这是账号的确定性状态（要续费/换号），不是探测故障（HTTP 200、响应结构正常）。
	// 此时确实取不到余额数字（Success=false），但需要管理端展示为状态而非报错；
	// 其它分支恒为 false，JSON 里因 omitempty 不出现该键。
	SubscriptionExpired bool `json:"subscription_expired,omitempty"`
	Error               string `json:"error,omitempty"`
}

// CNProviderBalanceService 探测 Kimi / DeepSeek payg 账号的账户余额。
type CNProviderBalanceService struct {
	accountRepo  AccountRepository
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config
	flight       singleflight.Group
	// thPassService TH（tokenharbor.ai）Pass/用量快照探测链（balance 入口 TH 分支）。
	thPassService *TokenHarborPassService
}

// NewCNProviderBalanceService 构造余额探测服务。
func NewCNProviderBalanceService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *CNProviderBalanceService {
	return &CNProviderBalanceService{
		accountRepo:  accountRepo,
		proxyRepo:    proxyRepo,
		httpUpstream: httpUpstream,
		cfg:          cfg,
		// F1：TH 探测链与 CN 余额链共用同一套出客户端/代理/凭据仓库。
		thPassService: NewTokenHarborPassService(accountRepo, proxyRepo, httpUpstream),
	}
}

// SetTokenHarborPassService 注入 TH Pass/用量探测服务（测试替换假站点用）。
func (s *CNProviderBalanceService) SetTokenHarborPassService(th *TokenHarborPassService) {
	if s != nil {
		s.thPassService = th
	}
}

// QueryBalance 探测指定 payg 账号的余额并落 Extra 快照。
// TH（tokenharbor.ai）账号在此处先行识别（platform 非 CN 供应商，不适用
// payg 校验，F1 生产入口）。
func (s *CNProviderBalanceService) QueryBalance(ctx context.Context, accountID int64) (*CNProviderBalanceResult, error) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusNotFound, "CN_BALANCE_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	return s.QueryBalanceForAccount(ctx, account)
}

// QueryBalanceForAccount 探测已加载账号（配额监控 fetcher / 周期余额检测复用，
// 避免二次 GetByID）。singleflight key 与 QueryBalance 相同，按账号 ID 合并。
// TH（tokenharbor.ai）账号不适用 payg 校验，先于校验分发进 TH 快照分支
// （F1：管理端 balance 探测手动查询按钮的 TH 生产入口）。
// QueryBalanceForAccount 探测已加载账号（配额监控 fetcher / 周期余额检测复用，
// 避免二次 GetByID）。singleflight key 与 QueryBalance 相同，按账号 ID 合并。
//
// 四级路由优先级（BAL-GATE-D1 §3.2 A1）：
//
//	① TH（tokenharbor.ai）专用路径优先：免 payg 校验（F1 生产入口）；
//	② Kira（kiraai.vn）上游：保留 payg 校验（语义不变），校验后走 Kira 分支；
//	③ 中转 relay：唯一选路入口 RelayProbeTargetURL，跳过 payg 校验，apiKey 读取
//	   改 account.GetCredential("api_key")（GetCNAPIKey 对非 CN 平台恒空不可用）；
//	④ 其余：完整 payg 校验 → 官方端点路径（kimi/deepseek 官方余额端点）。
//
// TH/Kira 命中后不经 RelayProbeTargetURL 过滤（助手本体已对二者返回 false）。
func (s *CNProviderBalanceService) QueryBalanceForAccount(ctx context.Context, account *Account) (*CNProviderBalanceResult, error) {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "CN_BALANCE_NOT_CONFIGURED", "cn provider balance service is not configured")
	}
	// ① TH（tokenharbor.ai）专用路径优先：免 payg 校验；未配置 → CN_BALANCE_TH_NOT_CONFIGURED。
	if accountIsTokenHarborBaseURL(account) {
		if s.thPassService == nil {
			return nil, infraerrors.New(http.StatusNotImplemented, "CN_BALANCE_TH_NOT_CONFIGURED", "tokenharbor pass service is not configured")
		}
		return s.runCNBalanceProbe(ctx, account.ID, func(probeCtx context.Context) (any, error) {
			return s.queryTokenHarborBalanceForAccount(probeCtx, account)
		})
	}
	// ② Kira（kiraai.vn）上游：保留 payg 校验（validatePayGAccount 对 Kira 生效，
	// 语义不变；非 CN platform 命中 Kira URL 仍 CN_BALANCE_INVALID_PLATFORM 且零出站）。
	if accountIsKiraBaseURL(account) {
		if err := validatePayGAccount(account); err != nil {
			return nil, err
		}
		return s.runCNBalanceProbe(ctx, account.ID, func(probeCtx context.Context) (any, error) {
			return s.QueryKiraBalance(probeCtx, account)
		})
	}
	// ③ 中转 relay 选路：唯一入口 RelayProbeTargetURL，跳过 payg 校验。apiKey 读取改
	// account.GetCredential("api_key")（GetCNAPIKey 对非 CN 平台恒空故不可用）。
	if probeURL, ok := RelayProbeTargetURL(account); ok {
		apiKey := strings.TrimSpace(account.GetCredential("api_key"))
		probeCfg := account.BalanceProbeConfig()
		// 空 key 校验只挂在需要 key 的令牌级路径（账户级分支不要求 api_key）。
		if probeCfg.AccessToken == "" && probeCfg.CountUnit <= 0 && !probeCfg.countUnitSet {
			if apiKey == "" {
				return nil, infraerrors.New(http.StatusBadRequest, "CN_BALANCE_NO_APIKEY", "account api_key is empty")
			}
		}
		// 选路结果覆盖 probeCfg.URL（含 base_url + /v1/usage 推导情形），
		// 与旧 queryBalanceForAccount relay 分支的 overrideURL 语义一致。
		probeCfg.URL = probeURL
		return s.runCNBalanceProbe(ctx, account.ID, func(probeCtx context.Context) (any, error) {
			return s.queryRelayBalance(probeCtx, account, apiKey, probeCfg)
		})
	}
	// ①a/①b 上游新增 Command Code / Cline 积分余额与套餐窗口同源，走各自官方端点，
	// 免 payg 平台校验（上游分支，见 command_code_usage.go / cline_usage.go）；fork 既
	// 有 ②③ 路由（TH/Kira/relay）保持不变，本分支仅在非上述专属路径时命中。
	if account.IsCommandCode() {
		return s.runCNBalanceProbe(ctx, account.ID, func(probeCtx context.Context) (any, error) {
			return s.queryCommandCodeBalance(probeCtx, account)
		})
	}
	if account.IsCline() {
		return s.runCNBalanceProbe(ctx, account.ID, func(probeCtx context.Context) (any, error) {
			return s.queryClineBalance(probeCtx, account)
		})
	}
	// ④ 其余：完整 payg 校验 → 官方端点路径（queryBalanceForAccount 仅含
	// kimi/deepseek 官方端点，Kira/relay 分支已上提为 ②③）。
	if err := validatePayGAccount(account); err != nil {
		return nil, err
	}
	return s.runCNBalanceProbe(ctx, account.ID, func(probeCtx context.Context) (any, error) {
		return s.queryBalanceForAccount(probeCtx, account)
	})
}

// runCNBalanceProbe 用 singleflight 合并同一账号的并发余额探测，复用既有
// "cn_balance:<id>" key 与超时/克隆语义（与重构前一致）。
func (s *CNProviderBalanceService) runCNBalanceProbe(ctx context.Context, accountID int64, fn func(probeCtx context.Context) (any, error)) (*CNProviderBalanceResult, error) {
	key := "cn_balance:" + strconv.FormatInt(accountID, 10)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), cnBalanceUpstreamTimeout*3)
		defer cancel()
		return fn(probeCtx)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return nil, flightResult.Err
		}
		result, ok := flightResult.Val.(*CNProviderBalanceResult)
		if !ok || result == nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "CN_BALANCE_PROBE_RESULT_INVALID", "invalid cn provider balance probe result")
		}
		cloned := *result
		return &cloned, nil
	}
}

// RelayProbeTargetURL 是中转 relay 余额探测的唯一选路事实源（BAL-GATE-D1 §3.2 A2）。
//
// 返回的 bool 仅表示"已选中 relay 目标"，不含 allowlist 通过语义——allowlist 校验
// 仍由 queryRelayBalance 内既有 cnValidateProbeURL 执行。TH/Kira 专用路径优先，
// 内嵌于助手本体，不经调用方过滤（调用方已先行分发到 ①②）。
func RelayProbeTargetURL(account *Account) (string, bool) {
	if account == nil {
		return "", false
	}
	// 专用路径优先：TH / Kira 账号不经本助手选路。
	if accountIsTokenHarborBaseURL(account) || accountIsKiraBaseURL(account) {
		return "", false
	}
	probeCfg := account.BalanceProbeConfig()
	if !probeCfg.Enabled {
		return "", false
	}
	// 显式 URL 直接选中（BalanceProbeConfig 已 TrimSpace）。
	if probeCfg.URL != "" {
		return probeCfg.URL, true
	}
	// 否则按 base_url + /v1/usage 推导——官方域名账号（base_url 即官方地址）
	// 不适用，否则官方账号会被错推到不存在的 /v1/usage。
	base := strings.TrimRight(strings.TrimSpace(account.GetCredential("base_url")), "/")
	if base != "" && !cnIsOfficialBalanceHost(account.Platform, base) {
		// base_url 常以 /v1 结尾（如 https://czc.zh.kg/v1）：必须先判后缀再拼，
		// 否则拼成 /v1/v1/usage → 上游 404 → 该站余额快照从不落库。口径与
		// BalanceProbeConfig.normalizedURL（account_balance_probe.go）一致。
		if strings.HasSuffix(base, "/v1") {
			return base + "/usage", true
		}
		return base + "/v1/usage", true
	}
	return "", false
}

func (s *CNProviderBalanceService) queryBalanceForAccount(ctx context.Context, account *Account) (*CNProviderBalanceResult, error) {
	// 仅含 kimi/deepseek 官方余额端点路径（BAL-GATE-D1 §3.2 A1 ④）：
	// Kira 分支上提为 QueryBalanceForAccount ②，relay 选路唯一入口上提为 ③，
	// 均不经此函数。platform gate 与 api_key 校验错误码保持与重构前一致。
	provider := account.Platform
	if provider != PlatformKimi && provider != PlatformDeepseek {
		return nil, infraerrors.New(http.StatusBadRequest, "CN_BALANCE_NO_ENDPOINT", "account provider has no balance endpoint")
	}

	apiKey := strings.TrimSpace(account.GetCNAPIKey())
	if apiKey == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "CN_BALANCE_NO_APIKEY", "account api_key is empty")
	}

	targetURL := cnBalanceURL(account)
	// 探测发起前过出站 URL 安全策略（与网关转发/Grok 探测同一套校验）：
	// DeepSeek 端点由账号 base_url 衍生，不得把 API key 发往策略外主机。
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "CN_BALANCE_URL_REJECTED", err.Error())
	}
	targetURL = validatedURL
	proxyURL := s.resolveProxyURL(ctx, account)
	callCtx, cancel := context.WithTimeout(ctx, cnBalanceUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "CN_BALANCE_REQUEST_BUILD_FAILED", "build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	account.ApplyHeaderOverrides(req.Header)

	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CN_BALANCE_REQUEST_FAILED", "upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, cnBalanceMaxBodyBytes))

	now := time.Now().UTC()
	result := &CNProviderBalanceResult{
		Provider:   provider,
		FetchedAt:  now.Unix(),
		StatusCode: resp.StatusCode,
		Available:  true,
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		result.Error = fmt.Sprintf("Authentication failed (HTTP %d)", resp.StatusCode)
		return result, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Error = fmt.Sprintf("API error (HTTP %d): %s", resp.StatusCode, truncate(strings.TrimSpace(string(bodyBytes)), 240))
		return result, nil
	}

	var entries []CNProviderBalanceEntry
	available := true
	switch provider {
	case PlatformKimi:
		// Moonshot：code==0 成功；data.available_balance（number），单币种 CNY。
		balance, _ := cnParseF64(gjson.GetBytes(bodyBytes, "data.available_balance").Value())
		entries = append(entries, CNProviderBalanceEntry{Currency: "CNY", Balance: balance})
	case PlatformDeepseek:
		// is_available 缺省视为 true（健康）；显式存在时取其值。
		if v := gjson.GetBytes(bodyBytes, "is_available"); v.Exists() {
			available = v.Bool()
		}
		// balance_infos 逐条解析：双币种账号同时返回 CNY + USD（数组顺序即
		// 主次序，首条为主币种）。
		balanceInfos := gjson.GetBytes(bodyBytes, "balance_infos")
		if !balanceInfos.Exists() || !balanceInfos.IsArray() {
			result.Error = "Invalid balance response: missing balance_infos"
			return result, nil
		}
		balanceInfos.ForEach(func(_, info gjson.Result) bool {
			currency := strings.ToUpper(strings.TrimSpace(info.Get("currency").String()))
			totalBalance := info.Get("total_balance")
			balance, ok := cnParseF64(totalBalance.Value())
			if !totalBalance.Exists() || !ok {
				return true
			}
			if currency == "" {
				currency = "CNY"
			}
			entries = append(entries, CNProviderBalanceEntry{Currency: currency, Balance: balance})
			return true
		})
		if len(entries) == 0 {
			result.Error = "Invalid balance response: no valid balance entries"
			return result, nil
		}
	}
	result.Balances = entries
	result.Balance = entries[0].Balance
	result.Currency = entries[0].Currency
	result.Available = available
	result.Success = true

	updates := cnBalanceExtraUpdates(provider, result, now)
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("cn_balance_persist_failed", "account_id", account.ID, "provider", provider, "error", err)
	} else {
		result.Persisted = true
	}
	return result, nil
}

// cnBalanceExtraUpdates 构造 provider 维度的余额快照更新。
func cnBalanceExtraUpdates(provider string, result *CNProviderBalanceResult, now time.Time) map[string]any {
	balanceUpdates := make([]any, 0, len(result.Balances))
	for _, entry := range result.Balances {
		balanceUpdates = append(balanceUpdates, map[string]any{
			"currency": entry.Currency,
			"balance":  entry.Balance,
		})
	}
	return map[string]any{
		cnExtraKey(provider, cnBalanceExtraSuffixBalance):   result.Balance,
		cnExtraKey(provider, cnBalanceExtraSuffixCurrency):  result.Currency,
		cnExtraKey(provider, cnBalanceExtraSuffixAvailable): result.Available,
		cnExtraKey(provider, cnBalanceExtraSuffixUpdated):   now.Format(time.RFC3339),
		cnExtraKey(provider, cnBalanceExtraSuffixBalances):  balanceUpdates,
		// 余额探测成功即清除响应式 402/429 写下的 balance_low 标记。
		cnExtraKey(provider, cnBalanceExtraSuffixLow): false,
	}
}

// queryTokenHarborBalanceForAccount 是 CNProviderBalanceService 的 TH 分支
// （F1 生产入口：管理端 balance 探测手动查询按钮链路）。TH 无数字余额概念，
// 本分支只做 Pass/用量快照刷新并输出 §4.3 快照读取输出；无余额数字，
// Balance/Currency 保持零值，Available=true（快照健康即可用）。
func (s *CNProviderBalanceService) queryTokenHarborBalanceForAccount(ctx context.Context, account *Account) (*CNProviderBalanceResult, error) {
	now := time.Now().UTC()
	result := &CNProviderBalanceResult{
		Provider:  TokenHarborPassProviderName,
		FetchedAt: now.Unix(),
		Available: true,
	}
	passSnapshot, err := s.thPassService.Probe(ctx, account)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	persisted := true
	if err := s.thPassService.PersistSnapshot(ctx, account.ID, passSnapshot); err != nil {
		slog.Warn("th_pass_persist_failed", "account_id", account.ID, "error", err)
		persisted = false
	}
	usageSnapshot, err := s.thPassService.ProbeUsageSnapshot(ctx, account)
	if err != nil {
		// usage CSV 传输/解析失败=失败关闭：不落 th_usage_snapshot，明确错误。
		result.Error = err.Error()
		result.Persisted = persisted
		return result, nil
	}
	if err := s.thPassService.PersistUsageSnapshot(ctx, account.ID, usageSnapshot); err != nil {
		slog.Warn("th_usage_persist_failed", "account_id", account.ID, "error", err)
		persisted = false
	}
	result.Success = true
	result.Persisted = persisted
	result.Snapshot = cnTokenHarborSnapshotOutput(passSnapshot, usageSnapshot)
	return result, nil
}

// queryRelayBalance 探测挂在国产平台下、但实际走第三方中转（同 sub2api 程序）
// 的账号余额：请求账号上配置的余额探测地址，按 sub2api /v1/usage 返回解析。
//
//   - 数字余额（remaining / quota.remaining / balance ≥ 0）：正常落快照，参与阈值停调；
//   - 订阅制不限量（remaining < 0）：置 Unlimited，周期检测跳过阈值停调，展示上游分组名；
//   - 覆盖地址同样经过出站 URL 安全策略校验（cnValidateProbeURL），不绕过 allowlist。
func (s *CNProviderBalanceService) queryRelayBalance(ctx context.Context, account *Account, apiKey string, probeCfg BalanceProbeConfig) (*CNProviderBalanceResult, error) {
	provider := account.Platform
	// 账户级探测分支（New API 系次数包）：配置了 access_token 时令牌级端点
	// 不可用（unlimited key 的 total_available 是残留值），改走
	// /api/subscription/self + /api/user/self。失败关闭，不回退令牌级探测。
	// access_token 与 count_unit 对称校验：配置了其一而缺另一时不落入旧
	// 令牌级路径（残留值会伪装成真数写入快照），直接配置错误失败关闭。
	// count_unit 键显式存在但无效（0/负/非数值）同样视为账户级探测意图，
	// 失败关闭，不静默回落令牌级探测。
	if probeCfg.AccessToken != "" || probeCfg.CountUnit > 0 || probeCfg.countUnitSet {
		if probeCfg.AccessToken == "" || probeCfg.CountUnit <= 0 {
			return nil, infraerrors.New(http.StatusBadRequest, "CN_BALANCE_INVALID_CONFIG", "balance probe access_token and count_unit must be configured together")
		}
		return s.queryRelaySubscriptionBalance(ctx, account, provider, probeCfg)
	}
	targetURL, err := cnValidateProbeURL(s.cfg, probeCfg.URL)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "CN_BALANCE_URL_REJECTED", err.Error())
	}
	proxyURL := s.resolveProxyURL(ctx, account)
	callCtx, cancel := context.WithTimeout(ctx, cnBalanceUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "CN_BALANCE_REQUEST_BUILD_FAILED", "build request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if probeCfg.BearerAuth {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Set("Authorization", apiKey)
	}
	account.ApplyHeaderOverrides(req.Header)

	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CN_BALANCE_REQUEST_FAILED", "upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, cnBalanceMaxBodyBytes))

	now := time.Now().UTC()
	result := &CNProviderBalanceResult{
		Provider:   provider,
		FetchedAt:  now.Unix(),
		StatusCode: resp.StatusCode,
		Available:  true,
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		result.Error = fmt.Sprintf("Authentication failed (HTTP %d)", resp.StatusCode)
		return result, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Error = fmt.Sprintf("API error (HTTP %d): %s", resp.StatusCode, truncate(strings.TrimSpace(string(bodyBytes)), 240))
		return result, nil
	}

	remaining := firstJSONNumber(bodyBytes, "remaining", "quota.remaining", "balance")
	if remaining == nil {
		// one-api/New API 系中转 token 配额形状（2026-10-08 星思云站
		// xing.xinxinyuntu.top 生产实测）：{"code":bool,"data":{"unlimited_quota":bool,
		// "total_available":num,"name":str,"object":"token_usage"}}。仅当 remaining
		// 键族全缺时回退进入；普通 /v1/usage 响应无 data.unlimited_quota 键，不误伤既有形状。
		if gjson.GetBytes(bodyBytes, "data.unlimited_quota").Exists() {
			dataName := gjson.GetBytes(bodyBytes, "data.name").String()
			if gjson.GetBytes(bodyBytes, "data.unlimited_quota").Bool() {
				// 订阅制不限量：Unlimited 徽标口径不变。total_available=剩余次数
				// （token 配额数），落快照供前端『剩余 N』展示；缺失或非数字不阻断
				// unlimited 成功路径（次数属附加信息），静默跳过保持 Unlimited=true。
				result.Unlimited = true
				result.PlanName = dataName
				result.Kind = CNBalanceKindSubscription
				result.Success = true
				if ta := gjson.GetBytes(bodyBytes, "data.total_available"); ta.Exists() {
					if bal, ok := cnParseF64(ta.Value()); ok {
						result.Balance = bal
						result.Balances = []CNProviderBalanceEntry{{Currency: "USD", Balance: bal}}
					}
				}
			} else {
				totalAvailable := gjson.GetBytes(bodyBytes, "data.total_available")
				if !totalAvailable.Exists() {
					result.Error = "Invalid balance response: one-api shape missing total_available"
					return result, nil
				}
				bal, ok := cnParseF64(totalAvailable.Value())
				if !ok {
					result.Error = "Invalid balance response: one-api shape missing total_available"
					return result, nil
				}
				result.Balance = bal
				result.Balances = []CNProviderBalanceEntry{{Currency: "USD", Balance: bal}}
				result.PlanName = dataName
				// 该分支 total_available 本就是次数（令牌配额数），标为次卡；
				// Currency/Balances 维持现状（USD），不动已上线的 137/138 展示。
				result.Kind = CNBalanceKindCountPack
				result.Success = true
			}
			var balanceUpdates []any
			for _, entry := range result.Balances {
				balanceUpdates = append(balanceUpdates, map[string]any{
					"currency": entry.Currency,
					"balance":  entry.Balance,
				})
			}
			// 共享落库块：result.Balance/result.Balances 已含 total_available
			// 数字快照（unlimited 有次数时），缺失则保持 Unlimited=true 且数字为空。
			updates := map[string]any{
				cnExtraKey(provider, cnBalanceExtraSuffixBalance):   result.Balance,
				cnExtraKey(provider, cnBalanceExtraSuffixCurrency):  result.Currency,
				cnExtraKey(provider, cnBalanceExtraSuffixAvailable): result.Available,
				cnExtraKey(provider, cnBalanceExtraSuffixUpdated):   now.Format(time.RFC3339),
				cnExtraKey(provider, cnBalanceExtraSuffixBalances):  balanceUpdates,
				cnExtraKey(provider, cnBalanceExtraSuffixUnlimited): result.Unlimited,
				cnExtraKey(provider, cnBalanceExtraSuffixPlanName):  result.PlanName,
				// 余额探测成功即清除响应式 402/429 写下的 balance_low 标记。
				cnExtraKey(provider, cnBalanceExtraSuffixLow): false,
			}
			if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
				slog.Warn("cn_balance_persist_failed", "account_id", account.ID, "provider", provider, "error", err)
			} else {
				result.Persisted = true
			}
			return result, nil
		}
		// 上游订阅到期后 /v1/usage 不再返回 remaining/subscription（订阅信息
		// 未注入 context）。此时保留上次快照（含到期时间，前端标红展示），
		// 仅报告可识别的原因，不覆盖历史余额快照。
		if plan := strings.TrimSpace(gjson.GetBytes(bodyBytes, "planName").String()); plan != "" {
			// 上游订阅已到期/不可用：这是账号的确定性状态（需续费/换号），
			// 不是探测故障（探测本身 HTTP 200、响应结构正常）。此时确实取不到
			// 余额数字（Success=false），管理端应展示为状态而非报错；保留上次好
			// 快照（含到期时间），不覆盖历史余额、不编造数字、不换端点伪造数值。
			result.SubscriptionExpired = true
			result.Error = fmt.Sprintf("Upstream subscription %q expired or unavailable", plan)
			return result, nil
		}
		result.Error = "Invalid balance response: missing remaining"
		return result, nil
	}
	unit := strings.ToUpper(strings.TrimSpace(gjson.GetBytes(bodyBytes, "unit").String()))
	if unit == "" {
		unit = "USD"
	}
	available := true
	if v := gjson.GetBytes(bodyBytes, "is_active"); v.Exists() {
		available = v.Bool()
	} else if v := gjson.GetBytes(bodyBytes, "isValid"); v.Exists() {
		available = v.Bool()
	}
	result.Available = available
	result.Currency = unit
	result.PlanName = strings.TrimSpace(gjson.GetBytes(bodyBytes, "planName").String())
	// 订阅用量（订阅制账号没有余额数字，用量是唯一可展示的数值）。
	if v := gjson.GetBytes(bodyBytes, "subscription.daily_usage_usd"); v.Exists() {
		if f, parseErr := strconv.ParseFloat(v.String(), 64); parseErr == nil {
			result.DailyUsage = &f
		}
	}
	if v := gjson.GetBytes(bodyBytes, "subscription.monthly_usage_usd"); v.Exists() {
		if f, parseErr := strconv.ParseFloat(v.String(), 64); parseErr == nil {
			result.MonthlyUsage = &f
		}
	}
	// 订阅有效期：订阅模式在 subscription.expires_at，限额模式在顶层 expires_at。
	// 只透传可解析的 RFC3339，坏值静默丢弃（不影响余额主链路）。
	if raw := strings.TrimSpace(gjson.GetBytes(bodyBytes, "subscription.expires_at").String()); raw != "" {
		if _, parseErr := time.Parse(time.RFC3339, raw); parseErr == nil {
			result.ExpiresAt = raw
		}
	} else if raw := strings.TrimSpace(gjson.GetBytes(bodyBytes, "expires_at").String()); raw != "" {
		if _, parseErr := time.Parse(time.RFC3339, raw); parseErr == nil {
			result.ExpiresAt = raw
		}
	}
	// 三态分派（/v1/usage 的 mode）：订阅制不限量 / 次卡（次数包）/ 数字余额。
	//
	// 订阅制的判定只看 remaining<0：mode=unrestricted 同时还覆盖「钱包余额」形状
	// （unrestricted + 正数 remaining 是真实钱包金额，见 gateway_handler.go
	// usageUnrestricted 钱包分支），该形状必须留在 balance 分支，否则会吞掉可展示
	// 的钱包数字（既有用例 TestCNProviderBalanceService_RelayOverrideUsesConfiguredURL）。
	mode := strings.TrimSpace(gjson.GetBytes(bodyBytes, "mode").String())
	switch {
	case *remaining < 0:
		result.Kind = CNBalanceKindSubscription
		result.Unlimited = true
	case mode == "quota_limited":
		// 次卡：单位是「次」。上游 unit 在次卡下误标 USD 不可信（quota.used 与
		// usage.today.requests 逐位相等），故 Currency 置空、Balances 置空，
		// 只保留次数口径的 Balance/QuotaLimit/QuotaUsed。
		result.Kind = CNBalanceKindCountPack
		if lim, ok := cnParseF64(gjson.GetBytes(bodyBytes, "quota.limit").Value()); ok && lim > 0 {
			result.QuotaLimit = lim
			if used, ok2 := cnParseF64(gjson.GetBytes(bodyBytes, "quota.used").Value()); ok2 {
				result.QuotaUsed = used
			}
		}
		result.Balance = *remaining
		result.Balances = nil
		result.Currency = ""
	default:
		result.Kind = CNBalanceKindBalance
		result.Balance = *remaining
		result.Balances = []CNProviderBalanceEntry{{Currency: unit, Balance: *remaining}}
	}
	result.Success = true

	var balanceUpdates []any
	for _, entry := range result.Balances {
		balanceUpdates = append(balanceUpdates, map[string]any{
			"currency": entry.Currency,
			"balance":  entry.Balance,
		})
	}
	updates := map[string]any{
		cnExtraKey(provider, cnBalanceExtraSuffixBalance):   result.Balance,
		cnExtraKey(provider, cnBalanceExtraSuffixCurrency):  result.Currency,
		cnExtraKey(provider, cnBalanceExtraSuffixAvailable): available,
		cnExtraKey(provider, cnBalanceExtraSuffixUpdated):   now.Format(time.RFC3339),
		cnExtraKey(provider, cnBalanceExtraSuffixBalances):  balanceUpdates,
		cnExtraKey(provider, cnBalanceExtraSuffixUnlimited): result.Unlimited,
		cnExtraKey(provider, cnBalanceExtraSuffixPlanName):  result.PlanName,
		// 余额探测成功即清除响应式 402/429 写下的 balance_low 标记。
		cnExtraKey(provider, cnBalanceExtraSuffixLow): false,
		// 余额种类恒写（balance 也写）：前端按 kind 切换订阅/次卡/金额展示口径。
		cnExtraKey(provider, cnBalanceExtraSuffixKind): result.Kind,
	}
	// 次卡额度仅次卡写：UpdateExtra 是合并语义，非次卡不写以避免清掉旧值。
	if result.Kind == CNBalanceKindCountPack {
		updates[cnExtraKey(provider, cnBalanceExtraSuffixQuotaLimit)] = result.QuotaLimit
		updates[cnExtraKey(provider, cnBalanceExtraSuffixQuotaUsed)] = result.QuotaUsed
	}
	// 订阅有效期仅在解析到时更新（UpdateExtra 是合并语义，缺省键保留旧值），
	// 避免上游不再返回 expires_at 时用空串覆盖有效快照。
	if result.ExpiresAt != "" {
		updates[cnExtraKey(provider, cnBalanceExtraSuffixExpiresAt)] = result.ExpiresAt
	}
	// 订阅用量仅在解析到时更新（UpdateExtra 是合并语义，缺省键保留旧值）。
	if result.DailyUsage != nil {
		updates[cnExtraKey(provider, cnBalanceExtraSuffixDailyUsed)] = *result.DailyUsage
	}
	if result.MonthlyUsage != nil {
		updates[cnExtraKey(provider, cnBalanceExtraSuffixMonthlyUsed)] = *result.MonthlyUsage
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("cn_balance_persist_failed", "account_id", account.ID, "provider", provider, "error", err)
	} else {
		result.Persisted = true
	}
	return result, nil
}

// queryRelaySubscriptionBalance 是中转账号的账户级探测分支（New API 系次数包，
// 2026-10-09 星思云站 xing.xinxinyuntu.top 生产实测）：令牌级 /api/usage/token
// 对 unlimited key 只返回残留值（total_available=1），账户共享的次数包余额只在
// 账户级端点。Bearer 访问令牌鉴权（实测 Bearer/裸 token 均收，无鉴权 401）。
//
//   - /api/subscription/self → data.all_subscriptions[] 取首个 status=active，
//     剩余次数 = (amount_total - amount_used) / count_unit（换算单位账号配置，
//     星思云站实测 5000 配额单位 = 1 次，与站点弹窗读数逐次吻合）；
//   - /api/user/self → data.group 作 PlanName（站点分组名，如「开发者 Pro · 代理组」）；
//   - 快照同时清零令牌级残留展示键：expires_at 置空（次数包无到期概念，上游
//     end_time 恒 9999 年）、daily/monthly_used 置 0（消费全部走次数包，USD
//     钱包用量恒 0）——UpdateExtra 合并语义删不掉键，只能显式覆盖旧值。
//
// 任一请求失败或响应形状不符均失败关闭，不回退令牌级探测（残留值会伪装成真数）。
func (s *CNProviderBalanceService) queryRelaySubscriptionBalance(ctx context.Context, account *Account, provider string, probeCfg BalanceProbeConfig) (*CNProviderBalanceResult, error) {
	if probeCfg.CountUnit <= 0 {
		return nil, infraerrors.New(http.StatusBadRequest, "CN_BALANCE_INVALID_CONFIG", "balance probe count_unit must be > 0 when access_token is set")
	}
	origin, err := cnProbeURLOrigin(probeCfg.URL)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "CN_BALANCE_INVALID_CONFIG", err.Error())
	}

	now := time.Now().UTC()
	result := &CNProviderBalanceResult{
		Provider:  provider,
		FetchedAt: now.Unix(),
		Available: true,
		Unlimited: true,
		Currency:  "USD",
	}

	subURL, err := cnValidateProbeURL(s.cfg, origin+"/api/subscription/self")
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "CN_BALANCE_URL_REJECTED", err.Error())
	}
	bodyBytes, statusCode, err := s.cnRelayAccountGet(ctx, account, subURL, probeCfg.AccessToken)
	if err != nil {
		return nil, err
	}
	result.StatusCode = statusCode
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		result.Error = fmt.Sprintf("Authentication failed (HTTP %d)", statusCode)
		return result, nil
	}
	if statusCode < 200 || statusCode >= 300 {
		result.Error = fmt.Sprintf("API error (HTTP %d): %s", statusCode, truncate(strings.TrimSpace(string(bodyBytes)), 240))
		return result, nil
	}

	remaining, err := cnParseActiveCountPack(bodyBytes, probeCfg.CountUnit)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	result.Balance = remaining
	result.Balances = []CNProviderBalanceEntry{{Currency: "USD", Balance: remaining}}

	selfURL, err := cnValidateProbeURL(s.cfg, origin+"/api/user/self")
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "CN_BALANCE_URL_REJECTED", err.Error())
	}
	selfBytes, selfStatus, err := s.cnRelayAccountGet(ctx, account, selfURL, probeCfg.AccessToken)
	if err != nil {
		return nil, err
	}
	// 先更新结果状态码再判定：user/self 失败时 StatusCode 必须反映实际失败
	// 请求，不得保留 subscription 请求的 200 与 Error/Success=false 矛盾。
	result.StatusCode = selfStatus
	if selfStatus < 200 || selfStatus >= 300 {
		result.Error = fmt.Sprintf("API error (HTTP %d) on user/self: %s", selfStatus, truncate(strings.TrimSpace(string(selfBytes)), 240))
		return result, nil
	}
	group := strings.TrimSpace(gjson.GetBytes(selfBytes, "data.group").String())
	if group == "" {
		result.Error = "Invalid user/self response: missing data.group"
		return result, nil
	}
	result.PlanName = group
	result.Success = true

	balanceUpdates := make([]any, 0, len(result.Balances))
	for _, entry := range result.Balances {
		balanceUpdates = append(balanceUpdates, map[string]any{
			"currency": entry.Currency,
			"balance":  entry.Balance,
		})
	}
	updates := map[string]any{
		cnExtraKey(provider, cnBalanceExtraSuffixBalance):   result.Balance,
		cnExtraKey(provider, cnBalanceExtraSuffixCurrency):  result.Currency,
		cnExtraKey(provider, cnBalanceExtraSuffixAvailable): result.Available,
		cnExtraKey(provider, cnBalanceExtraSuffixUpdated):   now.Format(time.RFC3339),
		cnExtraKey(provider, cnBalanceExtraSuffixBalances):  balanceUpdates,
		cnExtraKey(provider, cnBalanceExtraSuffixUnlimited): result.Unlimited,
		cnExtraKey(provider, cnBalanceExtraSuffixPlanName):  result.PlanName,
		// 余额探测成功即清除响应式 402/429 写下的 balance_low 标记。
		cnExtraKey(provider, cnBalanceExtraSuffixLow): false,
		// 令牌级残留展示键清零（见函数注释），显式覆盖合并语义下的旧值。
		cnExtraKey(provider, cnBalanceExtraSuffixExpiresAt):   "",
		cnExtraKey(provider, cnBalanceExtraSuffixDailyUsed):   float64(0),
		cnExtraKey(provider, cnBalanceExtraSuffixMonthlyUsed): float64(0),
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("cn_balance_persist_failed", "account_id", account.ID, "provider", provider, "error", err)
	} else {
		result.Persisted = true
	}
	return result, nil
}

// cnRelayAccountGet 发起账户级探测 GET（Bearer 访问令牌），返回响应体与状态码。
// 传输失败包成 CN_BALANCE_REQUEST_FAILED，与令牌级分支同口径。
func (s *CNProviderBalanceService) cnRelayAccountGet(ctx context.Context, account *Account, targetURL, accessToken string) ([]byte, int, error) {
	proxyURL := s.resolveProxyURL(ctx, account)
	callCtx, cancel := context.WithTimeout(ctx, cnBalanceUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, 0, infraerrors.Newf(http.StatusInternalServerError, "CN_BALANCE_REQUEST_BUILD_FAILED", "build request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	// 通用请求头覆盖先行，探测专用 Bearer 凭据后置锁定：账户级分支只允许
	// 使用 balance_probe_access_token，header overrides 不得顶替该鉴权。
	account.ApplyHeaderOverrides(req.Header)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, 0, infraerrors.Newf(http.StatusBadGateway, "CN_BALANCE_REQUEST_FAILED", "upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, cnBalanceMaxBodyBytes))
	return bodyBytes, resp.StatusCode, nil
}

// cnParseActiveCountPack 从 /api/subscription/self 响应取首个 active 订阅，
// 剩余次数 = (amount_total - amount_used) / countUnit。形状不符失败关闭。
func cnParseActiveCountPack(bodyBytes []byte, countUnit float64) (float64, error) {
	subs := gjson.GetBytes(bodyBytes, "data.all_subscriptions")
	if !subs.Exists() || !subs.IsArray() {
		return 0, fmt.Errorf("Invalid subscription response: missing data.all_subscriptions")
	}
	var active gjson.Result
	found := false
	subs.ForEach(func(_, sub gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(sub.Get("subscription.status").String()), "active") {
			active = sub
			found = true
			return false
		}
		return true
	})
	if !found {
		return 0, fmt.Errorf("Invalid subscription response: no active subscription")
	}
	total, okTotal := cnParseF64(active.Get("subscription.amount_total").Value())
	used, okUsed := cnParseF64(active.Get("subscription.amount_used").Value())
	if !okTotal || !okUsed {
		return 0, fmt.Errorf("Invalid subscription response: missing amount_total/amount_used")
	}
	return (total - used) / countUnit, nil
}

// cnProbeURLOrigin 从探测地址取 https 源（scheme://host），账户级端点挂同一源下。
func cnProbeURLOrigin(probeURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(probeURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("balance probe URL is invalid: %q", probeURL)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("balance probe URL must use https")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// cnIsOfficialBalanceHost 判定 base_url 是否指向厂商官方域名（官方账号的余额
// 探测必须走官方端点，不能被 /v1/usage 默认推导劫持）。
func cnIsOfficialBalanceHost(platform, baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	switch platform {
	case PlatformKimi:
		return host == "api.moonshot.cn"
	case PlatformDeepseek:
		return host == "api.deepseek.com"
	default:
		return false
	}
}

// validatePayGAccount 加载后的非 DB 校验（ForAccount 入口同样复用，
// 保证直传 account 也不绕过平台/模式检查）。
func validatePayGAccount(account *Account) error {
	if account == nil {
		return infraerrors.New(http.StatusNotFound, "CN_BALANCE_ACCOUNT_NOT_FOUND", "account not found")
	}
	// Cline 积分余额只查官方主机。
	if account.IsCline() {
		if !account.clineAccountAPISupported() {
			return infraerrors.New(http.StatusBadRequest, "CN_BALANCE_NO_ENDPOINT", "cline balance is only available for api key accounts on the official host")
		}
		return nil
	}
	// Command Code 积分余额与套餐窗口同源，只查官方主机。
	if account.IsCommandCode() {
		if !account.commandCodeUsageSupported() {
			return infraerrors.New(http.StatusBadRequest, "CN_BALANCE_NO_ENDPOINT", "command code balance is only available for api key accounts on the official host")
		}
		return nil
	}
	if !account.IsCNProvider() {
		return infraerrors.New(http.StatusBadRequest, "CN_BALANCE_INVALID_PLATFORM", "account is not a CN provider account")
	}
	// coding 账号走额度探测，余额端点不适用——Kira 例外（外审 F5）：Kira 账号
	// 无论 coding/payg 都有 dashboard JWT 余额/用量端点（cn_provider_kira.go），
	// 否则 coding 模式的 Kira 账号会被余额链拒之门外，快照无人刷新。
	if account.IsCodingPlan() && !accountIsKiraBaseURL(account) {
		return infraerrors.New(http.StatusBadRequest, "CN_BALANCE_CODING_PLAN", "coding plan account has no balance endpoint; use quota probe")
	}
	return nil
}

func (s *CNProviderBalanceService) resolveProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil {
		return ""
	}
	if account.Proxy != nil {
		return account.Proxy.URL()
	}
	if s != nil && s.proxyRepo != nil {
		if proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && proxy != nil {
			account.Proxy = proxy
			return proxy.URL()
		}
	}
	return ""
}

// cnBalanceURL 解析账号的余额端点。
//
//   - Kimi：固定 https://api.moonshot.cn/v1/users/me/balance（与 base_url 无关，Moonshot 仅此一处）
//   - DeepSeek：基于 base_url 拼接 /user/balance（支持自定义域名）
func cnBalanceURL(account *Account) string {
	switch account.Platform {
	case PlatformKimi:
		return "https://api.moonshot.cn/v1/users/me/balance"
	case PlatformDeepseek:
		// Anthropic 协议账号的凭证 base_url 指向 /anthropic 端点，余额探测需回退
		// 到 OpenAI 格式 base（协议感知）再拼接 /user/balance。
		return strings.TrimRight(account.GetOpenAIFormatBaseURL(), "/") + "/user/balance"
	default:
		return ""
	}
}
