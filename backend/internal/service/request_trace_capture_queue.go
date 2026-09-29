package service

import (
	"context"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

const (
	requestTraceCaptureQueueCapacity = 8
	requestTraceCaptureMaxBytes      = 8 << 20
	requestTraceCaptureMaxStages     = 128
	requestTraceWriteTimeout         = 3 * time.Second
	requestTraceShutdownWait         = 2 * time.Second
)

var (
	requestTraceQueueIDPattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	requestTraceQueueStagePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	requestTraceQueueReasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,95}$`)
)

// ValidRequestTraceStageReason reports whether a stage reason is the bounded code the
// capture queue and the repository both accept. A reason is a code, never free text:
// the gateway decision stage in particular must not carry a sentence or a raw value.
func ValidRequestTraceStageReason(reason string) bool {
	return requestTraceQueueReasonPattern.MatchString(reason)
}

// RequestTraceCaptureStats contains counts only; no request or response values.
type RequestTraceCaptureStats struct {
	Queued      uint64
	Stored      uint64
	WriteFailed uint64
	Dropped     uint64
	Rejected    uint64
}

type requestTraceCaptureJob struct {
	trace    RequestTrace
	stages   []RequestTraceStage
	finalize bool
}

// RequestTraceCaptureQueue separates best-effort Trace writes from the gateway.
// Callers must supply already-sanitized bodies and typed redacted facts. The
// queue never holds arbitrary metadata or a request context, and bounds work.
type RequestTraceCaptureQueue struct {
	repo   RequestTraceRepository
	linker RequestTraceUsageLinker
	jobs   chan requestTraceCaptureJob
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	stopped  bool
	stopOnce sync.Once

	queued      atomic.Uint64
	stored      atomic.Uint64
	writeFailed atomic.Uint64
	dropped     atomic.Uint64
	rejected    atomic.Uint64
}

// NewRequestTraceCaptureQueue starts exactly one bounded writer. A nil repository
// is accepted for fail-closed test/deployment wiring, but cannot enqueue work.
func NewRequestTraceCaptureQueue(repo RequestTraceRepository, linkers ...RequestTraceUsageLinker) *RequestTraceCaptureQueue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &RequestTraceCaptureQueue{
		repo: repo, jobs: make(chan requestTraceCaptureJob, requestTraceCaptureQueueCapacity),
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	if len(linkers) > 0 {
		q.linker = linkers[0]
	}
	go q.run()
	return q
}

// Enqueue copies each scalar and body before returning. It never waits for the
// database or a full queue. The caller must not mutate the inputs concurrently
// while Enqueue is copying them.
func (q *RequestTraceCaptureQueue) Enqueue(trace RequestTrace, stages []RequestTraceStage) bool {
	if q == nil || q.repo == nil || !requestTraceJobValid(trace, stages) {
		if q != nil {
			q.rejected.Add(1)
		}
		return false
	}
	job := requestTraceCaptureJob{trace: trace, stages: make([]RequestTraceStage, len(stages)), finalize: trace.CaptureState == RequestTraceStored}
	if trace.CompletedAt != nil {
		completedAt := *trace.CompletedAt
		job.trace.CompletedAt = &completedAt
	}
	if trace.CleanupAfter != nil {
		cleanupAfter := *trace.CleanupAfter
		job.trace.CleanupAfter = &cleanupAfter
	}
	if job.trace.CaptureState == RequestTraceStored {
		// Until every stage is written, "stored" would lie if a later append fails.
		// A repository implementing requestTraceCaptureFinalizer can upgrade it
		// after all appends have succeeded; otherwise partial remains honest.
		job.trace.CaptureState = RequestTracePartial
	}
	for i, stage := range stages {
		job.stages[i] = stage
		job.stages[i].Payload = append([]byte(nil), stage.Payload...)
		job.stages[i].Metadata = CloneRequestTraceStageFacts(stage.Stage, stage.Metadata)
		job.stages[i].Decision = CloneRequestTraceDecisionFacts(stage.Decision)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		q.dropped.Add(1)
		return false
	}
	select {
	case q.jobs <- job:
		q.queued.Add(1)
		return true
	default:
		q.dropped.Add(1)
		return false
	}
}

func requestTraceJobValid(trace RequestTrace, stages []RequestTraceStage) bool {
	if !requestTraceQueueIDPattern.MatchString(trace.TraceID) || trace.UsageLogID != nil ||
		len(stages) > requestTraceCaptureMaxStages {
		return false
	}
	switch trace.RouteFamily {
	case RequestTraceMessages, RequestTraceChatCompletions, RequestTraceResponses:
	default:
		return false
	}
	switch trace.InboundEndpoint {
	case "/v1/messages", "/v1/chat/completions", "/v1/responses", "/v1/responses/compact":
	default:
		return false
	}
	if trace.ClientStatus < 0 || trace.ClientStatus > 599 || (trace.CaptureState == RequestTraceStored && len(stages) == 0) {
		return false
	}
	switch trace.CaptureState {
	case RequestTraceNotObserved, RequestTraceStored, RequestTracePartial, RequestTraceWriteFailed:
	default:
		return false
	}
	var total int64
	lastOrdinal := 0
	for _, stage := range stages {
		if stage.Ordinal <= lastOrdinal {
			return false
		}
		lastOrdinal = stage.Ordinal
		if stage.TraceID != trace.TraceID || stage.Ordinal <= 0 || stage.AttemptIndex < 0 || stage.AttemptIndex > 1000 ||
			!requestTraceQueueStagePattern.MatchString(stage.Stage) || !ValidRequestTraceStageReason(stage.Reason) ||
			!ValidRequestTraceStageFacts(stage.Stage, stage.Metadata) || !ValidRequestTraceDecisionStage(stage) ||
			(stage.RedactionUnverified && stage.State != RequestTraceUnverified && stage.State != RequestTraceTruncated && stage.State != RequestTraceNotObserved) ||
			stage.ObservedBytes < 0 || stage.DroppedEvents < 0 ||
			len(stage.Payload) > RequestTraceStagePayloadLimit {
			return false
		}
		switch stage.View {
		case "", "transmitted", "decoded", "wire", "received", "downstream":
		default:
			return false
		}
		switch stage.State {
		case RequestTraceNotObserved, RequestTraceStored, RequestTraceTruncated,
			RequestTraceUnsupported, RequestTraceUnverified, RequestTraceWriteFailed:
		default:
			return false
		}
		if len(stage.Payload) > 0 && stage.State != RequestTraceStored && stage.State != RequestTraceTruncated && stage.State != RequestTraceUnverified {
			return false
		}
		if stage.State == RequestTraceStored && len(stage.Payload) == 0 {
			return false
		}
		if stage.State == RequestTraceUnverified && !stage.RedactionUnverified {
			return false
		}
		total += int64(len(stage.Payload))
		if total > requestTraceCaptureMaxBytes {
			return false
		}
	}
	return true
}

// requestTraceCaptureFinalizer is optional: legacy/new repository adapters that
// cannot atomically mark completion leave envelopes in truthful partial state.
type requestTraceCaptureFinalizer interface {
	FinalizeRequestTraceCapture(ctx context.Context, traceID string, state RequestTraceCaptureState) error
}

type requestTraceUsageReconciler interface {
	ReconcileRequestTraceUsage(ctx context.Context, traceID string) (bool, error)
}

func (q *RequestTraceCaptureQueue) run() {
	defer close(q.done)
	for {
		if q.ctx.Err() != nil {
			q.dropQueued()
			return
		}
		select {
		case <-q.ctx.Done():
			q.dropQueued()
			return
		case job := <-q.jobs:
			if q.ctx.Err() != nil {
				q.dropped.Add(1)
				q.dropQueued()
				return
			}
			q.persist(job)
		}
	}
}

func (q *RequestTraceCaptureQueue) persist(job requestTraceCaptureJob) {
	ctx, cancel := context.WithTimeout(q.ctx, requestTraceWriteTimeout)
	defer cancel()
	if _, err := q.repo.CreateRequestTrace(ctx, job.trace); err != nil {
		q.writeFailed.Add(1)
		return
	}
	if reconciler, ok := q.linker.(requestTraceUsageReconciler); ok {
		if _, err := reconciler.ReconcileRequestTraceUsage(ctx, job.trace.TraceID); err != nil {
			q.writeFailed.Add(1)
		}
	}
	for _, stage := range job.stages {
		if _, err := q.repo.AppendRequestTraceStage(ctx, stage); err != nil {
			q.writeFailed.Add(1)
			return
		}
	}
	if finalizer, ok := q.repo.(requestTraceCaptureFinalizer); ok && job.finalize {
		if err := finalizer.FinalizeRequestTraceCapture(ctx, job.trace.TraceID, RequestTraceStored); err != nil {
			q.writeFailed.Add(1)
			return
		}
	}
	q.stored.Add(1)
}

func (q *RequestTraceCaptureQueue) dropQueued() {
	for {
		select {
		case <-q.jobs:
			q.dropped.Add(1)
		default:
			return
		}
	}
}

// Stop cancels the in-flight write and drops queued work. It never waits more
// than two seconds even when a broken repository ignores context cancellation.
func (q *RequestTraceCaptureQueue) Stop() {
	if q == nil {
		return
	}
	q.stopOnce.Do(func() {
		q.mu.Lock()
		q.stopped = true
		q.cancel()
		q.mu.Unlock()
	})
	select {
	case <-q.done:
	case <-time.After(requestTraceShutdownWait):
	}
}

func (q *RequestTraceCaptureQueue) Stats() RequestTraceCaptureStats {
	if q == nil {
		return RequestTraceCaptureStats{}
	}
	return RequestTraceCaptureStats{
		Queued: q.queued.Load(), Stored: q.stored.Load(), WriteFailed: q.writeFailed.Load(),
		Dropped: q.dropped.Load(), Rejected: q.rejected.Load(),
	}
}
