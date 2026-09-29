//go:build unit

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

var errRequestTraceStubExecuted = errors.New("request trace stub must not execute DDL/DML")

func TestRequestTraceRepositoryRejectsInvalidCaptureWithoutDatabase(t *testing.T) {
	ctx := context.Background()
	repo := &requestTraceRepository{}
	_, err := repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{
		TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ordinal: 1, Stage: "wire",
		State: service.RequestTraceStored, Reason: "retained", ObservedBytes: 1,
	})
	require.ErrorIs(t, err, service.ErrRequestTraceRepositoryUnavailable)
}

// recordingRequestTraceQueryer captures the statement and bound arguments so tests can
// prove the projection is bound, never formatted into the SQL, and that a rejected
// projection never reaches the database at all.
type recordingRequestTraceQueryer struct {
	queries []string
	args    [][]any
}

func (q *recordingRequestTraceQueryer) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	q.queries = append(q.queries, query)
	q.args = append(q.args, append([]any(nil), args...))
	return nil, sql.ErrNoRows
}

// ExecContext is unused by the guarded write path, but the repository must hold a
// sqlExecutor; an unexpected call is reported instead of silently succeeding.
func (q *recordingRequestTraceQueryer) ExecContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	q.queries = append(q.queries, query)
	return nil, errRequestTraceStubExecuted
}

func validGatewayDecisionFacts() service.RequestTraceDecisionFacts {
	return service.RequestTraceDecisionFacts{
		Decision: service.RequestTraceDecisionModelMapping, Outcome: service.RequestTraceDecisionRewritten,
		Source: service.RequestTraceDecisionSourceAccount, Sequence: 3,
		ModelFrom: "gpt-5.3-codex", ModelTo: "gpt-5.3-codex-spark",
	}
}

func gatewayDecisionStage(t *testing.T, decision *service.RequestTraceDecisionFacts) service.RequestTraceStage {
	t.Helper()
	return service.NewRequestTraceGatewayDecisionStage("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 4, 1, "model_rewritten", decision)
}

func TestRequestTraceRepositoryRejectsInvalidDecisionStageBeforeSQL(t *testing.T) {
	invalidDecision := validGatewayDecisionFacts()
	invalidDecision.Sequence = 0

	validDecision := validGatewayDecisionFacts()
	base := gatewayDecisionStage(t, &validDecision)
	cases := []struct {
		name   string
		mutate func(*service.RequestTraceStage)
	}{
		{name: "missing decision", mutate: func(s *service.RequestTraceStage) { s.Decision = nil }},
		{name: "invalid decision", mutate: func(s *service.RequestTraceStage) { s.Decision = &invalidDecision }},
		{name: "body observed on a decision stage", mutate: func(s *service.RequestTraceStage) { s.Payload = []byte("body") }},
		{name: "decision stage not_observed only", mutate: func(s *service.RequestTraceStage) { s.State = service.RequestTraceStored }},
		{name: "transport facts on a decision stage", mutate: func(s *service.RequestTraceStage) {
			s.Metadata = &service.RequestTraceStageFacts{Method: "POST"}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stub := &recordingRequestTraceQueryer{}
			repo := &requestTraceRepository{q: stub}
			stage := base
			stage.Decision = service.CloneRequestTraceDecisionFacts(base.Decision)
			testCase.mutate(&stage)

			_, err := repo.AppendRequestTraceStage(context.Background(), stage)
			require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
			require.Empty(t, stub.queries, "a rejected projection must never reach SQL")
		})
	}

	// A decision smuggled onto any other stage is refused for the same reason.
	for _, stage := range []string{"wire_attempt", "client_metadata", "client_entry"} {
		t.Run("decision on "+stage, func(t *testing.T) {
			stub := &recordingRequestTraceQueryer{}
			repo := &requestTraceRepository{q: stub}
			smuggled := service.RequestTraceStage{
				TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ordinal: 1, Stage: stage,
				State: service.RequestTraceNotObserved, Reason: "not_observed",
				Decision: &invalidDecision,
			}
			_, err := repo.AppendRequestTraceStage(context.Background(), smuggled)
			require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
			require.Empty(t, stub.queries)
		})
	}
}

func TestRequestTraceRepositoryBindsTheTypedDecisionProjection(t *testing.T) {
	stub := &recordingRequestTraceQueryer{}
	repo := &requestTraceRepository{q: stub}
	decision := validGatewayDecisionFacts()

	_, err := repo.AppendRequestTraceStage(context.Background(), gatewayDecisionStage(t, &decision))
	require.ErrorIs(t, err, sql.ErrNoRows, "the stub returns no rows; the statement itself must have been sent")

	require.NotEmpty(t, stub.queries)
	require.NotContains(t, strings.Join(stub.queries, "\n"), `"decision"`, "the projection is bound, never interpolated")
	require.Len(t, stub.args[0], 13)
	require.Equal(t, "gateway_decision", stub.args[0][2])
	require.Equal(t, service.RequestTraceNotObserved, stub.args[0][5])
	// The body-less stage must bind SQL NULL rather than an empty bytea, which 258's
	// payload_size constraint would reject for a not_observed row.
	require.Nil(t, stub.args[0][11])
	encoded, ok := stub.args[0][12].([]byte)
	require.True(t, ok)
	var stored service.RequestTraceDecisionFacts
	require.NoError(t, json.Unmarshal(encoded, &stored))
	require.Equal(t, decision, stored)
	require.LessOrEqual(t, len(encoded), service.RequestTraceDecisionFactsLimit)
}

func TestEncodeRequestTraceStageProjectionSelectsExactlyOneTypedProjection(t *testing.T) {
	decision := validGatewayDecisionFacts()
	encoded, err := encodeRequestTraceStageProjection(gatewayDecisionStage(t, &decision))
	require.NoError(t, err)
	var decoded service.RequestTraceDecisionFacts
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, decision, decoded)

	// A body stage without facts keeps '{}' exactly like the column default.
	encoded, err = encodeRequestTraceStageProjection(service.RequestTraceStage{
		TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ordinal: 1, Stage: "client_entry",
		State: service.RequestTraceNotObserved, Reason: "not_observed",
	})
	require.NoError(t, err)
	require.Equal(t, "{}", string(encoded))

	// Transport facts ride along only on the stage that owns them.
	facts := service.RequestTraceStageFacts{Method: "POST", Protocol: "anthropic.messages"}
	encoded, err = encodeRequestTraceStageProjection(service.RequestTraceStage{
		TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ordinal: 2, Stage: "wire_attempt",
		State: service.RequestTraceNotObserved, Reason: "wire_observed", Metadata: &facts,
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"method":"POST","protocol":"anthropic.messages"}`, string(encoded))

	// The two projections cannot be swapped or mixed.
	for name, stage := range map[string]service.RequestTraceStage{
		"facts on the decision stage": {
			Stage: service.RequestTraceDecisionStage, State: service.RequestTraceNotObserved,
			Metadata: &facts,
		},
		"decision on a body stage": {
			Stage: "client_entry", State: service.RequestTraceNotObserved,
			Decision: &decision,
		},
		"decision on a transport stage": {
			Stage: "wire_attempt", State: service.RequestTraceNotObserved,
			Decision: &decision,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := encodeRequestTraceStageProjection(stage)
			require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
		})
	}
}

func TestDecodeRequestTraceStageProjectionFailsClosed(t *testing.T) {
	projection, err := decodeRequestTraceStageProjection(service.RequestTraceDecisionStage,
		[]byte(`{"decision":"identity","outcome":"rewritten","source":"identity","sequence":2,"model_from":"claude-sonnet-4-5"}`))
	require.NoError(t, err)
	require.Nil(t, projection.facts)
	require.NotNil(t, projection.decision)
	require.Equal(t, service.RequestTraceDecisionIdentity, projection.decision.Decision)

	// '{}' is the "no facts" value for a transport stage and a refusal for a decision.
	projection, err = decodeRequestTraceStageProjection("wire_attempt", []byte("{}"))
	require.NoError(t, err)
	require.Nil(t, projection.facts)
	require.Nil(t, projection.decision)
	projection, err = decodeRequestTraceStageProjection("client_entry", nil)
	require.NoError(t, err)
	require.Nil(t, projection.facts)
	require.Nil(t, projection.decision)

	transport := []byte(`{"method":"POST","url":"https://synthetic.example/v1/messages"}`)
	projection, err = decodeRequestTraceStageProjection("wire_attempt", transport)
	require.NoError(t, err)
	require.NotNil(t, projection.facts)
	require.Equal(t, "POST", projection.facts.Method)
	require.Nil(t, projection.decision)

	cases := []struct {
		name    string
		stage   string
		encoded string
	}{
		{name: "decision stage with no decision", stage: service.RequestTraceDecisionStage, encoded: `{}`},
		{name: "decision stage empty metadata", stage: service.RequestTraceDecisionStage, encoded: ``},
		{name: "decision with an unknown field", stage: service.RequestTraceDecisionStage,
			encoded: `{"decision":"route","outcome":"selected","source":"group","sequence":1,"url":"https://synthetic.example"}`},
		{name: "decision with an invalid enum", stage: service.RequestTraceDecisionStage,
			encoded: `{"decision":"authorization","outcome":"selected","source":"group","sequence":1}`},
		{name: "decision missing required fields", stage: service.RequestTraceDecisionStage, encoded: `{"sequence":1}`},
		{name: "decision array instead of object", stage: service.RequestTraceDecisionStage, encoded: `[]`},
		{name: "transport facts on the decision stage", stage: service.RequestTraceDecisionStage,
			encoded: `{"method":"POST"}`},
		{name: "decision keys on a transport stage", stage: "wire_attempt", encoded: `{"decision":"route"}`},
		{name: "non empty metadata on a body stage", stage: "client_entry", encoded: `{"method":"POST"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := decodeRequestTraceStageProjection(testCase.stage, []byte(testCase.encoded))
			require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
		})
	}

	// The 1024 byte application bound is tighter than the 4096 byte column bound, so a
	// decision blob bigger than the projection's own limit never becomes a value.
	oversized := `{"decision":"route","outcome":"selected","source":"group","sequence":1,"model_from":"` +
		strings.Repeat("m", service.RequestTraceDecisionFactsLimit) + `"}`
	_, err = decodeRequestTraceStageProjection(service.RequestTraceDecisionStage, []byte(oversized))
	require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)

	// Transport facts keep their own wider column bound.
	_, err = decodeRequestTraceStageProjection("wire_attempt",
		[]byte(`{"method":"POST","url":"https://synthetic.example/`+strings.Repeat("u", requestTraceStageMetadataLimit)+`"}`))
	require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
}

// client_status 的 0 是合法值（列默认 0），只有 nil 才表示"不过滤"。两者必须在绑定
// 上可区分，否则 ?client_status=0 会静默退化成"返回全部"，让运维以为筛选生效了。
func TestRequestTraceRepositoryBindsOptionalClientStatusFilter(t *testing.T) {
	ctx := context.Background()
	zero := 0
	// database/sql 会把 *int 解引用成具体值（nil 指针即 NULL），所以绑定的就是筛选
	// 值本身：nil 与指向 0 的指针在驱动层是两种不同的参数。
	for _, tc := range []struct {
		name   string
		filter service.RequestTraceListFilter
		bound  bool
	}{
		{name: "absent binds NULL", filter: service.RequestTraceListFilter{Page: 1, PageSize: 20}},
		{name: "zero binds 0", filter: service.RequestTraceListFilter{Page: 1, PageSize: 20, ClientStatus: &zero}, bound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &recordingRequestTraceQueryer{}
			repo := &requestTraceRepository{q: stub}
			_, _, err := repo.ListRequestTraces(ctx, tc.filter)
			require.Error(t, err, "the stub returns no rows")
			require.Len(t, stub.args, 1)
			bound, isOptional := stub.args[0][2].(*int)
			require.True(t, isOptional, "client_status must be bound as an optional value")
			if tc.bound {
				require.NotNil(t, bound)
				require.Equal(t, 0, *bound)
			} else {
				require.Nil(t, bound, "an absent filter must bind SQL NULL, never 0")
			}
			require.Contains(t, stub.queries[0], "$3::integer IS NULL OR client_status = $3")
		})
	}
}

// 越界值仍然在进 SQL 之前被拒绝：可选化不等于放宽校验。
func TestRequestTraceRepositoryRejectsOutOfRangeClientStatus(t *testing.T) {
	tooHigh := 600
	repo := &requestTraceRepository{q: &recordingRequestTraceQueryer{}}
	_, _, err := repo.ListRequestTraces(context.Background(),
		service.RequestTraceListFilter{Page: 1, PageSize: 20, ClientStatus: &tooHigh})
	require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
}

// 没有数据库句柄时必须 fail-closed：返回可识别的"不可用"，而不是 panic。
func TestRequestTraceRepositoryWithoutDatabaseHandleFailsClosed(t *testing.T) {
	repo := NewRequestTraceRepository(nil)
	require.ErrorIs(t, repo_appendStageErr(repo), service.ErrRequestTraceRepositoryUnavailable)
	_, _, err := repo.ListRequestTraces(context.Background(), service.RequestTraceListFilter{Page: 1, PageSize: 20})
	require.ErrorIs(t, err, service.ErrRequestTraceRepositoryUnavailable)
}

func repo_appendStageErr(repo service.RequestTraceRepository) error {
	_, err := repo.AppendRequestTraceStage(context.Background(), service.RequestTraceStage{
		TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ordinal: 1, Stage: "wire",
		State: service.RequestTraceStored, Reason: "retained", ObservedBytes: 1,
	})
	return err
}
