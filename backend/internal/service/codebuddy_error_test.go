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

// TestIsCodeBuddyContentSafetyError 覆盖内容安全审核拒绝检测。**只认审核语义**，
// 顶层 code 不再参与判定（2026-10-18 收窄：11140 已被上游复用为「账号异常行为限制」，
// 单凭 code 判定的旧口径会把账号级限制误判成请求级内容拒绝，账号留在池里被逐请求
// 重放，轮换全池后 502）。
func TestIsCodeBuddyContentSafetyError(t *testing.T) {
	positive := []string{
		// 生产实测形态：数字 code + displayMsg 中英双语。
		`{"code":11140,"msg":"request illegal","requestId":"abc","displayMsg":{"en":"The content did not pass the safety review...","zh":"内容未通过安全审核，请调整后重试。"}}`,
		// 仅 displayMsg 中文命中。
		`{"code":0,"displayMsg":{"zh":"内容未通过安全审核，请调整后重试。"}}`,
		// 仅 displayMsg 英文命中（大小写不敏感）。
		`{"code":0,"displayMsg":{"en":"Content did not pass the SAFETY REVIEW"}}`,
	}
	for _, body := range positive {
		if !isCodeBuddyContentSafetyError([]byte(body)) {
			t.Fatalf("expected true for body %q", body)
		}
	}

	negative := []string{
		`{"code":429,"msg":"rate limit"}`,
		`{"code":11128,"msg":"first message is not system prompt"}`,
		`{"error":{"message":"temporary edge rejection"}}`, // 无 displayMsg 的普通 403 结构
		`{"code":"not-a-number"}`,
		`plain text error page`,
		``,
		// 语义变更：由正例移入反例。仅顶层 code==11140 且无任何审核语义，不再判定为
		// 内容审核——该形态交由 isCodeBuddyAccountRestrictedError 按账号限制处理。
		`{"code":"11140","msg":"request illegal"}`,
		`{"code":11140,"msg":"request illegal"}`,
		// 生产原文形态（2026-10-18，账号 258/259/285/286/287）：账号级限制，无审核语义。
		`{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity","zh":"账号存在异常行为，已被限制使用"},"displayTips":{"zh":"系统检测到该账号存在异常使用行为，如需申诉请提交反馈"}}`,
	}
	for _, body := range negative {
		if isCodeBuddyContentSafetyError([]byte(body)) {
			t.Fatalf("expected false for body %q", body)
		}
	}
}

// codeBuddyAccountRestricted403Body 是 2026-10-18 生产实测的账号限制原文
// （账号 258/259/285/286/287：/v1/messages 恒返回 403 + 顶层 code=11140）。
const codeBuddyAccountRestricted403Body = `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity","zh":"账号存在异常行为，已被限制使用"},"displayTips":{"zh":"系统检测到该账号存在异常使用行为，如需申诉请提交反馈"}}`

func TestIsCodeBuddyAccountRestrictedError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "403 + 11140 + 账号限制文案 → true",
			status: http.StatusForbidden,
			body:   codeBuddyAccountRestricted403Body,
			want:   true,
		},
		{
			name:   "403 + 字符串码 11140 + 无审核语义 → true",
			status: http.StatusForbidden,
			body:   `{"code":"11140","msg":"request illegal"}`,
			want:   true,
		},
		{
			name:   "403 + 11140 + safety review → false（内容审核，非账号限制）",
			status: http.StatusForbidden,
			body:   `{"code":11140,"msg":"request illegal","displayMsg":{"en":"The content did not pass the safety review","zh":"内容未通过安全审核，请调整后重试。"}}`,
			want:   false,
		},
		{
			name:   "403 + 11140 + 中文安全审核 → false",
			status: http.StatusForbidden,
			body:   `{"code":11140,"msg":"request illegal","displayMsg":{"zh":"内容未通过安全审核，请调整后重试。"}}`,
			want:   false,
		},
		{
			name:   "非 403（400）+ 11140 + 限制文案 → false",
			status: http.StatusBadRequest,
			body:   codeBuddyAccountRestricted403Body,
			want:   false,
		},
		{
			name:   "非 403（429）+ 限制文案 → false",
			status: http.StatusTooManyRequests,
			body:   codeBuddyAccountRestricted403Body,
			want:   false,
		},
		{
			name:   "403 + code 不等于 11140 → false",
			status: http.StatusForbidden,
			body:   `{"code":11128,"msg":"first message is not system prompt"}`,
			want:   false,
		},
		{
			name:   "403 + 非 CodeBuddy 信封 → false",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"temporary edge rejection"}}`,
			want:   false,
		},
		{
			name:   "403 + 空体 → false",
			status: http.StatusForbidden,
			body:   ``,
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCodeBuddyAccountRestrictedError(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("expected %v, got %v (body=%q)", tc.want, got, tc.body)
			}
		})
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
// 11133（请求参数被模型提供方拒绝，上游 extError.code=400002）同类，一并钉住
// （2026-09-14 实测：codex-tui /responses 请求携带 Responses 特有字段时上游返回该码）。
func TestClassifyCodeBuddyError_Phase0NewCodes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"11128 first message is not system prompt", `{"code":11128,"msg":"first message is not system prompt"}`},
		{"11102 model service info not found", `{"code":11102,"msg":"model [gpt-5] service info not found"}`},
		{"11133 request parameters rejected by model provider", `{"code":11133,"msg":"the request parameters were rejected by the model provider","extError":{"code":"400002"}}`},
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
