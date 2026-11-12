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

	// 外审第三轮 must_fix #1 整改后的语义：**次数上限不再是「候选耗尽」的判据**，
	// 只做死循环兜底。因此在上限之前，调用方必须一直收到「继续换号」。
	// 候选耗尽由「选号返回无候选」判定（排除集合把所有候选排完）。
	for i := 1; i < maxConcurrencySlotFailoverAttempts; i++ {
		require.True(t, recordConcurrencySlotFailure(failed, int64(i), &attempts),
			"第 %d 次槽位失败必须继续换号（次数上限不是候选耗尽判据）", i)
		require.Contains(t, failed, int64(i), "满载账号要记入本请求排除集合")
	}

	// 达到兜底上限后停止（防止某个入口漏写排除动作导致死循环）。
	require.False(t, recordConcurrencySlotFailure(failed, int64(maxConcurrencySlotFailoverAttempts), &attempts),
		"达到兜底上限后不再换号")
	require.Contains(t, failed, int64(maxConcurrencySlotFailoverAttempts),
		"最后一次失败也要排除，否则会选回同一个账号")

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
