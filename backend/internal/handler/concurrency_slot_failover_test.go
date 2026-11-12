package handler

import (
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestRecordConcurrencySlotFailure(t *testing.T) {
	failed := map[int64]struct{}{}
	attempts := 0

	require.True(t, recordConcurrencySlotFailure(failed, 1, &attempts), "第一次槽位失败应换号")
	require.Contains(t, failed, int64(1), "满载账号要记入本请求排除集合")
	require.True(t, recordConcurrencySlotFailure(failed, 2, &attempts), "第二次槽位失败应换号")
	require.Contains(t, failed, int64(2))
	require.False(t, recordConcurrencySlotFailure(failed, 3, &attempts),
		"达到上限后不再换号，避免延迟被放大成 N × WaitPlan.Timeout")
	require.Contains(t, failed, int64(3), "最后一次失败也要排除，否则会选回同一个账号")

	// 防御：nil 集合 / nil 计数不得 panic，直接判定不再换号。
	require.False(t, recordConcurrencySlotFailure(nil, 1, &attempts))
	require.False(t, recordConcurrencySlotFailure(failed, 9, nil))
}

func TestConcurrencySlotExhaustedResponse(t *testing.T) {
	status, errType, code, message := concurrencySlotExhaustedResponse()

	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "api_error", errType)
	require.Equal(t, gatewayConcurrencyLimitCode, code)
	require.Equal(t, infraerrors.AccountSlotConcurrencyLimit, message,
		"终态文案必须与既有「槽位等不到」出口逐字节一致")
}

// TestConcurrencySlotExhaustedResponse_MatchesAccountSlotError 锁定「换号耗尽」与
// 「直接等不到」两条路径的终态输出一致 —— 锁定项：账号维度等待语义不变。
func TestConcurrencySlotExhaustedResponse_MatchesAccountSlotError(t *testing.T) {
	directStatus, directType, directCode, directMessage := concurrencyErrorResponse(
		&ConcurrencyError{SlotType: "account", Limit: 3}, "account", "")
	exhaustedStatus, exhaustedType, exhaustedCode, exhaustedMessage := concurrencySlotExhaustedResponse()

	require.Equal(t, directStatus, exhaustedStatus)
	require.Equal(t, directType, exhaustedType)
	require.Equal(t, directCode, exhaustedCode)
	require.Equal(t, directMessage, exhaustedMessage)
}
