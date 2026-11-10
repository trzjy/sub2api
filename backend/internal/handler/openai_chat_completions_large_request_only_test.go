//go:build unit

package handler

// 派发单 LRO-06：Chat Completions 入口挂「只接大请求」门 + 对账。
//
// 组合论证说明：完整 ChatCompletions 调用链（鉴权 / body 解析 / 产能槽 / 计费预检）不在
// 单测范围（与 openai_chat_completions_install_guard_test.go 同口径）。下方测试以与本入口
// 挂门点**完全相同**的调用形态（applyLargeRequestOnlyExclusion + 并入 failedAccountIDs）与
// 对账取值口径（RoutingEstimatedInputTokens / LargeRequestReroute）装配，断言门行为与
// 传给 OpenAIRecordUsageInput 的两个对账字段值。
//
// 复用 openai_large_request_only_gate_test.go（LRO-05，同包 //go:build unit）的
// largeRequestOnlyRepoStub / newLargeRequestOnlyTestAccount：门方法由 LRO-05 落地，
// 此处只验证 Chat Completions 入口侧的接线口径与协议参数。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// applyChatCompletionsLargeRequestOnlyForTest 复刻 openai_chat_completions.go 挂门点的
// 生产形态：调用一次门方法，并把返回的排除集合原样并入 failedAccountIDs。返回值即入口
// 后续透传给 RecordUsage 的对账两字段取值来源。
func applyChatCompletionsLargeRequestOnlyForTest(
	h *OpenAIGatewayHandler,
	groupID *int64,
	platform, model string,
	body []byte,
	failedAccountIDs map[int64]struct{},
) (int, bool, map[int64]struct{}) {
	routingEstimatedInputTokens, largeRequestOnlyAdds := h.applyLargeRequestOnlyExclusion(
		context.Background(), zap.NewNop(), groupID, platform, model,
		service.TierRoutingProtocolChatCompletions, body)
	for id := range largeRequestOnlyAdds {
		failedAccountIDs[id] = struct{}{}
	}
	return routingEstimatedInputTokens, len(largeRequestOnlyAdds) > 0, largeRequestOnlyAdds
}

// TestChatCompletionsLargeRequestOnlySmallRequestExcludesEnabledAccount 验证小请求
// （估算 input <= 阈值）时：开启「仅接大请求」开关的账号被并入 failedAccountIDs（即本轮
// 不参与选号），未开启账号保留；对账字段估算值 > 0 且 LargeRequestReroute 为 true。
func TestChatCompletionsLargeRequestOnlySmallRequestExcludesEnabledAccount(t *testing.T) {
	t.Parallel()

	enabled := newLargeRequestOnlyTestAccount(101, true)
	plain := newLargeRequestOnlyTestAccount(202, false)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*enabled, *plain}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw}

	smallBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	failedAccountIDs := make(map[int64]struct{})
	estimated, reroute, adds := applyChatCompletionsLargeRequestOnlyForTest(
		h, nil, service.PlatformOpenAI, "gpt-4o", smallBody, failedAccountIDs)

	require.Len(t, adds, 1, "小请求只应排除唯一一个开启「仅接大请求」的账号")
	_, enabledExcluded := failedAccountIDs[101]
	require.True(t, enabledExcluded, "开启「仅接大请求」的账号 101 在小请求上必须被排除")
	_, plainExcluded := failedAccountIDs[202]
	require.False(t, plainExcluded, "未开启开关的账号 202 不得被排除")

	// 对账口径：入口把 gate 返回值原样填进 RoutingEstimatedInputTokens / LargeRequestReroute。
	require.Greater(t, estimated, 0, "小请求估算必须成功，供 RoutingEstimatedInputTokens 对账")
	require.LessOrEqual(t, estimated, service.LargeRequestOnlyInputThreshold)
	require.True(t, reroute, "发生排除时 LargeRequestReroute 必须为 true")
}

// TestChatCompletionsLargeRequestOnlyLargeRequestKeepsAll 验证大请求（估算 input > 阈值）
// 时：不排除任何账号，failedAccountIDs 保持为空；对账估算值 > 阈值且 LargeRequestReroute
// 为 false。
func TestChatCompletionsLargeRequestOnlyLargeRequestKeepsAll(t *testing.T) {
	t.Parallel()

	enabled := newLargeRequestOnlyTestAccount(101, true)
	plain := newLargeRequestOnlyTestAccount(202, false)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*enabled, *plain}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw}

	huge := strings.Repeat("hello world ", 200000)
	bigBody := []byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, huge))
	failedAccountIDs := make(map[int64]struct{})
	estimated, reroute, adds := applyChatCompletionsLargeRequestOnlyForTest(
		h, nil, service.PlatformOpenAI, "gpt-4o", bigBody, failedAccountIDs)

	require.Empty(t, adds, "大请求不得排除任何「仅接大请求」账号")
	require.Empty(t, failedAccountIDs, "大请求下 failedAccountIDs 必须保持为空")
	require.Greater(t, estimated, service.LargeRequestOnlyInputThreshold, "大请求估算须超过阈值")
	require.False(t, reroute, "未发生排除时 LargeRequestReroute 必须为 false")
}

// TestChatCompletionsLargeRequestOnlyEstimateFailureFailOpen 验证估算失败（非法 body）时
// 失败开放：不排除任何账号、不 panic；对账估算值为 0 且 LargeRequestReroute 为 false。
func TestChatCompletionsLargeRequestOnlyEstimateFailureFailOpen(t *testing.T) {
	t.Parallel()

	enabled := newLargeRequestOnlyTestAccount(101, true)
	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*enabled}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := &OpenAIGatewayHandler{gatewayService: gw}

	var estimated int
	var reroute bool
	var adds map[int64]struct{}
	failedAccountIDs := make(map[int64]struct{})
	require.NotPanics(t, func() {
		estimated, reroute, adds = applyChatCompletionsLargeRequestOnlyForTest(
			h, nil, service.PlatformOpenAI, "gpt-4o", []byte("{not valid json"), failedAccountIDs)
	}, "估算失败不得 panic")

	require.Zero(t, estimated, "估算失败时对账估算值必须为 0")
	require.Empty(t, adds, "估算失败必须失败开放：不排除任何账号")
	require.Empty(t, failedAccountIDs, "估算失败时 failedAccountIDs 必须保持为空")
	require.False(t, reroute, "估算失败时 LargeRequestReroute 必须为 false")
}
