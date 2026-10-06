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

// UsageRiskRun 是异常调用分析的运行覆盖台账。
// 主键为 run_id（identity，自增），Go 字段名 id 经 StorageKey 落库为 run_id。
// 不引入 TimeMixin/SoftDeleteMixin，不定义 edges（避免生成外键列），与 SQL 字段一一对应。
// CHECK 约束与部分索引在 migrations/257_usage_risk_analysis.sql 中表达。
type UsageRiskRun struct {
	ent.Schema
}

func (UsageRiskRun) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "usage_risk_runs"},
	}
}

func (UsageRiskRun) Fields() []ent.Field {
	return []ent.Field{
		// 主键（identity 自增），列名 run_id。
		field.Int64("id").
			StorageKey("run_id").
			Unique(),
		field.Time("run_at").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("window_start").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("window_end").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Int("candidates_total").Default(0),
		field.Int("batches_done").Default(0),
		field.Int("batches_failed").Default(0),
		field.JSON("failed_batches", json.RawMessage{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Bool("budget_exhausted").Default(false),
		field.Time("recon_cursor_date").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "date"}),
		field.Int("recon_batch_offset").
			Optional().
			Nillable().
			Default(0),
		field.Int("recon_batches_done").Default(0),
		field.Int("consecutive_partials").Default(0),
		field.Int64("policy_version").Default(0),
		field.Bool("r1_reeval_pending").Default(false),
		field.JSON("policy_snapshot", json.RawMessage{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Bool("history_covered").Default(false),
		field.Time("finished_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("failure_stage").
			MaxLen(30).
			Optional().
			Nillable(),
		field.String("status").
			MaxLen(20).
			NotEmpty().
			Default("running"),
	}
}

func (UsageRiskRun) Edges() []ent.Edge {
	return nil
}

func (UsageRiskRun) Indexes() []ent.Index {
	return []ent.Index{
		// run_at 唯一键
		index.Fields("run_at").Unique(),
		// 状态检索索引：(status)
		index.Fields("status"),
	}
}
