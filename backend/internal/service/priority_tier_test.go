package service

import (
	"testing"
)

// setPriorityTierStrictForTest 在测试内固定开关状态，defer 传 nil 恢复环境变量口径。
func setPriorityTierStrictForTest(t *testing.T, on bool) {
	t.Helper()
	value := on
	priorityTierStrictTestOverride.Store(&value)
	t.Cleanup(func() {
		priorityTierStrictTestOverride.Store(nil)
	})
}

func ptrAccount(id int64, priority int) *Account {
	return &Account{ID: id, Priority: priority}
}

func ptrAccountIDs(accounts []*Account) []int64 {
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		if account != nil {
			ids = append(ids, account.ID)
		}
	}
	return ids
}

// 开关关闭：必须原样返回入参（零行为变化，保证回滚安全）。
func TestRestrictToMinPriorityTier_DisabledPassesThrough(t *testing.T) {
	setPriorityTierStrictForTest(t, false)

	in := []*Account{ptrAccount(1, 5), ptrAccount(2, 1), ptrAccount(3, 3)}
	out := restrictAccountPtrsToMinPriorityTier(in)

	if len(out) != len(in) {
		t.Fatalf("disabled tier gate must pass through: got %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("disabled tier gate must preserve order/identity at %d", i)
		}
	}
}

// 核心语义：只保留最小 priority 档，高优先级账号一个都不许漏进来。
func TestRestrictToMinPriorityTier_KeepsOnlyMinTier(t *testing.T) {
	setPriorityTierStrictForTest(t, true)

	in := []*Account{ptrAccount(10, 2), ptrAccount(11, 1), ptrAccount(12, 1), ptrAccount(13, 3)}
	out := restrictAccountPtrsToMinPriorityTier(in)

	ids := ptrAccountIDs(out)
	if len(ids) != 2 || ids[0] != 11 || ids[1] != 12 {
		t.Fatalf("want only priority=1 tier [11 12], got %v", ids)
	}
}

// 全同档：不做任何删减。
func TestRestrictToMinPriorityTier_SingleTierKeepsAll(t *testing.T) {
	setPriorityTierStrictForTest(t, true)

	in := []*Account{ptrAccount(1, 4), ptrAccount(2, 4)}
	if out := restrictAccountPtrsToMinPriorityTier(in); len(out) != 2 {
		t.Fatalf("single tier must keep all: got %d", len(out))
	}
}

// 空入参：返回空，不得 panic。
func TestRestrictToMinPriorityTier_Empty(t *testing.T) {
	setPriorityTierStrictForTest(t, true)

	if out := restrictAccountPtrsToMinPriorityTier(nil); len(out) != 0 {
		t.Fatalf("nil input must stay empty, got %d", len(out))
	}
	if out := restrictAccountPtrsToMinPriorityTier([]*Account{}); len(out) != 0 {
		t.Fatalf("empty input must stay empty, got %d", len(out))
	}
}

// []Account（值切片）形态与指针形态语义一致。
func TestRestrictToMinPriorityTier_ValueSlice(t *testing.T) {
	setPriorityTierStrictForTest(t, true)

	in := []Account{{ID: 1, Priority: 3}, {ID: 2, Priority: 1}}
	out := restrictAccountValuesToMinPriorityTier(in)
	if len(out) != 1 || out[0].ID != 2 {
		t.Fatalf("want only id=2 (priority=1), got %+v", out)
	}
}

// nil 元素不参与档位计算（视作最低优先级），不得影响最小档判定。
func TestRestrictToMinPriorityTier_NilElementIgnored(t *testing.T) {
	setPriorityTierStrictForTest(t, true)

	in := []*Account{nil, ptrAccount(7, 2)}
	out := restrictAccountPtrsToMinPriorityTier(in)
	ids := ptrAccountIDs(out)
	if len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("nil element must not win the tier: got %v", ids)
	}
}

// 返回新切片，不得改写调用方底层数组（避免调用方后续读到被篡改的候选集）。
func TestRestrictToMinPriorityTier_DoesNotMutateInput(t *testing.T) {
	setPriorityTierStrictForTest(t, true)

	in := []*Account{ptrAccount(1, 1), ptrAccount(2, 2), ptrAccount(3, 3)}
	backup := append([]*Account(nil), in...)
	_ = restrictAccountPtrsToMinPriorityTier(in)

	for i := range in {
		if in[i] != backup[i] {
			t.Fatalf("input slice mutated at %d", i)
		}
	}
}

func TestMinPriorityOfAccounts(t *testing.T) {
	if _, ok := minPriorityOfAccounts(nil); ok {
		t.Fatal("nil input must report no tier")
	}
	if _, ok := minPriorityOfAccounts([]*Account{nil}); ok {
		t.Fatal("all-nil input must report no tier")
	}
	minPriority, ok := minPriorityOfAccounts([]*Account{ptrAccount(1, 9), ptrAccount(2, 4)})
	if !ok || minPriority != 4 {
		t.Fatalf("want (4,true), got (%d,%v)", minPriority, ok)
	}
}

// 粘性跨档：低优先级账号不得因粘性长期占用会话。
func TestStickyAccountWithinPriorityTier(t *testing.T) {
	setPriorityTierStrictForTest(t, true)

	if !stickyAccountWithinPriorityTier(ptrAccount(1, 1), 1, true) {
		t.Fatal("same tier sticky must be allowed")
	}
	if stickyAccountWithinPriorityTier(ptrAccount(2, 2), 1, true) {
		t.Fatal("cross-tier sticky must be rejected")
	}
	// 无候选集（无法判定档位）时放行，保持原行为。
	if !stickyAccountWithinPriorityTier(ptrAccount(2, 2), 0, false) {
		t.Fatal("no tier context must not block sticky")
	}

	setPriorityTierStrictForTest(t, false)
	if !stickyAccountWithinPriorityTier(ptrAccount(2, 2), 1, true) {
		t.Fatal("disabled tier gate must not block sticky")
	}
}

// 档位下限的准入谓词：不可服务的账号不得把档位压低，否则粘性会被永久判为跨档。
func TestPriorityTierFloor_AdmitPredicate(t *testing.T) {
	// 档位 1 的账号不支持本模型 → 档位下限应取"真能服务"的 2。
	accounts := []*Account{
		{ID: 1, Priority: 1, Platform: PlatformOpenAI},
		{ID: 2, Priority: 2, Platform: PlatformOpenAI},
	}
	admit := func(account *Account) bool { return account.ID != 1 }

	minPriority, ok := priorityTierFloor(accounts, admit)
	if !ok || minPriority != 2 {
		t.Fatalf("want (2,true) with admit filter, got (%d,%v)", minPriority, ok)
	}
	// 不带谓词时取全量最小值。
	if minPriority, _ := priorityTierFloor(accounts, nil); minPriority != 1 {
		t.Fatalf("want 1 without admit filter, got %d", minPriority)
	}
	// 全部被谓词拒绝：报告"无档位"，调用方据此放行粘性（保持原行为）。
	if _, ok := priorityTierFloor(accounts, func(*Account) bool { return false }); ok {
		t.Fatal("all-rejected input must report no tier")
	}
}

// accountPointers 必须指向入参底层数组（各选号入口靠它复用同一份已加载列表）。
func TestAccountPointers(t *testing.T) {
	if got := accountPointers(nil); got != nil {
		t.Fatalf("nil input must stay nil, got %v", got)
	}
	accounts := []Account{{ID: 1}, {ID: 2}}
	ptrs := accountPointers(accounts)
	if len(ptrs) != 2 || ptrs[0] != &accounts[0] || ptrs[1] != &accounts[1] {
		t.Fatalf("pointers must address the input backing array, got %v", ptrs)
	}
}
