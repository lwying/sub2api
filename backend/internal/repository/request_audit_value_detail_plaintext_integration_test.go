//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPlaintextAuditValueDetailCascadesOnlyWithOwningUsage(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	usageRepo := newUsageLogRepositoryWithSQL(client, integrationDB)
	usage := createCascadeTestUsageLog(t, ctx, client, usageRepo)
	t.Cleanup(func() { _ = usageRepo.Delete(ctx, usage.ID) })
	audit := NewRequestAuditRepository(client)
	require.NoError(t, audit.CreateRequestAudit(ctx, &service.RequestAuditRecord{UsageLogID: usage.ID, CaptureCompleteness: service.RequestAuditCaptureComplete}))

	payload, err := service.EncodeRequestAuditValueDetailValues(service.RequestAuditValueDetailValues{Model: "claude-sonnet-4-5"})
	require.NoError(t, err)
	var created int64
	err = integrationDB.QueryRowContext(ctx, `
		INSERT INTO request_audit_value_details
		(usage_log_id, state, reason, storage_format, route, protocol, plaintext_payload, entry_count, payload_bytes, expires_at)
		SELECT $1, 'stored', 'retained', 'plaintext_usage_bound', '/v1/messages', 'anthropic.messages', $2, 1, $3, NULL
		WHERE EXISTS (SELECT 1 FROM request_audits WHERE usage_log_id = $1)
		RETURNING usage_log_id`, usage.ID, payload, len(payload)).Scan(&created)
	require.NoError(t, err)
	require.Equal(t, usage.ID, created)

	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_audit_value_details WHERE usage_log_id = $1`, usage.ID).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, usageRepo.Delete(ctx, usage.ID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_audit_value_details WHERE usage_log_id = $1`, usage.ID).Scan(&count))
	require.Zero(t, count)
}

func TestPlaintextAuditValueDetailDoesNotExpireAtLegacyDeadline(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	usageRepo := newUsageLogRepositoryWithSQL(client, integrationDB)
	usage := createCascadeTestUsageLog(t, ctx, client, usageRepo)
	t.Cleanup(func() { _ = usageRepo.Delete(ctx, usage.ID) })
	audit := NewRequestAuditRepository(client)
	require.NoError(t, audit.CreateRequestAudit(ctx, &service.RequestAuditRecord{UsageLogID: usage.ID, CaptureCompleteness: service.RequestAuditCaptureComplete}))
	payload, err := service.EncodeRequestAuditValueDetailValues(service.RequestAuditValueDetailValues{Model: "claude-sonnet-4-5"})
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO request_audit_value_details
		(usage_log_id, state, reason, storage_format, route, protocol, plaintext_payload, entry_count, payload_bytes, expires_at)
		VALUES ($1, 'stored', 'retained', 'plaintext_usage_bound', '/v1/messages', 'anthropic.messages', $2, 1, $3, NULL)`, usage.ID, payload, len(payload))
	require.NoError(t, err)
	var raw []byte
	var hasExpiry bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT plaintext_payload, (expires_at IS NOT NULL) FROM request_audit_value_details WHERE usage_log_id = $1`, usage.ID).Scan(&raw, &hasExpiry))
	require.Equal(t, payload, raw)
	require.False(t, hasExpiry)
}
