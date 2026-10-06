package service

import "strings"

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
	if a == nil || a.Extra == nil {
		return false
	}
	v, ok := a.Extra["mask_upstream_identity"]
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
