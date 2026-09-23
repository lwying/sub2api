package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const requestAuditReservationScopeKey = "request_audit_reservation_scope"

const (
	requestAuditIncompleteWriteFailedAfterStart = "write_failed_after_response_started"
	requestAuditIncompleteFinalizationFailed    = "finalization_failed"
)

type requestAuditReservationScopeContextKey struct{}

type forcedRequestAuditIncomplete struct {
	createdAt time.Time
	reason    string
	attempts  []RequestAuditAttempt
}

const (
	maxForcedRequestAuditIncompleteEntries = 4096
	maxForcedRequestAuditAttemptsPerKey    = 64
)

var forcedRequestAuditIncompleteByKey = struct {
	sync.Mutex
	entries       map[string]*forcedRequestAuditIncomplete
	overflowUntil time.Time
}{entries: make(map[string]*forcedRequestAuditIncomplete)}

func markForcedRequestAuditIncomplete(logicalKey, reason string, attempt RequestAuditAttempt) {
	if logicalKey == "" {
		return
	}
	now := time.Now()
	forcedRequestAuditIncompleteByKey.Lock()
	defer forcedRequestAuditIncompleteByKey.Unlock()
	for key, failure := range forcedRequestAuditIncompleteByKey.entries {
		if now.Sub(failure.createdAt) >= requestAuditReservationTTL {
			delete(forcedRequestAuditIncompleteByKey.entries, key)
		}
	}
	failure := forcedRequestAuditIncompleteByKey.entries[logicalKey]
	if failure == nil && len(forcedRequestAuditIncompleteByKey.entries) >= maxForcedRequestAuditIncompleteEntries {
		// Never evict an individual failure: that could let its audit finalize as
		// complete. Conservatively mark unknown forced audits incomplete instead.
		forcedRequestAuditIncompleteByKey.overflowUntil = now.Add(requestAuditReservationTTL)
		return
	}
	if failure == nil {
		failure = &forcedRequestAuditIncomplete{createdAt: now}
		forcedRequestAuditIncompleteByKey.entries[logicalKey] = failure
	}
	if failure.reason == "" || reason == requestAuditIncompleteWriteFailedAfterStart {
		failure.reason = reason
	}
	if (attempt.AccountID > 0 || attempt.Protocol != "") && len(failure.attempts) < maxForcedRequestAuditAttemptsPerKey {
		failure.attempts = append(failure.attempts, SanitizeRequestAuditAttempt(attempt))
	}
}

func forcedRequestAuditIncompleteForKey(logicalKey string) (string, []RequestAuditAttempt) {
	if logicalKey == "" {
		return "", nil
	}
	now := time.Now()
	forcedRequestAuditIncompleteByKey.Lock()
	defer forcedRequestAuditIncompleteByKey.Unlock()
	failure := forcedRequestAuditIncompleteByKey.entries[logicalKey]
	if failure != nil && now.Sub(failure.createdAt) < requestAuditReservationTTL {
		return failure.reason, append([]RequestAuditAttempt(nil), failure.attempts...)
	}
	if now.Before(forcedRequestAuditIncompleteByKey.overflowUntil) {
		return "reservation_missing_after_response_started", nil
	}
	return "", nil
}

func clearForcedRequestAuditIncomplete(logicalKey string) {
	forcedRequestAuditIncompleteByKey.Lock()
	delete(forcedRequestAuditIncompleteByKey.entries, logicalKey)
	forcedRequestAuditIncompleteByKey.Unlock()
}

func IsRequestAuditRequiredError(err error) bool {
	return httpattempt.IsRequiredAuditError(err)
}

const requestAuditReservationTTL = 24 * time.Hour

func prepareForcedRequestAudit(
	ctx context.Context,
	c *gin.Context,
	family RequestAuditRouteFamily,
	settings *SettingService,
	repo RequestAuditReservationRepository,
) (context.Context, error) {
	ctx = WithRequestAuditHTTPAttemptCounter(ctx, c)
	if c == nil || family == RequestAuditRouteUnknown {
		return ctx, nil
	}
	if settings == nil {
		// The production graph always supplies SettingService. A nil service is
		// retained as the disabled/default behavior for lightweight callers and
		// tests that do not construct the full gateway graph.
		return ctx, nil
	}
	if existing, ok := requestAuditReservationScope(c); ok {
		return context.WithValue(ctx, requestAuditReservationScopeContextKey{}, existing), nil
	}
	forceSettings, err := settings.GetRequestAuditForceSettings(ctx)
	if err != nil {
		return ctx, &httpattempt.RequiredAuditError{Cause: err}
	}
	forced := forceSettings.Required(family)
	if !forced {
		return ctx, nil
	}
	logicalKey := requestAuditLogicalKey(c)
	scope := RequestAuditReservationScope{
		LogicalKey: logicalKey, RouteFamily: family, Forced: true,
		Headers: SanitizeRequestAuditHeadersSnapshot(c.Request.Header), ExpiresAt: time.Now().Add(requestAuditReservationTTL),
	}
	c.Set(requestAuditReservationScopeKey, scope)
	ctx = context.WithValue(ctx, requestAuditReservationScopeContextKey{}, scope)
	counter := requestAuditHTTPAttemptCounter(c)
	counter.SetBeforeAttempt(func(attemptCtx context.Context, metadata httpattempt.Metadata) error {
		if metadata.AccountID <= 0 || strings.TrimSpace(metadata.Protocol) == "" {
			return fmt.Errorf("request audit attempt metadata is incomplete")
		}
		if repo == nil {
			return &httpattempt.RequiredAuditError{}
		}
		attempt := SanitizeRequestAuditAttempt(RequestAuditAttempt{
			AccountID:               metadata.AccountID,
			ModelFingerprint:        RequestAuditFingerprintFromContext(attemptCtx).DigestModel(metadata.Model),
			Protocol:                metadata.Protocol,
			Stage:                   RequestAuditStageWire,
			WireRequestHeaders:      metadata.RequestHeaders,
			UpstreamResponseHeaders: metadata.ResponseHeaders,
			UpstreamStatus:          metadata.StatusCode,
			RequestPayloadBytes:     metadata.RequestBytes,
			ResponsePayloadBytes:    metadata.ResponseBytes,
			ResponseReadComplete:    metadata.ResponseReadComplete,
		})
		reserveErr := repo.ReserveAttempt(attemptCtx, scope, attempt)
		if reserveErr != nil && c.Writer.Written() {
			markForcedRequestAuditIncomplete(scope.LogicalKey, requestAuditIncompleteWriteFailedAfterStart, attempt)
			if markErr := repo.MarkReservationIncomplete(attemptCtx, scope.LogicalKey, 0, requestAuditIncompleteWriteFailedAfterStart); markErr != nil {
				markForcedRequestAuditIncomplete(scope.LogicalKey, requestAuditIncompleteWriteFailedAfterStart, attempt)
			}
			return nil
		}
		return reserveErr
	}, true)
	return ctx, nil
}

func (s *GatewayService) PrepareRequestAudit(ctx context.Context, c *gin.Context, family RequestAuditRouteFamily) (context.Context, error) {
	return s.prepareForcedRequestAudit(ctx, c, family)
}

func (s *GatewayService) prepareForcedRequestAudit(ctx context.Context, c *gin.Context, family RequestAuditRouteFamily) (context.Context, error) {
	var repo RequestAuditReservationRepository
	if s != nil {
		repo, _ = s.requestAuditRepo.(RequestAuditReservationRepository)
		return prepareForcedRequestAudit(ctx, c, family, s.settingService, repo)
	}
	return prepareForcedRequestAudit(ctx, c, family, nil, nil)
}

func (s *OpenAIGatewayService) PrepareRequestAudit(ctx context.Context, c *gin.Context, family RequestAuditRouteFamily) (context.Context, error) {
	return s.prepareForcedRequestAudit(ctx, c, family)
}

func (s *OpenAIGatewayService) prepareForcedRequestAudit(ctx context.Context, c *gin.Context, family RequestAuditRouteFamily) (context.Context, error) {
	var repo RequestAuditReservationRepository
	if s != nil {
		repo, _ = s.requestAuditRepo.(RequestAuditReservationRepository)
		return prepareForcedRequestAudit(ctx, c, family, s.settingService, repo)
	}
	return prepareForcedRequestAudit(ctx, c, family, nil, nil)
}

func requestAuditReservationScope(c *gin.Context) (RequestAuditReservationScope, bool) {
	if c == nil {
		return RequestAuditReservationScope{}, false
	}
	value, ok := c.Get(requestAuditReservationScopeKey)
	scope, typed := value.(RequestAuditReservationScope)
	return scope, ok && typed && scope.LogicalKey != ""
}

func RequestAuditReservationScopeFromGin(c *gin.Context) (RequestAuditReservationScope, bool) {
	return requestAuditReservationScope(c)
}

func RequestAuditLogicalKeyFromGin(c *gin.Context) string {
	scope, _ := requestAuditReservationScope(c)
	return scope.LogicalKey
}

func RequestAuditForcedFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	scope, ok := ctx.Value(requestAuditReservationScopeContextKey{}).(RequestAuditReservationScope)
	return ok && scope.Forced
}

func requestAuditLogicalKey(c *gin.Context) string {
	if c == nil {
		return uuid.NewString()
	}
	if scope, ok := requestAuditReservationScope(c); ok {
		return scope.LogicalKey
	}
	if c.Request != nil {
		if value, ok := c.Request.Context().Value(ctxkey.ClientRequestID).(string); ok {
			if id := strings.TrimSpace(value); id != "" {
				return id
			}
		}
	}
	return uuid.NewString()
}
