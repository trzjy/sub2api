package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/stretchr/testify/require"
)

// fakeCapabilityRepo 是 AccountModelCapabilityRepository 的最小实现，
// ListByAccount 按预设返回（或返回错误），用于模拟账号级视觉能力探测结果。
type fakeCapabilityRepo struct {
	caps []*model.AccountModelCapability
	err  error
}

func (f *fakeCapabilityRepo) Get(ctx context.Context, accountID int64, upstreamModel, protocol string) (*model.AccountModelCapability, error) {
	return nil, nil
}

func (f *fakeCapabilityRepo) Upsert(ctx context.Context, cap *model.AccountModelCapability) (*model.AccountModelCapability, error) {
	return cap, nil
}

func (f *fakeCapabilityRepo) ListByAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.caps, nil
}

func (f *fakeCapabilityRepo) DeleteByAccount(ctx context.Context, accountID int64) error {
	return nil
}

// TestOpenAIGatewayService_VisionCapabilityWiring 是派发单 Vision-S1 的接线测试：
// 经 ProvideOpenAIGatewayService 构造服务时，必须把账号级视觉能力查询绑定到
// AccountModelCapabilityService.ModelSupportsVisionInput，使 docs/capability-routing-plan.md
// §3.4 的 vision_not_supported 排除与读错失败关闭在生产生效（之前恒为 unknown 放行的假实现）。
func TestOpenAIGatewayService_VisionCapabilityWiring(t *testing.T) {
	const accountID int64 = 77001
	account := &Account{ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
	reqBase := OpenAIAccountScheduleRequest{
		RequestedModel:     "gpt-5.1",
		RequiredTransport:  OpenAIUpstreamTransportAny,
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
		RequireVision:      true,
	}
	ctx := context.Background()

	// 经 provider 构造（其余依赖传 nil，仅注入 capability 以验证绑定接线）。
	provideWithCapability := func(repo *fakeCapabilityRepo) *OpenAIGatewayService {
		capability := NewAccountModelCapabilityService(repo, nil)
		svc := ProvideOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, nil,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, capability)
		t.Cleanup(func() { SetOpenAIAccountVisionCapabilityLookup(nil) }) // 复位为默认 unknown 放行
		return svc
	}

	t.Run("provider binds real capability: known-unsupported excluded (vision_not_supported)", func(t *testing.T) {
		repo := &fakeCapabilityRepo{caps: []*model.AccountModelCapability{{
			AccountID:      accountID,
			UpstreamModel:  "gpt-5.1",
			Protocol:       model.CapabilityProtocolChatCompletions,
			SupportsVision: false,
			Source:         model.CapabilitySourceDetect,
		}}}
		svc := provideWithCapability(repo)
		scheduler := &defaultOpenAIAccountScheduler{service: svc}

		compatible, reason := scheduler.isAccountRequestCompatibleReason(ctx, account, reqBase)
		require.False(t, compatible, "known=false 不支持视觉的账号须被排除")
		require.Equal(t, "vision_not_supported", reason)
	})

	t.Run("provider binds real capability: read error fails closed (vision_capability_unavailable)", func(t *testing.T) {
		repo := &fakeCapabilityRepo{err: errors.New("capability source unavailable")}
		svc := provideWithCapability(repo)
		scheduler := &defaultOpenAIAccountScheduler{service: svc}

		compatible, reason := scheduler.isAccountRequestCompatibleReason(ctx, account, reqBase)
		require.False(t, compatible, "能力源读取失败须失败关闭，不得静默降级为 unknown 放行")
		require.Equal(t, "vision_capability_unavailable", reason)
	})
}
