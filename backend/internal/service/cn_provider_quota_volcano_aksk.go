package service

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// 火山方舟订阅用量的 AK/SK 管理面探测（主路径）。
//
// 生产实测（2026-09-06，真实密钥 + 官方文档核对）：Agent/Coding Plan 推理端点的成功
// 响应**不携带任何 x-ratelimit-* 响应头**（仅有 istio 网关头），官方文档亦明确订阅用量
// 只能控制台查看（延迟 0.5~1 天），没有推理响应头可用。GetAFPUsage /
// GetCodingPlanUsage（open.volcengineapi.com，AK/SK SignatureV4 签名）是唯一返回真实
// Used/Quota 与官方重置时间的接口。因此：凭据里有 access_key/secret_key（或同 base_url
// 的同主体账号已配置过）时走本文件的管理面探测（主路径）；都没有时才回落到订阅 API Key
// 的推理响应头探测（doVolcanoProbe，仅能拿到确定性周/月重置时间，用量不可得）。

const (
	volcanoQuotaHost    = "open.volcengineapi.com"
	volcanoQuotaService = "ark"
	volcanoQuotaRegion  = "cn-beijing"
	volcanoQuotaVersion = "2024-01-01"
)

// volcanoAKSKProbe 用凭据中的 AK/SK 调方舟管理面用量接口。
// handled=false 表示账号（及其同 base_url 同主体账号）未配置 AK/SK，或调用失败
// （失败已记日志），调用方应回落到推理响应头探测，而不是把失败透传给用户。
func (s *CNProviderQuotaService) volcanoAKSKProbe(ctx context.Context, account *Account) (*CNProviderQuotaProbeResult, bool, error) {
	ak := strings.TrimSpace(account.GetCredential("access_key"))
	sk := strings.TrimSpace(account.GetCredential("secret_key"))
	if ak == "" || sk == "" {
		// 同主体继承：同一 base_url（同一火山订阅主体）的其他账号已录入过 AK/SK 时直接复用，
		// 无需每个账号重复录入。
		ak, sk = s.inheritVolcanoSiblingAKSK(ctx, account)
		if ak == "" || sk == "" {
			return nil, false, nil
		}
	}
	baseURL := account.GetBaseURL()
	action := volcanoUsageAction(baseURL)
	if action == "" {
		return nil, false, nil
	}

	query := url.Values{}
	query.Set("Action", action)
	query.Set("Version", volcanoQuotaVersion)
	canonQuery, signedHeaders, err := volcEngineSignQuery(ak, sk, volcanoQuotaRegion, volcanoQuotaService, volcanoQuotaHost, query, time.Now().UTC())
	if err != nil {
		slog.Warn("volcano_aksk_sign_failed", "account_id", account.ID, "error", err)
		return nil, false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+volcanoQuotaHost+"/?"+canonQuery, nil)
	if err != nil {
		slog.Warn("volcano_aksk_request_build_failed", "account_id", account.ID, "error", err)
		return nil, false, nil
	}
	for name, values := range signedHeaders {
		for _, v := range values {
			req.Header.Set(name, v)
		}
	}

	now := time.Now().UTC()
	proxyURL := s.resolveProxyURL(ctx, account)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		slog.Warn("volcano_aksk_request_failed", "account_id", account.ID, "error", err)
		return nil, false, nil
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, cnQuotaMaxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		slog.Warn("volcano_aksk_http_error", "account_id", account.ID, "status", resp.StatusCode,
			"body", truncate(strings.TrimSpace(string(bodyBytes)), 240))
		return nil, false, nil
	}

	var tiers []CNQuotaTier
	if action == "GetAFPUsage" {
		tiers = parseVolcanoAFPUsageTiers(bodyBytes)
	} else {
		tiers = parseVolcanoCodingUsageTiers(bodyBytes)
	}
	if len(tiers) == 0 {
		slog.Warn("volcano_aksk_empty_tiers", "account_id", account.ID, "action", action)
		return nil, false, nil
	}
	result := &CNProviderQuotaProbeResult{
		Provider:        providerVolcano,
		Source:          "coding_plan",
		FetchedAt:       now.Unix(),
		StatusCode:      resp.StatusCode,
		Success:         true,
		CredentialValid: true,
		Tiers:           tiers,
	}
	if perr := s.accountRepo.UpdateExtra(ctx, account.ID, cnQuotaExtraUpdates(providerVolcano, tiers, now)); perr != nil {
		slog.Warn("cn_quota_persist_failed", "account_id", account.ID, "provider", providerVolcano, "error", perr)
	} else {
		result.Persisted = true
	}
	return result, true, nil
}

// inheritVolcanoSiblingAKSK 同主体继承：在本账号未录入 AK/SK 时，查找同 base_url
// （同一火山订阅主体）的其他账号已录入的访问密钥。按 deepseek/kimi 两个挂靠平台各查一遍。
// base_url 比对前做归一化：OpenAI 协议账号常带 /v3 协议后缀（如 /api/plan/v3），
// 与订阅主体 base（/api/plan）是同一主体，须视为相同。
func (s *CNProviderQuotaService) inheritVolcanoSiblingAKSK(ctx context.Context, account *Account) (string, string) {
	baseURL := normalizeVolcanoPlanBaseURL(account.GetBaseURL())
	for _, platform := range []string{PlatformDeepseek, PlatformKimi} {
		siblings, err := s.accountRepo.ListByPlatform(ctx, platform)
		if err != nil {
			continue
		}
		for i := range siblings {
			cand := siblings[i]
			if cand.ID == account.ID {
				continue
			}
			if normalizeVolcanoPlanBaseURL(cand.GetBaseURL()) != baseURL {
				continue
			}
			ak := strings.TrimSpace(cand.GetCredential("access_key"))
			sk := strings.TrimSpace(cand.GetCredential("secret_key"))
			if ak != "" && sk != "" {
				slog.Info("volcano_aksk_inherited_from_sibling", "account_id", account.ID, "sibling_id", cand.ID)
				return ak, sk
			}
		}
	}
	return "", ""
}

// normalizeVolcanoPlanBaseURL 归一化火山订阅 base_url：去尾部斜杠与 OpenAI/Anthropic
// 协议路径后缀（/v3、/v1），使 /api/plan/v3 与 /api/plan 视为同一订阅主体。
func normalizeVolcanoPlanBaseURL(raw string) string {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	u = strings.TrimSuffix(u, "/v3")
	u = strings.TrimSuffix(u, "/v1")
	return strings.TrimRight(u, "/")
}

// volcanoUsageAction 根据账号 base_url 路径选择方舟用量管理 Action。
//   - /api/plan  → Agent Plan 订阅：GetAFPUsage（Result.AFPFiveHour/AFPWeekly/AFPMonthly）
//   - /api/coding → Coding Plan 订阅：GetCodingPlanUsage（Result.QuotaUsage[]，session=5h）
//   - 其他路径返回空串（不支持管理面用量查询）。
func volcanoUsageAction(baseURL string) string {
	switch {
	case strings.Contains(baseURL, "/api/plan"):
		return "GetAFPUsage"
	case strings.Contains(baseURL, "/api/coding"):
		return "GetCodingPlanUsage"
	default:
		return ""
	}
}

// parseVolcanoAFPUsageTiers 解析 Agent Plan 订阅（GetAFPUsage）响应：
// Result.AFPFiveHour / AFPWeekly / AFPMonthly：{Quota, Used, ResetTime(ms)}。
// 用量百分比 = Used/Quota*100；ResetTime 为毫秒时间戳，转 RFC3339 UTC 供前端倒计时。
// Daily 档官方仅覆盖部分视觉/Harness 模型，与网关三窗口模型不一致，不落快照。
func parseVolcanoAFPUsageTiers(body []byte) []CNQuotaTier {
	windows := []struct {
		window  string
		usedKey string
	}{
		{"5h", "AFPFiveHour"},
		{"weekly", "AFPWeekly"},
		{"monthly", "AFPMonthly"},
	}
	var tiers []CNQuotaTier
	for _, w := range windows {
		base := "Result." + w.usedKey
		quota := gjson.GetBytes(body, base+".Quota").Float()
		used := gjson.GetBytes(body, base+".Used").Float()
		resetMs := gjson.GetBytes(body, base+".ResetTime").Int()
		if quota <= 0 || resetMs <= 0 {
			continue
		}
		percent := used / quota * 100
		if percent < 0 {
			percent = 0
		}
		tiers = append(tiers, CNQuotaTier{
			Window:      w.window,
			UsedPercent: percent,
			ResetAt:     time.UnixMilli(resetMs).UTC().Format(time.RFC3339),
		})
	}
	return tiers
}

// parseVolcanoCodingUsageTiers 解析 Coding Plan 订阅（GetCodingPlanUsage）响应：
// Result.QuotaUsage[]：{Level:"session"|"weekly"|"monthly", Percent, ResetTimestamp(sec)}。
// Level "session" 即 5h 滚动窗口（锚定首次请求）；Percent 已是百分比（Cap=100）。
func parseVolcanoCodingUsageTiers(body []byte) []CNQuotaTier {
	levelToWindow := map[string]string{
		"session": "5h",
		"weekly":  "weekly",
		"monthly": "monthly",
	}
	var tiers []CNQuotaTier
	gjson.GetBytes(body, "Result.QuotaUsage").ForEach(func(_, item gjson.Result) bool {
		window := levelToWindow[item.Get("Level").String()]
		resetSec := item.Get("ResetTimestamp").Int()
		if window == "" || resetSec <= 0 {
			return true
		}
		percent := item.Get("Percent").Float()
		if percent < 0 {
			percent = 0
		}
		tiers = append(tiers, CNQuotaTier{
			Window:      window,
			UsedPercent: percent,
			ResetAt:     time.Unix(resetSec, 0).UTC().Format(time.RFC3339),
		})
		return true
	})
	return tiers
}
