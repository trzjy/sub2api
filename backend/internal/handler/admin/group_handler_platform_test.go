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

// 回归分组平台枚举:具体平台必须能通过 Create/Update 的 binding 校验（历史 bug:
// 调度/路由链路已支持这些平台,但校验白名单漏加,导致平台分组无法创建、账号
// "无可用分组"）;非法值仍须被拒。
//
// 2026-09-13 追加 codebuddy:活体验收 F1 实测「前端 GROUP_PLATFORM_OPTIONS 已含
// codebuddy、DB CHECK(迁移 244)已放开，但后端校验漏加」→ 管理员无法通过 API
// 创建 codebuddy 分组（HTTP 400），网关无法路由到 codebuddy 账号。此用例钉住修复。
//
// 2026-10-02 muse-9:生产实测 muse 账号已建、目录同步正常,但建组被拒——校验白名单
// 漏 muse。此派发单把 muse 加入全部平台校验白名单,下列锚定钉住 Create/Update/
// CompositeTarget 的 muse 通过校验。
//
// 合并说明（v0.2.15）:平台校验改由 domain/platforms.go 清单驱动——Create/Update 用
// group_platform（= 具体平台 ∪ composite），组合路由目标用 concrete_platform（= 具体
// 平台），不再在 binding 标签手写 oneof。上游新增平台 opencode_go/typesafe/command_code/
// cline 与我方自研 codebuddy/muse 一并登记于该清单，故以下允许集合 = 全部具体平台 + composite。
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
		"kimi", "zhipu", "deepseek", "minimax",
		"opencode_go", "typesafe", "command_code", "cline",
		"codebuddy", "muse",
		"composite",
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
// composite_model_routes.target_platform 的 DB CHECK，后端校验漏加会造成同类契约缺口。
//
// 合并说明（v0.2.15）:目标平台校验改由 concrete_platform 驱动,接受全部具体平台
// （我方 codebuddy/muse + 上游 opencode_go/typesafe/command_code/cline）,composite 被拒。
func TestCompositeRouteTargetPlatform_AllowsConcreteProviders(t *testing.T) {
	for _, platform := range []string{
		"kimi", "zhipu", "deepseek", "minimax",
		"opencode_go", "typesafe", "command_code", "cline",
		"codebuddy", "muse",
	} {
		var req CompositeRouteRequest
		body := fmt.Sprintf(`{"public_model":"m","target_platform":%q}`, platform)
		require.NoError(t, bindGroupPlatformJSON(t, &req, body))
		require.Equal(t, platform, req.TargetPlatform)
	}
}

func TestCompositeRouteTargetPlatform_RejectsComposite(t *testing.T) {
	var req CompositeRouteRequest
	require.Error(t, bindGroupPlatformJSON(t, &req, `{"public_model":"m","target_platform":"composite"}`))
}
