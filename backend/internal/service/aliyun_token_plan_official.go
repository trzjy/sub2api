package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// 阿里云百炼 Token Plan 用量的**官方数据源**。
//
// 为什么单独一个文件、为什么不复用老 RPC 签名：
// 官方的「订阅与用量」OpenAPI 用的是 **ACS3-HMAC-SHA256（V3）** 签名，与
// `/modelstudio/models`、`/modelstudio/billing/*` 用的老 RPC（HMAC-SHA1）是两套完全
// 不同的东西。混用会得到「签名不匹配」或「参数没生效」的空响应。
//
// 官方接口全景（2026-11-26 实测 + OpenAPI 元数据核对，
// https://api.aliyun.com/meta/v1/products/ModelStudio/versions/2026-02-10/api-docs ：
// 该产品唯一公开版本共 38 个接口，TokenPlan 目录 19 个，**没有任何一个叫「个人版额度」
// 或「已用量」的接口**。官方给程序读额度的只有两条路：
//
//  1. **团队席位版**（组织有席位时）—— 纯 AK/SK，直接读官方剩余额度，零计算：
//     - GET /tokenplan/subscription/stats
//       → Data.Items[].SeatCredits（席位总额度）
//         Data.Items[].SeatRemainingCredits（本周期剩余额度）
//         Data.Items[].SeatRefreshTime（本周期刷新时间）
//         Data.SubscriptionEndTime（订阅到期时间）
//     - GET /tokenplan/subscription/seat-detail
//       → Data.Items[].EquityList[].CycleTotalValue / CycleSurplusValue
//         Data.Items[].EquityList[].CycleStartTime / CycleEndTime
//
//  2. **个人版 solo**（本项目当前账号就是这种，账单商品码 sfm_tokenplansolo_public_cn）
//     —— 官方**没有** AK/SK 可编程读取额度的接口：
//     - OpenAPI 侧：GetSubscriptionStats 返回 {"Success":true} 但**无 Data**；
//       GetSubscriptionSeatDetails / ListSubscriptionSharedPackages 均 Total 0
//       （个人版没有席位）；GetTokenPlanAccountDetail 只有账号与组织信息，无额度字段。
//     - Console 网关侧：官方 CLI（bailian-cli）源码里的个人版三接口
//       zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/{usage,subscription,quota-config}
//       声明 auth=console，需要**浏览器登录态（Cookie + sec_token）**。
//       实测：用 AK/SK 换来的 CLI token（GenerateCLIAccessToken）打这三个接口一律
//       `BailianGateway.Team.NotAuthorised`；用 sk-sp- Key 各种放法一律
//       `BailianGateway.Login.NotLogined`；**官方 CLI 自身**（bl usage token-plan /
//       bl console call --api .../personal/api/v2/usage）用同一对 AK/SK 走同一条路，
//       得到完全相同的错误码 ⇒ 这是官方对 RAM 子账号的既定限制，不是实现问题。
//     - 唯一用 AK/SK 就能读通的是 **quota-config**（静态档位配额表），它给出各档位的
//       官方额度总量（standard.monthly=80000 等），可用来**零计算**确定档位与总额度。
//
// 因此本文件的职责边界：
//   - 团队席位版：AK/SK 直读官方剩余额度与重置时间（首选路径，零计算）。
//   - 个人版：AK/SK 直读官方 quota-config 档位配额表 + 账单金额定档；
//     **已用百分比**只有在账号配了百炼控制台 Cookie 时才能从官方 usage 接口读到。
//   - 任何情况下都不做「用本仓调用日志 × 官方单价表」的自算。

const (
	// aliyunModelStudioVersion 是订阅与用量 OpenAPI 的版本号（唯一公开版本）。
	aliyunModelStudioVersion = "2026-02-10"
	// aliyunTokenPlanSoloProductCode 是个人版 solo 的商品码（账单里也是它）。
	aliyunTokenPlanSoloProductCode = "sfm_tokenplansolo_public_cn"
	// aliyunConsoleQuotaHost 是个人版额度接口所在的主机（百炼 CLI 与 CodexBar 均用它）。
	aliyunConsoleQuotaHost = "bailian-cs.console.aliyun.com"
	// aliyunConsoleDashboardHost 是百炼控制台源站（Origin/Referer 要指它）。
	aliyunConsoleDashboardHost = "bailian.console.aliyun.com"
	// aliyunConsolePersonalUsageAPI 是官方 CLI 源码里的个人版用量接口名。
	aliyunConsolePersonalUsageAPI = "zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/usage"
	// aliyunConsolePersonalQuotaConfigAPI 是官方 CLI 源码里的个人版档位配额接口名。
	aliyunConsolePersonalQuotaConfigAPI = "zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/quota-config"
)

// aliyunACS3Signer 实现阿里云 OpenAPI V3 的 ACS3-HMAC-SHA256 签名。
//
// 规范（help.aliyun.com/zh/resource-management/developer-reference/signature-method）：
//
//	CanonicalRequest = METHOD \n CanonicalURI \n CanonicalQueryString \n
//	                   CanonicalHeaders \n SignedHeaders \n HashedRequestPayload
//	StringToSign    = "ACS3-HMAC-SHA256" \n HexEncode(SHA256(CanonicalRequest))
//	Signature       = HexEncode(HMAC-SHA256(Secret, StringToSign))
//	Authorization   = ACS3-HMAC-SHA256 Credential=<AK>,SignedHeaders=<h1;h2;...>,Signature=<sig>
//
// 必须带的公共头：x-acs-action / x-acs-version / x-acs-date / x-acs-content-sha256 /
// x-acs-signature-nonce。CanonicalHeaders 只收 host、content-type 与 x-acs-* 前缀的头，
// 名字转小写后按字母序排。
type aliyunACS3Signer struct {
	AccessKeyID     string
	AccessKeySecret string
}

// aliyunACS3Sign 计算签名并返回要写到请求上的头。
func (s *aliyunACS3Signer) aliyunACS3Sign(
	method string,
	host string,
	path string,
	query url.Values,
	headers map[string]string,
	body []byte,
	now time.Time,
) (map[string]string, error) {
	if s == nil || s.AccessKeyID == "" || s.AccessKeySecret == "" {
		return nil, fmt.Errorf("aliyun acs3 signer is not configured")
	}
	contentSHA := sha256.Sum256(body)
	hashedPayload := hex.EncodeToString(contentSHA[:])

	hdrs := map[string]string{
		"host":                 host,
		"x-acs-content-sha256": hashedPayload,
		"x-acs-date":           now.UTC().Format("2006-01-02T15:04:05Z"),
		"x-acs-signature-nonce": aliyunACS3Nonce(),
	}
	for k, v := range headers {
		if v == "" {
			continue
		}
		hdrs[strings.ToLower(k)] = v
	}

	// CanonicalHeaders：只留 host / content-type / x-acs-*，小写、字母序、去首尾空格。
	selected := map[string]string{}
	for name, value := range hdrs {
		if name == "host" || name == "content-type" || strings.HasPrefix(name, "x-acs-") {
			selected[name] = strings.TrimSpace(value)
		}
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)

	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(selected[name])
		canonicalHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		strings.ToUpper(method),
		aliyunACS3URIPath(path),
		aliyunACS3CanonicalQuery(query),
		canonicalHeaders.String(),
		signedHeaders,
		hashedPayload,
	}, "\n")

	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "ACS3-HMAC-SHA256\n" + hex.EncodeToString(sum[:])
	mac := hmac.New(sha256.New, []byte(s.AccessKeySecret))
	mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	out := map[string]string{}
	for name, value := range hdrs {
		if name == "host" {
			continue
		}
		out[name] = value
	}
	out["Authorization"] = fmt.Sprintf(
		"ACS3-HMAC-SHA256 Credential=%s,SignedHeaders=%s,Signature=%s",
		s.AccessKeyID, signedHeaders, signature)
	return out, nil
}

// aliyunACS3Nonce 生成签名随机串。
func aliyunACS3Nonce() string {
	// 用时间戳+随机数拼一个够散的唯一串即可（官方只要求每次请求不同）。
	const hexDigits = "0123456789abcdef"
	seed := time.Now().UnixNano()
	var sb strings.Builder
	for i := 0; i < 32; i++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		sb.WriteByte(hexDigits[(seed>>33)&0xF])
	}
	return sb.String()
}

// aliyunACS3URIPath 按 RFC3986 编码路径（斜杠保留）。
func aliyunACS3URIPath(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = aliyunACS3Encode(seg, true)
	}
	return strings.Join(segments, "/")
}

// aliyunACS3CanonicalQuery 生成规范化查询串（参数名字母序，值 RFC3986 编码）。
func aliyunACS3CanonicalQuery(query url.Values) string {
	if len(query) == 0 {
		return ""
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := query[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, aliyunACS3Encode(k, false)+"="+aliyunACS3Encode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

// aliyunACS3Encode 做 RFC3986 编码（-_.~ 不编码，空格 %20）。
func aliyunACS3Encode(value string, keepSlash bool) string {
	const unreserved = "-_.~"
	safe := unreserved
	if keepSlash {
		safe += "/"
	}
	var sb strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			strings.IndexByte(safe, c) >= 0 {
			sb.WriteByte(c)
			continue
		}
		sb.WriteString(fmt.Sprintf("%%%02X", c))
	}
	return sb.String()
}

// aliyunCallOpenAPIv3 调一次用 ACS3 签名的 OpenAPI，返回状态码与响应体。
func (s *CNProviderQuotaService) aliyunCallOpenAPIv3(
	ctx context.Context,
	account *Account,
	signer *aliyunACS3Signer,
	host string,
	action string,
	path string,
	query url.Values,
	method string,
) (int, []byte, error) {
	if s == nil || s.httpUpstream == nil {
		return 0, nil, fmt.Errorf("http upstream is not configured")
	}
	targetURL := "https://" + host + path
	if len(query) > 0 {
		targetURL += "?" + query.Encode()
	}
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return 0, nil, fmt.Errorf("aliyun probe url rejected: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, validatedURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("build aliyun v3 request: %w", err)
	}
	headers, err := signer.aliyunACS3Sign(method, host, path, query, map[string]string{
		"x-acs-action":  action,
		"x-acs-version": aliyunModelStudioVersion,
	}, nil, time.Now())
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := s.resolveProxyURL(ctx, account)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return 0, nil, fmt.Errorf("aliyun v3 request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, aliyunQuotaMaxBodyBytes))
	return resp.StatusCode, body, nil
}

// aliyunOfficialSeatQuota 是官方团队席位版额度（零计算，直接来自官方响应）。
type aliyunOfficialSeatQuota struct {
	// PlanName 是官方席位档位名（standard / pro / max）。
	PlanName string
	// TotalCredits 是官方返回的席位总 Credits 额度。
	TotalCredits float64
	// RemainingCredits 是官方返回的本周期剩余 Credits。
	RemainingCredits float64
	// UsedCredits = TotalCredits - RemainingCredits（官方给的两个数相减，不是自算消耗）。
	UsedCredits float64
	// UsedPercent 是官方两个数算出的占比。
	UsedPercent float64
	// ResetAt 是官方 SeatRefreshTime / CycleEndTime（毫秒时间戳转 RFC3339）。
	ResetAt string
	// SeatCount 是官方返回的席位数。
	SeatCount int
	// AssignedSeatCount 是官方返回的已分配席位数。
	AssignedSeatCount int
}

// aliyunFetchOfficialSeatQuota 用 AK/SK 读官方团队席位额度。
//
// 返回 (nil, nil) 表示「官方没给席位数据」（个人版就是这种），调用方应走个人版分支；
// 返回错误表示请求本身失败（网络、签名错误、权限不足等）。
func (s *CNProviderQuotaService) aliyunFetchOfficialSeatQuota(
	ctx context.Context,
	account *Account,
	signer *aliyunACS3Signer,
	host string,
) (*aliyunOfficialSeatQuota, error) {
	status, body, err := s.aliyunCallOpenAPIv3(ctx, account, signer, host,
		"GetSubscriptionStats", "/tokenplan/subscription/stats", nil, http.MethodGet)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("aliyun GetSubscriptionStats http %d: %s",
			status, truncate(strings.TrimSpace(string(body)), 240))
	}
	// 个人版：官方返回 {"Success":true} 但没有 Data 字段 ⇒ 没有席位数据。
	items := gjson.GetBytes(body, "Data.Items")
	if !items.Exists() || len(items.Array()) == 0 {
		return nil, nil
	}

	quota := &aliyunOfficialSeatQuota{}
	var refreshMs int64
	var endMs int64
	for _, item := range items.Array() {
		seatType := item.Get("SeatType").String()
		if seatType != "" {
			quota.PlanName = seatType
		}
		quota.TotalCredits += item.Get("SeatCredits").Float()
		quota.RemainingCredits += item.Get("SeatRemainingCredits").Float()
		quota.SeatCount += int(item.Get("TotalSeats").Int())
		quota.AssignedSeatCount += int(item.Get("AssignedSeats").Int())
		if v := item.Get("SeatRefreshTime").Int(); v > refreshMs {
			refreshMs = v
		}
	}
	if v := gjson.GetBytes(body, "Data.SubscriptionEndTime").Int(); v > endMs {
		endMs = v
	}
	if quota.TotalCredits <= 0 {
		return nil, nil
	}
	quota.UsedCredits = quota.TotalCredits - quota.RemainingCredits
	if quota.UsedCredits < 0 {
		quota.UsedCredits = 0
	}
	quota.UsedPercent = quota.UsedCredits / quota.TotalCredits * 100
	// 重置时间优先用席位刷新时间；没有就用订阅结束时间。
	resetMs := refreshMs
	if resetMs <= 0 {
		resetMs = endMs
	}
	if resetMs > 0 {
		quota.ResetAt = time.UnixMilli(resetMs).UTC().Format(time.RFC3339)
	}
	return quota, nil
}

// aliyunOfficialTierQuota 是官方档位配额（来自 quota-config，官方给的总配额）。
type aliyunOfficialTierQuota struct {
	// PlanName 是档位名（standard / lite / essential / pro）。
	PlanName string
	// MonthlyCredits 是官方档位表的每月额度。
	MonthlyCredits float64
	// FiveHourCredits 是官方档位表的 5 小时窗口额度。
	FiveHourCredits float64
	// AddonCredits 是官方档位表里附加包额度（无则 0）。
	AddonCredits float64
}

// aliyunOfficialTierQuotaTable 是官方 quota-config 返回的各档位配额表。
type aliyunOfficialTierQuotaTable map[string]aliyunOfficialTierQuota

// aliyunParseOfficialTierQuotaTable 解析官方 quota-config 响应。
//
// 官方响应形态（2026-11-26 实测）：
//
//	{"standard":{"five_hour":3000.0,"monthly":80000.0},"lite":{"five_hour":700.0,
//	 "monthly":20000.0},"essential":{...},"pro":{...},"addon_quota":{"extrabundle":20000.0}}
func aliyunParseOfficialTierQuotaTable(body []byte) aliyunOfficialTierQuotaTable {
	table := aliyunOfficialTierQuotaTable{}
	root := gjson.ParseBytes(body)
	// 响应可能裹在 data.DataV2.data.data 里，也可能直接就是档位表。
	if v := root.Get("data.DataV2.data.data"); v.Exists() {
		root = v
	} else if v := root.Get("data.data"); v.Exists() {
		root = v
	}
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if !value.IsObject() {
			return true
		}
		quota := aliyunOfficialTierQuota{
			PlanName:       name,
			FiveHourCredits: value.Get("five_hour").Float(),
			MonthlyCredits:  value.Get("monthly").Float(),
		}
		if name == "addon_quota" {
			quota.AddonCredits = value.Get("extrabundle").Float()
		}
		table[name] = quota
		return true
	})
	return table
}

// aliyunConsoleTierQuota 用 AK/SK 换 CLI token，再读官方 quota-config 档位配额表。
//
// 这是个人版唯一能用 AK/SK 读到的官方额度数据（静态表，不含已用量）。
// 步骤（来自官方 CLI 源码 bailian-cli-core）：
//  1. POST /modelstudio/cli/generateAccessToken（ACS3 签名）→ cliAccessToken
//  2. POST https://bailian-cs.console.aliyun.com/cli/api.json（Bearer cliAccessToken）
//     → api = zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/quota-config
func (s *CNProviderQuotaService) aliyunConsoleTierQuota(
	ctx context.Context,
	account *Account,
	signer *aliyunACS3Signer,
	host string,
) (aliyunOfficialTierQuotaTable, error) {
	token, err := s.aliyunGenerateCLIAccessToken(ctx, account, signer, host)
	if err != nil {
		return nil, err
	}
	body, err := s.aliyunConsoleCall(ctx, account, aliyunConsolePersonalQuotaConfigAPI, token, nil)
	if err != nil {
		return nil, err
	}
	table := aliyunParseOfficialTierQuotaTable(body)
	if len(table) == 0 {
		return nil, fmt.Errorf("aliyun quota-config returned no tier table")
	}
	return table, nil
}

// aliyunGenerateCLIAccessToken 用 AK/SK 换控制台 CLI token（官方 OpenAPI）。
func (s *CNProviderQuotaService) aliyunGenerateCLIAccessToken(
	ctx context.Context,
	account *Account,
	signer *aliyunACS3Signer,
	host string,
) (string, error) {
	status, body, err := s.aliyunCallOpenAPIv3(ctx, account, signer, host,
		"GenerateCLIAccessToken", "/modelstudio/cli/generateAccessToken", nil, http.MethodPost)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("aliyun GenerateCLIAccessToken http %d: %s",
			status, truncate(strings.TrimSpace(string(body)), 240))
	}
	token := strings.TrimSpace(gjson.GetBytes(body, "cliAccessToken").String())
	if token == "" {
		return "", fmt.Errorf("aliyun GenerateCLIAccessToken returned no cliAccessToken")
	}
	return token, nil
}

// aliyunConsoleCall 打百炼控制台的 cli 网关（Bearer 令牌鉴权）。
//
// 形态与官方 CLI 完全一致：
//   POST https://bailian-cs.console.aliyun.com/cli/api.json
//        ?action=BroadScopeAspnGateway&product=sfm_bailian&api=<api名>
//   Body: params={"Api":<api>,"V":"1.0","Data":{...,"cornerstoneParam":{...}}}&region=cn-beijing
//   Header: Authorization: Bearer <console token>
func (s *CNProviderQuotaService) aliyunConsoleCall(
	ctx context.Context,
	account *Account,
	api string,
	token string,
	data map[string]any,
) ([]byte, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, fmt.Errorf("http upstream is not configured")
	}
	cornerstone := map[string]any{
		"protocol":           "V2",
		"console":            "ONE_CONSOLE",
		"productCode":        "p_efm",
		"switchUserType":     3,
		"consoleSite":        "BAILIAN_ALIYUN",
		"userNickName":       "",
		"userPrincipalName": "",
		"xsp_lang":           "en-US",
	}
	payload := map[string]any{}
	for k, v := range data {
		payload[k] = v
	}
	payload["cornerstoneParam"] = cornerstone
	params, err := json.Marshal(map[string]any{"Api": api, "V": "1.0", "Data": payload})
	if err != nil {
		return nil, fmt.Errorf("marshal aliyun console params: %w", err)
	}
	form := url.Values{
		"params": {string(params)},
		"region": {aliyunTokenPlanRegionForHost(aliyunConsoleQuotaHost)},
	}
	targetURL := fmt.Sprintf("https://%s/cli/api.json?action=BroadScopeAspnGateway&product=sfm_bailian&api=%s",
		aliyunConsoleQuotaHost, url.QueryEscape(api))
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, fmt.Errorf("aliyun console url rejected: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, validatedURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build aliyun console request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := s.resolveProxyURL(ctx, account)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, fmt.Errorf("aliyun console request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, aliyunQuotaMaxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("aliyun console http %d: %s",
			resp.StatusCode, truncate(strings.TrimSpace(string(body)), 240))
	}
	return body, nil
}

// aliyunConsoleErrorCode 从控制台响应里取错误码（没有则空串）。
//
// 官方错误码（实测）：BailianGateway.Login.NotLogined（没登录态/令牌不对）、
// BailianGateway.Team.NotAuthorised（该账号在团队维度没有授权，个人版走 AK/SK 必现）。
func aliyunConsoleErrorCode(body []byte) string {
	code := strings.TrimSpace(gjson.GetBytes(body, "data.errorCode").String())
	if code != "" {
		return code
	}
	return strings.TrimSpace(gjson.GetBytes(body, "data.errorMsg").String())
}

// aliyunOfficialPersonalUsage 是官方个人版用量窗口（官方直接给百分比与重置时间）。
type aliyunOfficialPersonalUsage struct {
	// Window 是窗口名（per5Hour / perWeek / per1Month 等）。
	Window string
	// UsedPercent 是官方给的已用百分比（0-1 之间的小数）。
	UsedPercent float64
	// ResetAt 是官方给的重置时间（RFC3339）。
	ResetAt string
}

// aliyunParseOfficialPersonalUsage 解析官方个人版 usage 响应。
//
// 官方返回（来自百炼 CLI 源码 AlibabaTokenPlanPersonalUsageParser 的字段约定）：
//
//	{"per5HourPercentage":0.12,"per5HourResetTime":1788328800000,
//	 "per1MonthPercentage":0.34,"per1MonthResetTime":1790920800000}
//
// 官方只给**百分比**与**重置时间**，不给绝对已用量；绝对值需要配合官方 quota-config
// 的档位总量换算（乘一个官方给的数，不是自算 token 价）。
func aliyunParseOfficialPersonalUsage(body []byte) []aliyunOfficialPersonalUsage {
	root := gjson.ParseBytes(body)
	// 响应可能裹在 data.DataV2.data.data 里。
	if v := root.Get("data.DataV2.data.data"); v.Exists() {
		root = v
	} else if v := root.Get("data.data"); v.Exists() {
		root = v
	}
	// 官方把每个窗口拆成两个平级字段：per{N}{Unit}Percentage 与 per{N}{Unit}ResetTime。
	// 两遍扫描**分别**收各自的字段，不靠"值是否为 0"去猜字段类型
	// （百分比合法值就是 0，用值当判据会把 ResetTime 记录误当成 Percentage 记录）。
	percentages := map[string]float64{}
	resetTimes := map[string]int64{}
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		switch {
		case strings.HasSuffix(name, "Percentage"):
			pct := value.Float()
			if pct < 0 || pct > 1 {
				// 官方 CLI 的 ratio() 约定：百分比是 0-1 的小数；超出范围视为异常值丢弃。
				return true
			}
			percentages[strings.TrimSuffix(name, "Percentage")] = pct
		case strings.HasSuffix(name, "ResetTime"):
			ms := value.Int()
			if ms <= 0 {
				return true
			}
			resetTimes[strings.TrimSuffix(name, "ResetTime")] = ms
		}
		return true
	})
	// 窗口全集 = 两个字段族出现过的键的并集（官方可能只给百分比不给重置时间）。
	windows := make(map[string]struct{}, len(percentages)+len(resetTimes))
	for k := range percentages {
		windows[k] = struct{}{}
	}
	for k := range resetTimes {
		windows[k] = struct{}{}
	}
	out := make([]aliyunOfficialPersonalUsage, 0, len(windows))
	for window := range windows {
		usage := aliyunOfficialPersonalUsage{Window: window}
		if pct, ok := percentages[window]; ok {
			usage.UsedPercent = pct
		}
		if ms, ok := resetTimes[window]; ok {
			usage.ResetAt = time.UnixMilli(ms).UTC().Format(time.RFC3339)
		}
		out = append(out, usage)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Window < out[j].Window })
	return out
}

// aliyunFetchOfficialPersonalUsage 用控制台 Cookie 读官方个人版用量窗口。
//
// 个人版的官方 usage 接口**只认浏览器登录态**（Cookie + sec_token），AK/SK 走不通
// （实测一律 BailianGateway.Team.NotAuthorised）。这里走 CodexBar 验证过的形态：
// POST https://bailian-cs.console.aliyun.com/data/api.json
//
//	?action=BroadScopeAspnGateway&product=sfm_bailian&api=<api名>&_v=undefined
//
// Body: product/action/region/language/params（params 里带 cornerstoneParam）
// Header: Cookie（控制台登录态）+ X-Requested-With: XMLHttpRequest + 浏览器 UA
//
//	         + Origin/Referer 指向百炼控制台 + sec_token（可缺，缺了部分账号会被拒）
func (s *CNProviderQuotaService) aliyunFetchOfficialPersonalUsage(
	ctx context.Context,
	account *Account,
	cookie string,
	secToken string,
) ([]aliyunOfficialPersonalUsage, error) {
	if strings.TrimSpace(cookie) == "" {
		return nil, fmt.Errorf("aliyun console cookie is not configured")
	}
	dashboardURL := fmt.Sprintf("https://%s/cn-beijing?tab=plan#/efm/subscription/token-plan/personal",
		aliyunConsoleDashboardHost)
	cornerstone := map[string]any{
		"protocol":           "V2",
		"console":            "ONE_CONSOLE",
		"productCode":        "p_efm",
		"switchUserType":     3,
		"consoleSite":        "BAILIAN_ALIYUN",
		"userNickName":       "",
		"userPrincipalName": "",
		"xsp_lang":           "en-US",
		"domain":             aliyunConsoleDashboardHost,
		"feURL":              dashboardURL,
	}
	params, err := json.Marshal(map[string]any{
		"Api": aliyunConsolePersonalUsageAPI,
		"V":   "1.0",
		"Data": map[string]any{
			"cornerstoneParam": cornerstone,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal aliyun personal usage params: %w", err)
	}
	form := url.Values{
		"product":  {"sfm_bailian"},
		"action":   {"BroadScopeAspnGateway"},
		"region":   {aliyunTokenPlanRegionForHost(aliyunConsoleQuotaHost)},
		"language": {"en-US"},
		"params":   {string(params)},
	}
	if secToken != "" {
		form.Set("sec_token", secToken)
	}
	targetURL := fmt.Sprintf(
		"https://%s/data/api.json?action=BroadScopeAspnGateway&product=sfm_bailian&api=%s&_v=undefined",
		aliyunConsoleQuotaHost, url.QueryEscape(aliyunConsolePersonalUsageAPI))
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, fmt.Errorf("aliyun personal usage url rejected: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, validatedURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build aliyun personal usage request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", aliyunConsoleBrowserUA)
	req.Header.Set("Origin", "https://"+aliyunConsoleDashboardHost)
	req.Header.Set("Referer", dashboardURL)
	req.Header.Set("Cookie", cookie)
	if secToken != "" {
		req.Header.Set("sec_token", secToken)
	}
	if csrf := aliyunExtractCookieValue(cookie, "login_aliyunid_csrf"); csrf != "" {
		req.Header.Set("x-xsrf-token", csrf)
		req.Header.Set("x-csrf-token", csrf)
	} else if csrf := aliyunExtractCookieValue(cookie, "csrf"); csrf != "" {
		req.Header.Set("x-xsrf-token", csrf)
		req.Header.Set("x-csrf-token", csrf)
	}
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := s.resolveProxyURL(ctx, account)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, fmt.Errorf("aliyun personal usage request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, aliyunQuotaMaxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aliyun personal usage http %d: %s",
			resp.StatusCode, truncate(strings.TrimSpace(string(body)), 240))
	}
	if code := aliyunConsoleErrorCode(body); code != "" {
		return nil, fmt.Errorf("aliyun personal usage rejected: %s", code)
	}
	usages := aliyunParseOfficialPersonalUsage(body)
	if len(usages) == 0 {
		return nil, fmt.Errorf("aliyun personal usage returned no windows")
	}
	return usages, nil
}

// aliyunConsoleBrowserUA 是官方 CLI / CodexBar 用的浏览器 UA。
const aliyunConsoleBrowserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"

// aliyunExtractCookieValue 从 Cookie 头里取某个 cookie 的值。
func aliyunExtractCookieValue(cookieHeader string, name string) string {
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(part[:eq]), name) {
			return strings.TrimSpace(part[eq+1:])
		}
	}
	return ""
}
