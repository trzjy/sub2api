package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// RedeemBatch 福利兑换卡批次元数据。
// config 保存生成参数快照（分组勾选、金额区间、数量），供事后复盘与统计。
type RedeemBatch struct {
	ent.Schema
}

func (RedeemBatch) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "redeem_batches"},
	}
}

func (RedeemBatch) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").
			Default("").
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.JSON("config", map[string]any{}).
			Optional(),
		field.Int("code_count").
			Default(0),
		field.Int64("created_by").
			Default(0),
		field.Time("created_at").
			Immutable().
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (RedeemBatch) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("redeem_codes", RedeemCode.Type),
		edge.To("welfare_balances", WelfareBalance.Type),
	}
}

func (RedeemBatch) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at"),
	}
}
