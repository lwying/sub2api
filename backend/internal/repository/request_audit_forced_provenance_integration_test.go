//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestAuditForcedProvenanceMigrationKeepsHistoricalUnknown(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	old := createCascadeTestUsageLog(t, ctx, client, repo)
	forced := createCascadeTestUsageLog(t, ctx, client, repo)
	audit := NewRequestAuditRepository(client)
	require.NoError(t, audit.CreateRequestAudit(ctx, &service.RequestAuditRecord{UsageLogID: old.ID}))
	require.NoError(t, audit.CreateRequestAudit(ctx, &service.RequestAuditRecord{UsageLogID: forced.ID, ForcedProvenance: service.RequestAuditForcedProvenance}))

	for _, tc := range []struct {
		id   int64
		want string
	}{{old.ID, ""}, {forced.ID, service.RequestAuditForcedProvenance}} {
		rec, err := audit.GetByUsageLogID(ctx, tc.id)
		require.NoError(t, err)
		require.Equal(t, tc.want, rec.ForcedProvenance)
	}
}
