package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
)

// fakeSupplyCatalogService 实现 SupplyCatalogService，供 handler 测试注入。
type fakeSupplyCatalogService struct {
	platform *xianguanjia.SupplyPlatformInfo
	merchant *xianguanjia.SupplyMerchantInfo
	goods    []xianguanjia.SupplyGoods
	listErr  error
	getErr   error
}

func (f *fakeSupplyCatalogService) PlatformInfo(ctx context.Context) (*xianguanjia.SupplyPlatformInfo, error) {
	return f.platform, nil
}

func (f *fakeSupplyCatalogService) MerchantInfo(ctx context.Context) (*xianguanjia.SupplyMerchantInfo, error) {
	return f.merchant, nil
}

func (f *fakeSupplyCatalogService) ListGoods(ctx context.Context, req xianguanjia.ListGoodsRequest) (*xianguanjia.ListGoodsResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	pageNo, pageSize := req.PageNo, req.PageSize
	if pageNo < 1 {
		pageNo = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	all := f.goods
	start := (pageNo - 1) * pageSize
	if start > len(all) {
		start = len(all)
	}
	end := start + pageSize
	if end > len(all) {
		end = len(all)
	}
	return &xianguanjia.ListGoodsResult{
		List:     all[start:end],
		Total:    len(all),
		PageNo:   pageNo,
		PageSize: pageSize,
	}, nil
}

func (f *fakeSupplyCatalogService) GoodsDetail(ctx context.Context, goodsNo string) (*xianguanjia.SupplyGoods, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	for i := range f.goods {
		if f.goods[i].GoodsNo == goodsNo {
			g := f.goods[i]
			return &g, nil
		}
	}
	return nil, xianguanjia.ErrSupplyGoodsNotFound
}

func newSupplyTestHandler(fake *fakeSupplyCatalogService) *XianguanjiaSupplyHandler {
	return NewXianguanjiaSupplyHandler(fake)
}

// doSupply 用给定绑定方法（h.Method 形式的方法值）在 gin test context 上执行请求。
func doSupply(t *testing.T, invoke func(*gin.Context), body []byte) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/xgj-supply/test", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	invoke(c)
	return w
}

// decodeSupplyEnvelope 解析响应为通用 map，便于断言字段 JSON 类型（类型纪律）。
func decodeSupplyEnvelope(t *testing.T, w *httptest.ResponseRecorder) (int, map[string]any) {
	t.Helper()
	var raw struct {
		Code int            `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode envelope: %v, body=%s", err, w.Body.String())
	}
	return raw.Code, raw.Data
}

func testSupplyGoodsFixture() []xianguanjia.SupplyGoods {
	return []xianguanjia.SupplyGoods{
		{GoodsNo: "11", GoodsName: "月卡套餐", Price: 29.9, Stock: 5, GoodsStatus: 1},
		{GoodsNo: "12", GoodsName: "周卡套餐", Price: 9.9, Stock: 0, GoodsStatus: 0},
	}
}

func TestHandlerSupplyPlatformInfo(t *testing.T) {
	h := newSupplyTestHandler(&fakeSupplyCatalogService{platform: &xianguanjia.SupplyPlatformInfo{AppID: 1783283558647493}})
	w := doSupply(t, h.PlatformInfo, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	code, data := decodeSupplyEnvelope(t, w)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	// 类型纪律：app_id 必须是 JSON number（int64），不能是 string。
	appID, ok := data["app_id"].(float64)
	if !ok {
		t.Fatalf("app_id type = %T, want number", data["app_id"])
	}
	if int64(appID) != 1783283558647493 {
		t.Fatalf("app_id = %v, want 1783283558647493", appID)
	}
}

func TestHandlerSupplyMerchantInfoBalanceIsInteger(t *testing.T) {
	h := newSupplyTestHandler(&fakeSupplyCatalogService{merchant: &xianguanjia.SupplyMerchantInfo{MchID: 900001, Balance: 999999}})
	w := doSupply(t, h.MerchantInfo, nil)
	code, data := decodeSupplyEnvelope(t, w)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	// 类型纪律：balance 必须是 JSON number（整数），不能是字符串。
	balance, ok := data["balance"].(float64)
	if !ok {
		t.Fatalf("balance type = %T, want number (not string)", data["balance"])
	}
	if balance <= 0 {
		t.Fatalf("balance = %v, want > 0", balance)
	}
}

func TestHandlerSupplyListGoodsNormal(t *testing.T) {
	h := newSupplyTestHandler(&fakeSupplyCatalogService{goods: testSupplyGoodsFixture()})
	body, _ := json.Marshal(map[string]any{"goods_type": 2, "page_no": 1, "page_size": 20})
	w := doSupply(t, h.ListGoods, body)
	code, data := decodeSupplyEnvelope(t, w)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	list, ok := data["list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("list = %v, want 2 items", data["list"])
	}
	if total, _ := data["total"].(float64); int(total) != 2 {
		t.Fatalf("total = %v, want 2", data["total"])
	}
}

func TestHandlerSupplyListGoodsEmpty(t *testing.T) {
	h := newSupplyTestHandler(&fakeSupplyCatalogService{goods: nil})
	body, _ := json.Marshal(map[string]any{"keyword": "不存在"})
	w := doSupply(t, h.ListGoods, body)
	code, data := decodeSupplyEnvelope(t, w)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if list, ok := data["list"].([]any); !ok || len(list) != 0 {
		t.Fatalf("list = %v, want empty array", data["list"])
	}
}

func TestHandlerSupplyListGoodsPagination(t *testing.T) {
	h := newSupplyTestHandler(&fakeSupplyCatalogService{goods: testSupplyGoodsFixture()})
	body, _ := json.Marshal(map[string]any{"page_no": 2, "page_size": 1})
	w := doSupply(t, h.ListGoods, body)
	code, data := decodeSupplyEnvelope(t, w)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if pn, _ := data["page_no"].(float64); int(pn) != 2 {
		t.Fatalf("page_no echo = %v, want 2", data["page_no"])
	}
	list, _ := data["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("page2 len = %d, want 1", len(list))
	}
}

func TestHandlerSupplyGoodsDetailFound(t *testing.T) {
	h := newSupplyTestHandler(&fakeSupplyCatalogService{goods: testSupplyGoodsFixture()})
	body, _ := json.Marshal(map[string]any{"goods_no": "11"})
	w := doSupply(t, h.GoodsDetail, body)
	code, data := decodeSupplyEnvelope(t, w)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if data["goods_no"] != "11" {
		t.Fatalf("goods_no = %v, want 11", data["goods_no"])
	}
	// 类型纪律：stock 必须是 number。
	if _, ok := data["stock"].(float64); !ok {
		t.Fatalf("stock type = %T, want number", data["stock"])
	}
}

func TestHandlerSupplyGoodsDetailNotFound(t *testing.T) {
	h := newSupplyTestHandler(&fakeSupplyCatalogService{goods: testSupplyGoodsFixture()})
	body, _ := json.Marshal(map[string]any{"goods_no": "9999"})
	w := doSupply(t, h.GoodsDetail, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (envelope carries error)", w.Code)
	}
	code, _ := decodeSupplyEnvelope(t, w)
	if code != xianguanjia.SupplyCodeGoodsNotFound {
		t.Fatalf("code = %d, want %d", code, xianguanjia.SupplyCodeGoodsNotFound)
	}
}

func TestHandlerSupplyGoodsDetailInvalidBody(t *testing.T) {
	h := newSupplyTestHandler(&fakeSupplyCatalogService{goods: testSupplyGoodsFixture()})
	w := doSupply(t, h.GoodsDetail, []byte("{not json"))
	code, _ := decodeSupplyEnvelope(t, w)
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want %d", code, http.StatusBadRequest)
	}
}

func TestHandlerSupplyNilService(t *testing.T) {
	h := &XianguanjiaSupplyHandler{}
	w := doSupply(t, h.PlatformInfo, nil)
	code, _ := decodeSupplyEnvelope(t, w)
	if code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want %d", code, http.StatusInternalServerError)
	}
}
