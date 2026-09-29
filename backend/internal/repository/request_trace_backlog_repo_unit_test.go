//go:build unit

package repository

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceBacklogProbeRejectsOverLargeOrMissingLimit(t *testing.T) {
	repo := NewRequestTraceBacklogRepository(nil)
	_, err := repo.CountUnlinkedRequestTraceBacklog(context.Background(), time.Now(), service.RequestTraceBacklogProbeLimit)
	require.ErrorIs(t, err, service.ErrRequestTraceRepositoryUnavailable)

	// A bound that cannot be executed is refused before any statement is built.
	repo = &requestTraceBacklogRepository{q: &recordingRequestTraceQueryer{}}
	for _, limit := range []int{0, -1, service.RequestTraceBacklogProbeLimit + 1} {
		_, err := repo.CountUnlinkedRequestTraceBacklog(context.Background(), time.Now(), limit)
		require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord, "limit %d must be rejected", limit)
	}
}

func TestRequestTraceBacklogProbeIsBoundedAndReadsNoContent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	before := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT count\(\*\) FROM \(`).
		WithArgs(before, 250).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(7)))

	repo := NewRequestTraceBacklogRepository(db)
	count, err := repo.CountUnlinkedRequestTraceBacklog(context.Background(), before, 250)
	require.NoError(t, err)
	require.Equal(t, int64(7), count)
	require.NoError(t, mock.ExpectationsWereMet())
}

// The count must stay one bounded statement: no payload, metadata, trace id or
// stage join, and both values bound rather than formatted into the SQL.
func TestRequestTraceBacklogProbeStatementShape(t *testing.T) {
	lowered := strings.ToLower(requestTraceBacklogQuery)
	for _, forbidden := range []string{"payload", "metadata", "request_trace_stages", "select \\*", "order by"} {
		require.NotContains(t, lowered, forbidden, "the backlog count must not touch %q", forbidden)
	}
	require.Contains(t, requestTraceBacklogQuery, "usage_log_id IS NULL")
	require.Contains(t, requestTraceBacklogQuery, "cleanup_after <= $1")
	require.Contains(t, requestTraceBacklogQuery, "LIMIT $2")
	require.Regexp(t, regexp.MustCompile(`count\(\*\)`), requestTraceBacklogQuery)
}

func TestRequestTraceBacklogProbeSurfacesStorageErrorWithoutText(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT count\(\*\) FROM \(`).
		WillReturnError(errors.New("connection refused to 10.0.0.9:5432"))

	repo := NewRequestTraceBacklogRepository(db)
	_, err = repo.CountUnlinkedRequestTraceBacklog(context.Background(), time.Now(), 10)
	require.Error(t, err)
}
