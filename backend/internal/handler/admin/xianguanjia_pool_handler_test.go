package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeXgjKindSvc 可编程卡种服务（mock D4a 实现）。
type fakeXgjKindSvc struct {
	createID   int64
	currentID  int64
	createErr  error
	currentErr error
	gotName    string
	gotCatID   int64
	createCall int
}

func (s *fakeXgjKindSvc) KindCreate(ctx context.Context, name string, categoryID int64) (int64, error) {
	s.createCall++
	s.gotName = name
	s.gotCatID = categoryID
	if s.createErr != nil {
		return 0, s.createErr
	}
	return s.createID, nil
}

func (s *fakeXgjKindSvc) KindCurrent(ctx context.Context) (int64, error) {
	if s.currentErr != nil {
		return 0, s.currentErr
	}
	return s.currentID, nil
}

// fakeXgjPusher 可编程批量推仓（mock D4b 实现）。
type fakeXgjPusher struct {
	result    XgjPushResult
	err       error
	gotKindID int64
	gotCards  []XgjCardPair
	calls     int
}

func (p *fakeXgjPusher) PushCards(ctx context.Context, kindID int64, cards []XgjCardPair) (XgjPushResult, error) {
	p.calls++
	p.gotKindID = kindID
	p.gotCards = cards
	if p.err != nil {
		return XgjPushResult{}, p.err
	}
	return p.result, nil
}

func newXgjPoolRouter(h *XianguanjiaPoolHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/admin/xianguanjia/pool/kind", h.KindCreate)
	r.GET("/admin/xianguanjia/pool/kind", h.KindGet)
	r.POST("/admin/xianguanjia/pool/push", h.PoolPush)
	return r
}

func doXgjPoolRequest(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// POST /pool/kind 正常：透传 name/category_id 并回 kind_id。
func TestXgjPoolKindCreateOK(t *testing.T) {
	svc := &fakeXgjKindSvc{createID: 9001}
	h := NewXianguanjiaPoolHandler(svc, &fakeXgjPusher{})
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/kind", `{"name":"月卡","category_id":7}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, svc.createCall)
	require.Equal(t, "月卡", svc.gotName)
	require.Equal(t, int64(7), svc.gotCatID)
	require.Contains(t, w.Body.String(), `"kind_id":9001`)
}

// POST /pool/kind 缺 name：400，不调 service。
func TestXgjPoolKindCreateRequiresName(t *testing.T) {
	svc := &fakeXgjKindSvc{}
	h := NewXianguanjiaPoolHandler(svc, &fakeXgjPusher{})
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/kind", `{"name":"  "}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, 0, svc.createCall)
}

// POST /pool/kind category_id 为负：400。
func TestXgjPoolKindCreateRejectsNegativeCategory(t *testing.T) {
	svc := &fakeXgjKindSvc{}
	h := NewXianguanjiaPoolHandler(svc, &fakeXgjPusher{})
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/kind", `{"name":"月卡","category_id":-1}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, 0, svc.createCall)
}

// POST /pool/kind 非法 JSON：400。
func TestXgjPoolKindCreateBadJSON(t *testing.T) {
	h := NewXianguanjiaPoolHandler(&fakeXgjKindSvc{}, &fakeXgjPusher{})
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/kind", `{oops`)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// POST /pool/kind service 出错：500。
func TestXgjPoolKindCreateServiceError(t *testing.T) {
	svc := &fakeXgjKindSvc{createErr: errors.New("boom")}
	h := NewXianguanjiaPoolHandler(svc, &fakeXgjPusher{})
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/kind", `{"name":"月卡"}`)
	require.Equal(t, http.StatusInternalServerError, w.Code)
}

// GET /pool/kind 正常：返回当前 kind_id。
func TestXgjPoolKindGetOK(t *testing.T) {
	svc := &fakeXgjKindSvc{currentID: 42}
	h := NewXianguanjiaPoolHandler(svc, &fakeXgjPusher{})
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodGet, "/admin/xianguanjia/pool/kind", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"kind_id":42`)
}

// GET /pool/kind service 出错：500。
func TestXgjPoolKindGetServiceError(t *testing.T) {
	svc := &fakeXgjKindSvc{currentErr: errors.New("boom")}
	h := NewXianguanjiaPoolHandler(svc, &fakeXgjPusher{})
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodGet, "/admin/xianguanjia/pool/kind", "")
	require.Equal(t, http.StatusInternalServerError, w.Code)
}

// POST /pool/push 正常：透传 kind_id 与卡列表，回推仓结果。
func TestXgjPoolPushOK(t *testing.T) {
	pusher := &fakeXgjPusher{result: XgjPushResult{Total: 2, Succeeded: 2}}
	h := NewXianguanjiaPoolHandler(&fakeXgjKindSvc{}, pusher)
	body := `{"kind_id":9001,"cards":[{"card_no":"NO1","card_pwd":"PW1"},{"card_no":"NO2","card_pwd":"PW2"}]}`
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/push", body)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, pusher.calls)
	require.Equal(t, int64(9001), pusher.gotKindID)
	require.Len(t, pusher.gotCards, 2)
	require.Equal(t, "NO1", pusher.gotCards[0].CardNo)
	require.Contains(t, w.Body.String(), `"succeeded":2`)
}

// POST /pool/push 缺 kind_id：400。
func TestXgjPoolPushRequiresKindID(t *testing.T) {
	pusher := &fakeXgjPusher{}
	h := NewXianguanjiaPoolHandler(&fakeXgjKindSvc{}, pusher)
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/push", `{"cards":[{"card_no":"NO1","card_pwd":"PW1"}]}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, 0, pusher.calls)
}

// POST /pool/push cards 为空：400。
func TestXgjPoolPushRequiresCards(t *testing.T) {
	pusher := &fakeXgjPusher{}
	h := NewXianguanjiaPoolHandler(&fakeXgjKindSvc{}, pusher)
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/push", `{"kind_id":1,"cards":[]}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, 0, pusher.calls)
}

// POST /pool/push 某张卡缺 card_no：400。
func TestXgjPoolPushRequiresCardNo(t *testing.T) {
	pusher := &fakeXgjPusher{}
	h := NewXianguanjiaPoolHandler(&fakeXgjKindSvc{}, pusher)
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/push", `{"kind_id":1,"cards":[{"card_no":" ","card_pwd":"PW1"}]}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, 0, pusher.calls)
}

// POST /pool/push 超批量上限：400。
func TestXgjPoolPushRejectsOversizeBatch(t *testing.T) {
	pusher := &fakeXgjPusher{}
	h := NewXianguanjiaPoolHandler(&fakeXgjKindSvc{}, pusher)
	var sb strings.Builder
	sb.WriteString(`{"kind_id":1,"cards":[`)
	for i := 0; i < xgjPushMaxBatch+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"card_no":"N","card_pwd":"P"}`)
	}
	sb.WriteString(`]}`)
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/push", sb.String())
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, 0, pusher.calls)
}

// POST /pool/push service 出错：500。
func TestXgjPoolPushServiceError(t *testing.T) {
	pusher := &fakeXgjPusher{err: errors.New("boom")}
	h := NewXianguanjiaPoolHandler(&fakeXgjKindSvc{}, pusher)
	w := doXgjPoolRequest(t, newXgjPoolRouter(h), http.MethodPost, "/admin/xianguanjia/pool/push", `{"kind_id":1,"cards":[{"card_no":"N","card_pwd":"P"}]}`)
	require.Equal(t, http.StatusInternalServerError, w.Code)
}

// 依赖未注入：fail-closed 返回 503。
func TestXgjPoolHandlerNilDeps(t *testing.T) {
	h := NewXianguanjiaPoolHandler(nil, nil)
	r := newXgjPoolRouter(h)
	require.Equal(t, http.StatusServiceUnavailable,
		doXgjPoolRequest(t, r, http.MethodPost, "/admin/xianguanjia/pool/kind", `{"name":"x"}`).Code)
	require.Equal(t, http.StatusServiceUnavailable,
		doXgjPoolRequest(t, r, http.MethodGet, "/admin/xianguanjia/pool/kind", "").Code)
	require.Equal(t, http.StatusServiceUnavailable,
		doXgjPoolRequest(t, r, http.MethodPost, "/admin/xianguanjia/pool/push", `{"kind_id":1,"cards":[{"card_no":"N","card_pwd":"P"}]}`).Code)
}

var (
	_ XianguanjiaKindService = (*fakeXgjKindSvc)(nil)
	_ XianguanjiaPoolPusher  = (*fakeXgjPusher)(nil)
)
