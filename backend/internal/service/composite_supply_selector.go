package service

import (
	"context"
	"fmt"
	"log/slog"
	mathrand "math/rand"
	"time"
)

// selectCompositeSupplyByPriority 从候选账号中选出最小 Priority 档的一个账号：
// 先过滤 nil 与 excluded，取最小 Priority 档，同档内用 rnd 随机（rnd==nil 时取
// 该档第一个，便于测试）。无候选返回 nil。
func selectCompositeSupplyByPriority(accounts []*Account, excluded map[int64]struct{}, rnd *mathrand.Rand) *Account {
	var (
		minPriority    int
		minPrioritySet bool
	)
	for _, account := range accounts {
		if account == nil {
			continue
		}
		if _, skip := excluded[account.ID]; skip {
			continue
		}
		if !minPrioritySet || account.Priority < minPriority {
			minPriority = account.Priority
			minPrioritySet = true
		}
	}
	if !minPrioritySet {
		return nil
	}

	tier := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		if _, skip := excluded[account.ID]; skip {
			continue
		}
		if account.Priority == minPriority {
			tier = append(tier, account)
		}
	}
	if len(tier) == 0 {
		return nil
	}
	if len(tier) == 1 || rnd == nil {
		return tier[0]
	}
	return tier[rnd.Intn(len(tier))]
}

// compositeSelectorChains 是统一 selector 尝试的服务链顺序（与既有 chain 常量一致）。
// 账号在任一链上可服务本端点即视为本端点可服务。
var compositeSelectorChains = [...]string{
	CompositeChainOpenAI,
	CompositeChainAnthropic,
	CompositeChainGemini,
}

// SelectCompositeCrossChainAccount 按账号优先级 + 同优先级随机，从 A3 多链
// decision 的 SupplyAccounts 中选出一个本端点可服务的账号，并复用既有并发槽获取逻辑
// 抢占并发槽，返回所选账号与对应的 release。
//
// 所有资格门均复用既有实现，不新增评分公式或平台清单；本方法不写粘性、不记 usage、
// 不改账号状态。槽满即换号（不排队），读取失败与来源门读取失败一律失败关闭（返回 error）。
func (s *GatewayService) SelectCompositeCrossChainAccount(
	ctx context.Context,
	groupID int64,
	requestedModel, endpoint string,
	supply []CompositeOwnershipClaimant,
	excluded map[int64]struct{},
) (*Account, func(), error) {
	if s == nil || s.accountRepo == nil || groupID <= 0 || len(supply) == 0 {
		return nil, nil, fmt.Errorf("select composite cross-chain account: invalid arguments")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// 分组上下文：优先复用 ctx 中已解析的 Group，否则回退到 repo；groupID>0 但
	// group 未解析时由 accountAdmissionDecision 失败关闭。
	group := s.groupFromContext(ctx, groupID)
	if group == nil && s.groupRepo != nil {
		resolved, err := s.resolveGroupByID(ctx, groupID)
		if err != nil {
			return nil, nil, err
		}
		group = resolved
	}

	candidates := make([]*Account, 0, len(supply))
	for _, claim := range supply {
		// 逐账号取新鲜账号；读取错误失败关闭，不存在/不可调度则跳过。
		account, err := s.getSchedulableAccount(ctx, claim.AccountID)
		if err != nil {
			return nil, nil, err
		}
		if account == nil {
			continue
		}
		if _, skip := excluded[account.ID]; skip {
			continue
		}
		if !account.IsSchedulable() {
			continue
		}
		if !s.isAccountInGroup(account, &groupID) {
			continue
		}

		served := false
		for _, chain := range compositeSelectorChains {
			if _, ok := CompositeAccountChainPath(chain, endpoint, account); ok {
				served = true
				break
			}
		}
		if !served {
			continue
		}

		admitted, _, err := accountAdmissionDecision(ctx, account, account.Platform, requestedModel, &groupID, group)
		if err != nil {
			return nil, nil, err
		}
		if !admitted {
			continue
		}
		// 保留 Source=account_model 的显式声明约束。
		if !s.isModelSupportedByAccountWithContext(ctx, account, requestedModel) {
			continue
		}

		reason, err := cursorSupplySourceAvailabilityVetoReason(ctx, account)
		if err != nil {
			return nil, nil, err
		}
		if reason != "" {
			continue
		}
		if !s.isGatewayAccountProfitEligible(ctx, account) {
			continue
		}
		// 与 selectAccountForModelWithPlatform 普通候选循环同口径的可调度门。
		if !s.isAccountSchedulableForSelection(account) {
			continue
		}
		if !s.isAccountSchedulableForModelSelection(ctx, account, requestedModel) {
			continue
		}
		if !s.isAccountSchedulableForQuota(account) {
			continue
		}
		if !s.isAccountSchedulableForWindowCost(ctx, account, false) {
			continue
		}
		if !s.isAccountSchedulableForRPM(ctx, account, false) {
			continue
		}

		candidates = append(candidates, account)
	}

	// blocked = excluded ∪ slotBlocked：本地构造，不修改调用方传入的 excluded。
	blocked := make(map[int64]struct{}, len(excluded)+len(candidates))
	for id := range excluded {
		blocked[id] = struct{}{}
	}
	// 不得引入全局可变随机源：每次选择使用本地初始化的随机源。
	rnd := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))

	// 有界循环：每轮至多排除一个槽满账号，最多 len(candidates) 轮。
	for i := 0; i < len(candidates); i++ {
		selected := selectCompositeSupplyByPriority(candidates, blocked, rnd)
		if selected == nil {
			return nil, nil, fmt.Errorf("%w supporting model: %s", ErrNoAvailableAccounts, requestedModel)
		}
		acquired, err := s.tryAcquireAccountSlot(ctx, selected.ID, selected.Concurrency)
		if err != nil {
			return nil, nil, err
		}
		if acquired == nil || !acquired.Acquired {
			// 槽满即换号，不排队：把该账号加入本地 blocked，继续下一轮。
			blocked[selected.ID] = struct{}{}
			continue
		}
		release := acquired.ReleaseFunc
		if release == nil {
			release = func() {}
		}
		// 抢槽后终检：按最新快照再否决一次，避免等待/刷新期间倍率变化漏门。
		// 与 handler 侧 GatewayProfitControlVetoLatest 终检同口径，跨链入口共用。
		latest, vetoed, reason := s.GatewayProfitControlVetoLatest(ctx, selected)
		if vetoed {
			// 不处罚账号、不写粘性：释放本次抢到的槽并换下一个候选。
			release()
			blocked[selected.ID] = struct{}{}
			slog.Debug("composite_selector_profit_vetoed", "account_id", selected.ID, "reason", reason)
			continue
		}
		if latest != nil {
			// latest 是同一账号的刷新对象，保留本次抢槽的 release。
			selected = latest
		}
		return selected, release, nil
	}
	// 所有候选均槽满被排除。
	return nil, nil, fmt.Errorf("%w supporting model: %s", ErrNoAvailableAccounts, requestedModel)
}
