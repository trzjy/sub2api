package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// 平台归并重构 PR-2（docs/platform-merge-refactor-plan.md §5.8）：
// web-* 平台迁移 reconciler 的管理端触发入口。三字段原子性由 repository
// 层专用事务方法保证；本 handler 只做参数解析与报告回显。

type runPlatformMigrationRequest struct {
	Direction string `json:"direction" binding:"required,oneof=up down"`
	DryRun    bool   `json:"dry_run"`
}

// RunPlatformMigration 执行 web-* 平台迁移 reconciler（up：迁移到官方平台并
// 显式化 access_mode=web；down：按 migrated_from_platform 标记回滚）。
// dry_run=true 只核对分类，不写库。
func (h *AccountHandler) RunPlatformMigration(c *gin.Context) {
	var req runPlatformMigrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	report, err := h.adminService.MigrateWebPlatformAccounts(c.Request.Context(), req.Direction, req.DryRun)
	if err != nil {
		response.InternalError(c, "platform migration failed: "+err.Error())
		return
	}
	response.Success(c, report)
}
