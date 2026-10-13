package service

import (
	"context"
	"strings"
)

// bannedMarkers are the unambiguous signals in an upstream body/status that an
// account has been banned. Used only for path b classification (Definite/Banned).
var bannedMarkers = []string{
	"account_disabled",
	"banned:true",
	"reason=banned",
}

// quotaExhaustedMarkers are the placeholder signals in an upstream body that the
// account's quota is exhausted. Used only for path c classification (Definite/QuotaExhausted).
var quotaExhaustedMarkers = []string{
	"quota_exhausted",
	"insufficient_quota",
	"quota_exceeded",
}

// v3.4 占位：复用 cursor-free 插件硬编码阈值，后期真实余额覆盖。
// 当前无上游消费路径，阈值本身不参与判定，仅作为后期接入真实余额查询的锚点。
const cursorQuotaThresholdPlaceholder = 0

// CursorDeletionDecider implements DeletionDecider for the cursor platform.
// It only classifies; it never deletes. F6 hard constraint is enforced: no
// deletion reason is ever returned as Definite based solely on a status code.
type CursorDeletionDecider struct{}

// NewCursorDeletionDecider constructs a cursor-platform deletion classifier.
func NewCursorDeletionDecider() *CursorDeletionDecider {
	return &CursorDeletionDecider{}
}

// ClassifyAccountDeletion applies the a > b > c priority rules.
//
//	a   : 401 && refresh proved permanent failure (invalid_grant)  -> TokenExpired / Definite
//	a'  : 401 && refresh succeeded                                -> None (token still valid)
//	a'' : 401 && refresh errored (5xx/timeout/network)            -> None (external failure != permanent)
//	b   : banned marker present (body/403+reason=banned)          -> Banned / Definite
//	b'  : bare 403 (no banned marker)                             -> None
//	c   : quota-exhausted marker present                          -> QuotaExhausted / Definite
//	other: bare 401 / 5xx / network / unknown                     -> Uncertain (never delete)
func (d *CursorDeletionDecider) ClassifyAccountDeletion(ctx context.Context, accountID int64, resp *UpstreamResponse) ClassificationResult {
	if resp == nil {
		return ClassificationResult{
			Reason:    ReasonNone,
			Certainty: CertaintyUncertain,
			Message:   "nil upstream response: cannot classify, treat as uncertain",
		}
	}

	// --- Path a: 401 handling (highest priority) ---
	if resp.StatusCode == 401 {
		r := resp.Refresh
		if r.Attempted && r.PermanentFailure {
			return ClassificationResult{
				Reason:    ReasonTokenExpired,
				Certainty: CertaintyDefinite,
				Message:   "401 with token refresh proving permanent failure (e.g. invalid_grant): token is permanently invalid",
			}
		}
		// a' (refresh succeeded) and a'' (refresh errored) both mean the token is
		// not provably permanently invalid -> do NOT delete, and remain Uncertain
		// rather than claiming a definitive non-deletion verdict.
		return ClassificationResult{
			Reason:    ReasonNone,
			Certainty: CertaintyUncertain,
			Message:   "401 without provably permanent refresh failure: must not delete",
		}
	}

	// --- Path b: banned detection (only when an unambiguous banned marker exists) ---
	if hasAnyMarker(resp.Body, bannedMarkers) || resp.StatusCode == 403 && hasBannedReason(resp) {
		return ClassificationResult{
			Reason:    ReasonBanned,
			Certainty: CertaintyDefinite,
			Message:   "upstream reports account disabled/banned: definite ban",
		}
	}

	// b': bare 403 (no banned marker) -> must not delete.
	if resp.StatusCode == 403 {
		return ClassificationResult{
			Reason:    ReasonNone,
			Certainty: CertaintyUncertain,
			Message:   "bare 403 without banned marker: must not delete",
		}
	}

	// --- Path c: quota exhaustion (placeholder, marker-based) ---
	if hasAnyMarker(resp.Body, quotaExhaustedMarkers) {
		return ClassificationResult{
			Reason:    ReasonQuotaExhausted,
			Certainty: CertaintyDefinite,
			Message:   "quota-exhausted marker present: definite quota exhaustion (v3.4 占位)",
		}
	}

	// --- Everything else: 5xx / network / bare status / unknown -> Uncertain ---
	return ClassificationResult{
		Reason:    ReasonNone,
		Certainty: CertaintyUncertain,
		Message:   "no definitive deletion signal: treat as uncertain, do not delete",
	}
}

func hasAnyMarker(body []byte, markers []string) bool {
	if len(body) == 0 {
		return false
	}
	s := string(body)
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// hasBannedReason reports whether a 403 response carries an explicit banned reason,
// e.g. a query/body fragment like "reason=banned".
func hasBannedReason(resp *UpstreamResponse) bool {
	return hasAnyMarker(resp.Body, []string{"reason=banned"})
}
