package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

// Muse 额度窗口原始结构（上游 GET <base_url>/usage 的 usage.<window> 子对象）。
// 实测（2026-10-02 主会话亲测）：percent = 已用比例（新号 percent=0，控制台显示 100% left）；
// status = "ok" 表示窗口可用，非 "ok" 表示该窗口已打满/受限。
type museUsageWindowRaw struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

// museUsageResponseRaw 上游 /usage 原始响应结构。
type museUsageResponseRaw struct {
	Usage struct {
		Rolling museUsageWindowRaw `json:"rolling"`
		Weekly  museUsageWindowRaw `json:"weekly"`
		Monthly museUsageWindowRaw `json:"monthly"`
	} `json:"usage"`
}

// MuseWindow muse 单个额度窗口（已固化语义）。
type MuseWindow struct {
	Status   string     `json:"status"`
	Percent  float64    `json:"percent"` // 已用比例 0-100
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// MuseUsageInfo muse 三窗口额度，以及从三窗口推导出的调度冷却信息。
//
// 字段映射（外审-4 收敛，不可再改）：
//   - Percent = 已用比例（0 = 全新）。
//   - Paused  = 任一窗口 status != "ok" 且尚未到其 resetsAt（已到期窗口视为已重置） → 调度暂停。
//   - UnschedulableUntil = 所有「已打满且未到期」窗口 resetsAt 的最大值；
//     供调度器写 accounts.temp_unschedulable_until，到期自动恢复（muse-2 消费）。
type MuseUsageInfo struct {
	Rolling *MuseWindow `json:"rolling,omitempty"`
	Weekly  *MuseWindow `json:"weekly,omitempty"`
	Monthly *MuseWindow `json:"monthly,omitempty"`

	Paused              bool       `json:"paused"`
	UnschedulableUntil  *time.Time `json:"unschedulable_until,omitempty"`
}

const (
	museFetchTimeout                  = 30 * time.Second
	museConsecutiveFailAlertThreshold = 3
	museStaleDataThreshold            = 10 * time.Minute
)

// MuseQuotaFetcher 从 OpenCode Go 上游拉取 muse（Meta Muse Spark Contributor）三窗口额度。
//
// 设计约束（派发单 muse-5 / 禁区）：
//   - 上游不可达 / 非 2xx 必须失败关闭并返回明确错误，绝不 fallback（照 Antigravity 错误路径）。
//   - 不触碰 Antigravity/Grok/Claude 既有 fetcher 行为，不触碰 handler/ent/前端。
//   - 观测日志不泄露密钥（只记 account_id / http 状态码 / 三窗口 percent+resetsAt / 成功时间 / 连续失败 / 新鲜度）。
type MuseQuotaFetcher struct {
	proxyRepo ProxyRepository
	cfg       *config.Config
}

// NewMuseQuotaFetcher 创建 MuseQuotaFetcher。
func NewMuseQuotaFetcher(proxyRepo ProxyRepository, cfg *config.Config) *MuseQuotaFetcher {
	return &MuseQuotaFetcher{proxyRepo: proxyRepo, cfg: cfg}
}

// CanFetch 仅 muse 平台且已配置 api_key 的账号可拉取。
func (f *MuseQuotaFetcher) CanFetch(account *Account) bool {
	if account == nil || account.Platform != PlatformMuse {
		return false
	}
	return account.GetCredential("api_key") != ""
}

// GetProxyURL 解析账号代理 URL（与 AntigravityQuotaFetcher 同模式）。
func (f *MuseQuotaFetcher) GetProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil || f.proxyRepo == nil {
		return ""
	}
	proxy, err := f.proxyRepo.GetByID(ctx, *account.ProxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}

// FetchQuota 拉取 muse 三窗口额度。上游不可达 / 非 2xx 一律失败关闭（不 fallback）。
func (f *MuseQuotaFetcher) FetchQuota(ctx context.Context, account *Account, proxyURL string) (*QuotaResult, error) {
	apiKey := account.GetCredential("api_key")
	baseURL := strings.TrimRight(account.GetCredential("base_url"), "/")
	if apiKey == "" {
		return nil, fmt.Errorf("muse: missing api_key credential for account %d", account.ID)
	}
	if baseURL == "" {
		return nil, fmt.Errorf("muse: missing base_url credential for account %d", account.ID)
	}
	usageURL := baseURL + "/usage"

	reqCtx, cancel := context.WithTimeout(ctx, museFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("muse: build usage request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	client, err := httppool.GetClient(httppool.Options{
		ProxyURL:              proxyURL,
		Timeout:               museFetchTimeout,
		ResponseHeaderTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("muse: build http client failed: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		// 上游不可达：失败关闭，明确错误，绝不 fallback。
		return nil, fmt.Errorf("muse: usage request failed (upstream unreachable): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("muse: read usage response failed: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("muse: usage endpoint returned HTTP %d", resp.StatusCode)
	}

	var raw museUsageResponseRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("muse: decode usage response failed: %w", err)
	}

	now := time.Now()
	usage := buildMuseUsageInfo(&raw, now)

	// 运行观测（外审-8）：结构化日志，不泄露密钥。
	args := []any{
		slog.Int64("account_id", account.ID),
		slog.Int("http_status", resp.StatusCode),
		slog.Bool("paused", usage.Paused),
	}
	args = append(args, logMuseWindowAttrs("rolling", usage.Rolling)...)
	args = append(args, logMuseWindowAttrs("weekly", usage.Weekly)...)
	args = append(args, logMuseWindowAttrs("monthly", usage.Monthly)...)
	slog.Info("muse quota fetched", args...)

	return &QuotaResult{
		UsageInfo: &UsageInfo{UpdatedAt: &now, MuseUsage: usage},
		Raw:       map[string]any{"usage": json.RawMessage(body)},
	}, nil
}

// logMuseWindowAttrs 把单个窗口转成 slog 属性（不含任何密钥信息），供结构化日志使用。
func logMuseWindowAttrs(prefix string, w *MuseWindow) []any {
	if w == nil {
		return []any{slog.String(prefix+"_status", "absent")}
	}
	attrs := []any{
		slog.String(prefix+"_status", w.Status),
		slog.Float64(prefix+"_percent", w.Percent),
	}
	if w.ResetsAt != nil {
		attrs = append(attrs, slog.Time(prefix+"_resets_at", *w.ResetsAt))
	}
	return attrs
}

// parseMuseWindow 把上游原始窗口转成 MuseWindow，resetsAt 解析失败则留空。
func parseMuseWindow(w museUsageWindowRaw) *MuseWindow {
	mw := &MuseWindow{Status: w.Status, Percent: float64(w.Percent)}
	if w.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
			mw.ResetsAt = &t
		}
	}
	return mw
}

// buildMuseUsageInfo 由原始响应构造三窗口额度并推导暂停/恢复。
func buildMuseUsageInfo(raw *museUsageResponseRaw, now time.Time) *MuseUsageInfo {
	info := &MuseUsageInfo{
		Rolling: parseMuseWindow(raw.Usage.Rolling),
		Weekly:  parseMuseWindow(raw.Usage.Weekly),
		Monthly: parseMuseWindow(raw.Usage.Monthly),
	}
	info.Paused, info.UnschedulableUntil = computeMusePauseAndRecovery(info, now)
	return info
}

// computeMusePauseAndRecovery 推导暂停判定与恢复时间（纯函数，便于边界值测试锚定）。
//
// 暂停 = 存在任一窗口 status != "ok" 且其 resetsAt 尚未到期（已到期窗口视为已重置，不再阻塞）。
// 恢复 = 所有「已打满且未到期」窗口 resetsAt 的最大值；若无此类窗口，返回 nil（不暂停）。
func computeMusePauseAndRecovery(info *MuseUsageInfo, now time.Time) (bool, *time.Time) {
	windows := []*MuseWindow{info.Rolling, info.Weekly, info.Monthly}
	var maxReset *time.Time
	paused := false
	for _, w := range windows {
		if w == nil || w.Status == "ok" {
			continue
		}
		// 已到期：窗口已重置，不再阻塞调度（时间恢复语义）。
		if w.ResetsAt != nil && !w.ResetsAt.After(now) {
			continue
		}
		paused = true
		if w.ResetsAt != nil {
			if maxReset == nil || w.ResetsAt.After(*maxReset) {
				maxReset = w.ResetsAt
			}
		}
	}
	return paused, maxReset
}

// buildMuseDegradedUsage 从 FetchQuota 错误构建降级 UsageInfo（失败关闭，不 fallback）。
func buildMuseDegradedUsage(account *Account, fetchErr error) *UsageInfo {
	now := time.Now()
	slog.Warn("muse usage fetch failed, returning degraded response",
		"account_id", account.ID, "error", fetchErr)

	info := &UsageInfo{
		UpdatedAt: &now,
		Error:     fmt.Sprintf("muse usage API error: %v", fetchErr),
	}

	switch {
	case strings.Contains(fetchErr.Error(), "HTTP 401"):
		info.ErrorCode = errorCodeUnauthenticated
		info.NeedsReauth = true
	case strings.Contains(fetchErr.Error(), "HTTP 403"):
		info.ErrorCode = errorCodeForbidden
		info.IsForbidden = true
	case strings.Contains(fetchErr.Error(), "HTTP 429"):
		info.ErrorCode = errorCodeRateLimited
	default:
		info.ErrorCode = errorCodeNetworkError
	}
	return info
}

// museUsageCacheTTL 根据 UsageInfo 内容决定缓存 TTL（与 antigravity 同口径）。
func museUsageCacheTTL(info *UsageInfo) time.Duration {
	if info == nil || info.Error != "" || info.ErrorCode != "" {
		return antigravityErrorTTL // 1 分钟
	}
	return apiCacheTTL // 3 分钟
}
