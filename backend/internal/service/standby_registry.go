package service

import (
	"sync"
	"time"
)

// 主力渠道延迟/故障自适应备用切换 · 状态注册表（第 2a 片）。
//
// 注册表是调度层（后续片）唯一的状态读取入口：执行层只问 IsOpen(model)，
// 不感知判定细节。状态只存活在进程内（生产单容器单副本，方案 §2.2），
// 重启后表为空 = 全部 closed，这是安全默认（宁可不用备用，也不误开）。
//
// 判定与写入只由 StandbyController 驱动；本文件不做任何采集或迁移逻辑。

// StandbyStatus 是一个模型键的对外快照（值语义，拷贝即隔离）。
type StandbyStatus struct {
	// Model 是信号键，与注册表的 key 一致。
	Model string
	// Platform 是由模型名推导出的调度期目标平台（deepseek / kimi / zhipu）。
	Platform string
	// Open 表示该模型的备用渠道当前是否被前置。
	Open bool
	// Verdict 是最近一次判定结果（Inconclusive 表示未判定）。
	Verdict StandbyVerdict
	// Since 是当前 verdict 连续出现的起始时刻；刚迁移时等于迁移时刻。
	Since time.Time
	// OpenedAt 是本次开启的时刻；closed 态为零值。
	OpenedAt time.Time
	// Reason 是审计用的越线/恢复原因。
	Reason string
	// BaselineP50Ms / CurrentP50Ms 是最近一次判定所用的基线/当前 TTFT p50（毫秒），
	// 信号不可用时为 0。
	BaselineP50Ms float64
	CurrentP50Ms  float64
	// EvaluatedAt 是最近一次判定的时刻。
	EvaluatedAt time.Time
}

// StandbyRegistry 是 model → StandbyStatus 的进程内注册表，读写并发安全。
type StandbyRegistry struct {
	mu       sync.RWMutex
	statuses map[string]StandbyStatus
}

// NewStandbyRegistry 构造空注册表；空表即"全部 closed"。
func NewStandbyRegistry() *StandbyRegistry {
	return &StandbyRegistry{statuses: make(map[string]StandbyStatus, 8)}
}

// IsOpen 报告该模型是否处于备用开启态。未登记的模型一律视为 closed。
func (r *StandbyRegistry) IsOpen(model string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.statuses[model].Open
}

// Status 返回单个模型的快照；未登记时 ok=false。
func (r *StandbyRegistry) Status(model string) (StandbyStatus, bool) {
	if r == nil {
		return StandbyStatus{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.statuses[model]
	return st, ok
}

// Snapshot 返回全量快照的副本：调用方拿到的 map 与结构体都与注册表隔离。
func (r *StandbyRegistry) Snapshot() map[string]StandbyStatus {
	if r == nil {
		return map[string]StandbyStatus{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]StandbyStatus, len(r.statuses))
	for model, st := range r.statuses {
		out[model] = st
	}
	return out
}

// set 写入单条状态；只允许控制器调用。
func (r *StandbyRegistry) set(model string, st StandbyStatus) {
	if r == nil || model == "" {
		return
	}
	st.Model = model
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses[model] = st
}
