//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDSMLRestoreObservabilityFields 锁死 DSML 观测埋点的字段口径。
//
// 字段名沿用既有 skipped_total / skipped_types 家族（cursor_skip_ledger.go），
// 运维只需记一套字段；额外给出 dsml_restored / dsml_dropped 两个细分计数。
func TestDSMLRestoreObservabilityFields(t *testing.T) {
	fields := dsmlRestoreFields(3, 1)
	require.Len(t, fields, 3)

	byKey := map[string]int{}
	for _, f := range fields {
		byKey[f.Key] = int(f.Integer)
	}
	require.Equal(t, 4, byKey["skipped_total"])
	require.Equal(t, 3, byKey["dsml_restored"])
	require.Equal(t, 1, byKey["dsml_dropped"])
}
