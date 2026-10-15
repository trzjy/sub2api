package service

// cursor_model_catalog.go 是 cursor 平台**静态**模型清单数据文件（按厂商分组）。
//
// 数据来源：社区公开清单（Cursor 支持模型快照），纯数据、无业务逻辑。每条仅含
// {id, vendor}。该清单是 ApplyEntryDefaults "勾选模型 ∩ 静态清单" 交集运算的右操作数，
// 也是前端厂商勾选 → 模型多选的权威来源。probe_status（探活）字段不在此处（D2-b 域）。
//
// 注意：清单为静态快照，新增厂商/模型不触发 schema 变更；运行期模型获取走实测法（D2 裁定①）。

// CursorModelVendor 是 cursor 平台可接入的厂商标识常量。
const (
	CursorVendorOpenAI    = "openai"
	CursorVendorAnthropic = "anthropic"
	CursorVendorGoogle    = "google"
	CursorVendorXAI       = "xai"
	CursorVendorDeepSeek  = "deepseek"
	CursorVendorMuse      = "muse"
	CursorVendorMistral   = "mistral"
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
	// ---- Anthropic（shareai 像素工匠渠道实供，2026-10-15 用户截图名单）----
	{ID: "claude-opus-5.5", Vendor: CursorVendorAnthropic},
	{ID: "claude-fable-5.1", Vendor: CursorVendorAnthropic},
	{ID: "claude-sonnet-5.5", Vendor: CursorVendorAnthropic},
	// ---- OpenAI ----
	{ID: "gpt-6.1-sol", Vendor: CursorVendorOpenAI},
	{ID: "gpt-6-sol", Vendor: CursorVendorOpenAI},
	{ID: "gpt-5.6-sol", Vendor: CursorVendorOpenAI},
	{ID: "gpt-6-astra", Vendor: CursorVendorOpenAI},
	// ---- Google ----
	{ID: "gemini-3.8-flash", Vendor: CursorVendorGoogle},
	// ---- xAI ----
	{ID: "grok-4.7", Vendor: CursorVendorXAI},
	{ID: "grok-4.6", Vendor: CursorVendorXAI},
	// ---- Muse（渠道自有）----
	{ID: "muse-spark-1.3", Vendor: "muse"},
}

// CursorModelIDSet 返回静态清单中所有模型 ID 的集合（去重），供交集运算 O(1) 命中。
func CursorModelIDSet() map[string]struct{} {
	set := make(map[string]struct{}, len(CursorModelCatalog))
	for _, m := range CursorModelCatalog {
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
