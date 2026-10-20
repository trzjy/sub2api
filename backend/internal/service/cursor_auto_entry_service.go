package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// cursor_auto_entry_service.go 实现"入库可配置"域（派发单 D2-a）：
//   - cursor_auto_entry_config 的 Get/Update（存 settings 表新键，JSONB 值）；
//   - ApplyEntryDefaults：入库时把收口后的 priority/rate_multiplier（并发沿用列默认）
//     写入 account 列（P1-B2：entry 模型走静态清单 → credentials.model_mapping，
//     不再写 account.extra.entry_models）；
//   - ProbeKeyQuota：按取证结论（/root/cursor-evidence/01-protocol-doc.md）实现 lookup
//     —— 解密复用 QuotaKeyEncryptor、DJB2 device-id、浏览器 UA，并把 key_quota_snapshot
//     写入该 key 全部账号的 extra。
//
// 零新表/字段：配置走 settings 键值；账号侧仅写既有的 extra（JSONB）与既有列
// （concurrency/priority/rate_multiplier，均带列默认）。

// CursorAutoEntryConfigSettingKey 是 settings 表的新键（JSONB 字符串值）。
// 不经用户裁定不得更换（D2-a 禁区）。
const CursorAutoEntryConfigSettingKey = "cursor_auto_entry_config"

// CursorKeyQuotaSnapshotExtraKey 是 accounts.extra 中存放"key 级额度快照"的键。
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

// CursorKeyQuotaSnapshot 是写入 accounts.extra.key_quota_snapshot 的额度快照。
type CursorKeyQuotaSnapshot struct {
	Used       int    `json:"used"`
	Max        int    `json:"max"`
	StatusCode int    `json:"status_code"`
	CheckedAt  string `json:"checked_at"`
}

// KeyQuotaLookupInfo 是 lookup 返回并向上层（handler）汇总的 key_info 摘要。
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

// AccountFieldWriter 是"写账号列/extra"的窄接口：生产实现走 ent client；测试可替身。
// 与既有的 QuotaKeyRepository（聚焦 quota_keys 表）解耦，避免把账号写操作塞进 quota_keys 仓储。
type AccountFieldWriter interface {
	// ApplyEntryDefaults 持久化账号入库属性：priority/rate_multiplier 列、可选 concurrency 列
	// （P1-B2 起入库参数收口为固定值，不再写 extra.entry_models——该键无生产消费者，随卡清理）。
	// tx 非 nil 时必须在同一事务内持久化（D1 挂载路径：account 由上游刚建、尚未提交，独立 client
	// 看不见未提交行会 NotFound；只有复用挂载事务的 tx 才能写成功）。tx 为 nil 时退化为默认 ent client。
	ApplyEntryDefaults(ctx context.Context, tx *ent.Tx, id int64, priority int, rateMultiplier float64, concurrency *int) error
	// UpdateExtraKey 把 value 合并写入指定账号 extra 的 key（读取现有 extra，合并后整体写回，不覆盖其它键）。
	UpdateExtraKey(ctx context.Context, id int64, key string, value any) error
}

// CursorAutoEntryService 是"入库可配置"域的服务实现。
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
	// deviceIDSeed 用于生成 DJB2 device-id 的随机源（测试可注入固定值）。
	deviceIDGen func() string
}

// NewCursorAutoEntryService 构造入库可配置服务。
// settings/quotaKeys/enc/client 为既有依赖；httpDoer 为 nil 时构建默认客户端
// （180s 超时 + 跳过 TLS 校验，与取证通用 HTTP 行为一致）；lookupBaseURL 为 "" 时用默认基址。
func NewCursorAutoEntryService(settings SettingRepository, quotaKeys QuotaKeyRepository, enc QuotaKeyEncryptor, client *ent.Client, httpDoer HTTPDoer, lookupBaseURL string) *CursorAutoEntryService {
	if httpDoer == nil {
		httpDoer = &http.Client{
			Timeout: 180 * time.Second,
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
		deviceIDGen:   defaultDeviceIDGen,
	}
}

// SetWriter 注入账号字段写入器（测试替身）；生产默认走 ent client。
func (s *CursorAutoEntryService) SetWriter(w AccountFieldWriter) {
	if s != nil {
		s.writer = w
	}
}

// SetDeviceIDGen 注入 device-id 生成器（测试可固定返回值）。
func (s *CursorAutoEntryService) SetDeviceIDGen(fn func() string) {
	if s != nil {
		s.deviceIDGen = fn
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

// ProbeKeyQuota 按取证结论查询某激活码的 key 级额度，并把快照写入其全部账号 extra。
//
// 链路：GetByID 取加密 code → 解密（复用 QuotaKeyEncryptor）→ POST lookup（DJB2 device-id +
// 浏览器 UA + 解密 code 作为 key 头）→ 解析 key_info → 写 key_quota_snapshot 到该 key 全部账号
// extra。不实现模型探活（probe_status 留空，D2-b 域）；不打 api2.cursor.sh（禁区）。
// 返回 key_info 摘要（含写入账号数）。
func (s *CursorAutoEntryService) ProbeKeyQuota(ctx context.Context, quotaKeyID int64) (*KeyQuotaLookupInfo, error) {
	qk, err := s.quotaKeys.GetByID(ctx, quotaKeyID)
	if err != nil {
		return nil, err
	}
	plain, err := s.enc.Decrypt([]byte(qk.CodeEncrypted))
	if err != nil {
		return nil, fmt.Errorf("cursor_auto_entry: decrypt quota key %d: %w", quotaKeyID, err)
	}
	deviceID := s.deviceIDGen()
	info, err := s.lookupQuota(ctx, string(plain), deviceID)
	if err != nil {
		return nil, err
	}
	snapshot := CursorKeyQuotaSnapshot{
		Used:       info.UsedQuota,
		Max:        info.MaxQuota,
		StatusCode: info.StatusCode,
		CheckedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	accounts, err := s.quotaKeys.ListAccountsByQuotaKey(ctx, quotaKeyID)
	if err != nil {
		return nil, fmt.Errorf("cursor_auto_entry: list accounts for key %d: %w", quotaKeyID, err)
	}
	written := 0
	for _, acc := range accounts {
		if err := s.writer.UpdateExtraKey(ctx, acc.ID, CursorKeyQuotaSnapshotExtraKey, snapshot); err != nil {
			return nil, fmt.Errorf("cursor_auto_entry: write snapshot to account %d: %w", acc.ID, err)
		}
		written++
	}
	return &KeyQuotaLookupInfo{
		Group:           info.Group,
		StatusCode:      info.StatusCode,
		Max:             info.MaxQuota,
		Used:            info.UsedQuota,
		DeviceCount:     info.DeviceCount,
		DeviceMaxCount:  info.DeviceMaxCount,
		Mode:            info.Mode,
		ExpiredAt:       info.ExpiredAt,
		AccountsWritten: written,
	}, nil
}

// lookupResponse 是 shareai lookup 响应的 JSON 形状（仅取需要的字段）。
type lookupResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		KeyInfo lookupKeyInfo `json:"key_info"`
	} `json:"data"`
}

type lookupKeyInfo struct {
	Group          string  `json:"group"`
	StatusCode     int     `json:"key_status_code"`
	MaxQuota       int     `json:"quota_key_max_quota"`
	UsedQuota      int     `json:"quota_key_used_quota"`
	DeviceCount    int     `json:"device_count"`
	DeviceMaxCount int     `json:"device_max_count"`
	Mode           int     `json:"mode"`
	ExpiredAt      *string `json:"expired_at"`
}

// lookupQuota 向 shareai lookup 端点发起查询并解析 key_info（共享调用）。
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
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cursor_auto_entry: read lookup response: %w", err)
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
	return &lookupKeyInfo{
		Group:          ki.Group,
		StatusCode:     ki.StatusCode,
		MaxQuota:       ki.MaxQuota,
		UsedQuota:      ki.UsedQuota,
		DeviceCount:    ki.DeviceCount,
		DeviceMaxCount: ki.DeviceMaxCount,
		Mode:           ki.Mode,
		ExpiredAt:      ki.ExpiredAt,
	}, nil
}

// cursorBrowserUserAgent 是 lookup 使用的浏览器 UA（取证：UA 要求）。
const cursorBrowserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// ---------------------------------------------------------------------------
// DJB2 变种 device-id 生成（取证 01-protocol-doc.md：h*31+c 经典 Java hashCode，
// 8 字符大写十六进制，基于 UUID v4）。
// ---------------------------------------------------------------------------

// hashDeviceID 是 JS hashDeviceId 的 Go 等价实现：32-bit 有符号整型累加后取绝对值的
// 8 位大写十六进制（与前端 jg.cursor-free 扩展同算法，可本地重放）。
func hashDeviceID(input string) string {
	var h int32
	for _, c := range input {
		h = h*31 + int32(c)
	}
	var u uint32
	if h < 0 {
		u = uint32(-h)
	} else {
		u = uint32(h)
	}
	return fmt.Sprintf("%08X", u)
}

// defaultDeviceIDGen 生成"基于 UUID v4 的 DJB2 device-id"（每台机首次生成一次后持久化；
// 服务端每次探测可重新生成，lookup 不消耗 quota）。
func defaultDeviceIDGen() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	uuid := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return hashDeviceID(uuid)
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

// readExtra 读取指定账号的 extra 并复制一份返回（避免并发修改同一 map 引用）。
// 仅供 UpdateExtraKey（key 级额度快照合并写入）使用；tx 固定为 nil——快照写入只发生在
// 已提交账号的独立更新路径。
func (w *entAccountFieldWriter) readExtra(ctx context.Context, id int64) (map[string]any, error) {
	acc, err := w.client.Account.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("cursor_auto_entry: load account %d extra: %w", id, err)
	}
	if acc.Extra == nil {
		return map[string]any{}, nil
	}
	cp := make(map[string]any, len(acc.Extra)+1)
	for k, v := range acc.Extra {
		cp[k] = v
	}
	return cp, nil
}

// UpdateExtraKey 把 value 合并写入指定账号 extra 的 key（读取现有 extra，合并后整体写回，
// 不覆盖其它键——与 account_repo 的 extra 合并口径一致）。仅供已提交账号的独立更新使用。
func (w *entAccountFieldWriter) UpdateExtraKey(ctx context.Context, id int64, key string, value any) error {
	if w.client == nil {
		return fmt.Errorf("cursor_auto_entry: ent client not configured for account field writer")
	}
	extra, err := w.readExtra(ctx, id)
	if err != nil {
		return err
	}
	extra[key] = value
	if _, err := w.client.Account.UpdateOneID(id).SetExtra(extra).Save(ctx); err != nil {
		return fmt.Errorf("cursor_auto_entry: persist account %d extra[%s]: %w", id, key, err)
	}
	return nil
}
