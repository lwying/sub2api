//go:build unit

package repository

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"sort"
	"strings"
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

var (
	exportSourceCreatedAt    = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	exportSourceCleanupAfter = exportSourceCreatedAt.Add(30 * 24 * time.Hour)
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

// 返回具体类型：本文件的测试直接打具体实现，接口一致性由 var _ 断言保证。
func newRequestTraceExportSourceMock(t *testing.T) (*requestTraceExportSource, sqlmock.Sqlmock, *[]string) {
	t.Helper()
	queries := make([]string, 0, 2)
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(
		func(_, actual string) error {
			queries = append(queries, actual)
			return nil
		})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &requestTraceExportSource{q: db}, mock, &queries
}

func exportSourceEnvelopeRows(usageLogID any) *sqlmock.Rows {
	return exportSourceEnvelopeRowsWithPlatforms(usageLogID, nil)
}

func exportSourceEnvelopeRowsWithPlatforms(usageLogID, platforms any) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "route_family", "inbound_endpoint", "capture_state", "client_status", "usage_log_id",
		"created_at", "completed_at", "cleanup_after", "group_id", "requested_model", "observed_platforms", "user_id", "api_key_id"}).
		AddRow(exportSourceInternalTraceID, "messages", "/v1/messages", "stored", 200, usageLogID,
			exportSourceCreatedAt, nil, exportSourceCleanupAfter, nil, nil, platforms, nil, nil)
}

// 导出详情与管理员详情披露同款的请求时平台历史：非 NULL 的去重平台按原样导出；
// NULL 保持缺席（未知）；损坏的数组按"不可用"处理，绝不尽力解释后写进文件。
func TestRequestTraceExportSourceReadsEnvelopePlatformHistory(t *testing.T) {
	source, mock, _ := newRequestTraceExportSourceMock(t)
	mock.ExpectQuery("request trace envelope").WillReturnRows(
		exportSourceEnvelopeRowsWithPlatforms(nil, []byte(`["anthropic","opencode_go"]`)))
	mock.ExpectQuery("request trace stages").WillReturnRows(exportSourceStageRows())

	detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, []string{"anthropic", "opencode_go"}, detail.ObservedPlatforms)

	for _, malformed := range []string{`[]`, `["bad platform"]`, `["anthropic","anthropic"]`, `{"a":1}`} {
		malformedSource, malformedMock, _ := newRequestTraceExportSourceMock(t)
		malformedMock.ExpectQuery("request trace envelope").WillReturnRows(
			exportSourceEnvelopeRowsWithPlatforms(nil, []byte(malformed)))
		detail, available, err := malformedSource.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
		require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord, "malformed platform history %s must fail closed", malformed)
		require.False(t, available)
		require.Equal(t, service.RequestTraceExportApprovedDetail{}, detail)
	}
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

	ids, next, err := source.NextTracePage(context.Background(), service.RequestTraceExportFilter{}, "", 10)
	require.ErrorIs(t, err, errRequestTraceExportSourceUnavailable)
	require.Nil(t, ids)
	require.Empty(t, next)

	detail, available, err := source.ReadApprovedDetail(context.Background(), exportSourceTraceIDFirst)
	require.ErrorIs(t, err, errRequestTraceExportSourceUnavailable)
	require.False(t, available)
	require.Equal(t, service.RequestTraceExportApprovedDetail{}, detail)
}

func TestRequestTraceExportSourceNextTracePageBindsCursorAndMetadataFilters(t *testing.T) {
	stub := &recordingTraceExportSourceQueryer{err: errExportSourceStubQuery}
	source := &requestTraceExportSource{q: stub}
	from := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	linked := true
	filter := service.RequestTraceExportFilter{
		TraceID: exportSourceTraceIDThird, RouteFamily: string(service.RequestTraceMessages), ClientStatus: requestTraceExportTestStatus(200),
		CreatedFrom: &from, CreatedTo: &to, UsageLinked: &linked,
	}
	cursor := "v1:1759132800000000:4242"

	_, _, err := source.NextTracePage(context.Background(), filter, cursor, 128)
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
	// 次序键必须与 Trace 列表逐字一致，游标按 (created_at, 内部 id) 行构造比较。
	require.Contains(t, normalized, "(created_at, id) < ($21::timestamptz, $22::bigint)")
	require.Contains(t, normalized, "ORDER BY created_at DESC, id DESC LIMIT $23")
	// 账号筛选只匹配 wire_attempt 的类型化事实；平台筛选与列表同源，取 wire_attempt
	// 阶段 platform 与信封 observed_platforms 的并集（含"选中但未发出"的错误 Trace）。
	require.Contains(t, normalized, "s.stage = 'wire_attempt'")
	require.Contains(t, normalized, "jsonb_build_object('account_id', $8::bigint)")
	require.Contains(t, normalized, "s2.stage = 'wire_attempt'")
	require.Contains(t, normalized, "jsonb_build_object('platform', $13::text)")
	require.Contains(t, normalized, "OR observed_platforms @> jsonb_build_array($13::text)")
	require.Contains(t, normalized, "AND observed_platforms IS NULL")
	require.NotContains(t, normalized, "payload")
	require.NotContains(t, normalized, "SELECT *")
	require.NotContains(t, normalized, "SELECT s.metadata")
	// 游标与筛选值只会被绑定，绝不拼进语句。
	require.NotContains(t, normalized, cursor)
	require.NotContains(t, normalized, exportSourceTraceIDThird)
	require.NotContains(t, normalized, "messages")

	require.Len(t, stub.args[0], 23)
	require.Equal(t, exportSourceTraceIDThird, stub.args[0][0])
	require.Equal(t, string(service.RequestTraceMessages), stub.args[0][1])
	status, ok := stub.args[0][2].(*int)
	require.True(t, ok)
	require.NotNil(t, status)
	require.Equal(t, 200, *status)
	require.Equal(t, from.UTC(), stub.args[0][3])
	require.Equal(t, to.UTC(), stub.args[0][4])
	require.Equal(t, true, stub.args[0][5])
	// 未给出的条件一律绑定 NULL：它们不得退化成"匹配全部"以外的任何含义。
	require.Nil(t, stub.args[0][6], "an absent usage_log_id must bind SQL NULL")
	require.Nil(t, stub.args[0][7], "an absent account_id must bind SQL NULL")
	for index := 8; index <= 19; index++ {
		require.Nil(t, stub.args[0][index])
	}
	require.Equal(t, time.UnixMicro(1759132800000000).UTC(), stub.args[0][20])
	require.Equal(t, int64(4242), stub.args[0][21])
	require.Equal(t, 128, stub.args[0][22])
}

// 所有可选条件缺席时必须绑定 SQL NULL，游标是其后第一个非空参数；首屏游标本身
// 也必须是 NULL，而不是空串或某个默认值。
func TestRequestTraceExportSourceNextTracePageOmitsAbsentFilters(t *testing.T) {
	stub := &recordingTraceExportSourceQueryer{err: errExportSourceStubQuery}
	source := &requestTraceExportSource{q: stub}

	_, _, err := source.NextTracePage(context.Background(), service.RequestTraceExportFilter{}, "", 3)
	require.ErrorIs(t, err, errExportSourceStubQuery)
	require.Len(t, stub.args, 1)
	require.Len(t, stub.args[0], 23)
	require.Equal(t, "", stub.args[0][0])
	require.Equal(t, "", stub.args[0][1])
	status, ok := stub.args[0][2].(*int)
	require.True(t, ok)
	require.Nil(t, status, "an omitted status filter must bind SQL NULL")
	for index := 3; index <= 19; index++ {
		require.Nil(t, stub.args[0][index], "an omitted optional filter must bind SQL NULL")
	}
	require.Nil(t, stub.args[0][20], "首屏没有游标时必须绑定 SQL NULL")
	require.Nil(t, stub.args[0][21])
	require.Equal(t, 3, stub.args[0][22])
}

func TestRequestTraceExportSourceNextTracePagePropagatesQueryFailure(t *testing.T) {
	failure := errors.New("connection reset")
	stub := &recordingTraceExportSourceQueryer{err: failure}
	source := &requestTraceExportSource{q: stub}

	ids, next, err := source.NextTracePage(context.Background(), service.RequestTraceExportFilter{}, "", 10)
	require.ErrorIs(t, err, failure)
	require.Nil(t, ids)
	require.Empty(t, next)
}

// 所选 ID 必须以驱动可序列化的数组绑定；裸 []string 会被 lib/pq 在转换 $15 时
// 拒绝（"unsupported type []string"）。sqlmock 不执行 SQL，所以这里断言绑定形态。
func TestRequestTraceExportSourceNextTracePageBindsSelectedIDsAsDriverArray(t *testing.T) {
	stub := &recordingTraceExportSourceQueryer{err: errExportSourceStubQuery}
	source := &requestTraceExportSource{q: stub}

	_, _, err := source.NextTracePage(context.Background(),
		service.RequestTraceExportFilter{TraceIDs: []string{exportSourceTraceIDFirst, exportSourceTraceIDThird}}, "", 10)
	require.ErrorIs(t, err, errExportSourceStubQuery)
	require.Len(t, stub.args[0], 23)

	valuer, ok := stub.args[0][19].(driver.Valuer)
	require.True(t, ok, "所选 ID 必须绑成驱动可序列化的数组，而不是裸 []string")
	value, err := valuer.Value()
	require.NoError(t, err)
	require.NotNil(t, value, "有选中 ID 时不得绑成 NULL")
	rendered, ok := value.(string)
	require.True(t, ok)
	require.Contains(t, rendered, exportSourceTraceIDFirst)
	require.Contains(t, rendered, exportSourceTraceIDThird)
}

func TestRequestTraceExportSourceNextTracePageReturnsListOrderAndNextCursor(t *testing.T) {
	source, mock, queries := newRequestTraceExportSourceMock(t)
	newest := time.Date(2026, 9, 29, 12, 3, 0, 0, time.UTC)
	tied := newest.Add(-2 * time.Minute)
	mock.ExpectQuery("request trace export page").WillReturnRows(
		sqlmock.NewRows([]string{"trace_id", "created_at", "id"}).
			AddRow(exportSourceTraceIDSecond, newest, int64(9)).
			AddRow(exportSourceTraceIDFirst, newest, int64(4)).
			AddRow(exportSourceTraceIDThird, tied, int64(2)))

	ids, next, err := source.NextTracePage(context.Background(), service.RequestTraceExportFilter{}, "", 3)
	require.NoError(t, err)
	require.Equal(t, []string{exportSourceTraceIDSecond, exportSourceTraceIDFirst, exportSourceTraceIDThird}, ids,
		"同一 created_at 上内部 id 更大者在前，与列表的 id DESC 一致")
	require.Equal(t, encodeRequestTraceExportCursor(tied, 2), next)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Contains(t, normalizeSQLWhitespace((*queries)[0]), "ORDER BY created_at DESC, id DESC")
}

func TestRequestTraceExportSourceNextTracePageEmptyPageEndsIteration(t *testing.T) {
	source, mock, _ := newRequestTraceExportSourceMock(t)
	mock.ExpectQuery("request trace export page").WillReturnRows(
		sqlmock.NewRows([]string{"trace_id", "created_at", "id"}))

	ids, next, err := source.NextTracePage(context.Background(), service.RequestTraceExportFilter{}, "v1:1:1", 128)
	require.NoError(t, err)
	require.Empty(t, ids)
	require.Empty(t, next, "空页表示遍历结束，不返回游标")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRequestTraceExportSourceNextTracePageRefusesNonDescendingPage(t *testing.T) {
	newest := time.Date(2026, 9, 29, 12, 3, 0, 0, time.UTC)
	older := newest.Add(-time.Minute)
	cases := []struct {
		name string
		rows *sqlmock.Rows
	}{
		{name: "ascending created_at", rows: sqlmock.NewRows([]string{"trace_id", "created_at", "id"}).
			AddRow(exportSourceTraceIDFirst, older, int64(1)).
			AddRow(exportSourceTraceIDSecond, newest, int64(2))},
		{name: "ascending internal id on a tied created_at", rows: sqlmock.NewRows([]string{"trace_id", "created_at", "id"}).
			AddRow(exportSourceTraceIDFirst, newest, int64(1)).
			AddRow(exportSourceTraceIDSecond, newest, int64(2))},
		{name: "repeated row", rows: sqlmock.NewRows([]string{"trace_id", "created_at", "id"}).
			AddRow(exportSourceTraceIDFirst, newest, int64(5)).
			AddRow(exportSourceTraceIDFirst, newest, int64(5))},
		{name: "malformed id", rows: sqlmock.NewRows([]string{"trace_id", "created_at", "id"}).
			AddRow("not-a-trace-id", newest, int64(5))},
		{name: "non positive internal id", rows: sqlmock.NewRows([]string{"trace_id", "created_at", "id"}).
			AddRow(exportSourceTraceIDFirst, newest, int64(0))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source, mock, _ := newRequestTraceExportSourceMock(t)
			mock.ExpectQuery("request trace export page").WillReturnRows(testCase.rows)

			ids, next, err := source.NextTracePage(context.Background(), service.RequestTraceExportFilter{}, "", 128)
			require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
			require.Nil(t, ids)
			require.Empty(t, next)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestRequestTraceExportSourceNextTracePageRejectsUnboundedOrMalformedInput(t *testing.T) {
	cases := []struct {
		name   string
		filter service.RequestTraceExportFilter
		after  string
		limit  int
	}{
		{name: "zero limit", limit: 0},
		{name: "negative limit", limit: -1},
		{name: "limit above page bound", limit: requestTraceExportSourceMaxLimit + 1},
		{name: "malformed cursor", after: "not-a-cursor", limit: 10},
		{name: "fixed width cursor is not a cursor", after: exportSourceTraceIDFirst, limit: 10},
		{name: "malformed trace id filter", filter: service.RequestTraceExportFilter{TraceID: "zz"}, limit: 10},
		{name: "unsupported route family", filter: service.RequestTraceExportFilter{RouteFamily: "gemini"}, limit: 10},
		{name: "client status above range", filter: service.RequestTraceExportFilter{ClientStatus: requestTraceExportTestStatus(600)}, limit: 10},
		{name: "negative client status", filter: service.RequestTraceExportFilter{ClientStatus: requestTraceExportTestStatus(-1)}, limit: 10},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stub := &recordingTraceExportSourceQueryer{}
			source := &requestTraceExportSource{q: stub}

			ids, next, err := source.NextTracePage(context.Background(), testCase.filter, testCase.after, testCase.limit)
			require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
			require.Nil(t, ids)
			require.Empty(t, next)
			require.Empty(t, stub.queries, "rejected input must not reach SQL")
		})
	}
}

// 空时间窗（from >= to）是合法查询的空结果，不是非法输入：返回空页而不是错误，
// 且不触达 SQL。"查询全部"的内部上界（任务创建时刻）与一个落在未来的下界就会
// 形成这种区间，导出必须能以 0 行、清单"完整"收尾。
func TestRequestTraceExportSourceNextTracePageReturnsEmptyPageForNonPositiveWindow(t *testing.T) {
	from := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name  string
		from  time.Time
		until time.Time
	}{
		{name: "inverted window", from: from, until: from.Add(-time.Hour)},
		{name: "empty window", from: from, until: from},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stub := &recordingTraceExportSourceQueryer{}
			source := &requestTraceExportSource{q: stub}
			filter := service.RequestTraceExportFilter{CreatedFrom: &testCase.from, CreatedTo: &testCase.until}

			ids, next, err := source.NextTracePage(context.Background(), filter, "", 10)
			require.NoError(t, err)
			require.Nil(t, ids)
			require.Empty(t, next)
			require.Empty(t, stub.queries, "an empty window must not reach SQL")
		})
	}
}

func TestRequestTraceExportCursorRoundTripsAndRejectsMalformedTokens(t *testing.T) {
	createdAt := time.Date(2026, 9, 29, 12, 3, 0, 123456000, time.UTC)
	token := encodeRequestTraceExportCursor(createdAt, 4242)
	decoded, ok := decodeRequestTraceExportCursor(token)
	require.True(t, ok)
	require.Equal(t, createdAt.UnixMicro(), decoded.createdAtMicros)
	require.Equal(t, int64(4242), decoded.internalID)
	// 游标自证次序键：(created_at, 内部 id) 严格更小者才算"在游标之后"。
	require.True(t, decoded.strictlyBefore(createdAt.Add(-time.Microsecond), 9999))
	require.False(t, decoded.strictlyBefore(createdAt, 4242))
	require.False(t, decoded.strictlyBefore(createdAt, 4243))
	require.True(t, decoded.strictlyBefore(createdAt, 4241))

	for _, malformed := range []string{
		"", "v1:", "v1:1", "v1:1:", "v1::1", "v1:1:2:3", "v1:0:1", "v1:1:0", "v1:-1:1", "v1:1:+2",
		"v1: 1:1", "v2:1:1", exportSourceTraceIDFirst, strings.Repeat("9", requestTraceExportCursorMaxLen+1),
	} {
		_, ok := decodeRequestTraceExportCursor(malformed)
		require.False(t, ok, "token %q 必须被拒绝", malformed)
	}
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
		"id", "route_family", "inbound_endpoint", "capture_state", "client_status", "usage_log_id",
		"created_at", "completed_at", "cleanup_after", "group_id", "requested_model", "observed_platforms"}))

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
	require.Equal(t, exportSourceCreatedAt, detail.CreatedAt)
	require.Nil(t, detail.CompletedAt)
	// 与详情同一条规则：已关联使用记录的 Trace 不单独披露 cleanup_after。
	require.Nil(t, detail.CleanupAfter)
	require.Nil(t, detail.GroupID)
	require.Empty(t, detail.RequestedModel)
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
	// 键集合就是"详情同款字段契约"本身：详情恒定输出的信封键，导出也恒定输出，
	// 即便这一行的 completed_at / cleanup_after 是 nil（详情同样输出 null）。
	require.Equal(t, []string{"api_key_id", "capture_state", "cleanup_after", "client_status", "completed_at", "created_at", "inbound_endpoint", "route_family", "stages", "trace_id", "usage_log_id", "user_id"}, jsonKeys(t, encoded))
	require.Equal(t, []string{"attempt_index", "dropped_events", "observed_bytes", "ordinal", "payload_text", "reason",
		"redaction_unverified", "retained_bytes", "stage", "state", "view_name"}, jsonKeys(t, []byte(mustJSON(t, detail.Stages[0]))))
	raw := string(encoded)
	for _, forbidden := range []string{"metadata", "url", "header", "authorization", "cookie", "api_key_name", "query", "internal"} {
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
	// 未关联 usage 的 Trace，其 usage_log_id 与详情一样是显式的 null，而不是缺键。
	require.Contains(t, string(encoded), `"usage_log_id":null`)
	require.NotContains(t, string(encoded), "group_id")
	require.NotContains(t, string(encoded), "requested_model")
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
