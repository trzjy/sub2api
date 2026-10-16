package schema

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

// QuotaKey 定义 Cursor 激活码（quota key）实体的 schema。
//
// 激活码是 v3.1/v3.3 引入的新主体：平台先添加激活码，由激活码去上游
// （shareai）拉取 cursor 官方账号，账号强制归属激活码。多个激活码相互独立
// （独立计数器、独立拉号、独立倒计时、独立 exhausted 标记）。
//
// 凭据安全（v3.3 §4.1 / F1 消化，F1 封装要求已驳回）：
//   - code_encrypted：激活码原文经既有 Account.credentials 加密链加密后的密文，
//     原文绝不落库明文。
//   - code_mac：原文的 HMAC-SHA256 指纹（hex），用于 SQL 级查重/索引（密文随机
//     nonce 后无法靠密文查重，依赖 MAC；与 Account.credentials_mac 同型）。
//   - code_last4：原文末 4 位明文，仅用于 admin 端 UI 脱敏显示
//     （"XXXX-****-****-1234"），不与 MAC 冲突。
//   - C5 DTO 永不返回原文，仅返回 code_masked；日志/metrics 脱敏。
//
// 删除策略：软删除（D-CURSOR-014 裁定，v3.4 §3.5 删除唯一路径 = 软删口径）。
// 账号"删除" = 号池摘除 + 前端不显示 + 停止调度；数据库账号行与全部历史记录保留，
// 禁止物理 DELETE 账号行。quota_keys 与 accounts 均通过 SoftDeleteMixin 的
// deleted_at 实现软删；号池/列表查询由 mixin 的拦截器自动排除 deleted_at 非 NULL 的行。
type QuotaKey struct {
	ent.Schema
}

// Mixin 引入软删除能力：deleted_at 时间戳 + 查询自动过滤 + 删除转换为 UPDATE。
// 与 accounts 表使用同一 SoftDeleteMixin，保证"删除 = 软删除"口径一致。
func (QuotaKey) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.SoftDeleteMixin{},
	}
}

// Annotations 返回 schema 的注解配置。指定数据库表名为 "quota_keys"。
func (QuotaKey) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "quota_keys"},
	}
}

// Fields 定义激活码实体的所有字段（v3.3 §4.1）。
func (QuotaKey) Fields() []ent.Field {
	return []ent.Field{
		// code_encrypted：激活码原文密文（F1）。原文绝不落库明文；
		// 与 Account.credentials 复用同一既有加密能力。
		field.String("code_encrypted").
			MaxLen(2048).
			NotEmpty(),
		// code_mac：原文的 HMAC-SHA256 指纹（hex），用于查重/索引（F1）。
		// 与 Account.credentials_mac 同型；密文随机 nonce 后 SQL 级查重依赖 MAC。
		field.String("code_mac").
			Optional().
			MaxLen(64),
		// code_last4：原文末 4 位明文，仅用于 admin 端 UI 脱敏显示（F1）。
		field.String("code_last4").
			Optional().
			MaxLen(8),
		// status："active" / "exhausted" / "expired" / "disabled"。
		field.String("status").
			MaxLen(20).
			Default(domain.QuotaKeyStatusActive),
		// exhausted：拉号占位标记（上游任意错误 = true，可手动解除）。
		field.Bool("exhausted").
			Default(false),
		// last_refresh_error：拉号失败占位（v3.3 §3.4 CAS 写）；仅记录错误码/消息，
		// 不记录上游响应体。
		field.String("last_refresh_error").
			Optional().
			Nillable().
			MaxLen(2000),
		// last_refresh_at：最后拉号时间（不限成功失败）。
		field.Time("last_refresh_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		// expires_at：激活码到期时间；< now 标 expired。
		field.Time("expires_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		// device_id：cursor 出站鉴权头 x-jg-auth 中 `d` 字段的值（B-10）。
		//
		// 与 expires_at 上方的 deviceID(code) 派生**不是同一个东西**，两者用途不可混用：
		//   - deviceID(code)：取号链 POST /api/refresh 的 `device-id` 请求头，按激活码派生；
		//   - device_id：本列，出站 x-jg-auth 的 `d`。插件是 uuidv4() 随机生成后固化到
		//     ~/.cursor_free_data（deobfuscated.js:8131-8150 getDeviceId），与激活码无关。
		//
		// 用户裁定（2026-10-16）：站点随机生成并落库，每个激活码一份，首次需要时生成、
		// 之后永久复用——对齐插件语义，不依赖本机插件产物。故绝不可由激活码派生。
		// NULL = 尚未生成（首次使用时生成并回写）。
		field.String("device_id").
			Optional().
			Nillable().
			MaxLen(8),
		// created_at：创建时间（immutable）。
		field.Time("created_at").
			Default(time.Now).
			Immutable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		// updated_at：更新时间（ent 自动维护）。时间戳默认值由 ent 自动维护，
		// 无需额外 SQL。
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		// notes：admin 备注。
		field.String("notes").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
	}
}

// Edges 定义激活码实体的关联（v3.3 §4.1）：1:N 账号池。
// 反向边（accounts -> quota_key）由 C2 在 account.go 中以 edge.From 接入，
// 不在 C1 范围（禁区：不动 account.go）。
func (QuotaKey) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("accounts", Account.Type), // 1:N 账号池
	}
}

// Indexes 定义数据库索引（v3.3 §4.1 索引清单）。
// - code_mac 唯一索引（查重，密文随机 nonce 后 SQL 级查重依赖 MAC）
// - status 单索引（sweep + reconciler 周期扫表）
// - expires_at 部分索引（sweep_expired 只查 expires_at IS NOT NULL）
// - exhausted 部分索引（拉号只关心 exhausted = true 的）
// - (status, exhausted, expires_at) 复合部分索引（reconciler §3.4 周期扫表热路径）
func (QuotaKey) Indexes() []ent.Index {
	return []ent.Index{
		// code_mac 唯一索引（NULL 不参与唯一约束，避免多 NULL 互相冲突）
		index.Fields("code_mac").
			Unique().
			StorageKey("uq_quota_keys_code_mac"),
		// status 普通索引
		index.Fields("status").
			StorageKey("idx_quota_keys_status"),
		// expires_at 部分索引（仅非 NULL）
		index.Fields("expires_at").
			StorageKey("idx_quota_keys_expires_at").
			Annotations(entsql.IndexWhere("expires_at IS NOT NULL")),
		// exhausted 部分索引（仅 true）
		index.Fields("exhausted").
			StorageKey("idx_quota_keys_exhausted").
			Annotations(entsql.IndexWhere("exhausted = true")),
		// reconciler 复合部分索引（active + not exhausted + not null expires）
		index.Fields("status", "exhausted", "expires_at").
			StorageKey("idx_quota_keys_reconciler").
			Annotations(entsql.IndexWhere("status = 'active'")),
	}
}
