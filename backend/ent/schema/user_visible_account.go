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

// UserVisibleAccount 是「管理员逐用户分配的可见账号」关联边。
//
// 它刻意不复用分组／账号路由关系：管理员必须为具体普通用户逐条分配，
// 默认零行。分配只授予只读查看，不授予调度、编辑或凭据访问。
type UserVisibleAccount struct {
	ent.Schema
}

func (UserVisibleAccount) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "user_visible_accounts"},
		// 复合主键：(user_id, account_id)。
		field.ID("user_id", "account_id"),
	}
}

func (UserVisibleAccount) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("user_id"),
		field.Int64("account_id"),
		// 授权管理员 id；仅用于追责，可为空（例如运维脚本写入）。
		field.Int64("granted_by").
			Optional().
			Nillable(),
		field.Time("created_at").
			Immutable().
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (UserVisibleAccount) Edges() []ent.Edge {
	return []ent.Edge{
		// 用户或账号被物理删除时，分配关系随之清理（与迁移 250 的 FK 一致）。
		edge.To("user", User.Type).
			Unique().
			Required().
			Field("user_id").
			Annotations(entsql.OnDelete(entsql.Cascade)),
		edge.To("account", Account.Type).
			Unique().
			Required().
			Field("account_id").
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}

func (UserVisibleAccount) Indexes() []ent.Index {
	return []ent.Index{
		// 账号侧反查（账号被删除／禁用时的可见性校验）。
		index.Fields("account_id"),
	}
}
