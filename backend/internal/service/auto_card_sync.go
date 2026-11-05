package service

import "sync"

// autoCardSyncLocks 按激活码 ID 串行化「DB 状态写入 → 运行池纳管/注销」临界区。
//
// 为什么需要进程内串行边界：运行池（CursorDynamicPool）本身就是进程内状态，跨进程
// 协调无意义；而同一卡密的「写 quota_keys 快照/失败态」与「据该结果改动池成员」是
// 两次独立操作，并发单卡刷新会出现交错——后提交的 DB 状态被先到的池变更覆盖，运行期
// 池成员与重启回放（只依据 DB）由此分叉。把两者放进同一把按键分片的锁内，使「最后一个
// 进入临界区的请求」同时决定 DB 与池的最终状态。
//
// 键按 quota key ID 隔离：同 key 串行、异 key 并行；引用计数归零即回收条目，避免长期
// 运行后 map 无限增长。
type autoCardSyncLocks struct {
	mu      sync.Mutex
	entries map[int64]*autoCardSyncEntry
}

type autoCardSyncEntry struct {
	mu   sync.Mutex
	refs int
}

func newAutoCardSyncLocks() *autoCardSyncLocks {
	return &autoCardSyncLocks{entries: make(map[int64]*autoCardSyncEntry)}
}

func (l *autoCardSyncLocks) lock(id int64) func() {
	l.mu.Lock()
	e := l.entries[id]
	if e == nil {
		e = &autoCardSyncEntry{}
		l.entries[id] = e
	}
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.entries, id)
		}
		l.mu.Unlock()
	}
}

var globalAutoCardSyncLocks = newAutoCardSyncLocks()

// WithAutoCardSync 在指定激活码的串行边界内执行 fn：同 key 互斥、异 key 并行。
//
// 所有「会依据 DB 结果改动该卡运行池成员」的路径都必须经由本函数进入，否则串行边界
// 被绕过、并发交错重新出现。当前覆盖：批量导入入池、单卡刷新、全量刷新回写、卡密删除
// 与到期清扫的池注销。
func WithAutoCardSync(id int64, fn func() error) error {
	unlock := globalAutoCardSyncLocks.lock(id)
	defer unlock()
	return fn()
}
