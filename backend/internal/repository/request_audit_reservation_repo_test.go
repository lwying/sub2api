//go:build unit

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/ent/requestauditreservation"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

func newRequestAuditReservationTestClient(t *testing.T) *ent.Client {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared&_fk=1", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(ent.Driver(drv)))
	_, err = db.Exec("PRAGMA foreign_keys = OFF")
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestRequestAuditReservationRepositoryDeletesExpiredUnlinked(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	repo := NewRequestAuditRepository(client).(service.RequestAuditReservationRepository)
	require.NoError(t, repo.ReserveAttempt(ctx, service.RequestAuditReservationScope{
		LogicalKey: "expired", RouteFamily: service.RequestAuditRouteMessages, Forced: true,
		ExpiresAt: time.Now().Add(-time.Minute),
	}, service.RequestAuditAttempt{AccountID: 1, Protocol: service.RequestAuditProtocolAnthropic, Stage: service.RequestAuditStageWire}))

	deleted, err := repo.DeleteExpiredReservations(ctx, time.Now(), 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
}

func TestRequestAuditReservationCleanupPreservesReservationLinkedAfterSelection(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	repo := NewRequestAuditRepository(client).(service.RequestAuditReservationRepository)
	logicalKey := "expired-linked-before-delete"
	require.NoError(t, repo.ReserveAttempt(ctx, service.RequestAuditReservationScope{
		LogicalKey: logicalKey, RouteFamily: service.RequestAuditRouteMessages, Forced: true,
		ExpiresAt: time.Now().Add(-time.Minute),
	}, service.RequestAuditAttempt{AccountID: 1, Protocol: service.RequestAuditProtocolAnthropic, Stage: service.RequestAuditStageWire}))
	usage, err := client.UsageLog.Create().SetUserID(1).SetAPIKeyID(2).SetAccountID(1).
		SetRequestID("expired-linked-usage").SetModel("claude-sonnet").Save(ctx)
	require.NoError(t, err)

	client.RequestAuditReservation.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			if mutation.Op() == ent.OpDelete {
				if err := repo.MarkReservationIncomplete(ctx, logicalKey, usage.ID, "finalization_failed"); err != nil {
					return nil, err
				}
			}
			return next.Mutate(ctx, mutation)
		})
	})

	deleted, err := repo.DeleteExpiredReservations(ctx, time.Now(), 10)
	require.NoError(t, err)
	require.Zero(t, deleted)

	reservation, err := client.RequestAuditReservation.Query().
		Where(requestauditreservation.LogicalKeyEQ(logicalKey)).Only(ctx)
	require.NoError(t, err)
	require.Equal(t, usage.ID, *reservation.UsageLogID)
	require.Equal(t, service.RequestAuditCaptureIncomplete, reservation.CaptureCompleteness)
	require.Equal(t, "finalization_failed", reservation.CaptureReason)
}

func TestRequestAuditRepositoryFallsBackToLinkedIncompleteReservation(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	base := NewRequestAuditRepository(client)
	repo := base.(service.RequestAuditReservationRepository)
	scope := service.RequestAuditReservationScope{
		LogicalKey: "linked-incomplete", RouteFamily: service.RequestAuditRouteResponses, Forced: true,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, repo.ReserveAttempt(ctx, scope, service.RequestAuditAttempt{
		AccountID: 11, Model: "gpt-5.4", Protocol: service.RequestAuditProtocolOpenAIResp, Stage: service.RequestAuditStageWire,
	}))
	usage, err := client.UsageLog.Create().
		SetUserID(1).SetAPIKeyID(2).SetAccountID(11).
		SetRequestID("linked-incomplete-usage").SetModel("gpt-5.4").
		Save(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.MarkReservationIncomplete(ctx, scope.LogicalKey, usage.ID, "finalization_failed"))

	rec, err := base.GetByUsageLogID(ctx, usage.ID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, service.RequestAuditCaptureIncomplete, rec.CaptureCompleteness)
	require.Equal(t, "finalization_failed", rec.CaptureReason)
	require.Len(t, rec.Attempts, 1)
}

func TestRequestAuditReservationFinalizationPreservesPostStartIncomplete(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	base := NewRequestAuditRepository(client)
	repo := base.(service.RequestAuditReservationRepository)
	scope := service.RequestAuditReservationScope{
		LogicalKey: "post-start-incomplete", RouteFamily: service.RequestAuditRouteResponses, Forced: true,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, repo.ReserveAttempt(ctx, scope, service.RequestAuditAttempt{
		AccountID: 11, Protocol: service.RequestAuditProtocolOpenAIResp, Stage: service.RequestAuditStageWire,
	}))
	require.NoError(t, repo.MarkReservationIncomplete(ctx, scope.LogicalKey, 0, "write_failed_after_response_started"))
	usage, err := client.UsageLog.Create().SetUserID(1).SetAPIKeyID(2).SetAccountID(11).
		SetRequestID("post-start-incomplete-usage").SetModel("gpt-5.4").Save(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.FinalizeReservation(ctx, scope.LogicalKey, usage.ID, &service.RequestAuditRecord{
		CaptureCompleteness: service.RequestAuditCaptureComplete,
	}))
	rec, err := base.GetByUsageLogID(ctx, usage.ID)
	require.NoError(t, err)
	require.Equal(t, service.RequestAuditCaptureIncomplete, rec.CaptureCompleteness)
	require.Equal(t, "write_failed_after_response_started", rec.CaptureReason)
}

func TestRequestAuditReservationMissingFinalizesIncompleteAudit(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	base := NewRequestAuditRepository(client)
	repo := base.(service.RequestAuditReservationRepository)
	usage, err := client.UsageLog.Create().SetUserID(1).SetAPIKeyID(2).SetAccountID(11).
		SetRequestID("missing-forced-reservation-usage").SetModel("gpt-5.4").Save(ctx)
	require.NoError(t, err)

	err = repo.FinalizeReservation(ctx, "missing-forced-reservation", usage.ID, &service.RequestAuditRecord{
		Attempts: []service.RequestAuditAttempt{{AccountID: 11, Model: "untrusted-model", Protocol: service.RequestAuditProtocolOpenAIResp, Stage: service.RequestAuditStageWire}},
		CaptureCompleteness: service.RequestAuditCaptureComplete,
	})
	require.NoError(t, err)

	rec, err := base.GetByUsageLogID(ctx, usage.ID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, service.RequestAuditCaptureIncomplete, rec.CaptureCompleteness)
	require.Equal(t, "missing_reservation", rec.CaptureReason)
	require.Len(t, rec.Attempts, 1)
	require.Empty(t, rec.Attempts[0].Model)
}

func TestRequestAuditReservationIncompleteMarkerRequiresExistingRow(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	repo := NewRequestAuditRepository(client).(service.RequestAuditReservationRepository)
	require.Error(t, repo.MarkReservationIncomplete(ctx, "absent-reservation", 0, "write_failed_after_response_started"))
}

func TestRequestAuditReservationRepositoryReservesAndFinalizes(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	repo := NewRequestAuditRepository(client)
	forced := repo.(service.RequestAuditReservationRepository)
	scope := service.RequestAuditReservationScope{
		LogicalKey: "logical-1", RouteFamily: service.RequestAuditRouteResponses, Forced: true,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, forced.ReserveAttempt(ctx, scope, service.RequestAuditAttempt{
		AccountID: 11, Model: "gpt-5.4", Protocol: service.RequestAuditProtocolOpenAIResp, Stage: service.RequestAuditStageWire,
	}))
	require.NoError(t, forced.ReserveAttempt(ctx, scope, service.RequestAuditAttempt{
		AccountID: 22, Model: "gpt-5.4", Protocol: service.RequestAuditProtocolOpenAIResp, Stage: service.RequestAuditStageWire,
	}))
	usage, err := client.UsageLog.Create().
		SetUserID(1).SetAPIKeyID(2).SetAccountID(22).
		SetRequestID("forced-reservation-usage").SetModel("gpt-5.4").
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, forced.FinalizeReservation(ctx, "logical-1", usage.ID, &service.RequestAuditRecord{
		CaptureCompleteness: service.RequestAuditCaptureComplete,
	}))
	rec, err := repo.GetByUsageLogID(ctx, usage.ID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, service.RequestAuditCaptureComplete, rec.CaptureCompleteness)
	require.Len(t, rec.Attempts, 2)
	require.Equal(t, int64(11), rec.Attempts[0].AccountID)
	require.Equal(t, int64(22), rec.Attempts[1].AccountID)
	count, err := client.RequestAuditReservation.Query().Count(ctx)
	require.NoError(t, err)
	require.Zero(t, count)
}
