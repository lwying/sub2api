//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAdminUsagePageLinksOnlyReadableTraceAndVerifiedForcedAudit(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	audit := NewRequestAuditRepository(client)
	user := mustCreateUser(t, client, &service.User{Email: "usage-links-" + uuid.NewString() + "@example.com"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-usage-links-" + uuid.NewString(), Name: "usage-links"})
	account := mustCreateAccount(t, client, &service.Account{Name: "usage-links-" + uuid.NewString()})
	createUsage := func() int64 {
		log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID,
			RequestID: uuid.NewString(), Model: "claude-test", CreatedAt: time.Now().UTC()}
		_, err := repo.Create(ctx, log)
		require.NoError(t, err)
		return log.ID
	}
	linked := createUsage()
	historical := createUsage()
	absent := createUsage()
	traceID := "abcdabcdabcdabcdabcdabcdabcdabcd"
	_, err := tx.ExecContext(ctx, `INSERT INTO request_traces
		(trace_id, route_family, inbound_endpoint, usage_log_id) VALUES ($1, 'messages', '/v1/messages', $2)`, traceID, linked)
	require.NoError(t, err)
	require.NoError(t, audit.CreateRequestAudit(ctx, &service.RequestAuditRecord{
		UsageLogID: linked, ForcedProvenance: service.RequestAuditForcedProvenance,
		CaptureCompleteness: service.RequestAuditCaptureComplete,
	}))
	require.NoError(t, audit.CreateRequestAudit(ctx, &service.RequestAuditRecord{
		UsageLogID: historical, CaptureCompleteness: service.RequestAuditCaptureComplete,
	}))
	ids := []int64{linked, historical, absent}
	for _, id := range ids {
		rows, _, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, usagestats.UsageLogFilters{UsageLogID: id, IncludeAdminDiagnostics: true})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		row := rows[0]
		if id == linked {
			require.True(t, row.RequestTraceAvailable)
			require.NotNil(t, row.RequestTraceID)
			require.Equal(t, traceID, *row.RequestTraceID)
			require.True(t, row.RequestAuditForcedAvailable)
		} else {
			require.False(t, row.RequestTraceAvailable)
			require.Nil(t, row.RequestTraceID)
			require.False(t, row.RequestAuditForcedAvailable)
		}
	}
	// The ordinary user's metering list must not depend on any of the admin-only
	// Trace/audit lookups, even when the underlying records exist.
	rows, _, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, usagestats.UsageLogFilters{UsageLogID: linked})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.False(t, rows[0].RequestTraceAvailable)
	require.False(t, rows[0].RequestAuditForcedAvailable)
}
