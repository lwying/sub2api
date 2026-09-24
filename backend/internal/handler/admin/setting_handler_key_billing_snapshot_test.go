//go:build unit

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type keyBillingSnapshotAdminSettingsRepo struct {
	values map[string]string
}

func (r *keyBillingSnapshotAdminSettingsRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}
func (r *keyBillingSnapshotAdminSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}
func (r *keyBillingSnapshotAdminSettingsRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}
func (r *keyBillingSnapshotAdminSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			result[key] = value
		}
	}
	return result, nil
}
func (r *keyBillingSnapshotAdminSettingsRepo) SetMultiple(_ context.Context, values map[string]string) error {
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}
func (r *keyBillingSnapshotAdminSettingsRepo) GetAll(context.Context) (map[string]string, error) {
	return r.values, nil
}
func (r *keyBillingSnapshotAdminSettingsRepo) Delete(_ context.Context, key string) error {
	delete(r.values, key)
	return nil
}

func newKeyBillingSnapshotAdminSettingsHandler() (*SettingHandler, *keyBillingSnapshotAdminSettingsRepo) {
	repo := &keyBillingSnapshotAdminSettingsRepo{values: make(map[string]string)}
	return NewSettingHandler(service.NewSettingService(repo, nil), nil, nil, nil, nil, nil, nil), repo
}

func TestKeyBillingSnapshotSettingsCanBeReadAndUpdated(t *testing.T) {
	h, repo := newKeyBillingSnapshotAdminSettingsHandler()

	get := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(get)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/key-billing-snapshot", nil)
	h.GetKeyBillingSnapshotSettings(c)
	var response struct {
		Data struct {
			Enabled       bool `json:"enabled"`
			MaxStaleHours int  `json:"max_stale_hours"`
		} `json:"data"`
	}
	require.Equal(t, http.StatusOK, get.Code)
	require.NoError(t, json.Unmarshal(get.Body.Bytes(), &response))
	require.False(t, response.Data.Enabled)
	require.Equal(t, 72, response.Data.MaxStaleHours)

	put := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(put)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/key-billing-snapshot", bytes.NewBufferString(`{"enabled":true,"max_stale_hours":48}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateKeyBillingSnapshotSettings(c)
	require.Equal(t, http.StatusOK, put.Code)
	require.Contains(t, put.Body.String(), `"enabled":true`)
	require.Contains(t, put.Body.String(), `"max_stale_hours":48`)
	var persisted struct {
		Enabled       bool   `json:"enabled"`
		MaxStaleHours int    `json:"max_stale_hours"`
		Generation    string `json:"generation"`
	}
	require.NoError(t, json.Unmarshal([]byte(repo.values[service.SettingKeyKeyBillingSnapshot]), &persisted))
	require.True(t, persisted.Enabled)
	require.Equal(t, 48, persisted.MaxStaleHours)
	require.NotEmpty(t, persisted.Generation)

	get = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(get)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/key-billing-snapshot", nil)
	h.GetKeyBillingSnapshotSettings(c)
	require.Equal(t, http.StatusOK, get.Code)
	require.NoError(t, json.Unmarshal(get.Body.Bytes(), &response))
	require.True(t, response.Data.Enabled)
	require.Equal(t, 48, response.Data.MaxStaleHours)
}

func TestUpdateKeyBillingSnapshotSettingsRejectsStaleAgeOutsideRange(t *testing.T) {
	h, _ := newKeyBillingSnapshotAdminSettingsHandler()
	for _, body := range []string{
		`{"enabled":true,"max_stale_hours":23}`,
		`{"enabled":true,"max_stale_hours":721}`,
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/key-billing-snapshot", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateKeyBillingSnapshotSettings(c)
		require.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
}
