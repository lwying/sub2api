package service

import (
	"context"
	"sync"
	"time"
)

const (
	RequestTraceCleanupBatch    = 500
	RequestTraceCleanupInterval = 10 * time.Minute
)

type RequestTraceCleanupRepository interface {
	DeleteExpiredUnlinkedRequestTraces(ctx context.Context, before time.Time, limit int) (int64, error)
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
}

func NewRequestTraceCleanupService(repo RequestTraceCleanupRepository, linkers ...RequestTraceUsageLinker) *RequestTraceCleanupService {
	svc := &RequestTraceCleanupService{repo: repo, now: time.Now, stopCh: make(chan struct{})}
	if len(linkers) > 0 {
		svc.claims = linkers[0]
	}
	return svc
}

func (s *RequestTraceCleanupService) RunOnce(ctx context.Context) (int64, error) {
	if s == nil || s.repo == nil {
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
	return deleted, err
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
