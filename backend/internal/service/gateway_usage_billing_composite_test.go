//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// TestQuotaPlatformCompositeAmbiguityPreAndPostSelection 锁定同链歧义 composite
// 的平台配额口径：选号前 decision 的代表平台（openai）不得当作用户平台配额，
// 失败关闭返回空串；handler 选到实际账号平台并盖章 actual-resolved 后按实际平台
// 计量；concrete 决策与 ForcePlatform 路径保持既有语义不变。
func TestQuotaPlatformCompositeAmbiguityPreAndPostSelection(t *testing.T) {
	apiKey := &APIKey{Group: &Group{Platform: PlatformComposite}}

	// 同链歧义：代表平台 openai，真实供给为 zhipu/cursor。
	ambiguous := CompositeRouteDecision{
		Matched:         true,
		TargetPlatform:  PlatformOpenAI,
		Reason:          CompositeRouteReasonAmbiguous,
		SupplyPlatforms: []string{PlatformZhipu, PlatformCursor},
	}

	t.Run("pre-selection ambiguous composite yields empty", func(t *testing.T) {
		// WithCompositeRouteDecision 会把代表平台 openai 写入 ResolvedTargetPlatform；
		// 选号前未盖章 actual-resolved，必须返回空串而非代表平台。
		ctx := WithCompositeRouteDecision(context.Background(), ambiguous)
		if got := QuotaPlatform(ctx, apiKey); got != "" {
			t.Fatalf("pre-selection ambiguous quota platform = %q, want empty", got)
		}
	})

	t.Run("post-selection returns actual platform", func(t *testing.T) {
		ctx := WithCompositeRouteDecision(context.Background(), ambiguous)
		ctx = WithResolvedTargetPlatform(ctx, PlatformZhipu)
		ctx = WithCompositeActualPlatformResolved(ctx)
		if got := QuotaPlatform(ctx, apiKey); got != PlatformZhipu {
			t.Fatalf("post-selection quota platform = %q, want %q", got, PlatformZhipu)
		}
	})

	t.Run("concrete decision stays unchanged", func(t *testing.T) {
		concrete := CompositeRouteDecision{Matched: true, TargetPlatform: PlatformOpenAI}
		ctx := WithCompositeRouteDecision(context.Background(), concrete)
		ctx = WithResolvedTargetPlatform(ctx, PlatformOpenAI)
		if got := QuotaPlatform(ctx, apiKey); got != PlatformOpenAI {
			t.Fatalf("concrete decision quota platform = %q, want %q", got, PlatformOpenAI)
		}
	})

	t.Run("force platform wins over ambiguous decision", func(t *testing.T) {
		ctx := WithCompositeRouteDecision(context.Background(), ambiguous)
		ctx = context.WithValue(ctx, ctxkey.ForcePlatform, PlatformAntigravity)
		if got := QuotaPlatform(ctx, apiKey); got != PlatformAntigravity {
			t.Fatalf("force platform quota = %q, want %q", got, PlatformAntigravity)
		}
	})

	t.Run("actual resolved helpers are nil safe", func(t *testing.T) {
		if got := WithCompositeActualPlatformResolved(nil); got != nil {
			t.Fatalf("WithCompositeActualPlatformResolved(nil) = %v, want nil", got)
		}
		if compositeActualPlatformResolved(nil) {
			t.Fatal("compositeActualPlatformResolved(nil) = true, want false")
		}
	})
}
