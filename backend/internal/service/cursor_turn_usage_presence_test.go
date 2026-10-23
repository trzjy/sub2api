//go:build unit

package service

// §25.17.5 A06① 解析层用例：turn_ended 的 proto optional **逐字段存在性**必须
// 原样保留，不能被折叠成一组 int 0。
//
// 构造方式：全部用例都用**真实 protobuf 编码**（cursorAppendVarintField /
// cursorAppendBytesField 拼 TurnEndedUpdate 内层，再包 InteractionUpdate 与
// envelope），走整条 cursorParseStream 解析路径，绝不直接构造 cursorTurnUsage{}。
//
// 断言两个事实：
//   - 每个字段的 Seen（出现且合法）与未出现字段保持 false；
//   - billingFieldsComplete() 只认 f1 input / f2 output / f3 cache_read /
//     f4 cache_write 四者**逐一出现且合法**（四项全 0 也算完整）。
//
// f5 reasoning 仅诊断、f6 结束时间不参与 token 完整性：二者缺失不影响四字段判定。

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorTurnUsageRawFrame 用给定的 turn_ended 内层字节构造一帧
// interaction_update{14 turn_ended{…}}（包在 agent 信封里）。
func cursorTurnUsageRawFrame(inner []byte) []byte {
	update := cursorAppendBytesField(nil, cursorFldUpdateTurnEnded, inner)
	return cursorAgentFrame(cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update))
}

// cursorTurnUsageParse 把若干 (字段号, 值) 对编码成 turn_ended 内层，走完整解析路径。
func cursorTurnUsageParse(t *testing.T, frames ...[2]uint64) (cursorTurnUsage, error) {
	t.Helper()
	inner := make([]byte, 0, 8*len(frames))
	for _, f := range frames {
		inner = cursorAppendVarintField(inner, int(f[0]), f[1])
	}
	return cursorTurnUsageParseRaw(t, cursorTurnUsageRawFrame(inner))
}

// cursorTurnUsageParseRaw 同上，但接受已编码的整帧（错误 wire 等用例需要）。
func cursorTurnUsageParseRaw(t *testing.T, encoded ...[]byte) (cursorTurnUsage, error) {
	t.Helper()
	var stream []byte
	for _, f := range encoded {
		stream = append(stream, f...)
	}
	p := cursorNewStreamParser("cursor-test-model")
	if _, err := p.cursorParseStream(stream); err != nil {
		return cursorTurnUsage{}, err
	}
	return p.usage, nil
}

// TestCursorTurnUsagePresenceAndBillingComplete 覆盖四字段全 0 / 正常值 /
// f1–f4 逐一缺失 / timestamp-only / reasoning-only / 无 f5f6 / 空帧等形状。
func TestCursorTurnUsagePresenceAndBillingComplete(t *testing.T) {
	cases := []struct {
		name string
		// fields 是 (字段号, 值) 序列，按给定顺序编码进 TurnEndedUpdate。
		fields [][2]uint64
		// want 是解析出的完整 usage（含逐字段 Seen）。
		want cursorTurnUsage
		// complete 是 billingFieldsComplete() 的期望值。
		complete bool
	}{
		{
			// 27/28 活体原件形状之一：四项全部显式出现，含 cache_write=0。
			// 全 0 也是真实完整用量，不得用"总和为 0"推断缺失。
			name: "四计费字段全0",
			fields: [][2]uint64{
				{cursorFldTurnEndedInputTokens, 0},
				{cursorFldTurnEndedOutputTokens, 0},
				{cursorFldTurnEndedCacheReadTokens, 0},
				{cursorFldTurnEndedCacheWriteTokens, 0},
			},
			want: cursorTurnUsage{
				Seen: true, Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0,
				F1InputSeen: true, F2OutputSeen: true, F3CacheReadSeen: true, F4CacheWriteSeen: true,
			},
			complete: true,
		},
		{
			// f1–f4 正常值 + f5 reasoning + f6 时间戳（33 号证据形状）。
			name: "正常值含f5f6",
			fields: [][2]uint64{
				{cursorFldTurnEndedInputTokens, 12601},
				{cursorFldTurnEndedOutputTokens, 15},
				{cursorFldTurnEndedCacheReadTokens, 128},
				{cursorFldTurnEndedCacheWriteTokens, 64},
				{cursorFldTurnEndedReasoningTokens, 11},
				{cursorFldTurnEndedEndedAtMS, 1792309998331},
			},
			want: cursorTurnUsage{
				Seen: true, Input: 12601, Output: 15, CacheRead: 128, CacheWrite: 64,
				Reasoning: 11, EndedAtMS: 1792309998331,
				F1InputSeen: true, F2OutputSeen: true, F3CacheReadSeen: true, F4CacheWriteSeen: true,
				F5ReasoningSeen: true, F6EndedAtSeen: true,
			},
			complete: true,
		},
		{
			// 四项齐全但没有 f5/f6：reasoning 仅诊断、时间不必填 → 仍完整。
			name: "四字段齐全无f5f6",
			fields: [][2]uint64{
				{cursorFldTurnEndedInputTokens, 7},
				{cursorFldTurnEndedOutputTokens, 0},
				{cursorFldTurnEndedCacheReadTokens, 5},
				{cursorFldTurnEndedCacheWriteTokens, 0},
			},
			want: cursorTurnUsage{
				Seen: true, Input: 7, Output: 0, CacheRead: 5, CacheWrite: 0,
				F1InputSeen: true, F2OutputSeen: true, F3CacheReadSeen: true, F4CacheWriteSeen: true,
			},
			complete: true,
		},
		{
			name: "缺f1_input",
			fields: [][2]uint64{
				{cursorFldTurnEndedOutputTokens, 15},
				{cursorFldTurnEndedCacheReadTokens, 128},
				{cursorFldTurnEndedCacheWriteTokens, 64},
			},
			want: cursorTurnUsage{
				Seen: true, Output: 15, CacheRead: 128, CacheWrite: 64,
				F2OutputSeen: true, F3CacheReadSeen: true, F4CacheWriteSeen: true,
			},
			complete: false,
		},
		{
			name: "缺f2_output",
			fields: [][2]uint64{
				{cursorFldTurnEndedInputTokens, 12601},
				{cursorFldTurnEndedCacheReadTokens, 128},
				{cursorFldTurnEndedCacheWriteTokens, 64},
			},
			want: cursorTurnUsage{
				Seen: true, Input: 12601, CacheRead: 128, CacheWrite: 64,
				F1InputSeen: true, F3CacheReadSeen: true, F4CacheWriteSeen: true,
			},
			complete: false,
		},
		{
			name: "缺f3_cache_read",
			fields: [][2]uint64{
				{cursorFldTurnEndedInputTokens, 12601},
				{cursorFldTurnEndedOutputTokens, 15},
				{cursorFldTurnEndedCacheWriteTokens, 64},
			},
			want: cursorTurnUsage{
				Seen: true, Input: 12601, Output: 15, CacheWrite: 64,
				F1InputSeen: true, F2OutputSeen: true, F4CacheWriteSeen: true,
			},
			complete: false,
		},
		{
			name: "缺f4_cache_write",
			fields: [][2]uint64{
				{cursorFldTurnEndedInputTokens, 12601},
				{cursorFldTurnEndedOutputTokens, 15},
				{cursorFldTurnEndedCacheReadTokens, 128},
			},
			want: cursorTurnUsage{
				Seen: true, Input: 12601, Output: 15, CacheRead: 128,
				F1InputSeen: true, F2OutputSeen: true, F3CacheReadSeen: true,
			},
			complete: false,
		},
		{
			// 41 号证据的拒付样本形状：只有 f6，绝不当"真实 0 用量"。
			name:   "仅timestamp",
			fields: [][2]uint64{{cursorFldTurnEndedEndedAtMS, 1792327776714}},
			want: cursorTurnUsage{
				Seen: true, EndedAtMS: 1792327776714, F6EndedAtSeen: true,
			},
			complete: false,
		},
		{
			// 只有 f5 reasoning：同样不是完整用量（reasoning 不并入 output）。
			name:   "仅reasoning",
			fields: [][2]uint64{{cursorFldTurnEndedReasoningTokens, 11}},
			want: cursorTurnUsage{
				Seen: true, Reasoning: 11, F5ReasoningSeen: true,
			},
			complete: false,
		},
		{
			// 空 TurnEndedUpdate：帧出现过（Seen=true）但无任何字段存在性。
			name:     "空TurnEnded",
			fields:   nil,
			want:     cursorTurnUsage{Seen: true},
			complete: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cursorTurnUsageParse(t, tc.fields...)
			require.NoError(t, err)
			require.Equal(t, tc.want, got, "存在性与取值必须逐字段保留")
			require.Equal(t, tc.complete, got.billingFieldsComplete(), "四计费字段判定")
			// 帧级存在性与字段级存在性分开：收到空 turn_ended 不等于有完整用量。
			require.True(t, got.Seen, "turn_ended 帧出现过")
		})
	}
}

// TestCursorTurnUsageInvalidFieldsFailClosed 覆盖错误 wire / 负数 / 越界：
// 一律按解析失败上报 ErrCursorProtocol，绝不把损坏值补 0 当真实用量。
func TestCursorTurnUsageInvalidFieldsFailClosed(t *testing.T) {
	// 四项齐全的合法基线：只把被检的那一项换成非法编码，确保失败来自该字段。
	valid := func() []byte {
		inner := make([]byte, 0, 32)
		for _, f := range [][2]uint64{
			{cursorFldTurnEndedInputTokens, 1},
			{cursorFldTurnEndedOutputTokens, 2},
			{cursorFldTurnEndedCacheReadTokens, 3},
			{cursorFldTurnEndedCacheWriteTokens, 4},
		} {
			inner = cursorAppendVarintField(inner, int(f[0]), f[1])
		}
		return inner
	}

	cases := []struct {
		name  string
		inner []byte
	}{
		{
			// f1 用 len-delimited（wire 2）而不是 varint：wire 类型错误。
			name: "f1错误wire类型",
			inner: cursorAppendBytesField(
				cursorAppendVarintField(cursorAppendVarintField(cursorAppendVarintField(nil,
					cursorFldTurnEndedOutputTokens, 2), cursorFldTurnEndedCacheReadTokens, 3),
					cursorFldTurnEndedCacheWriteTokens, 4),
				cursorFldTurnEndedInputTokens, []byte("42")),
		},
		{
			// f1 负数：proto int64 的 -1 按 10 字节补码 varint 编码。
			name: "f1负数",
			inner: cursorAppendVarintField(
				cursorAppendVarintField(cursorAppendVarintField(cursorAppendVarintField(nil,
					cursorFldTurnEndedOutputTokens, 2), cursorFldTurnEndedCacheReadTokens, 3),
					cursorFldTurnEndedCacheWriteTokens, 4),
				cursorFldTurnEndedInputTokens, ^uint64(0)),
		},
		{
			// f2 越界：1<<63 作为 int64 已是负数，转 int 不安全。
			name: "f2越界",
			inner: cursorAppendVarintField(
				cursorAppendVarintField(cursorAppendVarintField(cursorAppendVarintField(nil,
					cursorFldTurnEndedInputTokens, 1), cursorFldTurnEndedCacheReadTokens, 3),
					cursorFldTurnEndedCacheWriteTokens, 4),
				cursorFldTurnEndedOutputTokens, uint64(1)<<63),
		},
		{
			// f3 用 wire 2：类型错误（不是"缺失"，不能当 0 处理）。
			name: "f3错误wire类型",
			inner: cursorAppendBytesField(
				cursorAppendVarintField(cursorAppendVarintField(cursorAppendVarintField(nil,
					cursorFldTurnEndedInputTokens, 1), cursorFldTurnEndedOutputTokens, 2),
					cursorFldTurnEndedCacheWriteTokens, 4),
				cursorFldTurnEndedCacheReadTokens, []byte{0x01, 0x02}),
		},
		{
			// f6 结束时间用 wire 2：非必填字段出现时也必须是合法 varint。
			name:  "f6错误wire类型",
			inner: cursorAppendBytesField(valid(), cursorFldTurnEndedEndedAtMS, []byte{0x01}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage, err := cursorTurnUsageParseRaw(t, cursorTurnUsageRawFrame(tc.inner))
			require.Error(t, err, "非法字段必须失败关闭")
			require.ErrorIs(t, err, ErrCursorProtocol)
			require.Equal(t, cursorTurnUsage{}, usage, "失败时不得留下任何 usage")
			require.False(t, usage.billingFieldsComplete())
		})
	}
}

// TestCursorTurnUsageValidBoundariesAccepted 反向对照：同样的真实编码但取值合法
// （含 int / uint64 边界）必须解析成功——证明上表的失败来自字段本身而非编码路径。
func TestCursorTurnUsageValidBoundariesAccepted(t *testing.T) {
	got, err := cursorTurnUsageParse(t,
		[2]uint64{cursorFldTurnEndedInputTokens, uint64(math.MaxInt)},
		[2]uint64{cursorFldTurnEndedOutputTokens, 0},
		[2]uint64{cursorFldTurnEndedCacheReadTokens, 1},
		[2]uint64{cursorFldTurnEndedCacheWriteTokens, 0},
		[2]uint64{cursorFldTurnEndedEndedAtMS, math.MaxUint64},
	)
	require.NoError(t, err)
	require.Equal(t, math.MaxInt, got.Input)
	require.True(t, got.F1InputSeen)
	require.True(t, got.F6EndedAtSeen)
	require.Equal(t, uint64(math.MaxUint64), got.EndedAtMS)
	require.True(t, got.billingFieldsComplete())
}
