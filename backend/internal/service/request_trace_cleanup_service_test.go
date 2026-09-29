//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type traceCleanupRepoStub struct {
	before time.Time
	limit  int
	count  int64
	err    error
}

func (s *traceCleanupRepoStub) DeleteExpiredUnlinkedRequestTraces(_ context.Context, before time.Time, limit int) (int64, error) {
	s.before = before
	s.limit = limit
	return s.count, s.err
}

func TestRequestTraceCleanupRunsBoundedPhysicalDeletionOnly(t *testing.T) {
	clock := time.Date(2026, 9, 28, 7, 30, 0, 0, time.UTC)
	repo := &traceCleanupRepoStub{count: 23}
	cleanup := NewRequestTraceCleanupService(repo)
	cleanup.now = func() time.Time { return clock }
	deleted, err := cleanup.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(23), deleted)
	require.Equal(t, clock, repo.before)
	require.Positive(t, repo.limit)
	require.LessOrEqual(t, repo.limit, 500)

	repo.err = errors.New("storage unavailable")
	deleted, err = cleanup.RunOnce(context.Background())
	require.Error(t, err)
	require.Equal(t, int64(23), deleted, "a partial deletion is still a physical fact when the repository reports a later error")
}
