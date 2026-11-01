//go:build unit

package service

// §2.4 上游注入"模型暂不可用"文本的识别（卡 A2）：
// 上游以普通 TextDelta 下发「当前模型暂不可用，请发送 继续 或新开对话 (ts-hex)」，
// 与拒付模板共用 cursorMatchKeyRejected，命中返回 *CursorModelUnavailableError。

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorModelUnavailableLiveSample 是证据 55-toolerr-r5-turn5.raw 的累计正文原文。
const cursorModelUnavailableLiveSample = "\n</think>\n\n[当前模型暂不可用，请发送 继续 或新开对话 (1793521393111-0d01dd7b)]"

func TestCursorMatchKeyRejected_ModelUnavailableLiveSample(t *testing.T) {
	err := cursorMatchKeyRejected(cursorModelUnavailableLiveSample)
	require.Error(t, err)

	var mu *CursorModelUnavailableError
	require.ErrorAs(t, err, &mu)
	require.Equal(t, "1793521393111-0d01dd7b", mu.TurnID)
	require.Equal(t, "当前模型暂不可用，请发送 继续 或新开对话 (1793521393111-0d01dd7b)]", mu.Template)
	require.True(t, errors.Is(err, ErrCursorUpstreamModelUnavailable))
	require.False(t, errors.Is(err, ErrCursorKeyRejectedUpstream), "模型不可用不得归为拒付（拒付会停号）")

	var kr *CursorKeyRejectedError
	require.False(t, errors.As(err, &kr))
}

func TestCursorMatchKeyRejected_KeyRejectedStillMatched(t *testing.T) {
	err := cursorMatchKeyRejected("密钥已过期或额度耗尽，请续费后使用 (1792310906551-b924f5ef)")
	var kr *CursorKeyRejectedError
	require.ErrorAs(t, err, &kr)
	require.Equal(t, "1792310906551-b924f5ef", kr.TurnID)
	require.True(t, errors.Is(err, ErrCursorKeyRejectedUpstream))
	require.False(t, errors.Is(err, ErrCursorUpstreamModelUnavailable))
}

func TestCursorMatchKeyRejected_ModelUnavailableWithoutSuffix(t *testing.T) {
	err := cursorMatchKeyRejected("当前模型暂不可用，请发送 继续 或新开对话")
	var mu *CursorModelUnavailableError
	require.ErrorAs(t, err, &mu)
	require.Empty(t, mu.TurnID)
	require.Equal(t, "当前模型暂不可用，请发送 继续 或新开对话", mu.Template)
}

func TestCursorMatchKeyRejected_PlainTextNotMatched(t *testing.T) {
	for _, text := range []string{
		"",
		"这个模型目前不可用的原因是配置问题。",
		"当前模型暂不可用",
		"当前模型暂不可用，请稍后再试",
		"请发送 继续 或新开对话",
	} {
		require.NoErrorf(t, cursorMatchKeyRejected(text), "text %q", text)
	}
}

// 一次性解析入口：命中即终态失败，不产出任何 SSE、不当成功。
func TestCursorParseStream_ModelUnavailableFailsClosed(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, cursorModelUnavailableLiveSample)...)
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedInputTokens, 100},
		[2]uint64{cursorFldTurnEndedOutputTokens, 20})...)

	lines, err := cursorNewStreamParser("m").cursorParseStream(stream)
	require.Nil(t, lines)
	require.ErrorIs(t, err, ErrCursorUpstreamModelUnavailable)
	require.NotErrorIs(t, err, ErrCursorKeyRejectedUpstream)

	_, err = cursorParseStreamToSSE(stream, "m")
	require.ErrorIs(t, err, ErrCursorUpstreamModelUnavailable)
	_, err = cursorParseStreamToCompletion(stream, "m")
	require.ErrorIs(t, err, ErrCursorUpstreamModelUnavailable)
}

// 流式读循环：模板帧到达时 next() 返回该错误，且该帧文本没有写进输出缓冲。
func TestCursorSSEStream_ModelUnavailableFrameNotWritten(t *testing.T) {
	var frames []byte
	frames = append(frames, cursorTextDeltaFrame(t, "前言正文")...)
	frames = append(frames, cursorTextDeltaFrame(t, "\n</think>\n\n[当前模型暂不可用，")...)
	frames = append(frames, cursorTextDeltaFrame(t, "请发送 继续 或新开对话 (1793521393111-0d01dd7b)]")...)

	s := &cursorSSEStream{
		source: io.NopCloser(bytes.NewReader(frames)),
		parser: cursorNewStreamParser("m"),
	}
	require.NoError(t, s.next())
	require.Contains(t, s.buffer.String(), "前言正文")
	require.NoError(t, s.next(), "模板未完整到达前不得误判")
	before := s.buffer.String()

	err := s.next()
	var mu *CursorModelUnavailableError
	require.ErrorAs(t, err, &mu)
	require.Equal(t, "1793521393111-0d01dd7b", mu.TurnID)
	require.Equal(t, before, s.buffer.String(), "命中帧不得写出")
	require.NotContains(t, s.buffer.String(), "新开对话")
}
