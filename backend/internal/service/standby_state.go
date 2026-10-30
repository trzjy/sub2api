package service

import "time"

// StandbyPhase 是（模型级）备用开关的相位。开启态表示该模型的备用渠道被置于候选首位。
type StandbyPhase int

const (
	// StandbyPhaseClosed 常态：主力通道，备用靠 priority 沉底兜底。
	StandbyPhaseClosed StandbyPhase = iota
	// StandbyPhaseOpen 备用已开启。
	StandbyPhaseOpen
)

func (p StandbyPhase) String() string {
	if p == StandbyPhaseOpen {
		return "open"
	}
	return "closed"
}

// StandbyTransition 是一次 Observe 的结果。Changed 只在真正迁移时为 true。
type StandbyTransition struct {
	From    StandbyPhase
	To      StandbyPhase
	Verdict StandbyVerdict
	Changed bool
	// Since 是当前待确认 verdict 的首次出现时刻；已迁移时为零值。
	Since time.Time
}

// StandbyStateMachine 是进程内的判定状态机（L5 / L11）：
//
//	closed + Degraded        连续 5 分钟 → open
//	closed + Healthy/Inconclusive        → closed（并重置计时）
//	open   + Healthy         连续 5 分钟 → closed
//	open   + Degraded/Inconclusive       → open（并重置计时）
//
// 计时按"verdict 连续不变"累计：中途 verdict 变化即从新 verdict 的出现时刻重新计时。
// Inconclusive 不迁移，但它同样是一次 verdict 变化，会重置计时。
//
// 状态机本身不读时钟：时刻由调用方传入，保证纯逻辑可测。
type StandbyStateMachine struct {
	phase   StandbyPhase
	confirm time.Duration

	pendingVerdict StandbyVerdict
	pendingSince   time.Time
	hasPending     bool
}

// NewStandbyStateMachine 构造状态机。confirm <= 0 时使用 StandbyConfirmDuration。
func NewStandbyStateMachine(confirm time.Duration) *StandbyStateMachine {
	if confirm <= 0 {
		confirm = StandbyConfirmDuration
	}
	return &StandbyStateMachine{confirm: confirm}
}

// Phase 返回当前相位。
func (m *StandbyStateMachine) Phase() StandbyPhase {
	if m == nil {
		return StandbyPhaseClosed
	}
	return m.phase
}

// Pending 返回正在累计的 verdict 及其首次出现时刻；没有待确认 verdict 时 ok=false。
func (m *StandbyStateMachine) Pending() (verdict StandbyVerdict, since time.Time, ok bool) {
	if m == nil || !m.hasPending {
		return StandbyVerdictInconclusive, time.Time{}, false
	}
	return m.pendingVerdict, m.pendingSince, true
}

// Observe 用一次判定结果推进状态机。now 为零值时不做时间推进（仅记录待确认 verdict）。
func (m *StandbyStateMachine) Observe(verdict StandbyVerdict, now time.Time) StandbyTransition {
	if m == nil {
		return StandbyTransition{From: StandbyPhaseClosed, To: StandbyPhaseClosed, Verdict: verdict}
	}
	if !m.hasPending || m.pendingVerdict != verdict {
		m.hasPending = true
		m.pendingVerdict = verdict
		m.pendingSince = now
		return StandbyTransition{From: m.phase, To: m.phase, Verdict: verdict, Since: now}
	}
	if verdict != m.actionableVerdict() {
		// 非迁移方向（含 Inconclusive）：只累计计时，永不迁移。
		return StandbyTransition{From: m.phase, To: m.phase, Verdict: verdict, Since: m.pendingSince}
	}
	if now.Sub(m.pendingSince) < m.confirm {
		return StandbyTransition{From: m.phase, To: m.phase, Verdict: verdict, Since: m.pendingSince}
	}

	from := m.phase
	if from == StandbyPhaseClosed {
		m.phase = StandbyPhaseOpen
	} else {
		m.phase = StandbyPhaseClosed
	}
	m.hasPending = false
	m.pendingVerdict = StandbyVerdictInconclusive
	m.pendingSince = time.Time{}
	return StandbyTransition{From: from, To: m.phase, Verdict: verdict, Changed: true}
}

// actionableVerdict 是当前相位下唯一能触发迁移的 verdict。
func (m *StandbyStateMachine) actionableVerdict() StandbyVerdict {
	if m.phase == StandbyPhaseClosed {
		return StandbyVerdictDegraded
	}
	return StandbyVerdictHealthy
}
