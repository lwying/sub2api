//go:build unit

package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// traceStatusStorageRepo fails every envelope write, which is what a database
// outage looks like from the capture queue's side.
type traceStatusStorageRepo struct {
	traceQueueRepo
	createErr error
}

func (r *traceStatusStorageRepo) CreateRequestTrace(context.Context, RequestTrace) (RequestTrace, error) {
	return RequestTrace{}, r.createErr
}

type traceStatusBacklogProbeStub struct {
	count int64
	err   error
	calls int
	limit int
}

func (p *traceStatusBacklogProbeStub) CountUnlinkedRequestTraceBacklog(_ context.Context, _ time.Time, limit int) (int64, error) {
	p.calls++
	p.limit = limit
	return p.count, p.err
}

func newTraceStatusService(t *testing.T, queue *RequestTraceCaptureQueue, probe RequestTraceBacklogProbe) *RequestTraceOpsStatusService {
	t.Helper()
	return NewRequestTraceOpsStatusService(queue, nil, nil, probe)
}

// A process that has captured nothing yet must not be reported as healthy: an
// idle gateway with a dead database looks exactly like an idle, healthy one.
// The bounded backlog probe is the only liveness signal in that state.
func TestRequestTraceOpsStatusSeparatesOutageFromNoTraffic(t *testing.T) {
	probe := &traceStatusBacklogProbeStub{err: errors.New("storage unavailable")}
	svc := newTraceStatusService(t, NewRequestTraceCaptureQueue(nil), probe)

	status := svc.Status(context.Background())
	require.Equal(t, RequestTraceStorageNotWired, status.Capture.Storage)
	require.False(t, status.Capture.RepositoryAvailable)
	require.Equal(t, RequestTraceStorageProbeUnavailable, status.StorageProbe)
	require.Equal(t, uint64(0), status.Capture.Accepted)
	require.Equal(t, 1, probe.calls, "the probe must run even when the queue saw no traffic")
	require.Positive(t, probe.limit)
	require.LessOrEqual(t, probe.limit, RequestTraceBacklogProbeLimit)
}

func TestRequestTraceOpsStatusReportsReachableIdleStorage(t *testing.T) {
	probe := &traceStatusBacklogProbeStub{}
	queue := NewRequestTraceCaptureQueue(&traceQueueRepo{})
	t.Cleanup(queue.Stop)
	svc := newTraceStatusService(t, queue, probe)

	status := svc.Status(context.Background())
	require.Equal(t, RequestTraceStorageProbeReachable, status.StorageProbe)
	require.True(t, status.Capture.RepositoryAvailable)
	require.Equal(t, RequestTraceStorageNoTraffic, status.Capture.Storage)
	require.Equal(t, requestTraceCaptureQueueCapacity, status.Capture.QueueCapacity)
	require.Equal(t, 0, status.Capture.QueueDepth)
	require.False(t, status.Capture.Stopped)
}

func TestRequestTraceOpsStatusReportsStorageFailure(t *testing.T) {
	queue := NewRequestTraceCaptureQueue(&traceStatusStorageRepo{createErr: errors.New("connection refused")})
	t.Cleanup(queue.Stop)
	svc := newTraceStatusService(t, queue, &traceStatusBacklogProbeStub{})

	trace, stages := traceQueueExample()
	require.True(t, queue.Enqueue(trace, stages))
	require.Eventually(t, func() bool { return queue.Stats().WriteFailed > 0 }, 2*time.Second, 5*time.Millisecond)

	status := svc.Status(context.Background())
	require.Equal(t, RequestTraceStorageFailing, status.Capture.Storage)
	require.Equal(t, uint64(1), status.Capture.WriteFailed)
	require.Equal(t, uint64(1), status.Capture.Accepted)
	require.Equal(t, uint64(0), status.Capture.Stored)
}

func TestRequestTraceOpsStatusReportsStoredTrafficAsHealthy(t *testing.T) {
	repo := &traceQueueRepo{}
	queue := NewRequestTraceCaptureQueue(repo)
	t.Cleanup(queue.Stop)
	svc := newTraceStatusService(t, queue, &traceStatusBacklogProbeStub{})

	trace, stages := traceQueueExample()
	require.True(t, queue.Enqueue(trace, stages))
	require.Eventually(t, func() bool { return queue.Stats().Stored > 0 }, 2*time.Second, 5*time.Millisecond)

	status := svc.Status(context.Background())
	require.Equal(t, RequestTraceStorageOk, status.Capture.Storage)
	require.Equal(t, uint64(1), status.Capture.Stored)
	require.Equal(t, uint64(0), status.Capture.WriteFailed)
	require.Equal(t, uint64(0), status.Capture.Dropped)
	require.Equal(t, uint64(0), status.Capture.Rejected)
}

func TestRequestTraceOpsStatusReportsStopWithoutTraffic(t *testing.T) {
	queue := NewRequestTraceCaptureQueue(&traceQueueRepo{})
	queue.Stop()
	svc := newTraceStatusService(t, queue, &traceStatusBacklogProbeStub{})

	status := svc.Status(context.Background())
	require.True(t, status.Capture.Stopped)
	require.Equal(t, RequestTraceStorageNoTraffic, status.Capture.Storage)
}

func TestRequestTraceOpsStatusBoundsTheBacklogProbe(t *testing.T) {
	probe := &traceStatusBacklogProbeStub{count: 7}
	svc := newTraceStatusService(t, NewRequestTraceCaptureQueue(&traceQueueRepo{}), probe)

	status := svc.Status(context.Background())
	require.Equal(t, int64(7), status.Cleanup.Backlog)
	require.Equal(t, RequestTraceBacklogMeasured, status.Cleanup.BacklogState)
	require.Equal(t, RequestTraceBacklogProbeLimit, status.Cleanup.BacklogLimit)

	probe.count = RequestTraceBacklogProbeLimit
	status = svc.Status(context.Background())
	require.Equal(t, RequestTraceBacklogAtLeast, status.Cleanup.BacklogState,
		"a count that reaches the cap is a lower bound, never an exact total")
	require.Equal(t, int64(RequestTraceBacklogProbeLimit), status.Cleanup.Backlog)
}

func TestRequestTraceOpsStatusWithoutBacklogProbe(t *testing.T) {
	svc := newTraceStatusService(t, NewRequestTraceCaptureQueue(&traceQueueRepo{}), nil)

	status := svc.Status(context.Background())
	require.Equal(t, RequestTraceStorageProbeNotConfigured, status.StorageProbe)
	require.Equal(t, RequestTraceBacklogUnavailable, status.Cleanup.BacklogState)
}

func TestRequestTraceOpsStatusSurvivesEveryNilComponent(t *testing.T) {
	svc := NewRequestTraceOpsStatusService(nil, nil, nil, nil)

	status := svc.Status(context.Background())
	require.Equal(t, RequestTraceStorageNotWired, status.Capture.Storage)
	require.False(t, status.Capture.RepositoryAvailable)
	require.False(t, status.Export.WorkerStarted)
	require.Equal(t, int64(0), status.Export.Ticks)
	require.Equal(t, int64(0), status.Cleanup.Runs)
}

// The unlinked cleanup backlog is only useful if the counters that swept it are
// visible at the same time.
func TestRequestTraceOpsStatusReportsCleanupProgress(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	repo := &traceCleanupRepoStub{count: 12}
	cleanup := NewRequestTraceCleanupService(repo)
	cleanup.now = func() time.Time { return now }
	_, err := cleanup.RunOnce(context.Background())
	require.NoError(t, err)
	repo.count = 0
	repo.err = errors.New("storage unavailable")
	_, err = cleanup.RunOnce(context.Background())
	require.Error(t, err)

	svc := NewRequestTraceOpsStatusService(nil, nil, cleanup, &traceStatusBacklogProbeStub{count: 40})
	status := svc.Status(context.Background())
	require.Equal(t, int64(2), status.Cleanup.Runs)
	require.Equal(t, int64(12), status.Cleanup.Deleted)
	require.Equal(t, int64(1), status.Cleanup.Failures)
	require.Equal(t, int64(0), status.Cleanup.LastDeleted,
		"a failed sweep must not leave the previous sweep's total looking current")
	require.Equal(t, int64(40), status.Cleanup.Backlog)
	require.Equal(t, RequestTraceBacklogMeasured, status.Cleanup.BacklogState)
}

func TestRequestTraceOpsStatusReportsExportWorkerCounters(t *testing.T) {
	runner := &traceExportWorkerRunnerStub{}
	worker := NewRequestTraceExportWorker(runner, RequestTraceExportWorkerOptions{})
	worker.Start()
	t.Cleanup(worker.Stop)
	_, err := worker.Tick(context.Background())
	require.NoError(t, err)

	svc := NewRequestTraceOpsStatusService(nil, worker, nil, &traceStatusBacklogProbeStub{})
	status := svc.Status(context.Background())
	require.True(t, status.Export.WorkerStarted)
	// Start launches an immediate background tick. It may run before or after the
	// explicit Tick above, so both one and two ticks are valid observations.
	require.GreaterOrEqual(t, status.Export.Ticks, int64(1))
	require.GreaterOrEqual(t, status.Export.Failures, int64(0))
}

// A status read must never be a way to reach request content: the whole DTO is
// counts and closed-set enums, and it stays that way under reflection.
func TestRequestTraceOpsStatusCarriesNoFreeText(t *testing.T) {
	svc := newTraceStatusService(t, NewRequestTraceCaptureQueue(&traceQueueRepo{}), &traceStatusBacklogProbeStub{})

	status := svc.Status(context.Background())
	fields := requestTraceStatusStringFields(t, status)
	require.NotEmpty(t, fields, "the DTO must expose at least one state enum")
	for _, field := range fields {
		require.Contains(t, []string{
			string(status.Capture.Storage),
			string(status.StorageProbe),
			string(status.Cleanup.BacklogState),
		}, field, "every string in the status DTO must be a closed-set enum")
	}
}

// requestTraceStatusStringFields walks the DTO so a later field cannot quietly
// add a free-text string (a path, an error message, a host name) to the surface.
func requestTraceStatusStringFields(t *testing.T, status RequestTraceOpsStatus) []string {
	t.Helper()
	var found []string
	var walk func(value reflect.Value, depth int)
	walk = func(value reflect.Value, depth int) {
		require.LessOrEqual(t, depth, 4, "status DTO must stay shallow")
		switch value.Kind() {
		case reflect.String:
			if value.Len() > 0 {
				found = append(found, value.String())
			}
		case reflect.Struct:
			for i := 0; i < value.NumField(); i++ {
				walk(value.Field(i), depth+1)
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < value.Len(); i++ {
				walk(value.Index(i), depth+1)
			}
		default:
		}
	}
	walk(reflect.ValueOf(status), 0)
	return found
}
