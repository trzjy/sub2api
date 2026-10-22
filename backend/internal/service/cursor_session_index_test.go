//go:build unit

package service

// 本文件是 §25.14-D「最小表示」中统一关联索引的定向验证。
//
// 覆盖（对应派发单 CR-B 的验收条款）：
//   - register/locate/markConsumed 基本语义；
//   - 索引重复消费冲突、同一 ID 重复登记冲突；
//   - 跨 session 混批 ID 可被识别（Entry.SessionID 供调用方比对）；
//   - DeleteSession 清全量（含历史批次的 response/item，不只当前 pending）；
//   - 键校验（空协议/空 ID/未知种类）与并发登记只有一个赢家。

import (
	"errors"
	"sync"
	"testing"
)

// cursorIndexKey 构造测试用键。
func cursorIndexKey(protocol string, kind CursorIDKind, id string) CursorSessionIndexKey {
	return CursorSessionIndexKey{Protocol: protocol, Kind: kind, ID: id}
}

// TestCursorSessionIndexRegisterLocate 覆盖登记与定位。
func TestCursorSessionIndexRegisterLocate(t *testing.T) {
	t.Run("登记后可定位且初始未消费", func(t *testing.T) {
		idx := NewCursorSessionIndex()
		key := cursorIndexKey("chat", CursorIDKindCall, "call_1")
		if err := idx.Register(key, "sess-1", "batch-1"); err != nil {
			t.Fatalf("Register 失败: %v", err)
		}
		entry, err := idx.Locate(key)
		if err != nil {
			t.Fatalf("Locate 失败: %v", err)
		}
		if entry.SessionID != "sess-1" || entry.BatchID != "batch-1" || entry.Consumed {
			t.Fatalf("定位结果不符: %+v", entry)
		}
		if !entry.OwnedBy("sess-1") || entry.OwnedBy("sess-2") {
			t.Fatalf("OwnedBy 判定不符: %+v", entry)
		}
		if !entry.SameBatch("batch-1") || entry.SameBatch("batch-2") {
			t.Fatalf("SameBatch 判定不符: %+v", entry)
		}
	})

	t.Run("三种ID种类与协议互不覆盖", func(t *testing.T) {
		idx := NewCursorSessionIndex()
		keys := []CursorSessionIndexKey{
			cursorIndexKey("chat", CursorIDKindCall, "same-id"),
			cursorIndexKey("chat", CursorIDKindResponse, "same-id"),
			cursorIndexKey("chat", CursorIDKindItem, "same-id"),
			cursorIndexKey("responses", CursorIDKindCall, "same-id"),
		}
		for i, key := range keys {
			if err := idx.Register(key, "sess-1", "batch-1"); err != nil {
				t.Fatalf("Register key[%d]=%s 失败: %v", i, key.String(), err)
			}
		}
		if idx.Len() != len(keys) {
			t.Fatalf("索引条目数应为 %d，实际 %d", len(keys), idx.Len())
		}
		for i, key := range keys {
			if _, err := idx.Locate(key); err != nil {
				t.Fatalf("Locate key[%d]=%s 失败: %v", i, key.String(), err)
			}
		}
		if _, err := idx.Locate(cursorIndexKey("messages", CursorIDKindCall, "same-id")); !errors.Is(err, ErrCursorSessionIndexNotFound) {
			t.Fatalf("未登记的协议键应 NotFound，实际 %v", err)
		}
	})

	t.Run("未登记ID定位返回未找到", func(t *testing.T) {
		idx := NewCursorSessionIndex()
		if _, err := idx.Locate(cursorIndexKey("chat", CursorIDKindCall, "ghost")); !errors.Is(err, ErrCursorSessionIndexNotFound) {
			t.Fatalf("期望 ErrCursorSessionIndexNotFound，实际 %v", err)
		}
	})

	t.Run("键与归属校验", func(t *testing.T) {
		idx := NewCursorSessionIndex()
		badKeys := []CursorSessionIndexKey{
			cursorIndexKey("", CursorIDKindCall, "id"),
			cursorIndexKey("chat", CursorIDKind("bogus"), "id"),
			cursorIndexKey("chat", CursorIDKind(""), "id"),
			cursorIndexKey("chat", CursorIDKindCall, ""),
		}
		for i, key := range badKeys {
			if err := idx.Register(key, "sess-1", "batch-1"); !errors.Is(err, ErrCursorSessionIndexInvalidKey) {
				t.Fatalf("坏键 %d 期望 ErrCursorSessionIndexInvalidKey，实际 %v", i, err)
			}
			if _, err := idx.Locate(key); !errors.Is(err, ErrCursorSessionIndexInvalidKey) {
				t.Fatalf("坏键 %d Locate 期望 ErrCursorSessionIndexInvalidKey，实际 %v", i, err)
			}
			if _, err := idx.MarkConsumed(key); !errors.Is(err, ErrCursorSessionIndexInvalidKey) {
				t.Fatalf("坏键 %d MarkConsumed 期望 ErrCursorSessionIndexInvalidKey，实际 %v", i, err)
			}
		}
		good := cursorIndexKey("chat", CursorIDKindCall, "id")
		if err := idx.Register(good, "", "batch-1"); !errors.Is(err, ErrCursorSessionIndexInvalidKey) {
			t.Fatalf("空 session 应被拒绝，实际 %v", err)
		}
		if err := idx.Register(good, "sess-1", ""); !errors.Is(err, ErrCursorSessionIndexInvalidKey) {
			t.Fatalf("空 batch 应被拒绝，实际 %v", err)
		}
		if idx.Len() != 0 {
			t.Fatalf("非法登记不得写入索引，实际条目 %d", idx.Len())
		}
	})
}

// TestCursorSessionIndexDuplicateRegisterConflict 覆盖同一 ID 重复登记。
func TestCursorSessionIndexDuplicateRegisterConflict(t *testing.T) {
	idx := NewCursorSessionIndex()
	key := cursorIndexKey("responses", CursorIDKindItem, "msg_1")
	if err := idx.Register(key, "sess-1", "batch-1"); err != nil {
		t.Fatalf("首次 Register 失败: %v", err)
	}

	t.Run("同 session 同 batch 重复登记冲突", func(t *testing.T) {
		err := idx.Register(key, "sess-1", "batch-1")
		if !errors.Is(err, ErrCursorSessionIndexConflict) {
			t.Fatalf("期望 ErrCursorSessionIndexConflict，实际 %v", err)
		}
	})

	t.Run("跨 session 重复登记冲突且不覆盖归属", func(t *testing.T) {
		err := idx.Register(key, "sess-2", "batch-9")
		if !errors.Is(err, ErrCursorSessionIndexConflict) {
			t.Fatalf("期望 ErrCursorSessionIndexConflict，实际 %v", err)
		}
		entry, locateErr := idx.Locate(key)
		if locateErr != nil {
			t.Fatalf("Locate 失败: %v", locateErr)
		}
		if entry.SessionID != "sess-1" || entry.BatchID != "batch-1" {
			t.Fatalf("冲突登记不得覆盖原归属: %+v", entry)
		}
		if idx.Len() != 1 {
			t.Fatalf("索引条目数应保持 1，实际 %d", idx.Len())
		}
	})

	t.Run("同 ID 不同协议可共存", func(t *testing.T) {
		if err := idx.Register(cursorIndexKey("chat", CursorIDKindItem, "msg_1"), "sess-2", "batch-2"); err != nil {
			t.Fatalf("不同协议同 ID 应可登记，实际 %v", err)
		}
	})
}

// TestCursorSessionIndexMarkConsumedConflict 覆盖重复消费冲突。
func TestCursorSessionIndexMarkConsumedConflict(t *testing.T) {
	idx := NewCursorSessionIndex()
	key := cursorIndexKey("chat", CursorIDKindCall, "call_x")
	if err := idx.Register(key, "sess-1", "batch-1"); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}

	t.Run("首次消费成功", func(t *testing.T) {
		entry, err := idx.MarkConsumed(key)
		if err != nil {
			t.Fatalf("首次 MarkConsumed 失败: %v", err)
		}
		if !entry.Consumed {
			t.Fatalf("消费后标记应为 true: %+v", entry)
		}
	})

	t.Run("重复消费冲突", func(t *testing.T) {
		entry, err := idx.MarkConsumed(key)
		if !errors.Is(err, ErrCursorSessionIndexAlreadyConsumed) {
			t.Fatalf("期望 ErrCursorSessionIndexAlreadyConsumed，实际 %v", err)
		}
		if !entry.Consumed || entry.SessionID != "sess-1" {
			t.Fatalf("冲突返回的条目应仍带原归属与已消费状态: %+v", entry)
		}
	})

	t.Run("消费标记可被定位读到", func(t *testing.T) {
		entry, err := idx.Locate(key)
		if err != nil {
			t.Fatalf("Locate 失败: %v", err)
		}
		if !entry.Consumed {
			t.Fatal("Locate 应返回已消费状态")
		}
	})

	t.Run("未登记ID消费返回未找到", func(t *testing.T) {
		if _, err := idx.MarkConsumed(cursorIndexKey("chat", CursorIDKindCall, "ghost")); !errors.Is(err, ErrCursorSessionIndexNotFound) {
			t.Fatalf("期望 ErrCursorSessionIndexNotFound，实际 %v", err)
		}
	})

	t.Run("同ID不同批次仍是同一关联不重复登记", func(t *testing.T) {
		if err := idx.Register(key, "sess-1", "batch-2"); !errors.Is(err, ErrCursorSessionIndexConflict) {
			t.Fatalf("旧批ID再登记应冲突，实际 %v", err)
		}
	})
}

// TestCursorSessionIndexCrossSessionIdentification 覆盖跨 session 混批 ID 的识别。
func TestCursorSessionIndexCrossSessionIdentification(t *testing.T) {
	idx := NewCursorSessionIndex()
	callA := cursorIndexKey("responses", CursorIDKindCall, "call_A")
	callB := cursorIndexKey("responses", CursorIDKindCall, "call_B")
	itemA := cursorIndexKey("responses", CursorIDKindItem, "item_A")
	if err := idx.Register(callA, "sess-A", "batch-A1"); err != nil {
		t.Fatalf("Register callA: %v", err)
	}
	if err := idx.Register(itemA, "sess-A", "batch-A1"); err != nil {
		t.Fatalf("Register itemA: %v", err)
	}
	if err := idx.Register(callB, "sess-B", "batch-B1"); err != nil {
		t.Fatalf("Register callB: %v", err)
	}

	t.Run("本会话ID比对通过", func(t *testing.T) {
		entry, err := idx.Locate(callA)
		if err != nil {
			t.Fatalf("Locate callA 失败: %v", err)
		}
		if !entry.OwnedBy("sess-A") || !entry.SameBatch("batch-A1") {
			t.Fatalf("callA 应属于 sess-A/batch-A1: %+v", entry)
		}
	})

	t.Run("跨会话ID可被识别并拒绝", func(t *testing.T) {
		// 会话 A 的续轮里出现会话 B 的 call ID：Locate 成功但归属不符。
		entry, err := idx.Locate(callB)
		if err != nil {
			t.Fatalf("Locate callB 失败: %v", err)
		}
		if entry.OwnedBy("sess-A") {
			t.Fatalf("callB 不应被判定属于 sess-A: %+v", entry)
		}
		if !entry.OwnedBy("sess-B") {
			t.Fatalf("callB 应属于 sess-B: %+v", entry)
		}
		if entry.SessionID == "sess-A" {
			t.Fatal("Entry.SessionID 必须暴露真实归属供调用方比对")
		}
	})

	t.Run("各会话自有关联互不串扰", func(t *testing.T) {
		entriesA := idx.EntriesForSession("sess-A")
		if len(entriesA) != 2 {
			t.Fatalf("sess-A 应有 2 条关联，实际 %d", len(entriesA))
		}
		if _, ok := entriesA[callB]; ok {
			t.Fatal("sess-A 快照不应包含 sess-B 的关联")
		}
		entriesB := idx.EntriesForSession("sess-B")
		if len(entriesB) != 1 {
			t.Fatalf("sess-B 应有 1 条关联，实际 %d", len(entriesB))
		}
		// 快照必须是拷贝：外部改动不影响索引。
		for key := range entriesA {
			delete(entriesA, key)
		}
		if idx.Len() != 3 {
			t.Fatalf("外部改动快照不应影响索引，实际条目 %d", idx.Len())
		}
	})
}

// TestCursorSessionIndexDeleteSessionClearsAll 覆盖 DeleteSession 的全量清理。
func TestCursorSessionIndexDeleteSessionClearsAll(t *testing.T) {
	idx := NewCursorSessionIndex()

	// 会话 A：历史批次 batch-A0（已消费的 response/item）+ 当前 pending 批 batch-A1。
	history := map[CursorSessionIndexKey]string{
		cursorIndexKey("chat", CursorIDKindCall, "old_call"):          "batch-A0",
		cursorIndexKey("responses", CursorIDKindResponse, "old_resp"): "batch-A0",
		cursorIndexKey("responses", CursorIDKindItem, "old_item"):     "batch-A0",
		cursorIndexKey("gemini", CursorIDKindCall, "cur_call"):        "batch-A1",
		cursorIndexKey("gemini", CursorIDKindResponse, "cur_resp"):    "batch-A1",
	}
	for key, batch := range history {
		if err := idx.Register(key, "sess-A", batch); err != nil {
			t.Fatalf("Register %s 失败: %v", key.String(), err)
		}
	}
	for _, key := range []CursorSessionIndexKey{
		cursorIndexKey("chat", CursorIDKindCall, "old_call"),
		cursorIndexKey("responses", CursorIDKindResponse, "old_resp"),
	} {
		if _, err := idx.MarkConsumed(key); err != nil {
			t.Fatalf("MarkConsumed %s 失败: %v", key.String(), err)
		}
	}

	// 会话 B：不应被误删。
	otherKey := cursorIndexKey("chat", CursorIDKindCall, "b_call")
	if err := idx.Register(otherKey, "sess-B", "batch-B1"); err != nil {
		t.Fatalf("Register sess-B 失败: %v", err)
	}

	t.Run("历史批次与pending一并清除", func(t *testing.T) {
		removed := idx.DeleteSession("sess-A")
		if removed != len(history) {
			t.Fatalf("应删除 %d 条（含历史批次），实际 %d", len(history), removed)
		}
		if idx.Len() != 1 {
			t.Fatalf("删除后应只剩 sess-B 的 1 条，实际 %d", idx.Len())
		}
		if got := idx.EntriesForSession("sess-A"); len(got) != 0 {
			t.Fatalf("sess-A 应无残留关联，实际 %d 条", len(got))
		}
		for key := range history {
			if _, err := idx.Locate(key); !errors.Is(err, ErrCursorSessionIndexNotFound) {
				t.Fatalf("%s 应已清除，实际 %v", key.String(), err)
			}
		}
	})

	t.Run("其它会话不受影响", func(t *testing.T) {
		entry, err := idx.Locate(otherKey)
		if err != nil {
			t.Fatalf("sess-B 关联应保留: %v", err)
		}
		if !entry.OwnedBy("sess-B") {
			t.Fatalf("sess-B 归属不符: %+v", entry)
		}
	})

	t.Run("重复删除与空session幂等", func(t *testing.T) {
		if removed := idx.DeleteSession("sess-A"); removed != 0 {
			t.Fatalf("重复删除应返回 0，实际 %d", removed)
		}
		if removed := idx.DeleteSession(""); removed != 0 {
			t.Fatalf("空 sessionID 应返回 0，实际 %d", removed)
		}
		if idx.Len() != 1 {
			t.Fatalf("幂等删除不应影响其它会话，实际 %d", idx.Len())
		}
	})

	t.Run("清理后不区分已结束与从未存在", func(t *testing.T) {
		// 方案要求：不留墓碑表，清理后的 ID 与从未登记的 ID 表现一致。
		cleaned := cursorIndexKey("chat", CursorIDKindCall, "old_call")
		never := cursorIndexKey("chat", CursorIDKindCall, "never_seen")
		_, errCleaned := idx.Locate(cleaned)
		_, errNever := idx.Locate(never)
		if !errors.Is(errCleaned, ErrCursorSessionIndexNotFound) || !errors.Is(errNever, ErrCursorSessionIndexNotFound) {
			t.Fatalf("清理后与从未登记的 ID 应行为一致: cleaned=%v never=%v", errCleaned, errNever)
		}
	})
}

// TestCursorSessionIndexConcurrentRegister 校验并发登记只有一个赢家。
func TestCursorSessionIndexConcurrentRegister(t *testing.T) {
	const workers = 32
	idx := NewCursorSessionIndex()
	key := cursorIndexKey("chat", CursorIDKindCall, "race_call")

	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	conflicts := 0
	unexpected := make([]error, 0)

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(n int) {
			defer wg.Done()
			err := idx.Register(key, "sess-1", "batch-1")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, ErrCursorSessionIndexConflict):
				conflicts++
			default:
				unexpected = append(unexpected, err)
			}
		}(i)
	}
	wg.Wait()

	if len(unexpected) != 0 {
		t.Fatalf("出现非预期错误: %v", unexpected[0])
	}
	if success != 1 {
		t.Fatalf("并发登记应只有一个成功，实际 %d", success)
	}
	if conflicts != workers-1 {
		t.Fatalf("冲突数应为 %d，实际 %d", workers-1, conflicts)
	}
	if idx.Len() != 1 {
		t.Fatalf("索引条目数应为 1，实际 %d", idx.Len())
	}
}
