package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// newLivelockUpstream 起一个恒定返回 {"code":429} 的假上游。
//
// 关键点：GetCredentialsSecure 在 JSON 解码后先校验 `code != 200` 就直接返回错误，
// 根本走不到 AES 解密，所以这里的 mock 不需要实现任何加密协议。
// 返回的 body 里带 "429"，用于断言错误信息确实来自上游而不是本地构造。
func newLivelockUpstream(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":429,"msg":"too many requests"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCursorDynamicPool_UpstreamErrorBoundedWithoutCooldown 锁定 429 活锁修复。
//
// 回归背景：pullNewTokenFromAnyCard 的 `err != nil` 分支原先只归还预留额度就 continue，
// 既不冷却该卡也不终止循环。上游持续返回 429（或 ctx 被取消导致每次请求立即失败）时，
// 每一轮循环都会重新选中同一张卡，形成无限活锁（实测 5 秒打上游 61686 次）。
//
// 修复后：引入「本轮已尝试」集合（tried），每张卡在一次调用里最多被打上游一次
// → 全轮试完必然 targetCard == nil → 返回 lastErr，循环必然终止。
//
// 冷却语义：429 / 网络错误属于瞬时限流，**不应**给卡加冷却（否则会误判卡不可用）；
// 只有上游明确回 force_reused（拉号过频）才给该卡加 60s 冷却。
//
// 本用例用单卡 + 恒定 429 的上游验证四件事：
//  1. AcquireToken 在超时内返回错误（不再空转）；
//  2. 上游被打的次数有界（活锁时会飙升到几万次）;
//  3. 失败的卡**没有**被误判冷却（CooldownUntil 为零值）;
//  4. 预留额度被归还。
func TestCursorDynamicPool_UpstreamErrorBoundedWithoutCooldown(t *testing.T) {
	var hits atomic.Int64
	srv := newLivelockUpstream(t, &hits)

	pool := NewCursorDynamicPool(srv.URL, zap.NewNop())
	pool.RegisterCard(1, "key", "dev", "md5", 100, 100)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type acqResult struct {
		tok *DynamicCursorToken
		err error
	}
	resCh := make(chan acqResult, 1)
	go func() {
		tok, err := pool.AcquireToken(ctx, "sess-livelock")
		resCh <- acqResult{tok: tok, err: err}
	}()

	var res acqResult
	select {
	case res = <-resCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("livelock: AcquireToken 未在 10s 内返回，上游已被打 %d 次（活锁未修复）", hits.Load())
	}

	if res.err == nil {
		t.Fatalf("期望上游 429 导致取号失败，实际返回 token=%+v", res.tok)
	}
	if !errors.Is(res.err, ErrNoAvailableCursorAutoToken) {
		t.Errorf("期望错误包装 ErrNoAvailableCursorAutoToken，实际 err=%v", res.err)
	}
	if msg := res.err.Error(); !strings.Contains(msg, "429") {
		t.Errorf("期望错误信息包含上游 429，实际 err=%v", res.err)
	}

	// 循环必须是有界的：单卡场景下最多打上游一次（第二轮即发现无可用新卡）。
	if n := hits.Load(); n > 2 {
		t.Fatalf("上游被打 %d 次，循环未终止（活锁）", n)
	}

	cards := pool.ListCards()
	if len(cards) != 1 {
		t.Fatalf("期望 1 张卡，实际 %d 张", len(cards))
	}
	// 429 是瞬时限流，不该给卡加冷却，否则会误判该卡不可用。
	if !cards[0].CooldownUntil.IsZero() {
		t.Errorf("期望 429 不冷却该卡，但 CooldownUntil=%v 非零值", cards[0].CooldownUntil)
	}
	// 预留额度必须归还，否则冷却结束后这张卡会被永久扣减。
	if cards[0].OverCount != 100 {
		t.Errorf("期望预留额度已归还 (OverCount=100)，实际 %d", cards[0].OverCount)
	}
}

// TestCursorDynamicPool_UpstreamErrorRetriesAtMostOncePerCard 锁定多卡场景的重试上界。
//
// 上游恒定 429 时，每张卡本轮至多被打一次（tried 集合保证），
// 因此取号失败的重试次数最多等于卡密数量；
// 全部卡试完后必须返回错误而不是继续空转，且全程不冷却任何卡。
func TestCursorDynamicPool_UpstreamErrorRetriesAtMostOncePerCard(t *testing.T) {
	const cardCount = 3

	var hits atomic.Int64
	srv := newLivelockUpstream(t, &hits)

	pool := NewCursorDynamicPool(srv.URL, zap.NewNop())
	for i := int64(1); i <= cardCount; i++ {
		pool.RegisterCard(i, "key", "dev", "md5", 100, 100)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type acqResult struct {
		tok *DynamicCursorToken
		err error
	}
	resCh := make(chan acqResult, 1)
	go func() {
		tok, err := pool.AcquireToken(ctx, "sess-livelock-multi")
		resCh <- acqResult{tok: tok, err: err}
	}()

	var res acqResult
	select {
	case res = <-resCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("livelock: AcquireToken 未在 10s 内返回，上游已被打 %d 次（活锁未修复）", hits.Load())
	}

	if res.err == nil {
		t.Fatalf("期望上游 429 导致取号失败，实际返回 token=%+v", res.tok)
	}
	if n := hits.Load(); n > cardCount {
		t.Fatalf("上游被打 %d 次，超过卡密数量 %d（每卡至多失败一次），循环未终止", n, cardCount)
	}

	cards := pool.ListCards()
	if len(cards) != cardCount {
		t.Fatalf("期望 %d 张卡，实际 %d 张", cardCount, len(cards))
	}
	for _, c := range cards {
		// 429 是瞬时限流，不该给卡加冷却。
		if !c.CooldownUntil.IsZero() {
			t.Errorf("卡 %d 期望 429 不冷却，但 CooldownUntil=%v 非零值", c.ID, c.CooldownUntil)
		}
		if c.OverCount != 100 {
			t.Errorf("卡 %d 期望预留额度已归还 (OverCount=100)，实际 %d", c.ID, c.OverCount)
		}
	}
}
