//go:build unit

package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 缺陷2：recordAggregatedStreamUpstreamError 不得把「平台对客户端的映射值 502」
// 写成上游状态码。
//
// 生产形态：上游已回 200（能读到部分 SSE 才走到聚合失败），平台回写 502。
// 旧实现用 setOpsUpstreamError(c, 502, ...) 同时污染 OpsUpstreamStatusCodeKey
// 与 attempt 的 upstream_status_code，与函数自身注释「不覆盖真实上游语义」相反。
// 落库后 applyOpsUpstreamFieldsFromContext 写入 entry.UpstreamStatusCode=502，
// 进入 queryErrorCounts 的 COALESCE(upstream_status_code, status_code, 0)，
// 污染 recovered 行的 429/529 分类与 upstream_excl 归属。
//
// 修复后契约：
//  1. OpsUpstreamStatusCodeKey 不被本函数写入（上游 200 是真实语义，
//     而 ops 状态键留给「上游真的报了非 2xx」的路径）；
//  2. attempt 事件登记上游真实回包状态码；
//  3. 上游错误上下文仍然建立（phase=upstream、owner=provider 不受影响）——
//     这是本函数存在的唯一目的，不能因为去掉状态码登记而丢失。

func newAggregatedErrCtx() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

// 上游真实回包 200（聚合失败发生在响应体内）→ 一律不得登记成 502。
func TestRecordAggregatedStreamUpstreamError_DoesNotFabricate502(t *testing.T) {
	c := newAggregatedErrCtx()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	// 用 Header.Set 而非字面量：Header.Get 会规范化 key（x-request-id → X-Request-Id），
	// 字面量小写 key 取不到，会让本用例假失败。
	resp.Header.Set("x-request-id", "req-1")

	recordAggregatedStreamUpstreamError(c, nil, resp, errors.New("stream ended early"))

	// 1. 不得写入合成的 502。
	if v, ok := c.Get(OpsUpstreamStatusCodeKey); ok {
		switch code := v.(type) {
		case int:
			require.NotEqual(t, http.StatusBadGateway, code,
				"平台映射值 502 不得写入 OpsUpstreamStatusCodeKey")
		case int64:
			require.NotEqual(t, int64(http.StatusBadGateway), code,
				"平台映射值 502 不得写入 OpsUpstreamStatusCodeKey")
		}
	}

	// 2. attempt 事件登记上游真实状态码（200），而非 502。
	events, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok, "attempt 事件必须登记，否则 phase/owner 归因丢失")
	list, ok := events.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, list, 1)
	require.Equal(t, http.StatusOK, list[0].UpstreamStatusCode,
		"attempt 事件必须承载上游真实回包状态码")
	require.Equal(t, "aggregated_stream_failed", list[0].Reason,
		"attempt 语义由 kind/reason 承载，不靠合成状态码")
	require.Equal(t, "request_error", list[0].Kind)
	require.Equal(t, "req-1", list[0].UpstreamRequestID)
}

// 上游真回 5xx 时必须如实登记，不能因为「不写状态键」把真实上游状态也丢掉。
func TestRecordAggregatedStreamUpstreamError_KeepsRealUpstreamStatus(t *testing.T) {
	c := newAggregatedErrCtx()
	resp := &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}}

	recordAggregatedStreamUpstreamError(c, nil, resp, errors.New("bad frame"))

	events, _ := c.Get(OpsUpstreamErrorsKey)
	list, ok := events.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, list, 1)
	require.Equal(t, http.StatusBadGateway, list[0].UpstreamStatusCode)
}

// 上游错误上下文必须仍然成立：本函数存在的目的是把归因从 platform/internal
// 纠正到 upstream/provider，去掉状态码登记后不能连这条一起丢掉。
func TestRecordAggregatedStreamUpstreamError_StillEstablishesUpstreamContext(t *testing.T) {
	c := newAggregatedErrCtx()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}

	recordAggregatedStreamUpstreamError(c, nil, resp, errors.New("stream ended early"))

	require.True(t, hasOpsUpstreamContextForTest(c),
		"上游错误上下文仍须建立：hasOpsUpstreamErrorContext 依赖 attempt 事件非空")
}

func hasOpsUpstreamContextForTest(c *gin.Context) bool {
	if v, ok := c.Get(OpsUpstreamErrorsKey); ok {
		if events, ok := v.([]*OpsUpstreamErrorEvent); ok && len(events) > 0 {
			return true
		}
	}
	return false
}

// resp 不可用时不臆造状态码：留 0，让落库侧 COALESCE 回落到客户端 status_code。
func TestRecordAggregatedStreamUpstreamError_NilRespLeavesStatusUnset(t *testing.T) {
	c := newAggregatedErrCtx()

	recordAggregatedStreamUpstreamError(c, nil, nil, errors.New("no response"))

	events, _ := c.Get(OpsUpstreamErrorsKey)
	list, ok := events.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, list, 1)
	require.Zero(t, list[0].UpstreamStatusCode, "resp 缺失时不得臆造上游状态码")
	if v, ok := c.Get(OpsUpstreamStatusCodeKey); ok {
		require.NotEqual(t, http.StatusBadGateway, v)
	}
}
