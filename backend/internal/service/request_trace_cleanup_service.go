package service

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const (
	RequestTraceCleanupBatch    = 500
	RequestTraceCleanupInterval = 10 * time.Minute
)

type RequestTraceCleanupRepository interface {
	DeleteExpiredUnlinkedRequestTraces(ctx context.Context, before time.Time, limit int) (int64, error)
}

// RequestTraceCleanupStats holds counters only: no error text and no row value.
type RequestTraceCleanupStats struct {
	Runs        int64 // completed RunOnce calls
	Deleted     int64 // unlinked traces physically removed
	Failures    int64 // RunOnce calls that returned an error
	LastDeleted int64 // rows removed by the most recent call
}

// RequestTraceCleanupService deletes only unlinked traces after their planned
// 30-day cleanup point. Until physical deletion, the row remains readable.
type RequestTraceCleanupService struct {
	repo      RequestTraceCleanupRepository
	claims    RequestTraceUsageLinker
	now       func() time.Time
	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	wg        sync.WaitGroup

	runs        atomic.Int64
	deleted     atomic.Int64
	failures    atomic.Int64
	lastDeleted atomic.Int64
}

func NewRequestTraceCleanupService(repo RequestTraceCleanupRepository, linkers ...RequestTraceUsageLinker) *RequestTraceCleanupService {
	svc := &RequestTraceCleanupService{repo: repo, now: time.Now, stopCh: make(chan struct{})}
	if len(linkers) > 0 {
		svc.claims = linkers[0]
	}
	return svc
}

// RunOnce performs one bounded sweep and records what it did. The counters are
// the only way an operator can tell a sweep that deleted nothing from a sweep
// that never ran, so a failed sweep is counted as a failure, not as silence.
func (s *RequestTraceCleanupService) RunOnce(ctx context.Context) (int64, error) {
	if s == nil || s.repo == nil {
		if s != nil {
			s.runs.Add(1)
			s.failures.Add(1)
			s.lastDeleted.Store(0)
		}
		return 0, ErrRequestTraceRepositoryUnavailable
	}
	deleted, err := s.repo.DeleteExpiredUnlinkedRequestTraces(ctx, s.now().UTC(), RequestTraceCleanupBatch)
	if s.claims != nil {
		claims, ok := s.claims.(interface {
			DeleteExpiredRequestTraceUsageClaims(context.Context, time.Time, int) (int64, error)
		})
		if ok {
			_, claimErr := claims.DeleteExpiredRequestTraceUsageClaims(ctx, s.now().UTC(), RequestTraceCleanupBatch)
			if claimErr != nil && err == nil {
				err = claimErr
			}
		}
	}
	s.runs.Add(1)
	s.deleted.Add(deleted)
	s.lastDeleted.Store(deleted)
	if err != nil {
		s.failures.Add(1)
	}
	return deleted, err
}

// Stats reports the sweep counters. It is value-free: no error text, no row id.
func (s *RequestTraceCleanupService) Stats() RequestTraceCleanupStats {
	if s == nil {
		return RequestTraceCleanupStats{}
	}
	return RequestTraceCleanupStats{
		Runs: s.runs.Load(), Deleted: s.deleted.Load(),
		Failures: s.failures.Load(), LastDeleted: s.lastDeleted.Load(),
	}
}

// Start begins bounded periodic cleanup. It never deletes usage-owned traces.
func (s *RequestTraceCleanupService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.runLoop()
	})
}

func (s *RequestTraceCleanupService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

func (s *RequestTraceCleanupService) runLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(RequestTraceCleanupInterval)
	defer ticker.Stop()
	s.cleanupOnce()
	for {
		select {
		case <-ticker.C:
			s.cleanupOnce()
		case <-s.stopCh:
			return
		}
	}
}

func (s *RequestTraceCleanupService) cleanupOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The repository error may contain stored values; no error text is logged here.
	_, _ = s.RunOnce(ctx)
}
