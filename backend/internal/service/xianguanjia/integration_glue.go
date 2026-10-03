package xianguanjia

import (
	"context"
	"fmt"
	"log/slog"
)

// D4i: D4 四单元集成胶水。
//
// 背景：D4a 交付 KindService、D4b 交付 PoolSyncService（二者构造时需要 *Client），
// 但生产环境 client 必须按 252 表 active 配置动态构造（ClientFactory，凭证不落盘
// 不缓存，配置变更即时生效）。本文件提供「惰性 client」包装：每次调用时
// factory.NewClient(ctx) 现场构造 client，再委托给 D4a/D4b 服务；无 active 配置时
// 返回 ErrNoActiveConfig（fail-closed，与 D2 出站方向一致）。
//
// 同时提供 KindIDStore 的 settings 仓储适配器：KindService 约定的窄接口
// GetValue/Set 与 infra 层 service.SettingRepository 方法签名天然一致，直接
// 组合适配（不改 repository 包）。marker 侧无需新适配器——D4b 的
// NewPushedCardMarker(db)（unused→delivered 原子 UPDATE，WHERE 限定
// status='unused' 防超发）即是 PushedCardMarker 的生产实现，wire 直接注入。

// SettingsKindIDStore 把 infra 层 SettingRepository（GetValue/Set，见
// backend/internal/repository/setting_repo.go:36/44，Set 为 upsert 语义）适配为
// KindIDStore。setRepo 为 nil 时方法 fail-closed 返回错误。
type SettingsKindIDStore struct {
	setRepo interface {
		GetValue(ctx context.Context, key string) (string, error)
		Set(ctx context.Context, key, value string) error
	}
}

// NewSettingsKindIDStore 构造 settings 适配器。repo 传 service.SettingRepository
// 的仓储实现（wire_gen 已有 settingRepository 实例）。
func NewSettingsKindIDStore(repo interface {
	GetValue(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
}) *SettingsKindIDStore {
	return &SettingsKindIDStore{setRepo: repo}
}

// GetValue 透传 settings 读取。
func (s *SettingsKindIDStore) GetValue(ctx context.Context, key string) (string, error) {
	if s == nil || s.setRepo == nil {
		return "", fmt.Errorf("xianguanjia settings kind store unavailable")
	}
	return s.setRepo.GetValue(ctx, key)
}

// Set 透传 settings 写入（底层 upsert：key UNIQUE 冲突时覆盖更新）。
func (s *SettingsKindIDStore) Set(ctx context.Context, key, value string) error {
	if s == nil || s.setRepo == nil {
		return fmt.Errorf("xianguanjia settings kind store unavailable")
	}
	return s.setRepo.Set(ctx, key, value)
}

// LazyKindService 惰性卡种服务：实现 admin.XianguanjiaKindService 所需的
// KindCreate/KindCurrent 语义（签名为基本类型，不依赖 admin 包，隐式满足）。
// 每次调用现场构造 client，委托 D4a KindService。
type LazyKindService struct {
	factory *ClientFactory
	store   KindIDStore
}

// NewLazyKindService 构造惰性卡种服务。factory/store 均必填（fail-closed）。
func NewLazyKindService(factory *ClientFactory, store KindIDStore) *LazyKindService {
	return &LazyKindService{factory: factory, store: store}
}

// KindCreate 现场构造 client 后委托 KindService.KindCreate。
func (s *LazyKindService) KindCreate(ctx context.Context, name string, categoryID int64) (int64, error) {
	if s == nil || s.factory == nil {
		return 0, ErrNoActiveConfig
	}
	client, err := s.factory.NewClient(ctx)
	if err != nil {
		return 0, err
	}
	return NewKindService(client, s.store).KindCreate(ctx, name, categoryID)
}

// KindCurrent 读取 settings 中已保存的 kind_id（不出站）。未配置时返回 0
// （对齐 admin 端点契约：GET /pool/kind 未设置返回 kind_id=0），仅当 store
// 缺失或读取失败时上抛错误。
func (s *LazyKindService) KindCurrent(ctx context.Context) (int64, error) {
	if s == nil || s.store == nil {
		return 0, fmt.Errorf("xianguanjia kind current: store not configured")
	}
	kindID, err := NewKindService(nil, s.store).GetKindID(ctx)
	if err != nil {
		// 未配置（ErrKindIDNotConfigured）按端点契约归一为 (0, nil)。
		if IsErrKindIDNotConfigured(err) {
			return 0, nil
		}
		return 0, err
	}
	return kindID, nil
}

// IsErrKindIDNotConfigured 判断 err 是否由 ErrKindIDNotConfigured 包装而来。
func IsErrKindIDNotConfigured(err error) bool {
	if err == nil {
		return false
	}
	for e := err; e != nil; {
		if e == ErrKindIDNotConfigured {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// LazyPoolSyncService 惰性推仓服务：每次调用现场构造 client，委托 D4b
// PoolSyncService.PushCards。marker 复用 D4b NewPushedCardMarker(db)。
type LazyPoolSyncService struct {
	factory *ClientFactory
	marker  PushedCardMarker
}

// NewLazyPoolSyncService 构造惰性推仓服务。factory/marker 均必填（fail-closed，
// 无标记能力的推仓会留下双卖窗口）。
func NewLazyPoolSyncService(factory *ClientFactory, marker PushedCardMarker) *LazyPoolSyncService {
	return &LazyPoolSyncService{factory: factory, marker: marker}
}

// PushCards 现场构造 client 后委托 PoolSyncService.PushCards。
func (s *LazyPoolSyncService) PushCards(ctx context.Context, kindID int64, cards []CardPair) (PushResult, error) {
	if s == nil || s.factory == nil {
		return PushResult{}, ErrNoActiveConfig
	}
	client, err := s.factory.NewClient(ctx)
	if err != nil {
		return PushResult{}, err
	}
	res, err := NewPoolSyncService(client, s.marker).PushCards(ctx, kindID, cards)
	if err != nil {
		slog.Warn("xianguanjia lazy pool push failed", "kind_id", kindID, "err", err)
	}
	return res, err
}
