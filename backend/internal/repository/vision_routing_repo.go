package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// visionRoutingRepository 以原生 SQL 读写 groups.vision_routing 列。
//
// 选型原因：vision_routing 是新增平行字段（docs/capability-routing-plan.md §3.7），
// 未纳入 ent 代码生成（避免重跑 generate 触碰 ent/ 生成文件，与派发单 B 对
// account_model_capabilities 的处理一致）。列由迁移 260 添加，这里直接以原生 SQL
// 读写 JSONB。
type visionRoutingRepository struct {
	db *sql.DB
}

// NewVisionRoutingRepository 创建视觉分流配置仓库。
func NewVisionRoutingRepository(db *sql.DB) service.VisionRoutingRepository {
	return &visionRoutingRepository{db: db}
}

// Get 返回指定分组的视觉分流配置；未配置或分组不存在返回空 map（nil）。
func (r *visionRoutingRepository) Get(ctx context.Context, groupID int64) (map[string][]int64, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil vision routing database")
	}
	var raw []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(vision_routing, '{}'::jsonb)::text
		FROM groups
		WHERE id = $1 AND deleted_at IS NULL
	`, groupID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrGroupNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query vision_routing for group %d: %w", groupID, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var routing map[string][]int64
	if err := json.Unmarshal(raw, &routing); err != nil {
		return nil, fmt.Errorf("decode vision_routing for group %d: %w", groupID, err)
	}
	if routing == nil {
		routing = map[string][]int64{}
	}
	return routing, nil
}

// GetByGroupIDs 批量返回指定分组的视觉分流配置，读同一列、同一解码语义，与 Get 等价：
// - 单条 SQL 一次查询（`WHERE id = ANY($1)`），应对 admin 列表读取链的批量水合；
// - 返回 map[groupID]routing；查询不到的组不在 map 中，调用方按空配置（空 map）处理；
// - groupIDs 为空时直接返回空 map，不发 SQL；
// - JSON 解码失败显式报错（含 groupID 上下文），不吞错。
func (r *visionRoutingRepository) GetByGroupIDs(ctx context.Context, groupIDs []int64) (map[int64]map[string][]int64, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil vision routing database")
	}
	result := make(map[int64]map[string][]int64, len(groupIDs))
	if len(groupIDs) == 0 {
		return result, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, COALESCE(vision_routing, '{}'::jsonb)::text
		FROM groups
		WHERE id = ANY($1) AND deleted_at IS NULL
	`, pq.Array(groupIDs))
	if err != nil {
		return nil, fmt.Errorf("query vision_routing for group ids %v: %w", groupIDs, err)
	}
	defer rows.Close()
	for rows.Next() {
		var groupID int64
		var raw []byte
		if err := rows.Scan(&groupID, &raw); err != nil {
			return nil, fmt.Errorf("scan vision_routing for group ids %v: %w", groupIDs, err)
		}
		var routing map[string][]int64
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &routing); err != nil {
				return nil, fmt.Errorf("decode vision_routing for group %d: %w", groupID, err)
			}
		}
		if routing == nil {
			routing = map[string][]int64{}
		}
		result[groupID] = routing
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vision_routing for group ids %v: %w", groupIDs, err)
	}
	return result, nil
}

// Set 覆盖写指定分组的视觉分流配置。routing 为 nil 时写入空对象。
// 调用方（service 层）须先完成同组校验；分组不存在返回 ErrGroupNotFound。
func (r *visionRoutingRepository) Set(ctx context.Context, groupID int64, routing map[string][]int64) error {
	if r == nil || r.db == nil {
		return errors.New("nil vision routing database")
	}
	if routing == nil {
		routing = map[string][]int64{}
	}
	raw, err := json.Marshal(routing)
	if err != nil {
		return fmt.Errorf("encode vision_routing for group %d: %w", groupID, err)
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE groups
		SET vision_routing = $1
		WHERE id = $2 AND deleted_at IS NULL
	`, raw, groupID)
	if err != nil {
		return fmt.Errorf("update vision_routing for group %d: %w", groupID, err)
	}
	affected, err := res.RowsAffected()
	if err == nil && affected == 0 {
		return service.ErrGroupNotFound
	}
	return nil
}
