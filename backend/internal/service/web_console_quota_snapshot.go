package service

import (
	"encoding/json"
	"fmt"
	"strconv"
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

// WebConsoleQuotaTokenPlanSeat Token Plan 单档席位配额（讯飞）。
type WebConsoleQuotaTokenPlanSeat struct {
	SeatType       float64 `json:"seat_type"`
	SeatTypeName   string  `json:"seat_type_name"`
	TotalCount     float64 `json:"total_count"`
	AssignedCount  float64 `json:"assigned_count"`
	RemainingCount float64 `json:"remaining_count"`
}

// WebConsoleQuotaTokenPlan Token Plan 席位汇总（讯飞）。
type WebConsoleQuotaTokenPlan struct {
	TotalCount     float64                        `json:"total_count"`
	AssignedCount  float64                        `json:"assigned_count"`
	RemainingCount float64                        `json:"remaining_count"`
	BySeat         []WebConsoleQuotaTokenPlanSeat `json:"by_seat,omitempty"`
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
	PlanIsTeam          bool                  `json:"plan_is_team,omitempty"` // 命中的是团队套餐（同一接口 IsTeam 作用域）
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
	// iflytek（讯飞星辰 MaaS）
	Balance         *float64                  `json:"balance,omitempty"`
	BalanceCurrency string                    `json:"balance_currency,omitempty"`
	Delinquent      *float64                  `json:"delinquent,omitempty"`
	VirtualBalance  *float64                  `json:"virtual_balance,omitempty"`
	TokenPlan       *WebConsoleQuotaTokenPlan `json:"token_plan,omitempty"`
	CodingPlanTiers []WebConsoleQuotaTier     `json:"coding_plan_tiers,omitempty"`
	// 判定结果
	Exhausted       bool   `json:"exhausted"`
	Until           string `json:"until,omitempty"` // RFC3339；无限期哨兵时写 "9999-12-31T00:00:00Z"
	UntilIndefinite bool   `json:"until_indefinite,omitempty"`
}

// 无限期停调哨兵：与同包 temp_unsched.go 的 tempUnschedIndefiniteUntil 完全同值同语义
// （9999-12-31T00:00:00Z），用于在快照层表达"无限期直到探针确认恢复"。该变量在同包
// 可见，本文件直接复用，绝不定义第二份。

// ---------------------------------------------------------------------------
// 时间戳兼容反序列化（Unix 秒/毫秒数字、数字字符串、RFC3339）
// ---------------------------------------------------------------------------

// webConsoleFlexTimestamp 是输入侧的宽松时间戳类型：上游（Python 适配器透传的
// 优云智算 API 字段）可能给出 Unix 秒/毫秒整数、纯数字字符串或 RFC3339 字符串。
// 统一归一为 RFC3339 UTC 字符串；无法解释的非空字符串原样保留（交给后续
// parseWebConsoleQuotaResetAt 判定或直接透传）。该类型仅用于输入结构，
// 对外输出结构体字段仍为 string，契约不变。
type webConsoleFlexTimestamp string

const webConsoleMillisThreshold int64 = 1_000_000_000_000

func webConsoleIsASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// webConsoleFormatEpoch 把 Unix 秒（或毫秒，>= 1e12 视为毫秒）格式化为 RFC3339 UTC。
// 非正值返回空串，表示"无有效重置时间"。
func webConsoleFormatEpoch(v int64) string {
	if v <= 0 {
		return ""
	}
	if v >= webConsoleMillisThreshold {
		return time.UnixMilli(v).UTC().Format(time.RFC3339)
	}
	return time.Unix(v, 0).UTC().Format(time.RFC3339)
}

// UnmarshalJSON 实现宽松解析。其他 JSON 类型（对象/数组/布尔）返回 json 错误，不静默吞错。
func (t *webConsoleFlexTimestamp) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*t = ""
		return nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*t = ""
			return nil
		}
		if webConsoleIsASCIIDigits(s) {
			if v, err := strconv.ParseInt(s, 10, 64); err == nil {
				*t = webConsoleFlexTimestamp(webConsoleFormatEpoch(v))
				return nil
			}
		}
		*t = webConsoleFlexTimestamp(s)
		return nil
	}

	if trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9') {
		// 布尔/对象/数组等非法类型：交由 json 产生标准错误
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*t = webConsoleFlexTimestamp(s)
		return nil
	}

	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	v, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		f, ferr := n.Float64()
		if ferr != nil {
			return ferr
		}
		v = int64(f)
	}
	*t = webConsoleFlexTimestamp(webConsoleFormatEpoch(v))
	return nil
}

// ---------------------------------------------------------------------------
// Worker 返回的 data（rawData）输入结构（仅本文件解析用，不对外暴露）
// ---------------------------------------------------------------------------

type webConsoleCompsharePlan struct {
	Code       string                  `json:"code"`
	PlanName   string                  `json:"plan_name"`
	IsTeam     bool                    `json:"is_team"`
	ExpireAt   int64                   `json:"expire_at"`
	PlanStatus int                     `json:"plan_status"`
	Limit5h    float64                 `json:"limit_5h"`
	Used5h     float64                 `json:"used_5h"`
	Reset5h    webConsoleFlexTimestamp `json:"reset_5h"`
	LimitWeek  float64                 `json:"limit_week"`
	UsedWeek   float64                 `json:"used_week"`
	ResetWeek  webConsoleFlexTimestamp `json:"reset_week"`
	LimitMonth float64                 `json:"limit_month"`
	UsedMonth  float64                 `json:"used_month"`
	ResetMonth webConsoleFlexTimestamp `json:"reset_month"`
	KeyHashes  []string                `json:"key_hashes"`
}

type webConsoleCompshareData struct {
	AuthState    string                    `json:"auth_state"`
	AuthReason   string                    `json:"auth_reason"`
	AuthError    string                    `json:"auth_error"`
	AccountLabel string                    `json:"account_label"`
	Plans        []webConsoleCompsharePlan `json:"plans"`
}

type webConsoleSensenovaWindow struct {
	Limit     float64                 `json:"limit"`
	Remaining float64                 `json:"remaining"`
	ResetAt   webConsoleFlexTimestamp `json:"reset_at"`
}

type webConsoleSensenovaPool struct {
	ID                          string                     `json:"id"`
	PoolType                    string                     `json:"pool_type"`
	Window5h                    *webConsoleSensenovaWindow `json:"window_5h"`
	Window7d                    *webConsoleSensenovaWindow `json:"window_7d"`
	GrantBalance                *float64                   `json:"grant_balance"`
	NearestGrantExpiringBalance *float64                   `json:"nearest_grant_expiring_balance"`
	NearestGrantExpiry          webConsoleFlexTimestamp    `json:"nearest_grant_expiry"`
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

// webConsoleIflytekUsage 是 Coding Plan 窗口用量字段的承载类型：既可能出现在
// row 顶层，也可能嵌套在 codingPlanUsageDTO 中。用指针以便区分"字段缺失"与"值为 0"。
type webConsoleIflytekUsage struct {
	RP5hLimit  *float64 `json:"rp5hLimit"`
	RP5hUsage  *float64 `json:"rp5hUsage"`
	RPwLimit   *float64 `json:"rpwLimit"`
	RPwUsage   *float64 `json:"rpwUsage"`
	PackageLimit *float64 `json:"packageLimit"`
	PackageUsage *float64 `json:"packageUsage"`
	PackageLeft  *float64 `json:"packageLeft"`
}

// webConsoleIflytekCodingRow 是 coding_plan_list.rows[] 的单条。取值规则：
// 先读 row 顶层字段，缺失（nil）时回退到 codingPlanUsageDTO 的同名字段。
type webConsoleIflytekCodingRow struct {
	RP5hLimit     *float64                  `json:"rp5hLimit"`
	RP5hUsage     *float64                  `json:"rp5hUsage"`
	RPwLimit      *float64                  `json:"rpwLimit"`
	RPwUsage      *float64                  `json:"rpwUsage"`
	PackageLimit  *float64                  `json:"packageLimit"`
	PackageUsage  *float64                  `json:"packageUsage"`
	PackageLeft   *float64                  `json:"packageLeft"`
	CodingPlanUsageDTO *webConsoleIflytekUsage `json:"codingPlanUsageDTO"`
}

type webConsoleIflytekCodingList struct {
	Page  int                         `json:"page"`
	Size  int                         `json:"size"`
	Total int                         `json:"total"`
	Rows  []webConsoleIflytekCodingRow `json:"rows"`
}

type webConsoleIflytekSeat struct {
	SeatType       float64 `json:"seatType"`
	SeatTypeName   string  `json:"seatTypeName"`
	TotalCount     float64 `json:"totalCount"`
	AssignedCount  float64 `json:"assignedCount"`
	RemainingCount float64 `json:"remainingCount"`
}

type webConsoleIflytekTokenPlanQuota struct {
	Quotas []webConsoleIflytekSeat `json:"quotas"`
}

type webConsoleIflytekBalance struct {
	Balance        float64 `json:"balance"`
	Delinquent     float64 `json:"delinquent"`
	VirtualBalance float64 `json:"virtualBalance"`
}

type webConsoleIflytekData struct {
	AuthState      string                       `json:"auth_state"`
	AuthReason     string                       `json:"auth_reason"`
	AuthError      string                       `json:"auth_error"`
	AccountLabel   string                       `json:"account_label"`
	Balance        webConsoleIflytekBalance     `json:"balance"`
	TokenPlanQuota webConsoleIflytekTokenPlanQuota `json:"token_plan_quota"`
	CodingPlanList webConsoleIflytekCodingList  `json:"coding_plan_list"`
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
	case providerIflytek:
		return buildIflytekSnapshot(snap, rawData, now)
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
		snap.PlanIsTeam = p.IsTeam
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
		snap.PlanIsTeam = matched.IsTeam
		return finalizeCompsharePlan(snap, *matched, now), nil
	}
}

func finalizeCompsharePlan(snap *WebConsoleQuotaSnapshot, p webConsoleCompsharePlan, now time.Time) *WebConsoleQuotaSnapshot {
	// Tiers 始终填三档（5h/week/month）
	snap.Tiers = []WebConsoleQuotaTier{
		{Window: "5h", Limit: p.Limit5h, Used: p.Used5h, ResetAt: string(p.Reset5h)},
		{Window: "week", Limit: p.LimitWeek, Used: p.UsedWeek, ResetAt: string(p.ResetWeek)},
		{Window: "month", Limit: p.LimitMonth, Used: p.UsedMonth, ResetAt: string(p.ResetMonth)},
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
		{p.Used5h, p.Limit5h, string(p.Reset5h)},
		{p.UsedWeek, p.LimitWeek, string(p.ResetWeek)},
		{p.UsedMonth, p.LimitMonth, string(p.ResetMonth)},
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
		if t, ok := parseWebConsoleQuotaResetAt(string(p.Window5h.ResetAt)); ok && t.After(now) {
			cands = append(cands, t)
		}
	}
	if p.Window7d != nil && p.Window7d.Limit > 0 && p.Window7d.Remaining <= 0 {
		if t, ok := parseWebConsoleQuotaResetAt(string(p.Window7d.ResetAt)); ok && t.After(now) {
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
		NearestGrantExpiry:          string(p.NearestGrantExpiry),
	}
	if p.Window5h != nil {
		out.Window5h = &WebConsoleQuotaTier{
			Window:  "5h",
			Limit:   p.Window5h.Limit,
			Used:    p.Window5h.Limit - p.Window5h.Remaining,
			ResetAt: string(p.Window5h.ResetAt),
		}
	}
	if p.Window7d != nil {
		out.Window7d = &WebConsoleQuotaTier{
			Window:  "7d",
			Limit:   p.Window7d.Limit,
			Used:    p.Window7d.Limit - p.Window7d.Remaining,
			ResetAt: string(p.Window7d.ResetAt),
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
				ResetAt: string(sel.Window5h.ResetAt),
			})
		}
		if sel.Window7d != nil {
			tiers = append(tiers, WebConsoleQuotaTier{
				Window:  "7d",
				Limit:   sel.Window7d.Limit,
				Used:    sel.Window7d.Limit - sel.Window7d.Remaining,
				ResetAt: string(sel.Window7d.ResetAt),
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

// ---------------------------------------------------------------------------
// iflytek（讯飞星辰 MaaS，方案 §3.2 ④⑤）
// ---------------------------------------------------------------------------

// webConsoleIflytekPick 取值规则：优先返回顶层字段 top（非 nil 即视为有值，含 0）；
// 顶层缺失时回退到嵌套 codingPlanUsageDTO 的字段 dto；两者皆缺失返回 0。
// 用指针区分"字段缺失"与"值为 0"，避免把真实 0 误判为未提供而错误回退。
func webConsoleIflytekPick(top *float64, dto *float64) float64 {
	if top != nil {
		return *top
	}
	if dto != nil {
		return *dto
	}
	return 0
}

// buildIflytekSnapshot 解析讯飞星辰 MaaS worker 回传的 data 为统一快照。
//
// 关于 Exhausted：本函数恒置 Exhausted=false。原因（见方案 §3.2 ⑤，实测 §2.6）：
// 讯飞生产账号（如 982/984 的 flash-free 免费模型）余额/套餐全为 0 但服务正常可用，
// 余额 0 不代表耗尽。判定"是否仍有免费额度"需要采集 free-quota，而本轮明确不做。
// 因此首版只展示、不停调：不得改成"余额 0 即耗尽"，否则会误杀正常账号。后续若要接
// 自动停调，须先采集免费额度或明确的欠费字段（delinquent>0 可作为强信号），并按新裁定
// 另行设计。
func buildIflytekSnapshot(snap *WebConsoleQuotaSnapshot, rawData []byte, now time.Time) (*WebConsoleQuotaSnapshot, error) {
	var d webConsoleIflytekData
	// JSON 解析失败返回 error（fail-closed），不 panic。
	if err := json.Unmarshal(rawData, &d); err != nil {
		return nil, err
	}
	snap.AuthState = defaultAuthState(d.AuthState)
	snap.AuthReason = d.AuthReason
	snap.AuthError = d.AuthError
	snap.AccountLabel = d.AccountLabel

	// 余额（指针，0 也保留）；币种固定 CNY。
	bal := d.Balance.Balance
	del := d.Balance.Delinquent
	vb := d.Balance.VirtualBalance
	snap.Balance = &bal
	snap.Delinquent = &del
	snap.VirtualBalance = &vb
	snap.BalanceCurrency = "CNY"

	// Token Plan 席位汇总：累加三项计数 + 保留 by_seat 明细。
	var total, assigned, remaining float64
	seats := make([]WebConsoleQuotaTokenPlanSeat, 0, len(d.TokenPlanQuota.Quotas))
	for _, q := range d.TokenPlanQuota.Quotas {
		total += q.TotalCount
		assigned += q.AssignedCount
		remaining += q.RemainingCount
		seats = append(seats, WebConsoleQuotaTokenPlanSeat{
			SeatType:       q.SeatType,
			SeatTypeName:   q.SeatTypeName,
			TotalCount:     q.TotalCount,
			AssignedCount:  q.AssignedCount,
			RemainingCount: q.RemainingCount,
		})
	}
	snap.TokenPlan = &WebConsoleQuotaTokenPlan{
		TotalCount:     total,
		AssignedCount:  assigned,
		RemainingCount: remaining,
		BySeat:         seats,
	}

	// Coding Plan 行 → 三个滚动窗口 tier；limit 为 0 的窗口不输出。
	// 仅取 rows[0]（账号级绑定，快照共享兄弟账号，见方案 §4.2）。
	if len(d.CodingPlanList.Rows) > 0 {
		row := d.CodingPlanList.Rows[0]
		var dto *webConsoleIflytekUsage
		if row.CodingPlanUsageDTO != nil {
			dto = row.CodingPlanUsageDTO
		}
		var tiers []WebConsoleQuotaTier

		// 5h 窗口：limit>0 才输出，used 取 rp5hUsage
		if l5 := webConsoleIflytekPick(row.RP5hLimit, nilIfFloat(dto, func(u webConsoleIflytekUsage) *float64 { return u.RP5hLimit })); l5 > 0 {
			tiers = append(tiers, WebConsoleQuotaTier{
				Window: "5h",
				Limit:  l5,
				Used:   webConsoleIflytekPick(row.RP5hUsage, nilIfFloat(dto, func(u webConsoleIflytekUsage) *float64 { return u.RP5hUsage })),
			})
		}
		// week 窗口：limit>0 才输出，used 取 rpwUsage
		if lw := webConsoleIflytekPick(row.RPwLimit, nilIfFloat(dto, func(u webConsoleIflytekUsage) *float64 { return u.RPwLimit })); lw > 0 {
			tiers = append(tiers, WebConsoleQuotaTier{
				Window: "week",
				Limit:  lw,
				Used:   webConsoleIflytekPick(row.RPwUsage, nilIfFloat(dto, func(u webConsoleIflytekUsage) *float64 { return u.RPwUsage })),
			})
		}
		// month（package）窗口：limit>0 才输出；
		// used 规则：packageUsage>0 用 packageUsage，否则用 max(packageLimit-packageLeft, 0)
		if pl := webConsoleIflytekPick(row.PackageLimit, nilIfFloat(dto, func(u webConsoleIflytekUsage) *float64 { return u.PackageLimit })); pl > 0 {
			pu := webConsoleIflytekPick(row.PackageUsage, nilIfFloat(dto, func(u webConsoleIflytekUsage) *float64 { return u.PackageUsage }))
			if pu <= 0 {
				left := webConsoleIflytekPick(row.PackageLeft, nilIfFloat(dto, func(u webConsoleIflytekUsage) *float64 { return u.PackageLeft }))
				used := pl - left
				if used < 0 {
					used = 0
				}
				pu = used
			}
			tiers = append(tiers, WebConsoleQuotaTier{
				Window: "month",
				Limit:  pl,
				Used:   pu,
			})
		}

		if len(tiers) > 0 {
			snap.CodingPlanTiers = tiers
		}
	}

	// 恒不耗尽（见函数顶部注释）。
	snap.Exhausted = false
	return snap, nil
}

// nilIfFloat 当 u 非 nil 时返回其字段访问器结果，否则返回 nil。
func nilIfFloat(u *webConsoleIflytekUsage, get func(webConsoleIflytekUsage) *float64) *float64 {
	if u == nil {
		return nil
	}
	return get(*u)
}
