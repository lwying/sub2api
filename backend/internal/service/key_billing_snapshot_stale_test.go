//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKeyBillingSnapshotInitialFailureDoesNotRetryDuringSharedBackoff(t *testing.T) {
	settings := &keyBillingSnapshotSettingsSource{settings: KeyBillingSnapshotSettings{Enabled: true, MaxStaleHours: 72, Generation: "epoch-a"}}
	store := newMemoryKeyBillingSnapshotStore()
	svc := NewKeyBillingSnapshotService(store, settings)
	svc.now = func() time.Time { return time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC) }
	binding := KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 1}
	calls := 0
	generate := func(context.Context) ([]byte, error) {
		calls++
		return nil, errors.New("initial generation failed")
	}

	_, _, firstErr := svc.GetOrCreate(context.Background(), binding, generate)
	_, _, secondErr := svc.GetOrCreate(context.Background(), binding, generate)

	require.ErrorIs(t, firstErr, ErrKeyBillingSnapshotUnavailable)
	require.ErrorIs(t, secondErr, ErrKeyBillingSnapshotUnavailable)
	require.Equal(t, 1, calls)
}

func TestKeyBillingSnapshotRefreshesAt24Hours(t *testing.T) {
	ctx := context.Background()
	settings := &keyBillingSnapshotSettingsSource{settings: KeyBillingSnapshotSettings{Enabled: true, MaxStaleHours: 72, Generation: "epoch-a"}}
	store := newMemoryKeyBillingSnapshotStore()
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	svc := NewKeyBillingSnapshotService(store, settings)
	svc.now = func() time.Time { return now }
	binding := KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 1}
	calls := 0
	generate := func(context.Context) ([]byte, error) {
		calls++
		return []byte(`declaration-` + time.Duration(calls).String()), nil
	}

	first, _, err := svc.GetOrCreate(ctx, binding, generate)
	require.NoError(t, err)
	now = now.Add(24 * time.Hour)
	second, _, err := svc.GetOrCreate(ctx, binding, generate)
	require.NoError(t, err)

	require.NotEqual(t, first, second)
	require.Equal(t, 2, calls)
}

func TestKeyBillingSnapshotReturnsStaleOnlyThroughInclusiveMaximum(t *testing.T) {
	ctx := context.Background()
	settings := &keyBillingSnapshotSettingsSource{settings: KeyBillingSnapshotSettings{Enabled: true, MaxStaleHours: 72, Generation: "epoch-a"}}
	store := newMemoryKeyBillingSnapshotStore()
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	svc := NewKeyBillingSnapshotService(store, settings)
	svc.now = func() time.Time { return now }
	binding := KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 1}
	old := []byte(`{"observed_at":"2026-09-23T08:00:00Z","effective_rate_multiplier":1.5}`)
	payload, _, err := svc.GetOrCreate(ctx, binding, func(context.Context) ([]byte, error) { return old, nil })
	require.NoError(t, err)

	now = now.Add(72 * time.Hour)
	stale, enabled, err := svc.GetOrCreate(ctx, binding, func(context.Context) ([]byte, error) { return nil, errors.New("upstream lookup failed") })
	require.NoError(t, err)
	require.True(t, enabled)
	require.Equal(t, payload, stale)

	now = now.Add(time.Nanosecond)
	tooOld, enabled, err := svc.GetOrCreate(ctx, binding, func(context.Context) ([]byte, error) { return nil, errors.New("upstream lookup failed") })
	require.ErrorIs(t, err, ErrKeyBillingSnapshotUnavailable)
	require.True(t, enabled)
	require.Nil(t, tooOld)
}
