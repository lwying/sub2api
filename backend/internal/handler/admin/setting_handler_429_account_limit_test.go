package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRateLimit429AccountLimitIsConfigurable(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})

	get := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(get)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/rate-limit-429-account-limit", nil)
	h.GetRateLimit429AccountLimit(c)
	require.Equal(t, http.StatusOK, get.Code)
	require.Contains(t, get.Body.String(), `"max_accounts":2`)

	put := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(put)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/rate-limit-429-account-limit", bytes.NewBufferString(`{"max_accounts":3}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateRateLimit429AccountLimit(c)
	require.Equal(t, http.StatusOK, put.Code)
	require.Contains(t, put.Body.String(), `"max_accounts":3`)

	get = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(get)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/rate-limit-429-account-limit", nil)
	h.GetRateLimit429AccountLimit(c)
	require.Equal(t, http.StatusOK, get.Code)
	require.Contains(t, get.Body.String(), `"max_accounts":3`)
}

func TestRateLimit429AccountLimitReadBackIsIndependentOfAccountCooldown(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{
		service.SettingKeyRateLimit429CooldownSettings: `{"enabled":true,"cooldown_seconds":30}`,
	})
	ctx := context.Background()
	limit, err := h.settingService.GetRateLimit429AccountLimit(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, limit)
	require.NoError(t, h.settingService.SetRateLimit429AccountLimit(ctx, 4))
	require.Equal(t, `{"enabled":true,"cooldown_seconds":30}`, repo.values[service.SettingKeyRateLimit429CooldownSettings])
}

func TestRateLimit429AccountLimitRejectsInvalidValues(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	for _, body := range []string{`{"max_accounts":0}`, `{"max_accounts":101}`, `{"max_accounts":-1}`} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/rate-limit-429-account-limit", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateRateLimit429AccountLimit(c)
		require.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
}

// TestRateLimit429AccountLimitAcceptsBoundaryValues 覆盖验收标准 1 的取值边界：
// 允许区间为 1–100（含端点），保存后可按原值读回。
func TestRateLimit429AccountLimitAcceptsBoundaryValues(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	ctx := context.Background()
	for _, value := range []int{1, 100} {
		require.NoError(t, h.settingService.SetRateLimit429AccountLimit(ctx, value), "value=%d", value)
		limit, err := h.settingService.GetRateLimit429AccountLimit(ctx)
		require.NoError(t, err)
		require.Equal(t, value, limit, "value=%d", value)
	}
}

// TestRateLimit429AccountLimitFallsBackToDefaultOnStoredGarbage 覆盖验收标准 1 的
// 旧部署/脏数据路径：存量值缺失、空串或越界时一律取默认 2，而不是报错或放大上限。
func TestRateLimit429AccountLimitFallsBackToDefaultOnStoredGarbage(t *testing.T) {
	for _, stored := range []string{"", "abc", "0", "-3", "101", "9999"} {
		h, _ := newStepUpSwitchTestHandler(t, map[string]string{
			service.SettingKeyRateLimit429AccountLimit: stored,
		})
		limit, err := h.settingService.GetRateLimit429AccountLimit(context.Background())
		require.NoError(t, err, "stored=%q", stored)
		require.Equal(t, 2, limit, "stored=%q", stored)
	}
}
