//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// The status endpoint reports the unlinked cleanup backlog, so the count has to
// mean exactly what the cleanup service deletes: unlinked rows past their
// planned point, never usage-owned rows and never rows still inside their
// retention window. This runs on a real PostgreSQL because the predicate is the
// thing being tested, not a Go stub.
func TestRequestTraceBacklogProbeCountsOnlyExpiredUnlinkedRows(t *testing.T) {
	ctx := context.Background()
	tx := newRequestTraceTestTx(t)
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Hour)
	future := now.Add(24 * time.Hour)

	insert := func(traceID string, cleanupAfter time.Time, usageLogID *int64) {
		t.Helper()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO request_traces (trace_id, route_family, inbound_endpoint, cleanup_after, usage_log_id)
			 VALUES ($1, 'messages', '/v1/messages', $2, $3)`, traceID, cleanupAfter, usageLogID)
		require.NoError(t, err)
	}
	for i, id := range []string{
		"00000000000000000000000000000001",
		"00000000000000000000000000000002",
		"00000000000000000000000000000003",
	} {
		insert(id, expired.Add(-time.Duration(i)*time.Minute), nil)
	}
	insert("00000000000000000000000000000004", future, nil) // still inside retention
	insert("00000000000000000000000000000005", future, nil)
	usageID := int64(11)
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs (id) VALUES ($1)`, usageID)
	require.NoError(t, err)
	insert("00000000000000000000000000000006", expired, &usageID) // usage-owned: never this backlog

	probe := &requestTraceBacklogRepository{q: tx}
	count, err := probe.CountUnlinkedRequestTraceBacklog(ctx, now, service.RequestTraceBacklogProbeLimit)
	require.NoError(t, err)
	require.Equal(t, int64(3), count)

	// The bound is a bound: a capped count is a lower bound, and the caller
	// reports it that way rather than as a total.
	capped, err := probe.CountUnlinkedRequestTraceBacklog(ctx, now, 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), capped)

	// Whatever the probe reports must disappear when the cleanup service runs:
	// both sides must read the same predicate inside the test transaction.
	cleanup := service.NewRequestTraceCleanupService(&requestTraceRepository{q: tx})
	_, err = cleanup.RunOnce(ctx)
	require.NoError(t, err)
	count, err = probe.CountUnlinkedRequestTraceBacklog(ctx, now, service.RequestTraceBacklogProbeLimit)
	require.NoError(t, err)
	require.Equal(t, int64(0), count, "the probe and the sweep must agree on the same predicate")
}
