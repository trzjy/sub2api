package service

import "time"

const (
	// CursorUpstreamFlapThreshold：单请求内已失败的不同账号数达到该值即判定集体抽风。
	// 导出（大写）：handler 包（backend/internal/handler）需要跨包读取该阈值。
	CursorUpstreamFlapThreshold = 3
	// CursorUpstreamFlapCooldown：判定为集体抽风时的停调时长。
	// 导出（大写）：handler 包需要跨包读取该时长。
	CursorUpstreamFlapCooldown = 30 * time.Second
)
