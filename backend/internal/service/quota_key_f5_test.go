//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CUR-F5：R2 回修验证。
//   R2-1：activate 非零业务码不得带英文前缀进手动响应（改用 UpstreamPullError 承载）。
//   R2-2：裸词"失效"删出终态匹配集（否定式/裸失效文案不得误判 expired）。
// ---------------------------------------------------------------------------

// TestCURF5_Activate_NonZeroCode_ExpiredCarriesOriginalMessage：activate 返回过期类
// 业务码错误时，手动响应取到的 message 为上游中文原文（无英文前缀），且归类准确。
func TestCURF5_Activate_NonZeroCode_ExpiredCarriesOriginalMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":400,"msg":"密钥已过期"}`))
	}))
	defer srv.Close()

	client := &shareAIUpstreamClient{
		httpClient:   srv.Client(),
		primaryBase:  srv.URL,
		fallbackBase: srv.URL,
	}
	err := client.activate(context.Background(), "anycode", "DID12345")
	require.Error(t, err, "activate non-zero code must surface an error (not success)")

	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe), "activate non-zero code must return *UpstreamPullError")
	require.Equal(t, UpstreamExpired, upe.Kind)
	require.Equal(t, "密钥已过期", upe.Message(), "manual message must be the original Chinese msg verbatim")
	require.NotContains(t, upe.Message(), "shareai:", "manual message must NOT carry the English prefix")
	require.True(t, errors.Is(err, ErrUpstreamKeyExpired), "expired activate msg must be classified as expired")
	require.Contains(t, upe.Error(), "activate code 400", "cause must retain internal context (business code)")
}

// TestCURF5_Activate_NonZeroCode_CooldownClassified：限频类 activate 错误同样归类冷却。
func TestCURF5_Activate_NonZeroCode_CooldownClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":400,"msg":"每5分钟内仅可刷新1次，请3分钟后再试"}`))
	}))
	defer srv.Close()

	client := &shareAIUpstreamClient{
		httpClient:   srv.Client(),
		primaryBase:  srv.URL,
		fallbackBase: srv.URL,
	}
	err := client.activate(context.Background(), "anycode", "DID12345")
	require.Error(t, err)

	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe))
	require.Equal(t, UpstreamCooldown, upe.Kind)
	require.Contains(t, upe.Message(), "每5分钟内仅可刷新1次")
	require.NotContains(t, upe.Message(), "shareai:", "manual message must NOT carry the English prefix")
	require.True(t, errors.Is(err, ErrUpstreamCooldown))
}

// TestCURF5_Activate_NonZeroCode_UnknownNoEnglishPrefix：未知文案走 UpstreamUnknown，
// message 仍逐字回显、无英文前缀；cause 仍保留内部上下文。
func TestCURF5_Activate_NonZeroCode_UnknownNoEnglishPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":400,"msg":"系统当前繁忙，请稍后再试"}`))
	}))
	defer srv.Close()

	client := &shareAIUpstreamClient{
		httpClient:   srv.Client(),
		primaryBase:  srv.URL,
		fallbackBase: srv.URL,
	}
	err := client.activate(context.Background(), "anycode", "DID12345")
	require.Error(t, err)

	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe))
	require.Equal(t, UpstreamUnknown, upe.Kind)
	require.Equal(t, "系统当前繁忙，请稍后再试", upe.Message(), "unknown msg must be echoed verbatim")
	require.NotContains(t, upe.Message(), "shareai:", "manual message must NOT carry the English prefix")
	require.False(t, errors.Is(err, ErrUpstreamKeyExpired))
	require.False(t, errors.Is(err, ErrUpstreamCooldown))
	require.Contains(t, upe.Error(), "activate code 400")
}

// TestCURF5_ClassifyUpstreamError_NoFalsePositiveOnShixiao：R2-2 裸词"失效"移除后，
// 否定式（"未失效"）与裸"失效"文案落入未知分支，不得标 expired。
func TestCURF5_ClassifyUpstreamError_NoFalsePositiveOnShixiao(t *testing.T) {
	neg := classifyUpstreamError("密钥未失效")
	require.False(t, errors.Is(neg, ErrUpstreamKeyExpired), "否定句'未失效' 不得判过期")
	require.False(t, errors.Is(neg, ErrUpstreamCooldown))

	bare := classifyUpstreamError("密钥失效")
	require.False(t, errors.Is(bare, ErrUpstreamKeyExpired), "裸词'密钥失效' 落入未知分支，不得标 expired")
	require.False(t, errors.Is(bare, ErrUpstreamCooldown))
	var upe *UpstreamPullError
	require.True(t, errors.As(bare, &upe))
	require.Equal(t, UpstreamUnknown, upe.Kind, "'密钥失效' 必须归未知失败关闭分支")

	// 三个生产真实文案正例回归不变。
	expired1 := classifyUpstreamError("密钥额度耗尽或已过期")
	require.True(t, errors.Is(expired1, ErrUpstreamKeyExpired), "'密钥额度耗尽或已过期' 仍判 expired")

	expired2 := classifyUpstreamError("密钥已过期")
	require.True(t, errors.Is(expired2, ErrUpstreamKeyExpired), "'密钥已过期' 仍判 expired")

	cooldown := classifyUpstreamError("每5分钟内仅可刷新1次，请3分钟后再试")
	require.True(t, errors.Is(cooldown, ErrUpstreamCooldown), "生产冷却样本仍判冷却")
	require.False(t, errors.Is(cooldown, ErrUpstreamKeyExpired))
}
