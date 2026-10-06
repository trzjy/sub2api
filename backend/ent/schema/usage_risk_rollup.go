package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// UsageRiskRollup 是 user+group × 小时事实桶（UTC instant 存储，不跨本地日界）。
// id 代理主键（BIGSERIAL）+ 唯一键 (user_id, group_id, bucket_hour)；因 ent v0.14.5
// 不支持含 Time 列的复合主键，改用代理主键 + 唯一约束。不引入 TimeMixin/SoftDeleteMixin。
// 字段集合、列类型、唯一键与 migrations/257_usage_risk_analysis.sql 逐一对应。
type UsageRiskRollup struct {
	ent.Schema
}

func (UsageRiskRollup) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "user_usage_metrics_rollup"},
	}
}

func (UsageRiskRollup) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("user_id"),
		field.Int64("group_id"),
		field.Time("bucket_hour").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Int("request_count").Default(0),
		field.Int64("input_tokens_sum").Default(0),
		field.Int64("output_tokens_sum").Default(0),
		field.Int64("cache_read_tokens_sum").Default(0),
		field.Float("cost_usd_sum").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,10)"}),
		field.Int64("occupied_ms_sum").Default(0),
		field.Int("non_whitelisted_ua_count").Default(0),
		field.Time("computed_at").
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (UsageRiskRollup) Edges() []ent.Edge {
	return nil
}

func (UsageRiskRollup) Indexes() []ent.Index {
	return []ent.Index{
		// 清理用索引：(bucket_hour)。部分索引/唯一键在 SQL 迁移中表达。
		index.Fields("bucket_hour"),
		// 方案 §6.1 唯一键：(user_id, group_id, bucket_hour)。
		index.Fields("user_id", "group_id", "bucket_hour").Unique(),
	}
}
