//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---- Web 控制台探测链独立预算（CARD-WCQ-CYCLE）----
//
// 缺陷背景：主预算按每账号 8s 估算 Web 控制台探测，但单账号真实上限是
// cfg.WebQuotaWorker.TimeoutSeconds（生产 60s）+ HTTP grace。串行 13 个账号
// ≈ 780s 远超主预算 300s 上限，排在后面的账号每轮都在 ctx 取消时被中断（确定性饿死）。
// 修复后该链使用独立预算 ctx，以下用例锁定预算函数的边界。

func TestCNBalanceWebConsoleProbeBudget(t *testing.T) {
	t.Run("zero accounts no budget", func(t *testing.T) {
		require.Equal(t, time.Duration(0), cnBalanceWebConsoleProbeBudget(0, 60))
		require.Equal(t, time.Duration(0), cnBalanceWebConsoleProbeBudget(-1, 60))
	})

	t.Run("zero timeout falls back to 60s", func(t *testing.T) {
		// 2 × (60 + 10) = 140s
		require.Equal(t, 140*time.Second, cnBalanceWebConsoleProbeBudget(2, 0))
	})

	t.Run("thirteen accounts clamped to cap", func(t *testing.T) {
		// 13 × (60 + 10) = 910s > 9min，夹顶。
		require.Equal(t, cnBalanceWebConsoleBudgetCap, cnBalanceWebConsoleProbeBudget(13, 60))
		require.Equal(t, 9*time.Minute, cnBalanceWebConsoleProbeBudget(13, 60))
	})

	t.Run("two accounts with 30s timeout", func(t *testing.T) {
		// 2 × (30 + 10) = 80s
		require.Equal(t, 80*time.Second, cnBalanceWebConsoleProbeBudget(2, 30))
	})

	t.Run("three accounts with 60s timeout not clamped", func(t *testing.T) {
		// 3 × 70 = 210s < 9min
		require.Equal(t, 210*time.Second, cnBalanceWebConsoleProbeBudget(3, 60))
	})
}

// TestCNBalanceWebConsoleBudgetWithinCycle 锁定 CARD-WCQ-CYCLE2 的周期夹取：
// Web 控制台探测的独立预算必须落在「本轮剩余周期」内，避免与主预算叠加后
// 单轮跨过 10 分钟巡检间隔（tick 持续被丢弃、周期链背靠背运行）。
func TestCNBalanceWebConsoleBudgetWithinCycle(t *testing.T) {
	t.Run("interval unset does not clamp", func(t *testing.T) {
		// interval<=0（测试构造）时不夹：等于未夹取的预算。
		require.Equal(t,
			cnBalanceWebConsoleProbeBudget(13, 60),
			cnBalanceWebConsoleBudgetWithinCycle(0, 2*time.Minute, cnBalanceWebConsoleProbeBudget(13, 60)))
	})

	t.Run("clamped to remaining cycle minus tail guard", func(t *testing.T) {
		// 10m - 2m - 30s = 7m30s
		require.Equal(t, 7*time.Minute+30*time.Second,
			cnBalanceWebConsoleBudgetWithinCycle(10*time.Minute, 2*time.Minute, 9*time.Minute))
	})

	t.Run("exhausted cycle yields non-positive", func(t *testing.T) {
		// 10m - 9m50s - 30s = -20s：调用方据此跳过本轮并记日志。
		got := cnBalanceWebConsoleBudgetWithinCycle(10*time.Minute, 9*time.Minute+50*time.Second, 9*time.Minute)
		require.LessOrEqual(t, got, time.Duration(0))
		require.Equal(t, -20*time.Second, got)
	})

	t.Run("uncapped smaller than remaining is not inflated", func(t *testing.T) {
		// uncapped=3m < 剩余 7m30s：保持 3m，绝不放大。
		require.Equal(t, 3*time.Minute,
			cnBalanceWebConsoleBudgetWithinCycle(10*time.Minute, 2*time.Minute, 3*time.Minute))
	})
}
