package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAccountListItemQuotaKeySerialization 锁定 v3.3 §4.2 / C2 的 DTO 契约：
//   - AccountListItem 暴露脱敏标量 quota_key_id / quota_key_label
//   - 账号项【禁止】内嵌完整 QuotaKey 聚合对象（F7）
func TestAccountListItemQuotaKeySerialization(t *testing.T) {
	qkID := int64(99)
	label := "XXXX-****-****-1234"
	item := &AccountListItem{
		ID:            1,
		Name:          "cursor-acc",
		Platform:      "cursor",
		Type:          "apikey",
		QuotaKeyID:    &qkID,
		QuotaKeyLabel: &label,
	}

	data, err := json.Marshal(item)
	require.NoError(t, err)
	body := string(data)

	require.Contains(t, body, `"quota_key_id":99`, "must expose quota_key_id scalar")
	require.Contains(t, body, `"quota_key_label":"XXXX-****-****-1234"`, "must expose quota_key_label scalar")
	// F7：账号项不得内嵌完整 QuotaKey 聚合（如 code_masked 等聚合字段不应出现在 item 内部）
	require.NotContains(t, body, `"code_masked"`, "account item must NOT embed full QuotaKey aggregate (F7)")
	require.NotContains(t, body, `"quota_key":`, "account item must NOT nest a quota_key object")

	// 缺省时两个脱敏字段必须省略（omitempty）
	plain := &AccountListItem{ID: 2, Name: "openai-acc", Platform: "openai", Type: "apikey"}
	plainData, err := json.Marshal(plain)
	require.NoError(t, err)
	require.NotContains(t, string(plainData), "quota_key_id", "omit quota_key_id when nil")
	require.NotContains(t, string(plainData), "quota_key_label", "omit quota_key_label when nil")
}
