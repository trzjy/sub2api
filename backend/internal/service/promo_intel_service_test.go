package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// ---------- 正文提取 ----------

func TestExtractPromoIntelText(t *testing.T) {
	html := `<html><head><title>x</title><meta name="a" content="b"></head>
	<body><script>var build="abc123";</script>
	<style>.a{color:red}</style>
	<h1>模型 &amp; 价格</h1>
	<p>降价 50%，&mdash;限时活动。</p>
	<noscript>请开启 JS</noscript>
	</body></html>`
	text, err := extractPromoIntelText([]byte(html))
	require.NoError(t, err)
	require.NotContains(t, text, "abc123")
	require.NotContains(t, text, "color:red")
	require.NotContains(t, text, "请开启 JS")
	require.NotContains(t, text, "<h1>")
	require.Contains(t, text, "模型 & 价格")
	require.Contains(t, text, "降价 50%，—限时活动。")
	// 空白折叠稳定。
	require.Equal(t, text, extractPromoIntelTextMust(t, strings.ReplaceAll(html, "\n", "  \t")))
}

func TestExtractPromoIntelTextUnclosedScript(t *testing.T) {
	// 未闭合 script：防御性丢弃剩余全部，不残留脚本内容。
	html := `<div>正文A</div><script>var x=1; if (a<b) {}`
	text, err := extractPromoIntelText([]byte(html))
	require.NoError(t, err)
	require.Contains(t, text, "正文A")
	require.NotContains(t, text, "var x=1")
}

func TestExtractPromoIntelTextEmpty(t *testing.T) {
	_, err := extractPromoIntelText([]byte("<div>   </div>"))
	require.Error(t, err)
	_, err = extractPromoIntelText(nil)
	require.Error(t, err)
}

func TestDecodePromoIntelEntitiesTerminates(t *testing.T) {
	// 混合非法/合法数字实体：必须终止且解出合法部分（回归死循环）。
	out := decodePromoIntelEntities("&#abc;&#65;&amp;x&#x42;")
	require.Contains(t, out, "A")
	require.Contains(t, out, "B")
	require.Contains(t, out, "&x")
	require.NotContains(t, out, "&#")
	// 孤立 &# 无分号：不解码，仅消费 '&'。
	out = decodePromoIntelEntities("a &#123 b")
	require.Equal(t, "a #123 b", out)
}

func extractPromoIntelTextMust(t *testing.T, html string) string {
	out, err := extractPromoIntelText([]byte(html))
	require.NoError(t, err)
	return out
}

// ---------- 指纹 ----------

func TestPromoIntelFingerprints(t *testing.T) {
	h1 := promoIntelContentHash("abc")
	require.Equal(t, h1, promoIntelContentHash("abc"))
	require.NotEqual(t, h1, promoIntelContentHash("abd"))

	// 降级指纹：同一源同一内容版本稳定；内容或源变化则变。
	require.Equal(t, promoIntelRawFingerprint("DeepSeek·定价", "h"), promoIntelRawFingerprint("DeepSeek·定价", "h"))
	require.NotEqual(t, promoIntelRawFingerprint("DeepSeek·定价", "h"), promoIntelRawFingerprint("DeepSeek·定价", "h2"))
	require.NotEqual(t, promoIntelRawFingerprint("Kimi·定价", "h"), promoIntelRawFingerprint("DeepSeek·定价", "h"))

	// 优惠指纹：标点/空白/大小写归一化。
	require.Equal(t,
		promoIntelOfferFingerprint("deepseek", "V3.2 半价限时优惠！"),
		promoIntelOfferFingerprint("deepseek", "v3.2 半价限时优惠"))
	require.NotEqual(t,
		promoIntelOfferFingerprint("deepseek", "半价"),
		promoIntelOfferFingerprint("kimi", "半价"))
}

// ---------- LLM 端点与 JSON 解析 ----------

func TestNormalizePromoIntelLLMEndpoint(t *testing.T) {
	require.Equal(t, "https://api.x.com/v1/chat/completions", normalizePromoIntelLLMEndpoint("https://api.x.com/v1"))
	require.Equal(t, "https://api.x.com/v1/chat/completions", normalizePromoIntelLLMEndpoint("https://api.x.com/v1/"))
	require.Equal(t, "https://api.x.com/v1/chat/completions", normalizePromoIntelLLMEndpoint("https://api.x.com/v1/chat/completions"))
}

func TestParsePromoIntelOffersJSON(t *testing.T) {
	// 干净数组。
	offers, err := parsePromoIntelOffersJSON(`[{"title":"首月5折","category":"subscription","relevance":"high","discount":"首月49元"}]`)
	require.NoError(t, err)
	require.Len(t, offers, 1)
	require.Equal(t, PromoIntelCategorySubscription, offers[0].Category)
	require.Equal(t, "首月49元", offers[0].Discount)

	// 代码围栏 + 前后噪音。
	fenced := "好的，以下是结果：\n```json\n[{\"title\":\"新模型上线\",\"relevance\":\"medium\"}]\n```"
	offers, err = parsePromoIntelOffersJSON(fenced)
	require.NoError(t, err)
	require.Len(t, offers, 1)
	require.Equal(t, PromoIntelCategoryOther, offers[0].Category) // 未知类型归 other

	// none 被丢弃；空标题被丢弃；非法 relevance 归 medium。
	offers, err = parsePromoIntelOffersJSON(`[
		{"title":"品牌宣传","relevance":"none"},
		{"title":"","relevance":"high"},
		{"title":"无相关性标注"},
		{"title":"免费额度","relevance":"super"}
	]`)
	require.NoError(t, err)
	require.Len(t, offers, 2)
	require.Equal(t, PromoIntelRelevanceMedium, offers[0].Relevance)
	require.Equal(t, PromoIntelRelevanceMedium, offers[1].Relevance)

	// 非法 JSON → 错误（调用方降级为原文待整理）。
	_, err = parsePromoIntelOffersJSON("完全不是 JSON")
	require.Error(t, err)
}

// ---------- fake 仓储 / settings ----------

type fakePromoIntelRepo struct {
	PromoIntelRepository

	sources        map[int64]*PromoIntelSource
	byName         map[string]*PromoIntelSource
	nextID         int64
	upserts        []*PromoIntelItem
	upsertCreated  []bool
	deletedPending []int64
	markFetched    []promoIntelMark

	items map[int64]*PromoIntelItem
}

type promoIntelMark struct {
	id     int64
	status string
	errMsg string
	hash   *string
}

func newFakePromoIntelRepo() *fakePromoIntelRepo {
	return &fakePromoIntelRepo{
		sources: map[int64]*PromoIntelSource{},
		byName:  map[string]*PromoIntelSource{},
		items:   map[int64]*PromoIntelItem{},
		nextID:  100,
	}
}

func (f *fakePromoIntelRepo) CreateSource(_ context.Context, src *PromoIntelSource) error {
	f.nextID++
	src.ID = f.nextID
	cp := *src
	f.sources[src.ID] = &cp
	f.byName[src.Name] = &cp
	return nil
}

func (f *fakePromoIntelRepo) GetSourceByID(_ context.Context, id int64) (*PromoIntelSource, error) {
	src, ok := f.sources[id]
	if !ok {
		return nil, ErrPromoIntelSourceNotFound
	}
	cp := *src
	return &cp, nil
}

func (f *fakePromoIntelRepo) GetSourceByName(_ context.Context, name string) (*PromoIntelSource, error) {
	src, ok := f.byName[name]
	if !ok {
		return nil, ErrPromoIntelSourceNotFound
	}
	cp := *src
	return &cp, nil
}

func (f *fakePromoIntelRepo) UpdateSource(_ context.Context, src *PromoIntelSource) error {
	cp := *src
	f.sources[src.ID] = &cp
	f.byName[src.Name] = &cp
	return nil
}

func (f *fakePromoIntelRepo) ListSources(_ context.Context, params PromoIntelSourceListParams) ([]*PromoIntelSource, int64, error) {
	out := make([]*PromoIntelSource, 0, len(f.sources))
	for _, s := range f.sources {
		cp := *s
		out = append(out, &cp)
	}
	return out, int64(len(out)), nil
}

func (f *fakePromoIntelRepo) MarkSourceFetched(_ context.Context, id int64, status, lastErr string, extractedHash *string, _ time.Time) error {
	src, ok := f.sources[id]
	if !ok {
		return ErrPromoIntelSourceNotFound
	}
	src.LastStatus = status
	src.LastError = lastErr
	if extractedHash != nil {
		src.LastExtractedHash = *extractedHash
	}
	f.markFetched = append(f.markFetched, promoIntelMark{id: id, status: status, errMsg: lastErr, hash: extractedHash})
	return nil
}

func (f *fakePromoIntelRepo) UpsertItem(_ context.Context, item *PromoIntelItem) (bool, error) {
	f.upserts = append(f.upserts, item)
	for _, existing := range f.items {
		if existing.Fingerprint == item.Fingerprint {
			created := false
			f.upsertCreated = append(f.upsertCreated, created)
			return created, nil
		}
	}
	f.nextID++
	item.ID = f.nextID
	cp := *item
	f.items[item.ID] = &cp
	f.upsertCreated = append(f.upsertCreated, true)
	return true, nil
}

func (f *fakePromoIntelRepo) DeletePendingItemsBySource(_ context.Context, sourceID int64) (int64, error) {
	f.deletedPending = append(f.deletedPending, sourceID)
	return 0, nil
}

func (f *fakePromoIntelRepo) ListItems(_ context.Context, params PromoIntelItemListParams) ([]*PromoIntelItem, int64, error) {
	out := []*PromoIntelItem{}
	for _, it := range f.items {
		if params.DigestDate != "" {
			if it.DigestDate == nil || it.DigestDate.Format("2006-01-02") != params.DigestDate {
				continue
			}
		}
		cp := *it
		out = append(out, &cp)
	}
	return out, int64(len(out)), nil
}

func (f *fakePromoIntelRepo) ListDueSources(_ context.Context, now time.Time, limit int) ([]*PromoIntelSource, error) {
	out := []*PromoIntelSource{}
	for _, s := range f.sources {
		if !s.Enabled {
			continue
		}
		if s.LastFetchedAt == nil || now.Sub(*s.LastFetchedAt) >= time.Duration(s.FetchIntervalMinutes)*time.Minute {
			cp := *s
			out = append(out, &cp)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

type fakePromoIntelSettings struct {
	SettingRepository
	vals map[string]string
}

func (f *fakePromoIntelSettings) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		out[k] = f.vals[k]
	}
	return out, nil
}

func (f *fakePromoIntelSettings) Set(_ context.Context, key, value string) error {
	f.vals[key] = value
	return nil
}

// ---------- ProcessSource 全链路 ----------

func newTestPromoIntelService(t *testing.T, repo *fakePromoIntelRepo, settings *fakePromoIntelSettings) *PromoIntelService {
	t.Helper()
	svc := NewPromoIntelService(repo, settings, &config.PromoIntelConfig{Enabled: true, SeedDefaults: false})
	svc.nowFn = func() time.Time { return time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC) }
	return svc
}

func newLLMServer(t *testing.T, content string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestProcessSourceLLMPath(t *testing.T) {
	repo := newFakePromoIntelRepo()
	settings := &fakePromoIntelSettings{vals: map[string]string{
		SettingKeyPromoIntelLLMModel:  "test-model",
		SettingKeyPromoIntelLLMAPIKey: "sk-secret",
	}}
	svc := newTestPromoIntelService(t, repo, settings)

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, promoIntelUserAgent, r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte("<html><body><p>DeepSeek 全线降价 50%，新用户注册送 1000 万 tokens。</p></body></html>"))
	}))
	defer page.Close()
	llm := newLLMServer(t, "```json\n[{\"title\":\"DeepSeek 全线降价50%\",\"category\":\"price_change\",\"relevance\":\"high\",\"discount\":\"降价50%\"},{\"title\":\"噪音\",\"relevance\":\"none\"}]\n```", nil)
	defer llm.Close()
	svc.testFetchClient = page.Client()
	svc.testLLMClient = llm.Client()
	// LLM base url 指向 httptest 测试服务器。
	settings.vals[SettingKeyPromoIntelLLMBaseURL] = llm.URL + "/v1"

	src := &PromoIntelSource{ID: 1, Name: "DeepSeek·定价", Vendor: "deepseek", URL: page.URL, LLMExtract: true, Enabled: true}
	repo.sources[1] = src

	res, err := svc.ProcessSource(context.Background(), src)
	require.NoError(t, err)
	require.True(t, res.ContentChanged)
	require.Equal(t, 1, res.ItemsCreated) // none 已被丢弃
	require.Len(t, repo.upserts, 1)
	require.Equal(t, PromoIntelExtractLLM, repo.upserts[0].ExtractStatus)
	require.Equal(t, "deepseek", repo.upserts[0].Vendor)
	require.Equal(t, PromoIntelItemStatusPending, repo.upserts[0].Status)
	require.Contains(t, repo.upserts[0].URL, "127.0.0.1")
	require.NotNil(t, repo.upserts[0].DigestDate)
	require.Equal(t, "2026-09-13", repo.upserts[0].DigestDate.Format("2006-01-02"))
	// 已整理指纹推进 + 待整理原文清理。
	require.Len(t, repo.markFetched, 1)
	require.NotNil(t, repo.markFetched[0].hash)
	require.Equal(t, []int64{1}, repo.deletedPending)
}

func TestProcessSourceUnchangedSkipsLLM(t *testing.T) {
	repo := newFakePromoIntelRepo()
	settings := &fakePromoIntelSettings{vals: map[string]string{
		SettingKeyPromoIntelLLMBaseURL: "http://llm.test/v1",
		SettingKeyPromoIntelLLMModel:   "test-model",
	}}
	svc := newTestPromoIntelService(t, repo, settings)

	body := "<p>stable content</p>"
	hash := promoIntelContentHash(mustExtract(t, body))
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer page.Close()
	svc.testFetchClient = page.Client()
	llmCalls := 0
	llm := newLLMServer(t, "[]", &llmCalls)
	defer llm.Close()
	svc.testLLMClient = llm.Client()

	src := &PromoIntelSource{ID: 1, Name: "S", Vendor: "x", URL: page.URL, Enabled: true, LastExtractedHash: hash}
	repo.sources[1] = src

	res, err := svc.ProcessSource(context.Background(), src)
	require.NoError(t, err)
	require.False(t, res.ContentChanged)
	require.Equal(t, "unchanged", res.SkippedReason)
	require.Equal(t, 0, llmCalls)
	require.Empty(t, repo.upserts)
}

func TestProcessSourceLLMNotConfiguredDegrades(t *testing.T) {
	repo := newFakePromoIntelRepo()
	svc := newTestPromoIntelService(t, repo, &fakePromoIntelSettings{vals: map[string]string{}})

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<p>content changed but no llm</p>"))
	}))
	defer page.Close()
	svc.testFetchClient = page.Client()

	src := &PromoIntelSource{ID: 1, Name: "S", Vendor: "x", URL: page.URL, Enabled: true, LLMExtract: true}
	repo.sources[1] = src

	res, err := svc.ProcessSource(context.Background(), src)
	require.NoError(t, err)
	require.True(t, res.ContentChanged)
	require.Equal(t, "llm_not_configured", res.SkippedReason)
	require.Len(t, repo.upserts, 1)
	require.Equal(t, PromoIntelExtractPending, repo.upserts[0].ExtractStatus)
	require.Contains(t, repo.upserts[0].Title, "待整理")
	// 已整理指纹不推进：配置 LLM 后自动补跑。
	require.Nil(t, repo.markFetched[0].hash)
}

func TestProcessSourceLLMFailureDegrades(t *testing.T) {
	repo := newFakePromoIntelRepo()
	settings := &fakePromoIntelSettings{vals: map[string]string{
		SettingKeyPromoIntelLLMBaseURL: "http://127.0.0.1:1/v1", // 不可达
		SettingKeyPromoIntelLLMModel:   "m",
	}}
	svc := newTestPromoIntelService(t, repo, settings)

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<p>content</p>"))
	}))
	defer page.Close()
	svc.testFetchClient = page.Client()

	src := &PromoIntelSource{ID: 1, Name: "S", Vendor: "x", URL: page.URL, Enabled: true, LLMExtract: true}
	repo.sources[1] = src

	_, err := svc.ProcessSource(context.Background(), src)
	require.Error(t, err)
	// 降级原文入库 + 状态 error + 指纹不推进。
	require.Len(t, repo.upserts, 1)
	require.Equal(t, PromoIntelExtractPending, repo.upserts[0].ExtractStatus)
	require.Nil(t, repo.markFetched[0].hash)
	require.Equal(t, PromoIntelFetchStatusError, repo.markFetched[0].status)
	require.Contains(t, repo.markFetched[0].errMsg, "llm")
}

func TestProcessSourceFetchFailure(t *testing.T) {
	repo := newFakePromoIntelRepo()
	svc := newTestPromoIntelService(t, repo, &fakePromoIntelSettings{vals: map[string]string{}})

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer page.Close()
	svc.testFetchClient = page.Client()

	src := &PromoIntelSource{ID: 1, Name: "S", Vendor: "x", URL: page.URL, Enabled: true, LLMExtract: true}
	repo.sources[1] = src

	_, err := svc.ProcessSource(context.Background(), src)
	require.Error(t, err)
	require.Equal(t, PromoIntelFetchStatusError, repo.markFetched[0].status)
	require.Contains(t, repo.markFetched[0].errMsg, "502")
}

func mustExtract(t *testing.T, body string) string {
	t.Helper()
	out, err := extractPromoIntelText([]byte(body))
	require.NoError(t, err)
	return out
}

// ---------- 种子幂等 ----------

func TestEnsureDefaultSourcesIdempotent(t *testing.T) {
	repo := newFakePromoIntelRepo()
	svc := newTestPromoIntelService(t, repo, &fakePromoIntelSettings{vals: map[string]string{}})

	require.NoError(t, svc.ensureDefaultSources(context.Background()))
	first := len(repo.sources)
	require.Greater(t, first, 0)
	// 再补种一轮：不新建、不覆盖。
	require.NoError(t, svc.ensureDefaultSources(context.Background()))
	require.Len(t, repo.sources, first)

	// 清单内容合法：URL 全部 http(s)，厂商满足校验。
	for _, def := range defaultPromoIntelSources() {
		require.NoError(t, validatePromoIntelURL(def.URL), def.Name)
		require.True(t, ValidatePromoIntelVendor(def.Vendor), def.Name)
	}
}

// ---------- 设置 ----------

func TestUpdateLLMSettingsKeyMasking(t *testing.T) {
	repo := newFakePromoIntelRepo()
	settings := &fakePromoIntelSettings{vals: map[string]string{
		SettingKeyPromoIntelLLMAPIKey: "sk-original-9876",
	}}
	svc := newTestPromoIntelService(t, repo, settings)

	// 掩码回写 → 保持不变。
	rt, err := svc.UpdateLLMSettings(context.Background(), PromoIntelSettingsUpdate{
		LLMAPIKey: promoStrPtr("sk-****9876"),
		LLMModel:  promoStrPtr("new-model"),
	})
	require.NoError(t, err)
	require.Equal(t, "sk-original-9876", settings.vals[SettingKeyPromoIntelLLMAPIKey])
	require.Equal(t, "new-model", settings.vals[SettingKeyPromoIntelLLMModel])
	require.True(t, rt.LLMConfigured == false) // base url 未配置

	// "-" 显式清空。
	_, err = svc.UpdateLLMSettings(context.Background(), PromoIntelSettingsUpdate{LLMAPIKey: promoStrPtr("-")})
	require.NoError(t, err)
	require.Empty(t, settings.vals[SettingKeyPromoIntelLLMAPIKey])

	// 正常写入 + 脱敏回读。
	_, err = svc.UpdateLLMSettings(context.Background(), PromoIntelSettingsUpdate{
		LLMBaseURL: promoStrPtr("https://api.deepseek.com/v1"),
		LLMAPIKey:  promoStrPtr("sk-abcd1234efgh5678"),
		LLMModel:   promoStrPtr("deepseek-chat"),
	})
	require.NoError(t, err)
	rt = svc.GetPromoIntelRuntime(context.Background())
	require.True(t, rt.LLMConfigured)
	require.True(t, rt.LLMAPIKeySet)
	require.Equal(t, "sk-****5678", rt.LLMAPIKeyMasked)
	require.NotContains(t, rt.LLMAPIKeyMasked, "abcd1234")
}

func TestGetPromoIntelRuntimeFailOpen(t *testing.T) {
	svc := NewPromoIntelService(newFakePromoIntelRepo(), nil, nil)
	rt := svc.GetPromoIntelRuntime(context.Background())
	require.True(t, rt.Enabled)
	require.False(t, rt.LLMConfigured)
}

func TestBriefingAggregation(t *testing.T) {
	repo := newFakePromoIntelRepo()
	svc := newTestPromoIntelService(t, repo, &fakePromoIntelSettings{vals: map[string]string{}})
	repo.items[1] = &PromoIntelItem{ID: 1, Vendor: "deepseek", Category: PromoIntelCategoryPriceChange,
		Relevance: PromoIntelRelevanceHigh, Status: PromoIntelItemStatusPending,
		DigestDate: promoDatePtr(time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC))}
	b, err := svc.GetBriefing(context.Background(), "2026-09-13")
	require.NoError(t, err)
	require.Equal(t, int64(1), b.Total)
	require.Equal(t, int64(1), b.HighCount)
	require.Equal(t, int64(1), b.ByVendor["deepseek"])
	_, err = svc.GetBriefing(context.Background(), "not-a-date")
	require.Error(t, err)
}

func promoStrPtr(s string) *string        { return &s }
func promoDatePtr(t time.Time) *time.Time { return &t }
