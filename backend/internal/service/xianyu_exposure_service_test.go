package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

)

// ---- mock 依赖 ----

type mockFactsStore struct {
	facts  map[int64]string
	updates map[int64]string
}

func (m *mockFactsStore) GetProductFacts(ctx context.Context, productID int64) (string, error) {
	return m.facts[productID], nil
}
func (m *mockFactsStore) UpdateProductFacts(ctx context.Context, productID int64, facts string) error {
	if m.updates == nil {
		m.updates = map[int64]string{}
	}
	m.updates[productID] = facts
	return nil
}

type mockOrderCounter struct {
	counts map[string]int
}

func (m *mockOrderCounter) CountClaimsByItem(ctx context.Context) (map[string]int, error) {
	return m.counts, nil
}

type mockSettingsReader struct {
	settings XianyuSettings
}

func (m *mockSettingsReader) GetSettings(ctx context.Context) (XianyuSettings, error) {
	return m.settings, nil
}

type mockWorkerClient struct {
	searchResults map[string]*XianyuSearchOnceResult
	detail        *XianyuItemDetailInfo
	detailErr     error
}

func (m *mockWorkerClient) SearchKeyword(ctx context.Context, keyword string, rowsPerPage int) (*XianyuSearchOnceResult, error) {
	if m.searchResults == nil {
		return &XianyuSearchOnceResult{Items: []XianyuSearchItem{}}, nil
	}
	r, ok := m.searchResults[keyword]
	if !ok {
		return &XianyuSearchOnceResult{Items: []XianyuSearchItem{}}, nil
	}
	return r, nil
}
func (m *mockWorkerClient) GetItemDetail(ctx context.Context, accountID, itemID string) (*XianyuItemDetailInfo, error) {
	return m.detail, m.detailErr
}

type mockProductLister struct {
	products []XianyuProduct
}

func (m *mockProductLister) ListProducts(ctx context.Context) ([]XianyuProduct, error) {
	return m.products, nil
}

// ---- 测试辅助 ----

func testProduct(id int64, itemID, title string, daysOnSale int) XianyuProduct {
	return XianyuProduct{
		ID:        id,
		AccountID: "acc1",
		ItemID:    itemID,
		Title:     title,
		Status:    "active",
		CreatedAt: time.Now().Add(-time.Duration(daysOnSale) * 24 * time.Hour),
	}
}

func testExposureService(t *testing.T, mocks ...any) (*XianyuExposureService, *httptest.Server, *httptest.Server) {
	t.Helper()
	var facts *mockFactsStore
	var orders *mockOrderCounter
	var settings *mockSettingsReader
	var worker *mockWorkerClient
	var products *mockProductLister
	var aiServer, wecomServer *httptest.Server

	for _, m := range mocks {
		switch v := m.(type) {
		case *mockFactsStore:
			facts = v
		case *mockOrderCounter:
			orders = v
		case *mockSettingsReader:
			settings = v
		case *mockWorkerClient:
			worker = v
		case *mockProductLister:
			products = v
		}
	}
	if facts == nil {
		facts = &mockFactsStore{facts: map[int64]string{}}
	}
	if orders == nil {
		orders = &mockOrderCounter{counts: map[string]int{}}
	}
	if settings == nil {
		settings = &mockSettingsReader{}
	}
	if worker == nil {
		worker = &mockWorkerClient{}
	}
	if products == nil {
		products = &mockProductLister{}
	}

	// AI mock server
	aiServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "Kimi K3 API Key日卡 24小时不限量 自动发货\n---\n关键词热度支撑"},
			}},
		})
	}))
	// wecom mock server
	wecomServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"errcode": 0})
	}))

	// 只补 URL/凭据空字段，不动调用方已设置的 enabled/stale/maxOrders
	if settings.settings.ExposureWecomWebhook == "" {
		settings.settings.ExposureWecomWebhook = wecomServer.URL
	}
	if settings.settings.ExposureAIBaseURL == "" {
		settings.settings.ExposureAIBaseURL = aiServer.URL
	}
	if settings.settings.ExposureAIAPIKey == "" {
		settings.settings.ExposureAIAPIKey = "test-key"
	}
	if settings.settings.ExposureAIModel == "" {
		settings.settings.ExposureAIModel = "test-model"
	}
	if settings.settings.ExposureStaleDays == 0 {
		settings.settings.ExposureStaleDays = 7
	}
	if settings.settings.ExposureMaxOrders == 0 {
		settings.settings.ExposureMaxOrders = 1
	}

	svc := NewXianyuExposureService(facts, orders, settings, worker, products, nil)
	// 覆盖 http client 用真实 server
	svc.httpClient = http.DefaultClient
	return svc, aiServer, wecomServer
}

func TestExposureRunOnce_GeneratesAndPushes(t *testing.T) {
	ctx := context.Background()
	products := &mockProductLister{products: []XianyuProduct{
		testProduct(1, "item1", "Kimi k3 月之暗面大模型24h不限次数不限额畅享体验卡", 10),
	}}
	orders := &mockOrderCounter{counts: map[string]int{"item1": 0}}
	worker := &mockWorkerClient{
		searchResults: map[string]*XianyuSearchOnceResult{
			"kimi": {Items: []XianyuSearchItem{{Title: "Kimi K3 API日卡", WantCount: 50}}},
		},
		detail: &XianyuItemDetailInfo{ItemID: "item1", Title: "Kimi", Desc: "Kimi K3 官方API，24小时不限量，自动发货"},
	}
	facts := &mockFactsStore{facts: map[int64]string{}}

	settings := &mockSettingsReader{settings: XianyuSettings{ExposureEnabled: true}}
	svc, _, _ := testExposureService(t, products, orders, worker, facts, settings)
	svc.runOnce(ctx)

	if len(facts.updates) != 1 {
		t.Fatalf("expected facts to be built, got %d", len(facts.updates))
	}
}

func TestExposureRunOnce_DisabledSkips(t *testing.T) {
	ctx := context.Background()
	products := &mockProductLister{products: []XianyuProduct{
		testProduct(1, "item1", "Kimi K3 体验卡", 10),
	}}
	settings := &mockSettingsReader{settings: XianyuSettings{ExposureEnabled: false}}
	svc, _, _ := testExposureService(t, products, settings)
	svc.runOnce(ctx)
	// 未配置 webhook/AI 也不应 panic；开关关闭直接 return
}

func TestExposureRunOnce_FreshProductSkipped(t *testing.T) {
	ctx := context.Background()
	products := &mockProductLister{products: []XianyuProduct{
		testProduct(1, "item1", "Kimi K3 体验卡", 2), // 上架2天 < 7天
	}}
	orders := &mockOrderCounter{counts: map[string]int{}}
	facts := &mockFactsStore{facts: map[int64]string{}}
	svc, _, _ := testExposureService(t, products, orders, facts)
	svc.runOnce(ctx)
	if len(facts.updates) != 0 {
		t.Fatalf("expected no facts build for fresh product, got %d", len(facts.updates))
	}
}

func TestExposureRunOnce_ProductWithOrdersExcluded(t *testing.T) {
	ctx := context.Background()
	products := &mockProductLister{products: []XianyuProduct{
		testProduct(1, "item1", "Kimi K3 体验卡", 10),
	}}
	orders := &mockOrderCounter{counts: map[string]int{"item1": 3}} // 3 单 >= maxOrders(1)
	facts := &mockFactsStore{facts: map[int64]string{}}
	svc, _, _ := testExposureService(t, products, orders, facts)
	svc.runOnce(ctx)
	if len(facts.updates) != 0 {
		t.Fatalf("expected no facts build for high-order product, got %d", len(facts.updates))
	}
}

func TestExposureRunOnce_AIFailureDegrades(t *testing.T) {
	ctx := context.Background()
	products := &mockProductLister{products: []XianyuProduct{
		testProduct(1, "item1", "Kimi K3 体验卡", 10),
	}}
	orders := &mockOrderCounter{counts: map[string]int{}}
	facts := &mockFactsStore{facts: map[int64]string{
		1: `{"model":"Kimi K3","quota":"24小时不限量","delivery":"自动发货"}`,
	}}
	// AI server 返回 500
	aiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	wecomServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"errcode": 0})
	}))
	settings := &mockSettingsReader{settings: XianyuSettings{
		ExposureEnabled:      true,
		ExposureWecomWebhook: wecomServer.URL,
		ExposureAIBaseURL:    aiServer.URL,
		ExposureAIAPIKey:     "k",
		ExposureAIModel:      "m",
		ExposureStaleDays:    7,
		ExposureMaxOrders:    1,
	}}
	svc := NewXianyuExposureService(facts, orders, settings, &mockWorkerClient{}, products, nil)
	svc.httpClient = http.DefaultClient
	svc.runOnce(ctx) // 不应 panic，应安静跳过
}

func TestExtractKeywords(t *testing.T) {
	got := extractKeywords("Kimi k3 月之暗面大模型 24h 不限次数 不限额 畅享体验卡")
	if len(got) == 0 {
		t.Fatal("expected keywords")
	}
}

func TestGenerateSuggestion_BlockedWordRejected(t *testing.T) {
	ctx := context.Background()
	// AI 返回含黑名单词的标题
	aiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "Kimi 官方 拼车 换绑 大甩卖\n---\n测试"},
			}},
		})
	}))
	defer aiServer.Close()
	svc := &XianyuExposureService{httpClient: http.DefaultClient}
	cfg := XianyuSettings{ExposureAIBaseURL: aiServer.URL, ExposureAIAPIKey: "k", ExposureAIModel: "m", ExposureBlockedWords: "官方,拼车,换绑"}
	title, _, _ := svc.generateSuggestion(ctx, "Kimi 体验卡", `{"model":"Kimi K3"}`, map[string][]XianyuSearchItem{}, cfg)
	if title != "" {
		t.Fatalf("expected blocked-word title rejected, got %q", title)
	}
}
