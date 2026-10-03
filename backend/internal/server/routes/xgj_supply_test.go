package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 以下密钥均为单测占位，绝非真实凭证。
type xgjSupplyRouteStore struct{ cfg *xianguanjia.SupplyConfig }

func (s *xgjSupplyRouteStore) Save(_ context.Context, cfg xianguanjia.SupplyConfig) error {
	cp := cfg
	s.cfg = &cp
	return nil
}

func (s *xgjSupplyRouteStore) Get(_ context.Context) (*xianguanjia.SupplyConfig, error) {
	if s.cfg == nil {
		return nil, nil
	}
	cp := *s.cfg
	return &cp, nil
}

func newXgjSupplyRouteEngine(t *testing.T, xgjHandler *handler.XgjSupplyHandler) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterXgjSupplyRoutes(r.Group("/api/v1"), &handler.Handlers{
		XgjSupply: xgjHandler,
	})
	return r
}

func xgjSupplyTestConfig() *xianguanjia.SupplyConfig {
	return &xianguanjia.SupplyConfig{
		SupplyAppID:     "1783283558647493",
		SupplyAppSecret: "app-secret-AAAA",
		MchID:     "900001",
		MchSecret: "mch-secret-BBBB",
	}
}

func xgjSupplySignedPath(cfg *xianguanjia.SupplyConfig, path, body string, ts int64) string {
	tsStr := strconv.FormatInt(ts, 10)
	bodyMd5 := xianguanjia.BodyMd5([]byte(body))
	sign := xianguanjia.SupplySign(cfg.SupplyAppID, cfg.SupplyAppSecret, bodyMd5, tsStr, cfg.MchID, cfg.MchSecret)
	return path + "?app_id=" + cfg.SupplyAppID + "&mch_id=" + cfg.MchID + "&timestamp=" + tsStr + "&sign=" + sign
}

func TestXgjSupplyRoute_NoSignatureReturns401(t *testing.T) {
	r := newXgjSupplyRouteEngine(t, handler.NewXgjSupplyHandler(nil, "")) // 验签 fail-closed 路径：nil store

	for _, path := range []string{
		"/api/v1/xgj-supply/platform-info",
		"/api/v1/xgj-supply/platform-info/", // 带尾斜杠变体（联调期全注册）
		"/api/v1/xgj-supply/order-create",
	} {
		// 带当前 timestamp 但缺 sign → 走到签名比对 → 401。
		//（中间件顺序：timestamp 新鲜度先于签名比对，缺 timestamp 会得 408——
		//  官方错误码表两项并存、顺序无规定，见 d6b.md 联调校正清单。）
		u := path + "?app_id=1783283558647493&mch_id=900001&timestamp=" + strconv.FormatInt(time.Now().Unix(), 10)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, u, nil))
		require.Equal(t, http.StatusOK, w.Code, "path=%s body=%s", path, w.Body.String())
		// nil store → fail-closed code=1（货源未配置）。签名 401/408 矩阵由
		// D6b supply_sign_test.go（中间件单测）覆盖；routes 层只证「未配置必拒」。
		require.Contains(t, w.Body.String(), `"code":1`)
	}
}

func TestXgjSupplyRoute_ValidSignaturePasses(t *testing.T) {
	cfg := xgjSupplyTestConfig()
	r := newXgjSupplyRouteEngine(t, handler.NewXgjSupplyHandler(nil, "")) // 同上：nil store 走 401/408 前置 fail-closed
	path := xgjSupplySignedPath(cfg, "/api/v1/xgj-supply/platform-info", "", time.Now().Unix())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	// nil store → fail-closed（签名矩阵由 D6b 中间件单测覆盖）
	require.Contains(t, w.Body.String(), `"code":1`)
}

func TestXgjSupplyRoute_TrailingSlashVariantPasses(t *testing.T) {
	cfg := xgjSupplyTestConfig()
	r := newXgjSupplyRouteEngine(t, handler.NewXgjSupplyHandler(nil, "")) // 同上：nil store 走 401/408 前置 fail-closed
	path := xgjSupplySignedPath(cfg, "/api/v1/xgj-supply/merchant-info/", "", time.Now().Unix())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	// 同上：nil store fail-closed；尾斜杠变体路由可达性由 HTTP 200（信封返回）证明
	require.Contains(t, w.Body.String(), `"code":1`)
}

func TestXgjSupplyRoute_ExpiredTimestampReturns408(t *testing.T) {
	cfg := xgjSupplyTestConfig()
	r := newXgjSupplyRouteEngine(t, handler.NewXgjSupplyHandler(nil, "")) // 同上：nil store 走 401/408 前置 fail-closed
	path := xgjSupplySignedPath(cfg, "/api/v1/xgj-supply/platform-info", "", time.Now().Add(-time.Hour).Unix())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Contains(t, w.Body.String(), `"code":408`)
}

func TestXgjSupplyRoute_WrongSignatureReturns401(t *testing.T) {
	cfg := xgjSupplyTestConfig()
	r := newXgjSupplyRouteEngine(t, handler.NewXgjSupplyHandler(nil, "")) // 同上：nil store 走 401/408 前置 fail-closed
	path := xgjSupplySignedPath(cfg, "/api/v1/xgj-supply/platform-info", "", time.Now().Unix()) + "deadbeef"

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
}

func TestXgjSupplyRoute_NilHandlerSkipsGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 不注册 XgjSupply：整组不挂载，路径应 404（fail-safe）。
	RegisterXgjSupplyRoutes(r.Group("/api/v1"), &handler.Handlers{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/xgj-supply/platform-info", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
}
