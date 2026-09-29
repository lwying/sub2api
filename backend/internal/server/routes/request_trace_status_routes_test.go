package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The status endpoint shares its parent path with /request-traces/:trace_id, so
// the static segment must win: "status" is not a trace id and must never be
// answered by the trace reader, which would require an admin login session.
func TestRequestTraceStatusRouteIsStaticAndRequiresAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		RequestTrace:       adminhandler.NewRequestTraceHandler(nil),
		RequestTraceStatus: adminhandler.NewRequestTraceStatusHandler(nil),
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer admin-token" {
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
			return
		}
		c.Set("auth_method", service.AuditAuthMethodJWT)
		c.Next()
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	registered := map[string]bool{}
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	require.True(t, registered[http.MethodGet+" /api/v1/admin/request-traces/status"])

	unauthed := httptest.NewRecorder()
	router.ServeHTTP(unauthed, httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces/status", nil))
	require.Equal(t, http.StatusUnauthorized, unauthed.Code)

	authed := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces/status", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	router.ServeHTTP(authed, request)
	// The status handler is unwired in this fixture, so it refuses with its own
	// code. A trace-id lookup would have answered NOT_FOUND instead, which is
	// exactly the confusion this assertion rules out.
	require.Equal(t, http.StatusServiceUnavailable, authed.Code)
	require.Contains(t, authed.Body.String(), "REQUEST_TRACE_STATUS_UNAVAILABLE")
	require.NotContains(t, authed.Body.String(), "REQUEST_TRACE_NOT_FOUND")
}
