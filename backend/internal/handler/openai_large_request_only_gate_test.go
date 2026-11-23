//go:build unit

package handler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// largeRequestOnlyRepoStub 是 applyRequestSizeScopeExclusion 在小请求分支下
// 列举可调度候选所需的极简账号仓库桩（仅实现本路径触达的方法）。
type largeRequestOnlyRepoStub struct {
	service.AccountRepository
	accounts []service.Account
}

func (r largeRequestOnlyRepoStub) ListSchedulableUngroupedByPlatform(_ context.Context, _ string) ([]service.Account, error) {
	return r.accounts, nil
}

func (r largeRequestOnlyRepoStub) ListSchedulableByPlatform(_ context.Context, _ string) ([]service.Account, error) {
	return r.accounts, nil
}

func newLargeRequestOnlyTestAccount(id int64, enabled bool) *service.Account {
	extra := map[string]any{}
	if enabled {
		extra[service.LargeRequestOnlyExtraKey] = true
	}
	return &service.Account{
		ID:       id,
		Name:     fmt.Sprintf("lro-%d", id),
		Platform: service.PlatformCursor,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
		Extra: extra,
	}
}

// TestApplyLargeRequestOnlyExclusionSmallRequest 验证估算 input <= 阈值（testRequestSizeRoutingThreshold，
// 小请求）时：开启「仅接大请求」开关的账号必须进入排除集合，未开启账号保留。
func TestApplyLargeRequestOnlyExclusionSmallRequest(t *testing.T) {
	t.Parallel()

	enabled := newLargeRequestOnlyTestAccount(101, true)
	plain := newLargeRequestOnlyTestAccount(202, false)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*enabled, *plain}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw, settingService: newSettingServiceWithThreshold(testRequestSizeRoutingThreshold)}
	reqLog := zap.NewNop()

	// 不含图短请求：估算远低于 testRequestSizeRoutingThreshold 阈值。
	smallBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	estimated, excluded := h.applyRequestSizeScopeExclusion(
		context.Background(), nil, reqLog, nil, service.PlatformCursor, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, "", smallBody)

	require.Greater(t, estimated, 0, "estimate must succeed for a small request")
	require.LessOrEqual(t, estimated, testRequestSizeRoutingThreshold, "small request estimate must stay below the threshold")
	require.Len(t, excluded, 1, "exactly one large-request-only account must be excluded")
	_, ok := excluded[101]
	require.True(t, ok, "large-request-only account 101 must be excluded for a small request")
	_, ok = excluded[202]
	require.False(t, ok, "non-large-request-only account 202 must not be excluded")
}

// TestApplyLargeRequestOnlyExclusionLargeRequest 验证估算 input > 阈值（大请求）时
// 不排除任何账号：全部账号参与候选，仅返回估算 input。
func TestApplyLargeRequestOnlyExclusionLargeRequest(t *testing.T) {
	t.Parallel()

	enabled := newLargeRequestOnlyTestAccount(101, true)
	plain := newLargeRequestOnlyTestAccount(202, false)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*enabled, *plain}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw, settingService: newSettingServiceWithThreshold(testRequestSizeRoutingThreshold)}
	reqLog := zap.NewNop()

	// 重复短语会被 BPE 高效压缩，放大到与 sensenova 测试同量级确保远超 testRequestSizeRoutingThreshold 阈值。
	huge := strings.Repeat("hello world ", 200000)
	bigBody := []byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, huge))
	estimated, excluded := h.applyRequestSizeScopeExclusion(
		context.Background(), nil, reqLog, nil, service.PlatformCursor, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, "", bigBody)

	require.Greater(t, estimated, testRequestSizeRoutingThreshold, "large request estimate must exceed the threshold")
	require.Empty(t, excluded, "large request must not exclude any large-request-only account")
}

// TestApplyLargeRequestOnlyExclusionEstimateFailed 验证估算失败（非法 body）时必须
// 失败开放：返回估算 0 + 空排除集合，且不 panic（绝不因估算失败拦请求）。
func TestApplyLargeRequestOnlyExclusionEstimateFailed(t *testing.T) {
	t.Parallel()

	enabled := newLargeRequestOnlyTestAccount(101, true)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*enabled}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw, settingService: newSettingServiceWithThreshold(testRequestSizeRoutingThreshold)}
	reqLog := zap.NewNop()

	badBody := []byte("{not valid json")
	estimated, excluded := h.applyRequestSizeScopeExclusion(
		context.Background(), nil, reqLog, nil, service.PlatformCursor, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, "", badBody)

	require.Zero(t, estimated, "estimate failure must report estimated input as 0")
	require.Empty(t, excluded, "estimate failure must fail-open with an empty exclusion set")
}

// TestApplyRequestSizeScopeExclusionLargeRequestSkipsSmallOnly 验证估算 input > 阈值
// （大请求）时：开启「仅接小请求」开关的账号必须进入排除集合，其余账号保留。
func TestApplyRequestSizeScopeExclusionLargeRequestSkipsSmallOnly(t *testing.T) {
	t.Parallel()

	smallOnly := &service.Account{
		ID:       303,
		Name:     "sro-303",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
		Extra: map[string]any{service.SmallRequestOnlyExtraKey: true},
	}
	plain := newLargeRequestOnlyTestAccount(404, false)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*smallOnly, *plain}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw, settingService: newSettingServiceWithThreshold(testRequestSizeRoutingThreshold)}
	reqLog := zap.NewNop()

	// 构造超过 testRequestSizeRoutingThreshold 阈值的估算：重复长文本。
	largeBody := []byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("word ", 500000)))
	estimated, excluded := h.applyRequestSizeScopeExclusion(
		context.Background(), nil, reqLog, nil, service.PlatformCursor, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, "", largeBody)

	require.Greater(t, estimated, testRequestSizeRoutingThreshold, "用例前提：必须被判为大请求")
	require.Len(t, excluded, 1, "大请求只能排除仅接小请求的账号")
	_, ok := excluded[303]
	require.True(t, ok, "small-only 账号 303 必须在大请求时被排除")
	_, ok = excluded[404]
	require.False(t, ok, "普通账号 404 不得被排除")
}

// TestApplyRequestSizeScopeExclusionSmallRequestSkipsLargeOnly 验证小请求时排除的是
// 「仅接大请求」账号，而不是「仅接小请求」账号（两侧互不干扰）。
func TestApplyRequestSizeScopeExclusionSmallRequestSkipsLargeOnly(t *testing.T) {
	t.Parallel()

	largeOnly := newLargeRequestOnlyTestAccount(505, true)
	smallOnly := &service.Account{
		ID:       606,
		Name:     "sro-606",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
		Extra: map[string]any{service.SmallRequestOnlyExtraKey: true},
	}

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*largeOnly, *smallOnly}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw, settingService: newSettingServiceWithThreshold(testRequestSizeRoutingThreshold)}
	reqLog := zap.NewNop()

	smallBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	_, excluded := h.applyRequestSizeScopeExclusion(
		context.Background(), nil, reqLog, nil, service.PlatformCursor, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, "", smallBody)

	require.Len(t, excluded, 1, "小请求只排除仅接大请求的账号")
	_, ok := excluded[505]
	require.True(t, ok, "large-only 账号 505 必须在小请求时被排除")
	_, ok = excluded[606]
	require.False(t, ok, "small-only 账号 606 不得在小请求时被排除")
}

// TestApplyRequestSizeScopeExclusionDirtyDoubleTruePrecedence 验证两个键同时为 true 的
// 脏数据按 RequestSizeScopeOf 归一化为「只接大请求」：大请求不排除、小请求排除，
// 绝不能因为另一侧键也被读取而让账号两侧都选不到。
func TestApplyRequestSizeScopeExclusionDirtyDoubleTruePrecedence(t *testing.T) {
	t.Parallel()

	dirty := &service.Account{
		ID:       707,
		Name:     "dirty-707",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
		Extra: map[string]any{
			service.LargeRequestOnlyExtraKey: true,
			service.SmallRequestOnlyExtraKey: true,
		},
	}
	plain := newLargeRequestOnlyTestAccount(808, false)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*dirty, *plain}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw, settingService: newSettingServiceWithThreshold(testRequestSizeRoutingThreshold)}
	reqLog := zap.NewNop()

	// 小请求：脏数据归一化为 large_only，应被排除。
	smallBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	_, excludedSmall := h.applyRequestSizeScopeExclusion(
		context.Background(), nil, reqLog, nil, service.PlatformCursor, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, "", smallBody)
	_, ok := excludedSmall[707]
	require.True(t, ok, "小请求必须排除归一化为 large_only 的脏数据账号")

	// 大请求：同一账号不得被 small 键连带排除，否则两侧都选不到。
	largeBody := []byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("word ", 500000)))
	_, excludedLarge := h.applyRequestSizeScopeExclusion(
		context.Background(), nil, reqLog, nil, service.PlatformCursor, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, "", largeBody)
	_, ok = excludedLarge[707]
	require.False(t, ok, "大请求不得排除归一化为 large_only 的账号（否则该账号两侧都不可选）")
}

// TestApplyRequestSizeScopeExclusionExemptPlatforms 验证 L5 三平台豁免：
// anthropic/openai/gemini 即使账号标记 large_request_only=true，小请求也不返回
// 任何排除集合（保持原模型质量，不参与大小请求分流）。
// 注入显式阈值，走「阈值可知」的正常分流路径。
func TestApplyRequestSizeScopeExclusionExemptPlatforms(t *testing.T) {
	t.Parallel()

	for _, platform := range []string{
		service.PlatformAnthropic,
		service.PlatformOpenAI,
		service.PlatformGemini,
	} {
		platform := platform
		t.Run(platform, func(t *testing.T) {
			t.Parallel()

			enabled := newLargeRequestOnlyTestAccount(101, true)
			gw := service.NewOpenAIGatewayService(
				largeRequestOnlyRepoStub{accounts: []service.Account{*enabled}},
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			)
			h := &OpenAIGatewayHandler{gatewayService: gw, settingService: newSettingServiceWithThreshold(testRequestSizeRoutingThreshold)}
			reqLog := zap.NewNop()

			// 同一小请求 body（估算远低于 testRequestSizeRoutingThreshold）；非豁免平台下该标记账号本应被排除。
			smallBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
			estimated, excluded := h.applyRequestSizeScopeExclusion(
				context.Background(), nil, reqLog, nil, platform, "gpt-4o",
				service.TierRoutingProtocolChatCompletions, "", smallBody)

			require.Greater(t, estimated, 0, "small request estimate must succeed")
			require.LessOrEqual(t, estimated, testRequestSizeRoutingThreshold,
				"small request estimate must stay below the default threshold")
			require.Empty(t, excluded, "exempt platform %s must return an empty exclusion set", platform)
		})
	}
}

// fixedSessionInputTokenCache 是 effective 对账用的 SessionInputTokenCache 桩：
// 恒返回预置的「上一单真实输入 token」。内嵌 service.GatewayCache 只为满足
// NewOpenAIGatewayService 第 7 个参数的静态类型，本测试只触达下面两个方法。
type fixedSessionInputTokenCache struct {
	service.GatewayCache
	tokens int
}

func (c *fixedSessionInputTokenCache) GetSessionInputTokens(_ context.Context, _ int64, _ string) (int, error) {
	return c.tokens, nil
}

func (c *fixedSessionInputTokenCache) SetSessionInputTokens(_ context.Context, _ int64, _ string, _ int, _ time.Duration) error {
	return nil
}

// TestApplyRequestSizeScopeExclusionExemptPlatformsLargeSmallOnly 验证 L5 三平台豁免
// 对 small_only 账号同样生效：大请求下 anthropic/openai/gemini 一律不排除
// 「仅接小请求」账号；同输入以 cursor（非豁免平台）调用则必须排除该账号（对照组）。
func TestApplyRequestSizeScopeExclusionExemptPlatformsLargeSmallOnly(t *testing.T) {
	t.Parallel()

	smallOnly := &service.Account{
		ID:       909,
		Name:     "sro-l5-909",
		Platform: service.PlatformCursor,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
		Extra: map[string]any{service.SmallRequestOnlyExtraKey: true},
	}
	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*smallOnly}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw, settingService: newSettingServiceWithThreshold(testRequestSizeRoutingThreshold)}
	reqLog := zap.NewNop()

	// 复用现有大请求构造量级：估算远超 testRequestSizeRoutingThreshold 阈值，确保判为大请求。
	largeBody := []byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("word ", 500000)))

	for _, platform := range []string{
		service.PlatformAnthropic,
		service.PlatformOpenAI,
		service.PlatformGemini,
	} {
		estimated, excluded := h.applyRequestSizeScopeExclusion(
			context.Background(), nil, reqLog, nil, platform, "gpt-4o",
			service.TierRoutingProtocolChatCompletions, "", largeBody)

		require.Greater(t, estimated, testRequestSizeRoutingThreshold,
			"用例前提：平台 %s 必须把该请求判为大请求", platform)
		require.Empty(t, excluded, "豁免平台 %s 在大请求下不得排除 small-only 账号", platform)
	}

	// 对照组：同一大请求、同一账号，cursor（非豁免平台）必须排除 small-only 账号。
	_, excludedCursor := h.applyRequestSizeScopeExclusion(
		context.Background(), nil, reqLog, nil, service.PlatformCursor, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, "", largeBody)
	_, ok := excludedCursor[909]
	require.True(t, ok, "cursor 对照组：大请求必须排除 small-only 账号 909")
}

// TestApplyRequestSizeScopeExclusionEffectiveReconciliation 验证 effective 对账：
// 预置会话上一单真实输入 180000 后，即便本地估算仅约 2 万，返回值也必须被校正为
// 180000；平台 openai 属 L5 豁免，排除集必须为空。缓存能力可经
// NewOpenAIGatewayService 第 7 个参数（cache）注入，无需改动生产代码。
func TestApplyRequestSizeScopeExclusionEffectiveReconciliation(t *testing.T) {
	t.Parallel()

	cache := &fixedSessionInputTokenCache{tokens: 180000}
	// cache 是 NewOpenAIGatewayService 第 7 个参数：repo 后 5 个 nil 再传桩。
	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{},
		nil, nil, nil, nil, nil,
		cache,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw, settingService: newSettingServiceWithThreshold(testRequestSizeRoutingThreshold)}
	reqLog := zap.NewNop()

	// 本地估算约 2 万（远低于真实值 180000），确保 effective 必须用真实值校正。
	body := []byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("hello world ", 15000)))
	effective, excluded := h.applyRequestSizeScopeExclusion(
		context.Background(), nil, reqLog, nil, service.PlatformOpenAI, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, "session-hash-effective", body)

	require.Equal(t, 180000, effective, "effective 必须被上一单真实输入 180000 校正")
	require.Empty(t, excluded, "豁免平台 openai 不得返回排除集合")
}
