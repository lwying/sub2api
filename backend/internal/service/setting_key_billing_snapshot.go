package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const (
	SettingKeyKeyBillingSnapshot           = "key_billing_snapshot_settings"
	DefaultKeyBillingSnapshotMaxStaleHours = 72
	MinKeyBillingSnapshotMaxStaleHours     = 24
	MaxKeyBillingSnapshotMaxStaleHours     = 720
)

type KeyBillingSnapshotSettings struct {
	Enabled       bool   `json:"enabled"`
	MaxStaleHours int    `json:"max_stale_hours"`
	Generation    string `json:"generation"`
}

type KeyBillingSnapshotSettingsWriter interface {
	SetKeyBillingSnapshotSettingsAtomic(ctx context.Context, enabled bool, maxStaleHours int) error
}

func (s *SettingService) GetKeyBillingSnapshotSettings(ctx context.Context) (KeyBillingSnapshotSettings, error) {
	defaults := KeyBillingSnapshotSettings{MaxStaleHours: DefaultKeyBillingSnapshotMaxStaleHours, Generation: "disabled"}
	if s == nil || s.settingRepo == nil {
		return defaults, errors.New("key billing snapshot settings are unavailable")
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyKeyBillingSnapshot)
	if errors.Is(err, ErrSettingNotFound) {
		return defaults, nil
	}
	if err != nil {
		return defaults, fmt.Errorf("get key billing snapshot settings: %w", err)
	}
	if raw == "" {
		return defaults, nil
	}
	var settings KeyBillingSnapshotSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return defaults, fmt.Errorf("decode key billing snapshot settings: %w", err)
	}
	if settings.MaxStaleHours < MinKeyBillingSnapshotMaxStaleHours || settings.MaxStaleHours > MaxKeyBillingSnapshotMaxStaleHours {
		settings.MaxStaleHours = DefaultKeyBillingSnapshotMaxStaleHours
	}
	if !settings.Enabled {
		settings.Generation = "disabled"
	}
	if settings.Enabled && strings.TrimSpace(settings.Generation) == "" {
		return defaults, errors.New("enabled key billing snapshot settings have no generation")
	}
	return settings, nil
}

var keyBillingSnapshotSettingMutex sync.Mutex

func (s *SettingService) SetKeyBillingSnapshotSettings(ctx context.Context, enabled bool, maxStaleHours int) error {
	keyBillingSnapshotSettingMutex.Lock()
	defer keyBillingSnapshotSettingMutex.Unlock()
	if maxStaleHours < MinKeyBillingSnapshotMaxStaleHours || maxStaleHours > MaxKeyBillingSnapshotMaxStaleHours {
		return fmt.Errorf("max_stale_hours must be between %d and %d", MinKeyBillingSnapshotMaxStaleHours, MaxKeyBillingSnapshotMaxStaleHours)
	}
	if s == nil || s.settingRepo == nil {
		return errors.New("key billing snapshot settings are unavailable")
	}
	if writer, ok := s.settingRepo.(KeyBillingSnapshotSettingsWriter); ok {
		return writer.SetKeyBillingSnapshotSettingsAtomic(ctx, enabled, maxStaleHours)
	}
	current, err := s.GetKeyBillingSnapshotSettings(ctx)
	if err != nil {
		return err
	}
	settings := KeyBillingSnapshotSettings{
		Enabled: enabled, MaxStaleHours: maxStaleHours, Generation: current.Generation,
	}
	if enabled && (!current.Enabled || current.Generation == "disabled") {
		settings.Generation, err = newSnapshotOwnerToken()
		if err != nil {
			return errors.New("could not create snapshot generation")
		}
	}
	if !enabled {
		settings.Generation = "disabled"
	}
	payload, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode key billing snapshot settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyKeyBillingSnapshot, string(payload))
}
