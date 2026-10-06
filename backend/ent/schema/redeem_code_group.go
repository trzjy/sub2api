package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// RedeemCodeGroup 福利兑换卡的分组勾选快照：一卡多分组，每组独立有效天数
// （天卡=1 / 周卡=7 / 月卡=30）。仅 welfare 类型兑换码使用。
type RedeemCodeGroup struct {
	ent.Schema
}

func (RedeemCodeGroup) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "redeem_code_groups"},
	}
}

func (RedeemCodeGroup) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("redeem_code_id"),
		field.Int64("group_id"),
		field.Int("validity_days").
			Default(30),
	}
}

func (RedeemCodeGroup) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("redeem_code", RedeemCode.Type).
			Ref("code_groups").
			Field("redeem_code_id").
			Unique().
			Required(),
		edge.From("group", Group.Type).
			Ref("redeem_code_groups").
			Field("group_id").
			Unique().
			Required(),
	}
}
