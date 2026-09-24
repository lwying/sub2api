package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// GetOrCreate coordinates one authoritative declaration per API key across instances.
func (r *keyBillingSnapshotRepository) GetOrCreate(
	ctx context.Context,
	binding service.KeyBillingSnapshotBinding,
	generation string,
	maxStale time.Duration,
	now func() time.Time,
	generate func(context.Context) ([]byte, error),
) ([]byte, error) {
	if r == nil || r.db == nil || generate == nil || now == nil || binding.APIKeyID <= 0 || binding.UserID <= 0 || binding.GroupID <= 0 || generation == "" ||
		maxStale < 24*time.Hour || maxStale > 720*time.Hour {
		return nil, service.ErrKeyBillingSnapshotUnavailable
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, service.ErrKeyBillingSnapshotUnavailable
		}
		current := now().UTC()
		var rawToken [16]byte
		if _, err := rand.Read(rawToken[:]); err != nil {
			return nil, service.ErrKeyBillingSnapshotUnavailable
		}
		ownerToken := hex.EncodeToString(rawToken[:])
		record, claim, claimed, retryAt, err := r.Acquire(ctx, binding, generation, ownerToken, current, 30*time.Second)
		if err != nil {
			slog.Warn("key_billing_snapshot_store_unavailable", "key_id", binding.APIKeyID)
			return nil, service.ErrKeyBillingSnapshotUnavailable
		}
		// The storage row is authoritative for a setting changed after the caller's read.
		if record.MaxStale < 24*time.Hour || record.MaxStale > 720*time.Hour || record.CheckedAt.IsZero() {
			return nil, service.ErrKeyBillingSnapshotUnavailable
		}
		maxStale = record.MaxStale
		current = record.CheckedAt
		validRecord := record.Binding == binding && record.Generation == generation && len(record.Payload) > 0 && !record.ObservedAt.IsZero()
		if validRecord {
			age := current.Sub(record.ObservedAt)
			if age >= 0 && age < 24*time.Hour {
				if !r.currentSnapshotSettingsAllow(ctx, generation, age) {
					return nil, service.ErrKeyBillingSnapshotUnavailable
				}
				slog.Debug("key_billing_snapshot_hit", "key_id", binding.APIKeyID)
				return append([]byte(nil), record.Payload...), nil
			}
			if !record.RetryAfter.IsZero() && current.Before(record.RetryAfter) {
				if age >= 0 && age <= maxStale && r.currentSnapshotSettingsAllow(ctx, generation, age) {
					slog.Warn("key_billing_snapshot_stale_backoff", "key_id", binding.APIKeyID, "age_seconds", int64(age.Seconds()))
					return append([]byte(nil), record.Payload...), nil
				}
				slog.Warn("key_billing_snapshot_expired", "key_id", binding.APIKeyID, "age_seconds", int64(age.Seconds()))
				return nil, service.ErrKeyBillingSnapshotUnavailable
			}
		}
		if !claimed {
			if validRecord && current.Sub(record.ObservedAt) > maxStale {
				slog.Warn("key_billing_snapshot_expired", "key_id", binding.APIKeyID, "age_seconds", int64(current.Sub(record.ObservedAt).Seconds()))
			}
			if !record.RefreshUntil.IsZero() && current.Before(record.RefreshUntil) {
				if err := waitForKeyBillingSnapshot(ctx, record.RefreshUntil); err != nil {
					return nil, service.ErrKeyBillingSnapshotUnavailable
				}
				continue
			}
			if !retryAt.IsZero() && current.Before(retryAt) {
				return nil, service.ErrKeyBillingSnapshotUnavailable
			}
			return nil, service.ErrKeyBillingSnapshotUnavailable
		}

		slog.Info("key_billing_snapshot_refresh_attempt", "key_id", binding.APIKeyID)
		payload, generationErr := generate(ctx)
		if generationErr != nil || len(payload) == 0 {
			failedAt, err := r.ReadSharedTime(ctx)
			if err != nil {
				slog.Warn("key_billing_snapshot_store_unavailable", "key_id", binding.APIKeyID)
				return nil, service.ErrKeyBillingSnapshotUnavailable
			}
			recorded, err := r.RecordFailure(ctx, claim, failedAt.Add(5*time.Minute), "generation_failed")
			if err != nil || !recorded {
				slog.Warn("key_billing_snapshot_refresh_unavailable", "key_id", binding.APIKeyID)
				return nil, service.ErrKeyBillingSnapshotUnavailable
			}
			age := failedAt.Sub(record.ObservedAt)
			if validRecord && age >= 0 && age <= maxStale && r.currentSnapshotSettingsAllow(ctx, generation, age) {
				slog.Warn("key_billing_snapshot_stale_fallback", "key_id", binding.APIKeyID, "age_seconds", int64(age.Seconds()))
				return append([]byte(nil), record.Payload...), nil
			}
			slog.Warn("key_billing_snapshot_refresh_failed", "key_id", binding.APIKeyID, "had_previous", validRecord)
			return nil, service.ErrKeyBillingSnapshotUnavailable
		}
		generatedAt, err := r.ReadSharedTime(ctx)
		if err != nil {
			slog.Warn("key_billing_snapshot_store_unavailable", "key_id", binding.APIKeyID)
			return nil, service.ErrKeyBillingSnapshotUnavailable
		}
		published, err := r.Publish(ctx, claim, payload, generatedAt)
		if err != nil {
			slog.Warn("key_billing_snapshot_store_unavailable", "key_id", binding.APIKeyID)
			return nil, service.ErrKeyBillingSnapshotUnavailable
		}
		if published {
			if !r.currentSnapshotSettingsAllow(ctx, generation, 0) {
				return nil, service.ErrKeyBillingSnapshotUnavailable
			}
			slog.Info("key_billing_snapshot_refreshed", "key_id", binding.APIKeyID)
			return append([]byte(nil), payload...), nil
		}
		// A changed binding or settings generation must not loop indefinitely.
		slog.Warn("key_billing_snapshot_publish_fenced", "key_id", binding.APIKeyID)
		return nil, service.ErrKeyBillingSnapshotUnavailable
	}
}

// keyBillingSnapshotPollInterval bounds one wait between two shared-row re-reads.
// Every re-read opens a transaction that locks the api_keys row (plus a settings
// read), so polling at the old 25ms let each waiting request drive ~40
// transactions per second per key. A moderate interval keeps the waiter
// responsive while cutting that rate by roughly an order of magnitude.
const keyBillingSnapshotPollInterval = 200 * time.Millisecond

// waitForKeyBillingSnapshot sleeps before the next shared-row re-read. The wait
// never runs past the lease end: a lease that is about to expire (or already
// has) is reclaimed on the next attempt instead of being waited out.
func waitForKeyBillingSnapshot(ctx context.Context, leaseEnd time.Time) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	until := time.Until(leaseEnd)
	if until <= 0 {
		// The lease is already gone; re-read now so the claim can be reclaimed.
		return nil
	}
	wait := keyBillingSnapshotPollInterval
	if until < wait {
		wait = until
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ service.KeyBillingSnapshotStore = (*keyBillingSnapshotRepository)(nil)
