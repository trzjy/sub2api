package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ============================================================================
// D-QLM-011：DTO 百分比字段去 omitempty（0% 用量不得被吞）
// ============================================================================

// DTO 层直接断言：plan_used_pct=0 / used_pct=0 时 JSON 输出包含 "plan_used_pct":0
// 与 "used_pct":0（不缺失）；缺失（零值结构体不填 free-tier 组）时其余键不被伪造。
func TestCNProviderSnapshotOutput_ZeroPctSerialized(t *testing.T) {
	out := &CNProviderSnapshotOutput{
		HasPass:       boolPtr(true),
		PassName:      "Agent Pass",
		WindowDays:    7,
		PlanUsedPct:   0,
		PlanExhausted: false,
		UsedPct:       0,
		Exhausted:     false,
	}
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	body := string(encoded)
	require.Contains(t, body, `"plan_used_pct":0`, "0% 是官方合法值，plan_used_pct 不得被 omitempty 吞掉")
	require.Contains(t, body, `"used_pct":0`, "0% 是官方合法值，used_pct 不得被 omitempty 吞掉")
	require.Contains(t, body, `"plan_exhausted":false`)
	require.Contains(t, body, `"exhausted":false`)
	// 0 值显式输出与「无快照数据」（键缺失）可区分：未填的 WindowDays=0 仍被 omitempty 省略。
	emptyOut := &CNProviderSnapshotOutput{}
	emptyBody, err := json.Marshal(emptyOut)
	require.NoError(t, err)
	require.NotContains(t, string(emptyBody), `"window_days"`, "window_days=0（未填）应保持 omitempty 省略")
	require.NotContains(t, string(emptyBody), `"plan_used_pct":null`, "plan_used_pct 不得以 null 形态出现")
}

// 全链路（生产入口 → Probe → DTO）：free-tier 官方返回 0% 用量时，探测结果
// Snapshot JSON 里 plan_used_pct=0 / used_pct=0 必须显式出现（不被 omitempty 剥掉）。
func TestCNProviderQuotaService_TokenHarborZeroPctDTO(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	upstream := &tokenHarborFakeUpstream{server: fakeTH.server}
	repo := &tokenHarborRepoStub{}
	quotaSvc := NewCNProviderQuotaService(repo, nil, upstream, nil)
	thSvc := NewTokenHarborPassService(repo, nil, upstream)
	thSvc.baseURL = fakeTH.server.URL
	quotaSvc.SetTokenHarborPassService(thSvc)
	now := time.Now().UTC()
	thSvc.now = func() time.Time { return now }
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()
	// 窗口起点：官方 API 返回 0% 用量（0 是合法值，非 unknown）。
	fakeTH.setFreeTierBody(`{"reset_at":"2026-10-12T16:06:10.343487+00:00",` +
		`"plan":{"window_days":7},"plan_used_pct":0,"plan_exhausted":false,` +
		`"used_pct":0,"exhausted":false}`)
	repo.account = thProductionAccount(22)

	result, err := quotaSvc.QueryUsage(context.Background(), 22)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.NotNil(t, result.Snapshot)
	require.InDelta(t, 0, result.Snapshot.PlanUsedPct, 0.001)
	require.InDelta(t, 0, result.Snapshot.UsedPct, 0.001)
	encoded, err := json.Marshal(result.Snapshot)
	require.NoError(t, err)
	body := string(encoded)
	require.Contains(t, body, `"plan_used_pct":0`, "窗口起点 plan_used_pct=0 必须显式序列化")
	require.Contains(t, body, `"used_pct":0`, "窗口起点 used_pct=0 必须显式序列化")
	require.Contains(t, body, `"reset_at":`, "reset_at 仍随快照透传")
	require.False(t, strings.Contains(body, `"plan_used_pct":null`))
}
