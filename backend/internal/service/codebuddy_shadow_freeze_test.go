package service

import (
	"context"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// codebuddyShadowFreezeRepoStub 仅实现 CreateShadow 冻结判定所需的最小 repo 接口
// （GetByID）。冻结判定位于任何分组/绑定写操作之前，故其余方法不会被调用。
type codebuddyShadowFreezeRepoStub struct {
	AccountRepository
}

func (s *codebuddyShadowFreezeRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	plat := PlatformCodeBuddy
	if id == 2 {
		// id==2 表示该母账号为 OpenAI（spark 维度），用于验证非 codebuddy 母账号不命中冻结。
		plat = PlatformOpenAI
	}
	return &Account{
		ID:          id,
		Name:        "parent",
		Platform:    plat,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Credentials: map[string]any{"access_token": "tok"},
	}, nil
}

// ---------------------------------------------------------------------------
// §3.1-2 最终写入边界统一拒绝：新建 CodeBuddy 影子
// ---------------------------------------------------------------------------

// 服务层入口：CreateShadow 以 codebuddy 母账号调用，应在领域边界被拒绝。
func TestCodebuddyShadowFreeze_CreateShadowRejected(t *testing.T) {
	svc := &adminServiceImpl{accountRepo: &codebuddyShadowFreezeRepoStub{}}
	_, err := svc.CreateShadow(context.Background(), 1, ShadowOptions{
		Model:    "glm-4.5",
		GroupIDs: []int64{11},
	})
	require.Error(t, err)
	require.Equal(t, "CODEBUDDY_SHADOW_CREATION_FROZEN", infraerrors.Reason(err))
}

// 非 codebuddy 母账号（OpenAI / spark 维度）不命中冻结判定（见
// TestCodebuddyShadowFreeze_SparkShadowUnaffected 的辅助函数断言）。

// 领域边界辅助函数：新建进入 codebuddy 影子组合即拒绝。
func TestCodebuddyShadowFreeze_HelperNewCreation(t *testing.T) {
	cbShadow := &Account{
		Platform:        PlatformZhipu,
		ParentAccountID: ptrInt64(1),
		QuotaDimension:  QuotaDimensionCodeBuddy,
	}
	require.True(t, isCodeBuddyShadowIdentity(cbShadow))
	require.Error(t, enforceCodeBuddyShadowFreeze(nil, cbShadow))

	// platform=codebuddy 的直绑账号（非影子）不在冻结范围。
	directBound := &Account{Platform: PlatformCodeBuddy}
	require.False(t, isCodeBuddyShadowIdentity(directBound))
	require.NoError(t, enforceCodeBuddyShadowFreeze(nil, directBound))
}

// ---------------------------------------------------------------------------
// §3.1-2 最终写入边界统一拒绝：更新转换进入组合（改 platform / 设 IsShadow 两路）
// ---------------------------------------------------------------------------

func TestCodebuddyShadowFreeze_ConversionRejected(t *testing.T) {
	// 路 1（改 platform）：普通账号把 platform 改为 codebuddy 并设为影子 → 进入组合。
	existingNormal := &Account{Platform: PlatformDeepseek}
	convertedByPlatform := &Account{
		Platform:        PlatformCodeBuddy,
		ParentAccountID: ptrInt64(9),
	}
	require.Error(t, enforceCodeBuddyShadowFreeze(existingNormal, convertedByPlatform))

	// 路 2（设 IsShadow）：直绑 codebuddy 账号被设置 IsShadow（ParentAccountID）+ 维度 → 进入组合。
	existingDirectBound := &Account{Platform: PlatformCodeBuddy}
	convertedByShadow := &Account{
		Platform:        PlatformCodeBuddy,
		ParentAccountID: ptrInt64(9),
		QuotaDimension:  QuotaDimensionCodeBuddy,
	}
	require.Error(t, enforceCodeBuddyShadowFreeze(existingDirectBound, convertedByShadow))

	// 反向（普通账号改 platform 但非影子）不进入组合，不应被拒。
	notShadow := &Account{Platform: PlatformCodeBuddy}
	require.NoError(t, enforceCodeBuddyShadowFreeze(existingNormal, notShadow))
}

// ---------------------------------------------------------------------------
// §3.1-2 身份字段不可变 + 非身份字段更新放行（存量 codebuddy 影子）
// ---------------------------------------------------------------------------

func TestCodebuddyShadowFreeze_IdentityImmutableAndNonIdentityAllowed(t *testing.T) {
	existing := &Account{
		Platform:        PlatformZhipu,
		ParentAccountID: ptrInt64(1),
		QuotaDimension:  QuotaDimensionCodeBuddy,
	}

	// 非身份字段更新（name / priority / status）放行。
	nonIdentity := &Account{
		Name:            "renamed",
		Priority:        5,
		Status:          StatusActive,
		Platform:        PlatformZhipu,
		ParentAccountID: ptrInt64(1),
		QuotaDimension:  QuotaDimensionCodeBuddy,
	}
	require.NoError(t, enforceCodeBuddyShadowFreeze(existing, nonIdentity))

	// 身份字段 platform 修改 → 拒绝。
	changedPlatform := &Account{
		Platform:        PlatformCodeBuddy,
		ParentAccountID: ptrInt64(1),
		QuotaDimension:  QuotaDimensionCodeBuddy,
	}
	require.Error(t, enforceCodeBuddyShadowFreeze(existing, changedPlatform))

	// 身份字段 IsShadow（ParentAccountID）取消 → 拒绝。
	unsetShadow := &Account{
		Platform:       PlatformZhipu,
		QuotaDimension: QuotaDimensionCodeBuddy,
	}
	require.Error(t, enforceCodeBuddyShadowFreeze(existing, unsetShadow))

	// 身份字段 QuotaDimension 修改 → 拒绝。
	changedQuota := &Account{
		Platform:        PlatformZhipu,
		ParentAccountID: ptrInt64(1),
		QuotaDimension:  QuotaDimensionSpark,
	}
	require.Error(t, enforceCodeBuddyShadowFreeze(existing, changedQuota))
}

// ---------------------------------------------------------------------------
// §3.1 范围外保留：spark 影子路径不受影响（创建/更新均不被冻结）
// ---------------------------------------------------------------------------

func TestCodebuddyShadowFreeze_SparkShadowUnaffected(t *testing.T) {
	sparkShadow := &Account{
		Platform:        PlatformOpenAI,
		ParentAccountID: ptrInt64(1),
		QuotaDimension:  QuotaDimensionSpark,
	}
	require.False(t, isCodeBuddyShadowIdentity(sparkShadow))
	// 新建 spark 影子不被冻结。
	require.NoError(t, enforceCodeBuddyShadowFreeze(nil, sparkShadow))
	// 存量 spark 影子身份字段变更（不命中 codebuddy 判定）放行。
	sparkChanged := &Account{
		Platform:        PlatformOpenAI,
		ParentAccountID: ptrInt64(1),
		QuotaDimension:  QuotaDimensionSpark,
		Name:            "spark-renamed",
	}
	require.NoError(t, enforceCodeBuddyShadowFreeze(sparkShadow, sparkChanged))
}

// ---------------------------------------------------------------------------
// §2.1-4 桥接通用平台判定：/responses → CC 转换按账号平台，不挂在影子专属函数上。
// 旧 isCodeBuddyShadowAccount 函数本体保留（B3c 才删）。
// ---------------------------------------------------------------------------

func TestCodebuddyShadowFreeze_BridgePlatformJudgment(t *testing.T) {
	// 直绑 codebuddy 账号（platform=codebuddy、非影子）命中通用平台判定。
	directBound := &Account{Platform: PlatformCodeBuddy}
	require.True(t, isCodeBuddyPlatformAccount(directBound))

	// 存量 codebuddy 影子 platform 为目标分组平台，不命中平台判定（仍由旧影子函数处理）。
	cbShadow := &Account{
		Platform:        PlatformDeepseek,
		ParentAccountID: ptrInt64(1),
		QuotaDimension:  QuotaDimensionCodeBuddy,
	}
	require.False(t, isCodeBuddyPlatformAccount(cbShadow))
	require.True(t, isCodeBuddyShadowAccount(cbShadow), "旧影子专属判定函数应保留并可识别 codebuddy 影子")

	// spark 影子 / 普通 OpenAI 账号不命中平台判定。
	sparkShadow := &Account{Platform: PlatformOpenAI, ParentAccountID: ptrInt64(1), QuotaDimension: QuotaDimensionSpark}
	require.False(t, isCodeBuddyPlatformAccount(sparkShadow))
	openaiNormal := &Account{Platform: PlatformOpenAI}
	require.False(t, isCodeBuddyPlatformAccount(openaiNormal))
}
