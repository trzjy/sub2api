package service

import "time"

// 批 2：模型级限流恢复探测的共享类型（repo 与 service 共用）。

// ModelProbeTask 是候选查询返回的单条探测任务快照。
// MetaLastEventAt 为 model_rate_limits_meta[scope].last_event_at 原文（空串 = 无 meta）。
type ModelProbeTask struct {
	AccountID       int64
	Scope           string
	Entry           map[string]any
	MetaLastEventAt string
}

// ModelProbePatchOp 是条目级最小改动操作。
type ModelProbePatchOp string

const (
	// ModelProbePatchReschedule 只改 probe_due_at/probe_gen（失败重排）。
	ModelProbePatchReschedule ModelProbePatchOp = "reschedule"
	// ModelProbePatchObserve 只改 observed_at/attempted_at（探测观测写回）。
	ModelProbePatchObserve ModelProbePatchOp = "observe"
	// ModelProbePatchDeleteScope 删除该 scope 条目（AICredits / 管理员单 scope 清除）。
	ModelProbePatchDeleteScope ModelProbePatchOp = "delete-scope"
)

// ModelProbePatch 是 PatchModelRateLimitEntry 的载荷。
type ModelProbePatch struct {
	Op         ModelProbePatchOp
	Due        time.Time // reschedule 使用
	ObserveKey string    // observe 使用：observed_at / attempted_at
	Observe    string    // observe 使用：RFC3339Nano 值
}
