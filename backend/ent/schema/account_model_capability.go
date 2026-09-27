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

// AccountModelCapability 是 (账号, 上游模型, 协议) 三元组上的上游能力标记。
//
// 背景（docs/capability-routing-plan.md §3.3）：同一账号在不同模型、同一模型在不同
// 协议下的能力并不相同（例如 infer 在 chat_completions 下不解析图片，返回 200+正常文本，
// 网关拿不到错误信号）。因此能力不能只记在账号级，必须细化到 (账号, 模型, 协议)。
//
// 存储取舍：独立表，不写入 accounts.credentials —— 避免与凭据密文混存、写放大与
// 并发冲突（方案 §3.3 / 外审重要1）。
//
// 记录语义：
//   - 命中记录 → 按 supports_vision 判定。
//   - 无记录（未标记）→ 按"未知"处理，读路径返回 known=false，由调用方保守放行。
//
// 不引入 TimeMixin/SoftDeleteMixin：本表不设 created_at/软删除，字段与 SQL 一一对应，
// CHECK 约束与部分索引在 migrations/259_account_model_capabilities.sql 中表达。
type AccountModelCapability struct {
	ent.Schema
}

// Annotations 返回 schema 的注解配置，指定数据库表名。
func (AccountModelCapability) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "account_model_capabilities"},
	}
}

// Fields 定义账号模型能力标记实体的所有字段。
func (AccountModelCapability) Fields() []ent.Field {
	return []ent.Field{
		// account_id: 账号主键（对应 accounts.id）。不建 ent edge，
		// 避免生成外键列导致与 SQL DDL 漂移；引用完整性由迁移中的外键保证。
		field.Int64("account_id"),

		// upstream_model: 上游模型名（发送��上游的原始模型标识）
		field.String("upstream_model").
			MaxLen(200).
			NotEmpty(),

		// protocol: 协议维度，chat_completions / responses / anthropic。
		// 该维度不可省：否则会复现跨协议污染（同一模型在一种协议下支持图片、
		// 另一种协议下不支持）。
		field.String("protocol").
			MaxLen(50).
			NotEmpty(),

		// supports_vision: 是否支持视觉（读图）
		field.Bool("supports_vision").
			Default(false),

		// source: 来源可追溯，detect（管理员一键检测落库）/ manual（人工覆盖）
		field.String("source").
			MaxLen(20).
			NotEmpty().
			Default("detect"),

		// detected_at: 最近一次检测时间（可空：纯人工覆盖无检测动作）
		field.Time("detected_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),

		// updated_at: 更新时间
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

// Edges 返回空：本表不声明 ent edge，关系由外键与查询显式表达。
func (AccountModelCapability) Edges() []ent.Edge {
	return nil
}

// Indexes 定义唯一约束与查询索引。
func (AccountModelCapability) Indexes() []ent.Index {
	return []ent.Index{
		// 唯一键：(account_id, upstream_model, protocol)
		// 三元组唯一：同一账号同一模型同一协议只保留一条标记，Upsert 依此收敛。
		index.Fields("account_id", "upstream_model", "protocol").Unique(),
		// ListByAccount 热路径：按账号取全量标记
		index.Fields("account_id"),
	}
}
