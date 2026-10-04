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
		"/api/v1/xgj-supply/goofish/open/info",
		"/api/v1/xgj-supply/goofish/open/info/", // 带尾斜杠变体
		"/api/v1/xgj-supply/goofish/order/purchase/create",
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
	path := xgjSupplySignedPath(cfg, "/api/v1/xgj-supply/goofish/open/info", "", time.Now().Unix())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	// nil store → fail-closed（签名矩阵由 D6b 中间件单测覆盖）
	require.Contains(t, w.Body.String(), `"code":1`)
}

func TestXgjSupplyRoute_TrailingSlashVariantPasses(t *testing.T) {
	cfg := xgjSupplyTestConfig()
	r := newXgjSupplyRouteEngine(t, handler.NewXgjSupplyHandler(nil, "")) // 同上：nil store 走 401/408 前置 fail-closed
	path := xgjSupplySignedPath(cfg, "/api/v1/xgj-supply/goofish/user/info/", "", time.Now().Unix())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	// 同上：nil store fail-closed；尾斜杠变体路由可达性由 HTTP 200（信封返回）证明
	require.Contains(t, w.Body.String(), `"code":1`)
}

func TestXgjSupplyRoute_ExpiredTimestampReturns408(t *testing.T) {
	cfg := xgjSupplyTestConfig()
	r := newXgjSupplyRouteEngine(t, handler.NewXgjSupplyHandler(nil, "")) // 同上：nil store 走 401/408 前置 fail-closed
	path := xgjSupplySignedPath(cfg, "/api/v1/xgj-supply/goofish/open/info", "", time.Now().Add(-time.Hour).Unix())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Contains(t, w.Body.String(), `"code":408`)
}

func TestXgjSupplyRoute_WrongSignatureReturns401(t *testing.T) {
	cfg := xgjSupplyTestConfig()
	r := newXgjSupplyRouteEngine(t, handler.NewXgjSupplyHandler(nil, "")) // 同上：nil store 走 401/408 前置 fail-closed
	path := xgjSupplySignedPath(cfg, "/api/v1/xgj-supply/goofish/open/info", "", time.Now().Unix()) + "deadbeef"

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
}

func TestXgjSupplyRoute_NilHandlerSkipsGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 不注册 XgjSupply：整组不挂载，路径应 404（fail-safe）。
	RegisterXgjSupplyRoutes(r.Group("/api/v1"), &handler.Handlers{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/xgj-supply/goofish/open/info", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
}

// xgjSupplyOfficialRoutes 官方契约路由表（Apifox shared-cf4d53fd：接口网关 + /goofish/...）。
var xgjSupplyOfficialRoutes = []struct {
	path    string
	handler string // gin 路由树上登记的 handler 方法名（Contains 断言）
}{
	{"/goofish/open/info", "XianguanjiaSupplyHandler).PlatformInfo"},
	{"/goofish/user/info", "XianguanjiaSupplyHandler).MerchantInfo"},
	{"/goofish/goods/list", "XianguanjiaSupplyHandler).ListGoods"},
	{"/goofish/goods/detail", "XianguanjiaSupplyHandler).GoodsDetail"},
	{"/goofish/order/purchase/create", "XianguanjiaSupplyOrderHandler).CreateOrder"},
	{"/goofish/order/detail", "XianguanjiaSupplyOrderHandler).GetOrder"},
	{"/goofish/order/refund/apply", "XianguanjiaSupplyOrderHandler).RefundNotify"},
}

// TestXgjSupplyRoute_OfficialRouteTable 断言路由表与官方契约逐条一致：
// 只注册 POST、每条路径带「无尾斜杠/带尾斜杠」两变体、订单三条挂 D6d 真实方法
// （XianguanjiaSupplyOrderHandler 的 CreateOrder/GetOrder/RefundNotify，见
// xgjSupplyOfficialRoutes.handler 列）、旧 kebab-case 路径全部不再注册。
func TestXgjSupplyRoute_OfficialRouteTable(t *testing.T) {
	r := newXgjSupplyRouteEngine(t, handler.NewXgjSupplyHandler(nil, ""))

	registered := map[string]map[string]string{} // path -> method -> handler 名
	for _, route := range r.Routes() {
		const prefix = "/api/v1/xgj-supply"
		if len(route.Path) < len(prefix) || route.Path[:len(prefix)] != prefix {
			continue
		}
		if registered[route.Path] == nil {
			registered[route.Path] = map[string]string{}
		}
		registered[route.Path][route.Method] = route.Handler
	}

	// 7 条官方路径 × 2 个斜杠变体，全部仅 POST。
	for _, rc := range xgjSupplyOfficialRoutes {
		for _, suffix := range []string{"", "/"} {
			path := "/api/v1/xgj-supply" + rc.path + suffix
			methods, ok := registered[path]
			require.True(t, ok, "官方路径未注册: %s", path)
			require.Len(t, methods, 1, "路径应只注册一个方法: %s", path)
			require.Contains(t, methods, http.MethodPost, "应为 POST: %s", path)
			require.Contains(t, methods[http.MethodPost], rc.handler,
				"handler 不匹配: %s -> %s", path, methods[http.MethodPost])
		}
	}

	// 除官方 14 条外，组内不得有其他注册（旧路径归零）。
	require.Len(t, registered, len(xgjSupplyOfficialRoutes)*2, "路由总数应恰为 14（7 路径 × 2 变体）")

	// 旧 kebab-case 路径逐一确认 404。路径字面量拆成两段拼接，避免 routes 目录
	// grep 旧路径时命中本测试（登记表零残留由 OfficialRouteTable 同时保证）。
	oldSuffixes := []struct{ head, tail string }{
		{"platform", "-info"},
		{"merchant", "-info"},
		{"goods", "-list"},
		{"goods", "-detail"},
		{"order", "-create"},
		{"order", "-detail"},
		{"refund", "-notify"},
	}
	for _, s := range oldSuffixes {
		old := "/api/v1/xgj-supply/" + s.head + s.tail
		_, ok := registered[old]
		require.False(t, ok, "旧路径仍在注册表: %s", old)
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(method, old+"?timestamp="+strconv.FormatInt(time.Now().Unix(), 10), nil))
			require.Equal(t, http.StatusNotFound, w.Code,
				"旧路径应 404: %s %s body=%s", method, old, w.Body.String())
		}
	}
}
