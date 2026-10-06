package service

// Kira（kiraai.vn）上游账号的用量窗口 / 余额探测辅助。
//
// 背景：站点上有 platform 分别为 kimi/deepseek/zhipu 的账号，但 base_url 都指向
// https://kiraai.vn/api/v1——platform 不反映真实上游，按 platform 分发会把余额
// 探测错打到 moonshot/deepseek，用量窗口则完全无人认领。判定统一按 base_url
// 大小写不敏感包含 "kiraai.vn"（isKiraBaseURL），先于现有 platform/provider 解析。
//
// 上游契约（2026-10-05 实测抓包，/tmp/kira_usage_live.json）：
//   - GET  {scheme}://{host}/api/user/usage，头 Authorization: Bearer <dashboard JWT>。
//     注意 base_url 是 …/api/v1，usage 端点在主机根 /api/user/usage。
//   - POST {scheme}://{host}/api/auth/login，body {"usernameOrEmail","password"}
//     → {"token":"<JWT>",...}，无验证码；JWT 约 7 天过期。
//   - kira_ 开头的 API key 打不开这两个端点（403），必须 dashboard JWT。
//
// 凭据（credentials JSONB）：kira_jwt（缓存）/ kira_email / kira_password。
// JWT 回写走 persistAccountCredentials（与 api_key 同一条加密写路径），日志严禁
// 出现 JWT/密码/完整凭据。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
)

const (
	// providerKira 标识 kiraai.vn 上游账号，作为 CN 探测链路内部供应商 token
	// （与 providerVolcano 同构：不是 accounts.platform 平台枚举）。
	providerKira = "kira"

	kiraCredentialJWT      = "kira_jwt"
	kiraCredentialEmail    = "kira_email"
	kiraCredentialPassword = "kira_password"

	// kiraUsageSnapshotExtraKey 用量快照 extra 键（单键 JSON，照
	// balance_probe_snapshot 模式），供管理端展示消费。
	kiraUsageSnapshotExtraKey = "kira_usage_snapshot"

	// kiraUsageWindowDaily kira_usage_snapshot.window 的唯一取值（§4.3 契约：Kira
	// 免费池每日重置，单窗口）。
	kiraUsageWindowDaily = "daily"

	kiraMaxBodyBytes = 256 * 1024
)

// isKiraBaseURL 报告 base_url 是否指向 kiraai.vn 上游（外审 F2：精确主机名
// 匹配——url.Parse 后 hostname 大小写不敏感精确等于 kiraai.vn，废除子串包含，
// 防 kiraai.vn.evil.example.com 类钓鱼主机误判）。这 3 行账号的 platform 是
// kimi/deepseek/zhipu，base_url 是唯一事实源。
func isKiraBaseURL(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Hostname(), "kiraai.vn")
}

// accountIsKiraBaseURL 按账号凭据判定是否 Kira 上游：OpenAI 协议 base_url 优先，
// 凭据原始 base_url 补充（与火山"凭据 base_url 是唯一事实源"的教训同构）。
func accountIsKiraBaseURL(account *Account) bool {
	if account == nil {
		return false
	}
	return isKiraBaseURL(account.GetOpenAIBaseURL()) || isKiraBaseURL(account.GetBaseURL())
}

// kiraHostRoot 从 base_url（…/api/v1）取 scheme://host——usage/login 端点都在
// 主机根，不跟随 base_url 路径。
func kiraHostRoot(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return ""
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + parsed.Host
}

// kiraProbeURLs 构造并校验 Kira 探测端点（usage + login 两条出站 URL 都过
// 同一套出站安全策略，不得把 dashboard JWT/密码发往策略外主机）。
func kiraProbeURLs(cfg *config.Config, account *Account) (usageURL, loginURL string, err error) {
	base := account.GetOpenAIBaseURL()
	if !isKiraBaseURL(base) {
		base = account.GetBaseURL()
	}
	root := kiraHostRoot(base)
	if root == "" {
		return "", "", errors.New("kira account base_url is empty or invalid")
	}
	usageURL, err = cnValidateProbeURL(cfg, root+"/api/user/usage")
	if err != nil {
		return "", "", err
	}
	loginURL, err = cnValidateProbeURL(cfg, root+"/api/auth/login")
	if err != nil {
		return "", "", err
	}
	return usageURL, loginURL, nil
}

// kiraProbeClient 复用 CN 探测链路既有出客户端（httpUpstream + 账号代理）。
type kiraProbeClient struct {
	upstream    HTTPUpstream
	proxyURL    string
	accountID   int64
	concurrency int
}

// getUsage 带 dashboard JWT 请求用量端点，返回响应体与状态码（非 2xx 不是
// 传输层错误，由调用方按状态分支）。传输失败才返回 error。
func (c *kiraProbeClient) getUsage(ctx context.Context, usageURL, jwt string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, 0, infraerrors.Newf(http.StatusInternalServerError, "KIRA_REQUEST_BUILD_FAILED", "build kira usage request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/json")
	resp, err := c.upstream.Do(req, c.proxyURL, c.accountID, c.concurrency)
	if err != nil {
		return nil, 0, infraerrors.Newf(http.StatusBadGateway, "KIRA_REQUEST_FAILED", "kira upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, kiraMaxBodyBytes))
	return body, resp.StatusCode, nil
}

// login 用 kira_email/kira_password 换新 dashboard JWT。失败返回明确错误；
// 错误信息只带 HTTP 状态，不带凭据。
func (c *kiraProbeClient) login(ctx context.Context, loginURL, email, password string) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"usernameOrEmail": email,
		"password":        password,
	})
	if err != nil {
		return "", infraerrors.Newf(http.StatusInternalServerError, "KIRA_LOGIN_REQUEST_BUILD_FAILED", "build kira login payload: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(string(payload)))
	if err != nil {
		return "", infraerrors.Newf(http.StatusInternalServerError, "KIRA_LOGIN_REQUEST_BUILD_FAILED", "build kira login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.upstream.Do(req, c.proxyURL, c.accountID, c.concurrency)
	if err != nil {
		return "", infraerrors.Newf(http.StatusBadGateway, "KIRA_REQUEST_FAILED", "kira login request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, kiraMaxBodyBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", infraerrors.Newf(http.StatusUnauthorized, "KIRA_LOGIN_FAILED", "kira 登录失败: HTTP %d", resp.StatusCode)
	}
	token := strings.TrimSpace(gjson.GetBytes(body, "token").String())
	if token == "" {
		return "", infraerrors.New(http.StatusUnauthorized, "KIRA_LOGIN_FAILED", "kira 登录失败: 响应缺少 token")
	}
	return token, nil
}

// fetchKiraUsageWithReauth 带 JWT 缓存的 Kira 用量获取：
// 有 kira_jwt 先直接用；401/403 或无 JWT → 用 kira_email/kira_password 登录换新
// JWT → 加密回写凭据 → 重试一次。仍 401/403 则把状态码交回调用方（不兜底、
// 不假数据）。返回 (body, statusCode, error)；error 仅传输/登录/凭据缺失等
// 硬失败，非 2xx 状态码不是 error。
func fetchKiraUsageWithReauth(
	ctx context.Context,
	client *kiraProbeClient,
	account *Account,
	repo AccountRepository,
	usageURL, loginURL string,
) ([]byte, int, error) {
	jwt := strings.TrimSpace(account.GetCredential(kiraCredentialJWT))
	if jwt != "" {
		body, status, err := client.getUsage(ctx, usageURL, jwt)
		if err != nil {
			return nil, 0, err
		}
		if status >= 200 && status < 300 {
			return body, status, nil
		}
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			return body, status, nil
		}
	}
	// 无 JWT 或 401/403：重新登录换 JWT。
	email := strings.TrimSpace(account.GetCredential(kiraCredentialEmail))
	password := account.GetCredential(kiraCredentialPassword)
	if email == "" || password == "" {
		return nil, 0, infraerrors.New(http.StatusUnauthorized, "KIRA_CREDENTIALS_MISSING",
			"kira 探测缺少凭据: credentials 需配置 kira_email/kira_password（或有效的 kira_jwt）")
	}
	newJWT, err := client.login(ctx, loginURL, email, password)
	if err != nil {
		return nil, 0, err
	}
	// JWT 回写走与 api_key 相同的加密通道（persistAccountCredentials 是凭据写入
	// 唯一汇聚点，repo 层负责敏感子键加密 + credentials_mac 维护）。回写失败仅
	// 告警，本次探测仍用内存中的新 JWT 重试。
	creds := shallowCopyMap(account.Credentials)
	creds[kiraCredentialJWT] = newJWT
	if perr := persistAccountCredentials(ctx, repo, account, creds); perr != nil {
		slog.Warn("kira_jwt_persist_failed", "account_id", account.ID, "error", perr)
	}
	return client.getUsage(ctx, usageURL, newJWT)
}

// parseKiraUsageTier 解析 /api/user/usage 的 summary 为一条"每日"窗口：
// used = tokensUsedToday；limit = freeDailyLimit>0 ? freeDailyLimit : baseFreeLimit+checkinBonus。
// 返回 (tier, used, limit, ok)；外审 F3：tokensUsedToday / 限额字段缺失、null、
// 解析失败或为负数一律 ok=false（失败关闭，调用方不得落任何快照/清标记）。
// reset_at 上游无响应字段，由"当日窗口"语义推导（越南时区每日零点重置，
// kiraNextDailyReset）；tier.ResetAt 为空表示由调用方按推导规则补齐。
func parseKiraUsageTier(body []byte) (CNQuotaTier, float64, float64, bool) {
	summary := gjson.GetBytes(body, "summary")
	if !summary.Exists() {
		return CNQuotaTier{}, 0, 0, false
	}
	usedRaw := summary.Get("tokensUsedToday")
	used, hasUsed := cnParseF64(usedRaw.Value())
	// null/缺失/非数值：gjson 对 null 的 Value() 是 nil → cnParseF64 返回 false。
	if !hasUsed || used < 0 {
		return CNQuotaTier{}, 0, 0, false
	}
	dailyLimit, hasDailyLimit := cnParseF64(summary.Get("freeDailyLimit").Value())
	if !hasDailyLimit || dailyLimit <= 0 {
		baseLimit, hasBase := cnParseF64(summary.Get("baseFreeLimit").Value())
		bonus, hasBonus := cnParseF64(summary.Get("checkinBonus").Value())
		if !hasBase || !hasBonus || baseLimit < 0 || bonus < 0 {
			// 限额字段缺失/null/解析失败/负数：无官方分母，失败关闭（F3）。
			return CNQuotaTier{}, 0, 0, false
		}
		dailyLimit = baseLimit + bonus
	}
	if dailyLimit <= 0 {
		return CNQuotaTier{}, 0, 0, false
	}
	var pct float64
	if dailyLimit > 0 {
		pct = used / dailyLimit * 100
	}
	return CNQuotaTier{Window: kiraUsageWindowDaily, UsedPercent: pct}, used, dailyLimit, true
}

// QueryKiraUsageForAccount 是 CNProviderQuotaService 的 Kira 分支（在 platform/
// provider 解析之前按 base_url 分发进来）。探测 dashboard 用量并落
// kira_usage_snapshot 快照。导出（本单接线点清单）：供状态机确认探针/
// QuotaSnapshotRefresher 复用（只导出，不改语义）。
func (s *CNProviderQuotaService) QueryKiraUsageForAccount(ctx context.Context, account *Account) (*CNProviderQuotaProbeResult, error) {
	usageURL, loginURL, err := kiraProbeURLs(s.cfg, account)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "CN_QUOTA_URL_REJECTED", err.Error())
	}
	proxyURL := s.resolveProxyURL(ctx, account)
	client := &kiraProbeClient{
		upstream:    s.httpUpstream,
		proxyURL:    proxyURL,
		accountID:   account.ID,
		concurrency: maxInt(account.Concurrency, 1),
	}
	// 最坏路径 = GET + 登录 + 重试 GET，预算放宽到 2× 单请求超时（外层
	// singleflight probeCtx 仍有总闸）。
	callCtx, cancel := context.WithTimeout(ctx, cnQuotaUpstreamTimeout*2)
	defer cancel()
	body, status, err := fetchKiraUsageWithReauth(callCtx, client, account, s.accountRepo, usageURL, loginURL)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := &CNProviderQuotaProbeResult{
		Provider:   providerKira,
		Source:     "kira_dashboard",
		FetchedAt:  now.Unix(),
		StatusCode: status,
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// 重登后仍被拒：不落快照（不覆盖之前的有效值），CredentialValid=false 供前端提示。
		result.Error = fmt.Sprintf("kira 鉴权失败 (HTTP %d)：已尝试重新登录仍被拒，请检查 kira_email/kira_password", status)
		return result, nil
	}
	if status < 200 || status >= 300 {
		result.Error = fmt.Sprintf("API error (HTTP %d): %s", status, truncate(strings.TrimSpace(string(body)), 240))
		return result, nil
	}
	tier, usedTokens, limitTokens, ok := parseKiraUsageTier(body)
	if !ok {
		// 外审 F3：字段缺失/null/解析失败/负数 → 失败关闭，明确错误，
		// 不落任何快照、不清 balance_low 标记。
		result.Error = "Invalid kira usage response: summary tokensUsedToday/limit fields missing, invalid or negative"
		return result, nil
	}
	// reset_at 推导规则：上游无重置字段，站点免费池按越南时区每日零点重置，
	// 由"当日窗口"语义推导下一次重置时刻（kiraNextDailyReset），RFC3339 输出。
	resetAt := kiraNextDailyReset(now)
	tier.ResetAt = resetAt.UTC().Format(time.RFC3339)
	result.Tiers = []CNQuotaTier{tier}
	result.Success = true
	result.CredentialValid = true

	// 快照照 balance_probe_snapshot 单键 JSON 模式落 extra，供管理端/前端消费
	//（§4.3 契约键名照抄：window/used_percent/used_tokens/limit_tokens/reset_at/fetched_at）。
	snapshot := map[string]any{
		"window":       tier.Window,
		"used_percent": tier.UsedPercent,
		"used_tokens":  usedTokens,
		"limit_tokens": limitTokens,
		// 上游无响应字段：由当日窗口语义推导（越南时区每日零点），RFC3339。
		"reset_at":   tier.ResetAt,
		"fetched_at": now.Format(time.RFC3339),
	}
	// DTO 快照读取输出（§4.3 键名对齐，供前端一次拉取）。
	result.Snapshot = &CNProviderSnapshotOutput{
		Window:      tier.Window,
		UsedPercent: &tier.UsedPercent,
		UsedTokens:  &usedTokens,
		LimitTokens: &limitTokens,
		ResetAt:     tier.ResetAt,
		FetchedAt:   now.Format(time.RFC3339),
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		kiraUsageSnapshotExtraKey: snapshot,
	}); err != nil {
		slog.Warn("kira_usage_persist_failed", "account_id", account.ID, "error", err)
	} else {
		result.Persisted = true
	}
	return result, nil
}

// QueryKiraBalance 是 CNProviderBalanceService 的 Kira 分支。复用 usage 端点
// （summary.vndBalance 即钱包 VND 余额），currency=VND。导出（本单接线点清单）：
// 供周期检测/状态机侧复用（只导出，不改语义）。
func (s *CNProviderBalanceService) QueryKiraBalance(ctx context.Context, account *Account) (*CNProviderBalanceResult, error) {
	usageURL, loginURL, err := kiraProbeURLs(s.cfg, account)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "CN_BALANCE_URL_REJECTED", err.Error())
	}
	proxyURL := s.resolveProxyURL(ctx, account)
	client := &kiraProbeClient{
		upstream:    s.httpUpstream,
		proxyURL:    proxyURL,
		accountID:   account.ID,
		concurrency: maxInt(account.Concurrency, 1),
	}
	callCtx, cancel := context.WithTimeout(ctx, cnBalanceUpstreamTimeout*2)
	defer cancel()
	body, status, err := fetchKiraUsageWithReauth(callCtx, client, account, s.accountRepo, usageURL, loginURL)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	// 余额快照键沿用 platform 前缀（与响应式 402/429 写下的 balance_low 键同前缀，
	// 成功探测才能清掉同一把标记）；Provider 字段同样回 platform，保持管理端消费形状。
	result := &CNProviderBalanceResult{
		Provider:   account.Platform,
		FetchedAt:  now.Unix(),
		StatusCode: status,
		Available:  true,
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		result.Error = fmt.Sprintf("kira 鉴权失败 (HTTP %d)：已尝试重新登录仍被拒，请检查 kira_email/kira_password", status)
		return result, nil
	}
	if status < 200 || status >= 300 {
		result.Error = fmt.Sprintf("API error (HTTP %d): %s", status, truncate(strings.TrimSpace(string(body)), 240))
		return result, nil
	}
	summary := gjson.GetBytes(body, "summary")
	if !summary.Exists() {
		result.Error = "Invalid kira balance response: missing summary"
		return result, nil
	}
	// 外审 F3：vndBalance 缺失/null/解析失败或为负数 → 失败关闭：明确错误、
	// Success=false、不落任何快照、不清 balance_low 标记。
	vnd, hasVnd := cnParseF64(summary.Get("vndBalance").Value())
	if !hasVnd || vnd < 0 {
		result.Error = "Invalid kira balance response: summary.vndBalance missing, invalid or negative"
		return result, nil
	}
	// 同源顺带解析当日用量（usage 端点同一响应），仅供 DTO 快照读取输出；
	// 解析失败不影响余额主链路（余额快照照常落库）。
	var snapshotOut *CNProviderSnapshotOutput
	if tier, usedTokens, limitTokens, ok := parseKiraUsageTier(body); ok {
		resetAt := kiraNextDailyReset(now)
		tier.ResetAt = resetAt.UTC().Format(time.RFC3339)
		snapshotOut = &CNProviderSnapshotOutput{
			Window:      tier.Window,
			UsedPercent: &tier.UsedPercent,
			UsedTokens:  &usedTokens,
			LimitTokens: &limitTokens,
			ResetAt:     tier.ResetAt,
			FetchedAt:   now.Format(time.RFC3339),
		}
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
			kiraUsageSnapshotExtraKey: map[string]any{
				"window":       tier.Window,
				"used_percent": tier.UsedPercent,
				"used_tokens":  usedTokens,
				"limit_tokens": limitTokens,
				"reset_at":     tier.ResetAt,
				"fetched_at":   now.Format(time.RFC3339),
			},
		}); err != nil {
			slog.Warn("kira_usage_persist_failed", "account_id", account.ID, "error", err)
		}
	}
	result.Balance = vnd
	result.Currency = "VND"
	result.Balances = []CNProviderBalanceEntry{{Currency: "VND", Balance: vnd}}
	result.Success = true
	result.Snapshot = snapshotOut
	// vndBalance <= 0 是免费/token 包账号的合法稳态（实测账号 vnd=0 仍正常跑免费额度）：
	// 置 Unlimited 让周期检测跳过 VND 阈值停调；vnd > 0 时正常参与阈值比较。
	result.Unlimited = vnd <= 0

	updates := map[string]any{
		cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance):   result.Balance,
		cnExtraKey(account.Platform, cnBalanceExtraSuffixCurrency):  result.Currency,
		cnExtraKey(account.Platform, cnBalanceExtraSuffixAvailable): true,
		cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated):   now.Format(time.RFC3339),
		cnExtraKey(account.Platform, cnBalanceExtraSuffixBalances): []any{
			map[string]any{"currency": "VND", "balance": vnd},
		},
		cnExtraKey(account.Platform, cnBalanceExtraSuffixUnlimited): result.Unlimited,
	}
	// 外审 F4：低余额标记单向清除——仅在 vnd>0 时写 balance_low=false（探测
	// 证实钱包有余额，响应式 402/429 写下的 balance_low 标记可以解除）；
	// vnd=0 时保留既有标记不动（vnd=0 本身就是钱包耗尽信号，写 false 会与
	// 响应式信号互相打摆）。
	if vnd > 0 {
		updates[cnExtraKey(account.Platform, cnBalanceExtraSuffixLow)] = false
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("kira_balance_persist_failed", "account_id", account.ID, "error", err)
	} else {
		result.Persisted = true
	}
	return result, nil
}
