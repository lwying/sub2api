//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// These tests exercise the key-scoped billing snapshot store against a real PostgreSQL
// server. Two repositories are instantiated independently over the same shared database
// handle (integrationDB) to model two application instances that only share storage,
// which is what tickets 04/05 require to be verified end to end.
//
// Every fixture is written straight to integrationDB (committed, not inside a rolled-back
// ent transaction) because the repositories under test own their own connections and could
// not observe an uncommitted fixture. Cleanup deletes what was created.

const (
	snapshotGenerationInitial = "snapshot-generation-initial"
	snapshotGenerationRestart = "snapshot-generation-after-restart"
)

type snapshotKeyFixture struct {
	userID     int64
	groupID    int64
	keyID      int64
	credential string
	revision   int64
}

func (f snapshotKeyFixture) binding() service.KeyBillingSnapshotBinding {
	return service.KeyBillingSnapshotBinding{
		APIKeyID: f.keyID,
		UserID:   f.userID,
		GroupID:  f.groupID,
		Revision: f.revision,
	}
}

// newSnapshotRepos returns two independently constructed repositories over the same
// integrationDB handle, standing in for two instances during one acceptance run.
func newSnapshotRepos(t *testing.T) (service.KeyBillingSnapshotStore, service.KeyBillingSnapshotStore) {
	t.Helper()

	first := NewKeyBillingSnapshotRepository(integrationDB)
	second := NewKeyBillingSnapshotRepository(integrationDB)
	require.NotSame(t, first, second, "each instance must own its own repository value")

	return first, second
}

func createSnapshotKeyFixture(t *testing.T, groupName string) snapshotKeyFixture {
	t.Helper()

	ctx := context.Background()
	client := testEntClient(t)
	suffix := time.Now().UnixNano()

	user, err := client.User.Create().
		SetEmail(fmt.Sprintf("snapshot-%d@test.com", suffix)).
		SetPasswordHash("test-password-hash").
		SetStatus(service.StatusActive).
		SetRole(service.RoleUser).
		Save(ctx)
	require.NoError(t, err, "create snapshot user")

	group, err := client.Group.Create().
		SetName(fmt.Sprintf("%s-%d", groupName, suffix)).
		SetStatus(service.StatusActive).
		Save(ctx)
	require.NoError(t, err, "create snapshot group")

	credential := fmt.Sprintf("sk-snapshot-%d", suffix)
	key, err := client.APIKey.Create().
		SetUserID(user.ID).
		SetGroupID(group.ID).
		SetKey(credential).
		SetName("snapshot key").
		SetStatus(service.StatusAPIKeyActive).
		Save(ctx)
	require.NoError(t, err, "create snapshot api key")

	fixture := snapshotKeyFixture{userID: user.ID, groupID: group.ID, keyID: key.ID, credential: credential}
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT billing_binding_revision FROM api_keys WHERE id = $1`, key.ID).Scan(&fixture.revision),
		"read initial billing binding revision")

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = integrationDB.ExecContext(bg, `DELETE FROM api_keys WHERE id = $1`, key.ID)
		_, _ = integrationDB.ExecContext(bg, `DELETE FROM groups WHERE id = $1`, group.ID)
		_, _ = integrationDB.ExecContext(bg, `DELETE FROM users WHERE id = $1`, user.ID)
	})

	return fixture
}

// createSiblingKey adds a second key for the same user and group, so isolation can be
// asserted even when both keys would resolve to identical rates.
func createSiblingKey(t *testing.T, owner snapshotKeyFixture) snapshotKeyFixture {
	t.Helper()

	ctx := context.Background()
	credential := fmt.Sprintf("sk-snapshot-sibling-%d", time.Now().UnixNano())
	key, err := testEntClient(t).APIKey.Create().
		SetUserID(owner.userID).
		SetGroupID(owner.groupID).
		SetKey(credential).
		SetName("snapshot sibling key").
		SetStatus(service.StatusAPIKeyActive).
		Save(ctx)
	require.NoError(t, err, "create sibling api key")

	sibling := snapshotKeyFixture{
		userID:     owner.userID,
		groupID:    owner.groupID,
		keyID:      key.ID,
		credential: credential,
	}
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT billing_binding_revision FROM api_keys WHERE id = $1`, key.ID).Scan(&sibling.revision))

	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM api_keys WHERE id = $1`, key.ID)
	})

	return sibling
}

func createSnapshotGroup(t *testing.T, name string) int64 {
	t.Helper()

	group, err := testEntClient(t).Group.Create().
		SetName(fmt.Sprintf("%s-%d", name, time.Now().UnixNano())).
		SetStatus(service.StatusActive).
		Save(context.Background())
	require.NoError(t, err, "create rebind target group")

	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM groups WHERE id = $1`, group.ID)
	})

	return group.ID
}

// enableSnapshot turns the feature on for the current fixture.
func enableSnapshot(t *testing.T, generation string, maxStaleHours int) {
	t.Helper()

	writeSnapshotSettings(t, service.KeyBillingSnapshotSettings{
		Enabled:       true,
		MaxStaleHours: maxStaleHours,
		Generation:    generation,
	})
}

func writeSnapshotSettings(t *testing.T, settings service.KeyBillingSnapshotSettings) {
	t.Helper()

	payload, err := json.Marshal(settings)
	require.NoError(t, err, "encode snapshot settings")

	_, err = integrationDB.ExecContext(context.Background(), `
		INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()
	`, service.SettingKeyKeyBillingSnapshot, string(payload))
	require.NoError(t, err, "write snapshot settings")
}

// disableSnapshot removes the settings row, which is the documented default-off state.
func disableSnapshot(t *testing.T) {
	t.Helper()

	_, err := integrationDB.ExecContext(context.Background(),
		`DELETE FROM settings WHERE key = $1`, service.SettingKeyKeyBillingSnapshot)
	require.NoError(t, err, "remove snapshot settings row")
}

func withSnapshotSettingsCleanup(t *testing.T) {
	t.Helper()

	var originalValue string
	err := integrationDB.QueryRowContext(context.Background(),
		`SELECT value FROM settings WHERE key = $1`, service.SettingKeyKeyBillingSnapshot).Scan(&originalValue)
	require.True(t, err == nil || errors.Is(err, sql.ErrNoRows), "read existing snapshot settings: %v", err)
	existed := err == nil
	t.Cleanup(func() {
		if !existed {
			_, err = integrationDB.ExecContext(context.Background(),
				`DELETE FROM settings WHERE key = $1`, service.SettingKeyKeyBillingSnapshot)
		} else {
			_, err = integrationDB.ExecContext(context.Background(), `
				INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, NOW())
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()
			`, service.SettingKeyKeyBillingSnapshot, originalValue)
		}
		require.NoError(t, err, "restore original snapshot settings")
	})
}

// ageSnapshot rewrites observed_at so the stored declaration looks older than the 24h
// freshness window without waiting for wall-clock time to pass.
func ageSnapshot(t *testing.T, keyID int64, age time.Duration) {
	t.Helper()

	result, err := integrationDB.ExecContext(context.Background(), `
		UPDATE key_billing_snapshots
		SET observed_at = NOW() - make_interval(secs => $2::double precision), updated_at = NOW()
		WHERE api_key_id = $1
	`, keyID, age.Seconds())
	require.NoError(t, err, "age snapshot")
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected, "expected an existing snapshot to age")
}

type snapshotRowState struct {
	groupID      int64
	revision     int64
	generation   string
	payload      sql.NullString
	observedAt   sql.NullTime
	refreshOwner sql.NullString
	leaseUntil   sql.NullTime
	retryAfter   sql.NullTime
	failureCode  sql.NullString
}

func readSnapshotRow(t *testing.T, keyID int64) snapshotRowState {
	t.Helper()

	var state snapshotRowState
	err := integrationDB.QueryRowContext(context.Background(), `
		SELECT group_id, binding_revision, generation, payload, observed_at,
		       refresh_owner, refresh_lease_until, retry_after, failure_code
		FROM key_billing_snapshots WHERE api_key_id = $1
	`, keyID).Scan(
		&state.groupID, &state.revision, &state.generation, &state.payload, &state.observedAt,
		&state.refreshOwner, &state.leaseUntil, &state.retryAfter, &state.failureCode,
	)
	require.NoError(t, err, "read snapshot row for key %d", keyID)
	return state
}

func countSnapshotRows(t *testing.T, keyID int64) int {
	t.Helper()

	var count int
	require.NoError(t, integrationDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM key_billing_snapshots WHERE api_key_id = $1`, keyID).Scan(&count))
	return count
}

func snapshotPayload(rate float64, observedAt string) []byte {
	return []byte(fmt.Sprintf(`{"rate":%g,"observed_at":%q,"scope":"key"}`, rate, observedAt))
}

func observedAtField(t *testing.T, payload []byte) string {
	t.Helper()

	var decoded struct {
		ObservedAt string `json:"observed_at"`
	}
	require.NoError(t, json.Unmarshal(payload, &decoded), "snapshot payload must stay valid JSON")
	return decoded.ObservedAt
}

func alwaysFails(context.Context) ([]byte, error) {
	return nil, fmt.Errorf("upstream rate introspection failed")
}

// --- 04: one shared declaration per key, across instances ---------------------------

func TestKeyBillingSnapshotIntegrationServesOneJSONAcrossInstances(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	withSnapshotSettingsCleanup(t)
	enableSnapshot(t, snapshotGenerationInitial, service.DefaultKeyBillingSnapshotMaxStaleHours)

	fixture := createSnapshotKeyFixture(t, "snapshot-cross-instance")
	first, second := newSnapshotRepos(t)

	var firstGenerations, secondGenerations int32
	firstPayload, err := first.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) {
			atomic.AddInt32(&firstGenerations, 1)
			return snapshotPayload(1.5, "2026-01-01T00:00:00Z"), nil
		})
	require.NoError(t, err, "first instance should publish the initial declaration")
	require.Equal(t, int32(1), atomic.LoadInt32(&firstGenerations))

	secondPayload, err := second.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) {
			atomic.AddInt32(&secondGenerations, 1)
			return snapshotPayload(9.9, "2026-01-01T00:00:00Z"), nil
		})
	require.NoError(t, err, "second instance should reuse the stored declaration")
	require.Equal(t, int32(0), atomic.LoadInt32(&secondGenerations),
		"a second instance must not regenerate a declaration that is still fresh")
	require.Equal(t, firstPayload, secondPayload, "both instances must serve byte-identical JSON")
	require.Equal(t, observedAtField(t, firstPayload), observedAtField(t, secondPayload),
		"the shared declaration must carry one observed_at")

	row := readSnapshotRow(t, fixture.keyID)
	require.Equal(t, string(firstPayload), row.payload.String)
	require.Equal(t, fixture.revision, row.revision)
	require.Equal(t, fixture.groupID, row.groupID)
	require.Equal(t, snapshotGenerationInitial, row.generation)
	require.False(t, row.leaseUntil.Valid, "a fresh read must not hold a refresh lease")
	require.False(t, row.retryAfter.Valid, "a fresh read must not open a retry window")
}

func TestKeyBillingSnapshotIntegrationIgnoresApplicationClockSkew(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	withSnapshotSettingsCleanup(t)
	enableSnapshot(t, snapshotGenerationInitial, service.DefaultKeyBillingSnapshotMaxStaleHours)

	fixture := createSnapshotKeyFixture(t, "snapshot-clock-skew")
	first, second := newSnapshotRepos(t)
	firstPayload := snapshotPayload(1.5, "2026-01-01T00:00:00Z")
	generated, err := first.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour,
		func() time.Time { return time.Now().Add(2 * time.Hour) },
		func(context.Context) ([]byte, error) { return firstPayload, nil })
	require.NoError(t, err)
	var secondGenerations int
	reused, err := second.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour,
		func() time.Time { return time.Now().Add(-2 * time.Hour) },
		func(context.Context) ([]byte, error) {
			secondGenerations++
			return snapshotPayload(9.9, "2026-01-01T00:00:00Z"), nil
		})
	require.NoError(t, err)
	require.Equal(t, 0, secondGenerations)
	require.Equal(t, generated, reused)
}

func TestKeyBillingSnapshotIntegrationIsolatesKeysOfTheSameOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	withSnapshotSettingsCleanup(t)
	enableSnapshot(t, snapshotGenerationInitial, service.DefaultKeyBillingSnapshotMaxStaleHours)

	owner := createSnapshotKeyFixture(t, "snapshot-isolation")
	sibling := createSiblingKey(t, owner)
	require.Equal(t, owner.userID, sibling.userID, "fixture precondition: same owner")
	require.Equal(t, owner.groupID, sibling.groupID, "fixture precondition: same group")

	repo, _ := newSnapshotRepos(t)

	ownerPayload, err := repo.GetOrCreate(ctx, owner.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) { return snapshotPayload(1.5, "2026-01-02T00:00:00Z"), nil })
	require.NoError(t, err)

	siblingPayload, err := repo.GetOrCreate(ctx, sibling.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) { return snapshotPayload(2.5, "2026-01-02T00:00:00Z"), nil })
	require.NoError(t, err)

	require.NotEqual(t, string(ownerPayload), string(siblingPayload),
		"keys of the same owner and group must keep independent declarations")

	// Re-reading one key must never pick up the other key's declaration.
	ownerAgain, err := repo.GetOrCreate(ctx, owner.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) { return snapshotPayload(8.8, "2026-01-02T00:00:00Z"), nil })
	require.NoError(t, err)
	require.Equal(t, ownerPayload, ownerAgain)

	ownerRow := readSnapshotRow(t, owner.keyID)
	siblingRow := readSnapshotRow(t, sibling.keyID)
	require.Equal(t, string(ownerPayload), ownerRow.payload.String)
	require.Equal(t, string(siblingPayload), siblingRow.payload.String)
	require.Equal(t, 1, countSnapshotRows(t, owner.keyID))
	require.Equal(t, 1, countSnapshotRows(t, sibling.keyID))
}

// --- 04: rebinding fences old claims and old declarations ---------------------------

func TestKeyBillingSnapshotIntegrationRebindInvalidatesOldOwnership(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	withSnapshotSettingsCleanup(t)
	enableSnapshot(t, snapshotGenerationInitial, service.DefaultKeyBillingSnapshotMaxStaleHours)

	fixture := createSnapshotKeyFixture(t, "snapshot-rebind")
	repo, _ := newSnapshotRepos(t)

	originalBinding := fixture.binding()
	_, claim, claimed, _, err := repo.Acquire(ctx, originalBinding, snapshotGenerationInitial, "owner-before-rebind", time.Now().UTC(), 30*time.Second)
	require.NoError(t, err)
	require.True(t, claimed, "first acquisition on a clean key must win the lease")

	// The administrator moves the key to another group; the trigger bumps the revision.
	targetGroup := createSnapshotGroup(t, "snapshot-rebind-target")
	_, err = integrationDB.ExecContext(ctx, `UPDATE api_keys SET group_id = $2 WHERE id = $1`, fixture.keyID, targetGroup)
	require.NoError(t, err, "rebind api key")

	identity, err := repo.ReadAuthoritativeIdentity(ctx, fixture.keyID, fixture.credential)
	require.NoError(t, err, "a rebind must not break authentication of the key itself")
	require.Equal(t, targetGroup, identity.Binding.GroupID, "identity must report the new group")
	require.Greater(t, identity.Binding.Revision, originalBinding.Revision, "rebind must bump the binding revision")

	// The in-flight refresh from before the rebind must not publish under the old ownership.
	published, err := repo.Publish(ctx, claim, snapshotPayload(7.7, "2026-01-03T00:00:00Z"), time.Now().UTC())
	require.NoError(t, err)
	require.False(t, published, "a claim taken before a rebind must not publish")

	recorded, err := repo.RecordFailure(ctx, claim, time.Now().UTC().Add(5*time.Minute), "generation_failed")
	require.NoError(t, err)
	require.False(t, recorded, "a claim taken before a rebind must not open a retry window")

	_, err = repo.GetOrCreate(ctx, originalBinding, snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) { return snapshotPayload(7.7, "2026-01-03T00:00:00Z"), nil })
	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable,
		"the pre-rebind ownership must no longer be servable")

	// The new ownership resolves and stores its own declaration.
	newBinding := service.KeyBillingSnapshotBinding{
		APIKeyID: fixture.keyID,
		UserID:   fixture.userID,
		GroupID:  targetGroup,
		Revision: identity.Binding.Revision,
	}
	reboundDeclaration := snapshotPayload(3.3, "2026-01-03T00:00:00Z")
	newPayload, err := repo.GetOrCreate(ctx, newBinding, snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) { return reboundDeclaration, nil })
	require.NoError(t, err, "the rebind target must be able to publish")
	require.Equal(t, reboundDeclaration, newPayload)

	row := readSnapshotRow(t, fixture.keyID)
	require.Equal(t, targetGroup, row.groupID, "the stored declaration must follow the rebind")
	require.Equal(t, identity.Binding.Revision, row.revision)
	require.Equal(t, string(newPayload), row.payload.String)

	// A different credential must never read the authoritative identity.
	_, err = repo.ReadAuthoritativeIdentity(ctx, fixture.keyID, fixture.credential+"-wrong")
	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable)
}

// --- 04: the shared-store fence rejects a refresh that lost its lease ---------------

func TestKeyBillingSnapshotIntegrationFencesRefreshThatLostItsLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	withSnapshotSettingsCleanup(t)
	enableSnapshot(t, snapshotGenerationInitial, service.DefaultKeyBillingSnapshotMaxStaleHours)

	fixture := createSnapshotKeyFixture(t, "snapshot-lease-fence")
	repo, _ := newSnapshotRepos(t)

	_, claim, claimed, _, err := repo.Acquire(ctx, fixture.binding(), snapshotGenerationInitial, "owner-stale", time.Now().UTC(), 30*time.Second)
	require.NoError(t, err)
	require.True(t, claimed)

	// Every guard outside the shared store still holds: the key is active, unrebound, and
	// the settings generation matches. Only the row-level lease fence can reject the write.
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE key_billing_snapshots SET refresh_owner = $2, refresh_lease_until = NOW() + INTERVAL '30 seconds'
		WHERE api_key_id = $1
	`, fixture.keyID, "owner-takeover")
	require.NoError(t, err)

	published, err := repo.Publish(ctx, claim, snapshotPayload(4.4, "2026-01-04T00:00:00Z"), time.Now().UTC())
	require.NoError(t, err)
	require.False(t, published, "a superseded owner must not publish")

	recorded, err := repo.RecordFailure(ctx, claim, time.Now().UTC().Add(5*time.Minute), "generation_failed")
	require.NoError(t, err)
	require.False(t, recorded, "a superseded owner must not open a retry window")

	row := readSnapshotRow(t, fixture.keyID)
	require.False(t, row.payload.Valid, "a fenced publish must not store a payload")
	require.False(t, row.retryAfter.Valid, "a fenced failure must not store a retry window")
	require.Equal(t, "owner-takeover", row.refreshOwner.String, "the fence must leave the winning owner intact")

	// An expired lease is equally unusable, even for the owner named on the row.
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE key_billing_snapshots SET refresh_owner = $2, refresh_lease_until = NOW() - INTERVAL '1 second'
		WHERE api_key_id = $1
	`, fixture.keyID, claim.OwnerToken)
	require.NoError(t, err)

	published, err = repo.Publish(ctx, claim, snapshotPayload(4.4, "2026-01-04T00:00:00Z"), time.Now().UTC())
	require.NoError(t, err)
	require.False(t, published, "an expired lease must not publish")

	recorded, err = repo.RecordFailure(ctx, claim, time.Now().UTC().Add(5*time.Minute), "generation_failed")
	require.NoError(t, err)
	require.False(t, recorded, "an expired lease must not open a retry window")

	row = readSnapshotRow(t, fixture.keyID)
	require.False(t, row.payload.Valid)
	require.False(t, row.retryAfter.Valid)
}

// --- 05: 24h refresh, stale fallback, hard deadline ---------------------------------

func TestKeyBillingSnapshotIntegrationRefreshesAfter24Hours(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	withSnapshotSettingsCleanup(t)
	enableSnapshot(t, snapshotGenerationInitial, service.DefaultKeyBillingSnapshotMaxStaleHours)

	fixture := createSnapshotKeyFixture(t, "snapshot-refresh")
	repo, otherInstance := newSnapshotRepos(t)

	initial := snapshotPayload(1.0, "2026-01-05T00:00:00Z")
	payload, err := repo.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) { return initial, nil })
	require.NoError(t, err)
	require.Equal(t, initial, payload)

	// Just under the freshness window: every instance keeps serving the stored declaration.
	ageSnapshot(t, fixture.keyID, 23*time.Hour)
	var underWindowGenerations int32
	reused, err := otherInstance.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) {
			atomic.AddInt32(&underWindowGenerations, 1)
			return snapshotPayload(6.6, "2026-01-05T00:00:00Z"), nil
		})
	require.NoError(t, err)
	require.Equal(t, initial, reused, "a declaration younger than 24h must be reused")
	require.Equal(t, int32(0), atomic.LoadInt32(&underWindowGenerations))

	// Past the freshness window: the next probe refreshes and publishes a new observed_at.
	ageSnapshot(t, fixture.keyID, 25*time.Hour)
	refreshed := snapshotPayload(5.5, "2026-01-06T00:00:00Z")
	payload, err = repo.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) { return refreshed, nil })
	require.NoError(t, err)
	require.Equal(t, refreshed, payload, "an expired declaration must be refreshed")

	row := readSnapshotRow(t, fixture.keyID)
	require.Equal(t, string(refreshed), row.payload.String)
	require.WithinDuration(t, time.Now().UTC(), row.observedAt.Time, 2*time.Minute,
		"a successful refresh must advance observed_at")
	require.False(t, row.leaseUntil.Valid, "a finished refresh must release its lease")
	require.False(t, row.failureCode.Valid)
}

func TestKeyBillingSnapshotIntegrationFallsBackToStaleDeclarationWithinDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	withSnapshotSettingsCleanup(t)
	enableSnapshot(t, snapshotGenerationInitial, service.DefaultKeyBillingSnapshotMaxStaleHours)

	fixture := createSnapshotKeyFixture(t, "snapshot-stale-fallback")
	first, second := newSnapshotRepos(t)

	stale := snapshotPayload(1.25, "2026-01-07T00:00:00Z")
	payload, err := first.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) { return stale, nil })
	require.NoError(t, err)
	require.Equal(t, stale, payload)

	ageSnapshot(t, fixture.keyID, 25*time.Hour)

	// The refresh attempt fails; the still-within-deadline declaration is served instead.
	payload, err = first.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now, alwaysFails)
	require.NoError(t, err, "a failed refresh inside the stale window must fall back, not error")
	require.Equal(t, stale, payload, "the shared stale declaration must be reused")

	row := readSnapshotRow(t, fixture.keyID)
	require.Equal(t, string(stale), row.payload.String, "a fallback must not overwrite the stored declaration")
	require.True(t, row.retryAfter.Valid, "a failed refresh must open a retry window")
	require.Equal(t, "generation_failed", row.failureCode.String)
	require.False(t, row.leaseUntil.Valid, "a failed refresh must release its lease")

	// The retry window stops every instance from re-attempting on each request.
	var retryGenerations int32
	payload, err = second.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
		func(context.Context) ([]byte, error) {
			atomic.AddInt32(&retryGenerations, 1)
			return snapshotPayload(9.9, "2026-01-07T00:00:00Z"), nil
		})
	require.NoError(t, err)
	require.Equal(t, stale, payload, "the backoff window must keep serving the stale declaration")
	require.Equal(t, int32(0), atomic.LoadInt32(&retryGenerations),
		"the backoff window must not trigger another generation attempt")
}

func TestKeyBillingSnapshotIntegrationRejectsDeclarationPastDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	withSnapshotSettingsCleanup(t)
	enableSnapshot(t, snapshotGenerationInitial, service.MinKeyBillingSnapshotMaxStaleHours)

	fixture := createSnapshotKeyFixture(t, "snapshot-hard-deadline")
	repo, _ := newSnapshotRepos(t)

	payload, err := repo.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 24*time.Hour, time.Now,
		func(context.Context) ([]byte, error) { return snapshotPayload(1.75, "2026-01-08T00:00:00Z"), nil })
	require.NoError(t, err)
	require.NotEmpty(t, payload)

	// Refreshed a little past the 24h freshness window but past the configured deadline:
	// a failed refresh must fail closed rather than serve an out-of-date declaration.
	ageSnapshot(t, fixture.keyID, 25*time.Hour)

	var generationAttempts int32
	_, err = repo.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 24*time.Hour, time.Now,
		func(context.Context) ([]byte, error) {
			atomic.AddInt32(&generationAttempts, 1)
			return nil, fmt.Errorf("upstream rate introspection failed")
		})
	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable,
		"a declaration past the longest-stale deadline must not be served")
	require.Equal(t, int32(1), atomic.LoadInt32(&generationAttempts), "the probe still attempted a refresh")

	row := readSnapshotRow(t, fixture.keyID)
	require.True(t, row.retryAfter.Valid, "the failed refresh must record a retry window")

	// The hard deadline outranks an open retry window: no attempt, no stale value.
	var retryAttempts int32
	_, err = repo.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 24*time.Hour, time.Now,
		func(context.Context) ([]byte, error) {
			atomic.AddInt32(&retryAttempts, 1)
			return snapshotPayload(1.75, "2026-01-08T00:00:00Z"), nil
		})
	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable,
		"a retry window must not extend the longest-stale deadline")
	require.Equal(t, int32(0), atomic.LoadInt32(&retryAttempts),
		"an expired deadline must not generate a new declaration")
}

// --- 04/05: concurrent first access publishes exactly one declaration ---------------

func TestKeyBillingSnapshotIntegrationConcurrentFirstReadPublishesOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	withSnapshotSettingsCleanup(t)
	enableSnapshot(t, snapshotGenerationInitial, service.DefaultKeyBillingSnapshotMaxStaleHours)

	fixture := createSnapshotKeyFixture(t, "snapshot-concurrent")
	first, second := newSnapshotRepos(t)
	repos := []service.KeyBillingSnapshotStore{first, second}

	declaration := snapshotPayload(2.25, "2026-01-09T00:00:00Z")
	var generations int32

	start := make(chan struct{})
	payloads := make([][]byte, len(repos))
	errs := make([]error, len(repos))
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		go func(index int, store service.KeyBillingSnapshotStore) {
			defer wg.Done()
			<-start
			payloads[index], errs[index] = store.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now,
				func(context.Context) ([]byte, error) {
					if atomic.AddInt32(&generations, 1) == 1 {
						// Hold the lease long enough for the other instance to observe it.
						time.Sleep(200 * time.Millisecond)
					}
					return declaration, nil
				})
		}(i, repo)
	}
	close(start)
	wg.Wait()

	require.NoError(t, errs[0], "first concurrent caller")
	require.NoError(t, errs[1], "second concurrent caller")
	require.Equal(t, int32(1), atomic.LoadInt32(&generations),
		"concurrent first access must produce exactly one reliable declaration")
	require.Equal(t, declaration, payloads[0])
	require.Equal(t, declaration, payloads[1])
	require.Equal(t, 1, countSnapshotRows(t, fixture.keyID))

	row := readSnapshotRow(t, fixture.keyID)
	require.Equal(t, string(declaration), row.payload.String)
	require.False(t, row.leaseUntil.Valid, "the winning refresh must release its lease")
	require.False(t, row.retryAfter.Valid)
}

// --- 04/05: the switch itself is the external contract ------------------------------

func TestKeyBillingSnapshotIntegrationDisabledSwitchFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	withSnapshotSettingsCleanup(t)
	disableSnapshot(t)

	fixture := createSnapshotKeyFixture(t, "snapshot-switch")
	repo, otherInstance := newSnapshotRepos(t)

	var generations int32
	// Each call must return distinct bytes. With a constant payload the final assertion
	// could not tell a reused declaration apart from a freshly generated identical one,
	// and would fail even when the new generation behaved correctly.
	generate := func(context.Context) ([]byte, error) {
		call := atomic.AddInt32(&generations, 1)
		return snapshotPayload(float64(call), "2026-01-10T00:00:00Z"), nil
	}

	// Default state: absent settings row means off.
	_, err := repo.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now, generate)
	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable, "default-off must fail closed")
	require.Equal(t, int32(0), atomic.LoadInt32(&generations), "a disabled switch must not generate")

	// Explicitly disabled, with a stale generation left over from a previous enable.
	writeSnapshotSettings(t, service.KeyBillingSnapshotSettings{
		Enabled:       false,
		MaxStaleHours: service.DefaultKeyBillingSnapshotMaxStaleHours,
		Generation:    snapshotGenerationInitial,
	})
	_, err = otherInstance.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now, generate)
	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable, "a disabled switch must fail closed")
	require.Equal(t, int32(0), atomic.LoadInt32(&generations))

	// Enabled, but the caller carries a generation the store does not recognise.
	enableSnapshot(t, snapshotGenerationInitial, service.DefaultKeyBillingSnapshotMaxStaleHours)
	_, err = repo.GetOrCreate(ctx, fixture.binding(), snapshotGenerationRestart, 72*time.Hour, time.Now, generate)
	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable,
		"a mismatched generation must never reuse an incompatible declaration")
	require.Equal(t, int32(0), atomic.LoadInt32(&generations))

	// A recognised generation publishes.
	payload, err := repo.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now, generate)
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&generations))

	// Switching off must immediately stop every instance from serving that declaration.
	disableSnapshot(t)
	_, err = otherInstance.GetOrCreate(ctx, fixture.binding(), snapshotGenerationInitial, 72*time.Hour, time.Now, generate)
	require.ErrorIs(t, err, service.ErrKeyBillingSnapshotUnavailable, "switching off must fail closed")
	require.Equal(t, int32(1), atomic.LoadInt32(&generations))

	// Switching back on starts a new generation, so the old declaration is not reused.
	enableSnapshot(t, snapshotGenerationRestart, service.DefaultKeyBillingSnapshotMaxStaleHours)
	reopened, err := repo.GetOrCreate(ctx, fixture.binding(), snapshotGenerationRestart, 72*time.Hour, time.Now, generate)
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&generations),
		"re-enabling must not serve a declaration from the previous generation")
	require.NotEqual(t, payload, reopened)

	row := readSnapshotRow(t, fixture.keyID)
	require.Equal(t, snapshotGenerationRestart, row.generation)
	require.Equal(t, string(reopened), row.payload.String)
}
