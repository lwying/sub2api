//go:build unit

package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceSettingsRejectAPIKeyEnableButAllowDisable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewSettingHandler(nil, nil, nil, nil, nil, nil, nil)
	for _, tc := range []struct {
		name, authMethod, payload string
		wantStatus                int
	}{
		{"api key enable", service.AuditAuthMethodAdminAPIKey, `{"enabled":true}`, http.StatusForbidden},
		{"no auth method enable", "", `{"enabled":true}`, http.StatusForbidden},
		{"api key disable", service.AuditAuthMethodAdminAPIKey, `{"enabled":false}`, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("auth_method", tc.authMethod)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/request-trace", bytes.NewBufferString(tc.payload))
			c.Request.Header.Set("Content-Type", "application/json")
			h.UpdateRequestTraceOperatorSettings(c)
			require.Equal(t, tc.wantStatus, w.Code)
		})
	}
}
