//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeyBillingSnapshotRateLookupDoesNotConvertRepositoryErrorToGroupFallback(t *testing.T) {
	groupDefault := 0.75
	svc := NewKeyBillingSnapshotService(nil, nil, &keyBillingSnapshotRateRepo{err: errors.New("database unavailable")})

	resolved, userRate, err := svc.ResolveKeyBillingRate(context.Background(), 12, 31, groupDefault)

	require.ErrorIs(t, err, ErrKeyBillingSnapshotUnavailable)
	require.Zero(t, resolved)
	require.Nil(t, userRate)
}

type keyBillingSnapshotRateRepo struct {
	UserGroupRateRepository
	rate *float64
	err  error
}

func (r *keyBillingSnapshotRateRepo) GetByUserAndGroup(context.Context, int64, int64) (*float64, error) {
	return r.rate, r.err
}
