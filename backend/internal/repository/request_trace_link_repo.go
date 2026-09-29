package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var traceLinkIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// A proven ownership collision is reported by a fixed code. These constants are
// the only variable part of the warning line, so the log can never carry a Trace
// ID, a usage ID, a header value or a body fragment.
const (
	// traceUsageCollisionClaimedByOtherTrace: the usage row is already claimed by
	// a different server Trace, so a second logical request cannot own it.
	traceUsageCollisionClaimedByOtherTrace = "trace_usage_claimed_by_other_trace"
	// traceUsageCollisionTraceReused: the same server Trace identity already
	// claims a different usage row.
	traceUsageCollisionTraceReused = "trace_identity_claims_other_usage"
)

// requestTraceUsageLinkWarn receives a fixed collision code and nothing else.
type requestTraceUsageLinkWarn func(code string)

type requestTraceUsageLinker struct {
	q    sqlQueryer
	db   *sql.DB
	warn requestTraceUsageLinkWarn
}

// NewRequestTraceUsageLinker never infers ownership from client-supplied IDs.
func NewRequestTraceUsageLinker(db *sql.DB) service.RequestTraceUsageLinker {
	if db == nil {
		return &requestTraceUsageLinker{}
	}
	return &requestTraceUsageLinker{q: db, db: db}
}

var _ service.RequestTraceUsageLinker = (*requestTraceUsageLinker)(nil)

// LinkRequestTraceUsage must only be invoked after a proven fresh usage INSERT.
// The claim survives an async usage worker that finishes before the HTTP Trace
// envelope. A unique usage FK prevents two logical Traces sharing ownership.
func (r *requestTraceUsageLinker) LinkRequestTraceUsage(ctx context.Context, traceID string, usageLogID int64) (bool, error) {
	if r == nil || r.q == nil || !traceLinkIDPattern.MatchString(traceID) || usageLogID <= 0 {
		return false, fmt.Errorf("invalid request trace usage link")
	}
	linked, collision, err := r.link(ctx, traceID, usageLogID)
	if err != nil {
		return false, err
	}
	// A False link is not enough to warn: a durable claim whose Trace envelope
	// has not been stored yet also reconciles to False, and only a proven
	// collision is reported. Billing already succeeded before this call.
	r.warnCollision(collision)
	return linked, nil
}

func (r *requestTraceUsageLinker) link(ctx context.Context, traceID string, usageLogID int64) (bool, string, error) {
	if r.db == nil {
		return r.linkRequestTraceUsage(ctx, r.q, traceID, usageLogID)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = tx.Rollback() }()
	linked, collision, err := r.linkRequestTraceUsage(ctx, tx, traceID, usageLogID)
	if err != nil {
		return false, "", err
	}
	if err := tx.Commit(); err != nil {
		return false, "", err
	}
	return linked, collision, nil
}

// linkRequestTraceUsage returns the link result plus a collision code that stays
// empty unless the usage row is provably owned by another Trace, or this Trace
// identity already owns a different row. A False result with an empty code is
// the expected late envelope: the durable claim exists, the HTTP Trace row has
// not been stored yet.
func (r *requestTraceUsageLinker) linkRequestTraceUsage(ctx context.Context, q sqlQueryer, traceID string, usageLogID int64) (bool, string, error) {
	const claim = `INSERT INTO request_trace_usage_claims (trace_id,usage_log_id)
		VALUES ($1,$2) ON CONFLICT DO NOTHING RETURNING usage_log_id`
	var claimedID int64
	err := scanSingleRow(ctx, q, claim, []any{traceID, usageLogID}, &claimedID)
	if errors.Is(err, sql.ErrNoRows) {
		// The claim did not insert. Either this Trace identity already holds a
		// claim, or the usage row is held by a different Trace.
		err = scanSingleRow(ctx, q,
			`SELECT usage_log_id FROM request_trace_usage_claims WHERE trace_id=$1`,
			[]any{traceID}, &claimedID)
		if errors.Is(err, sql.ErrNoRows) {
			// No claim exists for this Trace, so the conflict came from the
			// unique usage row: a different Trace already owns this
			// deduplicated usage. Return no link without an error so billing and
			// the caller's fail-open behavior stay unchanged.
			return false, traceUsageCollisionClaimedByOtherTrace, nil
		}
		if err != nil {
			if traceLinkDuplicate(err) {
				return false, "", nil
			}
			return false, "", err
		}
		if claimedID != usageLogID {
			// The same server Trace identity claims a different usage row.
			return false, traceUsageCollisionTraceReused, nil
		}
		// Idempotent re-link of the same pair: fall through to reconcile.
	} else if err != nil {
		if traceLinkDuplicate(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if claimedID != usageLogID {
		return false, "", nil
	}
	linked, err := reconcileRequestTraceUsage(ctx, q, traceID)
	if err != nil {
		return false, "", err
	}
	return linked, "", nil
}

func (r *requestTraceUsageLinker) ReconcileRequestTraceUsage(ctx context.Context, traceID string) (bool, error) {
	if r == nil || r.q == nil || !traceLinkIDPattern.MatchString(traceID) {
		return false, fmt.Errorf("invalid request trace id")
	}
	if r.db != nil {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback() }()
		linked, err := reconcileRequestTraceUsage(ctx, tx, traceID)
		if err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return linked, nil
	}
	return reconcileRequestTraceUsage(ctx, r.q, traceID)
}

func reconcileRequestTraceUsage(ctx context.Context, q sqlQueryer, traceID string) (bool, error) {
	const query = `UPDATE request_traces AS trace
		SET usage_log_id = claim.usage_log_id
		FROM request_trace_usage_claims AS claim
		WHERE claim.trace_id = $1 AND claim.trace_id = trace.trace_id
		  AND claim.expires_at > now() AND trace.usage_log_id IS NULL
		  AND EXISTS (SELECT 1 FROM usage_logs WHERE id = claim.usage_log_id)
		  AND NOT EXISTS (SELECT 1 FROM request_traces AS owner WHERE owner.usage_log_id = claim.usage_log_id)
		RETURNING trace.id`
	var id int64
	err := scanSingleRow(ctx, q, query, []any{traceID}, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		if traceLinkDuplicate(err) {
			return false, nil
		}
		return false, err
	}
	return id > 0, nil
}

// warnCollision emits one bounded warning for a proven collision. A Trace ID, a
// usage ID or any request value never reaches the sink.
func (r *requestTraceUsageLinker) warnCollision(code string) {
	if r == nil || code == "" {
		return
	}
	warn := r.warn
	if warn == nil {
		warn = logRequestTraceUsageLinkCollision
	}
	warn(code)
}

// requestTraceUsageLinkCollisionMessage is fixed apart from the collision code.
func requestTraceUsageLinkCollisionMessage(code string) string {
	return "Request trace usage link collision warning: code=" + code
}

func logRequestTraceUsageLinkCollision(code string) {
	logger.LegacyPrintf("repository.request_trace", "%s", requestTraceUsageLinkCollisionMessage(code))
}

func traceLinkDuplicate(err error) bool {
	var pgErr *pq.Error
	return errors.As(err, &pgErr) && string(pgErr.Code) == "23505"
}

func (r *requestTraceUsageLinker) DeleteExpiredRequestTraceUsageClaims(ctx context.Context, before time.Time, limit int) (int64, error) {
	if r == nil || r.q == nil || limit <= 0 || limit > 500 {
		return 0, fmt.Errorf("invalid request trace claim cleanup")
	}
	const query = `DELETE FROM request_trace_usage_claims WHERE trace_id IN (
		SELECT claim.trace_id FROM request_trace_usage_claims claim
		LEFT JOIN request_traces trace ON trace.trace_id=claim.trace_id
		WHERE claim.expires_at <= $1 AND trace.id IS NULL
		ORDER BY claim.expires_at, claim.trace_id LIMIT $2
	) RETURNING trace_id`
	rows, err := r.q.QueryContext(ctx, query, before.UTC(), limit)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var count int64
	for rows.Next() {
		count++
	}
	return count, rows.Err()
}
