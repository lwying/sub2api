package service

import (
	"context"
	"time"
)

const (
	// RequestTraceBacklogProbeLimit bounds the unlinked-backlog count. The query
	// stops at this many rows, so a reported count equal to the limit is a lower
	// bound and says so instead of pretending to be an exact total.
	RequestTraceBacklogProbeLimit = 1000
	// RequestTraceBacklogProbeTimeout bounds the whole status read, including
	// the count. The endpoint is admin-only and off the gateway path; the bound
	// exists so a slow plan cannot hold an operator request open.
	RequestTraceBacklogProbeTimeout = 3 * time.Second
)

// RequestTraceStorageState is a closed set describing whether captured traces
// can reach storage at all. It carries no error text, no host and no statement.
type RequestTraceStorageState string

const (
	// RequestTraceStorageNotWired means no repository is attached: capture
	// cannot be stored no matter how much traffic arrives.
	RequestTraceStorageNotWired RequestTraceStorageState = "not_wired"
	// RequestTraceStorageNoTraffic means nothing has been accepted since this
	// process started. It is deliberately not "ok": an idle process talking to
	// a dead database is indistinguishable from this state inside the queue,
	// so the storage probe below is what separates the two.
	RequestTraceStorageNoTraffic RequestTraceStorageState = "no_traffic"
	// RequestTraceStorageOk means work was accepted and none of it failed.
	RequestTraceStorageOk RequestTraceStorageState = "ok"
	// RequestTraceStorageFailing means at least one write or drop happened:
	// the queue is not keeping up or storage is refusing work.
	RequestTraceStorageFailing RequestTraceStorageState = "write_failed"
)

// RequestTraceStorageProbeState is a closed set describing one bounded read of
// the trace store. It is the only live database signal in this endpoint, which
// is what makes a database outage visible while no request is in flight.
type RequestTraceStorageProbeState string

const (
	// RequestTraceStorageProbeNotConfigured means no probe is wired (offline or
	// partially wired deployment): reachability is simply unknown.
	RequestTraceStorageProbeNotConfigured RequestTraceStorageProbeState = "not_configured"
	// RequestTraceStorageProbeReachable means the bounded count completed.
	RequestTraceStorageProbeReachable RequestTraceStorageProbeState = "reachable"
	// RequestTraceStorageProbeUnavailable means the bounded count failed or ran
	// out of time: the store is not answering.
	RequestTraceStorageProbeUnavailable RequestTraceStorageProbeState = "unavailable"
)

// RequestTraceBacklogState is a closed set describing the meaning of the
// backlog count, so a capped count is never read as a total.
type RequestTraceBacklogState string

const (
	// RequestTraceBacklogUnavailable means the count could not be taken.
	RequestTraceBacklogUnavailable RequestTraceBacklogState = "unavailable"
	// RequestTraceBacklogMeasured means the count is exact.
	RequestTraceBacklogMeasured RequestTraceBacklogState = "measured"
	// RequestTraceBacklogAtLeast means the count reached the probe cap: the real
	// backlog is this large or larger.
	RequestTraceBacklogAtLeast RequestTraceBacklogState = "at_least"
)

// RequestTraceBacklogProbe takes the bounded unlinked-cleanup backlog count.
// Implementations must not read request content: the count only needs the
// cleanup predicate, which is already indexed.
type RequestTraceBacklogProbe interface {
	CountUnlinkedRequestTraceBacklog(ctx context.Context, before time.Time, limit int) (int64, error)
}

// RequestTraceOpsCapture is the capture queue's value-free state.
type RequestTraceOpsCapture struct {
	Storage             RequestTraceStorageState `json:"storage"`
	RepositoryAvailable bool                     `json:"repository_available"`
	Stopped             bool                     `json:"stopped"`
	QueueDepth          int                      `json:"queue_depth"`
	QueueCapacity       int                      `json:"queue_capacity"`
	Accepted            uint64                   `json:"accepted"`
	Stored              uint64                   `json:"stored"`
	WriteFailed         uint64                   `json:"write_failed"`
	Dropped             uint64                   `json:"dropped"`
	Rejected            uint64                   `json:"rejected"`
}

// RequestTraceOpsExport is the export worker's cumulative counters.
type RequestTraceOpsExport struct {
	WorkerStarted  bool  `json:"worker_started"`
	Ticks          int64 `json:"ticks"`
	TasksRun       int64 `json:"tasks_run"`
	TasksCompleted int64 `json:"tasks_completed"`
	TasksFailed    int64 `json:"tasks_failed"`
	Failures       int64 `json:"failures"`
	DisabledTicks  int64 `json:"disabled_ticks"`
	Cleanups       int64 `json:"cleanups"`
	CleanedFiles   int64 `json:"cleaned_files"`
}

// RequestTraceOpsCleanup is the cleanup service's counters plus the bounded
// backlog count of unlinked traces that are past their planned cleanup point.
type RequestTraceOpsCleanup struct {
	Runs         int64                    `json:"runs"`
	Deleted      int64                    `json:"deleted"`
	Failures     int64                    `json:"failures"`
	LastDeleted  int64                    `json:"last_deleted"`
	Backlog      int64                    `json:"unlinked_backlog"`
	BacklogLimit int                      `json:"backlog_limit"`
	BacklogState RequestTraceBacklogState `json:"backlog_state"`
}

// RequestTraceOpsStatus is the whole value-free answer: counts, closed
// sets, and nothing that could carry a body, a header value, a credential, a
// raw query string or a filename.
type RequestTraceOpsStatus struct {
	StorageProbe RequestTraceStorageProbeState `json:"storage_probe"`
	Capture      RequestTraceOpsCapture        `json:"capture"`
	Export       RequestTraceOpsExport         `json:"export"`
	Cleanup      RequestTraceOpsCleanup        `json:"cleanup"`
}

// RequestTraceOpsStatusService answers one question for an operator:
// is capture reaching storage, and is the unlinked cleanup keeping up.
//
// It reads only counters that already exist plus one bounded count, it never
// inspects a trace, and every error is collapsed into an enum so no database
// message can leak through this surface.
type RequestTraceOpsStatusService struct {
	capture *RequestTraceCaptureQueue
	export  *RequestTraceExportWorker
	cleanup *RequestTraceCleanupService
	backlog RequestTraceBacklogProbe
	timeout time.Duration
	now     func() time.Time
}

func NewRequestTraceOpsStatusService(
	capture *RequestTraceCaptureQueue,
	export *RequestTraceExportWorker,
	cleanup *RequestTraceCleanupService,
	backlog RequestTraceBacklogProbe,
) *RequestTraceOpsStatusService {
	return &RequestTraceOpsStatusService{
		capture: capture, export: export, cleanup: cleanup, backlog: backlog,
		timeout: RequestTraceBacklogProbeTimeout, now: time.Now,
	}
}

// Status assembles the snapshot. It never returns an error: an unreadable store
// is a state an operator must be able to see, not a request that failed.
func (s *RequestTraceOpsStatusService) Status(ctx context.Context) RequestTraceOpsStatus {
	var out RequestTraceOpsStatus
	if s == nil {
		out.Capture.Storage = RequestTraceStorageNotWired
		out.StorageProbe = RequestTraceStorageProbeNotConfigured
		out.Cleanup.BacklogState = RequestTraceBacklogUnavailable
		return out
	}

	capture := s.capture.Stats()
	out.Capture = RequestTraceOpsCapture{
		Storage:             requestTraceStorageState(capture),
		RepositoryAvailable: capture.Available,
		Stopped:             capture.Stopped,
		QueueDepth:          capture.Depth,
		QueueCapacity:       capture.Capacity,
		Accepted:            capture.Queued,
		Stored:              capture.Stored,
		WriteFailed:         capture.WriteFailed,
		Dropped:             capture.Dropped,
		Rejected:            capture.Rejected,
	}

	if s.export != nil {
		worker := s.export.Stats()
		out.Export = RequestTraceOpsExport{
			WorkerStarted:  s.export.Started(),
			Ticks:          worker.Ticks,
			TasksRun:       worker.TasksRun,
			TasksCompleted: worker.TasksCompleted,
			TasksFailed:    worker.TasksFailed,
			Failures:       worker.Failures,
			DisabledTicks:  worker.DisabledTicks,
			Cleanups:       worker.Cleanups,
			CleanedFiles:   worker.CleanedFiles,
		}
	}

	if s.cleanup != nil {
		cleaned := s.cleanup.Stats()
		out.Cleanup.Runs = cleaned.Runs
		out.Cleanup.Deleted = cleaned.Deleted
		out.Cleanup.Failures = cleaned.Failures
		out.Cleanup.LastDeleted = cleaned.LastDeleted
	}
	out.Cleanup.BacklogLimit = RequestTraceBacklogProbeLimit
	s.probeBacklog(ctx, &out)
	return out
}

// probeBacklog takes the one bounded read. A missing probe leaves the state
// explicitly unknown rather than zero, because zero backlog is a claim.
func (s *RequestTraceOpsStatusService) probeBacklog(ctx context.Context, out *RequestTraceOpsStatus) {
	if s.backlog == nil {
		out.StorageProbe = RequestTraceStorageProbeNotConfigured
		out.Cleanup.BacklogState = RequestTraceBacklogUnavailable
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// A zero-value struct would otherwise time the probe out instantly and
	// report "unavailable" for a store that is answering fine.
	timeout := s.timeout
	if timeout <= 0 {
		timeout = RequestTraceBacklogProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	now := s.now
	if now == nil {
		now = time.Now
	}
	count, err := s.backlog.CountUnlinkedRequestTraceBacklog(probeCtx, now().UTC(), RequestTraceBacklogProbeLimit)
	if err != nil {
		out.StorageProbe = RequestTraceStorageProbeUnavailable
		out.Cleanup.BacklogState = RequestTraceBacklogUnavailable
		return
	}
	out.StorageProbe = RequestTraceStorageProbeReachable
	out.Cleanup.Backlog = count
	out.Cleanup.BacklogState = RequestTraceBacklogMeasured
	if count >= RequestTraceBacklogProbeLimit {
		out.Cleanup.BacklogState = RequestTraceBacklogAtLeast
	}
}

func requestTraceStorageState(stats RequestTraceCaptureStats) RequestTraceStorageState {
	switch {
	case !stats.Available:
		return RequestTraceStorageNotWired
	case stats.WriteFailed > 0 || stats.Dropped > 0:
		return RequestTraceStorageFailing
	case stats.Queued > 0:
		return RequestTraceStorageOk
	default:
		// Zero failures with zero accepted work is not evidence of health.
		return RequestTraceStorageNoTraffic
	}
}
