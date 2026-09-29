//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type traceQueueRepo struct {
	mu         sync.Mutex
	created    []RequestTrace
	stages     []RequestTraceStage
	createSeen chan struct{}
	allowWrite chan struct{}
	appendErr  error
	done       chan struct{}
}

func (r *traceQueueRepo) CreateRequestTrace(_ context.Context, trace RequestTrace) (RequestTrace, error) {
	if r.createSeen != nil {
		select {
		case r.createSeen <- struct{}{}:
		default:
		}
	}
	if r.allowWrite != nil {
		<-r.allowWrite
	}
	r.mu.Lock()
	r.created = append(r.created, trace)
	r.mu.Unlock()
	return trace, nil
}

func (r *traceQueueRepo) AppendRequestTraceStage(_ context.Context, stage RequestTraceStage) (RequestTraceStage, error) {
	if r.appendErr != nil {
		return RequestTraceStage{}, r.appendErr
	}
	r.mu.Lock()
	r.stages = append(r.stages, stage)
	r.mu.Unlock()
	if r.done != nil {
		select {
		case r.done <- struct{}{}:
		default:
		}
	}
	return stage, nil
}

func (*traceQueueRepo) ListRequestTraces(context.Context, RequestTraceListFilter) ([]RequestTrace, int64, error) {
	return nil, 0, nil
}
func (*traceQueueRepo) GetRequestTrace(context.Context, string) (*RequestTraceDetail, error) {
	return nil, nil
}
func (*traceQueueRepo) DeleteExpiredUnlinkedRequestTraces(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func traceQueueExample() (RequestTrace, []RequestTraceStage) {
	id := strings.Repeat("a", 32)
	completed := time.Now().UTC()
	trace := RequestTrace{
		TraceID: id, RouteFamily: RequestTraceMessages, InboundEndpoint: "/v1/messages",
		CaptureState: RequestTraceStored, ClientStatus: 200, CompletedAt: &completed,
	}
	stage := RequestTraceStage{
		TraceID: id, Ordinal: 1, Stage: "client_entry", State: RequestTraceStored,
		Reason: "retained", Payload: []byte("known-safe-payload"), ObservedBytes: int64(len("known-safe-payload")),
	}
	return trace, []RequestTraceStage{stage}
}

func TestRequestTraceCaptureQueueCopiesDataWithoutHoldingRequestContext(t *testing.T) {
	repo := &traceQueueRepo{createSeen: make(chan struct{}, 1), allowWrite: make(chan struct{}), done: make(chan struct{}, 1)}
	queue := NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	trace, stages := traceQueueExample()
	originalCompletedAt := *trace.CompletedAt
	require.True(t, queue.Enqueue(trace, stages))
	select {
	case <-repo.createSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never received queued trace")
	}
	*trace.CompletedAt = trace.CompletedAt.Add(time.Hour)
	stages[0].Payload[0] = 'X'
	stages[0].Metadata = &RequestTraceStageFacts{Method: "GET"}
	close(repo.allowWrite)
	select {
	case <-repo.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not persist the stage")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Len(t, repo.created, 1)
	require.Nil(t, repo.created[0].UsageLogID)
	require.Equal(t, originalCompletedAt, *repo.created[0].CompletedAt)
	require.Equal(t, RequestTracePartial, repo.created[0].CaptureState, "a staged write cannot claim complete before every stage has persisted")
	require.Len(t, repo.stages, 1)
	require.Equal(t, "known-safe-payload", string(repo.stages[0].Payload))
	require.Empty(t, repo.stages[0].Metadata)
}

func TestRequestTraceCaptureQueueCopiesOnlyRedactedTypedFacts(t *testing.T) {
	repo := &traceQueueRepo{createSeen: make(chan struct{}, 1), allowWrite: make(chan struct{}), done: make(chan struct{}, 1)}
	queue := NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	trace, stages := traceQueueExample()
	stages[0].Stage = "client_metadata"
	stages[0].State = RequestTraceNotObserved
	stages[0].Payload = nil
	stages[0].Metadata = &RequestTraceStageFacts{Method: "POST", RequestHeaders: http.Header{"Authorization": {"[REDACTED]"}, "X-Visible": {"safe"}}}
	require.True(t, queue.Enqueue(trace, stages))
	select {
	case <-repo.createSeen:
	case <-time.After(time.Second):
		t.Fatal("worker never started")
	}
	stages[0].Metadata.RequestHeaders.Set("X-Visible", "tampered")
	close(repo.allowWrite)
	select {
	case <-repo.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not persist redacted facts")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Len(t, repo.stages, 1)
	require.Equal(t, "safe", repo.stages[0].Metadata.RequestHeaders.Get("X-Visible"))
	require.Equal(t, "[REDACTED]", repo.stages[0].Metadata.RequestHeaders.Get("Authorization"))
}

func TestRequestTraceCaptureQueueAcceptsRedactionExpansion(t *testing.T) {
	repo := &traceQueueRepo{done: make(chan struct{}, 1)}
	queue := NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	trace, stages := traceQueueExample()
	stages[0].Payload = []byte(`{"key":"[REDACTED]"}`)
	stages[0].ObservedBytes = int64(len(`{"key":"x"}`))
	require.True(t, queue.Enqueue(trace, stages), "sanitized JSON may grow without adding observed wire bytes")
	select {
	case <-repo.done:
	case <-time.After(time.Second):
		require.FailNow(t, "redacted stage was not persisted")
	}
}

func TestRequestTraceCaptureQueueRejectsExcessPayloadAndArbitraryMetadata(t *testing.T) {
	repo := &traceQueueRepo{}
	queue := NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	trace, stages := traceQueueExample()
	stages[0].Payload = make([]byte, RequestTraceStagePayloadLimit+1)
	stages[0].ObservedBytes = int64(len(stages[0].Payload))
	require.False(t, queue.Enqueue(trace, stages))
	stages[0].Payload = []byte("safe")
	stages[0].ObservedBytes = 4
	stages[0].Metadata = &RequestTraceStageFacts{RequestHeaders: http.Header{"Authorization": {"Bearer secret"}}}
	require.False(t, queue.Enqueue(trace, stages), "unredacted headers must never reach storage")
	stages[0].Metadata = nil
	stages[0].RedactionUnverified = true
	require.False(t, queue.Enqueue(trace, stages), "a stage cannot be both safe stored and redaction-unverified")
	stages[0].RedactionUnverified = false
	owner := int64(123)
	trace.UsageLogID = &owner
	require.False(t, queue.Enqueue(trace, stages), "ownership is established only by the verified linker")
	trace.UsageLogID = nil
	stages[0].State = RequestTracePartial
	require.False(t, queue.Enqueue(trace, stages), "invalid stage state must be rejected before persistence")
	stages[0].State = RequestTraceStored
	trace.CaptureState = RequestTraceUnsupported
	require.False(t, queue.Enqueue(trace, stages), "invalid envelope state must be rejected before persistence")
	trace.CaptureState = RequestTraceStored
	require.False(t, queue.Enqueue(trace, nil), "stored Trace cannot claim completeness without any stage")
	stages = append(stages, RequestTraceStage{
		TraceID: trace.TraceID, Ordinal: 1, Stage: "wire", State: RequestTraceNotObserved, Reason: "not_observed",
	})
	require.False(t, queue.Enqueue(trace, stages), "duplicate ordinals must not masquerade as complete stages")
	stages = make([]RequestTraceStage, 9)
	for i := range stages {
		stages[i] = RequestTraceStage{
			TraceID: trace.TraceID, Ordinal: i + 1, Stage: "wire", State: RequestTraceStored,
			Reason: "retained", Payload: make([]byte, 1<<20), ObservedBytes: 1 << 20,
		}
	}
	require.False(t, queue.Enqueue(trace, stages), "aggregate snapshot must not exceed 8 MiB")
	require.Equal(t, uint64(9), queue.Stats().Rejected)
}

// A gateway_decision stage crosses the queue as a copied, typed projection: the
// caller may keep mutating its own value and no free-form decision can enter.
func TestRequestTraceCaptureQueueCopiesOnlyTheTypedDecision(t *testing.T) {
	repo := &traceQueueRepo{createSeen: make(chan struct{}, 1), allowWrite: make(chan struct{}), done: make(chan struct{}, 1)}
	queue := NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	trace, _ := traceQueueExample()
	decidedAt := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	decision := RequestTraceDecisionFacts{
		Decision: RequestTraceDecisionModelMapping, Outcome: RequestTraceDecisionRewritten,
		Source: RequestTraceDecisionSourceAccount, Sequence: 3,
		ModelFrom: "gpt-5.3-codex", ModelTo: "gpt-5.3-codex-spark", DecidedAt: &decidedAt,
	}
	stages := []RequestTraceStage{NewRequestTraceGatewayDecisionStage(trace.TraceID, 1, 1, "model_rewritten", &decision)}
	require.True(t, queue.Enqueue(trace, stages))
	select {
	case <-repo.createSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never received the queued trace")
	}
	decision.ModelTo = "tampered"
	decision.DecidedAt = nil
	close(repo.allowWrite)
	select {
	case <-repo.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not persist the decision stage")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Len(t, repo.stages, 1)
	require.Equal(t, RequestTraceDecisionStage, repo.stages[0].Stage)
	require.Empty(t, repo.stages[0].Payload, "a decision stage is body-less")
	require.Nil(t, repo.stages[0].Metadata, "a decision stage carries no transport facts")
	require.NotNil(t, repo.stages[0].Decision)
	require.Equal(t, 3, repo.stages[0].Decision.Sequence)
	require.Equal(t, "gpt-5.3-codex-spark", repo.stages[0].Decision.ModelTo)
	require.Equal(t, decidedAt, *repo.stages[0].Decision.DecidedAt)
}

func TestRequestTraceCaptureQueueRejectsDecisionsOutsideTheTypedContract(t *testing.T) {
	brokenDecision := RequestTraceDecisionFacts{
		Decision: RequestTraceDecisionRoute, Outcome: RequestTraceDecisionSelected,
		Source: RequestTraceDecisionSourceGroup, Sequence: 0,
	}
	validDecision := RequestTraceDecisionFacts{
		Decision: RequestTraceDecisionRoute, Outcome: RequestTraceDecisionSelected,
		Source: RequestTraceDecisionSourceGroup, Sequence: 1,
	}
	cases := []struct {
		name   string
		mutate func(*RequestTraceStage)
	}{
		{name: "missing decision", mutate: func(s *RequestTraceStage) { s.Decision = nil }},
		{name: "invalid decision", mutate: func(s *RequestTraceStage) { s.Decision = &brokenDecision }},
		{name: "decision with an observed body", mutate: func(s *RequestTraceStage) { s.Payload = []byte("body") }},
		{name: "decision that claims a stored body", mutate: func(s *RequestTraceStage) { s.State = RequestTraceStored }},
		{name: "decision carrying transport facts", mutate: func(s *RequestTraceStage) {
			s.Metadata = &RequestTraceStageFacts{Method: "POST"}
		}},
		{name: "decision with a free-form reason", mutate: func(s *RequestTraceStage) { s.Reason = "Route Selected" }},
		{name: "decision with an unbounded reason", mutate: func(s *RequestTraceStage) {
			s.Reason = "route_" + strings.Repeat("x", 96)
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repo := &traceQueueRepo{}
			queue := NewRequestTraceCaptureQueue(repo)
			defer queue.Stop()
			trace, _ := traceQueueExample()
			copied := validDecision
			stage := NewRequestTraceGatewayDecisionStage(trace.TraceID, 1, 1, "route_selected", &copied)
			testCase.mutate(&stage)
			require.False(t, queue.Enqueue(trace, []RequestTraceStage{stage}))
			require.Equal(t, uint64(1), queue.Stats().Rejected)
		})
	}

	// The decision stage name is not a password: any other stage carrying a decision
	// is refused as well, so a transport stage cannot smuggle one.
	repo := &traceQueueRepo{}
	queue := NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	trace, _ := traceQueueExample()
	smuggled := RequestTraceStage{
		TraceID: trace.TraceID, Ordinal: 1, Stage: "wire_attempt", AttemptIndex: 1,
		State: RequestTraceNotObserved, Reason: "wire_observed", Decision: &validDecision,
	}
	require.False(t, queue.Enqueue(trace, []RequestTraceStage{smuggled}))
}

func TestRequestTraceCaptureQueueDropsWhenFullWithoutBlockingGateway(t *testing.T) {
	repo := &traceQueueRepo{createSeen: make(chan struct{}, 1), allowWrite: make(chan struct{})}
	queue := NewRequestTraceCaptureQueue(repo)
	trace, stages := traceQueueExample()
	require.True(t, queue.Enqueue(trace, stages))
	select {
	case <-repo.createSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never picked first item")
	}
	accepted := 0
	for i := 0; i < 64; i++ {
		if queue.Enqueue(trace, stages) {
			accepted++
		}
	}
	require.Positive(t, accepted)
	require.Less(t, accepted, 64, "the in-memory queue must be bounded")
	require.Positive(t, queue.Stats().Dropped)
	close(repo.allowWrite)
	queue.Stop()
	require.False(t, queue.Enqueue(trace, stages), "stop must reject new work")
}

type traceQueueFinalizerRepo struct {
	traceQueueRepo
	finalized chan RequestTraceCaptureState
}

func (r *traceQueueFinalizerRepo) FinalizeRequestTraceCapture(_ context.Context, _ string, state RequestTraceCaptureState) error {
	r.finalized <- state
	return nil
}

func TestRequestTraceCaptureQueueFinalizesOnlyAfterAllStagesSucceed(t *testing.T) {
	repo := &traceQueueFinalizerRepo{
		traceQueueRepo: traceQueueRepo{done: make(chan struct{}, 1)},
		finalized:      make(chan RequestTraceCaptureState, 1),
	}
	queue := NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	trace, stages := traceQueueExample()
	require.True(t, queue.Enqueue(trace, stages))
	select {
	case <-repo.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not append stage")
	}
	select {
	case state := <-repo.finalized:
		require.Equal(t, RequestTraceStored, state)
	case <-time.After(2 * time.Second):
		t.Fatal("complete trace was not finalized")
	}
}

func TestRequestTraceCaptureQueueDoesNotUpgradeIncompleteTraceToStored(t *testing.T) {
	repo := &traceQueueFinalizerRepo{
		traceQueueRepo: traceQueueRepo{done: make(chan struct{}, 1)},
		finalized:      make(chan RequestTraceCaptureState, 1),
	}
	queue := NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	trace, stages := traceQueueExample()
	trace.CaptureState = RequestTracePartial
	require.True(t, queue.Enqueue(trace, stages))
	select {
	case <-repo.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not append the stage")
	}
	queue.Stop()
	select {
	case state := <-repo.finalized:
		t.Fatalf("incomplete trace was falsely marked %s", state)
	default:
	}
}

func TestRequestTraceCaptureQueueStopDoesNotHangOnUncooperativeStore(t *testing.T) {
	repo := &traceQueueRepo{createSeen: make(chan struct{}, 1), allowWrite: make(chan struct{})}
	queue := NewRequestTraceCaptureQueue(repo)
	trace, stages := traceQueueExample()
	require.True(t, queue.Enqueue(trace, stages))
	select {
	case <-repo.createSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never picked the item")
	}
	stopped := make(chan struct{})
	go func() {
		queue.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown waited indefinitely for storage")
	}
	close(repo.allowWrite)
}

func TestRequestTraceCaptureQueueCountsPartialWrites(t *testing.T) {
	repo := &traceQueueRepo{appendErr: errors.New("database unavailable"), createSeen: make(chan struct{}, 1)}
	queue := NewRequestTraceCaptureQueue(repo)
	trace, stages := traceQueueExample()
	require.True(t, queue.Enqueue(trace, stages))
	select {
	case <-repo.createSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never attempted to persist trace")
	}
	queue.Stop()
	stats := queue.Stats()
	require.Equal(t, uint64(1), stats.WriteFailed)
	require.Zero(t, stats.Stored)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Len(t, repo.created, 1)
	require.Equal(t, RequestTracePartial, repo.created[0].CaptureState)
}
