package service

// cursor_model_catalog.go 是 cursor 平台**静态**模型清单数据文件（按厂商分组）。
//
// 数据来源：社区公开清单（Cursor 支持模型快照），纯数据、无业务逻辑。每条仅含
// {id, vendor}。本清单（CursorModelCatalog）仅作存档参考：它记录官方名清单，但
// 不是任何入库/挂组判定的权威来源。
//
// 入库 entry 模型与账号 model_mapping 的**唯一静态来源** = CursorRealSupplyModels
// （实供清单，来源见 docs-local/cursor-model-catalog-fact-20261017.md）。
//
// P1-B2 起"勾选门控"职责已收敛：入库退出"人工勾选定义能力"，entry 模型恒等于实供清单
// （CursorStaticEntryModels），不再与人工勾选取交集、不再受厂商开关门控。实供清单在本
// 文件仅剩两处职责：
//  1. 作为静态实供数据本身（不随本轮收敛改动）；
//  2. 为 CursorAutoEntryConfig.Validate 提供"ID → 真实归属厂商"（CursorRealSupplyVendorByID），
//     仅用于拦截配置自报 vendor 冒用（CUR-F8），不参与入库模型集合的产出。
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
// 本清单是入库模型的【静态来源】（P1-B2）：ComputeEntryModels 直接以它产出 entry 模型集
// （恒等，不再与人工勾选取交集），DefaultAutoEntryConfig / GetConfigResponse 同样以它替代
// CursorModelCatalog 作为可选模型全集；Validate 的 vendor 校验也因 muse/composer 已纳入
// cursorKnownVendors 而放行实供模型。
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

// CursorStaticEntryModels 返回入库 entry 模型的静态清单（保序、去重、去空）。
//
// P1-B2：入库退出"人工勾选定义能力"——entry 模型恒等于实供清单本身，不再与人工勾选取
// 交集、不再受厂商开关门控。它是账号 credentials.model_mapping 的写入来源
// （经 CursorAccountModelMapping 生成恒等映射），也是挂组判定的模型上下文。
// 后续校准由 B1 入口覆盖同一字段（model_mapping），不动本清单。
func CursorStaticEntryModels() []string {
	seen := make(map[string]struct{}, len(CursorRealSupplyModels))
	out := make([]string, 0, len(CursorRealSupplyModels))
	for _, e := range CursorRealSupplyModels {
		if e.ID == "" {
			continue
		}
		if _, dup := seen[e.ID]; dup {
			continue
		}
		seen[e.ID] = struct{}{}
		out = append(out, e.ID)
	}
	return out
}

// CursorRealSupplyVendorByID 返回实供清单中"模型 ID → 真实归属厂商"的映射（去重）。
//
// 真实归属以 CursorRealSupplyModels 数据为准（上游 shareai 中转真实吐出的模型），
// 不受配置自报 vendor 影响。P1-B2 收敛后仅剩一处消费：
//   - Validate：若配置自报 vendor 与真实归属不符（如 gpt-6-sol 自报 xai 以绕过 openai 开关），
//     视为厂商开关冒用，直接拒绝。
//
// （ComputeEntryModels 已不再做厂商门控，故本映射不再参与入库模型集合的产出。）
func CursorRealSupplyVendorByID() map[string]string {
	m := make(map[string]string, len(CursorRealSupplyModels))
	for _, e := range CursorRealSupplyModels {
		m[e.ID] = e.Vendor
	}
	return m
}

// CursorRealSupplyModelIDSet 返回实供清单中所有模型 ID 的集合（去重），
// 供需要 O(1) 命中的调用方使用（P1-B2 收敛后 ComputeEntryModels 不再消费）。
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
