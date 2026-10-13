package service

import "context"

// DeletionReason enumerates the concrete reason an account may be soft-deleted.
// ReasonNone means "do not delete": the classifier could not (or must not) conclude deletion.
type DeletionReason int

const (
	ReasonNone DeletionReason = iota
	ReasonTokenExpired
	ReasonBanned
	ReasonQuotaExhausted
)

// ClassificationCertainty describes how conclusive the classification is.
// F6 hard constraint: a deletion decision must only be Definite when one of the
// three explicit conditions (a, b, c) is provably met. Anything else is Uncertain
// and must NOT trigger account deletion.
type ClassificationCertainty string

const (
	CertaintyDefinite  ClassificationCertainty = "definite"
	CertaintyUncertain ClassificationCertainty = "uncertain"
)

// ClassificationResult is the output of a single account-deletion classification.
// It only classifies; it never performs the deletion.
type ClassificationResult struct {
	Reason    DeletionReason
	Certainty ClassificationCertainty
	Message   string
}

// UpstreamResponse is the minimal input shape the classifier consumes. It is NOT
// an upstream protocol type — only the fields needed for classification are carried.
type UpstreamResponse struct {
	StatusCode int
	Body       []byte
	// Refresh carries the outcome of a token refresh that was attempted when the
	// business layer observed a 401.
	Refresh RefreshOutcome
}

// RefreshOutcome captures the result of a token-refresh attempt (only meaningful
// when the upstream returned 401).
type RefreshOutcome struct {
	Attempted        bool
	Success          bool
	PermanentFailure bool  // refresh returned a provably permanent failure (e.g. 401/400 invalid_grant)
	Err              error // 5xx / timeout / network failure
}

// DeletionDecider classifies whether an account should be soft-deleted after its
// upstream interaction. Implementations must never perform the deletion themselves.
type DeletionDecider interface {
	ClassifyAccountDeletion(ctx context.Context, accountID int64, resp *UpstreamResponse) ClassificationResult
}
