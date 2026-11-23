package service

import (
	"context"
	"testing"
	"time"
)

// testRequestSizeRoutingThreshold 是测试用的大/小请求分界阈值。
// 生产阈值的唯一事实源是 settings 表（见 SettingKeyRequestSizeRoutingThreshold），
// 代码内不再有默认常量；测试必须自带确定值，断言才不会随线上配置漂移。
const testRequestSizeRoutingThreshold = 128000

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

// TestResolveRequestSizeRouting 钉死分流判定的核心规则：
// 上一单真实值是本轮下界（effective 取两者较大），最终由 effective > threshold 判大活。
// 这是「长活被判成小活」（3 天漏判 463 次）的修复本体。
//
// 注：原先这里还有一条「滞回下沿」规则（硬编码 200000），实测该规则在
// 「上一单真实值 > threshold」的前提下与 effective > threshold 恒等（返回值相同），
// 属于不影响任何结果的死代码，已随阈值常量一并删除。
func TestResolveRequestSizeRouting(t *testing.T) {
	cases := []struct {
		name          string
		estimated     int
		lastReal      int
		hasLast       bool
		wantLarge     bool
		wantEffective int
	}{
		{
			name:      "没有上一单时保持既有行为_纯本地估算_小活",
			estimated: 100000, hasLast: false,
			wantLarge: false, wantEffective: 100000,
		},
		{
			name:      "没有上一单时保持既有行为_纯本地估算_大活",
			estimated: 300000, hasLast: false,
			wantLarge: true, wantEffective: 300000,
		},
		{
			// 这就是生产里的漏判：本地估算 15 万，但同一会话上一单上游报了 29.7 万
			//（984 上那 463 单的平均实际值）。修复前判成小活派给「仅接小请求」的 984；
			// 修复后必须判大。
			name:      "上一单真实值兜底_估算偏低不再漏判",
			estimated: 50000, lastReal: 297080, hasLast: true,
			wantLarge: true, wantEffective: 297080,
		},
		{
			// 真实值本身没到阈值时，仍按小活处理 —— 兜底不能反过来把正常请求推成大活。
			name:      "上一单真实值也不足阈值_保持小活",
			estimated: 50000, lastReal: 100000, hasLast: true,
			wantLarge: false, wantEffective: 100000,
		},
		{
			name:      "上一单真实值兜底_超过阈值判大",
			estimated: 150000, lastReal: 300000, hasLast: true,
			wantLarge: true, wantEffective: 300000,
		},
		{
			// 上一单在大活池（真实值 30 万），本轮估算掉到 21 万：
			// effective 取真实值 30 万 ⇒ 仍判大活，不因估算回落而换号、清缓存。
			name:      "大活池内估算回落_仍留大池",
			estimated: 200000, lastReal: 300000, hasLast: true,
			wantLarge: true, wantEffective: 300000,
		},
		{
			// 真实压缩（客户端把历史压掉）后，真实值本身也回落 ⇒ 回小活池。
			name:      "真实值也回落后才回小池",
			estimated: 80000, lastReal: 80000, hasLast: true,
			wantLarge: false, wantEffective: 80000,
		},
		{
			name:      "小活涨成大活_正常分流",
			estimated: 150000, lastReal: 90000, hasLast: true,
			wantLarge: true, wantEffective: 150000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			effective, isLarge := ResolveRequestSizeRouting(tc.estimated, tc.lastReal, tc.hasLast, testRequestSizeRoutingThreshold)
			if effective != tc.wantEffective {
				t.Fatalf("effective = %d, want %d", effective, tc.wantEffective)
			}
			if isLarge != tc.wantLarge {
				t.Fatalf("isLarge = %v, want %v", isLarge, tc.wantLarge)
			}
		})
	}
}

// TestResolveRequestSizeRouting_ThresholdBoundary 钉死阈值作为入参后的边界语义：
// 严格大于才判大（等于阈值判小）；上一单真实值高于阈值时，
// 由 effective > threshold 判大。
func TestResolveRequestSizeRouting_ThresholdBoundary(t *testing.T) {
	const threshold = 128000
	cases := []struct {
		name          string
		estimated     int
		lastReal      int
		hasLast       bool
		wantLarge     bool
		wantEffective int
	}{
		{
			name:      "估算等于阈值判小",
			estimated: 128000, hasLast: false,
			wantLarge: false, wantEffective: 128000,
		},
		{
			name:      "估算大于阈值判大",
			estimated: 128001, hasLast: false,
			wantLarge: true, wantEffective: 128001,
		},
		{
			// 上一单真实值 15 万：高于阈值，按 effective > threshold 判大。
			name:      "上一单真实值高于阈值_按阈值判大",
			estimated: 150000, lastReal: 150000, hasLast: true,
			wantLarge: true, wantEffective: 150000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			effective, isLarge := ResolveRequestSizeRouting(tc.estimated, tc.lastReal, tc.hasLast, threshold)
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

// TestResolveRequestSizeRouting_UnknownThresholdDoesNotSplit 验证「阈值不可知」
// （threshold=RequestSizeRoutingThresholdUnknown，即配置读不到）时不判定大小。
//
// 这是防止事故的核心防线：阈值读不到时若被当成「小请求」，调用方会去排除
// 「只接大请求」的账号，大活候选池被清空，大请求只能落到只接小请求的账号上，
// 复现 11-12「长活细节丢失」事故。正确行为是「不分流」。
func TestResolveRequestSizeRouting_UnknownThresholdDoesNotSplit(t *testing.T) {
	cases := []struct {
		name        string
		estimated   int
		lastReal    int
		hasLastReal bool
	}{
		{name: "只有本地估算", estimated: 5000},
		{name: "估算很大", estimated: 500000},
		{name: "有上一单真实值", estimated: 1000, lastReal: 300000, hasLastReal: true},
		{name: "上一单真实值很大", estimated: 10, lastReal: 900000, hasLastReal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			effective, isLarge := ResolveRequestSizeRouting(
				tc.estimated, tc.lastReal, tc.hasLastReal, RequestSizeRoutingThresholdUnknown)

			// effective 仍然要如实回报（用于对账落库）。
			wantEffective := tc.estimated
			if tc.hasLastReal && tc.lastReal > wantEffective {
				wantEffective = tc.lastReal
			}
			if effective != wantEffective {
				t.Fatalf("effective=%d, want %d", effective, wantEffective)
			}
			// 阈值不可知时绝不判为大请求（否则会误排除只接小请求的账号）。
			if isLarge {
				t.Fatal("阈值不可知时不得判为大请求")
			}
		})
	}
}
