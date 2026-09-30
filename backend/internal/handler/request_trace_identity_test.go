//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceCapturesOnlyAuthenticatedRequestIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, authenticated := range []bool{false, true} {
		repo := &gateSnapshotTraceRepo{}
		queue := service.NewRequestTraceCaptureQueue(repo)
		router := gin.New()
		router.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate {
			return RequestTraceGateForCapture(true)
		}, repo, queue), func(c *gin.Context) {
			if authenticated {
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 19, UserID: 23})
			}
			c.Status(http.StatusOK)
		})
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		waitForGateSnapshotTraces(t, repo, 1)
		trace := repo.storedTraces()[0]
		if authenticated {
			require.NotNil(t, trace.UserID)
			require.NotNil(t, trace.APIKeyID)
			require.Equal(t, int64(23), *trace.UserID)
			require.Equal(t, int64(19), *trace.APIKeyID)
		} else {
			require.Nil(t, trace.UserID)
			require.Nil(t, trace.APIKeyID)
		}
		queue.Stop()
	}
}
