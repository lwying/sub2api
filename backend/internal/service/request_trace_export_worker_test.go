//go:build unit

package service

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The wiring contract: the concrete export service must satisfy the runner
// interface the worker accepts.
var _ RequestTraceExportRunner = (*RequestTraceExportService)(nil)

// traceExportWorkerRunnerStub stands in for RequestTraceExportService. It only
// exercises the two calls the worker is allowed to make and never inspects
// payload values.
type traceExportWorkerRunnerStub struct {
	mu           sync.Mutex
	queue        []traceExportWorkerRunCall
	emptyErr     error
	hold         chan struct{}
	waitForCtx   bool
	runCalls     int
	cleanupCalls int
	cleanupLimit int
	cleanupErr   error
	cleaned      int64
	started      chan struct{}
}

type traceExportWorkerRunCall struct {
	task RequestTraceExportTask
	err  error
}

func (s *traceExportWorkerRunnerStub) RunOnce(ctx context.Context) (RequestTraceExportTask, error) {
	s.mu.Lock()
	s.runCalls++
	hold, waitForCtx, seen := s.hold, s.waitForCtx, s.started
	queued := len(s.queue) > 0
	var call traceExportWorkerRunCall
	if queued {
		call = s.queue[0]
		s.queue = s.queue[1:]
	}
	emptyErr := s.emptyErr
	s.mu.Unlock()
	if seen != nil {
		select {
		case seen <- struct{}{}:
		default:
		}
	}
	if hold != nil {
		if waitForCtx {
			select {
			case <-ctx.Done():
				return RequestTraceExportTask{}, ctx.Err()
			case <-hold:
				return RequestTraceExportTask{}, ctx.Err()
			}
		}
		<-hold
		return RequestTraceExportTask{}, ctx.Err()
	}
	if !queued {
		if emptyErr != nil {
			return RequestTraceExportTask{}, emptyErr
		}
		return RequestTraceExportTask{}, ErrRequestTraceExportNotFound
	}
	return call.task, call.err
}

func (s *traceExportWorkerRunnerStub) CleanupExpired(ctx context.Context, limit int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupCalls++
	s.cleanupLimit = limit
	return s.cleaned, s.cleanupErr
}

func (s *traceExportWorkerRunnerStub) counts() (runs, cleanups, cleanLimit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runCalls, s.cleanupCalls, s.cleanupLimit
}

func waitForGoroutineBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		current := runtime.NumGoroutine()
		if current <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker goroutine did not exit: baseline=%d current=%d", baseline, current)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRequestTraceExportWorkerDisabledClaimsNothingButStillSweepsExpiredFiles(t *testing.T) {
	stub := &traceExportWorkerRunnerStub{emptyErr: ErrRequestTraceExportDisabled, cleaned: 2}
	worker := NewRequestTraceExportWorker(stub, RequestTraceExportWorkerOptions{MaxTasksPerTick: 4, CleanupEvery: time.Hour})
	now := time.Now()
	worker.now = func() time.Time { return now }

	// Disabling export stops new tasks, but the rollback promise that already
	// completed files still expire keeps the sweep running.
	result, err := worker.Tick(context.Background())
	require.NoError(t, err)
	require.True(t, result.Disabled)
	require.Zero(t, result.Ran)
	require.True(t, result.CleanupRan)
	require.Equal(t, int64(2), result.Cleaned)
	runs, cleanups, limit := stub.counts()
	require.Equal(t, 1, runs, "a disabled capability must stop the claim loop instead of retrying per tick")
	require.Equal(t, 1, cleanups, "scheduled expiry of completed files outlives the capability")
	require.Equal(t, RequestTraceExportWorkerCleanupLimit, limit)
	require.Equal(t, int64(1), worker.Stats().DisabledTicks)
	require.Equal(t, int64(1), worker.Stats().Cleanups)

	// The sweep keeps its own cadence instead of following the re-check interval.
	result, err = worker.Tick(context.Background())
	require.NoError(t, err)
	require.True(t, result.Disabled)
	require.False(t, result.CleanupRan)
	_, cleanups, _ = stub.counts()
	require.Equal(t, 1, cleanups)

	now = now.Add(time.Hour)
	result, err = worker.Tick(context.Background())
	require.NoError(t, err)
	require.True(t, result.Disabled)
	require.True(t, result.CleanupRan)
	_, cleanups, _ = stub.counts()
	require.Equal(t, 2, cleanups)

	// A sweep that fails while disabled is reported rather than swallowed, and
	// is retried on the next tick so expiry cannot stall silently.
	failing := &traceExportWorkerRunnerStub{emptyErr: ErrRequestTraceExportDisabled, cleanupErr: ErrRequestTraceExportUnavailable}
	retried := NewRequestTraceExportWorker(failing, RequestTraceExportWorkerOptions{CleanupEvery: time.Hour})
	retried.now = func() time.Time { return now }
	result, err = retried.Tick(context.Background())
	require.Equal(t, ErrRequestTraceExportUnavailable, err)
	require.True(t, result.Disabled)
	require.True(t, result.CleanupRan)
	result, err = retried.Tick(context.Background())
	require.Equal(t, ErrRequestTraceExportUnavailable, err)
	require.True(t, result.CleanupRan)
	_, cleanups, _ = failing.counts()
	require.Equal(t, 2, cleanups)

	// A worker without a runner must stay inert rather than panic.
	var absent *RequestTraceExportWorker
	_, err = absent.Tick(context.Background())
	require.NoError(t, err)
	empty := NewRequestTraceExportWorker(nil, RequestTraceExportWorkerOptions{})
	inert, err := empty.Tick(context.Background())
	require.NoError(t, err)
	require.True(t, inert.Disabled)
	empty.Start()
	empty.Stop()
}

func TestRequestTraceExportWorkerDrainsPendingTasksUpToPerTickBound(t *testing.T) {
	done := RequestTraceExportTask{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: RequestTraceExportCompleted}
	stub := &traceExportWorkerRunnerStub{queue: []traceExportWorkerRunCall{{task: done}, {task: done}, {task: done}}}
	worker := NewRequestTraceExportWorker(stub, RequestTraceExportWorkerOptions{MaxTasksPerTick: 1000, CleanupLimit: 1000})
	require.Equal(t, 16, worker.opts.MaxTasksPerTick, "the per-tick budget is clamped")
	result, err := worker.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, result.Ran)
	require.Equal(t, 3, result.Completed)
	require.True(t, result.Idle, "the claim loop stops once the store has nothing pending")
	runs, cleanups, limit := stub.counts()
	require.Equal(t, 4, runs)
	require.Equal(t, 1, cleanups)
	require.Equal(t, 500, limit, "the cleanup page is clamped to the value the service accepts")
	stats := worker.Stats()
	require.Equal(t, int64(3), stats.TasksRun)
	require.Equal(t, int64(3), stats.TasksCompleted)
	require.Equal(t, int64(1), stats.Cleanups)

	// No work left and the cleanup interval has not elapsed: nothing is claimed.
	result, err = worker.Tick(context.Background())
	require.NoError(t, err)
	require.True(t, result.Idle)
	require.Zero(t, result.Ran)
	require.False(t, result.CleanupRan)
	runs, _, _ = stub.counts()
	require.Equal(t, 5, runs)

	// One worker never drains more than its per-tick budget.
	bounded := &traceExportWorkerRunnerStub{queue: make([]traceExportWorkerRunCall, 5)}
	for i := range bounded.queue {
		bounded.queue[i] = traceExportWorkerRunCall{task: done}
	}
	limited := NewRequestTraceExportWorker(bounded, RequestTraceExportWorkerOptions{MaxTasksPerTick: 2})
	result, err = limited.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, result.Ran)
	require.False(t, result.Idle)
	runs, _, _ = bounded.counts()
	require.Equal(t, 2, runs)
}

func TestRequestTraceExportWorkerReportsFailuresAsBoundedSentinels(t *testing.T) {
	failed := RequestTraceExportTask{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: RequestTraceExportFailed}
	stub := &traceExportWorkerRunnerStub{queue: []traceExportWorkerRunCall{{task: failed, err: ErrRequestTraceExportLimit}}}
	worker := NewRequestTraceExportWorker(stub, RequestTraceExportWorkerOptions{})
	result, err := worker.Tick(context.Background())
	require.Equal(t, ErrRequestTraceExportLimit, err, "only the sentinel is surfaced, never a value")
	require.Equal(t, 1, result.Ran)
	require.Equal(t, 1, result.Failed)
	require.False(t, result.Busy, "a task that failed its own size or runtime budget is not writer contention")
	require.Equal(t, int64(1), worker.Stats().TasksFailed)

	// A claim refused before any task was taken reports contention.
	busy := &traceExportWorkerRunnerStub{queue: []traceExportWorkerRunCall{{err: ErrRequestTraceExportLimit}}}
	contended := NewRequestTraceExportWorker(busy, RequestTraceExportWorkerOptions{})
	result, err = contended.Tick(context.Background())
	require.Equal(t, ErrRequestTraceExportLimit, err)
	require.True(t, result.Busy)
	require.Zero(t, result.Ran)

	transient := &traceExportWorkerRunnerStub{emptyErr: ErrRequestTraceExportUnavailable}
	other := NewRequestTraceExportWorker(transient, RequestTraceExportWorkerOptions{})
	result, err = other.Tick(context.Background())
	require.Equal(t, ErrRequestTraceExportUnavailable, err)
	require.Zero(t, result.Ran)
	require.False(t, result.Idle)
	require.False(t, result.Busy)
	require.Equal(t, int64(1), other.Stats().Failures)
}

func TestRequestTraceExportWorkerCleansAtStartupThenPerInterval(t *testing.T) {
	stub := &traceExportWorkerRunnerStub{cleaned: 3}
	worker := NewRequestTraceExportWorker(stub, RequestTraceExportWorkerOptions{CleanupEvery: time.Hour})
	now := time.Now()
	worker.now = func() time.Time { return now }

	// Startup cleanup reclaims files abandoned by a previous process.
	result, err := worker.Tick(context.Background())
	require.NoError(t, err)
	require.True(t, result.CleanupRan)
	require.Equal(t, int64(3), result.Cleaned)
	_, cleanups, _ := stub.counts()
	require.Equal(t, 1, cleanups)

	result, err = worker.Tick(context.Background())
	require.NoError(t, err)
	require.False(t, result.CleanupRan)
	_, cleanups, _ = stub.counts()
	require.Equal(t, 1, cleanups)

	now = now.Add(time.Hour)
	result, err = worker.Tick(context.Background())
	require.NoError(t, err)
	require.True(t, result.CleanupRan)
	_, cleanups, _ = stub.counts()
	require.Equal(t, 2, cleanups)

	// A failed cleanup is retried on the next tick rather than waiting a full interval.
	failing := &traceExportWorkerRunnerStub{cleanupErr: ErrRequestTraceExportUnavailable}
	retried := NewRequestTraceExportWorker(failing, RequestTraceExportWorkerOptions{CleanupEvery: time.Hour})
	retried.now = func() time.Time { return now }
	result, err = retried.Tick(context.Background())
	require.Equal(t, ErrRequestTraceExportUnavailable, err)
	require.True(t, result.CleanupRan)
	result, err = retried.Tick(context.Background())
	require.Equal(t, ErrRequestTraceExportUnavailable, err)
	require.True(t, result.CleanupRan)
	_, cleanups, _ = failing.counts()
	require.Equal(t, 2, cleanups)
}

func TestRequestTraceExportWorkerBoundsEachRunnerCall(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	stub := &traceExportWorkerRunnerStub{hold: hold, waitForCtx: true}
	worker := NewRequestTraceExportWorker(stub, RequestTraceExportWorkerOptions{RunTimeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := worker.Tick(context.Background())
	elapsed := time.Since(start)
	require.ErrorIs(t, err, ErrRequestTraceExportWorkerTimeout)
	require.Less(t, elapsed, 5*time.Second)
	require.GreaterOrEqual(t, elapsed, 20*time.Millisecond)
	require.Equal(t, int64(1), worker.Stats().Failures)
}

func TestRequestTraceExportWorkerPacingFollowsOutcome(t *testing.T) {
	worker := NewRequestTraceExportWorker(&traceExportWorkerRunnerStub{}, RequestTraceExportWorkerOptions{Interval: time.Second, IdleInterval: 2 * time.Second})
	require.Equal(t, RequestTraceExportWorkerDisabledInterval, worker.nextInterval(RequestTraceExportWorkerTickResult{Disabled: true}))
	require.Equal(t, 2*time.Second, worker.nextInterval(RequestTraceExportWorkerTickResult{Idle: true}))
	require.Equal(t, time.Second, worker.nextInterval(RequestTraceExportWorkerTickResult{Ran: 1}))
	require.Equal(t, time.Second, worker.nextInterval(RequestTraceExportWorkerTickResult{CleanupRan: true}))
	require.Equal(t, time.Second, worker.nextInterval(RequestTraceExportWorkerTickResult{}))
}

func TestRequestTraceExportWorkerStartStopIsBoundedAndLeavesNoGoroutine(t *testing.T) {
	stub := &traceExportWorkerRunnerStub{}
	worker := NewRequestTraceExportWorker(stub, RequestTraceExportWorkerOptions{
		Interval: 5 * time.Millisecond, IdleInterval: 5 * time.Millisecond, StopTimeout: 2 * time.Second,
	})
	baseline := runtime.NumGoroutine()

	start := time.Now()
	worker.Stop()
	require.Less(t, time.Since(start), time.Second, "Stop before Start must not block for the stop timeout")
	worker.Start()
	worker.Start()

	require.Eventually(t, func() bool {
		runs, _, _ := stub.counts()
		return runs >= 3
	}, 3*time.Second, 5*time.Millisecond)

	start = time.Now()
	worker.Stop()
	require.Less(t, time.Since(start), 2*time.Second)
	worker.Stop()
	waitForGoroutineBaseline(t, baseline)

	runs, _, _ := stub.counts()
	time.Sleep(50 * time.Millisecond)
	stopped, _, _ := stub.counts()
	require.Equal(t, runs, stopped, "no tick may run after Stop returned")
}

func TestRequestTraceExportWorkerStopStaysBoundedWhenRunnerIgnoresCancellation(t *testing.T) {
	hold := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hold) }) }
	t.Cleanup(release)
	stub := &traceExportWorkerRunnerStub{hold: hold, started: make(chan struct{}, 1)}
	worker := NewRequestTraceExportWorker(stub, RequestTraceExportWorkerOptions{
		Interval: time.Hour, RunTimeout: time.Hour, StopTimeout: 100 * time.Millisecond,
	})
	baseline := runtime.NumGoroutine()
	worker.Start()
	select {
	case <-stub.started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never issued a claim")
	}
	start := time.Now()
	worker.Stop()
	require.Less(t, time.Since(start), 2*time.Second, "Stop must not wait for a wedged runner")
	release()
	waitForGoroutineBaseline(t, baseline)
}

func TestRequestTraceExportWorkerRejectsCancelledContext(t *testing.T) {
	stub := &traceExportWorkerRunnerStub{}
	worker := NewRequestTraceExportWorker(stub, RequestTraceExportWorkerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := worker.Tick(ctx)
	require.True(t, errors.Is(err, context.Canceled))
	runs, cleanups, _ := stub.counts()
	require.Zero(t, runs)
	require.Zero(t, cleanups)
}
