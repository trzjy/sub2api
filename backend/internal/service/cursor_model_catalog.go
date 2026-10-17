package service

// cursor_model_catalog.go 是 cursor 平台**静态**模型清单数据文件（按厂商分组）。
//
// 数据来源：社区公开清单（Cursor 支持模型快照），纯数据、无业务逻辑。每条仅含
// {id, vendor}。本清单（CursorModelCatalog）仅作存档参考：它记录官方名清单，但
// 不是勾选门控或交集运算的权威来源。
//
// 勾选门控 / "勾选模型 ∩ 静态清单" 交集运算的权威 = CursorRealSupplyModels
// （实供清单，来源见 docs-local/cursor-model-catalog-fact-20261017.md）；前端厂商
// 勾选 → 模型多选的权威同样以实供清单为准，而非本文件的官方快照。
//
// 注意：清单为静态快照，新增厂商/模型不触发 schema 变更；运行期模型获取走实测法（D2 裁定①）。

// CursorModelVendor 是 cursor 平台可接入的厂商标识常量。
const (
	CursorVendorOpenAI    = "openai"
	CursorVendorAnthropic = "anthropic"
	CursorVendorGoogle    = "google"
	CursorVendorXAI       = "xai"
	CursorVendorDeepSeek  = "deepseek"
	CursorVendorMistral   = "mistral"
	// CursorVendorMuse 是 muse-spark-1.3 的自有中转厂商（非上游厂商，docs-local
	// cursor-model-catalog-fact.md §1 实供清单）。仅为校验口径登记，不触发任何差异化路由。
	CursorVendorMuse = "muse"
	// CursorVendorComposer 是 composer-2.5（§2 活体证据保留项）的归类厂商。
	CursorVendorComposer = "composer"
)

// cursorKnownVendors 是所有已知厂商的集合（用于格式校验：vendor 必须落在此集合）。
var cursorKnownVendors = map[string]struct{}{
	CursorVendorOpenAI:    {},
	CursorVendorAnthropic: {},
	CursorVendorGoogle:    {},
	CursorVendorXAI:       {},
	CursorVendorDeepSeek:  {},
	CursorVendorMistral:   {},
	CursorVendorMuse:      {},
	CursorVendorComposer:  {},
}

// CursorModelEntry 是静态清单中的一条模型记录（{id, vendor}）。
type CursorModelEntry struct {
	ID     string `json:"id"`
	Vendor string `json:"vendor"`
}

// CursorModelCatalog 是 cursor 平台静态模型清单（社区公开快照，按厂商分组）。
//
// 仅含 {id, vendor}；不在此处保存任何额度/探活信息。新增条目不影响 schema。
var CursorModelCatalog = []CursorModelEntry{
	// ---- OpenAI ----
	{ID: "gpt-4.1", Vendor: CursorVendorOpenAI},
	{ID: "gpt-4.1-mini", Vendor: CursorVendorOpenAI},
	{ID: "gpt-4.1-nano", Vendor: CursorVendorOpenAI},
	{ID: "gpt-4o", Vendor: CursorVendorOpenAI},
	{ID: "gpt-4o-mini", Vendor: CursorVendorOpenAI},
	{ID: "gpt-4.5-preview", Vendor: CursorVendorOpenAI},
	{ID: "gpt-5", Vendor: CursorVendorOpenAI},
	{ID: "gpt-5-mini", Vendor: CursorVendorOpenAI},
	{ID: "gpt-5-nano", Vendor: CursorVendorOpenAI},
	{ID: "o1", Vendor: CursorVendorOpenAI},
	{ID: "o1-mini", Vendor: CursorVendorOpenAI},
	{ID: "o3", Vendor: CursorVendorOpenAI},
	{ID: "o3-mini", Vendor: CursorVendorOpenAI},
	{ID: "o4-mini", Vendor: CursorVendorOpenAI},
	{ID: "chatgpt-4o-latest", Vendor: CursorVendorOpenAI},

	// ---- Anthropic ----
	{ID: "claude-3-5-sonnet", Vendor: CursorVendorAnthropic},
	{ID: "claude-3-5-haiku", Vendor: CursorVendorAnthropic},
	{ID: "claude-3-7-sonnet", Vendor: CursorVendorAnthropic},
	{ID: "claude-3-opus", Vendor: CursorVendorAnthropic},
	{ID: "claude-3-haiku", Vendor: CursorVendorAnthropic},
	{ID: "claude-sonnet-4", Vendor: CursorVendorAnthropic},
	{ID: "claude-opus-4", Vendor: CursorVendorAnthropic},
	{ID: "claude-4-sonnet", Vendor: CursorVendorAnthropic},
	{ID: "claude-4-opus", Vendor: CursorVendorAnthropic},

	// ---- Google ----
	{ID: "gemini-1.5-pro", Vendor: CursorVendorGoogle},
	{ID: "gemini-1.5-flash", Vendor: CursorVendorGoogle},
	{ID: "gemini-2.0-pro", Vendor: CursorVendorGoogle},
	{ID: "gemini-2.0-flash", Vendor: CursorVendorGoogle},
	{ID: "gemini-2.5-pro", Vendor: CursorVendorGoogle},
	{ID: "gemini-2.5-flash", Vendor: CursorVendorGoogle},

	// ---- xAI ----
	{ID: "grok-3", Vendor: CursorVendorXAI},
	{ID: "grok-3-mini", Vendor: CursorVendorXAI},
	{ID: "grok-4", Vendor: CursorVendorXAI},

	// ---- DeepSeek ----
	{ID: "deepseek-chat", Vendor: CursorVendorDeepSeek},
	{ID: "deepseek-reasoner", Vendor: CursorVendorDeepSeek},

	// ---- Mistral ----
	{ID: "mistral-large", Vendor: CursorVendorMistral},
	{ID: "mistral-small", Vendor: CursorVendorMistral},
}

// CursorModelIDSet 返回静态清单中所有模型 ID 的集合（去重），供交集运算 O(1) 命中。
func CursorModelIDSet() map[string]struct{} {
	set := make(map[string]struct{}, len(CursorModelCatalog))
	for _, m := range CursorModelCatalog {
		set[m.ID] = struct{}{}
	}
	return set
}

// ---------------------------------------------------------------------------
// CursorRealSupplyModels：cursor 渠道【实供】模型清单（上游 shareai 中转真实吐出的模型）。
//
// SSOT：docs-local/cursor-model-catalog-fact.md（2026-10-17 用户截图取证）。与
// CursorModelCatalog（社区公开"官方模型名"快照，gpt-4.1 / claude-3-5-sonnet …）语义
// 完全不同——§1 的 11 个实供模型全部不在官方清单内（11/11 MISSING，已实测）。
// 继续用官方清单做"勾选侧门控"会把本渠道所有实供模型判为非法、导致补池交集恒为空。
//
// 本清单是"用户勾选模型 ∩ 分组白名单"交集运算的【左操作数】（裁定 1）：ComputeEntryModels
// 与 DefaultAutoEntryConfig 以它替代 CursorModelCatalog 作为门控；Validate 的 vendor 校验
// 也因 muse/composer 已纳入 cursorKnownVendors 而放行实供模型。
//
// 取值：§1 实供 11 项 + §2 活体证据保留项 composer-2.5（其余 §2 差异项"如实登记、不擅自
// 合并"，待实测补验后再定，不列入本轮）。清单以既有 catalog 结构扩展落库，零新增表/字段。
// 该清单随上游变化，本轮不实现自动同步（见 SSOT §4 未决）。
// ---------------------------------------------------------------------------
var CursorRealSupplyModels = []CursorModelEntry{
	// §1 实供 11 项（2026-10-17 截图）
	{ID: "grok-4.7", Vendor: CursorVendorXAI},
	{ID: "grok-4.6", Vendor: CursorVendorXAI},
	{ID: "claude-opus-5-5", Vendor: CursorVendorAnthropic},
	{ID: "gpt-6-astra", Vendor: CursorVendorOpenAI},
	{ID: "gpt-6.1-sol", Vendor: CursorVendorOpenAI},
	{ID: "gpt-6-sol", Vendor: CursorVendorOpenAI},
	{ID: "gpt-5.6-sol", Vendor: CursorVendorOpenAI},
	{ID: "claude-fable-5.1", Vendor: CursorVendorAnthropic},
	{ID: "gemini-3.8-flash", Vendor: CursorVendorGoogle},
	{ID: "muse-spark-1.3", Vendor: CursorVendorMuse},
	{ID: "claude-sonnet-5-5", Vendor: CursorVendorAnthropic},
	// §2 活体证据保留项（既有可用性证据）
	{ID: "composer-2.5", Vendor: CursorVendorComposer},
}

// CursorRealSupplyVendorByID 返回实供清单中"模型 ID → 真实归属厂商"的映射（去重）。
//
// 真实归属以 CursorRealSupplyModels 数据为准（上游 shareai 中转真实吐出的模型），
// 不受配置自报 vendor 影响。CUR-F8 用它做两件事：
//   - Validate：若配置自报 vendor 与真实归属不符（如 gpt-6-sol 自报 xai 以绕过 openai 开关），
//     视为厂商开关冒用，直接拒绝；
//   - ComputeEntryModels：以真实归属判厂商开关，不信配置自报 vendor（自报 vendor 仅剩格式校验用途）。
func CursorRealSupplyVendorByID() map[string]string {
	m := make(map[string]string, len(CursorRealSupplyModels))
	for _, e := range CursorRealSupplyModels {
		m[e.ID] = e.Vendor
	}
	return m
}

// CursorRealSupplyModelIDSet 返回实供清单中所有模型 ID 的集合（去重），
// 供 ComputeEntryModels 等门控运算 O(1) 命中。
func CursorRealSupplyModelIDSet() map[string]struct{} {
	set := make(map[string]struct{}, len(CursorRealSupplyModels))
	for _, m := range CursorRealSupplyModels {
		set[m.ID] = struct{}{}
	}
	return set
}

// CursorCatalogModelByVendor 返回按厂商分组的模型条目（用于前端"厂商勾选 → 模型多选"）。
func CursorCatalogModelByVendor() map[string][]CursorModelEntry {
	out := make(map[string][]CursorModelEntry)
	for _, m := range CursorModelCatalog {
		out[m.Vendor] = append(out[m.Vendor], m)
	}
	return out
}

// ValidateCursorModelCatalog 校验静态清单自身格式：每条必须有非空 ID 与已知 vendor，
// 且 ID 在清单内唯一（防止数据文件重复/笔误）。返回首个违规描述；合规返回空串。
//
// 该函数供单测调用，确保纯数据文件始终自洽（零新增运行时依赖）。
func ValidateCursorModelCatalog() string {
	seen := make(map[string]struct{}, len(CursorModelCatalog))
	for _, m := range CursorModelCatalog {
		if m.ID == "" {
			return "empty model id"
		}
		if _, ok := cursorKnownVendors[m.Vendor]; !ok {
			return "unknown vendor: " + m.Vendor + " (model " + m.ID + ")"
		}
		if _, dup := seen[m.ID]; dup {
			return "duplicate model id: " + m.ID
		}
		seen[m.ID] = struct{}{}
	}
	return ""
}
