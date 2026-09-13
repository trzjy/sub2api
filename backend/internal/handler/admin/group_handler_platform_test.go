//go:build unit

package admin

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 回归分组平台枚举:kimi/zhipu/deepseek 必须能通过 Create/Update 的 binding 校验
// （历史 bug:调度/路由链路已支持 CN 平台分组,但 oneof 白名单漏加三平台,导致
// 平台分组无法创建、CN 账号"无可用分组"）;非法值仍须被拒。
//
// 2026-09-13 追加 codebuddy:活体验收 F1 实测「前端 GROUP_PLATFORM_OPTIONS 已含
// codebuddy、DB CHECK(迁移 244)已放开，但后端 oneof 漏加」→ 管理员无法通过 API
// 创建 codebuddy 分组（HTTP 400），网关无法路由到 codebuddy 账号。此用例钉住修复。
func bindGroupPlatformJSON(t *testing.T, target any, body string) error {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c.ShouldBindJSON(target)
}

func TestGroupPlatformBinding_AllowedPlatforms(t *testing.T) {
	allowed := []string{
		"anthropic", "openai", "gemini", "antigravity", "grok",
		"kimi", "zhipu", "deepseek", "minimax", "codebuddy", "composite",
	}
	for _, platform := range allowed {
		t.Run("create_"+platform, func(t *testing.T) {
			var req CreateGroupRequest
			body := fmt.Sprintf(`{"name":"g","platform":%q}`, platform)
			require.NoError(t, bindGroupPlatformJSON(t, &req, body),
				"platform %q 应通过 CreateGroupRequest 校验", platform)
			require.Equal(t, platform, req.Platform)
		})
		t.Run("update_"+platform, func(t *testing.T) {
			var req UpdateGroupRequest
			body := fmt.Sprintf(`{"platform":%q}`, platform)
			require.NoError(t, bindGroupPlatformJSON(t, &req, body),
				"platform %q 应通过 UpdateGroupRequest 校验", platform)
			require.Equal(t, platform, req.Platform)
		})
	}
}

func TestGroupPlatformBinding_RejectsInvalidPlatforms(t *testing.T) {
	invalid := []string{
		"moonshot", // 厂商别名,不是平台标识
		"Kimi",     // 大小写敏感
		"openai ",  // 尾随空格
		"glm",
		"bogus",
	}
	for _, platform := range invalid {
		t.Run("create_"+platform, func(t *testing.T) {
			var req CreateGroupRequest
			body := fmt.Sprintf(`{"name":"g","platform":%q}`, platform)
			require.Error(t, bindGroupPlatformJSON(t, &req, body),
				"platform %q 应被 CreateGroupRequest 拒绝", platform)
		})
		t.Run("update_"+platform, func(t *testing.T) {
			var req UpdateGroupRequest
			body := fmt.Sprintf(`{"platform":%q}`, platform)
			require.Error(t, bindGroupPlatformJSON(t, &req, body),
				"platform %q 应被 UpdateGroupRequest 拒绝", platform)
		})
	}
}

// composite 目标平台白名单同样必须含 codebuddy：前端 COMPOSITE_TARGET_PLATFORM_OPTIONS
// 由 CONCRETE_PLATFORM_OPTIONS 派生（含 codebuddy），迁移 244 亦已放开
// composite_model_routes.target_platform 的 DB CHECK，后端 oneof 漏加会造成同类契约缺口。
func TestCompositeRouteTargetPlatform_AllowsCodeBuddyAndCNProviders(t *testing.T) {
	for _, platform := range []string{"kimi", "zhipu", "deepseek", "minimax", "codebuddy"} {
		var req CompositeRouteRequest
		body := fmt.Sprintf(`{"public_model":"m","target_platform":%q}`, platform)
		require.NoError(t, bindGroupPlatformJSON(t, &req, body))
		require.Equal(t, platform, req.TargetPlatform)
	}
}
