//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
)

func newCursorDecider() *CursorDeletionDecider {
	return NewCursorDeletionDecider()
}

// T1: 401 + refresh success -> ReasonNone (token still valid, must not delete).
func TestCursorDeletionDecider_T1_401RefreshSuccess_None(t *testing.T) {
	d := newCursorDecider()
	resp := &UpstreamResponse{
		StatusCode: 401,
		Refresh: RefreshOutcome{
			Attempted: true,
			Success:   true,
		},
	}
	got := d.ClassifyAccountDeletion(context.Background(), 1, resp)
	if got.Reason != ReasonNone {
		t.Fatalf("T1: expected ReasonNone, got %v", got.Reason)
	}
	if got.Certainty == CertaintyDefinite {
		t.Fatalf("T1: must not be Definite, got %v", got.Certainty)
	}
}

// T2: 401 + refresh permanent failure (invalid_grant) -> TokenExpired / Definite.
func TestCursorDeletionDecider_T2_401RefreshPermanent_Definite(t *testing.T) {
	d := newCursorDecider()
	resp := &UpstreamResponse{
		StatusCode: 401,
		Refresh: RefreshOutcome{
			Attempted:        true,
			PermanentFailure: true,
		},
	}
	got := d.ClassifyAccountDeletion(context.Background(), 1, resp)
	if got.Reason != ReasonTokenExpired {
		t.Fatalf("T2: expected ReasonTokenExpired, got %v", got.Reason)
	}
	if got.Certainty != CertaintyDefinite {
		t.Fatalf("T2: expected CertaintyDefinite, got %v", got.Certainty)
	}
}

// T3: 401 + refresh errored (timeout) -> ReasonNone (external failure != permanent).
func TestCursorDeletionDecider_T3_401RefreshTimeout_None(t *testing.T) {
	d := newCursorDecider()
	resp := &UpstreamResponse{
		StatusCode: 401,
		Refresh: RefreshOutcome{
			Attempted: true,
			Err:       errors.New("refresh timeout"),
		},
	}
	got := d.ClassifyAccountDeletion(context.Background(), 1, resp)
	if got.Reason != ReasonNone {
		t.Fatalf("T3: expected ReasonNone, got %v", got.Reason)
	}
	if got.Certainty == CertaintyDefinite {
		t.Fatalf("T3: must not be Definite, got %v", got.Certainty)
	}
}

// T4: bare 403 (no banned marker) -> ReasonNone (must not delete).
func TestCursorDeletionDecider_T4_BareZZ403_None(t *testing.T) {
	d := newCursorDecider()
	resp := &UpstreamResponse{
		StatusCode: 403,
		Body:       []byte(`{"error":"forbidden"}`),
	}
	got := d.ClassifyAccountDeletion(context.Background(), 1, resp)
	if got.Reason != ReasonNone {
		t.Fatalf("T4: expected ReasonNone, got %v", got.Reason)
	}
	if got.Certainty == CertaintyDefinite {
		t.Fatalf("T4: must not be Definite, got %v", got.Certainty)
	}
}

// T5: 403 + banned marker -> Banned / Definite.
func TestCursorDeletionDecider_T5_ExplicitBanned_Definite(t *testing.T) {
	d := newCursorDecider()
	cases := []struct {
		name string
		resp *UpstreamResponse
	}{
		{"account_disabled", &UpstreamResponse{StatusCode: 403, Body: []byte(`{"error":"account_disabled"}`)}},
		{"banned_true", &UpstreamResponse{StatusCode: 403, Body: []byte(`banned:true`)}},
		{"reason_banned", &UpstreamResponse{StatusCode: 403, Body: []byte(`reason=banned`)}},
	}
	for _, c := range cases {
		got := d.ClassifyAccountDeletion(context.Background(), 1, c.resp)
		if got.Reason != ReasonBanned {
			t.Fatalf("T5/%s: expected ReasonBanned, got %v", c.name, got.Reason)
		}
		if got.Certainty != CertaintyDefinite {
			t.Fatalf("T5/%s: expected CertaintyDefinite, got %v", c.name, got.Certainty)
		}
	}
}

// T6: 5xx / network -> Uncertain (never delete).
func TestCursorDeletionDecider_T6_5xxOrNetwork_Uncertain(t *testing.T) {
	d := newCursorDecider()
	cases := []struct {
		name string
		resp *UpstreamResponse
	}{
		{"500", &UpstreamResponse{StatusCode: 500}},
		{"502", &UpstreamResponse{StatusCode: 502}},
		{"network_no_status", &UpstreamResponse{StatusCode: 0}},
	}
	for _, c := range cases {
		got := d.ClassifyAccountDeletion(context.Background(), 1, c.resp)
		if got.Certainty != CertaintyUncertain {
			t.Fatalf("T6/%s: expected CertaintyUncertain, got %v", c.name, got.Certainty)
		}
		if got.Reason != ReasonNone {
			t.Fatalf("T6/%s: expected ReasonNone, got %v", c.name, got.Reason)
		}
	}
}

// T7: quota-exhausted marker present -> QuotaExhausted / Definite.
func TestCursorDeletionDecider_T7_QuotaExhausted_Definite(t *testing.T) {
	d := newCursorDecider()
	cases := []struct {
		name string
		resp *UpstreamResponse
	}{
		{"quota_exhausted", &UpstreamResponse{StatusCode: 429, Body: []byte(`{"error":"quota_exhausted"}`)}},
		{"insufficient_quota", &UpstreamResponse{StatusCode: 200, Body: []byte(`insufficient_quota`)}},
		{"quota_exceeded", &UpstreamResponse{StatusCode: 429, Body: []byte(`quota_exceeded`)}},
	}
	for _, c := range cases {
		got := d.ClassifyAccountDeletion(context.Background(), 1, c.resp)
		if got.Reason != ReasonQuotaExhausted {
			t.Fatalf("T7/%s: expected ReasonQuotaExhausted, got %v", c.name, got.Reason)
		}
		if got.Certainty != CertaintyDefinite {
			t.Fatalf("T7/%s: expected CertaintyDefinite, got %v", c.name, got.Certainty)
		}
	}
}

// F6 防退化: 裸 401 / 裸 403 must never be classified as Definite.
func TestCursorDeletionDecider_NeverDefiniteOnBare401_403(t *testing.T) {
	d := newCursorDecider()
	bare401 := &UpstreamResponse{StatusCode: 401} // refresh not attempted
	bare403 := &UpstreamResponse{StatusCode: 403, Body: []byte(`{"error":"forbidden"}`)}

	for _, resp := range []*UpstreamResponse{bare401, bare403} {
		got := d.ClassifyAccountDeletion(context.Background(), 1, resp)
		if got.Certainty == CertaintyDefinite {
			t.Fatalf("F6: bare %d must not be Definite, got %v", resp.StatusCode, got.Certainty)
		}
		if got.Reason != ReasonNone {
			t.Fatalf("F6: bare %d must be ReasonNone, got %v", resp.StatusCode, got.Reason)
		}
	}
}
