//go:build unit

package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const (
	exportSourceTraceIDFirst    = "0123456789abcdef0123456789abcdef"
	exportSourceTraceIDSecond   = "11111111111111111111111111111111"
	exportSourceTraceIDThird    = "22222222222222222222222222222222"
	exportSourceInternalTraceID = int64(4242)
	// Non-ASCII on purpose: the approved detail carries UTF-8 payload text only.
	exportSourceSentinel = "合成哨兵-ünïcode-✓"
)

var errExportSourceStubQuery = errors.New("stub query must not be given rows")

// recordingTraceExportSourceQueryer captures the SQL text and bound arguments so
// tests can prove the cursor and filter values are parameterized, never formatted
// into the statement.
type recordingTraceExportSourceQueryer struct {
	queries []string
	args    [][]any
	err     error
}

func (q *recordingTraceExportSourceQueryer) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	q.queries = append(q.queries, query)
	q.args = append(q.args, append([]any(nil), args...))
	if q.err != nil {
		return nil, q.err
	}
	return nil, sql.ErrNoRows
}

func newRequestTraceExportSourceMock(t *testing.T) (service.RequestTraceExportSource, sqlmock.Sqlmock, *[]string) {
	t.Helper()
	queries := make([]string, 0, 2)
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(
		func(_, actual string) error {
			queries = append(queries, actual)
			return nil
		})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewRequestTraceExportSource(db), mock, &queries
}

func exportSourceEnvelopeRows(usageLogID any) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "route_family", "inbound_endpoint", "capture_state", "client_status", "usage_log_id"}).
		AddRow(exportSourceInternalTraceID, "messages", "/v1/messages", "stored", 200, usageLogID)
}

func exportSourceStageRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"ordinal", "stage", "attempt_index", "view_name", "state", "reason",
		"observed_bytes", "retained_bytes", "dropped_events", "redaction_unverified", "payload", "metadata"})
}

func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestNewRequestTraceExportSourceWithoutDatabaseFailsClosed(t *testing.T) {
	source := NewRequestTraceExportSource(nil)
	require.NotNil(t, source)

	ids, err := source.NextTraceIDs(context.Background(), service.RequestTraceExportFilter{}, "", 10)
	require.ErrorIs(t, err, errRequestTraceExportSourceUnavailable)
	require.Nil(t, ids)

	detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.ErrorIs(t, err, errRequestTraceExportSourceUnavailable)
	require.False(t, available)
	require.Equal(t, service.RequestTraceExportApprovedDetail{}, detail)
}

func TestRequestTraceExportSourceNextTraceIDsBindsCursorAndMetadataFilters(t *testing.T) {
	stub := &recordingTraceExportSourceQueryer{err: errExportSourceStubQuery}
	source := &requestTraceExportSource{q: stub}
	from := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	linked := true
	filter := service.RequestTraceExportFilter{
		TraceID: exportSourceTraceIDThird, RouteFamily: string(service.RequestTraceMessages), ClientStatus: requestTraceExportTestStatus(200),
		CreatedFrom: &from, CreatedTo: &to, UsageLinked: &linked,
	}

	_, err := source.NextTraceIDs(context.Background(), filter, exportSourceTraceIDSecond, 128)
	require.ErrorIs(t, err, errExportSourceStubQuery)
	require.Len(t, stub.queries, 1)

	normalized := normalizeSQLWhitespace(stub.queries[0])
	require.Contains(t, normalized, "FROM request_traces")
	require.Contains(t, normalized, "($1 = '' OR trace_id = $1)")
	require.Contains(t, normalized, "($2 = '' OR route_family = $2)")
	require.Contains(t, normalized, "($3::integer IS NULL OR client_status = $3)")
	require.Contains(t, normalized, "($4::timestamptz IS NULL OR created_at >= $4)")
	require.Contains(t, normalized, "($5::timestamptz IS NULL OR created_at < $5)")
	require.Contains(t, normalized, "($6::boolean IS NULL OR (usage_log_id IS NOT NULL) = $6)")
	require.Contains(t, normalized, "trace_id > $7")
	require.Contains(t, normalized, "ORDER BY trace_id LIMIT $8")
	// Selection is metadata only: no stage table, no payload and no wildcard.
	require.NotContains(t, normalized, "request_trace_stages")
	require.NotContains(t, normalized, "payload")
	require.NotContains(t, normalized, "metadata")
	require.NotContains(t, normalized, "SELECT *")
	// Values are bound, never interpolated into the statement text.
	require.NotContains(t, normalized, exportSourceTraceIDSecond)
	require.NotContains(t, normalized, exportSourceTraceIDThird)
	require.NotContains(t, normalized, "messages")

	require.Len(t, stub.args[0], 8)
	require.Equal(t, exportSourceTraceIDThird, stub.args[0][0])
	require.Equal(t, string(service.RequestTraceMessages), stub.args[0][1])
	status, ok := stub.args[0][2].(*int)
	require.True(t, ok)
	require.NotNil(t, status)
	require.Equal(t, 200, *status)
	require.Equal(t, from.UTC(), stub.args[0][3])
	require.Equal(t, to.UTC(), stub.args[0][4])
	require.Equal(t, true, stub.args[0][5])
	require.Equal(t, exportSourceTraceIDSecond, stub.args[0][6])
	require.Equal(t, 128, stub.args[0][7])
}

func TestRequestTraceExportSourceNextTraceIDsOmitsAbsentFilters(t *testing.T) {
	stub := &recordingTraceExportSourceQueryer{err: errExportSourceStubQuery}
	source := &requestTraceExportSource{q: stub}

	_, err := source.NextTraceIDs(context.Background(), service.RequestTraceExportFilter{}, "", 3)
	require.ErrorIs(t, err, errExportSourceStubQuery)
	require.Len(t, stub.args, 1)
	require.Len(t, stub.args[0], 8)
	require.Equal(t, "", stub.args[0][0])
	require.Equal(t, "", stub.args[0][1])
	status, ok := stub.args[0][2].(*int)
	require.True(t, ok)
	require.Nil(t, status, "an omitted status filter must bind SQL NULL")
	require.Nil(t, stub.args[0][3])
	require.Nil(t, stub.args[0][4])
	require.Nil(t, stub.args[0][5])
	require.Equal(t, "", stub.args[0][6])
}

func TestRequestTraceExportSourceNextTraceIDsRejectsUnboundedOrMalformedInput(t *testing.T) {
	from := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	earlier := from.Add(-time.Hour)
	cases := []struct {
		name   string
		filter service.RequestTraceExportFilter
		after  string
		limit  int
	}{
		{name: "zero limit", limit: 0},
		{name: "negative limit", limit: -1},
		{name: "limit above page bound", limit: requestTraceExportSourceMaxLimit + 1},
		{name: "malformed cursor", after: "not-a-trace-id", limit: 10},
		{name: "short cursor", after: "abc", limit: 10},
		{name: "uppercase cursor", after: "0123456789ABCDEF0123456789ABCDEF", limit: 10},
		{name: "malformed trace id filter", filter: service.RequestTraceExportFilter{TraceID: "zz"}, limit: 10},
		{name: "unsupported route family", filter: service.RequestTraceExportFilter{RouteFamily: "gemini"}, limit: 10},
		{name: "client status above range", filter: service.RequestTraceExportFilter{ClientStatus: requestTraceExportTestStatus(600)}, limit: 10},
		{name: "negative client status", filter: service.RequestTraceExportFilter{ClientStatus: requestTraceExportTestStatus(-1)}, limit: 10},
		{name: "inverted window", filter: service.RequestTraceExportFilter{CreatedFrom: &from, CreatedTo: &earlier}, limit: 10},
		{name: "empty window", filter: service.RequestTraceExportFilter{CreatedFrom: &from, CreatedTo: &from}, limit: 10},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stub := &recordingTraceExportSourceQueryer{}
			source := &requestTraceExportSource{q: stub}

			ids, err := source.NextTraceIDs(context.Background(), testCase.filter, testCase.after, testCase.limit)
			require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
			require.Nil(t, ids)
			require.Empty(t, stub.queries, "rejected input must not reach SQL")
		})
	}
}

func TestRequestTraceExportSourceNextTraceIDsReturnsAscendingPageAndCapsLimit(t *testing.T) {
	source, mock, queries := newRequestTraceExportSourceMock(t)
	mock.ExpectQuery("request trace export page").WillReturnRows(
		sqlmock.NewRows([]string{"trace_id"}).
			AddRow(exportSourceTraceIDFirst).
			AddRow(exportSourceTraceIDSecond))

	ids, err := source.NextTraceIDs(context.Background(), service.RequestTraceExportFilter{RouteFamily: string(service.RequestTraceChatCompletions)}, "", 2)
	require.NoError(t, err)
	require.Equal(t, []string{exportSourceTraceIDFirst, exportSourceTraceIDSecond}, ids)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, *queries, 1)
	require.Contains(t, normalizeSQLWhitespace((*queries)[0]), "ORDER BY trace_id LIMIT $8")
}

func TestRequestTraceExportSourceNextTraceIDsEmptyPageEndsIteration(t *testing.T) {
	source, mock, _ := newRequestTraceExportSourceMock(t)
	mock.ExpectQuery("request trace export page").WillReturnRows(sqlmock.NewRows([]string{"trace_id"}))

	ids, err := source.NextTraceIDs(context.Background(), service.RequestTraceExportFilter{}, exportSourceTraceIDThird, 128)
	require.NoError(t, err)
	require.Empty(t, ids)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRequestTraceExportSourceNextTraceIDsRefusesNonMonotonicPage(t *testing.T) {
	for _, testCase := range []struct {
		name string
		rows []string
	}{
		{name: "descending", rows: []string{exportSourceTraceIDThird, exportSourceTraceIDFirst}},
		{name: "repeated id", rows: []string{exportSourceTraceIDThird, exportSourceTraceIDThird}},
		{name: "malformed id", rows: []string{"not-a-trace-id"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			source, mock, _ := newRequestTraceExportSourceMock(t)
			rows := sqlmock.NewRows([]string{"trace_id"})
			for _, id := range testCase.rows {
				rows.AddRow(id)
			}
			mock.ExpectQuery("request trace export page").WillReturnRows(rows)

			ids, err := source.NextTraceIDs(context.Background(), service.RequestTraceExportFilter{}, "", 128)
			require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
			require.Nil(t, ids)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestRequestTraceExportSourceNextTraceIDsPropagatesQueryFailure(t *testing.T) {
	failure := errors.New("connection reset")
	stub := &recordingTraceExportSourceQueryer{err: failure}
	source := &requestTraceExportSource{q: stub}

	_, err := source.NextTraceIDs(context.Background(), service.RequestTraceExportFilter{}, "", 10)
	require.ErrorIs(t, err, failure)
}

func TestRequestTraceExportSourceIncludesBoundedRedactedAttemptFacts(t *testing.T) {
	source, mock, _ := newRequestTraceExportSourceMock(t)
	mock.ExpectQuery("request trace envelope").WillReturnRows(exportSourceEnvelopeRows(nil))
	mock.ExpectQuery("request trace stages").WillReturnRows(exportSourceStageRows().AddRow(
		1, "wire_attempt", 1, "", "not_observed", "wire_observed", int64(0), 0, 0, false, nil,
		[]byte(`{"method":"POST","url":"https://synthetic.example/v1/messages?api_key=%5BREDACTED%5D","protocol":"anthropic.messages","account_id":73,"status":200,"request_headers":{"Authorization":["[REDACTED]"]}}`)))
	detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.NoError(t, err)
	require.True(t, available)
	require.Len(t, detail.Stages, 1)
	require.NotNil(t, detail.Stages[0].Facts)
	require.Equal(t, "anthropic.messages", detail.Stages[0].Facts.Protocol)
	require.Equal(t, "[REDACTED]", detail.Stages[0].Facts.RequestHeaders.Get("Authorization"))
	encoded, err := json.Marshal(detail)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Bearer")
	require.NotContains(t, string(encoded), "metadata")
}

// A gateway_decision stage lives in the same JSONB column as transport facts.
// The export must reveal only its validated typed decision, never a raw map.
func TestRequestTraceExportSourceAcceptsAValidatedDecisionStage(t *testing.T) {
	source, mock, _ := newRequestTraceExportSourceMock(t)
	mock.ExpectQuery("request trace envelope").WillReturnRows(exportSourceEnvelopeRows(nil))
	mock.ExpectQuery("request trace stages").WillReturnRows(exportSourceStageRows().
		AddRow(1, "wire_attempt", 1, "wire", "not_observed", "wire_observed", int64(0), 0, 0, false, nil,
			[]byte(`{"method":"POST","protocol":"anthropic.messages"}`)).
		AddRow(2, "gateway_decision", 1, "", "not_observed", "model_rewritten", int64(0), 0, 0, false, nil,
			[]byte(`{"decision":"model_mapping","outcome":"rewritten","source":"account","sequence":2,"model_from":"gpt-5.3-codex","model_to":"gpt-5.3-codex-spark"}`)))

	detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.NoError(t, err)
	require.True(t, available)
	require.Len(t, detail.Stages, 2)
	require.NotNil(t, detail.Stages[0].Facts)
	require.Equal(t, "anthropic.messages", detail.Stages[0].Facts.Protocol)
	require.Nil(t, detail.Stages[1].Facts, "a decision is not transport facts and must not be disclosed as one")
	require.NotNil(t, detail.Stages[1].Decision)
	require.Equal(t, service.RequestTraceDecisionModelMapping, detail.Stages[1].Decision.Decision)
	require.Equal(t, "model_rewritten", detail.Stages[1].Reason)

	keys := jsonKeys(t, []byte(mustJSON(t, detail.Stages[1])))
	require.Contains(t, keys, "decision", "the validated decision must be present in the export")
	require.NotContains(t, keys, "metadata")
}

func TestRequestTraceExportSourceRefusesMalformedDecisionRows(t *testing.T) {
	cases := []struct {
		name    string
		stage   string
		encoded string
	}{
		{name: "decision stage without a decision", stage: "gateway_decision", encoded: `{}`},
		{name: "decision stage with an unknown field", stage: "gateway_decision",
			encoded: `{"decision":"route","outcome":"selected","source":"group","sequence":1,"url":"https://synthetic.example"}`},
		{name: "decision stage with an invalid enum", stage: "gateway_decision",
			encoded: `{"decision":"authorization","outcome":"selected","source":"group","sequence":1}`},
		{name: "decision keys on a transport stage", stage: "wire_attempt", encoded: `{"decision":"route"}`},
		{name: "non empty metadata on a body stage", stage: "client_entry", encoded: `{"method":"POST"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source, mock, _ := newRequestTraceExportSourceMock(t)
			mock.ExpectQuery("request trace envelope").WillReturnRows(exportSourceEnvelopeRows(nil))
			mock.ExpectQuery("request trace stages").WillReturnRows(exportSourceStageRows().
				AddRow(1, testCase.stage, 0, "", "not_observed", "bounded_code", int64(0), 0, 0, false, nil,
					[]byte(testCase.encoded)))

			detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
			require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
			require.False(t, available)
			require.Equal(t, service.RequestTraceExportApprovedDetail{}, detail)
		})
	}
}

func TestRequestTraceExportSourceReadApprovedDetailCountsDeletedTrace(t *testing.T) {
	source, mock, queries := newRequestTraceExportSourceMock(t)
	mock.ExpectQuery("request trace envelope").WillReturnRows(sqlmock.NewRows([]string{
		"id", "route_family", "inbound_endpoint", "capture_state", "client_status", "usage_log_id"}))

	detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.NoError(t, err)
	require.False(t, available)
	require.Equal(t, service.RequestTraceExportApprovedDetail{}, detail)
	require.NoError(t, mock.ExpectationsWereMet(), "a deleted trace must not trigger a stage read")
	require.Len(t, *queries, 1)
}

func TestRequestTraceExportSourceReadApprovedDetailAdmitsOnlyApprovedFields(t *testing.T) {
	source, mock, queries := newRequestTraceExportSourceMock(t)
	usageLogID := int64(9001)
	mock.ExpectQuery("request trace envelope").WillReturnRows(exportSourceEnvelopeRows(usageLogID))
	mock.ExpectQuery("request trace stages").WillReturnRows(exportSourceStageRows().
		AddRow(1, "client_entry", 0, "decoded", "stored", "captured", int64(2048), 1024, 0, false, []byte(exportSourceSentinel), "{}").
		AddRow(2, "upstream_wire", 1, "wire", "redaction_unverified", "unverified", int64(64), 4, 3, true, []byte{0xff, 0xfe, 0xfd}, "{}").
		AddRow(3, "downstream_response", 1, "", "not_observed", "not_observed", int64(0), 0, 0, false, nil, "{}"))

	detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, exportSourceTraceIDFirst, detail.TraceID)
	require.Equal(t, string(service.RequestTraceMessages), detail.RouteFamily)
	require.Equal(t, "/v1/messages", detail.InboundEndpoint)
	require.Equal(t, string(service.RequestTraceStored), detail.CaptureState)
	require.Equal(t, 200, detail.ClientStatus)
	require.NotNil(t, detail.UsageLogID)
	require.Equal(t, usageLogID, *detail.UsageLogID)
	require.Len(t, detail.Stages, 3)

	first := detail.Stages[0]
	require.Equal(t, 1, first.Ordinal)
	require.Equal(t, "client_entry", first.Stage)
	require.Equal(t, 0, first.AttemptIndex)
	require.Equal(t, "decoded", first.ViewName)
	require.Equal(t, string(service.RequestTraceStored), first.State)
	require.Equal(t, "captured", first.Reason)
	require.Equal(t, int64(2048), first.ObservedBytes)
	require.Equal(t, 1024, first.RetainedBytes)
	require.Equal(t, 0, first.DroppedEvents)
	require.False(t, first.RedactionUnverified)
	require.Equal(t, exportSourceSentinel, first.PayloadText)

	second := detail.Stages[1]
	require.True(t, second.RedactionUnverified)
	require.Equal(t, 3, second.DroppedEvents)
	require.Equal(t, "redaction_unverified", second.State)
	require.Empty(t, second.PayloadText, "invalid UTF-8 payload bytes must not be exposed lossily")

	third := detail.Stages[2]
	require.Empty(t, third.PayloadText)
	require.Equal(t, "not_observed", third.State)

	// The exported JSON is the approved allowlist itself: no arbitrary
	// metadata map or internal identifier may ride along.
	encoded, err := json.Marshal(detail)
	require.NoError(t, err)
	require.Equal(t, []string{"capture_state", "client_status", "inbound_endpoint", "route_family", "stages", "trace_id", "usage_log_id"}, jsonKeys(t, encoded))
	require.Equal(t, []string{"attempt_index", "dropped_events", "observed_bytes", "ordinal", "payload_text", "reason",
		"redaction_unverified", "retained_bytes", "stage", "state", "view_name"}, jsonKeys(t, []byte(mustJSON(t, detail.Stages[0]))))
	raw := string(encoded)
	for _, forbidden := range []string{"metadata", "url", "header", "authorization", "cookie", "api_key", "query", "internal", "created_at"} {
		require.NotContains(t, raw, forbidden, "approved detail must not expose %q", forbidden)
	}
	require.NotContains(t, raw, "�", "payload text must never be silently lossily re-encoded")
	require.Contains(t, raw, exportSourceSentinel)

	// Only allowlisted stage columns are read from the database.
	stagesQuery := normalizeSQLWhitespace((*queries)[1])
	require.Contains(t, stagesQuery, "FROM request_trace_stages")
	require.Contains(t, stagesQuery, "ORDER BY ordinal")
	require.Contains(t, stagesQuery, "substring(metadata::text from 1 for $4::integer)")
	require.NotContains(t, stagesQuery, "SELECT *")
	require.NotContains(t, stagesQuery, "request_traces r")
}

func TestRequestTraceExportSourceReadApprovedDetailKeepsEmptyStageListArray(t *testing.T) {
	source, mock, _ := newRequestTraceExportSourceMock(t)
	mock.ExpectQuery("request trace envelope").WillReturnRows(exportSourceEnvelopeRows(nil))
	mock.ExpectQuery("request trace stages").WillReturnRows(exportSourceStageRows())

	detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.NoError(t, err)
	require.True(t, available)
	require.NotNil(t, detail.Stages)
	require.Empty(t, detail.Stages)
	require.Nil(t, detail.UsageLogID)

	encoded, err := json.Marshal(detail)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"stages":[]`, "an unlinked trace with no stages still serializes as an empty array")
	require.NotContains(t, string(encoded), "usage_log_id")
}

func TestRequestTraceExportSourceReadApprovedDetailRejectsMalformedTraceID(t *testing.T) {
	stub := &recordingTraceExportSourceQueryer{}
	source := &requestTraceExportSource{q: stub}

	detail, available, err := source.ReadApprovedDetail(context.Background(), "not-a-trace-id")
	require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
	require.False(t, available)
	require.Equal(t, service.RequestTraceExportApprovedDetail{}, detail)
	require.Empty(t, stub.queries)
}

func TestRequestTraceExportSourceReadApprovedDetailPropagatesQueryFailure(t *testing.T) {
	failure := errors.New("connection reset")
	stub := &recordingTraceExportSourceQueryer{err: failure}
	source := &requestTraceExportSource{q: stub}

	_, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.ErrorIs(t, err, failure)
	require.False(t, available)
}

func TestRequestTraceExportSourceReadApprovedDetailBoundsPayloadSize(t *testing.T) {
	source, mock, _ := newRequestTraceExportSourceMock(t)
	mock.ExpectQuery("request trace envelope").WillReturnRows(exportSourceEnvelopeRows(nil))
	mock.ExpectQuery("request trace stages").WillReturnRows(exportSourceStageRows().
		AddRow(1, "client_entry", 0, "decoded", "stored", "captured", int64(service.RequestTraceStagePayloadLimit+1), 0, 0, false,
			bytes.Repeat([]byte("a"), service.RequestTraceStagePayloadLimit+1), "{}"))

	detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.ErrorIs(t, err, service.ErrRequestTraceExportLimit)
	require.False(t, available)
	require.Equal(t, service.RequestTraceExportApprovedDetail{}, detail)
}

func TestRequestTraceExportSourceReadApprovedDetailBoundsStageCount(t *testing.T) {
	source, mock, _ := newRequestTraceExportSourceMock(t)
	rows := exportSourceStageRows()
	for ordinal := 1; ordinal <= requestTraceExportSourceMaxStages+1; ordinal++ {
		rows.AddRow(ordinal, "client_entry", 0, "decoded", "stored", "captured", int64(1), 1, 0, false, []byte("a"), "{}")
	}
	mock.ExpectQuery("request trace envelope").WillReturnRows(exportSourceEnvelopeRows(nil))
	mock.ExpectQuery("request trace stages").WillReturnRows(rows)

	_, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.ErrorIs(t, err, service.ErrRequestTraceExportLimit)
	require.False(t, available)
}

func TestRequestTraceExportSourceReadApprovedDetailBoundsTotalTracePayload(t *testing.T) {
	source, mock, _ := newRequestTraceExportSourceMock(t)
	rows := exportSourceStageRows()
	perStage := bytes.Repeat([]byte("a"), service.RequestTraceStagePayloadLimit)
	// service.RequestTraceTotalBodyLimit admits eight full stages and rejects the ninth.
	for ordinal := 1; ordinal <= service.RequestTraceTotalBodyLimit/service.RequestTraceStagePayloadLimit+1; ordinal++ {
		rows.AddRow(ordinal, "client_entry", 0, "decoded", "stored", "captured", int64(len(perStage)), len(perStage), 0, false, perStage, "{}")
	}
	mock.ExpectQuery("request trace envelope").WillReturnRows(exportSourceEnvelopeRows(nil))
	mock.ExpectQuery("request trace stages").WillReturnRows(rows)

	_, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.ErrorIs(t, err, service.ErrRequestTraceExportLimit)
	require.False(t, available)
}

func TestRequestTraceExportSourceReadApprovedDetailAdmitsExactTotalTraceBudget(t *testing.T) {
	source, mock, _ := newRequestTraceExportSourceMock(t)
	rows := exportSourceStageRows()
	perStage := bytes.Repeat([]byte("a"), service.RequestTraceStagePayloadLimit)
	for ordinal := 1; ordinal <= service.RequestTraceTotalBodyLimit/service.RequestTraceStagePayloadLimit; ordinal++ {
		rows.AddRow(ordinal, "client_entry", 0, "decoded", "stored", "captured", int64(len(perStage)), len(perStage), 0, false, perStage, "{}")
	}
	mock.ExpectQuery("request trace envelope").WillReturnRows(exportSourceEnvelopeRows(nil))
	mock.ExpectQuery("request trace stages").WillReturnRows(rows)

	detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.NoError(t, err)
	require.True(t, available)
	require.Len(t, detail.Stages, service.RequestTraceTotalBodyLimit/service.RequestTraceStagePayloadLimit)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

// requestTraceExportTestStatus 构造可选的 client_status 筛选值：nil 与 0 是不同含义。
func requestTraceExportTestStatus(value int) *int {
	return &value
}
