package service

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// maskVLLMFingerprints strips vLLM-specific fingerprints from an upstream
// OpenAI Chat Completions response body so a masked-identity account does not
// leak that it is backed by a self-hosted vLLM instance:
//
//  1. removes the top-level "system_fingerprint" field (e.g. "vllm-0.26.0-<hash>");
//  2. folds the vLLM "reasoning" field into the kimi/OpenAI-style
//     "reasoning_content" field on each choice message/delta, and removes the
//     now-redundant "reasoning" field.
//
// It accepts either a single JSON object (non-streaming response, or one SSE
// data payload) or a full SSE body (lines beginning with "data:") and masks
// each event. It is a no-op (returns body unchanged) when no fingerprint is
// present, so callers may invoke it freely; masking should still be gated on
// IsIdentityMaskedAccount to keep non-masked accounts byte-for-byte unchanged.
func maskVLLMFingerprints(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// SSE body: mask the JSON payload of every "data:" line.
	if bytes.Contains(body, []byte("\ndata:")) || bytes.HasPrefix(body, []byte("data:")) {
		lines := bytes.Split(body, []byte("\n"))
		changed := false
		for i, line := range lines {
			trimmed := bytes.TrimSpace(line)
			if !bytes.HasPrefix(trimmed, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			masked := maskVLLMFingerprintsJSON(payload)
			if !bytes.Equal(masked, payload) {
				lines[i] = append([]byte("data: "), masked...)
				changed = true
			}
		}
		if changed {
			return bytes.Join(lines, []byte("\n"))
		}
		return body
	}
	return maskVLLMFingerprintsJSON(body)
}

// maskVLLMFingerprintsJSON masks a single JSON object (one Chat Completions
// response or one SSE data payload).
func maskVLLMFingerprintsJSON(b []byte) []byte {
	out := b
	changed := false

	// 1. Drop the vLLM "system_fingerprint" fingerprint.
	if gjson.GetBytes(out, "system_fingerprint").Exists() {
		out, _ = sjson.DeleteBytes(out, "system_fingerprint")
		changed = true
	}

	// 2. Fold vLLM "reasoning" into kimi-style "reasoning_content".
	choices := gjson.GetBytes(out, "choices")
	if choices.Exists() {
		choices.ForEach(func(key, _ gjson.Result) bool {
			k := key.String()
			for _, sub := range []string{"message", "delta"} {
				reasoningPath := "choices." + k + "." + sub + ".reasoning"
				r := gjson.GetBytes(out, reasoningPath)
				if !r.Exists() || r.String() == "" {
					continue
				}
				rcPath := "choices." + k + "." + sub + ".reasoning_content"
				rc := gjson.GetBytes(out, rcPath)
				if rc.Exists() && rc.String() != "" {
					continue // already exposes reasoning_content; leave as-is
				}
				nb, err := sjson.SetBytes(out, rcPath, r.String())
				if err != nil {
					continue
				}
				out = nb
				out, _ = sjson.DeleteBytes(out, reasoningPath)
				changed = true
			}
			return true
		})
	}

	if !changed {
		return b
	}
	return out
}

// IsIdentityMaskedAccount reports whether the account is configured to mask the
// upstream model identity from the client. Masked accounts rewrite the echoed
// upstream model name (including alias variants such as the :free-suffix
// stripped form) back to the client-requested model name, preventing upstream
// identity leakage when an upstream echoes a name that drops a suffix the client
// never sent.
//
// The flag is read from a.Extra["mask_upstream_identity"] as a boolean. Missing,
// empty, non-bool, or false values — as well as a nil account or an account
// with a nil Extra map — return false. This keeps the behavior a zero-impact
// no-op for every other platform account.
func IsIdentityMaskedAccount(a *Account) bool {
	asnap := a.SnapshotExtra()
	if a == nil || asnap == nil {
		return false
	}
	v, ok := asnap["mask_upstream_identity"]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		// Tolerate JSON-ish string encodings of the flag.
		return b == "true" || b == "1"
	default:
		return false
	}
}

// responseIdentityMaskKey 是「响应侧身份净化」账号级开关（extra 键名）。
// 与 mask_upstream_identity 的区别：mask_upstream_identity 同时是分层路由小档池的准入标记，
// 开启它会改变账号的档位归属；本开关只驱动响应侧净化（模型回写 / 错误体净化 / 指纹剥离），
// 不参与任何调度或分层判定。
const responseIdentityMaskKey = "response_identity_mask"

// ResponseIdentityMaskExtraKey 是 responseIdentityMaskKey 的导出别名，供仓储层调度快照
// 投影白名单引用——投影裁掉它，新开关在选号返回的账号上恒为 false，响应侧净化静默失效。
const ResponseIdentityMaskExtraKey = responseIdentityMaskKey

// IsResponseIdentityMaskedAccount 报告该账号是否需要响应侧身份净化。
// 语义 = mask_upstream_identity（旧开关，保持既有账号行为不变）|| extra.response_identity_mask（新开关）。
// 读取口径与 IsIdentityMaskedAccount 一致：SnapshotExtra 快照读取；bool 与 "true"/"1" 字符串均视为真；
// 缺键 / 空值 / 非 bool 非上述字符串 / nil 账号 / nil Extra 一律为 false。
func IsResponseIdentityMaskedAccount(a *Account) bool {
	// 先复用旧开关结果，保持既有账号行为不变，且不得改写 IsIdentityMaskedAccount 本身。
	if IsIdentityMaskedAccount(a) {
		return true
	}
	asnap := a.SnapshotExtra()
	if a == nil || asnap == nil {
		return false
	}
	// 新写法：统一从 identity_disguise 解析（唯一读侧入口，identity_disguise.go）。
	if d, ok := parseIdentityDisguiseExtra(asnap); ok {
		return d.RewriteModel
	}
	// 老写法：response_identity_mask 开关。
	v, ok := asnap[responseIdentityMaskKey]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		// 与 IsIdentityMaskedAccount 一致：宽容解析 "true" / "1"。
		return b == "true" || b == "1"
	default:
		return false
	}
}

// maskedInputBandKey 是「伪装线账号可接档位区间」声明（账号 extra 键名）。
//
// 背景：分层路由把请求按估算 input 分成小档（→ 伪装线池）与大档（→ 星思池）。伪装线池
// 里各账号的真实上下文能力并不一样：qwen 伪装线（codebuddy）只适合小请求，而 kiro 中转
// 背后是 20 万上下文的上游，适合吃 1 万以上的大请求。用本键把「谁能接哪一段」写成数据，
// 不再写死在代码里。
//
// 取值形如 {"min": 10000, "max": 150000}（单位 = 分层路由的估算 input tokens，闭区间）。
// 语义：
//   - 未声明本键 → 保持历史行为：只参与小档，永不被大档选中；
//   - 已声明本键 → 退出小档池，只在大档被选中，且仅当估算 input 落在 [min, max] 内。
//
// 上限必须小于上游真实上下文窗口：kiro-go 对非 Claude 模型按 200k 返回窗口大小，
// 超过后上游会静默截断（不报错、直接丢上下文），因此上限要留出输出与估算误差的余量。
const maskedInputBandKey = "masked_input_band"

// MaskedInputBandExtraKey 是 maskedInputBandKey 的导出别名，供仓储层的调度快照投影
// （filterSchedulerExtra 白名单）引用。该白名单必须保留本键：候选过滤读的是快照投影，
// 丢掉本键会让大档专用账号退化成「未声明区间」→ 被当成小档账号，反而去抢 1 万以下的小活。
const MaskedInputBandExtraKey = maskedInputBandKey

// MaskedInputBand 是伪装线账号声明承接的估算 input 区间（单位 tokens，闭区间）。
type MaskedInputBand struct {
	Min int
	Max int
}

// maskedInputBandUnboundedMax 是只声明 min 时的上界兜底（等价于「不设上限」）。
const maskedInputBandUnboundedMax = 1 << 30

// Contains 报告估算 input 是否落在本区间内（闭区间，含两端）。
func (b MaskedInputBand) Contains(estimatedInputTokens int) bool {
	return estimatedInputTokens >= b.Min && estimatedInputTokens <= b.Max
}

// MaskedInputBandOf 解析账号 extra.masked_input_band 声明。
//
// 返回 ok=false 表示未声明或声明非法（缺 min/max 之外的退化形态、max<=0、min>max、
// 非对象、nil 账号、nil Extra），此时账号按历史语义处理（只参与小档）。
// 只容忍「仅 min」或「仅 max」的写法：缺 min 视为 0，缺 max 视为无上限。
func MaskedInputBandOf(a *Account) (MaskedInputBand, bool) {
	asnap := a.SnapshotExtra()
	if a == nil || asnap == nil {
		return MaskedInputBand{}, false
	}
	raw, ok := asnap[maskedInputBandKey]
	if !ok {
		return MaskedInputBand{}, false
	}
	obj := maskedInputBandObject(raw)
	if len(obj) == 0 {
		return MaskedInputBand{}, false
	}
	minRaw, hasMin := obj["min"]
	maxRaw, hasMax := obj["max"]
	if !hasMin && !hasMax {
		return MaskedInputBand{}, false
	}
	band := MaskedInputBand{Min: 0, Max: maskedInputBandUnboundedMax}
	if hasMin {
		band.Min = int(parseExtraFloat64(minRaw))
	}
	if hasMax {
		band.Max = int(parseExtraFloat64(maxRaw))
	}
	if band.Min < 0 || band.Max <= 0 || band.Min > band.Max {
		return MaskedInputBand{}, false
	}
	return band, true
}

// AccountMaxInputExtraKey 是账号级「单请求估算 input 上限」的 extra 键。声明后，
// 估算 input 超过该值的请求不再把此账号列为候选，用于按次计费套餐 / 订阅额度保护。
const AccountMaxInputExtraKey = "max_input_tokens"

// AccountMaxInputTokensOf 解析账号声明的单请求估算 input 上限（单位 tokens）。
// 返回 ok=false 表示未声明或声明非法（<=0、非数值），此时账号零行为变化。
func AccountMaxInputTokensOf(a *Account) (int, bool) {
	if a == nil {
		return 0, false
	}
	asnap := a.SnapshotExtra()
	if asnap == nil {
		return 0, false
	}
	raw, ok := asnap[AccountMaxInputExtraKey]
	if !ok {
		return 0, false
	}
	max := int(parseExtraFloat64(raw))
	if max <= 0 {
		return 0, false
	}
	return max, true
}

// maskedInputBandObject 把 extra 里的原始值归一为 map[string]any。
// DB 读回的 extra 是 jsonb → map[string]any；升级/导入路径可能带 JSON 字符串，
// 两种形态都接受，其余形态返回 nil（视为未声明）。
func maskedInputBandObject(raw any) map[string]any {
	switch v := raw.(type) {
	case map[string]any:
		return v
	case string:
		var obj map[string]any
		if err := json.Unmarshal([]byte(v), &obj); err != nil {
			return nil
		}
		return obj
	default:
		return nil
	}
}

// IdentityRewriteAliases returns the set of upstream model name variants that a
// masked account should treat as matching the mapped (upstream-sent) model when
// rewriting the echoed model back to the client-requested name.
//
// It always includes mappedModel itself, plus the "tail-stripped" form: the
// substring before the last ':' and any trailing content. For example
// "qwen3.8-flash:free" yields ["qwen3.8-flash:free", "qwen3.8-flash"]. When
// mappedModel contains no ':' (or the ':' is the leading character), only
// [mappedModel] is returned. A empty mappedModel yields an empty slice.
func IdentityRewriteAliases(mappedModel string) []string {
	if mappedModel == "" {
		return []string{}
	}
	if idx := strings.LastIndex(mappedModel, ":"); idx > 0 {
		return []string{mappedModel, mappedModel[:idx]}
	}
	return []string{mappedModel}
}

// IsReasoningStrippedAccount reports whether the account is configured to strip
// the upstream reasoning_content (thinking / reasoning) field from the
// client-visible response. This is a defense-in-depth measure for self-hosted
// upstreams whose thinking text may echo competitor names or paraphrase the
// system prompt: the gateway removes the field entirely so clients never see it.
//
// The flag is read from a.Extra["strip_reasoning_content"] as a boolean.
// Missing, empty, non-bool, or false values — as well as a nil account or an
// account with a nil Extra map — return false. This keeps the behavior a
// zero-impact no-op for every other platform account.
func IsReasoningStrippedAccount(a *Account) bool {
	asnap := a.SnapshotExtra()
	if a == nil || asnap == nil {
		return false
	}
	// 新写法：统一从 identity_disguise 解析（唯一读侧入口，identity_disguise.go）。
	if d, ok := parseIdentityDisguiseExtra(asnap); ok {
		return d.StripReasoning
	}
	v, ok := asnap["strip_reasoning_content"]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true" || b == "1"
	default:
		return false
	}
}

// stripReasoningChoiceKey 计算 choices 数组元素的访问键：
// 优先用该元素自身的 index 字段值（必须是数值）；index 缺失或非数值时回退数组下标 pos。
// 这样既能尊重上游在 choice 上标注的 index，也能在 index 缺失时退化为位置访问。
func stripReasoningChoiceKey(choice gjson.Result, pos int) string {
	idx := choice.Get("index")
	if idx.Exists() && idx.Type == gjson.Number {
		return strconv.FormatInt(idx.Int(), 10)
	}
	return strconv.Itoa(pos)
}

// StripReasoningFromSSELine removes the reasoning_content and reasoning fields
// from the delta of every choice in a single OpenAI Chat Completions SSE data
// payload line. SenseNova (and other upstreams) may emit the thinking field as
// either "reasoning_content" or "reasoning"; this strips both so the upstream
// identity/competitor terms hidden in the thinking text never reach the client.
//
// Each choice is addressed by its own index field when present, falling back to
// the array position otherwise. Lines without any reasoning/reasoning_content
// field are returned byte-for-byte unchanged (zero-allocation fast path).
// Parse/delete failures fall back to the original line so the write path never
// drops a chunk on a malformed payload.
func StripReasoningFromSSELine(line string) string {
	// 零分配快路径：不含 reasoning 子串直接原样返回（reasoning_content / reasoning 均含该子串）。
	if !strings.Contains(line, "reasoning") {
		return line
	}
	if !gjson.Get(line, "choices").Exists() {
		return line
	}
	// 先探测是否存在待删字段，无任何目标字段时原样返回，避免无谓重序列化。
	hasTarget := false
	gjson.Get(line, "choices").ForEach(func(_, choice gjson.Result) bool {
		if choice.Get("delta.reasoning_content").Exists() || choice.Get("delta.reasoning").Exists() {
			hasTarget = true
			return false
		}
		return true
	})
	if !hasTarget {
		return line
	}
	out := line
	pos := 0
	gjson.Get(line, "choices").ForEach(func(_, choice gjson.Result) bool {
		key := stripReasoningChoiceKey(choice, pos)
		for _, sub := range []string{"reasoning_content", "reasoning"} {
			path := "choices." + key + ".delta." + sub
			if !gjson.Get(out, path).Exists() {
				continue
			}
			n, err := sjson.Delete(out, path)
			if err != nil {
				// 解析/删除失败回退原值，绝不丢行、绝不 panic。
				return false
			}
			out = n
		}
		pos++
		return true
	})
	return out
}

// StripReasoningFromResponseBody removes the reasoning_content and reasoning
// fields from the message of every choice in an OpenAI Chat Completions JSON
// response body. See StripReasoningFromSSELine for the field-name and indexing
// rationale. Bodies without any reasoning/reasoning_content field are returned
// byte-for-byte unchanged. Parse/delete failures fall back to the original body.
func StripReasoningFromResponseBody(body []byte) []byte {
	// 零分配快路径：不含 reasoning 子串直接原样返回。
	if !bytes.Contains(body, []byte("reasoning")) {
		return body
	}
	if !gjson.GetBytes(body, "choices").Exists() {
		return body
	}
	hasTarget := false
	gjson.GetBytes(body, "choices").ForEach(func(_, choice gjson.Result) bool {
		if choice.Get("message.reasoning_content").Exists() || choice.Get("message.reasoning").Exists() {
			hasTarget = true
			return false
		}
		return true
	})
	if !hasTarget {
		return body
	}
	out := body
	pos := 0
	gjson.GetBytes(body, "choices").ForEach(func(_, choice gjson.Result) bool {
		key := stripReasoningChoiceKey(choice, pos)
		for _, sub := range []string{"reasoning_content", "reasoning"} {
			path := "choices." + key + ".message." + sub
			if !gjson.GetBytes(out, path).Exists() {
				continue
			}
			n, err := sjson.DeleteBytes(out, path)
			if err != nil {
				return false
			}
			out = n
		}
		pos++
		return true
	})
	return out
}
