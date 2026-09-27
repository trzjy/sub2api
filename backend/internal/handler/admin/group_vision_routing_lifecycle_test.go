package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// recordingVisionRoutingRepo 是 VisionRoutingRepository 的测试替身：
// 记录 Set 调用次数并保存每组的落库配置（区别于 fakeVisionRoutingRepo，额外暴露
// setCalls 供"未携带不得触碰配置"断言）。
type recordingVisionRoutingRepo struct {
	mu       sync.Mutex
	data     map[int64]map[string][]int64
	setCalls int
}

func newRecordingVisionRoutingRepo(seed map[int64]map[string][]int64) *recordingVisionRoutingRepo {
	return &recordingVisionRoutingRepo{data: seed}
}

func (r *recordingVisionRoutingRepo) Get(_ context.Context, groupID int64) (map[string][]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.data[groupID], nil
}

func (r *recordingVisionRoutingRepo) GetByGroupIDs(ctx context.Context, groupIDs []int64) (map[int64]map[string][]int64, error) {
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

func (r *recordingVisionRoutingRepo) Set(_ context.Context, groupID int64, routing map[string][]int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setCalls++
	if r.data == nil {
		r.data = map[int64]map[string][]int64{}
	}
	r.data[groupID] = routing
	return nil
}

func (r *recordingVisionRoutingRepo) snapshot() (int, map[int64]map[string][]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make(map[int64]map[string][]int64, len(r.data))
	for k, v := range r.data {
		copied[k] = v
	}
	return r.setCalls, copied
}

// newVisionLifecycleHarness 装配分组 handler：注入视觉分流 service（同组账号固定 allowed），
// 并注册 create/update 路由。
func newVisionLifecycleHarness(t *testing.T, repo *recordingVisionRoutingRepo, allowed ...int64) (*gin.Engine, *stubAdminService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := newStubAdminService()
	vs := service.NewVisionRoutingService(repo, &fakeGroupAccountsReader{allowed: allowed})
	h := NewGroupHandlerWithConfig(svc, nil, nil, &config.Config{})
	h.SetVisionRoutingService(vs)

	r := gin.New()
	r.POST("/groups", h.Create)
	r.PUT("/groups/:id", h.Update)
	return r, svc
}

func doVisionRequest(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// 场景 A：显式空配置清空 / 未携带不动 / Create 空无配置
// ---------------------------------------------------------------------------

// Update 显式携带空 map ⇒ 既有配置被清空（DB 落库为空对象）。
func TestGroupVisionRoutingUpdateExplicitEmptyClears(t *testing.T) {
	repo := newRecordingVisionRoutingRepo(map[int64]map[string][]int64{
		42: {"gpt-4": {1}},
	})
	r, _ := newVisionLifecycleHarness(t, repo, 1, 2)

	rec := doVisionRequest(t, r, http.MethodPut, "/groups/42", `{"name":"g","vision_routing":{}}`)

	require.Equal(t, http.StatusOK, rec.Code, "显式空配置提交应成功")
	_, data := repo.snapshot()
	require.Contains(t, data, int64(42), "显式空配置必须落库（写入空对象而非跳过）")
	require.Empty(t, data[42], "既有规则必须被清空")
}

// Update 未携带 vision_routing ⇒ 不触碰既有配置（Set 零调用）。
func TestGroupVisionRoutingUpdateOmittedLeavesUnchanged(t *testing.T) {
	existing := map[string][]int64{"gpt-4": {1}}
	repo := newRecordingVisionRoutingRepo(map[int64]map[string][]int64{42: existing})
	r, _ := newVisionLifecycleHarness(t, repo, 1, 2)

	rec := doVisionRequest(t, r, http.MethodPut, "/groups/42", `{"name":"g"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	setCalls, data := repo.snapshot()
	require.Equal(t, 0, setCalls, "未携带字段不得调用 Set")
	require.Equal(t, existing, data[42], "既有配置必须保持不变")
}

// Create 显式携带空 map ⇒ 落库无配置（空对象），不残留。
func TestGroupVisionRoutingCreateExplicitEmptyWritesNoConfig(t *testing.T) {
	repo := newRecordingVisionRoutingRepo(nil)
	r, _ := newVisionLifecycleHarness(t, repo, 1, 2)

	rec := doVisionRequest(t, r, http.MethodPost, "/groups", `{"name":"n","platform":"openai","vision_routing":{}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	_, data := repo.snapshot()
	require.Contains(t, data, int64(200), "Create 携带空配置应写入空对象")
	require.Empty(t, data[200], "Create 空配置落库应为空")
}

// Update 显式携带 null ⇒ 视为显式空配置，清空既有配置（与 {} 等价）。
func TestGroupVisionRoutingUpdateExplicitNullClears(t *testing.T) {
	repo := newRecordingVisionRoutingRepo(map[int64]map[string][]int64{
		42: {"gpt-4": {1}},
	})
	r, _ := newVisionLifecycleHarness(t, repo, 1, 2)

	rec := doVisionRequest(t, r, http.MethodPut, "/groups/42", `{"name":"g","vision_routing":null}`)

	require.Equal(t, http.StatusOK, rec.Code, "显式 null 提交应成功")
	setCalls, data := repo.snapshot()
	require.Equal(t, 1, setCalls, "显式 null 必须调用 Set（清空）")
	require.Empty(t, data[42], "既有规则必须被清空")
}

// Create 未携带 vision_routing ⇒ 不写配置（无 Set 调用）。
func TestGroupVisionRoutingCreateOmittedWritesNothing(t *testing.T) {
	repo := newRecordingVisionRoutingRepo(nil)
	r, _ := newVisionLifecycleHarness(t, repo, 1, 2)

	rec := doVisionRequest(t, r, http.MethodPost, "/groups", `{"name":"n","platform":"openai"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	setCalls, _ := repo.snapshot()
	require.Equal(t, 0, setCalls, "未携带字段不得调用 Set")
}

// ---------------------------------------------------------------------------
// 场景 B：校验前置，消除部分提交
// ---------------------------------------------------------------------------

// Update 视觉分流校验失败（跨组）⇒ 分组零变更（UpdateGroup 不被调用）。
func TestGroupVisionRoutingUpdateValidationFailureSkipsGroupWrite(t *testing.T) {
	repo := newRecordingVisionRoutingRepo(map[int64]map[string][]int64{42: {"gpt-4": {1}}})
	r, svc := newVisionLifecycleHarness(t, repo, 1, 2)

	// 999 不属于分组 42 → 同组校验拒绝
	rec := doVisionRequest(t, r, http.MethodPut, "/groups/42", `{"name":"g","vision_routing":{"gpt-4":[999]}}`)

	require.GreaterOrEqual(t, rec.Code, 400)
	require.Less(t, rec.Code, 500)
	require.Empty(t, svc.updatedGroups, "校验失败必须零变更：不得调用 UpdateGroup")
	setCalls, data := repo.snapshot()
	require.Equal(t, 0, setCalls, "校验失败不得落库视觉分流配置")
	require.Equal(t, map[string][]int64{"gpt-4": {1}}, data[42], "既有配置保持不变")
}

// Create 视觉分流配置自身非法（空模型模式）⇒ 分组零落库（CreateGroup 不被调用）。
func TestGroupVisionRoutingCreateShapeValidationSkipsGroupWrite(t *testing.T) {
	repo := newRecordingVisionRoutingRepo(nil)
	r, svc := newVisionLifecycleHarness(t, repo, 1, 2)

	rec := doVisionRequest(t, r, http.MethodPost, "/groups", `{"name":"n","platform":"openai","vision_routing":{"":[1]}}`)

	require.GreaterOrEqual(t, rec.Code, 400)
	require.Less(t, rec.Code, 500)
	require.Empty(t, svc.createdGroups, "非法配置必须在分组创建前被拒绝（零落库）")
}

// Update 合法配置正常落库，零回归。
func TestGroupVisionRoutingUpdateValidPersists(t *testing.T) {
	repo := newRecordingVisionRoutingRepo(map[int64]map[string][]int64{42: {}})
	r, _ := newVisionLifecycleHarness(t, repo, 1, 2)

	rec := doVisionRequest(t, r, http.MethodPut, "/groups/42", `{"name":"g","vision_routing":{"gpt-4":[1,2]}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	_, data := repo.snapshot()
	require.Equal(t, map[string][]int64{"gpt-4": {1, 2}}, data[42])
}
