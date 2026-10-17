package service

// 本文件实现派发单 C2：纯直连探测原语 + 无流量渠道抽样探测调度器 + 其 probe_state 读写。
//
// 设计约束（严格照派发单 C2，防过度设计）：
//   - 直连原语 ProbeAccountDirect：入参 = 一个 Account + 一个 model；行为 = 读账号凭据
//     （resolveCredentialAccount 解影射 + GetOpenAIBaseURL / GetOpenAIProtocolAPIKey /
//     GetAnthropicProtocolBaseURL / GetCredential 等既有 getter）+ 按协议族自建最小请求
//     （OpenAI 兼容 → /v1/chat/completions；anthropic 型 → /v1/messages；gemini apikey →
//     generateContent）+ 直连上游 HTTP + 读响应 → 返回 ok + detail。
//     硬超时 5 分钟。绝不调用 AccountTestService 的 test*Connection 方法，绝不写账号副作用。
//   - 探测调度器 ChannelMonitorProbeWorker：复用 tryAcquireSingletonLeaderLock（独立锁键
//     channel_monitor:probe:leader）；每轮 tick：DeriveChannelMonitorCatalog 取目录 → 过滤
//     候选 = 目录 ∩ 无新鲜业务观测（业务 15 分钟）∩ next_due 到期 → 每渠道抽样一个在售账号
//     （候选人集按账号 ID 升序取首个可直连探测的账号；渠道无任可探测账号则跳过，不写
//     probe_state 行 → 展示层"无数据"）→ 直连探测 → UPSERT channel_monitor_probe_state →
//     next_due：ok → 30 分钟；!ok → 2 小时。每 tick ≤ max_per_tick。无状态机/无 CAS/无
//     reaper/无收据表。
//   - config probe 段：Enabled 默认 false（零探测）；MaxPerTick 默认 2。

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// ---------- 常量 ----------

const (
	// channelMonitorProbeLeaderKey 是探测 worker 的独立 leader 锁键（与聚合器锁键不同）。
	channelMonitorProbeLeaderKey = "channel_monitor:probe:leader"
	// probeLeaderLockMinTTL 是 leader 锁 TTL 的下限（min 兜底原 15 分钟常量语义）。
	// 真实 TTL 由 probeLeaderLockTTLFor 按 cfg.Probe.MaxPerTick 推导（见 B5）。
	probeLeaderLockMinTTL = 15 * time.Minute
	// probeTickInterval 探测 worker 的轮询间隔（与冷却窗口相比足够稀疏）。
	probeTickInterval = time.Minute
	// probeHardTimeout 单条直连探测的硬超时（派发单 C2 规定 5 分钟）。
	probeHardTimeout = 5 * time.Minute
	// probeCooldownOK 探测成功后的冷却窗口（30 分钟）。
	probeCooldownOK = 30 * time.Minute
	// probeCooldownFail 探测失败后的冷却窗口（2 小时）。
	probeCooldownFail = 2 * time.Hour
	// probeBusinessFreshnessWindow 判定"新鲜业务观测"的窗口（15 分钟）。
	probeBusinessFreshnessWindow = 15 * time.Minute

	// defaultGeminiBaseURL 是 gemini apikey 账号无自定义 base_url 时的默认端点。
	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"

	// probeSSELineLimit 是探测流式读取单行 SSE 的上限（1 MiB），与仓库既有响应读取上限
	// 量级一致（FIX-2）；上游持续发送无换行数据会被该上限截断，避免无界内存占用。超限
	// 视为探测失败（ok=false，detail 记 sse line limit exceeded），绝不跳行后把 2xx 记成功。
	probeSSELineLimit = 1 << 20
)

// ---------- ProbeState ----------

// ProbeState 是 channel_monitor_probe_state 表中一个 (platform, model, group) 三元组的最近探测结果。
type ProbeState struct {
	Platform      string
	Model         string
	GroupID       int64
	LastProbeAt   time.Time
	LastOK        bool
	LastDetail    string
	LastAccountID int64
	NextDue       time.Time
	// FirstTokenMs / TotalMs 是流式探测计时（B3，方案审锁定定义）：
	//   FirstTokenMs：请求发出到首个非空文本内容增量的毫秒数；仅有起始事件无文本 → nil。
	//   TotalMs：请求发出到流结束/连接关闭的毫秒数。
	// 二者均可空（历史行或!ok 行可能为 nil）。
	FirstTokenMs *int
	TotalMs      *int
}

// ---------- probeStore ----------

// probeStore 是探测调度器的数据访问窄面：probe_state 读写 + 新鲜业务渠道集合查询。
// 用接口隔离，便于单测以内存 fake 替换。生产实现为 postgresProbeStore（*sql.DB 支撑）。
type probeStore interface {
	// GetProbeState 读取指定三元组的最近探测状态；无记录返回 (nil, nil)。
	GetProbeState(ctx context.Context, platform, model string, groupID int64) (*ProbeState, error)
	// UpsertProbeState 幂等写入最近探测状态。
	UpsertProbeState(ctx context.Context, st *ProbeState) error
	// FreshBusinessChannels 返回 since 之后有业务观测的 (platform, model, group) 集合。
	// 数据来源为 channel_monitor_v2_metrics_1m（被动聚合的分钟级业务流量）。
	FreshBusinessChannels(ctx context.Context, since time.Time) (map[ChannelMonitorCatalogKey]struct{}, error)
}

// postgresProbeStore 是 probeStore 的 PostgreSQL 实现。
type postgresProbeStore struct {
	db *sql.DB
}

// NewChannelMonitorProbeStore 构造探测状态存储（PostgreSQL）。
func NewChannelMonitorProbeStore(db *sql.DB) probeStore {
	return &postgresProbeStore{db: db}
}

func (s *postgresProbeStore) GetProbeState(ctx context.Context, platform, model string, groupID int64) (*ProbeState, error) {
	const q = `SELECT last_probe_at, last_ok, last_detail, last_account_id, next_due, first_token_ms, total_ms
		FROM channel_monitor_probe_state WHERE platform = $1 AND model = $2 AND group_id = $3`
	row := s.db.QueryRowContext(ctx, q, platform, model, groupID)
	st := &ProbeState{Platform: platform, Model: model, GroupID: groupID}
	var lastProbeAt, nextDue sql.NullTime
	var lastOK sql.NullBool
	var lastDetail sql.NullString
	var lastAccountID sql.NullInt64
	var firstTokenMs, totalMs sql.NullInt64
	if err := row.Scan(&lastProbeAt, &lastOK, &lastDetail, &lastAccountID, &nextDue, &firstTokenMs, &totalMs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if lastProbeAt.Valid {
		st.LastProbeAt = lastProbeAt.Time
	}
	if lastOK.Valid {
		st.LastOK = lastOK.Bool
	}
	if lastDetail.Valid {
		st.LastDetail = lastDetail.String
	}
	if lastAccountID.Valid {
		st.LastAccountID = lastAccountID.Int64
	}
	if nextDue.Valid {
		st.NextDue = nextDue.Time
	}
	if firstTokenMs.Valid {
		v := int(firstTokenMs.Int64)
		st.FirstTokenMs = &v
	}
	if totalMs.Valid {
		v := int(totalMs.Int64)
		st.TotalMs = &v
	}
	return st, nil
}

func (s *postgresProbeStore) UpsertProbeState(ctx context.Context, st *ProbeState) error {
	const q = `INSERT INTO channel_monitor_probe_state
		(platform, model, group_id, last_probe_at, last_ok, last_detail, last_account_id, next_due, first_token_ms, total_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (platform, model, group_id) DO UPDATE SET
			last_probe_at = EXCLUDED.last_probe_at,
			last_ok = EXCLUDED.last_ok,
			last_detail = EXCLUDED.last_detail,
			last_account_id = EXCLUDED.last_account_id,
			next_due = EXCLUDED.next_due,
			first_token_ms = EXCLUDED.first_token_ms,
			total_ms = EXCLUDED.total_ms`
	var firstTokenMs, totalMs sql.NullInt64
	if st.FirstTokenMs != nil {
		firstTokenMs = sql.NullInt64{Int64: int64(*st.FirstTokenMs), Valid: true}
	}
	if st.TotalMs != nil {
		totalMs = sql.NullInt64{Int64: int64(*st.TotalMs), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, q,
		st.Platform, st.Model, st.GroupID, st.LastProbeAt, st.LastOK, st.LastDetail, st.LastAccountID, st.NextDue,
		firstTokenMs, totalMs)
	return err
}

func (s *postgresProbeStore) FreshBusinessChannels(ctx context.Context, since time.Time) (map[ChannelMonitorCatalogKey]struct{}, error) {
	const q = `SELECT DISTINCT platform, model, group_id
		FROM channel_monitor_v2_metrics_1m WHERE bucket_start >= $1`
	rows, err := s.db.QueryContext(ctx, q, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[ChannelMonitorCatalogKey]struct{})
	for rows.Next() {
		var k ChannelMonitorCatalogKey
		if err := rows.Scan(&k.Platform, &k.Model, &k.GroupID); err != nil {
			return nil, err
		}
		out[k] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------- 纯直连探测原语 ----------

// probeReqSpec 是直连原语构造的最简上游请求描述。
type probeReqSpec struct {
	method  string
	url     string
	headers map[string]string
	body    []byte
}

// defaultProbeHTTPClient 是直连原语默认 HTTP 客户端（硬超时 5 分钟，无代理/TLS 透传）。
var defaultProbeHTTPClient = &http.Client{Timeout: probeHardTimeout}

// ProbeAccountDirect 是 C2 的纯直连探测原语。
//
// 入参：
//   - accountRepo：仅用于解析影子账号（resolveCredentialAccount，只读 GetByID；普通账号零调用）；
//     绝不写账号副作用（不调用 SetError / SetTempUnschedulable / SetRateLimited /
//     UpdateExtra / ClearError 等）。
//   - account：被探测账号（或影子账号；会被解影射为母账号）。
//   - model：目录中的客户端请求模型名（已为 model_mapping 的键）。
//   - httpClient：直连使用的 HTTP 客户端；nil 时使用默认 5 分钟超时客户端（便于单测注入假上游）。
//
// 返回：
//   - ok：上游 2xx 视为可达（权威成功）；非 2xx / 网络错误视为权威失败（last_ok=false 合法）；
//   - detail：状态/错误摘要；
//   - unsupported：非权威标记——账号不可直连探测（协议族不支持或凭据缺失）。调用方遇
//     unsupported 时**绝不**写终态失败行（保持"无数据"语义），因为这是"无法探测"而非"渠道故障"。
//
// 协议族：OpenAI 兼容（/v1/chat/completions）、anthropic（/v1/messages）、gemini apikey
// （generateContent）。其余（OAuth 令牌刷新依赖、agent identity、vertex、bedrock 等）落
// unsupported——保守标记，不触碰令牌刷新/账号写路径。
func ProbeAccountDirect(
	ctx context.Context,
	accountRepo AccountRepository,
	account *Account,
	model string,
	httpClient *http.Client,
) (ok bool, detail string, unsupported bool, firstTokenMs *int, totalMs *int) {
	if account == nil {
		return false, "nil account", false, nil, nil
	}
	cred, err := resolveCredentialAccount(ctx, accountRepo, account)
	if err != nil {
		// 解析失败为权威失败（账号实体不可达），非 unsupported。
		return false, "resolve credential account: " + err.Error(), false, nil, nil
	}
	spec, err := buildProbeRequestSpec(cred, model)
	if err != nil {
		// 账号不可直连探测（协议族不支持或凭据缺失）：非权威，绝不落终态 last_ok=false。
		return false, err.Error(), true, nil, nil
	}
	if httpClient == nil {
		httpClient = defaultProbeHTTPClient
	}
	req, err := http.NewRequestWithContext(ctx, spec.method, spec.url, bytes.NewReader(spec.body))
	if err != nil {
		return false, "build request: " + err.Error(), false, nil, nil
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range spec.headers {
		req.Header.Set(k, v)
	}
	// 计时基准：请求发出（Do 发送前）。TTFT 终点 = 首个非空文本增量；total 终点 = 流结束。
	start := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, "upstream request failed: " + err.Error(), false, nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	// 流式读取响应并测量 TTFT / total（见 B3 方案审锁定定义）。
	family := probeFamilyOf(cred)
	fTok, tot, readErr := probeStreamRead(resp.Body, family, start)
	if readErr != nil {
		// 读取失败（连接中断 / 读超时 / SSE 单行超限）：失败关闭（FIX-1/FIX-2），
		// 绝不以 ok=true 写入。detail 标识 stream read error（含 sse line limit exceeded）。
		return false, "stream read error: " + readErr.Error(), false, fTok, tot
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, fmt.Sprintf("ok status=%d", resp.StatusCode), false, fTok, tot
	}
	return false, fmt.Sprintf("upstream status=%d", resp.StatusCode), false, fTok, tot
}

// isAnthropicProbeTarget 是"账号是否走 anthropic 直连探测（/v1/messages）"的单一事实源，
// 供 canDirectProbe 与 buildProbeRequestSpec 的 switch 共用，禁止在别处再写一份等价判定
// （否则两处会随协议族扩展而悄然分叉，本次 anthropic 永不被探测事故的根因）。
//
// 含 Platform == PlatformAnthropic：原生 anthropic 平台账号 GetAPIProtocol 默认回落
// chat_completions（见 account.go GetAPIProtocol），导致 IsAnthropicProtocol 为假、长期
// 无法被直连探测——这是本次生产事故的直接根因。
func isAnthropicProbeTarget(acc *Account) bool {
	return acc != nil && (acc.IsAnthropicProtocol() || acc.Platform == PlatformAnthropic)
}

// canDirectProbe 是"账号是否可被直连探测"的单一事实源：与原语 buildProbeRequestSpec 的
// switch 完全同条件。候选账号选取与原语构造都通过本判定把关，禁止在别处再写一份等价条件，
// 否则两处会随协议族扩展而悄然分叉（本次生产事故的根因）。
//
// 可探测协议族：anthropic 原生协议、OpenAI 兼容（含 grok/CN/other/codebuddy/muse/web）、
// gemini 且仅 apikey/upstream 型（gemini OAuth 依赖令牌刷新，不直连）。
func canDirectProbe(acc *Account) bool {
	// cursor 必须显式排除，尽管它 IsOpenAICompatible()==true：本探针按协议族构造请求，
	// cursor 命中的是 OpenAI Chat Completions 形状，而 cursor 上游是 Cursor **原生**端点
	//（/aiserver.v1.AIServiceService/*，protobuf-JSON + 私有 SSE 帧）。把 OpenAI 形状的
	// 探针打到 cursor 端点必然被拒，会把健康账号误标 last_ok=false。cursor 的可用性
	// 由 cursor 自身的探针口径负责（不在本探针范围）。
	if acc != nil && acc.Platform == PlatformCursor {
		return false
	}
	return acc != nil && (isAnthropicProbeTarget(acc) ||
		acc.IsOpenAICompatible() ||
		(acc.IsGemini() && isAPIKeyOrUpstream(acc)))
}

// buildProbeRequestSpec 按协议族构造最简直连请求。unsupported 时返回 error（由原语转
// ok=false + unsupported=true 非权威失败）。可探测性先由 canDirectProbe 把关，switch 仅
// 负责按协议族构造请求，二者共享同一条件。
func buildProbeRequestSpec(acc *Account, model string) (*probeReqSpec, error) {
	if !canDirectProbe(acc) {
		return nil, fmt.Errorf("unsupported account platform=%q type=%q for direct probe", acc.Platform, acc.Type)
	}
	switch {
	case isAnthropicProbeTarget(acc):
		return buildAnthropicMessagesSpec(acc, model)
	case acc.IsOpenAICompatible():
		return buildOpenAIChatCompletionsSpec(acc, model)
	case acc.IsGemini() && isAPIKeyOrUpstream(acc):
		return buildGeminiGenerateContentSpec(acc, model)
	default:
		// 不可达：canDirectProbe 已确保覆盖上述三种可探测协议族。
		return nil, fmt.Errorf("unsupported account platform=%q type=%q for direct probe", acc.Platform, acc.Type)
	}
}

func isAPIKeyOrUpstream(acc *Account) bool {
	return acc.Type == AccountTypeAPIKey || acc.Type == AccountTypeUpstream
}

// buildOpenAIChatCompletionsSpec 构造 OpenAI 兼容 chat/completions 最简请求。
func buildOpenAIChatCompletionsSpec(acc *Account, model string) (*probeReqSpec, error) {
	base := strings.TrimSpace(acc.GetOpenAIBaseURL())
	key := strings.TrimSpace(acc.GetOpenAIProtocolAPIKey())
	if acc.IsGrok() {
		// GetOpenAIBaseURL / GetOpenAIProtocolAPIKey 对 grok 返回空，单独取 grok 端点与 api_key。
		if base == "" {
			base = strings.TrimSpace(acc.GetGrokBaseURL())
		}
		if key == "" {
			key = strings.TrimSpace(acc.GetCredential("api_key"))
		}
	}
	if base == "" {
		return nil, fmt.Errorf("unsupported: no upstream base url (platform=%s)", acc.Platform)
	}
	if key == "" {
		return nil, fmt.Errorf("unsupported: no api key (platform=%s)", acc.Platform)
	}
	// 复用业务同源版本感知 helper：base 已含版本段（如 /v1、/v4）时只追加相对路径，
	// 避免裸拼 "/v1/chat/completions" 造成 /v1/v1 双段 404（对齐 openai_endpoint_url.go）。
	u := buildOpenAIEndpointURL(base, "/v1/chat/completions")
	m := acc.GetMappedModel(model)
	if strings.TrimSpace(m) == "" {
		m = model
	}
	return &probeReqSpec{
		method:  http.MethodPost,
		url:     u,
		headers: map[string]string{"Authorization": "Bearer " + key},
		body: probeMustJSON(map[string]any{
			"model":      m,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
			"max_tokens": 5,
			"stream":     true,
		}),
	}, nil
}

// buildAnthropicMessagesSpec 构造 anthropic /v1/messages 最简请求（apikey 或 oauth）。
func buildAnthropicMessagesSpec(acc *Account, model string) (*probeReqSpec, error) {
	h := http.Header{}
	var base, key string
	if isAPIKeyOrUpstream(acc) {
		base = strings.TrimSpace(acc.GetAnthropicProtocolBaseURL())
		if base == "" {
			// 与业务转发同源（gateway_anthropic_passthrough.go:349 用 GetBaseURL）：
			// anthropic 协议账号缺 base_url 时回退到通用 base，避免漏探。
			base = strings.TrimSpace(acc.GetBaseURL())
		}
		key = strings.TrimSpace(acc.GetCredential("api_key"))
		if base == "" {
			return nil, fmt.Errorf("unsupported: no upstream base url for anthropic account")
		}
		if key == "" {
			return nil, fmt.Errorf("unsupported: no api key for anthropic account")
		}
		setAnthropicAPIKeyAuthHeader(h, acc, key, base)
	} else {
		// OAuth（api.anthropic.com）：令牌可能过期，直连仅作最佳努力探活，失败关闭为 !ok。
		base = "https://api.anthropic.com"
		key = strings.TrimSpace(acc.GetCredential("access_token"))
		if key == "" {
			return nil, fmt.Errorf("unsupported: no access token for anthropic oauth account")
		}
		h.Set("Authorization", "Bearer "+key)
	}
	u := strings.TrimRight(base, "/") + "/v1/messages?beta=true"
	m := acc.GetMappedModel(model)
	if strings.TrimSpace(m) == "" {
		m = model
	}
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	for k := range h {
		headers[k] = h.Get(k)
	}
	return &probeReqSpec{
		method:  http.MethodPost,
		url:     u,
		headers: headers,
		body: probeMustJSON(map[string]any{
			"model":      m,
			"max_tokens": 16,
			"stream":     true,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		}),
	}, nil
}

// buildGeminiGenerateContentSpec 构造 gemini apikey generateContent 最简请求（流式 SSE）。
func buildGeminiGenerateContentSpec(acc *Account, model string) (*probeReqSpec, error) {
	key := strings.TrimSpace(acc.GetCredential("api_key"))
	if key == "" {
		return nil, fmt.Errorf("unsupported: no api key for gemini account")
	}
	base := strings.TrimSpace(acc.GetCredential("base_url"))
	if base == "" {
		base = defaultGeminiBaseURL
	}
	m := acc.GetMappedModel(model)
	if strings.TrimSpace(m) == "" {
		m = model
	}
	u := strings.TrimRight(base, "/") + "/v1beta/models/" + url.PathEscape(m) +
		":streamGenerateContent?alt=sse&key=" + url.QueryEscape(key)
	return &probeReqSpec{
		method:  http.MethodPost,
		url:     u,
		headers: map[string]string{},
		body: probeMustJSON(map[string]any{
			"contents": []map[string]any{
				{"role": "user", "parts": []map[string]any{{"text": "hi"}}},
			},
			"generationConfig": map[string]any{"maxOutputTokens": 5},
		}),
	}, nil
}

func probeMustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// 仅静态字面量参与序列化，理论上不可达。
		return []byte("{}")
	}
	return b
}

// probeFamilyOf 将账号归一到流式解析族（"openai" | "anthropic" | "gemini"），
// 与 buildProbeRequestSpec 的可探测协议族一致，驱动首个文本增量的判定规则。
func probeFamilyOf(acc *Account) string {
	switch {
	case isAnthropicProbeTarget(acc):
		return "anthropic"
	case acc.IsOpenAICompatible():
		return "openai"
	case acc.IsGemini() && isAPIKeyOrUpstream(acc):
		return "gemini"
	}
	return ""
}

// probeStreamRead 读取上游流式响应并测量 TTFT / total 计时（B3，方案审锁定定义）。
//
//   - firstTokenMs：从 start（请求发出）到首个非空文本内容增量的毫秒数；
//     仅有起始事件而无文本增量 → nil。
//   - totalMs：从 start 到流结束/连接关闭的毫秒数（读尽整条流以取 total，简单正确）。
//   - err：读取错误（连接中断 / 读超时 / SSE 单行超限）。EOF 不算错误（正常流结束）。
//
// 逐行读 SSE `data:` 帧；非 SSE（纯 JSON 错误体）无 `data:` 前缀 → 不产生文本判定。
// 单行长度以 probeSSELineLimit 为上限（FIX-2）：上游持续发送无换行数据触及上限即停止读取
// 并返回错误，避免无界内存占用；返回的错误交由调用方判 ok=false（detail 记 sse line limit
// exceeded），绝不跳行后把 2xx 记成功。读取失败（非 EOF）同样返回错误，调用方必须失败关闭
// （FIX-1），不得以 ok=true 写入。禁止引入新依赖。
func probeStreamRead(r io.Reader, family string, start time.Time) (firstTokenMs *int, totalMs *int, err error) {
	// NewReaderSize 把内部缓冲设为上限之上，使 ReadSlice 能在单行触及 probeSSELineLimit
	// 字节仍无换行时返回 bufio.ErrBufferFull（即"单行超限"信号），从而有界地检测并失败。
	br := bufio.NewReaderSize(r, probeSSELineLimit+1)
	foundFirst := false
	for {
		line, rerr := br.ReadSlice('\n')
		if len(line) > 0 {
			if probeSSEDataHasText(family, string(line)) && !foundFirst {
				t := int(time.Since(start) / time.Millisecond)
				firstTokenMs = &t
				foundFirst = true
			}
		}
		if rerr != nil {
			if rerr == bufio.ErrBufferFull {
				// 单行超过 probeSSELineLimit 仍无换行：停止读取，按探测失败处理（FIX-2）。
				return firstTokenMs, nil, fmt.Errorf("sse line limit exceeded (limit=%d bytes)", probeSSELineLimit)
			}
			if rerr == io.EOF {
				// 正常流结束：维持现有 ok 语义（EOF 不算错误）。
				break
			}
			// 真实读取错误（连接中断 / 读超时等）：停止读取，按探测失败处理（FIX-1）。
			return firstTokenMs, nil, rerr
		}
	}
	// foundFirst 为假时 firstTokenMs 保持 nil（仅有起始事件无文本增量）。
	t := int(time.Since(start) / time.Millisecond)
	totalMs = &t
	return firstTokenMs, totalMs, nil
}

// probeSSEDataHasText 解析单行 SSE，返回该行是否携带首个非空文本内容增量。
func probeSSEDataHasText(family, line string) bool {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data:") {
		return false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return false
	}
	switch family {
	case "openai":
		return probeOpenAIDataHasText(payload)
	case "anthropic":
		return probeAnthropicDataHasText(payload)
	case "gemini":
		return probeGeminiDataHasText(payload)
	}
	return false
}

func probeOpenAIDataHasText(payload string) bool {
	var m struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return false
	}
	for _, c := range m.Choices {
		if strings.TrimSpace(c.Delta.Content) != "" {
			return true
		}
	}
	return false
}

func probeAnthropicDataHasText(payload string) bool {
	var m struct {
		Type  string `json:"type"`
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return false
	}
	// 仅 content_block_delta 且 text 非空计数；忽略 message_start / ping /
	// content_block_start / content_block_stop 等无文本帧。
	if m.Type == "content_block_delta" && strings.TrimSpace(m.Delta.Text) != "" {
		return true
	}
	return false
}

func probeGeminiDataHasText(payload string) bool {
	var m struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return false
	}
	for _, c := range m.Candidates {
		for _, p := range c.Content.Parts {
			if strings.TrimSpace(p.Text) != "" {
				return true
			}
		}
	}
	return false
}

// ---------- 探测调度器 worker ----------

// ChannelMonitorProbeWorker 是无流量渠道抽样直连探测调度器（派发单 C2）。
//
// 每轮 tick（leader-gated）：
//  1. DeriveChannelMonitorCatalog 取目录（经既有账号/分组仓储）；
//  2. 过滤候选 = 目录 ∩ 无新鲜业务观测（业务 15 分钟）∩ next_due 到期；
//  3. 每渠道抽样一个在售账号（候选人集按账号 ID 升序取首个）；
//  4. 直连探测 → UPSERT probe_state，next_due：ok → 30 分钟；!ok → 2 小时；
//  5. 每 tick ≤ max_per_tick 条。
type ChannelMonitorProbeWorker struct {
	store       probeStore
	accountRepo AccountRepository
	groupRepo   GroupRepository
	leaderLock  LeaderLockCache
	cfg         *config.Config
	httpClient  *http.Client

	// 以下两个字段可注入以支撑单测，生产使用默认值。
	probeFn func(ctx context.Context, accountRepo AccountRepository, account *Account, model string, client *http.Client) (bool, string, bool, *int, *int)
	clock   func() time.Time

	ticker   *time.Ticker
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewChannelMonitorProbeWorker 构造探测 worker。
func NewChannelMonitorProbeWorker(
	store probeStore,
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	leaderLock LeaderLockCache,
	cfg *config.Config,
) *ChannelMonitorProbeWorker {
	return &ChannelMonitorProbeWorker{
		store:       store,
		accountRepo: accountRepo,
		groupRepo:   groupRepo,
		leaderLock:  leaderLock,
		cfg:         cfg,
		httpClient:  &http.Client{Timeout: probeHardTimeout},
		probeFn:     ProbeAccountDirect,
		clock:       time.Now,
	}
}

// Start 启动后台 tick 循环。worker 始终启动；当 probe.enabled=false 时每轮 RunOnce 直接返回（零探测）。
func (w *ChannelMonitorProbeWorker) Start() {
	if w == nil {
		return
	}
	w.stopCh = make(chan struct{})
	w.ticker = time.NewTicker(probeTickInterval)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-w.stopCh:
				return
			case <-w.ticker.C:
				// 轮次失败只记日志（best-effort），不阻断后续轮次。
				if err := w.RunOnce(context.Background()); err != nil {
					slog.Warn("channel_monitor_probe: tick failed", "error", err)
				}
			}
		}
	}()
}

// Stop 停止后台 tick 循环。幂等。
func (w *ChannelMonitorProbeWorker) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		if w.ticker != nil {
			w.ticker.Stop()
		}
		close(w.stopCh)
	})
	w.wg.Wait()
}

// RunOnce 执行一轮探测调度（leader-gated）。probe.enabled=false 时直接返回（零探测）。
func (w *ChannelMonitorProbeWorker) RunOnce(ctx context.Context) error {
	if w.cfg == nil || !w.cfg.Probe.Enabled {
		return nil
	}
	now := w.clock()

	// B5：leader 锁 TTL 由 maxPerTick 推导（最坏轮次运行时长 + 缓冲），min 兜底 15 分钟。
	release, acquired := tryAcquireSingletonLeaderLock(ctx, w.leaderLock, nil, channelMonitorProbeLeaderKey, "channel-monitor-probe", probeLeaderLockTTLFor(w.cfg.Probe.MaxPerTick))
	if !acquired {
		return nil
	}
	defer release()

	accounts, err := w.accountRepo.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("list active accounts: %w", err)
	}
	groups, err := w.groupRepo.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("list active groups: %w", err)
	}
	groupsByID := make(map[int64]*Group, len(groups))
	for i := range groups {
		groupsByID[groups[i].ID] = &groups[i]
	}

	// 账号查找表：供候选选取按 canDirectProbe 判定可探测性（单一事实源）。
	accountsByID := make(map[int64]*Account, len(accounts))
	for i := range accounts {
		accountsByID[accounts[i].ID] = &accounts[i]
	}

	catalog := DeriveChannelMonitorCatalog(accounts, groupsByID)

	fresh, err := w.store.FreshBusinessChannels(ctx, now.Add(-probeBusinessFreshnessWindow))
	if err != nil {
		return fmt.Errorf("fresh business channels: %w", err)
	}

	states := make(map[ChannelMonitorCatalogKey]*ProbeState, len(catalog.Keys))
	for key := range catalog.Keys {
		st, err := w.store.GetProbeState(ctx, key.Platform, key.Model, key.GroupID)
		if err != nil {
			return fmt.Errorf("get probe state: %w", err)
		}
		states[key] = st
	}

	maxPerTick := w.cfg.Probe.MaxPerTick
	if maxPerTick <= 0 {
		maxPerTick = 2
	}

	tasks := planProbeBatch(now, catalog, fresh, states, accountsByID, maxPerTick)

	for _, t := range tasks {
		account, err := w.accountRepo.GetByID(ctx, t.SampledAccountID)
		if err != nil || account == nil {
			continue
		}
		ok, detail, unsupported, firstTokenMs, totalMs := w.probeFn(ctx, w.accountRepo, account, t.Key.Model, w.httpClient)
		if unsupported {
			// 非权威：账号不可直连探测（竞态：选取后账号协议/凭据变化，或上游型账号被替换）。
			// 跳过写终态失败行，保持"无数据"语义——绝不把"无法探测"误判为渠道故障。
			continue
		}
		if err := w.store.UpsertProbeState(ctx, &ProbeState{
			Platform:      t.Key.Platform,
			Model:         t.Key.Model,
			GroupID:       t.Key.GroupID,
			LastProbeAt:   now,
			LastOK:        ok,
			LastDetail:    detail,
			LastAccountID: t.SampledAccountID,
			NextDue:       computeNextDue(now, ok),
			FirstTokenMs:  firstTokenMs,
			TotalMs:       totalMs,
		}); err != nil {
			return fmt.Errorf("upsert probe state: %w", err)
		}
	}
	return nil
}

// probeTask 是单条待探测渠道（已抽样账号）。
type probeTask struct {
	Key              ChannelMonitorCatalogKey
	SampledAccountID int64
}

// planProbeBatch 是候选筛选的纯函数（派发单 C2 谓词）：
//   - 目录 ∩ 无新鲜业务观测（fresh 中存在的三元组跳过）；
//   - ∩ next_due 到期（states[key] 非空且 NextDue 在 now 之后跳过）；
//   - 每渠道抽样一个在售账号：候选人集按账号 ID 升序取**第一个可直连探测**（canDirectProbe
//     为真）的账号；渠道无任何可探测账号 → 返回 0，跳过该渠道（不写 probe_state 行）；
//   - 每 tick ≤ maxPerTick 条（超出截断）。
//
// 返回结果按三元组字典序稳定排序，便于可重复测试。
func planProbeBatch(
	now time.Time,
	catalog ChannelMonitorCatalog,
	fresh map[ChannelMonitorCatalogKey]struct{},
	states map[ChannelMonitorCatalogKey]*ProbeState,
	accountByID map[int64]*Account,
	maxPerTick int,
) []probeTask {
	if maxPerTick < 0 {
		maxPerTick = 0
	}
	keys := make([]ChannelMonitorCatalogKey, 0, len(catalog.Keys))
	for key := range catalog.Keys {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return catalogKeyLess(keys[i], keys[j]) })

	tasks := make([]probeTask, 0, maxPerTick)
	for _, key := range keys {
		if len(tasks) >= maxPerTick {
			break
		}
		if _, hasFresh := fresh[key]; hasFresh {
			continue
		}
		if st := states[key]; st != nil && st.NextDue.After(now) {
			continue
		}
		// 候选账号选取：按账号 ID 升序取首个可直连探测的账号；无则跳过渠道（无数据态）。
		accID := selectProbeableAccountID(catalog.CandidateAccountIDs[key], accountByID)
		if accID == 0 {
			continue
		}
		tasks = append(tasks, probeTask{Key: key, SampledAccountID: accID})
	}
	return tasks
}

// selectProbeableAccountID 从候选人集中选取一个可直连探测（canDirectProbe 为真）的账号：
//   - 优先选 Schedulable 为真的可探账号（按 ID 升序）；
//   - 无 schedulable 可探账号时退回任意 active 可探账号（按 ID 升序）。
//
// 返回 0 表示无任何可探测账号（调用方据此跳过该渠道，保持"无数据"语义）。
func selectProbeableAccountID(ids []int64, accountByID map[int64]*Account) int64 {
	sorted := make([]int64, len(ids))
	copy(sorted, ids)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var fallback int64
	for _, id := range sorted {
		if !canDirectProbe(accountByID[id]) {
			continue
		}
		if accountByID[id] != nil && accountByID[id].IsSchedulable() {
			return id
		}
		if fallback == 0 {
			fallback = id
		}
	}
	return fallback
}

// computeNextDue 返回探测后的下次到期时间：ok → 30 分钟；!ok → 2 小时。
func computeNextDue(now time.Time, ok bool) time.Time {
	if ok {
		return now.Add(probeCooldownOK)
	}
	return now.Add(probeCooldownFail)
}

func catalogKeyLess(a, b ChannelMonitorCatalogKey) bool {
	if a.Platform != b.Platform {
		return a.Platform < b.Platform
	}
	if a.Model != b.Model {
		return a.Model < b.Model
	}
	return a.GroupID < b.GroupID
}

// probeLeaderLockTTLFor 推导探测 worker 的 leader 锁 TTL（B5）：
// 必须大于最坏轮次运行时长（每 tick 最多 maxPerTick 条，串行，单条硬超时 probeHardTimeout）
// 再叠加 5 分钟缓冲。当 maxPerTick 偏小（≤2）导致公式结果低于原 15 分钟常量时，用
// probeLeaderLockMinTTL 兜住原下限语义（maxPerTick=2 时公式恰为 15 分钟）。
func probeLeaderLockTTLFor(maxPerTick int) time.Duration {
	if maxPerTick <= 0 {
		maxPerTick = 2
	}
	ttl := time.Duration(maxPerTick)*probeHardTimeout + 5*time.Minute
	if ttl < probeLeaderLockMinTTL {
		ttl = probeLeaderLockMinTTL
	}
	return ttl
}

// ProvideChannelMonitorProbeWorker 构造并启动无流量渠道抽样探测调度器（派发单 C2）。
// probe_state 存储由 *sql.DB 支撑；leader 锁键与聚合器锁键独立。
func ProvideChannelMonitorProbeWorker(
	db *sql.DB,
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	leaderLock LeaderLockCache,
	cfg *config.Config,
) *ChannelMonitorProbeWorker {
	store := NewChannelMonitorProbeStore(db)
	w := NewChannelMonitorProbeWorker(store, accountRepo, groupRepo, leaderLock, cfg)
	w.Start()
	return w
}
