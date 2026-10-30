//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var standbyT0 = time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)

func TestStandbyStateMachine_OpenAfterContinuousFiveMinutes(t *testing.T) {
	m := NewStandbyStateMachine(0) // 默认 5 分钟
	require.Equal(t, StandbyPhaseClosed, m.Phase())

	tr := m.Observe(StandbyVerdictDegraded, standbyT0)
	require.False(t, tr.Changed)
	require.Equal(t, StandbyPhaseClosed, m.Phase())

	tr = m.Observe(StandbyVerdictDegraded, standbyT0.Add(4*time.Minute+59*time.Second))
	require.False(t, tr.Changed, "未满 5 分钟不得迁移")

	tr = m.Observe(StandbyVerdictDegraded, standbyT0.Add(5*time.Minute))
	require.True(t, tr.Changed)
	require.Equal(t, StandbyPhaseClosed, tr.From)
	require.Equal(t, StandbyPhaseOpen, tr.To)
	require.Equal(t, StandbyPhaseOpen, m.Phase())

	_, _, ok := m.Pending()
	require.False(t, ok, "迁移后计时清零")
}

func TestStandbyStateMachine_CloseAfterContinuousFiveMinutesHealthy(t *testing.T) {
	m := NewStandbyStateMachine(StandbyConfirmDuration)
	m.Observe(StandbyVerdictDegraded, standbyT0)
	require.True(t, m.Observe(StandbyVerdictDegraded, standbyT0.Add(5*time.Minute)).Changed)

	tr := m.Observe(StandbyVerdictHealthy, standbyT0.Add(6*time.Minute))
	require.False(t, tr.Changed)
	tr = m.Observe(StandbyVerdictHealthy, standbyT0.Add(11*time.Minute))
	require.True(t, tr.Changed)
	require.Equal(t, StandbyPhaseClosed, m.Phase())
}

// 中途 verdict 变化即重置计时：Degraded 累计 3 分钟 → Healthy 1 分钟 → 再 Degraded 时
// 必须从新的 Degraded 出现时刻重新累计。
func TestStandbyStateMachine_VerdictChangeResetsTimer(t *testing.T) {
	m := NewStandbyStateMachine(StandbyConfirmDuration)

	m.Observe(StandbyVerdictDegraded, standbyT0)
	m.Observe(StandbyVerdictDegraded, standbyT0.Add(3*time.Minute))
	require.Equal(t, StandbyPhaseClosed, m.Phase())

	m.Observe(StandbyVerdictHealthy, standbyT0.Add(3*time.Minute+30*time.Second))
	// 回到 Degraded 时计时从 t=4min 重新开始
	m.Observe(StandbyVerdictDegraded, standbyT0.Add(4*time.Minute))

	require.False(t, m.Observe(StandbyVerdictDegraded, standbyT0.Add(8*time.Minute)).Changed,
		"累计 4 分钟，不足 5 分钟")
	require.Equal(t, StandbyPhaseClosed, m.Phase())

	require.True(t, m.Observe(StandbyVerdictDegraded, standbyT0.Add(9*time.Minute)).Changed)
	require.Equal(t, StandbyPhaseOpen, m.Phase())
}

// Inconclusive 一律不迁移：既不能在关闭态开启，也不能在开启态关闭。
func TestStandbyStateMachine_InconclusiveNeverMigrates(t *testing.T) {
	m := NewStandbyStateMachine(StandbyConfirmDuration)
	for i := 0; i < 10; i++ {
		tr := m.Observe(StandbyVerdictInconclusive, standbyT0.Add(time.Duration(i)*time.Minute))
		require.False(t, tr.Changed)
	}
	require.Equal(t, StandbyPhaseClosed, m.Phase())

	m.Observe(StandbyVerdictDegraded, standbyT0)
	require.True(t, m.Observe(StandbyVerdictDegraded, standbyT0.Add(5*time.Minute)).Changed)

	for i := 0; i < 10; i++ {
		tr := m.Observe(StandbyVerdictInconclusive, standbyT0.Add(time.Duration(6+i)*time.Minute))
		require.False(t, tr.Changed)
	}
	require.Equal(t, StandbyPhaseOpen, m.Phase())
}

// 开启态下 Healthy 连续 5 分钟才关闭；中途退化或 Inconclusive 都重置退出计时。
func TestStandbyStateMachine_ExitTimerResetsOnAnyChange(t *testing.T) {
	m := NewStandbyStateMachine(StandbyConfirmDuration)
	m.Observe(StandbyVerdictDegraded, standbyT0)
	require.True(t, m.Observe(StandbyVerdictDegraded, standbyT0.Add(5*time.Minute)).Changed)

	m.Observe(StandbyVerdictHealthy, standbyT0.Add(10*time.Minute))
	m.Observe(StandbyVerdictHealthy, standbyT0.Add(13*time.Minute))
	// 出现 Inconclusive（样本枯竭）→ 退出计时作废，且不得迁移
	require.False(t, m.Observe(StandbyVerdictInconclusive, standbyT0.Add(13*time.Minute+30*time.Second)).Changed)
	m.Observe(StandbyVerdictHealthy, standbyT0.Add(14*time.Minute))
	require.False(t, m.Observe(StandbyVerdictHealthy, standbyT0.Add(18*time.Minute)).Changed,
		"重新累计仅 4 分钟")
	require.Equal(t, StandbyPhaseOpen, m.Phase())
	require.True(t, m.Observe(StandbyVerdictHealthy, standbyT0.Add(19*time.Minute)).Changed)
	require.Equal(t, StandbyPhaseClosed, m.Phase())
}

// 关闭态下的 Healthy 只用于清零计时，不会产生任何迁移。
func TestStandbyStateMachine_ClosedHealthyKeepsClosed(t *testing.T) {
	m := NewStandbyStateMachine(StandbyConfirmDuration)
	require.False(t, m.Observe(StandbyVerdictHealthy, standbyT0).Changed)
	require.False(t, m.Observe(StandbyVerdictHealthy, standbyT0.Add(time.Hour)).Changed)
	require.Equal(t, StandbyPhaseClosed, m.Phase())
}

func TestStandbyStateMachine_NilSafety(t *testing.T) {
	var m *StandbyStateMachine
	require.Equal(t, StandbyPhaseClosed, m.Phase())
	tr := m.Observe(StandbyVerdictDegraded, standbyT0)
	require.False(t, tr.Changed)
	var m2 *StandbyStateMachine = NewStandbyStateMachine(-time.Minute)
	require.Equal(t, StandbyConfirmDuration, m2.confirm)
}
