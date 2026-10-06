package xianguanjia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// pathKindCreate 是卡种创建的官方路径（POST /api/open/kami/kind/create）。
// 注意：派发单指定该路径；reference/open-platform 文档不在本 worktree，字段名假设见 KindCreate 注释。
const pathKindCreate = "/api/open/kami/kind/create"

// SettingKeyKindID 是 kind_id 在 settings 表中的存储 key。
//
// 存储选型说明（派发单要求写清）：复用既有 settings 通用 KV 表
// （migrations/005_schema_parity.sql，key UNIQUE + upsert），通过下方窄接口
// KindIDStore 注入；backend/internal/repository/setting_repo.go 的
// NewSettingRepository 返回值天然满足该接口，wire 层直接注入即可。
// kind_id 是单值配置而非业务实体，为其新增 268 迁移建小表属于过度设计，故不新增迁移。
const SettingKeyKindID = "xianguanjia_kind_id"

// ErrKindIDNotConfigured 表示 settings 中尚未保存 kind_id。
var ErrKindIDNotConfigured = errors.New("xianguanjia kind id not configured")

// KindIDStore 是 kind_id 存取所需的最小 KV 能力（窄接口，便于单测与替换实现）。
// service.SettingRepository（GetValue/Set）直接满足本接口。
type KindIDStore interface {
	GetValue(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
}

// KindService 封装卡种管理：kind/create 出站调用与 kind_id 本地存取。
type KindService struct {
	client *Client
	store  KindIDStore
}

// NewKindService 构造卡种服务。client 由 ClientFactory 按 active 配置构造；
// store 传入 settings 仓储（见 SettingKeyKindID 注释），可为 nil（仅存取功能不可用）。
func NewKindService(client *Client, store KindIDStore) *KindService {
	return &KindService{client: client, store: store}
}

// KindCreate 创建卡种并返回 kind_id。
//
// 字段名假设（本 worktree 无 reference 文档，无法坐实，按派发单要求写清）：
//   - 请求体假设为 {"name": <卡种名>, "category_id": <分类ID>}；
//   - 响应信封 data 假设为 {"kind_id": <数值或数值字符串>}，同时容忍 data 为裸数值。
//
// data 以 json.RawMessage 惰性解析，不做强类型绑定，真实联调时如字段名不符只需改 parseKindID。
func (s *KindService) KindCreate(ctx context.Context, name string, categoryID int64) (int64, error) {
	if s == nil || s.client == nil {
		return 0, fmt.Errorf("xianguanjia kind create: client not configured")
	}
	if name == "" {
		return 0, fmt.Errorf("xianguanjia kind create: name is required")
	}
	body, err := marshalBody(map[string]any{"name": name, "category_id": categoryID})
	if err != nil {
		return 0, fmt.Errorf("xianguanjia marshal kind create: %w", err)
	}
	raw, err := s.client.do(ctx, pathKindCreate, body)
	if err != nil {
		return 0, err
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return 0, err
	}
	kindID, err := parseKindID(env.Data)
	if err != nil {
		return 0, fmt.Errorf("xianguanjia kind create: %w", err)
	}
	return kindID, nil
}

// parseKindID 从响应 data 中提取 kind_id，兼容三种形态：
// 1) {"kind_id": 123}；2) {"kind_id": "123"}；3) 裸数值 123（防御性兜底）。
func parseKindID(data json.RawMessage) (int64, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("empty data, kind_id missing")
	}
	var obj struct {
		KindID json.RawMessage `json:"kind_id"`
	}
	if err := json.Unmarshal(data, &obj); err == nil && len(obj.KindID) > 0 {
		return rawToInt64(obj.KindID)
	}
	// 兜底：data 本身是裸数值。
	return rawToInt64(data)
}

// rawToInt64 把 JSON 数值或数值字符串解析为 int64。
func rawToInt64(raw json.RawMessage) (int64, error) {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("invalid kind_id %d", n)
		}
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		n, perr := strconv.ParseInt(s, 10, 64)
		if perr == nil && n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("cannot parse kind_id from %s", string(raw))
}

// SaveKindID 把 kind_id 写入 settings（key=xianguanjia_kind_id，覆盖写）。
func (s *KindService) SaveKindID(ctx context.Context, kindID int64) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("xianguanjia save kind id: store not configured")
	}
	if kindID <= 0 {
		return fmt.Errorf("xianguanjia save kind id: invalid kind id %d", kindID)
	}
	if err := s.store.Set(ctx, SettingKeyKindID, strconv.FormatInt(kindID, 10)); err != nil {
		return fmt.Errorf("xianguanjia save kind id: %w", err)
	}
	return nil
}

// GetKindID 从 settings 读回 kind_id；未配置或值非法时返回错误（fail-closed）。
func (s *KindService) GetKindID(ctx context.Context) (int64, error) {
	if s == nil || s.store == nil {
		return 0, fmt.Errorf("xianguanjia get kind id: store not configured")
	}
	v, err := s.store.GetValue(ctx, SettingKeyKindID)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrKindIDNotConfigured, err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: invalid value %q", ErrKindIDNotConfigured, v)
	}
	return n, nil
}
