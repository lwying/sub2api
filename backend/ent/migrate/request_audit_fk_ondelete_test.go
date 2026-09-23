package migrate

import (
	"testing"

	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/stretchr/testify/require"
)

// TestRequestAuditForeignKeyOnDeleteActions 断言生成代码里两条 request audit 外键都为 CASCADE。
//
// 依据 ADR 0002：请求审计记录与使用记录同生共死。usage_logs 删除时，request_audits 与
// request_audit_reservations 的相关行必须由数据库级联删除，而不是 NoAction/SetNull。
//
// Ent 的 OnDelete 注解只读非反向（edge.To）侧，所以注解写在 UsageLog.Edges 里；
// 这里在生成产物层面断言注解确实生效（例如 request_audit_reservations.usage_log_id 可空，
// 若注解失效会退回可空列的默认 SetNull）。
func TestRequestAuditForeignKeyOnDeleteActions(t *testing.T) {
	auditFK := findForeignKeyBySymbol(
		t,
		RequestAuditsTable,
		"request_audits_usage_logs_request_audit",
	)
	require.Len(t, auditFK.Columns, 1)
	require.Equal(t, "usage_log_id", auditFK.Columns[0].Name)
	require.False(t, auditFK.Columns[0].Nullable, "request_audits.usage_log_id 为必填外键")
	require.Len(t, auditFK.RefColumns, 1)
	require.Equal(t, "id", auditFK.RefColumns[0].Name)
	require.Equal(t, entschema.Cascade, auditFK.OnDelete)

	reservationFK := findForeignKeyBySymbol(
		t,
		RequestAuditReservationsTable,
		"request_audit_reservations_usage_logs_request_audit_reservation",
	)
	require.Len(t, reservationFK.Columns, 1)
	require.Equal(t, "usage_log_id", reservationFK.Columns[0].Name)
	require.True(t, reservationFK.Columns[0].Nullable, "request_audit_reservations.usage_log_id 允许为空")
	require.Len(t, reservationFK.RefColumns, 1)
	require.Equal(t, "id", reservationFK.RefColumns[0].Name)
	require.Equal(t, entschema.Cascade, reservationFK.OnDelete)
}
