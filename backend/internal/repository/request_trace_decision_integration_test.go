//go:build integration

package repository

// 迁移 262「gateway_decision 阶段与决策投影」在真实 PostgreSQL 上的验收。
//
// 261 把 request_trace_stages.metadata 这个唯一的 JSONB 列收窄到 14 个 transport key，
// 并且只允许 client_metadata／wire_attempt 两个阶段持有非空对象；其它阶段必须保持 '{}'。
// server/request_trace_decision.go 需要同一列承载第二种、与 transport 不相交的 typed 投影：
// 无正文的 gateway_decision 阶段保存 RequestTraceDecisionFacts。
//
// 这里要证明的是「约束行为」而不是「文件里出现了某段文本」：
//
//	1. 261 的 transport 投影在 262 之后仍然只能写它自己的 key，且仍然只允许那两个阶段；
//	2. 决策投影只能写它自己的 key，gateway_decision 不得携带 transport key，反之亦然；
//	3. gateway_decision 必须是真正的决策对象：'{}' 被拒，未知 key 被拒；
//	4. 非对象（数组／标量）与超过 4096 字节的 canonical 文本仍被拒（261 的兜底不变）；
//	5. 约束名与 261 保持一致，迁移可重放（灾备重放不因「约束已存在」失败）；
//	6. 真实 repository 写出的决策行同时满足这些约束，并能被同一 repository 原样读回。
//
// 隔离方式：每条测试在自己的临时 schema 内重放 258／261／262 的真实嵌入 SQL 文件，
// search_path 只指向该 schema，既不触碰 public 的在线表，也不修改任何既有数据；
// 整个 schema 随测试事务一起回滚。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

const (
	requestTraceEnvelopeMigration = "258_request_traces.sql"
	requestTraceFactsMigration    = "261_request_trace_stage_facts_bounds.sql"
	requestTraceDecisionMigration = "262_request_trace_gateway_decisions.sql"
	// 265 给信封补上分组与客户端模型事实、266 给阶段事实补上平台键：
	// 隔离 schema 必须按生产顺序应用它们，否则仓库写出的行会被自身 schema 拒绝。
	requestTraceScopeFactsMigration    = "265_request_trace_scope_facts.sql"
	requestTraceStagePlatformMigration = "266_request_trace_stage_facts_platform.sql"

	requestTraceStagesTable = "request_trace_stages"

	requestTraceMetadataShapeConstraint = "request_trace_stages_metadata_shape_allowed"
	requestTraceMetadataSizeConstraint  = "request_trace_stages_metadata_size_bound"
	requestTraceMetadataStageConstraint = "request_trace_stages_metadata_stage_allowed"

	requestTraceDecisionValidJSON = `{"decision":"route","outcome":"selected","source":"group","sequence":1,` +
		`"model_from":"claude-sonnet-4-5","model_to":"claude-sonnet-4-5-20250929",` +
		`"protocol_from":"anthropic.messages","protocol_to":"anthropic.messages",` +
		`"account_id":73,"decided_at":"2026-09-29T10:00:00Z"}`
	requestTraceTransportValidJSON = `{"method":"POST","url":"https://synthetic.example/v1/messages",` +
		`"protocol":"anthropic.messages","account_id":73,"status":200}`
	requestTraceTraceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// TestRequestTraceDecisionMigration262SeparatesTheTwoTypedProjections 是 262 的核心验收：
// 两种投影各写各的 key，互不串味，非对象与超限仍被拒。
func TestRequestTraceDecisionMigration262SeparatesTheTwoTypedProjections(t *testing.T) {
	ctx := context.Background()
	tx := newMigratedRequestTraceTx(t)

	for _, constraint := range []string{
		requestTraceMetadataShapeConstraint,
		requestTraceMetadataSizeConstraint,
		requestTraceMetadataStageConstraint,
	} {
		requireConstraintExists(t, tx, requestTraceSchemaName(t, tx), requestTraceStagesTable, constraint)
	}

	newRequestTraceDecisionStageRow(t, tx, 1, "gateway_decision", "not_observed", "route_selected", requestTraceDecisionValidJSON)
	newRequestTraceDecisionStageRow(t, tx, 2, "wire_attempt", "not_observed", "wire_observed", requestTraceTransportValidJSON)
	newRequestTraceDecisionStageRow(t, tx, 3, "client_metadata", "not_observed", "metadata_observed", `{"method":"POST"}`)
	newRequestTraceDecisionStageRow(t, tx, 4, "client_entry", "not_observed", "not_observed", "{}")

	for _, testCase := range requestTraceDecisionRejectedMetadataCases() {
		t.Run(testCase.name, func(t *testing.T) {
			expectPostgresError(t, tx, expectedPostgresCheckFailure, `
INSERT INTO request_trace_stages (trace_id, ordinal, stage, attempt_index, state, reason, metadata)
SELECT id, $1, $2, 0, 'not_observed', 'bounded_code', $3::jsonb FROM request_traces WHERE trace_id = $4
`, testCase.ordinal, testCase.stage, testCase.metadata, requestTraceTraceID)
		})
	}

	var surviving int64
	require.NoError(t, scanSingleRow(ctx, tx,
		`SELECT COUNT(*) FROM request_trace_stages`, nil, &surviving))
	require.Equal(t, int64(4), surviving, "only the four typed, in-policy rows may exist")
}

type requestTraceRejectedMetadataCase struct {
	name     string
	ordinal  int
	stage    string
	metadata string
}

// requestTraceDecisionRejectedMetadataCases 覆盖「261 语义保持」与「262 新增分流」两类。
func requestTraceDecisionRejectedMetadataCases() []requestTraceRejectedMetadataCase {
	return []requestTraceRejectedMetadataCase{
		{
			name: "decision stage without a decision object", ordinal: 10,
			stage: "gateway_decision", metadata: "{}",
		},
		{
			name: "decision stage with an unknown key", ordinal: 11,
			stage:    "gateway_decision",
			metadata: `{"decision":"route","outcome":"selected","source":"group","sequence":1,"url":"https://synthetic.example"}`,
		},
		{
			name: "decision stage with a transport key", ordinal: 12,
			stage:    "gateway_decision",
			metadata: `{"decision":"route","outcome":"selected","source":"group","sequence":1,"method":"POST"}`,
		},
		{
			name: "transport stage with a decision key", ordinal: 13,
			stage: "wire_attempt", metadata: `{"decision":"route"}`,
		},
		{
			name: "body stage with a decision key", ordinal: 14,
			stage: "client_entry", metadata: `{"decision":"route"}`,
		},
		{
			name: "body stage with an arbitrary object", ordinal: 15,
			stage: "client_entry", metadata: `{"anything":"goes"}`,
		},
		{
			name: "metadata as an array", ordinal: 16,
			stage: "gateway_decision", metadata: `[]`,
		},
		{
			name: "metadata as a scalar", ordinal: 17,
			stage: "gateway_decision", metadata: `"decision"`,
		},
		{
			name: "metadata beyond the 4096 byte bound", ordinal: 18,
			stage: "gateway_decision",
			metadata: `{"decision":"route","outcome":"selected","source":"group","sequence":1,"model_from":"` +
				strings.Repeat("m", 4100) + `"}`,
		},
		{
			name: "transport facts beyond the 4096 byte bound", ordinal: 19,
			stage:    "wire_attempt",
			metadata: `{"method":"POST","url":"https://synthetic.example/` + strings.Repeat("u", 4100) + `"}`,
		},
	}
}

// TestRequestTraceDecisionMigration262IsReplaySafe 证明同一份 262 SQL 可以被重放：
// 灾备重放不会因「约束已存在」失败，且重放后分流语义保持一致。
func TestRequestTraceDecisionMigration262IsReplaySafe(t *testing.T) {
	tx := newMigratedRequestTraceTx(t)
	applyRequestTraceMigrationFile(t, tx, requestTraceDecisionMigration)

	schema := requestTraceSchemaName(t, tx)
	for _, constraint := range []string{
		requestTraceMetadataShapeConstraint,
		requestTraceMetadataStageConstraint,
	} {
		requireConstraintExists(t, tx, schema, requestTraceStagesTable, constraint)
	}

	newRequestTraceDecisionStageRow(t, tx, 1, "gateway_decision", "not_observed", "route_selected", requestTraceDecisionValidJSON)
	newRequestTraceDecisionStageRow(t, tx, 2, "wire_attempt", "not_observed", "wire_observed", requestTraceTransportValidJSON)

	// 重放不得把 262 的分流收窄回 261 的单一 allowlist。
	expectPostgresError(t, tx, expectedPostgresCheckFailure, `
INSERT INTO request_trace_stages (trace_id, ordinal, stage, attempt_index, state, reason, metadata)
SELECT id, 3, 'gateway_decision', 0, 'not_observed', 'bounded_code', '{"decision":"route","method":"POST"}'::jsonb
FROM request_traces WHERE trace_id = $1
`, requestTraceTraceID)
}

// TestRequestTraceDecisionMigration262Keeps261TransportAllowlist 证明 261 判断的 14 个
// transport key 与「只有两个阶段能写非空对象」在 262 之后仍然成立。
func TestRequestTraceDecisionMigration262Keeps261TransportAllowlist(t *testing.T) {
	tx := newMigratedRequestTraceTx(t)

	fullTransport := `{"method":"POST","url":"https://synthetic.example/v1/messages","url_omitted":false,` +
		`"request_headers":{"X-Visible":["safe"]},"request_headers_omitted":0,` +
		`"response_headers":{"Content-Type":["application/json"]},"response_headers_omitted":0,` +
		`"account_id":73,"model":"claude-sonnet-4-5","protocol":"anthropic.messages",` +
		`"value_protocol":"anthropic.messages","status":200,` +
		`"started_at":"2026-09-29T09:59:59Z","ended_at":"2026-09-29T10:00:00Z"}`
	newRequestTraceDecisionStageRow(t, tx, 1, "wire_attempt", "not_observed", "wire_observed", fullTransport)

	expectPostgresError(t, tx, expectedPostgresCheckFailure, `
INSERT INTO request_trace_stages (trace_id, ordinal, stage, attempt_index, state, reason, metadata)
SELECT id, 2, 'wire_attempt', 0, 'not_observed', 'bounded_code', $1::jsonb
FROM request_traces WHERE trace_id = $2
`, `{"method":"POST","headers":{"X-Visible":["safe"]}}`, requestTraceTraceID)

	expectPostgresError(t, tx, expectedPostgresCheckFailure, `
INSERT INTO request_trace_stages (trace_id, ordinal, stage, attempt_index, state, reason, metadata)
SELECT id, 3, 'upstream_wire', 0, 'not_observed', 'bounded_code', '{"method":"POST"}'::jsonb
FROM request_traces WHERE trace_id = $1
`, requestTraceTraceID)
}

// TestRequestTraceDecisionRoundTripsThroughTheTypedRepository 用真实约束下的真实表验证
// 端到端：repository 写出的决策阶段满足 258/261/262 的全部约束，并能原样读回。
func TestRequestTraceDecisionRoundTripsThroughTheTypedRepository(t *testing.T) {
	ctx := context.Background()
	tx := newMigratedRequestTraceTx(t)
	repo := &requestTraceRepository{q: tx}

	decidedAt := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	decision := service.RequestTraceDecisionFacts{
		Decision: service.RequestTraceDecisionIdentity, Outcome: service.RequestTraceDecisionRewritten,
		Source: service.RequestTraceDecisionSourceIdentity, Sequence: 2,
		ModelFrom: "claude-sonnet-4-5", ModelTo: "claude-sonnet-4-5", DecidedAt: &decidedAt,
	}
	created, err := repo.CreateRequestTrace(ctx, service.RequestTrace{
		TraceID: requestTraceTraceID, RouteFamily: service.RequestTraceMessages, InboundEndpoint: "/v1/messages",
		CaptureState: service.RequestTraceStored, ClientStatus: 200, CreatedAt: decidedAt,
	})
	require.NoError(t, err)
	require.Equal(t, requestTraceTraceID, created.TraceID)

	stage := service.NewRequestTraceGatewayDecisionStage(requestTraceTraceID, 1, 0, "identity_rewritten", &decision)
	appended, err := repo.AppendRequestTraceStage(ctx, stage)
	require.NoError(t, err, "the migrated constraints must accept a typed decision stage")
	require.Positive(t, appended.ID)

	var storedMetadata string
	require.NoError(t, scanSingleRow(ctx, tx, `
SELECT metadata::text FROM request_trace_stages s
JOIN request_traces r ON r.id = s.trace_id WHERE r.trace_id = $1 AND s.ordinal = 1
`, []any{requestTraceTraceID}, &storedMetadata))
	require.JSONEq(t, requestTraceDecisionValidJSONKeys(decision), storedMetadata)

	detail, err := repo.GetRequestTrace(ctx, requestTraceTraceID)
	require.NoError(t, err)
	require.Len(t, detail.Stages, 1)
	readBack := detail.Stages[0]
	require.Equal(t, service.RequestTraceDecisionStage, readBack.Stage)
	require.Equal(t, service.RequestTraceNotObserved, readBack.State)
	require.Nil(t, readBack.Metadata, "a decision stage must not read back as transport facts")
	require.NotNil(t, readBack.Decision)
	require.Equal(t, decision, *readBack.Decision, "the decision projection must survive the JSONB round trip")
}

// requestTraceDecisionValidJSONKeys renders the expected stored JSON for a decision
// without depending on field order.
func requestTraceDecisionValidJSONKeys(decision service.RequestTraceDecisionFacts) string {
	return fmt.Sprintf(`{"decision":%q,"outcome":%q,"source":%q,"sequence":%d,"model_from":%q,"model_to":%q,"decided_at":%q}`,
		decision.Decision, decision.Outcome, decision.Source, decision.Sequence, decision.ModelFrom, decision.ModelTo,
		decision.DecidedAt.UTC().Format(time.RFC3339))
}

// --- fixture 与断言 helpers ------------------------------------------------

// newMigratedRequestTraceTx 创建只属于本测试的 schema，重放 258／261／262 的真实 SQL，
// 让 gatewy decision 的约束行为在真实 PostgreSQL 上被验证。
func newMigratedRequestTraceTx(t *testing.T) *sql.Tx {
	t.Helper()

	ctx := context.Background()
	tx := testTx(t)
	schema := fmt.Sprintf("trace_decision_it_%d", time.Now().UnixNano())

	_, err := tx.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err, "create isolated schema")
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+schema)
	require.NoError(t, err, "pin search_path to the isolated schema")

	_, err = tx.ExecContext(ctx, "CREATE TABLE usage_logs (id BIGSERIAL PRIMARY KEY)")
	require.NoError(t, err, "create isolated usage_logs")

	for _, name := range []string{
		requestTraceEnvelopeMigration, requestTraceFactsMigration, requestTraceDecisionMigration,
		requestTraceScopeFactsMigration, requestTraceStagePlatformMigration,
	} {
		applyRequestTraceMigrationFile(t, tx, name)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO request_traces (trace_id, route_family, inbound_endpoint) VALUES ($1, 'messages', '/v1/messages')`,
		requestTraceTraceID)
	require.NoError(t, err, "insert the trace envelope the stage rows hang off")
	return tx
}

func applyRequestTraceMigrationFile(t *testing.T, tx *sql.Tx, name string) {
	t.Helper()

	content, err := migrations.FS.ReadFile(name)
	require.NoErrorf(t, err, "read migration %s", name)
	_, err = tx.ExecContext(context.Background(), string(content))
	require.NoErrorf(t, err, "apply migration %s", name)
}

func requestTraceSchemaName(t *testing.T, tx *sql.Tx) string {
	t.Helper()

	var schema string
	require.NoError(t, scanSingleRow(context.Background(), tx, `SELECT current_schema()`, nil, &schema))
	return schema
}

func newRequestTraceDecisionStageRow(t *testing.T, tx *sql.Tx, ordinal int, stage, state, reason, metadata string) {
	t.Helper()

	_, err := tx.ExecContext(context.Background(), `
INSERT INTO request_trace_stages (trace_id, ordinal, stage, attempt_index, state, reason, metadata)
SELECT id, $1, $2, 0, $3, $4, $5::jsonb FROM request_traces WHERE trace_id = $6
`, ordinal, stage, state, reason, metadata, requestTraceTraceID)
	require.NoErrorf(t, err, "in-policy %s row with metadata %s must be stored", stage, metadata)
}
