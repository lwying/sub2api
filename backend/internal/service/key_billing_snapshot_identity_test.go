//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeyBillingSnapshotIdentityReaderRejectsUnavailableStore(t *testing.T) {
	identity, err := NewKeyBillingSnapshotService(nil, nil).ReadAuthoritativeIdentity(context.Background(), 51, "presented-credential")
	require.Error(t, err)
	require.Zero(t, identity.Binding.APIKeyID)
}

func TestKeyBillingSnapshotIdentityAllowsBillingInspectionForExhaustedOrExpiredKey(t *testing.T) {
	for _, keyStatus := range []string{StatusAPIKeyQuotaExhausted, StatusAPIKeyExpired} {
		t.Run(keyStatus, func(t *testing.T) {
			binding := KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
			store := &keyBillingSnapshotIdentityStore{identity: KeyBillingSnapshotIdentity{
				Binding: binding, KeyStatus: keyStatus, UserStatus: StatusActive, GroupStatus: StatusActive,
				GroupSubscriptionType: SubscriptionTypeStandard,
			}}
			svc := NewKeyBillingSnapshotService(store, nil)

			identity, err := svc.ReadAuthoritativeIdentity(context.Background(), binding.APIKeyID, "current-key")

			require.NoError(t, err)
			require.Equal(t, binding, identity.Binding)
		})
	}
}

func TestKeyBillingSnapshotIdentityAllowsSubscriptionGroupWithoutAllowedGroupEntry(t *testing.T) {
	binding := KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 4}
	store := &keyBillingSnapshotIdentityStore{identity: KeyBillingSnapshotIdentity{
		Binding: binding, KeyStatus: StatusAPIKeyActive, UserStatus: StatusActive, GroupStatus: StatusActive,
		GroupSubscriptionType:    SubscriptionTypeSubscription,
		UserRestrictPublicGroups: true, UserAllowedGroupIDs: []int64{99}, GroupIsExclusive: true,
	}}
	svc := NewKeyBillingSnapshotService(store, nil)

	got, err := svc.ReadAuthoritativeIdentity(context.Background(), binding.APIKeyID, "current-key")

	require.NoError(t, err)
	require.Equal(t, binding, got.Binding)
}

type keyBillingSnapshotIdentityStore struct {
	KeyBillingSnapshotStore
	identity KeyBillingSnapshotIdentity
}

func (s *keyBillingSnapshotIdentityStore) ReadAuthoritativeIdentity(context.Context, int64, string) (KeyBillingSnapshotIdentity, error) {
	return s.identity, nil
}
