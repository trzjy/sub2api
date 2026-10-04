package xianguanjia

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// 本文件密钥均为单测占位，绝非真实凭证；真实 app_secret/mch_secret 绝不写入代码/测试。

const (
	// supplyTestAppID 采用官方文档 doc-4985015 示例量级的 app_id（整数型字符串）。
	supplyTestAppID     = "677859093659717"
	supplyTestAppSecret = "fake-supply-app-secret"
	supplyTestMchID     = "900001"
	supplyTestMchSecret = "fake-mch-secret"
	// supplyTestVectorBody 是压缩 JSON（无空格），正是验签所要求的 body 形态。
	supplyTestVectorBody = `{"order_no":"O-VEC-1","goods_no":"G1","quantity":1}`
	supplyTestVectorTS   = "1700000000"
)

// 固定向量 golden（由独立 md5 工具预计算，测试内再用独立函数复算作"向量自洽"）：
//
//	bodyMd5 = md5(supplyTestVectorBody)                 = 6a4bf5196da7cba41bd547ce273b874d
//	sign    = md5("{app_id},{app_secret},{bodyMd5},{ts},{mch_id},{mch_secret}")
const (
	goldenBodyMd5   = "6a4bf5196da7cba41bd547ce273b874d"
	goldenSign      = "e56bf2676a14c7289d0a71e2dfae6cce"
	goldenEmptyMd5  = "99914b932bd37a50b983c5e7c90ae93b"
	goldenEmptySign = "6f73d39db0d2a3e293e71070515cf0f2"
)

// independentSupplySign 用最直白的方式重算六段逗号 md5，作为 SupplySign 的"独立向量"对照。
func independentSupplySign(appID, appSecret, bodyMd5, ts, mchID, mchSecret string) string {
	raw := appID + "," + appSecret + "," + bodyMd5 + "," + ts + "," + mchID + "," + mchSecret
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func md5HexIndependent(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// fakeSupplyConfigReader 是 SupplyConfigReader 的单测 fake（不依赖 D6a 编译）。
type fakeSupplyConfigReader struct {
	cfg *SupplyConfig
	err error
}

func (f *fakeSupplyConfigReader) GetSupplyConfig(ctx context.Context) (*SupplyConfig, error) {
	return f.cfg, f.err
}

func testSupplyConfig() *SupplyConfig {
	return &SupplyConfig{
		SupplyAppID:     supplyTestAppID,
		SupplyAppSecret: supplyTestAppSecret,
		MchID:           supplyTestMchID,
		MchSecret:       supplyTestMchSecret,
	}
}

// buildSignedQuery 给定 body 与时间戳，构造一套合法 query（app_id/timestamp/mch_id/sign）。
func buildSignedQuery(t *testing.T, body []byte, ts string) url.Values {
	t.Helper()
	bodyMd5 := BodyMd5(body)
	sign := SupplySign(supplyTestAppID, supplyTestAppSecret, bodyMd5, ts, supplyTestMchID, supplyTestMchSecret)
	q := url.Values{}
	q.Set("app_id", supplyTestAppID)
	q.Set("timestamp", ts)
	q.Set("mch_id", supplyTestMchID)
	q.Set("sign", sign)
	return q
}

// newSignRouter 构造带验签中间件的测试路由，命中后回显收到的 body 供"raw body 还原"断言。
func newSignRouter(reader SupplyConfigReader) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/xgj-supply/ping", SupplySignMiddleware(reader), func(c *gin.Context) {
		body := make([]byte, 0)
		if c.Request.Body != nil {
			buf := new(bytes.Buffer)
			if _, err := buf.ReadFrom(c.Request.Body); err == nil {
				body = buf.Bytes()
			}
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "msg": "ok", "data": gin.H{"echo": string(body)}})
	})
	return r
}

// doSigned 发一次请求并返回 recorder 与解析出的 body（作为字符串）。
func doSigned(t *testing.T, r *gin.Engine, body []byte, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/xgj-supply/ping?"+q.Encode(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json;charset=utf-8")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestSupplySignGoldenVector 断言六段公式与预计算 golden 一致，并用独立实现复算自洽。
func TestSupplySignGoldenVector(t *testing.T) {
	bodyMd5 := BodyMd5([]byte(supplyTestVectorBody))
	if bodyMd5 != goldenBodyMd5 {
		t.Fatalf("bodyMd5 mismatch: got %q want %q", bodyMd5, goldenBodyMd5)
	}
	got := SupplySign(supplyTestAppID, supplyTestAppSecret, bodyMd5, supplyTestVectorTS, supplyTestMchID, supplyTestMchSecret)
	if got != goldenSign {
		t.Fatalf("golden sign mismatch: got %q want %q", got, goldenSign)
	}
	if want := independentSupplySign(supplyTestAppID, supplyTestAppSecret, bodyMd5, supplyTestVectorTS, supplyTestMchID, supplyTestMchSecret); got != want {
		t.Fatalf("Sign vs independent mismatch: got %q want %q", got, want)
	}
	// 空 body → md5("{}")，其 golden 亦自洽。
	if emptyMd5 := BodyMd5(nil); emptyMd5 != goldenEmptyMd5 {
		t.Fatalf("empty bodyMd5 mismatch: got %q want %q", emptyMd5, goldenEmptyMd5)
	}
	emptySign := SupplySign(supplyTestAppID, supplyTestAppSecret, goldenEmptyMd5, supplyTestVectorTS, supplyTestMchID, supplyTestMchSecret)
	if emptySign != goldenEmptySign {
		t.Fatalf("empty golden sign mismatch: got %q want %q", emptySign, goldenEmptySign)
	}
	// 篡改任意一段都应改变结果。
	if SupplySign("X"+supplyTestAppID, supplyTestAppSecret, bodyMd5, supplyTestVectorTS, supplyTestMchID, supplyTestMchSecret) == got {
		t.Fatal("changing app_id should change sign")
	}
	if SupplySign(supplyTestAppID, "X", bodyMd5, supplyTestVectorTS, supplyTestMchID, supplyTestMchSecret) == got {
		t.Fatal("changing app_secret should change sign")
	}
	if SupplySign(supplyTestAppID, supplyTestAppSecret, md5HexIndependent("other"), supplyTestVectorTS, supplyTestMchID, supplyTestMchSecret) == got {
		t.Fatal("changing bodyMd5 should change sign")
	}
	if SupplySign(supplyTestAppID, supplyTestAppSecret, bodyMd5, "1700000001", supplyTestMchID, supplyTestMchSecret) == got {
		t.Fatal("changing timestamp should change sign")
	}
	if SupplySign(supplyTestAppID, supplyTestAppSecret, bodyMd5, supplyTestVectorTS, "900002", supplyTestMchSecret) == got {
		t.Fatal("changing mch_id should change sign")
	}
	if SupplySign(supplyTestAppID, supplyTestAppSecret, bodyMd5, supplyTestVectorTS, supplyTestMchID, "X") == got {
		t.Fatal("changing mch_secret should change sign")
	}
}

// TestVerifySupplySignPure 验证纯函数核：一致→nil；不一致→ErrSupplySignMismatch。
func TestVerifySupplySignPure(t *testing.T) {
	body := []byte(supplyTestVectorBody)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	q := buildSignedQuery(t, body, ts) // 复用同一公式，但用独立 golden 值核对
	if err := VerifySupplySign(body, q.Get("app_id"), ts, q.Get("mch_id"), q.Get("sign"), supplyTestAppSecret, supplyTestMchSecret); err != nil {
		t.Fatalf("valid sign should verify: %v", err)
	}
	if err := VerifySupplySign(body, q.Get("app_id"), ts, q.Get("mch_id"), "deadbeef", supplyTestAppSecret, supplyTestMchSecret); !errors.Is(err, ErrSupplySignMismatch) {
		t.Fatalf("bad sign should return ErrSupplySignMismatch, got %v", err)
	}
	// body 被篡改（签名按原 body 计算）→ 失配。
	if err := VerifySupplySign([]byte(`{"a":1}`), q.Get("app_id"), ts, q.Get("mch_id"), q.Get("sign"), supplyTestAppSecret, supplyTestMchSecret); !errors.Is(err, ErrSupplySignMismatch) {
		t.Fatalf("tampered body should mismatch, got %v", err)
	}
	// query 四参任一被篡改 → 失配。
	if err := VerifySupplySign(body, "1", ts, q.Get("mch_id"), q.Get("sign"), supplyTestAppSecret, supplyTestMchSecret); !errors.Is(err, ErrSupplySignMismatch) {
		t.Fatalf("tampered app_id should mismatch, got %v", err)
	}
	if err := VerifySupplySign(body, q.Get("app_id"), ts, "1", q.Get("sign"), supplyTestAppSecret, supplyTestMchSecret); !errors.Is(err, ErrSupplySignMismatch) {
		t.Fatalf("tampered mch_id should mismatch, got %v", err)
	}
}

// TestSupplySignMiddlewareAcceptsAndRestoresBody 合法请求放行，且后续 handler 能读到原 body。
func TestSupplySignMiddlewareAcceptsAndRestoresBody(t *testing.T) {
	body := []byte(supplyTestVectorBody)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	r := newSignRouter(&fakeSupplyConfigReader{cfg: testSupplyConfig()})
	w := doSigned(t, r, body, buildSignedQuery(t, body, ts))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Code int `json:"code"`
		Data struct {
			Echo string `json:"echo"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if got.Code != 0 {
		t.Fatalf("expected code=0, got %d body=%s", got.Code, w.Body.String())
	}
	if got.Data.Echo != supplyTestVectorBody {
		t.Fatalf("raw body not restored: got %q want %q", got.Data.Echo, supplyTestVectorBody)
	}
}

// TestSupplySignMiddlewareBadSign401 签名不符 → code=401,msg=签名错误。
func TestSupplySignMiddlewareBadSign401(t *testing.T) {
	body := []byte(supplyTestVectorBody)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	q := buildSignedQuery(t, body, ts)
	q.Set("sign", "deadbeefdeadbeefdeadbeefdeadbeef")
	r := newSignRouter(&fakeSupplyConfigReader{cfg: testSupplyConfig()})
	w := doSigned(t, r, body, q)
	assertSupplyEnvelope(t, w, SupplyCodeSignError, "签名错误")
}

// TestSupplySignMiddlewareTamperedQuery401 covers mch_id/body 篡改均 401。
// 注意：自 D6E-02R5 起，验签 app_id 位取库内 cfg.SupplyAppID，query 不再提供
// app_id，故「篡改 query app_id」已无意义（验签不读 query app_id），该子例移除。
func TestSupplySignMiddlewareTamperedQuery401(t *testing.T) {
	body := []byte(supplyTestVectorBody)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	r := newSignRouter(&fakeSupplyConfigReader{cfg: testSupplyConfig()})

	badMchID := buildSignedQuery(t, body, ts)
	badMchID.Set("mch_id", "1")
	assertSupplyEnvelope(t, doSigned(t, r, body, badMchID), SupplyCodeSignError, "签名错误")

	// body 与签名所用 body 不一致 → 401。
	assertSupplyEnvelope(t, doSigned(t, r, []byte(`{"x":1}`), buildSignedQuery(t, body, ts)), SupplyCodeSignError, "签名错误")
}

// buildSignedQueryNoAppID 构造合法 query，仅带 mch_id/timestamp/sign（不含 app_id），
// 复现 2026-10-04 生产取证：闲管家 go-resty 客户端实际不在 query 传 app_id。
func buildSignedQueryNoAppID(t *testing.T, body []byte, ts string) url.Values {
	t.Helper()
	bodyMd5 := BodyMd5(body)
	sign := SupplySign(supplyTestAppID, supplyTestAppSecret, bodyMd5, ts, supplyTestMchID, supplyTestMchSecret)
	q := url.Values{}
	q.Set("timestamp", ts)
	q.Set("mch_id", supplyTestMchID)
	q.Set("sign", sign)
	return q
}

// TestSupplySignMiddlewareQueryNoAppIDPasses 复现生产取证：query 不含 app_id 也验签通过。
// cfg.SupplyAppID 与签名所用 app_id 一致时，验签 app_id 位取库内值，放行。
func TestSupplySignMiddlewareQueryNoAppIDPasses(t *testing.T) {
	body := []byte(supplyTestVectorBody)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	r := newSignRouter(&fakeSupplyConfigReader{cfg: testSupplyConfig()})
	w := doSigned(t, r, body, buildSignedQueryNoAppID(t, body, ts))
	if w.Code != http.StatusOK {
		t.Fatalf("query without app_id should pass, status=%d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Code != 0 {
		t.Fatalf("query without app_id expected code=0, got %d body=%s", got.Code, w.Body.String())
	}
}

// TestSupplySignMiddlewareAppIDMismatch401 cfg.SupplyAppID 与签名所用 app_id 不一致
// → 401 签名错误（fail-closed，验证取参来源由 query 改为库内后不放宽）。
func TestSupplySignMiddlewareAppIDMismatch401(t *testing.T) {
	body := []byte(supplyTestVectorBody)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	cfg := testSupplyConfig()
	cfg.SupplyAppID = "wrong-app-id" // 与签名所用 supplyTestAppID 不一致
	r := newSignRouter(&fakeSupplyConfigReader{cfg: cfg})
	// query 中的 app_id 被完全忽略（带与不带、值是否一致均不影响），验签 app_id 位一律取 cfg.SupplyAppID。
	w := doSigned(t, r, body, buildSignedQuery(t, body, ts))
	assertSupplyEnvelope(t, w, SupplyCodeSignError, "签名错误")
}

// TestSupplySignMiddlewareExpiredTimestamp408 超窗（过去 301s）→ code=408。
func TestSupplySignMiddlewareExpiredTimestamp408(t *testing.T) {
	body := []byte(supplyTestVectorBody)
	old := strconv.FormatInt(time.Now().Add(-301*time.Second).Unix(), 10)
	r := newSignRouter(&fakeSupplyConfigReader{cfg: testSupplyConfig()})
	w := doSigned(t, r, body, buildSignedQuery(t, body, old))
	assertSupplyEnvelope(t, w, SupplyCodeTimestampExpired, "时间戳已超过有效期")
}

// TestSupplySignMiddlewareFutureTimestamp408 时间戳过于未来（+60s）→ code=408。
func TestSupplySignMiddlewareFutureTimestamp408(t *testing.T) {
	body := []byte(supplyTestVectorBody)
	future := strconv.FormatInt(time.Now().Add(60*time.Second).Unix(), 10)
	r := newSignRouter(&fakeSupplyConfigReader{cfg: testSupplyConfig()})
	w := doSigned(t, r, body, buildSignedQuery(t, body, future))
	assertSupplyEnvelope(t, w, SupplyCodeTimestampExpired, "时间戳已超过有效期")
}

// TestSupplySignMiddlewareWindowBoundary 300s 窗口边界：恰好 300s 通过，301s 拒绝。
func TestSupplySignMiddlewareWindowBoundary(t *testing.T) {
	body := []byte(supplyTestVectorBody)
	r := newSignRouter(&fakeSupplyConfigReader{cfg: testSupplyConfig()})

	boundary := strconv.FormatInt(time.Now().Add(-300*time.Second).Unix(), 10)
	w := doSigned(t, r, body, buildSignedQuery(t, body, boundary))
	if w.Code != http.StatusOK {
		t.Fatalf("300s boundary should pass, status=%d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Code != 0 {
		t.Fatalf("300s boundary expected code=0, got %d body=%s", got.Code, w.Body.String())
	}
}

// TestSupplySignMiddlewareNoConfigFailClosed 无配置/读取错误 → code=1,货源未配置（fail-closed）。
func TestSupplySignMiddlewareNoConfigFailClosed(t *testing.T) {
	body := []byte(supplyTestVectorBody)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	q := buildSignedQuery(t, body, ts)

	// reader 返回 (nil, nil)：无 active 配置。
	assertSupplyEnvelope(t, doSigned(t, newSignRouter(&fakeSupplyConfigReader{cfg: nil, err: nil}), body, q), SupplyCodeNoConfig, "货源未配置")
	// reader 返回 ErrSupplyNoConfig。
	assertSupplyEnvelope(t, doSigned(t, newSignRouter(&fakeSupplyConfigReader{cfg: nil, err: ErrSupplyNoConfig}), body, q), SupplyCodeNoConfig, "货源未配置")
	// reader 返回其他错误。
	assertSupplyEnvelope(t, doSigned(t, newSignRouter(&fakeSupplyConfigReader{cfg: nil, err: errors.New("boom")}), body, q), SupplyCodeNoConfig, "货源未配置")
	// nil reader（构造缺失）。
	assertSupplyEnvelope(t, doSigned(t, newSignRouter(nil), body, q), SupplyCodeNoConfig, "货源未配置")
	// 配置存在但密钥缺失（部分配置）→ 仍 fail-closed 到「货源未配置」。
	partial := testSupplyConfig()
	partial.SupplyAppSecret = ""
	assertSupplyEnvelope(t, doSigned(t, newSignRouter(&fakeSupplyConfigReader{cfg: partial}), body, q), SupplyCodeNoConfig, "货源未配置")
	partial2 := testSupplyConfig()
	partial2.MchSecret = ""
	assertSupplyEnvelope(t, doSigned(t, newSignRouter(&fakeSupplyConfigReader{cfg: partial2}), body, q), SupplyCodeNoConfig, "货源未配置")
}

// TestSupplySignMiddlewareEmptyBodyUsesMd5EmptyJSON 无 body 时用 md5("{}") 验签。
func TestSupplySignMiddlewareEmptyBodyUsesMd5EmptyJSON(t *testing.T) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	// body 为空：签名按 md5("{}") 计算。
	q := buildSignedQuery(t, nil, ts)
	if q.Get("sign") != SupplySign(supplyTestAppID, supplyTestAppSecret, goldenEmptyMd5, ts, supplyTestMchID, supplyTestMchSecret) {
		t.Fatalf("empty-body sign should use md5(\"{}\"): %q", q.Get("sign"))
	}
	r := newSignRouter(&fakeSupplyConfigReader{cfg: testSupplyConfig()})
	w := doSigned(t, r, nil, q)
	if w.Code != http.StatusOK {
		t.Fatalf("empty body request status=%d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Code int `json:"code"`
		Data struct {
			Echo string `json:"echo"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Code != 0 {
		t.Fatalf("empty body expected code=0, got %d body=%s", got.Code, w.Body.String())
	}
	if got.Data.Echo != "" {
		t.Fatalf("expected empty echoed body, got %q", got.Data.Echo)
	}
}

// assertSupplyEnvelope 断言响应为货源信封 {code,msg,data}，且 HTTP 恒为 200。
func assertSupplyEnvelope(t *testing.T, w *httptest.ResponseRecorder, wantCode int, wantMsg string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, w.Body.String())
	}
	if env.Code != wantCode {
		t.Fatalf("envelope code = %d, want %d, body=%s", env.Code, wantCode, w.Body.String())
	}
	if env.Msg != wantMsg {
		t.Fatalf("envelope msg = %q, want %q, body=%s", env.Msg, wantMsg, w.Body.String())
	}
}
