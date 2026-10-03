package xianguanjia

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// 以下凭证均为单测占位，绝非真实凭证。
const (
	testSupplyAppID int64 = 1783283558647493
	testSupplyMchID int64 = 900001
)

func testSupplyCfg() *SupplyConfig {
	return &SupplyConfig{
		MchID:                    "900001",
		MchSecretEncrypted:       "enc-mch-secret",
		SupplyAppID:              "1783283558647493",
		SupplyAppSecretEncrypted: "enc-app-secret",
		Status:                   "active",
	}
}

func testGoods() []SupplyGoods {
	return []SupplyGoods{
		{GoodsNo: "11", GoodsName: "月卡套餐", Price: 29.9, Stock: 5, GoodsStatus: SupplyGoodsStatusAvailable},
		{GoodsNo: "12", GoodsName: "周卡套餐", Price: 9.9, Stock: 0, GoodsStatus: SupplyGoodsStatusUnavailable},
		{GoodsNo: "13", GoodsName: "年卡套餐", Price: 199, Stock: 3, GoodsStatus: SupplyGoodsStatusAvailable},
	}
}

func newCatalogService(cfg *SupplyConfig, goods []SupplyGoods) *SupplyCatalogService {
	return NewSupplyCatalogService(
		NewSupplyConfigReaderMemory(cfg),
		NewSupplyGoodsSourceMemory(goods),
	)
}

func TestSupplyPlatformInfo(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), nil)
	info, err := svc.PlatformInfo(context.Background())
	if err != nil {
		t.Fatalf("PlatformInfo err = %v", err)
	}
	if info.AppID != testSupplyAppID {
		t.Fatalf("AppID = %d, want %d", info.AppID, testSupplyAppID)
	}
}

func TestSupplyPlatformInfoNoConfig(t *testing.T) {
	svc := NewSupplyCatalogService(NewSupplyConfigReaderMemory(nil), NewSupplyGoodsSourceMemory(nil))
	if _, err := svc.PlatformInfo(context.Background()); !errors.Is(err, ErrNoActiveConfig) {
		t.Fatalf("err = %v, want ErrNoActiveConfig", err)
	}
}

func TestSupplyPlatformInfoNonIntegerAppID(t *testing.T) {
	cfg := testSupplyCfg()
	cfg.SupplyAppID = "not-a-number"
	svc := newCatalogService(cfg, nil)
	if _, err := svc.PlatformInfo(context.Background()); err == nil {
		t.Fatal("want error for non-integer supply_app_id")
	}
}

func TestSupplyMerchantInfo(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), nil)
	info, err := svc.MerchantInfo(context.Background())
	if err != nil {
		t.Fatalf("MerchantInfo err = %v", err)
	}
	if info.MchID != testSupplyMchID {
		t.Fatalf("MchID = %d, want %d", info.MchID, testSupplyMchID)
	}
	if info.Balance <= 0 {
		t.Fatalf("Balance = %d, want > 0", info.Balance)
	}
}

func TestSupplyMerchantInfoNoConfig(t *testing.T) {
	svc := NewSupplyCatalogService(NewSupplyConfigReaderMemory(nil), NewSupplyGoodsSourceMemory(nil))
	if _, err := svc.MerchantInfo(context.Background()); !errors.Is(err, ErrNoActiveConfig) {
		t.Fatalf("err = %v, want ErrNoActiveConfig", err)
	}
}

func TestSupplyListGoodsNormal(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{GoodsType: SupplyGoodsTypeKami, PageNo: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Total != 3 || len(res.List) != 3 {
		t.Fatalf("total=%d len=%d, want 3/3", res.Total, len(res.List))
	}
}

func TestSupplyListGoodsKeywordFilter(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{Keyword: "月卡", GoodsType: SupplyGoodsTypeKami})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Total != 1 || len(res.List) != 1 || res.List[0].GoodsName != "月卡套餐" {
		t.Fatalf("unexpected filter result: %+v", res)
	}
}

func TestSupplyListGoodsEmpty(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{Keyword: "不存在的商品"})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Total != 0 || len(res.List) != 0 {
		t.Fatalf("total=%d len=%d, want 0/0", res.Total, len(res.List))
	}
	if res.List == nil {
		t.Fatal("List should be non-nil empty slice (JSON [] not null)")
	}
}

func TestSupplyListGoodsPagination(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{PageNo: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Total != 3 || len(res.List) != 1 {
		t.Fatalf("total=%d len=%d, want 3/1", res.Total, len(res.List))
	}
	if res.PageNo != 2 || res.PageSize != 2 {
		t.Fatalf("page echo = %d/%d, want 2/2", res.PageNo, res.PageSize)
	}
	if res.List[0].GoodsNo != "13" {
		t.Fatalf("page2 first = %s, want 13", res.List[0].GoodsNo)
	}
}

func TestSupplyListGoodsNormalizePagination(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{PageNo: 0, PageSize: -5})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.PageNo != supplyDefaultPageNo || res.PageSize != supplyDefaultPageSize {
		t.Fatalf("normalize = %d/%d, want %d/%d", res.PageNo, res.PageSize, supplyDefaultPageNo, supplyDefaultPageSize)
	}
}

func TestSupplyListGoodsNonKamiTypeEmpty(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{GoodsType: 1})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Total != 0 || len(res.List) != 0 {
		t.Fatalf("non-kami type should be empty, got %+v", res)
	}
}

func TestSupplyGoodsDetailFound(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	g, err := svc.GoodsDetail(context.Background(), "12")
	if err != nil {
		t.Fatalf("GoodsDetail err = %v", err)
	}
	if g.GoodsNo != "12" || g.GoodsName != "周卡套餐" || g.GoodsStatus != SupplyGoodsStatusUnavailable {
		t.Fatalf("unexpected goods: %+v", g)
	}
}

func TestSupplyGoodsDetailNotFound(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	_, err := svc.GoodsDetail(context.Background(), "9999")
	if !errors.Is(err, ErrSupplyGoodsNotFound) {
		t.Fatalf("err = %v, want ErrSupplyGoodsNotFound", err)
	}
	var apiErr *SupplyAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != SupplyCodeGoodsNotFound {
		t.Fatalf("err code = %v, want %d", err, SupplyCodeGoodsNotFound)
	}
}

func TestSupplyGoodsDetailEmptyNo(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	if _, err := svc.GoodsDetail(context.Background(), "  "); !errors.Is(err, ErrSupplyGoodsNotFound) {
		t.Fatalf("err = %v, want ErrSupplyGoodsNotFound", err)
	}
}

// ---- DB 数据源（sqlmock） ----

func TestSupplyGoodsSourceDBGetGoodsFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	rows := sqlmock.NewRows([]string{"id", "name", "status", "price", "stock"}).
		AddRow(int64(11), "月卡套餐", "active", 29.9, 5)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT`) + `\s+g\.id`).
		WithArgs(int64(11)).
		WillReturnRows(rows)

	src := NewSupplyGoodsSource(db)
	g, err := src.GetGoods(context.Background(), "11")
	if err != nil {
		t.Fatalf("GetGoods err = %v", err)
	}
	if g == nil || g.GoodsNo != "11" || g.GoodsStatus != SupplyGoodsStatusAvailable || g.Stock != 5 {
		t.Fatalf("unexpected goods: %+v", g)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSupplyGoodsSourceDBGetGoodsNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT`) + `\s+g\.id`).
		WithArgs(int64(9999)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "price", "stock"}))

	src := NewSupplyGoodsSource(db)
	g, err := src.GetGoods(context.Background(), "9999")
	if err != nil {
		t.Fatalf("GetGoods err = %v", err)
	}
	if g != nil {
		t.Fatalf("want nil for missing goods, got %+v", g)
	}
}

func TestSupplyGoodsSourceDBGetGoodsNonNumeric(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// 非数字 goods_no 不查库，直接归一为不存在。
	src := NewSupplyGoodsSource(db)
	g, err := src.GetGoods(context.Background(), "goods-abc")
	if err != nil || g != nil {
		t.Fatalf("got (%v,%v), want (nil,nil)", g, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected query issued: %v", err)
	}
}

func TestSupplyGoodsSourceDBListGoods(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM \(`).
		WithArgs("%月%").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`FROM "groups" g WHERE`).
		WithArgs("%月%", 2, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "price", "stock"}).
			AddRow(int64(11), "月卡套餐", "active", 29.9, 5))

	src := NewSupplyGoodsSource(db)
	list, total, err := src.ListGoods(context.Background(), "月", 0, 2)
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].GoodsNo != "11" {
		t.Fatalf("unexpected list: total=%d list=%+v", total, list)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
