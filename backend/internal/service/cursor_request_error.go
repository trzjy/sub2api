package service

import "fmt"

// CursorRequestError 是 Cursor 链路上带面向用户错误码的失败（方案 §2.2.1）。
// Unwrap 返回 ErrCursorUnsupportedRequest，既有 errors.Is 判定（cursor_phase_log.go 等）不受影响。
type CursorRequestError struct {
	Code   string // CUR-REQ-xx / CUR-EXEC-02 / CUR-EXEC-05 / CUR-EXEC-07 / CUR-EXEC-08（取自 pkg/errors 常量）
	Status int    // 400 用户请求问题；502 上游；500 管理员配置 / 内部不变量
	Detail string // 原英文细节，只进日志，不给用户
}

// Error 实现 error："cursor: " + Detail。
func (e *CursorRequestError) Error() string {
	return "cursor: " + e.Detail
}

// Unwrap 返回 ErrCursorUnsupportedRequest，保持既有 errors.Is 判定。
func (e *CursorRequestError) Unwrap() error {
	return ErrCursorUnsupportedRequest
}

// newCursorRequestError 在产生点标码构造 CursorRequestError，Detail = fmt.Sprintf(format, args...)。
//
// 约定：args 里若有 error，必须用 %v 格式化成文本，不得用 %w 把内部 error 再 wrap 进来。
// 本类型的 Unwrap 只指向 ErrCursorUnsupportedRequest；若内部 error 也进入链，
// errors.Is 可能同时命中别的哨兵（如 ErrCursorProtocol），造成 CursorUserFacingError 映射歧义。
func newCursorRequestError(code string, status int, format string, args ...any) error {
	return &CursorRequestError{Code: code, Status: status, Detail: fmt.Sprintf(format, args...)}
}
