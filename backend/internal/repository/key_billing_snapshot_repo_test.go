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

func TestKeyBillingSnapshotIdentityRequiresPresentedCredential(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	mock.ExpectQuery("SELECT k.id, k.user_id, COALESCE\\(k.group_id, 0\\), k.billing_binding_revision").
		WithArgs(int64(51)).
		WillReturnRows(snapshotIdentityRows("secret-current", []byte(`[]`), []byte(`[]`)))

	_, err = repo.ReadAuthoritativeIdentity(context.Background(), 51, "different-presented-secret")

	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotIdentityReturnsCurrentBindingAndIPRules(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	mock.ExpectQuery("SELECT k.id, k.user_id, COALESCE\\(k.group_id, 0\\), k.billing_binding_revision").
		WithArgs(int64(51)).
		WillReturnRows(snapshotIdentityRows("secret-current", []byte(`["203.0.113.0/24"]`), []byte(`["203.0.113.8"]`)))

	identity, err := repo.ReadAuthoritativeIdentity(context.Background(), 51, "secret-current")

	require.NoError(t, err)
	require.Equal(t, service.KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}, identity.Binding)
	require.Equal(t, []string{"203.0.113.0/24"}, identity.IPWhitelist)
	require.Equal(t, []string{"203.0.113.8"}, identity.IPBlacklist)
	require.Equal(t, service.SubscriptionTypeSubscription, identity.GroupSubscriptionType)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotIdentityReadsAllowedGroupsForRestrictedUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	// PostgreSQL arrays are not JSON; the projection must return a JSON array.
	rows := sqlmock.NewRows([]string{
		"id", "user_id", "group_id", "revision", "key", "key_status", "key_deleted", "whitelist", "blacklist",
		"user_status", "user_deleted", "user_restrict_public_groups", "user_allowed_group_ids", "group_status", "group_deleted", "group_exclusive", "group_rate", "platform", "subscription_type", "peak_enabled", "peak_start", "peak_end", "peak_multiplier",
	}).AddRow(int64(51), int64(12), int64(31), int64(4), "secret-current", service.StatusAPIKeyActive, nil,
		[]byte(`[]`), []byte(`[]`), service.StatusActive, nil, true, []byte(`[31,42]`), service.StatusActive, nil, true, 0.75,
		service.PlatformOpenAI, service.SubscriptionTypeStandard, false, "", "", 1.0)
	mock.ExpectQuery(`(?s)SELECT k.id, k.user_id, COALESCE\(k.group_id, 0\), k.billing_binding_revision.*to_jsonb\(.*array_agg\(ug.group_id`).
		WithArgs(int64(51)).WillReturnRows(rows)

	identity, err := repo.ReadAuthoritativeIdentity(context.Background(), 51, "secret-current")

	require.NoError(t, err)
	require.Equal(t, []int64{31, 42}, identity.UserAllowedGroupIDs)
	require.True(t, identity.UserRestrictPublicGroups)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotFailedRefreshRequiresUnexpiredLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
	binding := service.KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
	claim := service.KeyBillingSnapshotClaim{Binding: binding, Generation: "epoch-a", OwnerToken: "expired-owner", Revision: binding.Revision}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, user_id, COALESCE\\(group_id, 0\\), billing_binding_revision, status, deleted_at").
		WithArgs(binding.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "group_id", "revision", "status", "deleted_at"}).
			AddRow(binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, service.StatusAPIKeyActive, nil))
	mock.ExpectQuery("SELECT \\(value::jsonb->>'enabled'\\)::boolean, value::jsonb->>'generation'").
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "generation"}).AddRow(true, "epoch-a"))
	mock.ExpectExec(`(?s)UPDATE key_billing_snapshots AS snap.*snap.refresh_lease_until > NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	recorded, err := repo.RecordFailure(context.Background(), claim, time.Now().Add(5*time.Minute), "generation_failed")

	require.NoError(t, err)
	require.False(t, recorded)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKeyBillingSnapshotIdentityFailsClosedWhenOwnerOrGroupMissing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		userStatus  any
		groupStatus any
	}{
		{name: "missing owner", userStatus: nil, groupStatus: service.StatusActive},
		{name: "missing group", userStatus: service.StatusActive, groupStatus: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := NewKeyBillingSnapshotRepository(db).(*keyBillingSnapshotRepository)
			rows := sqlmock.NewRows([]string{
				"id", "user_id", "group_id", "revision", "key", "key_status", "key_deleted", "whitelist", "blacklist",
				"user_status", "user_deleted", "user_restrict_public_groups", "user_allowed_group_ids", "group_status", "group_deleted", "group_exclusive", "group_rate", "platform", "subscription_type", "peak_enabled", "peak_start", "peak_end", "peak_multiplier",
			}).AddRow(int64(51), int64(12), int64(31), int64(4), "secret-current", service.StatusAPIKeyActive, nil,
				[]byte(`[]`), []byte(`[]`), tc.userStatus, nil, false, []byte(`[]`), tc.groupStatus, nil, false, 0.75,
				service.PlatformOpenAI, service.SubscriptionTypeStandard, false, "", "", 1.0)
			mock.ExpectQuery("SELECT k.id, k.user_id, COALESCE\\(k.group_id, 0\\), k.billing_binding_revision").
				WithArgs(int64(51)).WillReturnRows(rows)

			identity, err := repo.ReadAuthoritativeIdentity(context.Background(), 51, "secret-current")

			require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable)
			require.Zero(t, identity.Binding.APIKeyID)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func snapshotIdentityRows(credential string, whitelist, blacklist []byte) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "user_id", "group_id", "revision", "key", "key_status", "key_deleted", "whitelist", "blacklist",
		"user_status", "user_deleted", "user_restrict_public_groups", "user_allowed_group_ids", "group_status", "group_deleted", "group_exclusive", "group_rate", "platform", "subscription_type", "peak_enabled", "peak_start", "peak_end", "peak_multiplier",
	}).AddRow(int64(51), int64(12), int64(31), int64(4), credential, service.StatusAPIKeyActive, nil,
		whitelist, blacklist, service.StatusActive, nil, false, []byte(`[]`), service.StatusActive, nil, false, 0.75,
		service.PlatformOpenAI, service.SubscriptionTypeSubscription, false, "", "", 1.0)
}
