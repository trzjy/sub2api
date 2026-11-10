//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 1. 静态清单格式校验
// ---------------------------------------------------------------------------

func TestValidateCursorModelCatalog(t *testing.T) {
	require.Equal(t, "", ValidateCursorModelCatalog(), "静态清单必须自洽")
}

func TestCursorModelIDSetContainsKnown(t *testing.T) {
	set := CursorModelIDSet()
	require.Contains(t, set, "gpt-4.1")
	require.Contains(t, set, "claude-3-7-sonnet")
	require.Contains(t, set, "gemini-2.5-pro")
}

// ---------------------------------------------------------------------------
// 2. ApplyEntryDefaults：静态 entry 来源 / 入库参数收口（P1-B2）
// ---------------------------------------------------------------------------

// fakeAccountWriter 是 AccountFieldWriter 的测试替身（捕获写入调用，不连库）。
type fakeAccountWriter struct {
	mu    sync.Mutex
	apply []accountApplyCall
}

type accountApplyCall struct {
	id             int64
	priority       int
	rateMultiplier float64
	concurrency    *int
}

func (w *fakeAccountWriter) ApplyEntryDefaults(_ context.Context, _ *ent.Tx, id int64, priority int, rateMultiplier float64, concurrency *int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.apply = append(w.apply, accountApplyCall{id, priority, rateMultiplier, concurrency})
	return nil
}

func autoEntryPtrInt(v int) *int { return &v }

// TestComputeEntryModelsIsStaticIdentity：P1-B2 核心——入库退出"人工勾选定义能力"。
// entry 模型恒等于静态实供清单，既不与人工勾选取交集，也不受厂商开关门控；
// 四种配置形态（全关 / 单勾 / 残留官方名 / nil）必须产出同一份静态清单。
func TestComputeEntryModelsIsStaticIdentity(t *testing.T) {
	static := CursorStaticEntryModels()
	realIDs := make([]string, 0, len(CursorRealSupplyModels))
	for _, e := range CursorRealSupplyModels {
		if e.ID != "" {
			realIDs = append(realIDs, e.ID)
		}
	}
	require.NotEmpty(t, static)
	require.Equal(t, realIDs, static, "静态 entry 清单必须与 CursorRealSupplyModels 恒等（保序、去重、去空）")

	allOff := &CursorAutoEntryConfig{
		Vendors: map[string]bool{CursorVendorOpenAI: false, CursorVendorAnthropic: false, CursorVendorXAI: false},
		Models:  nil,
	}
	oneSelected := &CursorAutoEntryConfig{
		Vendors: map[string]bool{CursorVendorOpenAI: true},
		Models:  []CursorModelRef{{ID: "gpt-6-sol", Vendor: CursorVendorOpenAI}},
	}
	staleOfficialOnly := &CursorAutoEntryConfig{
		Vendors: map[string]bool{CursorVendorOpenAI: true},
		// 旧配置残留官方模型名（不在实供清单）——曾使门控交集为空、入库账号零模型。
		Models: []CursorModelRef{{ID: "gpt-4.1", Vendor: CursorVendorOpenAI}},
	}
	require.Equal(t, static, ComputeEntryModels(allOff), "用户全关不得再把入库账号打成零模型")
	require.Equal(t, static, ComputeEntryModels(oneSelected), "只勾一个模型不得缩小入库模型集合")
	require.Equal(t, static, ComputeEntryModels(staleOfficialOnly), "残留旧官方模型名不得影响入库模型集合")
	require.Equal(t, static, ComputeEntryModels(nil), "nil 配置同样产出静态清单")
}

// TestCursorAccountModelMappingIsIdentityOfStaticEntry：写入 credentials.model_mapping
// 的形态必须是静态清单的恒等映射（模型名 → 自身），且不含任何非实供模型。
func TestCursorAccountModelMappingIsIdentityOfStaticEntry(t *testing.T) {
	static := CursorStaticEntryModels()
	mapping := CursorAccountModelMapping(static)
	require.Len(t, mapping, len(static))
	for _, id := range static {
		require.Equal(t, id, mapping[id], "恒等映射：%s 必须映射到自身", id)
	}
	require.NotContains(t, mapping, "gpt-4.1", "官方清单名（非实供）不得进入恒等映射")
}

// TestApplyEntryDefaultsWritesClosedParams：入库参数三值收口（P1-B2 §16.1）
// priority=1、rate_multiplier=1.0（成本中性）、concurrency 不写列（沿用列默认 3），
// 且不再写 extra（entry_models 无生产消费者，随卡清理）。
func TestApplyEntryDefaultsWritesClosedParams(t *testing.T) {
	w := &fakeAccountWriter{}
	svc := &CursorAutoEntryService{writer: w}

	acc := &ent.Account{ID: 7}
	updated, err := svc.ApplyEntryDefaults(context.Background(), nil, acc, DefaultAutoEntryConfig())
	require.NoError(t, err)
	require.Len(t, w.apply, 1)
	call := w.apply[0]
	require.Equal(t, int64(7), call.id)
	require.Equal(t, CursorEntryPriority, call.priority)
	require.Equal(t, 1, call.priority, "priority 必须收口为 1")
	require.Equal(t, CursorEntryRateMultiplier, call.rateMultiplier)
	require.Equal(t, 1.0, call.rateMultiplier, "rate_multiplier 必须收口为 1.0（账号成本中性）")
	require.Nil(t, call.concurrency, "concurrency 不写列 → 沿用 account 列默认 3")
	require.Equal(t, CursorEntryPriority, updated.Priority)
	require.Equal(t, CursorEntryRateMultiplier, updated.RateMultiplier)
}

// TestApplyEntryDefaultsIgnoresConfiguredParams：配置项不再参与入库参数——
// 即便配置里写了 9 / 2.5 / 9，也必须收口为 1 / 1.0 / nil。
func TestApplyEntryDefaultsIgnoresConfiguredParams(t *testing.T) {
	w := &fakeAccountWriter{}
	svc := &CursorAutoEntryService{writer: w}
	cfg := DefaultAutoEntryConfig()
	cfg.EntryDefaults.Priority = 9
	cfg.EntryDefaults.RateMultiplier = 2.5
	cfg.EntryDefaults.Concurrency = autoEntryPtrInt(9)
	_, err := svc.ApplyEntryDefaults(context.Background(), nil, &ent.Account{ID: 3}, cfg)
	require.NoError(t, err)
	require.Len(t, w.apply, 1)
	require.Equal(t, 1, w.apply[0].priority, "配置 priority=9 不得生效")
	require.Equal(t, 1.0, w.apply[0].rateMultiplier, "配置 rate_multiplier=2.5 不得生效（成本中性）")
	require.Nil(t, w.apply[0].concurrency, "配置 concurrency=9 不得生效（沿用列默认 3）")
}

// TestApplyEntryDefaultsPersistsClosedParams 走生产 ent 写入路径落库复核三值：
// priority=1、rate_multiplier=1.0、concurrency 不写列 → DB 列默认 3，且 extra 无
// entry_models 残留（Done when 3 的落库侧证据，fake writer 只覆盖调用参数）。
func TestApplyEntryDefaultsPersistsClosedParams(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	acc, err := client.Account.Create().
		SetName(fmt.Sprintf("p1b2-closed-params-%d", time.Now().UnixNano())).
		SetPlatform(PlatformCursor).
		SetType("oauth").
		SetQuotaKeyID(qk.ID).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		bg := context.Background()
		_ = client.Account.DeleteOneID(acc.ID).Exec(bg)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(bg)
	})

	svc := &CursorAutoEntryService{writer: &entAccountFieldWriter{client: client}}
	cfg := DefaultAutoEntryConfig()
	cfg.EntryDefaults.Priority = 9
	cfg.EntryDefaults.RateMultiplier = 2.5
	cfg.EntryDefaults.Concurrency = autoEntryPtrInt(9)
	_, err = svc.ApplyEntryDefaults(ctx, nil, acc, cfg)
	require.NoError(t, err)

	got, err := client.Account.Get(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, 1, got.Priority, "落库 priority 必须为 1")
	require.InDelta(t, 1.0, got.RateMultiplier, 1e-9, "落库 rate_multiplier 必须为 1.0（成本中性）")
	require.Equal(t, 3, got.Concurrency, "入库不写 concurrency → 落库为列默认 3")
	require.NotContains(t, got.Extra, "entry_models", "extra 不得再有 entry_models 残留")
}

// ---------------------------------------------------------------------------
// 3. ProbeKeyQuota：来源额度观测的唯一入口（§25.6 + §25.14-C 六步顺序）
// ---------------------------------------------------------------------------

// autoEntrySettingRepo 是 SettingRepository 的测试替身（仅实现本卡需要的读写）。
type autoEntrySettingRepo struct {
	mu   sync.Mutex
	data map[string]string
}

func newFakeSettingRepo() *autoEntrySettingRepo {
	return &autoEntrySettingRepo{data: map[string]string{}}
}

func (r *autoEntrySettingRepo) Get(_ context.Context, key string) (*Setting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.data[key]
	if !ok {
		return nil, ErrSettingNotFound
	}
	return &Setting{Key: key, Value: v}, nil
}

func (r *autoEntrySettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.data[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}

func (r *autoEntrySettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[key] = value
	return nil
}

func (r *autoEntrySettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := r.data[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (r *autoEntrySettingRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range settings {
		r.data[k] = v
	}
	return nil
}

func (r *autoEntrySettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	out := map[string]string{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.data {
		out[k] = v
	}
	return out, nil
}

func (r *autoEntrySettingRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.data, key)
	return nil
}

var deviceIDRe = regexp.MustCompile(`^[0-9A-F]{8}$`)

// observeTxRecord 记录观测事务骨架的提交/回滚结果（断言"失败观测提交后才返错"）。
type observeTxRecord struct {
	mu        sync.Mutex
	calls     int
	commits   int
	rollbacks int
}

func (r *observeTxRecord) snapshot() (calls, commits, rollbacks int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.commits, r.rollbacks
}

// capturingObserveTx 是观测事务骨架的替身：模拟"fn 返回 nil ⇒ 提交，返回错误 ⇒ 回滚"。
func (r *observeTxRecord) tx() func(txCtx, lockCtx context.Context, _ int64, fn func(tx *ent.Tx) error) error {
	return func(_, _ context.Context, _ int64, fn func(tx *ent.Tx) error) error {
		r.mu.Lock()
		r.calls++
		r.mu.Unlock()
		err := fn(nil)
		r.mu.Lock()
		if err == nil {
			r.commits++
		} else {
			r.rollbacks++
		}
		r.mu.Unlock()
		return err
	}
}

// lookupServer 是 lookup 端点的替身：按需回包并记录命中次数/请求头/并发度。
type lookupServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	hits     int
	gotKey   string
	gotDev   string
	gotUA    string
	gotPath  string
	inFlight int
	maxIn    int
}

func newLookupServer(t *testing.T, body func() string) *lookupServer {
	t.Helper()
	ls := &lookupServer{}
	ls.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		ls.hits++
		ls.gotKey = r.Header.Get("key")
		ls.gotDev = r.Header.Get("device-id")
		ls.gotUA = r.Header.Get("User-Agent")
		ls.gotPath = r.URL.Path
		ls.inFlight++
		if ls.inFlight > ls.maxIn {
			ls.maxIn = ls.inFlight
		}
		payload := body()
		ls.mu.Unlock()
		defer func() {
			ls.mu.Lock()
			ls.inFlight--
			ls.mu.Unlock()
		}()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(ls.srv.Close)
	return ls
}

func (l *lookupServer) stats() (hits int, device string, key string, ua string, path string, maxInFlight int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hits, l.gotDev, l.gotKey, l.gotUA, l.gotPath, l.maxIn
}

const probeOKBody = `{"code":0,"msg":"ok","data":{"key_info":{` +
	`"group":"Pro","key_status_code":1,"quota_key_max_quota":400,` +
	`"quota_key_used_quota":12,"device_count":1,"device_max_count":3,` +
	`"mode":1,"expired_at":"2026-12-31T00:00:00Z"}}}`

// newAutoEntryProbeService 构造以内存替身仓储装配的观测服务。
func newAutoEntryProbeService(srv *httptest.Server, repo *fakeQuotaKeyRepo) *CursorAutoEntryService {
	svc := NewCursorAutoEntryService(newFakeSettingRepo(), repo, testEncryptor{}, nil, &http.Client{Timeout: 5 * time.Second}, srv.URL)
	svc.SetWriter(&fakeAccountWriter{})
	return svc
}

// TestProbeKeyQuota_WritesSingleCommittedSnapshot：成功观测只写 quota_keys.usage_snapshot
// 一处（+ 既有 expires_at），不再逐账号复制到 extra。
func TestProbeKeyQuota_WritesSingleCommittedSnapshot(t *testing.T) {
	ls := newLookupServer(t, func() string { return probeOKBody })
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}
	qkID := int64(42)
	repo.accounts = []*ent.Account{{ID: 100, QuotaKeyID: &qkID, Extra: map[string]any{}}, {ID: 101, QuotaKeyID: &qkID, Extra: map[string]any{}}}

	svc := newAutoEntryProbeService(ls.srv, repo)
	rec := &observeTxRecord{}
	svc.SetObserveTx(rec.tx())

	info, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, 400, info.Max)
	require.Equal(t, 12, info.Used)
	require.Zero(t, info.AccountsWritten, "旧复制链已删除：不再逐账号写入")

	hits, device, key, ua, path, _ := ls.stats()
	require.Equal(t, 1, hits)
	require.Equal(t, "/lookup", path)
	require.Equal(t, "JG-CF-TEST", key, "必须使用解密后的明文 code 作为 key 头")
	require.Regexp(t, deviceIDRe, device, "设备身份必须来自三分支解析（8 位大写十六进制）")
	require.NotEmpty(t, ua, "必须携带浏览器 UA")

	// 唯一事实源：quota_keys.usage_snapshot。
	snapshot, ok := ParseQuotaKeyUsageSnapshot(repo.byID[42].UsageSnapshot)
	require.True(t, ok, "成功观测必须写入 quota_keys.usage_snapshot")
	require.NotNil(t, snapshot.Total)
	require.Equal(t, 400, *snapshot.Total)
	require.NotNil(t, snapshot.Used)
	require.Equal(t, 12, *snapshot.Used)
	require.Equal(t, "", snapshot.ErrorCode, "成功观测的 error_code 必须为空")
	require.NotNil(t, snapshot.CheckedAt)
	require.NotNil(t, snapshot.AttemptedAt)

	// 期限只写既有 expires_at。
	require.NotNil(t, repo.byID[42].ExpiresAt)
	require.Equal(t, 2026, repo.byID[42].ExpiresAt.Year())

	// 不再写账号 extra。
	for _, acc := range repo.accounts {
		require.NotContains(t, acc.Extra, CursorKeyQuotaSnapshotExtraKey,
			"来源额度不得再复制进账号 extra（唯一归属是 quota_keys.usage_snapshot）")
	}
	calls, commits, rollbacks := rec.snapshot()
	require.Equal(t, 1, calls)
	require.Equal(t, 1, commits)
	require.Zero(t, rollbacks)
}

// TestProbeKeyQuota_UnusedKeyKeepsExistingExpiry：未使用状态（status_code=0）的激活码，
// 上游把 activated_at 给成"本次查询时刻"、expired_at 给成"查询时刻 + 30 天"（滚动值）。
// 本地已有 expires_at 时不得被滚动值覆盖（含管理员手工填写的日期）。
func TestProbeKeyQuota_UnusedKeyKeepsExistingExpiry(t *testing.T) {
	body := `{"code":0,"msg":"ok","data":{"key_info":{` +
		`"group":"Pro","key_status_code":0,"quota_key_max_quota":30000,` +
		`"quota_key_used_quota":0,"device_count":0,"device_max_count":3,` +
		`"mode":1,"expired_at":"2026-12-03T13:09:24.788Z"}}}`
	ls := newLookupServer(t, func() string { return body })
	repo := newFakeQuotaKeyRepo()
	existing := time.Date(2026, 12, 3, 20, 33, 44, 0, time.Local)
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST", ExpiresAt: &existing}

	svc := newAutoEntryProbeService(ls.srv, repo)
	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)

	require.NotNil(t, repo.byID[42].ExpiresAt)
	require.True(t, existing.Equal(*repo.byID[42].ExpiresAt),
		"未使用状态的滚动期限不得覆盖本地已有值，实际=%v", repo.byID[42].ExpiresAt)
	// 额度快照仍照常写入（只冻结期限维度，不冻结观测）。
	snapshot, ok := ParseQuotaKeyUsageSnapshot(repo.byID[42].UsageSnapshot)
	require.True(t, ok)
	require.Equal(t, 30000, *snapshot.Total)
}

// TestProbeKeyQuota_UnusedKeyAdoptsExpiryOnceWhenEmpty：未使用状态且本地无期限 → 接受一次
// 上游值（把首次观测到的期限固化下来），后续不再滚动。
func TestProbeKeyQuota_UnusedKeyAdoptsExpiryOnceWhenEmpty(t *testing.T) {
	body := `{"code":0,"msg":"ok","data":{"key_info":{` +
		`"group":"Pro","key_status_code":0,"quota_key_max_quota":30000,` +
		`"quota_key_used_quota":0,"device_count":0,"device_max_count":3,` +
		`"mode":1,"expired_at":"2026-12-03T13:09:24.788Z"}}}`
	ls := newLookupServer(t, func() string { return body })
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}

	svc := newAutoEntryProbeService(ls.srv, repo)
	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)

	require.NotNil(t, repo.byID[42].ExpiresAt)
	require.Equal(t, 2026, repo.byID[42].ExpiresAt.UTC().Year())
	require.Equal(t, time.December, repo.byID[42].ExpiresAt.UTC().Month())
}

// TestProbeKeyQuota_InUseKeyOverwritesExpiry：进入使用中（status_code=1）后上游期限已固定，
// 仍是唯一权威，按原语义覆盖本地值。
func TestProbeKeyQuota_InUseKeyOverwritesExpiry(t *testing.T) {
	ls := newLookupServer(t, func() string { return probeOKBody })
	repo := newFakeQuotaKeyRepo()
	stale := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST", ExpiresAt: &stale}

	svc := newAutoEntryProbeService(ls.srv, repo)
	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)

	require.NotNil(t, repo.byID[42].ExpiresAt)
	require.True(t, repo.byID[42].ExpiresAt.After(stale), "使用中状态的上游期限必须覆盖过期的本地值")
	require.Equal(t, 2026, repo.byID[42].ExpiresAt.UTC().Year())
}

// TestProbeKeyQuota_DerivedDeviceForExistingAccounts：存量码（无持久 device、已有账号）
// 复用派生值 deviceID(code)，且**不写新身份**（§25.14-C 设备三分支②）。
func TestProbeKeyQuota_DerivedDeviceForExistingAccounts(t *testing.T) {
	ls := newLookupServer(t, func() string { return probeOKBody })
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}
	qkID := int64(42)
	repo.accounts = []*ent.Account{{ID: 100, QuotaKeyID: &qkID}}

	svc := newAutoEntryProbeService(ls.srv, repo)
	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)

	_, device, _, _, _, _ := ls.stats()
	require.Equal(t, deviceID("JG-CF-TEST"), device, "存量码必须复用既有派生值 deviceID(code)")
	require.Nil(t, repo.byID[42].DeviceID, "存量码不得因观测而写入新设备身份")
}

// TestProbeKeyQuota_FirstWriteDeviceForNewSourceWithoutAccounts：真新码（无 device、无账号）
// 沿既有条件首写落一份稳定值——来源没有账号也必须有额度观测。
func TestProbeKeyQuota_FirstWriteDeviceForNewSourceWithoutAccounts(t *testing.T) {
	ls := newLookupServer(t, func() string { return probeOKBody })
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-NEW-CODE"}

	svc := newAutoEntryProbeService(ls.srv, repo)
	info, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err, "无账号的来源也必须能被观测")
	require.Equal(t, 400, info.Max)

	_, device, _, _, _, _ := ls.stats()
	require.Regexp(t, deviceIDRe, device)
	require.NotNil(t, repo.byID[42].DeviceID, "真新码必须首写设备身份")
	require.Equal(t, device, *repo.byID[42].DeviceID, "首写值与本次使用值必须一致")

	// 第二次观测必须复用同一身份（不产生第二份）。
	_, err = svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)
	_, device2, _, _, _, _ := ls.stats()
	require.Equal(t, device, device2, "同一来源的设备身份必须稳定")
}

// TestProbeKeyQuota_FailureCommitsObservationThenReturnsError：lookup 失败时先提交失败观测，
// 提交之后才把 lookupErr 返回调用者（§25.14-C 步骤 4）。
func TestProbeKeyQuota_FailureCommitsObservationThenReturnsError(t *testing.T) {
	ls := newLookupServer(t, func() string { return `{"code":1,"msg":"每15分钟内仅可刷新1次"}` })
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}

	svc := newAutoEntryProbeService(ls.srv, repo)
	rec := &observeTxRecord{}
	svc.SetObserveTx(rec.tx())

	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.Error(t, err, "lookup 业务失败必须如实返错")
	require.Contains(t, err.Error(), "CURSOR_LOOKUP_CODE")

	_, commits, rollbacks := rec.snapshot()
	require.Zero(t, rollbacks, "失败观测不得走回滚分支")
	require.Equal(t, 1, commits, "失败观测必须随事务提交（回调返回 nil）后才返错")

	snapshot, ok := ParseQuotaKeyUsageSnapshot(repo.byID[42].UsageSnapshot)
	require.True(t, ok, "失败观测必须已落盘")
	require.Equal(t, "CURSOR_LOOKUP_CODE", snapshot.ErrorCode)
	require.NotEmpty(t, snapshot.ErrorCode)
	require.Nil(t, snapshot.Total, "失败不得写 0，也不得伪造总额")
	require.Nil(t, snapshot.Used)
	require.NotNil(t, snapshot.AttemptedAt)
}

// TestProbeKeyQuota_FailureKeepsLastSuccessValues：失败观测保留最近成功值与其时间
// （不写 0、不抹掉历史展示），只更新 attempted_at/error_code。
func TestProbeKeyQuota_FailureKeepsLastSuccessValues(t *testing.T) {
	ls := newLookupServer(t, func() string { return `{"code":1,"msg":"cooldown"}` })
	repo := newFakeQuotaKeyRepo()
	previous := `{"total":400,"used":86,"status_code":200,"checked_at":"2026-10-01T08:00:00Z","attempted_at":"2026-10-01T08:00:00Z","error_code":""}`
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST", UsageSnapshot: []byte(previous)}

	svc := newAutoEntryProbeService(ls.srv, repo)
	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.Error(t, err)

	snapshot, ok := ParseQuotaKeyUsageSnapshot(repo.byID[42].UsageSnapshot)
	require.True(t, ok)
	require.NotNil(t, snapshot.Total, "失败不得抹掉最近成功总额")
	require.Equal(t, 400, *snapshot.Total)
	require.NotNil(t, snapshot.Used)
	require.Equal(t, 86, *snapshot.Used)
	require.NotNil(t, snapshot.CheckedAt)
	require.Equal(t, 2026, snapshot.CheckedAt.Year())
	require.NotNil(t, snapshot.AttemptedAt)
	require.True(t, snapshot.AttemptedAt.After(*snapshot.CheckedAt), "attempted_at 必须是本次观测时刻")
	require.Equal(t, "CURSOR_LOOKUP_CODE", snapshot.ErrorCode)
}

// TestProbeKeyQuota_MissingQuotaFieldsAreNotZero：缺额度字段 ≠ 额度 0 —— 必须失败，
// 且失败观测里不出现伪造的 0 值。
func TestProbeKeyQuota_MissingQuotaFieldsAreNotZero(t *testing.T) {
	ls := newLookupServer(t, func() string {
		return `{"code":0,"msg":"ok","data":{"key_info":{"group":"Pro","key_status_code":1}}}`
	})
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}

	svc := newAutoEntryProbeService(ls.srv, repo)
	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.Error(t, err, "缺额度字段必须失败关闭，不得由 Go 零值补成真实 0")
	require.Contains(t, err.Error(), "CURSOR_LOOKUP_INVALID")

	snapshot, ok := ParseQuotaKeyUsageSnapshot(repo.byID[42].UsageSnapshot)
	require.True(t, ok)
	require.Nil(t, snapshot.Total)
	require.Nil(t, snapshot.Used)
	require.Equal(t, "CURSOR_LOOKUP_INVALID", snapshot.ErrorCode)
}

// TestProbeKeyQuota_StorageFailureNotReportedAsSuccess：写入/提交失败返回 storage error，
// 绝不报探测成功，也不把内存结果当已提交事实。
func TestProbeKeyQuota_StorageFailureNotReportedAsSuccess(t *testing.T) {
	ls := newLookupServer(t, func() string { return probeOKBody })
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}
	repo.failSnapshotWrite = true

	svc := newAutoEntryProbeService(ls.srv, repo)
	rec := &observeTxRecord{}
	svc.SetObserveTx(rec.tx())

	info, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.Error(t, err, "存储失败不得报成功")
	require.Nil(t, info)
	require.True(t, errors.Is(err, errCursorObservationStorage), "必须以 storage error 归类回传: %v", err)
	_, commits, rollbacks := rec.snapshot()
	require.Zero(t, commits)
	require.Equal(t, 1, rollbacks, "存储失败必须走回滚，不得提交半份事实")
	require.Nil(t, repo.byID[42].UsageSnapshot)
}

// TestProbeKeyQuota_SourceRotatedBeforeLockConflictsWithoutLookup：解析后来源被换码 ⇒
// 进锁重读发现 MAC 不一致，明确冲突且**零 lookup**（不拿旧明文继续观察）。
func TestProbeKeyQuota_SourceRotatedBeforeLockConflictsWithoutLookup(t *testing.T) {
	ls := newLookupServer(t, func() string { return probeOKBody })
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{
		ID:            42,
		CodeEncrypted: "enc:JG-OLD-CODE",
		CodeMAC:       quotaKeyMACOfPlaintext("JG-OLD-CODE"),
	}
	svc := newAutoEntryProbeService(ls.srv, repo)
	// 模拟"解析完成、尚未进锁"期间发生的换码（另一事务把凭据换成新 code）。
	svc.SetObserveTx(func(txCtx, lockCtx context.Context, _ int64, fn func(tx *ent.Tx) error) error {
		repo.byID[42].CodeMAC = quotaKeyMACOfPlaintext("JG-NEW-CODE")
		repo.byID[42].CodeEncrypted = "enc:JG-NEW-CODE"
		return fn(nil)
	})

	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.Error(t, err)
	require.True(t, errors.Is(err, errCursorObservationConflict), "换码后必须明确冲突: %v", err)
	hits, _, _, _, _, _ := ls.stats()
	require.Zero(t, hits, "来源已被换码时不得发起任何 lookup")
	require.Equal(t, quotaKeyMACOfPlaintext("JG-NEW-CODE"), repo.byID[42].CodeMAC, "冲突不得覆盖新凭据")
}

// TestProbeKeyQuota_CallerCancelledDuringLookupStillCommits：网络随调用者取消，但失败观测
// 仍在 ≤5s 收尾预算内提交（事务 context 与网络 context 分开）。
func TestProbeKeyQuota_CallerCancelledDuringLookupStillCommits(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	ls := newLookupServer(t, func() string {
		// 服务端阻塞到调用者取消，模拟慢 lookup。
		once.Do(func() {
			<-release
		})
		return probeOKBody
	})
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}

	svc := newAutoEntryProbeService(ls.srv, repo)
	rec := &observeTxRecord{}
	svc.SetObserveTx(rec.tx())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
		close(release)
	}()
	_, err := svc.ProbeKeyQuota(ctx, 42)
	require.Error(t, err, "调用者取消后不得报成功")

	_, commits, rollbacks := rec.snapshot()
	require.Equal(t, 1, commits, "网络取消不得带走 DB 收尾：失败观测必须提交")
	require.Zero(t, rollbacks)

	snapshot, ok := ParseQuotaKeyUsageSnapshot(repo.byID[42].UsageSnapshot)
	require.True(t, ok)
	require.NotEmpty(t, snapshot.ErrorCode, "取消必须登记成脱敏 error_code")
	require.Nil(t, snapshot.Total)
}

// TestProbeKeyQuota_CallerCancelledBeforeStartSkipsLookup：进入即已取消 ⇒ 不启动、不发请求。
func TestProbeKeyQuota_CallerCancelledBeforeStartSkipsLookup(t *testing.T) {
	ls := newLookupServer(t, func() string { return probeOKBody })
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}
	svc := newAutoEntryProbeService(ls.srv, repo)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.ProbeKeyQuota(ctx, 42)
	require.Error(t, err)
	hits, _, _, _, _, _ := ls.stats()
	require.Zero(t, hits, "调用者已取消时不得发起 lookup")
}

// TestProbeKeyQuota_NonSuccessHTTPWritesFailedObservation：非 2xx 不是额度数据。
func TestProbeKeyQuota_NonSuccessHTTPWritesFailedObservation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(probeOKBody))
	}))
	defer srv.Close()
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}

	svc := newAutoEntryProbeService(srv, repo)
	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.Error(t, err)
	snapshot, ok := ParseQuotaKeyUsageSnapshot(repo.byID[42].UsageSnapshot)
	require.True(t, ok, "传输失败也必须留下失败观测")
	require.Equal(t, "CURSOR_LOOKUP_FAILED", snapshot.ErrorCode)
	require.Nil(t, snapshot.Total)
}

// TestBuildCursorUsageSnapshot_MissingValuesNotZero：无成功值时数值字段必须缺失（nil），
// 有成功值时只保留最近成功值。
func TestBuildCursorUsageSnapshot_MissingValuesNotZero(t *testing.T) {
	raw, err := buildCursorUsageSnapshot(nil, nil, time.Now().UTC(), "CURSOR_LOOKUP_FAILED")
	require.NoError(t, err)
	snapshot, ok := ParseQuotaKeyUsageSnapshot(raw)
	require.True(t, ok)
	require.Nil(t, snapshot.Total)
	require.Nil(t, snapshot.Used)
	require.Equal(t, "CURSOR_LOOKUP_FAILED", snapshot.ErrorCode)

	info := &lookupKeyInfo{MaxQuota: 400, UsedQuota: 0, StatusCode: 1}
	raw, err = buildCursorUsageSnapshot(nil, info, time.Now().UTC(), "")
	require.NoError(t, err)
	snapshot, ok = ParseQuotaKeyUsageSnapshot(raw)
	require.True(t, ok)
	require.NotNil(t, snapshot.Used, "真实 0 必须保留为 0（不是缺失）")
	require.Equal(t, 0, *snapshot.Used)
}

// ---------------------------------------------------------------------------
// 4. 配置读写：缺省 / 校验 / 持久化
// ---------------------------------------------------------------------------

func TestGetConfigDefaultWhenMissing(t *testing.T) {
	svc := NewCursorAutoEntryService(newFakeSettingRepo(), newFakeQuotaKeyRepo(), testEncryptor{}, nil, nil, "")
	cfg, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, cfg.EntryDefaults.Priority)
	require.Equal(t, 1.0, cfg.EntryDefaults.RateMultiplier)
	require.Nil(t, cfg.EntryDefaults.Concurrency)
	require.True(t, cfg.Vendors[CursorVendorOpenAI])
}

func TestUpdateAndGetConfigRoundTrip(t *testing.T) {
	repo := newFakeSettingRepo()
	svc := NewCursorAutoEntryService(repo, newFakeQuotaKeyRepo(), testEncryptor{}, nil, nil, "")
	cfg := &CursorAutoEntryConfig{
		Vendors: map[string]bool{CursorVendorOpenAI: true},
		Models:  []CursorModelRef{{ID: "gpt-4.1", Vendor: CursorVendorOpenAI}},
		EntryDefaults: CursorEntryDefaults{
			Priority:       1,
			RateMultiplier: 1.0,
		},
	}
	// UpdateConfig 已随端点删除；此处直接写 settings 仓储（替代已删的持久化路径），
	// 再用 GetConfig 读回断言，保留配置读取路径的覆盖。
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, repo.Set(context.Background(), CursorAutoEntryConfigSettingKey, string(raw)))

	got, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "gpt-4.1", got.Models[0].ID)
	require.Equal(t, cfg.Vendors, got.Vendors)
}

// TestDeviceIDFormat：随机设备身份只在"真新码首写"处生成（GenerateCursorDeviceID），
// 形如插件 hashDeviceId 的 8 位大写十六进制。
func TestDeviceIDFormat(t *testing.T) {
	for i := 0; i < 20; i++ {
		d, err := GenerateCursorDeviceID()
		require.NoError(t, err)
		require.Regexp(t, deviceIDRe, d, "device-id 必须是 8 位大写十六进制")
	}
}

// ---------------------------------------------------------------------------
// CUR-F8（外审 R5 must_fix）：模型-vendor 归属校验，以实供清单真实归属为准，不信自报
// ---------------------------------------------------------------------------

// TestCURF8_ValidateRejectsVendorSpoofing：冒用反例。
// 提交 gpt-6-sol（真实归属 openai）+ vendor=xai，并开 xai / 关 openai。
// 旧实现只查 vendor 已知+启用，会被骗过；CUR-F8 应以真实归属拦截（UpdateConfig 已删，
// 此处直接断言 Validate 的冒用防线）。
func TestCURF8_ValidateRejectsVendorSpoofing(t *testing.T) {
	cfg := &CursorAutoEntryConfig{
		Vendors:       map[string]bool{CursorVendorOpenAI: false, CursorVendorXAI: true}, // 关 openai / 开 xai
		Models:        []CursorModelRef{{ID: "gpt-6-sol", Vendor: CursorVendorXAI}},      // 冒用：真实归属是 openai
		EntryDefaults: CursorEntryDefaults{Priority: 1, RateMultiplier: 1.0},
	}
	require.NotEmpty(t, cfg.Validate(), "gpt-6-sol 自报 xai 以绕过 openai 开关时必须被 Validate 拦截")
	require.Contains(t, cfg.Validate(), "belongs to vendor openai, not xai")
}

// TestCURF8_ComputeEntryModelsUnaffectedByVendorState：P1-B2 后 ComputeEntryModels 为
// 静态恒等，不再做厂商门控。冒用 vendor 的防线保留在 Validate（见上一条用例），
// 入库模型集合不再受厂商开关 / 自报 vendor 影响。
func TestCURF8_ComputeEntryModelsUnaffectedByVendorState(t *testing.T) {
	spoofed := &CursorAutoEntryConfig{
		Vendors: map[string]bool{CursorVendorOpenAI: false, CursorVendorXAI: true},
		Models:  []CursorModelRef{{ID: "gpt-6-sol", Vendor: CursorVendorXAI}}, // 冒用：真实归属 openai
	}
	got := ComputeEntryModels(spoofed)
	require.Equal(t, CursorStaticEntryModels(), got, "冒用配置不得改变入库模型集合（静态恒等）")
	require.Contains(t, got, "gpt-6-sol")
}

// TestCURF8_PositiveVendorMatch：正例。gpt-6-sol + vendor=openai + openai 开 →
// Validate 放行；入库模型集合为静态清单（含 gpt-6-sol）。
// （UpdateConfig 已删，此处直接断言 Validate + ComputeEntryModels。）
func TestCURF8_PositiveVendorMatch(t *testing.T) {
	cfg := &CursorAutoEntryConfig{
		Vendors:       map[string]bool{CursorVendorOpenAI: true},
		Models:        []CursorModelRef{{ID: "gpt-6-sol", Vendor: CursorVendorOpenAI}},
		EntryDefaults: CursorEntryDefaults{Priority: 1, RateMultiplier: 1.0},
	}
	require.Empty(t, cfg.Validate(), "真实归属匹配且厂商启用时应放行")
	got := ComputeEntryModels(cfg)
	require.Contains(t, got, "gpt-6-sol")
	require.Equal(t, CursorStaticEntryModels(), got)
}

// TestCURF8_DisabledRealVendorNoLongerShrinksEntryModels：真实归属厂商关闭
// 不再剔除入库模型（能力由静态实供清单决定，不由勾选决定）；Validate 仍拒绝自报厂商未勾选。
func TestCURF8_DisabledRealVendorNoLongerShrinksEntryModels(t *testing.T) {
	cfg := &CursorAutoEntryConfig{
		Vendors: map[string]bool{CursorVendorOpenAI: false}, // openai 关
		Models:  []CursorModelRef{{ID: "gpt-6-sol", Vendor: CursorVendorOpenAI}},
	}
	got := ComputeEntryModels(cfg)
	require.Equal(t, CursorStaticEntryModels(), got, "厂商关闭不得缩小静态入库模型集合")
	require.Contains(t, got, "gpt-6-sol")

	// Validate 层面仍拒绝（自报厂商未勾选）——CUR-F8 冒用防线不回归。
	require.NotEmpty(t, cfg.Validate(), "自报厂商未勾选时 Validate 必须拒绝")
}

// ---------------------------------------------------------------------------
// 3b. 真实 DB 语义：同 key 串行、无晚到覆盖、容量回滚不撤销先前观察
//     （沿用包内既有模式：连本地 quota_test 库，走 production 同款 advisory 锁 + 事务）
// ---------------------------------------------------------------------------

// TestProbeKeyQuota_SerializedByPerKeyLock：同一 key 的两个并发观测必须串行（先发起者先
// 提交），不存在"较早响应晚写覆盖较新观测"。
func TestProbeKeyQuota_SerializedByPerKeyLock(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	code := fmt.Sprintf("JG-SERIAL-CODE-%d", time.Now().UnixNano())
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:" + code).
		SetCodeMAC(quotaKeyMACOfPlaintext(code)).
		SetStatus(domain.QuotaKeyStatusActive).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.QuotaKey.DeleteOneID(qk.ID).Exec(context.Background()) })

	var mu sync.Mutex
	hit := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit++
		n := hit
		mu.Unlock()
		if n == 1 {
			// 第一次（先发起者）慢回，used=100。
			time.Sleep(120 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{"group":"Pro",` +
				`"key_status_code":1,"quota_key_max_quota":400,"quota_key_used_quota":100,` +
				`"device_count":1,"device_max_count":3,"mode":1}}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{"group":"Pro",` +
			`"key_status_code":1,"quota_key_max_quota":400,"quota_key_used_quota":200,` +
			`"device_count":1,"device_max_count":3,"mode":1}}}`))
	}))
	defer srv.Close()

	repo := &testDBRepo{client: client}
	svc := NewCursorAutoEntryService(newFakeSettingRepo(), repo, testEncryptor{}, client, &http.Client{Timeout: 5 * time.Second}, srv.URL)
	svc.SetWriter(&fakeAccountWriter{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = svc.ProbeKeyQuota(ctx, qk.ID)
	}()
	time.Sleep(20 * time.Millisecond) // 保证第一个观测先拿到锁
	go func() {
		defer wg.Done()
		_, _ = svc.ProbeKeyQuota(ctx, qk.ID)
	}()
	wg.Wait()

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	snapshot, ok := ParseQuotaKeyUsageSnapshot(got.UsageSnapshot)
	require.True(t, ok)
	require.NotNil(t, snapshot.Used)
	require.Equal(t, 200, *snapshot.Used,
		"后发起的观测值必须胜出：同锁内先提交后发起，晚到的旧响应不得覆盖")
	mu.Lock()
	require.Equal(t, 2, hit)
	mu.Unlock()
}

// TestProbeKeyQuota_ObservationSurvivesCapacityRollback：观测先独立提交，随后的补号容量
// 事务回滚不得撤销先前的观察（§25.14-C 步骤 6：补号前观察单独提交，再取容量锁）。
func TestProbeKeyQuota_ObservationSurvivesCapacityRollback(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	code := fmt.Sprintf("JG-ROLLBACK-CODE-%d", time.Now().UnixNano())
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:" + code).
		SetCodeMAC(quotaKeyMACOfPlaintext(code)).
		SetStatus(domain.QuotaKeyStatusActive).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = client.Account.Update().Where(account.QuotaKeyIDEQ(qk.ID)).ClearQuotaKeyID().Save(bg)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(bg)
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{"group":"Pro",` +
			`"key_status_code":1,"quota_key_max_quota":400,"quota_key_used_quota":37,` +
			`"device_count":1,"device_max_count":3,"mode":1}}}`))
	}))
	defer srv.Close()

	repo := &testDBRepo{client: client}
	observer := NewCursorAutoEntryService(newFakeSettingRepo(), repo, testEncryptor{}, client, &http.Client{Timeout: 5 * time.Second}, srv.URL)
	_, err = observer.ProbeKeyQuota(ctx, qk.ID)
	require.NoError(t, err)

	// 补号路径：注入挂组解析失败使容量事务整体回滚。
	up := &fakeUpstreamClient{batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
		{Name: "rollback-probe", Platform: domain.PlatformCursor, Type: AccountTypeAPIKey, Credentials: map[string]any{}},
	}}}
	pullSvc := NewQuotaKeyService(repo, testEncryptor{}, up, client)
	pullSvc.cursorGroupResolver = func(context.Context, *ent.Tx) ([]int64, error) {
		return nil, errors.New("injected mount resolve failure")
	}
	require.Error(t, pullSvc.PullAccountsForModel(ctx, qk.ID, "claude-sonnet-5-5"))

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	snapshot, ok := ParseQuotaKeyUsageSnapshot(got.UsageSnapshot)
	require.True(t, ok, "容量事务回滚不得撤销先前已提交的观测")
	require.NotNil(t, snapshot.Used)
	require.Equal(t, 37, *snapshot.Used)
}

// TestPruneLegacyCursorQuotaSnapshots：旧链清理白名单只清 Cursor 账号 extra 上的
// key_quota_snapshot 旧键并记录数量；其它 extra 键与其它厂商账号原样保留。
func TestPruneLegacyCursorQuotaSnapshots(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	cursorAcc, err := client.Account.Create().
		SetName(fmt.Sprintf("legacy-prune-cursor-%d", time.Now().UnixNano())).
		SetPlatform(PlatformCursor).
		SetType("oauth").
		SetExtra(map[string]any{
			CursorKeyQuotaSnapshotExtraKey: map[string]any{"used": 1, "max": 400},
			"keep_me":                      "vendor-neutral",
			"account_quota":                map[string]any{"vendor": "other"},
		}).
		Save(ctx)
	require.NoError(t, err)
	otherAcc, err := client.Account.Create().
		SetName(fmt.Sprintf("legacy-prune-other-%d", time.Now().UnixNano())).
		SetPlatform("openai").
		SetType("oauth").
		SetExtra(map[string]any{CursorKeyQuotaSnapshotExtraKey: map[string]any{"used": 5}}).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		bg := context.Background()
		_ = client.Account.DeleteOneID(cursorAcc.ID).Exec(bg)
		_ = client.Account.DeleteOneID(otherAcc.ID).Exec(bg)
	})

	svc := NewCursorAutoEntryService(newFakeSettingRepo(), newFakeQuotaKeyRepo(), testEncryptor{}, client, nil, "")
	pruned, err := svc.PruneLegacyCursorQuotaSnapshots(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, pruned, 1, "必须记录清理的账号数量")

	got, err := client.Account.Get(ctx, cursorAcc.ID)
	require.NoError(t, err)
	require.NotContains(t, got.Extra, CursorKeyQuotaSnapshotExtraKey, "Cursor 旧键必须被清掉")
	require.Equal(t, "vendor-neutral", got.Extra["keep_me"], "其它 extra 键不得被清理")
	require.Contains(t, got.Extra, "account_quota", "不得触碰其它账号级 extra")

	other, err := client.Account.Get(ctx, otherAcc.ID)
	require.NoError(t, err)
	require.Contains(t, other.Extra, CursorKeyQuotaSnapshotExtraKey,
		"白名单只清 Cursor 账号，其它厂商账号的 extra 不得被改")
}

// ---------------------------------------------------------------------------
// 5. KC-20B：观测路径按 quota_keys.upstream_key 分派（shareai 零变化 / kn_cy 走订阅用量 /
//    未知标识失败关闭）
// ---------------------------------------------------------------------------

// newAutoEntryKnCyProbeService 构造观测服务并注入指向 httptest 的 kn-cy 客户端。
func newAutoEntryKnCyProbeService(knCySrv *httptest.Server, repo *fakeQuotaKeyRepo) *CursorAutoEntryService {
	svc := NewCursorAutoEntryService(newFakeSettingRepo(), repo, testEncryptor{}, nil, nil, "")
	svc.SetKnCyClient(NewKnCyClient(knCySrv.URL))
	svc.SetWriter(&fakeAccountWriter{})
	return svc
}

// newKnCyUsageServer 是 kn-cy 订阅用量端点的替身：校验路径/方法并回包。
func newKnCyUsageServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/usage/subscription/", r.URL.Path, "kn-cy 额度观测必须打订阅用量接口")
		require.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeKeyQuota_KnCyWritesSnapshotAndExpiry：kn_cy 上游读订阅用量，余额以 TotalRemaining
// 进入同一 usage_snapshot（Used=0 ⇒ 剩余=TotalRemaining），到期写入同一 expires_at。
func TestProbeKeyQuota_KnCyWritesSnapshotAndExpiry(t *testing.T) {
	expUnix := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC).Unix()
	body := fmt.Sprintf(`{"status":"ok","message":"ok","data":{"total_remaining":1500,`+
		`"today_remaining":200,"expires_at":%d,"unlimited":false,"unit":"requests"}}`, expUnix)
	srv := newKnCyUsageServer(t, body)
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST", UpstreamKey: upstreamKeyKnCy}

	svc := newAutoEntryKnCyProbeService(srv, repo)
	rec := &observeTxRecord{}
	svc.SetObserveTx(rec.tx())

	info, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, 1500, info.Max, "kn-cy 余额须以 TotalRemaining 进入快照总额")
	require.Equal(t, 0, info.Used, "kn-cy 仅给余额不给已用，Used 须为 0")
	require.Equal(t, "kn-cy", info.Group)

	// 唯一事实源：同一 quota_keys.usage_snapshot。
	snapshot, ok := ParseQuotaKeyUsageSnapshot(repo.byID[42].UsageSnapshot)
	require.True(t, ok, "kn-cy 须写入 quota_keys.usage_snapshot（同一快照列）")
	require.NotNil(t, snapshot.Total)
	require.Equal(t, 1500, *snapshot.Total)
	require.NotNil(t, snapshot.Used)
	require.Equal(t, 0, *snapshot.Used)
	require.Equal(t, "", snapshot.ErrorCode, "成功观测 error_code 必须为空")

	// 到期只写既有 expires_at（同一到期列）。
	require.NotNil(t, repo.byID[42].ExpiresAt)
	require.Equal(t, 2027, repo.byID[42].ExpiresAt.UTC().Year())

	calls, commits, rollbacks := rec.snapshot()
	require.Equal(t, 1, calls)
	require.Equal(t, 1, commits)
	require.Zero(t, rollbacks)
}

// TestProbeKeyQuota_KnCyExhaustedWritesZeroRemaining：余额耗尽（total_remaining=0）须写入
// 剩余=0 的快照，使既有码级停调（剩余<下限）照常触发；expires_at=0（未给）⇒ 保持原值、不得
// 据未知补日期。
func TestProbeKeyQuota_KnCyExhaustedWritesZeroRemaining(t *testing.T) {
	body := `{"status":"ok","message":"ok","data":{"total_remaining":0,` +
		`"today_remaining":0,"expires_at":0,"unlimited":false,"unit":"requests"}}`
	srv := newKnCyUsageServer(t, body)
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST", UpstreamKey: upstreamKeyKnCy}

	svc := newAutoEntryKnCyProbeService(srv, repo)
	info, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, 0, info.Max)
	require.Equal(t, 0, info.Used)

	snapshot, ok := ParseQuotaKeyUsageSnapshot(repo.byID[42].UsageSnapshot)
	require.True(t, ok)
	require.NotNil(t, snapshot.Total)
	require.Equal(t, 0, *snapshot.Total)
	// 到期未给 ⇒ 解析为 nil ⇒ 保持原值（本例无原值，ExpiresAt 应为 nil）。
	require.Nil(t, repo.byID[42].ExpiresAt, "kn-cy 未给到期时不得据未知补日期")
}

// TestProbeKeyQuota_KnCyUpstreamErrorWritesFailureObservation：kn-cy 上游错误如实上抛，
// 走既有失败观测写入（不伪装成余额 0、不回落 ShareAI）。
func TestProbeKeyQuota_KnCyUpstreamErrorWritesFailureObservation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // 模拟"额度不足"类非 2xx
		_, _ = w.Write([]byte(`{"status":"error","message":"quota insufficient"}`))
	}))
	defer srv.Close()
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST", UpstreamKey: upstreamKeyKnCy}

	svc := newAutoEntryKnCyProbeService(srv, repo)
	rec := &observeTxRecord{}
	svc.SetObserveTx(rec.tx())

	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.Error(t, err, "kn-cy 上游错误必须如实返错")

	_, commits, rollbacks := rec.snapshot()
	require.Zero(t, rollbacks, "失败观测不得走回滚分支")
	require.Equal(t, 1, commits, "失败观测必须随事务提交后才返错")

	snapshot, ok := ParseQuotaKeyUsageSnapshot(repo.byID[42].UsageSnapshot)
	require.True(t, ok, "失败观测必须已落盘")
	require.NotEmpty(t, snapshot.ErrorCode, "失败须登记脱敏 error_code")
	require.Nil(t, snapshot.Total, "失败不得写伪造余额 0")
	require.Nil(t, snapshot.Used)
}

// TestProbeKeyQuota_UnknownUpstreamKeyFailsClosed：未知上游标识必须失败关闭（返回 error），
// 绝不回落 ShareAI（回落会让 kn-cy 卡密去打 ShareAI lookup，凭据泄露 + 白耗额度），且不得写任何快照。
func TestProbeKeyQuota_UnknownUpstreamKeyFailsClosed(t *testing.T) {
	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer dummy.Close()
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST", UpstreamKey: "bogus_vendor"}

	svc := newAutoEntryKnCyProbeService(dummy, repo)
	rec := &observeTxRecord{}
	svc.SetObserveTx(rec.tx())

	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.Error(t, err, "未知上游标识必须失败关闭")
	require.Contains(t, err.Error(), "unknown upstream key")
	require.Nil(t, repo.byID[42].UsageSnapshot, "失败关闭不得写任何快照（不回落 ShareAI）")
	calls, _, _ := rec.snapshot()
	require.Equal(t, 1, calls, "未知标识仍进入观测事务骨架，但分派即失败关闭")
}
