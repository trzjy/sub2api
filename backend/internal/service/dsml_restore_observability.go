package service

import (
	"context"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// DSML 工具调用还原的观测埋点（2026-11-12）。
//
// 背景：上游 CodeBuddy 按 DeepSeek 官方 DSML 方言把工具调用写成文本，
// 网关回程按官方规范还原成标准 tool_use / function_call（见
// internal/pkg/apicompat/dsml.go）。本文件负责把"还原了多少、丢弃了多少"落一条日志，
// 便于观测上游是否稳定吐 DSML、以及丢弃是否异常增多。
//
// 口径：
//   - 字段名沿用既有 skipped_total / skipped_types 家族（cursor_skip_ledger.go），
//     运维只需记一套字段；
//   - 只在有 DSML 出现时落日志（无 DSML 静默，不制造噪声）；
//   - 绝不回显任何 DSML 原文或工具参数（可能含用户数据）。
const (
	dsmlRestoredEvent = "dsml_tool_calls_restored"
	dsmlDroppedEvent  = "dsml_tool_calls_dropped"
)

// dsmlRestoreFields 组装 DSML 埋点字段（还原/丢弃两处共用）。
func dsmlRestoreFields(restored, dropped int) []zap.Field {
	return []zap.Field{
		zap.Int("skipped_total", restored+dropped),
		zap.Int("dsml_restored", restored),
		zap.Int("dsml_dropped", dropped),
	}
}

// logDSMLRestore 在流结束时落一条 DSML 观测日志。
//
// 由转换层（apicompat）把计数挂在 state 上，本函数是**唯一出口** ——
// 不在每个调用点各记一遍（那会散成 N 份口径，违反唯一事实源）。
//
// restored/dropped 均为 0 时静默：绝大多数请求根本没有 DSML。
func logDSMLRestore(ctx context.Context, model string, restored, dropped int) {
	if restored == 0 && dropped == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	level := zapcore.InfoLevel
	event := dsmlRestoredEvent
	if restored == 0 {
		// 只有丢弃、没有还原 → 视为异常（上游给了 DSML 却还原不出调用）。
		level = zapcore.WarnLevel
		event = dsmlDroppedEvent
	}
	cursorPhaseLog(ctx, level, event, model, dsmlRestoreFields(restored, dropped)...)
}
