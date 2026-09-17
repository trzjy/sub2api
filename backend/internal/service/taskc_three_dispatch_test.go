package service

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// --- 测试账号构造：平台归并后官方平台 + access_mode 维度 ---

// taskcWebZhipuNew: 官方 zhipu 平台 + 显式 access_mode=web（归并后新形态）。
func taskcWebZhipuNew() *Account {
	return &Account{Platform: PlatformZhipu, Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "chatglm_token=x"}}
}

// taskcWebZhipuOld: 旧 web-zhipu 平台常量 + cookie（形状推断兜底，迁移前兼容读取）。
func taskcWebZhipuOld() *Account {
	return &Account{Platform: PlatformWebZhipu, Credentials: map[string]any{"cookie": "chatglm_token=x"}}
}

// taskcAPIZhipu: 官方 zhipu 平台 API 模式（无 access_mode、无 cookie）→ 走 CN API forwarder。
func taskcAPIZhipu() *Account {
	return &Account{Platform: PlatformZhipu, Credentials: map[string]any{}}
}

// taskcWebDeepseekNew / taskcAPIDeepseek / taskcWebKimiNew / taskcAPIKimi 同构。
func taskcWebDeepseekNew() *Account {
	return &Account{Platform: PlatformDeepseek, Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "ds_session_id=x"}}
}
func taskcAPIDeepseek() *Account {
	return &Account{Platform: PlatformDeepseek, Credentials: map[string]any{}}
}
func taskcWebKimiNew() *Account {
	return &Account{Platform: PlatformKimi, Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at"}}
}
func taskcAPIKimi() *Account {
	return &Account{Platform: PlatformKimi, Credentials: map[string]any{}}
}

// TestTaskC_ThreeDispatchPredicates 锁定转发三分派判定（方案 §5.3）：web access mode +
// 对应官方平台命中对应 forward；API 模式同平台账号绝不入选；跨平台 web 账号绝不串线。
func TestTaskC_ThreeDispatchPredicates(t *testing.T) {
	// zhipu 维度
	require.True(t, isWebZhipuAccount(taskcWebZhipuNew()), "new zhipu+web must dispatch to forwardWebZhipu")
	require.True(t, isWebZhipuAccount(taskcWebZhipuOld()), "legacy web-zhipu must dispatch to forwardWebZhipu")
	require.False(t, isWebZhipuAccount(taskcAPIZhipu()), "API zhipu must NOT enter forwardWebZhipu")
	require.False(t, isWebZhipuAccount(taskcWebDeepseekNew()), "web deepseek must NOT enter forwardWebZhipu")

	// deepseek 维度
	require.True(t, isWebDeepseekAccount(taskcWebDeepseekNew()), "new deepseek+web must dispatch to forwardWebDeepseek")
	require.False(t, isWebDeepseekAccount(taskcAPIDeepseek()), "API deepseek must NOT enter forwardWebDeepseek")
	require.False(t, isWebDeepseekAccount(taskcWebZhipuNew()), "web zhipu must NOT enter forwardWebDeepseek")

	// kimi 维度
	require.True(t, isWebKimiAccount(taskcWebKimiNew()), "new kimi+web must dispatch to forwardWebKimi")
	require.False(t, isWebKimiAccount(taskcAPIKimi()), "API kimi must NOT enter forwardWebKimi")
	require.False(t, isWebKimiAccount(taskcWebDeepseekNew()), "web deepseek must NOT enter forwardWebKimi")
}

// TestTaskC_WebReverseAccountUnion 锁定兼容并集判定：旧 web-* 平台或官方平台 + web
// access mode 均命中 isWebReverseAccount（/v1/messages 分派口径）。
func TestTaskC_WebReverseAccountUnion(t *testing.T) {
	require.True(t, isWebReverseAccount(taskcWebZhipuNew()))
	require.True(t, isWebReverseAccount(taskcWebZhipuOld()))
	require.True(t, isWebReverseAccount(taskcWebDeepseekNew()))
	require.True(t, isWebReverseAccount(taskcWebKimiNew()))
	require.False(t, isWebReverseAccount(taskcAPIZhipu()))
	require.False(t, isWebReverseAccount(taskcAPIDeepseek()))
	require.False(t, isWebReverseAccount(taskcAPIKimi()))
	// 归一键：官方平台 + web 取对应 web-* 键。
	require.Equal(t, PlatformWebZhipu, webProviderKeyForAccount(taskcWebZhipuNew()))
	require.Equal(t, PlatformWebDeepseek, webProviderKeyForAccount(taskcWebDeepseekNew()))
	require.Equal(t, PlatformWebKimi, webProviderKeyForAccount(taskcWebKimiNew()))
}

// TestTaskC_WebZhipuDoubleAssertion 锁定 forwardWebZhipu 入口双断言 fail-closed（隔离红线 §6）：
// API 模式 zhipu 账号绝不入网页协议链；web 模式 deepseek 账号绝不误入 forwardWebZhipu。
func TestTaskC_WebZhipuDoubleAssertion(t *testing.T) {
	// API 模式 zhipu → 接入模式断言失败。
	err := assertWebZhipuAccount(taskcAPIZhipu())
	require.Error(t, err)
	require.Contains(t, err.Error(), "web access mode")

	// web 模式 deepseek → 平台断言失败（误入 zhipu 适配器）。
	err = assertWebZhipuAccount(taskcWebDeepseekNew())
	require.Error(t, err)
	require.Contains(t, err.Error(), "zhipu platform")

	// web 模式 zhipu（新/旧）双断言通过。
	require.NoError(t, assertWebZhipuAccount(taskcWebZhipuNew()))
	require.NoError(t, assertWebZhipuAccount(taskcWebZhipuOld()))

	// nil 账号。
	require.Error(t, assertWebZhipuAccount(nil))
}

// TestTaskC_WebDeepseekDoubleAssertion 同构：forwardWebDeepseek 双断言。
func TestTaskC_WebDeepseekDoubleAssertion(t *testing.T) {
	require.Error(t, assertWebDeepseekAccount(taskcAPIDeepseek()))
	require.Contains(t, assertWebDeepseekAccount(taskcAPIDeepseek()).Error(), "web access mode")

	require.Error(t, assertWebDeepseekAccount(taskcWebZhipuNew()))
	require.Contains(t, assertWebDeepseekAccount(taskcWebZhipuNew()).Error(), "deepseek platform")

	require.NoError(t, assertWebDeepseekAccount(taskcWebDeepseekNew()))
}

// TestTaskC_WebKimiDoubleAssertion 同构：forwardWebKimi 双断言。
func TestTaskC_WebKimiDoubleAssertion(t *testing.T) {
	require.Error(t, assertWebKimiAccount(taskcAPIKimi()))
	require.Contains(t, assertWebKimiAccount(taskcAPIKimi()).Error(), "web access mode")

	require.Error(t, assertWebKimiAccount(taskcWebDeepseekNew()))
	require.Contains(t, assertWebKimiAccount(taskcWebDeepseekNew()).Error(), "kimi platform")

	require.NoError(t, assertWebKimiAccount(taskcWebKimiNew()))
}

// TestTaskC_ForwardEntryFailClosed 确认双断言在转发入口即生效（任何网络请求之前返回错误）：
// API 模式 zhipu 账号经 forwardWebZhipu 入口返回接入模式错误，绝不触达网页协议链。
func TestTaskC_ForwardEntryFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &OpenAIGatewayService{}
	_, err := svc.forwardWebZhipu(context.Background(), c, taskcAPIZhipu(), []byte(`{}`), "glm-5.3-flash", false, time.Now(), webResponseModeChat)
	require.Error(t, err)
	require.Contains(t, err.Error(), "web access mode")

	_, err = svc.forwardWebDeepseek(context.Background(), c, taskcAPIDeepseek(), []byte(`{}`), "deepseek-chat", false, time.Now(), webResponseModeChat)
	require.Error(t, err)
	require.Contains(t, err.Error(), "web access mode")

	_, err = svc.forwardWebKimi(context.Background(), c, taskcAPIKimi(), []byte(`{}`), "kimi-k3", false, time.Now(), webResponseModeChat)
	require.Error(t, err)
	require.Contains(t, err.Error(), "web access mode")
}

// TestTaskC_WebZhipuUnknownModelFailClosed 确认 zhipu web 账号空 model_mapping 时未知模型
// 失败关闭（ValidateWebZhipuModel），且发生在任何上游请求之前（不经 API 目录回落）。
func TestTaskC_WebZhipuUnknownModelFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &OpenAIGatewayService{}
	acct := &Account{
		Platform:    PlatformWebZhipu,
		Credentials: map[string]any{"cookie": "chatglm_token=x", "base_url": "https://chatglm.cn"},
	}
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	_, err := svc.forwardWebZhipu(context.Background(), c, acct, body, "gpt-4", false, time.Now(), webResponseModeChat)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not supported")
}
