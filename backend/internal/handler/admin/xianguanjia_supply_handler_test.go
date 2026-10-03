package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 以下密钥均为单测占位，绝非真实凭证。
const (
	xgjSupplyTestAppSecret = "app-secret-AAAA"
	xgjSupplyTestMchSecret = "mch-secret-BBBB"
)

// fakeSupplyStore 实现 xianguanjia.SupplyConfigStore，记录写入并模拟持久化行。
type fakeSupplyStore struct {
	mu      sync.Mutex
	cfg     *xianguanjia.SupplyConfig
	saveErr error
	getErr  error
}

func (s *fakeSupplyStore) Save(_ context.Context, cfg xianguanjia.SupplyConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	cp := cfg
	s.cfg = &cp
	return nil
}

func (s *fakeSupplyStore) Get(_ context.Context) (*xianguanjia.SupplyConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.cfg == nil {
		return nil, nil
	}
	cp := *s.cfg
	return &cp, nil
}

func doSupplyRequest(t *testing.T, h *XianguanjiaSupplyHandler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/admin/xianguanjia/supply-config", h.Get)
	r.PUT("/admin/xianguanjia/supply-config", h.Put)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeSupplyData(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code, "body=%s", w.Body.String())
	return resp.Data
}

func TestXianguanjiaSupplyConfig_GetUnconfigured(t *testing.T) {
	h := NewXianguanjiaSupplyHandler(&fakeSupplyStore{}, "")
	w := doSupplyRequest(t, h, http.MethodGet, "/admin/xianguanjia/supply-config", "")
	require.Equal(t, http.StatusOK, w.Code)
	data := decodeSupplyData(t, w)
	require.Equal(t, false, data["configured"])
	require.Equal(t, xianguanjiaSupplyDefaultGateway, data["gateway"])
}

func TestXianguanjiaSupplyConfig_SaveAndReadBackMasked(t *testing.T) {
	store := &fakeSupplyStore{}
	h := NewXianguanjiaSupplyHandler(store, "")

	body := `{"supply_app_id":"1783283558647493","app_secret":"` + xgjSupplyTestAppSecret + `","mch_id":"900001","mch_secret":"` + xgjSupplyTestMchSecret + `"}`
	w := doSupplyRequest(t, h, http.MethodPut, "/admin/xianguanjia/supply-config", body)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Equal(t, true, decodeSupplyData(t, w)["saved"])

	// 落库应为明文语义（加密由 store 实现负责），且与输入一致。
	stored, err := store.Get(context.Background())
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, "1783283558647493", stored.SupplyAppID)
	require.Equal(t, xgjSupplyTestAppSecret, stored.SupplyAppSecret)
	require.Equal(t, "900001", stored.MchID)
	require.Equal(t, xgjSupplyTestMchSecret, stored.MchSecret)

	// GET 必须脱敏：只回是否设置 + 末 4 位，绝不回明文。
	w = doSupplyRequest(t, h, http.MethodGet, "/admin/xianguanjia/supply-config", "")
	require.Equal(t, http.StatusOK, w.Code)
	data := decodeSupplyData(t, w)
	require.Equal(t, true, data["configured"])
	require.Equal(t, "1783283558647493", data["supply_app_id"])
	require.Equal(t, true, data["app_secret_set"])
	require.Equal(t, "AAAA", data["app_secret_tail"])
	require.Equal(t, "900001", data["mch_id"])
	require.Equal(t, true, data["mch_secret_set"])
	require.Equal(t, "BBBB", data["mch_secret_tail"])
	require.NotContains(t, w.Body.String(), xgjSupplyTestAppSecret)
	require.NotContains(t, w.Body.String(), xgjSupplyTestMchSecret)
}

func TestXianguanjiaSupplyConfig_BlankSecretKeepsExisting(t *testing.T) {
	store := &fakeSupplyStore{}
	h := NewXianguanjiaSupplyHandler(store, "")

	first := `{"supply_app_id":"1783283558647493","app_secret":"` + xgjSupplyTestAppSecret + `","mch_id":"900001","mch_secret":"` + xgjSupplyTestMchSecret + `"}`
	require.Equal(t, http.StatusOK, doSupplyRequest(t, h, http.MethodPut, "/admin/xianguanjia/supply-config", first).Code)

	// 两个 secret 留空 = 保留原值，仅更新其他字段。
	second := `{"supply_app_id":"1783283558647493","mch_id":"900001"}`
	require.Equal(t, http.StatusOK, doSupplyRequest(t, h, http.MethodPut, "/admin/xianguanjia/supply-config", second).Code)

	stored, err := store.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, xgjSupplyTestAppSecret, stored.SupplyAppSecret)
	require.Equal(t, xgjSupplyTestMchSecret, stored.MchSecret)
}

func TestXianguanjiaSupplyConfig_FirstSaveRequiresSecrets(t *testing.T) {
	h := NewXianguanjiaSupplyHandler(&fakeSupplyStore{}, "")
	body := `{"supply_app_id":"1783283558647493","mch_id":"900001"}`
	w := doSupplyRequest(t, h, http.MethodPut, "/admin/xianguanjia/supply-config", body)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestXianguanjiaSupplyConfig_MissingIDsRejected(t *testing.T) {
	h := NewXianguanjiaSupplyHandler(&fakeSupplyStore{}, "")
	w := doSupplyRequest(t, h, http.MethodPut, "/admin/xianguanjia/supply-config", `{"app_secret":"x","mch_secret":"y"}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestXianguanjiaSupplyConfig_StoreErrorFailClosed(t *testing.T) {
	store := &fakeSupplyStore{getErr: errors.New("db down")}
	h := NewXianguanjiaSupplyHandler(store, "")
	require.Equal(t, http.StatusInternalServerError, doSupplyRequest(t, h, http.MethodGet, "/admin/xianguanjia/supply-config", "").Code)
}
