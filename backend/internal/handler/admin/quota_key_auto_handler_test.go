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
	return qk, nil
}

func (r *autoCardQuotaKeyRepo) GetByID(_ context.Context, id int64) (*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	qk, ok := r.keys[id]
	if !ok {
		return nil, &ent.NotFoundError{}
	}
	return qk, nil
}

// GetByIDInTx 满足 svc.Update 的观测窄接口断言（替身装配下 tx 为 nil）。
func (r *autoCardQuotaKeyRepo) GetByIDInTx(ctx context.Context, _ *ent.Tx, id int64) (*ent.QuotaKey, error) {
	return r.GetByID(ctx, id)
}

// UpdateMeta 复刻生产仓储的双指针语义：外层 nil = 不写；内层 nil = 显式清空；
// 否则写入内层指向的值（与 quotaKeyRepository.UpdateMeta 一致）。
func (r *autoCardQuotaKeyRepo) UpdateMeta(_ context.Context, _ *ent.Tx, id int64, expiresAt **time.Time, notes **string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	qk, ok := r.keys[id]
	if !ok {
		return &ent.NotFoundError{}
	}
	if expiresAt != nil {
		if *expiresAt == nil {
			qk.ExpiresAt = nil
		} else {
			v := **expiresAt
			qk.ExpiresAt = &v
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
}

func newAutoCardUpstreamStub(t *testing.T) *autoCardUpstreamStub {
	t.Helper()
	s := &autoCardUpstreamStub{
		validateCode: 200,
		overCount:    7,
		sumCount:     9,
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
			"code": 200,
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
// P3：安全配置（上游基址只来自环境配置）
// ---------------------------------------------------------------------------

func TestResolveCursorAutoUpstreamBaseURL_SecurityPolicy(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "未配置一律失败关闭", raw: "", wantErr: true},
		{name: "空白值等价于未配置", raw: "   ", wantErr: true},
		{name: "明文公网地址被拒", raw: "http://82.157.20.83:9098", wantErr: true},
		{name: "明文域名被拒", raw: "http://cursor.example.com", wantErr: true},
		{name: "非 http 协议被拒", raw: "ftp://cursor.example.com", wantErr: true},
		{name: "缺少主机名被拒", raw: "https:///api", wantErr: true},
		{name: "内嵌凭据被拒", raw: "https://user:pass@cursor.example.com", wantErr: true},
		{name: "https 公网端点放行", raw: "https://cursor.example.com:9098", want: "https://cursor.example.com:9098"},
		{name: "https 去掉尾部斜杠", raw: "https://cursor.example.com/", want: "https://cursor.example.com"},
		{name: "回环明文放行（本地联调）", raw: "http://127.0.0.1:9098", want: "http://127.0.0.1:9098"},
		{name: "内网明文放行", raw: "http://10.0.0.5:9098", want: "http://10.0.0.5:9098"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envCursorAutoUpstreamBaseURL, tc.raw)
			got, err := resolveCursorAutoUpstreamBaseURL()
			if tc.wantErr {
				require.Error(t, err, "期望拒绝该基址，实际通过：%q", tc.raw)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err, tc.raw)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestQuotaKeyAuto_BatchImport_UnconfiguredUpstream_FailsClosed(t *testing.T) {
	repo := newAutoCardQuotaKeyRepo()
	installAutoCardPool(t, service.NewCursorDynamicPool("", zap.NewNop()))
	t.Setenv(envCursorAutoUpstreamBaseURL, "")

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
	t.Setenv(envCursorAutoUpstreamBaseURL, upstream.server.URL)

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

func TestQuotaKeyAuto_BatchImport_BindFailure_NeutralizesRowAndSkipsPool(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	upstream.validateCode = 500
	upstream.validateMsg = "activation code invalid"
	repo := newAutoCardQuotaKeyRepo()
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(envCursorAutoUpstreamBaseURL, upstream.server.URL)

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(repo))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/batch_import",
		`{"cards":["CARD-KEY-0001"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := parseAutoCardBatchImport(t, rec)
	require.Equal(t, 0, res.SuccessCount)
	require.Equal(t, 1, res.FailedCount)
	require.Len(t, res.Errors, 1)
	require.Contains(t, res.Errors[0], "bind failed")

	// 绑定失败：本地行保留设备凭据（可人工恢复绑定），但退出补池候选集。
	rows := repo.snapshot()
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Notes)
	require.Contains(t, *rows[0].Notes, "auto_pool:dev=")
	require.Contains(t, *rows[0].Notes, "md5=")
	require.Contains(t, *rows[0].Notes, autoPoolBindFailedMarker)
	require.NotNil(t, rows[0].ExpiresAt, "绑定失败的行必须退出补池候选集")
	require.True(t, rows[0].ExpiresAt.Before(time.Now()), "失败行的 expires_at 必须落在过去")

	// 失败卡密不得污染内存池。
	require.Empty(t, pool.ListCards(), "绑定失败的卡密不得进入内存池")
}

func TestQuotaKeyAuto_BatchImport_Success_PersistsThenBindsThenRegistersInPool(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	repo := newAutoCardQuotaKeyRepo()
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(envCursorAutoUpstreamBaseURL, upstream.server.URL)

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
	}
}

func TestQuotaKeyAuto_BatchImport_FailureIsolation_OneCardFailsOthersSucceed(t *testing.T) {
	upstream := newAutoCardUpstreamStub(t)
	repo := newAutoCardQuotaKeyRepo()
	repo.failLast4["0001"] = true
	pool := service.NewCursorDynamicPool(upstream.server.URL, zap.NewNop())
	installAutoCardPool(t, pool)
	t.Setenv(envCursorAutoUpstreamBaseURL, upstream.server.URL)

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
	t.Setenv(envCursorAutoUpstreamBaseURL, upstream.server.URL)

	// 先让卡密进入冷却：上游返回 force_reused → 动态池把该卡置入 60s 冷却。
	pool.RegisterCard(1, "CARD-KEY-0001", "dev-code-1", "dev-md5-1", 5, 10)
	_, err := pool.AcquireToken(context.Background(), "")
	require.Error(t, err, "上游 force_reused 时取号应当失败")

	before := pool.ListCards()
	require.Len(t, before, 1)
	require.True(t, before[0].CooldownUntil.After(time.Now()), "前置条件：卡密应处于冷却中")

	// 刷新额度：只允许更新 OverCount/SumCount。
	router := setupAutoCardRouter(newAutoCardQuotaKeyService(newAutoCardQuotaKeyRepo()))
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
	t.Setenv(envCursorAutoUpstreamBaseURL, "")

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(newAutoCardQuotaKeyRepo()))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/refresh_all", "")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
}

func TestQuotaKeyAuto_RefreshAutoPool_PoolNotInitialized(t *testing.T) {
	installAutoCardPool(t, nil)
	t.Setenv(envCursorAutoUpstreamBaseURL, "https://cursor.example.com")

	router := setupAutoCardRouter(newAutoCardQuotaKeyService(newAutoCardQuotaKeyRepo()))
	rec := doAutoCardJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/auto/refresh_all", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "pool_not_initialized")
}
