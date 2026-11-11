package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 本文件实现 Web 控制台三家上游用量快照的纯函数归一与耗尽判定。
// 严格对应方案 docs-local/web-console-quota-playwright-plan.md §4.1–§4.3。
// 严禁任何 I/O、网络、DB 访问：本文件只做 JSON 解析与内存计算。

// ---------------------------------------------------------------------------
// 统一快照与上游数据结构（写入 accounts.extra["web_console_quota_snapshot"]）
// ---------------------------------------------------------------------------

// WebConsoleQuotaTier 单滚动窗口。
type WebConsoleQuotaTier struct {
	Window  string  `json:"window"` // "5h" | "7d" | "week" | "month"
	Limit   float64 `json:"limit"`
	Used    float64 `json:"used"`
	ResetAt string  `json:"reset_at,omitempty"` // RFC3339
}

// WebConsoleQuotaLot 资源包（longcat）。
type WebConsoleQuotaLot struct {
	LotID          int64   `json:"lot_id"`
	Status         string  `json:"status"`
	RemainingToken float64 `json:"remaining_token"`
	TotalToken     float64 `json:"total_token"`
	ExpireTime     string  `json:"expire_time,omitempty"`
	IsCurrent      bool    `json:"is_current,omitempty"`
}

// WebConsoleQuotaPool sensenova 单池。
type WebConsoleQuotaPool struct {
	ID                          string               `json:"id"`
	PoolType                    string               `json:"pool_type"`
	Window5h                    *WebConsoleQuotaTier `json:"window_5h,omitempty"`
	Window7d                    *WebConsoleQuotaTier `json:"window_7d,omitempty"`
	GrantBalance                *float64             `json:"grant_balance,omitempty"`
	NearestGrantExpiringBalance *float64             `json:"nearest_grant_expiring_balance,omitempty"`
	NearestGrantExpiry          string               `json:"nearest_grant_expiry,omitempty"`
}

// WebConsoleQuotaSnapshot 统一快照。
type WebConsoleQuotaSnapshot struct {
	Provider  string `json:"provider"`
	FetchedAt string `json:"fetched_at"` // RFC3339
	// 认证态
	AuthState    string `json:"auth_state"` // "ok" | "needs_interactive_auth" | "auth_failed" | "error"
	AuthReason   string `json:"auth_reason,omitempty"`
	AuthError    string `json:"auth_error,omitempty"`
	AccountLabel string `json:"account_label,omitempty"`
	// compshare
	PlanMatchStatus     string                `json:"plan_match_status,omitempty"` // "matched" | "unmatched" | "no_active_plan"
	PlanName            string                `json:"plan_name,omitempty"`
	ExpireAt            int64                 `json:"expire_at,omitempty"`
	SubscriptionExpired bool                  `json:"subscription_expired,omitempty"`
	Tiers               []WebConsoleQuotaTier `json:"tiers,omitempty"` // compshare 5h/week/month 或 sensenova 选定池的 5h/7d
	// sensenova
	Pools        []WebConsoleQuotaPool `json:"pools,omitempty"`
	GrantBalance *float64              `json:"grant_balance,omitempty"`
	// longcat
	Lots             []WebConsoleQuotaLot `json:"lots,omitempty"`
	PaygoBalanceCent *int64               `json:"paygo_balance_cent,omitempty"`
	PaygoStatus      string               `json:"paygo_status,omitempty"`
	// 判定结果
	Exhausted       bool   `json:"exhausted"`
	Until           string `json:"until,omitempty"` // RFC3339；无限期哨兵时写 "9999-12-31T00:00:00Z"
	UntilIndefinite bool   `json:"until_indefinite,omitempty"`
}

// 无限期停调哨兵：与同包 temp_unsched.go 的 tempUnschedIndefiniteUntil 完全同值同语义
// （9999-12-31T00:00:00Z），用于在快照层表达"无限期直到探针确认恢复"。该变量在同包
// 可见，本文件直接复用，绝不定义第二份。

// ---------------------------------------------------------------------------
// Worker 返回的 data（rawData）输入结构（仅本文件解析用，不对外暴露）
// ---------------------------------------------------------------------------

type webConsoleCompsharePlan struct {
	Code       string   `json:"code"`
	PlanName   string   `json:"plan_name"`
	ExpireAt   int64    `json:"expire_at"`
	PlanStatus int      `json:"plan_status"`
	Limit5h    float64  `json:"limit_5h"`
	Used5h     float64  `json:"used_5h"`
	Reset5h    string   `json:"reset_5h"`
	LimitWeek  float64  `json:"limit_week"`
	UsedWeek   float64  `json:"used_week"`
	ResetWeek  string   `json:"reset_week"`
	LimitMonth float64  `json:"limit_month"`
	UsedMonth  float64  `json:"used_month"`
	ResetMonth string   `json:"reset_month"`
	KeyHashes  []string `json:"key_hashes"`
}

type webConsoleCompshareData struct {
	AuthState    string                    `json:"auth_state"`
	AuthReason   string                    `json:"auth_reason"`
	AuthError    string                    `json:"auth_error"`
	AccountLabel string                    `json:"account_label"`
	Plans        []webConsoleCompsharePlan `json:"plans"`
}

type webConsoleSensenovaWindow struct {
	Limit     float64 `json:"limit"`
	Remaining float64 `json:"remaining"`
	ResetAt   string  `json:"reset_at"`
}

type webConsoleSensenovaPool struct {
	ID                          string                     `json:"id"`
	PoolType                    string                     `json:"pool_type"`
	Window5h                    *webConsoleSensenovaWindow `json:"window_5h"`
	Window7d                    *webConsoleSensenovaWindow `json:"window_7d"`
	GrantBalance                *float64                   `json:"grant_balance"`
	NearestGrantExpiringBalance *float64                   `json:"nearest_grant_expiring_balance"`
	NearestGrantExpiry          string                     `json:"nearest_grant_expiry"`
}

type webConsoleSensenovaData struct {
	AuthState    string                    `json:"auth_state"`
	AuthReason   string                    `json:"auth_reason"`
	AuthError    string                    `json:"auth_error"`
	AccountLabel string                    `json:"account_label"`
	Pools        []webConsoleSensenovaPool `json:"pools"`
}

type webConsoleLongcatLot struct {
	LotID          int64   `json:"lot_id"`
	Status         string  `json:"status"`
	RemainingToken float64 `json:"remaining_token"`
	TotalToken     float64 `json:"total_token"`
	ExpireTime     string  `json:"expire_time"`
	IsCurrent      bool    `json:"is_current"`
}

type webConsoleLongcatData struct {
	AuthState        string                 `json:"auth_state"`
	AuthReason       string                 `json:"auth_reason"`
	AuthError        string                 `json:"auth_error"`
	AccountLabel     string                 `json:"account_label"`
	Lots             []webConsoleLongcatLot `json:"lots"`
	PaygoBalanceCent int64                  `json:"paygo_balance_cent"`
	PaygoStatus      string                 `json:"paygo_status"`
}

// ---------------------------------------------------------------------------
// 主入口
// ---------------------------------------------------------------------------

// BuildWebConsoleQuotaSnapshot 把 worker 返回的 data（JSON）归一为快照，并判定耗尽与停调截止时间。
// 参数 now 为当前时间；apiKeySHA256 为当前账号 API Key 的 sha256 hex（compshare 套餐匹配用）；
// modelMapping 为调用方传入的 account.GetModelMapping()（仅消费其 value，sensenova 池判定用）。
//
// 本函数为纯函数，严禁任何 I/O、网络、DB 访问。
func BuildWebConsoleQuotaSnapshot(provider string, rawData []byte, apiKeySHA256 string, modelMapping map[string]string, now time.Time) (*WebConsoleQuotaSnapshot, error) {
	snap := &WebConsoleQuotaSnapshot{
		Provider:  provider,
		FetchedAt: now.UTC().Format(time.RFC3339),
	}
	switch provider {
	case "compshare":
		return buildCompshareSnapshot(snap, rawData, apiKeySHA256, now)
	case "sensenova":
		return buildSensenovaSnapshot(snap, rawData, modelMapping, now)
	case "longcat":
		return buildLongcatSnapshot(snap, rawData, now)
	default:
		return nil, fmt.Errorf("unsupported web console quota provider: %q", provider)
	}
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

func defaultAuthState(s string) string {
	if s == "" {
		return "ok"
	}
	return s
}

func parseWebConsoleQuotaResetAt(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ---------------------------------------------------------------------------
// compshare（§4.1）
// ---------------------------------------------------------------------------

func buildCompshareSnapshot(snap *WebConsoleQuotaSnapshot, rawData []byte, apiKeySHA256 string, now time.Time) (*WebConsoleQuotaSnapshot, error) {
	var d webConsoleCompshareData
	if err := json.Unmarshal(rawData, &d); err != nil {
		return nil, err
	}
	snap.AuthState = defaultAuthState(d.AuthState)
	snap.AuthReason = d.AuthReason
	snap.AuthError = d.AuthError
	snap.AccountLabel = d.AccountLabel

	plans := d.Plans
	switch {
	case len(plans) == 0:
		// 无有效订阅套餐
		snap.PlanMatchStatus = "no_active_plan"
		snap.SubscriptionExpired = true
		snap.Exhausted = true
		snap.UntilIndefinite = true
		return snap, nil
	case len(plans) == 1:
		p := plans[0]
		snap.PlanMatchStatus = "matched"
		snap.PlanName = p.PlanName
		return finalizeCompsharePlan(snap, p, now), nil
	default:
		// 多套餐：按 apiKeySHA256 精确匹配 key_hashes
		var matched *webConsoleCompsharePlan
		for i := range plans {
			for _, h := range plans[i].KeyHashes {
				if h == apiKeySHA256 {
					matched = &plans[i]
					break
				}
			}
			if matched != nil {
				break
			}
		}
		if matched == nil {
			// 未命中任何套餐：失败关闭，不触发停调也不触发清除
			snap.PlanMatchStatus = "unmatched"
			// Exhausted 保持 false，Until 保持空
			return snap, nil
		}
		snap.PlanMatchStatus = "matched"
		snap.PlanName = matched.PlanName
		return finalizeCompsharePlan(snap, *matched, now), nil
	}
}

func finalizeCompsharePlan(snap *WebConsoleQuotaSnapshot, p webConsoleCompsharePlan, now time.Time) *WebConsoleQuotaSnapshot {
	// Tiers 始终填三档（5h/week/month）
	snap.Tiers = []WebConsoleQuotaTier{
		{Window: "5h", Limit: p.Limit5h, Used: p.Used5h, ResetAt: p.Reset5h},
		{Window: "week", Limit: p.LimitWeek, Used: p.UsedWeek, ResetAt: p.ResetWeek},
		{Window: "month", Limit: p.LimitMonth, Used: p.UsedMonth, ResetAt: p.ResetMonth},
	}
	snap.ExpireAt = p.ExpireAt

	// 已过期判定
	if (p.ExpireAt > 0 && now.Unix() >= p.ExpireAt) || p.PlanStatus != 1 {
		snap.SubscriptionExpired = true
		snap.Exhausted = true
		snap.UntilIndefinite = true
		return snap
	}

	// 激活窗口集合 W = {w | w.limit > 0}
	windows := []struct {
		used  float64
		limit float64
		reset string
	}{
		{p.Used5h, p.Limit5h, p.Reset5h},
		{p.UsedWeek, p.LimitWeek, p.ResetWeek},
		{p.UsedMonth, p.LimitMonth, p.ResetMonth},
	}

	exhausted := false
	var rFuture []time.Time
	for _, w := range windows {
		if w.limit > 0 && w.used >= w.limit {
			exhausted = true
			if t, ok := parseWebConsoleQuotaResetAt(w.reset); ok && t.After(now) {
				rFuture = append(rFuture, t)
			}
		}
	}

	snap.Exhausted = exhausted
	if exhausted {
		if len(rFuture) > 0 {
			mx := rFuture[0]
			for _, t := range rFuture[1:] {
				if t.After(mx) {
					mx = t
				}
			}
			snap.Until = mx.UTC().Format(time.RFC3339)
		} else {
			// 官方未返回有效未来重置时间：固定取无限期哨兵
			snap.UntilIndefinite = true
		}
	}
	return snap
}

// ---------------------------------------------------------------------------
// sensenova（§4.2）
// ---------------------------------------------------------------------------

func findSensenovaPool(pools []webConsoleSensenovaPool, want string) *webConsoleSensenovaPool {
	for i := range pools {
		if pools[i].PoolType == want {
			return &pools[i]
		}
	}
	return nil
}

func sensenovaPoolBlocked(p *webConsoleSensenovaPool, now time.Time) bool {
	if p.Window5h != nil && p.Window5h.Limit > 0 && p.Window5h.Remaining <= 0 {
		return true
	}
	if p.Window7d != nil && p.Window7d.Limit > 0 && p.Window7d.Remaining <= 0 {
		return true
	}
	return false
}

// sensenovaPoolRecovery 返回池 p 的恢复时间：阻塞窗口的最晚未来重置时间；
// 若阻断窗口缺失未来 reset_at，则返回无限期哨兵。
func sensenovaPoolRecovery(p *webConsoleSensenovaPool, now time.Time) time.Time {
	var cands []time.Time
	if p.Window5h != nil && p.Window5h.Limit > 0 && p.Window5h.Remaining <= 0 {
		if t, ok := parseWebConsoleQuotaResetAt(p.Window5h.ResetAt); ok && t.After(now) {
			cands = append(cands, t)
		}
	}
	if p.Window7d != nil && p.Window7d.Limit > 0 && p.Window7d.Remaining <= 0 {
		if t, ok := parseWebConsoleQuotaResetAt(p.Window7d.ResetAt); ok && t.After(now) {
			cands = append(cands, t)
		}
	}
	if len(cands) == 0 {
		return tempUnschedIndefiniteUntil
	}
	mx := cands[0]
	for _, t := range cands[1:] {
		if t.After(mx) {
			mx = t
		}
	}
	return mx
}

func toOutputSensenovaPool(p webConsoleSensenovaPool) WebConsoleQuotaPool {
	out := WebConsoleQuotaPool{
		ID:                          p.ID,
		PoolType:                    p.PoolType,
		GrantBalance:                p.GrantBalance,
		NearestGrantExpiringBalance: p.NearestGrantExpiringBalance,
		NearestGrantExpiry:          p.NearestGrantExpiry,
	}
	if p.Window5h != nil {
		out.Window5h = &WebConsoleQuotaTier{
			Window:  "5h",
			Limit:   p.Window5h.Limit,
			Used:    p.Window5h.Limit - p.Window5h.Remaining,
			ResetAt: p.Window5h.ResetAt,
		}
	}
	if p.Window7d != nil {
		out.Window7d = &WebConsoleQuotaTier{
			Window:  "7d",
			Limit:   p.Window7d.Limit,
			Used:    p.Window7d.Limit - p.Window7d.Remaining,
			ResetAt: p.Window7d.ResetAt,
		}
	}
	return out
}

func buildSensenovaSnapshot(snap *WebConsoleQuotaSnapshot, rawData []byte, modelMapping map[string]string, now time.Time) (*WebConsoleQuotaSnapshot, error) {
	var d webConsoleSensenovaData
	if err := json.Unmarshal(rawData, &d); err != nil {
		return nil, err
	}
	snap.AuthState = defaultAuthState(d.AuthState)
	snap.AuthReason = d.AuthReason
	snap.AuthError = d.AuthError
	snap.AccountLabel = d.AccountLabel

	// 输出池
	outputPools := make([]WebConsoleQuotaPool, 0, len(d.Pools))
	for _, p := range d.Pools {
		outputPools = append(outputPools, toOutputSensenovaPool(p))
	}
	snap.Pools = outputPools

	// targets = 去空白转小写后的全部 value（非空）
	var hasFlash, hasNonFlash bool
	for _, v := range modelMapping {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if v == "sensenova-6.8-flash-lite" {
			hasFlash = true
		} else {
			hasNonFlash = true
		}
	}

	// 可用池集合 P
	var P []string
	if hasNonFlash && !hasFlash {
		P = []string{"general"}
	} else {
		// 纯 Flash / 空映射 / 混合
		P = []string{"flash-lite", "general"}
	}

	allBlocked := true
	var rVals []time.Time
	for _, name := range P {
		pool := findSensenovaPool(d.Pools, name)
		if pool == nil {
			// 后端未返回该池：无法阻塞，视为未阻塞（保守不误封）
			allBlocked = false
			continue
		}
		if !sensenovaPoolBlocked(pool, now) {
			allBlocked = false
		}
		rVals = append(rVals, sensenovaPoolRecovery(pool, now))
	}

	// 通用赠金可用条件 G
	G := false
	if gp := findSensenovaPool(d.Pools, "general"); gp != nil && gp.GrantBalance != nil && *gp.GrantBalance > 0 {
		G = true
	}

	snap.Exhausted = allBlocked && !G
	if snap.Exhausted {
		if len(rVals) > 0 {
			mn := rVals[0]
			for _, t := range rVals[1:] {
				if t.Before(mn) {
					mn = t
				}
			}
			if mn.Equal(tempUnschedIndefiniteUntil) {
				snap.UntilIndefinite = true
			} else {
				snap.Until = mn.UTC().Format(time.RFC3339)
			}
		} else {
			snap.UntilIndefinite = true
		}
	}

	// GrantBalance 取 general 池（空映射/缺类型时回退单池）
	if gp := findSensenovaPool(d.Pools, "general"); gp != nil {
		snap.GrantBalance = gp.GrantBalance
	} else if len(d.Pools) == 1 && d.Pools[0].PoolType == "" {
		snap.GrantBalance = d.Pools[0].GrantBalance
	}

	// Tiers 填所选（general 优先）池的 5h/7d
	var sel *webConsoleSensenovaPool
	if gp := findSensenovaPool(d.Pools, "general"); gp != nil {
		sel = gp
	} else if fl := findSensenovaPool(d.Pools, "flash-lite"); fl != nil {
		sel = fl
	} else if len(d.Pools) == 1 && d.Pools[0].PoolType == "" {
		sel = &d.Pools[0]
	}
	if sel != nil {
		var tiers []WebConsoleQuotaTier
		if sel.Window5h != nil {
			tiers = append(tiers, WebConsoleQuotaTier{
				Window:  "5h",
				Limit:   sel.Window5h.Limit,
				Used:    sel.Window5h.Limit - sel.Window5h.Remaining,
				ResetAt: sel.Window5h.ResetAt,
			})
		}
		if sel.Window7d != nil {
			tiers = append(tiers, WebConsoleQuotaTier{
				Window:  "7d",
				Limit:   sel.Window7d.Limit,
				Used:    sel.Window7d.Limit - sel.Window7d.Remaining,
				ResetAt: sel.Window7d.ResetAt,
			})
		}
		snap.Tiers = tiers
	}
	return snap, nil
}

// ---------------------------------------------------------------------------
// longcat（§4.3）
// ---------------------------------------------------------------------------

func buildLongcatSnapshot(snap *WebConsoleQuotaSnapshot, rawData []byte, now time.Time) (*WebConsoleQuotaSnapshot, error) {
	var d webConsoleLongcatData
	if err := json.Unmarshal(rawData, &d); err != nil {
		return nil, err
	}
	snap.AuthState = defaultAuthState(d.AuthState)
	snap.AuthReason = d.AuthReason
	snap.AuthError = d.AuthError
	snap.AccountLabel = d.AccountLabel

	var rPack float64
	lots := make([]WebConsoleQuotaLot, 0, len(d.Lots))
	for _, l := range d.Lots {
		lots = append(lots, WebConsoleQuotaLot{
			LotID:          l.LotID,
			Status:         l.Status,
			RemainingToken: l.RemainingToken,
			TotalToken:     l.TotalToken,
			ExpireTime:     l.ExpireTime,
			IsCurrent:      l.IsCurrent,
		})
		// 仅 status==ACTIVE 计入；max(0, remaining_token)
		if l.Status == "ACTIVE" && l.RemainingToken > 0 {
			rPack += l.RemainingToken
		}
	}
	snap.Lots = lots

	paygo := d.PaygoBalanceCent
	snap.PaygoBalanceCent = &paygo
	snap.PaygoStatus = d.PaygoStatus

	// OR 门：两路全无余额才耗尽
	snap.Exhausted = (rPack <= 0) && (paygo <= 0)
	if snap.Exhausted {
		// longcat 无周期重置：耗尽时置无限期哨兵
		snap.UntilIndefinite = true
	}
	return snap, nil
}
