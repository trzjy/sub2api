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

// WelfareBalance 福利余额池：一张福利卡一行，批次间互不影响、独立计时清零。
// 扣减优先于 users.balance（按 expires_at 最近优先）；到期由清零任务置
// status='expired' 且 amount_remaining=0。本表与 users.balance 完全隔离，
// 任何清零/扣减逻辑不得触碰 users.balance。
type WelfareBalance struct {
	ent.Schema
}

func (WelfareBalance) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "welfare_balances"},
	}
}

func (WelfareBalance) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("user_id"),
		field.Int64("redeem_code_id"),
		field.Int64("batch_id"),
		field.Float("amount_initial").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,8)"}).
			Default(0),
		field.Float("amount_remaining").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,8)"}).
			Default(0),
		field.String("status").
			MaxLen(20).
			Default("active"),
		field.Time("expires_at").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("created_at").
			Immutable().
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (WelfareBalance) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("welfare_balances").
			Field("user_id").
			Unique().
			Required(),
		edge.From("redeem_code", RedeemCode.Type).
			Ref("welfare_balances").
			Field("redeem_code_id").
			Unique().
			Required(),
		edge.From("batch", RedeemBatch.Type).
			Ref("welfare_balances").
			Field("batch_id").
			Unique().
			Required(),
	}
}

func (WelfareBalance) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "status", "expires_at"),
		index.Fields("status", "expires_at"),
	}
}
