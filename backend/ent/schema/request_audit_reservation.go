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

// RequestAuditReservation stores forced-audit metadata before an upstream send.
// It is temporary and never stores model request or response bodies.
type RequestAuditReservation struct {
	ent.Schema
}

func (RequestAuditReservation) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "request_audit_reservations"}}
}

func (RequestAuditReservation) Fields() []ent.Field {
	return []ent.Field{
		field.String("logical_key").NotEmpty(),
		field.String("route_family").NotEmpty(),
		field.Bool("forced").Default(false),
		field.JSON("headers", map[string]any{}).
			Default(map[string]any{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Comment("Sanitized protocol headers; never stores credential plaintext"),
		field.JSON("attempts", []map[string]any{}).
			Default([]map[string]any{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Comment("Reserved upstream attempt metadata; never stores model body"),
		field.Int64("usage_log_id").Optional().Nillable(),
		field.String("capture_completeness").Default("complete"),
		field.String("capture_reason").Default(""),
		field.Time("send_started_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("expires_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("created_at").Default(time.Now).Immutable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (RequestAuditReservation) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("usage_log", UsageLog.Type).
			Ref("request_audit_reservation").
			Field("usage_log_id").
			Unique(),
	}
}

func (RequestAuditReservation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("logical_key").Unique(),
		index.Fields("usage_log_id").Unique(),
		index.Fields("expires_at"),
	}
}
