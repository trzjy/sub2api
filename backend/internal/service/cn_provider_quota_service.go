package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

// volcanoPlanLoc 是火山方舟额度刷新使用的官方时区（Asia/Shanghai / UTC+8）。
// 周窗口刷新锚点固定按该时区计算（见 volcanoNextWeeklyReset）。
var volcanoPlanLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// 国产供应商 Coding Plan 滚动窗口额度探测服务（Kimi For Coding / 智谱 GLM Coding Plan）。
//
// 与 grok_quota_service 不同：CN 供应商走数据面 API Key（无 OAuth token provider），
// 额度端点为只读 GET，解析 5h + weekly 两档滚动窗口并落 account.Extra 快照，
// 供账号调度阈值评估（account_scheduling_threshold_eval.go）做主动停调。
//
// 解析逻辑对齐 cc-switch（farion1231/cc-switch）services/coding_plan.rs 的
// query_kimi / query_zhipu，包括智谱 unit 字段优先分类与 reset 兜底启发式。
const (
	cnQuotaUpstreamTimeout = 15 * time.Second
	cnQuotaMaxBodyBytes    = 256 * 1024

	// Extra 快照键后缀（加 provider 前缀，如 kimi_5h_used_percent）。
	cnExtraSuffix5hUsed       = "5h_used_percent"
	cnExtraSuffix5hReset      = "5h_reset_at"
	cnExtraSuffixWeeklyUsed   = "weekly_used_percent"
	cnExtraSuffixWeeklyReset  = "weekly_reset_at"
	cnExtraSuffixMonthlyUsed  = "monthly_used_percent"
	cnExtraSuffixMonthlyReset = "monthly_reset_at"
	cnExtraSuffixUsageUpdated = "usage_updated_at"
)

// providerVolcano 标识火山方舟（Volcengine Ark）Coding / Agent Plan 订阅账号，
// 作为 Coding Plan 供应商 token 参与额度快照键（volcano_5h_* 等）与调度路由。
// 注意它不是 accounts.platform 平台枚举（火山号平台仍为 deepseek），只用于
// CN 额度探测链路内部识别，与 GetCodingPlanProvider 的返回一致。
const providerVolcano = "volcano"

// cnExtraKey 拼接 provider 维度的 extra 键。
func cnExtraKey(provider, suffix string) string { return provider + "_" + suffix }

// isVolcanoBaseURL 报告 base_url 是否指向火山方舟订阅号（Agent Plan /api/plan 与
// Coding Plan /api/coding 共用 ark.cn-beijing.volces.com 域名），与接入模式无关。
// 用 net/url 主机解析精确判定，避免字符串 Contains 把带相似子串的主机误判为火山。
func isVolcanoBaseURL(baseURL string) bool {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Hostname(), volcanoPlanHost)
}

// resolveCNQuotaProvider 返回账号所属的 Coding Plan 额度供应商（kimi/zhipu/volcano）；
// 非 coding 或无法识别返回 "". 火山识别按凭据原始 base_url 优先（详见下方说明），
// 仅放行火山，不改变 Kimi / 智谱 / 普通 DeepSeek 的 coding-only 语义。
//
// 火山优先的必要性：火山订阅号账号的 platform 常被误存为 kimi/deepseek，且自适应
// 协议 (api_protocol=adaptive) 账号的 GetOpenAIBaseURL() 优先取 api_base_urls 的
// chat_completions（会指向 api.kimi.com 而非 ark.cn-beijing.volces.com）。若按
// GetOpenAIBaseURL 判定，这类账号会被误判为 Kimi、探测发往 Kimi /usages 端点，返回
// Kimi 结构的 0% 周用量且缺失 5h/月档。凭据 base_url 才是火山订阅号的唯一事实源，
// 故先用 GetBaseURL()（= ark.cn-beijing.volces.com）强制识别为火山。
func resolveCNQuotaProvider(account *Account) string {
	if isVolcanoBaseURL(account.GetBaseURL()) {
		return providerVolcano
	}
	provider := account.GetCodingPlanProvider()
	if provider == "" && isVolcanoBaseURL(account.GetOpenAIBaseURL()) {
		return providerVolcano
	}
	return provider
}

// CNQuotaTier 表示一个滚动用量窗口档位（5h / weekly）。
type CNQuotaTier struct {
	Window      string  `json:"window"`             // "5h" | "weekly"
	UsedPercent float64 `json:"used_percent"`       // 已用百分比（0-100+，不做裁剪）
	ResetAt     string  `json:"reset_at,omitempty"` // RFC3339，空表示无重置时间
	// UsedPercentUnknown 标记 used_percent 上游不可得（如火山周窗口：限流响应头仅含
	// 5h 窗口，周额度无可靠来源）。此时不得写假 0，仅落 reset_at，前端显示“未知”。
	UsedPercentUnknown bool `json:"used_percent_unknown,omitempty"`
}

// MarshalJSON 在“用量未知”档位把 used_percent 序列化为 null（而非 0），前端据此显示
// “未知/—”；其余档位保持数值。仅影响 API 响应序列化，不影响内部计算与持久化快照写入。
func (t CNQuotaTier) MarshalJSON() ([]byte, error) {
	type alias CNQuotaTier
	if !t.UsedPercentUnknown {
		return json.Marshal(alias(t))
	}
	return json.Marshal(struct {
		alias
		UsedPercent *float64 `json:"used_percent"`
	}{alias: alias(t)})
}

// CNProviderQuotaProbeResult 是 Coding Plan 额度探测的返回结构（管理端 + UI 消费）。
type CNProviderQuotaProbeResult struct {
	Provider        string        `json:"provider"`
	Source          string        `json:"source"`
	Success         bool          `json:"success"`
	CredentialValid bool          `json:"credential_valid"` // false = 401/403 鉴权失败
	Tiers           []CNQuotaTier `json:"tiers,omitempty"`
	PlanLevel       string        `json:"plan_level,omitempty"` // 智谱套餐等级
	StatusCode      int           `json:"status_code,omitempty"`
	FetchedAt       int64         `json:"fetched_at"`
	Persisted       bool          `json:"persisted"`
	Error           string        `json:"error,omitempty"`
}

// CNProviderQuotaService 探测 Kimi / Zhipu Coding Plan 的滚动窗口用量。
type CNProviderQuotaService struct {
	accountRepo  AccountRepository
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config
	flight       singleflight.Group
}

// NewCNProviderQuotaService 构造 Coding Plan 额度探测服务。
func NewCNProviderQuotaService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *CNProviderQuotaService {
	return &CNProviderQuotaService{
		accountRepo:  accountRepo,
		proxyRepo:    proxyRepo,
		httpUpstream: httpUpstream,
		cfg:          cfg,
	}
}

// QueryUsage 探测指定账号的 Coding Plan 滚动窗口用量并落 Extra 快照。
// 同一账号的并发探测会被 singleflight 合并。
func (s *CNProviderQuotaService) QueryUsage(ctx context.Context, accountID int64) (*CNProviderQuotaProbeResult, error) {
	account, err := s.loadCodingPlanAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.QueryUsageForAccount(ctx, account)
}

// QueryUsageForAccount 探测已加载账号（配额监控 fetcher 复用，避免二次 GetByID）。
// singleflight key 与 QueryUsage 相同，按账号 ID 与 admin 侧并发探测合并。
func (s *CNProviderQuotaService) QueryUsageForAccount(ctx context.Context, account *Account) (*CNProviderQuotaProbeResult, error) {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "CN_QUOTA_NOT_CONFIGURED", "cn provider quota service is not configured")
	}
	if err := validateCodingPlanAccount(account); err != nil {
		return nil, err
	}
	key := "cn_quota:" + strconv.FormatInt(account.ID, 10)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), cnQuotaUpstreamTimeout+5*time.Second)
		defer cancel()
		return s.queryUsageForAccount(probeCtx, account)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return nil, flightResult.Err
		}
		result, ok := flightResult.Val.(*CNProviderQuotaProbeResult)
		if !ok || result == nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "CN_QUOTA_PROBE_RESULT_INVALID", "invalid cn provider quota probe result")
		}
		cloned := *result
		return &cloned, nil
	}
}

func (s *CNProviderQuotaService) queryUsageForAccount(ctx context.Context, account *Account) (*CNProviderQuotaProbeResult, error) {
	provider := resolveCNQuotaProvider(account)
	if provider != PlatformKimi && provider != PlatformZhipu && provider != PlatformMiniMax && provider != providerVolcano {
		return nil, infraerrors.New(http.StatusBadRequest, "CN_QUOTA_NOT_CODING_PLAN", "account is not a kimi/zhipu/minimax/volcano coding plan account")
	}

	baseURL := account.GetOpenAIBaseURL()
	var (
		targetURL      string
		authHeader     string
		volcanoAPIKey  string
		volcanoProfile volcanoPlanProfile
		zhipuOrg       string
	)
	switch provider {
	case PlatformKimi:
		apiKey := strings.TrimSpace(account.GetCNAPIKey())
		if apiKey == "" {
			return nil, infraerrors.New(http.StatusBadRequest, "CN_QUOTA_NO_APIKEY", "account api_key is empty")
		}
		targetURL = kimiQuotaURL(baseURL)
		authHeader = "Bearer " + apiKey
	case PlatformZhipu:
		apiKey := strings.TrimSpace(account.GetCNAPIKey())
		if apiKey == "" {
			return nil, infraerrors.New(http.StatusBadRequest, "CN_QUOTA_NO_APIKEY", "account api_key is empty")
		}
		targetURL = zhipuQuotaURL(baseURL)
		authHeader = apiKey // 智谱额度端点鉴权不加 Bearer 前缀
		// 团队版 GLM Coding Plan：额度端点需 ?type=2 + 组织/项目请求头，
		// 否则官方 API 回「当前用户不存在coding plan」。组织 ID 存在即视为
		// 团队版（个人版凭据不含该字段，走原个人版查询路径）。
		zhipuOrg = strings.TrimSpace(account.GetCredential("zhipu_organization"))
		if zhipuOrg != "" {
			targetURL += "?type=2"
		}
	case PlatformMiniMax:
		apiKey := strings.TrimSpace(account.GetCNAPIKey())
		if apiKey == "" {
			return nil, infraerrors.New(http.StatusBadRequest, "CN_QUOTA_NO_APIKEY", "account api_key is empty")
		}
		targetURL = minimaxQuotaURL(baseURL)
		authHeader = "Bearer " + apiKey
	case providerVolcano:
		// 主路径：AK/SK 管理面用量接口（GetAFPUsage / GetCodingPlanUsage）。生产实测
		// （2026-09-06）推理端点成功响应不携带任何 x-ratelimit-* 头，官方文档亦明确订阅
		// 用量只有控制台可查（延迟 0.5~1 天），响应头探测拿不到用量，仅作无 AK/SK 时的
		// 回落（此时周/月重置仍按官方规则确定性计算，用量档显示"上游未提供"）。
		if akskResult, handled, perr := s.volcanoAKSKProbe(ctx, account); handled {
			return akskResult, perr
		}
		apiKey := strings.TrimSpace(account.GetCNAPIKey())
		if apiKey == "" {
			return nil, infraerrors.New(http.StatusBadRequest, "CN_QUOTA_NO_APIKEY", "account api_key is empty")
		}
		// 火山 profile 必须用凭据原始 base_url（= ark.cn-beijing.volces.com）解析，
		// 不取 baseURL（= GetOpenAIBaseURL，自适应账号会被 api_base_urls 覆盖为 kimi
		// 端点）；否则 profile 解析失败而误报非火山端点。原生 Anthropic 协议火山账号
		// 的 base URL 已由 GetAnthropicProtocolBaseURL 返回火山端点，同样可用。
		volcanoBaseURL := account.GetBaseURL()
		if account.IsAnthropicProtocol() {
			volcanoBaseURL = account.GetAnthropicProtocolBaseURL()
		}
		profile, ok := parseVolcanoPlanProfile(volcanoBaseURL)
		if !ok {
			return nil, infraerrors.New(http.StatusBadRequest, "CN_QUOTA_VOLCANO_BASEURL", "account base_url is not a volcano plan endpoint")
		}
		volcanoProfile = profile
		// targetURL 仅用于出站 URL 策略校验（host 与 Anthropic 端点一致），真实请求由
		// buildVolcanoProbeRequest 按账号 API 协议构造（Anthropic→/v1/messages，OpenAI→/v3）。
		targetURL = profile.openAIChatCompletionsURL()
		volcanoAPIKey = apiKey
	}

	// 探测发起前过出站 URL 安全策略（与网关转发/Grok 探测同一套校验）：
	// 端点多由账号 base_url 衍生，不得把 API key 发往策略外主机。
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "CN_QUOTA_URL_REJECTED", err.Error())
	}
	targetURL = validatedURL

	proxyURL := s.resolveProxyURL(ctx, account)
	callCtx, cancel := context.WithTimeout(ctx, cnQuotaUpstreamTimeout)
	defer cancel()
	if provider == providerVolcano {
		// 火山探测按候选模型依次尝试（model_mapping 排序值 + 默认回落模型）。某个 mapping
		// 模型在当前方舟套餐不可用（model-not-found 类 4xx）时回落下一个候选，避免单一失效
		// mapping 导致每次刷新都失败（外审 P2）。2xx/429（已获限流头）/鉴权失败（换模型无意义）
		// 直接返回，不再重试。
		models := volcanoProbeModels(account)
		var lastResult *CNProviderQuotaProbeResult
		for i, model := range models {
			res, retryable, rerr := s.doVolcanoProbe(callCtx, ctx, account, volcanoProfile, volcanoAPIKey, model, proxyURL)
			if rerr != nil {
				return nil, rerr
			}
			if res.Success || res.StatusCode == http.StatusTooManyRequests ||
				res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
				return res, nil
			}
			lastResult = res
			if !retryable || i == len(models)-1 {
				return res, nil
			}
		}
		if lastResult != nil {
			return lastResult, nil
		}
		return nil, infraerrors.New(http.StatusInternalServerError, "CN_QUOTA_NO_PROBE_MODEL", "no volcano probe model available")
	}

	var req *http.Request
	req, err = http.NewRequestWithContext(callCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "CN_QUOTA_REQUEST_BUILD_FAILED", "build request: %v", err)
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Accept", "application/json")
	if provider == PlatformZhipu || provider == PlatformMiniMax {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Language", "en-US,en")
		if zhipuOrg != "" {
			req.Header.Set("bigmodel-organization", zhipuOrg)
			if project := strings.TrimSpace(account.GetCredential("zhipu_project")); project != "" {
				req.Header.Set("bigmodel-project", project)
			}
		}
	}
	// 探测与真实转发保持同一套账号级请求头覆写，避免探测通过但转发失败。
	account.ApplyHeaderOverrides(req.Header)

	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CN_QUOTA_REQUEST_FAILED", "upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, cnQuotaMaxBodyBytes))

	now := time.Now().UTC()
	result := &CNProviderQuotaProbeResult{
		Provider:   provider,
		Source:     "coding_plan",
		FetchedAt:  now.Unix(),
		StatusCode: resp.StatusCode,
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// 鉴权失败：不落快照（不覆盖之前的有效值），仅返回失败结果供前端提示。
		result.Error = fmt.Sprintf("Authentication failed (HTTP %d)", resp.StatusCode)
		return result, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Error = fmt.Sprintf("API error (HTTP %d): %s", resp.StatusCode, truncate(strings.TrimSpace(string(bodyBytes)), 240))
		return result, nil
	}

	// 智谱业务级错误（HTTP 2xx 但 success=false）。
	if provider == PlatformZhipu {
		if success := gjson.GetBytes(bodyBytes, "success"); success.Exists() && !success.Bool() {
			msg := strings.TrimSpace(gjson.GetBytes(bodyBytes, "msg").String())
			if msg == "" {
				msg = "unknown zhipu quota error"
			}
			result.Error = "API error: " + msg
			return result, nil
		}
	}

	var tiers []CNQuotaTier
	switch provider {
	case PlatformKimi:
		tiers = parseKimiUsageTiers(bodyBytes)
	case PlatformZhipu:
		tiers = parseZhipuTokenTiers(gjson.GetBytes(bodyBytes, "data"))
		result.PlanLevel = strings.TrimSpace(gjson.GetBytes(bodyBytes, "data.level").String())
	case PlatformMiniMax:
		if status := gjson.GetBytes(bodyBytes, "base_resp.status_code"); status.Exists() && status.Int() != 0 {
			msg := strings.TrimSpace(gjson.GetBytes(bodyBytes, "base_resp.status_msg").String())
			if msg == "" {
				msg = "unknown minimax quota error"
			}
			result.Error = fmt.Sprintf("API error (%d): %s", status.Int(), msg)
			return result, nil
		}
		tiers = parseMiniMaxUsageTiers(bodyBytes)
		result.PlanLevel = strings.TrimSpace(gjson.GetBytes(bodyBytes, "current_subscribe_title").String())
	}
	result.Tiers = tiers
	result.Success = true
	result.CredentialValid = true

	updates := cnQuotaExtraUpdates(provider, tiers, now)
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("cn_quota_persist_failed", "account_id", account.ID, "provider", provider, "error", err)
	} else {
		result.Persisted = true
	}
	return result, nil
}

func (s *CNProviderQuotaService) loadCodingPlanAccount(ctx context.Context, accountID int64) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusNotFound, "CN_QUOTA_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	if err := validateCodingPlanAccount(account); err != nil {
		return nil, err
	}
	return account, nil
}

// validateCodingPlanAccount 加载后的非 DB 校验（ForAccount 入口同样复用，
// 保证直传 account 也不绕过平台/模式检查）。
func validateCodingPlanAccount(account *Account) error {
	if account == nil {
		return infraerrors.New(http.StatusNotFound, "CN_QUOTA_ACCOUNT_NOT_FOUND", "account not found")
	}
	if !account.IsCNProvider() {
		return infraerrors.New(http.StatusBadRequest, "CN_QUOTA_INVALID_PLATFORM", "account is not a CN provider account")
	}
	// 火山订阅号账号（platform=deepseek，base_url=ark.cn-beijing.volces.com）保存为
	// payg 时同样具备可探测的 Coding/Agent Plan 额度，放行非 coding；其余 CN 供应商
	// 仍仅限 coding。
	if !account.IsCodingPlan() && !isVolcanoBaseURL(account.GetOpenAIBaseURL()) {
		return infraerrors.New(http.StatusBadRequest, "CN_QUOTA_NOT_CODING_PLAN", "account is not a coding plan account")
	}
	return nil
}

func (s *CNProviderQuotaService) resolveProxyURL(ctx context.Context, account *Account) string {
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

// zhipuQuotaURL 根据 base_url 解析智谱额度端点（与数据面推理域名同主机）。
func zhipuQuotaURL(baseURL string) string {
	return zhipuQuotaHost(baseURL) + "/api/monitor/usage/quota/limit"
}

// kimiQuotaURL 根据 base_url 解析 Kimi For Coding 额度端点。
// cc-switch query_kimi 固定探测 https://api.kimi.com/coding/v1/usages
// （实测 /coding/usages 无 /v1 → 404）。coding/v1（CC 协议默认）与
// coding（Anthropic 协议默认）两种 base 统一剥掉尾部后拼回 /v1/usages，
// 协议切换不影响额度探测端点。
func kimiQuotaURL(baseURL string) string {
	base := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	return base + "/v1/usages"
}

// 火山方舟用量探测说明：
// 主路径为 AK/SK 管理面探测（见 cn_provider_quota_volcano_aksk.go）：GetAFPUsage /
// GetCodingPlanUsage 返回真实 Used/Quota 与官方重置时间，已在生产用真实密钥验证可用
// （订阅账号凭据中的 access_key/secret_key，或同 base_url 同主体账号的继承值）。
// 回落路径（无 AK/SK）：复用订阅 API Key（ark-*, Bearer）对真实推理端点
// （{base}/v3/chat/completions）发一次最小请求——但注意该响应不携带任何 x-ratelimit-*
// 头（生产实测），故只能落周/月确定性重置时间，用量档不可得。

// volcanoQuotaProbeDefaultModel 是火山探测回落模型（model_mapping 为空时使用）。
// 必须取方舟官方支持的通用模型 ID（与火山订阅文档候选一致，如 doubao-seed-2.0-lite /
// doubao-seed-2.0-mini / doubao-seed-2.1-turbo）；基础版 doubao-seed-2.0 不在官方
// 候选列表中，回落会得到 model-not-found 导致探测失败。Coding/Agent Plan 账号默认可用。
const volcanoQuotaProbeDefaultModel = "doubao-seed-2.0-lite"

// volcanoProbeModels 返回探测候选模型（按优先级）：账号 model_mapping 中所有非空上游模型
// （按 key 确定性排序，避免随机命中不可用模型）后追加默认回落模型，去重。探测时依次尝试，
// 某个 mapping 模型在当前方舟套餐不可用（model-not-found）时回落到下一个候选，保证最小请求
// 总能拿到 200/429 与限流响应头（外审 P2）。
func volcanoProbeModels(account *Account) []string {
	seen := make(map[string]struct{})
	var models []string
	if account != nil {
		if m := account.GetModelMapping(); len(m) > 0 {
			// model_mapping 为 map，遍历顺序随机；按 key 确定性排序后取所有非空值，
			// 避免每次探测随机选模型（可能命中不可用模型导致探针失败）。
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if model := strings.TrimSpace(m[k]); model != "" {
					if _, dup := seen[model]; !dup {
						seen[model] = struct{}{}
						models = append(models, model)
					}
				}
			}
		}
	}
	if _, dup := seen[volcanoQuotaProbeDefaultModel]; !dup {
		models = append(models, volcanoQuotaProbeDefaultModel)
	}
	return models
}

// volcanoProbeModel 取首个候选（与 volcanoProbeModels()[0] 一致），供测试与单模型路径使用。
func volcanoProbeModel(account *Account) string {
	models := volcanoProbeModels(account)
	if len(models) == 0 {
		return volcanoQuotaProbeDefaultModel
	}
	return models[0]
}

// doVolcanoProbe 对单个候选模型发一次火山最小探活请求并解析响应。返回值：
//   - *CNProviderQuotaProbeResult：本次探测结果（含是否已落快照）；
//   - bool：是否为“模型不可用”可重试错误（model-not-found 类 4xx），调用方据此尝试下一候选；
//   - error：请求构造/传输层错误（非业务响应），调用方直接返回，不再换模型重试。
func (s *CNProviderQuotaService) doVolcanoProbe(callCtx, ctx context.Context, account *Account, profile volcanoPlanProfile, apiKey, model, proxyURL string) (*CNProviderQuotaProbeResult, bool, error) {
	req, err := buildVolcanoProbeRequest(callCtx, account, profile, model, apiKey)
	if err != nil {
		return nil, false, infraerrors.Newf(http.StatusInternalServerError, "CN_QUOTA_REQUEST_BUILD_FAILED", "build volcano request: %v", err)
	}
	// 探测与真实转发保持同一套账号级请求头覆写，避免探测通过但转发失败。
	account.ApplyHeaderOverrides(req.Header)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, false, infraerrors.Newf(http.StatusBadGateway, "CN_QUOTA_REQUEST_FAILED", "upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, cnQuotaMaxBodyBytes))
	now := time.Now().UTC()
	result := &CNProviderQuotaProbeResult{
		Provider:   providerVolcano,
		Source:     "coding_plan",
		FetchedAt:  now.Unix(),
		StatusCode: resp.StatusCode,
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 鉴权失败：不落快照，仅返回失败结果供前端提示。
		result.Error = fmt.Sprintf("Authentication failed (HTTP %d)", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		// 429 同样携带 x-ratelimit-* 头，凭据有效，落 5h 重置快照。
		if tiers := parseVolcanoHeaderTiers(resp.Header); len(tiers) > 0 {
			if perr := s.accountRepo.UpdateExtra(ctx, account.ID, cnQuotaExtraUpdates(providerVolcano, tiers, now)); perr != nil {
				slog.Warn("cn_quota_persist_failed", "account_id", account.ID, "provider", providerVolcano, "error", perr)
			} else {
				result.Persisted = true
			}
		}
		result.CredentialValid = true
		result.Error = fmt.Sprintf("Rate limited (HTTP 429): %s", truncate(strings.TrimSpace(string(bodyBytes)), 240))
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		result.Error = fmt.Sprintf("API error (HTTP %d): %s", resp.StatusCode, truncate(strings.TrimSpace(string(bodyBytes)), 240))
		// 模型不可用（model-not-found 类 4xx）可触发回落到下一候选模型。
		return result, isVolcanoModelNotFoundError(bodyBytes, resp.StatusCode), nil
	default:
		// 5h 滚动窗口从 OpenAI 兼容限流响应头读取；周窗口按官方刷新规则
		// （每周一 00:00 Asia/Shanghai）确定性计算，不参与倚赖响应体的解析。
		tiers := parseVolcanoHeaderTiers(resp.Header)
		result.Tiers = tiers
		result.Success = true
		result.CredentialValid = true
		if perr := s.accountRepo.UpdateExtra(ctx, account.ID, cnQuotaExtraUpdates(providerVolcano, tiers, now)); perr != nil {
			slog.Warn("cn_quota_persist_failed", "account_id", account.ID, "provider", providerVolcano, "error", perr)
		} else {
			result.Persisted = true
		}
	}
	return result, false, nil
}

// isVolcanoModelNotFoundError 判断火山响应是否为“模型不可用”类错误（HTTP 400/404 且 error
// 字段指向模型不存在/不支持）。此类错误应触发探测回落到下一个候选模型，而非判定为账号级故障。
func isVolcanoModelNotFoundError(body []byte, status int) bool {
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		return false
	}
	code := strings.ToLower(gjson.GetBytes(body, "error.code").String())
	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if code != "" {
		if strings.Contains(code, "model") {
			return true
		}
		// 部分实现用通用 invalid_request_error，需配合 message 含模型关键字才判定为模型错误。
		if code == "invalid_request_error" && strings.Contains(msg, "model") {
			return true
		}
	}
	return strings.Contains(msg, "model") &&
		(strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") ||
			strings.Contains(msg, "unsupported") || strings.Contains(msg, "not available") ||
			strings.Contains(msg, "no access") || strings.Contains(msg, "forbidden"))
}

// parseVolcanoHeaderTiers 从 OpenAI 兼容限流响应头解析火山方舟滚动窗口用量。
//
//   - 5h 窗口：x-ratelimit-reset-requests（绝对 Unix 秒级时间戳；部分实现返回相对秒数时
//     按 now+秒 处理）为重置时间；x-ratelimit-limit/remaining-requests 计算已用百分比。
//     limit 与 remaining 必须同时可解析才计算用量，否则 remaining 缺失/畸形视为未知（用 0，
//     绝不算成 100% 满额），避免健康账号被误暂停。
//   - 周/月窗口：官方控制台展示近一周、近一月两档，但 OpenAI 兼容限流响应头仅含 5h 窗口，
//     故周、月额度无可靠上游来源，used 标记 Unknown，只落 reset_at，前端显示“未知”而非假 0；
//     重置时间按官方刷新规则确定性计算（每周一 00:00、每月同一天 23:59:59，Asia/Shanghai）。
//
// 只要 5h/周/月窗口有有效重置时间即返回对应 tier，供前端渲染倒计时。
func parseVolcanoHeaderTiers(h http.Header) []CNQuotaTier {
	var tiers []CNQuotaTier
	if resetAt, ok := parseVolcanoResetHeader(h.Get("x-ratelimit-reset-requests")); ok {
		used := 0.0
		usedUnknown := true
		if limit, lErr := strconv.ParseFloat(strings.TrimSpace(h.Get("x-ratelimit-limit-requests")), 64); lErr == nil && limit > 0 {
			if remaining, rErr := strconv.ParseFloat(strings.TrimSpace(h.Get("x-ratelimit-remaining-requests")), 64); rErr == nil {
				used = (limit - remaining) / limit * 100
				if used < 0 {
					used = 0
				}
				usedUnknown = false
			}
			// remaining 解析失败：保持 used=0 且 usedUnknown=true，前端显示"—"，不误报 100% 满额。
		}
		// limit 缺失/畸形：用量未知（usedUnknown=true），渲染为"—"，不被 scheduler 当作已用满。
		tiers = append(tiers, CNQuotaTier{
			Window:             "5h",
			UsedPercent:        used,
			ResetAt:            resetAt,
			UsedPercentUnknown: usedUnknown,
		})
	}
	if reset := volcanoNextWeeklyReset(); reset != "" {
		tiers = append(tiers, CNQuotaTier{
			Window:             "weekly",
			UsedPercent:        0,
			ResetAt:            reset,
			UsedPercentUnknown: true,
		})
	}
	if reset := volcanoNextMonthlyReset(); reset != "" {
		tiers = append(tiers, CNQuotaTier{
			Window:             "monthly",
			UsedPercent:        0,
			ResetAt:            reset,
			UsedPercentUnknown: true,
		})
	}
	return tiers
}

// parseVolcanoResetHeader 解析 OpenAI 兼容限流重置头（x-ratelimit-reset-*）：
// 复用 xai.ParseResetHeader，兼容秒级 epoch、毫秒 epoch、相对秒、Go duration（6m0s）、
// RFC3339 时间戳；结果必须落在未来，否则返回空（避免毫秒 epoch 误读为几万年后的时间戳）。
func parseVolcanoResetHeader(raw string) (string, bool) {
	epoch := xai.ParseResetHeader(raw)
	if epoch == nil {
		return "", false
	}
	t := time.Unix(*epoch, 0)
	if !t.After(time.Now()) {
		return "", false
	}
	return t.UTC().Format(time.RFC3339), true
}

// volcanoNextWeeklyReset 返回下一个周一 00:00（Asia/Shanghai）的 RFC3339 字符串。
// 官方规则：周限额每周一 00:00 刷新，确定性计算，不依赖上游响应。
func volcanoNextWeeklyReset() string {
	now := time.Now().In(volcanoPlanLoc)
	// 本周一 00:00（按 Asia/Shanghai）。
	weekday := int(now.Weekday()) // Sunday=0 … Saturday=6
	daysSinceMonday := (weekday + 6) % 7
	monday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, volcanoPlanLoc).
		AddDate(0, 0, -daysSinceMonday)
	if !monday.After(now) {
		monday = monday.AddDate(0, 0, 7)
	}
	return monday.UTC().Format(time.RFC3339)
}

// volcanoNextMonthlyReset 返回下一个月同一天 23:59:59（Asia/Shanghai）的 RFC3339 字符串。
// 火山方舟 Agent/Coding Plan 控制台展示近一月用量，月限额随包月订阅周期刷新；无状态时按
// "下个月同一天 23:59:59" 近似。若目标月份不存在该日期（如 1 月 31 日 → 2 月），则取目标月
// 最后一天，避免溢出到下下个月。
func volcanoNextMonthlyReset() string {
	return volcanoNextMonthlyResetAt(time.Now().In(volcanoPlanLoc))
}

// volcanoNextMonthlyResetAt 是 volcanoNextMonthlyReset 的可测试版本，接受固定 now。
func volcanoNextMonthlyResetAt(now time.Time) string {
	// 先尝试下个月同一天 23:59:59；Go time.Date 会规范化溢出日期。
	next := time.Date(now.Year(), now.Month()+1, now.Day(), 23, 59, 59, 0, volcanoPlanLoc)
	if next.Month() != now.Month()+1 && !(now.Month() == 11 && next.Month() == 0) {
		// 日期溢出导致跳到了下下个月（或跨年异常），回退到目标月最后一天。
		firstOfTarget := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, volcanoPlanLoc)
		next = firstOfTarget.AddDate(0, 1, -1)
		next = time.Date(next.Year(), next.Month(), next.Day(), 23, 59, 59, 0, volcanoPlanLoc)
	}
	if !next.After(now) {
		next = next.AddDate(0, 1, 0)
	}
	return next.UTC().Format(time.RFC3339)
}

// minimaxQuotaURL 根据推理域名选择 Token Plan / Coding Plan 额度主机。
// 官方 FAQ 写 www.minimax.io / www.minimaxi.com，实际以 Bearer Key 打 api.*。
// 国际站 api.minimax.io；国内站 api.minimaxi.com（含 api.minimax.com 与自定义回落）。
func minimaxQuotaURL(baseURL string) string {
	if strings.Contains(strings.ToLower(baseURL), "minimax.io") {
		return "https://api.minimax.io/v1/api/openplatform/coding_plan/remains"
	}
	return "https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains"
}

func zhipuQuotaHost(baseURL string) string {
	switch u := strings.ToLower(baseURL); {
	case strings.Contains(u, "bigmodel.cn"):
		return "https://open.bigmodel.cn"
	case strings.Contains(u, "z.ai"):
		return "https://api.z.ai"
	default:
		// 国产优先：未知域名回落国内站（与前端 zhipu 预设一致）。
		return "https://open.bigmodel.cn"
	}
}

// parseKimiUsageTiers 解析 Kimi For Coding 的 /usages 响应。
//
//   - limits[].detail.{limit,remaining,resetTime} → 5h 窗口（取首个 detail）
//   - usage.{limit,remaining,resetTime} → 周窗口
//
// utilization = (limit-remaining)/limit*100。
func parseKimiUsageTiers(body []byte) []CNQuotaTier {
	var tiers []CNQuotaTier

	if limits := gjson.GetBytes(body, "limits"); limits.IsArray() {
		limits.ForEach(func(_, item gjson.Result) bool {
			detail := item.Get("detail")
			if !detail.Exists() {
				return true
			}
			limit, _ := cnParseF64(detail.Get("limit").Value())
			remaining, _ := cnParseF64(detail.Get("remaining").Value())
			used := limit - remaining
			if used < 0 {
				used = 0
			}
			var util float64
			if limit > 0 {
				util = used / limit * 100
			}
			tiers = append(tiers, CNQuotaTier{
				Window:      "5h",
				UsedPercent: util,
				ResetAt:     cnNormalizeResetTime(detail.Get("resetTime").Value()),
			})
			return false // 取首个 detail 作为 5h 窗口
		})
	}

	if usage := gjson.GetBytes(body, "usage"); usage.Exists() {
		limit, _ := cnParseF64(usage.Get("limit").Value())
		remaining, _ := cnParseF64(usage.Get("remaining").Value())
		used := limit - remaining
		if used < 0 {
			used = 0
		}
		var util float64
		if limit > 0 {
			util = used / limit * 100
		}
		tiers = append(tiers, CNQuotaTier{
			Window:      "weekly",
			UsedPercent: util,
			ResetAt:     cnNormalizeResetTime(usage.Get("resetTime").Value()),
		})
	}

	return tiers
}

// parseMiniMaxUsageTiers 解析 MiniMax Token Plan / Coding Plan remains 响应。
//
// 只取 model_name == "general"（编程套餐），跳过 video。字段是剩余百分比，
// 展示已用 = 100 - remaining：
//   - 5h：current_interval_remaining_percent + end_time
//   - 周限额：仅 current_weekly_status == 1 时用 current_weekly_remaining_percent + weekly_end_time
func parseMiniMaxUsageTiers(body []byte) []CNQuotaTier {
	remains := gjson.GetBytes(body, "model_remains")
	if !remains.IsArray() {
		return nil
	}
	var general gjson.Result
	remains.ForEach(func(_, item gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(item.Get("model_name").String()), "general") {
			general = item
			return false
		}
		return true
	})
	if !general.Exists() {
		return nil
	}

	var tiers []CNQuotaTier
	if remaining, ok := cnParseF64(general.Get("current_interval_remaining_percent").Value()); ok {
		used := 100 - remaining
		if used < 0 {
			used = 0
		}
		tiers = append(tiers, CNQuotaTier{
			Window:      "5h",
			UsedPercent: used,
			ResetAt:     minimaxResetTime(general.Get("end_time")),
		})
	}
	if general.Get("current_weekly_status").Int() == 1 {
		if remaining, ok := cnParseF64(general.Get("current_weekly_remaining_percent").Value()); ok {
			used := 100 - remaining
			if used < 0 {
				used = 0
			}
			tiers = append(tiers, CNQuotaTier{
				Window:      "weekly",
				UsedPercent: used,
				ResetAt:     minimaxResetTime(general.Get("weekly_end_time")),
			})
		}
	}
	return tiers
}

func minimaxResetTime(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	ms := v.Int()
	if ms <= 0 {
		return cnNormalizeResetTime(v.Value())
	}
	if ms < 1_000_000_000_000 {
		return time.Unix(ms, 0).UTC().Format(time.RFC3339)
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// cnZhipuWindow 标识智谱 TOKENS_LIMIT 条目所属窗口。
type cnZhipuWindow int

const (
	cnZhipuWindowUnknown cnZhipuWindow = iota
	cnZhipuWindow5h
	cnZhipuWindowWeekly
)

// classifyZhipuWindowUnit 按 unit 字段判定窗口类型（3=5h，6=weekly）。
// unit 缺失或未识别时返回 Unknown，由调用方走 reset 时间启发式兜底。
func classifyZhipuWindowUnit(unit int64) cnZhipuWindow {
	switch unit {
	case 3:
		return cnZhipuWindow5h
	case 6:
		return cnZhipuWindowWeekly
	default:
		return cnZhipuWindowUnknown
	}
}

// parseZhipuTokenTiers 解析智谱额度响应 data.limits 为 5h + weekly 两档。
//
// 分类优先级（对齐 cc-switch parse_zhipu_token_tiers，issue #3036）：
//  1. 显式 unit 字段（3=5h / 6=weekly）——不能用 reset 排序代替，周期末尾
//     周窗口会比 5h 更早重置，时间排序必然标反。
//  2. unit 缺失/未识别：无 nextResetTime 的条目优先归 5h（0% 状态下 5h 桶可能
//     没有 reset），其余按 reset 升序依次填入仍空缺的槽位。
//
// CREDIT_LIMIT（信用额度）与 TOKENS_LIMIT（token 窗口）度量不同：两者同时返回时
// 只让 TOKENS_LIMIT 参与 5h/weekly 槽位竞争，避免信用额度百分比污染阈值停调
// 快照；仅当无任何 TOKENS_LIMIT 条目时才降级用 CREDIT_LIMIT 展示。
// 老套餐只回 1 条 TOKENS_LIMIT，自然降级为仅 5h；新套餐回 2 条。
func parseZhipuTokenTiers(data gjson.Result) []CNQuotaTier {
	type entry struct {
		resetMs    int64
		hasReset   bool
		percentage float64
		resetISO   string
	}
	var (
		fiveHour     entry
		fiveHourSet  bool
		weekly       entry
		weeklySet    bool
		unclassified []entry
	)

	classify := func(item gjson.Result, e entry) {
		switch classifyZhipuWindowUnit(item.Get("unit").Int()) {
		case cnZhipuWindow5h:
			if !fiveHourSet {
				fiveHour, fiveHourSet = e, true
			} else {
				unclassified = append(unclassified, e)
			}
		case cnZhipuWindowWeekly:
			if !weeklySet {
				weekly, weeklySet = e, true
			} else {
				unclassified = append(unclassified, e)
			}
		default:
			unclassified = append(unclassified, e)
		}
	}
	var creditFallback []entry
	hasTokensLimit := false

	data.Get("limits").ForEach(func(_, item gjson.Result) bool {
		limitType := strings.ToUpper(strings.TrimSpace(item.Get("type").String()))
		if limitType != "TOKENS_LIMIT" && limitType != "CREDIT_LIMIT" {
			return true
		}
		percentage := 0.0
		if p, ok := cnParseF64(item.Get("percentage").Value()); ok {
			percentage = p
		}
		var (
			resetMs  int64
			hasReset bool
			resetISO string
		)
		if nr := item.Get("nextResetTime"); nr.Exists() {
			switch nr.Type {
			case gjson.Number:
				resetMs = nr.Int()
				hasReset = resetMs > 0
				resetISO = cnMillisToRFC3339(resetMs)
			case gjson.String:
				resetISO = cnNormalizeResetTime(nr.String())
				hasReset = resetISO != ""
			}
		}
		e := entry{resetMs: resetMs, hasReset: hasReset, percentage: percentage, resetISO: resetISO}
		if limitType == "TOKENS_LIMIT" {
			hasTokensLimit = true
			classify(item, e)
		} else {
			creditFallback = append(creditFallback, e)
		}
		return true
	})

	// 无任何 TOKENS_LIMIT 条目（部分套餐只报信用额度）：降级用 CREDIT_LIMIT 展示。
	if !hasTokensLimit {
		unclassified = append(unclassified, creditFallback...)
	}

	// 无 reset 的条目排前，再按 reset 升序，依次填入仍空缺的槽位。
	sort.SliceStable(unclassified, func(i, j int) bool {
		if unclassified[i].hasReset != unclassified[j].hasReset {
			return !unclassified[i].hasReset
		}
		return unclassified[i].resetMs < unclassified[j].resetMs
	})
	for _, e := range unclassified {
		switch {
		case !fiveHourSet:
			fiveHour, fiveHourSet = e, true
		case !weeklySet:
			weekly, weeklySet = e, true
		}
	}

	var tiers []CNQuotaTier
	if fiveHourSet {
		tiers = append(tiers, CNQuotaTier{Window: "5h", UsedPercent: fiveHour.percentage, ResetAt: fiveHour.resetISO})
	}
	if weeklySet {
		tiers = append(tiers, CNQuotaTier{Window: "weekly", UsedPercent: weekly.percentage, ResetAt: weekly.resetISO})
	}
	return tiers
}

// cnQuotaExtraUpdates 根据 tier 列表构造 provider 维度的 Extra 快照更新。
// 语义：整轮快照替换。对 5h / weekly 两个窗口，本轮有档则写 used + reset
// （reset 为空时写 nil 清除旧倒计时，避免上游从有效时间变为 -1/缺失后前端
// 继续显示过期倒计时）；本轮缺档则整档键（used + reset）都写 nil 清除，
// 防止上游只返回单档时另一档的 stale 值残留。
func cnQuotaExtraUpdates(provider string, tiers []CNQuotaTier, now time.Time) map[string]any {
	updates := map[string]any{
		cnExtraKey(provider, cnExtraSuffixUsageUpdated): now.Format(time.RFC3339),
	}
	// 本轮实际存在的窗口（用于缺档清除判定）。
	presentWindows := map[string]bool{}
	for _, t := range tiers {
		presentWindows[t.Window] = true
	}
	writeTier := func(usedSuffix, resetSuffix string, used any, resetAt string) {
		updates[cnExtraKey(provider, usedSuffix)] = used
		if resetAt != "" {
			updates[cnExtraKey(provider, resetSuffix)] = resetAt
		} else {
			// 上游无有效重置时间：写 nil（JSON null）清除 DB 中旧 reset 值，
			// 与 parseSchedulingResetAt(nil) → nil、前端 readExtraString(null) → ''
			// 的读取链一致：既不再参与 429 冷却，也不渲染倒计时。
			updates[cnExtraKey(provider, resetSuffix)] = nil
		}
	}
	if presentWindows["5h"] {
		for _, t := range tiers {
			if t.Window == "5h" {
				if t.UsedPercentUnknown {
					// 5h 用量上游不可得（limit/remaining 缺失/畸形）时不得写假 0，否则前端渲染为
					// 0% 误导“未用”并影响调度；仅落重置时间，used 键置 nil 让前端显示“未知/—”。
					if t.ResetAt != "" {
						updates[cnExtraKey(provider, cnExtraSuffix5hReset)] = t.ResetAt
					} else {
						updates[cnExtraKey(provider, cnExtraSuffix5hReset)] = nil
					}
					updates[cnExtraKey(provider, cnExtraSuffix5hUsed)] = nil
				} else {
					writeTier(cnExtraSuffix5hUsed, cnExtraSuffix5hReset, t.UsedPercent, t.ResetAt)
				}
				break
			}
		}
	} else {
		// 缺 5h 档：清除残留的 5h used + reset 键。
		updates[cnExtraKey(provider, cnExtraSuffix5hUsed)] = nil
		updates[cnExtraKey(provider, cnExtraSuffix5hReset)] = nil
	}
	if presentWindows["weekly"] {
		for _, t := range tiers {
			if t.Window == "weekly" {
				if t.UsedPercentUnknown {
					// 周用量上游不可得（限流响应头仅含 5h 窗口），不得写假 0 覆盖旧值/
					// 误导“未用”。仅落确定性重置时间；used 键置 nil 让前端显示“未知”。
					if t.ResetAt != "" {
						updates[cnExtraKey(provider, cnExtraSuffixWeeklyReset)] = t.ResetAt
					} else {
						updates[cnExtraKey(provider, cnExtraSuffixWeeklyReset)] = nil
					}
					updates[cnExtraKey(provider, cnExtraSuffixWeeklyUsed)] = nil
				} else {
					writeTier(cnExtraSuffixWeeklyUsed, cnExtraSuffixWeeklyReset, t.UsedPercent, t.ResetAt)
				}
				break
			}
		}
	} else {
		// 缺 weekly 档：清除残留的 weekly used + reset 键。
		updates[cnExtraKey(provider, cnExtraSuffixWeeklyUsed)] = nil
		updates[cnExtraKey(provider, cnExtraSuffixWeeklyReset)] = nil
	}
	if presentWindows["monthly"] {
		for _, t := range tiers {
			if t.Window == "monthly" {
				if t.UsedPercentUnknown {
					// 月用量上游不可得，不得写假 0。仅落确定性重置时间；used 键置 nil。
					if t.ResetAt != "" {
						updates[cnExtraKey(provider, cnExtraSuffixMonthlyReset)] = t.ResetAt
					} else {
						updates[cnExtraKey(provider, cnExtraSuffixMonthlyReset)] = nil
					}
					updates[cnExtraKey(provider, cnExtraSuffixMonthlyUsed)] = nil
				} else {
					writeTier(cnExtraSuffixMonthlyUsed, cnExtraSuffixMonthlyReset, t.UsedPercent, t.ResetAt)
				}
				break
			}
		}
	} else {
		// 缺 monthly 档：清除残留的 monthly used + reset 键。
		updates[cnExtraKey(provider, cnExtraSuffixMonthlyUsed)] = nil
		updates[cnExtraKey(provider, cnExtraSuffixMonthlyReset)] = nil
	}
	return updates
}

// cnParseF64 把 JSON 数值或字符串解析为 float64（兼容 "100" 与 100）。
func cnParseF64(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// cnNormalizeResetTime 把上游重置时间（ISO8601 字符串 / 秒级 / 毫秒级数字）归一化为
// RFC3339 字符串；无法识别或非正时间戳返回空串。
func cnNormalizeResetTime(raw any) string {
	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return ""
		}
		if ts, err := parseSchedulingTime(s); err == nil {
			return ts.UTC().Format(time.RFC3339)
		}
		return ""
	case float64:
		return cnMillisToRFC3339(int64(v))
	case int:
		return cnMillisToRFC3339(int64(v))
	case int64:
		return cnMillisToRFC3339(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return cnMillisToRFC3339(n)
		}
		return ""
	default:
		return ""
	}
}

// cnMillisToRFC3339 把秒级（<1e12）或毫秒级时间戳转为 RFC3339 字符串；非正返回空串。
func cnMillisToRFC3339(n int64) string {
	if n <= 0 {
		return ""
	}
	var ms int64
	if n < 1_000_000_000_000 {
		ms = n * 1000
	} else {
		ms = n
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
