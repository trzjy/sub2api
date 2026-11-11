package service

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// 优先级严格分层（PRIORITY-TIER-STRICT）。
//
// 语义：`accounts.priority` 是"谁先被用"的唯一事实源。数值小者优先；只要最小的那一档
// 里还有可用账号，更大数值的账号一律不得参与本次选号。
//
// 为什么需要这一层：历史实现里"谁先被选中"由多处逻辑共同决定（组档位 SQL 排序、
// 内存 LRU、免费额度优先分区、低上游倍率优先、订阅优先、高级调度加权打分），
// 其中任何一条都能把高优先级的账号插到低优先级前面。本函数把这些跨档重排统一收口为
// **唯一的档位门**：在候选集进入各选号器之前先砍掉非最小档，后续所有排序逻辑
//（LRU / 负载 / 分区）只可能在同档内部起作用，物理上无法跨档。
//
// 调用纪律（新增选号入口必须遵守）：
//  1. 必须在**硬门与准入判定之后**调用——最小档若整体不可用（模型不支持、限流等），
//     必须允许降档，否则会造成"有号却 502"。
//  2. 全站只允许调用本函数做档位过滤，任何调用方不得自行再写一遍 min(priority) 判断。
//  3. 开关关闭时本函数原样返回入参，行为与改造前完全一致。

const priorityTierStrictEnvKey = "PRIORITY_TIER_STRICT_ENABLED"

var (
	priorityTierStrictOnce sync.Once
	priorityTierStrictOn   bool
	// priorityTierStrictTestOverride 仅供同包单测覆盖开关状态；生产路径恒为 nil。
	priorityTierStrictTestOverride atomic.Pointer[bool]
)

// priorityTierStrictEnabled 报告档位门是否生效。
//
// 默认**关闭**（与上线方案 §4 一致）：先部署一个行为完全不变的版本，验证后再把
// PRIORITY_TIER_STRICT_ENABLED 置为 1 打开。关闭时所有档位过滤原样返回入参，
// 生产行为与改造前逐字节一致，这是首次上线的安全边界。
func priorityTierStrictEnabled() bool {
	if override := priorityTierStrictTestOverride.Load(); override != nil {
		return *override
	}
	priorityTierStrictOnce.Do(func() {
		raw := strings.TrimSpace(os.Getenv(priorityTierStrictEnvKey))
		priorityTierStrictOn = raw == "1" || strings.EqualFold(raw, "true")
	})
	return priorityTierStrictOn
}

// restrictToMinPriorityTier 只保留最小 priority 档的元素。
//
// 入参为空时原样返回；开关关闭时原样返回（零行为变化）。
// 返回新切片，不改写调用方切片的底层数组。
func restrictToMinPriorityTier[T any](items []T, priorityOf func(T) int) []T {
	if len(items) == 0 || !priorityTierStrictEnabled() {
		return items
	}
	minPriority := priorityOf(items[0])
	for i := 1; i < len(items); i++ {
		if p := priorityOf(items[i]); p < minPriority {
			minPriority = p
		}
	}
	kept := make([]T, 0, len(items))
	for _, item := range items {
		if priorityOf(item) == minPriority {
			kept = append(kept, item)
		}
	}
	return kept
}

// restrictAccountPtrsToMinPriorityTier 是 []*Account 形态的唯一档位过滤入口。
func restrictAccountPtrsToMinPriorityTier(accounts []*Account) []*Account {
	if len(accounts) == 0 {
		return accounts
	}
	return restrictToMinPriorityTier(accounts, func(account *Account) int {
		if account == nil {
			// nil 元素不参与档位计算，视作最低优先级，交由调用方原有的空值处理。
			return int(^uint(0) >> 1)
		}
		return account.Priority
	})
}

// restrictAccountValuesToMinPriorityTier 是 []Account 形态的唯一档位过滤入口。
func restrictAccountValuesToMinPriorityTier(accounts []Account) []Account {
	if len(accounts) == 0 {
		return accounts
	}
	return restrictToMinPriorityTier(accounts, func(account Account) int {
		return account.Priority
	})
}

// priorityTierFloor 返回 accounts 中**通过 admit 判定**的最小 priority 与"是否存在这样的账号"。
//
// 这是全站计算"当前档位下限"的唯一实现；admit 为 nil 表示不额外过滤。
// 供需要在过滤前先做档位校验的调用方（粘性会话命中、路由候选）复用同一份 min 语义。
//
// 为什么要 admit 参数：粘性会话在第 1 层命中，此时还没走准入过滤。若直接对整张
// 账号表求 min，会把"根本不能服务本请求的账号"（如模型不支持、平台不符）算进档位，
// 导致粘性被永久判为跨档——虽然最终仍会选到同一个账号，但粘性绑定形同失效。
// 因此调用方把自己的准入谓词传进来，档位下限只由"真能服务本请求的账号"决定。
func priorityTierFloor(accounts []*Account, admit func(*Account) bool) (int, bool) {
	seen := false
	minPriority := 0
	for _, account := range accounts {
		if account == nil {
			continue
		}
		if admit != nil && !admit(account) {
			continue
		}
		if !seen || account.Priority < minPriority {
			minPriority = account.Priority
			seen = true
		}
	}
	return minPriority, seen
}

// minPriorityOfAccounts 返回候选集里的最小 priority 与"是否存在候选"。
func minPriorityOfAccounts(accounts []*Account) (int, bool) {
	return priorityTierFloor(accounts, nil)
}

// accountPointers 把账号值切片转成指针切片，避免调用方各自手写循环。
// 返回的指针指向入参切片底层数组，调用方不得在持有期间改动入参长度。
func accountPointers(accounts []Account) []*Account {
	if len(accounts) == 0 {
		return nil
	}
	ptrs := make([]*Account, len(accounts))
	for i := range accounts {
		ptrs[i] = &accounts[i]
	}
	return ptrs
}

// stickyAccountWithinPriorityTier 判断粘性命中的账号是否属于当前最小 priority 档。
//
// 粘性会话会长期把同一会话固定在一个账号上，若该账号优先级低于当前可用档，
// 它就成了绕开优先级的旁路。档位门生效时，跨档的粘性命中一律放弃（走 Layer 2 重新选号），
// 由 Layer 2 在选中后改写粘性绑定；不在此处删除绑定，避免档位恢复后会话彻底丢失。
//
// 档位门关闭、候选集为空（无法判定档位）、账号为空时一律放行，保持原行为。
func stickyAccountWithinPriorityTier(account *Account, minTierPriority int, hasTier bool) bool {
	if account == nil || !hasTier || !priorityTierStrictEnabled() {
		return true
	}
	return account.Priority == minTierPriority
}
