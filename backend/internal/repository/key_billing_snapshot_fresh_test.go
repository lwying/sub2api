//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func expectSnapshotDBClock(mock sqlmock.Sqlmock, now time.Time) {
	mock.ExpectQuery("SELECT NOW\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(now))
}

func TestKeyBillingSnapshotAt24HoursAttemptsRefresh(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	binding := service.KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
	observedAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	current := observedAt.Add(24 * time.Hour)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, user_id, COALESCE\\(group_id, 0\\), billing_binding_revision, status, deleted_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "group_id", "revision", "status", "deleted_at"}).
			AddRow(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, service.StatusAPIKeyActive, nil))
	mock.ExpectQuery("SELECT COALESCE\\(\\(value::jsonb->>'enabled'\\)::boolean, false").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation", "max_stale_hours"}).AddRow(true, "epoch-a", 72))
	expectSnapshotDBClock(mock, current)
	mock.ExpectQuery("SELECT owner_user_id, group_id, binding_revision, generation, payload, observed_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"owner_user_id", "group_id", "binding_revision", "generation", "payload", "observed_at",
			"refresh_owner", "refresh_lease_until", "retry_after", "failure_code",
		}).AddRow(binding.UserID, binding.GroupID, binding.Revision, "epoch-a", `{"rate":1.5}`, observedAt, nil, nil, nil, nil))
	mock.ExpectExec("INSERT INTO key_billing_snapshots").WithArgs(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, "epoch-a", "new-owner", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	_, claim, claimed, _, err := repo.Acquire(context.Background(), binding, "epoch-a", "new-owner", current, 30*time.Second)

	require.NoError(t, err)
	require.True(t, claimed, "at exactly 24h the old declaration must not count as fresh")
	require.Equal(t, "new-owner", claim.OwnerToken)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotStaleDeadlineIsInclusiveDuringBackoff(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	binding := service.KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
	observedAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	current := observedAt.Add(72 * time.Hour)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, user_id, COALESCE\\(group_id, 0\\), billing_binding_revision, status, deleted_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "group_id", "revision", "status", "deleted_at"}).
			AddRow(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, service.StatusAPIKeyActive, nil))
	mock.ExpectQuery("SELECT COALESCE\\(\\(value::jsonb->>'enabled'\\)::boolean, false").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation", "max_stale_hours"}).AddRow(true, "epoch-a", 72))
	expectSnapshotDBClock(mock, current)
	mock.ExpectQuery("SELECT owner_user_id, group_id, binding_revision, generation, payload, observed_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"owner_user_id", "group_id", "binding_revision", "generation", "payload", "observed_at",
			"refresh_owner", "refresh_lease_until", "retry_after", "failure_code",
		}).AddRow(binding.UserID, binding.GroupID, binding.Revision, "epoch-a", `{"rate":1.5}`, observedAt, nil, nil, current.Add(time.Minute), "generation_failed"))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT COALESCE\\(\\(value::jsonb->>'enabled'\\)::boolean, false").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation", "max_stale_hours"}).AddRow(true, "epoch-a", 72))

	payload, err := repo.GetOrCreate(context.Background(), binding, "epoch-a", 72*time.Hour, func() time.Time { return current },
		func(context.Context) ([]byte, error) {
			t.Fatal("backoff must not generate")
			return nil, nil
		})

	require.NoError(t, err)
	require.Equal(t, []byte(`{"rate":1.5}`), payload)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotRejectsBackoffStalePastDeadline(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	binding := service.KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
	observedAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	current := observedAt.Add(72*time.Hour + time.Nanosecond)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, user_id, COALESCE\\(group_id, 0\\), billing_binding_revision, status, deleted_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "group_id", "revision", "status", "deleted_at"}).
			AddRow(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, service.StatusAPIKeyActive, nil))
	mock.ExpectQuery("SELECT COALESCE\\(\\(value::jsonb->>'enabled'\\)::boolean, false").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation", "max_stale_hours"}).AddRow(true, "epoch-a", 72))
	expectSnapshotDBClock(mock, current)
	mock.ExpectQuery("SELECT owner_user_id, group_id, binding_revision, generation, payload, observed_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"owner_user_id", "group_id", "binding_revision", "generation", "payload", "observed_at",
			"refresh_owner", "refresh_lease_until", "retry_after", "failure_code",
		}).AddRow(binding.UserID, binding.GroupID, binding.Revision, "epoch-a", `{"rate":1.5}`, observedAt, nil, nil, current.Add(time.Minute), "generation_failed"))
	mock.ExpectCommit()

	payload, err := repo.GetOrCreate(context.Background(), binding, "epoch-a", 72*time.Hour, func() time.Time { return current },
		func(context.Context) ([]byte, error) {
			t.Fatal("backoff must not generate")
			return nil, nil
		})

	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable)
	require.Nil(t, payload)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotFreshReadUsesDatabaseTime(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	binding := service.KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
	observedAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	now := observedAt.Add(23*time.Hour + 59*time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, user_id, COALESCE\\(group_id, 0\\), billing_binding_revision, status, deleted_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "group_id", "revision", "status", "deleted_at"}).
			AddRow(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, service.StatusAPIKeyActive, nil))
	mock.ExpectQuery("SELECT COALESCE\\(\\(value::jsonb->>'enabled'\\)::boolean, false").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation", "max_stale_hours"}).AddRow(true, "epoch-a", 72))
	expectSnapshotDBClock(mock, now)
	mock.ExpectQuery("SELECT owner_user_id, group_id, binding_revision, generation, payload, observed_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"owner_user_id", "group_id", "binding_revision", "generation", "payload", "observed_at",
			"refresh_owner", "refresh_lease_until", "retry_after", "failure_code",
		}).AddRow(binding.UserID, binding.GroupID, binding.Revision, "epoch-a", `{"rate":1.5}`, observedAt, nil, nil, nil, nil))
	mock.ExpectCommit()

	record, _, claimed, _, err := repo.Acquire(context.Background(), binding, "epoch-a", "unused-owner", now.Add(-48*time.Hour), 30*time.Second)

	require.NoError(t, err)
	require.False(t, claimed)
	require.Equal(t, observedAt, record.ObservedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotNewGenerationDoesNotWaitForOldLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	binding := service.KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
	leaseEnd := time.Now().UTC().Add(20 * time.Second)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, user_id, COALESCE\\(group_id, 0\\), billing_binding_revision, status, deleted_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "group_id", "revision", "status", "deleted_at"}).
			AddRow(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, service.StatusAPIKeyActive, nil))
	mock.ExpectQuery("SELECT COALESCE\\(\\(value::jsonb->>'enabled'\\)::boolean, false").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation", "max_stale_hours"}).AddRow(true, "epoch-b", 72))
	expectSnapshotDBClock(mock, time.Now().UTC())
	mock.ExpectQuery("SELECT owner_user_id, group_id, binding_revision, generation, payload, observed_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"owner_user_id", "group_id", "binding_revision", "generation", "payload", "observed_at",
			"refresh_owner", "refresh_lease_until", "retry_after", "failure_code",
		}).AddRow(binding.UserID, binding.GroupID, binding.Revision, "epoch-a", nil, nil, "old-owner", leaseEnd, nil, nil))
	mock.ExpectExec("INSERT INTO key_billing_snapshots").
		WithArgs(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, "epoch-b", "new-owner", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	_, claim, claimed, _, err := repo.Acquire(context.Background(), binding, "epoch-b", "new-owner", time.Now(), 30*time.Second)

	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, "new-owner", claim.OwnerToken)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotConfigShrinkRejectsOldDeclarationDuringBackoff(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	binding := service.KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
	now := time.Now().UTC()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, user_id, COALESCE\\(group_id, 0\\), billing_binding_revision, status, deleted_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "group_id", "revision", "status", "deleted_at"}).
			AddRow(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, service.StatusAPIKeyActive, nil))
	mock.ExpectQuery("SELECT COALESCE\\(\\(value::jsonb->>'enabled'\\)::boolean, false").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation", "max_stale_hours"}).AddRow(true, "epoch-a", 24))
	expectSnapshotDBClock(mock, now)
	mock.ExpectQuery("SELECT owner_user_id, group_id, binding_revision, generation, payload, observed_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"owner_user_id", "group_id", "binding_revision", "generation", "payload", "observed_at",
			"refresh_owner", "refresh_lease_until", "retry_after", "failure_code",
		}).AddRow(binding.UserID, binding.GroupID, binding.Revision, "epoch-a", `{"rate":1.5}`, now.Add(-25*time.Hour), nil, nil, now.Add(time.Minute), "generation_failed"))
	mock.ExpectCommit()

	payload, err := repo.GetOrCreate(context.Background(), binding, "epoch-a", 72*time.Hour, func() time.Time { return now },
		func(context.Context) ([]byte, error) {
			t.Fatal("backoff must not refresh")
			return nil, nil
		})

	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable)
	require.Nil(t, payload)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotFreshReadUsesSharedClockWhenCallerIsBehind(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	binding := service.KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
	sharedNow := time.Now().UTC()
	observedAt := sharedNow.Add(-time.Hour)
	laggingAppClock := observedAt.Add(-time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, user_id, COALESCE\\(group_id, 0\\), billing_binding_revision, status, deleted_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "group_id", "revision", "status", "deleted_at"}).
			AddRow(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, service.StatusAPIKeyActive, nil))
	mock.ExpectQuery("SELECT COALESCE\\(\\(value::jsonb->>'enabled'\\)::boolean, false").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation", "max_stale_hours"}).AddRow(true, "epoch-a", 72))
	expectSnapshotDBClock(mock, sharedNow)
	mock.ExpectQuery("SELECT owner_user_id, group_id, binding_revision, generation, payload, observed_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"owner_user_id", "group_id", "binding_revision", "generation", "payload", "observed_at",
			"refresh_owner", "refresh_lease_until", "retry_after", "failure_code",
		}).AddRow(binding.UserID, binding.GroupID, binding.Revision, "epoch-a", `{"rate":1.5}`, observedAt, nil, nil, nil, nil))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT COALESCE\\(\\(value::jsonb->>'enabled'\\)::boolean, false").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation", "max_stale_hours"}).AddRow(true, "epoch-a", 72))

	payload, err := repo.GetOrCreate(context.Background(), binding, "epoch-a", 72*time.Hour, func() time.Time { return laggingAppClock },
		func(context.Context) ([]byte, error) {
			t.Fatal("a lagging instance must reuse the fresh shared declaration")
			return nil, nil
		})

	require.NoError(t, err)
	require.Equal(t, []byte(`{"rate":1.5}`), payload)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotFreshReadDoesNotAcquireRefreshLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	binding := service.KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
	observedAt := time.Now().UTC().Add(-time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, user_id, COALESCE\\(group_id, 0\\), billing_binding_revision, status, deleted_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "group_id", "revision", "status", "deleted_at"}).
			AddRow(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, service.StatusAPIKeyActive, nil))
	mock.ExpectQuery("SELECT COALESCE\\(\\(value::jsonb->>'enabled'\\)::boolean, false").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation", "max_stale_hours"}).AddRow(true, "epoch-a", 72))
	expectSnapshotDBClock(mock, time.Now().UTC())
	mock.ExpectQuery("SELECT owner_user_id, group_id, binding_revision, generation, payload, observed_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"owner_user_id", "group_id", "binding_revision", "generation", "payload", "observed_at",
			"refresh_owner", "refresh_lease_until", "retry_after", "failure_code",
		}).AddRow(binding.UserID, binding.GroupID, binding.Revision, "epoch-a", `{"rate":1.5}`, observedAt, nil, nil, nil, nil))
	mock.ExpectCommit()

	record, claim, claimed, retryAt, err := repo.Acquire(context.Background(), binding, "epoch-a", "unused-owner", time.Now(), 30*time.Second)

	require.NoError(t, err)
	require.Equal(t, []byte(`{"rate":1.5}`), record.Payload)
	require.Equal(t, observedAt, record.ObservedAt)
	require.False(t, claimed)
	require.Empty(t, claim.OwnerToken)
	require.True(t, retryAt.IsZero())
	require.NoError(t, mock.ExpectationsWereMet())
}

// A waiter between two shared-row re-reads must not spin: every re-read opens a
// transaction that locks the api_keys row, so the interval is a moderate bounded
// constant that never runs past the lease end.
func TestKeyBillingSnapshotWaitForLeaseIsBoundedAndModerate(t *testing.T) {
	// Floors are literal on purpose: this asserts observable behaviour between two
	// shared-row re-reads, not the value of the production interval constant.
	const moderateFloor = 100 * time.Millisecond

	start := time.Now()
	require.NoError(t, waitForKeyBillingSnapshot(context.Background(), time.Now().Add(30*time.Second)))
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, moderateFloor,
		"a waiter must not re-read the shared row faster than a moderate interval, got %v", elapsed)
	require.Less(t, elapsed, 5*time.Second, "one wait must stay bounded, got %v", elapsed)

	// A lease that ends before the interval is waited out exactly, so the expired
	// lease is reclaimed on the next attempt instead of being slept past.
	start = time.Now()
	require.NoError(t, waitForKeyBillingSnapshot(context.Background(), time.Now().Add(40*time.Millisecond)))
	elapsed = time.Since(start)
	require.GreaterOrEqual(t, elapsed, 20*time.Millisecond)
	require.Less(t, elapsed, moderateFloor)

	// An already-expired lease must not cost a wait at all.
	start = time.Now()
	require.NoError(t, waitForKeyBillingSnapshot(context.Background(), time.Now().Add(-time.Second)))
	require.Less(t, time.Since(start), moderateFloor/2)

	// Cancellation still interrupts the wait.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	require.ErrorIs(t, waitForKeyBillingSnapshot(ctx, time.Now().Add(30*time.Second)), context.Canceled)
	require.Less(t, time.Since(start), moderateFloor/2)
}
