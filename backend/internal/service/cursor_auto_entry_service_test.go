//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
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
// 2. ApplyEntryDefaults：勾选过滤 / 默认值写入 / entry_models 快照
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
	entryModels    []string
}

type accountExtraCall struct {
	id    int64
	key   string
	value any
}

func (w *fakeAccountWriter) ApplyEntryDefaults(_ context.Context, _ *ent.Tx, id int64, priority int, rateMultiplier float64, concurrency *int, entryModels []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.apply = append(w.apply, accountApplyCall{id, priority, rateMultiplier, concurrency, entryModels})
	return nil
}

func (w *fakeAccountWriter) UpdateExtraKey(_ context.Context, id int64, key string, value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = append(w.extra, accountExtraCall{id, key, value})
	return nil
}

func autoEntryPtrInt(v int) *int { return &v }

func TestComputeEntryModelsIntersection(t *testing.T) {
	// 厂商勾选：openai 启用、anthropic 禁用；清单含一个不在静态目录的失效 ID。
	cfg := &CursorAutoEntryConfig{
		Vendors: map[string]bool{CursorVendorOpenAI: true, CursorVendorAnthropic: false},
		Models: []CursorModelRef{
			{ID: "gpt-4.1", Vendor: CursorVendorOpenAI},
			{ID: "claude-3-7-sonnet", Vendor: CursorVendorAnthropic}, // 厂商禁用 → 剔除
			{ID: "not-a-real-model", Vendor: CursorVendorOpenAI},     // 不在静态清单 → 剔除
			{ID: "gpt-4o", Vendor: CursorVendorOpenAI},
		},
	}
	got := ComputeEntryModels(cfg)
	require.Equal(t, []string{"gpt-4.1", "gpt-4o"}, got)
}

func TestApplyEntryDefaultsWritesColumnsAndExtra(t *testing.T) {
	w := &fakeAccountWriter{}
	svc := &CursorAutoEntryService{writer: w}

	cfg := &CursorAutoEntryConfig{
		Vendors: map[string]bool{CursorVendorOpenAI: true},
		Models:  []CursorModelRef{{ID: "gpt-4.1", Vendor: CursorVendorOpenAI}},
		EntryDefaults: CursorEntryDefaults{
			Concurrency:    nil, // 沿用列默认 3
			Priority:       1,
			RateMultiplier: 1.0,
		},
	}
	acc := &ent.Account{ID: 7}
	updated, err := svc.ApplyEntryDefaults(context.Background(), nil, acc, cfg)
	require.NoError(t, err)
	require.Len(t, w.apply, 1)
	call := w.apply[0]
	require.Equal(t, int64(7), call.id)
	require.Equal(t, 1, call.priority)
	require.Equal(t, 1.0, call.rateMultiplier)
	require.Nil(t, call.concurrency, "concurrency=nil 时不应写该列")
	require.Equal(t, []string{"gpt-4.1"}, call.entryModels)
	// 内存实体回填：extra.entry_models
	require.Equal(t, []string{"gpt-4.1"}, updated.Extra[CursorEntryModelsExtraKey])
}

func TestApplyEntryDefaultsExplicitConcurrency(t *testing.T) {
	w := &fakeAccountWriter{}
	svc := &CursorAutoEntryService{writer: w}
	cfg := DefaultAutoEntryConfig()
	cfg.EntryDefaults.Concurrency = autoEntryPtrInt(9)
	acc := &ent.Account{ID: 3}
	_, err := svc.ApplyEntryDefaults(context.Background(), nil, acc, cfg)
	require.NoError(t, err)
	require.Len(t, w.apply, 1)
	require.NotNil(t, w.apply[0].concurrency)
	require.Equal(t, 9, *w.apply[0].concurrency)
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
