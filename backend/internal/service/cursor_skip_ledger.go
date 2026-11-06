package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// cursorSkipLedger 是「本层不支持 → 丢弃而不是报错」的唯一账本（丢弃原因 → 条数）。
//
// 为什么要有这个东西（产品口径，2026-11-06 明确要求）：
// 客户端每多一种新形状（新的 input 条目类型、新的消息角色、新的多模态 part、
// 非 data URI 的图片链接、畸形的工具 schema），如果一律整轮 400，客户看到的是
// 「你们模型不稳定」，而不是「你少给我一个字段」。所以口径是：
//
//	能丢就丢，丢了记账；只有「整轮真的没有可发送内容」和「能力缺口」才拒绝。
//
// 记账不是可选动作：本站没有指标系统，日志是唯一能看出「客户端在演进什么」的手段。
// 两条入站协议（chat/messages 与 responses）共用这一个账本类型，避免两套口径。
type cursorSkipLedger map[string]int

// 丢弃原因的稳定短串。日志与用例共用，禁止在产生点就地拼字符串——否则下次统计
// 「谁在丢什么」时又要重新对齐一遍名字。
const (
	cursorSkipUnknownRole        = "message_unknown_role"
	cursorSkipEmptyRole          = "message_empty_role"
	cursorSkipBadContent         = "message_bad_content"
	cursorSkipEmptyMessage        = "message_empty"
	cursorSkipUnsupportedPart    = "content_unsupported_part"
	cursorSkipLegacyFunctionCall = "legacy_function_call"
	cursorSkipLegacyFunctionNoID = "legacy_function_without_call_id"
	cursorSkipImageOutsideUser   = "image_outside_user"
	cursorSkipImageInvalid       = "image_invalid"
	cursorSkipToolSchemaInvalid  = "tool_schema_invalid"
	cursorSkipEmptyAssistantTail = "empty_assistant_tail"
)

// note 记一笔丢弃（原因统一小写，空原因按 unknown 记）。
//
// nil 账本静默返回：调用点允许不关心记账（例如探针用的固定请求），
// 但生产路径必须传已初始化的账本。
func (l cursorSkipLedger) note(kind string) {
	if l == nil {
		return
	}
	name := strings.ToLower(strings.TrimSpace(kind))
	if name == "" {
		// 沿用既有口径：没类型的条目按 message 记（线上 responses_items_skipped
		// 已经这么统计过，改名会让历史对比断掉）。
		name = "message"
	}
	l[name]++
}

// total 返回本次丢弃的总条数。
func (l cursorSkipLedger) total() int {
	total := 0
	for _, count := range l {
		total += count
	}
	return total
}

// summary 返回稳定排序的 `原因×条数` 串（排序是为了日志可对比、用例可断言）。
func (l cursorSkipLedger) summary() string {
	names := make([]string, 0, len(l))
	for name, count := range l {
		names = append(names, fmt.Sprintf("%s×%d", name, count))
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// log 把本次丢弃落一条日志；无丢弃时静默（不制造噪声）。
//
// 字段名固定 skipped_total / skipped_types：线上已经有 responses_items_skipped
// 用这两个字段在查，新增的 chat 路径日志沿用同一套字段名，运维只需记一套。
func (l cursorSkipLedger) log(ctx context.Context, event, model string) {
	if len(l) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cursorPhaseLog(ctx, zapcore.InfoLevel, event, model,
		zap.Int("skipped_total", l.total()),
		zap.String("skipped_types", l.summary()),
	)
}
