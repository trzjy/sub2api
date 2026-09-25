package admin

import (
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// UsageRiskHandler 暴露异常调用分析的 admin HTTP 层（派发单 U5）。
//
// 设计约束（派发单禁区）：
//   - 不重算/不打分/不做规则引擎（单一权威路径在 U4b service）；
//   - 不新建任何处置类接口（禁用/封号/调限额一律没有，裁定 #1）；
//   - invalidated_at 无任何写入口（只读展示，U5 不提供写）；
//   - 仅通过 service.UsageRiskService 接口消费数据；settings 读写唯一入口为
//     SettingService.GetUsageRiskSettings / UpdateUsageRiskSettings（U2 收口）。
//
// 状态变更审计：事件类 `admin.usage_risk.status` 由 U4b 在 UpdateStatus 事务内落账
// （方案 §5：audit 与状态变更同事务）。本 handler 通过 middleware.SkipAudit 跳过
// HTTP 审计中间件的自动记录，避免与事务内权威审计重复；并通过 adminID 把操作者身份
// 传入 service。
type UsageRiskHandler struct {
	svc     service.UsageRiskService
	setting *service.SettingService
}

// NewUsageRiskHandler 构造异常调用分析 admin handler（DI 注入，对齐现有 handler 模式）。
func NewUsageRiskHandler(svc service.UsageRiskService, setting *service.SettingService) *UsageRiskHandler {
	return &UsageRiskHandler{svc: svc, setting: setting}
}

// 状态流转合法目标集合（裁定 #1：纯人工记账，四态状态机 open→acknowledged→resolved，
// open/acknowledged→dismissed；handler 仅接受这三个终态转移的目标值，非法值直接拒绝）。
var usageRiskAllowedStatusTargets = map[string]struct{}{
	"acknowledged": {},
	"dismissed":    {},
	"resolved":     {},
}

// usageRiskAllowedLevels 是规则引擎 scoreLevel 实据产出的等级枚举
// （service/usage_risk_rules.go：critical/high/medium/low），列表 level 筛选据此校验。
var usageRiskAllowedLevels = map[string]struct{}{
	"critical": {},
	"high":     {},
	"medium":   {},
	"low":      {},
}

// usageRiskRuleIDPattern 匹配规则引擎实际写入 rule_hits 的规则标识
// R1/R2/R3a/R3b/R4/R5/R6/R7/R8（service/usage_risk_rules.go）。
var usageRiskRuleIDPattern = regexp.MustCompile(`^R([1-8]|3a|3b)$`)

// ListReports 列出异常调用分析报告（分页 + 筛选）。
// GET /api/v1/admin/usage-risk/reports
// min_score（入榜阈值）由 service/repo 从 settings 统一取，本 handler 不硬编码。
func (h *UsageRiskHandler) ListReports(c *gin.Context) {
	if h.svc == nil {
		response.InternalError(c, "usage risk service unavailable")
		return
	}
	adminID := getAdminIDFromContext(c)
	if adminID == 0 {
		response.Unauthorized(c, "authenticated administrator required")
		return
	}

	page, pageSize := response.ParsePagination(c)
	// 静默忽略解析错误会把“筛选失败”伪装成“全量结果”（管理员误输 user_id=abc 退化为查询
	// 全量用户；非法 date 还会在 PostgreSQL ::date 强转处炸成 500）——已提供的参数一律
	// 严格校验，失败即 400 关闭，不进入查询条件。非正数 ID 一并拒绝（repo 仅 >0 加条件，
	// 0/负数会静默退化全量）。
	f := service.UsageRiskListFilter{
		Page:     page,
		PageSize: pageSize,
	}
	if v := c.Query("user_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		// 外审⑨ 发现4：非正数 ID（0/负数）同样 400——repo 仅在 >0 时加条件，放行会静默
		// 退化全量查询（"筛选失败伪装成全量结果"的反模式残留）。
		if err != nil || id <= 0 {
			response.BadRequest(c, "invalid user_id: "+v)
			return
		}
		f.UserID = id
	}
	if v := c.Query("group_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "invalid group_id: "+v)
			return
		}
		f.GroupID = id
	}
	if raw := c.Query("include_low"); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			response.BadRequest(c, "invalid include_low: "+raw)
			return
		}
		f.IncludeLow = b
	}
	if v := c.Query("date"); v != "" {
		if _, err := time.Parse("2006-01-02", v); err != nil {
			response.BadRequest(c, "invalid date: "+v)
			return
		}
		f.ReportDate = v
	}
	if v := c.Query("level"); v != "" {
		if _, ok := usageRiskAllowedLevels[v]; !ok {
			response.BadRequest(c, "invalid level: "+v)
			return
		}
		f.Level = v
	}
	if v := c.Query("rule"); v != "" {
		// 规则标识实据：规则引擎常量 rule_hits 取值为 R1/R2/R3a/R3b/R4..R8
		// （service/usage_risk_rules.go 的 ruleWeight 与 addHit 调用处），
		// 非可配置集合，故按实据放宽为 R1-R8 外加 R3a/R3b 两条子规则。
		if !usageRiskRuleIDPattern.MatchString(v) {
			response.BadRequest(c, "invalid rule: "+v)
			return
		}
		f.Rule = v
	}

	list, err := h.svc.ListReports(c.Request.Context(), f)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, list)
}

// GetReport 获取单份报告详情（含 evidence，按需读取不在列表聚合）。
// GET /api/v1/admin/usage-risk/reports/:report_id
func (h *UsageRiskHandler) GetReport(c *gin.Context) {
	if h.svc == nil {
		response.InternalError(c, "usage risk service unavailable")
		return
	}
	adminID := getAdminIDFromContext(c)
	if adminID == 0 {
		response.Unauthorized(c, "authenticated administrator required")
		return
	}
	reportID, ok := parseReportIDParam(c)
	if !ok {
		response.BadRequest(c, "invalid report_id")
		return
	}

	detail, err := h.svc.GetReport(c.Request.Context(), reportID)
	if err != nil {
		if errors.Is(err, service.ErrUsageRiskReportNotFound) {
			response.NotFound(c, "usage risk report not found")
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, detail)
}

// usageRiskStatusRequest 是状态流转请求体。
type usageRiskStatusRequest struct {
	Status string `json:"status"`
}

// UpdateStatus 变更报告状态（状态机校验在 service 内完成，handler 层只接收合法目标值
// 并把操作者身份传入；非法转移/不存在由 service 返回哨兵错误，明确拒绝，不静默成功）。
// POST /api/v1/admin/usage-risk/reports/:report_id/status
func (h *UsageRiskHandler) UpdateStatus(c *gin.Context) {
	if h.svc == nil {
		response.InternalError(c, "usage risk service unavailable")
		return
	}
	adminID := getAdminIDFromContext(c)
	if adminID == 0 {
		response.Unauthorized(c, "authenticated administrator required")
		return
	}
	reportID, ok := parseReportIDParam(c)
	if !ok {
		response.BadRequest(c, "invalid report_id")
		return
	}

	var req usageRiskStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	if _, allowed := usageRiskAllowedStatusTargets[req.Status]; !allowed {
		response.BadRequest(c, "status must be one of acknowledged, dismissed, resolved")
		return
	}

	// 状态变更审计由 U4b 在事务内落账（admin.usage_risk.status）；跳过 HTTP 审计中间件
	// 的自动记录，避免与事务内权威审计重复。
	middleware.SkipAudit(c)

	if err := h.svc.UpdateStatus(c.Request.Context(), reportID, req.Status, adminID); err != nil {
		if errors.Is(err, service.ErrUsageRiskReportNotFound) {
			response.NotFound(c, "usage risk report not found")
			return
		}
		if errors.Is(err, service.ErrUsageRiskIllegalTransition) {
			response.BadRequest(c, "illegal status transition")
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"status": req.Status, "report_id": reportID})
}

// GetRunStatus 返回最近一次分析 run 的新鲜度信息（状态/数据截止/连续 partial/失败批次/覆盖）。
// GET /api/v1/admin/usage-risk/run-status
func (h *UsageRiskHandler) GetRunStatus(c *gin.Context) {
	if h.svc == nil {
		response.InternalError(c, "usage risk service unavailable")
		return
	}
	adminID := getAdminIDFromContext(c)
	if adminID == 0 {
		response.Unauthorized(c, "authenticated administrator required")
		return
	}

	status, err := h.svc.GetRunStatus(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

// GetSettings 返回 usage_risk_* 全部键的当前生效字符串值（默认兜底 + 存储覆盖）。
// GET /api/v1/admin/usage-risk/settings
// 这是 usage_risk_* 键的唯一 admin 读取入口（U2 遗留项 2 收口）。
func (h *UsageRiskHandler) GetSettings(c *gin.Context) {
	if h.setting == nil {
		response.InternalError(c, "settings service unavailable")
		return
	}
	adminID := getAdminIDFromContext(c)
	if adminID == 0 {
		response.Unauthorized(c, "authenticated administrator required")
		return
	}

	values, err := h.setting.GetUsageRiskSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"settings": values})
}

// PutSettings 保存 usage_risk_* 设置（唯一 admin 写入入口，U2 收口）。
// 走单一域校验器 + 保留期绑定校验，违反由 service 返回明确错误（不钳制）。
// PUT /api/v1/admin/usage-risk/settings
func (h *UsageRiskHandler) PutSettings(c *gin.Context) {
	if h.setting == nil {
		response.InternalError(c, "settings service unavailable")
		return
	}
	adminID := getAdminIDFromContext(c)
	if adminID == 0 {
		response.Unauthorized(c, "authenticated administrator required")
		return
	}

	var values map[string]string
	if err := c.ShouldBindJSON(&values); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	if err := h.setting.UpdateUsageRiskSettings(c.Request.Context(), values); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"status": "ok"})
}

// parseReportIDParam 解析路径参数 report_id；非法或非正返回 (0, false)。
func parseReportIDParam(c *gin.Context) (int64, bool) {
	raw := c.Param("report_id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
