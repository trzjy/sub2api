// 分组白名单自动候选：从组内适用账号的真实上游模型目录聚合公开请求名。
//
// 权威与边界：docs-local/group-model-allowlist-candidates-20261021/plan.md §5.2。
// 候选只来自账号真实供给（实时目录）+ 账号显式精确 alias + 组内已保存的精确
// composite 路由公开名；平台内置默认目录与默认映射注入一律不作为候选来源。
// 本次改动不改调度/路由能力、不改公开 /models、不改账号同步写链。

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// GroupModelCatalogReader 按账号读取上游真实模型目录的窄只读面。
// 生产实现 = groupModelCatalogReader（复用现有账号认证/代理/读取限额/超时）；
// 测试注入可控目录响应的替身。方法只服务本次候选读取，不新增目录仓库或平台注册中心。
type GroupModelCatalogReader interface {
	// FetchAccountModelCatalog 返回账号上游当前提供的模型 ID 列表。
	// 返回 (nil, nil) = 上游权威空目录（成功空，与读取失败区分）；错误不得当作空目录。
	FetchAccountModelCatalog(ctx context.Context, account *Account) ([]string, error)
}

// groupModelCatalogReader 是 GroupModelCatalogReader 的生产实现。
// openaiGateway 负责 OpenAI 平台账号（APIKey /v1/models 与 OAuth Codex 认证目录），
// accountTest 负责其余具名/协议兼容平台的通用只读读取与 muse 目录专用读取。
type groupModelCatalogReader struct {
	accountTest   *AccountTestService
	openaiGateway *OpenAIGatewayService
}

// NewGroupModelCatalogReader 构造生产目录读取适配器（wire provider 使用）。
// 任一依赖未装配时对应分支失败关闭（返回配置错误），不以缺依赖冒充空目录。
func NewGroupModelCatalogReader(accountTest *AccountTestService, openaiGateway *OpenAIGatewayService) GroupModelCatalogReader {
	return &groupModelCatalogReader{accountTest: accountTest, openaiGateway: openaiGateway}
}

// FetchAccountModelCatalog 按账号协议/凭证分派到已有只读目录读取。
//
// 分流必须先于任何读取完成（plan §5.1「候选纯读入口必须在调用前准确分流」）：
//   - 网页凭证账号（Cookie/Token）不是 API key，打上游 /v1/models 既无意义也泄漏凭证形状；
//   - 火山订阅号走真实对话端点全量探活，禁止弹窗打开时触发生成请求；
//   - Cursor 的静态初始化和全局目录不证明单账号全支持（P6 缺口），本轮明确不支持；
//   - muse 走专属只读目录（fetchMuseUpstreamCatalog），不进入通用 OpenAI /v1/models 分支。
//
// 其余接入（grok / anthropic / gemini / antigravity / CN 与 other / codebuddy
// 母与影账号 / 多协议 API Key）经 FetchUpstreamSupportedModels 的对应只读分支。
func (r *groupModelCatalogReader) FetchAccountModelCatalog(ctx context.Context, account *Account) ([]string, error) {
	if account == nil {
		return nil, newUpstreamModelSyncConfigError("Account is required for model catalog read", nil)
	}
	if account.IsWebAccessMode() {
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported platform for allowlist candidates: %s (web access mode)", account.Platform), nil,
		)
	}
	if isVolcanoPlanAccount(account) {
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported platform for allowlist candidates: %s (volcano plan account performs live generation probing)", account.Platform), nil,
		)
	}
	if account.Platform == PlatformCursor {
		return nil, newUpstreamModelSyncUnsupportedError(
			"Unsupported platform for allowlist candidates: cursor (no qualified per-account read-only catalog in this round; P6 gap)", nil,
		)
	}
	if isMuseAccount(account) {
		return r.fetchMuseCatalog(ctx, account)
	}
	if account.Platform == PlatformOpenAI {
		return r.fetchOpenAICatalog(ctx, account)
	}
	if r.accountTest == nil {
		return nil, newUpstreamModelSyncConfigError("Account test service is not configured for model catalog read", nil)
	}
	return r.accountTest.FetchUpstreamSupportedModels(ctx, account)
}

// fetchMuseCatalog 复用 muse 专属只读目录（上游 /models 原始 ID）；空的合法目录由
// 上游解析器统一按错误处理（与同步路径同源），本边界不改该语义（plan §5.1）。
func (r *groupModelCatalogReader) fetchMuseCatalog(ctx context.Context, account *Account) ([]string, error) {
	if r.accountTest == nil {
		return nil, newUpstreamModelSyncConfigError("Account test service is not configured for muse catalog read", nil)
	}
	models, _, err := r.accountTest.fetchMuseUpstreamCatalog(ctx, account)
	return models, err
}

// fetchOpenAICatalog 复用 FetchOpenAIModelsList 的原始目录（APIKey /v1/models 与
// OAuth Codex 认证目录 + 账号缓存），取原始 data 条目 ID；不带本地图片补项、
// 不回本地 Codex 默认 manifest、不做分组投影。上游权威空数组 = 成功空。
func (r *groupModelCatalogReader) fetchOpenAICatalog(ctx context.Context, account *Account) ([]string, error) {
	if r.openaiGateway == nil {
		return nil, newUpstreamModelSyncConfigError("OpenAI gateway is not configured for model catalog read", nil)
	}
	response, err := r.openaiGateway.FetchOpenAIModelsList(ctx, account)
	if err != nil {
		return nil, err
	}
	_, entries, err := modelCatalogEntries(response.Body, "data")
	if err != nil {
		return nil, invalidOpenAIModelsList(err)
	}
	ids := make([]string, 0, len(entries))
	for _, raw := range entries {
		var model struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &model); err != nil {
			return nil, invalidOpenAIModelsList(err)
		}
		if id := strings.TrimSpace(model.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// collectGroupModelAllowlistCandidates 聚合组内适用账号的真实供给候选（plan §5.2）。
//
//  1. 适用账号 = 组内既存 schedulable 账号按平台过滤（composite 取具体平台账号）；
//  2. 每个适用账号读取真实目录；任一账号读取失败 → 整次候选请求失败（P3），
//     不静默跳过失败账号拼出不完整并集；
//  3. 可枚举公开名只来自：真实目录 ID、账号显式精确 alias、组内已保存的精确
//     composite 路由公开名；每项用既有映射/路由解析实际供给目标并在相应目录命中；
//  4. 排序去重后输出；结果只表达目录与既有供给资格，不宣称生成探活/额度可用。
func (s *adminServiceImpl) collectGroupModelAllowlistCandidates(ctx context.Context, groupID int64, platform string, accounts []Account) ([]string, error) {
	// P1 空候选优先于 P3 依赖错误：先过滤适用账号，空组直接返回空候选，
	// 不借用全平台目录、不报依赖缺失。reader nil 校验置于"确有适用账号需要读目录"之后
	// （此前位于 applicable 过滤之前，导致空组也报配置错误，见 codex 验收 §3）。
	applicable := make([]Account, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		if platform == PlatformComposite {
			if !isConcreteRequestPlatform(acc.Platform) {
				continue
			}
		} else if acc.Platform != platform {
			// 复用既有混合供给规则：Gemini 组可纳入启用 mixed_scheduling 的
			// Antigravity 账号（与 gateway_service.go:1387 mixedListingAccountAllowed
			// 同源），不新开其他跨平台组合。
			if !mixedListingAccountAllowed(platform, acc) {
				continue
			}
		}
		applicable = append(applicable, *acc)
	}
	if len(applicable) == 0 {
		return []string{}, nil
	}
	reader := s.groupModelCatalogReader
	if reader == nil {
		return nil, fmt.Errorf("group model catalog reader is not configured")
	}

	catalogs := make([]map[string]struct{}, len(applicable))
	for i := range applicable {
		acc := &applicable[i]
		ids, err := reader.FetchAccountModelCatalog(ctx, acc)
		if err != nil {
			return nil, fmt.Errorf("load model catalog for account %d (%s): %w", acc.ID, acc.Platform, err)
		}
		set := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if id = strings.TrimSpace(id); id != "" {
				set[id] = struct{}{}
			}
		}
		catalogs[i] = set
	}

	seen := make(map[string]struct{})
	out := make([]string, 0)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}

	if platform == PlatformComposite {
		// 发现 1 整改：composite 模式下"真实目录 ID"与"账号显式精确 alias"
		// 两个来源同样须经 CompositeRouteResolver.Resolve 组路由决策，并按决策最终
		// 的 (TargetPlatform, UpstreamModel) 做供给校验（compositeRouteSupplyExists），
		// 不通过则不列。三个来源（目录 ID、alias、精确路由）统一走"resolver 决策 →
		// 供给校验"，不复用 per-account 准入绕过组路由。非 composite 平台语义不变。
		// 性能边界：resolver.Resolve 每次内部调 repo.ListByGroup，已按唯一 name 去重
		// 以收敛解析次数；不自建匹配器/缓存层（遵守整改边界）。
		if err := s.appendCompositeCatalogAndAliasCandidates(ctx, groupID, applicable, catalogs, add); err != nil {
			return nil, err
		}
		if err := s.appendCompositeRouteCandidates(ctx, groupID, applicable, catalogs, add); err != nil {
			return nil, err
		}
	} else {
		// 非 composite：逐账号既有准入语义（无组路由可查），语义不变。
		// 来源一：真实目录 ID（透传候选）；来源二：账号显式精确 alias。
		// mixed 账号（platform 为 antigravity）经既有混合规则纳入后，其产出的候选公开名
		// 须过 mixedListingModelAllowed（与运行时 gateway_service.go:1393 同源：仅 gemini-
		// 前缀的 wire ID 在 Gemini 组上有意义），不自行发明别的跨平台口径。纯平台账号
		// （acc.Platform == platform）语义不变。
		for i := range applicable {
			acc := &applicable[i]
			catalog := catalogs[i]
			isMixed := acc.Platform != platform
			for id := range catalog {
				if isMixed && !mixedListingModelAllowed(platform, id) {
					continue
				}
				if accountServesPublicModel(acc, id, catalog) {
					add(id)
				}
			}
			for key := range explicitExactMappingKeys(acc) {
				if isMixed && !mixedListingModelAllowed(platform, key) {
					continue
				}
				if accountServesPublicModel(acc, key, catalog) {
					add(key)
				}
			}
		}
	}

	sort.Strings(out)
	return out, nil
}

// appendCompositeRouteCandidates 追加组内已保存的精确路由公开名（plan §5.2 来源三）。
// 路由命中优先级完全沿既有 matchCompositeRoute（exact 优先 / endpoint 优先 /
// 最长 prefix / priority / ID），不复制算法；只对命中路由的（target, upstream）做
// 账号目录供给校验。通配/前缀路由是策略，不拼接成虚构具体模型。
func (s *adminServiceImpl) appendCompositeRouteCandidates(ctx context.Context, groupID int64, applicable []Account, catalogs []map[string]struct{}, add func(string)) error {
	if s.compositeRouteRepo == nil {
		return nil
	}
	repoRoutes, err := s.compositeRouteRepo.ListByGroup(ctx, groupID, false)
	if err != nil {
		return fmt.Errorf("list composite routes for allowlist candidates: %w", err)
	}
	resolver := s.compositeResolver
	if resolver == nil {
		resolver = NewCompositeRouteResolver(s.compositeRouteRepo)
	}
	for _, route := range repoRoutes {
		if !route.Enabled {
			continue
		}
		if normalizeCompositeRouteMatchType(route.MatchType) != CompositeRouteMatchExact {
			continue
		}
		publicModel := strings.TrimSpace(route.PublicModel)
		if publicModel == "" {
			continue
		}
		decision, err := resolver.Resolve(ctx, groupID, publicModel, normalizeCompositeRouteEndpoint(route.Endpoint))
		if err != nil {
			return fmt.Errorf("resolve composite route %q for allowlist candidates: %w", publicModel, err)
		}
		if !decision.Matched {
			continue
		}
		if !compositeRouteSupplyExists(applicable, catalogs, decision.TargetPlatform, decision.UpstreamModel) {
			continue
		}
		add(publicModel)
	}
	return nil
}

// appendCompositeCatalogAndAliasCandidates 在 composite 模式下把"真实目录 ID"与
// "账号显式精确 alias"两个来源统一经 CompositeRouteResolver.Resolve 决策，并对决策
// 的 (TargetPlatform, UpstreamModel) 做供给校验（compositeRouteSupplyExists），不通过
// 则不列。不新建第二套路由匹配：复用 s.compositeResolver（nil 时 fallback
// NewCompositeRouteResolver，同既有 appendCompositeRouteCandidates 约定）。endpoint
// 传 ""（归一为 any）以匹配无特定端点的目录候选；命中既有的组路由优先级链。
func (s *adminServiceImpl) appendCompositeCatalogAndAliasCandidates(ctx context.Context, groupID int64, applicable []Account, catalogs []map[string]struct{}, add func(string)) error {
	names := make(map[string]struct{})
	for i := range applicable {
		acc := &applicable[i]
		catalog := catalogs[i]
		for id := range catalog {
			if id = strings.TrimSpace(id); id != "" {
				names[id] = struct{}{}
			}
		}
		for key := range explicitExactMappingKeys(acc) {
			if key = strings.TrimSpace(key); key != "" {
				names[key] = struct{}{}
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	resolver := s.compositeResolver
	if resolver == nil {
		resolver = NewCompositeRouteResolver(s.compositeRouteRepo)
	}
	for name := range names {
		decision, err := resolver.Resolve(ctx, groupID, name, "")
		if err != nil {
			return fmt.Errorf("resolve composite candidate %q for allowlist candidates: %w", name, err)
		}
		if !decision.Matched {
			continue
		}
		if !compositeRouteSupplyExists(applicable, catalogs, decision.TargetPlatform, decision.UpstreamModel) {
			continue
		}
		add(name)
	}
	return nil
}

// compositeRouteSupplyExists 检查存在平台为 target 的适用账号，其真实目录里存在
// 路由 upstream 的实际供给目标（映射解析后命中目录；透传语义照 forward 链）。
func compositeRouteSupplyExists(applicable []Account, catalogs []map[string]struct{}, target, upstream string) bool {
	target = strings.TrimSpace(target)
	upstream = strings.TrimSpace(upstream)
	if target == "" || upstream == "" {
		return false
	}
	for i := range applicable {
		acc := &applicable[i]
		if acc.Platform != target {
			continue
		}
		if accountServesPublicModel(acc, upstream, catalogs[i]) {
			return true
		}
	}
	return false
}

// accountServesPublicModel 判定账号按既有准入与映射语义是否实际供给公开请求名：
//  1. 既有 IsModelSupported 准入（映射白名单 / 透传 / 空映射语义不变，plan §6）；
//  2. 用既有映射/路由解析实际供给目标（passthrough 账号照 forward 链原样透传）；
//  3. 解析出的目标必须命中该账号的真实目录——配置映射只负责命名与限制，不能单独
//     证明上游存在该模型（P2）。
func accountServesPublicModel(account *Account, publicModel string, catalog map[string]struct{}) bool {
	if account == nil || publicModel == "" || len(catalog) == 0 {
		return false
	}
	if !account.IsModelSupported(publicModel) {
		return false
	}
	target := publicModel
	if !(account.Platform == PlatformOpenAI && account.IsOpenAIPassthroughEnabled()) {
		target, _ = account.ResolveMappedModel(publicModel)
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	_, ok := catalog[target]
	return ok
}

// explicitExactMappingKeys 返回账号原始 credentials 显式映射中的精确键（排除通配键）。
// 平台默认注入（grok / antigravity / gemini GoogleOne）不在此列——默认映射键不是
// 候选枚举来源（plan §6「候选禁止用其默认键作为目录来源」）；显式 alias 经既有
// 解析后目标存在于账号目录才成为自动候选。
func explicitExactMappingKeys(account *Account) map[string]string {
	if account == nil || account.Credentials == nil {
		return nil
	}
	raw := stringMappingFromRaw(account.Credentials["model_mapping"])
	out := make(map[string]string, len(raw))
	for key := range raw {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" || strings.Contains(trimmed, "*") {
			continue
		}
		out[trimmed] = raw[key]
	}
	return out
}
