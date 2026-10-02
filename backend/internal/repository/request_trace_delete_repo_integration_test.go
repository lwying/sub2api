//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 手动清理的集成测试只在事务测试库可用时运行（integration tag + Docker）。
// 每张表都建在测试专用的 schema 里并随外层事务回滚，绝不触碰真实数据。
func newRequestTraceDeleteTestTx(t *testing.T) (*sql.Tx, context.Context) {
	t.Helper()
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	schema := pq.QuoteIdentifier(fmt.Sprintf("trace_delete_%d", time.Now().UnixNano()))
	_, err = tx.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
CREATE TABLE request_traces (
    id BIGSERIAL PRIMARY KEY,
    trace_id TEXT NOT NULL UNIQUE,
    route_family TEXT NOT NULL,
    inbound_endpoint TEXT NOT NULL,
    capture_state TEXT NOT NULL DEFAULT 'not_observed',
    client_status INTEGER NOT NULL DEFAULT 0,
    usage_log_id BIGINT UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    cleanup_after TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '30 days'),
    group_id BIGINT,
    requested_model TEXT,
    observed_platforms JSONB,
    user_id BIGINT,
    api_key_id BIGINT
);
CREATE TABLE request_trace_stages (
    id BIGSERIAL PRIMARY KEY,
    trace_id BIGINT NOT NULL REFERENCES request_traces(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    stage TEXT NOT NULL,
    attempt_index INTEGER NOT NULL DEFAULT 0,
    view_name TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    reason TEXT NOT NULL,
    observed_bytes BIGINT NOT NULL DEFAULT 0,
    retained_bytes INTEGER NOT NULL DEFAULT 0,
    dropped_events INTEGER NOT NULL DEFAULT 0,
    redaction_unverified BOOLEAN NOT NULL DEFAULT false,
    payload BYTEA,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(trace_id, ordinal)
);
CREATE TABLE request_trace_usage_claims (
    trace_id TEXT PRIMARY KEY,
    usage_log_id BIGINT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '30 days')
);`)
	require.NoError(t, err)
	return tx, ctx
}

func insertRequestTraceDeleteFixture(t *testing.T, ctx context.Context, tx *sql.Tx, traceID string, createdAt time.Time, withStage bool) int64 {
	t.Helper()
	var id int64
	require.NoError(t, tx.QueryRowContext(ctx, `
		INSERT INTO request_traces (trace_id, route_family, inbound_endpoint, capture_state, client_status, created_at)
		VALUES ($1, 'messages', '/v1/messages', 'stored', 200, $2) RETURNING id`,
		traceID, createdAt.UTC()).Scan(&id))
	if withStage {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO request_trace_stages (trace_id, ordinal, stage, state, reason)
			VALUES ($1, 1, 'client_entry', 'stored', 'retained')`, id)
		require.NoError(t, err)
	}
	return id
}

func TestRequestTraceDeleteByFilterRespectsSnapshotAndCascades(t *testing.T) {
	tx, ctx := newRequestTraceDeleteTestTx(t)
	now := time.Now().UTC()
	oldID := insertRequestTraceDeleteFixture(t, ctx, tx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", now.Add(-2*time.Hour), true)
	newID := insertRequestTraceDeleteFixture(t, ctx, tx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", now, false)
	t.Cleanup(func() {
		_, _ = tx.ExecContext(ctx, `DELETE FROM request_trace_stages WHERE trace_id IN ($1, $2)`, oldID, newID)
	})

	// A claim on the old trace must be removed explicitly (there is no FK cascade
	// between claims and request_traces in production).
	_, err := tx.ExecContext(ctx, `INSERT INTO request_trace_usage_claims (trace_id, usage_log_id) VALUES ($1, $2)`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 424242)
	require.NoError(t, err)

	filter := service.RequestTraceExportFilter{RouteFamily: "messages"}
	filter.CreatedTo = &now
	repo := &requestTraceDeleteRepository{q: tx}
	matched, snapshotMaxID, err := repo.PreviewRequestTraceDelete(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, int64(1), matched)
	require.Equal(t, oldID, snapshotMaxID)

	// A trace inserted after the preview must not fall inside the delete boundary.
	laterID := insertRequestTraceDeleteFixture(t, ctx, tx, "cccccccccccccccccccccccccccccccc", now.Add(time.Hour), false)
	t.Cleanup(func() { _, _ = tx.ExecContext(ctx, `DELETE FROM request_traces WHERE id = $1`, laterID) })

	deleted, deletedIDs, err := requestTraceDeleteBatchExec(ctx, tx, filter, snapshotMaxID, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	require.Equal(t, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, deletedIDs)
	require.NoError(t, requestTraceDeleteUsageClaimsExec(ctx, tx, deletedIDs))

	var stages int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_trace_stages WHERE trace_id = $1`, oldID).Scan(&stages))
	require.Zero(t, stages, "the deleted trace's stages must cascade away")
	var claims int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_trace_usage_claims WHERE trace_id = $1`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").Scan(&claims))
	require.Zero(t, claims, "the deleted trace's usage claim must be removed")
	var survivors int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_traces WHERE id IN ($1, $2)`, newID, laterID).Scan(&survivors))
	require.Equal(t, 2, survivors, "rows newer than the snapshot boundary must survive")
}

// A matching row locked by another session must never be silently skipped and then
// reported as a fully completed sweep. Without SKIP LOCKED the batch waits on the
// lock; when the caller's context expires the call returns the real partial count
// with completed=false and an error, and the locked row survives.
func TestRequestTraceDeleteByFilterDoesNotClaimFullSuccessWhileRowsAreLocked(t *testing.T) {
	ctx := context.Background()
	model := fmt.Sprintf("delete-lock-%d", time.Now().UnixNano())
	traceIDs := make([]string, 0, 3)
	internalIDs := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		traceID := fmt.Sprintf("%032x", time.Now().UnixNano()+int64(i))
		var id int64
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
			INSERT INTO request_traces (trace_id, route_family, inbound_endpoint, capture_state, client_status, requested_model)
			VALUES ($1, 'messages', '/v1/messages', 'stored', 200, $2) RETURNING id`, traceID, model).Scan(&id))
		traceIDs = append(traceIDs, traceID)
		internalIDs = append(internalIDs, id)
	}
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM request_traces WHERE trace_id = ANY($1)`, pq.Array(traceIDs))
	})

	locker, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = locker.Rollback() }()
	var lockedID int64
	require.NoError(t, locker.QueryRowContext(ctx, `SELECT id FROM request_traces WHERE id = $1 FOR UPDATE`, internalIDs[2]).Scan(&lockedID))

	filter := service.RequestTraceExportFilter{RequestedModel: model}
	repo := NewRequestTraceDeleteRepository(integrationDB)
	deleteCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	deleted, completed, err := repo.DeleteRequestTracesByFilter(deleteCtx, filter, internalIDs[2], 2)
	require.Error(t, err, "a locked matching row must not be reported as a completed sweep")
	require.False(t, completed)
	require.Equal(t, int64(2), deleted, "the unlocked batch must still be reported as really deleted")

	var survivors int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_traces WHERE id = $1`, internalIDs[2]).Scan(&survivors))
	require.Equal(t, 1, survivors, "the locked row must survive until its lock is released")
}

func TestRequestTraceDeleteByIDsRemovesTracesStagesAndClaims(t *testing.T) {
	tx, ctx := newRequestTraceDeleteTestTx(t)
	now := time.Now().UTC()
	id := "dddddddddddddddddddddddddddddddd"
	internalID := insertRequestTraceDeleteFixture(t, ctx, tx, id, now, true)
	_, err := tx.ExecContext(ctx, `INSERT INTO request_trace_usage_claims (trace_id, usage_log_id) VALUES ($1, $2)`, id, 515151)
	require.NoError(t, err)

	deleted, deletedIDs, err := requestTraceDeleteByTraceIDsExec(ctx, tx, []string{id})
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	require.Equal(t, []string{id}, deletedIDs)
	require.NoError(t, requestTraceDeleteUsageClaimsExec(ctx, tx, deletedIDs))

	var traces int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_traces WHERE id = $1`, internalID).Scan(&traces))
	require.Zero(t, traces)
	var claims int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_trace_usage_claims WHERE trace_id = $1`, id).Scan(&claims))
	require.Zero(t, claims)

	// Deleting an already-absent id is a truthful zero, not an error.
	again, missingIDs, err := requestTraceDeleteByTraceIDsExec(ctx, tx, []string{"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"})
	require.NoError(t, err)
	require.Zero(t, again)
	require.Empty(t, missingIDs)
}
