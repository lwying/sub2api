//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// The parent owns migration 258; this local schema tests the repository SQL until
// that migration is available in this isolated worktree.
func newRequestTraceTestTx(t *testing.T) *sql.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	schema := pq.QuoteIdentifier(fmt.Sprintf("trace_repo_%d", time.Now().UnixNano()))
	_, err = tx.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
CREATE TABLE usage_logs (id BIGINT PRIMARY KEY);
CREATE TABLE request_traces (
    id BIGSERIAL PRIMARY KEY,
    trace_id TEXT NOT NULL UNIQUE,
    route_family TEXT NOT NULL,
    inbound_endpoint TEXT NOT NULL,
    capture_state TEXT NOT NULL DEFAULT 'not_observed',
    client_status INTEGER NOT NULL DEFAULT 0,
    usage_log_id BIGINT UNIQUE REFERENCES usage_logs(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    cleanup_after TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '30 days'),
    -- 与 265 之后的信封一致：请求时分组与客户端模型都是可空的"未观察到"事实。
    group_id BIGINT,
    requested_model TEXT
);
CREATE TABLE request_trace_stages (
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
    redaction_unverified BOOLEAN NOT NULL DEFAULT false,
    payload BYTEA,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(trace_id, ordinal)
);`)
	require.NoError(t, err)
	return tx
}

func TestRequestTraceRepositoryPreservesUnlinkedRecordsUntilCleanup(t *testing.T) {
	ctx := context.Background()
	tx := newRequestTraceTestTx(t)
	repo := &requestTraceRepository{q: tx}
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	input := service.RequestTrace{TraceID: id, RouteFamily: service.RequestTraceMessages, InboundEndpoint: "/v1/messages", CreatedAt: created, ClientStatus: 401}

	stored, err := repo.CreateRequestTrace(ctx, input)
	require.NoError(t, err)
	require.Equal(t, id, stored.TraceID)
	require.NotNil(t, stored.CleanupAfter)
	require.Equal(t, created.Add(30*24*time.Hour), *stored.CleanupAfter)
	again, err := repo.CreateRequestTrace(ctx, input)
	require.NoError(t, err)
	require.Equal(t, stored.ID, again.ID, "retry must not create a second trace")

	stage := service.RequestTraceStage{TraceID: id, Ordinal: 1, Stage: "client_entry", State: service.RequestTraceNotObserved, Reason: "auth_rejected"}
	createdStage, err := repo.AppendRequestTraceStage(ctx, stage)
	require.NoError(t, err)
	duplicate, err := repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{
		TraceID: id, Ordinal: 1, Stage: "client_entry", State: service.RequestTraceNotObserved,
		Reason: "different incoming value must not masquerade as the stored stage",
	})
	require.NoError(t, err)
	require.Equal(t, createdStage.ID, duplicate.ID)
	require.Equal(t, "auth_rejected", duplicate.Reason)
	detail, err := repo.GetRequestTrace(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, detail)
	require.Len(t, detail.Stages, 1)
	require.Equal(t, createdStage.ID, detail.Stages[0].ID)
	require.Equal(t, service.RequestTraceNotObserved, detail.Stages[0].State)

	list, count, err := repo.ListRequestTraces(ctx, service.RequestTraceListFilter{RouteFamily: service.RequestTraceMessages, Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	require.Len(t, list, 1)

	// Day 30 is a cleanup candidate, not an exact read cutoff.
	detail, err = repo.GetRequestTrace(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, detail)
	deleted, err := repo.DeleteExpiredUnlinkedRequestTraces(ctx, created.Add(30*24*time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	detail, err = repo.GetRequestTrace(ctx, id)
	require.NoError(t, err)
	require.Nil(t, detail)
}

func TestRequestTraceRepositoryPersistsTypedRedactedFactsAcrossDetailRead(t *testing.T) {
	ctx := context.Background()
	repo := &requestTraceRepository{q: newRequestTraceTestTx(t)}
	id := "efefefefefefefefefefefefefefefef"
	_, err := repo.CreateRequestTrace(ctx, service.RequestTrace{
		TraceID: id, RouteFamily: service.RequestTraceMessages, InboundEndpoint: "/v1/messages",
	})
	require.NoError(t, err)
	url, err := url.Parse("https://synthetic.example/v1/messages?api_key=synthetic-query-secret&source=trace")
	require.NoError(t, err)
	facts := service.NewRequestTraceAttemptFacts("POST", url,
		http.Header{"Authorization": {"Bearer synthetic-header-secret"}, "X-Visible": {"benign"}},
		http.Header{"Content-Type": {"application/json"}}, 71, "synthetic-model", "anthropic.messages", "", 200, nil, nil)
	_, err = repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{
		TraceID: id, Ordinal: 1, Stage: "wire_attempt", AttemptIndex: 1,
		State: service.RequestTraceNotObserved, Reason: "wire_observed", Metadata: &facts,
	})
	require.NoError(t, err)
	detail, err := repo.GetRequestTrace(ctx, id)
	require.NoError(t, err)
	require.Len(t, detail.Stages, 1)
	require.NotNil(t, detail.Stages[0].Metadata)
	require.Equal(t, "[REDACTED]", detail.Stages[0].Metadata.RequestHeaders.Get("Authorization"))
	require.Equal(t, "benign", detail.Stages[0].Metadata.RequestHeaders.Get("X-Visible"))
	require.Contains(t, detail.Stages[0].Metadata.URL, "api_key=%5BREDACTED%5D")
	require.NotContains(t, detail.Stages[0].Metadata.URL, "synthetic-query-secret")
}

func TestRequestTraceRepositoryListsMetadataButRetainsBoundedStagePayload(t *testing.T) {
	ctx := context.Background()
	repo := &requestTraceRepository{q: newRequestTraceTestTx(t)}
	id := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	_, err := repo.CreateRequestTrace(ctx, service.RequestTrace{TraceID: id, RouteFamily: service.RequestTraceChatCompletions, InboundEndpoint: "/v1/chat/completions", ClientStatus: 200})
	require.NoError(t, err)
	payload := []byte(`{"messages":[{"content":"CANARY_PROMPT"}]}`)
	_, err = repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{TraceID: id, Ordinal: 1, Stage: "wire", AttemptIndex: 1, View: "wire", State: service.RequestTraceStored, Reason: "retained", ObservedBytes: int64(len(payload)), Payload: payload})
	require.NoError(t, err)
	list, count, err := repo.ListRequestTraces(ctx, service.RequestTraceListFilter{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	require.Len(t, list, 1)
	// Type-level list boundary: metadata-only RequestTrace has no payload/stages.
	result, err := repo.GetRequestTrace(ctx, id)
	require.NoError(t, err)
	require.Equal(t, payload, result.Stages[0].Payload)
	_, err = repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{TraceID: id, Ordinal: 2, Stage: "wire", AttemptIndex: 2, View: "wire", State: service.RequestTraceStored, Reason: "retained", ObservedBytes: 1<<20 + 1, Payload: make([]byte, 1<<20+1)})
	require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord, "oversized stage must not reach the database")
}

func TestRequestTraceRepositoryAcceptsRedactionExpansionBeyondObservedBytes(t *testing.T) {
	ctx := context.Background()
	repo := &requestTraceRepository{q: newRequestTraceTestTx(t)}
	id := "dddddddddddddddddddddddddddddddd"
	_, err := repo.CreateRequestTrace(ctx, service.RequestTrace{TraceID: id, RouteFamily: service.RequestTraceMessages, InboundEndpoint: "/v1/messages"})
	require.NoError(t, err)
	payload := []byte(`{"key":"[REDACTED]"}`)
	_, err = repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{TraceID: id, Ordinal: 1, Stage: "client_entry", View: "decoded", State: service.RequestTraceStored, Reason: "retained", ObservedBytes: int64(len(`{"key":"x"}`)), Payload: payload})
	require.NoError(t, err, "safe redaction may be longer than the original wire bytes")
}

func TestRequestTraceRepositoryRejectsPayloadStatusContradictions(t *testing.T) {
	ctx := context.Background()
	repo := &requestTraceRepository{q: newRequestTraceTestTx(t)}
	id := "cccccccccccccccccccccccccccccccc"
	_, err := repo.CreateRequestTrace(ctx, service.RequestTrace{TraceID: id, RouteFamily: service.RequestTraceResponses, InboundEndpoint: "/v1/responses"})
	require.NoError(t, err)
	for _, tc := range []struct {
		state service.RequestTraceCaptureState
		body  []byte
		bytes int64
	}{
		{service.RequestTraceStored, nil, 0},
		{service.RequestTraceNotObserved, []byte("secret"), 6},
	} {
		_, err := repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{TraceID: id, Ordinal: 1, Stage: "wire", State: tc.state, Payload: tc.body, ObservedBytes: tc.bytes})
		require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
	}
	_, err = repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{
		TraceID: id, Ordinal: 1, Stage: "wire", State: service.RequestTraceStored,
		Reason: "retained", Payload: []byte("text"), ObservedBytes: 1,
	})
	require.NoError(t, err, "redaction and encoding may expand retained bytes beyond wire bytes")
}

// --- 迁移 261：stage metadata 的数据库侧边界 -----------------------------------
//
// 258 把 request_trace_stages.metadata 建成无形状／键／大小约束的 JSONB。类型化的
// RequestTraceStageFacts 投影只在应用层强制，直接写库、未来写入者或跳过服务守卫的部署
// 都能存进数组、标量、任意键或无界 blob。261 只追加三条语义约束（对象形状＋闭集键、
// canonical 文本 ≤ 4096、非 client_metadata／wire_attempt 必须是 '{}'）。
//
// 这里在真实 PostgreSQL 上重放 261 的真实嵌入 SQL，并逐条验证「约束行为」：
// 合法事实可写、数组／标量被拒、未知键被拒、其他阶段被拒、4096 边界精确、可重放。
const (
	traceStageFactsMigration258 = "258_request_traces.sql"
	traceStageFactsMigration261 = "261_request_trace_stage_facts_bounds.sql"
	// 266 追加 platform 键：允许键集由它最终定义，因此本测试必须按 261 → 266 的
	// 真实迁移顺序应用，否则会拿已经被后续迁移取代的旧约束去断言。
	traceStageFactsMigrationPlatform = "266_request_trace_stage_facts_platform.sql"

	traceStageFactsTable           = "request_trace_stages"
	traceStageFactsShapeConstraint = "request_trace_stages_metadata_shape_allowed"
	traceStageFactsSizeConstraint  = "request_trace_stages_metadata_size_bound"
	traceStageFactsStageConstraint = "request_trace_stages_metadata_stage_allowed"

	// 写库语句：$1 = request_traces.id，$2 = ordinal，$3 = stage，$4 = metadata 文本。
	// 每个用例消耗一个新的 ordinal，避免与已成功的行冲突（UNIQUE(trace_id, ordinal)）。
	traceStageFactsInsertStage = `
INSERT INTO request_trace_stages (trace_id, ordinal, stage, state, reason, metadata)
VALUES ($1, $2, $3, 'not_observed', 'metadata_observed', $4::jsonb)`

	// 服务端 RequestTraceStageFacts 的完整 JSON 键集合。改键集合就必须同时改这里。
	traceStageFactsFullProjection = `{"method":"POST","url":"https://api.anthropic.com/v1/messages",` +
		`"url_omitted":false,"request_headers":{"Content-Type":["application/json"]},"request_headers_omitted":0,` +
		`"response_headers":{"Content-Type":["text/event-stream"]},"response_headers_omitted":1,` +
		`"account_id":42,"model":"claude-sonnet-4-5","protocol":"anthropic.messages","value_protocol":"anthropic.messages",` +
		`"status":200,"started_at":"2026-09-29T10:00:00Z","ended_at":"2026-09-29T10:00:01Z"}`

	traceStageFactsWireProjection = `{"method":"POST","url":"https://api.anthropic.com/v1/messages","status":200}`
)

// TestRequestTraceStageFactsMigration261BoundsStageMetadata 是 261 的核心验收：metadata
// 只接受有界、闭集键、且只出现在 client_metadata／wire_attempt 上的对象。
func TestRequestTraceStageFactsMigration261BoundsStageMetadata(t *testing.T) {
	tx, schema := newRequestTraceStageFactsSchema(t)

	// 261 之前必须有界地不存在：证明下面这些拒绝不是因为 258 自带的约束，测试不会空过。
	for _, name := range []string{traceStageFactsShapeConstraint, traceStageFactsSizeConstraint, traceStageFactsStageConstraint} {
		requireConstraintAbsent(t, tx, schema, traceStageFactsTable, name)
	}

	applyRequestTraceStageFactsMigration(t, tx)
	for _, name := range []string{traceStageFactsShapeConstraint, traceStageFactsSizeConstraint, traceStageFactsStageConstraint} {
		requireConstraintExists(t, tx, schema, traceStageFactsTable, name)
	}

	writer := newRequestTraceStageFactsWriter(t, tx)

	// 1. 服务端真正产生的投影（闭集 14 键）在两个事实阶段都必须可写。
	require.NoError(t, writer.insert("client_metadata", traceStageFactsFullProjection),
		"client_metadata 的全量投影必须可写")
	require.NoError(t, writer.insert("wire_attempt", traceStageFactsWireProjection),
		"wire_attempt 的投影必须可写")

	// 1b. 键集合必须与 RequestTraceStageFacts 的 json 标签保持一致：给 DTO 加字段却忘了
	//     同步 261 的允许键，会让线上写入直接被数据库拒绝。这里用 DTO 自己生成那次写入。
	require.NoError(t, writer.insert("wire_attempt", traceStageFactsProjectionFromDTO(t)),
		"RequestTraceStageFacts 的每个 json 键都必须被 261 接受")

	// 2. 空对象是「未观察到」，任何阶段都必须允许，否则正文阶段存不进行。
	for _, stage := range []string{"client_entry", "client_metadata", "wire_attempt", "wire_request", "upstream_response", "client_response", "capture_gap"} {
		require.NoErrorf(t, writer.insert(stage, `{}`), "阶段 %s 必须允许空的 metadata", stage)
	}

	// 3. 越界写入被数据库拒绝（SQLSTATE 23514），而不是留给应用层。
	for _, tc := range traceStageFactsRejectionCases() {
		t.Run(tc.name, func(t *testing.T) {
			writer.expectRejected(t, tc.stage, tc.metadata)
		})
	}

	// 4. canonical 文本的 4096 字节边界是精确的：4096 通过、4097 被拒。
	//    jsonb 的 canonical 输出会加 ": "／", " 分隔符，所以 fixture 直接以数据库
	//    读到的字节数为准，而不是信任 Go 侧的紧凑编码长度。
	atBound := `{"method": "` + strings.Repeat("P", 4082) + `"}`
	overBound := `{"method": "` + strings.Repeat("P", 4083) + `"}`
	require.Equal(t, 4096, traceStageFactsCanonicalBytes(t, tx, atBound), "fixture 必须正好落在边界上")
	require.Equal(t, 4097, traceStageFactsCanonicalBytes(t, tx, overBound), "fixture 必须正好越界一个字节")
	require.NoError(t, writer.insert("wire_attempt", atBound), "4096 字节的 canonical 文本必须可写")
	writer.expectRejected(t, "wire_attempt", overBound)
}

// TestRequestTraceStageFactsMigration261ClosesTheGapLeftBy258 证明这些拒绝是 261 带来的：
// 只有 258 时，同一批形状／键／阶段／大小违规全部可写。没有这一条，上面那些 23514 也可能
// 来自别处，测试会退化成「恰好失败」。
func TestRequestTraceStageFactsMigration261ClosesTheGapLeftBy258(t *testing.T) {
	tx, _ := newRequestTraceStageFactsSchema(t)
	writer := newRequestTraceStageFactsWriter(t, tx)

	for _, tc := range traceStageFactsRejectionCases() {
		require.NoErrorf(t, writer.insert(tc.stage, tc.metadata),
			"只有 258 时 %q 必须可写；否则 261 的拒绝不是它带来的", tc.name)
	}
	require.NoError(t, writer.insert("wire_attempt", `{"method": "`+strings.Repeat("P", 4083)+`"}`),
		"只有 258 时超长 metadata 必须可写")
}

// TestRequestTraceStageFactsMigration261IsReplaySafe 证明同一份 261 SQL 可被重放：
// 灾备重放不会因「约束已存在」失败，且重放后约束语义不退化。
func TestRequestTraceStageFactsMigration261IsReplaySafe(t *testing.T) {
	ctx := context.Background()
	tx, schema := newRequestTraceStageFactsSchema(t)

	applyRequestTraceStageFactsMigration(t, tx)
	writer := newRequestTraceStageFactsWriter(t, tx)
	require.NoError(t, writer.insert("client_metadata", traceStageFactsFullProjection))

	_, err := tx.ExecContext(ctx, requestTraceStageFactsMigrationText(t, traceStageFactsMigration261))
	require.NoError(t, err, "迁移 261 必须可重放（幂等），否则灾备重放会失败")

	for _, name := range []string{traceStageFactsShapeConstraint, traceStageFactsSizeConstraint, traceStageFactsStageConstraint} {
		requireConstraintExists(t, tx, schema, traceStageFactsTable, name)
	}
	require.NoError(t, writer.insert("wire_attempt", traceStageFactsWireProjection), "重放后合法投影仍必须可写")
	writer.expectRejected(t, "wire_attempt", `{"method":"POST","secret":"leak"}`)
	writer.expectRejected(t, "client_entry", `{"method":"POST"}`)
}

// TestRequestTraceStageFactsMigration261LandsOnMigratedSchema 断言 261 确实由迁移运行器
// 作用在真实 schema 上，而不只是「文件里有一段 SQL 文本」。
func TestRequestTraceStageFactsMigration261LandsOnMigratedSchema(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)

	var schema string
	require.NoError(t, scanSingleRow(ctx, tx, "SELECT current_schema()", nil, &schema))
	for _, name := range []string{traceStageFactsShapeConstraint, traceStageFactsSizeConstraint, traceStageFactsStageConstraint} {
		requireConstraintExists(t, tx, schema, traceStageFactsTable, name)
	}
}

type traceStageFactsCase struct {
	name     string
	stage    string
	metadata string
}

// traceStageFactsRejectionCases 只列数据库必须自己拦下的形状／键／阶段违规；字段类型、
// 脱敏与 URL 形状仍由应用层 ValidRequestTraceStageFacts 负责，不在本迁移范围内。
func traceStageFactsRejectionCases() []traceStageFactsCase {
	return []traceStageFactsCase{
		{
			name:     "unknown key is rejected",
			stage:    "wire_attempt",
			metadata: `{"method":"POST","authorization":"Bearer secret"}`,
		},
		{
			name:     "unknown key is rejected on client_metadata",
			stage:    "client_metadata",
			metadata: `{"url":"https://api.anthropic.com/v1/messages","extra":1}`,
		},
		{
			name:     "nested unknown key is rejected",
			stage:    "wire_attempt",
			metadata: `{"request_headers":{"Authorization":["Bearer secret"]},"body":"CANARY_BODY"}`,
		},
		{
			name:     "empty array is not an object",
			stage:    "wire_attempt",
			metadata: `[]`,
		},
		{
			name:     "array of allowed keys is not an object",
			stage:    "wire_attempt",
			metadata: `["method","url"]`,
		},
		{
			name:     "string is not an object",
			stage:    "wire_attempt",
			metadata: `"POST"`,
		},
		{
			name:     "number is not an object",
			stage:    "wire_attempt",
			metadata: `200`,
		},
		{
			name:     "null is not an object",
			stage:    "wire_attempt",
			metadata: `null`,
		},
		{
			name:     "facts are rejected on a body stage",
			stage:    "wire_request",
			metadata: traceStageFactsWireProjection,
		},
		{
			name:     "facts are rejected on the inbound stage",
			stage:    "client_entry",
			metadata: traceStageFactsWireProjection,
		},
		{
			name:     "facts are rejected on the downstream stage",
			stage:    "client_response",
			metadata: traceStageFactsWireProjection,
		},
		{
			name:     "facts are rejected on the gap stage",
			stage:    "capture_gap",
			metadata: `{"status":502}`,
		},
	}
}

// --- 261 fixture 与断言 helpers ----------------------------------------------

// newRequestTraceStageFactsSchema 建立只属于本测试的 schema，重放 258 的真实 SQL 后
// 得到与生产一致的阶段表（此时还没有 261 的边界约束）。
func newRequestTraceStageFactsSchema(t *testing.T) (*sql.Tx, string) {
	t.Helper()

	ctx := context.Background()
	tx := testTx(t)
	schema := fmt.Sprintf("trace_stage_facts_%d", time.Now().UnixNano())

	_, err := tx.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err, "create isolated schema")
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+schema)
	require.NoError(t, err, "pin search_path to the isolated schema")

	// 258 的 request_traces.usage_log_id 外键指向 usage_logs，这里只需要主键形状。
	_, err = tx.ExecContext(ctx, "CREATE TABLE usage_logs (id BIGSERIAL PRIMARY KEY)")
	require.NoError(t, err, "create isolated usage_logs")
	_, err = tx.ExecContext(ctx, requestTraceStageFactsMigrationText(t, traceStageFactsMigration258))
	require.NoError(t, err, "replay migration 258")

	return tx, schema
}

func applyRequestTraceStageFactsMigration(t *testing.T, tx *sql.Tx) {
	t.Helper()

	_, err := tx.ExecContext(context.Background(), requestTraceStageFactsMigrationText(t, traceStageFactsMigration261))
	require.NoError(t, err, "apply migration 261")
	_, err = tx.ExecContext(context.Background(), requestTraceStageFactsMigrationText(t, traceStageFactsMigrationPlatform))
	require.NoError(t, err, "apply migration 266")
}

func requestTraceStageFactsMigrationText(t *testing.T, name string) string {
	t.Helper()

	content, err := migrations.FS.ReadFile(name)
	require.NoErrorf(t, err, "read migration %s", name)
	return string(content)
}

// traceStageFactsWriter 在一次测试事务内往同一条 request_traces 行追加阶段，
// 每次尝试消耗一个新的 ordinal。
type traceStageFactsWriter struct {
	t       *testing.T
	tx      *sql.Tx
	tracePK int64
	ordinal int
}

func newRequestTraceStageFactsWriter(t *testing.T, tx *sql.Tx) *traceStageFactsWriter {
	t.Helper()

	var tracePK int64
	require.NoError(t, scanSingleRow(context.Background(), tx, `
INSERT INTO request_traces (trace_id, route_family, inbound_endpoint)
VALUES ('0123456789abcdef0123456789abcdef', 'messages', '/v1/messages') RETURNING id
`, nil, &tracePK))
	return &traceStageFactsWriter{t: t, tx: tx, tracePK: tracePK}
}

func (w *traceStageFactsWriter) args(stage, metadata string) []any {
	w.ordinal++
	return []any{w.tracePK, w.ordinal, stage, metadata}
}

func (w *traceStageFactsWriter) insert(stage, metadata string) error {
	w.t.Helper()
	_, err := w.tx.ExecContext(context.Background(), traceStageFactsInsertStage, w.args(stage, metadata)...)
	return err
}

// expectRejected 要求这条写入以 CHECK 违规（23514）失败。SAVEPOINT 隔离失败语句，
// 之后事务仍可用（PostgreSQL 会把失败语句之后的事务置为 aborted）。
func (w *traceStageFactsWriter) expectRejected(t *testing.T, stage, metadata string) {
	t.Helper()

	expectPostgresError(t, w.tx, expectedPostgresCheckFailure, traceStageFactsInsertStage, w.args(stage, metadata)...)
}

// traceStageFactsProjectionFromDTO 用服务的 RequestTraceStageFacts 的 json 标签拼出一个只
// 含这些键的对象。值一律为 null：261 只约束形状／键／大小，字段类型仍由
// service.ValidRequestTraceStageFacts 负责，这里要证明的是「键集合两边必须一致」。
func traceStageFactsProjectionFromDTO(t *testing.T) string {
	t.Helper()

	typ := reflect.TypeOf(service.RequestTraceStageFacts{})
	fields := make(map[string]any, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		require.NotEmptyf(t, name, "RequestTraceStageFacts.%s 必须有 json 名", field.Name)
		fields[name] = nil
	}
	require.NotEmpty(t, fields, "反射必须真的读到 DTO 的 json 键，否则这条断言会空过")
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	return string(encoded)
}

// traceStageFactsCanonicalBytes 返回 PostgreSQL 实际读到的 canonical jsonb 文本字节数。
func traceStageFactsCanonicalBytes(t *testing.T, tx *sql.Tx, metadata string) int {
	t.Helper()

	var size int
	require.NoError(t, scanSingleRow(context.Background(), tx, `SELECT octet_length(($1::jsonb)::text)`, []any{metadata}, &size))
	return size
}
