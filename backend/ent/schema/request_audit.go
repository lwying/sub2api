package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// RequestAudit holds protocol metadata for one usage log. It never stores model body.
type RequestAudit struct {
	ent.Schema
}

func (RequestAudit) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "request_audits"},
	}
}

func (RequestAudit) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("usage_log_id"),
		field.JSON("headers", map[string]any{}).
			Default(map[string]any{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Comment("Allowed protocol headers only; credentials stored as presence"),
		field.JSON("events", []map[string]any{}).
			Default([]map[string]any{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Comment("SSE event skeletons; never stores event text or deltas"),
		field.JSON("attempts", []map[string]any{}).
			Default([]map[string]any{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Comment("Upstream attempt timeline; never stores model body"),
		field.String("capture_completeness").
			Default("complete").
			Comment("采集完整性: complete, truncated, incomplete, not_captured"),
		field.String("capture_reason").
			Default("").
			Comment("采集原因码；phase1_uncovered 表示第一阶段未覆盖"),
		field.String("request_fingerprint").Optional().Nillable(),
		field.Int("fingerprint_key_version").Default(0),
		field.Bytes("fingerprint_salt").Optional().Nillable().StructTag(`json:"-"`).SchemaType(map[string]string{dialect.Postgres: "bytea"}).Comment("Internal per-record random salt; never exposed in audit DTOs"),
		field.JSON("metadata", map[string]any{}).Default(map[string]any{}).SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").
			Default(time.Now).
			Immutable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (RequestAudit) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("usage_log", UsageLog.Type).
			Ref("request_audit").
			Field("usage_log_id").
			Required().
			Unique(),
	}
}
