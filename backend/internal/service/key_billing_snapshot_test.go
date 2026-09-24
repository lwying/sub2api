//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKeyBillingSnapshotServiceKeepsFreshDeclarationPerKey(t *testing.T) {
	ctx := context.Background()
	settings := &keyBillingSnapshotSettingsSource{settings: KeyBillingSnapshotSettings{Enabled: true, MaxStaleHours: 72, Generation: "epoch-a"}}
	store := newMemoryKeyBillingSnapshotStore()
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	svc := NewKeyBillingSnapshotService(store, settings)
	svc.now = func() time.Time { return now }
	binding := KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31}
	generated := 0
	generate := func(context.Context) ([]byte, error) {
		generated++
		return []byte(`{"resolved_rate_multiplier":1.5,"observed_at":"2026-09-23T08:00:00Z"}`), nil
	}

	first, enabled, err := svc.GetOrCreate(ctx, binding, generate)
	require.NoError(t, err)
	require.True(t, enabled)
	now = now.Add(23*time.Hour + 59*time.Minute)
	second, enabled, err := svc.GetOrCreate(ctx, binding, generate)
	require.NoError(t, err)
	require.True(t, enabled)

	require.Equal(t, first, second)
	require.Equal(t, 1, generated)

	other := binding
	other.APIKeyID = 52
	third, enabled, err := svc.GetOrCreate(ctx, other, generate)
	require.NoError(t, err)
	require.True(t, enabled)
	require.Equal(t, first, third)
	require.Equal(t, 2, generated)
	require.Equal(t, 2, store.recordCount())
}

func TestKeyBillingSnapshotDisableAndReenableDiscardPriorGeneration(t *testing.T) {
	ctx := context.Background()
	settings := &keyBillingSnapshotSettingsSource{settings: KeyBillingSnapshotSettings{Enabled: true, MaxStaleHours: 72, Generation: "epoch-a"}}
	store := newMemoryKeyBillingSnapshotStore()
	svc := NewKeyBillingSnapshotService(store, settings)
	svc.now = func() time.Time { return time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC) }
	binding := KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31, Revision: 1}
	calls := 0
	payload, enabled, err := svc.GetOrCreate(ctx, binding, func(context.Context) ([]byte, error) {
		calls++
		return []byte("generation-" + settings.settings.Generation), nil
	})
	require.NoError(t, err)
	require.True(t, enabled)
	settings.settings.Enabled = false
	_, enabled, err = svc.GetOrCreate(ctx, binding, func(context.Context) ([]byte, error) {
		t.Fatal("disabled snapshot must not generate")
		return nil, nil
	})
	require.NoError(t, err)
	require.False(t, enabled)
	settings.settings.Enabled = true
	settings.settings.Generation = "epoch-b"
	fresh, enabled, err := svc.GetOrCreate(ctx, binding, func(context.Context) ([]byte, error) {
		calls++
		return []byte("generation-" + settings.settings.Generation), nil
	})
	require.NoError(t, err)
	require.True(t, enabled)
	require.NotEqual(t, payload, fresh)
	require.Equal(t, 2, calls)
}

func TestKeyBillingSnapshotServiceFailsClosedWithoutReliableFirstSnapshot(t *testing.T) {
	settings := &keyBillingSnapshotSettingsSource{settings: KeyBillingSnapshotSettings{Enabled: true, MaxStaleHours: 72, Generation: "epoch-a"}}
	svc := NewKeyBillingSnapshotService(newMemoryKeyBillingSnapshotStore(), settings)
	svc.now = func() time.Time { return time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC) }
	binding := KeyBillingSnapshotBinding{APIKeyID: 51, UserID: 12, GroupID: 31}

	payload, enabled, err := svc.GetOrCreate(context.Background(), binding, func(context.Context) ([]byte, error) {
		return nil, errors.New("sensitive database detail")
	})

	require.Error(t, err)
	require.True(t, enabled)
	require.Nil(t, payload)
	require.NotContains(t, err.Error(), "sensitive database detail")
}

type keyBillingSnapshotSettingsSource struct {
	settings KeyBillingSnapshotSettings
	err      error
}

func (s *keyBillingSnapshotSettingsSource) GetKeyBillingSnapshotSettings(context.Context) (KeyBillingSnapshotSettings, error) {
	return s.settings, s.err
}

type memoryKeyBillingSnapshotStore struct {
	mu      sync.Mutex
	records map[int64]KeyBillingSnapshotRecord
	claims  map[int64]KeyBillingSnapshotClaim
}

func newMemoryKeyBillingSnapshotStore() *memoryKeyBillingSnapshotStore {
	return &memoryKeyBillingSnapshotStore{
		records: make(map[int64]KeyBillingSnapshotRecord),
		claims:  make(map[int64]KeyBillingSnapshotClaim),
	}
}

func (s *memoryKeyBillingSnapshotStore) ReadAuthoritativeIdentity(context.Context, int64, string) (KeyBillingSnapshotIdentity, error) {
	return KeyBillingSnapshotIdentity{}, ErrKeyBillingSnapshotUnavailable
}
func (s *memoryKeyBillingSnapshotStore) ReadSharedTime(context.Context) (time.Time, error) {
	return time.Now().UTC(), nil
}
const (
	keyBillingSnapshotFreshFor       = 24 * time.Hour
	keyBillingSnapshotFailureBackoff = 5 * time.Minute
	keyBillingSnapshotRefreshLease   = 30 * time.Second
)

func (s *memoryKeyBillingSnapshotStore) GetOrCreate(ctx context.Context, binding KeyBillingSnapshotBinding, generation string, maxStale time.Duration, now func() time.Time, generate func(context.Context) ([]byte, error)) ([]byte, error) {
	if maxStale < 24*time.Hour || maxStale > 720*time.Hour {
		return nil, ErrKeyBillingSnapshotUnavailable
	}
	for {
		current := now()
		record, claim, claimed, _, err := s.Acquire(ctx, binding, generation, "memory-owner", current, keyBillingSnapshotRefreshLease)
		if err != nil {
			return nil, ErrKeyBillingSnapshotUnavailable
		}
		valid := record.Binding == binding && record.Generation == generation && len(record.Payload) > 0 && !record.ObservedAt.IsZero()
		if valid {
			age := current.Sub(record.ObservedAt)
			if age >= 0 && age < keyBillingSnapshotFreshFor {
				return append([]byte(nil), record.Payload...), nil
			}
			if !record.RetryAfter.IsZero() && current.Before(record.RetryAfter) {
				if age <= maxStale {
					return append([]byte(nil), record.Payload...), nil
				}
				return nil, ErrKeyBillingSnapshotUnavailable
			}
		}
		if !claimed {
			return nil, ErrKeyBillingSnapshotUnavailable
		}
		payload, generateErr := generate(ctx)
		if generateErr != nil || len(payload) == 0 {
			recorded, err := s.RecordFailure(ctx, claim, current.Add(keyBillingSnapshotFailureBackoff), "generation_failed")
			if err != nil || !recorded {
				return nil, ErrKeyBillingSnapshotUnavailable
			}
			if valid && now().Sub(record.ObservedAt) <= maxStale {
				return append([]byte(nil), record.Payload...), nil
			}
			return nil, ErrKeyBillingSnapshotUnavailable
		}
		published, err := s.Publish(ctx, claim, payload, now())
		if err != nil || !published {
			return nil, ErrKeyBillingSnapshotUnavailable
		}
		return append([]byte(nil), payload...), nil
	}
}
func (s *memoryKeyBillingSnapshotStore) Acquire(_ context.Context, binding KeyBillingSnapshotBinding, generation, ownerToken string, now time.Time, lease time.Duration) (KeyBillingSnapshotRecord, KeyBillingSnapshotClaim, bool, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[binding.APIKeyID]
	if current, ok := s.claims[binding.APIKeyID]; ok && now.Before(current.ExpiresAt) {
		return record, current, false, current.ExpiresAt, nil
	}
	if !record.RetryAfter.IsZero() && now.Before(record.RetryAfter) {
		return record, KeyBillingSnapshotClaim{}, false, record.RetryAfter, nil
	}
	claim := KeyBillingSnapshotClaim{Binding: binding, Generation: generation, OwnerToken: ownerToken, Revision: binding.Revision, ExpiresAt: now.Add(lease)}
	s.claims[binding.APIKeyID] = claim
	return record, claim, true, time.Time{}, nil
}
func (s *memoryKeyBillingSnapshotStore) Publish(_ context.Context, claim KeyBillingSnapshotClaim, payload []byte, generatedAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.claims[claim.Binding.APIKeyID]
	if !ok || current.OwnerToken != claim.OwnerToken {
		return false, nil
	}
	s.records[claim.Binding.APIKeyID] = KeyBillingSnapshotRecord{Binding: claim.Binding, Generation: claim.Generation, Payload: append([]byte(nil), payload...), ObservedAt: generatedAt}
	delete(s.claims, claim.Binding.APIKeyID)
	return true, nil
}
func (s *memoryKeyBillingSnapshotStore) RecordFailure(_ context.Context, claim KeyBillingSnapshotClaim, retryAt time.Time, safeCode string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.claims[claim.Binding.APIKeyID]
	if !ok || current.OwnerToken != claim.OwnerToken {
		return false, nil
	}
	record := s.records[claim.Binding.APIKeyID]
	record.Binding = claim.Binding
	record.Generation = claim.Generation
	record.RetryAfter = retryAt
	record.FailureCode = safeCode
	s.records[claim.Binding.APIKeyID] = record
	delete(s.claims, claim.Binding.APIKeyID)
	return true, nil
}
func (s *memoryKeyBillingSnapshotStore) recordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}
