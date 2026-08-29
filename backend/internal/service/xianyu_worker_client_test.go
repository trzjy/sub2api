package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateWorkerBaseURLRejectsLoopbackInCompose(t *testing.T) {
	// Compose 容器部署下禁止 127.0.0.1。
	require.ErrorIs(t, validateWorkerBaseURL("http://127.0.0.1:8089", true), ErrXianyuBaseURLLoopbackInvalid)
	require.ErrorIs(t, validateWorkerBaseURL("http://localhost:8089", true), ErrXianyuBaseURLLoopbackInvalid)
	require.ErrorIs(t, validateWorkerBaseURL("http://[::1]:8089", true), ErrXianyuBaseURLLoopbackInvalid)
}

func TestValidateWorkerBaseURLAcceptsDockerHostnameAndPrivateIP(t *testing.T) {
	require.NoError(t, validateWorkerBaseURL("http://xianyu-worker-backend:8089", true))
	require.NoError(t, validateWorkerBaseURL("http://172.16.0.5:8089", true))
	require.NoError(t, validateWorkerBaseURL("http://10.0.0.2:9090", true))
	require.NoError(t, validateWorkerBaseURL("http://192.168.1.10:8089", true))
}

func TestValidateWorkerBaseURLRejectsPublicAndMalformed(t *testing.T) {
	// 公网域名 / 公网 IP / HTTPS / 路径 / 查询参数 / 用户信息。
	require.ErrorIs(t, validateWorkerBaseURL("https://worker.example.com:8089", true), ErrXianyuBaseURLInvalid)
	require.ErrorIs(t, validateWorkerBaseURL("http://8.8.8.8:8089", true), ErrXianyuBaseURLInvalid)
	require.ErrorIs(t, validateWorkerBaseURL("http://sub2api.example.com:8089", true), ErrXianyuBaseURLInvalid)
	require.ErrorIs(t, validateWorkerBaseURL("http://xianyu-worker-backend:8089/path", true), ErrXianyuBaseURLInvalid)
	require.ErrorIs(t, validateWorkerBaseURL("http://xianyu-worker-backend:8089?x=1", true), ErrXianyuBaseURLInvalid)
	require.ErrorIs(t, validateWorkerBaseURL("http://user:pass@xianyu-worker-backend:8089", true), ErrXianyuBaseURLInvalid)
	require.ErrorIs(t, validateWorkerBaseURL("", true), ErrXianyuBaseURLInvalid)
	require.ErrorIs(t, validateWorkerBaseURL("xianyu-worker-backend:8089", true), ErrXianyuBaseURLInvalid)
	require.ErrorIs(t, validateWorkerBaseURL("http://xianyu-worker-backend", true), ErrXianyuBaseURLInvalid)
}

func TestAutoBindProductPrecedence(t *testing.T) {
	control := newXianyuControlStub()
	poolID := int64(1)
	product := XianyuProduct{
		ID: 1, AccountPK: 11, ItemID: "item-1", Title: "Standard Item",
		BindingStatus: XianyuBindingStatusUnmapped, BindingSource: XianyuBindingSourceAutoNew,
	}
	rules := []XianyuBindingRule{
		{ID: 1, AccountPK: 11, MatchType: XianyuBindingRuleKeyword, Keyword: "standard", PoolID: 10, Status: "active", Priority: 1},
		{ID: 2, AccountPK: 11, MatchType: XianyuBindingRuleAccountDefault, PoolID: poolID, Status: "active", Priority: 99},
	}
	require.NoError(t, autoBindProduct(t.Context(), control, product, rules))
	require.Equal(t, XianyuBindingStatusMapped, control.product.BindingStatus)
	require.Equal(t, XianyuBindingSourceKeyword, control.product.BindingSource)
	require.Equal(t, int64(10), *control.product.PoolID)
}

func TestAutoBindProductKeywordOverDefault(t *testing.T) {
	control := newXianyuControlStub()
	product := XianyuProduct{
		ID: 2, AccountPK: 11, ItemID: "item-2", Title: "special limited",
		BindingStatus: XianyuBindingStatusUnmapped,
	}
	rules := []XianyuBindingRule{
		{ID: 1, AccountPK: 11, MatchType: XianyuBindingRuleAccountDefault, PoolID: 5, Status: "active", Priority: 10},
		{ID: 2, AccountPK: 11, MatchType: XianyuBindingRuleKeyword, Keyword: "SPECIAL", PoolID: 6, Status: "active", Priority: 1},
	}
	// 关键词规则优先级更高，且大小写不敏感。
	require.NoError(t, autoBindProduct(t.Context(), control, product, rules))
	require.Equal(t, int64(6), *control.product.PoolID)
	require.Equal(t, XianyuBindingSourceKeyword, control.product.BindingSource)
}

func TestAutoBindProductDefaultPoolFallback(t *testing.T) {
	control := newXianyuControlStub()
	product := XianyuProduct{
		ID: 3, AccountPK: 11, ItemID: "item-3", Title: "nothing matching",
		BindingStatus: XianyuBindingStatusUnmapped,
	}
	rules := []XianyuBindingRule{
		{ID: 1, AccountPK: 11, MatchType: XianyuBindingRuleKeyword, Keyword: "nope", PoolID: 1, Status: "active", Priority: 1},
		{ID: 2, AccountPK: 11, MatchType: XianyuBindingRuleAccountDefault, PoolID: 7, Status: "active", Priority: 99},
	}
	require.NoError(t, autoBindProduct(t.Context(), control, product, rules))
	require.Equal(t, XianyuBindingStatusMapped, control.product.BindingStatus)
	require.Equal(t, int64(7), *control.product.PoolID)
	require.Equal(t, XianyuBindingSourceAccountDefault, control.product.BindingSource)
}

func TestAutoBindProductNoMatchStaysUnmapped(t *testing.T) {
	control := newXianyuControlStub()
	control.product = nil // 避免预置映射干扰断言
	product := XianyuProduct{
		ID: 4, AccountPK: 11, ItemID: "item-4", Title: "plain",
		BindingStatus: XianyuBindingStatusUnmapped,
	}
	rules := []XianyuBindingRule{
		{ID: 1, AccountPK: 11, MatchType: XianyuBindingRuleKeyword, Keyword: "nope", PoolID: 1, Status: "active", Priority: 1},
	}
	require.NoError(t, autoBindProduct(t.Context(), control, product, rules))
	require.Equal(t, 0, control.bindCalls)
}

func TestAutoBindProductDoesNotOverrideManual(t *testing.T) {
	control := newXianyuControlStub()
	control.product = nil // 避免预置映射干扰断言
	poolIDa := int64(9)
	product := XianyuProduct{
		ID: 5, AccountPK: 11, ItemID: "item-5", Title: "mapped already",
		BindingStatus: XianyuBindingStatusMapped, BindingSource: XianyuBindingSourceManual, PoolID: &poolIDa,
	}
	rules := []XianyuBindingRule{
		{ID: 1, AccountPK: 11, MatchType: XianyuBindingRuleKeyword, Keyword: "mapped", PoolID: 2, Status: "active", Priority: 1},
	}
	require.NoError(t, autoBindProduct(t.Context(), control, product, rules))
	// 已绑定商品不被自动覆盖：不触发任何绑定更新。
	require.Equal(t, 0, control.bindCalls)
}

func TestAutoBindProductIgnoresDisabledRulesAndOtherAccounts(t *testing.T) {
	control := newXianyuControlStub()
	control.product = nil // 避免预置映射干扰断言
	product := XianyuProduct{
		ID: 6, AccountPK: 11, ItemID: "item-6", Title: "disabled rule",
		BindingStatus: XianyuBindingStatusUnmapped,
	}
	rules := []XianyuBindingRule{
		{ID: 1, AccountPK: 11, MatchType: XianyuBindingRuleKeyword, Keyword: "disabled", PoolID: 1, Status: "disabled", Priority: 1},
		{ID: 2, AccountPK: 99, MatchType: XianyuBindingRuleAccountDefault, PoolID: 2, Status: "active", Priority: 1},
	}
	require.NoError(t, autoBindProduct(t.Context(), control, product, rules))
	require.Equal(t, 0, control.bindCalls)
}

func TestNormalizeProductIdentity(t *testing.T) {
	require.Equal(t, normalizeProductIdentity("a", "", ""), normalizeProductIdentity(" a ", "  ", ""))
	require.NotEqual(t, normalizeProductIdentity("a", "s", ""), normalizeProductIdentity("a", "t", ""))
}
