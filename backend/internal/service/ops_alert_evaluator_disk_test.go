package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestComputeRuleMetric_DiskUsagePercent verifies the disk_usage_percent metric
// reads the value from the latest system metrics snapshot (disk-guard plan
// 2026-09-26): present snapshot value => ok=true; nil snapshot value or missing
// snapshot => ok=false. The disk case returns before touching the ops repo, so
// the evaluator under test needs no repository stub.
func TestComputeRuleMetric_DiskUsagePercent(t *testing.T) {
	t.Parallel()

	svc := &OpsAlertEvaluatorService{}
	rule := &OpsAlertRule{MetricType: "disk_usage_percent"}
	ctx := context.Background()
	start := time.Now().UTC().Add(-5 * time.Minute)
	end := time.Now().UTC()

	t.Run("快照含磁盘使用率: 返回该值", func(t *testing.T) {
		t.Parallel()

		sm := &OpsSystemMetricsSnapshot{DiskUsagePercent: float64Ptr(92.5)}
		val, ok := svc.computeRuleMetric(ctx, rule, sm, start, end, "", nil)
		require.True(t, ok)
		require.InDelta(t, 92.5, val, 0.0001)
	})

	t.Run("快照无磁盘数据: ok=false", func(t *testing.T) {
		t.Parallel()

		sm := &OpsSystemMetricsSnapshot{}
		_, ok := svc.computeRuleMetric(ctx, rule, sm, start, end, "", nil)
		require.False(t, ok)
	})

	t.Run("无快照: ok=false", func(t *testing.T) {
		t.Parallel()

		_, ok := svc.computeRuleMetric(ctx, rule, nil, start, end, "", nil)
		require.False(t, ok)
	})

	t.Run("阈值比较: 86 > 85 触发，85 不触发", func(t *testing.T) {
		t.Parallel()

		require.True(t, compareMetric(86, ">", 85))
		require.False(t, compareMetric(85, ">", 85))
	})
}
