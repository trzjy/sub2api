package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeVRRepository 是 VisionRoutingRepository 的测试替身：记录 Set 调用并回放注入配置。
type fakeVRRepository struct {
	routing  map[string][]int64
	setCalls int
	setErr   error
}

func (r *fakeVRRepository) Get(_ context.Context, _ int64) (map[string][]int64, error) {
	return r.routing, nil
}

func (r *fakeVRRepository) GetByGroupIDs(_ context.Context, _ []int64) (map[int64]map[string][]int64, error) {
	panic("unexpected call to GetByGroupIDs")
}

func (r *fakeVRRepository) Set(_ context.Context, _ int64, routing map[string][]int64) error {
	r.setCalls++
	r.routing = routing
	return r.setErr
}

// fakeVRGroupReader 是 VisionRoutingGroupAccountsReader 的测试替身。
type fakeVRGroupReader struct{ allowed []int64 }

func (r *fakeVRGroupReader) GetAccountIDsByGroupIDs(_ context.Context, _ []int64) ([]int64, error) {
	return r.allowed, nil
}

// 场景 ①：模型模式为空/空白 → 写入被拒绝。
func TestVisionRoutingService_SetEmptyModelRejected(t *testing.T) {
	repo := &fakeVRRepository{}
	reader := &fakeVRGroupReader{allowed: []int64{1, 2}}
	svc := NewVisionRoutingService(repo, reader)

	cases := []map[string][]int64{
		{"": {1}},               // 空模型
		{"   ": {1}},            // 纯空白模型
		{"gpt-4": {1}, "": {2}}, // 混合：存在空模型
	}
	for _, routing := range cases {
		err := svc.Set(context.Background(), 1, routing)
		require.Error(t, err, "空/空白模型模式必须被拒绝")
		require.ErrorIs(t, err, ErrVisionRoutingInvalidConfig, "必须返回无效配置错误")
		require.Equal(t, 0, repo.setCalls, "拒绝时不得落库")
	}
}

// 场景 ②：目标账号列表为空 → 写入被拒绝。
func TestVisionRoutingService_SetEmptyTargetListRejected(t *testing.T) {
	repo := &fakeVRRepository{}
	reader := &fakeVRGroupReader{allowed: []int64{1, 2}}
	svc := NewVisionRoutingService(repo, reader)

	cases := []map[string][]int64{
		{"gpt-4": {}},               // 目标列表为空
		{"gpt-4": {1}, "gpt-5": {}}, // 混合：存在空目标列表
	}
	for _, routing := range cases {
		err := svc.Set(context.Background(), 1, routing)
		require.Error(t, err, "空目标账号列表必须被拒绝")
		require.ErrorIs(t, err, ErrVisionRoutingInvalidConfig, "必须返回无效配置错误")
		require.Equal(t, 0, repo.setCalls, "拒绝时不得落库")
	}
}

// 场景 ③：合法配置（非空模型 + 非空目标列表 + 同组）不受影响，正常落库。
func TestVisionRoutingService_SetValidConfigAllowed(t *testing.T) {
	repo := &fakeVRRepository{}
	reader := &fakeVRGroupReader{allowed: []int64{1, 2}}
	svc := NewVisionRoutingService(repo, reader)

	routing := map[string][]int64{"gpt-4": {1}, "gpt-5": {2}}
	err := svc.Set(context.Background(), 1, routing)
	require.NoError(t, err, "合法配置必须正常通过")
	require.Equal(t, 1, repo.setCalls, "合法配置必须落库")
	require.Equal(t, routing, repo.routing, "落库内容应与写入一致")

	// 无目标引用的配置（空 map）跳过跨组校验，仍正常落库空配置。
	repoEmpty := &fakeVRRepository{}
	svcEmpty := NewVisionRoutingService(repoEmpty, nil)
	err = svcEmpty.Set(context.Background(), 1, map[string][]int64{})
	require.NoError(t, err, "空配置（无规则）必须照常通过")
	require.Equal(t, 1, repoEmpty.setCalls, "空配置仍须落库")
}

// 场景 ④（Vision-S4）：含 0 账号 ID 的配置必须被拒绝，不绕过同组校验。
func TestVisionRoutingService_SetZeroTargetIDRejected(t *testing.T) {
	repo := &fakeVRRepository{}
	reader := &fakeVRGroupReader{allowed: []int64{1, 2}}
	svc := NewVisionRoutingService(repo, reader)

	cases := []map[string][]int64{
		{"gpt-4": {0}},               // 纯 0 ID
		{"gpt-4": {0, 1}},            // 混合：含 0 与合法 ID
		{"gpt-4": {1}, "gpt-5": {0}}, // 混合：另一模型含 0
	}
	for _, routing := range cases {
		err := svc.Set(context.Background(), 1, routing)
		require.Error(t, err, "含 0 账号 ID 的配置必须被拒绝")
		require.ErrorIs(t, err, ErrVisionRoutingInvalidConfig, "必须返回无效配置错误")
		require.Equal(t, 0, repo.setCalls, "拒绝时不得落库")
	}
}

// 场景 ⑤（Vision-S4）：含负数账号 ID 的配置必须被拒绝。
func TestVisionRoutingService_SetNegativeTargetIDRejected(t *testing.T) {
	repo := &fakeVRRepository{}
	reader := &fakeVRGroupReader{allowed: []int64{1, 2}}
	svc := NewVisionRoutingService(repo, reader)

	cases := []map[string][]int64{
		{"gpt-4": {-1}},               // 纯负数 ID
		{"gpt-4": {-5, 1}},            // 混合：含负数与合法 ID
		{"gpt-4": {1}, "gpt-5": {-2}}, // 混合：另一模型含负数
	}
	for _, routing := range cases {
		err := svc.Set(context.Background(), 1, routing)
		require.Error(t, err, "含负数账号 ID 的配置必须被拒绝")
		require.ErrorIs(t, err, ErrVisionRoutingInvalidConfig, "必须返回无效配置错误")
		require.Equal(t, 0, repo.setCalls, "拒绝时不得落库")
	}
}

// 场景 ⑥（Vision-S4）：合法正 ID 配置零回归（含 0/负数被拒后，合法配置仍正常落库）。
func TestVisionRoutingService_SetPositiveTargetIDZeroRegression(t *testing.T) {
	repo := &fakeVRRepository{}
	reader := &fakeVRGroupReader{allowed: []int64{1, 2, 3}}
	svc := NewVisionRoutingService(repo, reader)

	routing := map[string][]int64{"gpt-4": {1}, "gpt-5": {2, 3}}
	err := svc.Set(context.Background(), 1, routing)
	require.NoError(t, err, "合法正 ID 配置必须正常通过")
	require.Equal(t, 1, repo.setCalls, "合法配置必须落库")
	require.Equal(t, routing, repo.routing, "落库内容应与写入一致")
}

// 场景 ⑦（派发单 B）：Validate 与 Set 共用同一校验实现，但 Validate 不落库
// （setCalls 保持 0），校验结果与 Set 完全一致（同组/无效配置均被拒）。
func TestVisionRoutingService_ValidateDoesNotPersist(t *testing.T) {
	repo := &fakeVRRepository{}
	reader := &fakeVRGroupReader{allowed: []int64{1, 2}}
	svc := NewVisionRoutingService(repo, reader)

	// 合法配置：Validate 通过且不落库。
	require.NoError(t, svc.Validate(context.Background(), 1, map[string][]int64{"gpt-4": {1}}))
	require.Equal(t, 0, repo.setCalls, "Validate 不得落库")

	// 跨组配置：Validate 与 Set 返回同类错误。
	require.ErrorIs(t, svc.Validate(context.Background(), 1, map[string][]int64{"gpt-4": {999}}), ErrVisionRoutingCrossGroup)
	require.ErrorIs(t, svc.Set(context.Background(), 1, map[string][]int64{"gpt-4": {999}}), ErrVisionRoutingCrossGroup)

	// 非法配置：Validate 与 Set 返回同类错误。
	require.ErrorIs(t, svc.Validate(context.Background(), 1, map[string][]int64{"": {1}}), ErrVisionRoutingInvalidConfig)
	require.Equal(t, 0, repo.setCalls, "校验失败同样不得落库")

	// 空配置：Validate 通过（等价清空），不落库。
	require.NoError(t, svc.Validate(context.Background(), 1, map[string][]int64{}))
	require.Equal(t, 0, repo.setCalls)
}

// 场景 ⑧（派发单 B）：ValidateVisionRoutingShape 只校验配置自身形状，
// 不做同组校验（创建前分组尚无账号上下文）。
func TestValidateVisionRoutingShape(t *testing.T) {
	require.NoError(t, ValidateVisionRoutingShape(nil), "nil 视为无配置，合法")
	require.NoError(t, ValidateVisionRoutingShape(map[string][]int64{}), "空配置合法")
	// 跨组账号（同组校验无法在无上下文时完成）在形状校验中通过——由落库后 Set 兜底。
	require.NoError(t, ValidateVisionRoutingShape(map[string][]int64{"gpt-4": {999}}), "形状校验不做同组校验")
	require.ErrorIs(t, ValidateVisionRoutingShape(map[string][]int64{"": {1}}), ErrVisionRoutingInvalidConfig)
	require.ErrorIs(t, ValidateVisionRoutingShape(map[string][]int64{"gpt-4": {}}), ErrVisionRoutingInvalidConfig)
	require.ErrorIs(t, ValidateVisionRoutingShape(map[string][]int64{"gpt-4": {0}}), ErrVisionRoutingInvalidConfig)
}
