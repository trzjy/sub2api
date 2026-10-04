package xianguanjia

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// 以下凭证均为单测占位，绝非真实凭证。
const testSupplyAppID int64 = 1783283558647493

func testSupplyCfg() *SupplyConfig {
	return &SupplyConfig{
		MchID:             "900001",
		MchSecret:         "enc-mch-secret",
		SupplyAppID:       "1783283558647493",
		SupplyAppSecret:   "enc-app-secret",
	}
}

func testGoods() []SupplyGoods {
	return []SupplyGoods{
		{GoodsNo: "11", GoodsName: "月卡套餐", GoodsType: SupplyGoodsTypeKami, Price: 2990, Stock: 5, Status: SupplyGoodsStatusOnSale, UpdateTime: 1700000000},
		{GoodsNo: "12", GoodsName: "周卡套餐", GoodsType: SupplyGoodsTypeKami, Price: 990, Stock: 0, Status: SupplyGoodsStatusOffSale, UpdateTime: 1700000001},
		{GoodsNo: "13", GoodsName: "年卡套餐", GoodsType: SupplyGoodsTypeKami, Price: 19900, Stock: 3, Status: SupplyGoodsStatusOnSale, UpdateTime: 1700000002},
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
	// 契约：features 三值固定 {true, false, true}（方案 §3 决策 2）。
	if !info.Features.IsSupportOrderRefund || info.Features.IsSupportGoodsNotify || !info.Features.IsSupportLossPurchase {
		t.Fatalf("features = %+v, want {true,false,true}", info.Features)
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
	if info.Balance <= 0 {
		t.Fatalf("Balance = %d, want > 0", info.Balance)
	}
	// 契约：data 仅含 balance，不得有 mch_id 键（JSON 断言）。
	raw, jerr := json.Marshal(info)
	if jerr != nil {
		t.Fatalf("marshal: %v", jerr)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["mch_id"]; ok {
		t.Fatalf("MerchantInfo JSON 不应含 mch_id 键: %s", string(raw))
	}
	if _, ok := m["balance"]; !ok {
		t.Fatalf("MerchantInfo JSON 必须含 balance 键: %s", string(raw))
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
	if res.Count != 3 || len(res.List) != 3 {
		t.Fatalf("count=%d len=%d, want 3/3", res.Count, len(res.List))
	}
	// 全部为卡密商品，GoodsType 恒 2。
	for _, g := range res.List {
		if g.GoodsType != SupplyGoodsTypeKami {
			t.Fatalf("GoodsType = %d, want %d", g.GoodsType, SupplyGoodsTypeKami)
		}
	}
}

func TestSupplyListGoodsKeywordFilter(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{Keyword: "月卡", GoodsType: SupplyGoodsTypeKami})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Count != 1 || len(res.List) != 1 || res.List[0].GoodsName != "月卡套餐" {
		t.Fatalf("unexpected filter result: %+v", res)
	}
}

// TestSupplyListGoodsKeywordExactCode 验证 keyword 支持商品编码精准命中（goods_no = 分组 id）。
func TestSupplyListGoodsKeywordExactCode(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{Keyword: "12", GoodsType: SupplyGoodsTypeKami})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Count != 1 || len(res.List) != 1 || res.List[0].GoodsNo != "12" {
		t.Fatalf("keyword exact code match failed: %+v", res)
	}
}

// TestSupplyListGoodsKeywordNameForNumericName 验证纯名称模糊（kw 非数字时只匹配名称）。
func TestSupplyListGoodsKeywordNameOnly(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	// "卡" 非数字，只做名称模糊，应命中 月卡/周卡/年卡 三件。
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{Keyword: "卡", GoodsType: SupplyGoodsTypeKami})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Count != 3 {
		t.Fatalf("keyword name fuzzy want 3, got %d: %+v", res.Count, res.List)
	}
}

func TestSupplyListGoodsEmpty(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{Keyword: "不存在的商品"})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Count != 0 || len(res.List) != 0 {
		t.Fatalf("count=%d len=%d, want 0/0", res.Count, len(res.List))
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
	if res.Count != 3 || len(res.List) != 1 {
		t.Fatalf("count=%d len=%d, want 3/1", res.Count, len(res.List))
	}
	if res.List[0].GoodsNo != "13" {
		t.Fatalf("page2 first = %s, want 13", res.List[0].GoodsNo)
	}
}

func TestSupplyListGoodsNormalizePagination(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	// page_no=0 / page_size=-5 归一到默认 1/20，应返回全部 3 件（不回显分页字段）。
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{PageNo: 0, PageSize: -5})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Count != 3 || len(res.List) != 3 {
		t.Fatalf("normalize want 3/3, got count=%d len=%d", res.Count, len(res.List))
	}
}

// TestSupplyListGoodsNonKamiTypeEmpty goods_type=1（直充）应返回空列表。
func TestSupplyListGoodsNonKamiTypeEmpty(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{GoodsType: 1})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Count != 0 || len(res.List) != 0 {
		t.Fatalf("non-kami type should be empty, got %+v", res)
	}
}

// TestSupplyListGoodsTypeThreeEmpty goods_type=3（券码）应返回空列表。
func TestSupplyListGoodsTypeThreeEmpty(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{GoodsType: 3})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Count != 0 || len(res.List) != 0 {
		t.Fatalf("goods_type=3 should be empty, got %+v", res)
	}
}

// TestSupplyListGoodsPageSizeCap page_size=101 应截断为官方上限 100。
func TestSupplyListGoodsPageSizeCap(t *testing.T) {
	goods := make([]SupplyGoods, 150)
	for i := range goods {
		goods[i] = SupplyGoods{
			GoodsNo:    strconv.Itoa(i + 1),
			GoodsName:  "商品批量",
			GoodsType:  SupplyGoodsTypeKami,
			Price:      int64((i + 1) * 100),
			Stock:      1,
			Status:     SupplyGoodsStatusOnSale,
			UpdateTime: int64(i + 1700000000),
		}
	}
	// 用唯一名称避免 keyword 命中歧义：此处不使用 keyword。
	svc := newCatalogService(testSupplyCfg(), goods)
	res, err := svc.ListGoods(context.Background(), ListGoodsRequest{GoodsType: SupplyGoodsTypeKami, PageNo: 1, PageSize: 101})
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if res.Count != 150 {
		t.Fatalf("count = %d, want 150", res.Count)
	}
	if len(res.List) != supplyMaxPageSize {
		t.Fatalf("len = %d, want capped to %d", len(res.List), supplyMaxPageSize)
	}
}

func TestSupplyGoodsDetailFound(t *testing.T) {
	svc := newCatalogService(testSupplyCfg(), testGoods())
	g, err := svc.GoodsDetail(context.Background(), "12")
	if err != nil {
		t.Fatalf("GoodsDetail err = %v", err)
	}
	if g.GoodsNo != "12" || g.GoodsName != "周卡套餐" || g.Status != SupplyGoodsStatusOffSale {
		t.Fatalf("unexpected goods: %+v", g)
	}
	if g.GoodsType != SupplyGoodsTypeKami {
		t.Fatalf("GoodsType = %d, want %d", g.GoodsType, SupplyGoodsTypeKami)
	}
	if g.Price != 990 {
		t.Fatalf("Price = %d, want 990 (分)", g.Price)
	}
	if g.UpdateTime != 1700000001 {
		t.Fatalf("UpdateTime = %d, want 1700000001", g.UpdateTime)
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
	rows := sqlmock.NewRows([]string{"id", "name", "status", "price", "stock", "updated_at"}).
		AddRow(int64(11), "月卡套餐", "active", 29.9, 5, time.Unix(1700000000, 0).UTC())
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT`) + `\s+g\.id`).
		WithArgs(int64(11)).
		WillReturnRows(rows)

	src := NewSupplyGoodsSource(db)
	g, err := src.GetGoods(context.Background(), "11")
	if err != nil {
		t.Fatalf("GetGoods err = %v", err)
	}
	if g == nil || g.GoodsNo != "11" {
		t.Fatalf("unexpected goods: %+v", g)
	}
	if g.Status != SupplyGoodsStatusOnSale {
		t.Fatalf("Status = %d, want %d (1=在架)", g.Status, SupplyGoodsStatusOnSale)
	}
	if g.GoodsType != SupplyGoodsTypeKami {
		t.Fatalf("GoodsType = %d, want %d", g.GoodsType, SupplyGoodsTypeKami)
	}
	if g.Stock != 5 {
		t.Fatalf("Stock = %d, want 5", g.Stock)
	}
	// DB 元值 29.9 → 分 2990。
	if g.Price != 2990 {
		t.Fatalf("Price = %d, want 2990 (元29.9换算分)", g.Price)
	}
	if g.UpdateTime != 1700000000 {
		t.Fatalf("UpdateTime = %d, want 1700000000", g.UpdateTime)
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
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "price", "stock", "updated_at"}))

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
	// "月" 非数字 → 仅名称模糊，args=["%月%"]。
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM \(`).
		WithArgs("%月%").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`FROM "groups" g WHERE`).
		WithArgs("%月%", 2, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "price", "stock", "updated_at"}).
			AddRow(int64(11), "月卡套餐", "active", 29.9, 5, time.Unix(1700000000, 0).UTC()))

	src := NewSupplyGoodsSource(db)
	list, total, err := src.ListGoods(context.Background(), "月", 0, 2)
	if err != nil {
		t.Fatalf("ListGoods err = %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].GoodsNo != "11" {
		t.Fatalf("unexpected list: total=%d list=%+v", total, list)
	}
	// 验证元→分换算。
	if list[0].Price != 2990 {
		t.Fatalf("Price = %d, want 2990", list[0].Price)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestSupplyGoodsSourceDBListGoodsNumericKeyword 验证 DB 源 numeric keyword 走 OR 精准编码。
func TestSupplyGoodsSourceDBListGoodsNumericKeyword(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// kw="11" 为数字 → args=["%11%", "11"]，SQL 含 (g.name ILIKE $1 OR g.id = $2)。
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM \(`).
		WithArgs("%11%", "11").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`FROM "groups" g WHERE`).
		WithArgs("%11%", "11", 10, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "price", "stock", "updated_at"}).
			AddRow(int64(11), "月卡套餐", "active", 29.9, 5, time.Unix(1700000000, 0).UTC()))

	src := NewSupplyGoodsSource(db)
	list, total, err := src.ListGoods(context.Background(), "11", 0, 10)
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
