package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/requestauditreservation"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *requestAuditRepository) ReserveAttempt(ctx context.Context, scope service.RequestAuditReservationScope, attempt service.RequestAuditAttempt) error {
	if r == nil || r.client == nil || scope.LogicalKey == "" || scope.RouteFamily == service.RequestAuditRouteUnknown {
		return fmt.Errorf("invalid request audit reservation scope")
	}
	now := time.Now()
	_, _ = r.DeleteExpiredReservations(ctx, now, 100)
	client := clientFromContext(ctx, r.client)
	attemptMaps := requestAuditAttemptsToMaps([]service.RequestAuditAttempt{attempt})
	appendExisting := func() (int, error) {
		update := client.RequestAuditReservation.Update().
			Where(requestauditreservation.LogicalKeyEQ(scope.LogicalKey)).
			AppendAttempts(attemptMaps).
			SetSendStartedAt(now)
		if !scope.ExpiresAt.IsZero() {
			update.SetExpiresAt(scope.ExpiresAt)
		}
		return update.Save(ctx)
	}
	affected, err := appendExisting()
	if err != nil || affected > 0 {
		return err
	}
	expiresAt := scope.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = now.Add(24 * time.Hour)
	}
	_, err = client.RequestAuditReservation.Create().
		SetLogicalKey(scope.LogicalKey).
		SetRouteFamily(string(scope.RouteFamily)).
		SetForced(scope.Forced).
		SetHeaders(service.SanitizeRequestAuditHeaders(scope.Headers)).
		SetAttempts(attemptMaps).
		SetSendStartedAt(now).
		SetExpiresAt(expiresAt).
		Save(ctx)
	if ent.IsConstraintError(err) {
		_, err = appendExisting()
	}
	return err
}

func (r *requestAuditRepository) FinalizeReservation(ctx context.Context, logicalKey string, usageLogID int64, rec *service.RequestAuditRecord) error {
	if r == nil || r.client == nil || logicalKey == "" || usageLogID <= 0 || rec == nil {
		return fmt.Errorf("invalid request audit reservation finalization")
	}
	return r.withTx(ctx, func(txCtx context.Context, txClient *ent.Client) error {
		row, err := txClient.RequestAuditReservation.Query().
			Where(requestauditreservation.LogicalKeyEQ(logicalKey)).
			Only(txCtx)
		if ent.IsNotFound(err) {
			final := *rec
			final.UsageLogID = usageLogID
			final.CaptureCompleteness = service.RequestAuditCaptureIncomplete
			if final.CaptureReason == "" {
				final.CaptureReason = "missing_reservation"
			}
			return NewRequestAuditRepository(txClient).CreateRequestAudit(txCtx, &final)
		}
		if err != nil {
			return err
		}
		final := *rec
		final.UsageLogID = usageLogID
		if len(final.Headers) == 0 {
			final.Headers = row.Headers
		}
		if len(final.Attempts) == 0 {
			final.Attempts = requestAuditMapsToAttempts(row.Attempts)
		}
		if final.CaptureCompleteness == "" || row.CaptureCompleteness == service.RequestAuditCaptureIncomplete {
			final.CaptureCompleteness = row.CaptureCompleteness
		}
		if final.CaptureReason == "" || row.CaptureCompleteness == service.RequestAuditCaptureIncomplete {
			final.CaptureReason = row.CaptureReason
		}
		if err := NewRequestAuditRepository(txClient).CreateRequestAudit(txCtx, &final); err != nil {
			return err
		}
		return txClient.RequestAuditReservation.DeleteOneID(row.ID).Exec(txCtx)
	})
}

func (r *requestAuditRepository) MarkReservationIncomplete(ctx context.Context, logicalKey string, usageLogID int64, reason string) error {
	if r == nil || r.client == nil || logicalKey == "" {
		return fmt.Errorf("invalid request audit reservation incomplete marker")
	}
	update := clientFromContext(ctx, r.client).RequestAuditReservation.Update().
		Where(requestauditreservation.LogicalKeyEQ(logicalKey)).
		SetCaptureCompleteness(service.RequestAuditCaptureIncomplete).
		SetCaptureReason(reason)
	if usageLogID > 0 {
		update.SetUsageLogID(usageLogID)
	}
	affected, err := update.Save(ctx)
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("request audit reservation not found")
	}
	return nil
}

func (r *requestAuditRepository) DeleteExpiredReservations(ctx context.Context, before time.Time, limit int) (int64, error) {
	if r == nil || r.client == nil {
		return 0, nil
	}
	if limit <= 0 {
		limit = 1000
	}
	client := clientFromContext(ctx, r.client)
	rows, err := client.RequestAuditReservation.Query().
		Where(
			requestauditreservation.UsageLogIDIsNil(),
			requestauditreservation.ExpiresAtLTE(before),
		).
		Limit(limit).
		All(ctx)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	deleted, err := client.RequestAuditReservation.Delete().Where(
		requestauditreservation.IDIn(ids...),
		requestauditreservation.UsageLogIDIsNil(),
		requestauditreservation.ExpiresAtLTE(before),
	).Exec(ctx)
	return int64(deleted), err
}

func (r *requestAuditRepository) withTx(ctx context.Context, fn func(context.Context, *ent.Client) error) error {
	if tx := ent.TxFromContext(ctx); tx != nil {
		return fn(ctx, tx.Client())
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := ent.NewTxContext(ctx, tx)
	if err := fn(txCtx, tx.Client()); err != nil {
		return err
	}
	return tx.Commit()
}
