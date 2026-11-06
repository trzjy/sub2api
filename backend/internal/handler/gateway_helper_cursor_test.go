package handler

// 本文件覆盖 CARD-KC-06：Cursor 平台下两种账号形态的**计费驱动方式**区分。
//
// 背景：Cursor 平台有两种账号形态——
//  1. ShareAI 号池 OAuth 账号（type≠api_key）：经 CursorExecutionService 会话式出站，
//     用量由会话终态的唯一结算钩子驱动，handler 在 HTTP 返回时不得直接提交；
//  2. 第三方 OpenAI 兼容反代账号（如 kn-cy.com，type=api_key）：标准 HTTP REST 转发，
//     没有会话终态可挂，必须由 handler 在 HTTP 返回时直接提交用量。
//
// 两类判定必须成对一致：注册侧（openai_chat_completions.go）只对会话式账号注册钩子，
// 提交侧（submitChatUsage）只跳过会话式账号的直接提交。若 api_key 形态被判为"会话结算"，
// 其用量将永远无人提交（漏账）。
//
// 命名说明：函数名以 TestCursorAccountBilledBySession 为前缀（不含 build tag，默认构建即可
// 运行）；unit tag 下既有 cursor_gemini_settlement_test.go 的同名用例保持独立，两者不冲突。

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestCursorAccountBilledBySession_AccountTypeDrivesBilling 断言 cursorAccountBilledBySession
// 按账号形态分流：OAuth（会话式号池账号）→ true；APIKey（OpenAI 兼容反代）→ false；
// 非 Cursor 平台 → false。
func TestCursorAccountBilledBySession_AccountTypeDrivesBilling(t *testing.T) {
	// 1. platform=cursor && type=oauth：ShareAI 号池会话账号，账只由会话终态结算驱动。
	oauthAccount := &service.Account{Platform: service.PlatformCursor, Type: service.AccountTypeOAuth}
	require.True(t, cursorAccountBilledBySession(oauthAccount),
		"cursor + oauth 账号必须走会话终态结算，HTTP 返回时不得直接提交")

	// 2. platform=cursor && type=api_key：第三方 OpenAI 兼容反代，属标准 HTTP 请求，
	//    必须由 handler 在 HTTP 返回时直接提交用量（不得挂起等待不存在的会话终态）。
	apiKeyAccount := &service.Account{
		Platform:    service.PlatformCursor,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-kn-cy-secret"},
	}
	require.False(t, cursorAccountBilledBySession(apiKeyAccount),
		"cursor + api_key 账号没有会话终态，必须由 handler 直接提交用量（否则漏账）")

	// 3. 非 cursor 平台：保持原有直接提交路径。
	require.False(t, cursorAccountBilledBySession(&service.Account{Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth}),
		"非 cursor 平台账号保持原直接提交路径")
	require.False(t, cursorAccountBilledBySession(&service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}),
		"非 cursor 平台的 api_key 账号保持原直接提交路径")

	// 4. nil 安全：不得 panic。
	require.False(t, cursorAccountBilledBySession(nil), "nil 账号不得被判定为会话结算")
}
