package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func rawToolsOf(t *testing.T, body string) []any {
	t.Helper()
	var tools []any
	require.NoError(t, json.Unmarshal([]byte(body), &tools))
	return tools
}

// 原生转发路径（map[string]any）与原 chat 桥路径（ResponsesTool）必须给同一个答案：
// 同一条工具在两个声明站点各写一次，合并后只剩一个。
func TestMergeDeclaredRawResponsesToolsDeduplicatesAcrossSites(t *testing.T) {
	base := rawToolsOf(t, `[{"type":"function","name":"wait","description":"top-level"}]`)
	extra := rawToolsOf(t, `[{"type":"function","name":"wait","description":"additional"},
		{"type":"function","name":"get_weather"}]`)

	merged, dropped := MergeDeclaredRawResponsesTools(base, extra)
	require.True(t, dropped, "丢弃了重复项要如实回报，调用方据此决定是否重写请求体")
	require.Len(t, merged, 2, "同名工具跨站点只算一个")
	first, _ := merged[0].(map[string]any)
	second, _ := merged[1].(map[string]any)
	require.Equal(t, "wait", first["name"])
	require.Equal(t, "top-level", first["description"], "先见者为准")
	require.Equal(t, "get_weather", second["name"])
}

// 批内部重复不在合并阶段处理：那是客户端定义冲突，要能透出去显式报错。
func TestMergeDeclaredRawResponsesToolsKeepsWithinBatchDuplicates(t *testing.T) {
	base := rawToolsOf(t, `[]`)
	extra := rawToolsOf(t, `[{"type":"function","name":"wait"},{"type":"function","name":"wait"}]`)

	merged, dropped := MergeDeclaredRawResponsesTools(base, extra)
	require.False(t, dropped)
	require.Len(t, merged, 2, "同一批里的两个同名声明原样保留")
}

// typed 路径与 raw 路径的身份判定必须一致，否则两条链路对"重复"的理解会再次分叉。
func TestResponsesToolIdentityMatchesBetweenTypedAndRaw(t *testing.T) {
	cases := []struct{ typeName, name string }{
		{"function", "wait"},
		{"Function", " Wait "},
		{"custom", "exec"},
		{"namespace", "collaboration"},
		{"web_search", ""},
		{"", "bare-name"},
	}
	for _, tc := range cases {
		raw := map[string]any{}
		if tc.typeName != "" {
			raw["type"] = tc.typeName
		}
		if tc.name != "" {
			raw["name"] = tc.name
		}
		require.Equal(t,
			responsesToolIdentity(ResponsesTool{Type: tc.typeName, Name: tc.name}),
			responsesToolIdentityOfRaw(raw),
			"typed 与 raw 身份必须同值: %+v", tc)
	}
}

// 身份键必须把类型也纳入：同名但类型不同（function vs custom）不是同一条工具，
// 它们的冲突由下游的显式检查负责，不能被身份合并悄悄吞掉。
func TestResponsesToolIdentityDistinguishesType(t *testing.T) {
	require.NotEqual(t,
		responsesToolIdentity(ResponsesTool{Type: "function", Name: "exec"}),
		responsesToolIdentity(ResponsesTool{Type: "custom", Name: "exec"}))
}
