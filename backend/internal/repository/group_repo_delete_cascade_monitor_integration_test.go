//go:build integration

package repository

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/channelmonitor"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestDeleteCascadeSoftHidesChannelMonitor verifies that deleting a group via the
// normal cascade path (DeleteCascade, used by 普通模式) soft-hides the channel
// monitors whose group_name matches the deleted group's name instead of leaving
// them enabled (which would keep them on the user channel-monitor cards).
func TestDeleteCascadeSoftHidesChannelMonitor(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()

	groupName := "cascade-monitor-group-normal"
	group := mustCreateGroup(t, client, &service.Group{
		Name: groupName,
	})

	monitor := mustCreateMonitorForGroup(t, client, groupName, true)

	repo := newGroupRepositoryWithSQL(client, tx)
	_, err := repo.DeleteCascade(ctx, group.ID)
	require.NoError(t, err)

	assertMonitorSoftHidden(t, client, ctx, monitor.ID)
}

// TestDeleteCascadeIfEmptySoftHidesChannelMonitor verifies the simple-mode path
// (DeleteCascadeIfEmpty, used by 简单模式 when the group is empty) also soft-hides
// the matching channel monitors.
func TestDeleteCascadeIfEmptySoftHidesChannelMonitor(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()

	groupName := "cascade-monitor-group-simple"
	group := mustCreateGroup(t, client, &service.Group{
		Name: groupName,
	})

	monitor := mustCreateMonitorForGroup(t, client, groupName, true)

	repo := newGroupRepositoryWithSQL(client, tx)
	_, err := repo.DeleteCascadeIfEmpty(ctx, group.ID)
	require.NoError(t, err)

	assertMonitorSoftHidden(t, client, ctx, monitor.ID)
}

// TestDeleteCascadeDoesNotTouchOtherGroupMonitors guards against a vacuous pass:
// a monitor bound to a different group name must stay enabled when an unrelated
// group is deleted.
func TestDeleteCascadeDoesNotTouchOtherGroupMonitors(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()

	deletedGroupName := "cascade-monitor-group-other"
	otherGroupName := "cascade-monitor-group-untouched"
	deletedGroup := mustCreateGroup(t, client, &service.Group{Name: deletedGroupName})
	_ = mustCreateGroup(t, client, &service.Group{Name: otherGroupName})

	otherMonitor := mustCreateMonitorForGroup(t, client, otherGroupName, true)

	repo := newGroupRepositoryWithSQL(client, tx)
	_, err := repo.DeleteCascade(ctx, deletedGroup.ID)
	require.NoError(t, err)

	stored, err := client.ChannelMonitor.Get(ctx, otherMonitor.ID)
	require.NoError(t, err)
	require.True(t, stored.Enabled, "monitor of a different group must remain enabled")
}

func mustCreateMonitorForGroup(t *testing.T, client *dbent.Client, groupName string, enabled bool) *dbent.ChannelMonitor {
	t.Helper()
	ctx := context.Background()

	monitor, err := client.ChannelMonitor.Create().
		SetName("monitor-" + groupName).
		SetProvider(channelmonitor.ProviderOpenai).
		SetAPIMode(service.MonitorAPIModeResponses).
		SetEndpoint("https://api.example.com").
		SetAPIKeyEncrypted("encrypted-key").
		SetPrimaryModel("gpt-5.4-mini").
		SetIntervalSeconds(60).
		SetCreatedBy(1).
		SetGroupName(groupName).
		SetEnabled(enabled).
		Save(ctx)
	require.NoError(t, err, "create channel monitor")
	return monitor
}

func assertMonitorSoftHidden(t *testing.T, client *dbent.Client, ctx context.Context, monitorID int64) {
	t.Helper()
	stored, err := client.ChannelMonitor.Get(ctx, monitorID)
	require.NoError(t, err, "get channel monitor after group delete")
	require.False(t, stored.Enabled, "channel monitor bound to deleted group must be soft-hidden (enabled=false)")
}
