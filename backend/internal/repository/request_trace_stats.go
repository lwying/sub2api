package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// RequestTraceQueryStats aggregates only persisted envelopes matching the same
// metadata predicate as list and export. No stage body is read or counted.
func (r *requestTraceRepository) RequestTraceQueryStats(ctx context.Context, filter service.RequestTraceListFilter) (service.RequestTraceQueryStats, error) {
	var stats service.RequestTraceQueryStats
	if r == nil || r.q == nil {
		return stats, service.ErrRequestTraceRepositoryUnavailable
	}
	if err := validateRequestTraceFilter(filter); err != nil {
		return stats, err
	}
	const query = `SELECT
		COUNT(*),
		COUNT(*) FILTER (WHERE client_status BETWEEN 200 AND 299),
		COUNT(*) FILTER (WHERE client_status BETWEEN 300 AND 399),
		COUNT(*) FILTER (WHERE client_status BETWEEN 400 AND 499),
		COUNT(*) FILTER (WHERE client_status BETWEEN 500 AND 599),
		COUNT(*) FILTER (WHERE client_status < 200),
		COUNT(*) FILTER (WHERE capture_state='not_observed'),
		COUNT(*) FILTER (WHERE capture_state='stored'),
		COUNT(*) FILTER (WHERE capture_state='partial'),
		COUNT(*) FILTER (WHERE capture_state='write_failed'),
		COUNT(*) FILTER (WHERE usage_log_id IS NOT NULL),
		COUNT(*) FILTER (WHERE usage_log_id IS NULL)
		FROM request_traces WHERE ` + requestTraceFilterWhere
	err := scanSingleRow(ctx, r.q, query, requestTraceListFilterArgs(filter),
		&stats.MatchedTotal, &stats.Status.OK, &stats.Status.Redirect, &stats.Status.Client,
		&stats.Status.Server, &stats.Status.Other, &stats.Capture.NotObserved,
		&stats.Capture.Stored, &stats.Capture.Partial, &stats.Capture.WriteFailed,
		&stats.Usage.Linked, &stats.Usage.Unlinked)
	return stats, err
}
