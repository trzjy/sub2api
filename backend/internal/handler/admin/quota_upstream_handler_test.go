package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeUpstreamRepo 是 quotaUpstreamRepository 的内存替身（不依赖数据库），
// 用于验证 List / Update 行为，尤其是「改 key/type 被拒」「改 name 生效」「缺失项 404」。
type fakeUpstreamRepo struct {
	byKey  map[string]*ent.QuotaUpstream
	nextID int64
	getErr error
	updErr error
}

func newFakeUpstreamRepo() *fakeUpstreamRepo {
	return &fakeUpstreamRepo{
		byKey: map[string]*ent.QuotaUpstream{
			"kn_cy":   {ID: 1, Key: "kn_cy", Name: "kn-cy.com 网关", Type: "kncy", RenewalURL: ptr("https://shop.kn-cy.com/products")},
			"shareai": {ID: 2, Key: "shareai", Name: "ShareAI 号池", Type: "shareai", RenewalURL: nil, Notes: ptr("默认上游")},
		},
		nextID: 3,
	}
}

func ptr[T any](v T) *T { return &v }

func (f *fakeUpstreamRepo) GetByKey(ctx context.Context, key string) (*ent.QuotaUpstream, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if u, ok := f.byKey[key]; ok {
		return u, nil
	}
	return nil, &ent.NotFoundError{}
}

func (f *fakeUpstreamRepo) List(ctx context.Context) ([]*ent.QuotaUpstream, error) {
	out := make([]*ent.QuotaUpstream, 0, len(f.byKey))
	for _, u := range f.byKey {
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeUpstreamRepo) Update(ctx context.Context, id int64, name string, renewalURL **string, notes **string) error {
	if f.updErr != nil {
		return f.updErr
	}
	for _, u := range f.byKey {
		if u.ID == id {
			u.Name = name
			if renewalURL != nil {
				u.RenewalURL = *renewalURL
			}
			if notes != nil {
				u.Notes = *notes
			}
			return nil
		}
	}
	return &ent.NotFoundError{}
}

func newUpstreamCtx(method, target string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(method, target, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

func decodeUpstreamView(t *testing.T, w *httptest.ResponseRecorder) quotaUpstreamView {
	t.Helper()
	var resp struct {
		Data quotaUpstreamView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Data
}

func TestQuotaUpstreamHandler_List(t *testing.T) {
	h := NewQuotaUpstreamHandler(newFakeUpstreamRepo())
	c, w := newUpstreamCtx(http.MethodGet, "/admin/quota_upstreams", nil)
	h.List(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Data struct {
			Items []quotaUpstreamView `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Data.Items, 2)
	// 不含任何凭据字段；续费 URL 缺失项不出现 renewal_url。
	for _, it := range resp.Data.Items {
		require.NotEmpty(t, it.Key)
		require.NotEmpty(t, it.Name)
	}
}

func TestQuotaUpstreamHandler_Update_NameOnly(t *testing.T) {
	repo := newFakeUpstreamRepo()
	h := NewQuotaUpstreamHandler(repo)
	// 仅传 name：renewal_url / notes 缺省（不出现在 JSON 中）→ 不改这两个字段。
	body, _ := json.Marshal(map[string]any{"name": "kn-cy.com 网关（新名）"})
	c, w := newUpstreamCtx(http.MethodPut, "/admin/quota_upstreams/kn_cy", body)
	c.Params = gin.Params{{Key: "key", Value: "kn_cy"}}
	h.Update(c)

	require.Equal(t, http.StatusOK, w.Code)
	v := decodeUpstreamView(t, w)
	require.Equal(t, "kn-cy.com 网关（新名）", v.Name)
	require.Equal(t, "kn_cy", v.Key) // key 不变
	require.Equal(t, "kncy", v.Type) // type 不变
	// 回读后存储已更新；renewal_url 未被清空（缺省字段不改）。
	require.Equal(t, "kn-cy.com 网关（新名）", repo.byKey["kn_cy"].Name)
	require.NotNil(t, repo.byKey["kn_cy"].RenewalURL)
	require.Equal(t, "https://shop.kn-cy.com/products", *repo.byKey["kn_cy"].RenewalURL)
}

func TestQuotaUpstreamHandler_Update_RejectsKeyAndType(t *testing.T) {
	h := NewQuotaUpstreamHandler(newFakeUpstreamRepo())
	// 试图改 key。
	body, _ := json.Marshal(updateQuotaUpstreamRequest{Name: ptr("x"), Key: ptr("hacked")})
	c, w := newUpstreamCtx(http.MethodPut, "/admin/quota_upstreams/kn_cy", body)
	c.Params = gin.Params{{Key: "key", Value: "kn_cy"}}
	h.Update(c)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 试图改 type。
	body, _ = json.Marshal(updateQuotaUpstreamRequest{Name: ptr("x"), Type: ptr("hacked")})
	c, w = newUpstreamCtx(http.MethodPut, "/admin/quota_upstreams/kn_cy", body)
	c.Params = gin.Params{{Key: "key", Value: "kn_cy"}}
	h.Update(c)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestQuotaUpstreamHandler_Update_NameRequired(t *testing.T) {
	h := NewQuotaUpstreamHandler(newFakeUpstreamRepo())
	body, _ := json.Marshal(updateQuotaUpstreamRequest{RenewalURL: ptr("https://x")})
	c, w := newUpstreamCtx(http.MethodPut, "/admin/quota_upstreams/kn_cy", body)
	c.Params = gin.Params{{Key: "key", Value: "kn_cy"}}
	h.Update(c)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestQuotaUpstreamHandler_Update_NotFound(t *testing.T) {
	h := NewQuotaUpstreamHandler(newFakeUpstreamRepo())
	body, _ := json.Marshal(updateQuotaUpstreamRequest{Name: ptr("x")})
	c, w := newUpstreamCtx(http.MethodPut, "/admin/quota_upstreams/missing", body)
	c.Params = gin.Params{{Key: "key", Value: "missing"}}
	h.Update(c)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestQuotaUpstreamHandler_Update_ClearRenewalURL(t *testing.T) {
	repo := newFakeUpstreamRepo()
	h := NewQuotaUpstreamHandler(repo)
	// renewal_url 置为空串 → 清空（JSON 中 null 与缺省均被解析为「不改」，
	// 故清空走空串语义：前端编辑框留空即提交 ""）。
	body, _ := json.Marshal(map[string]any{"name": "kn-cy.com 网关", "renewal_url": ""})
	c, w := newUpstreamCtx(http.MethodPut, "/admin/quota_upstreams/kn_cy", body)
	c.Params = gin.Params{{Key: "key", Value: "kn_cy"}}
	h.Update(c)
	require.Equal(t, http.StatusOK, w.Code)
	v := decodeUpstreamView(t, w)
	require.NotNil(t, v.RenewalURL)
	require.Equal(t, "", *v.RenewalURL)
	require.NotNil(t, repo.byKey["kn_cy"].RenewalURL)
	require.Equal(t, "", *repo.byKey["kn_cy"].RenewalURL)
}
