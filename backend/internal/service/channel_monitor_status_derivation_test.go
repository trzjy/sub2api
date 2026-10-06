//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------- 转换表（方案工作项 2 表，逐行） ----------

func TestDeriveChannelStatus_TransformationTable(t *testing.T) {
	now := time.Now()
	base := ChannelDerivationInput{ConfigBaseline: 120 * time.Second, Now: now}

	cases := []struct {
		name  string
		input func() ChannelDerivationInput
		want  string
	}{
		{
			// 账号级免费档 429 / 模型级限流 → no-op（不降渠道状态）。
			name: "model_rate_limited is no-op",
			input: func() ChannelDerivationInput {
				in := base
				in.AccountSideAnomalies = []AccountSideAnomaly{{Kind: "model_rate_limited", Active: true}}
				in.Observations = []ChannelObservation{{Status: MonitorStatusOperational, ObservedAt: now}}
				return in
			},
			want: MonitorStatusOperational, // 不降为 degraded/failed
		},
		{
			name: "free_tier_429 is no-op",
			input: func() ChannelDerivationInput {
				in := base
				in.AccountSideAnomalies = []AccountSideAnomaly{{Kind: "free_tier_429", Active: true}}
				in.Observations = []ChannelObservation{{Status: MonitorStatusOperational, ObservedAt: now}}
				return in
			},
			want: MonitorStatusOperational,
		},
		{
			// 账号级停调（temp-unschedulable/熔断）→ degraded。
			name: "temp_unschedulable is degraded",
			input: func() ChannelDerivationInput {
				in := base
				in.AccountSideAnomalies = []AccountSideAnomaly{{Kind: "temp_unschedulable", Active: true}}
				in.Observations = []ChannelObservation{{Status: MonitorStatusOperational, ObservedAt: now}}
				return in
			},
			want: MonitorStatusDegraded,
		},
		{
			name: "circuit_breaker is degraded",
			input: func() ChannelDerivationInput {
				in := base
				in.AccountSideAnomalies = []AccountSideAnomaly{{Kind: "circuit_breaker", Active: true}}
				in.Observations = []ChannelObservation{{Status: MonitorStatusOperational, ObservedAt: now}}
				return in
			},
			want: MonitorStatusDegraded,
		},
		{
			// 业务有效成功 → operational + 推进观测时间。
			name: "business success is operational and advances observed time",
			input: func() ChannelDerivationInput {
				in := base
				obsAt := now.Add(-time.Minute)
				in.Observations = []ChannelObservation{{Status: MonitorStatusOperational, ObservedAt: obsAt}}
				return in
			},
			want: MonitorStatusOperational,
		},
		{
			// 既有探测/业务传输硬故障 → error（沿用既有转换）。
			name: "probe hard failure is error",
			input: func() ChannelDerivationInput {
				in := base
				in.Observations = []ChannelObservation{{Status: MonitorStatusError, ObservedAt: now.Add(-time.Minute)}}
				return in
			},
			want: MonitorStatusError,
		},
		{
			// 额度无数据 → 空状态（横线），不伪装为 operational。
			name: "quota no-data yields empty state",
			input: func() ChannelDerivationInput {
				in := base
				in.Observations = []ChannelObservation{{
					Status: MonitorStatusFailed, ObservedAt: now, NoData: true, // 无数据产物
				}}
				return in
			},
			want: "", // #9：无权威观测 → 空状态
		},
		{
			// 无 latest 行且无账号侧事实 → 空状态。
			name: "no observations and no account side facts yields empty state",
			input: func() ChannelDerivationInput {
				return base
			},
			want: "",
		},
		{
			// 账号侧停调事实（无任何 latest）→ 仍为 degraded（账号侧事实是权威信号）。
			name: "account suspension alone is degraded not empty",
			input: func() ChannelDerivationInput {
				in := base
				in.AccountSideAnomalies = []AccountSideAnomaly{{Kind: "temp_unschedulable", Active: true}}
				return in
			},
			want: MonitorStatusDegraded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveChannelStatus(tc.input())
			require.Equal(t, tc.want, got.Status, "status mismatch")
		})
	}
}

// ---------- 输入生命周期（方案 C）：顺序测试 ----------

func TestDeriveChannelStatus_Lifecycle_DegradedPersistsDuringSuspension(t *testing.T) {
	now := time.Now()
	// ① 停调持续中出现业务成功 → 渠道保持 degraded。
	in := ChannelDerivationInput{
		ConfigBaseline:       120 * time.Second,
		Now:                  now,
		AccountSideAnomalies: []AccountSideAnomaly{{Kind: "temp_unschedulable", Active: true}},
		Observations:         []ChannelObservation{{Status: MonitorStatusOperational, ObservedAt: now}},
	}
	got := DeriveChannelStatus(in)
	require.Equal(t, MonitorStatusDegraded, got.Status, "business success must not lift a suspended channel")
	require.Equal(t, now, got.ObservedAt, "account anomaly change advances observed time to Now")
}

func TestDeriveChannelStatus_Lifecycle_OlderErrorRecoveredByNewerSuccess(t *testing.T) {
	now := time.Now()
	newer := now.Add(-1 * time.Minute)
	// ② 旧 error 后出现较新权威成功 → 渠道回升 operational。
	// E3 #5：跨模型不再「只留最新一行」，同模型键内的新事件替代旧事件由读路径
	// ListLatestPerModel 保证（每模型只返回当前行）。故纯函数输入即该模型键的当前行
	// （较新成功），此处以单条较新成功表达替代后的结果，覆盖不降低。
	in := ChannelDerivationInput{
		ConfigBaseline: 120 * time.Second,
		Now:            now,
		Observations: []ChannelObservation{
			{Status: MonitorStatusOperational, ObservedAt: newer},
		},
	}
	got := DeriveChannelStatus(in)
	require.Equal(t, MonitorStatusOperational, got.Status, "newer authoritative success must recover channel")
	require.Equal(t, newer, got.ObservedAt)
}

// ---------- E3 #5：跨模型最坏档位聚合（不再「只留最新一行」） ----------

func TestDeriveChannelStatus_WorstAcrossModels_FailedBeatsNewerOperational(t *testing.T) {
	now := time.Now()
	// 模型 A failed（较早）+ 模型 B operational（较晚）→ 渠道 failed。
	// 旧语义「只留时间最新一行」会把渠道错误提升为 operational；最坏聚合必须保持 failed。
	in := ChannelDerivationInput{
		ConfigBaseline: 120 * time.Second,
		Now:            now,
		Observations: []ChannelObservation{
			{Status: MonitorStatusFailed, ObservedAt: now.Add(-10 * time.Minute)},
			{Status: MonitorStatusOperational, ObservedAt: now.Add(-1 * time.Minute)},
		},
	}
	got := DeriveChannelStatus(in)
	require.Equal(t, MonitorStatusFailed, got.Status,
		"older failed model must not be lifted by newer operational model")
	// 观测时间取全部权威观测的最大值。
	require.Equal(t, now.Add(-1*time.Minute), got.ObservedAt,
		"observed time takes max across all authoritative observations")
}

func TestDeriveChannelStatus_WorstAcrossModels_ErrorBeatsAll(t *testing.T) {
	now := time.Now()
	in := ChannelDerivationInput{
		ConfigBaseline: 120 * time.Second,
		Now:            now,
		Observations: []ChannelObservation{
			{Status: MonitorStatusOperational, ObservedAt: now},
			{Status: MonitorStatusDegraded, ObservedAt: now.Add(-time.Minute)},
			{Status: MonitorStatusError, ObservedAt: now.Add(-2 * time.Minute)},
			{Status: MonitorStatusFailed, ObservedAt: now.Add(-3 * time.Minute)},
		},
	}
	got := DeriveChannelStatus(in)
	require.Equal(t, MonitorStatusError, got.Status, "error is the worst tier and must dominate")
	require.Equal(t, now, got.ObservedAt)
}

func TestDeriveChannelStatus_WorstAcrossModels_NoDataDoesNotMaskFailed(t *testing.T) {
	now := time.Now()
	// 一条真实 failed + 一条无数据（NoData）：无数据不参与，渠道仍 failed。
	in := ChannelDerivationInput{
		ConfigBaseline: 120 * time.Second,
		Now:            now,
		Observations: []ChannelObservation{
			{Status: MonitorStatusFailed, ObservedAt: now.Add(-time.Minute)},
			{Status: MonitorStatusFailed, ObservedAt: now, NoData: true},
		},
	}
	got := DeriveChannelStatus(in)
	require.Equal(t, MonitorStatusFailed, got.Status)
	require.Equal(t, now.Add(-time.Minute), got.ObservedAt,
		"no-data observation must not advance observed time")
}

// ---------- 无数据 → 空状态 + 不推进观测时间（方案 D / #9） ----------

func TestChannelObservation_NoDataNoOp(t *testing.T) {
	now := time.Now()
	in := ChannelDerivationInput{
		ConfigBaseline: 120 * time.Second,
		Now:            now,
		Observations: []ChannelObservation{
			{Status: MonitorStatusFailed, ObservedAt: now, NoData: true},
		},
	}
	got := DeriveChannelStatus(in)
	require.Equal(t, "", got.Status, "no-data must yield empty state (dash)")
	require.True(t, got.ObservedAt.IsZero(), "no-data must not advance observed time")
	require.Equal(t, now, got.LastAttemptAt, "no-data still records attempt time")
	require.False(t, got.AlertEligible, "empty state must not be alert-eligible")
}

// ---------- 阈值（方案 F）：空集合 = 基线 ----------

func TestComputeChannelFreshnessThreshold_EmptySetIsBaseline(t *testing.T) {
	// 空集合（无账号异常候选，ub=0）→ 返回配置基线，不退化为零值。
	require.Equal(t, 120*time.Second, ComputeChannelFreshnessThreshold(120*time.Second, 0))
	// 渠道 error 且无账号异常候选：同源计算，仍返回基线。
	require.Equal(t, 120*time.Second, ComputeChannelFreshnessThreshold(120*time.Second, 0))
	// 有账号侧最坏上界且更大 → 取上界。
	require.Equal(t, 300*time.Second, ComputeChannelFreshnessThreshold(120*time.Second, 300*time.Second))
	// 上界小于基线 → 取基线（max）。
	require.Equal(t, 120*time.Second, ComputeChannelFreshnessThreshold(120*time.Second, 60*time.Second))
}

// ---------- 告警生命周期（方案 G） ----------

func TestChannelAlertEligible(t *testing.T) {
	for _, s := range []string{MonitorStatusDegraded, MonitorStatusFailed, MonitorStatusError} {
		require.True(t, ChannelAlertEligible(s), s)
	}
	require.False(t, ChannelAlertEligible(MonitorStatusOperational))
}

func TestChannelAlertClosedOnRecovery(t *testing.T) {
	// 状态回升（error→operational）原子关闭告警。
	require.True(t, ChannelAlertClosedOnRecovery(MonitorStatusError, MonitorStatusOperational))
	require.True(t, ChannelAlertClosedOnRecovery(MonitorStatusDegraded, MonitorStatusOperational))
	// 未回升（保持可告警状态）不触发关闭。
	require.False(t, ChannelAlertClosedOnRecovery(MonitorStatusDegraded, MonitorStatusFailed))
	// operational 间无告警可关。
	require.False(t, ChannelAlertClosedOnRecovery(MonitorStatusOperational, MonitorStatusOperational))
}

// ---------- 存量迁移（方案 E）：完成门禁 ----------

func TestMigrateLegacyChannelFailed(t *testing.T) {
	t.Run("judgable source cleared and recomputed", func(t *testing.T) {
		rec := []LegacyChannelStatusRecord{
			{MonitorID: 1, Model: "a", Status: MonitorStatusFailed, EvidenceNoData: true},
		}
		res := MigrateLegacyChannelFailed(rec)
		require.True(t, res.Judgable)
		require.Len(t, res.Cleared, 1)
		require.Empty(t, res.Preserved)
	})

	t.Run("indeterminable source preserved and gates completion", func(t *testing.T) {
		rec := []LegacyChannelStatusRecord{
			{MonitorID: 1, Model: "a", Status: MonitorStatusFailed, EvidenceNoData: false},
		}
		res := MigrateLegacyChannelFailed(rec)
		// 不可判定：不得清除，judgable=false。
		require.False(t, res.Judgable, "indeterminable source must set judgable=false")
		require.Empty(t, res.Cleared, "must not clear indeterminable failed")
		require.Len(t, res.Preserved, 1)
	})

	t.Run("mixed: judgable cleared, indeterminable preserved, gated", func(t *testing.T) {
		rec := []LegacyChannelStatusRecord{
			{MonitorID: 1, Model: "a", Status: MonitorStatusFailed, EvidenceNoData: true},
			{MonitorID: 1, Model: "b", Status: MonitorStatusFailed, EvidenceNoData: false},
		}
		res := MigrateLegacyChannelFailed(rec)
		require.False(t, res.Judgable, "presence of indeterminable source gates completion")
		require.Len(t, res.Cleared, 1)
		require.Len(t, res.Preserved, 1)
		require.Equal(t, "a", res.Cleared[0].Model)
		require.Equal(t, "b", res.Preserved[0].Model)
	})

	t.Run("non-failed records preserved untouched", func(t *testing.T) {
		rec := []LegacyChannelStatusRecord{
			{MonitorID: 1, Model: "a", Status: MonitorStatusOperational},
			{MonitorID: 1, Model: "b", Status: MonitorStatusError},
		}
		res := MigrateLegacyChannelFailed(rec)
		require.True(t, res.Judgable)
		require.Empty(t, res.Cleared)
		require.Len(t, res.Preserved, 2, "non-failed records stay preserved")
	})
}

// ---------- 运行期无数据判别（方案 B）：runCheckForModel 两分支 ----------

func TestRunCheckForModel_ReplaceEmptyTextIsNoData(t *testing.T) {
	h := &captureHandler{respondText: ""} // 上游 2xx 但文本为空
	endpoint := setupFakeAnthropic(t, h)

	opts := &CheckOptions{
		BodyOverrideMode: MonitorBodyOverrideModeReplace,
		BodyOverride:     map[string]any{"model": "x", "messages": []any{}},
	}
	res := runCheckForModel(context.Background(), MonitorProviderAnthropic, endpoint, "sk-fake", "claude-x", opts)

	require.Equal(t, MonitorStatusFailed, res.Status, "per-model status stays failed (enum unchanged)")
	require.True(t, res.NoData, "no-data marker must be set for empty 2xx text")
}

func TestRunCheckForModel_ChallengeMismatchIsRealFailed(t *testing.T) {
	// 上游返回与 challenge 不匹配的文本 → 真实 failed，NoData=false。
	h := &captureHandler{respondText: "wrong-answer"}
	endpoint := setupFakeAnthropic(t, h)

	res := runCheckForModel(context.Background(), MonitorProviderAnthropic, endpoint, "sk-fake", "claude-x", nil)

	require.Equal(t, MonitorStatusFailed, res.Status)
	require.False(t, res.NoData, "challenge mismatch is a real failure, not no-data")
}

// ---------- E3 #4：NoData 读路径贯通（persist 跳过权威行 → 推导自然 no-op） ----------

// latestFromPersistedHistory 按 ListLatestPerModel 语义（per-model 当前行）从落库历史行
// 构造 latest 切片，供测试验证 persist → latest → derive 的闭环。
func latestFromPersistedHistory(rows []*ChannelMonitorHistoryRow) []*ChannelMonitorLatest {
	out := make([]*ChannelMonitorLatest, 0, len(rows))
	for _, r := range rows {
		out = append(out, &ChannelMonitorLatest{
			Model:         r.Model,
			Status:        r.Status,
			LatencyMs:     r.LatencyMs,
			PingLatencyMs: r.PingLatencyMs,
			CheckedAt:     r.CheckedAt,
			Quota:         r.Quota,
		})
	}
	return out
}

func TestPersistNoData_SkipsAuthoritativeRow(t *testing.T) {
	repo := &quotaModeRepoStub{}
	svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{})
	m := &ChannelMonitor{ID: 1, Name: "ch"}
	now := time.Now()

	svc.persistCheckResults(context.Background(), m, []*CheckResult{
		{Model: "a", Status: MonitorStatusFailed, NoData: true, CheckedAt: now},
	})

	require.Empty(t, repo.history, "no-data result must not write an authoritative latest row")
	require.Equal(t, []int64{1}, repo.markedIDs, "no-data still records the attempt time")
}

func TestPersistNoData_ClosesDeriveReadPath(t *testing.T) {
	m := &ChannelMonitor{ID: 1, Name: "ch"}
	now := time.Now()

	// 分支 1：新产生无数据 → latest 不出现 failed 行 → 渠道档位不被无数据推进。
	repoNoData := &quotaModeRepoStub{}
	svcNoData := NewChannelMonitorService(repoNoData, &duplicateChannelMonitorEncryptor{})
	svcNoData.persistCheckResults(context.Background(), m, []*CheckResult{
		{Model: "a", Status: MonitorStatusFailed, NoData: true, CheckedAt: now},
	})
	gotNoData, nerr := svcNoData.deriveChannelStatusFromLatest(context.Background(), 1,
		latestFromPersistedHistory(repoNoData.history))
	require.NoError(t, nerr)
	require.Equal(t, "", gotNoData.Status,
		"no-data must not produce a status (empty-state dash, not operational/failed)")
	require.True(t, gotNoData.ObservedAt.IsZero(),
		"no-data must not advance channel observed time")

	// 分支 2：challenge 真失败 → 照常落行降档。
	repoFailed := &quotaModeRepoStub{}
	svcFailed := NewChannelMonitorService(repoFailed, &duplicateChannelMonitorEncryptor{})
	svcFailed.persistCheckResults(context.Background(), m, []*CheckResult{
		{Model: "a", Status: MonitorStatusFailed, NoData: false, CheckedAt: now},
	})
	require.Len(t, repoFailed.history, 1, "real failure must persist an authoritative row")
	gotFailed, ferr := svcFailed.deriveChannelStatusFromLatest(context.Background(), 1,
		latestFromPersistedHistory(repoFailed.history))
	require.NoError(t, ferr)
	require.Equal(t, MonitorStatusFailed, gotFailed.Status,
		"real failure must downgrade channel tier")
	require.Equal(t, now, gotFailed.ObservedAt)
}

// ---------- 集成：DeriveChannelStatus 方法（缺口登记） ----------

type stubAnomalySource struct {
	anomalies []AccountSideAnomaly
	err       error
}

func (s *stubAnomalySource) ListActiveAnomalies(_ context.Context, _ int64) ([]AccountSideAnomaly, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.anomalies, nil
}

type deriveStatusRepoStub struct {
	ChannelMonitorRepository
	latest []*ChannelMonitorLatest
}

func (r *deriveStatusRepoStub) ListLatestPerModel(_ context.Context, _ int64) ([]*ChannelMonitorLatest, error) {
	return r.latest, nil
}

func TestChannelMonitorService_DeriveChannelStatus_PerModelPart(t *testing.T) {
	now := time.Now()
	repo := &deriveStatusRepoStub{latest: []*ChannelMonitorLatest{
		{Model: "a", Status: MonitorStatusOperational, CheckedAt: now},
	}}
	svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{})

	got, err := svc.DeriveChannelStatus(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, MonitorStatusOperational, got.Status)
	require.Equal(t, now, got.ObservedAt)
	// 未注入账号侧来源 → 不消费账号异常（缺口），但不得报错。
	require.False(t, got.AlertEligible)
}

func TestChannelMonitorService_DeriveChannelStatus_WithAnomalySource(t *testing.T) {
	now := time.Now()
	repo := &deriveStatusRepoStub{latest: []*ChannelMonitorLatest{
		{Model: "a", Status: MonitorStatusOperational, CheckedAt: now},
	}}
	svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{})
	svc.SetAccountAnomalySource(&stubAnomalySource{
		anomalies: []AccountSideAnomaly{{Kind: "temp_unschedulable", Active: true}},
	})

	got, err := svc.DeriveChannelStatus(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, MonitorStatusDegraded, got.Status, "injected account anomaly must drive degraded")
	require.True(t, got.AlertEligible)
}
