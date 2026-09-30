//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
-- 迁移 270 的同一个校验函数（未限定 schema，落在当前 search_path 的 schema 里）。
CREATE OR REPLACE FUNCTION request_trace_observed_platforms_valid(platforms JSONB)
RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE
        WHEN platforms IS NULL THEN TRUE
        WHEN jsonb_typeof(platforms) <> 'array' THEN FALSE
        WHEN jsonb_array_length(platforms) NOT BETWEEN 1 AND 16 THEN FALSE
        WHEN EXISTS (SELECT 1 FROM jsonb_array_elements(platforms) AS element
                     WHERE jsonb_typeof(element) <> 'string') THEN FALSE
        WHEN EXISTS (SELECT 1 FROM jsonb_array_elements_text(platforms) AS element
                     WHERE element !~ '^[A-Za-z0-9][A-Za-z0-9_./:+-]{0,127}$') THEN FALSE
        WHEN (SELECT count(*) FROM jsonb_array_elements_text(platforms))
             <> (SELECT count(DISTINCT element) FROM jsonb_array_elements_text(platforms) AS element) THEN FALSE
        WHEN octet_length(platforms::text) > 2048 THEN FALSE
        ELSE TRUE
    END
$$;
CREATE TABLE IF NOT EXISTS usage_logs (id BIGSERIAL PRIMARY KEY);
CREATE TABLE IF NOT EXISTS users (id BIGINT PRIMARY KEY, email TEXT);
CREATE TABLE IF NOT EXISTS api_keys (id BIGINT PRIMARY KEY, name TEXT);
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
    -- 与 265 之后的信封一致：请求时分组与客户端模型可空，NULL 表示"未观察到"。
    group_id BIGINT,
    requested_model TEXT,
    -- 与 270 之后的信封一致：实际选中平台历史。NULL = 从未选到账号（未知）；
    -- 非 NULL 必须是非空 JSON 数组（1..16 个去重 token），空数组与混入的非法元素都非法。
    observed_platforms JSONB,
    user_id BIGINT,
    api_key_id BIGINT,
    CONSTRAINT request_traces_observed_platforms_shape_allowed
        CHECK (request_trace_observed_platforms_valid(observed_platforms)),
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

// insertRequestTraceWithPlatforms 写入一条带真实创建时间与信封平台历史的 Trace，
// 用于在同一批行上比较列表与导出的平台筛选。
func insertRequestTraceWithPlatforms(t *testing.T, tx *sql.Tx, traceID string, createdAt time.Time, platforms []string) {
	t.Helper()
	var observed any
	if platforms != nil {
		encoded, err := json.Marshal(platforms)
		require.NoError(t, err)
		observed = encoded
	}
	_, err := tx.ExecContext(context.Background(), `INSERT INTO request_traces
		(trace_id, route_family, inbound_endpoint, capture_state, client_status, created_at, observed_platforms)
		VALUES ($1, 'messages', '/v1/messages', 'stored', 429, $2, $3)`,
		traceID, createdAt.UTC(), observed)
	require.NoError(t, err)
}

// 平台筛选在列表与导出必须逐字同源：否则"导出当前查询全部"就不是管理员在列表里
// 看到的那一批记录。这里在同一批真实行上对同一组平台条件分别跑列表与导出分页，
// 断言两者的 ID 序列完全一致（含次序），并覆盖"选中但未发出"的信封平台事实与
// 只带阶段平台的历史行。
func TestRequestTraceExportPageMatchesListPlatformFilters(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	listRepo := &requestTraceRepository{q: tx}

	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	// 每条用不同的 created_at，列表与导出都按 created_at DESC, id DESC 给出唯一次序。
	const (
		envelopeOnly = "0000000000000000000000000000000a"
		multiPlat    = "0000000000000000000000000000000b"
		stageOnly    = "0000000000000000000000000000000c"
		unknownPlat  = "0000000000000000000000000000000d"
	)
	// 账号已选中、但从未发出上游：平台只在信封上。
	insertRequestTraceWithPlatforms(t, tx, envelopeOnly, base.Add(4*time.Minute), []string{service.PlatformAnthropic})
	// A→B 换号：两个平台都是请求时事实。
	insertRequestTraceWithPlatforms(t, tx, multiPlat, base.Add(3*time.Minute), []string{service.PlatformAnthropic, service.PlatformOpenAI})
	// 270 之前的历史行：信封为 NULL，只有阶段平台。
	insertRequestTraceWithPlatforms(t, tx, stageOnly, base.Add(2*time.Minute), nil)
	_, err := tx.ExecContext(ctx, `INSERT INTO request_trace_stages
		(trace_id, ordinal, stage, view_name, state, reason, metadata)
		SELECT id, 1, 'wire_attempt', 'wire', 'not_observed', 'wire_observed',
		       jsonb_build_object('platform', $2::text)
		FROM request_traces WHERE trace_id = $1`, stageOnly, service.PlatformGemini)
	require.NoError(t, err)
	// 从未选到账号：两处都没有平台事实。
	insertRequestTraceWithPlatforms(t, tx, unknownPlat, base.Add(time.Minute), nil)

	unknown := true
	cases := map[string]service.RequestTraceExportFilter{
		"envelope platform": {Platform: service.PlatformAnthropic},
		"stage platform":    {Platform: service.PlatformGemini},
		"failover platform": {Platform: service.PlatformOpenAI},
		"absent platform":   {Platform: service.PlatformZhipu},
		"unknown platform":  {PlatformUnknown: &unknown},
	}
	for name, filter := range cases {
		t.Run(name, func(t *testing.T) {
			list, count, err := listRepo.ListRequestTraces(ctx, service.RequestTraceListFilter{
				Platform: filter.Platform, PlatformUnknown: filter.PlatformUnknown, Page: 1, PageSize: 50,
			})
			require.NoError(t, err)
			listIDs := make([]string, 0, len(list))
			for _, trace := range list {
				listIDs = append(listIDs, trace.TraceID)
			}
			require.Equal(t, int64(len(listIDs)), count)
			exported := walkRequestTraceExportPage(t, source, filter, 50)
			require.Equal(t, listIDs, exported,
				"导出与列表必须对同一平台条件返回同一批次、同一次序：列表=%v 导出=%v", listIDs, exported)
		})
	}
}

// 列表能用的每个条件在导出侧都必须等价，且次序就是列表次序 created_at DESC,
// id DESC：这里用一个真实行集逐条件断言精确次序（含跨页翻读）。
func TestRequestTraceExportPageHonoursListFilters(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	now := time.Now().UTC()
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

	// 五条都共享 now（除最旧一条），所以组内次序由内部自增 id 决定：最后插入者在前。
	require.Equal(t, []string{ids[4], ids[3], ids[2], ids[0], ids[1]},
		walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{}, 2),
		"逐条翻页必须给出与列表一致的次序，且不重不漏")

	require.Equal(t, []string{ids[4], ids[3], ids[2], ids[1]},
		walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{RouteFamily: string(service.RequestTraceMessages)}, 2))
	statusOK := 200
	require.Equal(t, []string{ids[3], ids[2], ids[0], ids[1]},
		walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{ClientStatus: &statusOK}, 4))
	require.Equal(t, []string{ids[2]},
		walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{TraceID: ids[2]}, 4))

	linked := true
	require.Equal(t, []string{ids[2]},
		walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{UsageLinked: &linked}, 4))
	unlinked := false
	require.Equal(t, []string{ids[4], ids[3], ids[0], ids[1]},
		walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{UsageLinked: &unlinked}, 4))

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)
	require.Equal(t, []string{ids[4], ids[3], ids[2], ids[0]},
		walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{CreatedFrom: &from, CreatedTo: &to}, 4),
		"the created_at window must exclude the older row without dropping later ones")
}

func TestRequestTraceExportKeywordMatchesListAndNeverSearchesBody(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	list := &requestTraceRepository{q: tx}
	userID, keyID := int64(923), int64(819)
	_, err := tx.ExecContext(ctx, `INSERT INTO users(id,email) VALUES($1,'lookup@example.test')`, userID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO api_keys(id,name) VALUES($1,'lookup-key')`, keyID)
	require.NoError(t, err)
	const traceID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	_, err = tx.ExecContext(ctx, `INSERT INTO request_traces
		(trace_id,route_family,inbound_endpoint,user_id,api_key_id) VALUES($1,'messages','/v1/messages',$2,$3)`, traceID, userID, keyID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO request_trace_stages(trace_id,ordinal,stage,state,reason,observed_bytes,retained_bytes,payload)
		SELECT id,1,'wire_request','stored','retained',length($2::bytea),length($2::bytea),$2 FROM request_traces WHERE trace_id=$1`, traceID, []byte("body-only-canary"))
	require.NoError(t, err)
	for _, keyword := range []string{"lookup-key", "lookup@example", "eeeee", "messages"} {
		rows, total, err := list.ListRequestTraces(ctx, service.RequestTraceListFilter{Page: 1, PageSize: 10, Keyword: keyword})
		require.NoError(t, err)
		require.Equal(t, int64(1), total)
		require.Len(t, rows, 1)
		require.Equal(t, []string{traceID}, walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{Keyword: keyword}, 10))
	}
	require.Empty(t, walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{Keyword: "body-only-canary"}, 10))
}

// traceIDOrderOppositeToCreatedAtFixture 造出 trace_id 次序与 created_at 次序相反的
// 真实行：最新的两行共用同一 created_at，只有按 (created_at DESC, id DESC) 才能
// 得到列表次序。返回期望的导出行次序（= Trace 列表次序）。
func traceIDOrderOppositeToCreatedAtFixture(t *testing.T, tx *sql.Tx) []string {
	t.Helper()
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	// 插入顺序决定内部 id：先插的 id 更小。
	// 00000000000000000000000000000001 与 ...0002 共用最新 created_at，前者先插入
	// （内部 id 更小），所以列表次序里 ...0002 在前——这与 trace_id 升序相反。
	insertRequestTrace(t, tx, "00000000000000000000000000000001", string(service.RequestTraceMessages), 200, base.Add(3*time.Minute), nil)
	insertRequestTrace(t, tx, "00000000000000000000000000000002", string(service.RequestTraceMessages), 200, base.Add(3*time.Minute), nil)
	insertRequestTrace(t, tx, "00000000000000000000000000000003", string(service.RequestTraceMessages), 200, base.Add(2*time.Minute), nil)
	insertRequestTrace(t, tx, "00000000000000000000000000000004", string(service.RequestTraceMessages), 200, base.Add(time.Minute), nil)
	insertRequestTrace(t, tx, "00000000000000000000000000000005", string(service.RequestTraceMessages), 200, base, nil)
	return []string{
		"00000000000000000000000000000002",
		"00000000000000000000000000000001",
		"00000000000000000000000000000003",
		"00000000000000000000000000000004",
		"00000000000000000000000000000005",
	}
}

// 导出文件次序必须与 Trace 列表一致（created_at DESC, id DESC）。fixture 刻意让
// trace_id 次序与 created_at 次序相反，并在最新 created_at 上制造并列，
// 所以 trace_id 升序、created_at DESC 但以 trace_id 破并列都会得到错误次序。
func TestRequestTraceExportSourcePagesRealRowsInListOrder(t *testing.T) {
	tx, source := newRequestTraceExportSourceTx(t)
	expected := traceIDOrderOppositeToCreatedAtFixture(t, tx)

	// 一页装得下与按小页翻读必须得到同一批次、同一次序，且不重不漏。
	require.Equal(t, expected, walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{}, 10))
	require.Equal(t, expected, walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{}, 2))
}

// "导出所选"：只导出勾选的 trace_id，且次序仍是列表次序。所选集合必须以驱动可
// 序列化的数组形式绑定（pq.Array）：裸 []string 会被 lib/pq 直接拒绝。
func TestRequestTraceExportPageSelectsOnlyTheGivenTraceIDs(t *testing.T) {
	tx, source := newRequestTraceExportSourceTx(t)
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	selected := []string{
		"0000000000000000000000000000000b",
		"0000000000000000000000000000000d",
	}
	insertRequestTrace(t, tx, "0000000000000000000000000000000a", string(service.RequestTraceMessages), 200, base, nil)
	insertRequestTrace(t, tx, selected[0], string(service.RequestTraceMessages), 200, base.Add(time.Minute), nil)
	insertRequestTrace(t, tx, "0000000000000000000000000000000c", string(service.RequestTraceMessages), 200, base.Add(2*time.Minute), nil)
	insertRequestTrace(t, tx, selected[1], string(service.RequestTraceMessages), 200, base.Add(3*time.Minute), nil)
	insertRequestTrace(t, tx, "0000000000000000000000000000000e", string(service.RequestTraceMessages), 200, base.Add(4*time.Minute), nil)

	require.Equal(t, []string{selected[1], selected[0]},
		walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{TraceIDs: selected}, 10),
		"只有勾选的两条，且按 created_at DESC, id DESC")
	// 勾选给出的顺序不影响结果，小页翻读也一样。
	require.Equal(t, []string{selected[1], selected[0]},
		walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{TraceIDs: []string{selected[1], selected[0]}}, 1))
	// 未勾选的行一条都不出现；勾选了不存在的 ID 得到空结果而不是错误。
	require.Empty(t, walkRequestTraceExportPage(t, source, service.RequestTraceExportFilter{TraceIDs: []string{"0000000000000000000000000000000f"}}, 10))
}

// walkRequestTraceExportPage 用不透明游标按列表次序翻页，直到空页结束。
func walkRequestTraceExportPage(t *testing.T, source *requestTraceExportSource, filter service.RequestTraceExportFilter, pageSize int) []string {
	t.Helper()
	ctx := context.Background()
	collected := make([]string, 0)
	cursor := ""
	for {
		page, next, err := source.NextTracePage(ctx, filter, cursor, pageSize)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page), pageSize)
		for _, id := range page {
			require.NotContains(t, collected, id, "每一行只能被翻到一次")
		}
		collected = append(collected, page...)
		if next == "" {
			require.Empty(t, page, "遍历以空页结束，空页不返回游标")
			return collected
		}
		require.NotEmpty(t, page, "游标只能来自非空页")
		require.NotEqual(t, cursor, next, "游标必须前进，否则遍历不会终止")
		cursor = next
	}
}

// 游标钉的是 (created_at, 内部 id)，不依赖那一行仍然存在：边遍历边被清理的记录，
// 包括刚好是游标所在的那一行，都不会让后续页丢失或重复。
func TestRequestTraceExportPageSurvivesDeletingTheCursorRow(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	expected := traceIDOrderOppositeToCreatedAtFixture(t, tx)

	first, cursor, err := source.NextTracePage(ctx, service.RequestTraceExportFilter{}, "", 2)
	require.NoError(t, err)
	require.Equal(t, expected[:2], first)
	require.NotEmpty(t, cursor)

	// 清理删掉的正是游标那一行：续读仍必须从它之后继续，既不重放也不跳过。
	_, err = tx.ExecContext(ctx, `DELETE FROM request_traces WHERE trace_id = $1`, expected[1])
	require.NoError(t, err)

	rest := make([]string, 0, len(expected)-2)
	for cursor != "" {
		page, next, pageErr := source.NextTracePage(ctx, service.RequestTraceExportFilter{}, cursor, 2)
		require.NoError(t, pageErr)
		rest = append(rest, page...)
		cursor = next
	}
	require.Equal(t, expected[2:], rest, "游标行被清理后仍必须精确续读")
}

// 非法游标必须在触达 SQL 之前被拒绝：游标是私有的有界不透明串。
func TestRequestTraceExportPageRejectsMalformedCursor(t *testing.T) {
	_, source := newRequestTraceExportSourceTx(t)
	filter := service.RequestTraceExportFilter{}
	for _, cursor := range []string{
		"not-a-cursor",
		"0123456789abcdef0123456789abcdef",
		"v1:",
		"v1:1",
		"v1:1:",
		"v1::1",
		"v1:1:2:3",
		"v1:0:1",
		"v1:1:0",
		"v1:-1:1",
		"v1:1:+2",
		"v2:1:1",
		"v1:" + strings.Repeat("9", requestTraceExportCursorMaxLen) + ":1",
	} {
		ids, next, err := source.NextTracePage(context.Background(), filter, cursor, 10)
		require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord, "cursor %q 必须被拒绝", cursor)
		require.Nil(t, ids)
		require.Empty(t, next)
	}
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

// 导出详情必须与管理员详情披露同款信封字段：创建/完成/清理时间、请求时分组与
// 客户端模型。已关联使用记录的 Trace 与详情一样不单独披露 cleanup_after；
// 未观察到的事实保持零值，不伪造成某个具体分组或模型。
func TestRequestTraceExportSourceKeepsDetailEnvelopeParity(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	createdAt := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	completedAt := createdAt.Add(time.Minute)
	cleanupAfter := createdAt.Add(30 * 24 * time.Hour)
	groupID := int64(77)
	var usageLogID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO usage_logs DEFAULT VALUES RETURNING id`).Scan(&usageLogID))

	linked := "0000000000000000000000000000000a"
	unlinked := "0000000000000000000000000000000b"
	_, err := tx.ExecContext(ctx, `INSERT INTO request_traces
		(trace_id, route_family, inbound_endpoint, capture_state, client_status, usage_log_id,
		 created_at, completed_at, cleanup_after, group_id, requested_model)
		VALUES ($1, 'messages', '/v1/messages', 'stored', 200, $2, $3, $4, $5, $6, $7),
		       ($8, 'responses', '/v1/responses', 'partial', 503, NULL, $3, NULL, $5, NULL, NULL)`,
		linked, &usageLogID, createdAt, completedAt, cleanupAfter, &groupID, "gpt-5.3-codex", unlinked)
	require.NoError(t, err)

	identityUser, identityKey := int64(741), int64(951)
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,email) VALUES($1,'synthetic-current@example.test')`, identityUser)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO api_keys(id,name) VALUES($1,'synthetic-current-key')`, identityKey)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE request_traces SET user_id=$1, api_key_id=$2 WHERE trace_id=$3`, identityUser, identityKey, linked)
	require.NoError(t, err)

	linkedDetail, available, err := source.ReadApprovedDetail(ctx, linked)
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, createdAt, linkedDetail.CreatedAt.UTC())
	require.NotNil(t, linkedDetail.CompletedAt)
	require.Equal(t, completedAt, linkedDetail.CompletedAt.UTC())
	require.Nil(t, linkedDetail.CleanupAfter, "已关联使用记录的 Trace 与详情一样不单独披露 cleanup_after")
	require.NotNil(t, linkedDetail.GroupID)
	require.Equal(t, groupID, *linkedDetail.GroupID)
	require.Equal(t, "gpt-5.3-codex", linkedDetail.RequestedModel)
	require.Equal(t, &identityUser, linkedDetail.UserID)
	require.Equal(t, &identityKey, linkedDetail.APIKeyID)
	require.Equal(t, "synthetic-current@example.test", linkedDetail.UserEmail)
	require.Equal(t, "synthetic-current-key", linkedDetail.APIKeyName)

	unlinkedDetail, available, err := source.ReadApprovedDetail(ctx, unlinked)
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, createdAt, unlinkedDetail.CreatedAt.UTC())
	require.Nil(t, unlinkedDetail.CompletedAt)
	require.NotNil(t, unlinkedDetail.CleanupAfter)
	require.Equal(t, cleanupAfter, unlinkedDetail.CleanupAfter.UTC())
	require.Nil(t, unlinkedDetail.GroupID)
	require.Empty(t, unlinkedDetail.RequestedModel)
}

func TestRequestTraceExportSourceCountsTraceDeletedAfterPaging(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	kept := "0000000000000000000000000000000a"
	removed := "0000000000000000000000000000000b"
	exportSourceTraceIDs(t, tx, []string{kept, removed})

	ids, _, err := source.NextTracePage(ctx, service.RequestTraceExportFilter{}, "", 10)
	require.NoError(t, err)
	// 列表次序：后插入（created_at 不早于、内部 id 更大）的 removed 在前。
	require.Equal(t, []string{removed, kept}, ids)

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
	ids, next, err := source.NextTracePage(ctx, service.RequestTraceExportFilter{TraceID: traceID}, "", 10)
	require.NoError(t, err)
	require.Equal(t, []string{traceID}, ids)
	require.NotEmpty(t, next, "非空页必须返回可续读的游标")

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

// ListForSession keeps the same three-way filtering the real store does, so the
// in-memory fixture cannot accidentally answer a recall question the database
// would refuse to answer.
func (s *inMemoryRequestTraceExportStore) ListForSession(_ context.Context, adminUserID int64, sessionDigest, instanceID string, limit int) ([]service.RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > service.RequestTraceExportMaxListedTasks {
		return nil, service.ErrRequestTraceExportLimit
	}
	matched := make([]service.RequestTraceExportTask, 0)
	for _, task := range s.tasks {
		if task.AdminUserID != adminUserID || task.SessionDigest != sessionDigest || task.InstanceID != instanceID {
			continue
		}
		matched = append(matched, task)
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID > matched[j].ID
	})
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
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

func (s *deletingRequestTraceExportSource) NextTracePage(ctx context.Context, filter service.RequestTraceExportFilter, after string, limit int) ([]string, string, error) {
	ids, next, err := s.RequestTraceExportSource.NextTracePage(ctx, filter, after, limit)
	if err != nil {
		return nil, "", err
	}
	s.once.Do(func() { _ = s.delete() })
	return ids, next, nil
}

// 只给出**落在未来**的下界：CreateTask 先校验调用方的筛选再盖内部上界（现在），
// 于是源侧看到的是一个空区间。空区间不是失败：导出必须以 0 行、清单"完整"收尾，
// 与列表返回 200 空集同一语义。存量行都落在过去，它们只因窗口为空才不被选中。
func TestRequestTraceExportServiceCompletesEmptyForFutureCreatedFrom(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	exportSourceTraceIDs(t, tx, []string{"0000000000000000000000000000000a", "0000000000000000000000000000000b"})

	future := time.Now().UTC().Add(24 * time.Hour)
	dir := t.TempDir()
	store := &inMemoryRequestTraceExportStore{}
	exportSvc := service.NewRequestTraceExportService(store, source, service.RequestTraceExportOptions{
		Enabled: true, SingleInstanceDeclared: true, InstanceID: "integration-instance", TempDir: dir,
	})
	exportSvc.SetAcknowledgementSatisfiedForTest(true)
	actor := service.RequestTraceExportActor{AdminUserID: 7, SessionID: "admin-session"}
	task, err := exportSvc.CreateTask(ctx, actor, service.RequestTraceExportFilter{CreatedFrom: &future})
	require.NoError(t, err, "只给了未来下界的合法查询必须能创建任务，而不是被内部上界变成倒置区间")

	finished, err := exportSvc.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, service.RequestTraceExportCompleted, finished.Status)
	require.Equal(t, int64(0), finished.RowsExported)
	require.Equal(t, int64(0), finished.RowsSkipped)

	manifestFile, result, err := exportSvc.OpenDownload(ctx, actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = manifestFile.Close() })
	content, err := os.ReadFile(manifestFile.Name())
	require.NoError(t, err)
	var manifest service.RequestTraceExportManifest
	require.NoError(t, json.Unmarshal(content, &manifest))
	require.Equal(t, int64(0), manifest.Rows)
	require.Equal(t, int64(0), manifest.Skipped)
	require.True(t, manifest.Complete, "空区间是完整结果，不是不完整交付")
	require.Empty(t, manifest.Reason)
	require.Equal(t, int64(0), result.RowsExported)
}

// TestRequestTraceExportServiceExportsRealPagesAndCountsDeletedTraces drives the
// real export service over the real source: 131 traces span more than one page,
// one of them is deleted mid-export, and the finished file must contain the
// approved detail of every surviving trace only.
func TestRequestTraceExportServiceExportsRealPagesAndCountsDeletedTraces(t *testing.T) {
	ctx := context.Background()
	tx, source := newRequestTraceExportSourceTx(t)
	const totalTraces = 131
	// created_at 取"过去"：CreateTask 会把任务创建时刻盖成时间上界（不含），
	// 用库侧 NOW() 会和应用时钟产生偏差，让本该在范围内的行被上界挡掉。
	_, err := tx.ExecContext(ctx, `INSERT INTO request_traces
		(trace_id, route_family, inbound_endpoint, capture_state, client_status, created_at)
		SELECT lpad(to_hex(g), 32, '0'), 'messages', '/v1/messages', 'stored', 200, NOW() - INTERVAL '1 hour'
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
	// 夹具显式声明"管理员已接受当前版本的导出风险声明"；未确认时创建任务按设计被拒。
	exportSvc.SetAcknowledgementSatisfiedForTest(true)
	actor := service.RequestTraceExportActor{AdminUserID: 7, SessionID: "admin-session"}
	task, err := exportSvc.CreateTask(ctx, actor, service.RequestTraceExportFilter{})
	require.NoError(t, err)
	finished, err := exportSvc.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, service.RequestTraceExportCompleted, finished.Status)
	require.Equal(t, int64(totalTraces-1), finished.RowsExported)
	require.Equal(t, int64(1), finished.RowsSkipped, "the trace deleted mid-export is skipped and counted")

	// 交付形态是"清单 + 分片"：清单说明范围与完整性，分片才是逐行详情。
	manifestFile, result, err := exportSvc.OpenDownload(ctx, actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = manifestFile.Close() })
	manifestContent, err := os.ReadFile(manifestFile.Name())
	require.NoError(t, err)
	require.Equal(t, int64(totalTraces-1), result.RowsExported)

	var manifest service.RequestTraceExportManifest
	require.NoError(t, json.Unmarshal(manifestContent, &manifest))
	require.Equal(t, int64(totalTraces-1), manifest.Rows)
	require.Equal(t, int64(1), manifest.Skipped)
	require.False(t, manifest.Complete, "a trace that vanished mid-paging makes the result incomplete")
	require.Equal(t, service.RequestTraceExportIncompleteSourceGone, manifest.Reason)
	require.NotEmpty(t, manifest.Shards)

	seen := make(map[string]bool, totalTraces-1)
	sentinelSeen := 0
	totalLines := 0
	for ordinal := 1; ordinal <= len(manifest.Shards); ordinal++ {
		shardFile, _, _, shardErr := exportSvc.OpenShardDownload(ctx, actor, task.ID, ordinal)
		require.NoError(t, shardErr)
		shardContent, readErr := os.ReadFile(shardFile.Name())
		_ = shardFile.Close()
		require.NoError(t, readErr)
		require.NotContains(t, string(shardContent), "metadata")
		lines := strings.Split(strings.TrimSuffix(string(shardContent), "\n"), "\n")
		if len(shardContent) == 0 {
			lines = nil
		}
		totalLines += len(lines)
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
	}
	require.Equal(t, totalTraces-1, totalLines)
	require.Len(t, seen, totalTraces-1)
	require.Equal(t, 1, sentinelSeen)
}
