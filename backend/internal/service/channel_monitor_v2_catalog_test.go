package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// buildC3Catalog constructs a catalog with three on-sale triples under group 1.
func buildC3Catalog() (ChannelMonitorCatalog, map[int64]*Group) {
	catalog := ChannelMonitorCatalog{
		Keys: map[ChannelMonitorCatalogKey]struct{}{
			{Platform: "openai", Model: "gpt-5", GroupID: 1}: {},
			{Platform: "openai", Model: "gpt-4", GroupID: 1}: {},
			{Platform: "openai", Model: "gpt-3", GroupID: 1}: {},
		},
	}
	groupsByID := map[int64]*Group{1: {ID: 1, Name: "默认组", Status: StatusActive}}
	return catalog, groupsByID
}

func c3Coverage() ChannelMonitorV2Coverage {
	dt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return ChannelMonitorV2Coverage{
		RequestedStart:   dt.Add(-90 * time.Minute),
		RequestedEnd:     dt,
		CoverageStart:    dt.Add(-90 * time.Minute),
		DataThrough:      dt,
		ComputedAt:       dt,
		CoverageComplete: true,
		BucketSeconds:    300,
	}
}

func c3Now() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }

func c3BusinessRow(model string, requestCount int64, healthOverall string) ChannelMonitorV2MatrixRow {
	return ChannelMonitorV2MatrixRow{
		Platform:  "openai",
		GroupID:   c3Int64Ptr(1),
		GroupName: "默认组",
		Model:     model,
		Metrics:   ChannelMonitorV2Metric{RequestCount: requestCount, SuccessRequests: requestCount, ErrorRequests: 0, ErrorRate: 0, SuccessRate: 1},
		Health:    ChannelMonitorV2Health{Overall: healthOverall},
	}
}

func c3Int64Ptr(v int64) *int64 { return &v }

func findRow(rows []ChannelMonitorV2MatrixRow, model string) *ChannelMonitorV2MatrixRow {
	for i := range rows {
		if rows[i].Model == model {
			return &rows[i]
		}
	}
	return nil
}

func TestChannelMonitorV2MergeCatalogThreeStates(t *testing.T) {
	catalog, groupsByID := buildC3Catalog()
	base := &ChannelMonitorV2Matrix{
		GroupBy:  ChannelMonitorV2GroupByPlatformGroupModel,
		Coverage: c3Coverage(),
		Items:    []ChannelMonitorV2MatrixRow{c3BusinessRow("gpt-5", 100, "healthy")},
	}
	// gpt-4 has a fresh OK probe; gpt-3 has neither traffic nor probe.
	probeStates := map[ChannelMonitorCatalogKey]*ProbeState{
		{Platform: "openai", Model: "gpt-4", GroupID: 1}: {
			Platform: "openai", Model: "gpt-4", GroupID: 1,
			LastProbeAt: c3Now().Add(-5 * time.Minute), LastOK: true,
		},
	}

	result := channelMonitorV2MergeCatalogMatrix(
		ChannelMonitorV2Config{}, ChannelMonitorV2GroupByPlatformGroupModel,
		catalog, groupsByID, base, probeStates, c3Now,
	)

	require.Len(t, result.Items, 3)

	// ① business
	biz := findRow(result.Items, "gpt-5")
	require.NotNil(t, biz)
	require.True(t, biz.HasData)
	require.NotNil(t, biz.Source)
	require.Equal(t, ChannelMonitorV2SourceBusiness, *biz.Source)
	require.NotNil(t, biz.Status)
	require.Equal(t, ChannelMonitorV2StatusOperational, *biz.Status)
	require.NotNil(t, biz.ObservedAt) // falls back to coverage.DataThrough

	// ② probe (sampled, fresh)
	probe := findRow(result.Items, "gpt-4")
	require.NotNil(t, probe)
	require.True(t, probe.HasData)
	require.NotNil(t, probe.Source)
	require.Equal(t, ChannelMonitorV2SourceProbe, *probe.Source)
	require.NotNil(t, probe.Status)
	require.Equal(t, ChannelMonitorV2StatusOperational, *probe.Status)
	require.NotNil(t, probe.ObservedAt)

	// ③ no-data
	none := findRow(result.Items, "gpt-3")
	require.NotNil(t, none)
	require.False(t, none.HasData)
	require.Nil(t, none.Source)
	require.Nil(t, none.Status)
	require.Nil(t, none.ObservedAt)
}

func TestChannelMonitorV2MergeCatalogProbeFailedStatus(t *testing.T) {
	catalog, groupsByID := buildC3Catalog()
	base := &ChannelMonitorV2Matrix{GroupBy: ChannelMonitorV2GroupByPlatformGroupModel, Coverage: c3Coverage(), Items: nil}
	probeStates := map[ChannelMonitorCatalogKey]*ProbeState{
		{Platform: "openai", Model: "gpt-4", GroupID: 1}: {
			Platform: "openai", Model: "gpt-4", GroupID: 1,
			LastProbeAt: c3Now().Add(-3 * time.Minute), LastOK: false,
		},
		{Platform: "openai", Model: "gpt-3", GroupID: 1}: {
			Platform: "openai", Model: "gpt-3", GroupID: 1,
			LastProbeAt: c3Now().Add(-3 * time.Minute), LastOK: true,
		},
	}
	result := channelMonitorV2MergeCatalogMatrix(
		ChannelMonitorV2Config{}, ChannelMonitorV2GroupByPlatformGroupModel,
		catalog, groupsByID, base, probeStates, c3Now,
	)
	require.Len(t, result.Items, 3)
	probe := findRow(result.Items, "gpt-4")
	require.NotNil(t, probe)
	require.True(t, probe.HasData)
	require.Equal(t, ChannelMonitorV2StatusFailed, *probe.Status)
	// gpt-3 fresh OK probe → operational
	ok := findRow(result.Items, "gpt-3")
	require.NotNil(t, ok)
	require.Equal(t, ChannelMonitorV2StatusOperational, *ok.Status)
}

func TestChannelMonitorV2MergeCatalogProbeFreshnessExpiry(t *testing.T) {
	catalog, groupsByID := buildC3Catalog()
	base := &ChannelMonitorV2Matrix{GroupBy: ChannelMonitorV2GroupByPlatformGroupModel, Coverage: c3Coverage(), Items: nil}
	// B4：新鲜度门已废除——即使探测早于 30 分钟，最新一次探测结果仍无条件作为
	// 探测来源状态展示（不再折叠为 no-data）。仅该渠道从未有 probe_state 才 has_data=false。
	probeStates := map[ChannelMonitorCatalogKey]*ProbeState{
		{Platform: "openai", Model: "gpt-4", GroupID: 1}: {
			Platform: "openai", Model: "gpt-4", GroupID: 1,
			LastProbeAt: c3Now().Add(-31 * time.Minute), LastOK: true,
		},
	}
	result := channelMonitorV2MergeCatalogMatrix(
		ChannelMonitorV2Config{}, ChannelMonitorV2GroupByPlatformGroupModel,
		catalog, groupsByID, base, probeStates, c3Now,
	)
	probe := findRow(result.Items, "gpt-4")
	require.NotNil(t, probe)
	require.True(t, probe.HasData) // 废除新鲜度门后，陈旧探测仍展示为 probe 来源
	require.NotNil(t, probe.Source)
	require.Equal(t, ChannelMonitorV2SourceProbe, *probe.Source)
	require.NotNil(t, probe.Status)
	require.Equal(t, ChannelMonitorV2StatusOperational, *probe.Status)
	require.NotNil(t, probe.ObservedAt)
}

func TestChannelMonitorV2MergeCatalogDTOMinimalContract(t *testing.T) {
	// Invariant: has_data=false ⇒ status/source/observed_at all null;
	// has_data=true ⇒ all three non-null. Enforced for every emitted row.
	catalog, groupsByID := buildC3Catalog()
	base := &ChannelMonitorV2Matrix{
		GroupBy:  ChannelMonitorV2GroupByPlatformGroupModel,
		Coverage: c3Coverage(),
		Items:    []ChannelMonitorV2MatrixRow{c3BusinessRow("gpt-5", 50, "warning")},
	}
	probeStates := map[ChannelMonitorCatalogKey]*ProbeState{
		{Platform: "openai", Model: "gpt-4", GroupID: 1}: {
			Platform: "openai", Model: "gpt-4", GroupID: 1,
			LastProbeAt: c3Now().Add(-2 * time.Minute), LastOK: true,
		},
	}
	result := channelMonitorV2MergeCatalogMatrix(
		ChannelMonitorV2Config{}, ChannelMonitorV2GroupByPlatformGroupModel,
		catalog, groupsByID, base, probeStates, c3Now,
	)
	for _, row := range result.Items {
		if row.HasData {
			require.NotNil(t, row.Source, "has_data=true requires source for model=%s", row.Model)
			require.NotNil(t, row.Status, "has_data=true requires status for model=%s", row.Model)
			require.NotNil(t, row.ObservedAt, "has_data=true requires observed_at for model=%s", row.Model)
			require.Contains(t, []string{ChannelMonitorV2SourceBusiness, ChannelMonitorV2SourceProbe}, *row.Source)
			require.Contains(t, []string{
				ChannelMonitorV2StatusOperational, ChannelMonitorV2StatusDegraded,
				ChannelMonitorV2StatusFailed, ChannelMonitorV2StatusError,
			}, *row.Status)
		} else {
			require.Nil(t, row.Source, "has_data=false requires nil source for model=%s", row.Model)
			require.Nil(t, row.Status, "has_data=false requires nil status for model=%s", row.Model)
			require.Nil(t, row.ObservedAt, "has_data=false requires nil observed_at for model=%s", row.Model)
		}
	}
}

func TestChannelMonitorV2MergeCatalogCoarserGrainProjected(t *testing.T) {
	// platform_group grain collapses the model dimension; every on-sale triple under
	// group 1 should still appear once, driven by the catalog (no business traffic).
	catalog, groupsByID := buildC3Catalog()
	base := &ChannelMonitorV2Matrix{GroupBy: ChannelMonitorV2GroupByPlatformGroup, Coverage: c3Coverage(), Items: nil}
	result := channelMonitorV2MergeCatalogMatrix(
		ChannelMonitorV2Config{}, ChannelMonitorV2GroupByPlatformGroup,
		catalog, groupsByID, base, map[ChannelMonitorCatalogKey]*ProbeState{}, c3Now,
	)
	// All three models collapse to a single (openai, group 1) grain → one no-data row.
	require.Len(t, result.Items, 1)
	row := result.Items[0]
	require.Equal(t, "openai", row.Platform)
	require.Equal(t, int64(1), *row.GroupID)
	require.Empty(t, row.Model) // model collapsed at this grain
	require.False(t, row.HasData)
}
