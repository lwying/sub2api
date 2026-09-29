package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func doGet429AccountLimit(t *testing.T, h *SettingHandler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/rate-limit-429-account-limit", nil)
	h.GetRateLimit429AccountLimit(c)
	return rec
}

func doPut429AccountLimit(t *testing.T, h *SettingHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/rate-limit-429-account-limit", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateRateLimit429AccountLimit(c)
	return rec
}

// TestRateLimit429AccountLimitCooldownDefaultsOffSessionSixty 覆盖验收标准 1/7：
// 首次读取在 N 之外返回「关闭 / 会话级 / 60 秒」，且读取本身不落库（无 schema 迁移、无隐式写入）。
func TestRateLimit429AccountLimitCooldownDefaultsOffSessionSixty(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})

	rec := doGet429AccountLimit(t, h)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, `"max_accounts":2`)
	require.Contains(t, body, `"enabled":false`)
	require.Contains(t, body, `"scope":"session"`)
	require.Contains(t, body, `"cooldown_seconds":60`)
	_, stored := repo.values[service.SettingKeyRateLimit429AccountLimitCooldown]
	require.False(t, stored, "GET 不得创建冷却设置键")
}

// TestRateLimit429AccountLimitAndCooldownSavedInOneWrite 覆盖验收标准 7 的读写一致与
// 「N 与新配置一次写入」：同一区域保存后按原值读回，且 N 与新 JSON 出现在同一次 SetMultiple 中。
func TestRateLimit429AccountLimitAndCooldownSavedInOneWrite(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})

	put := doPut429AccountLimit(t, h, `{"max_accounts":5,"enabled":true,"scope":"device","cooldown_seconds":120}`)

	require.Equal(t, http.StatusOK, put.Code)
	require.Equal(t, "5", repo.values[service.SettingKeyRateLimit429AccountLimit])
	require.JSONEq(t, `{"enabled":true,"scope":"device","cooldown_seconds":120}`,
		repo.values[service.SettingKeyRateLimit429AccountLimitCooldown])
	require.Equal(t, "5", repo.lastUpdates[service.SettingKeyRateLimit429AccountLimit],
		"N 与新配置必须同一次写入")
	require.NotEmpty(t, repo.lastUpdates[service.SettingKeyRateLimit429AccountLimitCooldown],
		"N 与新配置必须同一次写入")

	get := doGet429AccountLimit(t, h)
	require.Equal(t, http.StatusOK, get.Code)
	body := get.Body.String()
	require.Contains(t, body, `"max_accounts":5`)
	require.Contains(t, body, `"enabled":true`)
	require.Contains(t, body, `"scope":"device"`)
	require.Contains(t, body, `"cooldown_seconds":120`)
}

// TestRateLimit429AccountLimitOldClientPutKeepsCooldownSettings 覆盖向后兼容：
// 只提交 max_accounts 的旧客户端不得重置已保存的开关/粒度/秒数，也不得把新键写空。
func TestRateLimit429AccountLimitOldClientPutKeepsCooldownSettings(t *testing.T) {
	stored := `{"enabled":true,"scope":"device","cooldown_seconds":120}`
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{
		service.SettingKeyRateLimit429AccountLimitCooldown: stored,
	})

	put := doPut429AccountLimit(t, h, `{"max_accounts":7}`)

	require.Equal(t, http.StatusOK, put.Code)
	require.Contains(t, put.Body.String(), `"max_accounts":7`)
	require.Contains(t, put.Body.String(), `"scope":"device"`)
	require.Contains(t, put.Body.String(), `"cooldown_seconds":120`)
	require.JSONEq(t, stored, repo.values[service.SettingKeyRateLimit429AccountLimitCooldown])
	require.Equal(t, map[string]string{service.SettingKeyRateLimit429AccountLimit: "7"}, repo.lastUpdates,
		"旧客户端只写 N 一个键")
}

// TestRateLimit429AccountLimitCooldownPartialUpdateKeepsOtherFields 覆盖省略字段保持现值：
// 只提交 scope 时开关与秒数不变。
func TestRateLimit429AccountLimitCooldownPartialUpdateKeepsOtherFields(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{
		service.SettingKeyRateLimit429AccountLimitCooldown: `{"enabled":true,"scope":"session","cooldown_seconds":120}`,
	})

	put := doPut429AccountLimit(t, h, `{"max_accounts":2,"scope":"device"}`)

	require.Equal(t, http.StatusOK, put.Code)
	require.JSONEq(t, `{"enabled":true,"scope":"device","cooldown_seconds":120}`,
		repo.values[service.SettingKeyRateLimit429AccountLimitCooldown])
}

// TestRateLimit429AccountLimitCooldownAcceptsBoundaryValues 覆盖验收标准 1/7：
// 秒数 1 与 7200（含端点）可保存并读回，两种粒度均可保存。
func TestRateLimit429AccountLimitCooldownAcceptsBoundaryValues(t *testing.T) {
	for _, scope := range []string{"session", "device"} {
		for _, seconds := range []int{1, 7200} {
			h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
			body := `{"max_accounts":2,"enabled":true,"scope":"` + scope + `","cooldown_seconds":` + strconv.Itoa(seconds) + `}`
			require.Equal(t, http.StatusOK, doPut429AccountLimit(t, h, body).Code, body)

			got := doGet429AccountLimit(t, h).Body.String()
			require.Contains(t, got, `"scope":"`+scope+`"`, body)
			require.Contains(t, got, `"cooldown_seconds":`+strconv.Itoa(seconds), body)
		}
	}
}

// TestRateLimit429AccountLimitCooldownRejectsInvalidValues 覆盖验收标准 1/7 的越界拒绝：
// 0/7201 秒即使开关关闭也拒绝，非法粒度拒绝，且拒绝请求不得落任何键。
func TestRateLimit429AccountLimitCooldownRejectsInvalidValues(t *testing.T) {
	for _, body := range []string{
		`{"max_accounts":2,"enabled":false,"cooldown_seconds":0}`,
		`{"max_accounts":2,"enabled":false,"cooldown_seconds":7201}`,
		`{"max_accounts":2,"cooldown_seconds":0}`,
		`{"max_accounts":2,"cooldown_seconds":-1}`,
		`{"max_accounts":2,"scope":"key","cooldown_seconds":60}`,
		`{"max_accounts":2,"scope":"","cooldown_seconds":60}`,
		`{"max_accounts":2,"enabled":true,"scope":"device","cooldown_seconds":7201}`,
	} {
		h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
		rec := doPut429AccountLimit(t, h, body)

		require.Equal(t, http.StatusBadRequest, rec.Code, body)
		_, storedCooldown := repo.values[service.SettingKeyRateLimit429AccountLimitCooldown]
		require.False(t, storedCooldown, body)
		_, storedLimit := repo.values[service.SettingKeyRateLimit429AccountLimit]
		require.False(t, storedLimit, body)
	}
}

// TestRateLimit429AccountLimitCooldownFallsBackToDefaultsOnStoredGarbage 覆盖旧部署/脏数据路径：
// 存量冷却 JSON 无法解析或字段越界时按字段回落到默认值，不报错。
func TestRateLimit429AccountLimitCooldownFallsBackToDefaultsOnStoredGarbage(t *testing.T) {
	for _, stored := range []string{"", "abc", "{}", `{"scope":"key","cooldown_seconds":0}`, `{"enabled":true,"cooldown_seconds":99999}`} {
		h, _ := newStepUpSwitchTestHandler(t, map[string]string{
			service.SettingKeyRateLimit429AccountLimitCooldown: stored,
		})

		cfg, err := h.settingService.GetRateLimit429AccountLimitCooldown(context.Background())
		require.NoError(t, err, "stored=%q", stored)
		expected := service.RateLimit429AccountLimitCooldown{Enabled: false, Scope: service.RateLimit429CooldownScopeSession, CooldownSeconds: 60}
		if stored == `{"enabled":true,"cooldown_seconds":99999}` {
			expected.Enabled = true
		}
		require.Equal(t, expected, cfg, "stored=%q", stored)
	}
}
