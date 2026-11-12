package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// newMappingTestAccount 构造带账号级 model_mapping 的账号。
//
// 生产实证形态（2026-11-12）：账号 982 的 model_mapping 为
// {"deepseek-flash": "spark-x2.6-4b"}，上游讯飞对该账号的请求一律回
// 400 {"error":{"code":10005,"message":"invalid request"}}（10005 = 用户参数值有错误）。
func newMappingTestAccount(mapping map[string]string) *Account {
	creds := map[string]any{}
	if len(mapping) > 0 {
		mm := map[string]any{}
		for k, v := range mapping {
			mm[k] = v
		}
		creds["model_mapping"] = mm
	}
	return &Account{
		ID:          982,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Credentials: creds,
	}
}

func TestIsMappingInducedRejection(t *testing.T) {
	acc := newMappingTestAccount(map[string]string{"deepseek-flash": "spark-x2.6-4b"})

	require.True(t, IsMappingInducedRejection(acc, "deepseek-flash", http.StatusBadRequest),
		"改写过模型名的 400 必须归因到账号/上游侧")
	require.True(t, IsMappingInducedRejection(acc, "deepseek-flash", http.StatusUnprocessableEntity),
		"改写过模型名的 4xx 一律归因到账号/上游侧")

	// 客户自己把请求写错：请求没经过改写，行为必须与改造前完全一致。
	require.False(t, IsMappingInducedRejection(acc, "deepseek-v4-pro", http.StatusBadRequest),
		"未命中映射的 400 仍是客户侧问题")

	// 恒等映射等价于没换模型。
	ident := newMappingTestAccount(map[string]string{"deepseek-flash": "deepseek-flash"})
	require.False(t, IsMappingInducedRejection(ident, "deepseek-flash", http.StatusBadRequest),
		"恒等映射不算改写")

	// 5xx 属上游可用性问题，不归本判据。
	require.False(t, IsMappingInducedRejection(acc, "deepseek-flash", http.StatusBadGateway))
	require.False(t, IsMappingInducedRejection(acc, "deepseek-flash", http.StatusInternalServerError))

	// 没配映射的账号。
	plain := newMappingTestAccount(nil)
	require.False(t, IsMappingInducedRejection(plain, "deepseek-flash", http.StatusBadRequest))

	// 防御：nil 账号 / 空查表键。
	require.False(t, IsMappingInducedRejection(nil, "deepseek-flash", http.StatusBadRequest))
	require.False(t, IsMappingInducedRejection(acc, "   ", http.StatusBadRequest))
}

func TestShouldFailoverOpenAIUpstreamResponse_MappingInduced(t *testing.T) {
	svc := &OpenAIGatewayService{}
	acc := newMappingTestAccount(map[string]string{"deepseek-flash": "spark-x2.6-4b"})
	body := []byte(`{"error":{"code":10005,"message":"invalid request","type":"invalid_request_error"}}`)

	require.True(t,
		svc.shouldFailoverOpenAIUpstreamResponse(acc, http.StatusBadRequest, "invalid request", body, "deepseek-flash"),
		"改写归因的 400 必须换号，否则客户会一直吃到同一句他改不动的报错")

	require.False(t,
		svc.shouldFailoverOpenAIUpstreamResponse(acc, http.StatusBadRequest, "invalid request", body, "deepseek-v4-pro"),
		"客户自己的 400 保持既有行为：不换号")
}

func TestNewOpenAIAccountFailoverError_MappingInduced(t *testing.T) {
	svc := &OpenAIGatewayService{}
	acc := newMappingTestAccount(map[string]string{"deepseek-flash": "spark-x2.6-4b"})
	body := []byte(`{"error":{"code":10005,"message":"invalid request"}}`)

	err := svc.newOpenAIAccountFailoverError(acc, http.StatusBadRequest, http.Header{}, body, "invalid request", false, false, "deepseek-flash")
	require.NotNil(t, err)
	require.True(t, err.ShouldRetryNextAccount(), "必须换号")
	require.Equal(t, modelMappingInducedRejectionReason, err.Reason)
	require.Equal(t, http.StatusBadGateway, err.ClientStatusCode,
		"换号耗尽时按上游不可用告知，不把上游原文当成客户的错")
	require.NotEmpty(t, err.ClientMessage)
	require.False(t, err.RetryableOnSameAccount, "同一账号再试只会撞同一面墙")

	// 未改写：既有行为不变（不改归因、不设 502 文案）。
	// 注意「要不要换号」由判定层 shouldFailoverOpenAIUpstreamResponse 决定，
	// 构造层只负责把「已判定为改写归因」翻译成错误对象的字段。
	err2 := svc.newOpenAIAccountFailoverError(acc, http.StatusBadRequest, http.Header{}, body, "invalid request", false, false, "deepseek-v4-pro")
	require.NotEqual(t, modelMappingInducedRejectionReason, err2.Reason,
		"未改写时不得套用改写归因")
	require.Zero(t, err2.ClientStatusCode,
		"未改写时不得改写客户端可见状态码")
	require.Empty(t, err2.ClientMessage)
}

func TestShouldFailoverUpstreamStatusCode(t *testing.T) {
	for _, code := range []int{401, 402, 403, 405, 429, 529, 500, 502, 503, 504} {
		require.True(t, shouldFailoverUpstreamStatusCode(code), "状态码 %d 应换号", code)
	}
	for _, code := range []int{200, 204, 400, 404, 409, 422} {
		require.False(t, shouldFailoverUpstreamStatusCode(code), "状态码 %d 不应换号", code)
	}
}

func TestIsUpstreamFirstByteTimeout(t *testing.T) {
	// 结构化标记命中：通用护栏 / CN 家族首包超时 / OpenAI 首输出超时。
	require.True(t, IsUpstreamFirstByteTimeout(&UpstreamFailoverError{Reason: GatewayFirstByteTimeoutReason}, nil))

	// 响应体 type 命中：历史构造点与既有契约测试以响应体为准。
	require.True(t, IsUpstreamFirstByteTimeout(&UpstreamFailoverError{},
		[]byte(`{"error":{"type":"upstream_first_byte_timeout"}}`)))

	// 都不命中。
	require.False(t, IsUpstreamFirstByteTimeout(&UpstreamFailoverError{}, []byte(`{"error":{"type":"upstream_error"}}`)))
	require.False(t, IsUpstreamFirstByteTimeout(&UpstreamFailoverError{Reason: "other"}, nil))
	require.False(t, IsUpstreamFirstByteTimeout(nil, nil))
}
