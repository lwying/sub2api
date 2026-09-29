//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The replica puts the sidecar in an isolated search_path so real catalog and
// cascade behavior can be verified without touching shared integration tables.
func TestUsageCleanupCascadesRequestTraces(t *testing.T) {
	ctx := context.Background()
	repo, db := newPartitionGuardReplica(t, ctx, partitionGuardSupported)
	aged := time.Date(2020, 1, 5, 3, 0, 0, 0, time.UTC)
	seedPartitionGuardUsage(t, ctx, db, 1, aged)
	_, err := db.ExecContext(ctx, `INSERT INTO request_traces (id, usage_log_id, payload) VALUES (1, 1, 'trace')`)
	require.NoError(t, err)

	require.NoError(t, repo.CleanupUsageLogs(ctx, aged.Add(24*time.Hour)))
	require.Zero(t, partitionGuardCount(t, ctx, db, "request_traces WHERE usage_log_id = 1"))
}

func TestUsageCleanupPartitionDropRefusesLinkedRequestTrace(t *testing.T) {
	ctx := context.Background()
	repo, db := newPartitionGuardReplica(t, ctx, partitionGuardPartitionedCompositeFK)
	_, err := db.ExecContext(ctx, `CREATE TABLE request_traces (
		id BIGINT PRIMARY KEY, usage_log_id BIGINT, usage_created_at TIMESTAMPTZ, payload BYTEA,
		FOREIGN KEY (usage_log_id, usage_created_at) REFERENCES usage_logs (id, created_at) ON DELETE CASCADE
	)`)
	require.NoError(t, err)
	april := time.Date(2026, 4, 3, 3, 0, 0, 0, time.UTC)
	may := time.Date(2026, 5, 20, 3, 0, 0, 0, time.UTC)
	seedPartitionGuardUsage(t, ctx, db, 1, april)
	seedPartitionGuardUsage(t, ctx, db, 2, may)
	_, err = db.ExecContext(ctx, `INSERT INTO request_traces
		(id, usage_log_id, usage_created_at, payload) VALUES (1, 1, $1, 'april'), (2, 2, $2, 'may')`, april, may)
	require.NoError(t, err)

	err = repo.CleanupUsageLogs(ctx, time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC))
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified,
		"composite FKs on partitioned usage are not the supported single-column ownership shape")
	require.ErrorContains(t, err, "request_traces.usage_log_id")
	require.True(t, partitionGuardRelationExists(t, ctx, db, "usage_logs_202604"))
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db, "request_traces WHERE usage_log_id = 1"),
		"unsupported partition shape must refuse row deletes as well as DROP")
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db, "request_traces WHERE usage_log_id = 2"),
		"a different partition's linked trace must remain")
}

func TestUsageCleanupRefusesRequestTraceWithoutOwnership(t *testing.T) {
	ctx := context.Background()
	repo, db := newPartitionGuardReplica(t, ctx, partitionGuardSupported)
	aged := time.Date(2020, 1, 5, 3, 0, 0, 0, time.UTC)
	seedPartitionGuardUsage(t, ctx, db, 1, aged)
	_, err := db.ExecContext(ctx, `ALTER TABLE request_traces DROP CONSTRAINT IF EXISTS request_traces_usage_log_id_fkey`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO request_traces (id, usage_log_id, payload) VALUES (1, 1, 'trace')`)
	require.NoError(t, err)

	err = repo.CleanupUsageLogs(ctx, aged.Add(24*time.Hour))
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified)
	require.ErrorContains(t, err, "request_traces.usage_log_id")
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db, "usage_logs WHERE id = 1"))
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db, "request_traces WHERE usage_log_id = 1"))
}
