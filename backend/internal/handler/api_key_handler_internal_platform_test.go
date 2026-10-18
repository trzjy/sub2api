//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 用户 API Key 四端点（List/GetByID/Create/Update）的内部供给平台剥离测试。
// 判定源 domain.IsInternalSupplyPlatform；方案见
// docs-local/internal-platform-key-visibility-repair-plan.md §4。

// keyVisStubAPIKeyRepo 只实现四个端点实际触达的方法，其余由嵌入接口兜底。
type keyVisStubAPIKeyRepo struct {
	service.APIKeyRepository
	byID   map[int64]*service.APIKey
	byUser []service.APIKey
	nextID int64
}

func (r *keyVisStubAPIKeyRepo) GetByID(_ context.Context, id int64) (*service.APIKey, error) {
	if k, ok := r.byID[id]; ok {
		return k, nil
	}
	return nil, service.ErrAPIKeyNotFound
}

func (r *keyVisStubAPIKeyRepo) ListByUserID(_ context.Context, _ int64, _ pagination.PaginationParams, _ service.APIKeyListFilters) ([]service.APIKey, *pagination.PaginationResult, error) {
	total := int64(len(r.byUser))
	return r.byUser, &pagination.PaginationResult{Total: total, Page: 1, PageSize: 20, Pages: 1}, nil
}

func (r *keyVisStubAPIKeyRepo) Create(_ context.Context, key *service.APIKey) error {
	r.nextID++
	key.ID = r.nextID
	if r.byID == nil {
		r.byID = map[int64]*service.APIKey{}
	}
	r.byID[key.ID] = key
	return nil
}

func (r *keyVisStubAPIKeyRepo) Update(_ context.Context, _ *service.APIKey, _ service.APIKeyUpdateFields) error {
	return nil
}

// keyVisStubUserRepo / keyVisStubGroupRepo 供 Create 路径的归属与分组校验。
type keyVisStubUserRepo struct {
	service.UserRepository
	user *service.User
}

func (r *keyVisStubUserRepo) GetByID(_ context.Context, _ int64) (*service.User, error) {
	return r.user, nil
}

type keyVisStubGroupRepo struct {
	service.GroupRepository
	groups map[int64]*service.Group
}

func (r *keyVisStubGroupRepo) GetByID(_ context.Context, id int64) (*service.Group, error) {
	if g, ok := r.groups[id]; ok {
		return g, nil
	}
	return nil, service.ErrGroupNotFound
}

// keyVisFixtures 构造五个分组：三个内部供给平台 + composite + anthropic 公开组。
func keyVisFixtures() (keys []service.APIKey, byID map[int64]*service.APIKey, groups map[int64]*service.Group) {
	mkGroup := func(id int64, platform string, aggregate bool) *service.Group {
		return &service.Group{
			ID:                        id,
			Name:                      "group-" + platform + "-fixture",
			Platform:                  platform,
			Status:                    "active",
			AggregateCodeBuddyEnabled: aggregate,
		}
	}
	groups = map[int64]*service.Group{
		10: mkGroup(10, "codebuddy", true),
		11: mkGroup(11, "muse", false),
		12: mkGroup(12, "cursor", false),
		20: mkGroup(20, "composite", true),
		30: mkGroup(30, "anthropic", false),
	}
	mkKey := func(id int64, groupID int64) service.APIKey {
		gid := groupID
		return service.APIKey{
			ID:      id,
			UserID:  1,
			Key:     "sk-fixture-" + strings.ToUpper(string(rune('A'+id))),
			Name:    "key-" + string(rune('A'+id)),
			GroupID: &gid,
			Group:   groups[groupID],
			Status:  "active",
		}
	}
	keys = []service.APIKey{
		mkKey(101, 10),
		mkKey(102, 11),
		mkKey(103, 12),
		mkKey(201, 20),
		mkKey(301, 30),
	}
	byID = map[int64]*service.APIKey{}
	for i := range keys {
		byID[keys[i].ID] = &keys[i]
	}
	return keys, byID, groups
}

func newKeyVisHandler() (*APIKeyHandler, map[int64]*service.APIKey, map[int64]*service.Group) {
	keys, byID, groups := keyVisFixtures()
	repo := &keyVisStubAPIKeyRepo{byID: byID, byUser: keys, nextID: 1000}
	// cfg 只需让 GenerateKey 拿到 key 前缀；其余限额字段保持零值即关闭。
	cfg := &config.Config{Default: config.DefaultConfig{APIKeyPrefix: "sk-"}}
	svc := service.NewAPIKeyService(
		repo,
		&keyVisStubUserRepo{user: &service.User{ID: 1, Email: "u@test", RestrictPublicGroups: false}},
		&keyVisStubGroupRepo{groups: groups},
		nil, nil, nil, cfg,
	)
	return NewAPIKeyHandler(svc), byID, groups
}

func runKeyVisContext(method, path string, body io.Reader) (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, body)
	if body != nil {
		c.Request.Header.Set("Content-Type", "application/json")
	}
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
	return w, c
}

// decodeKeyVisData 解码响应信封 data 字段。
func decodeKeyVisData(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code)
	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	require.NoError(t, json.Unmarshal(envelope.Data, out))
}

// assertNoSupplyPlatformValue 断言响应 JSON 值层面不含三个内部供给平台标识。
// 2026-10-18 用户裁定后：聚合直绑字段已整体迁出用户侧 Group DTO，用户响应
// 既不含旧 tag aggregate_codebuddy_enabled、也不含新 tag aggregate_direct_enabled。
func assertNoSupplyPlatformValue(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	body := w.Body.String()
	for _, platform := range []string{`"platform":"codebuddy"`, `"platform":"muse"`, `"platform":"cursor"`} {
		require.NotContains(t, body, platform)
	}
	require.NotContains(t, body, "aggregate_", "user response must not contain the admin-only aggregate flag under any name")
}

func TestAPIKeyListStripsInternalSupplyPlatform(t *testing.T) {
	h, _, _ := newKeyVisHandler()
	w, c := runKeyVisContext(http.MethodGet, "/api/v1/api-keys", nil)
	h.List(c)
	var out struct {
		Items []dto.APIKey `json:"items"`
	}
	decodeKeyVisData(t, w, &out)
	keysOut := out.Items
	require.Len(t, keysOut, 5)
	assertNoSupplyPlatformValue(t, w)

	byGroupPlatform := map[int64]string{}
	for _, k := range keysOut {
		require.NotNil(t, k.Group, "bound key must keep its group object (no fake 'ungrouped' state)")
		require.NotNil(t, k.GroupID)
		byGroupPlatform[*k.GroupID] = k.Group.Platform
	}
	require.Empty(t, byGroupPlatform[10], "codebuddy group platform must be stripped")
	require.Empty(t, byGroupPlatform[11], "muse group platform must be stripped")
	require.Empty(t, byGroupPlatform[12], "cursor group platform must be stripped")
	require.Equal(t, "composite", byGroupPlatform[20], "composite group must keep its platform")
	require.Equal(t, "anthropic", byGroupPlatform[30], "public group must keep its platform")
}

func TestAPIKeyGetByIDStripsEachInternalSupplyPlatform(t *testing.T) {
	cases := []struct {
		keyID        int64
		groupID      int64
		isInternal   bool
		wantPlatform string
	}{
		{101, 10, true, ""},
		{102, 11, true, ""},
		{103, 12, true, ""},
		{201, 20, false, "composite"},
		{301, 30, false, "anthropic"},
	}
	for _, tc := range cases {
		h, _, _ := newKeyVisHandler()
		w, c := runKeyVisContext(http.MethodGet, "/api/v1/api-keys/101", nil)
		c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(tc.keyID, 10)}}
		h.GetByID(c)
		var out dto.APIKey
		decodeKeyVisData(t, w, &out)
		assertNoSupplyPlatformValue(t, w)

		require.NotNil(t, out.Group)
		require.NotNil(t, out.GroupID)
		require.Equal(t, tc.groupID, *out.GroupID, "group_id binding must be preserved")
		require.Equal(t, tc.wantPlatform, out.Group.Platform, "exact platform contract (group %d)", tc.groupID)
	}
}

func TestAPIKeyUpdateStripsInternalSupplyPlatform(t *testing.T) {
	// 三内部平台分别覆盖 Update 响应（101/102/103 分别绑定 codebuddy/muse/cursor 组）。
	for _, keyID := range []int64{101, 102, 103} {
		h, _, _ := newKeyVisHandler()
		w, c := runKeyVisContext(http.MethodPut, "/api/v1/api-keys/101", strings.NewReader(`{"name":"renamed"}`))
		c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(keyID, 10)}}
		h.Update(c)
		var out dto.APIKey
		decodeKeyVisData(t, w, &out)
		assertNoSupplyPlatformValue(t, w)

		require.NotNil(t, out.Group, "updated key must still show its group binding")
		require.NotNil(t, out.GroupID)
		require.Equal(t, keyID-91, *out.GroupID, "group_id must be unchanged")
		require.Empty(t, out.Group.Platform)
	}
}

func TestAPIKeyCreateWithInternalSupplyGroupKeepsBindingWithoutSupplier(t *testing.T) {
	h, _, _ := newKeyVisHandler()
	w, c := runKeyVisContext(http.MethodPost, "/api/v1/api-keys", strings.NewReader(`{"name":"new-key","group_id":10}`))
	h.Create(c)
	var out dto.APIKey
	decodeKeyVisData(t, w, &out)
	assertNoSupplyPlatformValue(t, w)

	// 契约钉死（外审 must_fix 消化，不留条件断言盲区）：
	// ① 绑定保留：group_id 原样返回，Key 不因创建而失去分组归属；
	// ② service.Create 不 eager-load Group（既有响应形态），因此响应 group 恒为
	//    null——此时供给方平台值无从出现；前端创建成功后立即 loadApiKeys()
	//    （KeysView.vue 的列表重载）从 List 端点取已剥离的分组数据渲染，
	//    Create 响应的 group 字段不进入任何 UI 渲染路径。
	// 若未来 service.Create 开始回带 Group，本断言失败即强制决策者显式更新
	// 该序列化契约，不允许静默引入未剥离的分组对象。
	require.NotNil(t, out.GroupID, "created key must stay bound to the requested group")
	require.Equal(t, int64(10), *out.GroupID)
	require.Nil(t, out.Group, "create response must not carry an unstripped group object; if service starts eager-loading Group, update this contract consciously")
}
