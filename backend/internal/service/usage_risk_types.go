package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// 本文件定义异常调用分析功能的对外契约（派发单 U4c）：
// DTO 结构体 + 服务接口 + 哨兵错误。
//
// 设计约束（派发单禁区）：
//   - 仅类型与接口，零逻辑、零 IO、零 DB/redis/net；
//   - 不重定义 RuleHit（复用 U4a 的 usage_risk_rules.go）；
//   - 不改动 U4a / U2 文件；
//   - 字段名与 JSON tag 严格对齐方案 §8 前端契约 / 派发单字段表（snake_case）。
//
// 本契约作为 U4b（编排实现）与 U5（handler 消费）之间的解耦边界：
// U4b 实现 UsageRiskService 接口；U5 仅依赖本文件类型与接口消费数据。

// UsageRiskListFilter 是分析页列表查询的过滤条件（对齐方案 §8 列表筛选）。
type UsageRiskListFilter struct {
	Page      int    `json:"page"`       // 页码，从 1 开始
	PageSize  int    `json:"page_size"`  // 每页大小
	ReportDate string `json:"report_date"` // "YYYY-MM-DD"，空=全部日期
	Level     string `json:"level"`      // 级别过滤（low/medium/high/critical），空=全部
	UserID    int64  `json:"user_id"`    // 0=全部用户
	GroupID   int64  `json:"group_id"`   // 0=全部分组
	Rule      string `json:"rule"`       // 命中规则过滤（如 "R1"），空=全部
	IncludeLow bool  `json:"include_low"` // 是否包含低于入榜线的低分留档行
}

// UsageRiskReportItem 是报告列表单行的对外表示（对齐方案 §6.2 / §8 列表）。
// RuleHits 复用 U4a 的 RuleHit 类型，不在此重定义。
type UsageRiskReportItem struct {
	ReportID      int64     `json:"report_id"`
	UserID        int64     `json:"user_id"`
	Username      string    `json:"username"`
	GroupID       int64     `json:"group_id"`
	GroupName     string    `json:"group_name"`
	ReportDate    string    `json:"report_date"`
	Score         int       `json:"score"`
	Level         string    `json:"level"`
	RuleHits      []RuleHit `json:"rule_hits"`
	Status        string    `json:"status"`
	InvalidatedAt *time.Time `json:"invalidated_at"` // 仅系统写入；NULL=有效
}

// UsageRiskReportDetail 是报告详情（列表项 + 证据 + 策略版本）。
// 匿名嵌入 UsageRiskReportItem，其 JSON 字段被提升为详情对象的顶层字段。
type UsageRiskReportDetail struct {
	UsageRiskReportItem
	Evidence      json.RawMessage `json:"evidence"`       // 阶段二明细（24h 热力/IP/UA/key/R2 上下文等）
	PolicyVersion string           `json:"policy_version"` // 产出本报告的策略版本（hex 小写，与版本哈希串同格式；F1 位保真）
}

// UsageRiskReportList 是报告列表的分页响应。
type UsageRiskReportList struct {
	Items    []UsageRiskReportItem `json:"items"`
	Total    int                   `json:"total"`
	MinScore int                   `json:"min_score"` // 当前生效的入榜阈值（usage_risk_listing_min_score）
}

// UsageRiskRunStatus 是最近一次分析 run 的新鲜度信息（对齐方案 §8 页头新鲜度条 / §6.3）。
type UsageRiskRunStatus struct {
	Status              string     `json:"status"`                // running / completed / partial / error
	WindowEnd           *time.Time `json:"window_end"`            // 本次重算数据截止时间
	ConsecutivePartials int        `json:"consecutive_partials"`  // 连续 partial 运行次数
	FailedBatches       int        `json:"failed_batches"`        // 失败批次数
	HistoryCovered      bool       `json:"history_covered"`       // 保留窗口历史桶回填/重评是否完成
	ReconProgress       string     `json:"recon_progress"`        // 对账/重评进度（游标位置/窗口终点描述，如 "2026-09-20/2026-09-01"）
	TZName              string     `json:"tz_name,omitempty"`     // 最近一轮 run 的策略快照时区名（B/J：时区变更检测依赖）
	R1ReevalPending     bool       `json:"r1_reeval_pending,omitempty"` // 覆盖翻转后待重评 R1 标记（E）
	Error               string     `json:"error,omitempty"`       // 失败关闭态错误（L：优先于 LatestRun 暴露）
}

// UsageRiskTopUser 是风险摘要中的 Top N 用户项（对齐方案 §8 仪表盘卡片）。
type UsageRiskTopUser struct {
	UserID   int64    `json:"user_id"`
	Username string   `json:"username"`
	MaxScore int      `json:"max_score"` // 该用户跨有效分组报告行的 MAX(score)
	Level    string   `json:"level"`
	TopRules []string `json:"top_rules"` // 命中规则名（去重）
}

// UsageRiskSummary 是管理仪表盘风险摘要卡片（对齐方案 §8 仪表盘卡片 / §6.2 用户级聚合口径）。
type UsageRiskSummary struct {
	OpenTotal int                  `json:"open_total"` // 有效且 status='open' 且 score≥入榜线的报告数
	ByLevel   map[string]int       `json:"by_level"`   // 各级别计数
	Top       []UsageRiskTopUser   `json:"top"`        // Top N 用户
	Error     string               `json:"error"`      // 分析 job 失败关闭时呈现错误态，空=正常
}

// 哨兵错误（U4b 返回 / U5 判定）：
//   - ErrUsageRiskReportNotFound：report_id 不存在或被对账失效剔除；
//   - ErrUsageRiskIllegalTransition：状态机非法转移（§6.2）。
var (
	ErrUsageRiskReportNotFound   = errors.New("usage risk report not found")
	ErrUsageRiskIllegalTransition = errors.New("usage risk illegal status transition")
)

// UsageRiskService 是异常调用分析对外服务接口（U4b 实现、U5 消费）。
// 方法集即为本契约对外暴露的能力边界。
type UsageRiskService interface {
	ListReports(ctx context.Context, f UsageRiskListFilter) (*UsageRiskReportList, error)
	GetReport(ctx context.Context, reportID int64) (*UsageRiskReportDetail, error)
	UpdateStatus(ctx context.Context, reportID int64, newStatus string, adminID int64) error
	GetRunStatus(ctx context.Context) (*UsageRiskRunStatus, error)
	GetRiskSummary(ctx context.Context) (*UsageRiskSummary, error)
}
