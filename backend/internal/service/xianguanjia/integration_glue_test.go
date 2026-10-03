package xianguanjia

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	_ "modernc.org/sqlite"
)

// ---- SettingsKindIDStore 读写回环 ----

// fakeSettingRepo 模拟 service.SettingRepository 的 GetValue/Set 子集（upsert 语义）。
type fakeSettingRepo struct {
	m map[string]string
}

func newFakeSettingRepo() *fakeSettingRepo {
	return &fakeSettingRepo{m: map[string]string{}}
}

func (r *fakeSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	v, ok := r.m[key]
	if !ok {
		return "", errors.New("setting not found")
	}
	return v, nil
}

func (r *fakeSettingRepo) Set(_ context.Context, key, value string) error {
	r.m[key] = value // settings 表 key UNIQUE + upsert 的内存等价
	return nil
}

func TestSettingsKindIDStoreRoundTrip(t *testing.T) {
	repo := newFakeSettingRepo()
	store := NewSettingsKindIDStore(repo)
	ctx := context.Background()

	// 未写入前读取报错（fail-closed 由上层 KindService.GetKindID 归一）。
	if _, err := store.GetValue(ctx, SettingKeyKindID); err == nil {
		t.Fatalf("expected error before Set, got nil")
	}
	// 写入后回读一致。
	if err := store.Set(ctx, SettingKeyKindID, "123"); err != nil {
		t.Fatalf("Set returned error: %v", err)
	}
	v, err := store.GetValue(ctx, SettingKeyKindID)
	if err != nil {
		t.Fatalf("GetValue returned error: %v", err)
	}
	if v != "123" {
		t.Fatalf("round trip mismatch: got %q, want %q", v, "123")
	}
	// upsert 覆盖写。
	if err := store.Set(ctx, SettingKeyKindID, "456"); err != nil {
		t.Fatalf("Set (overwrite) returned error: %v", err)
	}
	if v, _ := store.GetValue(ctx, SettingKeyKindID); v != "456" {
		t.Fatalf("overwrite mismatch: got %q, want %q", v, "456")
	}
	// nil 依赖 fail-closed。
	var nilStore *SettingsKindIDStore
	if err := nilStore.Set(ctx, SettingKeyKindID, "1"); err == nil {
		t.Fatalf("nil store Set: expected error, got nil")
	}
}

// TestKindCurrentUnsetReturnsZero 验证 KindCurrent 在未配置 kind_id 时返回 (0, nil)
// （对齐 admin GET /pool/kind 端点契约），而非上抛错误。
func TestKindCurrentUnsetReturnsZero(t *testing.T) {
	svc := NewLazyKindService(nil, NewSettingsKindIDStore(newFakeSettingRepo()))
	kindID, err := svc.KindCurrent(context.Background())
	if err != nil {
		t.Fatalf("KindCurrent (unset) returned error: %v", err)
	}
	if kindID != 0 {
		t.Fatalf("KindCurrent (unset): got %d, want 0", kindID)
	}
	// 写入后能读回。
	store := NewSettingsKindIDStore(newFakeSettingRepo())
	_ = store.Set(context.Background(), SettingKeyKindID, "88")
	svc2 := NewLazyKindService(nil, store)
	kindID, err = svc2.KindCurrent(context.Background())
	if err != nil || kindID != 88 {
		t.Fatalf("KindCurrent: got (%d, %v), want (88, nil)", kindID, err)
	}
}

// TestIsErrKindIDNotConfiguredWrapped 验证包装链识别。
func TestIsErrKindIDNotConfiguredWrapped(t *testing.T) {
	wrapped := ErrKindIDNotConfigured
	if !IsErrKindIDNotConfigured(wrapped) {
		t.Fatalf("direct error not recognized")
	}
	further := fmt.Errorf("get kind id: %w", wrapped)
	if !IsErrKindIDNotConfigured(further) {
		t.Fatalf("wrapped error not recognized")
	}
	if IsErrKindIDNotConfigured(errors.New("other")) || IsErrKindIDNotConfigured(nil) {
		t.Fatalf("false positive")
	}
}

// ---- marker：只标记 unused（已 delivered/其他状态不动）----

func newMarkerDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock, *pushedCardMarkerDB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock, NewPushedCardMarker(db)
}

func TestMarkerOnlyMarksUnused(t *testing.T) {
	_, mock, marker := newMarkerDB(t)
	ctx := context.Background()

	// C1: unused → 命中条件更新（affected=1）。
	mock.ExpectExec(`UPDATE redeem_codes SET status = 'delivered'
			WHERE code = \$1 AND status = 'unused'`).
		WithArgs("C1").WillReturnResult(driver.RowsAffected(1))
	// C2: 已 delivered → 条件未命中（affected=0），回读状态 delivered。
	mock.ExpectExec(`UPDATE redeem_codes SET status = 'delivered'`).
		WithArgs("C2").WillReturnResult(driver.RowsAffected(0))
	mock.ExpectQuery(`SELECT status FROM redeem_codes WHERE code = \$1`).
		WithArgs("C2").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("delivered"))
	// C3: used → 不动，回读分类 skipped。
	mock.ExpectExec(`UPDATE redeem_codes SET status = 'delivered'`).
		WithArgs("C3").WillReturnResult(driver.RowsAffected(0))
	mock.ExpectQuery(`SELECT status FROM redeem_codes WHERE code = \$1`).
		WithArgs("C3").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("used"))
	// C4: 池内无此卡。
	mock.ExpectExec(`UPDATE redeem_codes SET status = 'delivered'`).
		WithArgs("C4").WillReturnResult(driver.RowsAffected(0))
	mock.ExpectQuery(`SELECT status FROM redeem_codes WHERE code = \$1`).
		WithArgs("C4").WillReturnRows(sqlmock.NewRows([]string{"status"}))

	res, err := marker.MarkPushedToXianyu(ctx, []string{"C1", "C2", "C3", "C4"})
	if err != nil {
		t.Fatalf("MarkPushedToXianyu returned error: %v", err)
	}
	want := map[string]string{
		"C1": "marked",
		"C2": "already_delivered",
		"C3": "skipped:used",
		"C4": "missing",
	}
	for no, st := range want {
		if res[no] != st {
			t.Fatalf("card %s: got %q, want %q", no, res[no], st)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestMarkerSQLGuardsUnused 编译期/输出期断言：UPDATE 必须带 status='unused'
// 防超发守卫（对 D4b 交付实现的方向性锁定，防回归）。
func TestMarkerSQLGuardsUnused(t *testing.T) {
	_, mock, marker := newMarkerDB(t)
	// 故意声明只接受不含 unused 守卫的查询 → 若实现丢掉守卫本测试反而会通过
	// 期望；这里反向断言：用严格正则要求守卫存在。
	mock.ExpectExec(`WHERE code = \$1 AND status = 'unused'$`).
		WithArgs("X").WillReturnResult(driver.RowsAffected(1))
	res, err := marker.MarkPushedToXianyu(context.Background(), []string{"X"})
	if err != nil {
		t.Fatalf("MarkPushedToXianyu returned error: %v", err)
	}
	if res["X"] != "marked" {
		t.Fatalf("expected marked, got %q", res["X"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unused guard missing from UPDATE: %v", err)
	}
}

// ---- Lazy 服务：factory 无 active 配置 → ErrNoActiveConfig ----

func TestLazyServicesNoActiveConfig(t *testing.T) {
	// nil factory → fail-closed。
	lk := NewLazyKindService(nil, NewSettingsKindIDStore(newFakeSettingRepo()))
	if _, err := lk.KindCreate(context.Background(), "n", 0); !errors.Is(err, ErrNoActiveConfig) {
		t.Fatalf("LazyKindService.KindCreate (nil factory): got %v, want ErrNoActiveConfig", err)
	}
	lp := NewLazyPoolSyncService(nil, nil)
	if _, err := lp.PushCards(context.Background(), 1, []CardPair{{CardNo: "C"}}); !errors.Is(err, ErrNoActiveConfig) {
		t.Fatalf("LazyPoolSyncService.PushCards (nil factory): got %v, want ErrNoActiveConfig", err)
	}

	// 空 DB（无 active 配置行）→ GetActiveConfig 返回 nil → ErrNoActiveConfig。
	db, err := sql.Open("sqlite", "file:d4i_noactive?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	factory := NewClientFactory(NewConfigStore(db), func(string) (string, error) { return "s", nil })

	if _, err := factory.NewClient(context.Background()); !errors.Is(err, ErrNoActiveConfig) {
		t.Fatalf("factory.NewClient (empty db): got %v, want ErrNoActiveConfig", err)
	}
	lk2 := NewLazyKindService(factory, NewSettingsKindIDStore(newFakeSettingRepo()))
	if _, err := lk2.KindCreate(context.Background(), "n", 0); !errors.Is(err, ErrNoActiveConfig) {
		t.Fatalf("LazyKindService.KindCreate (empty db): got %v, want ErrNoActiveConfig", err)
	}
	lp2 := NewLazyPoolSyncService(factory, nil)
	if _, err := lp2.PushCards(context.Background(), 1, []CardPair{{CardNo: "C"}}); !errors.Is(err, ErrNoActiveConfig) {
		t.Fatalf("LazyPoolSyncService.PushCards (empty db): got %v, want ErrNoActiveConfig", err)
	}
}

// TestLazyServicesDelegateWithActiveConfig 验证有 active 配置时 Lazy 层真实委托：
// factory 从 sqlite 252 表读 active 配置并出站到 httptest mock 网关，
// LazyPoolSyncService.PushCards 走通推仓+标记全链。
func TestLazyServicesDelegateWithActiveConfig(t *testing.T) {
	// storage/create mock 网关（整单成功，无失败明细），URL 同时作为配置行 base_url
	// ——请求真实落到本网关，bodies 即委托侧证。
	var gatewayURL string
	var mu sync.Mutex
	var hitCount int
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hitCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(gwSrv.Close)
	gatewayURL = gwSrv.URL

	db, err := sql.Open("sqlite", "file:d4i_active?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE xianyu_xianguanjia_config (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		base_url TEXT, app_id TEXT, app_secret_encrypted TEXT,
		push_url TEXT, status TEXT, health_status TEXT,
		created_at TIMESTAMP, updated_at TIMESTAMP)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	_, err = db.Exec(`INSERT INTO xianyu_xianguanjia_config
		(base_url, app_id, app_secret_encrypted, push_url, status, health_status)
		VALUES (?, 'AK', 'ENC', 'PU', 'active', 'ok')`, gatewayURL)
	if err != nil {
		t.Fatalf("insert config: %v", err)
	}
	factory := NewClientFactory(NewConfigStore(db), func(c string) (string, error) { return "secret", nil })

	marker := &recordingMarker{}
	lp := NewLazyPoolSyncService(factory, marker)
	res, err := lp.PushCards(context.Background(), 9, []CardPair{{CardNo: "C1", CardPwd: "P1"}})
	if err != nil {
		t.Fatalf("PushCards returned error: %v", err)
	}
	if len(res.Succeeded) != 1 || res.Succeeded[0].CardNo != "C1" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if marker.got[0] != "C1" {
		t.Fatalf("marker not invoked with C1: %v", marker.got)
	}
	mu.Lock()
	hits := hitCount
	mu.Unlock()
	if hits < 1 {
		t.Fatalf("storage/create not hit via lazy delegation")
	}
}

// recordingMarker 是 PushedCardMarker 的最小记录实现（测试用）。
type recordingMarker struct {
	got []string
}

func (r *recordingMarker) MarkPushedToXianyu(_ context.Context, cardNos []string) (map[string]string, error) {
	r.got = append(r.got, cardNos...)
	out := make(map[string]string, len(cardNos))
	for _, no := range cardNos {
		out[no] = "marked"
	}
	return out, nil
}
