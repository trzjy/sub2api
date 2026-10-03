package xianguanjia

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"
)

// fakeSupplyEncryptor 是单测用的可逆加密器：Encrypt 反转明文并加前缀，Decrypt 还原。
// 反转保证密文不含明文子串，便于断言「密文 ≠ 明文」；绝非真实加密实现。
type fakeSupplyEncryptor struct{}

func (fakeSupplyEncryptor) Encrypt(plaintext string) (string, error) {
	r := []rune(plaintext)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return "enc:" + string(r), nil
}

func (fakeSupplyEncryptor) Decrypt(ciphertext string) (string, error) {
	if !strings.HasPrefix(ciphertext, "enc:") {
		return "", errors.New("bad ciphertext")
	}
	r := []rune(strings.TrimPrefix(ciphertext, "enc:"))
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r), nil
}

// supplyTestDBSeq 为每个测试库生成唯一内存库名，避免 t.Name() 重复导致的同名冲突。
var supplyTestDBSeq atomic.Int64

// newSupplyTestDB 建一张与 268 迁移后 schema 等价的 sqlite 表（含 ERP 与货源全列）。
func newSupplyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	name := "supply_cfg_" + strconv.FormatInt(supplyTestDBSeq.Add(1), 10)
	db, err := sql.Open("sqlite", "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE xianyu_xianguanjia_config (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		base_url TEXT NOT NULL DEFAULT '',
		app_id TEXT NOT NULL DEFAULT '',
		app_secret_encrypted TEXT NOT NULL DEFAULT '',
		mch_id TEXT NOT NULL DEFAULT '',
		mch_secret_encrypted TEXT NOT NULL DEFAULT '',
		supply_app_id TEXT NOT NULL DEFAULT '',
		supply_app_secret_encrypted TEXT NOT NULL DEFAULT '',
		push_url TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'disabled',
		health_status TEXT NOT NULL DEFAULT 'unknown',
		created_at TIMESTAMP,
		updated_at TIMESTAMP)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func TestSupplyConfigSaveThenGetRoundTrip(t *testing.T) {
	db := newSupplyTestDB(t)
	store := NewSupplyConfigStore(db, fakeSupplyEncryptor{})
	ctx := context.Background()

	// 未配置：无 active 行 → ErrSupplyNoConfig。
	if _, err := store.Get(ctx); !errors.Is(err, ErrSupplyNoConfig) {
		t.Fatalf("Get (empty) = %v, want ErrSupplyNoConfig", err)
	}

	// 写入（明文经 fake 加密器加密落库）。
	in := SupplyConfig{
		SupplyAppID:     "1000000000000001",
		SupplyAppSecret: "supply-secret-placeholder",
		MchID:           "777777",
		MchSecret:       "mch-secret-placeholder",
	}
	if err := store.Save(ctx, in); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 回读一致（secret 解密还原为明文）。
	got, err := store.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SupplyAppID != in.SupplyAppID || got.MchID != in.MchID {
		t.Fatalf("id mismatch: %+v", got)
	}
	if got.SupplyAppSecret != in.SupplyAppSecret || got.MchSecret != in.MchSecret {
		t.Fatalf("secret round-trip mismatch: %+v", got)
	}
	if got.GatewayPath != DefaultSupplyGatewayPath {
		t.Fatalf("gateway path = %q, want %q", got.GatewayPath, DefaultSupplyGatewayPath)
	}

	// 幂等/更新：再次 Save（同 active 行）应更新而非新增行。
	if err := store.Save(ctx, in); err != nil {
		t.Fatalf("Save (2nd): %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM xianyu_xianguanjia_config`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected single config row after re-save, got %d", n)
	}
}

func TestSupplyConfigSecretsEncryptedAtRest(t *testing.T) {
	db := newSupplyTestDB(t)
	store := NewSupplyConfigStore(db, fakeSupplyEncryptor{})
	ctx := context.Background()

	const plainApp = "supply-secret-placeholder"
	const plainMch = "mch-secret-placeholder"
	if err := store.Save(ctx, SupplyConfig{
		SupplyAppID: "1000000000000001", SupplyAppSecret: plainApp,
		MchID: "777777", MchSecret: plainMch,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 直接读裸列：两列必须为密文，且不含明文。
	var appEnc, mchEnc string
	if err := db.QueryRow(`SELECT supply_app_secret_encrypted, mch_secret_encrypted
		FROM xianyu_xianguanjia_config WHERE status='active'`).Scan(&appEnc, &mchEnc); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if appEnc == plainApp || mchEnc == plainMch {
		t.Fatalf("secret stored in plaintext: app=%q mch=%q", appEnc, mchEnc)
	}
	if strings.Contains(appEnc, plainApp) || strings.Contains(mchEnc, plainMch) {
		t.Fatalf("ciphertext contains plaintext: app=%q mch=%q", appEnc, mchEnc)
	}
	// 非密钥列（app_id / mch_id）为明文标识，可直接落库。
	var appID, mchID string
	if err := db.QueryRow(`SELECT supply_app_id, mch_id FROM xianyu_xianguanjia_config
		WHERE status='active'`).Scan(&appID, &mchID); err != nil {
		t.Fatalf("read ids: %v", err)
	}
	if appID != "1000000000000001" || mchID != "777777" {
		t.Fatalf("ids not stored as-is: app=%q mch=%q", appID, mchID)
	}
}

func TestSupplyConfigPartialFieldsIsNoConfig(t *testing.T) {
	db := newSupplyTestDB(t)
	store := NewSupplyConfigStore(db, fakeSupplyEncryptor{})
	ctx := context.Background()

	// active 行存在但密钥列为空（部分配置）→ 仍视为未配置，fail-closed。
	if _, err := db.Exec(`INSERT INTO xianyu_xianguanjia_config
		(supply_app_id, mch_id, status, health_status)
		VALUES ('1000000000000001', '777777', 'active', 'unknown')`); err != nil {
		t.Fatalf("insert partial: %v", err)
	}
	if _, err := store.Get(ctx); !errors.Is(err, ErrSupplyNoConfig) {
		t.Fatalf("Get (partial) = %v, want ErrSupplyNoConfig", err)
	}

	// 缺 mch_id 同样视为未配置。
	if _, err := db.Exec(`UPDATE xianyu_xianguanjia_config SET mch_id=''`); err != nil {
		t.Fatalf("clear mch_id: %v", err)
	}
	if _, err := store.Get(ctx); !errors.Is(err, ErrSupplyNoConfig) {
		t.Fatalf("Get (no mch_id) = %v, want ErrSupplyNoConfig", err)
	}
}

func TestSupplyConfigSaveKeepExistingSecret(t *testing.T) {
	db := newSupplyTestDB(t)
	store := NewSupplyConfigStore(db, fakeSupplyEncryptor{})
	ctx := context.Background()

	if err := store.Save(ctx, SupplyConfig{
		SupplyAppID: "1000000000000001", SupplyAppSecret: "supply-secret-placeholder",
		MchID: "777777", MchSecret: "mch-secret-placeholder",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// secret 留空 → 保留已存密文；app_id 变更生效。
	if err := store.Save(ctx, SupplyConfig{SupplyAppID: "1000000000000002", MchID: "777777"}); err != nil {
		t.Fatalf("Save (keep secrets): %v", err)
	}
	got, err := store.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SupplyAppSecret != "supply-secret-placeholder" || got.MchSecret != "mch-secret-placeholder" {
		t.Fatalf("secrets not preserved: %+v", got)
	}
	if got.SupplyAppID != "1000000000000002" {
		t.Fatalf("app_id not updated: %+v", got)
	}

	// 首次保存且两个 secret 均空 → 报错、不落行。
	fresh := newSupplyTestDB(t)
	freshStore := NewSupplyConfigStore(fresh, fakeSupplyEncryptor{})
	if err := freshStore.Save(ctx, SupplyConfig{SupplyAppID: "1", MchID: "2"}); err == nil {
		t.Fatalf("expected error on first save without secrets")
	}
}

// TestSupplyConfigSharesRowWithERP 验证货源配置与 ERP ConfigStore 共用 252 表同一行：
// 货源 Save 不新建行、不覆盖 ERP 方向列；已有 ERP 行时反向亦然。
func TestSupplyConfigSharesRowWithERP(t *testing.T) {
	db := newSupplyTestDB(t)
	ctx := context.Background()

	// 先由 ERP 写侧写入 active 行（含 ERP 列）。
	erp := NewConfigStore(db)
	if err := erp.UpsertConfig(ctx, ConfigUpsert{
		BaseURL: "https://open.goofish.pro", AppID: "erp-app",
		AppSecretEncrypted: "enc:erp-secret", PushURL: "https://push", Status: "active",
	}); err != nil {
		t.Fatalf("ERP UpsertConfig: %v", err)
	}

	// 货源写侧保存 → 复用同一 active 行。
	supply := NewSupplyConfigStore(db, fakeSupplyEncryptor{})
	if err := supply.Save(ctx, SupplyConfig{
		SupplyAppID: "1000000000000001", SupplyAppSecret: "supply-secret-placeholder",
		MchID: "777777", MchSecret: "mch-secret-placeholder",
	}); err != nil {
		t.Fatalf("supply Save: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM xianyu_xianguanjia_config`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected shared single row, got %d", n)
	}

	// ERP 列必须原样保留。
	erpRow, err := erp.GetConfigRow(ctx)
	if err != nil || erpRow == nil {
		t.Fatalf("ERP GetConfigRow = (%+v, %v)", erpRow, err)
	}
	if erpRow.AppID != "erp-app" || erpRow.AppSecretEncrypted != "enc:erp-secret" ||
		erpRow.BaseURL != "https://open.goofish.pro" || erpRow.PushURL != "https://push" {
		t.Fatalf("ERP columns clobbered by supply save: %+v", erpRow)
	}

	// 反向：ERP 再保存不覆盖货源四列。
	if err := erp.UpsertConfig(ctx, ConfigUpsert{
		BaseURL: "https://open.goofish.pro", AppID: "erp-app-2",
		AppSecretEncrypted: "enc:erp-secret-2", PushURL: "https://push", Status: "active",
	}); err != nil {
		t.Fatalf("ERP UpsertConfig (2nd): %v", err)
	}
	sc, err := supply.Get(ctx)
	if err != nil {
		t.Fatalf("supply Get after ERP save: %v", err)
	}
	if sc.SupplyAppID != "1000000000000001" || sc.MchID != "777777" ||
		sc.SupplyAppSecret != "supply-secret-placeholder" || sc.MchSecret != "mch-secret-placeholder" {
		t.Fatalf("supply columns clobbered by ERP save: %+v", sc)
	}
}
