package service

// 本文件实现 Cursor 跨 HTTP 会话的统一关联索引（方案 §25.14-D「最小表示」）。
//
// 设计约束（硬性，逐条对应方案）：
//   - 键 = (客户端协议, ID 种类 call|response|item, ID 字符串)；
//     值 = sessionID、batchID、已消费标记。call/response/item 全在同一索引，
//     不按协议或 ID 种类另建 registry，不建第二索引。
//   - 进程内 map + sync.Mutex；不持久化、不建 Redis、不设后台 worker。
//   - Register：登记新 ID；同一键重复 Register 返回冲突错误。
//   - Locate：定位并返回 session/batch/消费状态（调用方自行比对 sessionID，
//     以此识别跨 session 混批 ID）。
//   - MarkConsumed：标记已消费；重复消费返回冲突错误（用于拒绝重复或旧批）。
//   - DeleteSession：删除该 session 的全部关联，包含历史批次的 call/response/item，
//     不是只删当前 pending。
//   - 会话表、本索引与所有权切换的锁由上层服务统一持有（§25.14-D「整批接管顺序」），
//     本索引只保证自身字段的并发安全，不引入额外锁序。

import (
	"errors"
	"fmt"
	"sync"
)

// CursorIDKind 是统一索引的 ID 种类（call|response|item）。
type CursorIDKind string

const (
	// CursorIDKindCall 表示工具调用 ID（如 tool_call_id / function_call.id / functionCall.id）。
	CursorIDKindCall CursorIDKind = "call"
	// CursorIDKindResponse 表示响应 ID（如 response.id / previous_response_id 目标）。
	CursorIDKindResponse CursorIDKind = "response"
	// CursorIDKindItem 表示条目 ID（如 output item id / tool_use.id 对应的结果锚点）。
	CursorIDKindItem CursorIDKind = "item"
)

// 索引层错误。
var (
	// ErrCursorSessionIndexInvalidKey 表示键不完整或 ID 种类未知。
	ErrCursorSessionIndexInvalidKey = errors.New("cursor session index: invalid key")
	// ErrCursorSessionIndexConflict 表示同一键已被登记（含登记到另一 session/batch）。
	ErrCursorSessionIndexConflict = errors.New("cursor session index: id already registered")
	// ErrCursorSessionIndexNotFound 表示键未登记。
	ErrCursorSessionIndexNotFound = errors.New("cursor session index: id not found")
	// ErrCursorSessionIndexAlreadyConsumed 表示该 ID 已被消费（重复/旧批消费冲突）。
	ErrCursorSessionIndexAlreadyConsumed = errors.New("cursor session index: id already consumed")
)

// CursorSessionIndexKey 是统一索引的键：(客户端协议, ID 种类, ID)。
type CursorSessionIndexKey struct {
	// Protocol 是客户端协议标识，由调用方按现有入口传入（如 chat / responses / messages / gemini）。
	// 索引不硬编码协议清单，只做非空校验。
	Protocol string
	// Kind 是 ID 种类。
	Kind CursorIDKind
	// ID 是协议侧的原始 ID 字符串。
	ID string
}

// String 返回可安全记录的键描述（不含上下文正文）。
func (k CursorSessionIndexKey) String() string {
	return fmt.Sprintf("(%s,%s,%s)", k.Protocol, k.Kind, k.ID)
}

// Validate 校验键完整性。
func (k CursorSessionIndexKey) Validate() error {
	if k.Protocol == "" {
		return fmt.Errorf("%w: empty protocol", ErrCursorSessionIndexInvalidKey)
	}
	switch k.Kind {
	case CursorIDKindCall, CursorIDKindResponse, CursorIDKindItem:
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrCursorSessionIndexInvalidKey, string(k.Kind))
	}
	if k.ID == "" {
		return fmt.Errorf("%w: empty id", ErrCursorSessionIndexInvalidKey)
	}
	return nil
}

// CursorSessionIndexEntry 是统一索引的值：归属 session、归属批次与消费状态。
//
// 身份（user / API key / 组 / 模型等）不复制到索引，统一从 session 唯一记录读取
// （§25.14-D「最小表示」）。
type CursorSessionIndexEntry struct {
	// SessionID 是本次原生会话 ID。
	SessionID string
	// BatchID 是登记该 ID 的交付批次 ID。
	BatchID string
	// Consumed 表示该 ID 是否已被消费（保留会话存续期间的已消费记录以拒绝重复/旧批）。
	Consumed bool
}

// OwnedBy 判断该关联是否属于给定 session（调用方用于识别跨 session 混批 ID）。
func (e CursorSessionIndexEntry) OwnedBy(sessionID string) bool {
	return e.SessionID != "" && e.SessionID == sessionID
}

// SameBatch 判断该关联是否属于给定批次。
func (e CursorSessionIndexEntry) SameBatch(batchID string) bool {
	return e.BatchID != "" && e.BatchID == batchID
}

// CursorSessionIndex 是进程内统一关联索引。
//
// 零值不可用，请使用 NewCursorSessionIndex。
type CursorSessionIndex struct {
	mu      sync.Mutex
	entries map[CursorSessionIndexKey]CursorSessionIndexEntry
}

// NewCursorSessionIndex 返回空索引。
func NewCursorSessionIndex() *CursorSessionIndex {
	return &CursorSessionIndex{entries: make(map[CursorSessionIndexKey]CursorSessionIndexEntry)}
}

// Register 登记一个新 ID（键 = protocol/kind/id）。
//
// 同一键重复登记返回 ErrCursorSessionIndexConflict；调用方不得据此覆盖已有归属。
func (x *CursorSessionIndex) Register(key CursorSessionIndexKey, sessionID, batchID string) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if sessionID == "" {
		return fmt.Errorf("%w: empty session id", ErrCursorSessionIndexInvalidKey)
	}
	if batchID == "" {
		return fmt.Errorf("%w: empty batch id", ErrCursorSessionIndexInvalidKey)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.entries == nil {
		x.entries = make(map[CursorSessionIndexKey]CursorSessionIndexEntry)
	}
	if existing, ok := x.entries[key]; ok {
		return fmt.Errorf("%w: key=%s session=%s batch=%s (existing session=%s batch=%s)",
			ErrCursorSessionIndexConflict, key.String(), sessionID, batchID, existing.SessionID, existing.BatchID)
	}
	x.entries[key] = CursorSessionIndexEntry{SessionID: sessionID, BatchID: batchID}
	return nil
}

// Locate 定位一个 ID 的关联（session/batch/消费状态）。
//
// 未登记返回 ErrCursorSessionIndexNotFound 且附带键描述；调用方拿到 Entry 后必须比对
// SessionID/BatchID，以拒绝跨 session 或旧批的混用。
func (x *CursorSessionIndex) Locate(key CursorSessionIndexKey) (CursorSessionIndexEntry, error) {
	if err := key.Validate(); err != nil {
		return CursorSessionIndexEntry{}, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	entry, ok := x.entries[key]
	if !ok {
		return CursorSessionIndexEntry{}, fmt.Errorf("%w: key=%s", ErrCursorSessionIndexNotFound, key.String())
	}
	return entry, nil
}

// MarkConsumed 把该 ID 标记为已消费；重复消费返回 ErrCursorSessionIndexAlreadyConsumed。
func (x *CursorSessionIndex) MarkConsumed(key CursorSessionIndexKey) (CursorSessionIndexEntry, error) {
	if err := key.Validate(); err != nil {
		return CursorSessionIndexEntry{}, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	entry, ok := x.entries[key]
	if !ok {
		return CursorSessionIndexEntry{}, fmt.Errorf("%w: key=%s", ErrCursorSessionIndexNotFound, key.String())
	}
	if entry.Consumed {
		return entry, fmt.Errorf("%w: key=%s session=%s batch=%s",
			ErrCursorSessionIndexAlreadyConsumed, key.String(), entry.SessionID, entry.BatchID)
	}
	entry.Consumed = true
	x.entries[key] = entry
	return entry, nil
}

// DeleteSession 删除该 session 的全部关联（包含历史批次的 call/response/item），返回删除条目数。
//
// 这是整批 finalizer 的清理入口：不能只删当前 pending 批次。
func (x *CursorSessionIndex) DeleteSession(sessionID string) int {
	if sessionID == "" {
		return 0
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	removed := 0
	for key, entry := range x.entries {
		if entry.SessionID == sessionID {
			delete(x.entries, key)
			removed++
		}
	}
	return removed
}

// Len 返回当前索引条目数（用于自检/测试，不参与业务判定）。
func (x *CursorSessionIndex) Len() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.entries)
}

// EntriesForSession 返回该 session 当前的全部关联快照（用于自检/测试）。
//
// 返回的切片是拷贝，调用方可自由修改而不影响索引内部状态。
func (x *CursorSessionIndex) EntriesForSession(sessionID string) map[CursorSessionIndexKey]CursorSessionIndexEntry {
	out := make(map[CursorSessionIndexKey]CursorSessionIndexEntry)
	if sessionID == "" {
		return out
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for key, entry := range x.entries {
		if entry.SessionID == sessionID {
			out[key] = entry
		}
	}
	return out
}
