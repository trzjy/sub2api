package service

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// 生产活体原文（2026-11-03）：上游以普通 TextDelta 下发，无 is_server_notice 位、
// EndStream 不带业务错误码，只能按正文模板识别。
const cursorQuotaExhaustedLiveSample = "今日额度已用完，每日早上8点重置（时长卡每日用量有上限，可查看时长卡详情）（1793676349583-clard21b）"

// TestCursorQuotaExhaustedTemplateMatched 验证「今日额度已用完」模板能被识别为
// ErrCursorQuotaExhaustedInStream，并解析出全角括号里的轮次标识。
//
// 修复前：cursorMatchKeyRejected 只认「密钥已过期或额度耗尽」与「当前模型暂不可用」
// 两个模板，本模板被当成正常正文透传给客户端（含卡 ID），既不换号也不停号。
func TestCursorQuotaExhaustedTemplateMatched(t *testing.T) {
	err := cursorMatchKeyRejected(cursorQuotaExhaustedLiveSample)
	require.Error(t, err)

	var qe *CursorQuotaExhaustedError
	require.True(t, errors.As(err, &qe), "应识别为 CursorQuotaExhaustedError，实际: %v", err)
	require.True(t, errors.Is(err, ErrCursorQuotaExhaustedInStream))
	// 轮次标识取最后一个括号组（全角），前面的时长卡说明括号不得被误取。
	require.Equal(t, "1793676349583-clard21b", qe.TurnID)
	// 片段原样留存（含动态后缀），供诊断。
	require.Contains(t, qe.Template, "今日额度已用完")
}

// TestCursorQuotaExhaustedTemplateHalfWidth 半角括号也要能取到轮次标识。
func TestCursorQuotaExhaustedTemplateHalfWidth(t *testing.T) {
	err := cursorMatchKeyRejected("今日额度已用完，每日早上8点重置 (1793676349583-clard21b)")
	var qe *CursorQuotaExhaustedError
	require.True(t, errors.As(err, &qe))
	require.Equal(t, "1793676349583-clard21b", qe.TurnID)
}

// TestCursorQuotaExhaustedTemplateNoTurnID 无后缀时不得 panic，TurnID 为空串。
func TestCursorQuotaExhaustedTemplateNoTurnID(t *testing.T) {
	err := cursorMatchKeyRejected("今日额度已用完")
	var qe *CursorQuotaExhaustedError
	require.True(t, errors.As(err, &qe))
	require.Empty(t, qe.TurnID)
	require.Equal(t, "今日额度已用完", qe.Template)
}

// TestCursorExistingTemplatesStillMatched 既有两支模板的匹配不得被新分支破坏
// （顺序敏感：拒付 / 模型暂不可用 必须先于额度耗尽判定）。
func TestCursorExistingTemplatesStillMatched(t *testing.T) {
	rejectErr := cursorMatchKeyRejected("密钥已过期或额度耗尽，请续费后使用 (1792310906551-b924f5ef)")
	var rej *CursorKeyRejectedError
	require.True(t, errors.As(rejectErr, &rej), "拒付模板仍须命中 CursorKeyRejectedError")
	require.True(t, errors.Is(rejectErr, ErrCursorKeyRejectedUpstream))

	modelErr := cursorMatchKeyRejected("当前模型暂不可用，请发送 继续 或新开对话 (1793521393111-0d01dd7b)")
	var mu *CursorModelUnavailableError
	require.True(t, errors.As(modelErr, &mu), "模型暂不可用模板仍须命中 CursorModelUnavailableError")
	require.True(t, errors.Is(modelErr, ErrCursorUpstreamModelUnavailable))

	// 无关正文不得误报任何终态错误。
	require.NoError(t, cursorMatchKeyRejected("这是一段正常回复正文，没有任何模板。"))
}

// TestIsCursorQuotaExhaustedReason 验证停号判据：只有本原因才触发该分支，
// 其它换号原因不得被误判（否则会把正常账号连坐停掉）。
func TestIsCursorQuotaExhaustedReason(t *testing.T) {
	require.True(t, IsCursorQuotaExhaustedReason(CursorQuotaExhaustedReason))
	require.False(t, IsCursorQuotaExhaustedReason(CursorUpstreamAccountFailureReason))
	require.False(t, IsCursorQuotaExhaustedReason(GatewayFailureReason("model_rate_limited")))
	require.False(t, IsCursorQuotaExhaustedReason(GatewayFailureReason("")))
}
