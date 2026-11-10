//go:build unit

package repository

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// usageLogScanColumnIndex 返回 usageLogSelectColumns 中 name 的位置（0 基）。
func usageLogScanColumnIndex(t *testing.T, name string) int {
	t.Helper()
	for i, col := range strings.Split(usageLogSelectColumns, ",") {
		if strings.TrimSpace(col) == name {
			return i
		}
	}
	t.Fatalf("column %q not found in usageLogSelectColumns", name)
	return -1
}

// usageLogScanDestStub 记录 scanUsageLog 实际请求的 Scan 目标个数，
// 并为每个目标回填类型零值；对账两列注入非零值以验证映射。
// 不复用 usageLogScannerStub（request_type 测试），它严格校验
// dest 数量与预置 values 完全一致，耦合具体列数。
type usageLogScanDestStub struct {
	scanDestCount        int
	estimatedInputTokens int
	largeRequestReroute  bool
}

func (s *usageLogScanDestStub) Scan(dest ...any) error {
	s.scanDestCount = len(dest)
	estIdx := -1
	rerouteIdx := -1
	for i, col := range strings.Split(usageLogSelectColumns, ",") {
		switch strings.TrimSpace(col) {
		case "estimated_input_tokens":
			estIdx = i
		case "large_request_reroute":
			rerouteIdx = i
		}
	}
	for i := range dest {
		dv := reflect.ValueOf(dest[i])
		if dv.Kind() != reflect.Pointer {
			return &stubDestNotPointerError{index: i}
		}
		elem := dv.Elem()
		var v reflect.Value
		switch i {
		case estIdx:
			v = reflect.ValueOf(s.estimatedInputTokens)
		case rerouteIdx:
			v = reflect.ValueOf(s.largeRequestReroute)
		default:
			v = reflect.Zero(elem.Type())
		}
		if !v.IsValid() || v.Type() != elem.Type() {
			return &stubDestTypeMismatchError{index: i, got: v.Type(), want: elem.Type()}
		}
		elem.Set(v)
	}
	return nil
}

type stubDestNotPointerError struct{ index int }

func (e *stubDestNotPointerError) Error() string {
	return "scan dest is not pointer"
}

type stubDestTypeMismatchError struct {
	index     int
	got, want reflect.Type
}

func (e *stubDestTypeMismatchError) Error() string {
	return "scan dest type mismatch"
}

// TestUsageLogQueryLargeRequestScanMapping 覆盖读侧链路把对账两列从行扫描落到
// service.UsageLog：estimated_input_tokens→EstimatedInputTokens、
// large_request_reroute→LargeRequestReroute。
func TestUsageLogQueryLargeRequestScanMapping(t *testing.T) {
	stub := &usageLogScanDestStub{
		estimatedInputTokens: 12345,
		largeRequestReroute:  true,
	}
	log, err := scanUsageLog(stub)
	require.NoError(t, err)
	require.Equal(t, 12345, log.EstimatedInputTokens,
		"estimated_input_tokens 必须映射到 UsageLog.EstimatedInputTokens")
	require.True(t, log.LargeRequestReroute,
		"large_request_reroute 必须映射到 UsageLog.LargeRequestReroute")
}

// TestUsageLogQueryLargeRequestZeroValues 覆盖未估算、未重路由时读侧回退零值。
func TestUsageLogQueryLargeRequestZeroValues(t *testing.T) {
	stub := &usageLogScanDestStub{}
	log, err := scanUsageLog(stub)
	require.NoError(t, err)
	require.Zero(t, log.EstimatedInputTokens)
	require.False(t, log.LargeRequestReroute)
}

// TestUsageLogQueryLargeRequestSelectScanCountsConsistent 是列清单一致性测试：
// SELECT 列清单（usageLogSelectColumns）中的列数与 scanUsageLog 请求的
// Scan 目标数量必须相等，且对账两列在列清单中的位置与 Scan 落位一致。
func TestUsageLogQueryLargeRequestSelectScanCountsConsistent(t *testing.T) {
	cols := strings.Split(usageLogSelectColumns, ",")
	estIdx := usageLogScanColumnIndex(t, "estimated_input_tokens")
	rerouteIdx := usageLogScanColumnIndex(t, "large_request_reroute")
	require.Less(t, estIdx, len(cols))
	require.Less(t, rerouteIdx, len(cols))

	// 两列顺序与 INSERT 侧列清单一致：long_context_billing_applied 与 channel_id 之间。
	longCtxIdx := usageLogScanColumnIndex(t, "long_context_billing_applied")
	channelIdx := usageLogScanColumnIndex(t, "channel_id")
	require.Less(t, longCtxIdx, estIdx)
	require.Less(t, estIdx, rerouteIdx)
	require.Less(t, rerouteIdx, channelIdx)

	// Scan 目标个数必须与 SELECT 列数相等（对账两列计入）。
	stub := &usageLogScanDestStub{}
	_, err := scanUsageLog(stub)
	require.NoError(t, err)
	require.Equal(t, len(cols), stub.scanDestCount,
		"SELECT 列数与 Scan 目标数必须一致")
}

// TestUsageLogQueryLargeRequestColumnPresentInSelect 兜底断言两列名称
// 显式出现在 SELECT 列清单中，防列清单被截断或手滑遗漏。
func TestUsageLogQueryLargeRequestColumnPresentInSelect(t *testing.T) {
	require.Contains(t, usageLogSelectColumns, "estimated_input_tokens")
	require.Contains(t, usageLogSelectColumns, "large_request_reroute")
}
