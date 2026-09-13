package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

// PromoIntelSource holds the schema definition for a promo-intel source:
// 一个厂商官方公告/更新日志/价格/活动页的轮询配置。
type PromoIntelSource struct {
	ent.Schema
}

func (PromoIntelSource) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "promo_intel_sources"},
	}
}

func (PromoIntelSource) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (PromoIntelSource) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty().MaxLen(100),
		// 厂商标识（deepseek/kimi/zhipu/minimax/volcano/alibaba/tencent/baidu/...），
		// 允许自定义新厂商，单一权威校验在 service 层。
		field.String("vendor").Default("other").MaxLen(50),
		// announcement | changelog | pricing | activity | blog
		field.String("category").Default("announcement").MaxLen(32),
		field.String("url").Default("").MaxLen(2048),
		field.Int("fetch_interval_minutes").Default(1440).Min(30).Max(60 * 24 * 30),
		field.Bool("enabled").Default(true),
		// false = 只入库原文，不调 LLM（用于噪音源或人工源）。
		field.Bool("llm_extract").Default(true),
		field.Text("notes").Default(""),
		field.Time("last_fetched_at").Optional().Nillable(),
		// 上次成功进入整理层的内容指纹；空 = 尚无成功整理记录（LLM 可用后自动补跑）。
		field.String("last_extracted_hash").Default("").MaxLen(64),
		field.String("last_status").Default("").MaxLen(16),
		field.Text("last_error").Default(""),
		field.Int64("created_by").Default(0),
	}
}

func (PromoIntelSource) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("items", PromoIntelItem.Type).
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}

func (PromoIntelSource) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("name").Unique(),
		index.Fields("enabled", "last_fetched_at"),
	}
}
