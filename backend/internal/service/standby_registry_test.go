//go:build unit

package service

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStandbyRegistry_DefaultClosed(t *testing.T) {
	reg := NewStandbyRegistry()
	require.False(t, reg.IsOpen("deepseek-v4-pro"))

	status, ok := reg.Status("deepseek-v4-pro")
	require.False(t, ok)
	require.False(t, status.Open)
	require.Empty(t, reg.Snapshot())

	var nilRegistry *StandbyRegistry
	require.False(t, nilRegistry.IsOpen("kimi-k3"))
	require.Empty(t, nilRegistry.Snapshot())
}

func TestStandbyRegistry_ConcurrentReadWrite(t *testing.T) {
	reg := NewStandbyRegistry()
	models := []string{"deepseek-v4-pro", "kimi-k3", "glm-5.3"}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for round := 0; round < 200; round++ {
				model := models[(i+round)%len(models)]
				reg.set(model, StandbyStatus{
					Platform:    "deepseek",
					Open:        round%2 == 0,
					Verdict:     StandbyVerdictDegraded,
					Since:       now,
					OpenedAt:    now,
					Reason:      "ttft_p50_above_enter",
					EvaluatedAt: now,
				})
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			for round := 0; round < 200; round++ {
				model := models[(i*3+round)%len(models)]
				_ = reg.IsOpen(model)
				_, _ = reg.Status(model)
				snapshot := reg.Snapshot()
				for _, st := range snapshot {
					_ = st.Open
				}
			}
		}(i)
	}
	wg.Wait()

	snapshot := reg.Snapshot()
	require.Len(t, snapshot, len(models))
	for _, model := range models {
		st, ok := snapshot[model]
		require.True(t, ok)
		require.Equal(t, model, st.Model)
		require.Equal(t, "ttft_p50_above_enter", st.Reason)
	}
}

func TestStandbyRegistry_SnapshotIsIsolated(t *testing.T) {
	reg := NewStandbyRegistry()
	reg.set("kimi-k3", StandbyStatus{Platform: "kimi", Open: true})

	snapshot := reg.Snapshot()
	st := snapshot["kimi-k3"]
	st.Open = false
	snapshot["kimi-k3"] = st
	snapshot["injected"] = StandbyStatus{Open: true}

	require.True(t, reg.IsOpen("kimi-k3"))
	require.False(t, reg.IsOpen("injected"))
	require.Len(t, reg.Snapshot(), 1)
}

func TestStandbyRegistry_SetIgnoresEmptyModel(t *testing.T) {
	reg := NewStandbyRegistry()
	reg.set("", StandbyStatus{Open: true})
	reg.set("glm-5.3", StandbyStatus{Platform: "zhipu", Open: true, Reason: "ttft_p50_above_enter"})

	require.Empty(t, reg.Snapshot()[""])

	st, ok := reg.Status("glm-5.3")
	require.True(t, ok)
	require.Equal(t, "glm-5.3", st.Model)
	require.Equal(t, "ttft_p50_above_enter", st.Reason)
}
