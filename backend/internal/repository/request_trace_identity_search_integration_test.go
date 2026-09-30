//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTraceMetadataKeywordDoesNotSearchBodiesAndMatchesCurrentIdentity(t *testing.T) {
	ctx := context.Background()
	tx := newRequestTraceTestTx(t)
	repo := &requestTraceRepository{q: tx}
	userID, keyID := int64(23), int64(19)
	_, err := tx.ExecContext(ctx, `INSERT INTO users(id, email) VALUES ($1, 'current@example.test')`, userID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO api_keys(id, name) VALUES ($1, 'current-key')`, keyID)
	require.NoError(t, err)
	id := "abcdefabcdefabcdefabcdefabcdefab"
	_, err = repo.CreateRequestTrace(ctx, service.RequestTrace{TraceID: id, RouteFamily: service.RequestTraceMessages,
		InboundEndpoint: "/v1/messages", UserID: &userID, APIKeyID: &keyID})
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO request_trace_stages(trace_id, ordinal, stage, state, reason, payload)
		SELECT id, 1, 'wire_request', 'stored', 'retained', $1 FROM request_traces WHERE trace_id=$2`, []byte("payload-secret-canary"), id)
	require.NoError(t, err)
	matches := func(q string) []service.RequestTrace {
		rows, _, err := repo.ListRequestTraces(ctx, service.RequestTraceListFilter{Page: 1, PageSize: 10, Keyword: q})
		require.NoError(t, err)
		return rows
	}
	require.Len(t, matches("abcde"), 1)
	require.Len(t, matches("messages"), 1)
	require.Len(t, matches("current@example"), 1)
	require.Len(t, matches("current-key"), 1)
	require.Empty(t, matches("payload-secret"), "Trace body must never be indexed")
	rows := matches("current-key")
	require.Equal(t, "current@example.test", rows[0].UserEmail)
	require.Equal(t, "current-key", rows[0].APIKeyName)
	_, err = tx.ExecContext(ctx, `DELETE FROM api_keys WHERE id=$1`, keyID)
	require.NoError(t, err)
	rows, _, err = repo.ListRequestTraces(ctx, service.RequestTraceListFilter{Page: 1, PageSize: 10, APIKeyID: &keyID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, keyID, *rows[0].APIKeyID)
	require.Empty(t, rows[0].APIKeyName, "deleted key name cannot be invented")
	require.Empty(t, matches("current-key"), "keyword follows current labels")
}
