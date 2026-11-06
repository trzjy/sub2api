package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/cursorupstream"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// CARD-02：Auto 卡密生命周期管理与批量导入事务闭环的定向测试。
//
// 覆盖 Codex 外审抓出的三项缺陷：
//
//  1. P1 防止孤儿绑定 —— 本地落库必须先于上游设备绑定：
//     落库失败时上游**一次都不能被调用**（否则卡密在上游已被绑定却无本地记录，
//     即孤儿绑定），且失败卡密不得进入内存池；单卡失败不影响同批其它卡密。
//  2. P2 状态保留刷新 —— RefreshAutoPool 走 pool.UpdateCardCounts，
//     冷却中的卡密刷新额度后 CooldownUntil 必须原样保留（整卡重建会把它抹掉）。
//  3. P3 安全配置 —— 上游基址只从环境配置读取：未配置 / 非安全端点一律失败关闭，
//     绝不回落到硬编码的明文公网地址。
//
// 本文件自带仓储替身与 HTTP 辅助（不依赖同包其它测试文件），可独立编译执行。

// ---------------------------------------------------------------------------
// quota_keys 仓储替身
// ---------------------------------------------------------------------------

// autoCardQuotaKeyRepo 是 service.QuotaKeyRepository 的测试替身。
//
// 嵌入接口字段：未被本文件显式实现的方法在调用时直接 panic —— 只实现本用例
// 实际走到的仓储方法，任何意外的仓储调用都会立刻暴露而不是静默返回零值。
type autoCardQuotaKeyRepo struct {
	service.QuotaKeyRepository

	mu        sync.Mutex
	keys      map[int64]*ent.QuotaKey
	nextID    int64
	failLast4 map[string]bool // 明文末 4 位 → 落库失败（模拟 DB 抖动/唯一键冲突）

	// failUpdateMetaAll 为真时 UpdateMeta 一律失败（模拟 DB 完全不可写）。
	failUpdateMetaAll bool
	// failUpdateStatusFields 为真时 UpdateStatusFields 失败（模拟失败态无法持久化）。
	failUpdateStatusFields bool
	// failGetByID 为真时 GetByID 失败（模拟刷新成功后无法复核权威状态）。
	// 只影响 GetStatus 路径；事务内 GetByIDInTx 不受影响，与生产两种读入口一致。
	failGetByID bool
	// failUpdateMetaExpireTimes 控制「带 expires_at 的 UpdateMeta 调用」的失败次数：
	// 0 = 不失败；-1 = 一直失败；n>0 = 前 n 次失败（覆盖瞬时 DB 抖动）。
	// 只写 notes 的调用不受它影响（模拟「转可选写入被 DB 拒绝、但标记写入可用」）。
	failUpdateMetaExpireTimes int
	// updateMetaCalls 记录 UpdateMeta 的总调用次数。
	updateMetaCalls int
	// updateMetaExpireCalls 记录带 expires_at 的 UpdateMeta 调用次数（校验重试次数）。
	updateMetaExpireCalls int
	// updateMetaNotesOnlyCalls 记录只带 notes（expires_at 省略）的 UpdateMeta 调用次数，
	// 用于确认「待人工中和」标记写入确实发生过。
	updateMetaNotesOnlyCalls int
	// markExpiredOnExpireWrite 为真时，带 expires_at 的 UpdateMeta 成功后会额外把行
	// 状态改成 expired —— 模拟生产清扫器在「落库 → 转可选」窗口内抢先把该行
	// 标成 expired（MarkExpiredIfStillExpired）的竞态。
	markExpiredOnExpireWrite bool
	// createdRows 记录每次 Create 的落库快照（ID + 落库瞬间的 expires_at）。
	// 用于断言「行从落库那一刻起就不可选」（而非事后中和出来的）。
	createdRows []autoCardCreatedRow
}

// autoCardCreatedRow 是 Create 落库瞬间的行快照。
type autoCardCreatedRow struct {
	ID        int64
	ExpiresAt *time.Time
	Notes     *string
}

func newAutoCardQuotaKeyRepo() *autoCardQuotaKeyRepo {
	return &autoCardQuotaKeyRepo{
		keys:      map[int64]*ent.QuotaKey{},
		failLast4: map[string]bool{},
	}
}

func (r *autoCardQuotaKeyRepo) Create(_ context.Context, input service.CreateQuotaKeyInput) (*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failLast4[input.CodeLast4] {
		return nil, fmt.Errorf("quota_keys: write failed for last4=%s", input.CodeLast4)
	}
	r.nextID++
	qk := &ent.QuotaKey{
		ID:            r.nextID,
		CodeEncrypted: input.CodeEncrypted,
		CodeMAC:       input.CodeMAC,
		CodeLast4:     input.CodeLast4,
		Status:        input.Status,
		Exhausted:     input.Exhausted,
		ExpiresAt:     input.ExpiresAt,
		Notes:         input.Notes,
		Priority:      input.Priority,
	}
	r.keys[qk.ID] = qk
	// 落库瞬间快照：用于断言「行从出生起就不可选」（expires_at 落在过去），
	// 而不是事后靠中和写入把它改成不可选。
	var createdExpires *time.Time
	if qk.ExpiresAt != nil {
		v := *qk.ExpiresAt
		createdExpires = &v
	}
	r.createdRows = append(r.createdRows, autoCardCreatedRow{
		ID:        qk.ID,
		ExpiresAt: createdExpires,
		Notes:     qk.Notes,
	})
	return qk, nil
}

func (r *autoCardQuotaKeyRepo) GetByID(_ context.Context, id int64) (*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failGetByID {
		return nil, fmt.Errorf("quota_keys: get by id failed")
	}
	qk, ok := r.keys[id]
	if !ok {
		return nil, &ent.NotFoundError{}
	}
	return qk, nil
}

// GetByIDInTx 满足 svc.Update 的观测窄接口断言（替身装配下 tx 为 nil）。
// 刻意不复用 GetByID：生产里事务内读与 GetStatus 读是两个入口，failGetByID 只模拟后者。
func (r *autoCardQuotaKeyRepo) GetByIDInTx(_ context.Context, _ *ent.Tx, id int64) (*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	qk, ok := r.keys[id]
	if !ok {
		return nil, &ent.NotFoundError{}
	}
	return qk, nil
}

// List 满足 QuotaKeyRepository.List：返回替身内所有持久化激活码，供 RefreshAutoPool
// 以「持久化行」为准全量枚举 Auto 卡密（修复单卡刷新失败移出池后无法恢复入池的 P1）。
// 仅返回本用例实际用到的字段（ID/Notes/Status/ExpiresAt/Priority）。
func (r *autoCardQuotaKeyRepo) List(_ context.Context) ([]*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*ent.QuotaKey, 0, len(r.keys))
	for _, qk := range r.keys {
		out = append(out, qk)
	}
	return out, nil
}

// UpdateMeta 复刻生产仓储的双指针语义：外层 nil = 不写；内层 nil = 显式清空；
// 否则写入内层指向的值（与 quotaKeyRepository.UpdateMeta 一致）。
//
// 失败注入语义：
//   - failUpdateMetaAll：整个 UpdateMeta 调用失败（生产里是一条 SQL，要么成要么败，
//     故此处整体失败、不写任何列）；
//   - failUpdateMetaExpire：仅「带 expires_at 的调用」失败（模拟该列写入被 DB 拒绝），
//     只写 notes 的调用仍成功。
func (r *autoCardQuotaKeyRepo) UpdateMeta(_ context.Context, _ *ent.Tx, id int64, expiresAt **time.Time, notes **string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	qk, ok := r.keys[id]
	if !ok {
		return &ent.NotFoundError{}
	}
	r.updateMetaCalls++
	if expiresAt != nil {
		r.updateMetaExpireCalls++
	} else if notes != nil {
		r.updateMetaNotesOnlyCalls++
	}
	if r.failUpdateMetaAll {
		return fmt.Errorf("quota_keys: update meta failed")
	}
	// 仅「带 expires_at 的调用」失败：模拟该列写入被 DB 拒绝，
	// 而只写 notes 的调用仍可用（生产里是一条 SQL，这里按列区分以覆盖
	// 「转可选写入失败但标记写入可用」的分支）。
	if expiresAt != nil && r.failUpdateMetaExpireTimes != 0 {
		if r.failUpdateMetaExpireTimes < 0 {
			return fmt.Errorf("quota_keys: update meta (expires_at) failed")
		}
		r.failUpdateMetaExpireTimes--
		return fmt.Errorf("quota_keys: update meta (expires_at) failed")
	}
	if expiresAt != nil {
		if *expiresAt == nil {
			qk.ExpiresAt = nil
		} else {
			v := **expiresAt
			qk.ExpiresAt = &v
		}
		// 模拟清扫器竞态：写 expires_at 的同时把行标成 expired。
		if r.markExpiredOnExpireWrite {
			qk.Status = domain.QuotaKeyStatusExpired
		}
	}
	if notes != nil {
		if *notes == nil {
			qk.Notes = nil
		} else {
			v := **notes
			qk.Notes = &v
		}
	}
	return nil
}

// UpdateUsageSnapshot / UpdateCredentialsAtomically 仅用于满足观测窄接口断言，本用例不触发。
func (r *autoCardQuotaKeyRepo) UpdateUsageSnapshot(context.Context, *ent.Tx, int64, []byte, **time.Time) error {
	return nil
}

// UpdateStatusFields 复刻生产仓储的同名语义：批量导入阶段 4 写回快照成功后，服务层
// 会在同一每 key 事务内按原 status/exhausted 清空 last_refresh_error；失败态分支写入
// 错误消息。替身装配下 tx 为 nil，直接更新内存行（不实现的方法保持嵌入接口的 panic
// 语义，避免意外调用被静默吞掉）。
func (r *autoCardQuotaKeyRepo) UpdateStatusFields(_ context.Context, _ *ent.Tx, id int64, status string, exhausted bool, lastError *string, lastRefreshAt *time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	qk, ok := r.keys[id]
	if !ok {
		return &ent.NotFoundError{}
	}
	if r.failUpdateStatusFields {
		return fmt.Errorf("quota_keys: update status fields failed")
	}
	qk.Status = status
	qk.Exhausted = exhausted
	qk.LastRefreshError = lastError
	if lastRefreshAt != nil {
		qk.LastRefreshAt = lastRefreshAt
	}
	return nil
}

func (r *autoCardQuotaKeyRepo) UpdateCredentialsAtomically(context.Context, *ent.Tx, int64, string, string, string, bool) error {
	return nil
}

func (r *autoCardQuotaKeyRepo) UpdatePriority(context.Context, *ent.Tx, int64, int) (int, error) {
	return 0, nil
}

func (r *autoCardQuotaKeyRepo) snapshot() []*ent.QuotaKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*ent.QuotaKey, 0, len(r.keys))
	for _, qk := range r.keys {
		out = append(out, qk)
	}
	return out
}

// ---------------------------------------------------------------------------
// 上游 mock
// ---------------------------------------------------------------------------

// autoCardUpstreamCall 记录一次上游请求。
type autoCardUpstreamCall struct {
	Path string
	Card string
}

// autoCardUpstreamStub 模拟 Cursor Auto 上游服务。
//
// validateActivationCode / refreshCount 是明文 JSON；getCredentialsSecure 是 AES 加密
// （见 internal/pkg/cursorupstream），必须走真实协议才能命中 force_reused 解析分支。
type autoCardUpstreamStub struct {
	server *httptest.Server

	mu    sync.Mutex
	calls []autoCardUpstreamCall

	validateCode int    // validateActivationCode 返回的 code（非 200 即绑定失败）
	validateMsg  string // validateActivationCode 返回的 msg
	overCount    int
	sumCount     int
	forceReused  bool
	// refreshCode 控制 /refreshCount 返回的业务 code（非 200 即客户端判失败）。
	refreshCode int
}

func newAutoCardUpstreamStub(t *testing.T) *autoCardUpstreamStub {
	t.Helper()
	s := &autoCardUpstreamStub{
		validateCode: 200,
		overCount:    7,
		sumCount:     9,
		refreshCode:  200,
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *autoCardUpstreamStub) handle(w http.ResponseWriter, r *http.Request) {
	card := r.URL.Query().Get("device_code")
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/validateActivationCode"):
		s.record(r.URL.Path, "")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": s.validateCode, "msg": s.validateMsg})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/refreshCount"):
		s.record(r.URL.Path, card)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": s.refreshCode,
			"msg":  "ok",
			"data": map[string]any{"over_count": s.overCount, "sum_count": s.sumCount},
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/getCredentialsSecure"):
		s.record(r.URL.Path, card)
		// force_reused=true 会让动态池把该卡密置入 60s 冷却。
		payload := fmt.Sprintf(`{"email":"auto@example.com","token":"tok","force_reused":%v}`, s.forceReused)
		enc, err := cursorupstream.NewCrypto().Encrypt([]byte(payload))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "msg": "ok", "data": enc})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *autoCardUpstreamStub) record(path, card string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, autoCardUpstreamCall{Path: path, Card: card})
}

// callsTo 返回命中指定路径后缀的请求数。
func (s *autoCardUpstreamStub) callsTo(suffix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.HasSuffix(c.Path, suffix) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 路由与依赖装配
// ---------------------------------------------------------------------------

// setupAutoCardRouter 装配 Auto 卡密三个端点的路由。
func setupAutoCardRouter(svc *service.QuotaKeyService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := NewQuotaKeyHandler(svc, nil, nil)
	qk := router.Group("/api/v1/admin/quota_keys/auto")
	qk.POST("/batch_import", h.BatchImportCards)
	qk.POST("/refresh_all", h.RefreshAutoPool)
	qk.GET("/status", h.GetAutoPoolStatus)
	qk.POST("/:id/refresh", h.RefreshAutoCard)
	return router
}

// installAutoCardPool 装配全局动态池并返回清理函数。
func installAutoCardPool(t *testing.T, pool *service.CursorDynamicPool) {
	t.Helper()
	service.SetGlobalCursorDynamicPool(pool)
	t.Cleanup(func() { service.SetGlobalCursorDynamicPool(nil) })
}

// newAutoCardQuotaKeyService 构造使用替身仓储的 QuotaKeyService（client 为 nil：
// svc.Update 的每 key 锁在替身装配下退化为直调，与 admin 包既有测试同口径）。
func newAutoCardQuotaKeyService(repo service.QuotaKeyRepository) *service.QuotaKeyService {
	return service.NewQuotaKeyService(repo, service.NewCredcryptEncryptor(), nil, nil)
}

// doAutoCardJSON 发起一次 admin JSON 请求。
func doAutoCardJSON(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

// autoCardBatchImportResult 是批量导入端点的响应投影。
type autoCardBatchImportResult struct {
	SuccessCount int      `json:"success_count"`
	FailedCount  int      `json:"failed_count"`
	Errors       []string `json:"errors"`
}

func parseAutoCardBatchImport(t *testing.T, rec *httptest.ResponseRecorder) autoCardBatchImportResult {
	t.Helper()
	var body struct {
		Data autoCardBatchImportResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Data
}

// ---------------------------------------------------------------------------
// P3：安全配置（上游基址只来自环境配置，解析口径由 service 层统一覆盖）
// ---------------------------------------------------------------------------

func TestQuotaKeyAuto_BatchImport_UnconfiguredUpstream_FailsClosed(t *testing.T) {
	repo := newAutoCardQuotaKeyRepo()
	installAutoCardPool(t, service.NewCursorDynamicPool("", zap.NewNop()))
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, "")

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001"]}`)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Empty(t, repo.snapshot(), "上游端点不可信时不得落库")
}

// ---------------------------------------------------------------------------
// P1：落库与设备绑定的闭环（无孤儿绑定 + 失败隔离）
// ---------------------------------------------------------------------------

func TestQuotaKeyAuto_BatchImport_PersistFailure_NeverBindsUpstream(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	repo := newAutoCardQuotaKeyRepo()
	repo.failLast4["0001"] = true
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := parseAutoCardBatchImport(t, rec)
	require.Equal(t, 0, res.SuccessCount)
	require.Equal(t, 1, res.FailedCount)
	require.Len(t, res.Errors, 1)
	require.Contains(t, res.Errors[0], "persist failed")
	require.Contains(t, res.Errors[0], "CARD****0001")

	// P1 核心断言：本地落库失败时上游一次都不能被调用。
	// 否则卡密在上游已被绑定却无本地记录 → 孤儿绑定。
	require.Zero(t, upstream.callsTo("/validateActivationCode"),
		"落库失败时不得调用上游绑定接口")
	require.Zero(t, upstream.callsTo("/refreshCount"),
		"落库失败时不得调用上游额度接口")

	// 失败卡密不得污染内存池。
	require.Empty(t, pool.ListCards(), "失败卡密不得进入内存池")
}

// TestQuotaKeyAuto_BatchImport_BindFailure_BornUnselectableAndSkipsPool 锁定 P2 的
// 核心不变量：**绑定失败的行从落库那一刻起就不可选**。
//
// 回归背景（终审 CARD-06 P2）：上轮修复只写 notes 标记 + 错误日志，
// markAutoCardBindFailed 不改 expires_at，行仍 active+未过期 →
// 会被补池候选查询（选 active+未过期）重消费，而它从未成功绑定过上游。
// 终审明确「人工告警不等于关闭」。
//
// 本用例断言落库瞬间（Create 返回时）该行就已是不可选态 —— 不是事后靠某个
// 可能失败的中和写入把它改成不可选的。
func TestQuotaKeyAuto_BatchImport_BindFailure_BornUnselectableAndSkipsPool(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	upstream.validateCode = 500
	upstream.validateMsg = "activation code invalid"
	repo := newAutoCardQuotaKeyRepo()
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := parseAutoCardBatchImport(t, rec)
	require.Equal(t, 0, res.SuccessCount)
	require.Equal(t, 1, res.FailedCount)
	require.Len(t, res.Errors, 1)
	require.Contains(t, res.Errors[0], "bind failed")

	// P2 核心断言：行在**落库瞬间**就已不可选（expires_at 落在过去），
	// 而不是靠后续某个可能失败的中和写入才变成不可选。
	require.Len(t, repo.createdRows, 1)
	created := repo.createdRows[0]
	require.NotNil(t, created.ExpiresAt,
		"落库瞬间就必须带过去时间的 expires_at —— 该行从出生起就不可选")
	require.True(t, created.ExpiresAt.Before(time.Now()),
		"落库瞬间的 expires_at 必须落在过去，否则该行会被补池候选查询选中")
	require.NotNil(t, created.Notes)
	require.Contains(t, *created.Notes, "auto_pool:dev=")
	require.Contains(t, *created.Notes, "md5=")

	// 当前行仍不可选（expires_at 在过去），且带失败标记供运维检索。
	rows := repo.snapshot()
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].ExpiresAt, "绑定失败的行必须退出补池候选集")
	require.True(t, rows[0].ExpiresAt.Before(time.Now()), "失败行的 expires_at 必须落在过去")
	require.NotNil(t, rows[0].Notes)
	require.Contains(t, *rows[0].Notes, "auto_pool:dev=")
	require.Contains(t, *rows[0].Notes, "md5=")
	require.Contains(t, *rows[0].Notes, autoPoolBindFailedMarker)

	// 失败卡密不得污染内存池。
	require.Empty(t, pool.ListCards(), "绑定失败的卡密不得进入内存池")
}

// TestQuotaKeyAuto_BatchImport_BindFailure_MarkerWriteFails_StillUnselectable 锁定
// P2 的最坏分支：绑定失败**且**失败标记也写不进去时，该行仍必须不可选。
//
// 这是 P2 相对上轮修复的根本改进：上轮把「行不可选」寄托在一次可能失败的
// 中和写入上，中和失败就留下 active+未过期的行（会被重消费）；现在行从落库起
// 就不可选，任何后续写入失败都改变不了这一点。
func TestQuotaKeyAuto_BatchImport_BindFailure_MarkerWriteFails_StillUnselectable(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	upstream.validateCode = 500
	upstream.validateMsg = "activation code invalid"
	repo := newAutoCardQuotaKeyRepo()
	repo.failUpdateMetaAll = true // DB 完全不可写
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := parseAutoCardBatchImport(t, rec)
	require.Equal(t, 0, res.SuccessCount)
	require.Equal(t, 1, res.FailedCount)
	require.Len(t, res.Errors, 1)
	require.Contains(t, res.Errors[0], "bind failed")

	// 标记写失败必须如实上报（运维据此知道这行缺少 bind=failed 信号）。
	require.Contains(t, res.Errors[0], "failed-marker NOT persisted")

	// P2 核心断言：即便所有后续写入都失败，该行仍不可选 ——
	// 因为它从落库那一刻起就带着过去时间的 expires_at。
	rows := repo.snapshot()
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].ExpiresAt,
		"标记写失败不得让该行回到 active+未过期的可重消费状态")
	require.True(t, rows[0].ExpiresAt.Before(time.Now()),
		"标记写失败时该行的 expires_at 仍必须落在过去（不可选）")

	// 旧的软性措辞必须已被移除 —— 它正是让失败静默滑过的根源。
	require.NotContains(t, res.Errors[0], "left active for manual recovery")
	require.NotContains(t, res.Errors[0], "left ACTIVE and will be re-consumed")

	require.Empty(t, pool.ListCards(), "绑定失败的卡密不得进入内存池")
}

// TestQuotaKeyAuto_BatchImport_BindSuccess_ActivateFails_StaysUnselectable 锁定
// P2 阶段 3 的失败语义：绑定成功但「转可选」失败时，该行必须留在不可选安全态
// （不会被重消费），且按**硬失败**上报并标注需人工处理。
func TestQuotaKeyAuto_BatchImport_BindSuccess_ActivateFails_StaysUnselectable(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	repo := newAutoCardQuotaKeyRepo()
	// 带 expires_at 的写（= 转可选那一步）一律失败；只写 notes 的调用仍可用。
	repo.failUpdateMetaExpireTimes = -1
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := parseAutoCardBatchImport(t, rec)
	require.Equal(t, 0, res.SuccessCount)
	require.Equal(t, 1, res.FailedCount)
	require.Len(t, res.Errors, 1)

	// 硬失败信号：已绑定且已查额、但未转可选，需人工处理。
	require.Contains(t, res.Errors[0], "已绑定已查额但未转可选")
	require.Contains(t, res.Errors[0], "需人工处理")

	// P2 核心断言：转可选失败后该行仍不可选（expires_at 仍在过去），
	// 不会被补池候选查询重新消费。
	rows := repo.snapshot()
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].ExpiresAt)
	require.True(t, rows[0].ExpiresAt.Before(time.Now()),
		"转可选失败后该行的 expires_at 必须仍落在过去（不可选安全态）")

	// 转可选失败 ⇒ 不入池。
	require.Empty(t, pool.ListCards(), "转可选失败的卡密不得进入内存池")
}

// TestQuotaKeyAuto_BatchImport_BindSuccess_SweeperRace_NotSelectable 锁定 P2 阶段 3
// 的清扫器竞态防护：转可选写成功但清扫器已把该行标成 expired 时，必须按硬失败
// 上报（行确实不可选），绝不能谎报成功。
func TestQuotaKeyAuto_BatchImport_BindSuccess_SweeperRace_NotSelectable(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	repo := newAutoCardQuotaKeyRepo()
	repo.markExpiredOnExpireWrite = true // 模拟清扫器抢先标 expired
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := parseAutoCardBatchImport(t, rec)
	require.Equal(t, 0, res.SuccessCount, "清扫器竞态下不得谎报成功")
	require.Equal(t, 1, res.FailedCount)
	require.Len(t, res.Errors, 1)
	require.Contains(t, res.Errors[0], "已绑定已查额但未转可选")
	require.Contains(t, res.Errors[0], "需人工处理")

	rows := repo.snapshot()
	require.Len(t, rows, 1)
	require.Equal(t, domain.QuotaKeyStatusExpired, rows[0].Status,
		"清扫器已把该行标成 expired —— 该行确实不可选")

	require.Empty(t, pool.ListCards(), "不可选的卡密不得进入内存池")
}

// TestQuotaKeyAuto_BatchImport_BindFailure_NoExpiryWriteAfterCreate 锁定 P2 的
// 结构性改进：绑定失败路径**不再有任何「中和」写入**。
//
// 回归背景：上轮修复在绑定失败后调用 neutralize 把 expires_at 置为过去，
// 该写入是「行不可选」的唯一依据 —— 它失败就留下 active+未过期的可重消费行。
// 现在行从落库起就不可选，中和步骤被彻底移除，故失败路径上不应再有
// 任何带 expires_at 的写入（只有一次只写 notes 的失败标记写入）。
func TestQuotaKeyAuto_BatchImport_BindFailure_NoExpiryWriteAfterCreate(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	upstream.validateCode = 500
	upstream.validateMsg = "activation code invalid"
	repo := newAutoCardQuotaKeyRepo()
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := parseAutoCardBatchImport(t, rec)
	require.Equal(t, 1, res.FailedCount)

	// 核心断言：失败路径上没有任何带 expires_at 的写入 —— 「中和」步骤已移除。
	require.Zero(t, repo.updateMetaExpireCalls,
		"绑定失败路径不得再有中和写入（行从落库起就不可选）")
	// 只有一次「只写 notes」的失败标记写入。
	require.Equal(t, 1, repo.updateMetaNotesOnlyCalls,
		"绑定失败应写一次 bind=failed 标记（notes only）")
	require.Equal(t, 1, repo.updateMetaCalls)

	require.Empty(t, pool.ListCards(), "绑定失败的卡密不得进入内存池")
}

// TestQuotaKeyAuto_BatchImport_Success_BornUnselectableThenActivated 锁定 P2 的完整
// 状态迁移：成功路径上的行同样是「落库即不可选 → 绑成功后转可选」。
//
// 这条不变量对成功路径同样重要：Create 与 ValidateActivationCode 之间存在时间窗，
// 若行在落库时即可选，它会在「尚未绑定上游」时被补池候选查询消费。
func TestQuotaKeyAuto_BatchImport_Success_BornUnselectableThenActivated(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	repo := newAutoCardQuotaKeyRepo()
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := parseAutoCardBatchImport(t, rec)
	require.Equal(t, 1, res.SuccessCount)
	require.Equal(t, 0, res.FailedCount)

	// 落库瞬间：不可选（expires_at 在过去）。
	require.Len(t, repo.createdRows, 1)
	require.NotNil(t, repo.createdRows[0].ExpiresAt)
	require.True(t, repo.createdRows[0].ExpiresAt.Before(time.Now()),
		"落库瞬间该行必须不可选，否则「落库→绑定」窗口内会被候选查询消费")

	// 绑定成功后：转可选（expires_at 被清空），行回到候选集。
	rows := repo.snapshot()
	require.Len(t, rows, 1)
	require.Nil(t, rows[0].ExpiresAt, "绑定成功后该行必须转可选（expires_at 清空）")
	require.Equal(t, domain.QuotaKeyStatusActive, rows[0].Status)

	// 转可选这一步恰好发生一次。
	require.Equal(t, 1, repo.updateMetaExpireCalls)

	require.Len(t, pool.ListCards(), 1)
}

func TestQuotaKeyAuto_BatchImport_Success_PersistsThenBindsThenRegistersInPool(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	repo := newAutoCardQuotaKeyRepo()
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001","  ","CARD-KEY-0002"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := parseAutoCardBatchImport(t, rec)
	require.Equal(t, 2, res.SuccessCount)
	require.Equal(t, 0, res.FailedCount)
	require.Empty(t, res.Errors)

	// 两张卡都完成「落库 → 上游绑定 → 入池」。
	require.Equal(t, 2, upstream.callsTo("/validateActivationCode"))
	require.Equal(t, 2, upstream.callsTo("/refreshCount"))

	cards := pool.ListCards()
	require.Len(t, cards, 2)
	for _, card := range cards {
		require.NotEmpty(t, card.DeviceCode)
		require.NotEmpty(t, card.DeviceCodeMD5)
		require.Equal(t, 7, card.OverCount)
		require.Equal(t, 9, card.SumCount)
	}

	// 本地行落库时即记录完整绑定凭据（dev + md5），绑定关系可复原。
	rows := repo.snapshot()
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.NotNil(t, row.Notes)
		require.Contains(t, *row.Notes, "auto_pool:dev=")
		require.Contains(t, *row.Notes, "md5=")
		require.NotContains(t, *row.Notes, autoPoolBindFailedMarker)
		// P2：绑定成功后必须转可选 —— expires_at 被清空（永不过期），
		// 该行重新进入补池候选集。
		require.Nil(t, row.ExpiresAt,
			"绑定成功转可选后 expires_at 必须被清空，该行重新可选")
		require.Equal(t, domain.QuotaKeyStatusActive, row.Status)
	}
}

func TestQuotaKeyAuto_BatchImport_FailureIsolation_OneCardFailsOthersSucceed(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	repo := newAutoCardQuotaKeyRepo()
	repo.failLast4["0001"] = true
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001","CARD-KEY-0002"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := parseAutoCardBatchImport(t, rec)
	require.Equal(t, 1, res.SuccessCount, "同批其它卡密必须照常导入")
	require.Equal(t, 1, res.FailedCount)
	require.Len(t, res.Errors, 1)
	require.Contains(t, res.Errors[0], "persist failed")

	// 只有绑定成功的卡密进入内存池。
	cards := pool.ListCards()
	require.Len(t, cards, 1)
	require.Equal(t, "CARD-KEY-0002", cards[0].CardKey, "入池的应是成功落库的第二张卡")
	require.Equal(t, 1, upstream.callsTo("/validateActivationCode"),
		"落库失败的卡不得触发上游绑定")
}

// ---------------------------------------------------------------------------
// P2：刷新额度必须保留单卡状态（CooldownUntil / Disabled）
// ---------------------------------------------------------------------------

func TestQuotaKeyAuto_RefreshAutoPool_UpdatesCountsButPreservesCooldownState(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	upstream.forceReused = true
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	// 先让卡密进入冷却：上游返回 force_reused → 动态池把该卡置入 60s 冷却。
	pool.RegisterCard(1, "CARD-KEY-0001", "dev-code-1", "dev-md5-1", 5, 10)
	_, err := pool.AcquireToken(context.Background(), "")
	require.Error(t, err, "上游 force_reused 时取号应当失败")

	before := pool.ListCards()
	require.Len(t, before, 1)
	require.True(t, before[0].CooldownUntil.After(time.Now()), "前置条件：卡密应处于冷却中")

	// 刷新额度：只允许更新 OverCount/SumCount。
	// 号池内的卡密在生产中必有对应 quota_keys 行；刷新成功后要回写快照，
	// 故替身需预置同一 ID 的行，否则会被「快照落库失败」判为刷新失败。
	repo := newAutoCardQuotaKeyRepo()
	repo.keys[1] = &ent.QuotaKey{
		ID:        1,
		CodeLast4: "0001",
		Status:    domain.QuotaKeyStatusActive,
		Notes:     strPtr(formatAutoPoolNotes("dev-code-1", "dev-md5-1")),
	}
	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/refresh_all", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Data struct {
			Refreshed int `json:"refreshed_count"`
			Failed    int `json:"failed_count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.Equal(t, 1, body.Data.Refreshed)
	require.Equal(t, 0, body.Data.Failed)

	after := pool.ListCards()
	require.Len(t, after, 1)
	require.Equal(t, upstream.overCount, after[0].OverCount, "额度必须被刷新")
	require.Equal(t, upstream.sumCount, after[0].SumCount, "总额必须被刷新")
	require.True(t, after[0].CooldownUntil.After(time.Now()),
		"P2：刷新额度不得抹掉单卡的冷却状态（整卡重建会清零 CooldownUntil）")
	require.False(t, after[0].Disabled, "刷新额度不得改变单卡的禁用状态")
	require.Equal(t, "dev-code-1", after[0].DeviceCode, "刷新额度不得改写设备码")
	require.Equal(t, "dev-md5-1", after[0].DeviceCodeMD5, "刷新额度不得改写设备 MD5")
}

func TestQuotaKeyAuto_RefreshAutoPool_UnconfiguredUpstream_FailsClosed(t *testing.T) {
	installAutoCardPool(t, service.NewCursorDynamicPool("", zap.NewNop()))
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, "")

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(newAutoCardQuotaKeyRepo()))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/refresh_all", "")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
}

func TestQuotaKeyAuto_RefreshAutoPool_PoolNotInitialized(t *testing.T) {
	installAutoCardPool(t, nil)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, "https://cursor.example.com")

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(newAutoCardQuotaKeyRepo()))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/refresh_all", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "pool_not_initialized")
}

// ---------------------------------------------------------------------------
// 单卡刷新：失败态持久化与恢复重纳管的运行期/重启期同态保护
// ---------------------------------------------------------------------------

// TestQuotaKeyAuto_RefreshAutoCard_FailurePersistError_KeepsCardInPool 锁定：
// 上游刷新失败但失败态**无法持久化**时，不得单方面把卡撤出运行池 —— 启动回放只拦截
// 已持久化的 last_refresh_error，先撤池后落库失败会让重启把该卡以旧额度重新纳管。
func TestQuotaKeyAuto_RefreshAutoCard_FailurePersistError_KeepsCardInPool(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	upstream.refreshCode = 500
	repo := newAutoCardQuotaKeyRepo()
	repo.keys[1] = &ent.QuotaKey{
		ID:        1,
		CodeLast4: "0001",
		Status:    domain.QuotaKeyStatusActive,
		Notes:     strPtr(formatAutoPoolNotes("dev-code-1", "dev-md5-1")),
	}
	repo.failUpdateStatusFields = true
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	pool.RegisterCard(1, "0001", "dev-code-1", "dev-md5-1", 5, 10)
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/1/refresh", "")

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Len(t, pool.ListCards(), 1,
		"失败态未能持久化时撤池会造成重启回放分叉，运行池必须保持不变")
}

// TestQuotaKeyAuto_RefreshAutoCard_FailureThenSuccess_ReRegisters 锁定完整的
// 「失败 → 撤池并落库失败态 → 下次刷新成功清错 → 重新纳管」状态转换，而不是只验证
// 「空池刷新成功会纳管」。断言覆盖失败态是否真的持久化、卡是否真的被撤池，以及恢复后
// 库内 priority 是否被沿用 —— 否则运行期（池外）与重启期（回放入池）再次分叉。
func TestQuotaKeyAuto_RefreshAutoCard_FailureThenSuccess_ReRegisters(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	repo := newAutoCardQuotaKeyRepo()
	repo.keys[1] = &ent.QuotaKey{
		ID:        1,
		CodeLast4: "0001",
		Status:    domain.QuotaKeyStatusActive,
		Notes:     strPtr(formatAutoPoolNotes("dev-code-1", "dev-md5-1")),
		Priority:  5,
	}
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	pool.RegisterCard(1, "0001", "dev-code-1", "dev-md5-1", 5, 10)
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))

	// 第一步：上游刷新失败 → 失败态落库 + 卡被撤出运行池。
	upstream.refreshCode = 500
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/1/refresh", "")
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.Empty(t, pool.ListCards(), "失败态已持久化，卡必须被移出运行池")
	require.NotNil(t, repo.keys[1].LastRefreshError, "失败态必须落库，供重启回放拦截")

	// 第二步：上游恢复 → 失败态被清除，卡按统一注册路径重新纳管并沿用库内 priority。
	upstream.refreshCode = 200
	rec = doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/1/refresh", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Nil(t, repo.keys[1].LastRefreshError, "刷新成功必须清除持久化的失败态")
	cards := pool.ListCards()
	require.Len(t, cards, 1, "刷新成功后必须重新纳管被移出的卡密")
	require.Equal(t, int64(1), cards[0].ID)
	require.Equal(t, 5, cards[0].Priority, "重新纳管必须沿用库内 priority，不得重置为 0")
	require.Equal(t, upstream.overCount, cards[0].OverCount)
	require.Equal(t, upstream.sumCount, cards[0].SumCount)
}

func strPtr(s string) *string { return &s }

// TestQuotaKeyAuto_RefreshAutoCard_RecheckError_WithdrawsAndFailsClosed 锁定：
// 刷新成功但无法复核权威状态（GetStatus 失败）时，必须撤出运行池并硬失败 ——
// 宁可不服务，也不能让运行期池成员与重启回放依据的 DB 状态分叉。
func TestQuotaKeyAuto_RefreshAutoCard_RecheckError_WithdrawsAndFailsClosed(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	repo := newAutoCardQuotaKeyRepo()
	repo.keys[1] = &ent.QuotaKey{
		ID:        1,
		CodeLast4: "0001",
		Status:    domain.QuotaKeyStatusActive,
		Notes:     strPtr(formatAutoPoolNotes("dev-code-1", "dev-md5-1")),
		Priority:  5,
	}
	// 快照写入走事务内读，仍可成功；复核走 GetStatus（GetByID）并失败。
	repo.failGetByID = true
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	pool.RegisterCard(1, "0001", "dev-code-1", "dev-md5-1", 5, 10)
	installAutoCardPool(t, pool)
	t.Setenv(service.EnvCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/1/refresh", "")

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Empty(t, pool.ListCards(), "无法复核权威状态时必须撤池，不得与重启回放分叉")
}
