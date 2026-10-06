package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// extractJSONKeys 将任意 DTO 序列化后提取其顶层 JSON 字段名集合。
// 匿名嵌入结构体的字段会被 json 包提升为顶层字段，因此 UsageRiskReportDetail
// 的顶层 key 包含被嵌入 UsageRiskReportItem 的全部字段。
func extractJSONKeys(t *testing.T, v interface{}) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

// assertKeys 断言实际序列化出的 key 集合与期望集合逐字一致。
func assertKeys(t *testing.T, name string, actual map[string]bool, want []string) {
	t.Helper()
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	for _, k := range want {
		if !actual[k] {
			t.Errorf("%s: 期望 JSON 字段 %q 缺失", name, k)
		}
	}
	for k := range actual {
		if !wantSet[k] {
			t.Errorf("%s: 出现未约定的 JSON 字段 %q", name, k)
		}
	}
}

// TestUsageRiskTypes 断言全部 DTO 的 JSON tag 与派发单/方案 §8 前端契约字段名逐字一致。
func TestUsageRiskTypes(t *testing.T) {
	ts := time.Now()

	// UsageRiskListFilter
	assertKeys(t, "UsageRiskListFilter", extractJSONKeys(t, UsageRiskListFilter{}), []string{
		"page", "page_size", "report_date", "level",
		"user_id", "group_id", "rule", "include_low",
	})

	// UsageRiskReportItem（RuleHits 复用 U4a RuleHit，其 tag 为 rule/scope/detail/points）
	assertKeys(t, "UsageRiskReportItem", extractJSONKeys(t, UsageRiskReportItem{}), []string{
		"report_id", "user_id", "username", "group_id", "group_name",
		"report_date", "score", "level", "rule_hits", "status", "invalidated_at",
	})

	// UsageRiskReportDetail（嵌入 Item 的字段被提升 + evidence + policy_version）
	detailKeys := extractJSONKeys(t, UsageRiskReportDetail{
		UsageRiskReportItem: UsageRiskReportItem{ReportID: 1, RuleHits: []RuleHit{{Rule: "R1"}}},
	})
	assertKeys(t, "UsageRiskReportDetail", detailKeys, []string{
		"report_id", "user_id", "username", "group_id", "group_name",
		"report_date", "score", "level", "rule_hits", "status", "invalidated_at",
		"evidence", "policy_version",
	})

	// UsageRiskReportList
	assertKeys(t, "UsageRiskReportList", extractJSONKeys(t, UsageRiskReportList{}), []string{
		"items", "total", "min_score",
	})

	// UsageRiskRunStatus
	assertKeys(t, "UsageRiskRunStatus", extractJSONKeys(t, UsageRiskRunStatus{WindowEnd: &ts}), []string{
		"status", "window_end", "consecutive_partials", "failed_batches",
		"history_covered", "recon_progress",
	})

	// UsageRiskTopUser
	assertKeys(t, "UsageRiskTopUser", extractJSONKeys(t, UsageRiskTopUser{}), []string{
		"user_id", "username", "max_score", "level", "top_rules",
	})

	// UsageRiskSummary（空 ByLevel 序列化为 null，key 仍应存在）
	assertKeys(t, "UsageRiskSummary", extractJSONKeys(t, UsageRiskSummary{Top: []UsageRiskTopUser{}}), []string{
		"open_total", "by_level", "top", "error",
	})
}

// TestUsageRiskDTORoundTrip 验证报告项/详情可正确往返序列化，且 RuleHit 嵌套结构保留。
func TestUsageRiskDTORoundTrip(t *testing.T) {
	item := UsageRiskReportItem{
		ReportID:   42,
		UserID:     7,
		Username:   "alice",
		GroupID:    3,
		GroupName:  "unlimited",
		ReportDate: "2026-09-25",
		Score:      88,
		Level:      "high",
		RuleHits:   []RuleHit{{Rule: "R1", Scope: "user", Detail: "20h active", Points: 25}},
		Status:     "open",
	}
	b, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	var got UsageRiskReportItem
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	if got.ReportID != 42 || got.Score != 88 || len(got.RuleHits) != 1 || got.RuleHits[0].Rule != "R1" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	detail := UsageRiskReportDetail{
		UsageRiskReportItem: item,
		Evidence:            json.RawMessage(`{"ip_top":[]}`),
		PolicyVersion:       "3",
	}
	db, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	var gotD UsageRiskReportDetail
	if err := json.Unmarshal(db, &gotD); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if gotD.PolicyVersion != "3" || string(gotD.Evidence) != `{"ip_top":[]}` || gotD.ReportID != 42 {
		t.Fatalf("detail round-trip mismatch: %+v", gotD)
	}
}

// TestUsageRiskDetailPolicyVersionIsHexString 是 H 组主锁定测试：
// 证明 PolicyVersion 字段在 JSON 中序列化为字符串（hex 小写）而非 JSON number，
// 从而不会在超 2^53 时静默失真（F1 位保真）。
func TestUsageRiskDetailPolicyVersionIsHexString(t *testing.T) {
	detail := UsageRiskReportDetail{
		UsageRiskReportItem: UsageRiskReportItem{ReportID: 1, RuleHits: []RuleHit{{Rule: "R1"}}},
		Evidence:            json.RawMessage(`{}`),
		PolicyVersion:       "a1b2c3d4e5f60718",
	}

	b, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}

	var got UsageRiskReportDetail
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if got.PolicyVersion != "a1b2c3d4e5f60718" {
		t.Fatalf("PolicyVersion 往返失真: got %q", got.PolicyVersion)
	}

	// 核心断言：序列化结果中 policy_version 必须是 string，而非 float64（JSON number）。
	var raw map[string]interface{}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	pv, ok := raw["policy_version"]
	if !ok {
		t.Fatalf("policy_version 字段缺失")
	}
	if _, isStr := pv.(string); !isStr {
		t.Fatalf("H/F1：policy_version 应为 string，实际类型 %T（值=%v），超 2^53 将失真", pv, pv)
	}
	require.Equal(t, "a1b2c3d4e5f60718", pv.(string))
}
