package service

import (
	"bytes"
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

// StripReasoningFromSSELine removes the reasoning_content field from a single
// OpenAI Chat Completions SSE data payload line
// (choices.0.delta.reasoning_content). Lines without a reasoning_content field
// are returned unchanged. Parse/delete failures fall back to the original line
// so the write path never drops a chunk on a malformed payload.
func StripReasoningFromSSELine(line string) string {
	if !gjson.Get(line, "choices.0.delta.reasoning_content").Exists() {
		return line
	}
	out, err := sjson.Delete(line, "choices.0.delta.reasoning_content")
	if err != nil {
		return line
	}
	return out
}

// StripReasoningFromResponseBody removes the reasoning_content field from an
// OpenAI Chat Completions JSON response body
// (choices.0.message.reasoning_content). Bodies without a reasoning_content
// field are returned unchanged. Parse/delete failures fall back to the original
// body.
func StripReasoningFromResponseBody(body []byte) []byte {
	if !gjson.GetBytes(body, "choices.0.message.reasoning_content").Exists() {
		return body
	}
	out, err := sjson.DeleteBytes(body, "choices.0.message.reasoning_content")
	if err != nil {
		return body
	}
	return out
}
