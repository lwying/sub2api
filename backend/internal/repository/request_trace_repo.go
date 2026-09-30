package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var traceIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// requestTraceStageMetadataLimit mirrors the database-side bound of the single JSONB
// column (migrations 261/262: octet_length(metadata::text) <= 4096).
const requestTraceStageMetadataLimit = 4096

type requestTraceRepository struct {
	q sqlExecutor
}

func NewRequestTraceRepository(db *sql.DB) service.RequestTraceRepository {
	if db == nil {
		// 不能把 typed-nil 的 *sql.DB 装进接口：那样 r.q == nil 永远为假，第一个查询
		// 会 panic，而不是像其它仓储那样返回 ErrRequestTraceRepositoryUnavailable。
		return &requestTraceRepository{}
	}
	return &requestTraceRepository{q: db}
}

var _ service.RequestTraceRepository = (*requestTraceRepository)(nil)

const requestTraceEnvelopeColumns = `id, trace_id, route_family, inbound_endpoint, capture_state, client_status, usage_log_id, created_at, completed_at, cleanup_after, group_id, requested_model`

func scanRequestTraceEnvelope(row interface{ Scan(...any) error }) (service.RequestTrace, error) {
	var trace service.RequestTrace
	var family, state string
	var usageID sql.NullInt64
	var completedAt sql.NullTime
	var traceCleanupAfter time.Time
	var groupID sql.NullInt64
	var requestedModel sql.NullString
	err := row.Scan(&trace.ID, &trace.TraceID, &family, &trace.InboundEndpoint, &state, &trace.ClientStatus,
		&usageID, &trace.CreatedAt, &completedAt, &traceCleanupAfter, &groupID, &requestedModel)
	if err != nil {
		return service.RequestTrace{}, err
	}
	trace.RouteFamily = service.RequestTraceRouteFamily(family)
	trace.CaptureState = service.RequestTraceCaptureState(state)
	if usageID.Valid {
		trace.UsageLogID = &usageID.Int64
	}
	if completedAt.Valid {
		trace.CompletedAt = &completedAt.Time
	}
	if trace.UsageLogID == nil {
		trace.CleanupAfter = &traceCleanupAfter
	}
	// 未观察到的事实保持为零值，页面按"未知"呈现，不伪造成某个具体分组或模型。
	if groupID.Valid {
		value := groupID.Int64
		trace.GroupID = &value
	}
	trace.RequestedModel = requestedModel.String
	return trace, nil
}

func requestTraceFamilyValid(family service.RequestTraceRouteFamily) bool {
	switch family {
	case service.RequestTraceMessages, service.RequestTraceChatCompletions, service.RequestTraceResponses:
		return true
	default:
		return false
	}
}

func requestTraceStateValid(state service.RequestTraceCaptureState) bool {
	switch state {
	case service.RequestTraceNotObserved, service.RequestTraceStored, service.RequestTraceTruncated,
		service.RequestTraceUnsupported, service.RequestTraceUnverified, service.RequestTraceWriteFailed:
		return true
	default:
		return false
	}
}

func (r *requestTraceRepository) CreateRequestTrace(ctx context.Context, trace service.RequestTrace) (service.RequestTrace, error) {
	if r == nil || r.q == nil {
		return service.RequestTrace{}, service.ErrRequestTraceRepositoryUnavailable
	}
	if !traceIDShape.MatchString(trace.TraceID) || !requestTraceFamilyValid(trace.RouteFamily) || trace.ClientStatus < 0 || trace.ClientStatus > 599 {
		return service.RequestTrace{}, service.ErrRequestTraceInvalidRecord
	}
	if trace.CaptureState == "" {
		trace.CaptureState = service.RequestTraceNotObserved
	}
	if trace.CaptureState != service.RequestTraceNotObserved && trace.CaptureState != service.RequestTraceStored &&
		trace.CaptureState != service.RequestTracePartial && trace.CaptureState != service.RequestTraceWriteFailed {
		return service.RequestTrace{}, service.ErrRequestTraceInvalidRecord
	}
	createdAt := trace.CreatedAt.UTC()
	if trace.CreatedAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var completedAt any
	if trace.CompletedAt != nil {
		completedAt = trace.CompletedAt.UTC()
	}
	const query = `
		INSERT INTO request_traces (trace_id, route_family, inbound_endpoint, capture_state, client_status, created_at, completed_at, cleanup_after, group_id, requested_model)
		VALUES ($1, $2, $3, $4, $5, $6::timestamptz, $7, $6::timestamptz + INTERVAL '30 days', $8, $9)
		ON CONFLICT (trace_id) DO NOTHING
		RETURNING ` + requestTraceEnvelopeColumns
	stored, err := scanRequestTraceEnvelopeForQuery(ctx, r.q, query, trace.TraceID, trace.RouteFamily, trace.InboundEndpoint,
		trace.CaptureState, trace.ClientStatus, createdAt, completedAt, trace.GroupID, nullIfEmptyTraceModel(trace.RequestedModel))
	if errors.Is(err, sql.ErrNoRows) {
		const existing = `SELECT ` + requestTraceEnvelopeColumns + ` FROM request_traces WHERE trace_id = $1`
		return scanRequestTraceEnvelopeForQuery(ctx, r.q, existing, trace.TraceID)
	}
	return stored, err
}

// nullIfEmptyTraceModel 让"未观察到模型"落库为 NULL 而不是空串，
// 这样查询侧能区分"未知"与"确实请求了空模型名"。
func nullIfEmptyTraceModel(model string) any {
	if model == "" {
		return nil
	}
	return model
}

func scanRequestTraceEnvelopeForQuery(ctx context.Context, q sqlQueryer, query string, args ...any) (service.RequestTrace, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return service.RequestTrace{}, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return service.RequestTrace{}, err
		}
		return service.RequestTrace{}, sql.ErrNoRows
	}
	trace, err := scanRequestTraceEnvelope(rows)
	if err != nil {
		return service.RequestTrace{}, err
	}
	return trace, rows.Err()
}

func requestTraceViewValid(view string) bool {
	switch view {
	case "", "transmitted", "decoded", "wire", "received", "downstream":
		return true
	default:
		return false
	}
}

func (r *requestTraceRepository) AppendRequestTraceStage(ctx context.Context, stage service.RequestTraceStage) (service.RequestTraceStage, error) {
	if r == nil || r.q == nil {
		return service.RequestTraceStage{}, service.ErrRequestTraceRepositoryUnavailable
	}
	if !traceIDShape.MatchString(stage.TraceID) || stage.Ordinal <= 0 || stage.AttemptIndex < 0 || stage.AttemptIndex > 1000 ||
		stage.Stage == "" || !requestTraceViewValid(stage.View) || !requestTraceStateValid(stage.State) ||
		stage.ObservedBytes < 0 || stage.DroppedEvents < 0 || len(stage.Payload) > service.RequestTraceStagePayloadLimit ||
		(stage.Payload != nil && stage.State != service.RequestTraceStored && stage.State != service.RequestTraceTruncated && stage.State != service.RequestTraceUnverified) ||
		(stage.Payload == nil && stage.State == service.RequestTraceStored) {
		return service.RequestTraceStage{}, service.ErrRequestTraceInvalidRecord
	}
	stage.RetainedBytes = len(stage.Payload)
	// The single JSONB column carries the one typed projection that belongs to this
	// stage. A stage cannot smuggle the other projection, a caller map or an
	// oversized value past this boundary.
	if !service.ValidRequestTraceStageFacts(stage.Stage, stage.Metadata) || !service.ValidRequestTraceDecisionStage(stage) {
		return service.RequestTraceStage{}, service.ErrRequestTraceInvalidRecord
	}
	encoded, err := encodeRequestTraceStageProjection(stage)
	if err != nil {
		return service.RequestTraceStage{}, err
	}
	const query = `
		INSERT INTO request_trace_stages
		    (trace_id, ordinal, stage, attempt_index, view_name, state, reason, observed_bytes,
		     retained_bytes, dropped_events, redaction_unverified, payload, metadata)
		SELECT id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
		FROM request_traces WHERE trace_id = $1
		ON CONFLICT (trace_id, ordinal) DO NOTHING
		RETURNING id, created_at`
	// A stage with no body -- every not_observed/unsupported/write_failed stage and the
	// body-less gateway_decision stage -- must store SQL NULL, not an empty bytea: the
	// driver would otherwise encode a nil []byte as '' and 258's
	// request_trace_stages_payload_size constraint requires a non-NULL payload to belong
	// to a stored/truncated/redaction_unverified row.
	var payload any
	if stage.Payload != nil {
		payload = stage.Payload
	}
	err = scanSingleRow(ctx, r.q, query, []any{stage.TraceID, stage.Ordinal, stage.Stage, stage.AttemptIndex, stage.View,
		stage.State, stage.Reason, stage.ObservedBytes, stage.RetainedBytes, stage.DroppedEvents, stage.RedactionUnverified,
		payload, encoded}, &stage.ID, &stage.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		const existing = `SELECT s.id, s.stage, s.attempt_index, s.view_name, s.state, s.reason,
			s.observed_bytes, s.retained_bytes, s.dropped_events, s.redaction_unverified,
			s.payload, s.metadata, s.created_at
			FROM request_trace_stages s JOIN request_traces r ON r.id = s.trace_id
			WHERE r.trace_id = $1 AND s.ordinal = $2`
		rows, queryErr := r.q.QueryContext(ctx, existing, stage.TraceID, stage.Ordinal)
		if queryErr != nil {
			return service.RequestTraceStage{}, queryErr
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return service.RequestTraceStage{}, err
			}
			return service.RequestTraceStage{}, sql.ErrNoRows
		}
		stored := service.RequestTraceStage{TraceID: stage.TraceID, Ordinal: stage.Ordinal}
		var state string
		var metadata []byte
		if err := rows.Scan(&stored.ID, &stored.Stage, &stored.AttemptIndex, &stored.View, &state, &stored.Reason,
			&stored.ObservedBytes, &stored.RetainedBytes, &stored.DroppedEvents, &stored.RedactionUnverified,
			&stored.Payload, &metadata, &stored.CreatedAt); err != nil {
			return service.RequestTraceStage{}, err
		}
		stored.State = service.RequestTraceCaptureState(state)
		projection, err := decodeRequestTraceStageProjection(stored.Stage, metadata)
		if err != nil {
			return service.RequestTraceStage{}, err
		}
		stored.Metadata = projection.facts
		stored.Decision = projection.decision
		return stored, rows.Err()
	}
	return stage, err
}

// requestTraceStageProjection is the decoded form of the stage's single JSONB
// column: exactly one of the typed projections (or neither, for a body stage).
type requestTraceStageProjection struct {
	facts    *service.RequestTraceStageFacts
	decision *service.RequestTraceDecisionFacts
}

// encodeRequestTraceStageProjection serializes the one typed projection a stage may
// carry into the JSONB column: the transport facts for client_metadata/wire_attempt,
// the gateway decision for gateway_decision, and '{}' for every body stage. Only the
// typed structs can reach JSON, and the database bound is re-checked here so an
// oversized projection fails closed before the statement is sent.
func encodeRequestTraceStageProjection(stage service.RequestTraceStage) ([]byte, error) {
	var projection any = struct{}{}
	switch {
	case stage.Stage == service.RequestTraceDecisionStage:
		if !service.ValidRequestTraceDecisionStage(stage) {
			return nil, service.ErrRequestTraceInvalidRecord
		}
		projection = stage.Decision
	case stage.Decision != nil:
		return nil, service.ErrRequestTraceInvalidRecord
	case stage.Metadata != nil:
		if !service.ValidRequestTraceStageFacts(stage.Stage, stage.Metadata) {
			return nil, service.ErrRequestTraceInvalidRecord
		}
		projection = stage.Metadata
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return nil, fmt.Errorf("encode request trace stage metadata: %w", err)
	}
	if len(encoded) > requestTraceStageMetadataLimit {
		return nil, service.ErrRequestTraceInvalidRecord
	}
	return encoded, nil
}

// decodeRequestTraceStageProjection rebuilds the typed projection a stored stage
// carries. A row whose JSONB does not match its stage's closed key set, carries
// unknown fields, exceeds the projection's own bound or is not a validated value
// fails closed instead of being disclosed as a partially trusted map; a decision
// stage with no decision is refused for the same reason.
func decodeRequestTraceStageProjection(stage string, encoded []byte) (requestTraceStageProjection, error) {
	empty := len(encoded) == 0 || string(encoded) == "{}"
	switch stage {
	case service.RequestTraceDecisionStage:
		if empty || len(encoded) > service.RequestTraceDecisionFactsLimit {
			return requestTraceStageProjection{}, service.ErrRequestTraceInvalidRecord
		}
		var decision service.RequestTraceDecisionFacts
		if decodeRequestTraceProjectionJSON(encoded, &decision) != nil || !service.ValidRequestTraceDecisionFacts(&decision) {
			return requestTraceStageProjection{}, service.ErrRequestTraceInvalidRecord
		}
		return requestTraceStageProjection{decision: &decision}, nil
	case "client_metadata", "wire_attempt":
		if empty {
			return requestTraceStageProjection{}, nil
		}
		if len(encoded) > requestTraceStageMetadataLimit {
			return requestTraceStageProjection{}, service.ErrRequestTraceInvalidRecord
		}
		var facts service.RequestTraceStageFacts
		if decodeRequestTraceProjectionJSON(encoded, &facts) != nil || !service.ValidRequestTraceStageFacts(stage, &facts) {
			return requestTraceStageProjection{}, service.ErrRequestTraceInvalidRecord
		}
		return requestTraceStageProjection{facts: &facts}, nil
	default:
		// Every other stage must keep '{}' (migrations 261/262), so a non-empty
		// object here is a row the writer would never have produced.
		if empty {
			return requestTraceStageProjection{}, nil
		}
		return requestTraceStageProjection{}, service.ErrRequestTraceInvalidRecord
	}
}

func decodeRequestTraceProjectionJSON(encoded []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func (r *requestTraceRepository) GetRequestTrace(ctx context.Context, traceID string) (*service.RequestTraceDetail, error) {
	if r == nil || r.q == nil {
		return nil, service.ErrRequestTraceRepositoryUnavailable
	}
	if !traceIDShape.MatchString(traceID) {
		return nil, service.ErrRequestTraceInvalidRecord
	}
	const envelope = `SELECT ` + requestTraceEnvelopeColumns + ` FROM request_traces WHERE trace_id = $1`
	trace, err := scanRequestTraceEnvelopeForQuery(ctx, r.q, envelope, traceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := r.q.QueryContext(ctx, `SELECT id, ordinal, stage, attempt_index, view_name, state, reason,
		observed_bytes, retained_bytes, dropped_events, redaction_unverified, payload, metadata, created_at
		FROM request_trace_stages WHERE trace_id = $1 ORDER BY ordinal`, trace.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	detail := &service.RequestTraceDetail{RequestTrace: trace, Stages: []service.RequestTraceStage{}}
	for rows.Next() {
		var stage service.RequestTraceStage
		var state string
		var metadata []byte
		if err := rows.Scan(&stage.ID, &stage.Ordinal, &stage.Stage, &stage.AttemptIndex, &stage.View, &state, &stage.Reason,
			&stage.ObservedBytes, &stage.RetainedBytes, &stage.DroppedEvents, &stage.RedactionUnverified, &stage.Payload,
			&metadata, &stage.CreatedAt); err != nil {
			return nil, err
		}
		stage.TraceID = traceID
		stage.State = service.RequestTraceCaptureState(state)
		projection, err := decodeRequestTraceStageProjection(stage.Stage, metadata)
		if err != nil {
			return nil, err
		}
		stage.Metadata = projection.facts
		stage.Decision = projection.decision
		detail.Stages = append(detail.Stages, stage)
	}
	return detail, rows.Err()
}

func (r *requestTraceRepository) ListRequestTraces(ctx context.Context, filter service.RequestTraceListFilter) ([]service.RequestTrace, int64, error) {
	if r == nil || r.q == nil {
		return nil, 0, service.ErrRequestTraceRepositoryUnavailable
	}
	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 20
	}
	if filter.PageSize > 100 {
		filter.PageSize = 100
	}
	if filter.TraceID != "" && !traceIDShape.MatchString(filter.TraceID) {
		return nil, 0, service.ErrRequestTraceInvalidRecord
	}
	if filter.RouteFamily != "" && !requestTraceFamilyValid(filter.RouteFamily) {
		return nil, 0, service.ErrRequestTraceInvalidRecord
	}
	if filter.ClientStatus != nil && (*filter.ClientStatus < 0 || *filter.ClientStatus > 599) {
		return nil, 0, service.ErrRequestTraceInvalidRecord
	}
	if filter.UsageLogID != nil && *filter.UsageLogID <= 0 {
		return nil, 0, service.ErrRequestTraceInvalidRecord
	}
	if filter.AccountID != nil && *filter.AccountID <= 0 {
		return nil, 0, service.ErrRequestTraceInvalidRecord
	}
	// 账号检索只认 wire_attempt 阶段的类型化事实：stage 过滤把匹配范围钉在真实
	// 上游尝试上，JSONB 包含判断用绑定参数而非把 ID 拼进语句。列表投影保持
	// requestTraceEnvelopeColumns，绝不会 SELECT 出 metadata，因此检索条件不会
	// 变成把阶段 JSONB 回传给管理端。
	const where = `($1 = '' OR trace_id = $1)
		AND ($2 = '' OR route_family = $2)
		AND ($3::integer IS NULL OR client_status = $3)
		AND ($4::timestamptz IS NULL OR created_at >= $4)
		AND ($5::timestamptz IS NULL OR created_at < $5)
		AND ($6::boolean IS NULL OR (usage_log_id IS NOT NULL) = $6)
		AND ($7::bigint IS NULL OR usage_log_id = $7)
		AND ($8::bigint IS NULL OR EXISTS (
			SELECT 1 FROM request_trace_stages s
			WHERE s.trace_id = request_traces.id
			  AND s.stage = 'wire_attempt'
			  AND s.metadata @> jsonb_build_object('account_id', $8::bigint)
		))
		AND ($9::bigint IS NULL OR group_id = $9)
		AND ($10::boolean IS NULL OR (group_id IS NULL) = $10)
		AND ($11::text IS NULL OR lower(requested_model) = lower($11))
		AND ($12::boolean IS NULL OR (requested_model IS NULL) = $12)
		AND ($13::text IS NULL OR EXISTS (
			SELECT 1 FROM request_trace_stages s2
			WHERE s2.trace_id = request_traces.id
			  AND s2.stage = 'wire_attempt'
			  AND s2.metadata @> jsonb_build_object('platform', $13::text)
		))
		AND ($14::boolean IS NULL OR (NOT EXISTS (
			SELECT 1 FROM request_trace_stages s3
			WHERE s3.trace_id = request_traces.id
			  AND s3.stage = 'wire_attempt'
			  AND s3.metadata ? 'platform'
		)) = $14)`
	var from, to any
	if !filter.CreatedFrom.IsZero() {
		from = filter.CreatedFrom
	}
	if !filter.CreatedTo.IsZero() {
		to = filter.CreatedTo
	}
	var usageLinked any
	if filter.UsageLinked != nil {
		usageLinked = *filter.UsageLinked
	}
	var groupUnknown, modelUnknown, platformUnknown any
	if filter.GroupUnknown != nil {
		groupUnknown = *filter.GroupUnknown
	}
	if filter.ModelUnknown != nil {
		modelUnknown = *filter.ModelUnknown
	}
	if filter.PlatformUnknown != nil {
		platformUnknown = *filter.PlatformUnknown
	}
	args := []any{
		filter.TraceID, filter.RouteFamily, filter.ClientStatus, from, to, usageLinked,
		filter.UsageLogID, filter.AccountID, filter.GroupID, groupUnknown,
		nullIfEmptyTraceModel(filter.RequestedModel), modelUnknown,
		nullIfEmptyTraceModel(filter.Platform), platformUnknown,
	}
	var count int64
	if err := scanSingleRow(ctx, r.q, `SELECT COUNT(*) FROM request_traces WHERE `+where, args, &count); err != nil {
		return nil, 0, err
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+requestTraceEnvelopeColumns+` FROM request_traces WHERE `+where+
		` ORDER BY created_at DESC, id DESC LIMIT $15 OFFSET $16`, append(args, filter.PageSize, (filter.Page-1)*filter.PageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	list := make([]service.RequestTrace, 0)
	for rows.Next() {
		trace, err := scanRequestTraceEnvelope(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, trace)
	}
	return list, count, rows.Err()
}

func (r *requestTraceRepository) FinalizeRequestTraceCapture(ctx context.Context, traceID string, state service.RequestTraceCaptureState) error {
	if r == nil || r.q == nil || !traceIDShape.MatchString(traceID) || state != service.RequestTraceStored {
		return service.ErrRequestTraceInvalidRecord
	}
	result, err := r.q.ExecContext(ctx, `UPDATE request_traces SET capture_state='stored' WHERE trace_id=$1 AND capture_state='partial'`, traceID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return service.ErrRequestTraceInvalidRecord
	}
	return nil
}

func (r *requestTraceRepository) DeleteExpiredUnlinkedRequestTraces(ctx context.Context, before time.Time, limit int) (int64, error) {
	if r == nil || r.q == nil {
		return 0, service.ErrRequestTraceRepositoryUnavailable
	}
	if limit <= 0 || limit > 500 {
		return 0, service.ErrRequestTraceInvalidRecord
	}
	rows, err := r.q.QueryContext(ctx, `DELETE FROM request_traces WHERE id IN (
		SELECT id FROM request_traces WHERE usage_log_id IS NULL AND cleanup_after <= $1
		ORDER BY cleanup_after ASC, id ASC LIMIT $2
	) RETURNING id`, before.UTC(), limit)
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
