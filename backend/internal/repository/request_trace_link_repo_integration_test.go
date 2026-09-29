//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func traceLinkTestDB(t *testing.T) (*requestTraceUsageLinker, *sql.Tx) {
	t.Helper()
	ctx := context.Background()
	tx := testTx(t)
	schema := pq.QuoteIdentifier(fmt.Sprintf("trace_link_%d", time.Now().UnixNano()))
	_, err := tx.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `CREATE TABLE usage_logs(id BIGINT PRIMARY KEY);
		CREATE TABLE request_traces(id BIGSERIAL PRIMARY KEY,trace_id TEXT UNIQUE NOT NULL,usage_log_id BIGINT UNIQUE REFERENCES usage_logs(id) ON DELETE CASCADE);
		CREATE TABLE request_trace_usage_claims(trace_id TEXT PRIMARY KEY,usage_log_id BIGINT UNIQUE NOT NULL REFERENCES usage_logs(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + interval '30 days');`)
	require.NoError(t, err)
	// Collision warnings are asserted explicitly through recordTraceLinkWarnings;
	// replacing the default sink keeps the suite quiet about the expected ones.
	return &requestTraceUsageLinker{q: tx, warn: func(string) {}}, tx
}

// recordTraceLinkWarnings captures the collision codes a linker reports. The
// sink only ever receives a fixed code, never a Trace ID or usage ID.
func recordTraceLinkWarnings(linker *requestTraceUsageLinker) *[]string {
	codes := &[]string{}
	linker.warn = func(code string) { *codes = append(*codes, code) }
	return codes
}

func TestRequestTraceClaimReconcilesLateEnvelopeAndExpiresUnlinked(t *testing.T) {
	ctx := context.Background()
	linker, tx := traceLinkTestDB(t)
	warnings := recordTraceLinkWarnings(linker)
	id := strings.Repeat("c", 32)
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs(id) VALUES (9)`)
	require.NoError(t, err)
	linked, err := linker.LinkRequestTraceUsage(ctx, id, 9)
	require.NoError(t, err)
	require.False(t, linked, "claim is durable even if request Trace row is not yet stored")
	var claims int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_trace_usage_claims WHERE trace_id=$1 AND usage_log_id=9`, id).Scan(&claims))
	require.Equal(t, 1, claims)
	_, err = tx.ExecContext(ctx, `INSERT INTO request_traces(trace_id) VALUES ($1)`, id)
	require.NoError(t, err)
	linked, err = linker.ReconcileRequestTraceUsage(ctx, id)
	require.NoError(t, err)
	require.True(t, linked)
	var owner sql.NullInt64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT usage_log_id FROM request_traces WHERE trace_id=$1`, id).Scan(&owner))
	require.True(t, owner.Valid)
	require.Equal(t, int64(9), owner.Int64)
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_logs WHERE id=9`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_trace_usage_claims`).Scan(&claims))
	require.Zero(t, claims, "claim must cascade with usage")
	require.Empty(t, *warnings, "a durable claim awaiting its Trace envelope is not a collision")
}

func TestRequestTraceLinkerRefusesDuplicateAndCascadesWithUsage(t *testing.T) {
	ctx := context.Background()
	linker, tx := traceLinkTestDB(t)
	warnings := recordTraceLinkWarnings(linker)
	first := strings.Repeat("a", 32)
	second := strings.Repeat("b", 32)
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs(id) VALUES (7)`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO request_traces(trace_id) VALUES ($1), ($2)`, first, second)
	require.NoError(t, err)
	linked, err := linker.LinkRequestTraceUsage(ctx, first, 7)
	require.NoError(t, err)
	require.True(t, linked)
	require.Empty(t, *warnings, "the owning Trace links without a collision warning")
	linked, err = linker.LinkRequestTraceUsage(ctx, second, 7)
	require.NoError(t, err)
	require.False(t, linked, "second logical request must not attach to deduplicated usage")
	require.Equal(t, []string{traceUsageCollisionClaimedByOtherTrace}, *warnings,
		"a proven usage collision is reported exactly once, without Trace or usage IDs")
	var owner sql.NullInt64
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT usage_log_id FROM request_traces WHERE trace_id=$1", second).Scan(&owner))
	require.False(t, owner.Valid)
	// Billing facts are untouched: the deduplicated usage row still exists and
	// the proven owner keeps it.
	var usageRows int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_logs WHERE id=7").Scan(&usageRows))
	require.Equal(t, 1, usageRows, "collision must not delete or rewrite the billing usage row")
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT trace_id FROM request_traces WHERE usage_log_id=7").Scan(&second))
	require.Equal(t, first, second)
	_, err = tx.ExecContext(ctx, "DELETE FROM usage_logs WHERE id=7")
	require.NoError(t, err)
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_traces").Scan(&count))
	require.Equal(t, 1, count, "first cascades, second unlinked still exists")
}

// TestRequestTraceLinkWarningCarriesNoIdentifiers pins the emitted line to a
// fixed prefix plus a collision code, so no Trace ID, usage ID, header value or
// body fragment can reach the log.
func TestRequestTraceLinkWarningCarriesNoIdentifiers(t *testing.T) {
	require.Equal(t,
		"Request trace usage link collision warning: code=trace_usage_claimed_by_other_trace",
		requestTraceUsageLinkCollisionMessage(traceUsageCollisionClaimedByOtherTrace))
	for _, code := range []string{
		traceUsageCollisionClaimedByOtherTrace,
		traceUsageCollisionTraceReused,
	} {
		require.Regexp(t, `^trace_[a-z_]+$`, code, "collision codes must stay identifier-free")
	}
}

func TestRequestTraceLinkerWarnsWhenTheSameTraceClaimsAnotherUsage(t *testing.T) {
	ctx := context.Background()
	linker, tx := traceLinkTestDB(t)
	warnings := recordTraceLinkWarnings(linker)
	id := strings.Repeat("f", 32)
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs(id) VALUES (21), (22)`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO request_traces(trace_id) VALUES ($1)`, id)
	require.NoError(t, err)
	linked, err := linker.LinkRequestTraceUsage(ctx, id, 21)
	require.NoError(t, err)
	require.True(t, linked)
	require.Empty(t, *warnings)
	linked, err = linker.LinkRequestTraceUsage(ctx, id, 22)
	require.NoError(t, err)
	require.False(t, linked, "one server Trace identity cannot own two usage rows")
	require.Equal(t, []string{traceUsageCollisionTraceReused}, *warnings)
	var owner sql.NullInt64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT usage_log_id FROM request_traces WHERE trace_id=$1`, id).Scan(&owner))
	require.True(t, owner.Valid)
	require.Equal(t, int64(21), owner.Int64, "the first proven ownership must not be rewritten")
}

func TestRequestTraceClaimDoesNotLinkWhenUsageIsDeletedBeforeLateEnvelope(t *testing.T) {
	ctx := context.Background()
	linker, tx := traceLinkTestDB(t)
	id := strings.Repeat("e", 32)
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs(id) VALUES (12)`)
	require.NoError(t, err)
	linked, err := linker.LinkRequestTraceUsage(ctx, id, 12)
	require.NoError(t, err)
	require.False(t, linked)
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_logs WHERE id=12`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO request_traces(trace_id) VALUES ($1)`, id)
	require.NoError(t, err)
	linked, err = linker.ReconcileRequestTraceUsage(ctx, id)
	require.NoError(t, err)
	require.False(t, linked, "deleted usage cannot be resurrected by late Trace")
	var owner sql.NullInt64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT usage_log_id FROM request_traces WHERE trace_id=$1`, id).Scan(&owner))
	require.False(t, owner.Valid)
}

func TestRequestTracePendingClaimClearsAfterDeadlineWhenEnvelopeNeverArrives(t *testing.T) {
	ctx := context.Background()
	linker, tx := traceLinkTestDB(t)
	id := strings.Repeat("d", 32)
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs(id) VALUES (11)`)
	require.NoError(t, err)
	linked, err := linker.LinkRequestTraceUsage(ctx, id, 11)
	require.NoError(t, err)
	require.False(t, linked)
	_, err = tx.ExecContext(ctx, `UPDATE request_trace_usage_claims SET expires_at=$2 WHERE trace_id=$1`, id, time.Now().UTC().Add(-time.Minute))
	require.NoError(t, err)
	deleted, err := linker.DeleteExpiredRequestTraceUsageClaims(ctx, time.Now().UTC(), 500)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	_, err = tx.ExecContext(ctx, `INSERT INTO request_traces(trace_id) VALUES ($1)`, id)
	require.NoError(t, err)
	linked, err = linker.ReconcileRequestTraceUsage(ctx, id)
	require.NoError(t, err)
	require.False(t, linked, "expired unmatched ownership must never revive")
}
