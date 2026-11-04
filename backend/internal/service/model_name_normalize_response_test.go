package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResponseModelNameNormalizedForClient 锁死用户裁定：
// 上游提供的 deepseek-v4.1-flash-1017 经过本站必须归一化成
// deepseek-v4.1-flash 再提供给用户。
//
// 重点覆盖「客户端直接用上游写法请求」的场景（originalModel == mappedModel == -1017）：
// 这时外层 if 守卫（originalModel != mappedModel）原本直接跳过改写，客户端在响应体
// 和流里看到的仍是上游真名 -1017，与归一化裁定冲突。
func TestResponseModelNameNormalizedForClient(t *testing.T) {
	svc := &OpenAIGatewayService{}
	const upstream = "deepseek-v4.1-flash-1017"
	const canonical = "deepseek-v4.1-flash"

	t.Run("json_non_stream", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash-1017","choices":[{"message":{"content":"ok"}}]}`)
		got := string(svc.replaceModelInResponseBody(body, upstream, upstream))
		require.Contains(t, got, `"model":"`+canonical+`"`)
		require.NotContains(t, got, upstream)
	})

	t.Run("sse_stream", func(t *testing.T) {
		stream := "data: {\"model\":\"deepseek-v4.1-flash-1017\",\"choices\":[]}\n\n"
		got := svc.replaceModelInSSEBody(stream, upstream, upstream)
		require.Contains(t, got, canonical)
		require.NotContains(t, got, upstream)
	})

	// 非别名族模型一律原样，避免归一化顺带改坏别的模型名。
	t.Run("out_of_family_untouched", func(t *testing.T) {
		for _, model := range []string{"gpt-5.6-sol", "claude-opus-5", "glm-5.3-flash"} {
			body := []byte(`{"model":"` + model + `"}`)
			got := string(svc.replaceModelInResponseBody(body, model, model))
			require.Equal(t, string(body), got, "族外模型不得被改写：%s", model)
		}
	})

	// 族内其它写法也归到统一写法（用户裁定：去掉尾部版本后缀）。
	t.Run("family_alias_normalized", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-pro-0813"}`)
		got := string(svc.replaceModelInResponseBody(body, "deepseek-v4-pro-0813", "deepseek-v4-pro-0813"))
		require.Contains(t, got, `"model":"deepseek-v4-pro"`)
	})
}

// TestNormalizeResponseModelFamilyForAllAccounts 锁死：**普通（非掩码）账号**的响应
// 也必须做同族改名。
//
// 生产实证（2026-11-04）：客服号 239 的映射链是
// deepseek-v4.1-flash → deepseek-v4.1-flash-1017，上游回显 -1017；raw chat 路径的
// 改写门只看掩码账号（rawChatCompletionsMaskGate），普通账号原样透传，用户就在响应里
// 看见了 deepseek-v4.1-flash-1017，与归一化裁定冲突。
func TestNormalizeResponseModelFamilyForAllAccounts(t *testing.T) {
	svc := &OpenAIGatewayService{}
	const upstream = "deepseek-v4.1-flash-1017"
	const canonical = "deepseek-v4.1-flash"

	t.Run("ordinary_account_family_rewritten", func(t *testing.T) {
		body := []byte(`{"model":"` + upstream + `","choices":[{"message":{"content":"ok"}}]}`)
		got := string(svc.normalizeResponseModelFamily(body))
		require.Contains(t, got, `"model":"`+canonical+`"`)
		require.NotContains(t, got, upstream)
	})

	// 跨族映射必须如实显示上游真实模型：把 deepseek-v4-pro 说成 deepseek-v4.1-flash
	// 会误导用户（那是两个不同模型）。同族改名只对同一族的名字生效。
	t.Run("cross_family_left_truthful", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-pro","choices":[]}`)
		require.Equal(t, string(body), string(svc.normalizeResponseModelFamily(body)))
	})

	t.Run("already_canonical_untouched", func(t *testing.T) {
		body := []byte(`{"model":"` + canonical + `","choices":[]}`)
		require.Equal(t, string(body), string(svc.normalizeResponseModelFamily(body)))
	})

	t.Run("no_model_field_untouched", func(t *testing.T) {
		body := []byte(`{"choices":[]}`)
		require.Equal(t, string(body), string(svc.normalizeResponseModelFamily(body)))
	})

	t.Run("non_json_untouched", func(t *testing.T) {
		body := []byte(`not json at all`)
		require.Equal(t, string(body), string(svc.normalizeResponseModelFamily(body)))
	})
}
