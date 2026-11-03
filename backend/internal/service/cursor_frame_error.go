package service

// 本文件给 `ErrCursorProtocol` 这个**兜底大类**补一层"帧级类别 + 帧序号"。
//
// 背景（11-03 立项）：生产上 `CUR-EXEC-03` 的 `error_class` 永远只写 `protocol_frame`，
// 而 `ErrCursorProtocol` 在 Cursor 链路里有 48 处包裹点（信封长度、protobuf 字段、
// turn_ended 字段、listed 计数、mcp args 解码、duplex KV、委托登记、内联工具调用…）。
// 事故复盘只能看到"某个帧结构坏了"，无法知道"第几帧、坏在哪一类"，因此无法定位根因。
//
// 设计约束（刻意保守）：
//  1. **不改变错误文案**：`Error()` 仍渲染 `cursor: malformed protocol frame (fail closed): <tail>`，
//     与改造前逐字一致，避免影响面向用户的提示与既有测试断言。
//  2. **不改变哨兵语义**：`Unwrap()` 仍含 `ErrCursorProtocol`，`errors.Is` 判定与
//     `CursorUserFacingError` → `CUR-EXEC-03` 的映射完全不变。
//  3. **不丢底层原因**：需要保留 `%w` 链的（如读流时的 io 错误）走 `cursorFrameErrorWrapf`，
//     用 `Unwrap() []error` 同时暴露哨兵与 cause。
//  4. 只输出枚举值，绝不把帧内容写进类别或日志（帧里可能有上游错误文案/账号信息）。

import (
	"errors"
	"fmt"

	"go.uber.org/zap"
)

// cursorFrameClass 是 Cursor 协议失败关闭的**帧级类别**枚举。
//
// 命名即日志字段取值（稳定契约，不可随意改名——生产看板按它聚合）。
type cursorFrameClass string

const (
	// cursorFrameEnvelope：5 字节信封层——头截断、长度声明与实收不符、超大帧、压缩帧、
	// 读取中途 io 错误。特征是"还没进到 protobuf 就坏了"。
	cursorFrameEnvelope cursorFrameClass = "frame_envelope"

	// cursorFrameProtobuf：protobuf 字段层——非法字段号、字段声明长度越界、
	// 不支持的 wire type（fixed32/fixed64）、varint 溢出/过长/截断。
	cursorFrameProtobuf cursorFrameClass = "frame_protobuf"

	// cursorFrameMessage：消息语义层——turn_ended 计费字段 wire/取值非法、
	// tool_requests_listed 缺 call_count 或 wire/越界、EndStream 之后多出帧、
	// InteractionQuery 字段不是 message。
	cursorFrameMessage cursorFrameClass = "frame_message"

	// cursorFrameEndStreamPayload：EndStream 帧 payload 不是合法 JSON（该帧 payload 是 JSON
	// 而非 protobuf，单独一类便于区分"终态帧本身畸形"与"业务报错"）。
	cursorFrameEndStreamPayload cursorFrameClass = "frame_endstream_payload"

	// cursorFrameMcpArgs：mcp_args 解码/还原层——Value oneof kind 未设置、非有限数值、
	// protojson 序列化/压缩失败、按 schema retype 失败。
	cursorFrameMcpArgs cursorFrameClass = "frame_mcp_args"

	// cursorFrameDuplex：AgentRun 双工/KV 层——缺 AgentRun 请求、预取 blob 畸形、
	// KV 存储超限/未知 blob/不支持的操作、exec 调度顺序破坏、响应超限、原生 exec 拒绝字段。
	cursorFrameDuplex cursorFrameClass = "frame_duplex"

	// cursorFrameDelegation：委托登记/封口簿记层——重复委托 ID、缺 tool_call_id、
	// mcp_args 缺 f11、listed 计数为负、wire id 分配耗尽、nil stream。
	cursorFrameDelegation cursorFrameClass = "frame_delegation"

	// cursorFrameInlineToolCall：上游把工具调用写进了正文（未走结构化委托帧）。
	cursorFrameInlineToolCall cursorFrameClass = "frame_inline_tool_call"
)

// cursorProtocolFramePrefix 是 `ErrCursorProtocol` 的固定前缀，逐字保持与改造前一致。
const cursorProtocolFramePrefix = "cursor: malformed protocol frame (fail closed): "

// cursorFrameError 是带帧级类别的协议错误。
//
// index 为 -1 表示"流路径未提供帧序号"（非流路径、或错误发生在读第一帧之前）。
type cursorFrameError struct {
	class cursorFrameClass
	index int
	tail  string
	cause error
}

// Error 逐字复刻改造前 `fmt.Errorf("%w: <tail>", ErrCursorProtocol, …)` 的渲染结果。
func (e *cursorFrameError) Error() string {
	return cursorProtocolFramePrefix + e.tail
}

// Unwrap 暴露哨兵与可选底层原因。
//
// 用 []error 形式（Go 1.20+）：`errors.Is(err, ErrCursorProtocol)` 仍为 true，
// 同时 `errors.Is(err, io.ErrUnexpectedEOF)` 之类的原有判定不退化。
func (e *cursorFrameError) Unwrap() []error {
	if e.cause != nil {
		return []error{ErrCursorProtocol, e.cause}
	}
	return []error{ErrCursorProtocol}
}

// FrameClass 返回帧级类别（诊断用）。
func (e *cursorFrameError) FrameClass() cursorFrameClass { return e.class }

// FrameIndex 返回帧序号；-1 表示未知。
func (e *cursorFrameError) FrameIndex() int { return e.index }

// cursorFrameErrorf 构造一条帧级协议错误。
func cursorFrameErrorf(class cursorFrameClass, format string, args ...any) error {
	return &cursorFrameError{class: class, index: -1, tail: fmt.Sprintf(format, args...)}
}

// cursorFrameErrorWrapf 构造一条带底层原因的帧级协议错误。
func cursorFrameErrorWrapf(class cursorFrameClass, cause error, format string, args ...any) error {
	return &cursorFrameError{class: class, index: -1, tail: fmt.Sprintf(format, args...), cause: cause}
}

// cursorFrameClassify 取出错误链里的帧级类别与帧序号。
//
// ok=false 表示这不是一条帧级错误（或根本没有帧级信息）。
func cursorFrameClassify(err error) (class cursorFrameClass, index int, ok bool) {
	var fe *cursorFrameError
	if errors.As(err, &fe) && fe != nil {
		return fe.class, fe.index, true
	}
	// *cursorRetypeError 是既有结构化错误（Unwrap 到 ErrCursorProtocol，带 §3.5 原因枚举），
	// 它本身没有帧序号；归入 mcp_args 家族，避免退化回 protocol_frame 兜底。
	var re *cursorRetypeError
	if errors.As(err, &re) && re != nil {
		return cursorFrameMcpArgs, -1, true
	}
	return "", -1, false
}

// cursorAnnotateFrame 给帧级错误补上"第几帧"，并原样返回非帧级错误。
//
// 用于读循环顶层：流路径每读一帧递增计数，读失败时把序号写进错误，
// 生产日志即可给出 `frame_index=N frame_class=frame_envelope`。
func cursorAnnotateFrame(err error, index int) error {
	if err == nil {
		return nil
	}
	var fe *cursorFrameError
	if !errors.As(err, &fe) || fe == nil || fe.index >= 0 {
		return err
	}
	clone := *fe
	clone.index = index
	return &clone
}

// cursorFrameErrFields 把帧级诊断渲染成 zap 安全字段（供日志附加，无帧级信息时返回 nil）。
//
// 只写枚举与整数，帧内容一概不落盘。
func cursorFrameErrFields(err error) []zap.Field {
	class, index, ok := cursorFrameClassify(err)
	if !ok {
		return nil
	}
	fields := []zap.Field{zap.String("protocol_frame_class", string(class))}
	if index >= 0 {
		fields = append(fields, zap.Int("protocol_frame_index", index))
	}
	return fields
}

// CursorFrameErrFieldsForLog 是 cursorFrameErrFields 的导出包装，供 handler 层在
// `cursor_user_error` 日志上补齐帧级诊断字段（与 service 内部同一套枚举，不重复实现）。
func CursorFrameErrFieldsForLog(err error) []zap.Field {
	return cursorFrameErrFields(err)
}

// CursorStreamErrClassForLog 是 cursorStreamErrClass 的导出包装（handler 层日志用）。
//
// 与 service 内部同一实现，保证"终态日志"和"用户可见错误日志"的 error_class 取值完全一致。
func CursorStreamErrClassForLog(err error) string {
	return cursorStreamErrClass(err)
}
