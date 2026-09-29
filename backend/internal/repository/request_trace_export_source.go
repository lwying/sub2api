package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The export service walks one monotonic trace_id cursor at a time, so a page has
// to stay strictly ascending and never exceed the requested limit. The upper limit
// mirrors the export service's own accepted page size with headroom for callers
// that ask for a bigger page than the service itself uses.
const requestTraceExportSourceMaxLimit = 1000

// The export service refuses an approved detail with more than 1000 stages. Paging
// the stage read to the same bound keeps a single read bounded in memory while
// never rejecting a record that the export service itself would have accepted.
const requestTraceExportSourceMaxStages = 1000

var (
	requestTraceExportSourceIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

	errRequestTraceExportSourceUnavailable = errors.New("request trace export source has no queryer")
)

// requestTraceExportSource reads the same rows the session-only admin Trace detail
// reads, using parameterized SQL only. It owns no write path and no payload cache:
// every stage payload is bounded before it is handed to the export service, which
// is the single writer of the exported file.
type requestTraceExportSource struct {
	q sqlQueryer
}

func NewRequestTraceExportSource(db *sql.DB) service.RequestTraceExportSource {
	if db == nil {
		return &requestTraceExportSource{}
	}
	return &requestTraceExportSource{q: db}
}

var _ service.RequestTraceExportSource = (*requestTraceExportSource)(nil)

// NextTraceIDs returns at most limit trace IDs strictly greater than after, ordered
// by trace_id. Selection uses Trace envelope metadata only: body text, stage
// content and arbitrary metadata never take part in filtering.
func (s *requestTraceExportSource) NextTraceIDs(ctx context.Context, filter service.RequestTraceExportFilter, after string, limit int) ([]string, error) {
	if s == nil || s.q == nil {
		return nil, errRequestTraceExportSourceUnavailable
	}
	if !requestTraceExportSourcePageValid(filter, after, limit) {
		return nil, service.ErrRequestTraceInvalidRecord
	}
	const query = `
		SELECT trace_id FROM request_traces
		WHERE ($1 = '' OR trace_id = $1)
			AND ($2 = '' OR route_family = $2)
			AND ($3 = 0 OR client_status = $3)
			AND ($4::timestamptz IS NULL OR created_at >= $4)
			AND ($5::timestamptz IS NULL OR created_at < $5)
			AND ($6::boolean IS NULL OR (usage_log_id IS NOT NULL) = $6)
			AND trace_id > $7
		ORDER BY trace_id
		LIMIT $8`
	var createdFrom, createdTo any
	if filter.CreatedFrom != nil {
		createdFrom = filter.CreatedFrom.UTC()
	}
	if filter.CreatedTo != nil {
		createdTo = filter.CreatedTo.UTC()
	}
	var usageLinked any
	if filter.UsageLinked != nil {
		usageLinked = *filter.UsageLinked
	}
	rows, err := s.q.QueryContext(ctx, query, filter.TraceID, string(filter.RouteFamily), filter.ClientStatus,
		createdFrom, createdTo, usageLinked, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0, limit)
	previous := after
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		// The cursor is only sound while the stored order matches the order the
		// caller compares with: a repeated, malformed or descending ID would let a
		// page skip or replay records, so it is refused instead of exported.
		if !requestTraceExportSourceIDShape.MatchString(id) || id <= previous {
			return nil, service.ErrRequestTraceInvalidRecord
		}
		previous = id
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func requestTraceExportSourcePageValid(filter service.RequestTraceExportFilter, after string, limit int) bool {
	if limit <= 0 || limit > requestTraceExportSourceMaxLimit {
		return false
	}
	if after != "" && !requestTraceExportSourceIDShape.MatchString(after) {
		return false
	}
	if filter.TraceID != "" && !requestTraceExportSourceIDShape.MatchString(filter.TraceID) {
		return false
	}
	switch filter.RouteFamily {
	case "", string(service.RequestTraceMessages), string(service.RequestTraceChatCompletions), string(service.RequestTraceResponses):
	default:
		return false
	}
	if filter.ClientStatus < 0 || filter.ClientStatus > 599 {
		return false
	}
	if filter.CreatedFrom != nil && filter.CreatedTo != nil && !filter.CreatedFrom.Before(*filter.CreatedTo) {
		return false
	}
	return true
}

// ReadApprovedDetail loads a Trace that still exists and maps it into the typed
// allowlist the session-only admin detail API already discloses. Arbitrary stage
// metadata and raw payload bytes stay in the database: only the same bounded,
// redacted URL/header facts and UTF-8 payload text disclosed by the admin detail
// can be exported. A row deleted between paging and reading is reported as unavailable so the
// export counts it as skipped instead of resurrecting it.
func (s *requestTraceExportSource) ReadApprovedDetail(ctx context.Context, id string) (service.RequestTraceExportApprovedDetail, bool, error) {
	if s == nil || s.q == nil {
		return service.RequestTraceExportApprovedDetail{}, false, errRequestTraceExportSourceUnavailable
	}
	if !requestTraceExportSourceIDShape.MatchString(id) {
		return service.RequestTraceExportApprovedDetail{}, false, service.ErrRequestTraceInvalidRecord
	}
	const envelopeQuery = `
		SELECT id, route_family, inbound_endpoint, capture_state, client_status, usage_log_id
		FROM request_traces WHERE trace_id = $1`
	var internalID int64
	var family, endpoint, state string
	var status int
	var usageLogID sql.NullInt64
	err := scanSingleRow(ctx, s.q, envelopeQuery, []any{id}, &internalID, &family, &endpoint, &state, &status, &usageLogID)
	if errors.Is(err, sql.ErrNoRows) {
		return service.RequestTraceExportApprovedDetail{}, false, nil
	}
	if err != nil {
		return service.RequestTraceExportApprovedDetail{}, false, err
	}
	detail := service.RequestTraceExportApprovedDetail{
		TraceID:         id,
		RouteFamily:     family,
		InboundEndpoint: endpoint,
		CaptureState:    state,
		ClientStatus:    status,
		Stages:          make([]service.RequestTraceExportApprovedStage, 0, 8),
	}
	if usageLogID.Valid {
		linked := usageLogID.Int64
		detail.UsageLogID = &linked
	}
	// The payload column is read as bytes and never exposed as bytes: it is bounded
	// one byte above the stage limit so an out-of-policy row fails closed instead of
	// being truncated into a misleading value, and it only becomes text when it is
	// valid UTF-8.
	const stagesQuery = `
		SELECT ordinal, stage, attempt_index, view_name, state, reason, observed_bytes,
			retained_bytes, dropped_events, redaction_unverified,
			substring(payload from 1 for $2::integer), substring(metadata::text from 1 for $4::integer)
		FROM request_trace_stages WHERE trace_id = $1 ORDER BY ordinal LIMIT $3`
	rows, err := s.q.QueryContext(ctx, stagesQuery, internalID,
		service.RequestTraceStagePayloadLimit+1, requestTraceExportSourceMaxStages+1, 4097)
	if err != nil {
		return service.RequestTraceExportApprovedDetail{}, false, err
	}
	defer func() { _ = rows.Close() }()
	var retainedPayload int64
	for rows.Next() {
		if len(detail.Stages) >= requestTraceExportSourceMaxStages {
			return service.RequestTraceExportApprovedDetail{}, false, service.ErrRequestTraceExportLimit
		}
		var stage service.RequestTraceExportApprovedStage
		var payload []byte
		var metadata string
		if err := rows.Scan(&stage.Ordinal, &stage.Stage, &stage.AttemptIndex, &stage.ViewName, &stage.State, &stage.Reason,
			&stage.ObservedBytes, &stage.RetainedBytes, &stage.DroppedEvents, &stage.RedactionUnverified, &payload, &metadata); err != nil {
			return service.RequestTraceExportApprovedDetail{}, false, err
		}
		if len(payload) > service.RequestTraceStagePayloadLimit || len(metadata) > requestTraceStageMetadataLimit {
			return service.RequestTraceExportApprovedDetail{}, false, service.ErrRequestTraceExportLimit
		}
		// The JSONB projection is decoded through the same typed rule used by
		// the repository and admin detail; no arbitrary metadata is exported.
		projection, err := decodeRequestTraceStageProjection(stage.Stage, []byte(metadata))
		if err != nil {
			return service.RequestTraceExportApprovedDetail{}, false, err
		}
		stage.Facts = projection.facts
		stage.Decision = projection.decision
		retainedPayload += int64(len(payload))
		// service.RequestTraceTotalBodyLimit is the collector's whole-trace body
		// budget, so a bigger payload set cannot be normal capture: it fails closed
		// rather than letting one trace exhaust the export's memory.
		if retainedPayload > service.RequestTraceTotalBodyLimit {
			return service.RequestTraceExportApprovedDetail{}, false, service.ErrRequestTraceExportLimit
		}
		if len(payload) > 0 && utf8.Valid(payload) {
			stage.PayloadText = string(payload)
		}
		detail.Stages = append(detail.Stages, stage)
	}
	if err := rows.Err(); err != nil {
		return service.RequestTraceExportApprovedDetail{}, false, err
	}
	return detail, true, nil
}
