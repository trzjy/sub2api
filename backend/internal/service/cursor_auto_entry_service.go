package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// cursor_auto_entry_service.go 实现"入库可配置"域（派发单 D2-a）与**来源额度观测的唯一入口**
// （派发单 W2-B1 / 方案 §25.6 + §25.14-C）：
//   - cursor_auto_entry_config 的 Get/Update（存 settings 表新键，JSONB 值）；
//   - ApplyEntryDefaults：入库时把收口后的 priority/rate_multiplier（并发沿用列默认）
//     写入 account 列（P1-B2：entry 模型走静态清单 → credentials.model_mapping，
//     不再写 account.extra.entry_models）；
//   - ProbeKeyQuota：来源额度观测的唯一实现（管理员探测与 reconciler 周期刷新共用），
//     完成解密 → 设备准备 → 每 key 锁事务 → lookup → 同事务写 usage_snapshot/expires_at
//     → 提交，六步顺序见 §25.14-C。
//
// 旧链已删除：不再把同一份来源快照复制写入 accounts.extra.key_quota_snapshot
// （账号 extra 不是额度归属，来源没有账号时也要可展示）。
//
// 零新表/字段：配置走 settings 键值；账号侧仅写既有的列（priority/rate_multiplier，
// 均带列默认）；来源额度只写 quota_keys.usage_snapshot 与既有 expires_at。

// CursorAutoEntryConfigSettingKey 是 settings 表的新键（JSONB 字符串值）。
// 不经用户裁定不得更换（D2-a 禁区）。
const CursorAutoEntryConfigSettingKey = "cursor_auto_entry_config"

// CursorKeyQuotaSnapshotExtraKey 是**历史**"key 级额度快照复制链"写入 accounts.extra 的键名
// （已被 quota_keys.usage_snapshot 取代，生产不再写入）。
//
// 保留该常量只为旧链清理白名单（PruneLegacyCursorQuotaSnapshots）能指名要清的键：
// 白名单只清 Cursor 这一把旧键并记录数量，绝不触碰其它厂商/账号自身快照。
const CursorKeyQuotaSnapshotExtraKey = "key_quota_snapshot"

// 入库参数收口（P1-B2 §16.1）：入库账号的三项参数固定取值，不再随配置漂移。
//   - priority = 1：常规优先级，不做入库侧加权；
//   - rate_multiplier = 1.0：账号成本中性，不放大/缩小计费（不批改存量非 1 的账号）；
//   - concurrency 不写（nil）→ 沿用 account 列默认 3。
const (
	CursorEntryPriority       = 1
	CursorEntryRateMultiplier = 1.0
)

// CursorAutoEntryDefaultConcurrency 是 concurrency 留空（nil）时沿用的 account 列默认值。
// 与 ent account 的 DefaultConcurrency（=3）一致；实测前不拍默认值（D2 裁定②）。
const CursorAutoEntryDefaultConcurrency = 3

// CursorAutoEntryConfig 是 cursor_auto_entry_config 的设置结构（JSONB）。
//
// vendors：厂商勾选（UI 层过滤依据）；models：勾选的模型清单（静态，probe_status 留空，D2-b 域）；
// entry_defaults：入库默认值——priority/rate_multiplier（D2 裁定③=1/1.0），
// concurrency=nil 表示沿用列默认 3（实测前不拍，D2 裁定②）。
type CursorAutoEntryConfig struct {
	Vendors       map[string]bool     `json:"vendors"`
	Models        []CursorModelRef    `json:"models"`
	EntryDefaults CursorEntryDefaults `json:"entry_defaults"`
}

// CursorModelRef 是配置中一条被勾选的模型引用（含 vendor 与探活占位）。
type CursorModelRef struct {
	ID          string `json:"id"`
	Vendor      string `json:"vendor"`
	ProbeStatus string `json:"probe_status"` // D2-b 域：留空，本卡不实现探活
}

// CursorEntryDefaults 是入库默认值（写入 account 列）。
type CursorEntryDefaults struct {
	Concurrency    *int    `json:"concurrency"` // nil = 沿用列默认 3
	Priority       int     `json:"priority"`
	RateMultiplier float64 `json:"rate_multiplier"`
}

// KeyQuotaLookupInfo 是 lookup 返回并向上层（handler）汇总的 key_info 摘要。
//
// AccountsWritten 是**历史字段**：旧链会把它写成"复制到账号 extra 的行数"。§25.6 收敛后
// 来源额度只写 quota_keys.usage_snapshot（一份），不再逐账号复制，故该字段恒为 0；
// 保留字段只为既有响应形态不变（消费闭包⑤的 DTO 由后续 UI/DTO 单改造）。
type KeyQuotaLookupInfo struct {
	Group           string  `json:"group"`
	StatusCode      int     `json:"status_code"`
	Max             int     `json:"max"`
	Used            int     `json:"used"`
	DeviceCount     int     `json:"device_count"`
	DeviceMaxCount  int     `json:"device_max_count"`
	Mode            int     `json:"mode"`
	ExpiredAt       *string `json:"expired_at"`
	AccountsWritten int     `json:"accounts_written"`
}

// HTTPDoer 抽象 HTTP 客户端（便于单测以替身/httptest 注入）。
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// AccountFieldWriter 是"写账号列"的窄接口：生产实现走 ent client；测试可替身。
// 与既有的 QuotaKeyRepository（聚焦 quota_keys 表）解耦，避免把账号写操作塞进 quota_keys 仓储。
//
// 旧链的 UpdateExtraKey（逐账号写 key_quota_snapshot）已随 §25.6 收敛删除——额度归属在
// quota_keys.usage_snapshot，账号 extra 不承载额度事实。
type AccountFieldWriter interface {
	// ApplyEntryDefaults 持久化账号入库属性：priority/rate_multiplier 列、可选 concurrency 列
	// （P1-B2 起入库参数收口为固定值，不再写 extra.entry_models——该键无生产消费者，随卡清理）。
	// tx 非 nil 时必须在同一事务内持久化（D1 挂载路径：account 由上游刚建、尚未提交，独立 client
	// 看不见未提交行会 NotFound；只有复用挂载事务的 tx 才能写成功）。tx 为 nil 时退化为默认 ent client。
	ApplyEntryDefaults(ctx context.Context, tx *ent.Tx, id int64, priority int, rateMultiplier float64, concurrency *int) error
}

// CursorAutoEntryService 是"入库可配置"域与来源额度观测唯一入口的服务实现。
type CursorAutoEntryService struct {
	settings  SettingRepository
	quotaKeys QuotaKeyRepository
	enc       QuotaKeyEncryptor
	client    *ent.Client
	writer    AccountFieldWriter
	httpDoer  HTTPDoer
	// lookupBaseURL 是 shareai lookup 基址（含 /api 路径前缀）。
	// 默认 https://cur-api.shareai.pro/quota/api（取证 01-protocol-doc.md）。
	lookupBaseURL string
	// observeTx 是"每 key 互斥 + 单事务"骨架的可替换执行点（默认 withQuotaKeyLockTx）。
	// 生产装配恒为 DB advisory 锁 + 真实事务；替身仓储（无 ent client）时退化为直接执行。
	observeTx func(txCtx, lockCtx context.Context, quotaKeyID int64, fn func(tx *ent.Tx) error) error
}

// NewCursorAutoEntryService 构造入库可配置服务。
// settings/quotaKeys/enc/client 为既有依赖；httpDoer 为 nil 时构建默认客户端
// （180s 超时 + 跳过 TLS 校验，与取证通用 HTTP 行为一致）；lookupBaseURL 为 "" 时用默认基址。
func NewCursorAutoEntryService(settings SettingRepository, quotaKeys QuotaKeyRepository, enc QuotaKeyEncryptor, client *ent.Client, httpDoer HTTPDoer, lookupBaseURL string) *CursorAutoEntryService {
	if httpDoer == nil {
		httpDoer = &http.Client{
			Timeout: cursorLookupHTTPBudget,
			Transport: &http.Transport{
				// 取证：rejectUnauthorized=false（跳过 TLS 校验）。
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}
	if lookupBaseURL == "" {
		lookupBaseURL = "https://cur-api.shareai.pro/quota/api"
	}
	return &CursorAutoEntryService{
		settings:      settings,
		quotaKeys:     quotaKeys,
		enc:           enc,
		client:        client,
		writer:        &entAccountFieldWriter{client: client},
		httpDoer:      httpDoer,
		lookupBaseURL: lookupBaseURL,
	}
}

// SetWriter 注入账号字段写入器（测试替身）；生产默认走 ent client。
func (s *CursorAutoEntryService) SetWriter(w AccountFieldWriter) {
	if s != nil {
		s.writer = w
	}
}

// SetObserveTx 注入"每 key 互斥 + 单事务"骨架的替身（测试用；生产恒为 DB 实现）。
func (s *CursorAutoEntryService) SetObserveTx(fn func(txCtx, lockCtx context.Context, quotaKeyID int64, fn2 func(tx *ent.Tx) error) error) {
	if s != nil {
		s.observeTx = fn
	}
}

// DefaultAutoEntryConfig 返回缺省配置（settings 无该键时）：所有已知厂商启用 + 全量静态清单，
// entry_defaults 取 D2 裁定值（priority=1, rate_multiplier=1.0, concurrency=nil 沿用列默认 3）。
func DefaultAutoEntryConfig() *CursorAutoEntryConfig {
	vendors := make(map[string]bool, len(cursorKnownVendors))
	for v := range cursorKnownVendors {
		vendors[v] = true
	}
	models := make([]CursorModelRef, 0, len(CursorRealSupplyModels))
	for _, m := range CursorRealSupplyModels {
		models = append(models, CursorModelRef{ID: m.ID, Vendor: m.Vendor, ProbeStatus: ""})
	}
	return &CursorAutoEntryConfig{
		Vendors: vendors,
		Models:  models,
		EntryDefaults: CursorEntryDefaults{
			Concurrency:    nil,
			Priority:       1,
			RateMultiplier: 1.0,
		},
	}
}

// GetConfig 读取 cursor_auto_entry_config；键不存在时返回缺省配置（不写库，避免无谓写入）。
// 返回值经过 Normalize 保证结构完整（vendors/models 非 nil）。
func (s *CursorAutoEntryService) GetConfig(ctx context.Context) (*CursorAutoEntryConfig, error) {
	if s.settings == nil {
		return DefaultAutoEntryConfig(), nil
	}
	raw, err := s.settings.GetValue(ctx, CursorAutoEntryConfigSettingKey)
	if err != nil {
		if errorsIsNotFound(err) {
			return DefaultAutoEntryConfig(), nil
		}
		return nil, fmt.Errorf("cursor_auto_entry: read config: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return DefaultAutoEntryConfig(), nil
	}
	// R4-2：顶层字面 null 必须显式报错，不得静默成零值空模型集
	// （否则配置损坏会被当作"合法空配置"静默吞掉）。(nil / "null") 两种形态都拦截，
	// 与现有 INVALID 错误同型。
	if strings.TrimSpace(raw) == "null" {
		return nil, infraerrors.New(http.StatusBadRequest, "CURSOR_AUTO_ENTRY_CONFIG_INVALID",
			"cursor_auto_entry_config must not be null")
	}
	var cfg CursorAutoEntryConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "CURSOR_AUTO_ENTRY_CONFIG_INVALID",
			"cursor_auto_entry_config is not valid JSON: "+err.Error())
	}
	cfg.Normalize()
	return &cfg, nil
}

// UpdateConfig 校验并持久化 cursor_auto_entry_config（走既有 settings.Set，upsert）。
// 返回规范化后的配置。
func (s *CursorAutoEntryService) UpdateConfig(ctx context.Context, cfg *CursorAutoEntryConfig) (*CursorAutoEntryConfig, error) {
	if cfg == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "CURSOR_AUTO_ENTRY_CONFIG_INVALID",
			"cursor_auto_entry_config must not be null")
	}
	if errStr := cfg.Validate(); errStr != "" {
		return nil, infraerrors.New(http.StatusBadRequest, "CURSOR_AUTO_ENTRY_CONFIG_INVALID", errStr)
	}
	cfg.Normalize()
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("cursor_auto_entry: marshal config: %w", err)
	}
	if err := s.settings.Set(ctx, CursorAutoEntryConfigSettingKey, string(raw)); err != nil {
		return nil, fmt.Errorf("cursor_auto_entry: persist config: %w", err)
	}
	return cfg, nil
}

// AutoEntryConfigResponse 是 GET auto_entry_config 端点的响应体：在既有配置字段
// （vendors/models/entry_defaults）之外附带"可选模型全集" available_models——前端
// "厂商勾选 → 模型多选"的渲染源。available_models 来自 F1 落地的实供清单
// CursorRealSupplyModels（绝非官方 catalog：后者会把实供模型全部判为非法），为只读派生
// 数据，不写入 settings，不新增任何 DB 字段/表。
type AutoEntryConfigResponse struct {
	Vendors         map[string]bool     `json:"vendors"`
	Models          []CursorModelRef    `json:"models"`
	EntryDefaults   CursorEntryDefaults `json:"entry_defaults"`
	AvailableModels []CursorModelRef    `json:"available_models"`
}

// GetConfigResponse 读取配置并返回含"可选模型全集"的 API 响应（供 GetAutoEntryConfig 端点）。
// available_models 以实供清单 CursorRealSupplyModels 为准（含 muse-spark-1.3 等），与
// DefaultAutoEntryConfig / ComputeEntryModels 的口径一致，避免前端硬编码一份可能漂移的全集。
func (s *CursorAutoEntryService) GetConfigResponse(ctx context.Context) (*AutoEntryConfigResponse, error) {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	available := make([]CursorModelRef, 0, len(CursorRealSupplyModels))
	for _, m := range CursorRealSupplyModels {
		available = append(available, CursorModelRef{ID: m.ID, Vendor: m.Vendor, ProbeStatus: ""})
	}
	return &AutoEntryConfigResponse{
		Vendors:         cfg.Vendors,
		Models:          cfg.Models,
		EntryDefaults:   cfg.EntryDefaults,
		AvailableModels: available,
	}, nil
}

// Normalize 保证 vendors/models 非 nil（缺省时填零值结构，避免前端 null 崩溃）。
func (c *CursorAutoEntryConfig) Normalize() {
	if c.Vendors == nil {
		c.Vendors = make(map[string]bool)
	}
	if c.Models == nil {
		c.Models = []CursorModelRef{}
	}
}

// Validate 校验配置自洽：models 中每条必须有非空 ID 与已知 vendor，且 vendor 在 vendors 中启用
// （勾选模型必须对应已勾选厂商）；entry_defaults 的 rate_multiplier 必须为正。返回首个违规描述。
func (c *CursorAutoEntryConfig) Validate() string {
	// CUR-F8：实供清单的"ID → 真实归属厂商"映射。ID 落在实供清单内时，
	// 自报 vendor 必须等于真实归属，否则视为厂商开关被冒用绕过。
	realVendor := CursorRealSupplyVendorByID()
	for _, m := range c.Models {
		if m.ID == "" {
			return "model entry with empty id"
		}
		if _, ok := cursorKnownVendors[m.Vendor]; !ok {
			return "unknown vendor: " + m.Vendor
		}
		// 落入实供清单：自报 vendor 必须等同清单真实归属（CUR-F8 冒用校验）。
		// 例：gpt-6-sol 自报 vendor=xai 并关 openai / 开 xai，意在把 OpenAI 模型
		// 经 xai 开关纳入补池——真实归属为 openai，必须拦截。
		if real, ok := realVendor[m.ID]; ok && real != m.Vendor {
			return "model " + m.ID + " belongs to vendor " + real + ", not " + m.Vendor
		}
		if !c.Vendors[m.Vendor] {
			return "model " + m.ID + " belongs to disabled vendor " + m.Vendor
		}
	}
	if c.EntryDefaults.RateMultiplier <= 0 {
		return "rate_multiplier must be positive"
	}
	return ""
}

// ComputeEntryModels 返回入库 entry 模型集合（P1-B2：静态清单恒等，退出人工勾选门控）。
//
// 语义：entry 模型恒等于静态实供清单 CursorRealSupplyModels（经 CursorStaticEntryModels
// 保序去重），既不与人工勾选 Models 取交集，也不受厂商开关 Vendors 门控——入库不再以
// "人工勾选"定义账号能力。cfg 参数保留仅为兼容既有调用点（AutoEntryConfigProvider 契约），
// 不参与结果产出。
//
// 由此消除两类旧缺陷（证据：docs-local/cursor-model-catalog-fact.md）：
//   - 门控清单错用官方名快照 CursorModelCatalog：11/11 实供模型被判为非法、补池交集恒空；
//   - 勾选态（用户全关 / 残留旧官方名）可使入库账号零模型：能力本应由上游实供决定。
func ComputeEntryModels(_ *CursorAutoEntryConfig) []string {
	return CursorStaticEntryModels()
}

// CursorAccountModelMapping 把实供清单门控后的选中模型集转换为账号级恒等模型映射
// （credentials.model_mapping 的既有契约形态：键 = 站点请求模型名，值 = Cursor 上游
// wire 模型名；实供清单 ID 本身即 wire 名——cursor_gateway_forward 把 ResolveMappedModel
// 结果直接发往 Cursor 原生端点，故为恒等映射。证据链：docs-local/cursor-model-catalog-fact-20261017.md）。
//
// 空集（配置空选 / autoEntry 未装配）返回 nil：调用方保持无映射 → IsModelSupported 对
// cursor 平台失败关闭（既有闸门，不放宽、不默认放行、无通配）。
func CursorAccountModelMapping(selectedModels []string) map[string]any {
	if len(selectedModels) == 0 {
		return nil
	}
	mapping := make(map[string]any, len(selectedModels))
	for _, id := range selectedModels {
		if id == "" {
			continue
		}
		mapping[id] = id
	}
	if len(mapping) == 0 {
		return nil
	}
	return mapping
}

// ApplyEntryDefaults 把收口后的入库参数写入账号的列（D1 挂载点调用）。
//
// 仅写账号属性，不动挂组集合计算（D2-a 禁区）：
//   - priority → account.priority 列，恒为 CursorEntryPriority（=1）；
//   - rate_multiplier → account.rate_multiplier 列，恒为 CursorEntryRateMultiplier（=1.0，
//     账号成本中性；不批改存量非 1 的账号）；
//   - concurrency：入库不写该列（nil）→ 沿用 account 列默认 3。
//
// 参数不再取自 cfg.EntryDefaults（P1-B2 收口）：cfg 保留仅为 AutoEntryConfigProvider 契约兼容。
//
// tx 非 nil 时（D1 挂载路径）必须传入挂载事务：account 由上游刚建、尚未提交，只有复用同一 tx
// 才能在本事务内写成功；tx 为 nil（如独立调用场景）时退化为默认 ent client 写入已提交账号。
func (s *CursorAutoEntryService) ApplyEntryDefaults(ctx context.Context, tx *ent.Tx, account *ent.Account, _ *CursorAutoEntryConfig) (*ent.Account, error) {
	if account == nil {
		return nil, fmt.Errorf("cursor_auto_entry: nil account")
	}
	if err := s.writer.ApplyEntryDefaults(ctx, tx, account.ID, CursorEntryPriority, CursorEntryRateMultiplier, nil); err != nil {
		return nil, fmt.Errorf("cursor_auto_entry: apply entry defaults to account %d: %w", account.ID, err)
	}
	// 回填内存实体，便于调用方继续复用（如挂组）。
	// 注：此处 account 为 *ent.Account（ent ORM 实体，单次请求内新建/加载、随请求结束即弃），
	// 并非并发收口机制所保护的 service.Account，属请求局部实体，直接写字段为正当保留。
	account.Priority = CursorEntryPriority
	account.RateMultiplier = CursorEntryRateMultiplier
	// concurrency 不回填：保留列默认 3，避免以零值覆盖。
	return account, nil
}

// ---------------------------------------------------------------------------
// 来源额度观测（§25.6 唯一事实源 + §25.14-C 观测完整顺序 1–6）
// ---------------------------------------------------------------------------

const (
	// cursorLookupHTTPBudget 是单次 lookup 的既有 180 秒上限。网络 deadline 取"调用者剩余
	// 时间与 180 秒的较小者"：由调用者 context 派生，天然先到先停，不是每层各给 180 秒。
	cursorLookupHTTPBudget = 180 * time.Second
	// cursorObservationCommitBudget 是网络结束/取消后的 DB 收尾预算（§25.14-C 步骤 5）：
	// 只用它写入与提交，不重发网络、不无限保活、不启脱离生命周期的后台 goroutine。
	cursorObservationCommitBudget = 5 * time.Second
)

// 观测失败的两类内部错误：把"存储失败"与"来源身份冲突"从 lookup 结果里分出来，
// 使调用方能区分"上游查不到（lookupErr，已提交失败观测）"与"本地没写成/来源已换码
// （storageErr / conflict，绝不报成功）"（§25.14-C 步骤 4–6）。
var (
	errCursorObservationStorage  = errors.New("cursor_auto_entry: usage observation storage failure")
	errCursorObservationConflict = errors.New("cursor_auto_entry: quota key source identity changed during observation")
)

// ProbeKeyQuota 是对外暴露的**唯一**来源额度观测入口：管理员额度探测与 reconciler 周期
// 刷新都调用它，QuotaKeyService 不复制 lookup 客户端/解析器。
//
// 六步顺序（§25.14-C）：
//  1. 进入时检查调用者未取消；读取来源、解密、完成设备准备（三分支唯一实现，首写在锁外）；
//  2. 用既有每 key 锁开启观测事务，持锁后重读来源未删、来源身份（MAC/设备）与解析结果一致；
//  3. 持锁执行唯一 lookup（网络 context 继承调用者取消，deadline 取剩余与 180s 较小者）；
//  4. 上游结果与存储结果分开：成功写数值/时间并清 error；失败写 attempted_at/error_code 并
//     保留最近成功值，**回调写完失败观测后返回 nil 使事务提交，提交后才向调用者返回 lookupErr**；
//  5. 事务生命周期 context 与网络 context 分开（WithoutCancel + 网络预算 + 5 秒收尾），
//     写入或 commit 失败返回 storageErr 且不报告成功；
//  6. 提交成功后才返回新投影；同 key 较早 lookup 始终在锁内先提交，后一个才发起，
//     因此不需要 attempt ID 或晚到比较器。
//
// 不实现模型探活（probe_status 留空，D2-b 域）；不打 api2.cursor.sh（禁区）。
func (s *CursorAutoEntryService) ProbeKeyQuota(ctx context.Context, quotaKeyID int64) (*KeyQuotaLookupInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// ── 步骤 1：调用者未取消；读取来源 / 解密 / 设备准备 ────────────────────────
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cursor_auto_entry: observe quota key %d: caller cancelled before start: %w", quotaKeyID, err)
	}
	if s.enc == nil || s.quotaKeys == nil {
		return nil, errors.New("cursor_auto_entry: quota observation dependencies not configured")
	}
	qk, err := s.quotaKeys.GetByID(ctx, quotaKeyID)
	if err != nil {
		return nil, err
	}
	if qk == nil {
		return nil, fmt.Errorf("cursor_auto_entry: quota key %d not found", quotaKeyID)
	}
	plain, err := s.enc.Decrypt([]byte(qk.CodeEncrypted))
	if err != nil {
		// 明文绝不外泄：错误信息不含 code 内容。
		return nil, fmt.Errorf("cursor_auto_entry: decrypt quota key %d: %w", quotaKeyID, err)
	}
	code := string(plain)
	// 设备解析与取号/出站同一份三分支实现；随机生成只出现在"真新码首写"分支，且首写
	// 发生在每 key 锁事务之前（O01 边界保持）。
	identity, err := resolveCursorDeviceIdentity(ctx, s.quotaKeys, qk, code)
	if err != nil {
		return nil, fmt.Errorf("cursor_auto_entry: resolve device for quota key %d: %w", quotaKeyID, err)
	}
	obs, err := asObservationRepo(s.quotaKeys)
	if err != nil {
		return nil, err
	}

	// ── 步骤 5（前半）：网络 context 与事务生命周期 context 分开 ────────────────
	netCtx, netCancel := context.WithTimeout(ctx, cursorLookupHTTPBudget)
	defer netCancel()
	txCtx, txCancel := context.WithTimeout(context.WithoutCancel(ctx), cursorLookupHTTPBudget+cursorObservationCommitBudget)
	defer txCancel()

	var (
		observed  *lookupKeyInfo
		lookupErr error // 只暂存；**提交成功之后**才回传给调用者
	)
	runErr := s.runObservationTx(txCtx, ctx, quotaKeyID, func(tx *ent.Tx) error {
		// ── 步骤 2：持锁重读来源未删、来源身份一致 ──────────────────────────────
		current, rerr := obs.GetByIDInTx(ctx, tx, quotaKeyID)
		if rerr != nil {
			return fmt.Errorf("%w: re-read quota key %d under lock: %v", errCursorObservationStorage, quotaKeyID, rerr)
		}
		if current == nil || current.DeletedAt != nil {
			return fmt.Errorf("%w: quota key %d is gone or soft-deleted under lock", errCursorObservationStorage, quotaKeyID)
		}
		if identity.CodeMAC != "" && current.CodeMAC != "" && current.CodeMAC != identity.CodeMAC {
			// 来源已被换码：本次解析的明文/设备属于旧来源 → 明确冲突、零 lookup。
			// 库内 MAC 为空（历史行 / 尚未落 MAC）时无法比对，按"未观察到变更"放行：
			// 不把"信息不足"伪造成冲突，也不因此跳过来源身份复核（换码路径已落 MAC）。
			return fmt.Errorf("%w: quota key %d credentials rotated before observation", errCursorObservationConflict, quotaKeyID)
		}
		if current.DeviceID != nil && *current.DeviceID != "" && *current.DeviceID != identity.DeviceID {
			return fmt.Errorf("%w: quota key %d device identity changed before observation", errCursorObservationConflict, quotaKeyID)
		}

		// ── 步骤 3：持锁执行唯一 lookup ────────────────────────────────────────
		info, lerr := s.lookupQuota(netCtx, code, identity.DeviceID)

		// ── 步骤 4/5：上游结果与存储结果分开；写入用 ≤5s 收尾 context ────────────
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), cursorObservationCommitBudget)
		defer finishCancel()
		observedAt := time.Now().UTC()
		if lerr != nil {
			lookupErr = lerr
			snapshot, berr := buildCursorUsageSnapshot(current.UsageSnapshot, nil, observedAt, cursorUsageErrorCode(lerr))
			if berr != nil {
				return fmt.Errorf("%w: build failure observation for quota key %d: %v", errCursorObservationStorage, quotaKeyID, berr)
			}
			if werr := obs.UpdateUsageSnapshot(finishCtx, tx, quotaKeyID, snapshot, nil); werr != nil {
				return fmt.Errorf("%w: write failure observation for quota key %d: %v", errCursorObservationStorage, quotaKeyID, werr)
			}
			// 返回 nil ⇒ 事务提交刚写的失败观测；lookupErr 由外层在**提交之后**回传。
			// 绝不能在这里 return lerr，否则刚写的失败记录会随回滚一起消失。
			return nil
		}
		snapshot, berr := buildCursorUsageSnapshot(current.UsageSnapshot, info, observedAt, "")
		if berr != nil {
			return fmt.Errorf("%w: build observation for quota key %d: %v", errCursorObservationStorage, quotaKeyID, berr)
		}
		var expiresAt **time.Time
		if parsed := parseKeyExpiresAt(info.ExpiredAt); parsed != nil {
			// 期限只写既有 quota_keys.expires_at（唯一期限列）；空/无效期限保留原值。
			expiresAt = &parsed
		}
		if werr := obs.UpdateUsageSnapshot(finishCtx, tx, quotaKeyID, snapshot, expiresAt); werr != nil {
			return fmt.Errorf("%w: write observation for quota key %d: %v", errCursorObservationStorage, quotaKeyID, werr)
		}
		observed = info
		return nil
	})
	if runErr != nil {
		// 存储失败 / 身份冲突：既不是"查询失败"，也不得报成探测成功。
		return nil, runErr
	}
	if lookupErr != nil {
		// 走到这里说明失败观测已随事务提交，可以如实向调用者返回上游错误。
		return nil, lookupErr
	}
	if observed == nil {
		return nil, fmt.Errorf("%w: quota key %d observation produced no result", errCursorObservationStorage, quotaKeyID)
	}
	return &KeyQuotaLookupInfo{
		Group:          observed.Group,
		StatusCode:     observed.StatusCode,
		Max:            observed.MaxQuota,
		Used:           observed.UsedQuota,
		DeviceCount:    observed.DeviceCount,
		DeviceMaxCount: observed.DeviceMaxCount,
		Mode:           observed.Mode,
		ExpiredAt:      observed.ExpiredAt,
	}, nil
}

// runObservationTx 执行"每 key 互斥 + 单事务"骨架（步骤 2–6 的载体）。
// 未配置 ent client（内存替身装配）时退化为直接执行 fn：此时没有事务与 advisory 锁可用。
func (s *CursorAutoEntryService) runObservationTx(txCtx, lockCtx context.Context, quotaKeyID int64, fn func(tx *ent.Tx) error) error {
	if s.observeTx != nil {
		return s.observeTx(txCtx, lockCtx, quotaKeyID, fn)
	}
	if s.client == nil {
		return fn(nil)
	}
	return withQuotaKeyLockTx(txCtx, lockCtx, s.client, quotaKeyID, fn)
}

// buildCursorUsageSnapshot 把"最近成功值 + 最近一次观测"合并成 usage_snapshot 的钉死形态
// （§25.14-C 唯一状态表示）：
//   - 成功：写新 total/used/status_code/checked_at，attempted_at=本次，error_code 清空；
//   - 失败：保留最近成功值与其 checked_at（仅供带时间的历史展示），attempted_at=本次，
//     error_code=脱敏分类码；**不写 0、不抹掉最近成功数值**；
//   - 无成功值时数值字段缺省（marshal 为 null），读侧解析成"缺失"，绝不补成真实 0。
func buildCursorUsageSnapshot(previous []byte, success *lookupKeyInfo, observedAt time.Time, failureCode string) ([]byte, error) {
	doc := quotaKeyUsageSnapshotJSON{}
	if prev, ok := ParseQuotaKeyUsageSnapshot(previous); ok {
		doc.Total = prev.Total
		doc.Used = prev.Used
		doc.StatusCode = prev.StatusCode
		if prev.CheckedAt != nil {
			checked := prev.CheckedAt.UTC().Format(time.RFC3339)
			doc.CheckedAt = &checked
		}
	}
	attempted := observedAt.UTC().Format(time.RFC3339)
	doc.AttemptedAt = &attempted
	doc.ErrorCode = strings.TrimSpace(failureCode)
	if doc.ErrorCode == "" && success != nil {
		total, used, status := success.MaxQuota, success.UsedQuota, success.StatusCode
		doc.Total, doc.Used, doc.StatusCode = &total, &used, &status
		checked := attempted
		doc.CheckedAt = &checked
	}
	return json.Marshal(doc)
}

// cursorUsageErrorCode 把 lookup 失败归成脱敏的 error_code（业务码 / 传输 / 解析分别分类）。
// 不把 key_status_code 猜成过期，也不把错误响应里的数字当余额。
func cursorUsageErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if reason := infraerrors.Reason(err); reason != "" {
		return reason
	}
	return "lookup_failed"
}

// lookupResponse 是 shareai lookup 响应的 JSON 形状（仅取需要的字段）。
//
// key_info 与额度字段都用指针：缺字段必须是"缺失"，不得由 Go 零值补成"真实 0"
// （§25.14-C 步骤 3 的字段存在性校验）。
type lookupResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		KeyInfo *lookupKeyInfoJSON `json:"key_info"`
	} `json:"data"`
}

// lookupKeyInfoJSON 是 lookup 响应的原始 key_info（字段全部可缺失以便区分 0 与缺省）。
type lookupKeyInfoJSON struct {
	Group          string  `json:"group"`
	StatusCode     *int    `json:"key_status_code"`
	MaxQuota       *int    `json:"quota_key_max_quota"`
	UsedQuota      *int    `json:"quota_key_used_quota"`
	DeviceCount    *int    `json:"device_count"`
	DeviceMaxCount *int    `json:"device_max_count"`
	Mode           *int    `json:"mode"`
	ExpiredAt      *string `json:"expired_at"`
}

// lookupKeyInfo 是**已校验**的 key_info（额度字段确定存在且非负）。
type lookupKeyInfo struct {
	Group          string
	StatusCode     int
	MaxQuota       int
	UsedQuota      int
	DeviceCount    int
	DeviceMaxCount int
	Mode           int
	ExpiredAt      *string
}

// lookupQuota 向 shareai lookup 端点发起查询并解析 key_info（唯一实现，不发第二套请求）。
//
// 校验口径（§25.14-C 步骤 3）：HTTP 状态、JSON 可解析、code==0、key_info 存在、
// 必需额度字段存在且数值合法——一起验证；任一不满足即失败，绝不把缺字段当 0 观测。
func (s *CursorAutoEntryService) lookupQuota(ctx context.Context, activationCode, deviceID string) (*lookupKeyInfo, error) {
	url := strings.TrimRight(s.lookupBaseURL, "/") + "/lookup"
	body, _ := json.Marshal(map[string]any{})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("cursor_auto_entry: build lookup request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("key", activationCode)
	req.Header.Set("device-id", deviceID)
	// 取证：浏览器 UA 要求（D2-a 卡：lookup 端点/字段/DJB2/UA 要求/解密链）。
	req.Header.Set("User-Agent", cursorBrowserUserAgent)

	resp, err := s.httpDoer.Do(req)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "CURSOR_LOOKUP_FAILED",
			"lookup request failed: "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	// 传输层状态与业务体分别验证：非 2xx 不是"额度数据"。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CURSOR_LOOKUP_FAILED",
			"lookup returned http status %d", resp.StatusCode)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "CURSOR_LOOKUP_FAILED",
			"read lookup response failed: "+err.Error())
	}
	var parsed lookupResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "CURSOR_LOOKUP_INVALID",
			"lookup response not valid JSON: "+err.Error())
	}
	if parsed.Code != 0 {
		return nil, infraerrors.New(http.StatusBadGateway, "CURSOR_LOOKUP_CODE",
			fmt.Sprintf("lookup returned code=%d msg=%s", parsed.Code, parsed.Msg))
	}
	ki := parsed.Data.KeyInfo
	if ki == nil {
		return nil, infraerrors.New(http.StatusBadGateway, "CURSOR_LOOKUP_INVALID",
			"lookup response has no key_info")
	}
	if ki.MaxQuota == nil || ki.UsedQuota == nil {
		// 缺额度字段 ≠ 额度为 0：不得由 Go 零值补成"真实 0"。
		return nil, infraerrors.New(http.StatusBadGateway, "CURSOR_LOOKUP_INVALID",
			"lookup key_info is missing quota fields")
	}
	if *ki.MaxQuota < 0 || *ki.UsedQuota < 0 {
		return nil, infraerrors.New(http.StatusBadGateway, "CURSOR_LOOKUP_INVALID",
			"lookup key_info has illegal negative quota values")
	}
	info := &lookupKeyInfo{
		Group:          ki.Group,
		MaxQuota:       *ki.MaxQuota,
		UsedQuota:      *ki.UsedQuota,
		DeviceCount:    intOrZero(ki.DeviceCount),
		DeviceMaxCount: intOrZero(ki.DeviceMaxCount),
		Mode:           intOrZero(ki.Mode),
		ExpiredAt:      ki.ExpiredAt,
	}
	if ki.StatusCode != nil && *ki.StatusCode >= 0 {
		info.StatusCode = *ki.StatusCode
	}
	return info, nil
}

// intOrZero 展开可缺失的辅助字段（设备数/模式等非额度事实）。
func intOrZero(v *int) int {
	if v == nil || *v < 0 {
		return 0
	}
	return *v
}

// cursorBrowserUserAgent 是 lookup 使用的浏览器 UA（取证：UA 要求）。
const cursorBrowserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// ---------------------------------------------------------------------------
// 旧链清理（§25.6 旧链清理同边界）：白名单只清 Cursor 旧键并记录数量。
// ---------------------------------------------------------------------------

// PruneLegacyCursorQuotaSnapshots 从 Cursor 账号的 extra 中删除**历史**键
// key_quota_snapshot（旧复制链的遗留物），返回受影响账号数量。
//
// 白名单纪律：只清这一把 Cursor 旧键，绝不删除其它厂商快照、账号自身 extra 或任何
// 额度归属数据；不清 quota_keys 的 usage_snapshot（那是唯一事实源）。
func (s *CursorAutoEntryService) PruneLegacyCursorQuotaSnapshots(ctx context.Context) (int, error) {
	if s == nil || s.client == nil {
		return 0, errors.New("cursor_auto_entry: ent client not configured for legacy snapshot cleanup")
	}
	accounts, err := s.client.Account.Query().
		Where(account.PlatformEQ(PlatformCursor)).
		All(ctx)
	if err != nil {
		return 0, fmt.Errorf("cursor_auto_entry: list cursor accounts for legacy snapshot cleanup: %w", err)
	}
	pruned := 0
	for _, acc := range accounts {
		if acc.Extra == nil {
			continue
		}
		if _, ok := acc.Extra[CursorKeyQuotaSnapshotExtraKey]; !ok {
			continue
		}
		extra := make(map[string]any, len(acc.Extra))
		for k, v := range acc.Extra {
			if k == CursorKeyQuotaSnapshotExtraKey {
				continue
			}
			extra[k] = v
		}
		if _, err := s.client.Account.UpdateOneID(acc.ID).SetExtra(extra).Save(ctx); err != nil {
			return pruned, fmt.Errorf("cursor_auto_entry: prune legacy snapshot on account %d: %w", acc.ID, err)
		}
		pruned++
	}
	if pruned > 0 {
		log.Printf("[CursorAutoEntry] pruned legacy account extra[%s] on %d cursor account(s)", CursorKeyQuotaSnapshotExtraKey, pruned)
	}
	return pruned, nil
}

// errorsIsNotFound 兼容 setting 仓库的"未找到"错误（ErrSettingNotFound 或 ent.IsNotFound）。
func errorsIsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if err == ErrSettingNotFound {
		return true
	}
	return ent.IsNotFound(err)
}

// ---------------------------------------------------------------------------
// entAccountFieldWriter：AccountFieldWriter 的生产实现，走 ent client 写账号列/extra。
// ---------------------------------------------------------------------------

// entAccountFieldWriter 是 AccountFieldWriter 的 ent 实现（与既有的 account_repo 同型：
// 列用 UpdateOneID 设置，extra 合并写入不覆盖其它键）。
type entAccountFieldWriter struct {
	client *ent.Client
}

// ApplyEntryDefaults 持久化 priority/rate_multiplier 列与可选 concurrency 列
// （P1-B2：不再写 extra.entry_models——该键无生产消费者，随卡清理）。
// concurrency 为 nil 时跳过该列（沿用列默认 3）。tx 非 nil 时在同一事务内写入
// （D1 挂载路径，account 未提交）；否则退化为默认 ent client（用于已提交账号的独立更新）。
func (w *entAccountFieldWriter) ApplyEntryDefaults(ctx context.Context, tx *ent.Tx, id int64, priority int, rateMultiplier float64, concurrency *int) error {
	if w.client == nil {
		return fmt.Errorf("cursor_auto_entry: ent client not configured for account field writer")
	}
	var builder *ent.AccountUpdateOne
	if tx != nil {
		builder = tx.Account.UpdateOneID(id)
	} else {
		builder = w.client.Account.UpdateOneID(id)
	}
	builder = builder.SetPriority(priority).SetRateMultiplier(rateMultiplier)
	if concurrency != nil {
		builder = builder.SetConcurrency(*concurrency)
	}
	if _, err := builder.Save(ctx); err != nil {
		return fmt.Errorf("cursor_auto_entry: persist account %d entry defaults: %w", id, err)
	}
	return nil
}
