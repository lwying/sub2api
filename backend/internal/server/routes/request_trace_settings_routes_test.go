package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceOperatorSettingsRoutesRequireAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	settings := service.NewSettingService(errorDiagnosticSettingsRouteStubRepo{}, &config.Config{})
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		Setting:      adminhandler.NewSettingHandler(settings, nil, nil, nil, nil, nil, nil),
		RequestTrace: adminhandler.NewRequestTraceHandler(nil),
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer admin-token" {
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
			return
		}
		c.Set("auth_method", service.AuditAuthMethodJWT)
		c.Next()
	})
	noopAudit := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, noopAudit, stepUp, nil, nil)

	for _, tc := range []struct {
		name, method, path, token string
		want                      int
	}{
		{"unauthed read", http.MethodGet, "/api/v1/admin/settings/request-trace", "", http.StatusUnauthorized},
		{"admin read", http.MethodGet, "/api/v1/admin/settings/request-trace", "Bearer admin-token", http.StatusOK},
		{"unauthed change", http.MethodPut, "/api/v1/admin/settings/request-trace", "", http.StatusUnauthorized},
		{"unauthed trace list", http.MethodGet, "/api/v1/admin/request-traces", "", http.StatusUnauthorized},
		{"unauthed trace detail", http.MethodGet, "/api/v1/admin/request-traces/0123456789abcdef0123456789abcdef", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.token != "" {
				req.Header.Set("Authorization", tc.token)
			}
			router.ServeHTTP(w, req)
			require.Equal(t, tc.want, w.Code)
		})
	}
}
