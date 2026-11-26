package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// 阿里云百炼 Token Plan 用量显示：读官方数据，不做本地实算。
//
// ## 为什么是「读官方」而不是「算出来」
//
// 2026-11-26 用户裁定：用量显示必须读取官方数据，本地按调用日志 × 官方单价表折算
// Credits 的做法**已全部作废**（价格表分页拉取、阶梯逐请求选档、夜间折扣系数、
// Credits 公式、Go/Python 双实现、官方算例单价护栏测试全部删除）。
//
// ## 官方接口全景（实证，非推测）
//
// 官方 OpenAPI 元数据（https://api.aliyun.com/meta/v1/products/ModelStudio/versions/
// 2026-02-10/api-docs，唯一公开版本共 38 个接口，TokenPlan 目录 19 个）里
// **没有任何「个人版已用量」接口**。给程序读额度的官方通道只有两条：
//
//  1. **团队席位版**（组织下有席位时）：AK/SK 直读官方剩余额度，零计算。
//     GET /tokenplan/subscription/stats → Data.Items[].SeatCredits / SeatRemainingCredits
//                                     / SeatRefreshTime；Data.SubscriptionEndTime。
//     GET /tokenplan/subscription/seat-detail → Data.Items[].EquityList[].
//                                     CycleTotalValue / CycleSurplusValue / CycleEndTime。
//
//  2. **个人版 solo**（本项目当前账号；账单商品码 sfm_tokenplansolo_public_cn）：
//     OpenAPI 侧 GetSubscriptionStats 返回 {"Success":true} 但**无 Data**（没有席位），
//     seat-detail / shared-packages 均 Total 0，GetTokenPlanAccountDetail 只有账号与
//     组织信息。官方给程序读个人版用量的唯一通道是**控制台登录态**（Cookie + sec_token）：
//     zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/{usage,subscription,quota-config}
//     （官方 CLI 源码里声明 auth=console）。
//     实测：用 AK/SK 换来的 CLI token 打这三个接口一律
//     `BailianGateway.Team.NotAuthorised`；sk-sp- Key 各种放法一律
//     `BailianGateway.Login.NotLogined`；**官方 CLI 自身**用同一对 AK/SK 走同一条路
//     得到完全相同的错误码 ⇒ 官方对 RAM 子账号的既定限制，不是实现问题。
//     唯一用 AK/SK 能读通的是 **quota-config**（官方档位配额表，静态），它给出各档位的
//     官方额度总量（standard.monthly=80000 等）。
//
// ## 本文件的探测口径
//
//   - 团队席位版：官方 stats/seat-detail 直接给「总额度 / 剩余额度 / 重置时间」⇒ 全是官方值。
//   - 个人版：官方 quota-config 给档位与总额度（零计算）；官方 usage 接口给
//     「已用百分比 + 重置时间」（需账号配控制台 Cookie）。
//   - 绝不用本仓调用日志折算 Credits。

const (
	// aliyunQuotaMaxBodyBytes 是探测响应体的读取上限。
	aliyunQuotaMaxBodyBytes = 2 << 20
)

// aliyunTokenPlanBaseURLHosts 是阿里 Token Plan 的入口主机（base_url 认它 ⇒ 认成阿里账号）。
var aliyunTokenPlanBaseURLHosts = map[string]string{
	"token-plan.cn-beijing.maas.aliyuncs.com":     "modelstudio.cn-beijing.aliyuncs.com",
	"token-plan.ap-southeast-1.maas.aliyuncs.com": "modelstudio.ap-southeast-1.aliyuncs.com",
}

// isAliyunTokenPlanBaseURL 判断 base_url 是不是阿里 Token Plan 入口。
//
// 口径：主机名**精确匹配**白名单（子域、相似域名一律不算），并要求路径含
// compatible-mode 段（官方 Token Plan 专属 Key 的调用地址）。
func isAliyunTokenPlanBaseURL(baseURL string) bool {
	host, ok := aliyunBaseURLHost(baseURL)
	if !ok {
		return false
	}
	_, known := aliyunTokenPlanBaseURLHosts[host]
	return known
}

// aliyunBaseURLHost 从 base_url 取小写主机名。
func aliyunBaseURLHost(baseURL string) (string, bool) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return "", false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	return strings.ToLower(parsed.Hostname()), true
}

// aliyunModelStudioHostForBaseURL 把 Token Plan 入口主机映射到 ModelStudio 主机。
func aliyunModelStudioHostForBaseURL(baseURL string) (string, bool) {
	host, ok := aliyunBaseURLHost(baseURL)
	if !ok {
		return "", false
	}
	modelStudio, known := aliyunTokenPlanBaseURLHosts[host]
	if !known {
		return "", false
	}
	return modelStudio, true
}

// aliyunTokenPlanRegionForHost 把 ModelStudio 主机映射到地域 ID。
func aliyunTokenPlanRegionForHost(host string) string {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "modelstudio.ap-southeast-1.aliyuncs.com":
		return "ap-southeast-1"
	default:
		return "cn-beijing"
	}
}

// aliyunQuotaSnapshot 是阿里 Token Plan 用量快照（落 account.Extra 与前端展示共用）。
//
// 所有字段都来自**官方接口**，没有一个是本地折算出来的。
type aliyunQuotaSnapshot struct {
	// PlanName 是官方档位名（standard / lite / essential / pro；团队席位版是席位档位）。
	PlanName string `json:"plan_name"`
	// MonthlyCredit 是官方给出的该档位每月 Credits 额度（quota-config / SeatCredits）。
	MonthlyCredit float64 `json:"monthly_credit"`
	// FiveHourCredit 是官方给出的 5 小时窗口额度（quota-config 的 five_hour）。
	FiveHourCredit float64 `json:"five_hour_credit,omitempty"`
	// UsedCredit 是**已用** Credits。
	//
	// 数据来源分两种，都不涉及本地按 token 折算：
	//   - 团队席位版：官方 SeatCredits - SeatRemainingCredits（官方两个数相减）。
	//   - 个人版：官方 usage 接口给的是**已用百分比**，官方不直接给 Credits 数；
	//     此处按「官方百分比 × 官方 quota-config 月额度」换算成 Credits 便于阅读，
	//     换算的两个因子都是官方值，不是本地估的。
	//
	// ⚠️ 语义边界：官方百分比对应的**实际配额**未必等于档位上限（叠加用量包时更高），
	// 所以个人版的 UsedCredit 是「按档位上限折算的参考值」，UsedPercent 才是官方原值。
	UsedCredit float64 `json:"used_credit"`
	// RemainingCredit 是剩余 Credits（团队席位版 = 官方 SeatRemainingCredits）。
	RemainingCredit float64 `json:"remaining_credit"`
	// UsedPercent 是**官方原值**的已用百分比（0-100+）。
	// 团队席位版 = 官方 SeatCredits/SeatRemainingCredits 推出的百分比；
	// 个人版 = 官方 usage 接口的 per{N}MonthPercentage × 100（原值，未加工）。
	UsedPercent float64 `json:"used_percent"`
	// ResetAt 是官方给出的重置时间（RFC3339）。
	ResetAt string `json:"reset_at,omitempty"`
	// Exhausted 表示上游明确回报额度已耗尽（429 insufficient_quota，硬信号）。
	Exhausted bool `json:"exhausted"`
	// UncoveredModels 保留字段：团队席位版读不到模型级明细时为空（不再做模型级实算）。
	UncoveredModels []string `json:"uncovered_models,omitempty"`
	// PoolAccountCount 是共享该额度池的账号数（1 = 该订阅只挂在本账号上）。
	PoolAccountCount int `json:"pool_account_count,omitempty"`
	// DataSource 标明这批数字来自哪个官方接口（便于排查，不参与计算）。
	DataSource string `json:"data_source,omitempty"`
	// FetchedAt 是快照抓取时间（RFC3339）。
	FetchedAt string `json:"fetched_at"`
}

// aliyunQuotaSnapshotKey 是阿里 Token Plan 快照在 account.Extra 里的键。
const aliyunQuotaSnapshotKey = "aliyun_token_plan_usage_snapshot"

// aliyun 探测的稳定错误码（前端据此显示中文提示，不解析 error 文案）。
const (
	// aliyunErrNoCredential：账号没填 AccessKey/SecretKey（最常见的阻塞点）。
	aliyunErrNoCredential = "ALIYUN_NO_CREDENTIAL"
	// aliyunErrBadEndpoint：账号的 base_url 不是阿里 Token Plan 入口。
	aliyunErrBadEndpoint = "ALIYUN_BAD_ENDPOINT"
	// aliyunErrHostUnresolvable：base_url 对应的 ModelStudio 主机认不出来。
	aliyunErrHostUnresolvable = "ALIYUN_HOST_UNRESOLVABLE"
	// aliyunErrQuotaUnavailable：官方额度接口调用失败（网络/权限/接口报错）。
	aliyunErrQuotaUnavailable = "ALIYUN_QUOTA_UNAVAILABLE"
	// aliyunErrBillingUnavailable：官方账单拉不到（反查不出档位）。
	aliyunErrBillingUnavailable = "ALIYUN_BILLING_UNAVAILABLE"
	// aliyunErrTierUnknown：官方没给出可识别的档位配额。
	aliyunErrTierUnknown = "ALIYUN_TIER_UNKNOWN"
	// aliyunErrPoolResolveFailed：额度池归并失败。
	aliyunErrPoolResolveFailed = "ALIYUN_POOL_RESOLVE_FAILED"
	// aliyunErrUsageStatsUnavailable：本仓用量读取失败（保留码，前端有中文文案）。
	aliyunErrUsageStatsUnavailable = "ALIYUN_USAGE_STATS_UNAVAILABLE"
	// aliyunErrConsoleCookieRequired：个人版官方用量接口只认控制台登录态，
	// 账号没配控制台 Cookie 时报这个（这是官方限制，不是配置错）。
	aliyunErrConsoleCookieRequired = "ALIYUN_CONSOLE_COOKIE_REQUIRED"
	// aliyunErrConsoleRejected：控制台登录态被官方拒了（Cookie 过期或 sec_token 缺失）。
	aliyunErrConsoleRejected = "ALIYUN_CONSOLE_REJECTED"
)

// aliyunQuotaPool 描述一个共享额度池。
type aliyunQuotaPool struct {
	// PoolKey 是归组键（主机 + AK 主体）。
	PoolKey string
	// AccountIDs 是池内账号 ID 列表。
	AccountIDs []int64
	// PoolAccountCount 是池内账号数。
	PoolAccountCount int
	// AnchorDay 是池内最早创建账号的创建日（订阅月锚点）。
	AnchorDay int
}

// aliyunQuotaPoolKey 生成额度池归组键：主机 + AccessKey 主体。
//
// 口径（2026-11-26 实证）：官方 FAQ「系统会根据 API Key 自动识别对应的套餐」+
// 「重置 API Key 会使旧 Key 立即失效」⇒ 池绑订阅；「同一个 API Key 可配到多台设备」
// ⇒ 一把 Key 同池。归组键用**主机 + AK 主体**，SK 不参与（同一主体可有多把 AK）；
// **不按 sk-sp- Key 归并**（同一订阅的多个 Key 各不相同，按它归并会拆成多份）。
func aliyunQuotaPoolKey(host, accessKey string) string {
	return strings.ToLower(strings.TrimSpace(host)) + "|" + strings.TrimSpace(accessKey)
}

// aliyunResolveQuotaPool 归并共享同一额度池的账号。
//
// 官方口径（《Token Plan 常见问题》）：
//   - 「额度绑定个人账号，仅限本人使用」⇒ 一份套餐额度属于**账号**。
//   - 「同一个 API Key 可以配置到本人的多台设备上使用」⇒ 同一订阅下的多把
//     sk-sp- Key 扣的是**同一份额度**。
//   - 「系统会根据 API Key 自动识别对应的套餐」⇒ 套餐归属挂在 Key 背后的账号上。
//
// ⇒ 归组键用**主机 + AccessKey 主体**（同一订阅下的多把 sk-sp- Key 共享额度，
// 但它们的 AK 相同 ⇒ 归到同一池）。不按 sk-sp- Key 归并（同一订阅的多个 Key
// 各不相同，按它归并会把一份订阅拆成多份，导致每个账号都显示"才用了 1/3"）。
//
// 候选取全部启用中的账号（凭据在仓储层已解密，可直接比对 access_key）。
// 账号量级是运营规模（百级），逐个比对代价可忽略；用 ListActive 而不是另开
// 一条 SQL，是为了让"凭据解密"这件事只有仓储层一处实现。
func (s *CNProviderQuotaService) aliyunResolveQuotaPool(
	ctx context.Context,
	account *Account,
	host string,
	ak string,
) (*aliyunQuotaPool, error) {
	poolKey := aliyunQuotaPoolKey(host, ak)
	// 池归并只看启用中的账号（停用账号不参与调度，也不该影响池子计数）。
	candidates, err := s.accountRepo.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	pool := &aliyunQuotaPool{PoolKey: poolKey, AccountIDs: []int64{account.ID}, PoolAccountCount: 1}
	anchorDay := account.CreatedAt.Day()
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.ID == account.ID {
			continue
		}
		if !isAliyunTokenPlanBaseURL(candidate.GetBaseURL()) {
			continue
		}
		candidateHost, ok := aliyunModelStudioHostForBaseURL(candidate.GetBaseURL())
		if !ok || !strings.EqualFold(candidateHost, host) {
			continue
		}
		candidateAK := strings.TrimSpace(candidate.GetCredential("access_key"))
		if candidateAK == "" || candidateAK != ak {
			continue
		}
		pool.AccountIDs = append(pool.AccountIDs, candidate.ID)
		pool.PoolAccountCount++
		if day := candidate.CreatedAt.Day(); day < anchorDay {
			anchorDay = day
		}
	}
	pool.AnchorDay = anchorDay
	return pool, nil
}

// aliyunSubscriptionMonthRange 算本期订阅月的起止（锚点 = 池内最早创建账号的创建日）。
//
// 官方口径：额度按**订阅月**重置，重置时间取决于订阅日期（不是每月 1 日）。
func aliyunSubscriptionMonthRange(now time.Time, anchorDay int) (time.Time, time.Time) {
	if anchorDay < 1 || anchorDay > 31 {
		anchorDay = 1
	}
	loc := aliyunTokenPlanLoc()
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), anchorDay, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 1, 0)
	if !local.Before(start) {
		return start, end
	}
	// 已过锚点 ⇒ 本期是上个月开始的那一期。
	return start.AddDate(0, -1, 0), end.AddDate(0, -1, 0)
}

// aliyunTokenPlanLoc 返回阿里计费时区（Asia/Shanghai）。
var aliyunTokenPlanLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}

// aliyunInsufficientQuotaPattern 匹配上游"额度用尽"的硬信号。
//
// 官方《Token Plan 常见问题》：「429 insufficient_quota: Your token-plan quota has been
// exhausted. 套餐额度或用量包额度用尽」—— 上游亲口说的"没额度了"。
var aliyunInsufficientQuotaPattern = regexp.MustCompile(`(?i)insufficient_quota|quota has been exhausted`)

// aliyunIsInsufficientQuota 报告响应体是否为"额度用尽"。
func aliyunIsInsufficientQuota(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	return aliyunInsufficientQuotaPattern.Match(body)
}

// aliyunFetchSubscriptionFee 读官方账单当月金额（个人版套餐费）。
//
// 返回金额与是否 solo 商品码。
func (s *CNProviderQuotaService) aliyunFetchSubscriptionFee(
	ctx context.Context,
	account *Account,
	signer *aliyunACS3Signer,
	host string,
	billMonth string,
) (float64, bool, error) {
	query := url.Values{}
	query.Set("billMonth", billMonth)
	query.Set("groupBy", `[{"code":"ARTICLE_CODE"}]`)
	status, body, err := s.aliyunCallModelStudio(ctx, account, signer, host,
		"/modelstudio/billing/overview", query)
	if err != nil {
		return 0, false, err
	}
	if status != http.StatusOK {
		return 0, false, fmt.Errorf("aliyun billing http %d: %s",
			status, truncate(strings.TrimSpace(string(body)), 240))
	}
	fee := 0.0
	isSolo := false
	groups := gjson.GetBytes(body, "data.groups")
	if groups.Exists() {
		for _, group := range groups.Array() {
			amount := group.Get("amount")
			if amount.Exists() {
				if parsed, perr := strconv.ParseFloat(amount.String(), 64); perr == nil {
					fee += parsed
				}
			}
			for _, code := range group.Get("articleCodes").Array() {
				if strings.EqualFold(code.String(), aliyunTokenPlanSoloProductCode) {
					isSolo = true
				}
			}
		}
	}
	if fee == 0 {
		return 0, false, fmt.Errorf("aliyun billing returned no subscription fee")
	}
	return fee, isSolo, nil
}

// aliyunCallModelStudio 调一次 ModelStudio OpenAPI（/modelstudio/billing/* 等）。
//
// 签名统一用 ACS3-HMAC-SHA256（见 aliyun_token_plan_official.go）：
// 2026-11-26 生产实测 —— 老 RPC 签名（HMAC-SHA1 + Date 头）**整条路都不通**：
//   - Date 头要 RFC1123（ISO8601 ⇒ 服务端报 InvalidDate）
//   - 即便用 RFC1123，服务端回显的 stringToSign 是 RPC 风格
//     （Accept / Content-MD5 / Content-Type / Date 各占一行），
//     与老 RPC 的 ROA 风格不同，怎么试都对不上
//   - 同一个接口换 ACS3（x-acs-date = ISO8601）⇒ HTTP 200 直接拿到账单数据
// ⇒ 全站只保留 ACS3 一套签名，不再有"两套签名各写一份"的认知分裂。
func (s *CNProviderQuotaService) aliyunCallModelStudio(
	ctx context.Context,
	account *Account,
	signer *aliyunACS3Signer,
	host string,
	path string,
	query url.Values,
) (int, []byte, error) {
	if s == nil || s.httpUpstream == nil {
		return 0, nil, fmt.Errorf("http upstream is not configured")
	}
	targetURL := "https://" + host + path
	if len(query) > 0 {
		encoded := url.Values{}
		for k, vs := range query {
			for _, v := range vs {
				encoded.Add(k, v)
			}
		}
		targetURL += "?" + encoded.Encode()
	}
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return 0, nil, fmt.Errorf("aliyun probe url rejected: %w", err)
	}
	proxyURL := s.resolveProxyURL(ctx, account)
	// 网络层瞬时故障做有界退避重试（业务错误不重试）。
	return aliyunRetryOnTransient(ctx, 3, func() (int, []byte, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, validatedURL, nil)
		if rerr != nil {
			return 0, nil, fmt.Errorf("build aliyun request: %w", rerr)
		}
		// billing 这类接口按路径寻址，不带 x-acs-action（带了会 404 InvalidAction.NotFound）。
		headers, herr := signer.aliyunACS3Sign(http.MethodGet, host, path, query, map[string]string{
			"x-acs-version": aliyunModelStudioVersion,
		}, nil, time.Now())
		if herr != nil {
			return 0, nil, herr
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		account.ApplyHeaderOverrides(req.Header)

		resp, derr := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
		if derr != nil {
			return 0, nil, fmt.Errorf("aliyun request failed: %w", derr)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := readAllLimited(resp.Body, aliyunQuotaMaxBodyBytes)
		return resp.StatusCode, body, nil
	})
}

// readAllLimited 读响应体（限长）。
func readAllLimited(r interface{ Read([]byte) (int, error) }, limit int64) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 32*1024)
	var total int64
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			total += int64(n)
			if total > limit {
				keep := int(total - limit)
				if keep < n {
					buf = append(buf, tmp[:keep]...)
				}
				return buf, nil
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			return buf, nil
		}
	}
}

// aliyunTierQuotaByName 从官方 quota-config 表里取某个档位的额度。
func aliyunTierQuotaByName(table aliyunOfficialTierQuotaTable, name string) (aliyunOfficialTierQuota, bool) {
	quota, ok := table[strings.ToLower(strings.TrimSpace(name))]
	return quota, ok
}

// aliyunProbeTokenPlan 是阿里 Token Plan 用量探测的编排（读官方数据，零计算）。
//
// 分支：
//  1. **团队席位版**（AK/SK）：官方 GetSubscriptionStats / GetSubscriptionSeatDetails
//     直接给总额度、剩余额度、重置时间 ⇒ 全是官方值。
//  2. **个人版 solo**（AK/SK + 可选控制台 Cookie）：
//     - 官方 quota-config 给档位与官方额度总量（AK/SK 即可读）；
//     - 官方 usage 接口给已用百分比与重置时间（**必须**控制台 Cookie，官方只认登录态）；
//     - 没有 Cookie 时仍可给出档位与总额度，并明确告知"已用需要控制台登录态"。
//
// 任一环节失败 → 失败关闭（明确错误码，不落快照、不产出假 0%）。
func (s *CNProviderQuotaService) aliyunProbeTokenPlan(ctx context.Context, account *Account) (*CNProviderQuotaProbeResult, error) {
	now := time.Now().UTC()
	result := &CNProviderQuotaProbeResult{
		Provider:  providerAliyun,
		Source:    "aliyun_token_plan",
		FetchedAt: now.Unix(),
	}

	// 凭据：AK/SK 放在账号凭据里（与火山方舟同模式：access_key / secret_key）。
	// 不做跨账号继承 —— 密钥是阿里主体（IAM 身份）级凭据，不同主体绝不能互相借用。
	ak := strings.TrimSpace(account.GetCredential("access_key"))
	sk := strings.TrimSpace(account.GetCredential("secret_key"))
	baseURL := account.GetBaseURL()
	if !isAliyunTokenPlanBaseURL(baseURL) {
		result.Error = "account base_url is not an aliyun token plan endpoint"
		result.ErrorCode = aliyunErrBadEndpoint
		return result, nil
	}
	host, ok := aliyunModelStudioHostForBaseURL(baseURL)
	if !ok {
		result.Error = "aliyun token plan host is not resolvable"
		result.ErrorCode = aliyunErrHostUnresolvable
		return result, nil
	}
	if ak == "" || sk == "" {
		result.Error = "aliyun access key is not configured for this account"
		result.ErrorCode = aliyunErrNoCredential
		return result, nil
	}
	acs3 := &aliyunACS3Signer{AccessKeyID: ak, AccessKeySecret: sk}

	// 额度池归并：同一份订阅被拆成多个账号时，官方把它们算作**同一个池子**
	// （同一订阅的多个 sk-sp- Key 扣的是同一份额度）。
	pool, err := s.aliyunResolveQuotaPool(ctx, account, host, ak)
	if err != nil {
		result.Error = "aliyun quota pool resolution failed: " + err.Error()
		result.ErrorCode = aliyunErrPoolResolveFailed
		return result, nil
	}

	snapshot := aliyunQuotaSnapshot{
		FetchedAt:        now.Format(time.RFC3339),
		PoolAccountCount: pool.PoolAccountCount,
	}

	// 1) 先试团队席位版：官方直接给剩余额度（零计算，最准）。
	seatQuota, seatErr := s.aliyunFetchOfficialSeatQuota(ctx, account, acs3, host)
	if seatErr != nil {
		slog.Warn("aliyun_seat_quota_failed", "account_id", account.ID, "error", seatErr)
	} else if seatQuota != nil {
		snapshot.PlanName = seatQuota.PlanName
		snapshot.MonthlyCredit = seatQuota.TotalCredits
		snapshot.UsedCredit = seatQuota.UsedCredits
		snapshot.RemainingCredit = seatQuota.RemainingCredits
		snapshot.UsedPercent = seatQuota.UsedPercent
		snapshot.ResetAt = seatQuota.ResetAt
		snapshot.DataSource = "official_subscription_stats"
		return s.aliyunFinishProbe(ctx, account, result, snapshot)
	}

	// 2) 个人版：官方 quota-config（AK/SK 可读）给档位与官方额度总量。
	tierTable, tierErr := s.aliyunConsoleTierQuota(ctx, account, acs3, host)
	if tierErr != nil {
		slog.Warn("aliyun_quota_config_failed", "account_id", account.ID, "error", tierErr)
	}
	planName, monthlyCredit, fiveHourCredit := s.aliyunResolvePersonalTier(ctx, account, acs3, host, tierTable, tierErr)
	if planName == "" {
		result.Error = "aliyun subscription tier is unknown (official quota-config and billing did not resolve a plan)"
		result.ErrorCode = aliyunErrTierUnknown
		return result, nil
	}
	snapshot.PlanName = planName
	snapshot.MonthlyCredit = monthlyCredit
	snapshot.FiveHourCredit = fiveHourCredit
	snapshot.DataSource = "official_quota_config"

	// 3) 个人版的已用百分比与重置时间：官方 usage 接口（**只认控制台登录态**）。
	cookie := strings.TrimSpace(account.GetCredential("console_cookie"))
	secToken := strings.TrimSpace(account.GetCredential("console_sec_token"))
	if cookie == "" {
		result.Error = "official aliyun usage api requires a console session; " +
			"configure console_cookie for this account to read used credits"
		result.ErrorCode = aliyunErrConsoleCookieRequired
		// 仍然把已确认的官方档位与总额度落快照（这几个数字是官方的，不是猜的）。
		snapshot.RemainingCredit = monthlyCredit
		if err := s.aliyunPersistSnapshot(ctx, account, snapshot, result); err != nil {
			return result, err
		}
		result.AliyunUsage = &snapshot
		result.PlanLevel = planName
		result.Tiers = []CNQuotaTier{{
			Window:  "monthly",
			ResetAt: snapshot.ResetAt,
		}}
		return result, nil
	}
	usages, usageErr := s.aliyunFetchOfficialPersonalUsage(ctx, account, cookie, secToken)
	if usageErr != nil {
		if strings.Contains(usageErr.Error(), "rejected") {
			result.ErrorCode = aliyunErrConsoleRejected
		} else {
			result.ErrorCode = aliyunErrQuotaUnavailable
		}
		result.Error = "aliyun official usage api failed: " + usageErr.Error()
		return result, nil
	}
	monthlyUsage, hasMonthly := aliyunPickPersonalUsage(usages, "per1Month")
	if hasMonthly {
		// 官方给的是百分比；乘官方 quota-config 的月额度得到官方口径的已用 Credits。
		snapshot.UsedPercent = monthlyUsage.UsedPercent * 100
		snapshot.UsedCredit = monthlyUsage.UsedPercent * monthlyCredit
		snapshot.RemainingCredit = monthlyCredit - snapshot.UsedCredit
		snapshot.ResetAt = monthlyUsage.ResetAt
	} else {
		result.Error = "aliyun official usage api returned no monthly window"
		result.ErrorCode = aliyunErrQuotaUnavailable
		return result, nil
	}
	if snapshot.RemainingCredit < 0 {
		snapshot.RemainingCredit = 0
	}
	return s.aliyunFinishProbe(ctx, account, result, snapshot)
}

// aliyunResolvePersonalTier 定档位与官方额度（零计算）。
//
// 口径：
//   - 官方 quota-config 的档位表给出各档的**官方额度**（月额度 + 5 小时窗口额度）；
//   - 官方档位表本身不含价格，所以用「官方账单金额 → 官方档位」的官方公布对应关系
//     定位档位名，再回官方 quota-config 取该档的官方额度。
//
// 返回：档位名、月度 Credits、5 小时窗口 Credits（任一为 0 表示没取到）。
func (s *CNProviderQuotaService) aliyunResolvePersonalTier(
	ctx context.Context,
	account *Account,
	signer *aliyunACS3Signer,
	host string,
	tierTable aliyunOfficialTierQuotaTable,
	tierErr error,
) (string, float64, float64) {
	if tierErr != nil || len(tierTable) == 0 {
		return "", 0, 0
	}
	billMonth := time.Now().In(aliyunTokenPlanLoc()).Format("2006-01")
	fee, _, err := s.aliyunFetchSubscriptionFee(ctx, account, signer, host, billMonth)
	if err != nil {
		slog.Warn("aliyun_billing_read_failed", "account_id", account.ID, "error", err)
		return "", 0, 0
	}
	name, found := aliyunTierNameByOfficialFee(fee)
	if !found {
		return "", 0, 0
	}
	quota, ok := aliyunTierQuotaByName(tierTable, name)
	if !ok || quota.MonthlyCredits <= 0 {
		return "", 0, 0
	}
	return quota.PlanName, quota.MonthlyCredits, quota.FiveHourCredits
}

// aliyunTierNameByOfficialFee 用官方账单金额定位官方档位名。
//
// 口径：官方《Token Plan 个人版订阅与权益》给出的档位与月费：
//
//	Lite 39 元 / Essential 79 元 / Standard 139 元 / Pro 312 元
//
// 金额来自官方 billing/overview（套餐费），档位名与月额度来自官方 quota-config。
// 这不是"猜"——两边都是官方数据，金额↔档位是官方公布的对应关系。
func aliyunTierNameByOfficialFee(fee float64) (string, bool) {
	switch {
	case fee >= 312:
		return "pro", true
	case fee >= 139:
		return "standard", true
	case fee >= 79:
		return "essential", true
	case fee >= 39:
		return "lite", true
	default:
		return "", false
	}
}

// aliyunPickPersonalUsage 从官方窗口里挑一个（按窗口名精确匹配）。
func aliyunPickPersonalUsage(usages []aliyunOfficialPersonalUsage, window string) (aliyunOfficialPersonalUsage, bool) {
	for _, usage := range usages {
		if usage.Window == window {
			return usage, true
		}
	}
	return aliyunOfficialPersonalUsage{}, false
}

// aliyunPersistSnapshot 把快照落进 account.Extra。
func (s *CNProviderQuotaService) aliyunPersistSnapshot(
	ctx context.Context,
	account *Account,
	snapshot aliyunQuotaSnapshot,
	result *CNProviderQuotaProbeResult,
) error {
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		aliyunQuotaSnapshotKey: snapshot,
	}); err != nil {
		slog.Warn("aliyun_quota_persist_failed", "account_id", account.ID, "error", err)
		return nil
	}
	result.Persisted = true
	return nil
}

// aliyunFinishProbe 收尾：落快照 + 回填结果字段。
func (s *CNProviderQuotaService) aliyunFinishProbe(
	ctx context.Context,
	account *Account,
	result *CNProviderQuotaProbeResult,
	snapshot aliyunQuotaSnapshot,
) (*CNProviderQuotaProbeResult, error) {
	if err := s.aliyunPersistSnapshot(ctx, account, snapshot, result); err != nil {
		return result, err
	}
	// 同一份快照随探测结果返回（前端点了「查询」当场就能显示数字，
	// 不用等列表刷新拿新 extra）。
	result.AliyunUsage = &snapshot
	result.PlanLevel = snapshot.PlanName
	result.Tiers = []CNQuotaTier{{
		Window:      "monthly",
		UsedPercent: snapshot.UsedPercent,
		ResetAt:     snapshot.ResetAt,
	}}
	result.Success = true
	result.CredentialValid = true
	result.StatusCode = http.StatusOK
	result.Snapshot = &CNProviderSnapshotOutput{
		Window:      "monthly",
		UsedPercent: &snapshot.UsedPercent,
		ResetAt:     snapshot.ResetAt,
		FetchedAt:   snapshot.FetchedAt,
	}
	return result, nil
}
