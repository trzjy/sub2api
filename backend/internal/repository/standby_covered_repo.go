package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// 主力渠道延迟/故障自适应备用切换 · 覆盖组合只读查询（第 2a 片）。
//
// 覆盖范围（方案 L12）：绑定在主力分组里的 codebuddy 账号，取其
// credentials.model_mapping 的键作为信号键（模型名天然标识供给平台）。
// composite 分组（110/118）同样走这条路：模型归属由模型名决定，不依赖
// composite_model_routes（实测全表仅分组 94 有 1 条路由）。
//
// 只读、不新增表、不新增埋点。credentials 是 JSONB，model_mapping 不是敏感子键
// （见 SensitiveCredentialKeys），因此无需解密即可读出。

// standbyCoveredGroupIDs 是 L12 锁定的主力分组：deepseek 49/57/115、
// kimi 52/61/117、glm 43/116、聚合 110/118。
var standbyCoveredGroupIDs = []int64{49, 57, 115, 52, 61, 117, 43, 116, 110, 118}

// standbyCoveredModelsSQL 取覆盖分组内 codebuddy 账号 model_mapping 的全部键。
//
//   - platform = codebuddy：只有备用账号的映射键代表"可能用到的模型"；
//   - deleted_at IS NULL：软删账号不参与；
//   - jsonb_typeof = 'object'：防御历史行把 model_mapping 存成 JSON 字符串；
//   - DISTINCT：同一模型被多账号/多分组重复覆盖只评估一次。
const standbyCoveredModelsSQL = `
SELECT DISTINCT m.key
FROM accounts a
JOIN account_groups ag ON ag.account_id = a.id
CROSS JOIN LATERAL jsonb_object_keys(a.credentials->'model_mapping') AS m(key)
WHERE a.deleted_at IS NULL
  AND a.platform = $1
  AND ag.group_id = ANY($2)
  AND jsonb_typeof(a.credentials->'model_mapping') = 'object'
ORDER BY 1
`

type standbyCoveredRepository struct {
	db       *sql.DB
	groupIDs []int64
}

// NewStandbyCoveredRepository 返回覆盖组合数据源（只读）。本片不接线，
// 由后续启动装配注入控制器。
func NewStandbyCoveredRepository(db *sql.DB) service.StandbyCoveredModelSource {
	return &standbyCoveredRepository{db: db, groupIDs: standbyCoveredGroupIDs}
}

func (r *standbyCoveredRepository) ListCoveredModels(ctx context.Context) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil standby covered repository")
	}
	rows, err := r.db.QueryContext(ctx, standbyCoveredModelsSQL, "codebuddy", pq.Array(r.groupIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	models := make([]string, 0, 16)
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return models, nil
}
