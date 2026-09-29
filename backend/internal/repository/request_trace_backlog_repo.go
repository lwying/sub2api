package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// requestTraceBacklogRepository answers one question with one bounded count:
// how many unlinked traces are past their planned cleanup point. It is the only
// live storage signal the value-free status endpoint has, so it runs even when
// the capture queue has seen no traffic at all.
//
// The count reads no column of a trace beyond the cleanup predicate and never
// touches payload, metadata or an id: nothing it returns can carry content.
type requestTraceBacklogRepository struct {
	q sqlQueryer
}

func NewRequestTraceBacklogRepository(db *sql.DB) service.RequestTraceBacklogProbe {
	if db == nil {
		// A typed-nil *sql.DB would make the nil check below permanently false and
		// turn the first probe into a panic instead of an "unavailable" state.
		return &requestTraceBacklogRepository{}
	}
	return &requestTraceBacklogRepository{q: db}
}

var _ service.RequestTraceBacklogProbe = (*requestTraceBacklogRepository)(nil)

// requestTraceBacklogQuery counts at most the bound the caller asked for. The
// predicate is exactly the one the partial index
// request_traces_unlinked_cleanup_idx (cleanup_after, id) WHERE usage_log_id IS
// NULL covers, and there is deliberately no ORDER BY: a count that reaches the
// LIMIT stops early instead of sorting the whole backlog.
const requestTraceBacklogQuery = `SELECT count(*) FROM (
	SELECT 1 FROM request_traces
	WHERE usage_log_id IS NULL AND cleanup_after <= $1
	LIMIT $2
) AS unlinked_backlog`

func (r *requestTraceBacklogRepository) CountUnlinkedRequestTraceBacklog(ctx context.Context, before time.Time, limit int) (int64, error) {
	if r == nil || r.q == nil {
		return 0, service.ErrRequestTraceRepositoryUnavailable
	}
	if limit <= 0 || limit > service.RequestTraceBacklogProbeLimit {
		return 0, service.ErrRequestTraceInvalidRecord
	}
	var count int64
	if err := scanSingleRow(ctx, r.q, requestTraceBacklogQuery, []any{before.UTC(), limit}, &count); err != nil {
		// The error is returned, not logged: it may name a host or a statement.
		// The caller collapses it into a state enum.
		return 0, err
	}
	return count, nil
}
