package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDefaultCodeBuddyShadowModelMapping 覆盖向导创建影子时自动写入的默认
// model_mapping（2026-09-22 用户拍板）：官方 ID 清单一律取自 DefaultWebModelIDs
// SSOT，附 shadow_model identity 键；无清单平台/空模型保持 nil（不臆造）。
func TestDefaultCodeBuddyShadowModelMapping(t *testing.T) {
	t.Run("deepseek shadow writes official ids plus identity", func(t *testing.T) {
		m := defaultCodeBuddyShadowModelMapping(PlatformDeepseek, "deepseek-v4.1-flash")
		require.Equal(t, map[string]any{
			"deepseek-chat":       "deepseek-v4.1-flash",
			"deepseek-reasoner":   "deepseek-v4.1-flash",
			"deepseek-v4.1-flash": "deepseek-v4.1-flash",
		}, m)
	})

	t.Run("zhipu shadow writes single official id plus identity", func(t *testing.T) {
		m := defaultCodeBuddyShadowModelMapping(PlatformZhipu, "glm-5.3-pro")
		require.Equal(t, map[string]any{
			"glm-5.3-flash": "glm-5.3-pro",
			"glm-5.3-pro":   "glm-5.3-pro",
		}, m)
	})

	t.Run("kimi shadow writes official ids plus identity", func(t *testing.T) {
		// 官方 ID 清单随 DefaultWebModelIDs SSOT 走：2026-09-22 补全 kimi 目录
		// （免费档 k2d6 / k2d6-chat 可用）后，影子默认 mapping 同步覆盖全部目录项。
		m := defaultCodeBuddyShadowModelMapping(PlatformKimi, "kimi-k3-turbo")
		require.Equal(t, map[string]any{
			"kimi-k2d6-chat":      "kimi-k3-turbo",
			"kimi-k2d6":           "kimi-k3-turbo",
			"kimi-k3":             "kimi-k3-turbo",
			"kimi-k3-agent-ultra": "kimi-k3-turbo",
			"kimi-k3-turbo":       "kimi-k3-turbo",
		}, m)
	})

	t.Run("platform without official catalog stays nil", func(t *testing.T) {
		require.Nil(t, defaultCodeBuddyShadowModelMapping(PlatformMiniMax, "minimax-m2"),
			"minimax 无代码内官方清单，必须保持空 mapping=透传")
		require.Nil(t, defaultCodeBuddyShadowModelMapping(PlatformOther, "qwen-coder"))
	})

	t.Run("empty or blank shadow model stays nil", func(t *testing.T) {
		require.Nil(t, defaultCodeBuddyShadowModelMapping(PlatformDeepseek, ""))
		require.Nil(t, defaultCodeBuddyShadowModelMapping(PlatformDeepseek, "   "))
	})

	t.Run("shadow model already an official id collapses to identity entries", func(t *testing.T) {
		m := defaultCodeBuddyShadowModelMapping(PlatformDeepseek, "deepseek-chat")
		require.Equal(t, map[string]any{
			"deepseek-chat":     "deepseek-chat",
			"deepseek-reasoner": "deepseek-chat",
		}, m)
	})
}

// TestCreateShadowCodeBuddyAutoModelMapping 见 admin_service_spark_shadow_test.go
// （依赖 unit 标签下的 sparkShadowRepoStub 测试设施）。

// TestInferCodeBuddyShadowPlatform 覆盖方案 §2.2 一母多影目标平台推断。
func TestInferCodeBuddyShadowPlatform(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"DeepSeek-V3-0324", PlatformDeepseek},
		{"deepseek-v3.2", PlatformDeepseek},
		{"GLM-4.5", PlatformZhipu},
		{"glm-4.6", PlatformZhipu},
		{"kimi-k2", PlatformKimi},
		{"Kimi-Large", PlatformKimi},
		{"MiniMax-01", PlatformMiniMax},
		{"minimax-m2", PlatformMiniMax},
		{"qwen-coder", PlatformOther},
		{"", PlatformOther},
		{"   ", PlatformOther},
	}
	for _, c := range cases {
		require.Equal(t, c.want, inferCodeBuddyShadowPlatform(c.model),
			"inferCodeBuddyShadowPlatform(%q)", c.model)
	}
}

// TestShadowTargetsModel 覆盖一母多影去重判定的空值守卫与大小写无关匹配。
func TestShadowTargetsModel(t *testing.T) {
	t.Run("nil shadow", func(t *testing.T) {
		require.False(t, shadowTargetsModel(nil, "deepseek-v3"))
	})
	t.Run("empty model", func(t *testing.T) {
		sh := &Account{Extra: map[string]any{ShadowModelExtraKey: "deepseek-v3"}}
		require.False(t, shadowTargetsModel(sh, ""))
	})
	t.Run("match case-insensitive", func(t *testing.T) {
		sh := &Account{Extra: map[string]any{ShadowModelExtraKey: "DeepSeek-V3"}}
		require.True(t, shadowTargetsModel(sh, "deepseek-v3"))
	})
	t.Run("mismatch", func(t *testing.T) {
		sh := &Account{Extra: map[string]any{ShadowModelExtraKey: "deepseek-v3"}}
		require.False(t, shadowTargetsModel(sh, "glm-4.5"))
	})
}

// TestParentHealthyForShadow 覆盖影子分发前母账号可用性校验（F1 决策 A + 外审 D）：
// 母账号须仍是 OpenAI/CodeBuddy OAuth 且凭据可用；否则影子不可调度。
func TestParentHealthyForShadow(t *testing.T) {
	parent := func(acc *Account) func(int64) *Account {
		return func(id int64) *Account { return acc }
	}
	newParent := func(platform, typ string) *Account {
		return &Account{ID: 10, Platform: platform, Type: typ, Status: StatusActive}
	}

	t.Run("codebuddy oauth usable parent", func(t *testing.T) {
		shadow := &Account{ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionCodeBuddy}
		require.True(t, parentHealthyForShadow(shadow, parent(newParent(PlatformCodeBuddy, AccountTypeOAuth))))
	})
	t.Run("openai oauth usable parent (spark shadow)", func(t *testing.T) {
		shadow := &Account{ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionSpark}
		require.True(t, parentHealthyForShadow(shadow, parent(newParent(PlatformOpenAI, AccountTypeOAuth))))
	})
	t.Run("codebuddy apikey parent usable (2026-09-15 APIKey shadow)", func(t *testing.T) {
		shadow := &Account{ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionCodeBuddy}
		require.True(t, parentHealthyForShadow(shadow, parent(newParent(PlatformCodeBuddy, AccountTypeAPIKey))))
	})
	t.Run("non-codebuddy/oauth parent rejected", func(t *testing.T) {
		shadow := &Account{ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionCodeBuddy}
		require.False(t, parentHealthyForShadow(shadow, parent(newParent(PlatformCodeBuddy, AccountTypeSetupToken))))
	})
	t.Run("parent in cooldown rejected", func(t *testing.T) {
		shadow := &Account{ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionCodeBuddy}
		p := newParent(PlatformCodeBuddy, AccountTypeOAuth)
		p.TempUnschedulableUntil = ptrTime(time.Now().Add(time.Hour))
		require.False(t, parentHealthyForShadow(shadow, parent(p)))
	})
	t.Run("inactive parent rejected", func(t *testing.T) {
		shadow := &Account{ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionCodeBuddy}
		p := newParent(PlatformCodeBuddy, AccountTypeOAuth)
		p.Status = "disabled"
		require.False(t, parentHealthyForShadow(shadow, parent(p)))
	})
	t.Run("parent not found rejected", func(t *testing.T) {
		shadow := &Account{ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionCodeBuddy}
		require.False(t, parentHealthyForShadow(shadow, func(int64) *Account { return nil }))
	})
	t.Run("non-shadow passes through", func(t *testing.T) {
		require.True(t, parentHealthyForShadow(&Account{ID: 1}, parent(newParent(PlatformCodeBuddy, AccountTypeOAuth))))
	})
}

// TestCodeBuddyRPMGated 覆盖 B1 限流分类（方案 G3）：codebuddy 母账号与 codebuddy 影子
// 纳入限流；普通 deepseek/openai 账号不纳入；影子须带 quota_dimension=codebuddy 才算。
// 这是「测试路径防护」的核心：codebuddy 影子（platform=deepseek）必须被识别为 codebuddy
// 限流对象，而非被当成普通 deepseek 账号漏过 B1 聚合。
func TestCodeBuddyRPMGated(t *testing.T) {
	t.Run("codebuddy oauth parent", func(t *testing.T) {
		require.True(t, codeBuddyRPMGated(&Account{Platform: PlatformCodeBuddy, Type: AccountTypeOAuth}))
	})
	t.Run("codebuddy shadow", func(t *testing.T) {
		require.True(t, codeBuddyRPMGated(&Account{
			Platform: PlatformDeepseek, Type: AccountTypeOAuth,
			ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionCodeBuddy,
		}))
	})
	t.Run("deepseek apikey not gated", func(t *testing.T) {
		require.False(t, codeBuddyRPMGated(&Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey}))
	})
	t.Run("deepseek oauth without shadow not gated", func(t *testing.T) {
		require.False(t, codeBuddyRPMGated(&Account{Platform: PlatformDeepseek, Type: AccountTypeOAuth}))
	})
	t.Run("shadow with wrong quota dimension not gated", func(t *testing.T) {
		require.False(t, codeBuddyRPMGated(&Account{
			Platform: PlatformDeepseek, Type: AccountTypeOAuth,
			ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionSpark,
		}))
	})
	t.Run("nil not gated", func(t *testing.T) {
		require.False(t, codeBuddyRPMGated(nil))
	})
}

// TestCodeBuddyRPMKeyAccountID 覆盖方案 G3 按母聚合：codebuddy 影子用母账号 ID 作为
// RPM 计数桶，防止一母多影被当成 N 个独立账号导致 N 倍超售上游真实限额。
func TestCodeBuddyRPMKeyAccountID(t *testing.T) {
	t.Run("codebuddy parent uses self", func(t *testing.T) {
		a := &Account{ID: 5, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth}
		require.Equal(t, int64(5), codeBuddyRPMKeyAccountID(a))
	})
	t.Run("codebuddy shadow aggregates to parent", func(t *testing.T) {
		a := &Account{ID: 7, Platform: PlatformDeepseek, Type: AccountTypeOAuth,
			ParentAccountID: ptrI64(5), QuotaDimension: QuotaDimensionCodeBuddy}
		require.Equal(t, int64(5), codeBuddyRPMKeyAccountID(a))
	})
	t.Run("deepseek account uses self", func(t *testing.T) {
		a := &Account{ID: 9, Platform: PlatformDeepseek, Type: AccountTypeOAuth}
		require.Equal(t, int64(9), codeBuddyRPMKeyAccountID(a))
	})
	t.Run("nil returns 0", func(t *testing.T) {
		require.Equal(t, int64(0), codeBuddyRPMKeyAccountID(nil))
	})
}

func ptrI64(v int64) *int64 { return &v }
