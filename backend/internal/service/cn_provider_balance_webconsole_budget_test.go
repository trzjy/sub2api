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
