//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
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
	extra []accountExtraCall
}

type accountApplyCall struct {
	id             int64
	priority       int
	rateMultiplier float64
	concurrency    *int
}

type accountExtraCall struct {
	id    int64
	key   string
	value any
}

func (w *fakeAccountWriter) ApplyEntryDefaults(_ context.Context, _ *ent.Tx, id int64, priority int, rateMultiplier float64, concurrency *int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.apply = append(w.apply, accountApplyCall{id, priority, rateMultiplier, concurrency})
	return nil
}

func (w *fakeAccountWriter) UpdateExtraKey(_ context.Context, id int64, key string, value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = append(w.extra, accountExtraCall{id, key, value})
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
	require.Empty(t, w.extra, "不得再写 extra（entry_models 已随卡清理）")
	require.NotContains(t, updated.Extra, "entry_models", "内存实体不得回填 extra.entry_models")
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
// 6. GetConfigResponse：可选模型全集 = 实供清单（CUR-F2 裁定）
// ---------------------------------------------------------------------------

func TestGetConfigResponse_AvailableModelsMatchesRealSupply(t *testing.T) {
	// settings 为 nil 时 GetConfig 返回缺省配置；可用全集来自 CursorRealSupplyModels。
	s := NewCursorAutoEntryService(nil, nil, nil, nil, nil, "")
	resp, err := s.GetConfigResponse(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotNil(t, resp.AvailableModels, "响应必须含可选模型全集 available_models")

	// 逐项断言（id→vendor）与实供清单完全一致，不只断言长度。
	real := map[string]string{}
	for _, m := range CursorRealSupplyModels {
		real[m.ID] = m.Vendor
	}
	got := map[string]string{}
	for _, m := range resp.AvailableModels {
		got[m.ID] = m.Vendor
	}
	require.Equal(t, real, got, "可选模型全集必须与 CursorRealSupplyModels 逐项一致（id→vendor）")
	require.Len(t, resp.AvailableModels, len(CursorRealSupplyModels))

	// muse-spark-1.3（vendor=muse）必须在全集内——它属实供、按 muse 归类入库。
	require.Equal(t, CursorVendorMuse, got["muse-spark-1.3"], "muse-spark-1.3 必须在可选全集中且 vendor=muse")

	// 反向校验：不得混入官方 catalog 独有的模型（误用 CursorModelCatalog 会把实供判非法）。
	for id := range got {
		_, inReal := real[id]
		require.True(t, inReal, "可选全集含非实供模型 %s（误用官方 catalog？）", id)
	}
}

// ---------------------------------------------------------------------------
// 3. ProbeKeyQuota：替身 HTTP（断言请求头 key/device-id/UA）与 snapshot 写入
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

func TestProbeKeyQuotaHeadersAndSnapshot(t *testing.T) {
	// 替身 lookup 服务器：捕获请求头并回固定 key_info。
	var gotKey, gotDeviceID, gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("key")
		gotDeviceID = r.Header.Get("device-id")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{` +
			`"group":"Pro","key_status_code":1,"quota_key_max_quota":400,` +
			`"quota_key_used_quota":12,"device_count":1,"device_max_count":3,` +
			`"mode":1,"expired_at":"2026-12-31T00:00:00Z"}}}`))
	}))
	defer srv.Close()

	// 加密器替身：明文 passthrough。
	enc := testEncryptor{}
	// quota_keys 仓储替身：返回带加密 code 的 key + 归属该 key 的两个账号。
	// 注意：fake 的 ListAccountsByQuotaKey 按 account.QuotaKeyID 过滤，
	// 账号必须挂上 QuotaKeyID=42 才会被返回（QuotaKeyID 为 nil 一律过滤掉）。
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:JG-CF-TEST"}
	// 夹具账号必须挂 QuotaKeyID=42：fakeQuotaKeyRepo.ListAccountsByQuotaKey 按
	// QuotaKeyID 过滤（quota_key_service_test.go），不设置则写入数为 0。
	qkID := int64(42)
	repo.accounts = []*ent.Account{{ID: 100, QuotaKeyID: &qkID}, {ID: 101, QuotaKeyID: &qkID}}

	writer := &fakeAccountWriter{}
	svc := NewCursorAutoEntryService(newFakeSettingRepo(), repo, enc, nil, &http.Client{Timeout: 5 * time.Second}, srv.URL)
	svc.SetWriter(writer)
	// 固定 device-id 以便断言格式。
	svc.SetDeviceIDGen(func() string { return "6653FFCD" })

	info, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, "/lookup", gotPath)
	require.Equal(t, "JG-CF-TEST", gotKey, "必须使用解密后的明文 code 作为 key 头")
	require.Equal(t, "6653FFCD", gotDeviceID, "必须携带 DJB2 device-id")
	require.NotEmpty(t, gotUA, "必须携带浏览器 UA")
	require.Equal(t, 400, info.Max)
	require.Equal(t, 12, info.Used)
	require.Equal(t, 1, info.StatusCode)
	require.Equal(t, 2, info.AccountsWritten)
	require.Len(t, writer.extra, 2, "应为每个账号写一份 snapshot")
	for _, c := range writer.extra {
		require.Equal(t, CursorKeyQuotaSnapshotExtraKey, c.key)
		snap, ok := c.value.(CursorKeyQuotaSnapshot)
		require.True(t, ok)
		require.Equal(t, 12, snap.Used)
		require.Equal(t, 400, snap.Max)
		require.Equal(t, 1, snap.StatusCode)
		require.NotEmpty(t, snap.CheckedAt)
	}
}

func TestProbeKeyQuotaNonZeroCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1,"msg":"每15分钟内仅可刷新1次"}`))
	}))
	defer srv.Close()
	repo := newFakeQuotaKeyRepo()
	repo.byID[42] = &ent.QuotaKey{ID: 42, CodeEncrypted: "enc:X"}
	svc := NewCursorAutoEntryService(newFakeSettingRepo(), repo, testEncryptor{}, nil, &http.Client{Timeout: 5 * time.Second}, srv.URL)
	svc.SetWriter(&fakeAccountWriter{})
	_, err := svc.ProbeKeyQuota(context.Background(), 42)
	require.Error(t, err, "lookup 返回非 0 code 应报错")
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
	_, err := svc.UpdateConfig(context.Background(), cfg)
	require.NoError(t, err)
	// 落库值应为合法 JSON。
	raw, err := repo.GetValue(context.Background(), CursorAutoEntryConfigSettingKey)
	require.NoError(t, err)
	var back CursorAutoEntryConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &back))
	require.Equal(t, cfg.Vendors, back.Vendors)

	got, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "gpt-4.1", got.Models[0].ID)
}

func TestUpdateConfigRejectsDisabledVendorModel(t *testing.T) {
	svc := NewCursorAutoEntryService(newFakeSettingRepo(), newFakeQuotaKeyRepo(), testEncryptor{}, nil, nil, "")
	cfg := &CursorAutoEntryConfig{
		Vendors:       map[string]bool{CursorVendorOpenAI: false}, // 关掉 openai
		Models:        []CursorModelRef{{ID: "gpt-4.1", Vendor: CursorVendorOpenAI}},
		EntryDefaults: CursorEntryDefaults{Priority: 1, RateMultiplier: 1.0},
	}
	_, err := svc.UpdateConfig(context.Background(), cfg)
	require.Error(t, err, "模型所属厂商未勾选时必须拒绝")
}

func TestDeviceIDFormat(t *testing.T) {
	for i := 0; i < 20; i++ {
		d := hashDeviceID(randomHexUUIDForTest())
		require.Regexp(t, deviceIDRe, d, "device-id 必须是 8 位大写十六进制")
	}
}

// randomHexUUIDForTest 生成一个伪 UUID 串（仅用于 device-id 格式断言，不依赖 crypto/rand）。
func randomHexUUIDForTest() string {
	return "abc12345-6789-0abc-def1-23456789abcd"
}

// ---------------------------------------------------------------------------
// CUR-F8（外审 R5 must_fix）：模型-vendor 归属校验，以实供清单真实归属为准，不信自报
// ---------------------------------------------------------------------------

// TestCURF8_ValidateRejectsVendorSpoofing：冒用反例。
// 提交 gpt-6-sol（真实归属 openai）+ vendor=xai，并开 xai / 关 openai。
// 旧实现只查 vendor 已知+启用，会被骗过；CUR-F8 应以真实归属拦截 UpdateConfig。
func TestCURF8_ValidateRejectsVendorSpoofing(t *testing.T) {
	svc := NewCursorAutoEntryService(newFakeSettingRepo(), newFakeQuotaKeyRepo(), testEncryptor{}, nil, nil, "")
	cfg := &CursorAutoEntryConfig{
		Vendors:       map[string]bool{CursorVendorOpenAI: false, CursorVendorXAI: true}, // 关 openai / 开 xai
		Models:        []CursorModelRef{{ID: "gpt-6-sol", Vendor: CursorVendorXAI}},      // 冒用：真实归属是 openai
		EntryDefaults: CursorEntryDefaults{Priority: 1, RateMultiplier: 1.0},
	}
	_, err := svc.UpdateConfig(context.Background(), cfg)
	require.Error(t, err, "gpt-6-sol 自报 xai 以绕过 openai 开关时必须被 Validate 拦截")
	require.Contains(t, err.Error(), "belongs to vendor openai, not xai")
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
// UpdateConfig 通过（Validate 放行）；入库模型集合为静态清单（含 gpt-6-sol）。
func TestCURF8_PositiveVendorMatch(t *testing.T) {
	svc := NewCursorAutoEntryService(newFakeSettingRepo(), newFakeQuotaKeyRepo(), testEncryptor{}, nil, nil, "")
	cfg := &CursorAutoEntryConfig{
		Vendors:       map[string]bool{CursorVendorOpenAI: true},
		Models:        []CursorModelRef{{ID: "gpt-6-sol", Vendor: CursorVendorOpenAI}},
		EntryDefaults: CursorEntryDefaults{Priority: 1, RateMultiplier: 1.0},
	}
	_, err := svc.UpdateConfig(context.Background(), cfg)
	require.NoError(t, err, "真实归属匹配且厂商启用时应放行")
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
