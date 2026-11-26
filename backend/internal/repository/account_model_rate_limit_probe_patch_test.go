//go:build unit

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPatchModelRateLimitEntryDropSchedulePreservesLimitScope(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	applied, err := repo.PatchModelRateLimitEntry(context.Background(), 41, "cline:pass", 23, service.ModelProbePatch{
		Op: service.ModelProbePatchDropSchedule,
	})

	require.NoError(t, err)
	require.False(t, applied, "zero affected rows means the generation was replaced")
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "probe_due_at")
	require.Contains(t, normalized, "probe_gen")
	require.Contains(t, normalized, "- 'probe_due_at' - 'probe_gen'")
	require.Contains(t, normalized, "(extra->'model_rate_limits'->$2::text->>'probe_gen')::bigint = $3")
	require.Equal(t, []any{int64(41), "cline:pass", int64(23)}, exec.execArgs[0])
}
