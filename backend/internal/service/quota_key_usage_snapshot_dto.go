package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
)

// errQuotaKeyUsageRepositoryUnavailable 是读侧仓储未装配时的明确错误（失败关闭，不返回空投影冒充"无额度"）。
var errQuotaKeyUsageRepositoryUnavailable = errors.New("quota_key_service: quota key repository unavailable")

// 本文件是「两层额度」中**来源（激活码）额度快照的唯一读侧投影**（方案 §25.6 消费闭包
// ⑤⑥⑦ 的后端展示部分 + §25.14-C「唯一状态表示」 + §25.7-B 展示口径）。
//
// 边界（派发单 W2-D1）：
//   - 只读：快照写入者（观测生命周期）由后续单落地；本文件不写库、不发任何网络请求；
//   - 快照形态由 §25.14-C 钉死，不得自行增删字段：
//     {"total":400,"used":86,"status_code":200,"checked_at":"RFC3339",
//     "attempted_at":"RFC3339","error_code":""}
//   - 语义：total/used/status_code/checked_at 只描述最近一次**成功**观测（成功时 error_code
//     为空）；attempted_at/error_code 描述最近一次观测（无论成败）；无成功值时 total/used
//     缺失（无键或 null）——**缺失不是 0**，绝不把 Go 零值补成"真实 0"。
//
// 为什么投影只出现在这一处：quota key 列表/详情/probe 与账号 lite/full DTO 必须展示
// **同一个**快照值（§25.8 lite/full 同值），因此解析与投影函数只有一份实现。

const (
	// QuotaKeyUsageErrorCodeParseFailed 是读侧"落库快照无法解析"的失败语义登记值。
	// 它只表达"这条记录读不出来"，不改写库内 error_code，也不把数值补成 0。
	QuotaKeyUsageErrorCodeParseFailed = "snapshot_parse_failed"

	// QuotaKeyUsageScope 标识额度归属：quota_key = 供应商按激活码合并的额度。
	// 它**不是**本站账单余额，也**不是**账号自身的供应商余额（§25.6 开头）。
	QuotaKeyUsageScope = "quota_key"

	// QuotaKeyUsageLabel 是展示标签（§25.6⑥：「所属激活码额度」）。
	QuotaKeyUsageLabel = "所属激活码额度"

	// VendorAccountQuotaUnavailableNote 是账号页的展示注明（§25.7-B）：
	// 上游没有可核验的账号级真实余额，本轮只显示本站真实已结算用量 + 本注明。
	VendorAccountQuotaUnavailableNote = "供应商账号额度未提供"
)

// quotaKeyUsageSnapshotJSON 是 usage_snapshot 列的钉死 JSON 形态（§25.14-C）。
// 字段名与语义均不得自行变更；指针字段用于区分"缺失/null"与"真实 0"。
type quotaKeyUsageSnapshotJSON struct {
	Total       *int    `json:"total"`
	Used        *int    `json:"used"`
	StatusCode  *int    `json:"status_code"`
	CheckedAt   *string `json:"checked_at"`
	AttemptedAt *string `json:"attempted_at"`
	ErrorCode   string  `json:"error_code"`
}

// QuotaKeyUsageSnapshot 是快照的类型化表示（读侧 DTO）。
//
// 数值字段保持指针：nil = 没有最近成功值（缺失），与"真实 0"严格区分。
type QuotaKeyUsageSnapshot struct {
	// Total 是最近一次成功观测的供应商总额；nil 表示无成功值。
	Total *int
	// Used 是最近一次成功观测的供应商已用；nil 表示无成功值。
	Used *int
	// StatusCode 是最近一次成功观测的供应商 key 状态码（可选）。
	StatusCode *int
	// CheckedAt 是最近一次成功观测的时点。
	CheckedAt *time.Time
	// AttemptedAt 是最近一次观测（无论成败）的时点。
	AttemptedAt *time.Time
	// ErrorCode 是最近一次观测的脱敏错误码；成功时为空。
	ErrorCode string
	// ParseFailed 表示"记录存在但无法按钉死形态解析"。此时不产生任何数值，
	// 只按失败语义登记 error_code（缺失容错，不返回错误、不返回 0 值）。
	ParseFailed bool
}

// HasSuccessValues 报告是否存在最近一次成功观测的数值。
func (s QuotaKeyUsageSnapshot) HasSuccessValues() bool {
	return s.Total != nil || s.Used != nil
}

// QuotaKeyUsageProjection 是快照的展示投影（列表/详情/probe 与账号行共用同一结构）。
//
// 展示纪律（§25.6 / §25.7-B）：
//   - Scope/Label 恒为"所属激活码额度"，提醒该数字是供应商按激活码合并的额度，
//     不是本站账单余额、也不是账号自身余额；
//   - 查询失败只做标记（query_failed + error_code + attempted_at），保留上次成功值
//     与其时间（checked_at），**不写 0、不抹掉最近成功数值**；
//   - 无成功值时 total/used/remaining 全部缺失（omitempty），前端不得按 0 渲染。
type QuotaKeyUsageProjection struct {
	Scope string `json:"scope"`
	Label string `json:"label"`

	Total      *int `json:"total,omitempty"`
	Used       *int `json:"used,omitempty"`
	Remaining  *int `json:"remaining,omitempty"` // 算术剩余 total-used，仅当两者都存在
	StatusCode *int `json:"status_code,omitempty"`

	CheckedAt   *time.Time `json:"checked_at,omitempty"`
	AttemptedAt *time.Time `json:"attempted_at,omitempty"`
	ErrorCode   string     `json:"error_code,omitempty"`

	// HasSnapshot 表示该激活码是否已有可用的快照记录（含只有失败观测的情形）。
	HasSnapshot bool `json:"has_snapshot"`
	// QueryFailed 表示最近一次观测失败（error_code 非空，或记录无法解析）。
	QueryFailed bool `json:"query_failed"`
}

// ParseQuotaKeyUsageSnapshot 把 usage_snapshot 列的原始字节解析成类型化快照。
//
// 缺失容错（派发单要求）：
//   - nil / 空 / "null" → 返回零值 + ok=false：这是"无快照"，不是错误、更不是 0；
//   - 坏 JSON → 返回带 ParseFailed=true 与失败 error_code 的值 + ok=false：
//     同样按"无快照"处理（不产生数值），但把"读不出来"登记成 error_code 语义；
//   - 非法数值（负数）或非法时间 → 该字段按缺失处理，绝不补成 0。
func ParseQuotaKeyUsageSnapshot(raw []byte) (QuotaKeyUsageSnapshot, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return QuotaKeyUsageSnapshot{}, false
	}
	var doc quotaKeyUsageSnapshotJSON
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return QuotaKeyUsageSnapshot{
			ParseFailed: true,
			ErrorCode:   QuotaKeyUsageErrorCodeParseFailed,
		}, false
	}
	snapshot := QuotaKeyUsageSnapshot{
		Total:       normalizeQuotaCounter(doc.Total),
		Used:        normalizeQuotaCounter(doc.Used),
		StatusCode:  doc.StatusCode,
		CheckedAt:   parseQuotaSnapshotTime(doc.CheckedAt),
		AttemptedAt: parseQuotaSnapshotTime(doc.AttemptedAt),
		ErrorCode:   strings.TrimSpace(doc.ErrorCode),
	}
	// 空对象（或全部字段缺失/为 null）等价于"尚未观测"，不产出伪造的成功值。
	if !snapshot.HasSuccessValues() && snapshot.CheckedAt == nil && snapshot.AttemptedAt == nil && snapshot.ErrorCode == "" {
		return QuotaKeyUsageSnapshot{}, false
	}
	return snapshot, true
}

// ProjectQuotaKeyUsageSnapshot 把类型化快照投影成展示对象。
// ok=false 且非解析失败时返回 nil（无快照 → 响应里不出现该字段，前端不得按 0 渲染）。
func ProjectQuotaKeyUsageSnapshot(snapshot QuotaKeyUsageSnapshot, ok bool) *QuotaKeyUsageProjection {
	if !ok && !snapshot.ParseFailed {
		return nil
	}
	projection := &QuotaKeyUsageProjection{
		Scope:       QuotaKeyUsageScope,
		Label:       QuotaKeyUsageLabel,
		HasSnapshot: ok,
		QueryFailed: snapshot.ErrorCode != "",
		ErrorCode:   snapshot.ErrorCode,
	}
	if !ok {
		// 解析失败：只登记失败语义，不回填任何数值（不写 0）。
		return projection
	}
	projection.Total = snapshot.Total
	projection.Used = snapshot.Used
	if snapshot.Total != nil && snapshot.Used != nil {
		// 算术剩余：仅做减法，不宣称可识别赠额消耗顺序（§25.6）。
		remaining := *snapshot.Total - *snapshot.Used
		projection.Remaining = &remaining
	}
	projection.StatusCode = snapshot.StatusCode
	projection.CheckedAt = snapshot.CheckedAt
	projection.AttemptedAt = snapshot.AttemptedAt
	return projection
}

// BuildQuotaKeyUsageProjection 是 []byte → 展示投影的一步封装（列表/详情/probe 与账号行共用）。
func BuildQuotaKeyUsageProjection(raw []byte) *QuotaKeyUsageProjection {
	snapshot, ok := ParseQuotaKeyUsageSnapshot(raw)
	return ProjectQuotaKeyUsageSnapshot(snapshot, ok)
}

// LoadQuotaKeyUsageProjections 批量读取激活码的额度快照投影（id → 投影）。
//
// 关键约束（§25.6 消费闭包⑥、派发单）：
//   - **不逐行查询、不发网络**：单 id 走一次 GetByID，多 id 走一次 List（既有仓储方法），
//     调用方拿到的 map 只按 id 索引，行数不影响查询次数；
//   - 只读已提交列：不调用任何探测/刷新入口，不复制整份 key 聚合；
//   - 无快照的 id 不出现在返回值里（调用方据此省略字段，不得落成 0）。
func (s *QuotaKeyService) LoadQuotaKeyUsageProjections(ctx context.Context, quotaKeyIDs []int64) (map[int64]*QuotaKeyUsageProjection, error) {
	if len(quotaKeyIDs) == 0 {
		return map[int64]*QuotaKeyUsageProjection{}, nil
	}
	if s == nil || s.repo == nil {
		return nil, errQuotaKeyUsageRepositoryUnavailable
	}
	out := make(map[int64]*QuotaKeyUsageProjection, len(quotaKeyIDs))
	if len(quotaKeyIDs) == 1 {
		qk, err := s.repo.GetByID(ctx, quotaKeyIDs[0])
		if err != nil {
			return nil, err
		}
		collectQuotaKeyUsageProjection(out, qk)
		return out, nil
	}
	wanted := make(map[int64]struct{}, len(quotaKeyIDs))
	for _, id := range quotaKeyIDs {
		wanted[id] = struct{}{}
	}
	rows, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, qk := range rows {
		if qk == nil {
			continue
		}
		if _, ok := wanted[qk.ID]; !ok {
			continue
		}
		collectQuotaKeyUsageProjection(out, qk)
	}
	return out, nil
}

// QuotaKeyUsageProjectionFor 读取单个激活码的额度快照投影（详情/probe 用；一次按主键查询）。
func (s *QuotaKeyService) QuotaKeyUsageProjectionFor(ctx context.Context, quotaKeyID int64) (*QuotaKeyUsageProjection, error) {
	if quotaKeyID <= 0 {
		return nil, nil
	}
	byID, err := s.LoadQuotaKeyUsageProjections(ctx, []int64{quotaKeyID})
	if err != nil {
		return nil, err
	}
	return byID[quotaKeyID], nil
}

// ListWithUsageProjections 用一次仓储查询同时产出脱敏状态与额度快照投影。
//
// 与 List 的区别仅在于：状态对象上额外带 Usage 投影（同一行记录解析而来），
// 因此列表端点不需要为投影再查一次库。其余字段/顺序与 List 完全一致。
func (s *QuotaKeyService) ListWithUsageProjections(ctx context.Context) ([]*QuotaKeyStatus, error) {
	if s == nil || s.repo == nil {
		return nil, errQuotaKeyUsageRepositoryUnavailable
	}
	rows, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*QuotaKeyStatus, 0, len(rows))
	for _, qk := range rows {
		status := quotaKeyToStatus(qk)
		status.Usage = BuildQuotaKeyUsageProjection([]byte(qk.UsageSnapshot))
		out = append(out, status)
	}
	return out, nil
}

// collectQuotaKeyUsageProjection 把单行快照投影写入 id 索引（无快照则不写键）。
func collectQuotaKeyUsageProjection(out map[int64]*QuotaKeyUsageProjection, qk *ent.QuotaKey) {
	if out == nil || qk == nil {
		return
	}
	projection := BuildQuotaKeyUsageProjection([]byte(qk.UsageSnapshot))
	if projection == nil {
		return
	}
	out[qk.ID] = projection
}

// normalizeQuotaCounter 把上游计数规整为"缺失 vs 真实 0"可控的形态：
// 负数是不合法观测，按缺失处理（nil），绝不落成 0。
func normalizeQuotaCounter(value *int) *int {
	if value == nil || *value < 0 {
		return nil
	}
	normalized := *value
	return &normalized
}

// parseQuotaSnapshotTime 解析快照时间戳（RFC3339 / RFC3339Nano）。
// 解析失败返回 nil：时间缺失只是"没有时间"，不是零时刻。
func parseQuotaSnapshotTime(value *string) *time.Time {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}
