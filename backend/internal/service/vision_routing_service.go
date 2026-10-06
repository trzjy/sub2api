package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ErrVisionRoutingCrossGroup 配置写入时目标账号不属于该分组（跨组拒绝）。
// docs/capability-routing-plan.md §3.7 / 重审 I-3。
var ErrVisionRoutingCrossGroup = infraerrors.BadRequest(
	"VISION_ROUTING_CROSS_GROUP",
	"vision routing target accounts must belong to this group",
)

// ErrVisionRoutingInvalidConfig 配置写入时模型模式为空/空白或目标账号列表为空
// （终审 P2-5）。空规则会被 lookupVisionRoutingModel 当作未命中，触发调度静默回落
// 普通候选池，故写入侧直接拒绝，使持久化配置恒为有效目标集合。
var ErrVisionRoutingInvalidConfig = infraerrors.BadRequest(
	"VISION_ROUTING_INVALID_CONFIG",
	"vision routing config is invalid: every model pattern must be non-empty and every target account list must be non-empty",
)

// VisionRoutingRepository 定义视觉分流配置（groups.vision_routing）的数据访问接口。
type VisionRoutingRepository interface {
	// Get 返回指定分组的视觉分流配置；未配置返回空 map（nil）。
	Get(ctx context.Context, groupID int64) (map[string][]int64, error)
	// GetByGroupIDs 批量返回多个分组的视觉分流配置（单次查询）。
	// 返回 map[groupID]routing；查询不到的组不在 map 中，调用方按空配置处理。
	GetByGroupIDs(ctx context.Context, groupIDs []int64) (map[int64]map[string][]int64, error)
	// Set 覆盖写指定分组的视觉分流配置；分组不存在返回 ErrGroupNotFound。
	Set(ctx context.Context, groupID int64, routing map[string][]int64) error
}

// VisionRoutingGroupAccountsReader 提供同组校验所需的分组账号集合。
// 由既有 GroupRepository 实现（GetAccountIDsByGroupIDs）。
type VisionRoutingGroupAccountsReader interface {
	// GetAccountIDsByGroupIDs 获取多个分组的所有账号 ID（去重）。
	GetAccountIDsByGroupIDs(ctx context.Context, groupIDs []int64) ([]int64, error)
}

// VisionRoutingService 管理分组视觉分流配置（docs/capability-routing-plan.md §3.7）。
//
// 职责：
//   - 配置写入时强制同组校验（vision_target ⊆ 该分组账号），跨组拒绝；
//   - 提供读取方法供调度（派发单 C）接入时查询目标账号。
type VisionRoutingService struct {
	repo      VisionRoutingRepository
	groupRepo VisionRoutingGroupAccountsReader
}

// NewVisionRoutingService 创建视觉分流配置服务。
func NewVisionRoutingService(
	repo VisionRoutingRepository,
	groupRepo VisionRoutingGroupAccountsReader,
) *VisionRoutingService {
	return &VisionRoutingService{repo: repo, groupRepo: groupRepo}
}

// Get 返回指定分组的视觉分流配置。
func (s *VisionRoutingService) Get(ctx context.Context, groupID int64) (map[string][]int64, error) {
	if s == nil || s.repo == nil {
		return nil, errors.New("vision routing service is not configured")
	}
	return s.repo.Get(ctx, groupID)
}

// GetByGroupIDs 批量返回多个分组的视觉分流配置，直通 repo（单次查询，禁止 N+1）。
// 返回 map[groupID]routing；查询不到的组不在 map 中，调用方按空配置处理。
// 与既有 Get/GetVisionRoutingAccountIDs 直读语义一致，不引入缓存层。
func (s *VisionRoutingService) GetByGroupIDs(ctx context.Context, groupIDs []int64) (map[int64]map[string][]int64, error) {
	if s == nil || s.repo == nil {
		return nil, errors.New("vision routing service is not configured")
	}
	return s.repo.GetByGroupIDs(ctx, groupIDs)
}

// ValidateVisionRoutingShape 仅校验配置自身合法性（模型模式非空、目标列表非空、
// 目标 ID 为正），不做同组校验（需要分组上下文）。供分组创建路径在落库前预校验，
// 与 Set/Validate 内部走同一 validateVisionRoutingConfig，避免第二套校验实现。
func ValidateVisionRoutingShape(routing map[string][]int64) error {
	if routing == nil {
		return nil
	}
	return validateVisionRoutingConfig(routing)
}

// Validate 校验指定分组的视觉分流配置但不写存储，语义与 Set 的写入前校验完全一致
// （同一 validateRouting 实现，非第二套校验）。供分组 Update 在写分组之前前置调用，
// 使视觉分流校验失败时分组写整体不发生（消除部分提交，派发单 B）。
func (s *VisionRoutingService) Validate(ctx context.Context, groupID int64, routing map[string][]int64) error {
	if s == nil || s.repo == nil {
		return errors.New("vision routing service is not configured")
	}
	if routing == nil {
		routing = map[string][]int64{}
	}
	return s.validateRouting(ctx, groupID, routing)
}

// Set 覆盖写指定分组的视觉分流配置，并在写入前执行同组校验：
// 每个模型的 target 账号必须 ⊆ 该分组账号，否则拒绝并返回 ErrVisionRoutingCrossGroup。
// routing 为 nil 或空 map 时写入空对象（等价于清空配置）。
func (s *VisionRoutingService) Set(ctx context.Context, groupID int64, routing map[string][]int64) error {
	if s == nil || s.repo == nil {
		return errors.New("vision routing service is not configured")
	}
	if routing == nil {
		routing = map[string][]int64{}
	}
	if err := s.validateRouting(ctx, groupID, routing); err != nil {
		return err
	}
	return s.repo.Set(ctx, groupID, routing)
}

// validateRouting 是配置校验的唯一实现：先做配置有效性校验（模型模式/目标列表非空、
// 目标 ID 为正），再做同组校验（target ⊆ 分组账号）。Set 与 Validate 共用，避免校验漂移。
func (s *VisionRoutingService) validateRouting(ctx context.Context, groupID int64, routing map[string][]int64) error {
	// 写入校验：模型模式不得为空/空白，目标账号列表不得为空（终审 P2-5）。
	if err := validateVisionRoutingConfig(routing); err != nil {
		return err
	}

	// 收集配置中引用的全部目标账号，做一次分组账号查询（避免对每个模型重复查库）。
	targets := collectVisionRoutingTargetAccountIDs(routing)
	if len(targets) > 0 {
		if s.groupRepo == nil {
			return errors.New("vision routing group accounts reader is not configured")
		}
		groupAccountIDs, err := s.groupRepo.GetAccountIDsByGroupIDs(ctx, []int64{groupID})
		if err != nil {
			return fmt.Errorf("load group %d accounts for vision routing validation: %w", groupID, err)
		}
		allowed := make(map[int64]struct{}, len(groupAccountIDs))
		for _, id := range groupAccountIDs {
			allowed[id] = struct{}{}
		}
		for _, id := range targets {
			if _, ok := allowed[id]; !ok {
				return fmt.Errorf("%w: account %d is not in group %d", ErrVisionRoutingCrossGroup, id, groupID)
			}
		}
	}
	return nil
}

// GetVisionRoutingAccountIDs 返回指定分组中某模型的视觉分流目标账号 ID 列表。
// 未命中返回 nil。语义与 Group.GetVisionRoutingAccountIDs 一致（精确优先、
// 通配符前缀匹配），供调度（派发单 C）直接从存储读取。
func (s *VisionRoutingService) GetVisionRoutingAccountIDs(ctx context.Context, groupID int64, requestedModel string) ([]int64, error) {
	if s == nil || s.repo == nil {
		return nil, errors.New("vision routing service is not configured")
	}
	if requestedModel == "" {
		return nil, nil
	}
	routing, err := s.repo.Get(ctx, groupID)
	if err != nil {
		return nil, err
	}
	return lookupVisionRoutingModel(routing, requestedModel), nil
}

// validateVisionRoutingConfig 校验写入配置的有效性（终审 P2-5 / Vision-S4）：
//   - 每个模型模式（map key）不得为空字符串或纯空白；
//   - 每个模型的目标账号列表不得为空（长度为 0）；
//   - 每个目标账号 ID 必须为正整数（>0），≤0（含 0 与负数）判为非法配置直接拒绝。
//
// 空规则会被 lookupVisionRoutingModel 当作未命中，触发调度静默回落普通候选池；
// ≤0 的账号 ID 在运行时被 collectVisionRoutingTargetAccountIDs 排除会导致"配置成功
// 但永远无账号"，故两者都必须在写入侧拒绝，保证持久化配置恒为有效目标集合
// （目标集合 ⊆ 分组账号，且每个元素均为正 ID）。
func validateVisionRoutingConfig(routing map[string][]int64) error {
	for model, targets := range routing {
		if strings.TrimSpace(model) == "" {
			return fmt.Errorf("%w: model pattern must not be empty or whitespace", ErrVisionRoutingInvalidConfig)
		}
		if len(targets) == 0 {
			return fmt.Errorf("%w: target account list for model %q must not be empty", ErrVisionRoutingInvalidConfig, model)
		}
		for _, id := range targets {
			if id <= 0 {
				return fmt.Errorf("%w: target account id %d for model %q must be a positive integer", ErrVisionRoutingInvalidConfig, id, model)
			}
		}
	}
	return nil
}

// collectVisionRoutingTargetAccountIDs 汇总配置中所有模型引用的目标账号 ID（去重）。
// 注意：此处不再跳过 ≤0 的 ID（该非法值已由 validateVisionRoutingConfig 在写入
// 侧先行拒绝），保证同组校验覆盖配置中的"每一个"目标值，不遗漏任何元素
// （Vision-S4：同组校验不得跳过任何元素）。
func collectVisionRoutingTargetAccountIDs(routing map[string][]int64) []int64 {
	seen := make(map[int64]struct{})
	var ids []int64
	for _, modelTargets := range routing {
		for _, id := range modelTargets {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// lookupVisionRoutingModel 在视觉分流配置中查找请求模型的目标账号：精确匹配优先，
// 其次通配符前缀匹配（与 matchModelPattern 语义一致）。
func lookupVisionRoutingModel(routing map[string][]int64, requestedModel string) []int64 {
	if len(routing) == 0 || requestedModel == "" {
		return nil
	}
	if accountIDs, ok := routing[requestedModel]; ok && len(accountIDs) > 0 {
		return accountIDs
	}
	for pattern, accountIDs := range routing {
		if matchModelPattern(pattern, requestedModel) && len(accountIDs) > 0 {
			return accountIDs
		}
	}
	return nil
}
