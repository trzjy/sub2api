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

// 多 group_ids 一次绑定：按模型聚合的创建请求应把全部分组写入同一影子。
func TestCreateShadowCodeBuddy_BindsAllGroups(t *testing.T) {
	ctx := context.Background()
	svc, repo, parent := newCodeBuddyShadowTestService(t)

	shadow, err := svc.CreateShadow(ctx, parent.ID, ShadowOptions{
		Name:     "cb-parent:cn:glm-4.5",
		Model:    "glm-4.5",
		Platform: PlatformZhipu,
		GroupIDs: []int64{11, 22, 33},
	})
	require.NoError(t, err)
	require.Equal(t, []int64{11, 22, 33}, shadow.GroupIDs)
	require.Equal(t, []int64{11, 22, 33}, repo.groupsOf[shadow.ID], "BindGroups 应收到完整分组数组")
	require.Equal(t, QuotaDimensionCodeBuddy, shadow.QuotaDimension)
	require.Equal(t, "glm-4.5", shadow.GetExtraString(ShadowModelExtraKey))
	require.Equal(t, map[string]any{"glm-4.5": "glm-4.5"}, shadow.Credentials["model_mapping"])
}

// 同模型去重：已存在该模型的影子时再次创建应 409；不同模型不受影响。
func TestCreateShadowCodeBuddy_ModelDedup(t *testing.T) {
	ctx := context.Background()
	svc, _, parent := newCodeBuddyShadowTestService(t)

	_, err := svc.CreateShadow(ctx, parent.ID, ShadowOptions{Model: "glm-4.5", GroupIDs: []int64{11}})
	require.NoError(t, err)

	_, err = svc.CreateShadow(ctx, parent.ID, ShadowOptions{Model: "glm-4.5", GroupIDs: []int64{22}})
	require.Error(t, err)
	require.Equal(t, http.StatusConflict, infraerrors.Code(err))

	_, err = svc.CreateShadow(ctx, parent.ID, ShadowOptions{Model: "deepseek-v3", GroupIDs: []int64{22}})
	require.NoError(t, err)
}

// ListAccountShadows 返回摘要：含 shadow_model 与绑定的全部分组 ID。
func TestListAccountShadows_Summary(t *testing.T) {
	ctx := context.Background()
	svc, _, parent := newCodeBuddyShadowTestService(t)

	_, err := svc.CreateShadow(ctx, parent.ID, ShadowOptions{
		Name:     "cb-parent:cn:glm-4.5",
		Model:    "glm-4.5",
		Platform: PlatformZhipu,
		GroupIDs: []int64{11, 22},
	})
	require.NoError(t, err)

	summaries, err := svc.ListAccountShadows(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Equal(t, "glm-4.5", summaries[0].ShadowModel)
	require.Equal(t, []int64{11, 22}, summaries[0].GroupIDs)
	require.Equal(t, PlatformZhipu, summaries[0].Platform)

	_, err = svc.ListAccountShadows(ctx, 99999)
	require.Error(t, err, "母账号不存在应报错")
}
