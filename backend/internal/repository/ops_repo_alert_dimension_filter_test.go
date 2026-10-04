//go:build unit

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestBuildOpsAlertEventsWhere_DimensionExact 验证按完整维度键值的 JSONB 文本精确匹配
// 生成正确 WHERE 条件：数值（int64/float64）按 JSON 数字文本比较、字符串原文比较、
// 键含单引号时转义防注入。该能力支撑 GetActiveFreshnessAlert 的"按维度唯一查询活跃事件"，
// 不以固定上限兜底存在性判定。
func TestBuildOpsAlertEventsWhere_DimensionExact(t *testing.T) {
	where, args := buildOpsAlertEventsWhere(&service.OpsAlertEventFilter{
		Status: service.OpsAlertStatusFiring,
		DimensionExact: map[string]any{
			"kind":       "account_model",
			"account_id": int64(1),
			"scope":      "gpt-4",
		},
	})

	require.Contains(t, where, "status = $1")
	require.Contains(t, where, "(dimensions->>'kind') = ")
	require.Contains(t, where, "(dimensions->>'account_id') = ")
	require.Contains(t, where, "(dimensions->>'scope') = ")
	// 三个维度键各自绑定一个占位符，且不依赖 map 遍历顺序（子句顺序不定）。
	dimArgs := map[string]string{}
	for _, a := range args[1:] {
		s, ok := a.(string)
		require.True(t, ok, "维度过滤值应为字符串文本")
		dimArgs[s] = s
	}
	require.Equal(t, map[string]string{"account_model": "account_model", "1": "1", "gpt-4": "gpt-4"}, dimArgs,
		"数值维度应按 JSON 数字文本比较（int64 → \"1\"）")
	require.Equal(t, service.OpsAlertStatusFiring, args[0], "第一个占位符是 status")
}

// TestDimValueToText_NumericText 维度值转 JSONB ->> 文本比较值：int64/float64 输出与
// json.Marshal 的 JSON 数字一致（整数不带小数点）。
func TestDimValueToText_NumericText(t *testing.T) {
	require.Equal(t, "7", dimValueToText(int64(7)))
	require.Equal(t, "7", dimValueToText(7))
	require.Equal(t, "7", dimValueToText(float64(7)))
	require.Equal(t, "1.5", dimValueToText(float64(1.5)))
	require.Equal(t, "gpt-4", dimValueToText("gpt-4"))
}

// TestQuoteJSONKey_EscapesQuote 维度键中的单引号须被转义为 SQL 的 ''（防注入），
// 且首尾保留包裹单引号。
func TestQuoteJSONKey_EscapesQuote(t *testing.T) {
	require.Equal(t, "'ac''count'", quoteJSONKey("ac'count"))
	require.Equal(t, "'scope'", quoteJSONKey("scope"))
}
