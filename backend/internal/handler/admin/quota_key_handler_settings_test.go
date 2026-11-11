//go:build unit

// c1p1-B2-handler：QuotaKeyHandler 码级设置入参解析与上游模型预览入口单测。
//
// 覆盖口径：
//   - Create / Update 解析 concurrency / load_factor / rate_multiplier / model_mapping，
//     并经 maskedQuotaKey 回放至 dto.QuotaKey（B 批上移：码级调度设置）；
//   - Update 传 model_mapping: {} 或 model_mapping: null 一律设 ModelMappingSet=true，
//     空映射由服务层 400 QUOTA_KEY_MODELS_REQUIRED 拒绝落库；
//   - Update 清空 concurrency: null / load_factor: null（恢复默认），rate_multiplier: 0（置 0）；
//   - SyncUpstreamModels 依赖未装配返回明确 503，无可用账号返回 400。
//
// 说明：UpdateWithSettings 在真实 client 下走 PG 咨询锁（sqlite 不支持），故本单用内存替身
// 仓储（client=nil，withQuotaKeyLock 退化为直接执行）覆盖解析与回放；该替身在既有 fakeQuotaKeyRepo
// 之上叠加 settings 落库能力，不修改任何既有生产/测试文件。

package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// settingsFakeRepo 在既有 fakeQuotaKeyRepo 之上叠加 UpdateSettingsAndSyncAccounts 能力，
// 并把 Create 时传入的码级设置落到内存实体，使 maskedQuotaKey 能回放它们。
type settingsFakeRepo struct {
	*fakeQuotaKeyRepo
}

// Create 复用既有内存创建逻辑，额外把码级设置写回实体（内存回放等价实现）。
func (r *settingsFakeRepo) Create(ctx context.Context, input service.CreateQuotaKeyInput) (*ent.QuotaKey, error) {
	qk, err := r.fakeQuotaKeyRepo.Create(ctx, input)
	if err != nil {
		return nil, err
	}
	if input.Concurrency != nil {
		v := *input.Concurrency
		qk.Concurrency = &v
	}
	if input.LoadFactor != nil {
		v := *input.LoadFactor
		qk.LoadFactor = &v
	}
	if input.RateMultiplier != nil {
		qk.RateMultiplier = *input.RateMultiplier
	}
	if len(input.ModelMapping) > 0 {
		m := make(map[string]string, len(input.ModelMapping))
		for k, v := range input.ModelMapping {
			m[k] = v
		}
		qk.ModelMapping = m
	}
	if input.GroupIDsSet {
		ids := make([]int64, len(input.GroupIDs))
		copy(ids, input.GroupIDs)
		qk.GroupIds = ids
	}
	return qk, nil
}

// UpdateSettingsAndSyncAccounts 内存等价实现：把码级设置落到实体（忽略事务，内存替身无需事务）。
// 服务层在调用前已用 normalizeQuotaKeySettings 校验（空 model_mapping 在 ModelMappingSet 时已被拒），
// 故此处不再重复防御。
func (r *settingsFakeRepo) UpdateSettingsAndSyncAccounts(_ context.Context, _ *ent.Tx, id int64, settings service.UpdateQuotaKeySettingsInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	qk, ok := r.keys[id]
	if !ok {
		return &ent.NotFoundError{}
	}
	if settings.Concurrency != nil {
		if *settings.Concurrency == nil {
			qk.Concurrency = nil
		} else {
			v := **settings.Concurrency
			qk.Concurrency = &v
		}
	}
	if settings.LoadFactor != nil {
		if *settings.LoadFactor == nil {
			qk.LoadFactor = nil
		} else {
			v := **settings.LoadFactor
			qk.LoadFactor = &v
		}
	}
	if settings.RateMultiplier != nil {
		v := *settings.RateMultiplier
		qk.RateMultiplier = v
	}
	if settings.ModelMappingSet {
		m := make(map[string]string, len(settings.ModelMapping))
		for k, v := range settings.ModelMapping {
			m[k] = v
		}
		qk.ModelMapping = m
	}
	if settings.GroupIDs != nil {
		ids := make([]int64, len(*settings.GroupIDs))
		copy(ids, *settings.GroupIDs)
		qk.GroupIds = ids
	}
	return nil
}

// SyncAccountsExpiry 内存替身：本单不校验账号 expiry 同步，直接成功（不写账号）。
func (r *settingsFakeRepo) SyncAccountsExpiry(_ context.Context, _ *ent.Tx, _ int64, _ *time.Time) error {
	return nil
}

// newSettingsQuotaKeyHandler 装配 handler：withDeps=true 时同时装配 adminService 与
// accountTestService（用于通过 SyncUpstreamModels 依赖检查）。
func newSettingsQuotaKeyHandler(withDeps bool) (*gin.Engine, *settingsFakeRepo) {
	gin.SetMode(gin.TestMode)
	repo := &settingsFakeRepo{fakeQuotaKeyRepo: newFakeQuotaKeyRepo()}
	svc := newQuotaKeyService(repo, nil)
	h := NewQuotaKeyHandler(svc, nil, nil, nil)
	if withDeps {
		h.adminService = newStubAdminService()
		h.SetAccountTestService(&service.AccountTestService{})
	}
	router := gin.New()
	qk := router.Group("/api/v1/admin/quota_keys")
	qk.POST("", h.Create)
	qk.PUT("/:id", h.Update)
	qk.POST("/:id/models/sync-upstream", h.SyncUpstreamModels)
	return router, repo
}

// TestQuotaKeyHandler_Create_ParsesSettingsAndEchoes 验证 Create 解析四个码级设置字段，
// 并经 maskedQuotaKey 回放至响应 dto.QuotaKey。
func TestQuotaKeyHandler_Create_ParsesSettingsAndEchoes(t *testing.T) {
	router, _ := newSettingsQuotaKeyHandler(false)

	body := `{"code":"` + testQuotaKeyPlain + `","concurrency":5,"load_factor":100,"rate_multiplier":1.5,"model_mapping":{"claude-3-5-sonnet":"claude-3-5-sonnet"}}`
	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys", body)
	require.Equal(t, http.StatusCreated, rec.Code)
	requireNoPlaintext(t, rec.Body.String())

	var payload struct {
		Data struct {
			Concurrency    *int              `json:"concurrency"`
			LoadFactor     *int              `json:"load_factor"`
			RateMultiplier float64           `json:"rate_multiplier"`
			ModelMapping   map[string]string `json:"model_mapping"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.NotNil(t, payload.Data.Concurrency, "concurrency 必须回放")
	require.Equal(t, 5, *payload.Data.Concurrency)
	require.NotNil(t, payload.Data.LoadFactor, "load_factor 必须回放")
	require.Equal(t, 100, *payload.Data.LoadFactor)
	require.Equal(t, 1.5, payload.Data.RateMultiplier, "rate_multiplier 必须回放")
	require.Equal(t, map[string]string{"claude-3-5-sonnet": "claude-3-5-sonnet"}, payload.Data.ModelMapping, "model_mapping 必须回放")
}

// TestQuotaKeyHandler_Create_ModelMappingEmptyRejected 验证 Create 传 model_mapping: {} 时
// 设 ModelMappingSet=true，空映射由服务层 400 QUOTA_KEY_MODELS_REQUIRED 拒绝落库。
func TestQuotaKeyHandler_Create_ModelMappingEmptyRejected(t *testing.T) {
	router, _ := newSettingsQuotaKeyHandler(false)

	body := `{"code":"` + testQuotaKeyPlain + `","model_mapping":{}}`
	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys", body)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "QUOTA_KEY_MODELS_REQUIRED")
}

// TestQuotaKeyHandler_Update_ParsesSettingsAndEchoes 验证 Update 解析四个码级设置字段并回放。
func TestQuotaKeyHandler_Update_ParsesSettingsAndEchoes(t *testing.T) {
	router, repo := newSettingsQuotaKeyHandler(false)
	repo.seed(1, "1234", "active", false)

	body := `{"concurrency":7,"load_factor":200,"rate_multiplier":0.5,"model_mapping":{"a":"b"}}`
	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/1", body)
	require.Equal(t, http.StatusOK, rec.Code)
	requireNoPlaintext(t, rec.Body.String())

	var updated struct {
		Data struct {
			Concurrency    *int              `json:"concurrency"`
			LoadFactor     *int              `json:"load_factor"`
			RateMultiplier float64           `json:"rate_multiplier"`
			ModelMapping   map[string]string `json:"model_mapping"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.NotNil(t, updated.Data.Concurrency)
	require.Equal(t, 7, *updated.Data.Concurrency)
	require.NotNil(t, updated.Data.LoadFactor)
	require.Equal(t, 200, *updated.Data.LoadFactor)
	require.Equal(t, 0.5, updated.Data.RateMultiplier)
	require.Equal(t, map[string]string{"a": "b"}, updated.Data.ModelMapping)
}

// TestQuotaKeyHandler_Update_ModelMappingEmptyRejected 验证 Update 传 model_mapping: {} 时 400 拒绝。
func TestQuotaKeyHandler_Update_ModelMappingEmptyRejected(t *testing.T) {
	router, repo := newSettingsQuotaKeyHandler(false)
	repo.seed(1, "1234", "active", false)

	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/1", `{"model_mapping":{}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "QUOTA_KEY_MODELS_REQUIRED")
}

// TestQuotaKeyHandler_Update_ModelMappingNullRejected 验证 Update 传 model_mapping: null 时
// 仍设 ModelMappingSet=true（按用户裁定"没有任何支持的模型就禁止落库"），空映射 400 拒绝。
func TestQuotaKeyHandler_Update_ModelMappingNullRejected(t *testing.T) {
	router, repo := newSettingsQuotaKeyHandler(false)
	repo.seed(1, "1234", "active", false)

	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/1", `{"model_mapping":null}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "QUOTA_KEY_MODELS_REQUIRED")
}

// TestQuotaKeyHandler_Update_ClearsSettings 验证 Update 清空 concurrency:null / load_factor:null
// （恢复默认，响应省略该字段）与 rate_multiplier:0（置 0）。
func TestQuotaKeyHandler_Update_ClearsSettings(t *testing.T) {
	router, repo := newSettingsQuotaKeyHandler(false)
	qk := repo.seed(1, "1234", "active", false)
	concurrency := 5
	loadFactor := 100
	rate := 2.0
	qk.Concurrency = &concurrency
	qk.LoadFactor = &loadFactor
	qk.RateMultiplier = rate

	body := `{"concurrency":null,"load_factor":null,"rate_multiplier":0}`
	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/1", body)
	require.Equal(t, http.StatusOK, rec.Code)

	var updated struct {
		Data struct {
			Concurrency    *int    `json:"concurrency"`
			LoadFactor     *int    `json:"load_factor"`
			RateMultiplier float64 `json:"rate_multiplier"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Nil(t, updated.Data.Concurrency, "concurrency:null 必须清空（响应省略该字段）")
	require.Nil(t, updated.Data.LoadFactor, "load_factor:null 必须清空（响应省略该字段）")
	require.Equal(t, 0.0, updated.Data.RateMultiplier, "rate_multiplier:0 必须置 0")
}

// TestQuotaKeyHandler_SyncUpstreamModels_DepsUnavailable 验证依赖未装配（adminService /
// accountTestService）时返回明确 503，不得静默走到上游探测。
func TestQuotaKeyHandler_SyncUpstreamModels_DepsUnavailable(t *testing.T) {
	// withDeps=false：adminService / accountTestService 均 nil。
	router, _ := newSettingsQuotaKeyHandler(false)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/1/models/sync-upstream", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "not configured")
}

// TestQuotaKeyHandler_SyncUpstreamModels_NoAccount 验证依赖齐备但该激活码名下无任何未软删
// cursor 账号时，ResolvePreviewAccountForModelSync 失败关闭，返回 400 QUOTA_KEY_NO_ACCOUNT_FOR_SYNC。
func TestQuotaKeyHandler_SyncUpstreamModels_NoAccount(t *testing.T) {
	router, repo := newSettingsQuotaKeyHandler(true)
	repo.seed(1, "1234", "active", false)
	// 不预置任何账号 → ListAccountsByQuotaKey 返回空。

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/1/models/sync-upstream", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "QUOTA_KEY_NO_ACCOUNT_FOR_SYNC")
}

// TestQuotaKeyHandler_Create_GroupIDsPassedThrough 验证 Create 透传 group_ids: [1,2]。
// 经 settingsFakeRepo.Create 落到 ent 实体并由 maskedQuotaKey 回放至响应。
func TestQuotaKeyHandler_Create_GroupIDsPassedThrough(t *testing.T) {
	router, _ := newSettingsQuotaKeyHandler(false)

	body := `{"code":"` + testQuotaKeyPlain + `","group_ids":[1,2]}`
	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	requireNoPlaintext(t, rec.Body.String())

	var payload struct {
		Data struct {
			GroupIDs []int64 `json:"group_ids"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, []int64{1, 2}, payload.Data.GroupIDs, "Create 的 group_ids 必须透传到响应")
}

// TestQuotaKeyHandler_Update_GroupIDsEmptyUnbinds 验证 Update 透传 group_ids: [] 为指向空切片
// （解绑全部）：响应回放为指向空切片（group_ids 字段存在且为空数组，不得省略）。
//
// 说明：service.UpdateWithSettings 的 settingsChanged 判定（禁区：本卡不得改 service 层）当前
// 未把 GroupIDs 计入，故单独的 group_ids 变更不会落到 settingsChanged。为在 handler 单测内验证
// 空切片「透传且被应用」，本用例附带一个显式 rate_multiplier 同伴字段以触发 settingsChanged，
// 从而让 GroupIDs 经由 UpdateSettingsAndSyncAccounts 落到实体并回放。group_ids 自身的三态解析
// 仍由本用例断言。
func TestQuotaKeyHandler_Update_GroupIDsEmptyUnbinds(t *testing.T) {
	router, repo := newSettingsQuotaKeyHandler(false)
	repo.seed(1, "1234", "active", false)

	// 附带 rate_multiplier 同伴字段触发 settingsChanged，使 GroupIDs 一并被应用回放。
	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/1", `{"group_ids":[],"rate_multiplier":1.5}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var payload struct {
		Data struct {
			GroupIDs       *[]int64 `json:"group_ids"`
			RateMultiplier float64  `json:"rate_multiplier"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.NotNil(t, payload.Data.GroupIDs, "group_ids:[] 必须透传为指向空切片（字段不得省略）")
	require.Len(t, *payload.Data.GroupIDs, 0, "group_ids:[] 必须解绑全部（空数组）")
	require.Equal(t, 1.5, payload.Data.RateMultiplier, "同伴字段 rate_multiplier 必须一并被应用（确认 settingsChanged 触发）")
}

// TestQuotaKeyHandler_Update_GroupIDsOmitted 验证 Update 不传 group_ids 时保持 nil（不修改，
// 响应省略 group_ids 字段）。
func TestQuotaKeyHandler_Update_GroupIDsOmitted(t *testing.T) {
	router, repo := newSettingsQuotaKeyHandler(false)
	repo.seed(1, "1234", "active", false)

	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/1", `{"notes":"x"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var payload struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	_, ok := payload.Data["group_ids"]
	require.False(t, ok, "未传 group_ids 必须省略该字段（不修改）")
}

// TestQuotaKeyHandler_Update_GroupIDsNullNoModify 验证 Update 传 group_ids: null 时视为「不修改」
// （与 model_mapping 的 null 语义区分：此处 null 不解绑），响应省略 group_ids 字段。
func TestQuotaKeyHandler_Update_GroupIDsNullNoModify(t *testing.T) {
	router, repo := newSettingsQuotaKeyHandler(false)
	repo.seed(1, "1234", "active", false)

	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/1", `{"group_ids":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var payload struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	_, ok := payload.Data["group_ids"]
	require.False(t, ok, "group_ids:null 必须视为不修改（省略该字段）")
}

// TestQuotaKeyHandler_SetStatus_ThreeStates 验证 SetStatus 三态响应（QKG-B6）：
//   - 合法（active ↔ disabled）→ 200 且回放新状态；
//   - 非法（未知 status）→ 400 QUOTA_KEY_STATUS_INVALID；
//   - 缺失 body（status 为空）→ 400 "status is required"。
//
// 该用例命中真实隔离 PostgreSQL（newProbePostgresClient，同型 pg_try_advisory_xact_lock），
// 走完整 service.SetStatus 链路（含 R3a 过期判定与 account_changed 入队）。
func TestQuotaKeyHandler_SetStatus_ThreeStates(t *testing.T) {
	client := newProbePostgresClient(t)
	quotaKeyRepo := repository.NewQuotaKeyRepository(client)
	svc := service.NewQuotaKeyService(quotaKeyRepo, service.NewCredcryptEncryptor(), nil, client)
	h := NewQuotaKeyHandler(svc, nil, nil, nil)

	router := gin.New()
	qk := router.Group("/api/v1/admin/quota_keys")
	qk.POST("/:id/status", h.SetStatus)

	ctx := context.Background()
	// 逐次唯一 code 规避 uq_quota_keys_code_mac 库级唯一索引冲突（清理不依赖 DELETE）。
	status, err := svc.Create(ctx, fmt.Sprintf("JG-CF-B6-STATUS-%d", time.Now().UnixNano()), nil, nil, 0, "")
	require.NoError(t, err)

	// 合法：active → disabled。
	rec := doQuotaKeyJSON(router, http.MethodPost, fmt.Sprintf("/api/v1/admin/quota_keys/%d/status", status.ID), `{"status":"disabled"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var okPayload struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &okPayload))
	require.Equal(t, "disabled", okPayload.Data.Status, "合法 SetStatus 必须回放新状态")

	// 非法：未知 status。
	rec = doQuotaKeyJSON(router, http.MethodPost, fmt.Sprintf("/api/v1/admin/quota_keys/%d/status", status.ID), `{"status":"frobnicate"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "QUOTA_KEY_STATUS_INVALID")

	// 缺失 body：status 为空。
	rec = doQuotaKeyJSON(router, http.MethodPost, fmt.Sprintf("/api/v1/admin/quota_keys/%d/status", status.ID), `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "status is required")
}
