package service

// F3 403 content-policy 恢复链（方案 §3.3 / 派发单 C1-b②）：paused-until 解析器、
// §7.1 事件名契约、恢复探针响应类别、窄面仓储接口、转交目标载体与 F3 恢复记录读取助手。
//
// 接口位置约定（承 C1-a-r3 / C1-b① 先例）：服务侧一律经窄面接口 http403RecoveryRepo
// + 一次性类型断言消费 accountRepository 具体类型上的 F3 原语；新 repo 查询/原语定义在
// accountRepository 具体类型上，不加入 AccountRepository 大接口。repo 侧常量（HTTP403*
// /SchedStateRevision*）在本包按 tokenharbor 会话键先例逐字镜像，生产代码不跨包 import
// repository（避免 import 环），仅字面值契约一致。

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---- F3 extra 键族字面值镜像（与 repository 包逐字一致，禁止改值） ----

const (
	// http403RecoveryExtraKey 是 F3 恢复链独占的状态键名。
	http403RecoveryExtraKey = "http_403_recovery"
	// http403GenerationCounterExtraKey 是独立持久代际计数键（不随 http_403_recovery 删除而删除）。
	http403GenerationCounterExtraKey = "http_403_gen_counter"
	// http403RecoveryOwnerF3 是 F3 链固定所有者标识，CAS 条件之一。
	http403RecoveryOwnerF3 = "f3_lifecycle"
	// http403RecoveryTempUnschedulableReasonPrefix 是 F3 写入 temp_unschedulable 时给
	// reason 加的所有权标记前缀；CAS 清理/转交仅在该前缀命中时清除 temp_unschedulable_until。
	http403RecoveryTempUnschedulableReasonPrefix = "http_403_recovery_f3: "
	// schedStateRevisionExtraKey 是账号调度状态 revision 计数键（跨链并发检测）。
	schedStateRevisionExtraKey = "sched_state_revision"
)

// ---- §7.1 F3 恢复链可验收事件名（逐字锁定，R13-F4 / R22-S2） ----

const (
	eventCNQuota403PausedUntilParsed          = "cn_quota_403_paused_until_parsed"
	eventCNQuota403PausedUntilParseFailed     = "cn_quota_403_paused_until_parse_failed"
	eventCNQuota403RecoveryProbeStarted       = "cn_quota_403_recovery_probe_started"
	eventCNQuota403RecoveryProbeResult        = "cn_quota_403_recovery_probe_result"
	eventCNQuota403RecoveryCASConflict        = "cn_quota_403_recovery_cas_conflict"
	eventCNQuota403RecoveryFastRetryExhausted = "cn_quota_403_recovery_fast_retry_exhausted"
)

// f3ProbeResponseClass 是恢复探针派生的响应类别（比 §7.1 事件 response_class 更细，
// 供分派逻辑区分 401/402/429/仍403 等）。
type f3ProbeResponseClass string

const (
	f3ProbeRecovered2xx f3ProbeResponseClass = "recovered_2xx"
	f3ProbeStill403     f3ProbeResponseClass = "still_403"
	f3ProbeAuth401      f3ProbeResponseClass = "auth_401"
	f3ProbeQuota402     f3ProbeResponseClass = "quota_402"
	f3ProbeRate429      f3ProbeResponseClass = "rate_429"
	// f3ProbeTransient 瞬时传输错误/5xx：受控快速重探，达上限转既有 sweep 周期。
	f3ProbeTransient f3ProbeResponseClass = "transient_retry"
	// f3ProbeUnclassified 未列举响应（R18-F3）：失败关闭，不清 error、保留下轮候选。
	f3ProbeUnclassified f3ProbeResponseClass = "unclassified"
)

// f3EventResponseClass §7.1 cn_quota_403_recovery_probe_result 事件的 response_class 枚举
// （方案锁定逐字：recovered_2xx / still_403_cooldown / handed_off_401_402_429 /
// transient_retry / fast_retry_exhausted_to_sweep）。
const (
	f3EventResponseClassRecovered2xx       = "recovered_2xx"
	f3EventResponseClassStill403Cooldown   = "still_403_cooldown"
	f3EventResponseClassHandedOff401402429 = "handed_off_401_402_429"
	f3EventResponseClassTransientRetry     = "transient_retry"
	f3EventResponseClassFastRetryExhausted = "fast_retry_exhausted_to_sweep"
)

// f3ProbeResult 是 runHTTP403RecoveryProbe 的返回值：响应类别 + HTTP 状态码 + 响应体
// （仍 403 时需从响应体解析 paused until）+ 传输错误。
type f3ProbeResult struct {
	Class  f3ProbeResponseClass
	Status int
	Body   []byte
	Err    error
}

// HTTP403TransitionTarget 描述 F3 403 恢复链向其他链（401/402/429）原子转交时要写入的
// 新暂停状态效果（承载自 repository 包，repository 已 import service，故类型归属 service
// 包；单测/生产均经此结构传参，避免 service 反向 import repository 形成环）。字段语义：
//   - Status / ErrorMessage / Schedulable：无条件写入（新链暂停状态）；
//   - TempUnschedulableUntil / TempUnschedulableReason：非 nil 时写入；为 nil 时若当前
//     until 为 F3 拥有（reason 前缀命中）则清除，否则保留（R15-F2 所有权感知）。
type HTTP403TransitionTarget struct {
	Status                  string
	ErrorMessage            string
	Schedulable             bool
	TempUnschedulableUntil  *time.Time
	TempUnschedulableReason *string
}

// http403RecoveryRepo 是 F3 403 恢复链全部存储原语的窄面（定义于 service 包）：服务侧
// 对 accountRepo 做一次性类型断言消费，断言失败静默跳过 F3 分支（与 tempUnschedulableShortener
// 同构）。不扩大 AccountRepository 大接口面。
type http403RecoveryRepo interface {
	// AllocHTTP403Generation 在同一语句内自增持久代际计数键并 RETURNING（禁止读-改-写）。
	AllocHTTP403Generation(ctx context.Context, accountID int64) (int64, error)
	// Mark403PausedWithRecovery 原子完成 F3 首次写入（复刻 SetError 字段效果 + 合并恢复键 + 自增 revision）。
	Mark403PausedWithRecovery(ctx context.Context, accountID int64, until time.Time, generation int64, reason string, errorMsg string, tempUnschedulable *time.Time) error
	// ClearHTTP403RecoveryIfOwned CAS 条件清理（generation + owner=F3 + 键内 state_revision=全局）。
	ClearHTTP403RecoveryIfOwned(ctx context.Context, accountID int64, generation int64) (bool, error)
	// TransitionHTTP403RecoveryTo 原子转交（同一单条 UPDATE 内写入新链状态 + 移除 F3 元数据 + 自增 revision）。
	TransitionHTTP403RecoveryTo(ctx context.Context, accountID int64, generation int64, target HTTP403TransitionTarget) (bool, error)
	// ListHTTP403RecoveryDueAccounts F3 候选查询（持有恢复键且 until<=now，不受 schedulable/error 过滤）。
	ListHTTP403RecoveryDueAccounts(ctx context.Context, now time.Time, limit int) ([]*Account, error)
	// UpdateHTTP403RecoveryUntil 同键改 until/reason（CAS 三条件），用于仍 403 续等/冷却轮。
	UpdateHTTP403RecoveryUntil(ctx context.Context, accountID int64, generation int64, newUntil time.Time, reason string) (bool, error)
	// RemoveHTTP403RecoveryRecord stale 清理：仅删键（不动 status/error/temp_unschedulable），generation 匹配。
	RemoveHTTP403RecoveryRecord(ctx context.Context, accountID int64, generation int64) (bool, error)
	// ListLegacyHTTP403ErrorAccounts 存量候选（status=error 且 error_message 以 403 前缀起、无 F3 键）。
	ListLegacyHTTP403ErrorAccounts(ctx context.Context, limit int) ([]*Account, error)
}

// http403RecoveryRecord 是 extra[http_403_recovery] 的内存形态（读取自账号快照）。
type http403RecoveryRecord struct {
	Until         string `json:"until"`
	Generation    int64  `json:"generation"`
	Reason        string `json:"reason"`
	Owner         string `json:"owner"`
	StateRevision int64  `json:"state_revision"`
}

// http403RecoveryFromExtra 从账号快照 extra 读取 F3 恢复记录；缺失/损坏/until 为空返回 false。
func http403RecoveryFromExtra(extra map[string]any) (*http403RecoveryRecord, bool) {
	raw, ok := extra[http403RecoveryExtraKey]
	if !ok || raw == nil {
		return nil, false
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var rec http403RecoveryRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, false
	}
	if rec.Until == "" {
		return nil, false
	}
	return &rec, true
}

// http403GlobalStateRevision 读取账号快照 extra 内的全局 sched_state_revision（无则 0）。
func http403GlobalStateRevision(extra map[string]any) int64 {
	v, ok := extra[schedStateRevisionExtraKey]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return i
		}
	}
	return 0
}

// ParseHTTP403PausedUntil 解析上游 403 报文中的 paused-until 时间（方案 §3.3 / 派发单
// 第 4 条）：input 拼接 upstreamMsg 与 responseBody（JSON 提取 message 字段后拼接，承接
// buildForbiddenErrorMessage 的信息面）。大小写不敏感，要求 paused 与 until 同现，捕获
// until 后的时间片段；尝试多格式解析（RFC3339 / `2006-01-02 15:04:05` / RFC1123 /
// `2006-01-02` 按 UTC 当日末）。全部失败 → 返回 not-found（写点走冷却循环、存量走冷却循环+告警）。
//
// 已知生产样例 `Access forbidden (403): API access from your region is not available` 无
// until → 必须落入 not-found 分支。
func ParseHTTP403PausedUntil(upstreamMsg string, responseBody []byte) (time.Time, bool) {
	text := strings.TrimSpace(upstreamMsg)
	if len(responseBody) > 0 {
		var bodyObj map[string]any
		if json.Unmarshal(responseBody, &bodyObj) == nil {
			if m, ok := bodyObj["message"]; ok {
				if s, ok := m.(string); ok && strings.TrimSpace(s) != "" {
					if text != "" {
						text += " "
					}
					text += strings.TrimSpace(s)
				}
			}
		}
	}
	return parsePausedUntilFromText(text)
}

// f3PausedUntilLayouts 是 until 后时间片段的候选解析格式（按严格度排序），re 预编译。
var f3PausedUntilLayouts = []struct {
	layout   string
	re       *regexp.Regexp
	endOfDay bool
}{
	{layout: time.RFC3339, re: regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)},
	{layout: "2006-01-02 15:04:05", re: regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)},
	{layout: time.RFC1123, re: regexp.MustCompile(`[A-Za-z]{3}, \d{2} [A-Za-z]{3} \d{4} \d{2}:\d{2}:\d{2} [A-Za-z]{3,4}`)},
	{layout: time.RFC1123Z, re: regexp.MustCompile(`[A-Za-z]{3}, \d{2} [A-Za-z]{3} \d{4} \d{2}:\d{2}:\d{2} [+-]\d{4}`)},
	{layout: "2006-01-02", re: regexp.MustCompile(`\d{4}-\d{2}-\d{2}`), endOfDay: true},
}

// parsePausedUntilFromText 在拼合文本中确认 paused 与 until 同现（大小写不敏感），并在
// 最后一个 until 之后搜索候选时间片段尝试解析。
func parsePausedUntilFromText(text string) (time.Time, bool) {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "paused") || !strings.Contains(lower, "until") {
		return time.Time{}, false
	}
	// 仅在 until 之后搜索时间片段（paused until <时间> 结构）。
	idx := strings.LastIndex(lower, "until")
	if idx < 0 {
		return time.Time{}, false
	}
	remainder := text[idx+len("until"):]
	for _, cand := range f3PausedUntilLayouts {
		m := cand.re.FindString(remainder)
		if m == "" {
			continue
		}
		t, err := time.Parse(cand.layout, strings.TrimSpace(m))
		if err != nil {
			continue
		}
		if cand.endOfDay {
			t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC)
		}
		return t, true
	}
	return time.Time{}, false
}

// classifyF3ProbeStatus 将恢复探针 HTTP 状态码分类为 F3 响应类别（未列举 → 失败关闭）。
func classifyF3ProbeStatus(status int) f3ProbeResponseClass {
	switch {
	case status >= 200 && status < 300:
		return f3ProbeRecovered2xx
	case status == http.StatusUnauthorized:
		return f3ProbeAuth401
	case status == http.StatusPaymentRequired:
		return f3ProbeQuota402
	case status == http.StatusForbidden:
		return f3ProbeStill403
	case status == http.StatusTooManyRequests:
		return f3ProbeRate429
	case status >= 500 && status < 600:
		return f3ProbeTransient
	default:
		return f3ProbeUnclassified
	}
}

// f3LogWarnOnce 在能力缺失降级时输出一次 Warn（调用方保证一次性断言，这里仅格式化）。
func f3CapabilityWarn(repo interface{}, detail string) {
	slog.Warn("cn_quota_403_recovery_repo_unsupported",
		"reason", "accountRepo does not implement http403RecoveryRepo; F3 branch skipped (capability degradation, not error swallowing)",
		"detail", detail,
		"has_repo", repo != nil,
	)
}

// errF3NoProbeTransport 是恢复探针缺失传输层时的失败关闭错误（归类为瞬时，受控重探）。
var errF3NoProbeTransport = errors.New("f3 recovery probe: transport not configured")
