//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Non-ASCII on purpose: only UTF-8 payload text may cross the export boundary.
const exportSourceRealSentinel = "合成哨兵-ünïcode-✓"

// Migration 258 owns the real table shapes. The isolated schema below lets these
// tests run against the same shapes until that migration is available here.
const requestTraceExportSourceTestSchema = `
CREATE TABLE IF NOT EXISTS usage_logs (id BIGSERIAL PRIMARY KEY);
CREATE TABLE IF NOT EXISTS request_traces (
    id BIGSERIAL PRIMARY KEY,
    trace_id TEXT NOT NULL UNIQUE,
    route_family TEXT NOT NULL,
    inbound_endpoint TEXT NOT NULL,
    capture_state TEXT NOT NULL DEFAULT 'not_observed',
    client_status INTEGER NOT NULL DEFAULT 0,
    usage_log_id BIGINT UNIQUE REFERENCES usage_logs(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    cleanup_after TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '30 days'),
    CONSTRAINT request_traces_trace_id_shape CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    CONSTRAINT request_traces_route_family_allowed CHECK (
        route_family IN ('messages', 'chat_completions', 'responses')
    ),
    CONSTRAINT request_traces_capture_state_allowed CHECK (
        capture_state IN ('not_observed', 'stored', 'partial', 'write_failed')
    ),
    CONSTRAINT request_traces_client_status_range CHECK (client_status BETWEEN 0 AND 599)
);
CREATE TABLE IF NOT EXISTS request_trace_stages (
    id BIGSERIAL PRIMARY KEY,
    trace_id BIGINT NOT NULL REFERENCES request_traces(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    stage TEXT NOT NULL,
    attempt_index INTEGER NOT NULL DEFAULT 0,
    view_name TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    reason TEXT NOT NULL,
    observed_bytes BIGINT NOT NULL DEFAULT 0,
    retained_bytes INTEGER NOT NULL DEFAULT 0,
    dropped_events INTEGER NOT NULL DEFAULT 0,
    redaction_unverified BOOLEAN NOT NULL DEFAULT FALSE,
    payload BYTEA,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT request_trace_stages_ordinal_positive CHECK (ordinal > 0),
    CONSTRAINT request_trace_stages_attempt_range CHECK (attempt_index BETWEEN 0 AND 1000),
    CONSTRAINT request_trace_stages_view_allowed CHECK (
        view_name IN ('', 'transmitted', 'decoded', 'wire', 'received', 'downstream')
    ),
    CONSTRAINT request_trace_stages_state_allowed CHECK (
        state IN ('not_observed', 'stored', 'truncated', 'unsupported', 'redaction_unverified', 'write_failed')
    ),
    CONSTRAINT request_trace_stages_payload_size CHECK (
        observed_bytes >= 0 AND retained_bytes BETWEEN 0 AND 1048576
        AND dropped_events >= 0
        AND octet_length(payload) <= 1048576
        AND (payload IS NULL OR (state IN ('stored', 'truncated', 'redaction_unverified') AND retained_bytes = octet_length(payload)))
    ),
    UNIQUE (trace_id, ordinal)
);`

// newRequestTraceExportSourceTx returns a source bound to a transaction whose
// search_path points at a private schema, so these tests never read or write the
// shared tables and never need cleanup.
func newRequestTraceExportSourceTx(t *testing.T) (*sql.Tx, *requestTraceExportSource) {
	t.Helper()
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	schema := pq.QuoteIdentifier(fmt.Sprintf("trace_export_source_%d", time.Now().UnixNano()))
	_, err = tx.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, requestTraceExportSourceTestSchema)
	require.NoError(t, err)
	return tx, &requestTraceExportSource{q: tx}
}

func insertRequestTrace(t *testing.T, tx *sql.Tx, traceID, family string, status int, createdAt time.Time, usageLogID *int64) {
	t.Helper()
	_, err := tx.ExecContext(context.Background(), `INSERT INTO request_traces
		(trace_id, route_family, inbound_endpoint, capture_state, client_status, created_at, usage_log_id)
		VALUES ($1, $2, '/v1/messages', 'stored', $3, $4, $5)`,
		traceID, family, status, createdAt.UTC(), usageLogID)
	require.NoError(t, err)
}

func insertRequestTraceStage(t *testing.T, tx *sql.Tx, traceID string, ordinal int, stage, view, state, reason string,
	observedBytes int64, retainedBytes int, droppedEvents int, redactionUnverified bool, payload any) {
	t.Helper()
	_, err := tx.ExecContext(context.Background(), `INSERT INTO request_trace_stages
		(trace_id, ordinal, stage, attempt_index, view_name, state, reason, observed_bytes, retained_bytes,
		 dropped_events, redaction_unverified, payload)
		SELECT id, $2, $3, 0, $4, $5, $6, $7, $8, $9, $10, $11 FROM request_traces WHERE trace_id = $1`,
		traceID, ordinal, stage, view, state, reason, observedBytes, retainedBytes, droppedEvents, redactionUnverified, payload)
	require.NoError(t, err)
}

func exportSourceTraceIDs(t *testing.T, tx *sql.Tx, ids []string) {
	t.Helper()
	for _, id := range ids {
		insertRequestTrace(t, tx, id, string(service.RequestTraceMessages), 200, time.Now().UTC(), nil)
	}
}

func walkRequestTraceExportSource(t *testing.T, source service.RequestTraceExportSource, filter service.RequestTraceExportFilter, pageSize int) []string {
	t.Helper()
	ctx := context.Background()
	collected := make([]string, 0)
	cursor := ""
	for {
		page, err := source.NextTraceIDs(ctx, filter, cursor, pageSize)
		require.NoError(t, err)
		if len(page) == 0 {
			require.LessOrEqual(t, len(page), pageSize)
			return collected
		}
		require.LessOrEqual(t, len(page), pageSize)
		require.Greater(t, page[0], cursor, "every page must resume strictly after the cursor")
		for index, id := range page {
			if index > 0 {
				require.Greater(t, id, page[index-1], "ids inside a page must be strictly ascending")
			}
		}
		collected = append(collected, page...)
		cursor = page[len(page)-1]
	}
}

func TestRequestTraceExportSourcePagesRealRowsByMonotonicCursor(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	now := time.Now().UTC()
	// Inserted out of order on purpose: the page order must come from trace_id.
	ids := []string{
		"0000000000000000000000000000000c",
		"0000000000000000000000000000000a",
		"0000000000000000000000000000000e",
		"0000000000000000000000000000000b",
		"0000000000000000000000000000000d",
	}
	var usageLogID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO usage_logs DEFAULT VALUES RETURNING id`).Scan(&usageLogID))
	insertRequestTrace(t, tx, ids[0], "chat_completions", 200, now, nil)
	insertRequestTrace(t, tx, ids[1], "messages", 200, now.Add(-72*time.Hour), nil)
	insertRequestTrace(t, tx, ids[2], "messages", 200, now, &usageLogID)
	insertRequestTrace(t, tx, ids[3], "messages", 200, now, nil)
	insertRequestTrace(t, tx, ids[4], "messages", 500, now, nil)

	all := walkRequestTraceExportSource(t, source, service.RequestTraceExportFilter{}, 2)
	require.Equal(t, []string{ids[1], ids[3], ids[0], ids[4], ids[2]}, all, "cursor paging must return every row exactly once in trace_id order")

	require.Equal(t, []string{ids[1], ids[3], ids[4], ids[2]},
		walkRequestTraceExportSource(t, source, service.RequestTraceExportFilter{RouteFamily: string(service.RequestTraceMessages)}, 2))
	statusOK := 200
	require.Equal(t, []string{ids[1], ids[3], ids[0], ids[2]},
		walkRequestTraceExportSource(t, source, service.RequestTraceExportFilter{ClientStatus: &statusOK}, 4))
	require.Equal(t, []string{ids[2]},
		walkRequestTraceExportSource(t, source, service.RequestTraceExportFilter{TraceID: ids[2]}, 4))

	linked := true
	require.Equal(t, []string{ids[2]},
		walkRequestTraceExportSource(t, source, service.RequestTraceExportFilter{UsageLinked: &linked}, 4))
	unlinked := false
	require.Equal(t, []string{ids[1], ids[3], ids[0], ids[4]},
		walkRequestTraceExportSource(t, source, service.RequestTraceExportFilter{UsageLinked: &unlinked}, 4))

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)
	require.Equal(t, []string{ids[3], ids[0], ids[4], ids[2]},
		walkRequestTraceExportSource(t, source, service.RequestTraceExportFilter{CreatedFrom: &from, CreatedTo: &to}, 4),
		"the created_at window must exclude the older row without dropping later ones")
}

func TestRequestTraceExportSourceReadsApprovedDetailFromRealRows(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	traceID := "0000000000000000000000000000000a"
	var usageLogID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO usage_logs DEFAULT VALUES RETURNING id`).Scan(&usageLogID))
	insertRequestTrace(t, tx, traceID, string(service.RequestTraceMessages), 200, time.Now().UTC(), &usageLogID)
	sentinel := []byte(exportSourceRealSentinel)
	insertRequestTraceStage(t, tx, traceID, 1, "client_entry", "decoded", "stored", "captured", 4096, len(sentinel), 0, false, sentinel)
	insertRequestTraceStage(t, tx, traceID, 2, "upstream_wire", "wire", "redaction_unverified", "unverified", 64, 3, 3, true, []byte{0xff, 0xfe, 0xfd})
	insertRequestTraceStage(t, tx, traceID, 3, "downstream_response", "", "not_observed", "not_observed", 0, 0, 0, false, nil)

	detail, available, err := source.ReadApprovedDetail(ctx, traceID)
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, traceID, detail.TraceID)
	require.Equal(t, string(service.RequestTraceMessages), detail.RouteFamily)
	require.Equal(t, "/v1/messages", detail.InboundEndpoint)
	require.Equal(t, string(service.RequestTraceStored), detail.CaptureState)
	require.Equal(t, 200, detail.ClientStatus)
	require.NotNil(t, detail.UsageLogID)
	require.Equal(t, usageLogID, *detail.UsageLogID)
	require.Len(t, detail.Stages, 3)
	require.Equal(t, exportSourceRealSentinel, detail.Stages[0].PayloadText)
	require.Empty(t, detail.Stages[1].PayloadText, "invalid UTF-8 bytes must be omitted, not mangled")
	require.True(t, detail.Stages[1].RedactionUnverified)
	require.Equal(t, 3, detail.Stages[1].DroppedEvents)
	require.Empty(t, detail.Stages[2].PayloadText)

	encoded, err := json.Marshal(detail)
	require.NoError(t, err)
	raw := string(encoded)
	require.Contains(t, raw, exportSourceRealSentinel)
	require.NotContains(t, raw, "�")
	for _, forbidden := range []string{"metadata", "url", "header", "authorization", "cookie", "query"} {
		require.NotContains(t, raw, forbidden, "exported detail must not expose %q", forbidden)
	}
}

func TestRequestTraceExportSourceCountsTraceDeletedAfterPaging(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	kept := "0000000000000000000000000000000a"
	removed := "0000000000000000000000000000000b"
	exportSourceTraceIDs(t, tx, []string{kept, removed})

	ids, err := source.NextTraceIDs(ctx, service.RequestTraceExportFilter{}, "", 10)
	require.NoError(t, err)
	require.Equal(t, []string{kept, removed}, ids)

	_, err = tx.ExecContext(ctx, `DELETE FROM request_traces WHERE trace_id = $1`, removed)
	require.NoError(t, err)

	detail, available, err := source.ReadApprovedDetail(ctx, removed)
	require.NoError(t, err)
	require.False(t, available, "a row deleted after paging is skipped, not resurrected")
	require.Equal(t, service.RequestTraceExportApprovedDetail{}, detail)

	keptDetail, available, err := source.ReadApprovedDetail(ctx, kept)
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, kept, keptDetail.TraceID)
	require.NotNil(t, keptDetail.Stages)
	require.Empty(t, keptDetail.Stages)
}

func TestRequestTraceExportSourceBoundsRealStageCount(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	traceID := "0000000000000000000000000000000a"
	exportSourceTraceIDs(t, tx, []string{traceID})
	_, err := tx.ExecContext(ctx, `INSERT INTO request_trace_stages
		(trace_id, ordinal, stage, view_name, state, reason)
		SELECT id, g, 'client_entry', 'decoded', 'not_observed', 'not_observed'
		FROM request_traces, generate_series(1, $1) AS g WHERE trace_id = $2`, requestTraceExportSourceMaxStages, traceID)
	require.NoError(t, err)

	detail, available, err := source.ReadApprovedDetail(ctx, traceID)
	require.NoError(t, err)
	require.True(t, available)
	require.Len(t, detail.Stages, requestTraceExportSourceMaxStages)

	insertRequestTraceStage(t, tx, traceID, requestTraceExportSourceMaxStages+1, "extra", "decoded", "not_observed", "not_observed", 0, 0, 0, false, nil)
	_, available, err = source.ReadApprovedDetail(ctx, traceID)
	require.ErrorIs(t, err, service.ErrRequestTraceExportLimit)
	require.False(t, available)
}

func TestNewRequestTraceExportSourceReadsRealDatabase(t *testing.T) {
	ctx := context.Background()
	_, err := integrationDB.ExecContext(ctx, requestTraceExportSourceTestSchema)
	require.NoError(t, err)
	// Unique per run and cleaned up below: the shared tables must not accumulate.
	traceID := fmt.Sprintf("%032x", time.Now().UnixNano())
	emptyID := fmt.Sprintf("%032x", time.Now().UnixNano()+1)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(),
			`DELETE FROM request_traces WHERE trace_id IN ($1, $2)`, traceID, emptyID)
	})
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO request_traces
		(trace_id, route_family, inbound_endpoint, capture_state, client_status, created_at)
		VALUES ($1, 'messages', '/v1/messages', 'stored', 201, NOW()), ($2, 'responses', '/v1/responses', 'partial', 503, NOW())`,
		traceID, emptyID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO request_trace_stages
		(trace_id, ordinal, stage, view_name, state, reason, observed_bytes, retained_bytes, payload)
		SELECT id, 1, 'client_entry', 'decoded', 'stored', 'captured', $2::bigint, $2::integer, $3 FROM request_traces WHERE trace_id = $1`,
		traceID, len(exportSourceRealSentinel), []byte(exportSourceRealSentinel))
	require.NoError(t, err)

	source := NewRequestTraceExportSource(integrationDB)
	ids, err := source.NextTraceIDs(ctx, service.RequestTraceExportFilter{TraceID: traceID}, "", 10)
	require.NoError(t, err)
	require.Equal(t, []string{traceID}, ids)

	detail, available, err := source.ReadApprovedDetail(ctx, traceID)
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, 201, detail.ClientStatus)
	require.Len(t, detail.Stages, 1)
	require.Equal(t, exportSourceRealSentinel, detail.Stages[0].PayloadText)

	empty, available, err := source.ReadApprovedDetail(ctx, emptyID)
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, 503, empty.ClientStatus)
	require.Equal(t, string(service.RequestTracePartial), empty.CaptureState)
	require.Empty(t, empty.Stages)
}

type inMemoryRequestTraceExportStore struct {
	mu    sync.Mutex
	tasks map[string]service.RequestTraceExportTask
}

func (s *inMemoryRequestTraceExportStore) Create(_ context.Context, task service.RequestTraceExportTask, maxInFlight int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil {
		s.tasks = map[string]service.RequestTraceExportTask{}
	}
	active := 0
	for _, current := range s.tasks {
		if current.InstanceID == task.InstanceID &&
			(current.Status == service.RequestTraceExportPending || current.Status == service.RequestTraceExportRunning) {
			active++
		}
	}
	if active >= maxInFlight {
		return service.ErrRequestTraceExportLimit
	}
	s.tasks[task.ID] = task
	return nil
}

func (s *inMemoryRequestTraceExportStore) Get(_ context.Context, id string) (service.RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return service.RequestTraceExportTask{}, service.ErrRequestTraceExportNotFound
	}
	return task, nil
}

func (s *inMemoryRequestTraceExportStore) Claim(_ context.Context, instanceID string) (service.RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, task := range s.tasks {
		if task.InstanceID != instanceID || task.Status != service.RequestTraceExportPending {
			continue
		}
		task.Status = service.RequestTraceExportRunning
		s.tasks[id] = task
		return task, nil
	}
	return service.RequestTraceExportTask{}, service.ErrRequestTraceExportNotFound
}

func (s *inMemoryRequestTraceExportStore) Finish(_ context.Context, task service.RequestTraceExportTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = task
	return nil
}

func (s *inMemoryRequestTraceExportStore) ListStale(_ context.Context, _ string, _ time.Time, _ int) ([]service.RequestTraceExportTask, error) {
	return nil, nil
}

func (s *inMemoryRequestTraceExportStore) Expired(_ context.Context, _ time.Time, _ int) ([]service.RequestTraceExportTask, error) {
	return nil, nil
}

func (s *inMemoryRequestTraceExportStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, id)
	return nil
}

// deletingRequestTraceExportSource removes a row the first time it is paged, which
// reproduces the documented "deleted while exporting" race deterministically.
type deletingRequestTraceExportSource struct {
	service.RequestTraceExportSource
	once   sync.Once
	delete func() error
}

func (s *deletingRequestTraceExportSource) NextTraceIDs(ctx context.Context, filter service.RequestTraceExportFilter, after string, limit int) ([]string, error) {
	ids, err := s.RequestTraceExportSource.NextTraceIDs(ctx, filter, after, limit)
	if err != nil {
		return nil, err
	}
	s.once.Do(func() { _ = s.delete() })
	return ids, nil
}

// TestRequestTraceExportServiceExportsRealPagesAndCountsDeletedTraces drives the
// real export service over the real source: 131 traces span more than one page,
// one of them is deleted mid-export, and the finished file must contain the
// approved detail of every surviving trace only.
func TestRequestTraceExportServiceExportsRealPagesAndCountsDeletedTraces(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	const totalTraces = 131
	_, err := tx.ExecContext(ctx, `INSERT INTO request_traces
		(trace_id, route_family, inbound_endpoint, capture_state, client_status, created_at)
		SELECT lpad(to_hex(g), 32, '0'), 'messages', '/v1/messages', 'stored', 200, NOW()
		FROM generate_series(1, $1) AS g`, totalTraces)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO request_trace_stages
		(trace_id, ordinal, stage, view_name, state, reason, observed_bytes, retained_bytes, payload)
		SELECT id, 1, 'client_entry', 'decoded', 'stored', 'captured', $1::bigint, $1::integer, $2
		FROM request_traces WHERE trace_id = lpad(to_hex(7), 32, '0')`,
		len(exportSourceRealSentinel), []byte(exportSourceRealSentinel))
	require.NoError(t, err)
	// A trace can disappear between paging and reading. The export must count it as
	// skipped instead of resurrecting it, so this row is deleted once it has already
	// been handed to the exporter.
	deletedID := fmt.Sprintf("%032x", 65)
	racingSource := &deletingRequestTraceExportSource{RequestTraceExportSource: source, delete: func() error {
		_, deleteErr := tx.ExecContext(context.Background(), `DELETE FROM request_traces WHERE trace_id = $1`, deletedID)
		return deleteErr
	}}

	dir := t.TempDir()
	store := &inMemoryRequestTraceExportStore{}
	exportSvc := service.NewRequestTraceExportService(store, racingSource, service.RequestTraceExportOptions{
		Enabled: true, SingleInstanceDeclared: true, InstanceID: "integration-instance", TempDir: dir,
	})
	actor := service.RequestTraceExportActor{AdminUserID: 7, SessionID: "admin-session"}
	task, err := exportSvc.CreateTask(ctx, actor, service.RequestTraceExportFilter{})
	require.NoError(t, err)
	finished, err := exportSvc.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, service.RequestTraceExportCompleted, finished.Status)
	require.Equal(t, int64(totalTraces-1), finished.RowsExported)
	require.Equal(t, int64(1), finished.RowsSkipped, "the trace deleted mid-export is skipped and counted")

	file, result, err := exportSvc.OpenDownload(ctx, actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	content, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	require.Equal(t, int64(totalTraces-1), result.RowsExported)

	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	require.Len(t, lines, totalTraces-1)
	seen := make(map[string]bool, len(lines))
	sentinelSeen := 0
	for _, line := range lines {
		var detail service.RequestTraceExportApprovedDetail
		require.NoError(t, json.Unmarshal([]byte(line), &detail))
		require.False(t, seen[detail.TraceID], "every page must be exported exactly once")
		seen[detail.TraceID] = true
		require.NotEqual(t, fmt.Sprintf("%032x", 65), detail.TraceID, "a deleted trace must never be exported")
		if len(detail.Stages) == 1 {
			sentinelSeen++
			require.Equal(t, exportSourceRealSentinel, detail.Stages[0].PayloadText)
		}
	}
	require.Len(t, seen, totalTraces-1)
	require.Equal(t, 1, sentinelSeen)
	require.NotContains(t, string(content), "metadata")
}
