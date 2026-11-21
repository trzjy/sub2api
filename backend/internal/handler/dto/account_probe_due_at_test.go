//go:build unit

package dto

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 批1：until 已到期但仍有待确认探测的账号，管理端必须显示为临时停调。
func TestTempUnschedulableActive_ProbeDueAt(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	due := time.Now().Add(time.Minute)

	pending := &service.Account{TempUnschedulableUntil: &past, ProbeDueAt: &due}
	require.True(t, tempUnschedulableActive(pending))

	pendingNoUntil := &service.Account{ProbeDueAt: &due}
	require.True(t, tempUnschedulableActive(pendingNoUntil))

	recovered := &service.Account{TempUnschedulableUntil: &past}
	require.False(t, tempUnschedulableActive(recovered))
}
