//go:build integration

package repository

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPlainErrorDiagnosticLifecycle(t *testing.T) {
	ctx := context.Background()
	_, _, _, usageID := createErrorDiagnosticUsageFixture(t, ctx)
	id, err := service.NewErrorDiagnosticID()
	require.NoError(t, err)
	now := time.Now().UTC().Add(-45 * 24 * time.Hour)
	digest := strings.Repeat("a", 64)
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO error_diagnostic_records (
		diagnostic_id, protocol, attempt_index, stage, upstream_status,
		body_state, body_reason, header_state, header_reason,
		created_at, metadata_expires_at,
		plain_record, plain_link_digest, plain_link_attempt_index, plain_link_wire_status,
		plain_body_state, plain_body_reason, plain_body_payload, plain_body_bytes
	) VALUES ($1, 'messages', 1, 'wire', 429, 'not_observed', 'not_observed',
	'not_observed', 'not_observed', $2, $3, true, $4, 1, 429,
	'stored', 'plain_body_retained', $5, $6)`, id, now, now.Add(30*24*time.Hour), digest, []byte(`{"model":"claude"}`), len(`{"model":"claude"}`))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, id)
	})

	_, err = integrationDB.ExecContext(ctx, `INSERT INTO request_audits (usage_log_id, metadata, attempts)
		VALUES ($1, jsonb_build_object('ids', jsonb_build_object('local_request_fingerprint', $2::text)),
		jsonb_build_array(jsonb_build_object('stage', 'wire', 'upstream_status', 429)))`, usageID, digest)
	require.NoError(t, err)
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	linked, err := repo.(service.ErrorDiagnosticUsageAttacher).LinkPlainErrorDiagnostics(ctx, usageID, digest, now.Add(20*24*time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 1, linked)
	var storedOwner sql.NullInt64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT plain_owner_usage_log_id FROM error_diagnostic_records WHERE diagnostic_id = $1`, id).Scan(&storedOwner))
	require.True(t, storedOwner.Valid)
	require.Equal(t, usageID, storedOwner.Int64)

	var linkedRemaining int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM error_diagnostic_records WHERE diagnostic_id = $1`, id).Scan(&linkedRemaining))
	require.Equal(t, 1, linkedRemaining)
	_, err = integrationDB.ExecContext(ctx, `DELETE FROM usage_logs WHERE id = $1`, usageID)
	require.NoError(t, err)
	var remaining int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM error_diagnostic_records WHERE diagnostic_id = $1`, id).Scan(&remaining))
	require.Zero(t, remaining)
}

func TestPlainErrorDiagnosticUnlinkedExpiresAndCannotRelink(t *testing.T) {
	ctx := context.Background()
	_, _, _, usageID := createErrorDiagnosticUsageFixture(t, ctx)
	t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, `DELETE FROM usage_logs WHERE id = $1`, usageID) })
	id, err := service.NewErrorDiagnosticID()
	require.NoError(t, err)
	now := time.Now().UTC().Add(-31 * 24 * time.Hour)
	digest := strings.Repeat("b", 64)
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO error_diagnostic_records (
		diagnostic_id, protocol, attempt_index, stage, upstream_status,
		body_state, body_reason, header_state, header_reason,
		created_at, metadata_expires_at,
		plain_record, plain_link_digest, plain_link_attempt_index, plain_link_wire_status,
		plain_body_state, plain_body_reason, plain_body_payload, plain_body_bytes
	) VALUES ($1, 'messages', 1, 'wire', 429, 'not_observed', 'not_observed',
	'not_observed', 'not_observed', $2, $3, true, $4, 1, 429,
	'stored', 'plain_body_retained', $5, $6)`, id, now, now.Add(30*24*time.Hour), digest, []byte(`{"model":"claude"}`), len(`{"model":"claude"}`))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, id)
	})
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO request_audits (usage_log_id, metadata, attempts)
		VALUES ($1, jsonb_build_object('ids', jsonb_build_object('local_request_fingerprint', $2::text)),
		jsonb_build_array(jsonb_build_object('stage', 'wire', 'upstream_status', 429)))`, usageID, digest)
	require.NoError(t, err)
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	linked, err := repo.(service.ErrorDiagnosticUsageAttacher).LinkPlainErrorDiagnostics(ctx, usageID, digest, time.Now().UTC())
	require.NoError(t, err)
	require.Zero(t, linked)
	var storedOwner sql.NullInt64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT plain_owner_usage_log_id FROM error_diagnostic_records WHERE diagnostic_id = $1`, id).Scan(&storedOwner))
	require.False(t, storedOwner.Valid, "expired provisional record must never be revived")
}
