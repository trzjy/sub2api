package service

// 文件：internal/service/upstream_heartbeat_context.go
//
// CF524 D2 心跳 owner 的 context 传递契约（G2a，与 G1a 预算快照 helper 同型）。
//
// 职责边界（与方案冲突处以报告说明，不自行改语义）：
//   - 本文件只承载"请求级心跳 owner 在 context 中的写/读"——owner 的创建与 writer
//     绑定由 G2b 在 handler 入口安装点完成；本卡（G2a）的三挂点只消费 owner，禁止重建。
//   - 无 owner 时 upstreamHeartbeatFromContext 返回 (nil, false)，按方案定义不兜底
//     ——挂点据以走既有无护栏旧路径（与 G1b 挂点 3 同型）。

import "context"

type upstreamHeartbeatKey struct{}

// withUpstreamHeartbeat 把请求级心跳 owner 写入 context（G2b 入口安装点调用）。
func withUpstreamHeartbeat(ctx context.Context, hb *UpstreamHeartbeat) context.Context {
	return context.WithValue(ctx, upstreamHeartbeatKey{}, hb)
}

// upstreamHeartbeatFromContext 从 context 读回请求级心跳 owner。无 owner 时返回
// (nil, false)——按方案定义不兜底（调用方据以走既有无护栏旧路径）。
func upstreamHeartbeatFromContext(ctx context.Context) (*UpstreamHeartbeat, bool) {
	if ctx == nil {
		return nil, false
	}
	hb, ok := ctx.Value(upstreamHeartbeatKey{}).(*UpstreamHeartbeat)
	if !ok || hb == nil {
		return nil, false
	}
	return hb, true
}
