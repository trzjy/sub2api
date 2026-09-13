package service

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyCodeBuddyError_BalanceExhausted(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{http.StatusPaymentRequired, `{"code":0}`},
		{http.StatusOK, `{"msg":"余额不足"}`},
		{http.StatusBadRequest, `{"msg":"insufficient credit"}`},
	}
	for _, c := range cases {
		if got := ClassifyCodeBuddyError(c.status, []byte(c.body)); got != CodeBuddyErrKindBalanceExhausted {
			t.Fatalf("status=%d body=%q: expected BalanceExhausted, got %s", c.status, c.body, got)
		}
	}
}

func TestClassifyCodeBuddyError_SessionDead(t *testing.T) {
	// 12153 或 Offline user session not found → SessionDead。
	if got := ClassifyCodeBuddyError(http.StatusBadRequest, []byte(`{"code":12153,"msg":"x"}`)); got != CodeBuddyErrKindSessionDead {
		t.Fatalf("expected SessionDead, got %s", got)
	}
	if got := ClassifyCodeBuddyError(http.StatusOK, []byte(`Offline user session not found`)); got != CodeBuddyErrKindSessionDead {
		t.Fatalf("expected SessionDead, got %s", got)
	}
}

func TestClassifyCodeBuddyError_ModelLimitBeforeSoftLimit(t *testing.T) {
	// 关键顺序：429 + code 6004 且 body 同时含限流文案 → 必须判模型级（而非账号级）。
	body := `{"code":6004,"msg":"请求过于频繁，请稍后再试"}`
	if got := ClassifyCodeBuddyError(http.StatusTooManyRequests, []byte(body)); got != CodeBuddyErrKindModelLimit {
		t.Fatalf("expected ModelLimit (6004 must precede rate-limit text), got %s", got)
	}
}

func TestClassifyCodeBuddyError_SessionDeadBeforeSoftLimit(t *testing.T) {
	// 关键顺序：12153 与 rate limit 文案混排 → 判 session 死亡（宁可判死也不留死号）。
	body := `{"code":12153,"msg":"rate limit exceeded, session not found"}`
	if got := ClassifyCodeBuddyError(http.StatusTooManyRequests, []byte(body)); got != CodeBuddyErrKindSessionDead {
		t.Fatalf("expected SessionDead (12153 precede rate-limit), got %s", got)
	}
}

func TestClassifyCodeBuddyError_ContentAudit(t *testing.T) {
	body := `{"code":400,"msg":"blocked by security policy"}`
	if got := ClassifyCodeBuddyError(http.StatusBadRequest, []byte(body)); got != CodeBuddyErrKindContentAudit {
		t.Fatalf("expected ContentAudit, got %s", got)
	}
	body2 := `illegal api invocation detected`
	if got := ClassifyCodeBuddyError(http.StatusBadRequest, []byte(body2)); got != CodeBuddyErrKindContentAudit {
		t.Fatalf("expected ContentAudit, got %s", got)
	}
}

func TestClassifyCodeBuddyError_RequestBody(t *testing.T) {
	body := `{"code":11101,"msg":"x"}`
	if got := ClassifyCodeBuddyError(http.StatusBadRequest, []byte(body)); got != CodeBuddyErrKindRequestBody {
		t.Fatalf("expected RequestBody, got %s", got)
	}
	body2 := `Unmarshal chat params failed`
	if got := ClassifyCodeBuddyError(http.StatusBadRequest, []byte(body2)); got != CodeBuddyErrKindRequestBody {
		t.Fatalf("expected RequestBody, got %s", got)
	}
}

func TestClassifyCodeBuddyError_AccountSoftLimit(t *testing.T) {
	// 裸 429 → 账号级软限流。
	if got := ClassifyCodeBuddyError(http.StatusTooManyRequests, []byte(`{}`)); got != CodeBuddyErrKindAccountSoftLimit {
		t.Fatalf("expected AccountSoftLimit, got %s", got)
	}
	// 非 429 但含 rate limit 文案（覆盖 200+code 11140 等非 429 限流语义）。
	if got := ClassifyCodeBuddyError(http.StatusOK, []byte(`rate limit exceeded`)); got != CodeBuddyErrKindAccountSoftLimit {
		t.Fatalf("expected AccountSoftLimit, got %s", got)
	}
}

func TestClassifyCodeBuddyError_UpstreamFault(t *testing.T) {
	// 5xx / 404 且无软限流文案 → 上游故障（不罚账号）。
	if got := ClassifyCodeBuddyError(http.StatusNotFound, []byte(`{}`)); got != CodeBuddyErrKindUpstreamFault {
		t.Fatalf("expected UpstreamFault for 404, got %s", got)
	}
	if got := ClassifyCodeBuddyError(http.StatusInternalServerError, []byte(`database connection refused`)); got != CodeBuddyErrKindUpstreamFault {
		t.Fatalf("expected UpstreamFault for 5xx (no rate-limit text), got %s", got)
	}
}

func TestClassifyCodeBuddyError_SoftLimitBeforeUpstreamFault(t *testing.T) {
	// 严格表序：软限流文案优先于状态码判定，覆盖「5xx + 限流文案」场景（参照实现 issue #28）。
	// 500 + "rate limit" → 账号级软限流（而非上游故障）。
	if got := ClassifyCodeBuddyError(http.StatusInternalServerError, []byte(`rate limit exceeded`)); got != CodeBuddyErrKindAccountSoftLimit {
		t.Fatalf("expected AccountSoftLimit for 5xx+rate-limit (soft-limit text precedes status), got %s", got)
	}
}

func TestClassifyCodeBuddyError_None(t *testing.T) {
	if got := ClassifyCodeBuddyError(http.StatusBadRequest, []byte(`{"code":0,"msg":"some random error"}`)); got != CodeBuddyErrKindNone {
		t.Fatalf("expected None, got %s", got)
	}
}

func TestParseCodeBuddyResetTime_UTC8(t *testing.T) {
	body := `将在 2026-09-13 12:00:00 重置，请稍后再试`
	got, ok := parseCodeBuddyResetTime([]byte(body))
	if !ok {
		t.Fatalf("expected parse success")
	}
	// 固定按 UTC+8 解释：本地等价 04:00:00 UTC。
	want := time.Date(2026, 9, 13, 12, 0, 0, 0, codeBuddyResetTimeLocation)
	if !got.Equal(want) {
		t.Fatalf("expected %s, got %s", want, got)
	}
	// 确认时区为 UTC+8（即 04:00:00 UTC）。
	if got.UTC().Hour() != 4 {
		t.Fatalf("expected 04:00 UTC (UTC+8 12:00), got %s", got.UTC())
	}

	if _, ok := parseCodeBuddyResetTime([]byte(`no reset marker here`)); ok {
		t.Fatalf("expected parse failure for missing marker")
	}
}

// TestClassifyCodeBuddyError_Phase0NewCodes 覆盖 Phase 0 校准新增码（D7）：
// 11128（首条须 system）与 11102（模型无效/无权限）均归为「请求/模型问题、不罚账号」。
func TestClassifyCodeBuddyError_Phase0NewCodes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"11128 first message is not system prompt", `{"code":11128,"msg":"first message is not system prompt"}`},
		{"11102 model service info not found", `{"code":11102,"msg":"model [gpt-5] service info not found"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyCodeBuddyError(http.StatusBadRequest, []byte(tc.body))
			if got != CodeBuddyErrKindRequestBody {
				t.Fatalf("expected RequestBody, got %s", got)
			}
		})
	}
}
