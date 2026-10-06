package schema

import (
	"encoding/json"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// UsageRiskReport 是 user+group × 日风险报告（应用时区日界）。
// 主键为 report_id（identity，自增），Go 字段名 id 经 StorageKey 落库为 report_id。
// 不引入 TimeMixin/SoftDeleteMixin，不定义 edges（避免生成外键列），与 SQL 字段一一对应。
// CHECK 约束与部分索引在 migrations/257_usage_risk_analysis.sql 中表达。
type UsageRiskReport struct {
	ent.Schema
}

func (UsageRiskReport) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "usage_risk_reports"},
	}
}

func (UsageRiskReport) Fields() []ent.Field {
	return []ent.Field{
		// 主键（identity 自增），列名 report_id。
		field.Int64("id").
			StorageKey("report_id").
			Unique(),
		field.Int64("user_id"),
		field.Int64("group_id"),
		field.Time("report_date").
			SchemaType(map[string]string{dialect.Postgres: "date"}),
		field.Int64("policy_version").Default(0),
		field.Int("score").Default(0),
		field.String("level").
			MaxLen(20).
			NotEmpty(),
		field.JSON("rule_hits", json.RawMessage{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.JSON("evidence", json.RawMessage{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.String("status").
			MaxLen(20).
			NotEmpty().
			Default("open"),
		field.Time("invalidated_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Int64("status_updated_by").
			Optional().
			Nillable(),
		field.Time("status_updated_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (UsageRiskReport) Edges() []ent.Edge {
	return nil
}

func (UsageRiskReport) Indexes() []ent.Index {
	return []ent.Index{
		// 唯一键：(user_id, group_id, report_date)
		index.Fields("user_id", "group_id", "report_date").Unique(),
		// 按用户维度检索：(user_id, report_date)。（report_date, score）与 Dashboard 聚合为部分索引，仅存于 SQL。
		index.Fields("user_id", "report_date"),
	}
}
