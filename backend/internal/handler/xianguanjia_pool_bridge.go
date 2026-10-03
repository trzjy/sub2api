package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
)

// D4i: admin 端点桥接适配器。
//
// admin.XianguanjiaPoolPusher 的签名引用 admin 包自身类型（XgjCardPair/XgjPushResult），
// 而 admin 包已 import xianguanjia（D3 config handler），service 包不能反向 import
// admin（依赖环）。故 Pusher 桥接放 handler 侧：本文件同时可见 admin 与 xianguanjia，
// 只做类型搬运，不含业务逻辑。KindService 侧签名全为基本类型，LazyKindService
// 已隐式满足 admin.XianguanjiaKindService，无需桥接。

// xgjKindServiceFromLazy 把 xianguanjia.LazyKindService 适配为 admin.XianguanjiaKindService。
// 签名一致（KindCreate/KindCurrent，参数与返回均为基本类型），直接断言即可（编译期校验）。
func xgjKindServiceFromLazy(lazy *xianguanjia.LazyKindService) admin.XianguanjiaKindService {
	return lazy
}

// xgjPoolPushBridge 是 admin.XianguanjiaPoolPusher 的 handler 侧桥接：
// XgjCardPair ↔ xianguanjia.CardPair、XgjPushResult ↔ xianguanjia.PushResult 双向搬运。
type xgjPoolPushBridge struct {
	inner *xianguanjia.LazyPoolSyncService
}

func (b *xgjPoolPushBridge) PushCards(ctx context.Context, kindID int64, cards []admin.XgjCardPair) (admin.XgjPushResult, error) {
	converted := make([]xianguanjia.CardPair, len(cards))
	for i, c := range cards {
		converted[i] = xianguanjia.CardPair{CardNo: c.CardNo, CardPwd: c.CardPwd}
	}
	res, err := b.inner.PushCards(ctx, kindID, converted)
	if err != nil {
		return admin.XgjPushResult{}, err
	}
	out := admin.XgjPushResult{
		Total:     len(res.Succeeded) + len(res.Failed),
		Succeeded: len(res.Succeeded),
		Failed:    len(res.Failed),
	}
	for _, f := range res.Failed {
		out.Failures = append(out.Failures, f.CardNo)
	}
	return out, nil
}

// newXgjPoolPushBridge 构造桥接（编译期实现校验）。
func newXgjPoolPushBridge(inner *xianguanjia.LazyPoolSyncService) admin.XianguanjiaPoolPusher {
	return &xgjPoolPushBridge{inner: inner}
}
