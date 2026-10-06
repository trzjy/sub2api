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
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

// PromoIntelItem holds the schema definition for a promo-intel item:
// 一条从资讯源整理出的优惠情报（LLM 结构化结果或降级原文）。
type PromoIntelItem struct {
	ent.Schema
}

func (PromoIntelItem) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "promo_intel_items"},
	}
}

func (PromoIntelItem) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (PromoIntelItem) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("source_id"),
		field.String("vendor").Default("other").MaxLen(50),
		// free_quota | discount | subscription | price_change | new_model |
		// new_product | event | policy | other
		field.String("category").Default("other").MaxLen(32),
		field.Text("title").Default(""),
		field.Text("summary").Default(""),
		field.Text("details").Default(""),
		field.Text("discount_info").Default(""),
		// LLM 提取的有效期（自由文本，如 "2026-09-30" / "长期"）。
		field.Text("valid_until").Default(""),
		field.Text("url").Default(""),
		// high | medium | low | none
		field.String("relevance").Default("medium").MaxLen(16),
		// pending（待阅） | useful（有用） | ignored（忽略）
		field.String("status").Default("pending").MaxLen(16),
		// 去重指纹：LLM 条目 = hash(vendor+归一化标题)；降级条目 = hash(源+内容版本)。
		field.String("fingerprint").NotEmpty().MaxLen(64),
		field.Text("raw_excerpt").Default(""),
		// llm = 已结构化整理；pending = 原文待整理（降级态，LLM 可用后自动补跑）。
		field.String("extract_status").Default("llm").MaxLen(16),
		// 首次发现日期（每日简报按此聚合）。
		field.Time("digest_date").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "date"}),
		field.Time("source_fetched_at").Default(time.Now),
	}
}

func (PromoIntelItem) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("source", PromoIntelSource.Type).
			Ref("items").
			Field("source_id").
			Unique().
			Required(),
	}
}

func (PromoIntelItem) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("fingerprint").Unique(),
		index.Fields("status", "digest_date"),
		index.Fields("vendor"),
		index.Fields("source_id"),
	}
}
