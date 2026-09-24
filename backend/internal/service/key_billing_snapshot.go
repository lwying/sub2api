package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var ErrKeyBillingSnapshotUnavailable = errors.New("key billing snapshot is temporarily unavailable")

// ErrKeyBillingSnapshotUnboundKey reports that the authoritative key row binds
// the key to no group. This is a decision, not an outage: the live billing path
// answers the same condition with 403, so the snapshot path must preserve that
// answer instead of collapsing it into "temporarily unavailable".
var ErrKeyBillingSnapshotUnboundKey = errors.New("key billing snapshot key is not bound to a group")

type KeyBillingSnapshotSettingsReader interface {
	GetKeyBillingSnapshotSettings(context.Context) (KeyBillingSnapshotSettings, error)
}

type KeyBillingSnapshotBinding struct {
	APIKeyID int64
	UserID   int64
	GroupID  int64
	Revision int64
}

type KeyBillingSnapshotIdentity struct {
	Binding                  KeyBillingSnapshotBinding
	KeyStatus                string
	KeyDeleted               bool
	IPWhitelist              []string
	IPBlacklist              []string
	UserStatus               string
	UserDeleted              bool
	GroupStatus              string
	GroupDeleted             bool
	GroupIsExclusive         bool
	UserRestrictPublicGroups bool
	UserAllowedGroupIDs      []int64
	GroupRate                float64
	GroupPlatform            string
	GroupSubscriptionType    string
	PeakRateEnabled          bool
	PeakStart                string
	PeakEnd                  string
	PeakRateMultiplier       float64
}

type KeyBillingSnapshotRecord struct {
	Binding      KeyBillingSnapshotBinding
	Generation   string
	Payload      []byte
	ObservedAt   time.Time
	RetryAfter   time.Time
	RefreshUntil time.Time
	FailureCode  string
	MaxStale     time.Duration
	CheckedAt    time.Time
}

type KeyBillingSnapshotClaim struct {
	Binding    KeyBillingSnapshotBinding
	Generation string
	OwnerToken string
	Revision   int64
	ExpiresAt  time.Time
}

type KeyBillingSnapshotStore interface {
	ReadAuthoritativeIdentity(ctx context.Context, keyID int64, presentedCredential string) (KeyBillingSnapshotIdentity, error)
	ReadSharedTime(ctx context.Context) (time.Time, error)
	GetOrCreate(ctx context.Context, binding KeyBillingSnapshotBinding, generation string, maxStale time.Duration, now func() time.Time, generate func(context.Context) ([]byte, error)) ([]byte, error)
	Acquire(ctx context.Context, binding KeyBillingSnapshotBinding, generation, ownerToken string, now time.Time, lease time.Duration) (record KeyBillingSnapshotRecord, claim KeyBillingSnapshotClaim, claimed bool, retryAt time.Time, err error)
	Publish(ctx context.Context, claim KeyBillingSnapshotClaim, payload []byte, generatedAt time.Time) (published bool, err error)
	RecordFailure(ctx context.Context, claim KeyBillingSnapshotClaim, retryAt time.Time, safeCode string) (recorded bool, err error)
}

type KeyBillingSnapshotService struct {
	store    KeyBillingSnapshotStore
	settings KeyBillingSnapshotSettingsReader
	rateRepo UserGroupRateRepository
	now      func() time.Time
}

func NewKeyBillingSnapshotService(store KeyBillingSnapshotStore, settings KeyBillingSnapshotSettingsReader, rateRepo ...UserGroupRateRepository) *KeyBillingSnapshotService {
	var rates UserGroupRateRepository
	if len(rateRepo) > 0 {
		rates = rateRepo[0]
	}
	return &KeyBillingSnapshotService{store: store, settings: settings, rateRepo: rates, now: time.Now}
}

// ReadSharedTime reads the store's authoritative clock for snapshot age checks.
func (s *KeyBillingSnapshotService) ReadSharedTime(ctx context.Context) (time.Time, error) {
	if s == nil || s.store == nil {
		return time.Time{}, ErrKeyBillingSnapshotUnavailable
	}
	now, err := s.store.ReadSharedTime(ctx)
	if err != nil || now.IsZero() {
		return time.Time{}, ErrKeyBillingSnapshotUnavailable
	}
	return now, nil
}

// ReadAuthoritativeIdentity verifies the current key, user, and group before a snapshot is served.
func (s *KeyBillingSnapshotService) ReadAuthoritativeIdentity(ctx context.Context, keyID int64, presentedCredential string) (KeyBillingSnapshotIdentity, error) {
	if s == nil || s.store == nil || keyID <= 0 || presentedCredential == "" {
		return KeyBillingSnapshotIdentity{}, ErrKeyBillingSnapshotUnavailable
	}
	identity, err := s.store.ReadAuthoritativeIdentity(ctx, keyID, presentedCredential)
	if err != nil || identity.Binding.APIKeyID != keyID || identity.Binding.UserID <= 0 ||
		(identity.KeyStatus != StatusAPIKeyActive && identity.KeyStatus != StatusAPIKeyQuotaExhausted && identity.KeyStatus != StatusAPIKeyExpired) ||
		identity.KeyDeleted || identity.UserStatus != StatusActive || identity.UserDeleted {
		return KeyBillingSnapshotIdentity{}, ErrKeyBillingSnapshotUnavailable
	}
	// The key row is authoritative for the group binding — the cached
	// apiKey.GroupID may be stale and is never consulted here. A key the row
	// binds to no group keeps the live path's 403, which is a decision rather
	// than an outage; every other group-level rejection still fails closed.
	if identity.Binding.GroupID <= 0 {
		return KeyBillingSnapshotIdentity{}, ErrKeyBillingSnapshotUnboundKey
	}
	if identity.GroupStatus != StatusActive || identity.GroupDeleted ||
		(identity.GroupSubscriptionType != SubscriptionTypeSubscription &&
			!(&User{ID: identity.Binding.UserID, RestrictPublicGroups: identity.UserRestrictPublicGroups, AllowedGroups: identity.UserAllowedGroupIDs}).CanBindGroup(identity.Binding.GroupID, identity.GroupIsExclusive)) {
		return KeyBillingSnapshotIdentity{}, ErrKeyBillingSnapshotUnavailable
	}
	return identity, nil
}

func (s *KeyBillingSnapshotService) ResolveKeyBillingRate(ctx context.Context, userID, groupID int64, groupDefault float64) (float64, *float64, error) {
	if s == nil || s.rateRepo == nil || userID <= 0 || groupID <= 0 {
		return 0, nil, ErrKeyBillingSnapshotUnavailable
	}
	userRate, err := s.rateRepo.GetByUserAndGroup(ctx, userID, groupID)
	if err != nil {
		return 0, nil, ErrKeyBillingSnapshotUnavailable
	}
	if userRate == nil {
		return groupDefault, nil, nil
	}
	resolved := *userRate
	return resolved, &resolved, nil
}

// GetOrCreate returns the shared per-key declaration. Disabled snapshots leave the live path unchanged.
func (s *KeyBillingSnapshotService) GetOrCreate(ctx context.Context, binding KeyBillingSnapshotBinding, generate func(context.Context) ([]byte, error)) ([]byte, bool, error) {
	if s == nil || s.store == nil || s.settings == nil || generate == nil || binding.APIKeyID <= 0 || binding.UserID <= 0 || binding.GroupID <= 0 {
		return nil, false, ErrKeyBillingSnapshotUnavailable
	}
	settings, err := s.settings.GetKeyBillingSnapshotSettings(ctx)
	if err != nil {
		return nil, false, ErrKeyBillingSnapshotUnavailable
	}
	if !settings.Enabled {
		return nil, false, nil
	}
	if settings.Generation == "" {
		return nil, true, ErrKeyBillingSnapshotUnavailable
	}
	maxStale := time.Duration(settings.MaxStaleHours) * time.Hour
	if maxStale < 24*time.Hour || maxStale > 720*time.Hour {
		return nil, true, ErrKeyBillingSnapshotUnavailable
	}
	payload, err := s.store.GetOrCreate(ctx, binding, settings.Generation, maxStale, s.now, generate)
	if err != nil || len(payload) == 0 {
		return nil, true, ErrKeyBillingSnapshotUnavailable
	}
	return payload, true, nil
}

func newSnapshotOwnerToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate snapshot owner token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
