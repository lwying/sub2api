//go:build unit

package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeyBillingSnapshotSettingsDefaultOffWith72HourMaximumStaleAge(t *testing.T) {
	repo := &settingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, nil)

	got, err := svc.GetKeyBillingSnapshotSettings(context.Background())

	require.NoError(t, err)
	require.False(t, got.Enabled)
	require.Equal(t, 72, got.MaxStaleHours)
}

func TestKeyBillingSnapshotSettingsPersistValidateAndAdvanceGeneration(t *testing.T) {
	repo := &keyBillingSnapshotSettingRepo{values: map[string]string{}}
	svc := NewSettingService(repo, nil)

	require.NoError(t, svc.SetKeyBillingSnapshotSettings(context.Background(), true, 48))
	first, err := svc.GetKeyBillingSnapshotSettings(context.Background())
	require.NoError(t, err)
	require.True(t, first.Enabled)
	require.Equal(t, 48, first.MaxStaleHours)
	require.NotEmpty(t, first.Generation)

	require.NoError(t, svc.SetKeyBillingSnapshotSettings(context.Background(), false, 48))
	require.NoError(t, svc.SetKeyBillingSnapshotSettings(context.Background(), true, 48))
	second, err := svc.GetKeyBillingSnapshotSettings(context.Background())
	require.NoError(t, err)
	require.NotEqual(t, first.Generation, second.Generation)
	for _, invalid := range []int{23, 721} {
		require.Error(t, svc.SetKeyBillingSnapshotSettings(context.Background(), true, invalid))
	}
}

type keyBillingSnapshotSettingRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func (r *keyBillingSnapshotSettingRepo) Get(_ context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}
func (r *keyBillingSnapshotSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}
func (r *keyBillingSnapshotSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}
func (r *keyBillingSnapshotSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	return nil, nil
}
func (r *keyBillingSnapshotSettingRepo) SetMultiple(_ context.Context, values map[string]string) error {
	return nil
}
func (r *keyBillingSnapshotSettingRepo) GetAll(context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}
func (r *keyBillingSnapshotSettingRepo) Delete(context.Context, string) error {
	panic("unexpected Delete call")
}

func TestKeyBillingSnapshotSettingsPersistAsValidJSON(t *testing.T) {
	repo := &keyBillingSnapshotSettingRepo{values: map[string]string{}}
	svc := NewSettingService(repo, nil)
	require.NoError(t, svc.SetKeyBillingSnapshotSettings(context.Background(), true, 72))
	var persisted KeyBillingSnapshotSettings
	require.NoError(t, json.Unmarshal([]byte(repo.values[SettingKeyKeyBillingSnapshot]), &persisted))
	require.True(t, persisted.Enabled)
}
