package service

import "time"

// cursorSessionSnapshot 是会话释放前保存的最小可恢复状态（方案 §2.2）。
//
// 只包含导出字段，便于 JSON 序列化到 Redis；不含任何正文、参数或凭据。
type cursorSessionSnapshot struct {
	SessionID string
	// ProtocolValue 是 cursorProtocol 枚举原值（int）：恢复时直接转回枚举，
	// 不经字符串映射（避免第二套协议映射）；ProtocolName 仅供日志。
	ProtocolValue  int
	ProtocolName   string
	IndexProtocol  string // 统一索引的协议命名空间
	RequestedModel string
	UpstreamModel  string
	AccountID      int64
	QuotaKeyID     int64
	Identity       CursorSessionIdentity
	// 来源指纹（首次生成 x-jg-auth 时记录）；恢复记录必须带回，否则重建分支会以
	// source_fingerprint_unavailable 失败（cursorSessionSourceFingerprint）。
	SourceQuotaKeyID int64
	SourceCodeMAC    string

	ConfigHex            string
	ConfigItems          int
	ConfigSystemHex      string
	ConfigSystemItems    int
	ConfigSamplingHex    string
	ConfigSamplingItems  int
	HistoryHex           string
	HistoryItems         int
	HistorySkeletonHex   string
	HistorySkeletonItems int

	Tools []string

	PendingBatchID    string
	PendingResponseID string
	PendingCallIDs    []string

	ItemIDs []string // 已登记 Responses item ID

	CreatedAt time.Time
}

// cursorSnapshotFromRecord 从一条仍持有有效待续批的会话记录构造恢复快照。
//
// 返回 (nil,false) 的条件（均按派发单 B2-S2）：
//   - record 或 record.pending 缺失；
//   - 四个摘要任一缺失（不写残缺快照）；
//   - 待续批不是「已交付且未消费」；
//   - 无法从 record 取得完整的已登记 item ID。
//
// 注：方案文本里的 plan 摘要在本实现中直接挂在 record 上
// （configDigest / configSystemDigest / configSamplingDigest / historyDigest），
// 是 record 上唯一的摘要来源。
func cursorSnapshotFromRecord(record *cursorSessionRecord) (*cursorSessionSnapshot, bool) {
	if record == nil || record.pending == nil {
		return nil, false
	}
	if record.configDigest == nil || record.configSystemDigest == nil ||
		record.configSamplingDigest == nil || record.historyDigest == nil {
		return nil, false
	}
	pending := record.pending
	if !pending.delivered || pending.consumed {
		return nil, false
	}

	// ItemIDs 是可选增强：record 上当前没有该字段来源，取不到时留空即可——
	// 恢复依赖 pending 的 response/call ID 重建索引，不依赖 item 索引。
	itemIDs, _ := cursorSnapshotItemIDsFromRecord(record)

	return &cursorSessionSnapshot{
		SessionID:        record.id,
		ProtocolValue:    int(record.spec.protocol),
		ProtocolName:     cursorProtocolIndexName(record.spec.protocol),
		IndexProtocol:    cursorSessionIndexProtocol(record.spec),
		RequestedModel:   record.spec.requestedModel,
		UpstreamModel:    record.spec.upstreamModel,
		AccountID:        record.spec.accountID,
		QuotaKeyID:       record.spec.quotaKeyID,
		Identity:         record.identity,
		SourceQuotaKeyID: record.spec.sourceQuotaKeyID,
		SourceCodeMAC:    record.spec.sourceCodeMAC,

		ConfigHex:            record.configDigest.HexString(),
		ConfigItems:          record.configDigest.ItemCount(),
		ConfigSystemHex:      record.configSystemDigest.HexString(),
		ConfigSystemItems:    record.configSystemDigest.ItemCount(),
		ConfigSamplingHex:    record.configSamplingDigest.HexString(),
		ConfigSamplingItems:  record.configSamplingDigest.ItemCount(),
		HistoryHex:           record.historyDigest.HexString(),
		HistoryItems:         record.historyDigest.ItemCount(),
		HistorySkeletonHex:   record.historyDigest.SkeletonHexString(),
		HistorySkeletonItems: record.historyDigest.SkeletonItemCount(),

		Tools: append([]string(nil), record.exposedTools...),

		PendingBatchID:    pending.batchID,
		PendingResponseID: pending.responseID,
		PendingCallIDs:    pending.cloneCallIDs(),

		ItemIDs:   itemIDs,
		CreatedAt: record.createdAt,
	}, true
}

// cursorSnapshotItemIDsFromRecord 取本会话已登记的 Responses item ID。
//
// 当前实现中该 ID 只由 CursorExecutionService 持有的统一索引（CursorIDKindItem）
// 保存，record 上没有对应的字段（toolCallIDs / deliveredToolCalls 是 call ID 语义，
// 与 registerItemIDBatch 登记的 item ID 不是同一类键）。本函数只接收 record，且派发单
// 禁止新增索引读取接口，故按「若确实取不到，返回 (nil,false) 并在报告说明」处理。
func cursorSnapshotItemIDsFromRecord(record *cursorSessionRecord) ([]string, bool) {
	return nil, false
}
