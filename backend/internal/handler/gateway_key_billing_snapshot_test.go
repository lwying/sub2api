//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type snapshotHandlerSettingsRepo struct {
	service.SettingRepository
	value string
}

func (r *snapshotHandlerSettingsRepo) GetValue(context.Context, string) (string, error) {
	return r.value, nil
}

type snapshotHandlerStore struct {
	service.KeyBillingSnapshotStore
	identity               service.KeyBillingSnapshotIdentity
	payload                []byte
	calls                  int
	changeBindingOnPublish bool
	onReadIdentity         func(int)
	identityReads          int
	sharedNow              time.Time
	sharedTimeErr          error
}

func (s *snapshotHandlerStore) ReadAuthoritativeIdentity(_ context.Context, _ int64, _ string) (service.KeyBillingSnapshotIdentity, error) {
	s.identityReads++
	if s.onReadIdentity != nil {
		s.onReadIdentity(s.identityReads)
	}
	return s.identity, nil
}
func (s *snapshotHandlerStore) ReadSharedTime(context.Context) (time.Time, error) {
	if s.sharedTimeErr != nil {
		return time.Time{}, s.sharedTimeErr
	}
	if s.sharedNow.IsZero() {
		return time.Now().UTC(), nil
	}
	return s.sharedNow, nil
}
func (s *snapshotHandlerStore) GetOrCreate(ctx context.Context, binding service.KeyBillingSnapshotBinding, generation string, _ time.Duration, now func() time.Time, generate func(context.Context) ([]byte, error)) ([]byte, error) {
	if len(s.payload) > 0 {
		return append([]byte(nil), s.payload...), nil
	}
	payload, err := generate(ctx)
	if err != nil || len(payload) == 0 {
		return nil, service.ErrKeyBillingSnapshotUnavailable
	}
	claim := service.KeyBillingSnapshotClaim{Binding: binding, Generation: generation}
	published, err := s.Publish(ctx, claim, payload, now())
	if err != nil || !published {
		return nil, service.ErrKeyBillingSnapshotUnavailable
	}
	return append([]byte(nil), payload...), nil
}
func (s *snapshotHandlerStore) Acquire(_ context.Context, binding service.KeyBillingSnapshotBinding, generation, ownerToken string, now time.Time, lease time.Duration) (service.KeyBillingSnapshotRecord, service.KeyBillingSnapshotClaim, bool, time.Time, error) {
	if len(s.payload) > 0 {
		return service.KeyBillingSnapshotRecord{Binding: binding, Generation: generation, Payload: s.payload, ObservedAt: now}, service.KeyBillingSnapshotClaim{}, false, time.Time{}, nil
	}
	return service.KeyBillingSnapshotRecord{}, service.KeyBillingSnapshotClaim{Binding: binding, Generation: generation, OwnerToken: ownerToken}, true, time.Time{}, nil
}
func (s *snapshotHandlerStore) Publish(_ context.Context, _ service.KeyBillingSnapshotClaim, payload []byte, _ time.Time) (bool, error) {
	s.calls++
	s.payload = append([]byte(nil), payload...)
	if s.changeBindingOnPublish {
		s.identity.Binding.Revision++
	}
	return true, nil
}

func TestKeyBillingInfoRejectsOldBindingAfterRefresh(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{ID: 5, UserID: 11, Key: "sk-test", GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 0.75}}
	store := &snapshotHandlerStore{identity: service.KeyBillingSnapshotIdentity{
		Binding:   service.KeyBillingSnapshotBinding{APIKeyID: 5, UserID: 11, GroupID: groupID},
		KeyStatus: service.StatusAPIKeyActive, UserStatus: service.StatusActive, GroupStatus: service.StatusActive,
		GroupRate: 0.75, GroupPlatform: service.PlatformAnthropic,
	}}
	store.changeBindingOnPublish = true
	settings := service.NewSettingService(&snapshotHandlerSettingsRepo{value: `{"enabled":true,"max_stale_hours":72,"generation":"epoch-a"}`}, nil)
	repo := &keyBillingUserGroupRateRepo{}
	h := newKeyBillingHandler(repo)
	h.cfg = &config.Config{}
	h.settingService = settings
	h.SetKeyBillingSnapshotService(service.NewKeyBillingSnapshotService(store, settings, repo))
	c, w := newKeyBillingContext(apiKey)
	c.Request.Header.Set("Authorization", "Bearer "+apiKey.Key)
	h.KeyBillingInfo(c)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotContains(t, w.Body.String(), "resolved_rate_multiplier")
}

func TestKeyBillingInfoDoesNotServeOldDeclarationAfterDisableDuringRead(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{ID: 5, UserID: 11, Key: "sk-test", GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 0.75}}
	store := &snapshotHandlerStore{identity: service.KeyBillingSnapshotIdentity{
		Binding:   service.KeyBillingSnapshotBinding{APIKeyID: 5, UserID: 11, GroupID: groupID},
		KeyStatus: service.StatusAPIKeyActive, UserStatus: service.StatusActive, GroupStatus: service.StatusActive,
		GroupRate: 0.75, GroupPlatform: service.PlatformAnthropic,
	}, payload: []byte(`{"resolved_rate_multiplier":0.75,"observed_at":"2026-09-20T00:00:00Z"}`)}
	settingsRepo := &snapshotHandlerSettingsRepo{value: `{"enabled":true,"max_stale_hours":72,"generation":"epoch-a"}`}
	settings := service.NewSettingService(settingsRepo, nil)
	repo := &keyBillingUserGroupRateRepo{}
	h := newKeyBillingHandler(repo)
	h.cfg = &config.Config{}
	h.settingService = settings
	h.SetKeyBillingSnapshotService(service.NewKeyBillingSnapshotService(store, settings, repo))
	store.onReadIdentity = func(read int) {
		if read == 2 {
			settingsRepo.value = `{"enabled":false,"max_stale_hours":72,"generation":"disabled"}`
		}
	}
	c, w := newKeyBillingContext(apiKey)
	c.Request.Header.Set("Authorization", "Bearer "+apiKey.Key)

	h.KeyBillingInfo(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotContains(t, w.Body.String(), "2026-09-20T00:00:00Z")
	require.NotContains(t, w.Body.String(), "resolved_rate_multiplier")
}

func TestKeyBillingInfoRejectsStaleResponseAfterMaximumReducedDuringRead(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{ID: 5, UserID: 11, Key: "sk-test", GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 0.75}}
	store := &snapshotHandlerStore{identity: service.KeyBillingSnapshotIdentity{
		Binding:   service.KeyBillingSnapshotBinding{APIKeyID: 5, UserID: 11, GroupID: groupID},
		KeyStatus: service.StatusAPIKeyActive, UserStatus: service.StatusActive, GroupStatus: service.StatusActive,
		GroupRate: 0.75, GroupPlatform: service.PlatformAnthropic,
	}}
	settingsRepo := &snapshotHandlerSettingsRepo{value: `{"enabled":true,"max_stale_hours":72,"generation":"epoch-a"}`}
	settings := service.NewSettingService(settingsRepo, nil)
	repo := &keyBillingUserGroupRateRepo{}
	h := newKeyBillingHandler(repo)
	h.cfg = &config.Config{}
	h.settingService = settings
	h.SetKeyBillingSnapshotService(service.NewKeyBillingSnapshotService(store, settings, repo))
	store.payload = []byte(`{"resolved_rate_multiplier":0.75,"observed_at":"` + time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339) + `"}`)
	store.onReadIdentity = func(read int) {
		if read == 2 {
			settingsRepo.value = `{"enabled":true,"max_stale_hours":24,"generation":"epoch-a"}`
		}
	}
	c, w := newKeyBillingContext(apiKey)
	c.Request.Header.Set("Authorization", "Bearer "+apiKey.Key)

	h.KeyBillingInfo(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotContains(t, w.Body.String(), "resolved_rate_multiplier")
}

func TestKeyBillingInfoFailsClosedWithoutFirstSnapshot(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{ID: 5, UserID: 11, Key: "sk-test", GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 0.75}}
	store := &snapshotHandlerStore{identity: service.KeyBillingSnapshotIdentity{
		Binding:   service.KeyBillingSnapshotBinding{APIKeyID: 5, UserID: 11, GroupID: groupID},
		KeyStatus: service.StatusAPIKeyActive, UserStatus: service.StatusActive, GroupStatus: service.StatusActive,
		GroupRate: 0.75, GroupPlatform: service.PlatformAnthropic,
	}}
	settings := service.NewSettingService(&snapshotHandlerSettingsRepo{value: `{"enabled":true,"max_stale_hours":72,"generation":"epoch-a"}`}, nil)
	repo := &keyBillingUserGroupRateRepo{err: errors.New("database password leaked")}
	h := newKeyBillingHandler(repo)
	h.cfg = &config.Config{}
	h.settingService = settings
	h.SetKeyBillingSnapshotService(service.NewKeyBillingSnapshotService(store, settings, repo))
	c, w := newKeyBillingContext(apiKey)
	c.Request.Header.Set("Authorization", "Bearer "+apiKey.Key)

	h.KeyBillingInfo(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotContains(t, w.Body.String(), "rate_multiplier")
	require.NotContains(t, w.Body.String(), "database password leaked")
	require.Equal(t, 1, repo.lookupCalls)
	require.Zero(t, store.calls)
}

func TestKeyBillingInfoRejectsDifferentSnapshotGenerationAfterReenable(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{ID: 5, UserID: 11, Key: "sk-test", GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 0.75}}
	store := &snapshotHandlerStore{identity: service.KeyBillingSnapshotIdentity{
		Binding:   service.KeyBillingSnapshotBinding{APIKeyID: 5, UserID: 11, GroupID: groupID},
		KeyStatus: service.StatusAPIKeyActive, UserStatus: service.StatusActive, GroupStatus: service.StatusActive,
		GroupRate: 0.75, GroupPlatform: service.PlatformAnthropic,
	}, payload: []byte(`{"resolved_rate_multiplier":0.75,"observed_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)}
	settingsRepo := &snapshotHandlerSettingsRepo{value: `{"enabled":true,"max_stale_hours":72,"generation":"epoch-a"}`}
	settings := service.NewSettingService(settingsRepo, nil)
	repo := &keyBillingUserGroupRateRepo{}
	h := newKeyBillingHandler(repo)
	h.cfg = &config.Config{}
	h.settingService = settings
	h.SetKeyBillingSnapshotService(service.NewKeyBillingSnapshotService(store, settings, repo))
	store.onReadIdentity = func(read int) {
		if read == 2 {
			settingsRepo.value = `{"enabled":true,"max_stale_hours":72,"generation":"epoch-b"}`
		}
	}
	c, w := newKeyBillingContext(apiKey)
	c.Request.Header.Set("Authorization", "Bearer "+apiKey.Key)

	h.KeyBillingInfo(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotContains(t, w.Body.String(), "resolved_rate_multiplier")
}

func TestKeyBillingInfoGeneratesAtSharedClockWhenAppClockIsAhead(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{ID: 5, UserID: 11, Key: "sk-test", GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 0.75}}
	sharedNow := time.Now().UTC().Add(-2 * time.Hour)
	store := &snapshotHandlerStore{identity: service.KeyBillingSnapshotIdentity{
		Binding:   service.KeyBillingSnapshotBinding{APIKeyID: 5, UserID: 11, GroupID: groupID},
		KeyStatus: service.StatusAPIKeyActive, UserStatus: service.StatusActive, GroupStatus: service.StatusActive,
		GroupRate: 0.75, GroupPlatform: service.PlatformAnthropic,
	}, sharedNow: sharedNow}
	settings := service.NewSettingService(&snapshotHandlerSettingsRepo{value: `{"enabled":true,"max_stale_hours":72,"generation":"epoch-a"}`}, nil)
	repo := &keyBillingUserGroupRateRepo{}
	h := newKeyBillingHandler(repo)
	h.cfg = &config.Config{}
	h.settingService = settings
	h.SetKeyBillingSnapshotService(service.NewKeyBillingSnapshotService(store, settings, repo))
	c, w := newKeyBillingContext(apiKey)
	c.Request.Header.Set("Authorization", "Bearer "+apiKey.Key)

	h.KeyBillingInfo(c)

	require.Equal(t, http.StatusOK, w.Code)
	var data keyBillingInfoResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &data))
	require.WithinDuration(t, sharedNow, data.ObservedAt, time.Second)
	require.Equal(t, 1, store.calls)
}

func TestKeyBillingInfoServesDatabaseFreshSnapshotDespiteLaggingAppClock(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{ID: 5, UserID: 11, Key: "sk-test", GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 0.75}}
	store := &snapshotHandlerStore{identity: service.KeyBillingSnapshotIdentity{
		Binding:   service.KeyBillingSnapshotBinding{APIKeyID: 5, UserID: 11, GroupID: groupID},
		KeyStatus: service.StatusAPIKeyActive, UserStatus: service.StatusActive, GroupStatus: service.StatusActive,
		GroupRate: 0.75, GroupPlatform: service.PlatformAnthropic,
	}}
	observedAt := time.Now().UTC().Add(5 * time.Minute)
	store.sharedNow = observedAt.Add(time.Second)
	store.payload = []byte(`{"resolved_rate_multiplier":0.75,"observed_at":"` + observedAt.Format(time.RFC3339) + `"}`)
	settings := service.NewSettingService(&snapshotHandlerSettingsRepo{value: `{"enabled":true,"max_stale_hours":72,"generation":"epoch-a"}`}, nil)
	repo := &keyBillingUserGroupRateRepo{}
	h := newKeyBillingHandler(repo)
	h.cfg = &config.Config{}
	h.settingService = settings
	h.SetKeyBillingSnapshotService(service.NewKeyBillingSnapshotService(store, settings, repo))
	c, w := newKeyBillingContext(apiKey)
	c.Request.Header.Set("Authorization", "Bearer "+apiKey.Key)

	h.KeyBillingInfo(c)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, store.payload, w.Body.Bytes())
}

func TestKeyBillingInfoFailsClosedWhenSharedClockUnavailable(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{ID: 5, UserID: 11, Key: "sk-test", GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 0.75}}
	store := &snapshotHandlerStore{identity: service.KeyBillingSnapshotIdentity{
		Binding:   service.KeyBillingSnapshotBinding{APIKeyID: 5, UserID: 11, GroupID: groupID},
		KeyStatus: service.StatusAPIKeyActive, UserStatus: service.StatusActive, GroupStatus: service.StatusActive,
		GroupRate: 0.75, GroupPlatform: service.PlatformAnthropic,
	}, sharedTimeErr: errors.New("database password leaked")}
	store.payload = []byte(`{"resolved_rate_multiplier":0.75,"observed_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	settings := service.NewSettingService(&snapshotHandlerSettingsRepo{value: `{"enabled":true,"max_stale_hours":72,"generation":"epoch-a"}`}, nil)
	repo := &keyBillingUserGroupRateRepo{}
	h := newKeyBillingHandler(repo)
	h.cfg = &config.Config{}
	h.settingService = settings
	h.SetKeyBillingSnapshotService(service.NewKeyBillingSnapshotService(store, settings, repo))
	c, w := newKeyBillingContext(apiKey)
	c.Request.Header.Set("Authorization", "Bearer "+apiKey.Key)

	h.KeyBillingInfo(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotContains(t, w.Body.String(), "resolved_rate_multiplier")
	require.NotContains(t, w.Body.String(), "database password leaked")
}

func TestKeyBillingInfoReturnsSameDeclarationWhileEnabled(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{ID: 5, UserID: 11, Key: "sk-test", GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 0.75}}
	store := &snapshotHandlerStore{identity: service.KeyBillingSnapshotIdentity{
		Binding:   service.KeyBillingSnapshotBinding{APIKeyID: 5, UserID: 11, GroupID: groupID},
		KeyStatus: service.StatusAPIKeyActive, UserStatus: service.StatusActive, GroupStatus: service.StatusActive,
		GroupRate: 0.75, GroupPlatform: service.PlatformAnthropic,
	}}
	settings := service.NewSettingService(&snapshotHandlerSettingsRepo{value: `{"enabled":true,"max_stale_hours":72,"generation":"epoch-a"}`}, nil)
	repo := &keyBillingUserGroupRateRepo{}
	h := newKeyBillingHandler(repo)
	h.cfg = &config.Config{}
	h.settingService = settings
	h.SetKeyBillingSnapshotService(service.NewKeyBillingSnapshotService(store, settings, repo))

	call := func() (int, []byte) {
		c, w := newKeyBillingContext(apiKey)
		c.Request.Header.Set("Authorization", "Bearer "+apiKey.Key)
		h.KeyBillingInfo(c)
		return w.Code, w.Body.Bytes()
	}
	status, first := call()
	require.Equal(t, http.StatusOK, status)
	status, second := call()
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(first), string(second))
	require.Equal(t, 1, store.calls)
	var data map[string]any
	require.NoError(t, json.Unmarshal(first, &data))
	require.Equal(t, 0.75, data["resolved_rate_multiplier"])
}

// An unbound key is a decision, not an outage: the live billing path answers
// 403/API key is not assigned to a group, and enabling the snapshot must not
// turn that into 503. The decision must come from the authoritative key row,
// never from a potentially stale cached apiKey.GroupID.
func TestKeyBillingInfoPreservesUnboundKeyForbiddenWhenSnapshotEnabled(t *testing.T) {
	liveSettings := `{"enabled":false,"max_stale_hours":72,"generation":"disabled"}`
	enabledSettings := `{"enabled":true,"max_stale_hours":72,"generation":"epoch-a"}`

	run := func(t *testing.T, settingsValue string, cachedGroupID *int64, store *snapshotHandlerStore, repo *keyBillingUserGroupRateRepo) (int, string) {
		t.Helper()
		apiKey := &service.APIKey{ID: 5, UserID: 11, Key: "sk-test", GroupID: cachedGroupID}
		settings := service.NewSettingService(&snapshotHandlerSettingsRepo{value: settingsValue}, nil)
		h := newKeyBillingHandler(repo)
		h.cfg = &config.Config{}
		h.settingService = settings
		h.SetKeyBillingSnapshotService(service.NewKeyBillingSnapshotService(store, settings, repo))
		c, w := newKeyBillingContext(apiKey)
		c.Request.Header.Set("Authorization", "Bearer "+apiKey.Key)
		h.KeyBillingInfo(c)
		return w.Code, w.Body.String()
	}

	// unboundStore reports a key the authoritative row binds to no group.
	newUnboundStore := func() *snapshotHandlerStore {
		return &snapshotHandlerStore{identity: service.KeyBillingSnapshotIdentity{
			Binding:   service.KeyBillingSnapshotBinding{APIKeyID: 5, UserID: 11, GroupID: 0, Revision: 4},
			KeyStatus: service.StatusAPIKeyActive, UserStatus: service.StatusActive, GroupStatus: service.StatusActive,
			GroupSubscriptionType: service.SubscriptionTypeStandard,
		}}
	}

	cachedGroup := int64(7)
	for _, tc := range []struct {
		name          string
		cachedGroupID *int64
	}{
		{name: "cached key has no group"},
		{name: "cached key still points at a stale group", cachedGroupID: &cachedGroup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newUnboundStore()
			status, body := run(t, enabledSettings, tc.cachedGroupID, store, &keyBillingUserGroupRateRepo{})

			require.Equal(t, http.StatusForbidden, status)
			require.Contains(t, body, "permission_error")
			require.NotContains(t, body, "temporarily unavailable")
			require.Zero(t, store.calls, "an unbound key must not publish a declaration")
			require.Equal(t, 1, store.identityReads, "the handler must read the authoritative identity exactly once")
		})
	}

	t.Run("matches the live path when the snapshot is disabled", func(t *testing.T) {
		liveStatus, liveBody := run(t, liveSettings, nil, newUnboundStore(), &keyBillingUserGroupRateRepo{})
		snapshotStatus, snapshotBody := run(t, enabledSettings, nil, newUnboundStore(), &keyBillingUserGroupRateRepo{})

		require.Equal(t, http.StatusForbidden, liveStatus)
		require.Equal(t, liveStatus, snapshotStatus)
		require.JSONEq(t, liveBody, snapshotBody)
	})
}
