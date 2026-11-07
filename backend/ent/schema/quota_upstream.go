package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

// QuotaUpstream 定义 cursor 号池上游注册表的 schema（CARD-KC-16）。
//
// 背景：cursor 平台已有一个接通的号池上游 ShareAI（激活码 → 拉号 → cursor oauth 账号），
// 现在要接第二个上游 kn-cy.com（第三方 OpenAI 兼容网关）。用户裁定（R2/R3）：
//   - 不同上游要区分显示，带上上游名称和标识；
//   - 上游的名称也要可编辑；
//   - 粒度 = 全局上游条目（一个上游 = 一个注册条目，改一次名称对该上游下所有卡密生效）。
//
// 因此每张上游注册表条目包含：
//   - key：上游稳定标识（不可变，是 quota_keys.upstream_key 的引用目标）；
//   - name：可编辑显示名（默认由 key 派生，admin 可改）；
//   - type：上游类型，决定用哪个拉号客户端实现（shareai / kn_cy）；
//   - renewal_url：续费链接（kn-cy 有公开商铺，ShareAI 无 → 空）。
//
// 软删：照抄 quota_key.go 的 mixins.SoftDeleteMixin（deleted_at + 查询自动过滤）。
// 索引：照抄 quota_key.go —— key 唯一索引（引用目标不可重复）+ type 单索引（按类型查）。
//
// ent 生成物（backend/ent/**）是 source of truth，本文件与迁移 297 字段一一对应。
type QuotaUpstream struct {
	ent.Schema
}

// Mixin 引入软删除能力：deleted_at 时间戳 + 查询自动过滤 + 删除转换为 UPDATE。
// 与 quota_keys / accounts 表使用同一 SoftDeleteMixin，保证"删除 = 软删除"口径一致。
func (QuotaUpstream) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.SoftDeleteMixin{},
	}
}

// Annotations 返回 schema 的注解配置。指定数据库表名为 "quota_upstreams"。
func (QuotaUpstream) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "quota_upstreams"},
	}
}

// Fields 定义上游注册表实体的所有字段。
func (QuotaUpstream) Fields() []ent.Field {
	return []ent.Field{
		// key：上游稳定标识（不可变，引用目标）。
		//
		// 决定 quota_keys 用哪个上游拉号 + 管理端按上游区分显示；标识一旦写入即不可改，
		// 改标识会破坏 quota_keys.upstream_key 的外键语义。故本列不可编辑。
		field.String("key").
			MaxLen(64).
			NotEmpty(),
		// name：上游显示名（可编辑，用户裁定 R3）。默认由 key 派生，admin 可改。
		field.String("name").
			MaxLen(100),
		// type：上游类型，决定用哪个拉号客户端实现（shareai / kn_cy）。
		// 取值集合由迁移 297 的 CHECK 约束限定。
		field.String("type").
			MaxLen(32),
		// renewal_url：续费链接（可选）。kn-cy 有公开商铺，ShareAI 无 → 空。
		field.String("renewal_url").
			Optional().
			Nillable().
			MaxLen(500),
		// notes：admin 备注（可选）。
		field.String("notes").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		// created_at：创建时间（immutable）。
		field.Time("created_at").
			Default(time.Now).
			Immutable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		// updated_at：更新时间（ent 自动维护）。
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

// Indexes 定义数据库索引（照抄 quota_key.go 风格）：
//   - key 唯一索引（引用目标不可重复）；
//   - type 单索引（便于按上游类型查，决定拉号客户端）。
func (QuotaUpstream) Indexes() []ent.Index {
	return []ent.Index{
		// key 唯一索引（上游稳定标识，引用目标）
		index.Fields("key").
			Unique().
			StorageKey("uq_quota_upstreams_key"),
		// type 普通索引（按上游类型查）
		index.Fields("type").
			StorageKey("idx_quota_upstreams_type"),
	}
}
