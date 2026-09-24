package repository

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type keyBillingSnapshotRepository struct {
	db *sql.DB
}

func NewKeyBillingSnapshotRepository(db *sql.DB) service.KeyBillingSnapshotStore {
	return &keyBillingSnapshotRepository{db: db}
}

func (r *keyBillingSnapshotRepository) ReadAuthoritativeIdentity(ctx context.Context, keyID int64, presentedCredential string) (service.KeyBillingSnapshotIdentity, error) {
	if r == nil || r.db == nil || keyID <= 0 || presentedCredential == "" {
		return service.KeyBillingSnapshotIdentity{}, service.ErrKeyBillingSnapshotUnavailable
	}
	var identity service.KeyBillingSnapshotIdentity
	var credential string
	var whitelistJSON, blacklistJSON, allowedGroupIDsJSON []byte
	var keyDeletedAt, userDeletedAt, groupDeletedAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT k.id, k.user_id, COALESCE(k.group_id, 0), k.billing_binding_revision,
		       k.key, k.status, k.deleted_at, k.ip_whitelist, k.ip_blacklist,
		       u.status, u.deleted_at, u.restrict_public_groups,
	       to_jsonb(COALESCE((SELECT array_agg(ug.group_id ORDER BY ug.group_id) FROM user_allowed_groups ug WHERE ug.user_id = u.id), ARRAY[]::bigint[])),
	       g.status, g.deleted_at, g.is_exclusive, g.rate_multiplier,
	       g.platform, g.subscription_type, g.peak_rate_enabled,
	       g.peak_start, g.peak_end, g.peak_rate_multiplier
		FROM api_keys k
		LEFT JOIN users u ON u.id = k.user_id
		LEFT JOIN groups g ON g.id = k.group_id
		WHERE k.id = $1
	`, keyID).Scan(
		&identity.Binding.APIKeyID, &identity.Binding.UserID, &identity.Binding.GroupID, &identity.Binding.Revision,
		&credential, &identity.KeyStatus, &keyDeletedAt, &whitelistJSON, &blacklistJSON,
		&identity.UserStatus, &userDeletedAt, &identity.UserRestrictPublicGroups, &allowedGroupIDsJSON,
		&identity.GroupStatus, &groupDeletedAt, &identity.GroupIsExclusive, &identity.GroupRate, &identity.GroupPlatform, &identity.GroupSubscriptionType,
		&identity.PeakRateEnabled, &identity.PeakStart, &identity.PeakEnd, &identity.PeakRateMultiplier,
	)
	if err != nil || subtle.ConstantTimeCompare([]byte(credential), []byte(presentedCredential)) != 1 {
		return service.KeyBillingSnapshotIdentity{}, service.ErrKeyBillingSnapshotUnavailable
	}
	if len(whitelistJSON) > 0 {
		if err := json.Unmarshal(whitelistJSON, &identity.IPWhitelist); err != nil {
			return service.KeyBillingSnapshotIdentity{}, service.ErrKeyBillingSnapshotUnavailable
		}
	}
	if len(blacklistJSON) > 0 {
		if err := json.Unmarshal(blacklistJSON, &identity.IPBlacklist); err != nil {
			return service.KeyBillingSnapshotIdentity{}, service.ErrKeyBillingSnapshotUnavailable
		}
	}
	if len(allowedGroupIDsJSON) > 0 {
		if err := json.Unmarshal(allowedGroupIDsJSON, &identity.UserAllowedGroupIDs); err != nil {
			return service.KeyBillingSnapshotIdentity{}, service.ErrKeyBillingSnapshotUnavailable
		}
	}
	identity.KeyDeleted = keyDeletedAt.Valid
	identity.UserDeleted = userDeletedAt.Valid
	identity.GroupDeleted = groupDeletedAt.Valid
	return identity, nil
}

func billingSnapshotKeyStatusAllowed(status string) bool {
	return status == service.StatusAPIKeyActive || status == service.StatusAPIKeyExpired || status == service.StatusAPIKeyQuotaExhausted
}

func (r *keyBillingSnapshotRepository) ReadSharedTime(ctx context.Context) (time.Time, error) {
	if r == nil || r.db == nil {
		return time.Time{}, service.ErrKeyBillingSnapshotUnavailable
	}
	var current time.Time
	err := r.db.QueryRowContext(ctx, `SELECT NOW()`).Scan(&current)
	if err != nil {
		return time.Time{}, err
	}
	return current.UTC(), nil
}

func (r *keyBillingSnapshotRepository) currentSnapshotSettingsAllow(ctx context.Context, generation string, age time.Duration) bool {
	var enabled bool
	var currentGeneration string
	var maxStaleHours int
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE((value::jsonb->>'enabled')::boolean, false),
		       COALESCE(value::jsonb->>'generation', ''),
		       COALESCE((value::jsonb->>'max_stale_hours')::integer, 0)
		FROM settings WHERE key = $1
	`, service.SettingKeyKeyBillingSnapshot).Scan(&enabled, &currentGeneration, &maxStaleHours)
	return err == nil && enabled && currentGeneration == generation && maxStaleHours >= 24 && maxStaleHours <= 720 &&
		age >= 0 && age <= time.Duration(maxStaleHours)*time.Hour
}

func (r *keyBillingSnapshotRepository) Acquire(ctx context.Context, binding service.KeyBillingSnapshotBinding, generation, ownerToken string, now time.Time, lease time.Duration) (service.KeyBillingSnapshotRecord, service.KeyBillingSnapshotClaim, bool, time.Time, error) {
	if r == nil || r.db == nil {
		return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, errors.New("snapshot database is unavailable")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var current service.KeyBillingSnapshotBinding
	var status string
	var deletedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT id, user_id, COALESCE(group_id, 0), billing_binding_revision, status, deleted_at
		FROM api_keys WHERE id = $1 FOR UPDATE
	`, binding.APIKeyID).Scan(&current.APIKeyID, &current.UserID, &current.GroupID, &current.Revision, &status, &deletedAt)
	if err != nil {
		return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, err
	}
	if deletedAt.Valid || !billingSnapshotKeyStatusAllowed(status) || current != binding {
		return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, service.ErrKeyBillingSnapshotUnavailable
	}
	var enabled bool
	var currentGeneration string
	var maxStaleHours int
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE((value::jsonb->>'enabled')::boolean, false),
		       COALESCE(value::jsonb->>'generation', 'disabled'),
		       COALESCE((value::jsonb->>'max_stale_hours')::integer, 72)
		FROM settings WHERE key = $1
	`, service.SettingKeyKeyBillingSnapshot).Scan(&enabled, &currentGeneration, &maxStaleHours)
	if errors.Is(err, sql.ErrNoRows) {
		enabled = false
		currentGeneration = "disabled"
	} else if err != nil {
		return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, err
	}
	if !enabled || currentGeneration != generation || maxStaleHours < 24 || maxStaleHours > 720 {
		return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, service.ErrKeyBillingSnapshotUnavailable
	}

	var record service.KeyBillingSnapshotRecord
	record.MaxStale = time.Duration(maxStaleHours) * time.Hour
	if err := tx.QueryRowContext(ctx, `SELECT NOW()`).Scan(&record.CheckedAt); err != nil {
		return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, err
	}
	var payload []byte
	var observedAt, retryAfter, leaseUntil sql.NullTime
	var refreshOwner, failureCode sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT owner_user_id, group_id, binding_revision, generation, payload, observed_at,
		       refresh_owner, refresh_lease_until, retry_after, failure_code
		FROM key_billing_snapshots WHERE api_key_id = $1
	`, binding.APIKeyID).Scan(
		&record.Binding.UserID, &record.Binding.GroupID, &record.Binding.Revision, &record.Generation,
		&payload, &observedAt, &refreshOwner, &leaseUntil, &retryAfter, &failureCode,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, err
	}
	if err == nil {
		record.Binding.APIKeyID = binding.APIKeyID
		record.Payload = payload
		if observedAt.Valid {
			record.ObservedAt = observedAt.Time
		}
		if retryAfter.Valid {
			record.RetryAfter = retryAfter.Time
		}
		if leaseUntil.Valid {
			record.RefreshUntil = leaseUntil.Time
		}
		if failureCode.Valid {
			record.FailureCode = failureCode.String
		}
	}
	if record.Binding != binding || record.Generation != generation {
		record = service.KeyBillingSnapshotRecord{MaxStale: time.Duration(maxStaleHours) * time.Hour, CheckedAt: record.CheckedAt}
	}

	nowDB := record.CheckedAt
	if observedAt.Valid && nowDB.Sub(observedAt.Time) >= 0 && nowDB.Sub(observedAt.Time) < 24*time.Hour && record.Binding == binding && record.Generation == generation && len(record.Payload) > 0 {
		if err := tx.Commit(); err != nil {
			return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, err
		}
		return record, service.KeyBillingSnapshotClaim{}, false, time.Time{}, nil
	}
	if record.Binding == binding && record.Generation == generation && leaseUntil.Valid && leaseUntil.Time.After(nowDB) {
		claim := service.KeyBillingSnapshotClaim{Binding: binding, Generation: generation, OwnerToken: refreshOwner.String, Revision: binding.Revision, ExpiresAt: leaseUntil.Time}
		if err := tx.Commit(); err != nil {
			return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, err
		}
		return record, claim, false, leaseUntil.Time, nil
	}
	if record.RetryAfter.After(nowDB) {
		if err := tx.Commit(); err != nil {
			return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, err
		}
		return record, service.KeyBillingSnapshotClaim{}, false, record.RetryAfter, nil
	}

	claim := service.KeyBillingSnapshotClaim{Binding: binding, Generation: generation, OwnerToken: ownerToken, Revision: binding.Revision, ExpiresAt: nowDB.Add(lease)}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO key_billing_snapshots (api_key_id, owner_user_id, group_id, binding_revision, generation, refresh_owner, refresh_lease_until, retry_after, failure_code, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULL, NULL, NOW())
		ON CONFLICT (api_key_id) DO UPDATE SET
			owner_user_id = EXCLUDED.owner_user_id,
			group_id = EXCLUDED.group_id,
			binding_revision = EXCLUDED.binding_revision,
			generation = EXCLUDED.generation,
			payload = CASE WHEN key_billing_snapshots.generation = EXCLUDED.generation
				AND key_billing_snapshots.owner_user_id = EXCLUDED.owner_user_id
				AND key_billing_snapshots.group_id = EXCLUDED.group_id
				AND key_billing_snapshots.binding_revision = EXCLUDED.binding_revision
				THEN key_billing_snapshots.payload ELSE NULL END,
			observed_at = CASE WHEN key_billing_snapshots.generation = EXCLUDED.generation
				AND key_billing_snapshots.owner_user_id = EXCLUDED.owner_user_id
				AND key_billing_snapshots.group_id = EXCLUDED.group_id
				AND key_billing_snapshots.binding_revision = EXCLUDED.binding_revision
				THEN key_billing_snapshots.observed_at ELSE NULL END,
			refresh_owner = EXCLUDED.refresh_owner,
			refresh_lease_until = EXCLUDED.refresh_lease_until,
			retry_after = NULL,
			failure_code = NULL,
			updated_at = NOW()
	`, binding.APIKeyID, binding.UserID, binding.GroupID, binding.Revision, generation, ownerToken, claim.ExpiresAt)
	if err != nil {
		return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, err
	}
	return record, claim, true, time.Time{}, nil
}

func (r *keyBillingSnapshotRepository) Publish(ctx context.Context, claim service.KeyBillingSnapshotClaim, payload []byte, _ time.Time) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("snapshot database is unavailable")
	}
	if !json.Valid(payload) {
		return false, errors.New("snapshot response is not valid JSON")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var current service.KeyBillingSnapshotBinding
	var status string
	var deletedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT id, user_id, COALESCE(group_id, 0), billing_binding_revision, status, deleted_at
		FROM api_keys WHERE id = $1 FOR UPDATE
	`, claim.Binding.APIKeyID).Scan(&current.APIKeyID, &current.UserID, &current.GroupID, &current.Revision, &status, &deletedAt); err != nil {
		return false, nil
	}
	if current != claim.Binding || !billingSnapshotKeyStatusAllowed(status) || deletedAt.Valid {
		return false, nil
	}
	var enabled bool
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT (value::jsonb->>'enabled')::boolean, value::jsonb->>'generation' FROM settings WHERE key = $1 FOR SHARE`, service.SettingKeyKeyBillingSnapshot).Scan(&enabled, &generation); err != nil {
		return false, nil
	}
	if !enabled || generation != claim.Generation {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE key_billing_snapshots AS snap
		SET payload = $8, observed_at = NOW(), refresh_owner = NULL, refresh_lease_until = NULL,
		    retry_after = NULL, failure_code = NULL, updated_at = NOW()
		WHERE snap.api_key_id = $1 AND snap.owner_user_id = $2 AND snap.group_id = $3
		  AND snap.binding_revision = $4 AND snap.generation = $5 AND snap.refresh_owner = $6
		  AND snap.refresh_lease_until > NOW()
		  AND EXISTS (
		      SELECT 1 FROM api_keys k WHERE k.id = snap.api_key_id
		        AND k.user_id = $2 AND COALESCE(k.group_id, 0) = $3
		        AND k.billing_binding_revision = $4 AND k.status IN ('active', 'expired', 'quota_exhausted') AND k.deleted_at IS NULL
		  )
		  AND EXISTS (
		      SELECT 1 FROM settings s WHERE s.key = $7
		        AND (s.value::jsonb->>'enabled')::boolean = TRUE
		        AND s.value::jsonb->>'generation' = $5
		  )
	`, claim.Binding.APIKeyID, claim.Binding.UserID, claim.Binding.GroupID, claim.Revision, claim.Generation,
		claim.OwnerToken, service.SettingKeyKeyBillingSnapshot, string(payload))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (r *keyBillingSnapshotRepository) RecordFailure(ctx context.Context, claim service.KeyBillingSnapshotClaim, retryAt time.Time, safeCode string) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("snapshot database is unavailable")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var current service.KeyBillingSnapshotBinding
	var status string
	var deletedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT id, user_id, COALESCE(group_id, 0), billing_binding_revision, status, deleted_at
		FROM api_keys WHERE id = $1 FOR UPDATE
	`, claim.Binding.APIKeyID).Scan(&current.APIKeyID, &current.UserID, &current.GroupID, &current.Revision, &status, &deletedAt); err != nil {
		return false, nil
	}
	if current != claim.Binding || !billingSnapshotKeyStatusAllowed(status) || deletedAt.Valid {
		return false, nil
	}
	var enabled bool
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT (value::jsonb->>'enabled')::boolean, value::jsonb->>'generation' FROM settings WHERE key = $1 FOR SHARE`, service.SettingKeyKeyBillingSnapshot).Scan(&enabled, &generation); err != nil {
		return false, nil
	}
	if !enabled || generation != claim.Generation {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE key_billing_snapshots AS snap
		SET refresh_owner = NULL, refresh_lease_until = NULL, retry_after = $7,
		    failure_code = $8, updated_at = NOW()
		WHERE snap.api_key_id = $1 AND snap.owner_user_id = $2 AND snap.group_id = $3
		  AND snap.binding_revision = $4 AND snap.generation = $5 AND snap.refresh_owner = $6
		  AND snap.refresh_lease_until > NOW()
		  AND EXISTS (
		      SELECT 1 FROM api_keys k WHERE k.id = snap.api_key_id
		        AND k.user_id = $2 AND COALESCE(k.group_id, 0) = $3
		        AND k.billing_binding_revision = $4 AND k.status IN ('active', 'expired', 'quota_exhausted') AND k.deleted_at IS NULL
		  )
		  AND EXISTS (
		      SELECT 1 FROM settings s WHERE s.key = $9
		        AND (s.value::jsonb->>'enabled')::boolean = TRUE
		        AND s.value::jsonb->>'generation' = $5
		  )
	`, claim.Binding.APIKeyID, claim.Binding.UserID, claim.Binding.GroupID, claim.Revision, claim.Generation,
		claim.OwnerToken, retryAt, safeCode, service.SettingKeyKeyBillingSnapshot)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
