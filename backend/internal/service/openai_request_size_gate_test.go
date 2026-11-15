package service

import (
	"context"
	"testing"
	"time"
)

// fileSessionInputCache 是 SessionInputTokenCache 的最小实现，用于验证「写回 → 下一单读回」闭环。
// 内嵌 GatewayCache 接口只为满足 s.cache 的静态类型；本测试只调到下面两个方法。
type fileSessionInputCache struct {
	GatewayCache
	tokens map[string]int
	sets   int
}

func newFileSessionInputCache() *fileSessionInputCache {
	return &fileSessionInputCache{tokens: make(map[string]int)}
}

func (c *fileSessionInputCache) key(groupID int64, sessionHash string) string {
	return string(rune(groupID)) + "|" + sessionHash
}

func (c *fileSessionInputCache) GetSessionInputTokens(_ context.Context, groupID int64, sessionHash string) (int, error) {
	return c.tokens[c.key(groupID, sessionHash)], nil
}

func (c *fileSessionInputCache) SetSessionInputTokens(_ context.Context, groupID int64, sessionHash string, tokens int, _ time.Duration) error {
	c.sets++
	c.tokens[c.key(groupID, sessionHash)] = tokens
	return nil
}

// TestResolveRequestSizeRouting 钉死分流判定的两条规则：
// ① 上一单真实值是本轮下界；② 已在大活池的会话要跌破滞回下沿才回小池。
// 这两条是「长活被判成小活」（3 天漏判 463 次）的修复本体。
func TestResolveRequestSizeRouting(t *testing.T) {
	cases := []struct {
		name        string
		estimated   int
		lastReal    int
		hasLast     bool
		wantLarge   bool
		wantEffective int
	}{
		{
			name: "没有上一单时保持既有行为_纯本地估算_小活",
			estimated: 150000, hasLast: false,
			wantLarge: false, wantEffective: 150000,
		},
		{
			name: "没有上一单时保持既有行为_纯本地估算_大活",
			estimated: 300000, hasLast: false,
			wantLarge: true, wantEffective: 300000,
		},
		{
			// 这就是生产里的漏判：本地估算 15 万，但同一会话上一单上游报了 29.7 万
			//（984 上那 463 单的平均实际值）。修复前判成小活派给「仅接小请求」的 984；
			// 修复后必须判大。
			name:      "上一单真实值兜底_估算偏低不再漏判",
			estimated: 151024, lastReal: 297080, hasLast: true,
			wantLarge: true, wantEffective: 297080,
		},
		{
			// 真实值本身没到阈值时，仍按小活处理 —— 兜底不能反过来把正常请求推成大活。
			name:      "上一单真实值也不足阈值_保持小活",
			estimated: 151024, lastReal: 238524, hasLast: true,
			wantLarge: false, wantEffective: 238524,
		},
		{
			name: "上一单真实值兜底_超过阈值判大",
			estimated: 150000, lastReal: 300000, hasLast: true,
			wantLarge: true, wantEffective: 300000,
		},
		{
			// 滞回：上一单在大活池，本轮估算掉到 21 万（仍在 256k 线下），
			// 不得立刻回小池，否则会在阈值线上反复换号、清缓存。
			name: "滞回_大活池内小幅回落仍留大池",
			estimated: 210000, lastReal: 300000, hasLast: true,
			wantLarge: true, wantEffective: 300000,
		},
		{
			// 真实压缩（客户端把历史压掉）后才允许回小池。
			name: "滞回_真实回落到下沿以下才回小池",
			estimated: 80000, lastReal: 80000, hasLast: true,
			wantLarge: false, wantEffective: 80000,
		},
		{
			name: "小活涨成大活_正常分流",
			estimated: 260000, lastReal: 90000, hasLast: true,
			wantLarge: true, wantEffective: 260000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			effective, isLarge := ResolveRequestSizeRouting(tc.estimated, tc.lastReal, tc.hasLast)
			if effective != tc.wantEffective {
				t.Fatalf("effective = %d, want %d", effective, tc.wantEffective)
			}
			if isLarge != tc.wantLarge {
				t.Fatalf("isLarge = %v, want %v", isLarge, tc.wantLarge)
			}
		})
	}
}

// TestRequestSizeSessionHashRoundTrip 验证会话哈希在 ctx 上的存取：
// handler 选号前装入、RecordUsage 取出，是「写回真实值」的唯一通道。
func TestRequestSizeSessionHashRoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := RequestSizeSessionHash(ctx); got != "" {
		t.Fatalf("空 ctx 应返回空哈希，实际 %q", got)
	}
	ctx = WithRequestSizeSessionHash(ctx, "abc123")
	if got := RequestSizeSessionHash(ctx); got != "abc123" {
		t.Fatalf("哈希未正确装入，实际 %q", got)
	}
	// 空哈希不得装入，避免把「没有会话」写成同一个 key。
	if got := RequestSizeSessionHash(WithRequestSizeSessionHash(ctx, "")); got != "abc123" {
		t.Fatalf("空哈希不应覆盖既有值，实际 %q", got)
	}
}

// TestRememberAndLastSessionInputTokens 验证写回-读回闭环，以及未装配缓存时的失败开放。
func TestRememberAndLastSessionInputTokens(t *testing.T) {
	cache := newFileSessionInputCache()
	svc := &OpenAIGatewayService{cache: cache}
	groupID := int64(57)
	ctx := context.Background()

	if _, ok := svc.LastSessionInputTokens(ctx, &groupID, "sess-1"); ok {
		t.Fatal("未写入前不应读到上一单值")
	}

	svc.rememberSessionInputTokens(ctx, &groupID, "sess-1", 300000)
	if cache.sets != 1 {
		t.Fatalf("应写入一次，实际 %d", cache.sets)
	}
	tokens, ok := svc.LastSessionInputTokens(ctx, &groupID, "sess-1")
	if !ok || tokens != 300000 {
		t.Fatalf("读回失败: tokens=%d ok=%v", tokens, ok)
	}

	// 不同分组互不串味（与 sticky_session 同 groupID 隔离语义）。
	other := int64(115)
	if _, ok := svc.LastSessionInputTokens(ctx, &other, "sess-1"); ok {
		t.Fatal("不同分组不应共享上一单值")
	}

	// <=0 视为无效写入。
	svc.rememberSessionInputTokens(ctx, &groupID, "sess-2", 0)
	if _, ok := svc.LastSessionInputTokens(ctx, &groupID, "sess-2"); ok {
		t.Fatal("0 token 不应被记录")
	}

	// 未装配缓存（测试桩/降级实现）→ 失败开放，返回「没有上一单」而不是报错崩掉。
	bare := &OpenAIGatewayService{}
	if _, ok := bare.LastSessionInputTokens(ctx, &groupID, "sess-1"); ok {
		t.Fatal("无缓存实现时应按无数据处理")
	}
	bare.rememberSessionInputTokens(ctx, &groupID, "sess-1", 123) // 不得 panic
}
