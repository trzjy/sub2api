package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

// adminGroupHydrationGroupRepo 是 AdminService 分组读取链的自包含替身（无 build tag，
// 保证无 tags 的 `go test ./internal/service/ -run VisionHydration` 能编译运行）。
type adminGroupHydrationGroupRepo struct {
	byID           map[int64]*Group
	active         []Group
	activePlatform []Group
	filtered       []Group
}

func (r *adminGroupHydrationGroupRepo) GetByID(_ context.Context, id int64) (*Group, error) {
	g, ok := r.byID[id]
	if !ok {
		return nil, ErrGroupNotFound
	}
	return g, nil
}

func (r *adminGroupHydrationGroupRepo) ListActive(_ context.Context) ([]Group, error) {
	return r.active, nil
}

func (r *adminGroupHydrationGroupRepo) ListActiveByPlatform(_ context.Context, _ string) ([]Group, error) {
	return r.activePlatform, nil
}

func (r *adminGroupHydrationGroupRepo) ListWithFilters(_ context.Context, _ pagination.PaginationParams, _, _, _ string, _ *bool) ([]Group, *pagination.PaginationResult, error) {
	return r.filtered, &pagination.PaginationResult{Total: int64(len(r.filtered))}, nil
}

// 以下方法不在 admin 读取链水合测试范围内，均不应被调用；panic 兜底防止误用。
func (r *adminGroupHydrationGroupRepo) Create(context.Context, *Group) error {
	panic("unexpected Create call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) GetByIDLite(context.Context, int64) (*Group, error) {
	panic("unexpected GetByIDLite call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) Update(context.Context, *Group) error {
	panic("unexpected Update call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) Delete(context.Context, int64) error {
	panic("unexpected Delete call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) DeleteCascade(context.Context, int64) ([]int64, error) {
	panic("unexpected DeleteCascade call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) List(context.Context, pagination.PaginationParams) ([]Group, *pagination.PaginationResult, error) {
	panic("unexpected List call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) ExistsByName(context.Context, string) (bool, error) {
	panic("unexpected ExistsByName call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) GetAccountCount(context.Context, int64) (int64, int64, error) {
	panic("unexpected GetAccountCount call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) DeleteAccountGroupsByGroupID(context.Context, int64) (int64, error) {
	panic("unexpected DeleteAccountGroupsByGroupID call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) GetAccountIDsByGroupIDs(context.Context, []int64) ([]int64, error) {
	panic("unexpected GetAccountIDsByGroupIDs call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) BindAccountsToGroup(context.Context, int64, []int64) error {
	panic("unexpected BindAccountsToGroup call in hydration test")
}
func (r *adminGroupHydrationGroupRepo) UpdateSortOrders(context.Context, []GroupSortOrderUpdate) error {
	panic("unexpected UpdateSortOrders call in hydration test")
}

// visionRoutingHydrationRepo 记录单组 Get / 批量 GetByGroupIDs 调用次数与入参，
// 可注入读错误，用于证明批量路径单次查询（且不逐组 Get）、读错误失败关闭。
type visionRoutingHydrationRepo struct {
	mu         sync.Mutex
	data       map[int64]map[string][]int64
	getCalls   int
	batchCalls int
	batchIDs   []int64
	batchFn    func(ctx context.Context, groupIDs []int64) (map[int64]map[string][]int64, error)
}

func (r *visionRoutingHydrationRepo) Get(_ context.Context, groupID int64) (map[string][]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getCalls++
	return r.data[groupID], nil
}

func (r *visionRoutingHydrationRepo) GetByGroupIDs(_ context.Context, groupIDs []int64) (map[int64]map[string][]int64, error) {
	r.mu.Lock()
	r.batchCalls++
	r.batchIDs = append(r.batchIDs, groupIDs...)
	fn := r.batchFn
	r.mu.Unlock()
	if fn != nil {
		return fn(context.Background(), groupIDs)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[int64]map[string][]int64, len(groupIDs))
	for _, id := range groupIDs {
		if v, ok := r.data[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func (r *visionRoutingHydrationRepo) Set(context.Context, int64, map[string][]int64) error {
	panic("unexpected Set call in hydration test")
}

func TestAdminGroupVisionHydration_GetAllGroups_BatchSingleQuery(t *testing.T) {
	// 批量路径必须一次 GetByGroupIDs（无 N+1），返回的 VisionRouting 与注入配置一致。
	groupRepo := &adminGroupHydrationGroupRepo{
		active: []Group{
			{ID: 1, Name: "g1", Platform: PlatformOpenAI, Status: StatusActive},
			{ID: 2, Name: "g2", Platform: PlatformOpenAI, Status: StatusActive},
			{ID: 3, Name: "g3", Platform: PlatformOpenAI, Status: StatusActive},
		},
	}
	vr := &visionRoutingHydrationRepo{data: map[int64]map[string][]int64{
		1: {"gpt-5": {11, 12}},
		2: {"claude-*": {21}},
		// 3 无配置 → 应水合为空 map
	}}
	svc := &adminServiceImpl{groupRepo: groupRepo, visionRouting: NewVisionRoutingService(vr, nil)}

	groups, err := svc.GetAllGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, groups, 3)

	require.Equal(t, map[string][]int64{"gpt-5": {11, 12}}, groups[0].VisionRouting)
	require.Equal(t, map[string][]int64{"claude-*": {21}}, groups[1].VisionRouting)
	require.NotNil(t, groups[2].VisionRouting, "未配置分组应水合为空 map 而非 nil")
	require.Empty(t, groups[2].VisionRouting)

	vr.mu.Lock()
	defer vr.mu.Unlock()
	require.Equal(t, 1, vr.batchCalls, "批量路径必须恰好一次 GetByGroupIDs")
	require.Equal(t, 0, vr.getCalls, "批量路径禁止逐组调用 Get（N+1）")
	require.Equal(t, []int64{1, 2, 3}, vr.batchIDs)
}

func TestAdminGroupVisionHydration_GetAllGroupsByPlatform_Hydrates(t *testing.T) {
	// 消费方取证：GetAllGroupsByPlatform 的返回值喂给 GroupFromServiceAdmin（group_handler.go:647/669），
	// 属于 admin 分组管理读取链，必须水合。
	groupRepo := &adminGroupHydrationGroupRepo{
		activePlatform: []Group{{ID: 7, Name: "p7", Platform: PlatformAnthropic, Status: StatusActive}},
	}
	vr := &visionRoutingHydrationRepo{data: map[int64]map[string][]int64{7: {"claude-opus-*": {55}}}}
	svc := &adminServiceImpl{groupRepo: groupRepo, visionRouting: NewVisionRoutingService(vr, nil)}

	groups, err := svc.GetAllGroupsByPlatform(context.Background(), PlatformAnthropic)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, map[string][]int64{"claude-opus-*": {55}}, groups[0].VisionRouting)
}

func TestAdminGroupVisionHydration_ListGroupsAndIncludingInactive(t *testing.T) {
	groupRepo := &adminGroupHydrationGroupRepo{
		filtered: []Group{{ID: 9, Name: "g9", Platform: PlatformOpenAI, Status: StatusActive}},
	}
	vr := &visionRoutingHydrationRepo{data: map[int64]map[string][]int64{9: {"gpt-4o": {9}}}}
	svc := &adminServiceImpl{groupRepo: groupRepo, visionRouting: NewVisionRoutingService(vr, nil)}

	groups, total, err := svc.ListGroups(context.Background(), 1, 10, "", "", "", nil, "id", "asc")
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, map[string][]int64{"gpt-4o": {9}}, groups[0].VisionRouting)

	groups, err = svc.GetAllGroupsIncludingInactive(context.Background())
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, map[string][]int64{"gpt-4o": {9}}, groups[0].VisionRouting)
}

func TestAdminGroupVisionHydration_GetGroup_SingleConsistency(t *testing.T) {
	groupRepo := &adminGroupHydrationGroupRepo{
		byID: map[int64]*Group{42: {ID: 42, Name: "g42", Platform: PlatformOpenAI, Status: StatusActive}},
	}
	vr := &visionRoutingHydrationRepo{data: map[int64]map[string][]int64{42: {"gpt-5": {1, 2}}}}
	svc := &adminServiceImpl{groupRepo: groupRepo, visionRouting: NewVisionRoutingService(vr, nil)}

	group, err := svc.GetGroup(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, map[string][]int64{"gpt-5": {1, 2}}, group.VisionRouting)
}

func TestAdminGroupVisionHydration_ReadErrorFailsClosed(t *testing.T) {
	// 水合读错误必须使整个读取方法失败关闭，禁止静默当 nil。
	wantErr := errors.New("vision routing backend unavailable")
	groupRepo := &adminGroupHydrationGroupRepo{
		active: []Group{{ID: 1, Name: "g1", Platform: PlatformOpenAI, Status: StatusActive}},
	}
	vr := &visionRoutingHydrationRepo{batchFn: func(context.Context, []int64) (map[int64]map[string][]int64, error) {
		return nil, wantErr
	}}
	svc := &adminServiceImpl{groupRepo: groupRepo, visionRouting: NewVisionRoutingService(vr, nil)}

	_, err := svc.GetAllGroups(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, wantErr)
}

func TestAdminGroupVisionHydration_NilVisionRoutingServiceKeepsEmptyMap(t *testing.T) {
	// visionRouting 未注入时读取链保持既有行为：VisionRouting 恒为空 map（不 panic、不报错）。
	groupRepo := &adminGroupHydrationGroupRepo{
		active: []Group{{ID: 1, Name: "g1", Platform: PlatformOpenAI, Status: StatusActive}},
	}
	svc := &adminServiceImpl{groupRepo: groupRepo}

	groups, err := svc.GetAllGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.NotNil(t, groups[0].VisionRouting)
	require.Empty(t, groups[0].VisionRouting)
}
