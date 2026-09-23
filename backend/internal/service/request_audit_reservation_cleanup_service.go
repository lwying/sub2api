package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// RequestAuditReservationCleanupService removes expired, unlinked pre-send reservations.
type RequestAuditReservationCleanupService struct {
	repo     RequestAuditReservationRepository
	interval time.Duration
	batch    int

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func NewRequestAuditReservationCleanupService(repo RequestAuditReservationRepository) *RequestAuditReservationCleanupService {
	return &RequestAuditReservationCleanupService{
		repo: repo, interval: 10 * time.Minute, batch: 500, stopCh: make(chan struct{}),
	}
}

func (s *RequestAuditReservationCleanupService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.runLoop()
	})
}

func (s *RequestAuditReservationCleanupService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

func (s *RequestAuditReservationCleanupService) runLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.interval)
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

func (s *RequestAuditReservationCleanupService) cleanupOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deleted, err := s.repo.DeleteExpiredReservations(ctx, time.Now(), s.batch)
	if err != nil {
		logger.LegacyPrintf("service.request_audit_reservation_cleanup", "cleanup failed: %v", err)
		return
	}
	if deleted > 0 {
		logger.LegacyPrintf("service.request_audit_reservation_cleanup", "deleted expired reservations: %d", deleted)
	}
}
