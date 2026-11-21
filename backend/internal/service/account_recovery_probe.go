package service

import (
	"encoding/json"
	"strings"
	"time"
)

// 账号级停调恢复探测（批1）的状态载体。
//
// 唯一事实源是 accounts.extra["account_recovery_probe"]：
//
//	{
//	  "quota":     {"gen":123,"due":"...","mode":"echo|noecho|overload","kind":"quota|actual|both"},
//	  "transport": {"gen":124,"due":"...","mode":"transient|unavailable"},
//	  "migrated":  false
//	}
//
// accounts.probe_due_at 是派生列：恒等于两个分量 due 的最小值（无分量为 NULL）。
// 写入/顺延/清除都必须由同一条 UPDATE 先算出写后的 extra 再派生列值，禁止对列做
// LEAST 增量维护（否则顺延后列会残留过期时刻，候选查询空转）。
const (
	// AccountRecoveryProbeExtraKey 是 extra 中承载恢复探测状态的键。
	AccountRecoveryProbeExtraKey = "account_recovery_probe"

	// accountRecoveryProbeSource 是通用恢复拥有的停调来源标记，写入
	// temp_unschedulable_reason 的 JSON 与 extra 元数据中，供
	// ClearTempUnschedulableIfOwned 的来源约束与写入守卫使用。
	accountRecoveryProbeSource = "recovery_probe"

	// recoveryProbeComponentQuota 覆盖额度类停调（429 有/无回显、过载）。
	recoveryProbeComponentQuota = "quota"
	// recoveryProbeComponentTransport 覆盖共享凭据/传输类停调（503、5xx/502 等）。
	recoveryProbeComponentTransport = "transport"
)

// recoveryProbeComponent 是单个分量的探测排程。
type recoveryProbeComponent struct {
	Gen  int64  `json:"gen"`
	Due  string `json:"due"`
	Mode string `json:"mode,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// HasPendingTransportProbe 报告该账号是否仍有未确认恢复的 transport 类探测。
// 用于影子账号连坐判定：until 到期后、探测确认前，影子仍不可用。
func (a *Account) HasPendingTransportProbe() bool {
	if a == nil {
		return false
	}
	_, ok := a.recoveryProbeComponent(recoveryProbeComponentTransport)
	return ok
}

// RecoveryProbeComponent 返回指定分量（quota / transport）的排程快照。
// 第二返回值表示该分量是否存在且可解析。
func (a *Account) RecoveryProbeComponent(component string) (gen int64, due time.Time, mode, kind string, ok bool) {
	if a == nil {
		return 0, time.Time{}, "", "", false
	}
	raw, exists := a.recoveryProbeComponent(component)
	if !exists {
		return 0, time.Time{}, "", "", false
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw.Due))
	if err != nil {
		return 0, time.Time{}, raw.Mode, raw.Kind, false
	}
	return raw.Gen, t, raw.Mode, raw.Kind, true
}

// RecoveryProbeMigrated 报告该账号的探测状态是否由存量迁移写入。
func (a *Account) RecoveryProbeMigrated() bool {
	if a == nil {
		return false
	}
	snapshot := a.SnapshotExtra()
	raw, ok := snapshot[AccountRecoveryProbeExtraKey].(map[string]any)
	if !ok {
		return false
	}
	migrated, _ := raw["migrated"].(bool)
	return migrated
}

func (a *Account) recoveryProbeComponent(component string) (recoveryProbeComponent, bool) {
	if a == nil || component == "" {
		return recoveryProbeComponent{}, false
	}
	snapshot := a.SnapshotExtra()
	root, ok := snapshot[AccountRecoveryProbeExtraKey].(map[string]any)
	if !ok {
		return recoveryProbeComponent{}, false
	}
	raw, ok := root[component]
	if !ok {
		return recoveryProbeComponent{}, false
	}
	// extra 可能来自 JSONB 解码（map[string]any）或 Redis 快照（原始结构）：
	// 统一经 JSON 往返归一，避免各调用点各自断言类型。
	buf, err := json.Marshal(raw)
	if err != nil {
		return recoveryProbeComponent{}, false
	}
	var out recoveryProbeComponent
	if err := json.Unmarshal(buf, &out); err != nil {
		return recoveryProbeComponent{}, false
	}
	return out, true
}
