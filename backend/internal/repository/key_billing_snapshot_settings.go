package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func newSnapshotSettingsGeneration() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (r *settingRepository) SetKeyBillingSnapshotSettingsAtomic(ctx context.Context, enabled bool, maxStaleHours int) error {
	if r == nil || r.client == nil {
		return errors.New("key billing snapshot settings are unavailable")
	}
	if maxStaleHours < service.MinKeyBillingSnapshotMaxStaleHours || maxStaleHours > service.MaxKeyBillingSnapshotMaxStaleHours {
		return fmt.Errorf("max_stale_hours must be between %d and %d", service.MinKeyBillingSnapshotMaxStaleHours, service.MaxKeyBillingSnapshotMaxStaleHours)
	}
	driver, ok := r.client.Driver().(*entsql.Driver)
	if !ok || driver.DB() == nil {
		return errors.New("key billing snapshot settings database is unavailable")
	}
	db := driver.DB()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	const key = service.SettingKeyKeyBillingSnapshot
	defaultValue, err := json.Marshal(service.KeyBillingSnapshotSettings{
		Enabled: false, MaxStaleHours: service.DefaultKeyBillingSnapshotMaxStaleHours, Generation: "disabled",
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO NOTHING
	`, key, string(defaultValue), time.Now().UTC()); err != nil {
		return err
	}
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1 FOR UPDATE`, key).Scan(&raw)
	current := service.KeyBillingSnapshotSettings{MaxStaleHours: service.DefaultKeyBillingSnapshotMaxStaleHours, Generation: "disabled"}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && raw != "" {
		if decodeErr := json.Unmarshal([]byte(raw), &current); decodeErr != nil {
			return fmt.Errorf("decode key billing snapshot settings: %w", decodeErr)
		}
	}
	if current.MaxStaleHours < service.MinKeyBillingSnapshotMaxStaleHours || current.MaxStaleHours > service.MaxKeyBillingSnapshotMaxStaleHours {
		current.MaxStaleHours = service.DefaultKeyBillingSnapshotMaxStaleHours
	}
	updated := service.KeyBillingSnapshotSettings{Enabled: enabled, MaxStaleHours: maxStaleHours, Generation: current.Generation}
	if enabled && (!current.Enabled || current.Generation == "disabled" || current.Generation == "") {
		updated.Generation, err = newSnapshotSettingsGeneration()
		if err != nil {
			return errors.New("could not create snapshot generation")
		}
	}
	if !enabled {
		updated.Generation = "disabled"
	}
	payload, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at
	`, key, string(payload), time.Now().UTC())
	if err != nil {
		return err
	}
	return tx.Commit()
}
