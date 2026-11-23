//go:build unit

package handler

import (
	"context"
	"strconv"
	"testing"

	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// testRequestSizeRoutingThreshold 是测试用的大/小请求分界阈值。
// 显式写死在这里而不是引用任何生产默认值：生产阈值的唯一事实源是 settings 表，
// 测试必须自带一个确定值，否则断言会随线上配置漂移。
const testRequestSizeRoutingThreshold = 128000

// requestSizeThresholdSettingRepo 是 SettingRepository 的只读桩：
// 只提供 request_size_routing_threshold 一个键，供分流阈值注入用。
//
// 背景：阈值唯一事实源是 settings 表，读不到即「阈值不可知 → 不做规模分流」。
// 因此凡是断言「大/小请求排除行为」的用例，都必须显式注入一个阈值，
// 不能依赖任何代码内默认数字。
type requestSizeThresholdSettingRepo struct {
	service.SettingRepository
	threshold int
}

func (r *requestSizeThresholdSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if key == service.SettingKeyRequestSizeRoutingThreshold {
		return strconv.Itoa(r.threshold), nil
	}
	return "", nil
}

// newSettingServiceWithThreshold 构造一个只含指定阈值的 SettingService。
func newSettingServiceWithThreshold(threshold int) *service.SettingService {
	return service.NewSettingService(&requestSizeThresholdSettingRepo{threshold: threshold}, &config.Config{})
}

// TestApplyRequestSizeScopeExclusion_UnknownThresholdExcludesNothing 验证
// 「阈值不可知」时本轮不做规模分流：两类账号一个都不排除。
//
// 这是事故防线：阈值读不到时若被当成「小请求」，只接大请求的账号会被全部踢出
// 候选池，大请求只能落到只接小请求的账号上（11-12「长活细节丢失」事故形态）。
// 正确行为是「不缩小候选」。
func TestApplyRequestSizeScopeExclusion_UnknownThresholdExcludesNothing(t *testing.T) {
	t.Parallel()

	enabled := newLargeRequestOnlyTestAccount(101, true)
	plain := newLargeRequestOnlyTestAccount(202, false)

	gw := service.NewOpenAIGatewayService(
		largeRequestOnlyRepoStub{accounts: []service.Account{*enabled, *plain}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	// settingService 为 nil ⇒ 阈值不可知。
	h := &OpenAIGatewayHandler{gatewayService: gw}
	reqLog := zap.NewNop()

	smallBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	estimated, excluded := h.applyRequestSizeScopeExclusion(
		context.Background(), nil, reqLog, nil, service.PlatformCursor, "gpt-4o",
		service.TierRoutingProtocolChatCompletions, "", smallBody)

	require.Greater(t, estimated, 0, "estimate must still succeed")
	require.Empty(t, excluded, "阈值不可知时不得排除任何账号（不缩小候选池）")
}
