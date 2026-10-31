package service

import (
	"bytes"
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
