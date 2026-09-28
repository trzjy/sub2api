//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// codeBuddyShadowListRepoStub 在 sparkShadowRepoStub 基础上覆盖 ListShadowsByParent，
// 使其同时返回 spark 与 codebuddy 维度的影子并带上 GroupIDs（与真实 repo 一致）。
type codeBuddyShadowListRepoStub struct {
	sparkShadowRepoStub
}

func (s *codeBuddyShadowListRepoStub) ListShadowsByParent(_ context.Context, parentID int64) ([]*Account, error) {
	var result []*Account
	for _, acc := range s.accounts {
		if acc.ParentAccountID == nil || *acc.ParentAccountID != parentID {
			continue
		}
		if acc.QuotaDimension != QuotaDimensionSpark && acc.QuotaDimension != QuotaDimensionCodeBuddy {
			continue
		}
		cp := *acc
		cp.GroupIDs = append([]int64(nil), s.groupsOf[acc.ID]...)
		result = append(result, &cp)
	}
	return result, nil
}

func newCodeBuddyShadowTestService(t *testing.T) (*adminServiceImpl, *codeBuddyShadowListRepoStub, *Account) {
	t.Helper()
	repo := &codeBuddyShadowListRepoStub{sparkShadowRepoStub: *newSparkShadowRepoStub()}
	svc := &adminServiceImpl{accountRepo: repo}
	parent := &Account{
		Name:        "cb-parent",
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Credentials: map[string]any{"access_token": "token"},
	}
	require.NoError(t, repo.Create(context.Background(), parent))
	return svc, repo, parent
}

// CreateShadow 对 codebuddy 母账号一律在领域边界被冻结拒绝（方案 v15 §3.1-2，
// CODEBUDDY_SHADOW_CREATION_FROZEN / HTTP 403）。原"多 group_ids 一次绑定"用例
// 断言创建成功，已随冻结失效；此处改写为冻结拒绝用例，并钉死拒绝发生在任何
// 写操作之前：repo 中不得出现影子账号，groupsOf 不得收到任何绑定调用。
func TestCreateShadowCodeBuddy_FrozenRejectsBeforeBinding(t *testing.T) {
	ctx := context.Background()
	svc, repo, parent := newCodeBuddyShadowTestService(t)

	shadow, err := svc.CreateShadow(ctx, parent.ID, ShadowOptions{
		Name:     "cb-parent:cn:glm-4.5",
		Model:    "glm-4.5",
		Platform: PlatformZhipu,
		GroupIDs: []int64{11, 22, 33},
	})
	require.Error(t, err)
	require.Nil(t, shadow, "冻结拒绝不得返回影子账号")
	require.Equal(t, "CODEBUDDY_SHADOW_CREATION_FROZEN", infraerrors.Reason(err))
	require.Equal(t, http.StatusForbidden, infraerrors.Code(err))

	// 拒绝点早于任何写入：repo 内只有母账号，无任何影子。
	require.Len(t, repo.accounts, 1, "冻结后不得产生影子账号")
	for id, acc := range repo.accounts {
		require.Equal(t, parent.ID, id)
		require.Nil(t, acc.ParentAccountID, "唯一存在的账号应是母账号本身")
	}
	require.Empty(t, repo.groupsOf, "冻结发生在分组绑定之前，BindGroups 不应被调用")
}

// 同模型去重路径（CODEBUDDY_SHADOW_MODEL_EXISTS / 409）在冻结后已不可达：任何
// 创建尝试（首次、同模型重复、不同模型）都在唯一性校验之前被冻结拒绝，因此不再
// 保留 409 断言。
func TestCreateShadowCodeBuddy_ModelDedupUnreachableAfterFreeze(t *testing.T) {
	ctx := context.Background()
	svc, _, parent := newCodeBuddyShadowTestService(t)

	attempts := []ShadowOptions{
		{Model: "glm-4.5", GroupIDs: []int64{11}},              // 首次创建
		{Model: "glm-4.5", GroupIDs: []int64{22}},              // 同模型重复
		{Model: "deepseek-v3", Platform: PlatformDeepseek, GroupIDs: []int64{22}}, // 不同模型
	}
	for i, opts := range attempts {
		shadow, err := svc.CreateShadow(ctx, parent.ID, opts)
		require.Nil(t, shadow, "第 %d 次创建尝试不得返回影子账号", i+1)
		require.Error(t, err, "第 %d 次创建尝试应被冻结拒绝", i+1)
		require.Equal(t, "CODEBUDDY_SHADOW_CREATION_FROZEN", infraerrors.Reason(err), "第 %d 次创建尝试", i+1)
	}
}

// ListAccountShadows 读取路径不在冻结范围（只封新建/转换），必须保留覆盖。
// 造数不得经 svc.CreateShadow（该入口已冻结），改为直接经 stub repo 注入存量
// codebuddy 影子：QuotaDimension=codebuddy、ParentAccountID 指向母账号、
// Credentials["model_mapping"]、Extra[ShadowModelExtraKey]；GroupIDs 以
// BindGroups 语义直接填 groupsOf。
func TestListAccountShadows_Summary(t *testing.T) {
	ctx := context.Background()
	svc, repo, parent := newCodeBuddyShadowTestService(t)

	shadow := &Account{
		Name:            "cb-parent:cn:glm-4.5",
		Platform:        PlatformZhipu,
		Type:            AccountTypeAPIKey,
		Status:          StatusActive,
		ParentAccountID: &parent.ID,
		QuotaDimension:  QuotaDimensionCodeBuddy,
		Credentials:     map[string]any{"model_mapping": map[string]any{"glm-4.5": "glm-4.5"}},
		Extra:           map[string]any{ShadowModelExtraKey: "glm-4.5"},
	}
	require.NoError(t, repo.Create(ctx, shadow))
	repo.groupsOf[shadow.ID] = []int64{11, 22}

	summaries, err := svc.ListAccountShadows(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Equal(t, "glm-4.5", summaries[0].ShadowModel)
	require.Equal(t, []int64{11, 22}, summaries[0].GroupIDs)
	require.Equal(t, PlatformZhipu, summaries[0].Platform)
	require.Equal(t, shadow.ID, summaries[0].ID)
	require.Equal(t, "cb-parent:cn:glm-4.5", summaries[0].Name)

	_, err = svc.ListAccountShadows(ctx, 99999)
	require.Error(t, err, "母账号不存在应报错")
}
