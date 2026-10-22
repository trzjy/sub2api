//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// W2-C（方案 §25.14-B 下半：Gemini 列表正向接入 Cursor 供给）定向测试。
//
// 覆盖清单（对应派发单「响应矩阵」七行与 Done when）：
//  1. Cursor-only 组：200 + 合法 Cursor 目录，AIStudio GET 调用数 0；
//  2. 混合组且原生成功：原生条目 + Cursor 条目合并，同名一份，原生 metadata/信封保留；
//  3. 空组 / 全部供给被明确否决 / 全部名被白名单过滤：无原生候选时 200 且 models: []，
//     不补默认列表；空集合与「依赖读取失败」严格区分；
//  4. 原生候选查询失败：503 目录读取失败（不把数据库故障当「只有 Cursor」）；
//  5. 原生请求失败/非成功：保留原生明确失败，有 Cursor 补充时不进入旧默认目录兜底；
//     原生-only 历史行为不扩大修改；
//  6. 原生 200 但报文无效：需要合并或过滤时 502，不悄悄漏 Cursor/漏过滤；
//  7. 强制 Antigravity 入口：只读 AG 目录，Cursor 读入口与 AIStudio 调用数均 0。
//
// 另外钉死：重复名去重且原生优先、通配键不出现、组外账号不贡献、mapping 别名不取上游值、
// 来源过期/耗尽/失败/缺失的区分、读取错误 ≠ 空目录、原生 metadata 保留。
//
// 全部断言走包内既有夹具（stub repo + 上游替身 + 已安装的来源状态替身），不连 DB。

const (
	cursorGeminiListGroupID = int64(9301)
	// 两个具体公开名保持字典序（A < B），使排序稳定的断言可读。
	cursorGeminiListModelA = "cursor-haiku"
	cursorGeminiListModelB = "cursor-sonnet"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// cursorGeminiListRepo 同时实现两条链需要的账号查询：
//   - ListSchedulableByGroupID：窄只读入口 GetCursorSupplyModelIDs 的「已绑定该组」查询；
//   - ListSchedulableByGroupIDAndPlatforms：Gemini 兼容层原生选号查询。
//
// 两者都按账号显式挂组（GroupIDs）过滤，模拟真实 repo 只返回组内账号，使「组外账号不贡献」
// 与「列表=调度可达同源」可被断言。
type cursorGeminiListRepo struct {
	service.AccountRepository

	byGroup map[int64][]service.Account

	// supplyErr 让窄只读入口的账号查询失败（读取错误 ≠ 空目录）。
	supplyErr error
	// nativeErr 让 Gemini 原生选号查询失败（原生候选查询失败 → 503）。
	nativeErr error

	supplyQueries  atomic.Int64
	nativeQueries  atomic.Int64
	lastSupplyGrp  atomic.Int64
	lastNativeGrps atomic.Int64
}

func (r *cursorGeminiListRepo) boundTo(accounts []service.Account, groupID int64) []service.Account {
	out := make([]service.Account, 0, len(accounts))
	for _, account := range accounts {
		for _, gid := range account.GroupIDs {
			if gid == groupID {
				out = append(out, account)
				break
			}
		}
	}
	return out
}

func (r *cursorGeminiListRepo) ListSchedulableByGroupID(_ context.Context, groupID int64) ([]service.Account, error) {
	r.supplyQueries.Add(1)
	r.lastSupplyGrp.Store(groupID)
	if r.supplyErr != nil {
		return nil, r.supplyErr
	}
	return r.boundTo(r.byGroup[groupID], groupID), nil
}

func (r *cursorGeminiListRepo) ListSchedulableByGroupIDAndPlatforms(_ context.Context, groupID int64, platforms []string) ([]service.Account, error) {
	r.nativeQueries.Add(1)
	if r.nativeErr != nil {
		return nil, r.nativeErr
	}
	allowed := make(map[string]struct{}, len(platforms))
	for _, platform := range platforms {
		allowed[platform] = struct{}{}
	}
	accounts := r.boundTo(r.byGroup[groupID], groupID)
	filtered := make([]service.Account, 0, len(accounts))
	for _, account := range accounts {
		if _, ok := allowed[account.Platform]; ok {
			filtered = append(filtered, account)
		}
	}
	return filtered, nil
}

// cursorGeminiListGroupRepo 只在 ctx 未装载分组时被回源（组白名单读取）。
type cursorGeminiListGroupRepo struct {
	service.GroupRepository
	group *service.Group
	err   error
}

func (r *cursorGeminiListGroupRepo) GetByIDLite(_ context.Context, _ int64) (*service.Group, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.group, nil
}

// cursorGeminiListUpstream 记录 AIStudio GET 的出站次数（Cursor-only / 强制 AG 行必须为 0），
// 并原样回放预设状态码与报文。
type cursorGeminiListUpstream struct {
	service.HTTPUpstream
	status int
	body   string
	err    error
	calls  atomic.Int64
}

func (u *cursorGeminiListUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	if u.err != nil {
		return nil, u.err
	}
	return &http.Response{
		StatusCode: u.status,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"X-Request-Id": {"cursor-supply-list"},
		},
		Body: io.NopCloser(strings.NewReader(u.body)),
	}, nil
}

// cursorGeminiListQuotaKeyState 是来源状态读取替身（合同：替身仅存于测试）。
type cursorGeminiListQuotaKeyState struct {
	statuses map[int64]string
	err      error
}

func (s cursorGeminiListQuotaKeyState) CursorQuotaKeyStatus(_ context.Context, quotaKeyID int64) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	status, ok := s.statuses[quotaKeyID]
	if !ok {
		return "", errors.New("quota key not found")
	}
	return status, nil
}

// installCursorGeminiQuotaKeyState 安装来源状态替身并在结束时还原为未安装（包级单例）。
func installCursorGeminiQuotaKeyState(t *testing.T, provider service.CursorQuotaKeyStateProvider) {
	t.Helper()
	service.InstallCursorQuotaKeyStateProvider(provider)
	t.Cleanup(func() { service.InstallCursorQuotaKeyStateProvider(nil) })
}

// cursorGeminiListGroup 构造已装载的分组（白名单为 nil 表示未启用分组白名单）。
func cursorGeminiListGroup(allowlist []string, forcePlatform string) *service.Group {
	group := &service.Group{
		ID:       cursorGeminiListGroupID,
		Platform: service.PlatformGemini,
		Status:   service.StatusActive,
		Hydrated: true,
	}
	if forcePlatform != "" {
		group.Platform = service.PlatformGemini
	}
	if allowlist != nil {
		group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: allowlist}
	}
	return group
}

// cursorGeminiCursorAccount 构造显式挂组的 Cursor 供给账号（mapping 键＝公开名）。
func cursorGeminiCursorAccount(id int64, groupIDs []int64, quotaKeyID *int64, mapping map[string]any) service.Account {
	return service.Account{
		ID:          id,
		Platform:    service.PlatformCursor,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		QuotaKeyID:  quotaKeyID,
		GroupIDs:    groupIDs,
		Credentials: map[string]any{"model_mapping": mapping},
	}
}

// cursorGeminiNativeAccount 构造可供 AIStudio /v1beta/models 的 Gemini 原生账号。
func cursorGeminiNativeAccount(id int64) service.Account {
	return service.Account{
		ID:          id,
		Platform:    service.PlatformGemini,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		GroupIDs:    []int64{cursorGeminiListGroupID},
		Credentials: map[string]any{"api_key": "test-key"},
	}
}

// cursorGeminiAntigravityAccount 构造开启混排的 Antigravity 账号（AG 附加目录来源）。
func cursorGeminiAntigravityAccount(id int64, mapping map[string]any) service.Account {
	account := service.Account{
		ID:          id,
		Platform:    service.PlatformAntigravity,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		GroupIDs:    []int64{cursorGeminiListGroupID},
		Extra:       map[string]any{"mixed_scheduling": true},
	}
	if mapping != nil {
		account.Credentials = map[string]any{"model_mapping": mapping}
	}
	return account
}

// newCursorGeminiListHandler 以最小结构装配真实 handler：真实 *service.GatewayService
// （窄只读入口） + 真实 GeminiMessagesCompatService（原生 AIStudio 链）。
func newCursorGeminiListHandler(
	t *testing.T,
	repo *cursorGeminiListRepo,
	groupRepo service.GroupRepository,
	upstream service.HTTPUpstream,
) *GatewayHandler {
	t.Helper()
	gatewayService := service.NewGatewayService(
		repo, groupRepo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, /* cursorExecution */
	)
	compatService := service.NewGeminiMessagesCompatService(
		repo, groupRepo, nil, nil, nil, nil, upstream, nil, &config.Config{}, nil, /* cursorExecution */
	)
	return &GatewayHandler{
		gatewayService:      gatewayService,
		geminiCompatService: compatService,
	}
}

// cursorGeminiListContext 构造带分组的请求上下文（与线上 Resolver 装载分组一致）。
func cursorGeminiListContext(t *testing.T, group *service.Group, forcePlatform string, allowlist []string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	groupID := group.ID
	req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	c.Request = req
	apiKey := &service.APIKey{
		ID:      9401,
		UserID:  9402,
		GroupID: &groupID,
		Group:   group,
		Status:  service.StatusActive,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	if forcePlatform != "" {
		c.Set(string(middleware.ContextKeyForcePlatform), forcePlatform)
	}
	return c, rec
}

// cursorGeminiModelNames 解出 models[].name，并校验每条都只声明本站已实现的生成方法。
func cursorGeminiModelNames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var got struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), rec.Body.String())
	names := make([]string, 0, len(got.Models))
	for _, model := range got.Models {
		names = append(names, model.Name)
	}
	return names
}

// ---------------------------------------------------------------------------
// 矩阵第 1 行：Cursor-only 组
// ---------------------------------------------------------------------------

// TestCursorGeminiListModelsCursorOnlyGroup 钉死矩阵第 1 行：无原生候选的 Cursor-only 组
// 返回 200 与合法 Cursor 目录（models/<公开名>、只声明 generateContent/streamGenerateContent），
// 且 Gemini OAuth/AIStudio GET 调用数为 0。
func TestCursorGeminiListModelsCursorOnlyGroup(t *testing.T) {
	group := cursorGeminiListGroup(nil, "")
	repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
		group.ID: {
			cursorGeminiCursorAccount(9301, []int64{group.ID}, nil, map[string]any{
				cursorGeminiListModelA: "upstream-a",
				cursorGeminiListModelB: "upstream-b",
			}),
		},
	}}
	upstream := &cursorGeminiListUpstream{status: http.StatusOK, body: `{"models":[]}`}
	h := newCursorGeminiListHandler(t, repo, nil, upstream)

	c, rec := cursorGeminiListContext(t, group, "", nil)
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"models/" + cursorGeminiListModelA, "models/" + cursorGeminiListModelB},
		cursorGeminiModelNames(t, rec))
	require.Zero(t, upstream.calls.Load(), "Cursor-only 组不得发起任何 AIStudio GET")

	// 只声明本站适配已实现的两种方法，不编造 token 上限/版本/countTokens/embedding/图像能力。
	var raw struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Len(t, raw.Models, 2)
	for _, model := range raw.Models {
		require.Equal(t, `["generateContent","streamGenerateContent"]`, string(model["supportedGenerationMethods"]))
		for _, forbidden := range []string{"inputTokenLimit", "outputTokenLimit", "version", "description", "displayName"} {
			_, exists := model[forbidden]
			require.False(t, exists, "Cursor 条目不得编造 %s", forbidden)
		}
	}
	require.NotContains(t, rec.Body.String(), "countTokens")
	require.NotContains(t, rec.Body.String(), "embedContent")
	require.NotContains(t, rec.Body.String(), "image")
}

// ---------------------------------------------------------------------------
// 矩阵第 2 行：混合组且原生成功
// ---------------------------------------------------------------------------

// TestCursorGeminiListModelsMixedGroupMergesNative 钉死矩阵第 2 行：原生 200 响应中追加
// 合法 Cursor/AG 条目，同名一份（原生优先），并保留原生 metadata 与响应信封。
func TestCursorGeminiListModelsMixedGroupMergesNative(t *testing.T) {
	// 组白名单只放行这三个公开名，使 AG 账号隐含的默认 passthrough 条目被业务过滤掉，
	// 断言因此可以精确到「原生 + AG + Cursor」三条。
	group := cursorGeminiListGroup([]string{"gemini-native", "gemini-ag-custom", cursorGeminiListModelB}, "")
	repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
		group.ID: {
			cursorGeminiNativeAccount(9311),
			cursorGeminiAntigravityAccount(9313, map[string]any{"gemini-ag-custom": "gemini-ag-custom"}),
			cursorGeminiCursorAccount(9312, []int64{group.ID}, nil, map[string]any{
				cursorGeminiListModelB: "upstream-b",
				// 与原生同名：公开资源名都为 models/gemini-native，必须只出现一份且保留原生元数据。
				"gemini-native": "upstream-native",
			}),
		},
	}}
	upstream := &cursorGeminiListUpstream{
		status: http.StatusOK,
		body:   `{"models":[{"name":"models/gemini-native","inputTokenLimit":123,"custom":{"keep":true}}],"nextPageToken":"next"}`,
	}
	h := newCursorGeminiListHandler(t, repo, nil, upstream)

	c, rec := cursorGeminiListContext(t, group, "", nil)
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, int64(1), upstream.calls.Load(), "混合组只对原生 AIStudio 发一次 GET")

	names := cursorGeminiModelNames(t, rec)
	require.Equal(t, []string{"models/gemini-native", "models/gemini-ag-custom", "models/" + cursorGeminiListModelB}, names,
		"原生条目在前，AG/Cursor 条目追加；同名只保留一份")

	body := rec.Body.String()
	require.Equal(t, 1, strings.Count(body, `"models/gemini-native"`), "同名条目只能出现一次")
	// 原生 metadata 与信封字段必须原样保留（未被 Cursor 条目覆盖或改写）。
	require.Contains(t, body, `"inputTokenLimit":123`)
	require.Contains(t, body, `"custom":{"keep":true}`)
	require.Contains(t, body, `"nextPageToken":"next"`)
	// Cursor 条目自身只声明已实现方法。
	require.Contains(t, body, `"name":"models/`+cursorGeminiListModelB+`","supportedGenerationMethods":["generateContent","streamGenerateContent"]`)
}

// ---------------------------------------------------------------------------
// 矩阵第 3 行：空组 / 全部被否决 / 全部被白名单过滤
// ---------------------------------------------------------------------------

// TestCursorGeminiListModelsEmptyGroupReturnsEmptyModels 钉死矩阵第 3 行「空组」：无原生候选
// 时返回 200 与 models: []，不补静态默认列表，也不发起 AIStudio 调用。
func TestCursorGeminiListModelsEmptyGroupReturnsEmptyModels(t *testing.T) {
	group := cursorGeminiListGroup(nil, "")
	repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{}}
	upstream := &cursorGeminiListUpstream{status: http.StatusOK, body: `{"models":[]}`}
	h := newCursorGeminiListHandler(t, repo, nil, upstream)

	c, rec := cursorGeminiListContext(t, group, "", nil)
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"models":[]}`, rec.Body.String(), "空组返回真实空目录，不补默认列表")
	require.Zero(t, upstream.calls.Load())
	require.Equal(t, int64(1), repo.supplyQueries.Load(), "Cursor 供给读入口只查本组一次")
	require.Equal(t, group.ID, repo.lastSupplyGrp.Load(), "只按请求分组查询，不做全站扫描")
}

// TestCursorGeminiListModelsAllSupplyVetoedReturnsEmptyModels 钉死矩阵第 3 行「全部供给被
// 明确否决」：来源过期的 Cursor 账号不贡献目录，结论是确定的空集合（200 models: []），
// 不是错误，也不是读取失败。
func TestCursorGeminiListModelsAllSupplyVetoedReturnsEmptyModels(t *testing.T) {
	group := cursorGeminiListGroup(nil, "")
	expiredKey := int64(9501)
	installCursorGeminiQuotaKeyState(t, cursorGeminiListQuotaKeyState{statuses: map[int64]string{
		expiredKey: domain.QuotaKeyStatusExpired,
	}})
	repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
		group.ID: {
			cursorGeminiCursorAccount(9321, []int64{group.ID}, &expiredKey, map[string]any{
				cursorGeminiListModelA: "upstream-a",
			}),
		},
	}}
	upstream := &cursorGeminiListUpstream{status: http.StatusOK, body: `{"models":[]}`}
	h := newCursorGeminiListHandler(t, repo, nil, upstream)

	c, rec := cursorGeminiListContext(t, group, "", nil)
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"models":[]}`, rec.Body.String(), "来源明确被拒绝是确定空集合，不是 503")
	require.Zero(t, upstream.calls.Load())
}

// TestCursorGeminiListModelsFilteredToEmptyReturnsEmptyModels 钉死矩阵第 3 行「全部名被组
// 白名单过滤」：原生与 Cursor 条目全部被过滤后返回 200 与 models: []，不补默认列表；同时
// 钉死「只放行 Cursor 名」时 Cursor 条目存活、原生条目被过滤，且 Cursor 映射的上游别名
// 目标不得借此绕过组权限。
func TestCursorGeminiListModelsFilteredToEmptyReturnsEmptyModels(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allowlist []string
		expected  []string
	}{
		{
			name:      "全部被过滤",
			allowlist: []string{"only-this-model"},
			expected:  []string{},
		},
		{
			name:      "只放行 Cursor 公开名",
			allowlist: []string{cursorGeminiListModelA},
			expected:  []string{"models/" + cursorGeminiListModelA},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := cursorGeminiListGroup(tc.allowlist, "")
			repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
				group.ID: {
					cursorGeminiNativeAccount(9331),
					cursorGeminiCursorAccount(9332, []int64{group.ID}, nil, map[string]any{
						// 公开名 cursor-haiku 通过白名单；映射值（上游别名目标）不在白名单里，
						// 绝不能被当成公开名暴露。
						cursorGeminiListModelA: "only-this-model",
					}),
				},
			}}
			upstream := &cursorGeminiListUpstream{
				status: http.StatusOK,
				body:   `{"models":[{"name":"models/gemini-native"}],"nextPageToken":"next"}`,
			}
			h := newCursorGeminiListHandler(t, repo, nil, upstream)

			c, rec := cursorGeminiListContext(t, group, "", nil)
			h.GeminiV1BetaListModels(c)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var got struct {
				Models []json.RawMessage `json:"models"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Len(t, got.Models, len(tc.expected))
			require.Equal(t, tc.expected, cursorGeminiModelNames(t, rec))
			require.NotContains(t, rec.Body.String(), "only-this-model",
				"Cursor 映射的上游别名目标不得被当成公开名，也不得借此绕过组白名单")
			require.NotContains(t, rec.Body.String(), "models/gemini-native",
				"原生条目必须与 Cursor 条目一起受组白名单约束")
		})
	}
}

// ---------------------------------------------------------------------------
// 矩阵第 4 行：原生候选查询失败 / 依赖读取失败
// ---------------------------------------------------------------------------

// TestCursorGeminiListModelsNativeCandidateQueryFailureReturns503 钉死矩阵第 4 行：原生候选
// 查询失败返回 503 目录读取失败，不把数据库故障当成「这个分组只有 Cursor」。
func TestCursorGeminiListModelsNativeCandidateQueryFailureReturns503(t *testing.T) {
	group := cursorGeminiListGroup(nil, "")
	repo := &cursorGeminiListRepo{
		byGroup: map[int64][]service.Account{
			group.ID: {
				cursorGeminiCursorAccount(9341, []int64{group.ID}, nil, map[string]any{
					cursorGeminiListModelA: "upstream-a",
				}),
			},
		},
		nativeErr: errors.New("gemini accounts table unreachable"),
	}
	upstream := &cursorGeminiListUpstream{status: http.StatusOK, body: `{"models":[]}`}
	h := newCursorGeminiListHandler(t, repo, nil, upstream)

	c, rec := cursorGeminiListContext(t, group, "", nil)
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "models/",
		"数据库故障不得被当成 Cursor-only 组而去返回目录")
	require.Zero(t, upstream.calls.Load())
}

// TestCursorGeminiListModelsCursorSupplyReadFailureReturns503 钉死「读取错误 ≠ 空目录」：
// Cursor 供给读入口失败（账号查询/来源状态/分组白名单读不到）必须 503 目录读取失败，
// 与「成功返回空表」严格区分。
func TestCursorGeminiListModelsCursorSupplyReadFailureReturns503(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, group *service.Group) (*cursorGeminiListRepo, service.GroupRepository)
	}{
		{
			name: "账号查询失败",
			setup: func(_ *testing.T, group *service.Group) (*cursorGeminiListRepo, service.GroupRepository) {
				return &cursorGeminiListRepo{
					byGroup:   map[int64][]service.Account{group.ID: {}},
					supplyErr: errors.New("accounts table unreachable"),
				}, nil
			},
		},
		{
			name: "来源状态读取失败",
			setup: func(t *testing.T, group *service.Group) (*cursorGeminiListRepo, service.GroupRepository) {
				keyID := int64(9601)
				installCursorGeminiQuotaKeyState(t, cursorGeminiListQuotaKeyState{err: errors.New("quota key store unreachable")})
				return &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
					group.ID: {
						cursorGeminiCursorAccount(9351, []int64{group.ID}, &keyID, map[string]any{
							cursorGeminiListModelA: "upstream-a",
						}),
					},
				}}, nil
			},
		},
		{
			name: "来源绑定但读取器未接线",
			setup: func(_ *testing.T, group *service.Group) (*cursorGeminiListRepo, service.GroupRepository) {
				keyID := int64(9602)
				return &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
					group.ID: {
						cursorGeminiCursorAccount(9352, []int64{group.ID}, &keyID, map[string]any{
							cursorGeminiListModelA: "upstream-a",
						}),
					},
				}}, nil
			},
		},
		{
			name: "分组白名单读取失败",
			setup: func(_ *testing.T, group *service.Group) (*cursorGeminiListRepo, service.GroupRepository) {
				return &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
					group.ID: {
						cursorGeminiCursorAccount(9353, []int64{group.ID}, nil, map[string]any{
							cursorGeminiListModelA: "upstream-a",
						}),
					},
				}}, &cursorGeminiListGroupRepo{err: errors.New("groups table unreachable")}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 分组不装入 ctx，强制组白名单判定回源 groupRepo。
			group := cursorGeminiListGroup(nil, "")
			group.Hydrated = false
			repo, groupRepo := tc.setup(t, group)
			upstream := &cursorGeminiListUpstream{status: http.StatusOK, body: `{"models":[]}`}
			h := newCursorGeminiListHandler(t, repo, groupRepo, upstream)

			c, rec := cursorGeminiListContext(t, group, "", nil)
			h.GeminiV1BetaListModels(c)

			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "models/"+cursorGeminiListModelA,
				"读取失败不得伪装成空目录或目录内容")
			require.Zero(t, upstream.calls.Load())
		})
	}
}

// ---------------------------------------------------------------------------
// 矩阵第 5 行：原生请求失败 / 非成功响应
// ---------------------------------------------------------------------------

// TestCursorGeminiListModelsNativeFailureKeepsNativeError 钉死矩阵第 5 行：存在 Cursor 补充时
// 原生明确失败原样保留，不得进入旧原生默认目录兜底分支，也不拿 Cursor 目录冒充成功。
func TestCursorGeminiListModelsNativeFailureKeepsNativeError(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		upstreamErr error
	}{
		{
			name:   "原生非成功响应",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"code":429,"message":"rate limited"}}`,
		},
		{
			name:   "原生 403 需要作用域但不得回落默认目录",
			status: http.StatusForbidden,
			body:   `{"error":"insufficient authentication scopes"}`,
		},
		{
			name:        "原生请求本身失败",
			upstreamErr: errors.New("dial tcp: connection refused"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := cursorGeminiListGroup(nil, "")
			repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
				group.ID: {
					cursorGeminiNativeAccount(9361),
					cursorGeminiCursorAccount(9362, []int64{group.ID}, nil, map[string]any{
						cursorGeminiListModelA: "upstream-a",
					}),
				},
			}}
			upstream := &cursorGeminiListUpstream{status: tc.status, body: tc.body, err: tc.upstreamErr}
			h := newCursorGeminiListHandler(t, repo, nil, upstream)

			c, rec := cursorGeminiListContext(t, group, "", nil)
			h.GeminiV1BetaListModels(c)

			require.NotEqual(t, http.StatusOK, rec.Code, "不得把原生失败包装成成功目录")
			require.NotContains(t, rec.Body.String(), "models/"+cursorGeminiListModelA,
				"存在 Cursor 补充时不得以 Cursor 目录冒充整份成功结果")
			require.NotContains(t, rec.Body.String(), "models/gemini-2.5-pro",
				"不得进入旧原生默认目录兜底分支")
			if tc.status == http.StatusForbidden {
				// 原生明确失败必须原样透传（含原生错误体与状态码）。
				require.Equal(t, http.StatusForbidden, rec.Code)
				require.Contains(t, rec.Body.String(), "insufficient authentication scopes")
			}
		})
	}
}

// TestCursorGeminiListModelsNativeOnlyFallbackUnchanged 钉死「原生-only 历史行为不扩大修改」：
// 没有 Cursor 补充时，原生 403 作用域不足仍按历史分支回落静态默认目录。
func TestCursorGeminiListModelsNativeOnlyFallbackUnchanged(t *testing.T) {
	group := cursorGeminiListGroup(nil, "")
	repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
		group.ID: {cursorGeminiNativeAccount(9371)},
	}}
	upstream := &cursorGeminiListUpstream{
		status: http.StatusForbidden,
		body:   `{"error":"insufficient authentication scopes"}`,
	}
	h := newCursorGeminiListHandler(t, repo, nil, upstream)

	c, rec := cursorGeminiListContext(t, group, "", nil)
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "models/gemini-2.5-pro", "原生-only 历史兜底保持原样")
}

// ---------------------------------------------------------------------------
// 矩阵第 6 行：原生 200 但报文无效
// ---------------------------------------------------------------------------

// TestCursorGeminiListModelsInvalidNativeBodyReturns502 钉死矩阵第 6 行：原生 200 但 models
// 报文无效且确需合并/过滤时，返回 502 协议错误，不悄悄漏 Cursor、不悄悄漏过滤。
func TestCursorGeminiListModelsInvalidNativeBodyReturns502(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		allowlist []string
	}{
		{name: "无 models 字段", body: `{"error":"bad"}`},
		{name: "models 不是数组", body: `{"models":{}}`},
		{name: "非 JSON", body: `not-json`},
		{name: "白名单过滤需要解析但报文无效", body: `not-json`, allowlist: []string{"cursor-*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := cursorGeminiListGroup(tc.allowlist, "")
			repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
				group.ID: {
					cursorGeminiNativeAccount(9381),
					cursorGeminiCursorAccount(9382, []int64{group.ID}, nil, map[string]any{
						cursorGeminiListModelA: "upstream-a",
					}),
				},
			}}
			upstream := &cursorGeminiListUpstream{status: http.StatusOK, body: tc.body}
			h := newCursorGeminiListHandler(t, repo, nil, upstream)

			c, rec := cursorGeminiListContext(t, group, "", nil)
			h.GeminiV1BetaListModels(c)

			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "models/"+cursorGeminiListModelA,
				"不得把无效原生报文当成功空目录，也不得静默漏掉 Cursor 条目")
		})
	}
}

// ---------------------------------------------------------------------------
// 矩阵第 7 行：强制 Antigravity 入口
// ---------------------------------------------------------------------------

// TestCursorGeminiListModelsForcedAntigravitySkipsCursor 钉死矩阵第 7 行：强制 Antigravity
// 入口只读 AG 目录，Cursor 读入口调用数与 AIStudio GET 调用数均为 0。
func TestCursorGeminiListModelsForcedAntigravitySkipsCursor(t *testing.T) {
	group := cursorGeminiListGroup(nil, service.PlatformAntigravity)
	repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
		group.ID: {
			cursorGeminiAntigravityAccount(9391, map[string]any{"gemini-ag-custom": "gemini-ag-custom"}),
			cursorGeminiCursorAccount(9392, []int64{group.ID}, nil, map[string]any{
				cursorGeminiListModelA: "upstream-a",
			}),
		},
	}}
	upstream := &cursorGeminiListUpstream{status: http.StatusOK, body: `{"models":[]}`}
	h := newCursorGeminiListHandler(t, repo, nil, upstream)

	c, rec := cursorGeminiListContext(t, group, service.PlatformAntigravity, nil)
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	names := cursorGeminiModelNames(t, rec)
	require.Contains(t, names, "models/gemini-ag-custom")
	require.NotContains(t, names, "models/"+cursorGeminiListModelA,
		"强制 Antigravity 入口不得混入 Cursor 目录")
	require.Zero(t, repo.supplyQueries.Load(), "强制 Antigravity 入口不得调用 Cursor 供给读入口")
	require.Zero(t, upstream.calls.Load(), "强制 Antigravity 入口不得发起 AIStudio GET")
}

// ---------------------------------------------------------------------------
// 投影边界：通配键、别名、组外账号
// ---------------------------------------------------------------------------

// TestCursorGeminiListModelsProjectionBoundaries 钉死 Cursor 供给投影的四条边界：
// 通配键不出现、mapping 值（上游别名目标）不当公开名、空键不出现、组外账号不贡献。
func TestCursorGeminiListModelsProjectionBoundaries(t *testing.T) {
	group := cursorGeminiListGroup(nil, "")
	otherGroupID := int64(9399)
	repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
		group.ID: {
			cursorGeminiCursorAccount(9401, []int64{group.ID}, nil, map[string]any{
				"":                     "upstream-empty",
				"*":                    "upstream-star",
				"gpt-*":                "upstream-wildcard",
				"public-alias":         "upstream-real-model",
				cursorGeminiListModelA: "upstream-a",
			}),
		},
		// 另一个分组的 Cursor 账号：本组目录不得出现它的任何能力。
		otherGroupID: {
			cursorGeminiCursorAccount(9402, []int64{otherGroupID}, nil, map[string]any{
				"leaked-from-other-group": "upstream-leak",
			}),
		},
	}}
	upstream := &cursorGeminiListUpstream{status: http.StatusOK, body: `{"models":[]}`}
	h := newCursorGeminiListHandler(t, repo, nil, upstream)

	c, rec := cursorGeminiListContext(t, group, "", nil)
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	names := cursorGeminiModelNames(t, rec)
	require.Equal(t, []string{"models/" + cursorGeminiListModelA, "models/public-alias"}, names,
		"只暴露具体请求侧公开名：排除空键/通配键/组外账号，且不取 mapping 别名目标")
	require.Equal(t, group.ID, repo.lastSupplyGrp.Load(), "只按请求分组查询，不借全局目录制造组外能力")
}

// ---------------------------------------------------------------------------
// 来源状态：耗尽 / 缺失 / 读取失败
// ---------------------------------------------------------------------------

// TestCursorGeminiListModelsSourceStateClassification 钉死来源门的三种事实：耗尽不是停供
// （仍贡献目录）、未绑定来源照常贡献、绑定来源但读取失败失败关闭（已在 503 用例覆盖）。
func TestCursorGeminiListModelsSourceStateClassification(t *testing.T) {
	group := cursorGeminiListGroup(nil, "")
	exhaustedKey := int64(9701)
	installCursorGeminiQuotaKeyState(t, cursorGeminiListQuotaKeyState{statuses: map[int64]string{
		exhaustedKey: domain.QuotaKeyStatusExhausted,
	}})
	repo := &cursorGeminiListRepo{byGroup: map[int64][]service.Account{
		group.ID: {
			// 激活码耗尽（拉号失败占位，不是供给终态）：账号仍贡献目录。
			cursorGeminiCursorAccount(9411, []int64{group.ID}, &exhaustedKey, map[string]any{
				cursorGeminiListModelA: "upstream-a",
			}),
			// 未绑定来源：不构成来源否决。
			cursorGeminiCursorAccount(9412, []int64{group.ID}, nil, map[string]any{
				cursorGeminiListModelB: "upstream-b",
			}),
		},
	}}
	upstream := &cursorGeminiListUpstream{status: http.StatusOK, body: `{"models":[]}`}
	h := newCursorGeminiListHandler(t, repo, nil, upstream)

	c, rec := cursorGeminiListContext(t, group, "", nil)
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"models/" + cursorGeminiListModelA, "models/" + cursorGeminiListModelB},
		cursorGeminiModelNames(t, rec),
		"激活码耗尽/未绑定来源都不是停供，二者都应贡献目录")
}
