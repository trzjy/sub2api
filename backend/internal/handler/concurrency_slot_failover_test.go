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

	// 外审第三轮 must_fix #1 整改后的语义：**候选耗尽不再由尝试次数判定**，
	// 而由「选号返回无候选」判定。因此调用方必须一直收到「继续换号」——
	// 否则池里 4 个以上账号时，前 3 个满、第 4 个空闲也会被误报成耗尽。
	for i := 1; i <= 60; i++ {
		require.True(t, recordConcurrencySlotFailure(nil, failed, int64(i), &attempts),
			"第 %d 次槽位失败必须继续换号（耗尽由选号无候选判定，不以次数代替）", i)
		require.Contains(t, failed, int64(i), "满载账号要记入本请求排除集合")
	}

	// 防御：nil 集合 / nil 计数不得 panic，直接判定不再换号。
	require.False(t, recordConcurrencySlotFailure(nil, nil, 1, &attempts))
	require.False(t, recordConcurrencySlotFailure(nil, failed, 9, nil))
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
