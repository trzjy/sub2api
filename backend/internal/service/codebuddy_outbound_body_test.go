//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCodeBuddyOutboundBodyRewritesMappedModel 锁定 CodeBuddy 出站模型名改写：
// 账号 mapping 命中且值不同于请求名时，发给上游的 body.model 必须是映射后的上游模型。
// 否则上游（CodeBuddy CN 目录）不认识分组别名，会返回 400 code=11102
// （2026-10-29 生产实证：deepseek-v4.1-flash-1017 别名 400，glm-5.3-flash 同名 200）。
func TestCodeBuddyOutboundBodyRewritesMappedModel(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-1017","messages":[{"role":"user","content":"hi"}]}`)
	got := codeBuddyOutboundBody(body, "deepseek-v4.1-flash-1017", "hy3")
	require.JSONEq(t, `{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`, string(got))
}

// TestCodeBuddyOutboundBodyNoopCases 锁定零行为变化分支：未命中映射（同名为恒等映射、
// upstreamModel 为空）时不得改动任何字节，避免影响历史 kimi/space-bunny 号出站。
func TestCodeBuddyOutboundBodyNoopCases(t *testing.T) {
	body := []byte(`{"model":"glm-5.3-flash","messages":[]}`)

	same := codeBuddyOutboundBody(body, "glm-5.3-flash", "glm-5.3-flash")
	require.Equal(t, body, same, "恒等映射必须原样返回")

	empty := codeBuddyOutboundBody(body, "glm-5.3-flash", "")
	require.Equal(t, body, empty, "空 upstreamModel 必须原样返回")
}
