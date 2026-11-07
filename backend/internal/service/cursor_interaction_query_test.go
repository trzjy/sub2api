//go:build unit

package service

// Q1：duplex 应答 InteractionQuery（AgentServerMessage.f7）的定向测试。
//
// 依据 docs-local/cursor-tool-failure-user-diagnosis-plan-20261101.md §1.2 应答表：
//   - 10 种可应答询问逐一构造 → 断言回帧外层 AgentClientMessage.f6、InteractionResponse.f1 id
//     原样、oneof 字段号与询问同号、内层形状与 reason/error 文本逐层解回精确一致；
//   - mcp_auth 两分支（sub2api → approved；其它名 → rejected 且 reason 含该名）；
//   - f12 generate_image / 不存在字段 / 缺 oneof：派发单 D 阶段 2 起回通用 rejected 应答、整轮继续，
//     不再 502 失败关闭（不写失败关闭帧）；
//   - 子消息解码失败：返回 ErrCursorProtocol 且不写帧；
//   - 日志 interaction_query_answered 不含 args 正文（搜索词 / URL / 问题文本）。
//
// 本文件所有字段号均为字面数字（取自证据 39b 与方案 §1.2），不引用实现常量，防止自证。

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const cursorIQTestMarker = "IQ-SECRET-MARKER-7f3a"

func cursorIQTestDuplex(t *testing.T) (*cursorAgentDuplex, *observer.ObservedLogs, func() []byte) {
	t.Helper()
	pr, pw := io.Pipe()
	core, logs := observer.New(zap.DebugLevel)
	d := &cursorAgentDuplex{
		reader:      pr,
		writer:      pw,
		delegations: newLocalCursorToolDelegations(),
		lg:          zap.New(core),
	}
	return d, logs, captureDuplex(t, d)
}

// cursorIQTestQuery 构造 AgentServerMessage{f7 InteractionQuery{f1 id, f<field> sub}}。
func cursorIQTestQuery(id uint64, field int, sub []byte) []byte {
	q := cursorAppendVarintField(nil, 1, id)
	q = cursorAppendBytesField(q, field, sub)
	return cursorAppendBytesField(nil, 7, q)
}

// cursorIQTestArgs 构造 {f1 args{f1 <marker 文本>}}，模拟携带搜索词 / URL / 问题的询问。
func cursorIQTestArgs(text string) []byte {
	return cursorAppendBytesField(nil, 1, cursorAppendStringField(nil, 1, text))
}

// cursorIQTestDecodeOnlyResponse 把捕获字节解成唯一一帧，并返回 InteractionResponse 的字段。
func cursorIQTestDecodeOnlyResponse(t *testing.T, wire []byte) []cursorPBField {
	t.Helper()
	require.NotEmpty(t, wire, "必须写出一帧 InteractionResponse")
	frame, next, err := cursorReadEnvelopeFrame(wire, 0)
	require.NoError(t, err)
	require.Equal(t, len(wire), next, "只应写出一帧")
	outer, err := cursorDecodeProtobuf(frame.Data)
	require.NoError(t, err)
	require.Len(t, outer, 1)
	require.Equal(t, 6, outer[0].Number, "外层必须是 AgentClientMessage.f6 InteractionResponse")
	require.Equal(t, 2, outer[0].Wire)
	resp, err := cursorDecodeProtobuf(outer[0].Bytes)
	require.NoError(t, err)
	return resp
}

// cursorIQTestDescend 沿 path 逐层下钻，每层必须恰好只有该字段号一个字段，返回最内层消息字节。
func cursorIQTestDescend(t *testing.T, msg []byte, path []int) []byte {
	t.Helper()
	for _, num := range path {
		fields, err := cursorDecodeProtobuf(msg)
		require.NoError(t, err)
		require.Len(t, fields, 1, "path %v 层级字段数不为 1", path)
		require.Equal(t, num, fields[0].Number, "path %v 字段号不符", path)
		require.Equal(t, 2, fields[0].Wire)
		msg = fields[0].Bytes
	}
	return msg
}

// cursorIQTestCase：path 是 InteractionResponse.f<field> 之下逐层的字段号；
// leaf 为最内层期望的字段（字段号 → 字符串值），nil 表示最内层是空消息。
type cursorIQTestCase struct {
	name   string
	field  int
	sub    []byte
	path   []int
	leaf   map[int]string
	answer string
}

func cursorIQTestAnswerCases() []cursorIQTestCase {
	args := cursorIQTestArgs(cursorIQTestMarker)
	mcpAuth := func(id string) []byte {
		return cursorAppendBytesField(nil, 1, cursorAppendStringField(nil, 1, id))
	}
	return []cursorIQTestCase{
		{name: "f2_web_search", field: 2, sub: args, path: []int{1}, answer: "approved"},
		{name: "f9_web_fetch", field: 9, sub: args, path: []int{1}, answer: "approved"},
		{name: "f11_mcp_auth_sub2api", field: 11, sub: mcpAuth("sub2api"), path: []int{1}, answer: "approved"},
		{name: "f11_mcp_auth_other", field: 11, sub: mcpAuth("other-srv"), path: []int{2},
			leaf: map[int]string{1: "MCP server 'other-srv' not found"}, answer: "rejected"},
		{name: "f8_setup_vm_environment", field: 8,
			sub:  cursorAppendStringField(nil, 2, cursorIQTestMarker),
			path: []int{1}, answer: "success"},
		{name: "f3_ask_question", field: 3, sub: args, path: []int{1, 3},
			leaf:   map[int]string{1: "No interactive user is available in this environment. Ask the user directly in your reply text."},
			answer: "rejected"},
		{name: "f4_switch_mode", field: 4, sub: args, path: []int{2},
			leaf:   map[int]string{1: "Mode switching is not supported in this environment. Continue in the current agent mode."},
			answer: "rejected"},
		{name: "f7_create_plan", field: 7, sub: args, path: []int{1, 2},
			leaf:   map[int]string{1: "Plan files cannot be created in this environment. List the plan directly in your reply."},
			answer: "error"},
		{name: "f10_pr_management", field: 10, sub: args, path: []int{2},
			leaf: map[int]string{1: "PR management is only available in cloud agents"}, answer: "error"},
		// setup_logs 为空串：proto3 默认值省略，线上与显式写 "" 等价，故内层只出现 f1。
		{name: "f13_replace_env", field: 13, sub: args, path: []int{2},
			leaf: map[int]string{1: "Environment replacement is not supported in the IDE client"}, answer: "failure"},
		{name: "f14_connect_scm", field: 14, sub: args, path: []int{2},
			leaf: map[int]string{1: "Connecting source control is not available in this environment"}, answer: "rejected"},
	}
}

func TestCursorInteractionQueryAnswers(t *testing.T) {
	for i, tc := range cursorIQTestAnswerCases() {
		t.Run(tc.name, func(t *testing.T) {
			id := uint64(100 + i)
			d, logs, stop := cursorIQTestDuplex(t)
			done, err := d.respond(cursorIQTestQuery(id, tc.field, tc.sub))
			require.NoError(t, err)
			require.False(t, done)
			resp := cursorIQTestDecodeOnlyResponse(t, stop())

			require.Len(t, resp, 2, "InteractionResponse 只应含 f1 id 与一个 oneof")
			require.Equal(t, 1, resp[0].Number)
			require.Equal(t, 0, resp[0].Wire)
			require.Equal(t, id, resp[0].Varint, "id 必须原样回写")
			require.Equal(t, tc.field, resp[1].Number, "result oneof 必须与询问同号")
			require.Equal(t, 2, resp[1].Wire)

			leaf := cursorIQTestDescend(t, resp[1].Bytes, tc.path)
			got, err := cursorDecodeProtobuf(leaf)
			require.NoError(t, err)
			if tc.leaf == nil {
				require.Empty(t, got, "最内层应为空消息")
			} else {
				require.Len(t, got, len(tc.leaf))
				for _, f := range got {
					want, ok := tc.leaf[f.Number]
					require.True(t, ok, "意外的内层字段 f%d", f.Number)
					require.Equal(t, 2, f.Wire)
					require.Equal(t, want, f.String())
				}
			}

			entries := logs.FilterField(zap.String("phase", "interaction_query_answered")).All()
			require.Len(t, entries, 1)
			ctx := entries[0].ContextMap()
			require.EqualValues(t, id, ctx["query_id"])
			require.EqualValues(t, tc.field, ctx["query_field"])
			require.Equal(t, tc.answer, ctx["answer"])
		})
	}
}

func TestCursorInteractionQueryMcpAuthRejectReasonContainsName(t *testing.T) {
	d, _, stop := cursorIQTestDuplex(t)
	sub := cursorAppendBytesField(nil, 1, cursorAppendStringField(nil, 1, "someone-else"))
	_, err := d.respond(cursorIQTestQuery(9, 11, sub))
	require.NoError(t, err)
	resp := cursorIQTestDecodeOnlyResponse(t, stop())
	leaf := cursorIQTestDescend(t, resp[1].Bytes, []int{2})
	got, err := cursorDecodeProtobuf(leaf)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Contains(t, got[0].String(), "someone-else")
}

func TestCursorInteractionQueryUnsupportedFailsClosed(t *testing.T) {
	// 派发单 D 阶段 2：未知 interaction 类型（f12 generate_image / f30 未登记字段）不再以 502 失败关闭，
	// 而是回通用 rejected 应答让模型换条路继续（不写失败关闭帧）。
	for _, tc := range []struct {
		name    string
		payload []byte
		field   int
	}{
		{name: "f12_generate_image", payload: cursorIQTestQuery(5, 12, cursorIQTestArgs("draw")), field: 12},
		{name: "f30_unknown", payload: cursorIQTestQuery(6, 30, cursorIQTestArgs("x")), field: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, logs, stop := cursorIQTestDuplex(t)
			// 派发单 D 阶段 2：未知 interaction 类型不再以 502 失败关闭，而是回通用
			// rejected 应答让模型换条路继续（绝不写帧失败关闭）。
			_, err := d.respond(tc.payload)
			require.NoError(t, err, "未知 interaction 类型不得 502（派发单 D：回通用 rejected，整轮继续）")
			require.Equal(t, 0, phaseCount(logs, "close"), "连接不得被终止")
			// 必须写出通用 rejected 应答 {query_field:{f2:{f1:reason}}}。
			cursorStage2ExpectInteractionRejected(t, bytes.NewBuffer(stop()))
			entries := logs.FilterField(zap.String("phase", "interaction_query_unsupported")).All()
			require.Len(t, entries, 1)
			require.EqualValues(t, tc.field, entries[0].ContextMap()["query_field"])
		})
	}
	// 缺 oneof（只有 f1 id、无实际 query 字段 → query field=0）：同样不再 502，回通用 rejected 继续；
	// 因 query field 为 0，回包外层 field=0 在重新解码时被视为非法字段号，故只校验「写帧 + 不终止」，
	// 不按常规 rejected 形状逐层解码（生产对此边角仍回帧、不关闭连接）。
	t.Run("missing_oneof", func(t *testing.T) {
		d, logs, stop := cursorIQTestDuplex(t)
		_, err := d.respond(cursorAppendBytesField(nil, 7, cursorAppendVarintField(nil, 1, 7)))
		require.NoError(t, err, "缺 oneof interaction 不得 502（派发单 D：回通用 rejected，整轮继续）")
		require.Equal(t, 0, phaseCount(logs, "close"), "连接不得被终止")
		require.NotEmpty(t, stop(), "缺 oneof 必须写出一帧通用 rejected 应答（即使外层 field=0）")
	})
}

func TestCursorInteractionQueryMalformedSubmessage(t *testing.T) {
	bad := []byte{0x0a, 0x05, 0x01} // f1 声明 5 字节，实际只有 1 字节
	cases := map[string][]byte{
		"web_search_sub": cursorIQTestQuery(1, 2, bad),
		"mcp_auth_args":  cursorIQTestQuery(2, 11, cursorAppendBytesField(nil, 1, bad)),
		"query_body":     cursorAppendBytesField(nil, 7, bad),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			d, _, stop := cursorIQTestDuplex(t)
			_, err := d.respond(payload)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrCursorProtocol), "got %v", err)
			require.Empty(t, stop(), "解码失败时不得写出任何帧")
		})
	}
}

func TestCursorInteractionQueryLogOmitsArgs(t *testing.T) {
	queries := map[string][]byte{
		"web_search":   cursorIQTestQuery(1, 2, cursorIQTestArgs("search_term "+cursorIQTestMarker)),
		"web_fetch":    cursorIQTestQuery(2, 9, cursorIQTestArgs("https://example.invalid/"+cursorIQTestMarker)),
		"ask_question": cursorIQTestQuery(3, 3, cursorIQTestArgs("question "+cursorIQTestMarker)),
	}
	for name, payload := range queries {
		t.Run(name, func(t *testing.T) {
			d, logs, stop := cursorIQTestDuplex(t)
			_, err := d.respond(payload)
			require.NoError(t, err)
			_ = stop()
			entries := logs.FilterField(zap.String("phase", "interaction_query_answered")).All()
			require.Len(t, entries, 1)
			for _, e := range logs.All() {
				require.NotContains(t, e.Message, cursorIQTestMarker)
				for k, v := range e.ContextMap() {
					require.False(t, strings.Contains(fmt.Sprint(v), cursorIQTestMarker), "日志字段 %s 泄露了 args 正文", k)
				}
			}
		})
	}
}
