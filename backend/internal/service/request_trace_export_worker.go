package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Default cadence and bound values for RequestTraceExportWorker. Each can be
// overridden through RequestTraceExportWorkerOptions.
const (
	// RequestTraceExportWorkerInterval is the poll interval after a tick that
	// did work: something may still be queued.
	RequestTraceExportWorkerInterval = 10 * time.Second
	// RequestTraceExportWorkerIdleInterval is the poll interval when the store
	// had nothing pending. Export tasks are created by admin sessions, so a
	// short delay before pickup is acceptable.
	RequestTraceExportWorkerIdleInterval = 30 * time.Second
	// RequestTraceExportWorkerDisabledInterval is how often a started worker
	// re-checks a disabled capability. The expiry sweep keeps following
	// CleanupEvery, so this only bounds the re-check while no new task can
	// appear.
	RequestTraceExportWorkerDisabledInterval = 5 * time.Minute
	// RequestTraceExportWorkerErrorBackoff is the wait after a failed call so a
	// broken store cannot turn into a hot loop.
	RequestTraceExportWorkerErrorBackoff = time.Minute
	// RequestTraceExportWorkerRunTimeout bounds a single service call. It must
	// stay above the export service's own MaxRuntime (default 10m) so a normal
	// long export is finished by the service's budget, while a wedged call is
	// still cut short.
	RequestTraceExportWorkerRunTimeout = 15 * time.Minute
	// RequestTraceExportWorkerCleanupEvery matches the request-trace cleanup
	// cadence (10m): expired files may outlive their download window, so a late
	// sweep is enough. It is spelled out rather than shared with the trace
	// cleanup service so this worker does not depend on that file.
	RequestTraceExportWorkerCleanupEvery = 10 * time.Minute
	// RequestTraceExportWorkerCleanupLimit is the default page size for one
	// cleanup sweep.
	RequestTraceExportWorkerCleanupLimit = 200
	// RequestTraceExportWorkerDefaultTasksPerTick matches the store rule of at
	// most one in-flight export per instance.
	RequestTraceExportWorkerDefaultTasksPerTick = 1
	// RequestTraceExportWorkerStopTimeout bounds how long Stop waits for the
	// loop to unwind.
	RequestTraceExportWorkerStopTimeout = 5 * time.Second

	requestTraceExportWorkerCleanupLimitMax = 500
	requestTraceExportWorkerTasksPerTickMax = 16
)

var (
	// ErrRequestTraceExportWorkerTimeout reports that a single service call did
	// not return within the worker's bound. It carries no values.
	ErrRequestTraceExportWorkerTimeout = errors.New("request trace export worker call exceeded its bound")
)

// RequestTraceExportRunner is the slice of *RequestTraceExportService the worker
// is allowed to use. *RequestTraceExportService satisfies it as written; the
// worker never reaches into the store, the source or the temp directory itself.
type RequestTraceExportRunner interface {
	RunOnce(ctx context.Context) (RequestTraceExportTask, error)
	CleanupExpired(ctx context.Context, limit int) (int64, error)
}

type RequestTraceExportWorkerOptions struct {
	Interval        time.Duration
	IdleInterval    time.Duration
	RunTimeout      time.Duration
	MaxTasksPerTick int
	CleanupEvery    time.Duration
	CleanupLimit    int
	StopTimeout     time.Duration
}

// RequestTraceExportWorkerTickResult is one tick's value-free outcome.
type RequestTraceExportWorkerTickResult struct {
	Ran        int   // tasks claimed and driven to a terminal state
	Completed  int   // tasks that finished with a downloadable file
	Failed     int   // tasks that ended in the failed state
	Idle       bool  // the store had nothing pending
	Busy       bool  // another caller held the export writer
	Disabled   bool  // new export creation is disabled; no task was claimed
	CleanupRan bool  // a cleanup sweep was attempted
	Cleaned    int64 // tasks/files removed by that sweep
}

// RequestTraceExportWorkerStats holds cumulative counters only. No task ID,
// path, byte count of a payload or error text is retained.
type RequestTraceExportWorkerStats struct {
	Ticks          int64
	TasksRun       int64
	TasksCompleted int64
	TasksFailed    int64
	Cleanups       int64
	CleanedFiles   int64
	Failures       int64
	DisabledTicks  int64
}

// RequestTraceExportWorker drives the node-local plaintext export in the
// background: Start launches one loop that claims pending tasks and sweeps
// expired tasks and the temp files they left behind; Stop ends it.
//
// When the export capability is disabled the loop claims nothing new, but it
// keeps sweeping: a rollback must not strand files that already completed and
// were promised a bounded download window.
//
// The worker holds no plaintext itself and performs no logging at all: request
// bodies, header values, file names and database error text never leave the
// export service. Callers observe it through Stats, which contains only
// counters.
//
// Bounds:
//   - one goroutine per worker; claim/run calls are serialised by the export
//     service itself, so the worker never adds export parallelism,
//   - every call into the service is bounded by RunTimeout (default above the
//     service's own MaxRuntime, so a wedged call rather than a long export is
//     what gets cut short),
//   - one tick claims at most MaxTasksPerTick tasks and sweeps at most
//     CleanupLimit expired entries,
//   - Stop cancels in-flight work and waits no longer than StopTimeout.
type RequestTraceExportWorker struct {
	runner RequestTraceExportRunner
	opts   RequestTraceExportWorkerOptions
	now    func() time.Time

	startOnce sync.Once
	started   atomic.Bool
	cancel    context.CancelFunc // guarded by mu: Stop may run before Start
	done      chan struct{}

	mu          sync.Mutex
	nextCleanup time.Time

	ticks          atomic.Int64
	tasksRun       atomic.Int64
	tasksCompleted atomic.Int64
	tasksFailed    atomic.Int64
	cleanups       atomic.Int64
	cleanedFiles   atomic.Int64
	failures       atomic.Int64
	disabledTicks  atomic.Int64
}

func NewRequestTraceExportWorker(runner RequestTraceExportRunner, opts RequestTraceExportWorkerOptions) *RequestTraceExportWorker {
	if opts.Interval <= 0 {
		opts.Interval = RequestTraceExportWorkerInterval
	}
	if opts.IdleInterval <= 0 {
		opts.IdleInterval = RequestTraceExportWorkerIdleInterval
	}
	if opts.RunTimeout <= 0 {
		opts.RunTimeout = RequestTraceExportWorkerRunTimeout
	}
	if opts.MaxTasksPerTick <= 0 {
		opts.MaxTasksPerTick = RequestTraceExportWorkerDefaultTasksPerTick
	}
	if opts.MaxTasksPerTick > requestTraceExportWorkerTasksPerTickMax {
		opts.MaxTasksPerTick = requestTraceExportWorkerTasksPerTickMax
	}
	if opts.CleanupEvery <= 0 {
		opts.CleanupEvery = RequestTraceExportWorkerCleanupEvery
	}
	if opts.CleanupLimit <= 0 {
		opts.CleanupLimit = RequestTraceExportWorkerCleanupLimit
	}
	if opts.CleanupLimit > requestTraceExportWorkerCleanupLimitMax {
		opts.CleanupLimit = requestTraceExportWorkerCleanupLimitMax
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = RequestTraceExportWorkerStopTimeout
	}
	return &RequestTraceExportWorker{runner: runner, opts: opts, now: time.Now, done: make(chan struct{})}
}

// Start launches the single background loop. It is a no-op without a runner and
// is safe to call repeatedly. The caller is expected to have verified that
// export is enabled; a disabled service is detected from RunOnce.
func (w *RequestTraceExportWorker) Start() {
	if w == nil || w.runner == nil {
		return
	}
	w.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		w.mu.Lock()
		w.cancel = cancel
		w.mu.Unlock()
		w.started.Store(true)
		go w.runLoop(ctx)
	})
}

// Stop cancels in-flight work and waits, bounded by StopTimeout, for the loop to
// exit. It is safe to call repeatedly and before Start: cancellation is
// idempotent and does not consume a one-shot guard, so a Stop issued before
// Start cannot leave a started loop uncancelled. If a runner ignores
// cancellation, Stop still returns at the bound; the loop then exits as soon as
// that call returns.
func (w *RequestTraceExportWorker) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !w.started.Load() {
		return
	}
	timer := time.NewTimer(w.opts.StopTimeout)
	defer timer.Stop()
	select {
	case <-w.done:
	case <-timer.C:
	}
}

// Tick performs one bounded unit of work: claim and run pending tasks, then run
// a cleanup sweep when its interval has elapsed. Expected outcomes (nothing
// pending, another writer busy, capability disabled) are reported in the
// result; the returned error is a sentinel from the export service, or
// ErrRequestTraceExportWorkerTimeout, and never carries values.
func (w *RequestTraceExportWorker) Tick(ctx context.Context) (RequestTraceExportWorkerTickResult, error) {
	result, err := w.tick(ctx)
	if w != nil {
		w.record(result, err)
	}
	return result, err
}

func (w *RequestTraceExportWorker) tick(ctx context.Context) (RequestTraceExportWorkerTickResult, error) {
	var result RequestTraceExportWorkerTickResult
	if w == nil || w.runner == nil {
		result.Disabled = true
		return result, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	var failure error
	for i := 0; i < w.opts.MaxTasksPerTick; i++ {
		if err := ctx.Err(); err != nil {
			failure = err
			break
		}
		runCtx, cancel := context.WithTimeout(ctx, w.opts.RunTimeout)
		task, err := w.runner.RunOnce(runCtx)
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		stopping := ctx.Err() != nil
		cancel()
		switch {
		case err == nil:
			result.Ran++
			if task.Status == RequestTraceExportCompleted {
				result.Completed++
			} else {
				result.Failed++
			}
			continue
		case errors.Is(err, ErrRequestTraceExportNotFound):
			// Nothing pending: stop claiming, this is the steady state.
			result.Idle = true
		case errors.Is(err, ErrRequestTraceExportDisabled):
			// Disabled: claim nothing, but the sweep below still runs so files
			// from tasks completed before the capability was turned off expire
			// on schedule.
			result.Disabled = true
		case timedOut:
			result.countTerminal(task)
			failure = ErrRequestTraceExportWorkerTimeout
		case stopping:
			result.countTerminal(task)
			failure = ctx.Err()
		case errors.Is(err, ErrRequestTraceExportLimit) && task.Status != RequestTraceExportFailed:
			// No task was taken: another caller holds the writer, or the store
			// refused the claim. Leave the capacity to that caller until the
			// next tick.
			result.Busy = true
			failure = err
		default:
			result.countTerminal(task)
			failure = err
		}
		break
	}
	// A disabled capability does not stop the sweep: rolling export back must
	// not leave completed files on disk forever.
	if !w.cleanupDue() {
		return result, failure
	}
	result.CleanupRan = true
	cleanupCtx, cancel := context.WithTimeout(ctx, w.opts.RunTimeout)
	cleaned, err := w.runner.CleanupExpired(cleanupCtx, w.opts.CleanupLimit)
	timedOut := errors.Is(cleanupCtx.Err(), context.DeadlineExceeded)
	cancel()
	result.Cleaned = cleaned
	switch {
	case err == nil:
		w.scheduleNextCleanup()
	case failure != nil:
	default:
		if timedOut {
			failure = ErrRequestTraceExportWorkerTimeout
		} else {
			failure = err
		}
	}
	return result, failure
}

// countTerminal counts a task the service already drove to its failed state.
func (r *RequestTraceExportWorkerTickResult) countTerminal(task RequestTraceExportTask) {
	if task.Status == RequestTraceExportFailed {
		r.Ran++
		r.Failed++
	}
}

// nextInterval reports how long the loop should wait after this outcome.
func (w *RequestTraceExportWorker) nextInterval(result RequestTraceExportWorkerTickResult) time.Duration {
	if w == nil {
		return RequestTraceExportWorkerIdleInterval
	}
	switch {
	case result.Disabled:
		return RequestTraceExportWorkerDisabledInterval
	case result.Ran > 0 || result.CleanupRan:
		return w.opts.Interval
	case result.Idle:
		return w.opts.IdleInterval
	default:
		return w.opts.Interval
	}
}

func (w *RequestTraceExportWorker) record(result RequestTraceExportWorkerTickResult, err error) {
	w.ticks.Add(1)
	if result.Ran > 0 {
		w.tasksRun.Add(int64(result.Ran))
	}
	if result.Completed > 0 {
		w.tasksCompleted.Add(int64(result.Completed))
	}
	if result.Failed > 0 {
		w.tasksFailed.Add(int64(result.Failed))
	}
	if result.CleanupRan {
		w.cleanups.Add(1)
	}
	if result.Cleaned > 0 {
		w.cleanedFiles.Add(result.Cleaned)
	}
	if err != nil {
		w.failures.Add(1)
	}
	if result.Disabled {
		w.disabledTicks.Add(1)
	}
}

// Started reports whether the background loop was launched. It is a state, not
// a counter: a worker that was never started and a worker that is idle between
// ticks have identical counters, and an operator needs to tell them apart.
func (w *RequestTraceExportWorker) Started() bool {
	if w == nil {
		return false
	}
	return w.started.Load()
}

func (w *RequestTraceExportWorker) Stats() RequestTraceExportWorkerStats {
	if w == nil {
		return RequestTraceExportWorkerStats{}
	}
	return RequestTraceExportWorkerStats{
		Ticks:          w.ticks.Load(),
		TasksRun:       w.tasksRun.Load(),
		TasksCompleted: w.tasksCompleted.Load(),
		TasksFailed:    w.tasksFailed.Load(),
		Cleanups:       w.cleanups.Load(),
		CleanedFiles:   w.cleanedFiles.Load(),
		Failures:       w.failures.Load(),
		DisabledTicks:  w.disabledTicks.Load(),
	}
}

func (w *RequestTraceExportWorker) runLoop(ctx context.Context) {
	defer close(w.done)
	for {
		result, err := w.Tick(ctx)
		var next time.Duration
		if err == nil {
			next = w.nextInterval(result)
		} else {
			next = RequestTraceExportWorkerErrorBackoff
		}
		if !w.wait(ctx, next) {
			return
		}
	}
}

func (w *RequestTraceExportWorker) wait(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *RequestTraceExportWorker) cleanupDue() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nextCleanup.IsZero() || !w.now().Before(w.nextCleanup)
}

// scheduleNextCleanup is only called after a sweep returned without error, so a
// failed sweep is retried on the next tick instead of a full interval later.
func (w *RequestTraceExportWorker) scheduleNextCleanup() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextCleanup = w.now().Add(w.opts.CleanupEvery)
}
