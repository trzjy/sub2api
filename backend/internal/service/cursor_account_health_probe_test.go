//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestBuildCursorAccountHealthBreakerState 断言状态构造函数的字段口径与
// pool401EscalationState 同型：MatchedKeyword 为本卡 marker、Tier=3(HealthBreakerTierTrip)、
// RuleIndex=-1、UntilUnix=9999 哨兵、TriggeredAtUnix=now、StatusCode/ErrorMessage/Model 透传。
func TestCursorAccountHealthProbe_BuildState(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const statusCode = 502
	const errMsg = "cursor upstream status 502 (some upstream error)"
	const model = "claude-opus-4"

	state := BuildCursorAccountHealthBreakerState(now, statusCode, errMsg, model)
	if state == nil {
		t.Fatal("BuildCursorAccountHealthBreakerState returned nil")
	}

	if state.MatchedKeyword != cursorAccountHealthBreakerMarker {
		t.Errorf("MatchedKeyword = %q, want %q", state.MatchedKeyword, cursorAccountHealthBreakerMarker)
	}
	// Done when #3：Tier == HealthBreakerTierTrip (3)
	if state.Tier != HealthBreakerTierTrip {
		t.Errorf("Tier = %d, want %d (HealthBreakerTierTrip)", state.Tier, HealthBreakerTierTrip)
	}
	// Done when #3：RuleIndex == -1
	if state.RuleIndex != -1 {
		t.Errorf("RuleIndex = %d, want -1", state.RuleIndex)
	}
	// Done when #3/#5：UntilUnix == 9999 哨兵
	if state.UntilUnix != tempUnschedIndefiniteUntil.Unix() {
		t.Errorf("UntilUnix = %d, want 9999 sentinel %d", state.UntilUnix, tempUnschedIndefiniteUntil.Unix())
	}
	if got := time.Unix(state.UntilUnix, 0).UTC().Year(); got != 9999 {
		t.Errorf("UntilUnix year = %d, want 9999", got)
	}
	if state.TriggeredAtUnix != now.Unix() {
		t.Errorf("TriggeredAtUnix = %d, want %d", state.TriggeredAtUnix, now.Unix())
	}
	if state.StatusCode != statusCode {
		t.Errorf("StatusCode = %d, want %d", state.StatusCode, statusCode)
	}
	if state.ErrorMessage != errMsg {
		t.Errorf("ErrorMessage = %q, want %q", state.ErrorMessage, errMsg)
	}
	if state.Model != model {
		t.Errorf("Model = %q, want %q", state.Model, model)
	}

	// 复用既有 json.Marshal 用法：序列化后 marker 必须原样落进 matched_keyword 字段。
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal(state) failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if decoded["matched_keyword"] != cursorAccountHealthBreakerMarker {
		t.Errorf("serialized matched_keyword = %v, want %q", decoded["matched_keyword"], cursorAccountHealthBreakerMarker)
	}
	if decoded["tier"] != float64(HealthBreakerTierTrip) {
		t.Errorf("serialized tier = %v, want %d", decoded["tier"], HealthBreakerTierTrip)
	}
	if decoded["rule_index"] != float64(-1) {
		t.Errorf("serialized rule_index = %v, want -1", decoded["rule_index"])
	}
}

// TestBuildCursorAccountHealthBreakerState_ZeroNow 回落 time.Now()：TriggeredAtUnix
// 必须与"调用时刻"基本一致（允许 1 秒容差）。
func TestCursorAccountHealthProbe_BuildStateZeroNow(t *testing.T) {
	before := time.Now().Unix()
	state := BuildCursorAccountHealthBreakerState(time.Time{}, 500, "x", "m")
	after := time.Now().Unix()
	if state.TriggeredAtUnix < before || state.TriggeredAtUnix > after {
		t.Errorf("TriggeredAtUnix = %d, expected within [%d, %d]", state.TriggeredAtUnix, before, after)
	}
	if state.UntilUnix != tempUnschedIndefiniteUntil.Unix() {
		t.Errorf("UntilUnix = %d, want 9999 sentinel", state.UntilUnix)
	}
}

// TestProbeCursorAccountHealth_NotCursorConnectSessionAccount 覆盖 Done when #5：
// 账号不是 cursor Connect 会话型时，封装必须安全地失败关闭（err != nil），且不得在
// exec 为零值时解引用（不发起出站请求）。
func TestCursorAccountHealthProbe_NotCursorConnectSessionAccount(t *testing.T) {
	cases := []struct {
		name    string
		account *Account
	}{
		{
			name:    "nil account",
			account: nil,
		},
		{
			name:    "non-cursor platform",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		},
		{
			name:    "cursor but APIKey (OpenAI-compatible reverse proxy, not Connect session)",
			account: &Account{Platform: PlatformCursor, Type: AccountTypeAPIKey},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// exec 故意传 nil：若封装在账号校验前解引用 exec 会 panic，本用例同时验证顺序。
			status, detail, text, err := ProbeCursorAccountHealth(
				context.Background(), nil, tc.account, "claude-opus-4", cursorAccountHealthProbeMinBudget,
			)
			if err == nil {
				t.Fatalf("expected error for non-cursor-Connect-session account, got nil (status=%d detail=%q text=%q)", status, detail, text)
			}
			if !errors.Is(err, nil) && status != 0 {
				t.Errorf("on safe-close failure status should be 0, got %d", status)
			}
			if detail != "" || text != "" {
				t.Errorf("on safe-close failure detail/text should be empty, got detail=%q text=%q", detail, text)
			}
		})
	}
}
