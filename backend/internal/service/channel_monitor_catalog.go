package service

// 本文件实现渠道监控目录推导（派发单 C1）。目录是 (platform, model, group) 三元组的
// 集合，用于驱动后续探测调度（C2）与展示（C3）；本文件只负责推导，不接入调度器、
// 不直接连原语、不渲染前端。
//
// 判定谓词（严格按派发单 C1）：
//   - account.Enabled == true  —— 即 account.IsActive()（Status == active）。
//     temp_unschedulable 账号的 Status 仍为 active（仅被调度层临时摘牌），故保留在目录；
//     此处刻意不使用 Schedulable，否则瞬态账号会被错误排除。
//   - account.Platform == platform  —— 三元组中的 platform 直接取自 account.Platform。
//   - account.IsModelSupported(model) —— 直接调用 account.go:858，不重新实现。
//   - 对每个所属分组：group 启用（IsActive）且
//     (group.ModelAllowlistEnabled() == false 或 group.ModelAllowlist.Allows(model))。
//
// 候选模型取自账号 model_mapping 的键（客户端请求模型名），不复制平台表、不做笛卡尔积、
// 不并入完整调度可达性。调用方通过既有账号/分组仓储查询传入 accounts 与 groupsByID。

// ChannelMonitorCatalogKey 唯一标识监控目录中的一个 (platform, model, group) 三元组。
type ChannelMonitorCatalogKey struct {
	Platform string
	Model    string
	GroupID  int64
}

// ChannelMonitorCatalogEntry 是目录中由某账号提供的可监控 (platform, model, group) 能力。
type ChannelMonitorCatalogEntry struct {
	Platform  string
	Model     string
	GroupID   int64
	AccountID int64
}

// ChannelMonitorCatalog 是目录推导结果：去重后的三元组集合及其贡献账号。
type ChannelMonitorCatalog struct {
	// Keys 是去重后的三元组集合，便于 O(1) 成员判定。
	Keys map[ChannelMonitorCatalogKey]struct{}
	// Entries 是带贡献账号的有序条目（去重前顺序不定，调用方如需稳定排序可自行排序）。
	Entries []ChannelMonitorCatalogEntry
	// CandidateAccountIDs 是每个三元组的所有候选贡献账号 ID（无论分组，仅 enabled
	// + 模型支持 + 分组通过的活跃账号）。探测调度器据此按账号 ID 升序抽样首个在售账号。
	CandidateAccountIDs map[ChannelMonitorCatalogKey][]int64
}

// DeriveChannelMonitorCatalog 严格按派发单 C1 谓词推导渠道监控目录。
//
// 参数：
//   - accounts：已加载的账号集合（调用方通过既有账号仓储查询获取；本函数不触碰数据库）。
//   - groupsByID：分组查找表（调用方通过既有分组仓储查询获取），键为 group.id。
//     账号通过 account.GroupIDs / account.Groups 定位其所属分组。
//
// 返回：去重后的 (platform, model, group) 三元组集合（及贡献账号）。
func DeriveChannelMonitorCatalog(accounts []Account, groupsByID map[int64]*Group) ChannelMonitorCatalog {
	catalog := ChannelMonitorCatalog{
		Keys:                make(map[ChannelMonitorCatalogKey]struct{}),
		CandidateAccountIDs: make(map[ChannelMonitorCatalogKey][]int64),
	}
	if groupsByID == nil {
		groupsByID = map[int64]*Group{}
	}
	for i := range accounts {
		acc := &accounts[i]
		// 谓词 1：enabled（active）。temp_unschedulable 账号 Status 仍为 active，自然保留。
		if !acc.IsActive() {
			continue
		}
		// 候选模型：model_mapping 的键（客户端请求模型名）。空映射 => 无具体候选，
		// 由真实流量数据在 C3 阶段补充，此处不做笛卡尔积。
		models := candidateCatalogModels(acc)
		for _, model := range models {
			// 谓词 3：账号是否支持该模型（直接调用，不重新实现）。
			if !acc.IsModelSupported(model) {
				continue
			}
			// 谓词 4：对每个所属分组判定。
			for _, gid := range accountCatalogGroupIDs(acc) {
				group := groupsByID[gid]
				if group == nil {
					continue
				}
				// 分组启用。
				if !group.IsActive() {
					continue
				}
				// 分组模型白名单：未启用 => 放行；启用则必须 Allows(model)。
				if group.ModelAllowlistEnabled() && !group.ModelAllowlist.Allows(model) {
					continue
				}
				key := ChannelMonitorCatalogKey{Platform: acc.Platform, Model: model, GroupID: gid}
				if _, seen := catalog.Keys[key]; !seen {
					catalog.Keys[key] = struct{}{}
					catalog.Entries = append(catalog.Entries, ChannelMonitorCatalogEntry{
						Platform:  acc.Platform,
						Model:     model,
						GroupID:   gid,
						AccountID: acc.ID,
					})
				}
				// 候选贡献账号累加（去重前的每条命中都计入，供探测抽样按 ID 升序取首个）。
				catalog.CandidateAccountIDs[key] = append(catalog.CandidateAccountIDs[key], acc.ID)
			}
		}
	}
	return catalog
}

// candidateCatalogModels 返回账号用于目录推导的候选模型名（model_mapping 的键）。
// 这些键即客户端请求模型名，与 IsModelSupported 的入参语义一致。
func candidateCatalogModels(acc *Account) []string {
	mapping := acc.GetModelMapping()
	if len(mapping) == 0 {
		return nil
	}
	models := make([]string, 0, len(mapping))
	for requested := range mapping {
		models = append(models, requested)
	}
	return models
}

// accountCatalogGroupIDs 收集账号所属分组的去重 ID 列表，合并 GroupIDs 与 Groups 两种来源。
func accountCatalogGroupIDs(acc *Account) []int64 {
	seen := make(map[int64]struct{}, len(acc.GroupIDs)+len(acc.Groups))
	out := make([]int64, 0, len(acc.GroupIDs)+len(acc.Groups))
	add := func(id int64) {
		if id <= 0 {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, gid := range acc.GroupIDs {
		add(gid)
	}
	for _, g := range acc.Groups {
		if g == nil {
			continue
		}
		add(g.ID)
	}
	return out
}
